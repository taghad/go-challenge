package main

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const repositoryRoot = "../.."

func committedResults(t *testing.T) results {
	t.Helper()
	r, err := loadResults(filepath.Join(repositoryRoot, "docs", "results"))
	if err != nil {
		t.Fatalf("loadResults: %v", err)
	}
	return r
}

// The committed report must be what the committed results produce, so nobody can edit a
// number by hand or change a result without regenerating the report.
func TestCommittedReportMatchesResults(t *testing.T) {
	for _, file := range generate(committedResults(t)) {
		got, err := os.ReadFile(filepath.Join(repositoryRoot, file.path))
		if err != nil {
			t.Errorf("read generated file: %v", err)
			continue
		}
		if string(got) != file.content {
			t.Errorf("%s is out of date; run go run ./cmd/report", file.path)
		}
	}
}

func TestGenerateIsRepeatable(t *testing.T) {
	r := committedResults(t)
	first, second := generate(r), generate(r)
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("%s differs between two runs", first[i].path)
		}
	}
}

func TestChartsAreWellFormedAndComplete(t *testing.T) {
	r := committedResults(t)
	want := map[string]bool{"ramp": true, "burst": true, "boundary": true, "rules": true, "hll": true, "coverage": true}
	for _, s := range buildSections(r) {
		if s.Chart == "" {
			continue
		}
		delete(want, s.Chart)
		if err := xml.Unmarshal([]byte(s.Canvas.svg(svgStyle)), new(struct{ XMLName xml.Name })); err != nil {
			t.Errorf("%s is not valid XML: %v", s.Chart, err)
		}
	}
	for chart := range want {
		t.Errorf("chart %s is missing", chart)
	}
}

// Nothing joins two points, and there is exactly one mark per real value, so a chart
// cannot show a point that was not measured.
func TestChartsDrawOneMarkPerValueAndNoConnectingLine(t *testing.T) {
	r := committedResults(t)
	values := 0
	for _, item := range r.HLL.Results {
		values += len(item.ErrorsPercent)
	}
	cases := []struct {
		name  string
		chart *canvas
		marks int
	}{
		{"ramp", rampChart(r.Ramp), 2 * len(r.Ramp.Steps)},
		{"hll", hllChart(r.HLL), values},
		{"boundary", boundaryChart(r.Boundary), len(r.Boundary.Cases)},
	}
	for _, c := range cases {
		svg := c.chart.svg("")
		if got := strings.Count(svg, "<circle"); got != c.marks {
			t.Errorf("%s has %d dots, want %d", c.name, got, c.marks)
		}
		for _, joined := range []string{"<path", "<polyline", "<polygon"} {
			if strings.Contains(svg, joined) {
				t.Errorf("%s uses %s", c.name, joined)
			}
		}
	}
}

func TestBurstChartHasNoBarForASecondWithoutAttempts(t *testing.T) {
	b := burstResult{PeakStartSeconds: 0, PeakDurationSeconds: 1, Timeline: []timelineSecond{
		{Second: 0, FirstAttempts: 2, FirstAcknowledged: 1, Rejected503: 1},
		{Second: 1},
		{Second: 2, FirstAttempts: 1, FirstAcknowledged: 1},
	}}
	if got := strings.Count(burstChart(b).svg(""), `class="s1"><title>Second`); got != 2 {
		t.Errorf("acknowledged bars = %d, want 2", got)
	}
	if got := strings.Count(burstChart(b).svg(""), `class="s2"><title>Second`); got != 1 {
		t.Errorf("rejected bars = %d, want 1", got)
	}
}

func TestRecovery(t *testing.T) {
	ok := timelineSecond{FirstAttempts: 2, FirstAcknowledged: 2}
	bad := timelineSecond{FirstAttempts: 2, FirstAcknowledged: 1, Rejected503: 1}
	timeline := func(seconds ...timelineSecond) []timelineSecond {
		for i := range seconds {
			seconds[i].Second = i
		}
		return seconds
	}
	cases := []struct {
		name     string
		timeline []timelineSecond
		peakEnd  int
		second   int
		found    bool
	}{
		{"failures end with the peak", timeline(bad, bad, ok, ok), 2, 2, true},
		{"failures continue after the peak", timeline(bad, bad, bad, ok), 2, 3, true},
		{"a later failure moves it", timeline(bad, ok, bad, ok), 1, 3, true},
		{"failures before the peak end do not count", timeline(bad, ok, ok, ok), 2, 2, true},
		{"never recovers in the run", timeline(bad, bad), 1, 2, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			second, found := recovery(c.timeline, c.peakEnd)
			if second != c.second || found != c.found {
				t.Errorf("recovery = %d, %t, want %d, %t", second, found, c.second, c.found)
			}
		})
	}
}

func TestDecisionRulesUseTheRealLimits(t *testing.T) {
	r := committedResults(t)
	rules := decisionRules(r)
	if len(rules) != 4 {
		t.Fatalf("rules = %d, want 4", len(rules))
	}
	r.Bench.Rows[1].P99Ms = 21
	if rules := decisionRules(r); r.Bench.Rows[1].Bucket == "1h" && rules[1].Holds {
		t.Error("rule 2 holds above 20 ms")
	}
}

func TestLoadResultsRejectsMissingAndUnknownData(t *testing.T) {
	if _, err := loadResults(t.TempDir()); err == nil {
		t.Error("an empty directory was accepted")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "machine.json"), []byte(`{"invented":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var m machine
	if err := loadJSON(dir, "machine.json", &m); err == nil {
		t.Error("an unknown field was accepted")
	}
}

func TestValidateRejectsAGapInTheTimeline(t *testing.T) {
	r := committedResults(t)
	r.Burst.Timeline = append(r.Burst.Timeline, timelineSecond{Second: len(r.Burst.Timeline) + 1})
	if err := r.validate(); err == nil {
		t.Error("a timeline with a gap was accepted")
	}
}

func TestReportShowsMachineAndCapacityWarningFirst(t *testing.T) {
	r := committedResults(t)
	sections := buildSections(r)
	for name, content := range map[string]string{"markdown": renderMarkdown(r, sections), "page": renderPage(r, sections)} {
		warning := strings.Index(content, "Not production capacity")
		model := strings.Index(content, r.Machine.Model)
		first := strings.Index(content, sections[0].Title)
		if warning < 0 || model < 0 || warning > first || model > first {
			t.Errorf("%s: warning at %d, machine at %d, first section at %d", name, warning, model, first)
		}
		for _, label := range []string{"[measured]", "[calculated]"} {
			if !strings.Contains(content, label) {
				t.Errorf("%s does not use %s", name, label)
			}
		}
	}
}

func TestPageLoadsNothingExternal(t *testing.T) {
	r := committedResults(t)
	page := renderPage(r, buildSections(r))
	// The SVG namespace is an identifier, and it is never fetched.
	page = strings.ReplaceAll(page, "http://www.w3.org/2000/svg", "")
	for _, external := range []string{"http://", "https://", "<link", "<script", "@import", "url("} {
		if strings.Contains(page, external) {
			t.Errorf("page contains %s", external)
		}
	}
	for _, want := range []string{"prefers-color-scheme:dark", "color-scheme:light dark", `name="viewport"`, "overflow-x:auto"} {
		if !strings.Contains(page, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
}

func TestFormatCount(t *testing.T) {
	cases := map[float64]string{0: "0", 999: "999", 1000: "1,000", 2048000: "2,048,000"}
	for value, want := range cases {
		if got := formatCount(value); got != want {
			t.Errorf("formatCount(%v) = %q, want %q", value, got, want)
		}
	}
}

func TestParseTestEvents(t *testing.T) {
	stream := strings.NewReader(`{"Action":"pass","Package":"github.com/taghad/go-challenge/a","Test":"T1"}
{"Action":"fail","Package":"github.com/taghad/go-challenge/a","Test":"T2"}
{"Action":"skip","Package":"github.com/taghad/go-challenge/a","Test":"T3"}
{"Action":"output","Package":"github.com/taghad/go-challenge/a","Output":"coverage: 81.5% of statements\n"}
{"Action":"pass","Package":"github.com/taghad/go-challenge/a"}
{"Action":"pass","Package":"github.com/taghad/go-challenge/b","Test":"T1"}
`)
	packages, err := parseTestEvents(stream)
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 2 {
		t.Fatalf("packages = %+v", packages)
	}
	a, b := packages[0], packages[1]
	if a.Name != "a" || a.Passed != 1 || a.Failed != 1 || a.Skipped != 1 || a.CoveragePercent == nil || *a.CoveragePercent != 81.5 {
		t.Errorf("a = %+v", a)
	}
	if b.CoveragePercent != nil {
		t.Errorf("b has invented coverage: %v", *b.CoveragePercent)
	}
}

func TestParseVulnFindingsKeepsTheDeepestLevel(t *testing.T) {
	stream := strings.NewReader(`{"config":{"go_version":"go1"}}
{"finding":{"osv":"GO-1","trace":[{"module":"m"}]}}
{"finding":{"osv":"GO-1","trace":[{"module":"m","package":"p"}]}}
{"finding":{"osv":"GO-2","trace":[{"module":"m","package":"p","function":"f"}]}}
{"finding":{"osv":"GO-3","trace":[{"module":"m"}]}}
`)
	findings, err := parseVulnFindings(stream)
	if err != nil {
		t.Fatal(err)
	}
	want := []vulnFinding{{"GO-1", "package"}, {"GO-2", "symbol"}, {"GO-3", "module"}}
	if len(findings) != len(want) {
		t.Fatalf("findings = %+v", findings)
	}
	for i := range want {
		if findings[i] != want[i] {
			t.Errorf("finding %d = %+v, want %+v", i, findings[i], want[i])
		}
	}
}
