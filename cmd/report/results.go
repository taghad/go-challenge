package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The types below match docs/results/*.json. Every value in them comes from a real run.
// A file that is missing or has an unknown field stops the generator, so that a chart can
// never be drawn from a value that is not there.

type machine struct {
	Model    string `json:"model"`
	Chip     string `json:"chip"`
	CPUs     int    `json:"cpus"`
	MemoryGB int    `json:"memory_gb"`
	OS       string `json:"os"`
	Docker   string `json:"docker"`
	Redis    string `json:"redis"`
	Go       string `json:"go"`
	Note     string `json:"note"`
}

type estimateCheck struct {
	Segments        int     `json:"segments"`
	InRange         int     `json:"in_range"`
	OutOfRange      int     `json:"out_of_range"`
	Failed          int     `json:"failed"`
	MinErrorPercent float64 `json:"min_error_percent"`
	MaxErrorPercent float64 `json:"max_error_percent"`
}

type services struct {
	ES        float64 `json:"es"`
	Redis     float64 `json:"redis"`
	Simulator float64 `json:"simulator"`
}

type rampStep struct {
	OfferedEventsPerSecond float64  `json:"offered_events_per_second"`
	FirstAcceptancePercent float64  `json:"first_attempt_acceptance_percent"`
	FirstP99Ms             float64  `json:"first_attempt_p99_ms"`
	FirstAttempts          int64    `json:"first_attempts"`
	Rejected503            int64    `json:"rejected_503"`
	Timeouts               int64    `json:"timeouts"`
	FinalAcceptancePercent float64  `json:"final_acceptance_percent"`
	AbandonedEvents        int64    `json:"abandoned_events"`
	SendLagP99Ms           float64  `json:"send_lag_p99_ms"`
	Saturated              bool     `json:"saturated"`
	SaturationReasons      []string `json:"saturation_reasons"`
	CPUMeanPercent         services `json:"cpu_mean_percent"`
	MemoryMaxMiB           services `json:"memory_max_mib"`
}

type rampResult struct {
	Source              string        `json:"source"`
	BucketSize          string        `json:"bucket_size"`
	Batch               int           `json:"batch"`
	StepSeconds         int           `json:"step_seconds"`
	Users               int           `json:"users"`
	Segments            int           `json:"segments"`
	StoppedBy           string        `json:"stopped_by"`
	HighestSustained    float64       `json:"highest_sustained_events_per_second"`
	ESStoreFailures     int           `json:"es_store_failures"`
	ESStorePoolTimeouts int           `json:"es_store_pool_timeouts"`
	ESRestarts          int           `json:"es_restarts"`
	Steps               []rampStep    `json:"steps"`
	EstimateCheck       estimateCheck `json:"estimate_check"`
}

type burstAttempts struct {
	Attempts          int64   `json:"attempts"`
	AcceptancePercent float64 `json:"acceptance_percent"`
	Rejected503       int64   `json:"rejected_503"`
	Timeouts          int64   `json:"timeouts"`
	NetworkErrors     int64   `json:"network_errors"`
}

type timelineSecond struct {
	Second              int     `json:"second"`
	FirstAttempts       int64   `json:"first_attempts"`
	FirstAcknowledged   int64   `json:"first_acknowledged"`
	Retries             int64   `json:"retries"`
	RetriesAcknowledged int64   `json:"retries_acknowledged"`
	Rejected503         int64   `json:"rejected_503"`
	MaxLatencyMs        float64 `json:"max_latency_ms"`
}

type burstResult struct {
	Source                 string           `json:"source"`
	BucketSize             string           `json:"bucket_size"`
	BaseEventsPerSecond    float64          `json:"base_events_per_second"`
	Multiplier             float64          `json:"multiplier"`
	PeakStartSeconds       int              `json:"peak_start_seconds"`
	PeakDurationSeconds    int              `json:"peak_duration_seconds"`
	DurationSeconds        int              `json:"duration_seconds"`
	OfferedEvents          int64            `json:"offered_events"`
	FirstAttempts          burstAttempts    `json:"first_attempts"`
	Retries                burstAttempts    `json:"retries"`
	FinalAcceptancePercent float64          `json:"final_acceptance_percent"`
	AbandonedEvents        int64            `json:"abandoned_events"`
	FinalP99Ms             float64          `json:"final_latency_p99_ms"`
	SendLagP99Ms           float64          `json:"send_lag_p99_ms"`
	PeakCPUPercent         services         `json:"peak_cpu_percent"`
	PeakMemoryMiB          services         `json:"peak_memory_mib"`
	ESStoreFailures        int              `json:"es_store_failures"`
	ESRestarts             int              `json:"es_restarts"`
	Timeline               []timelineSecond `json:"timeline"`
	EstimateCheck          estimateCheck    `json:"estimate_check"`
}

type hllResult struct {
	Source  string `json:"source"`
	Results []struct {
		Users         int       `json:"users"`
		ErrorsPercent []float64 `json:"errors_percent"`
	} `json:"results"`
}

type boundaryCase struct {
	Profile        string  `json:"profile"`
	ReturnShare    int     `json:"return_share_percent"`
	RealizedReturn float64 `json:"realized_return_percent"`
	Bucket         string  `json:"bucket"`
	MeanUndercount float64 `json:"mean_undercount_percent"`
	MaxUndercount  float64 `json:"max_undercount_percent"`
}

type boundaryResult struct {
	Source  string         `json:"source"`
	Users   int            `json:"users"`
	Seed    int            `json:"seed"`
	Samples int            `json:"samples"`
	Cases   []boundaryCase `json:"cases"`
}

type benchRow struct {
	Bucket             string  `json:"bucket"`
	Keys               int     `json:"keys"`
	UsedMemoryBytes    int64   `json:"used_memory_bytes"`
	KeyBytes           int     `json:"key_bytes"`
	P50Ms              float64 `json:"client_p50_ms"`
	P99Ms              float64 `json:"client_p99_ms"`
	UsecPerCall        float64 `json:"usec_per_call"`
	SharePercent       float64 `json:"recomputation_share_percent"`
	ClientSharePercent float64 `json:"client_mean_share_percent"`
}

type benchResult struct {
	Source   string     `json:"source"`
	Segments int        `json:"segments"`
	Samples  int        `json:"samples"`
	Rows     []benchRow `json:"rows"`
}

type boundaryBurstRow struct {
	Bucket      string  `json:"bucket"`
	After503    float64 `json:"estimate_503_percent_after_boundary"`
	Warmup503   float64 `json:"estimate_503_percent_warmup"`
	P50Ms       float64 `json:"estimate_p50_ms"`
	P99Ms       float64 `json:"estimate_p99_ms"`
	Add503      int     `json:"add_503"`
	AddRequests int     `json:"add_requests"`
	AddP99Ms    float64 `json:"add_p99_ms"`
	AllServedMs float64 `json:"all_segments_served_ms"`
}

type boundaryBurstResult struct {
	Source string             `json:"source"`
	Rows   []boundaryBurstRow `json:"rows"`
}

type packageResult struct {
	Name            string   `json:"name"`
	Passed          int      `json:"passed"`
	Failed          int      `json:"failed"`
	Skipped         int      `json:"skipped"`
	CoveragePercent *float64 `json:"coverage_percent"`
}

type vulnFinding struct {
	ID    string `json:"id"`
	Level string `json:"level"`
}

type qualityResult struct {
	Source             string          `json:"source"`
	GeneratedAt        string          `json:"generated_at"`
	Go                 string          `json:"go"`
	TestCommand        string          `json:"test_command"`
	TestsPassed        bool            `json:"tests_passed"`
	RaceEnabled        bool            `json:"race_enabled"`
	Packages           []packageResult `json:"packages"`
	VetClean           bool            `json:"vet_clean"`
	GofmtClean         bool            `json:"gofmt_clean"`
	GovulncheckVersion string          `json:"govulncheck_version"`
	Findings           []vulnFinding   `json:"govulncheck_findings"`
}

type results struct {
	Machine       machine
	Ramp          rampResult
	Burst         burstResult
	HLL           hllResult
	Boundary      boundaryResult
	Bench         benchResult
	BoundaryBurst boundaryBurstResult
	Quality       qualityResult
}

func loadJSON(dir, name string, into any) error {
	file, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func loadResults(dir string) (results, error) {
	var r results
	err := errors.Join(
		loadJSON(dir, "machine.json", &r.Machine),
		loadJSON(dir, "ramp.json", &r.Ramp),
		loadJSON(dir, "burst.json", &r.Burst),
		loadJSON(dir, "hll.json", &r.HLL),
		loadJSON(dir, "boundary.json", &r.Boundary),
		loadJSON(dir, "bench.json", &r.Bench),
		loadJSON(dir, "boundary_burst.json", &r.BoundaryBurst),
		loadJSON(dir, "quality.json", &r.Quality),
	)
	if err != nil {
		return results{}, err
	}
	return r, r.validate()
}

// validate rejects results that are too incomplete to report. It checks shape only, and it
// never fills anything in.
func (r results) validate() error {
	var errs []error
	check := func(ok bool, message string) {
		if !ok {
			errs = append(errs, errors.New(message))
		}
	}
	check(r.Machine.Model != "", "machine.json has no model")
	check(len(r.Ramp.Steps) > 0, "ramp.json has no steps")
	check(len(r.Burst.Timeline) > 0, "burst.json has no timeline")
	for i, t := range r.Burst.Timeline {
		check(t.Second == i, fmt.Sprintf("burst.json: timeline entry %d has second %d", i, t.Second))
	}
	check(len(r.HLL.Results) > 0, "hll.json has no results")
	check(len(r.Boundary.Cases) == 8, "boundary.json must have 8 cases")
	_, have6h := r.Bench.row("6h")
	_, have1h := r.Bench.row("1h")
	check(have6h && have1h, "bench.json must have 6h and 1h")
	_, haveScenario := r.Boundary.find("uniform", 50, "6h")
	check(haveScenario, "boundary.json has no decision scenario")
	check(len(r.BoundaryBurst.Rows) > 0, "boundary_burst.json has no rows")
	check(len(r.Quality.Packages) > 0, "quality.json has no packages")
	return errors.Join(errs...)
}

func (b benchResult) row(bucket string) (benchRow, bool) {
	for _, row := range b.Rows {
		if row.Bucket == bucket {
			return row, true
		}
	}
	return benchRow{}, false
}

func (b boundaryResult) find(profile string, returnShare int, bucket string) (boundaryCase, bool) {
	for _, c := range b.Cases {
		if c.Profile == profile && c.ReturnShare == returnShare && c.Bucket == bucket {
			return c, true
		}
	}
	return boundaryCase{}, false
}

// recovery returns the first second of the timeline from which every attempt was
// acknowledged, and whether there is one at or after the end of the peak. The result is
// calculated from measured counts, at a resolution of one second.
func recovery(timeline []timelineSecond, peakEnd int) (int, bool) {
	second := peakEnd
	for _, t := range timeline {
		if unacknowledged(t) > 0 && t.Second >= second {
			second = t.Second + 1
		}
	}
	return second, second < len(timeline)
}

func unacknowledged(t timelineSecond) int64 {
	return t.FirstAttempts + t.Retries - t.FirstAcknowledged - t.RetriesAcknowledged
}
