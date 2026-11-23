package main

import (
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"
)

// The charts are rendered on the server as inline SVG: no JavaScript, no
// chart library, no CDN. Colors come from CSS classes in the page, so they
// follow light and dark mode. All text in the SVG is built from numbers and
// fixed labels, never from outside input.
//
// All charts share one time axis (timeAxis): x is the day within the window,
// and a month's bar covers exactly its days. The charts therefore line up
// month by month when they have the same width and margins.

var monthNames = []string{"Jan", "Feb", "Mär", "Apr", "Mai", "Jun", "Jul", "Aug", "Sep", "Okt", "Nov", "Dez"}

func monthLabel(t time.Time) string {
	return fmt.Sprintf("%s %02d", monthNames[t.Month()-1], t.Year()%100)
}

// Shared horizontal layout, so that charts stacked on the page line up.
const (
	chartWidth = 720.0
	chartLeft  = 56.0
	chartRight = 12.0
)

// chartFrame holds the plot area of a chart and one y scale.
type chartFrame struct {
	width, height            float64
	left, right, top, bottom float64 // margins
	yMax                     float64
}

func newFrame(height, bottom float64) chartFrame {
	return chartFrame{width: chartWidth, height: height, left: chartLeft, right: chartRight, top: 26, bottom: bottom}
}

func (f chartFrame) plotWidth() float64  { return f.width - f.left - f.right }
func (f chartFrame) plotHeight() float64 { return f.height - f.top - f.bottom }
func (f chartFrame) y(v float64) float64 { return f.top + f.plotHeight()*(1-v/f.yMax) }
func (f chartFrame) axisY() float64      { return f.y(0) }

// open writes the svg element, horizontal grid lines and the left y labels.
func (f chartFrame) open(b *strings.Builder, label, unit string, step float64) {
	fmt.Fprintf(b, `<svg viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, f.width, f.height, label)
	for v := 0.0; v <= f.yMax+step/2; v += step {
		y := f.y(v)
		fmt.Fprintf(b, `<line class="grid" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, f.left, f.width-f.right, y, y)
		fmt.Fprintf(b, `<text class="lbl" x="%.1f" y="%.1f" text-anchor="end">%s</text>`, f.left-6, y+4, formatThousands(v))
	}
	fmt.Fprintf(b, `<text class="lbl" x="%.1f" y="%.1f" text-anchor="start">%s</text>`, f.left-50, f.top-14, unit)
}

// niceStep returns a step of 1, 2, 2.5 or 5 times a power of ten that
// divides top into about four intervals.
func niceStep(top float64) float64 {
	if top <= 0 {
		return 1
	}
	raw := top / 4
	pow := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if m*pow >= raw {
			return m * pow
		}
	}
	return 10 * pow
}

// monthSpan is one month on the time axis.
type monthSpan struct {
	stat        monthStat
	first, days int // index of the first day, number of days in the window
}

// timeAxis maps the days of the window to x positions.
type timeAxis struct {
	left, dx float64
	days     []dayStat
	months   []monthSpan
}

func newTimeAxis(r result, f chartFrame) timeAxis {
	a := timeAxis{left: f.left, dx: f.plotWidth() / float64(len(r.Days)), days: r.Days}
	stats := map[time.Time]monthStat{}
	for _, m := range r.Months {
		stats[m.Month] = m
	}
	for i, d := range r.Days {
		month := time.Date(d.Day.Year(), d.Day.Month(), 1, 0, 0, 0, 0, time.UTC)
		if len(a.months) == 0 || !a.months[len(a.months)-1].stat.Month.Equal(month) {
			a.months = append(a.months, monthSpan{stat: stats[month], first: i})
		}
		a.months[len(a.months)-1].days++
	}
	return a
}

// dayX is the center of day i.
func (a timeAxis) dayX(i int) float64 { return a.left + a.dx*(float64(i)+0.5) }

// span returns the left edge and the width of a month.
func (a timeAxis) span(m monthSpan) (float64, float64) {
	return a.left + a.dx*float64(m.first), a.dx * float64(m.days)
}

// minLabelWidth: months narrower than this (the few days at the ends of
// the window) get no name; the date row below names the exact days.
const minLabelWidth = 34.0

// monthAxis writes the month boundaries, the month names centered in their
// span, optionally the share of valid hours, and a row with the exact first
// and last day.
func (a timeAxis) monthAxis(b *strings.Builder, f chartFrame, withCompleteness bool) {
	axis := f.axisY()
	row := axis + 16
	for k, m := range a.months {
		left, width := a.span(m)
		if k > 0 {
			fmt.Fprintf(b, `<line class="month" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, left, left, f.top, axis+12)
		}
		if width < minLabelWidth {
			continue
		}
		label := monthLabel(m.stat.Month)
		if m.stat.Partial() {
			label += "*"
		}
		fmt.Fprintf(b, `<text class="lbl" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, left+width/2, row, label)
		if withCompleteness && m.stat.ExpectedHours > 0 {
			class := "lbl"
			if m.stat.CompletenessPct() < 95 {
				class = "lbl low"
			}
			fmt.Fprintf(b, `<text class="%s" x="%.1f" y="%.1f" text-anchor="middle">%.0f %%</text>`, class, left+width/2, row+15, m.stat.CompletenessPct())
		}
	}
	dateRow := row + 18
	if withCompleteness {
		dateRow += 15
	}
	first, last := a.days[0].Day, a.days[len(a.days)-1].Day
	fmt.Fprintf(b, `<text class="lbl" x="%.1f" y="%.1f" text-anchor="start">← %s</text>`, f.left, dateRow, first.Format("02.01.2006"))
	fmt.Fprintf(b, `<text class="lbl" x="%.1f" y="%.1f" text-anchor="end">%s →</text>`, f.width-f.right, dateRow, last.Format("02.01.2006"))
}

// monthBars draws the gross energy per month as bars over the month's
// days. In mode scale the filled part is stacked in a lighter color;
// partial months are faded.
func (a timeAxis) monthBars(b *strings.Builder, f chartFrame) {
	for _, m := range a.months {
		left, width := a.span(m)
		x := left + width*0.12
		barWidth := width * 0.76
		yMeasured := f.y(m.stat.MeasuredMWh)
		yGross := f.y(m.stat.GrossMWh)

		group := ""
		note := ""
		if m.stat.Partial() {
			group = ` class="partial"`
			note = " (nur teilweise im Zeitraum)"
		}
		fmt.Fprintf(b, `<g%s><title>%s%s: %s MWh brutto, davon %s MWh gemessen; %d von %d Stunden gültig</title>`,
			group, monthLabel(m.stat.Month), note, formatThousands(m.stat.GrossMWh), formatThousands(m.stat.MeasuredMWh), m.stat.ValidHours, m.stat.ExpectedHours)
		fmt.Fprintf(b, `<rect class="bar" x="%.1f" y="%.1f" width="%.1f" height="%.1f"/>`, x, yMeasured, barWidth, f.axisY()-yMeasured)
		if m.stat.GrossMWh > m.stat.MeasuredMWh {
			fmt.Fprintf(b, `<rect class="bar-filled" x="%.1f" y="%.1f" width="%.1f" height="%.1f"/>`, x, yGross, barWidth, yMeasured-yGross)
		}
		b.WriteString(`</g>`)
	}
}

// windLine draws the daily mean wind speed. The line breaks on days without
// any valid hour; days with missing hours get a mark below the axis.
func (a timeAxis) windLine(b *strings.Builder, f chartFrame) {
	var segment []string
	flush := func() {
		if len(segment) > 0 {
			fmt.Fprintf(b, `<polyline class="line" points="%s"/>`, strings.Join(segment, " "))
			segment = segment[:0]
		}
	}
	for i, d := range a.days {
		if d.ValidHours == 0 {
			flush()
			continue
		}
		segment = append(segment, fmt.Sprintf("%.1f,%.1f", a.dayX(i), f.y(d.MeanHubSpeedMS)))
	}
	flush()

	axis := f.axisY()
	for i, d := range a.days {
		if d.ValidHours < d.ExpectedHours {
			fmt.Fprintf(b, `<line class="gap" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"><title>%s: %d von %d Stunden gültig</title></line>`,
				a.dayX(i), a.dayX(i), axis+3, axis+9, d.Day.Format("2006-01-02"), d.ValidHours, d.ExpectedHours)
		}
	}
}

func maxGrossMWh(r result) float64 {
	m := 0.0
	for _, s := range r.Months {
		m = math.Max(m, s.GrossMWh)
	}
	return m
}

func maxDailySpeed(r result) float64 {
	m := 0.0
	for _, d := range r.Days {
		m = math.Max(m, d.MeanHubSpeedMS)
	}
	return m
}

// monthChart shows the gross energy per month as bars, each over its days.
func monthChart(r result) template.HTML {
	if len(r.Days) == 0 {
		return ""
	}
	maxMWh := maxGrossMWh(r)
	step := niceStep(maxMWh)
	f := newFrame(296, 66)
	f.yMax = math.Max(step, math.Ceil(maxMWh/step)*step)
	a := newTimeAxis(r, f)

	var b strings.Builder
	f.open(&b, "Bruttoenergie pro Monat", "MWh", step)
	a.monthBars(&b, f)
	a.monthAxis(&b, f, true)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// dayChart shows the daily mean wind speed at hub height as a line.
func dayChart(r result) template.HTML {
	if len(r.Days) == 0 {
		return ""
	}
	step := 5.0
	f := newFrame(256, 52)
	f.yMax = math.Max(step, math.Ceil(maxDailySpeed(r)/step)*step)
	a := newTimeAxis(r, f)

	var b strings.Builder
	f.open(&b, "Tagesmittel der Windgeschwindigkeit auf Nabenhöhe", "m/s", step)
	a.monthAxis(&b, f, false)
	a.windLine(&b, f)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// formatThousands rounds to an integer and groups thousands with a dot,
// German style: 12345.6 -> "12.346".
func formatThousands(v float64) string {
	n := int64(math.Round(v))
	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	}
	digits := fmt.Sprintf("%d", n)
	var groups []string
	for len(digits) > 3 {
		groups = append([]string{digits[len(digits)-3:]}, groups...)
		digits = digits[:len(digits)-3]
	}
	groups = append([]string{digits}, groups...)
	return sign + strings.Join(groups, ".")
}
