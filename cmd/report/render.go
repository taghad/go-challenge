package main

import (
	"fmt"
	"html"
	"strconv"
	"strings"
)

// table is one data table. The first LeftColumns columns hold text, and the rest hold
// numbers. Every chart has a table, so no value can only be read from a picture.
type table struct {
	Headers     []string
	Rows        [][]string
	LeftColumns int
}

type section struct {
	ID         string
	Title      string
	Paragraphs []string
	Chart      string // file name without extension; empty if the section has no chart
	Canvas     *canvas
	Tables     []table
}

const banner = "Not production capacity. These results describe one laptop, with the simulator in the same VM as ES and Redis, and with repeated users."

const labels = "Labels: [measured] was read from a run on the machine below. [calculated] is arithmetic on measured values, with no run of its own. [assumption] was chosen, not measured. Marks are separate: no line joins two points, and no value is drawn between them."

func buildSections(r results) []section {
	peakEnd := r.Burst.PeakStartSeconds + r.Burst.PeakDurationSeconds
	recovered, ok := recovery(r.Burst.Timeline, peakEnd)
	recoveryText := "not within the run"
	if ok {
		recoveryText = fmt.Sprintf("%d s, at a resolution of 1 s", recovered-peakEnd)
	}
	lastRetry := 0
	for _, t := range r.Burst.Timeline {
		if t.Retries > 0 {
			lastRetry = t.Second
		}
	}

	rampRows := [][]string{}
	for _, s := range r.Ramp.Steps {
		rampRows = append(rampRows, []string{
			formatCount(s.OfferedEventsPerSecond),
			percent(s.FirstAcceptancePercent),
			ms(s.FirstP99Ms),
			percent(s.FinalAcceptancePercent),
			fmt.Sprintf("%s of %s", formatCount(float64(s.Rejected503)), formatCount(float64(s.FirstAttempts))),
			fmt.Sprintf("%.0f / %.0f / %.0f%%", s.CPUMeanPercent.ES, s.CPUMeanPercent.Redis, s.CPUMeanPercent.Simulator),
			yesNo(s.Saturated),
		})
	}

	timelineRows := [][]string{}
	for _, t := range r.Burst.Timeline {
		timelineRows = append(timelineRows, []string{
			strconv.Itoa(t.Second),
			fmt.Sprintf("%d / %d", t.FirstAcknowledged, t.FirstAttempts),
			fmt.Sprintf("%d / %d", t.RetriesAcknowledged, t.Retries),
			strconv.FormatInt(t.Rejected503, 10),
			ms(t.MaxLatencyMs),
		})
	}

	boundaryRows := [][]string{}
	for _, item := range r.Boundary.Cases {
		boundaryRows = append(boundaryRows, []string{
			item.Profile, item.Bucket,
			fmt.Sprintf("%d%%", item.ReturnShare),
			percent(item.RealizedReturn),
			percent(item.MeanUndercount),
			percent(item.MaxUndercount),
		})
	}

	rules := decisionRules(r)
	ruleRows := [][]string{}
	for _, item := range rules {
		ruleRows = append(ruleRows, []string{
			item.Name, item.Limit,
			formatRuleValue(item.Value6h, item.Unit),
			formatRuleValue(item.Value1h, item.Unit),
			item.Label, yesNo(item.Holds),
		})
	}
	benchRows := [][]string{}
	for _, row := range r.Bench.Rows {
		benchRows = append(benchRows, []string{
			row.Bucket,
			formatCount(float64(row.Keys)),
			fmt.Sprintf("%.2f GB", float64(row.UsedMemoryBytes)/1e9),
			formatCount(float64(row.KeyBytes)),
			fmt.Sprintf("%.2f / %.2f ms", row.P50Ms, row.P99Ms),
			fmt.Sprintf("%.0f", row.UsecPerCall),
			percent(row.SharePercent),
		})
	}

	hllRows := [][]string{}
	for _, item := range r.HLL.Results {
		errorsText, largest := []string{}, 0.0
		for _, value := range item.ErrorsPercent {
			errorsText = append(errorsText, fmt.Sprintf("%+.2f", value))
			largest = max(largest, abs(value))
		}
		hllRows = append(hllRows, []string{formatCount(float64(item.Users)), strings.Join(errorsText, ", "), percent(largest)})
	}

	burstBoundaryRows := [][]string{}
	for _, row := range r.BoundaryBurst.Rows {
		burstBoundaryRows = append(burstBoundaryRows, []string{
			row.Bucket,
			percent(row.After503),
			percent(row.Warmup503),
			fmt.Sprintf("%.3f / %.1f ms", row.P50Ms, row.P99Ms),
			fmt.Sprintf("%d of %d", row.Add503, row.AddRequests),
			ms(row.AddP99Ms),
			fmt.Sprintf("%.2f s", row.AllServedMs/1000),
		})
	}

	testRows := [][]string{}
	total := 0
	for _, p := range r.Quality.Packages {
		coverage := "not reported"
		if p.CoveragePercent != nil {
			coverage = percent(*p.CoveragePercent)
		}
		total += p.Passed
		testRows = append(testRows, []string{p.Name, strconv.Itoa(p.Passed), strconv.Itoa(p.Failed), strconv.Itoa(p.Skipped), coverage})
	}
	symbol, pkg, module := 0, 0, 0
	findings := []string{}
	for _, f := range r.Quality.Findings {
		switch f.Level {
		case "symbol":
			symbol++
		case "package":
			pkg++
		default:
			module++
		}
		findings = append(findings, fmt.Sprintf("%s (%s level)", f.ID, f.Level))
	}
	findingsText := "none"
	if len(findings) > 0 {
		findingsText = strings.Join(findings, ", ")
	}
	checkRows := [][]string{
		{"Tests", fmt.Sprintf("%s; %d passed", passFail(r.Quality.TestsPassed), total)},
		{"Race detector", fmt.Sprintf("%s; a detected race fails the run", enabled(r.Quality.RaceEnabled))},
		{"go vet", cleanText(r.Quality.VetClean)},
		{"gofmt", cleanText(r.Quality.GofmtClean)},
		{"govulncheck " + r.Quality.GovulncheckVersion, fmt.Sprintf("%d reached by the code, %d in imported packages, %d at module level: %s", symbol, pkg, module, findingsText)},
	}

	return []section{
		{
			ID:    "ramp",
			Title: "Ramp and saturation",
			Paragraphs: []string{
				fmt.Sprintf("%s buckets, batches of %d, %d s per step, %s users, %d segments. Stopped by %s. Highest sustained rate: %s events per second [measured].",
					r.Ramp.BucketSize, r.Ramp.Batch, r.Ramp.StepSeconds, formatCount(float64(r.Ramp.Users)), r.Ramp.Segments, r.Ramp.StoppedBy, formatCount(r.Ramp.HighestSustained)),
				estimateCheckText("After the ramp", r.Ramp.EstimateCheck),
				"Source: " + r.Ramp.Source,
			},
			Chart:  "ramp",
			Canvas: rampChart(r.Ramp),
			Tables: []table{{
				Headers:     []string{"Offered events/s", "First-attempt acceptance", "First-attempt p99", "Final acceptance", "503 of first attempts", "Mean CPU, ES / Redis / simulator", "Saturated"},
				Rows:        rampRows,
				LeftColumns: 0,
			}},
		},
		{
			ID:    "burst",
			Title: "Burst",
			Paragraphs: []string{
				fmt.Sprintf("%s buckets. A base of %s events per second, %.0fx from %d s for %d s, in a %d s run. The peak is %s events per second, and %s events were offered [measured].",
					r.Burst.BucketSize, formatCount(r.Burst.BaseEventsPerSecond), r.Burst.Multiplier, r.Burst.PeakStartSeconds, r.Burst.PeakDurationSeconds, r.Burst.DurationSeconds, formatCount(r.Burst.BaseEventsPerSecond*r.Burst.Multiplier), formatCount(float64(r.Burst.OfferedEvents))),
				estimateCheckText("After the burst", r.Burst.EstimateCheck),
				"Source: " + r.Burst.Source,
			},
			Chart:  "burst",
			Canvas: burstChart(r.Burst),
			Tables: []table{
				{
					Headers: []string{"Result", "Value", "Label"},
					Rows: [][]string{
						{"First-attempt acceptance, whole run", percent(r.Burst.FirstAttempts.AcceptancePercent), "measured"},
						{"Final acceptance", percent(r.Burst.FinalAcceptancePercent), "measured"},
						{"Abandoned events", formatCount(float64(r.Burst.AbandonedEvents)), "measured"},
						{"503 rate, first attempts", percent(rate(r.Burst.FirstAttempts.Rejected503, r.Burst.FirstAttempts.Attempts)), "calculated"},
						{"503 rate, retries", percent(rate(r.Burst.Retries.Rejected503, r.Burst.Retries.Attempts)), "calculated"},
						{"Timeouts and network errors", strconv.FormatInt(r.Burst.FirstAttempts.Timeouts+r.Burst.FirstAttempts.NetworkErrors+r.Burst.Retries.Timeouts+r.Burst.Retries.NetworkErrors, 10), "measured"},
						{"Final latency p99, with retry waits", ms(r.Burst.FinalP99Ms), "measured"},
						{"Time to recover after the peak", recoveryText, "calculated"},
						{"Last second with a retry", strconv.Itoa(lastRetry), "measured"},
						{"Peak memory, ES / Redis / simulator", fmt.Sprintf("%.1f / %.1f / %.1f MiB", r.Burst.PeakMemoryMiB.ES, r.Burst.PeakMemoryMiB.Redis, r.Burst.PeakMemoryMiB.Simulator), "measured"},
						{"Peak CPU, ES / Redis / simulator", fmt.Sprintf("%.0f / %.0f / %.0f%%", r.Burst.PeakCPUPercent.ES, r.Burst.PeakCPUPercent.Redis, r.Burst.PeakCPUPercent.Simulator), "measured"},
						{"ES store failures; ES restarts", fmt.Sprintf("%d; %d", r.Burst.ESStoreFailures, r.Burst.ESRestarts), "measured"},
					},
					LeftColumns: 1,
				},
				{
					Headers:     []string{"Second", "First attempts acknowledged / total", "Retries acknowledged / total", "503", "Largest latency"},
					Rows:        timelineRows,
					LeftColumns: 0,
				},
			},
		},
		{
			ID:    "boundary",
			Title: "Boundary undercount",
			Paragraphs: []string{
				fmt.Sprintf("In process with exact sets: %s users, seed %d, %d samples [measured]. The traffic model is an [assumption], so these values hold only for that model.", formatCount(float64(r.Boundary.Users)), r.Boundary.Seed, r.Boundary.Samples),
				"Source: " + r.Boundary.Source,
			},
			Chart:  "boundary",
			Canvas: boundaryChart(r.Boundary),
			Tables: []table{{
				Headers:     []string{"Profile", "Bucket", "Return share", "Realized return share", "Mean undercount", "Maximum undercount"},
				Rows:        boundaryRows,
				LeftColumns: 2,
			}},
		},
		{
			ID:    "rules",
			Title: "1h against 6h, with the four rules",
			Paragraphs: []string{
				"The rule was fixed before measuring, and its limits are owner decisions and [assumption]. Each rule has its own scale, and the dashed mark is its limit.",
				fmt.Sprintf("%d segments, with %d uncached calls at each size. Source: %s", r.Bench.Segments, r.Bench.Samples, r.Bench.Source),
			},
			Chart:  "rules",
			Canvas: rulesChart(rules),
			Tables: []table{
				{Headers: []string{"Rule", "Limit", "6h", "1h", "Label", "Holds"}, Rows: ruleRows, LeftColumns: 2},
				{Headers: []string{"Bucket", "Keys", "used_memory", "Bytes per key", "Client p50 / p99", "usec_per_call", "Recomputation share [calculated]"}, Rows: benchRows, LeftColumns: 1},
			},
		},
		{
			ID:         "hll",
			Title:      "HyperLogLog error",
			Paragraphs: []string{"On real Redis through the Store, with one independent set of users per dot [measured].", "Source: " + r.HLL.Source},
			Chart:      "hll",
			Canvas:     hllChart(r.HLL),
			Tables:     []table{{Headers: []string{"Users", "Error in each set, %", "Largest absolute error"}, Rows: hllRows, LeftColumns: 0}},
		},
		{
			ID:         "boundary-burst",
			Title:      "Boundary burst",
			Paragraphs: []string{"The 15 s after a bucket boundary, in process with real Redis, on 500 dense segments [measured]. The time to serve all segments is likely shaped by the test's fixed 1 s retry. The Add p99 has only 30 samples.", "Source: " + r.BoundaryBurst.Source},
			Tables:     []table{{Headers: []string{"Bucket", "Estimate 503", "503 during warm-up", "Estimate p50 / p99", "Add 503", "Add p99", "All segments served"}, Rows: burstBoundaryRows, LeftColumns: 1}},
		},
		{
			ID:         "quality",
			Title:      "Tests, coverage, race detector, and govulncheck",
			Paragraphs: []string{fmt.Sprintf("Collected %s with %s: %s [measured].", r.Quality.GeneratedAt, r.Quality.Go, r.Quality.TestCommand), "Source: " + r.Quality.Source},
			Chart:      "coverage",
			Canvas:     coverageChart(r.Quality),
			Tables: []table{
				{Headers: []string{"Check", "Result"}, Rows: checkRows, LeftColumns: 2},
				{Headers: []string{"Package", "Passed", "Failed", "Skipped", "Coverage"}, Rows: testRows, LeftColumns: 1},
			},
		},
	}
}

func machineLines(m machine) []string {
	return []string{
		fmt.Sprintf("Host: %s, %s, %d CPUs, %d GB, %s", m.Model, m.Chip, m.CPUs, m.MemoryGB, m.OS),
		"Docker: " + m.Docker,
		fmt.Sprintf("Redis %s; Go %s", m.Redis, m.Go),
		m.Note,
	}
}

func renderMarkdown(r results, sections []section) string {
	var b strings.Builder
	b.WriteString("<!-- Generated by cmd/report from docs/results. Do not edit by hand. -->\n# Test report\n\n")
	fmt.Fprintf(&b, "> **%s**\n\n", banner)
	b.WriteString("**Machine**\n\n")
	for _, line := range machineLines(r.Machine) {
		fmt.Fprintf(&b, "- %s\n", line)
	}
	fmt.Fprintf(&b, "\n%s\n\n", labels)
	for _, s := range sections {
		fmt.Fprintf(&b, "## %s\n\n", s.Title)
		for _, p := range s.Paragraphs {
			fmt.Fprintf(&b, "%s\n\n", p)
		}
		if s.Chart != "" {
			fmt.Fprintf(&b, "![%s](report/%s.svg)\n\n", s.Canvas.title, s.Chart)
		}
		for _, t := range s.Tables {
			fmt.Fprintf(&b, "| %s |\n|", strings.Join(t.Headers, " | "))
			for i := range t.Headers {
				if i < t.LeftColumns {
					b.WriteString("---|")
				} else {
					b.WriteString("---:|")
				}
			}
			b.WriteString("\n")
			for _, row := range t.Rows {
				fmt.Fprintf(&b, "| %s |\n", strings.Join(row, " | "))
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

const pageStyle = `:root{color-scheme:light dark;--page:#f5f5f2;--text:#1d1f21;--muted:#5e6469;--line:#d9d9d4;--panel:#ffffff;--notice:#fbeee6;--notice-line:#c4561f;--link:#1f5f8b}
@media (prefers-color-scheme:dark){:root{--page:#15171a;--text:#eceded;--muted:#a2a8ad;--line:#363a3e;--panel:#1e2124;--notice:#33241b;--notice-line:#d9895f;--link:#7cb9e2}}
*{box-sizing:border-box}
body{margin:0;background:var(--page);color:var(--text);font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;line-height:1.5}
main{max-width:52rem;margin:0 auto;padding:2rem 1rem}
a{color:var(--link)}
h1{font-size:1.75rem;margin:.5rem 0 1rem;line-height:1.2}
h2{font-size:1.25rem;margin:2.5rem 0 .75rem;line-height:1.25}
p,ul{margin:0 0 .75rem}
.notice{background:var(--notice);border-left:4px solid var(--notice-line);border-radius:.25rem;padding:.75rem 1rem;font-weight:600}
.muted{color:var(--muted);font-size:.925rem}
.scroll{overflow-x:auto;margin:0 0 1rem}
.chart svg{display:block;width:100%;min-width:36rem;height:auto}
table{border-collapse:collapse;width:100%;font-size:.875rem;background:var(--panel)}
th,td{border:1px solid var(--line);padding:.35rem .6rem;text-align:left;vertical-align:top}
td.n,th.n{text-align:right;font-variant-numeric:tabular-nums}
`

func renderPage(r results, sections []section) string {
	var b strings.Builder
	b.WriteString("<!doctype html>\n<!-- Generated by cmd/report from docs/results. Do not edit by hand. -->\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n<title>Test report</title>\n<style>\n")
	b.WriteString(pageStyle)
	b.WriteString(svgStyle)
	b.WriteString("\n</style>\n</head>\n<body>\n<main>\n<p><a href=\"/\">Estimation Service</a></p>\n<h1>Test report</h1>\n")
	fmt.Fprintf(&b, "<p class=\"notice\">%s</p>\n<ul class=\"muted\">\n", html.EscapeString(banner))
	for _, line := range machineLines(r.Machine) {
		fmt.Fprintf(&b, "<li>%s</li>\n", html.EscapeString(line))
	}
	fmt.Fprintf(&b, "</ul>\n<p class=\"muted\">%s</p>\n", html.EscapeString(labels))
	for _, s := range sections {
		fmt.Fprintf(&b, "<section id=\"%s\">\n<h2>%s</h2>\n", s.ID, html.EscapeString(s.Title))
		for _, p := range s.Paragraphs {
			fmt.Fprintf(&b, "<p>%s</p>\n", html.EscapeString(p))
		}
		if s.Chart != "" {
			fmt.Fprintf(&b, "<div class=\"scroll chart\">%s</div>\n", s.Canvas.svg(""))
		}
		for _, t := range s.Tables {
			b.WriteString("<div class=\"scroll\"><table>\n<thead><tr>")
			for i, header := range t.Headers {
				fmt.Fprintf(&b, "<th%s>%s</th>", numericClass(i, t), html.EscapeString(header))
			}
			b.WriteString("</tr></thead>\n<tbody>\n")
			for _, row := range t.Rows {
				b.WriteString("<tr>")
				for i, cell := range row {
					fmt.Fprintf(&b, "<td%s>%s</td>", numericClass(i, t), html.EscapeString(cell))
				}
				b.WriteString("</tr>\n")
			}
			b.WriteString("</tbody>\n</table></div>\n")
		}
		b.WriteString("</section>\n")
	}
	b.WriteString("</main>\n</body>\n</html>\n")
	return b.String()
}

func numericClass(column int, t table) string {
	if column < t.LeftColumns {
		return ""
	}
	return ` class="n"`
}

func estimateCheckText(prefix string, c estimateCheck) string {
	return fmt.Sprintf("%s: %d of %d segments in range, %d out of range, and %d failed, with the estimate from %+.2f%% to %+.2f%% of acknowledged users [measured].",
		prefix, c.InRange, c.Segments, c.OutOfRange, c.Failed, c.MinErrorPercent, c.MaxErrorPercent)
}

func percent(v float64) string { return fmt.Sprintf("%.2f%%", v) }
func ms(v float64) string      { return fmt.Sprintf("%.1f ms", v) }

func rate(part, whole int64) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole) * 100
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func passFail(v bool) string {
	if v {
		return "pass"
	}
	return "fail"
}

func enabled(v bool) string {
	if v {
		return "enabled"
	}
	return "not enabled"
}

func cleanText(v bool) string {
	if v {
		return "clean"
	}
	return "problems found"
}

// formatCount writes a whole number with thousands separators.
func formatCount(v float64) string {
	digits := strconv.FormatInt(int64(v), 10)
	var b strings.Builder
	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(digit)
	}
	return b.String()
}

// shortRate is the axis label of a rate. It is the only place that rounds a rate.
func shortRate(v float64) string {
	if v >= 1e6 {
		return fmt.Sprintf("%.2fM", v/1e6)
	}
	return fmt.Sprintf("%.0fk", v/1e3)
}
