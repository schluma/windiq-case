package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// measurement is one normalized hourly value.
type measurement struct {
	Time      time.Time `json:"time"`     // start of the hour, UTC
	SpeedMS   float64   `json:"speed_ms"` // wind speed at measurement height in m/s, only meaningful if Valid
	Valid     bool      `json:"valid"`
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

// maxPlausibleSpeedMS: larger values are treated as faulty. Hourly means
// above about 40 m/s at 100 m are not realistic for an inland site; gusts
// are higher, but we get hourly means. In the API data (Nov 2026) all
// regular values lie below 25 m/s, then there is a gap up to 55 m/s and
// 13 isolated spikes between 55 and 83 m/s. 40 m/s lies in that gap.
const maxPlausibleSpeedMS = 40.0

// calmToleranceMS: slightly negative values come from the sensor's zero
// offset in calm conditions. Down to -calmToleranceMS they count as 0 m/s.
// In the API data (Nov 2026) the negative values spread evenly from 0 to
// -2.89 m/s, i.e. one phenomenon; 3 m/s covers all of them.
const calmToleranceMS = 3.0

// placeholders are values that data loggers commonly write instead of a
// missing measurement.
var placeholders = []float64{-999, -9999, 9999}

// Kinds of invalid wind speeds. They group similar reasons in the log.
const (
	reasonMissing     = "missing"
	reasonPlaceholder = "placeholder"
	reasonNotANumber  = "not a number"
	reasonUnknownUnit = "unknown unit"
	reasonOutOfRange  = "out of range"
)

// invalidReason explains why a wind speed is unusable.
type invalidReason struct {
	Kind   string // one of the reason constants
	Detail string // the concrete value, for the log
}

// normalize converts a raw API record.
//
// An error means the record cannot be placed in time and has to be skipped.
// An unusable wind speed is not an error: the record is returned with
// Valid=false and a reason. That way a newer version that is broken still
// replaces an older valid value instead of being silently ignored.
func normalize(r rawMeasurement) (measurement, *invalidReason, error) {
	t, err := time.Parse(time.RFC3339, r.Timestamp)
	if err != nil {
		return measurement{}, nil, fmt.Errorf("timestamp %q: %w", r.Timestamp, err)
	}
	updated, err := time.Parse(time.RFC3339, r.UpdatedAt)
	if err != nil {
		return measurement{}, nil, fmt.Errorf("updated_at %q: %w", r.UpdatedAt, err)
	}

	m := measurement{Time: t.UTC(), Version: r.Version, UpdatedAt: updated.UTC()}
	speed, reason := parseSpeed(r.WindSpeed, r.Unit)
	if reason != nil {
		return m, reason, nil
	}
	m.SpeedMS = speed
	m.Valid = true
	return m, nil, nil
}

// parseSpeed reads a wind speed given as number or string and converts it
// to m/s.
func parseSpeed(raw json.RawMessage, unit string) (float64, *invalidReason) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return 0, &invalidReason{reasonMissing, "null"}
	}
	if strings.HasPrefix(text, `"`) {
		var s string
		err := json.Unmarshal(raw, &s)
		if err != nil {
			return 0, &invalidReason{reasonNotANumber, text}
		}
		// Accept a decimal comma, e.g. "6,42".
		text = strings.ReplaceAll(strings.TrimSpace(s), ",", ".")
		if text == "" {
			return 0, &invalidReason{reasonMissing, `""`}
		}
	}

	v, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, &invalidReason{reasonNotANumber, strconv.Quote(text)}
	}
	for _, p := range placeholders {
		if v == p {
			return 0, &invalidReason{reasonPlaceholder, text}
		}
	}

	factor, ok := unitToMS(unit)
	if !ok {
		return 0, &invalidReason{reasonUnknownUnit, strconv.Quote(unit)}
	}
	v *= factor
	if v < 0 && v >= -calmToleranceMS {
		return 0, nil
	}
	if v < 0 || v > maxPlausibleSpeedMS {
		return 0, &invalidReason{reasonOutOfRange, fmt.Sprintf("%.2f m/s", v)}
	}
	return v, nil
}

// unitToMS returns the factor that converts the given unit to m/s.
// Unknown units are rejected instead of guessed.
func unitToMS(unit string) (float64, bool) {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "m/s":
		return 1, true
	case "km/h":
		return 1 / 3.6, true
	case "kn", "kt", "knots":
		return 0.514444, true
	case "mph":
		return 0.44704, true
	}
	return 0, false
}
