package main

import (
	"fmt"
	"math"
)

const chartWidth = 720

// rampChart has two panels with one axis each, because acceptance and latency have
// different scales. Each step is one mark, and nothing is drawn between steps.
func rampChart(r rampResult) *canvas {
	c := newCanvas(chartWidth, 452, "Ramp: first-attempt acceptance and p99 by offered rate")
	left, right := 62.0, float64(chartWidth-20)
	band := (right - left) / float64(len(r.Steps))
	x := func(i int) float64 { return left + (float64(i)+0.5)*band }

	const aTop, aBottom = 76.0, 190.0
	const pTop, pBottom = 244.0, 392.0
	acceptanceY := func(v float64) float64 { return aBottom - v/100*(aBottom-aTop) }
	p99Y := func(v float64) float64 { return pBottom - math.Log10(v)/3*(pBottom-pTop) }

	for i, step := range r.Steps {
		if step.Saturated {
			c.rect(x(i)-band/2, aTop-18, band, pBottom-aTop+18, "band", "Saturated step")
			c.text(x(i), aTop-6, "t", "middle", "saturated")
		}
	}
	c.text(16, 52, "m", "start", "First-attempt acceptance of offered events, % [measured]")
	for _, tick := range []float64{0, 50, 100} {
		c.line(left, acceptanceY(tick), right, acceptanceY(tick), "g")
		c.text(left-8, acceptanceY(tick)+4, "m", "end", fmt.Sprintf("%.0f", tick))
	}
	c.line(left, acceptanceY(99), right, acceptanceY(99), "lim")
	c.text(left+4, acceptanceY(99)+15, "m", "start", "dashed: saturation below 99%")

	c.text(16, 226, "m", "start", "First-attempt p99, ms, log scale [measured]")
	for _, tick := range []float64{1, 10, 100, 1000} {
		c.line(left, p99Y(tick), right, p99Y(tick), "g")
		c.text(left-8, p99Y(tick)+4, "m", "end", fmt.Sprintf("%.0f", tick))
	}
	c.line(left, p99Y(500), right, p99Y(500), "lim")
	c.text(left+4, p99Y(500)+15, "m", "start", "dashed: saturation above 500 ms")

	for i, step := range r.Steps {
		class := "s1"
		if step.Saturated {
			class = "s2"
		}
		rate := formatCount(step.OfferedEventsPerSecond)
		c.dot(x(i), acceptanceY(step.FirstAcceptancePercent), class, fmt.Sprintf("%s events/s: %.2f%% accepted", rate, step.FirstAcceptancePercent))
		c.dot(x(i), p99Y(step.FirstP99Ms), class, fmt.Sprintf("%s events/s: p99 %.1f ms", rate, step.FirstP99Ms))
		c.text(x(i), pBottom+18, "m", "middle", shortRate(step.OfferedEventsPerSecond))
		if step.Saturated {
			c.text(x(i)-9, acceptanceY(step.FirstAcceptancePercent)+4, "t", "end", fmt.Sprintf("%.2f%%", step.FirstAcceptancePercent))
			c.text(x(i)-9, p99Y(step.FirstP99Ms)+4, "t", "end", fmt.Sprintf("%.1f ms", step.FirstP99Ms))
		}
	}
	c.text((left+right)/2, pBottom+40, "m", "middle", "Offered events per second; each step doubles the one before")
	return c
}

// burstChart stacks the attempts that finished in each second. A second without attempts
// has no bar.
func burstChart(b burstResult) *canvas {
	c := newCanvas(chartWidth, 330, "Burst: attempts finished in each second")
	c.legend(50, [2]string{"s1", "Acknowledged"}, [2]string{"s2", "Not acknowledged (503)"})
	left, right := 62.0, float64(chartWidth-20)
	const top, bottom = 84.0, 280.0
	band := (right - left) / float64(len(b.Timeline))
	var largest int64
	for _, t := range b.Timeline {
		largest = max(largest, t.FirstAttempts+t.Retries)
	}
	yMax := niceCeil(float64(largest))
	height := func(v int64) float64 { return float64(v) / yMax * (bottom - top) }

	peakEnd := b.PeakStartSeconds + b.PeakDurationSeconds
	x := func(second int) float64 { return left + float64(second)*band }
	c.rect(x(b.PeakStartSeconds), top-14, x(peakEnd)-x(b.PeakStartSeconds), bottom-top+14, "band", "Peak")
	c.text(x(b.PeakStartSeconds)+4, top-2, "t", "start", "peak")
	for _, tick := range []float64{0, yMax / 2, yMax} {
		y := bottom - tick/yMax*(bottom-top)
		c.line(left, y, right, y, "g")
		c.text(left-8, y+4, "m", "end", fmt.Sprintf("%.0f", tick))
	}
	for _, t := range b.Timeline {
		acknowledged := t.FirstAcknowledged + t.RetriesAcknowledged
		failed := unacknowledged(t)
		tooltip := fmt.Sprintf("Second %d: %d acknowledged, %d not acknowledged, of which %d were 503", t.Second, acknowledged, failed, t.Rejected503)
		if acknowledged > 0 {
			c.rect(x(t.Second)+1, bottom-height(acknowledged), band-2, height(acknowledged), "s1", tooltip)
		}
		if failed > 0 {
			c.rect(x(t.Second)+1, bottom-height(acknowledged)-height(failed)-2, band-2, height(failed), "s2", tooltip)
		}
		if t.Second%5 == 0 {
			c.text(x(t.Second)+band/2, bottom+18, "m", "middle", fmt.Sprintf("%d", t.Second))
		}
	}
	if second, ok := recovery(b.Timeline, peakEnd); ok {
		c.line(x(second), top-14, x(second), bottom, "lim")
		c.text(x(second)+6, top-2, "t", "start", fmt.Sprintf("recovered %d s after the peak [calculated]", second-peakEnd))
	}
	c.text((left+right)/2, bottom+40, "m", "middle", "Seconds from the start of the run")
	return c
}

// boundaryChart shows all 8 cases on one linear axis, with the value beside each mark.
func boundaryChart(b boundaryResult) *canvas {
	const rowHeight = 34.0
	c := newCanvas(chartWidth, int(92+rowHeight*float64(len(b.Cases))), "Boundary undercount, all 8 cases")
	c.legend(50, [2]string{"s1", "Maximum"}, [2]string{"s2", "Mean"})
	left, right := 176.0, float64(chartWidth-190)
	maxValue := 0.0
	for _, item := range b.Cases {
		maxValue = max(maxValue, item.MaxUndercount)
	}
	axis := niceCeil(maxValue)
	x := func(v float64) float64 { return left + v/axis*(right-left) }
	for i, item := range b.Cases {
		y := 72 + float64(i)*rowHeight
		label := fmt.Sprintf("%s, r=%d%%, %s", item.Profile, item.ReturnShare, item.Bucket)
		c.text(left-10, y+15, "t", "end", label)
		c.line(left, y, left, y+24, "g")
		c.rect(left, y+5, max(x(item.MaxUndercount)-left, 1), 14, "s1", fmt.Sprintf("%s: maximum %.2f%%", label, item.MaxUndercount))
		c.dot(x(item.MeanUndercount), y+12, "s2", fmt.Sprintf("%s: mean %.2f%%", label, item.MeanUndercount))
		c.text(x(item.MaxUndercount)+10, y+16, "m", "start", fmt.Sprintf("max %.2f%%, mean %.2f%%", item.MaxUndercount, item.MeanUndercount))
	}
	return c
}

// rule is one part of the decision rule. Each has its own scale, because the units differ.
type rule struct {
	Name, Limit, Unit string
	Label             string
	Value6h, Value1h  float64
	LimitValue        float64
	Holds             bool
}

// decisionRules applies the rule fixed in docs/DESIGN.md to the measured values.
func decisionRules(r results) []rule {
	row6h, _ := r.Bench.row("6h")
	row1h, _ := r.Bench.row("1h")
	scenario6h, _ := r.Boundary.find("uniform", 50, "6h")
	scenario1h, _ := r.Boundary.find("uniform", 50, "1h")
	memory6h, memory1h := float64(row6h.UsedMemoryBytes)/1e9, float64(row1h.UsedMemoryBytes)/1e9
	return []rule{
		{"Rule 1: worst undercount, decision scenario", "6h above 1%", "%", "measured", scenario6h.MaxUndercount, scenario1h.MaxUndercount, 1, scenario6h.MaxUndercount > 1},
		{"Rule 2: uncached estimate p99", "1h at most 20 ms", " ms", "measured", row6h.P99Ms, row1h.P99Ms, 20, row1h.P99Ms <= 20},
		{"Rule 3: Redis memory, 500 dense segments", "1h at most 2.5 GB", " GB", "measured", memory6h, memory1h, 2.5, memory1h <= 2.5},
		{"Rule 4: recomputation share of Redis time", "1h at most 20%", "%", "calculated", row6h.SharePercent, row1h.SharePercent, 20, row1h.SharePercent <= 20},
	}
}

func rulesChart(rules []rule) *canvas {
	const rowHeight = 92.0
	c := newCanvas(chartWidth, int(66+rowHeight*float64(len(rules))), "6h and 1h against the four rules")
	c.legend(50, [2]string{"s1", "6h buckets"}, [2]string{"s2", "1h buckets"})
	left, right := 44.0, float64(chartWidth-110)
	for i, item := range rules {
		y := 66 + float64(i)*rowHeight
		verdict := "does not hold"
		if item.Holds {
			verdict = "holds"
		}
		c.text(16, y+14, "t", "start", fmt.Sprintf("%s [%s]", item.Name, item.Label))
		c.text(16, y+31, "m", "start", fmt.Sprintf("Limit: %s; %s", item.Limit, verdict))
		axis := max(item.LimitValue, item.Value6h, item.Value1h) * 1.05
		x := func(v float64) float64 { return left + v/axis*(right-left) }
		for j, bar := range []struct {
			name, class string
			value       float64
		}{{"6h", "s1", item.Value6h}, {"1h", "s2", item.Value1h}} {
			barY := y + 38 + float64(j)*20
			c.text(left-6, barY+13, "m", "end", bar.name)
			c.rect(left, barY+1, max(x(bar.value)-left, 1), 14, bar.class, fmt.Sprintf("%s: %s", bar.name, formatRuleValue(bar.value, item.Unit)))
			// A value just below the limit moves past the limit mark, so they do not collide.
			labelX := x(bar.value)
			if bar.value < item.LimitValue && x(item.LimitValue)-labelX < 60 {
				labelX = x(item.LimitValue)
			}
			c.text(labelX+6, barY+13, "t", "start", formatRuleValue(bar.value, item.Unit))
		}
		c.line(x(item.LimitValue), y+36, x(item.LimitValue), y+80, "lim")
	}
	return c
}

func formatRuleValue(value float64, unit string) string {
	return fmt.Sprintf("%.2f%s", value, unit)
}

// hllChart draws one dot per independent set of users, at its measured error.
func hllChart(h hllResult) *canvas {
	const rowHeight = 52.0
	c := newCanvas(chartWidth, int(110+rowHeight*float64(len(h.Results))), "HyperLogLog error by number of users")
	left, right := 110.0, float64(chartWidth-30)
	const limit, axis = 2.43, 3.0
	x := func(v float64) float64 { return left + (v+axis)/(2*axis)*(right-left) }
	top, bottom := 70.0, 70+rowHeight*float64(len(h.Results))
	c.text(16, 50, "m", "start", "Error of the estimate, %, one dot per set of users [measured]")
	for _, tick := range []float64{-3, -2, -1, 0, 1, 2, 3} {
		c.line(x(tick), top, x(tick), bottom, "g")
		c.text(x(tick), bottom+18, "m", "middle", fmt.Sprintf("%+.0f", tick))
	}
	for _, edge := range []float64{-limit, limit} {
		c.line(x(edge), top, x(edge), bottom, "lim")
	}
	c.text(x(0), bottom+36, "m", "middle", "dashed: the test fails beyond 2.43%")
	for i, item := range h.Results {
		y := top + (float64(i)+0.5)*rowHeight
		c.text(left-14, y+4, "t", "end", formatCount(float64(item.Users)))
		for _, value := range item.ErrorsPercent {
			c.dot(x(value), y, "s1", fmt.Sprintf("%s users: %+.2f%%", formatCount(float64(item.Users)), value))
		}
	}
	return c
}

// coverageChart leaves out a package with no reported coverage, instead of showing 0.
func coverageChart(q qualityResult) *canvas {
	const rowHeight = 30.0
	c := newCanvas(chartWidth, int(60+rowHeight*float64(len(q.Packages))), "Statement coverage by package [measured]")
	left, right := 210.0, float64(chartWidth-70)
	x := func(v float64) float64 { return left + v/100*(right-left) }
	for i, item := range q.Packages {
		y := 44 + float64(i)*rowHeight
		c.text(left-10, y+15, "t", "end", item.Name)
		if item.CoveragePercent == nil {
			c.text(left+6, y+15, "m", "start", "not reported")
			continue
		}
		c.rect(left, y+4, max(x(*item.CoveragePercent)-left, 1), 14, "s1", fmt.Sprintf("%s: %.1f%%", item.Name, *item.CoveragePercent))
		c.text(x(*item.CoveragePercent)+6, y+15, "t", "start", fmt.Sprintf("%.1f%%", *item.CoveragePercent))
	}
	return c
}
