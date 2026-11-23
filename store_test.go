package main

import (
	"testing"
	"time"
)

var hour0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func meas(hour int, speed float64, valid bool, version int, updatedMinute int) measurement {
	t := hour0.Add(time.Duration(hour) * time.Hour)
	return measurement{
		Time:      t,
		SpeedMS:   speed,
		Valid:     valid,
		Version:   version,
		UpdatedAt: t.Add(time.Hour + time.Duration(updatedMinute)*time.Minute),
	}
}

func TestUpsertKeepsNewestVersion(t *testing.T) {
	s := newStore()
	steps := []struct {
		name            string
		m               measurement
		wantAdded       int
		wantCorrections int
		wantSpeed       float64
	}{
		{"first value", meas(0, 5, true, 1, 0), 1, 0, 5},
		{"same again", meas(0, 5, true, 1, 0), 0, 0, 5},
		{"higher version", meas(0, 6, true, 2, 10), 0, 1, 6},
		{"older version arrives late", meas(0, 5, true, 1, 0), 0, 0, 6},
		{"same version, later update", meas(0, 7, true, 2, 20), 0, 1, 7},
	}
	for _, step := range steps {
		res := s.upsert([]measurement{step.m})
		if len(res.Added) != step.wantAdded || len(res.Corrections) != step.wantCorrections {
			t.Errorf("%s: added = %d, corrections = %d, want %d and %d",
				step.name, len(res.Added), len(res.Corrections), step.wantAdded, step.wantCorrections)
		}
		got := s.measurements[hour0.Unix()].SpeedMS
		if got != step.wantSpeed {
			t.Errorf("%s: speed = %v, want %v", step.name, got, step.wantSpeed)
		}
	}
}

func TestStatusNewestIgnoresInvalid(t *testing.T) {
	s := newStore()
	s.upsert([]measurement{meas(0, 5, true, 1, 0), meas(1, 0, false, 1, 0)})
	got := s.status().NewestMeasurement
	if !got.Equal(hour0) {
		t.Errorf("newest = %v, want %v", got, hour0)
	}
}

func TestCorrectionCanInvalidate(t *testing.T) {
	// A newer version that is broken replaces the old valid value.
	s := newStore()
	s.upsert([]measurement{meas(0, 5, true, 1, 0)})
	res := s.upsert([]measurement{meas(0, 0, false, 2, 10)})
	if s.measurements[hour0.Unix()].Valid {
		t.Error("expected the hour to be invalid after the correction")
	}
	if len(res.Corrections) != 1 || !res.Corrections[0].Old.Valid || res.Corrections[0].New.Valid {
		t.Errorf("corrections = %+v, want one valid -> invalid", res.Corrections)
	}
}

func TestCountInvalidations(t *testing.T) {
	s := newStore()
	s.upsert([]measurement{meas(0, 5, true, 1, 0), meas(1, 5, true, 1, 0), meas(2, 0, false, 1, 0)})

	batch := []measurement{
		meas(0, 0, false, 2, 10), // valid -> invalid: counts
		meas(1, 0, false, 2, 10), // invalid v2 ...
		meas(1, 6, true, 3, 20),  // ... but v3 in the same batch is valid: does not count
		meas(2, 0, false, 2, 10), // was already invalid: does not count
		meas(3, 0, false, 1, 0),  // new hour: does not count
	}
	got := s.countInvalidations(batch, false)
	if got != 1 {
		t.Errorf("countInvalidations = %d, want 1", got)
	}
	if !s.measurements[hour0.Unix()].Valid {
		t.Error("countInvalidations must not change the store")
	}
}

func TestResultIsCachedAndRefreshed(t *testing.T) {
	s := newStore()
	_, err := s.result(gapIgnore)
	if err == nil {
		t.Fatal("expected an error without master data")
	}

	s.setMasterData(&mast{MeasurementHeightM: 100}, &turbine{HubHeightM: 100, PowerCurve: testCurve}, &site{})
	s.upsert([]measurement{meas(0, 5, true, 1, 0)})
	first, err := s.result(gapIgnore)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "gross after first value", first.GrossMWh, 1) // 5 m/s = 1000 kW

	generation := s.resultsGeneration
	_, _ = s.result(gapIgnore)
	if s.resultsGeneration != generation {
		t.Error("result was recomputed without a data change")
	}

	s.upsert([]measurement{meas(1, 10, true, 1, 0)})
	second, err := s.result(gapIgnore)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "gross after second value", second.GrossMWh, 3) // + 2000 kW

	_, err = s.result("guess")
	if err == nil {
		t.Error("unknown mode: expected error")
	}
}

func TestPrune(t *testing.T) {
	s := newStore()
	s.upsert([]measurement{meas(0, 5, true, 1, 0), meas(1, 5, true, 1, 0), meas(2, 5, true, 1, 0)})
	s.clearDirty()

	removed := s.prune(hour0.Add(2 * time.Hour))
	if removed != 2 || len(s.measurements) != 1 {
		t.Errorf("removed = %d, left = %d, want 2 and 1", removed, len(s.measurements))
	}
	if !s.isDirty() {
		t.Error("prune should mark the store dirty")
	}
}

func TestMaxUpdatedAt(t *testing.T) {
	s := newStore()
	if !s.maxUpdatedAt().IsZero() {
		t.Error("empty store should return zero")
	}
	a := meas(0, 5, true, 2, 30) // updated 01:30
	b := meas(1, 5, true, 1, 0)  // updated 02:00
	s.upsert([]measurement{a, b})
	if !s.maxUpdatedAt().Equal(b.UpdatedAt) {
		t.Errorf("maxUpdatedAt = %v, want %v", s.maxUpdatedAt(), b.UpdatedAt)
	}
}
