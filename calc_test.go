package main

import (
	"math"
	"testing"
	"time"
)

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6*math.Max(1, math.Abs(want)) {
		t.Errorf("%s = %.9f, want %.9f", name, got, want)
	}
}

// Simple test turbine: linear from 0 kW at 0 m/s to 2000 kW at 10 m/s,
// then flat up to the cut-out at 20 m/s.
var testCurve = []powerCurvePoint{{0, 0}, {10, 2000}, {20, 2000}}

func TestPowerKW(t *testing.T) {
	cutIn := []powerCurvePoint{{3, 0}, {4, 100}, {25, 3000}}
	tests := []struct {
		name  string
		curve []powerCurvePoint
		v     float64
		want  float64
	}{
		{"on a point", testCurve, 10, 2000},
		{"between points", testCurve, 5, 1000},
		{"flat part", testCurve, 15, 2000},
		{"at cut-out", testCurve, 20, 2000},
		{"above cut-out", testCurve, 20.01, 0},
		{"calm", testCurve, 0, 0},
		{"below cut-in", cutIn, 2.9, 0},
		{"between cut-in and next point", cutIn, 3.5, 50},
	}
	for _, tc := range tests {
		near(t, tc.name, powerKW(tc.curve, tc.v), tc.want)
	}
}

func TestCalculateByHand(t *testing.T) {
	newest := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	ms := []measurement{
		{Time: newest.Add(-2 * time.Hour), SpeedMS: 5, Valid: true}, // 1000 kW
		{Time: newest.Add(-1 * time.Hour), Valid: false},            // gap
		{Time: newest, SpeedMS: 10, Valid: true},                    // 2000 kW
		{Time: newest.AddDate(-1, 0, 0), SpeedMS: 10, Valid: true},  // exactly one year back: outside
		{Time: newest.AddDate(-2, 0, 0), SpeedMS: 10, Valid: true},  // far outside
	}
	m := mast{MeasurementHeightM: 100}
	tb := turbine{HubHeightM: 100, PowerCurve: testCurve, Version: 7} // same height: no shear
	s := site{ShearExponent: 0.2, Losses: losses{0.03, 0.02, 0.05}, Uncertainty: 0.1, Version: 9}

	r, err := calculate(ms, m, tb, s, gapIgnore)
	if err != nil {
		t.Fatal(err)
	}

	// Window: 2025-11-23 10:00 (exclusive) to 2026-11-23 10:00 = 365 days.
	if r.ExpectedHours != 8760 || r.ValidHours != 2 {
		t.Errorf("hours = %d of %d, want 2 of 8760", r.ValidHours, r.ExpectedHours)
	}
	near(t, "shear factor", r.ShearFactor, 1)
	near(t, "mean hub speed", r.MeanHubSpeedMS, 7.5)
	// 1000 kWh + 2000 kWh
	near(t, "gross", r.GrossMWh, 3)
	// 0.97 * 0.98 * 0.95 = 0.90307
	near(t, "loss factor", r.LossFactor, 0.90307)
	// 3 * 0.90307
	near(t, "P50", r.P50MWh, 2.70921)
	// 2.70921 * (1 - 1.282 * 0.1) = 2.70921 * 0.8718
	near(t, "P90", r.P90MWh, 2.361889278)
	if r.TurbineVersion != 7 || r.SiteVersion != 9 {
		t.Errorf("versions = %d/%d, want 7/9", r.TurbineVersion, r.SiteVersion)
	}

	// Scaling: 3 MWh from 2 hours, extrapolated to 8760 hours.
	scaled, err := calculate(ms, m, tb, s, gapScale)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "scaled gross", scaled.GrossMWh, 3*8760.0/2)
	near(t, "scaled P50", scaled.P50MWh, 3*8760.0/2*0.90307)
}

func TestCalculateShear(t *testing.T) {
	newest := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	ms := []measurement{{Time: newest, SpeedMS: 5, Valid: true}}
	m := mast{MeasurementHeightM: 100}
	tb := turbine{HubHeightM: 140, PowerCurve: testCurve}
	s := site{ShearExponent: 0.2}

	r, err := calculate(ms, m, tb, s, gapIgnore)
	if err != nil {
		t.Fatal(err)
	}
	// 1.4 ^ 0.2 = e ^ (0.2 * ln 1.4) = e ^ 0.0672944 = 1.0696104; 5 m/s at the
	// mast become 5.348052 m/s at the hub, which is 1069.6104 kW on the linear
	// part of the curve (200 kW per m/s), so 1.0696104 MWh in one hour.
	if math.Abs(r.ShearFactor-1.0696104) > 1e-6 {
		t.Errorf("shear factor = %v, want about 1.0696104", r.ShearFactor)
	}
	if math.Abs(r.GrossMWh-1.0696104) > 1e-6 {
		t.Errorf("gross = %v MWh, want about 1.0696104", r.GrossMWh)
	}
}

// fullYear returns one valid measurement per hour of the window ending at
// newest: speedIn(t) gives the speed, a negative value leaves the hour out.
func fullYear(newest time.Time, speedIn func(time.Time) float64) []measurement {
	var ms []measurement
	for h := newest.AddDate(-1, 0, 0).Add(time.Hour); !h.After(newest); h = h.Add(time.Hour) {
		v := speedIn(h)
		if v >= 0 {
			ms = append(ms, measurement{Time: h, SpeedMS: v, Valid: true, Version: 1})
		}
	}
	return ms
}

func TestScalePerMonth(t *testing.T) {
	// Without shear and losses; 5 m/s = 1 MWh per hour, 10 m/s = 2 MWh.
	newest := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	march := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	gapStart := march.Add(10 * 24 * time.Hour)
	// March is windy (10 m/s), all other months 5 m/s. 100 hours of March are missing.
	ms := fullYear(newest, func(h time.Time) float64 {
		if h.Month() != time.March {
			return 5
		}
		if !h.Before(gapStart) && h.Before(gapStart.Add(100*time.Hour)) {
			return -1
		}
		return 10
	})
	m := mast{MeasurementHeightM: 100}
	tb := turbine{HubHeightM: 100, PowerCurve: testCurve}

	ignored, err := calculate(ms, m, tb, site{}, gapIgnore)
	if err != nil {
		t.Fatal(err)
	}
	// 8016 hours outside March at 1 MWh, 644 hours of March at 2 MWh.
	near(t, "ignore", ignored.GrossMWh, 8016+644*2)

	scaled, err := calculate(ms, m, tb, site{}, gapScale)
	if err != nil {
		t.Fatal(err)
	}
	// The missing March hours are filled with March's own 2 MWh per hour,
	// not with the yearly mean (global scaling would give 9304 * 8760 / 8660).
	near(t, "scale", scaled.GrossMWh, 8016+744*2)

	if len(scaled.Months) != 13 {
		t.Fatalf("months = %d, want 13 (partial Nov 2025 and Nov 2026)", len(scaled.Months))
	}
	first, last := scaled.Months[0], scaled.Months[len(scaled.Months)-1]
	if !first.Partial() || !last.Partial() {
		t.Errorf("first and last month should be partial: %v, %v", first.Partial(), last.Partial())
	}
	sum := 0
	for i, mo := range scaled.Months {
		if i > 0 && i < len(scaled.Months)-1 && mo.Partial() {
			t.Errorf("%v marked partial", mo.Month)
		}
		sum += mo.ExpectedHours
		if mo.Month.Equal(march) {
			if mo.ExpectedHours != 744 || mo.ValidHours != 644 {
				t.Errorf("March: %d of %d hours, want 644 of 744", mo.ValidHours, mo.ExpectedHours)
			}
			near(t, "March measured", mo.MeasuredMWh, 1288)
			near(t, "March scaled", mo.GrossMWh, 1488)
		}
	}
	if sum != 8760 {
		t.Errorf("expected hours over all months = %d, want 8760", sum)
	}
}

func TestScaleFallsBackToOverallMean(t *testing.T) {
	// April has only 10 valid hours: too few to stand for the month.
	newest := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	april := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	ms := fullYear(newest, func(h time.Time) float64 {
		if h.Month() != time.April {
			return 5
		}
		if h.Before(april.Add(10 * time.Hour)) {
			return 10
		}
		return -1
	})

	r, err := calculate(ms, mast{MeasurementHeightM: 100}, turbine{HubHeightM: 100, PowerCurve: testCurve}, site{}, gapScale)
	if err != nil {
		t.Fatal(err)
	}
	// Valid: 8040 hours at 1 MWh outside April and 10 at 2 MWh = 8060 MWh in 8050 hours.
	// April's 710 missing hours are filled with that overall mean.
	overall := 8060.0 / 8050.0
	near(t, "gross", r.GrossMWh, 8040+20+710*overall)
}

func TestDailyMeans(t *testing.T) {
	newest := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	ms := []measurement{
		{Time: newest.Add(-2 * time.Hour), SpeedMS: 4, Valid: true},
		{Time: newest.Add(-1 * time.Hour), SpeedMS: 6, Valid: true},
	}
	r, err := calculate(ms, mast{MeasurementHeightM: 100}, turbine{HubHeightM: 100, PowerCurve: testCurve}, site{}, gapIgnore)
	if err != nil {
		t.Fatal(err)
	}
	// The window touches 366 calendar days (partial first and last day).
	if len(r.Days) != 366 {
		t.Fatalf("days = %d, want 366", len(r.Days))
	}
	last := r.Days[len(r.Days)-1]
	// The window ends at the newest valid value, 09:00: the last day has the
	// hours 00:00 to 09:00.
	if !last.Day.Equal(time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)) || last.ExpectedHours != 10 || last.ValidHours != 2 {
		t.Errorf("last day = %v with %d of %d hours, want 2026-11-23 with 2 of 10", last.Day, last.ValidHours, last.ExpectedHours)
	}
	near(t, "last day mean", last.MeanHubSpeedMS, 5)
}

func TestFormatThousands(t *testing.T) {
	tests := map[float64]string{0: "0", 999.4: "999", 1000: "1.000", 12345.6: "12.346", 1234567: "1.234.567", -1500: "-1.500"}
	for v, want := range tests {
		got := formatThousands(v)
		if got != want {
			t.Errorf("formatThousands(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestNiceStep(t *testing.T) {
	tests := map[float64]float64{1000: 250, 950: 250, 1200: 500, 8: 2, 40: 10}
	for max, want := range tests {
		got := niceStep(max)
		if got != want {
			t.Errorf("niceStep(%v) = %v, want %v", max, got, want)
		}
	}
}

func TestCalculateLeapYearWindow(t *testing.T) {
	// The window 2028-03-01 back to 2027-03-01 contains 29 Feb 2028.
	newest := time.Date(2028, 3, 1, 0, 0, 0, 0, time.UTC)
	ms := []measurement{{Time: newest, SpeedMS: 5, Valid: true}}
	r, err := calculate(ms, mast{MeasurementHeightM: 100}, turbine{HubHeightM: 100, PowerCurve: testCurve}, site{}, gapIgnore)
	if err != nil {
		t.Fatal(err)
	}
	if r.ExpectedHours != 8784 {
		t.Errorf("expected hours = %d, want 8784", r.ExpectedHours)
	}
}

func TestCalculateErrors(t *testing.T) {
	m := mast{MeasurementHeightM: 100}
	tb := turbine{HubHeightM: 140, PowerCurve: testCurve}
	invalidOnly := []measurement{{Time: time.Now(), Valid: false}}

	_, err := calculate(invalidOnly, m, tb, site{}, gapIgnore)
	if err == nil {
		t.Error("no valid measurements: expected error")
	}
	_, err = calculate([]measurement{{Time: time.Now(), SpeedMS: 5, Valid: true}}, m, tb, site{}, "guess")
	if err == nil {
		t.Error("unknown mode: expected error")
	}
}
