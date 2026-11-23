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

var monthNames = []string{"Jan", "Feb", "Mär", "Apr", "Mai", "Jun", "Jul", "Aug", "Sep", "Okt", "Nov", "Dez"}

func monthLabel(t time.Time) string {
	return fmt.Sprintf("%s %02d", monthNames[t.Month()-1], t.Year()%100)
}

// chartFrame holds the plot area of a chart.
type chartFrame struct {
	width, height            float64
	left, right, top, bottom float64 // margins
	yMax                     float64
}

func (f chartFrame) plotWidth() float64  { return f.width - f.left - f.right }
func (f chartFrame) plotHeight() float64 { return f.height - f.top - f.bottom }
func (f chartFrame) y(v float64) float64 { return f.top + f.plotHeight()*(1-v/f.yMax) }

// open writes the svg element, horizontal grid lines and y labels.
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
// divides max into about four intervals.
func niceStep(max float64) float64 {
	if max <= 0 {
		return 1
	}
	raw := max / 4
	pow := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if m*pow >= raw {
			return m * pow
		}
	}
	return 10 * pow
}

// monthChart shows the gross energy per month as bars. In mode scale the
// filled part is stacked in a lighter color. Below each bar: month and
// share of valid hours.
func monthChart(r result) template.HTML {
	if len(r.Months) == 0 {
		return ""
	}
	maxMWh := 0.0
	for _, m := range r.Months {
		maxMWh = math.Max(maxMWh, m.GrossMWh)
	}
	step := niceStep(maxMWh)
	f := chartFrame{width: 720, height: 280, left: 56, right: 8, top: 26, bottom: 46}
	f.yMax = math.Max(step, math.Ceil(maxMWh/step)*step)

	var b strings.Builder
	f.open(&b, "Bruttoenergie pro Monat", "MWh", step)
	slot := f.plotWidth() / float64(len(r.Months))
	barWidth := slot * 0.7
	for i, m := range r.Months {
		x := f.left + slot*float64(i) + (slot-barWidth)/2
		center := x + barWidth/2
		yMeasured := f.y(m.MeasuredMWh)
		yGross := f.y(m.GrossMWh)

		// The first and last month lie only partly inside the window; their
		// bars are lower for that reason alone, so they are drawn faded and
		// marked with an asterisk.
		label := monthLabel(m.Month)
		groupClass := ""
		note := ""
		if m.Partial() {
			label += "*"
			groupClass = ` class="partial"`
			note = " (nur teilweise im Zeitraum)"
		}

		fmt.Fprintf(&b, `<g%s><title>%s%s: %s MWh brutto, davon %s MWh gemessen; %d von %d Stunden gültig</title>`,
			groupClass, monthLabel(m.Month), note, formatThousands(m.GrossMWh), formatThousands(m.MeasuredMWh), m.ValidHours, m.ExpectedHours)
		fmt.Fprintf(&b, `<rect class="bar" x="%.1f" y="%.1f" width="%.1f" height="%.1f"/>`, x, yMeasured, barWidth, f.y(0)-yMeasured)
		if m.GrossMWh > m.MeasuredMWh {
			fmt.Fprintf(&b, `<rect class="bar-filled" x="%.1f" y="%.1f" width="%.1f" height="%.1f"/>`, x, yGross, barWidth, yMeasured-yGross)
		}
		b.WriteString(`</g>`)

		fmt.Fprintf(&b, `<text class="lbl" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, center, f.height-f.bottom+16, label)
		completenessClass := "lbl"
		if m.CompletenessPct() < 95 {
			completenessClass = "lbl low"
		}
		fmt.Fprintf(&b, `<text class="%s" x="%.1f" y="%.1f" text-anchor="middle">%.0f %%</text>`, completenessClass, center, f.height-f.bottom+32, m.CompletenessPct())
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// dayChart shows the daily mean wind speed at hub height as a line. The line
// breaks on days without any valid hour; days with missing hours get a mark
// below the axis.
func dayChart(r result) template.HTML {
	if len(r.Days) == 0 {
		return ""
	}
	maxSpeed := 0.0
	for _, d := range r.Days {
		maxSpeed = math.Max(maxSpeed, d.MeanHubSpeedMS)
	}
	step := 5.0
	f := chartFrame{width: 720, height: 240, left: 56, right: 8, top: 26, bottom: 40}
	f.yMax = math.Max(step, math.Ceil(maxSpeed/step)*step)

	var b strings.Builder
	f.open(&b, "Tagesmittel der Windgeschwindigkeit auf Nabenhöhe", "m/s", step)
	dx := f.plotWidth() / float64(len(r.Days))
	x := func(i int) float64 { return f.left + dx*(float64(i)+0.5) }

	var segment []string
	flush := func() {
		if len(segment) > 0 {
			fmt.Fprintf(&b, `<polyline class="line" points="%s"/>`, strings.Join(segment, " "))
			segment = segment[:0]
		}
	}
	for i, d := range r.Days {
		if d.ValidHours == 0 {
			flush()
			continue
		}
		segment = append(segment, fmt.Sprintf("%.1f,%.1f", x(i), f.y(d.MeanHubSpeedMS)))
	}
	flush()

	axis := f.y(0)
	for i, d := range r.Days {
		if d.ValidHours < d.ExpectedHours {
			fmt.Fprintf(&b, `<line class="gap" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"><title>%s: %d von %d Stunden gültig</title></line>`,
				x(i), x(i), axis+3, axis+9, d.Day.Format("2006-01-02"), d.ValidHours, d.ExpectedHours)
		}
		if d.Day.Day() == 1 {
			fmt.Fprintf(&b, `<text class="lbl" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, x(i), f.height-f.bottom+26, monthLabel(d.Day))
		}
	}
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
