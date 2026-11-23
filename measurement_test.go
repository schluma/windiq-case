package main

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name       string
		timestamp  string
		speed      string // raw JSON
		unit       string
		wantReason string // kind of invalidReason, empty for a valid value
		wantSpeed  float64
		wantTime   time.Time
	}{
		{"number", "2026-10-20T08:00:00Z", `6.42`, "m/s", "", 6.42, time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)},
		{"string", "2026-10-20T08:00:00Z", `"6.42"`, "m/s", "", 6.42, time.Time{}},
		{"string with decimal comma", "2026-10-20T08:00:00Z", `"6,42"`, "m/s", "", 6.42, time.Time{}},
		{"unit with other case and spaces", "2026-10-20T08:00:00Z", `6.42`, " M/S ", "", 6.42, time.Time{}},
		{"km/h", "2026-10-20T08:00:00Z", `36`, "km/h", "", 10, time.Time{}},
		{"knots", "2026-10-20T08:00:00Z", `10`, "kn", "", 5.14444, time.Time{}},
		{"offset is converted to UTC", "2026-10-20T10:00:00+02:00", `5`, "m/s", "", 5, time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)},
		{"null", "2026-10-20T08:00:00Z", `null`, "m/s", reasonMissing, 0, time.Time{}},
		{"empty string", "2026-10-20T08:00:00Z", `""`, "m/s", reasonMissing, 0, time.Time{}},
		{"placeholder -999", "2026-10-20T08:00:00Z", `-999`, "m/s", reasonPlaceholder, 0, time.Time{}},
		{"placeholder as string", "2026-10-20T08:00:00Z", `"-9999"`, "m/s", reasonPlaceholder, 0, time.Time{}},
		{"text", "2026-10-20T08:00:00Z", `"n/a"`, "m/s", reasonNotANumber, 0, time.Time{}},
		{"NaN as text", "2026-10-20T08:00:00Z", `"NaN"`, "m/s", reasonNotANumber, 0, time.Time{}},
		{"unknown unit", "2026-10-20T08:00:00Z", `5`, "furlong/fortnight", reasonUnknownUnit, 0, time.Time{}},
		{"slightly negative is calm", "2026-10-20T08:00:00Z", `-1.22`, "m/s", "", 0, time.Time{}},
		{"negative seen in the data", "2026-10-20T08:00:00Z", `-2.89`, "m/s", "", 0, time.Time{}},
		{"calm limit", "2026-10-20T08:00:00Z", `-3`, "m/s", "", 0, time.Time{}},
		{"slightly negative in km/h", "2026-10-20T08:00:00Z", `-3.6`, "km/h", "", 0, time.Time{}},
		{"clearly negative", "2026-10-20T08:00:00Z", `-3.5`, "m/s", reasonOutOfRange, 0, time.Time{}},
		{"storm still plausible", "2026-10-20T08:00:00Z", `39.9`, "m/s", "", 39.9, time.Time{}},
		{"upper limit", "2026-10-20T08:00:00Z", `40`, "m/s", "", 40, time.Time{}},
		{"spike seen in the data", "2026-10-20T08:00:00Z", `64.31`, "m/s", reasonOutOfRange, 0, time.Time{}},
		{"spike in km/h", "2026-10-20T08:00:00Z", `150`, "km/h", reasonOutOfRange, 0, time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := rawMeasurement{
				Timestamp: tc.timestamp,
				WindSpeed: json.RawMessage(tc.speed),
				Unit:      tc.unit,
				Version:   1,
				UpdatedAt: "2026-10-20T09:05:00Z",
			}
			m, reason, err := normalize(raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			gotReason := ""
			if reason != nil {
				gotReason = reason.Kind
			}
			if gotReason != tc.wantReason {
				t.Errorf("reason = %q (%v), want %q", gotReason, reason, tc.wantReason)
			}
			if m.Valid != (tc.wantReason == "") {
				t.Errorf("valid = %v, but reason = %q", m.Valid, gotReason)
			}
			if m.Valid && math.Abs(m.SpeedMS-tc.wantSpeed) > 1e-4 {
				t.Errorf("speed = %v, want %v", m.SpeedMS, tc.wantSpeed)
			}
			if !tc.wantTime.IsZero() && !m.Time.Equal(tc.wantTime) {
				t.Errorf("time = %v, want %v", m.Time, tc.wantTime)
			}
			if m.Time.Location() != time.UTC {
				t.Errorf("time not in UTC: %v", m.Time)
			}
		})
	}
}

func TestNormalizeRejectsBadTimestamp(t *testing.T) {
	for _, ts := range []string{"", "2026-10-20 08:00", "2026-10-20T08:00:00"} {
		raw := rawMeasurement{Timestamp: ts, WindSpeed: json.RawMessage(`5`), Unit: "m/s", UpdatedAt: "2026-10-20T09:00:00Z"}
		_, _, err := normalize(raw)
		if err == nil {
			t.Errorf("timestamp %q: expected error", ts)
		}
	}
}
