package main

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// gapMode says how hours without a valid measurement are handled.
type gapMode string

const (
	// gapIgnore sums up only the hours with a valid measurement.
	gapIgnore gapMode = "ignore"
	// gapScale fills the missing hours of each month with that month's
	// mean energy per valid hour (see minValidHoursPerMonth).
	gapScale gapMode = "scale"
)

// z90 is the one-sided 90 % quantile of the standard normal distribution.
const z90 = 1.282

// minValidHoursPerMonth: a month needs at least one week of valid hours for
// its own mean to fill its gaps. Below that, a few hours would decide the
// whole month, so the mean over the whole window is used instead.
const minValidHoursPerMonth = 7 * 24

// result is the yield calculation for the 12 months up to the newest valid
// measurement.
type result struct {
	Mode gapMode
	// The window contains the hours with From < start of hour <= To.
	From, To       time.Time
	ExpectedHours  int
	ValidHours     int
	MeanHubSpeedMS float64
	ShearFactor    float64 // (hub height / measurement height) ^ alpha
	Losses         losses  // as fractions
	LossFactor     float64 // (1 - availability) (1 - electrical) (1 - wake)
	Uncertainty    float64 // U, as a fraction
	GrossMWh       float64 // energy according to the power curve
	P50MWh         float64 // = net AEP
	P90MWh         float64
	TurbineVersion int
	SiteVersion    int
	Months         []monthStat // in time order
	Days           []dayStat   // in time order
}

type monthStat struct {
	Month         time.Time // first day of the month, UTC
	ExpectedHours int       // hours of this month inside the window
	ValidHours    int
	MeasuredMWh   float64 // gross energy of the valid hours
	GrossMWh      float64 // MeasuredMWh plus the filled gaps in mode scale
}

type dayStat struct {
	Day            time.Time // UTC
	ExpectedHours  int
	ValidHours     int
	MeanHubSpeedMS float64 // only meaningful if ValidHours > 0
	hubSpeedSum    float64
}

// CompletenessPct is the share of valid hours in the window in percent.
func (r result) CompletenessPct() float64 {
	return 100 * float64(r.ValidHours) / float64(r.ExpectedHours)
}

// CompletenessPct is the share of valid hours in the month in percent.
func (m monthStat) CompletenessPct() float64 {
	return 100 * float64(m.ValidHours) / float64(m.ExpectedHours)
}

// Partial reports whether only part of the month lies inside the window,
// which is the case for the first and the last month.
func (m monthStat) Partial() bool {
	hoursInMonth := int(m.Month.AddDate(0, 1, 0).Sub(m.Month).Hours())
	return m.ExpectedHours < hoursInMonth
}

// calculate computes the yield.
//
//	v_hub     = v_mast * (h_hub / h_mast) ^ alpha
//	AEP gross = sum over the hours of power(v_hub) * 1 h
//	AEP net   = AEP gross * (1 - availability) * (1 - electrical) * (1 - wake)
//	P50       = AEP net
//	P90       = P50 * (1 - 1.282 * U)
func calculate(ms []measurement, m mast, t turbine, s site, mode gapMode) (result, error) {
	if mode != gapIgnore && mode != gapScale {
		return result{}, fmt.Errorf("unknown gap mode %q", mode)
	}

	var newest time.Time
	for _, x := range ms {
		if x.Valid && x.Time.After(newest) {
			newest = x.Time
		}
	}
	if newest.IsZero() {
		return result{}, errors.New("no valid measurements")
	}
	from := newest.AddDate(-1, 0, 0)

	r := result{
		Mode:           mode,
		From:           from,
		To:             newest,
		ShearFactor:    math.Pow(t.HubHeightM/m.MeasurementHeightM, s.ShearExponent),
		Losses:         s.Losses,
		LossFactor:     (1 - s.Losses.Availability) * (1 - s.Losses.Electrical) * (1 - s.Losses.Wake),
		Uncertainty:    s.Uncertainty,
		TurbineVersion: t.Version,
		SiteVersion:    s.Version,
	}

	months := map[time.Time]*monthStat{}
	days := map[time.Time]*dayStat{}
	monthOf := func(ts time.Time) *monthStat {
		key := time.Date(ts.Year(), ts.Month(), 1, 0, 0, 0, 0, time.UTC)
		if months[key] == nil {
			months[key] = &monthStat{Month: key}
		}
		return months[key]
	}
	dayOf := func(ts time.Time) *dayStat {
		key := time.Date(ts.Year(), ts.Month(), ts.Day(), 0, 0, 0, 0, time.UTC)
		if days[key] == nil {
			days[key] = &dayStat{Day: key}
		}
		return days[key]
	}

	// Every hour of the window counts as expected.
	for h := from.Add(time.Hour); !h.After(newest); h = h.Add(time.Hour) {
		monthOf(h).ExpectedHours++
		dayOf(h).ExpectedHours++
		r.ExpectedHours++
	}

	var hubSpeedSum, measuredMWh float64
	for _, x := range ms {
		if !x.Valid || !x.Time.After(from) || x.Time.After(newest) {
			continue
		}
		hubSpeed := x.SpeedMS * r.ShearFactor
		mwh := powerKW(t.PowerCurve, hubSpeed) / 1000 // one hour at this power

		mo := monthOf(x.Time)
		mo.ValidHours++
		mo.MeasuredMWh += mwh
		d := dayOf(x.Time)
		d.ValidHours++
		d.hubSpeedSum += hubSpeed

		hubSpeedSum += hubSpeed
		measuredMWh += mwh
		r.ValidHours++
	}
	r.MeanHubSpeedMS = hubSpeedSum / float64(r.ValidHours)
	overallPerHour := measuredMWh / float64(r.ValidHours)

	for _, mo := range months {
		mo.GrossMWh = mo.MeasuredMWh
		missing := mo.ExpectedHours - mo.ValidHours
		if mode == gapScale && missing > 0 {
			perHour := overallPerHour
			if mo.ValidHours >= minValidHoursPerMonth {
				perHour = mo.MeasuredMWh / float64(mo.ValidHours)
			}
			mo.GrossMWh += float64(missing) * perHour
		}
		r.GrossMWh += mo.GrossMWh
		r.Months = append(r.Months, *mo)
	}
	sort.Slice(r.Months, func(i, j int) bool { return r.Months[i].Month.Before(r.Months[j].Month) })

	for _, d := range days {
		if d.ValidHours > 0 {
			d.MeanHubSpeedMS = d.hubSpeedSum / float64(d.ValidHours)
		}
		r.Days = append(r.Days, *d)
	}
	sort.Slice(r.Days, func(i, j int) bool { return r.Days[i].Day.Before(r.Days[j].Day) })

	r.P50MWh = r.GrossMWh * r.LossFactor
	r.P90MWh = r.P50MWh * (1 - z90*s.Uncertainty)
	return r, nil
}

// powerKW interpolates the power curve linearly. Below the first and above
// the last point the turbine produces nothing; the last point is the
// cut-out speed.
func powerKW(curve []powerCurvePoint, v float64) float64 {
	if len(curve) == 0 || v < curve[0].WindSpeedMS || v > curve[len(curve)-1].WindSpeedMS {
		return 0
	}
	for i := 1; i < len(curve); i++ {
		if v <= curve[i].WindSpeedMS {
			a, b := curve[i-1], curve[i]
			return a.PowerKW + (v-a.WindSpeedMS)/(b.WindSpeedMS-a.WindSpeedMS)*(b.PowerKW-a.PowerKW)
		}
	}
	return 0
}
