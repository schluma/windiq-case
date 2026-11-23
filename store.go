package main

import (
	"reflect"
	"sort"
	"sync"
	"time"
)

// store holds all data in memory. The ingest writes, the HTTP handlers read.
type store struct {
	mu sync.RWMutex

	measurements map[int64]measurement // key: Unix seconds of the hour start
	mast         *mast
	turbine      *turbine
	site         *site

	// dirty is true while there are changes not yet written to the snapshot.
	dirty bool

	lastSuccessfulPoll time.Time
	lastError          string
}

func newStore() *store {
	return &store{measurements: map[int64]measurement{}}
}

// correction is a newer version of an hour that was already stored.
type correction struct {
	Old, New measurement
}

type upsertResult struct {
	Added       []measurement // hours that were not stored before
	Corrections []correction
}

// applied returns all measurements that were taken over.
func (r upsertResult) applied() []measurement {
	all := append([]measurement{}, r.Added...)
	for _, c := range r.Corrections {
		all = append(all, c.New)
	}
	return all
}

// upsert adds measurements. Per hour the highest version wins; with equal
// versions the later updated_at wins.
func (s *store) upsert(ms []measurement) upsertResult {
	return s.merge(ms, false)
}

// reapply is upsert for a full reload: the same version of an hour also
// replaces the stored value if it now normalizes differently, e.g. after a
// change of the normalization rules. Hours missing from ms are kept.
func (s *store) reapply(ms []measurement) upsertResult {
	return s.merge(ms, true)
}

func (s *store) merge(ms []measurement, reload bool) upsertResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	var res upsertResult
	for _, m := range ms {
		key := m.Time.Unix()
		old, exists := s.measurements[key]
		if exists && !replaces(m, old, reload) {
			continue
		}
		s.measurements[key] = m
		if exists {
			res.Corrections = append(res.Corrections, correction{Old: old, New: m})
		} else {
			res.Added = append(res.Added, m)
		}
	}
	if len(res.Added) > 0 || len(res.Corrections) > 0 {
		s.dirty = true
	}
	return res
}

// countInvalidations returns how many currently valid hours upsert(ms), or
// reapply(ms) if reload is set, would turn invalid, without changing anything.
func (s *store) countInvalidations(ms []measurement, reload bool) int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Within one batch an hour may come in several versions; only the
	// newest one would end up in the store.
	newest := map[int64]measurement{}
	for _, m := range ms {
		key := m.Time.Unix()
		prev, seen := newest[key]
		if !seen || isNewer(m, prev) {
			newest[key] = m
		}
	}

	count := 0
	for key, m := range newest {
		old, exists := s.measurements[key]
		if exists && old.Valid && !m.Valid && replaces(m, old, reload) {
			count++
		}
	}
	return count
}

// replaces reports whether m takes the place of the stored value old.
func replaces(m, old measurement, reload bool) bool {
	if isNewer(m, old) {
		return true
	}
	sameVersion := m.Version == old.Version && m.UpdatedAt.Equal(old.UpdatedAt)
	changedValue := m.Valid != old.Valid || m.SpeedMS != old.SpeedMS
	return reload && sameVersion && changedValue
}

func isNewer(m, old measurement) bool {
	if m.Version != old.Version {
		return m.Version > old.Version
	}
	return m.UpdatedAt.After(old.UpdatedAt)
}

// prune removes measurements older than cutoff and returns how many.
func (s *store) prune(cutoff time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for key, m := range s.measurements {
		if m.Time.Before(cutoff) {
			delete(s.measurements, key)
			removed++
		}
	}
	if removed > 0 {
		s.dirty = true
	}
	return removed
}

// maxUpdatedAt is the latest updated_at of all measurements; the next poll
// asks the API for everything changed since then.
func (s *store) maxUpdatedAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var latest time.Time
	for _, m := range s.measurements {
		if m.UpdatedAt.After(latest) {
			latest = m.UpdatedAt
		}
	}
	return latest
}

// setMasterData stores the given master data. nil keeps the current value.
// It returns true if anything changed.
func (s *store) setMasterData(m *mast, t *turbine, st *site) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	changed := false
	if m != nil && !reflect.DeepEqual(s.mast, m) {
		s.mast = m
		changed = true
	}
	if t != nil && !reflect.DeepEqual(s.turbine, t) {
		s.turbine = t
		changed = true
	}
	if st != nil && !reflect.DeepEqual(s.site, st) {
		s.site = st
		changed = true
	}
	if changed {
		s.dirty = true
	}
	return changed
}

// recordPoll remembers the outcome of a poll for /health.
func (s *store) recordPoll(err error, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err != nil {
		s.lastError = err.Error()
		return
	}
	s.lastSuccessfulPoll = now
	s.lastError = ""
}

type ingestStatus struct {
	NewestMeasurement  time.Time // newest valid measurement, zero if none
	LastSuccessfulPoll time.Time
	LastError          string
}

func (s *store) status() ingestStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st := ingestStatus{LastSuccessfulPoll: s.lastSuccessfulPoll, LastError: s.lastError}
	for _, m := range s.measurements {
		if m.Valid && m.Time.After(st.NewestMeasurement) {
			st.NewestMeasurement = m.Time
		}
	}
	return st
}

func (s *store) isDirty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dirty
}

func (s *store) clearDirty() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirty = false
}

// snapshot returns a copy of all data, measurements sorted by time.
func (s *store) snapshot(now time.Time) snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ms := make([]measurement, 0, len(s.measurements))
	for _, m := range s.measurements {
		ms = append(ms, m)
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].Time.Before(ms[j].Time) })

	return snapshot{
		FormatVersion: snapshotFormatVersion,
		SavedAt:       now.UTC(),
		Measurements:  ms,
		Mast:          s.mast,
		Turbine:       s.turbine,
		Site:          s.site,
	}
}

// restore replaces all data with the content of a snapshot.
func (s *store) restore(snap snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.measurements = make(map[int64]measurement, len(snap.Measurements))
	for _, m := range snap.Measurements {
		s.measurements[m.Time.Unix()] = m
	}
	s.mast = snap.Mast
	s.turbine = snap.Turbine
	s.site = snap.Site
	s.dirty = false
}
