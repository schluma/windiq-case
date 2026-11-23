package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestIngester(t *testing.T) (*fakeAPI, *ingester) {
	t.Helper()
	f, c := newFakeAPI(t)
	in := &ingester{
		client:           c,
		store:            newStore(),
		snapshotPath:     filepath.Join(t.TempDir(), "snapshot.json"),
		interval:         5 * time.Minute,
		maxInvalidations: 24,
	}
	return f, in
}

// recentHour returns the start of the hour n hours ago, so that the data
// is not pruned as too old.
func recentHour(n int) string {
	return time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(n) * time.Hour).Format(time.RFC3339)
}

func TestPollBackfillThenIncremental(t *testing.T) {
	f, in := newTestIngester(t)
	f.measurements = []map[string]any{
		rec(recentHour(3), 5.0, "m/s", 1, recentHour(2)),
		rec(recentHour(2), "6,5", "m/s", 1, recentHour(1)),
		rec(recentHour(1), nil, "m/s", 1, recentHour(0)),
	}

	err := in.poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(in.store.measurements) != 3 {
		t.Fatalf("stored %d measurements, want 3", len(in.store.measurements))
	}
	if strings.Contains(f.lastQueries[0], "updated_since") {
		t.Errorf("first poll should fetch everything, query %q", f.lastQueries[0])
	}
	// The newest hour is null, so the newest valid one is two hours ago.
	newest := in.store.status().NewestMeasurement.Format(time.RFC3339)
	if newest != recentHour(2) {
		t.Errorf("newest = %s, want %s", newest, recentHour(2))
	}

	snap, _, err := loadSnapshot(in.snapshotPath)
	if err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
	if len(snap.Measurements) != 3 || snap.Site == nil {
		t.Errorf("snapshot has %d measurements, site %v", len(snap.Measurements), snap.Site)
	}

	// Next poll: a correction of the null hour and nothing else.
	f.measurements = append(f.measurements, rec(recentHour(1), 7.0, "m/s", 2, time.Now().UTC().Format(time.RFC3339)))
	err = in.poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Incremental with one hour of overlap: the newest updated_at after the
	// first poll was recentHour(0), so the query starts at recentHour(1).
	lastQuery, err := url.ParseQuery(f.lastQueries[len(f.lastQueries)-1])
	if err != nil {
		t.Fatal(err)
	}
	gotSince := lastQuery.Get("updated_since")
	if gotSince != recentHour(1) {
		t.Errorf("updated_since = %q, want %q", gotSince, recentHour(1))
	}
	newest = in.store.status().NewestMeasurement.Format(time.RFC3339)
	if newest != recentHour(1) {
		t.Errorf("after correction newest = %s, want %s", newest, recentHour(1))
	}
}

func TestPollRejectsMassInvalidation(t *testing.T) {
	f, in := newTestIngester(t)
	f.pageSize = 500
	for h := 1; h <= 30; h++ {
		f.measurements = append(f.measurements, rec(recentHour(h), 5.0, "m/s", 1, recentHour(h-1)))
	}
	err := in.poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// A broken delivery: a new version with -999 for all 30 hours.
	now := time.Now().UTC().Format(time.RFC3339)
	for h := 1; h <= 30; h++ {
		f.measurements = append(f.measurements, rec(recentHour(h), -999, "m/s", 2, now))
	}
	err = in.poll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "would turn 30 valid hours invalid") {
		t.Fatalf("err = %v, want rejection", err)
	}
	for _, m := range in.store.measurements {
		if !m.Valid {
			t.Fatalf("hour %v was invalidated despite the rejection", m.Time)
		}
	}

	// The API fixes it with version 3; the next poll takes it over.
	for h := 1; h <= 30; h++ {
		f.measurements = append(f.measurements, rec(recentHour(h), 6.0, "m/s", 3, now))
	}
	err = in.poll(context.Background())
	if err != nil {
		t.Fatalf("after the fix: %v", err)
	}
	for _, m := range in.store.measurements {
		if m.Version != 3 || m.SpeedMS != 6 {
			t.Fatalf("hour %v: version %d speed %v, want 3 and 6", m.Time, m.Version, m.SpeedMS)
		}
	}
}

func TestPollAllowsFewInvalidations(t *testing.T) {
	// Single corrections to invalid are normal and must pass.
	f, in := newTestIngester(t)
	f.measurements = []map[string]any{rec(recentHour(1), 5.0, "m/s", 1, recentHour(0))}
	err := in.poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.measurements = append(f.measurements, rec(recentHour(1), nil, "m/s", 2, time.Now().UTC().Format(time.RFC3339)))
	err = in.poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if in.store.status().NewestMeasurement.Format(time.RFC3339) == recentHour(1) {
		t.Error("the single invalidation was not applied")
	}
}

func TestPollKeepsLastValidMasterData(t *testing.T) {
	f, in := newTestIngester(t)
	err := in.poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Uncertainty given in percent instead of as a fraction.
	f.site["uncertainty"] = 10
	err = in.poll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid site") {
		t.Fatalf("err = %v, want invalid site", err)
	}
	if in.store.site.Uncertainty != 0.1 {
		t.Errorf("uncertainty = %v, want last valid 0.1", in.store.site.Uncertainty)
	}
}

func TestPollAPIDown(t *testing.T) {
	f, in := newTestIngester(t)
	f.failNext = []int{http.StatusServiceUnavailable, http.StatusServiceUnavailable}
	err := in.poll(context.Background())
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want api status 503", err)
	}
}

func TestSnapshotSurvivesRestart(t *testing.T) {
	f, in := newTestIngester(t)
	f.measurements = []map[string]any{rec(recentHour(1), 5.0, "m/s", 1, recentHour(0))}
	err := in.poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// "Restart": a fresh store filled from the snapshot only.
	snap, outdated, err := loadSnapshot(in.snapshotPath)
	if err != nil || outdated {
		t.Fatalf("err = %v, outdated = %v", err, outdated)
	}
	restarted := newStore()
	restarted.restore(snap)
	if restarted.status().NewestMeasurement.IsZero() {
		t.Error("restored store has no data")
	}
	if restarted.turbine == nil || len(restarted.turbine.PowerCurve) != 4 {
		t.Error("restored store lost the turbine")
	}
	if !restarted.maxUpdatedAt().Equal(in.store.maxUpdatedAt()) {
		t.Error("restored store would not continue incrementally")
	}
}

func TestLoadSnapshotErrors(t *testing.T) {
	dir := t.TempDir()

	_, _, err := loadSnapshot(filepath.Join(dir, "missing.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: err = %v, want os.ErrNotExist", err)
	}

	broken := filepath.Join(dir, "broken.json")
	err = os.WriteFile(broken, []byte(`{"format_version":1,"measurements":[`), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = loadSnapshot(broken)
	if err == nil {
		t.Error("broken file: expected error")
	}

	future := filepath.Join(dir, "future.json")
	err = os.WriteFile(future, []byte(`{"format_version":99,"measurements":[]}`), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = loadSnapshot(future)
	if err == nil {
		t.Error("newer format: expected error")
	}
}

func TestFullReloadReplacesOutdatedSnapshotOnlyOnSuccess(t *testing.T) {
	f, in := newTestIngester(t)

	// A format-1 snapshot: back then -1 m/s was invalid, now it counts as calm.
	calmHour := recentHour(2)
	old := snapshot{
		FormatVersion: 1,
		Measurements: []measurement{
			{Time: mustParse(t, calmHour), Valid: false, Version: 1, UpdatedAt: mustParse(t, recentHour(1))},
			{Time: mustParse(t, recentHour(1)), SpeedMS: 5, Valid: true, Version: 1, UpdatedAt: mustParse(t, recentHour(0))},
		},
	}
	err := saveSnapshot(in.snapshotPath, old)
	if err != nil {
		t.Fatal(err)
	}
	snap, outdated, err := loadSnapshot(in.snapshotPath)
	if err != nil || !outdated {
		t.Fatalf("err = %v, outdated = %v, want an outdated snapshot", err, outdated)
	}
	in.store.restore(snap)
	in.fullReload = true

	// The API is down: the old data stays visible and the old file stays on disk.
	f.failNext = []int{http.StatusServiceUnavailable, http.StatusServiceUnavailable}
	err = in.poll(context.Background())
	if err == nil {
		t.Fatal("expected the poll to fail")
	}
	if in.store.status().NewestMeasurement.IsZero() {
		t.Error("old data is gone after a failed reload")
	}
	_, outdated, _ = loadSnapshot(in.snapshotPath)
	if !outdated || !in.fullReload {
		t.Error("old snapshot was replaced although the reload failed")
	}

	// The API is back with the same records (same versions).
	f.measurements = []map[string]any{
		rec(calmHour, -1.0, "m/s", 1, recentHour(1)),
		rec(recentHour(1), 5.0, "m/s", 1, recentHour(0)),
	}
	err = in.poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.lastQueries[len(f.lastQueries)-1], "updated_since") {
		t.Error("a full reload must fetch everything")
	}
	reloaded := in.store.measurements[mustParse(t, calmHour).Unix()]
	if !reloaded.Valid || reloaded.SpeedMS != 0 {
		t.Errorf("calm hour = %+v, want valid 0 m/s under the new rule", reloaded)
	}
	_, outdated, err = loadSnapshot(in.snapshotPath)
	if err != nil || outdated || in.fullReload {
		t.Errorf("after the reload: err = %v, outdated = %v, fullReload = %v", err, outdated, in.fullReload)
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts.UTC()
}

func TestRetryDelay(t *testing.T) {
	plain := errors.New("connection refused")
	tests := []struct {
		failures int
		err      error
		want     time.Duration
	}{
		{1, plain, 15 * time.Second},
		{2, plain, 30 * time.Second},
		{3, plain, 60 * time.Second},
		{10, plain, 5 * time.Minute}, // capped at the poll interval
		{1, &apiError{Status: 429, RetryAfter: 2 * time.Minute}, 2 * time.Minute},
	}
	for _, tc := range tests {
		got := retryDelay(tc.failures, tc.err, 5*time.Minute)
		if got != tc.want {
			t.Errorf("failures=%d err=%v: got %v, want %v", tc.failures, tc.err, got, tc.want)
		}
	}
}

func TestValidateMasterData(t *testing.T) {
	validTurbine := turbine{HubHeightM: 140, PowerCurve: []powerCurvePoint{{0, 0}, {10, 2000}}}
	unsorted := turbine{HubHeightM: 140, PowerCurve: []powerCurvePoint{{10, 2000}, {5, 500}}}
	validSite := site{ShearExponent: 0.2, Losses: losses{0.03, 0.02, 0.05}, Uncertainty: 0.1}
	lossInPercent := validSite
	lossInPercent.Losses.Wake = 5

	if validateMast(mast{MeasurementHeightM: 100}) != nil {
		t.Error("valid mast rejected")
	}
	if validateMast(mast{}) == nil {
		t.Error("mast without height accepted")
	}
	if validateTurbine(validTurbine) != nil {
		t.Error("valid turbine rejected")
	}
	if validateTurbine(unsorted) == nil {
		t.Error("unsorted power curve accepted")
	}
	if validateSite(validSite) != nil {
		t.Error("valid site rejected")
	}
	if validateSite(lossInPercent) == nil {
		t.Error("loss in percent accepted")
	}
}
