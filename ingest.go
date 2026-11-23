package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// retention is how long measurements are kept. The calculation needs the
// last 12 months; the rest is a buffer.
const retention = 400 * 24 * time.Hour

// pollOverlap is subtracted from the newest known updated_at before asking
// for changes. Records that become visible late, or whose updated_at lies
// slightly before ours, are still fetched. The overlap costs a few records
// per poll that change nothing.
const pollOverlap = time.Hour

// ingester polls the API regularly and writes the results into the store
// and the snapshot.
type ingester struct {
	client       *apiClient
	store        *store
	snapshotPath string
	interval     time.Duration
	// maxInvalidations is how many valid hours a single poll may turn
	// invalid. A poll above the limit is rejected as a likely broken delivery.
	maxInvalidations int
	// fullReload makes the next poll fetch everything and re-normalize the
	// stored hours (see store.reapply). It is set when the snapshot has an
	// old format and cleared once the new snapshot is written.
	fullReload bool
}

// run polls until ctx is cancelled. After a failed poll it retries sooner,
// with a growing delay.
func (in *ingester) run(ctx context.Context) {
	failures := 0
	for {
		err := in.poll(ctx)
		if ctx.Err() != nil {
			return
		}
		in.store.recordPoll(err, time.Now())

		wait := in.interval
		if err != nil {
			failures++
			wait = retryDelay(failures, err, in.interval)
			slog.Error("poll failed", "err", err, "failures", failures, "retry_in", wait)
		} else {
			failures = 0
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// retryDelay doubles from 15s per failure up to max and honors Retry-After.
func retryDelay(failures int, err error, max time.Duration) time.Duration {
	d := 15 * time.Second << min(failures-1, 10)
	if d > max {
		d = max
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > d {
		d = apiErr.RetryAfter
	}
	return d
}

// poll fetches master data and all measurements changed since the last
// poll. On the first poll (empty store) this is the complete history.
func (in *ingester) poll(ctx context.Context) error {
	// Measurements are fetched even when the master data fails; the
	// error is reported at the end.
	masterErr := in.refreshMasterData(ctx)

	// Zero means: fetch everything.
	var since time.Time
	if !in.fullReload {
		since = in.store.maxUpdatedAt()
	}
	if !since.IsZero() {
		since = since.Add(-pollOverlap)
	}
	raws, err := in.client.fetchMeasurements(ctx, since)
	if err != nil {
		return errors.Join(masterErr, fmt.Errorf("fetch measurements: %w", err))
	}

	ms := make([]measurement, 0, len(raws))
	reasons := map[versionKey]*invalidReason{}
	var skipped []string
	for _, r := range raws {
		m, reason, err := normalize(r)
		if err != nil {
			skipped = append(skipped, err.Error())
			continue
		}
		if reason != nil {
			reasons[keyOf(m)] = reason
		}
		ms = append(ms, m)
	}

	// Nothing is taken over from a suspicious delivery. Since the store does
	// not change, the next poll fetches the same records again; if the API
	// has fixed them by then, they go through.
	invalidations := in.store.countInvalidations(ms, in.fullReload)
	if invalidations > in.maxInvalidations {
		return errors.Join(masterErr, fmt.Errorf(
			"rejected %d fetched records: they would turn %d valid hours invalid (limit %d)",
			len(raws), invalidations, in.maxInvalidations))
	}

	var res upsertResult
	if in.fullReload {
		res = in.store.reapply(ms)
	} else {
		res = in.store.upsert(ms)
	}
	pruned := in.store.prune(time.Now().Add(-retention))

	// Report only invalid values that reached the store. Records the store
	// already knows (e.g. the boundary value of updated_since) are no news.
	invalid := map[string][]string{}
	invalidCount := 0
	for _, m := range res.applied() {
		reason := reasons[keyOf(m)]
		if reason == nil {
			continue
		}
		invalid[reason.Kind] = append(invalid[reason.Kind], m.Time.Format(time.RFC3339)+": "+reason.Detail)
		invalidCount++
	}

	logInvalid(invalid)
	if len(skipped) > 0 {
		slog.Warn("skipped measurements", "count", len(skipped), "examples", firstN(skipped, 5))
	}
	logCorrections(res.Corrections)
	slog.Info("poll done",
		"updated_since", since,
		"full_reload", in.fullReload,
		"fetched", len(raws),
		"added", len(res.Added),
		"corrected", len(res.Corrections),
		"invalid", invalidCount,
		"skipped", len(skipped),
		"pruned", pruned,
		"newest", in.store.status().NewestMeasurement,
	)

	saveErr := in.saveSnapshot()
	if saveErr == nil && in.fullReload {
		in.fullReload = false
		slog.Info("full reload done, snapshot rewritten in the current format")
	}
	return errors.Join(masterErr, saveErr)
}

// refreshMasterData fetches mast, turbine and site. Invalid data is not
// taken over; the last valid version stays in use and an error is returned.
func (in *ingester) refreshMasterData(ctx context.Context) error {
	m, err := in.client.fetchMast(ctx)
	if err != nil {
		return fmt.Errorf("fetch mast: %w", err)
	}
	t, err := in.client.fetchTurbine(ctx)
	if err != nil {
		return fmt.Errorf("fetch turbine: %w", err)
	}
	s, err := in.client.fetchSite(ctx)
	if err != nil {
		return fmt.Errorf("fetch site: %w", err)
	}

	var errs []error
	var validMast *mast
	var validTurbine *turbine
	var validSite *site

	err = validateMast(m)
	if err != nil {
		errs = append(errs, fmt.Errorf("invalid mast, keeping last valid: %w", err))
	} else {
		validMast = &m
	}
	err = validateTurbine(t)
	if err != nil {
		errs = append(errs, fmt.Errorf("invalid turbine, keeping last valid: %w", err))
	} else {
		validTurbine = &t
	}
	err = validateSite(s)
	if err != nil {
		errs = append(errs, fmt.Errorf("invalid site, keeping last valid: %w", err))
	} else {
		validSite = &s
	}

	changed := in.store.setMasterData(validMast, validTurbine, validSite)
	if changed {
		slog.Info("master data updated", "turbine_version", t.Version, "site_version", s.Version)
	}
	return errors.Join(errs...)
}

// versionKey identifies one version of one hour.
type versionKey struct {
	unix    int64
	version int
}

func keyOf(m measurement) versionKey {
	return versionKey{m.Time.Unix(), m.Version}
}

// logInvalid writes one line per kind of reason with a few examples, so a
// broken backfill cannot flood the log.
func logInvalid(invalid map[string][]string) {
	kinds := make([]string, 0, len(invalid))
	for kind := range invalid {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		slog.Warn("invalid measurements", "reason", kind, "count", len(invalid[kind]), "examples", firstN(invalid[kind], 3))
	}
}

// logCorrections reports hours that got a new version. Corrections that turn
// a valid hour invalid are counted separately because they remove data.
func logCorrections(corrections []correction) {
	if len(corrections) == 0 {
		return
	}
	invalidated := 0
	examples := make([]string, 0, 5)
	for _, c := range corrections {
		if c.Old.Valid && !c.New.Valid {
			invalidated++
		}
		if len(examples) < 5 {
			examples = append(examples, fmt.Sprintf("%s: v%d %s -> v%d %s",
				c.New.Time.Format(time.RFC3339), c.Old.Version, speedText(c.Old), c.New.Version, speedText(c.New)))
		}
	}
	slog.Info("corrected measurements", "count", len(corrections), "valid_to_invalid", invalidated, "examples", examples)
}

func speedText(m measurement) string {
	if !m.Valid {
		return "invalid"
	}
	return fmt.Sprintf("%.2f m/s", m.SpeedMS)
}

func firstN(s []string, n int) []string {
	return s[:min(n, len(s))]
}

func (in *ingester) saveSnapshot() error {
	// After a full reload the snapshot is written even without changes,
	// to replace the old format.
	if !in.store.isDirty() && !in.fullReload {
		return nil
	}
	err := saveSnapshot(in.snapshotPath, in.store.snapshot(time.Now()))
	if err != nil {
		return fmt.Errorf("save snapshot: %w", err)
	}
	in.store.clearDirty()
	return nil
}

func validateMast(m mast) error {
	if m.MeasurementHeightM <= 0 {
		return fmt.Errorf("measurement height %v m is not positive", m.MeasurementHeightM)
	}
	return nil
}

func validateTurbine(t turbine) error {
	if t.HubHeightM <= 0 {
		return fmt.Errorf("hub height %v m is not positive", t.HubHeightM)
	}
	if len(t.PowerCurve) < 2 {
		return fmt.Errorf("power curve has %d points, need at least 2", len(t.PowerCurve))
	}
	for i, p := range t.PowerCurve {
		if p.PowerKW < 0 {
			return fmt.Errorf("power curve point %d has negative power", i)
		}
		if i > 0 && p.WindSpeedMS <= t.PowerCurve[i-1].WindSpeedMS {
			return fmt.Errorf("power curve not strictly ascending at point %d", i)
		}
	}
	return nil
}

func validateSite(s site) error {
	if s.ShearExponent < 0 || s.ShearExponent > 1 {
		return fmt.Errorf("shear exponent %v outside 0..1", s.ShearExponent)
	}
	fractions := []struct {
		name  string
		value float64
	}{
		{"availability loss", s.Losses.Availability},
		{"electrical loss", s.Losses.Electrical},
		{"wake loss", s.Losses.Wake},
		{"uncertainty", s.Uncertainty},
	}
	for _, f := range fractions {
		if f.value < 0 || f.value >= 1 {
			return fmt.Errorf("%s %v is not a fraction in 0..1", f.name, f.value)
		}
	}
	return nil
}
