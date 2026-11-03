package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/taghad/go-challenge/internal/estimator"
)

func stepReport(offered, acknowledged int64, p99, lag float64) report {
	return report{
		OfferedEvents: offered,
		FirstAttempts: attemptReport{AcknowledgedEvents: acknowledged, LatencyMs: latencySummary{P99: p99}},
		SendLagMs:     latencySummary{P99: lag},
	}
}

func TestSaturationRule(t *testing.T) {
	tests := []struct {
		name   string
		report report
		want   []string
	}{
		{"all accepted and fast", stepReport(1000, 1000, 10, 0), nil},
		{"acceptance at the limit", stepReport(1000, 990, 10, 0), nil},
		{"acceptance below the limit", stepReport(1000, 989, 10, 0), []string{"first_attempt_acceptance"}},
		{"p99 at the limit", stepReport(1000, 1000, 500, 0), nil},
		{"p99 above the limit", stepReport(1000, 1000, 500.1, 0), []string{"first_attempt_p99"}},
		{"both", stepReport(1000, 0, 501, 0), []string{"first_attempt_acceptance", "first_attempt_p99"}},
		{"nothing offered", stepReport(0, 0, 0, 0), []string{"first_attempt_acceptance"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := saturationReasons(tt.report); !slices.Equal(got, tt.want) {
				t.Errorf("reasons = %v, want %v", got, tt.want)
			}
		})
	}
}

// A batch that was scheduled but never sent counts against acceptance, so a simulator
// that falls behind cannot look like a healthy ES.
func TestSaturationUsesOfferedEvents(t *testing.T) {
	r := stepReport(1000, 500, 10, 0)
	r.FirstAttempts.Events = 500
	if got := saturationReasons(r); !slices.Equal(got, []string{"first_attempt_acceptance"}) {
		t.Errorf("reasons = %v", got)
	}
}

// Retries must not hide an overload.
func TestSaturationIgnoresRetries(t *testing.T) {
	r := stepReport(1000, 0, 10, 0)
	r.Retries = attemptReport{AcknowledgedEvents: 1000}
	r.Final = finalReport{AcknowledgedEvents: 1000, Acceptance: 1}
	if got := saturationReasons(r); !slices.Equal(got, []string{"first_attempt_acceptance"}) {
		t.Errorf("reasons = %v", got)
	}
}

func TestSimulatorLagThreshold(t *testing.T) {
	if newRampStep(stepReport(1, 1, 1, 100)).SimulatorLimited {
		t.Error("a p99 lag of 100 ms is flagged, want only above it")
	}
	if !newRampStep(stepReport(1, 1, 1, 100.1)).SimulatorLimited {
		t.Error("a p99 lag above 100 ms is not flagged")
	}
}

func rampOptions(url string) options {
	o := testOptions(url)
	o.rate = 1000
	o.batch = 10
	o.duration = 100 * time.Millisecond
	o.maxRetries = 0
	o.ramp = true
	o.rampMaxSteps = 12
	return o
}

// rejectAfter accepts the first n requests and then answers 503.
func rejectAfter(limit int) func(int, http.ResponseWriter, *http.Request) {
	return func(n int, w http.ResponseWriter, r *http.Request) {
		if n >= limit {
			reject503(w)
			return
		}
		accept(n, w, r)
	}
}

func TestRampDoublesUntilSaturation(t *testing.T) {
	// 10 batches in the first step and 20 in the second, so the third is rejected.
	es := newFakeES(t, rejectAfter(30))

	r := runRamp(context.Background(), rampOptions(es.URL), newClient())

	if len(r.Steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(r.Steps))
	}
	for i, want := range []float64{1000, 2000, 4000} {
		if got := r.Steps[i].OfferedEventsPerSecond; got != want {
			t.Errorf("step %d offered = %f, want %f", i, got, want)
		}
	}
	if r.Steps[1].Saturated {
		t.Errorf("step 1 is saturated: %v", r.Steps[1].SaturationReasons)
	}
	if !r.Steps[2].Saturated {
		t.Error("step 2 is not saturated")
	}
	if r.StoppedBy != stoppedBySaturation {
		t.Errorf("stopped by %q", r.StoppedBy)
	}
	if r.HighestSustainedEventsPerSecond != 2000 {
		t.Errorf("highest sustained = %f, want 2000", r.HighestSustainedEventsPerSecond)
	}
}

func TestRampStopsAtMaxSteps(t *testing.T) {
	es := newFakeES(t, accept)
	o := rampOptions(es.URL)
	o.rampMaxSteps = 2

	r := runRamp(context.Background(), o, newClient())

	if len(r.Steps) != 2 || r.StoppedBy != stoppedByMaxSteps {
		t.Errorf("steps = %d, stopped by %q", len(r.Steps), r.StoppedBy)
	}
	if r.HighestSustainedEventsPerSecond != 2000 {
		t.Errorf("highest sustained = %f, want 2000", r.HighestSustainedEventsPerSecond)
	}
}

func TestRampStopsWhenInterrupted(t *testing.T) {
	es := newFakeES(t, accept)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := runRamp(ctx, rampOptions(es.URL), newClient())

	if len(r.Steps) != 1 || r.StoppedBy != stoppedByInterrupt {
		t.Errorf("steps = %d, stopped by %q", len(r.Steps), r.StoppedBy)
	}
}

// If each step restarted its batch index, a later step would repeat the users of the
// first one, and the ramp would stop finding new users.
func TestRampStepsDrawNewEvents(t *testing.T) {
	es := newFakeES(t, accept)
	o := rampOptions(es.URL)
	o.rampMaxSteps = 2

	runRamp(context.Background(), o, newClient())

	seen := make(map[string]bool)
	for _, body := range es.bodies {
		if seen[string(body)] {
			t.Fatal("two batches of a ramp have the same body")
		}
		seen[string(body)] = true
	}
	if len(seen) != 30 {
		t.Errorf("batches = %d, want 30", len(seen))
	}
}

// fakeEstimates acknowledges events and tracks the users of each segment, so that it
// can answer estimates with the true count, changed by the given function.
func fakeEstimates(t *testing.T, change func(uint64) uint64) *fakeES {
	t.Helper()
	users := make(map[string]map[string]bool)
	var es *fakeES
	es = newFakeES(t, func(n int, w http.ResponseWriter, r *http.Request) {
		es.mu.Lock()
		defer es.mu.Unlock()
		if r.Method == http.MethodPost {
			var body struct {
				Events []estimator.Event `json:"events"`
			}
			if err := json.Unmarshal(es.bodies[n], &body); err != nil {
				t.Errorf("decode: %v", err)
			}
			for _, event := range body.Events {
				if users[event.Segment] == nil {
					users[event.Segment] = make(map[string]bool)
				}
				users[event.Segment][event.UserID] = true
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		segment := strings.Split(r.URL.Path, "/")[3]
		fmt.Fprintf(w, `{"estimate":%d}`, change(uint64(len(users[segment]))))
	})
	return es
}

func TestRampCheckComparesEstimatesWithAllSteps(t *testing.T) {
	es := fakeEstimates(t, func(count uint64) uint64 { return count })
	o := rampOptions(es.URL)
	o.rampMaxSteps = 2
	o.checkEstimates = true

	r := runRamp(context.Background(), o, newClient())

	check := r.EstimateCheck
	if check == nil {
		t.Fatal("no estimate check")
	}
	if check.Segments != o.segments || check.InRange != o.segments || check.OutOfRange != 0 || check.Failed != 0 {
		t.Errorf("check = %+v", check)
	}
	var total int64
	for _, result := range check.Results {
		total += result.Acknowledged
		if int64(result.Estimate) != result.Acknowledged {
			t.Errorf("%s estimate = %d, acknowledged = %d", result.Segment, result.Estimate, result.Acknowledged)
		}
	}
	if total == 0 {
		t.Error("no users were tracked")
	}
}

func TestRampChecksOnlyWhenAsked(t *testing.T) {
	es := newFakeES(t, accept)
	o := rampOptions(es.URL)
	o.rampMaxSteps = 1

	r := runRamp(context.Background(), o, newClient())

	if r.EstimateCheck != nil {
		t.Error("the ramp ran a check that was not asked for")
	}
	for i, step := range r.Steps {
		if step.Segments != nil {
			t.Errorf("step %d has per-segment counts", i)
		}
	}
}

func TestEstimateRange(t *testing.T) {
	low, high := estimateRange(userCounts{Acknowledged: 1000, Attempted: 2000})
	if want := 975.7; low < want-1e-9 || low > want+1e-9 {
		t.Errorf("low = %f, want %f", low, want)
	}
	if want := 2048.6; high < want-1e-9 || high > want+1e-9 {
		t.Errorf("high = %f, want %f", high, want)
	}
}

func checkOptions(url string) options {
	o := testOptions(url)
	o.segments = 1
	o.maxRetries = 1
	return o
}

func TestCheckEstimatesFindsOutOfRange(t *testing.T) {
	tests := []struct {
		name     string
		estimate uint64
		inRange  bool
	}{
		{"below the range", 975, false},
		{"at the bottom", 976, true},
		{"between", 1500, true},
		{"at the top", 2048, true},
		{"above the range", 2049, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			es := newFakeES(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"estimate":%d}`, tt.estimate)
			})
			counts := []segmentCounts{{Segment: "seg-0", userCounts: userCounts{Acknowledged: 1000, Attempted: 2000}}}

			check := checkEstimates(context.Background(), newClient(), checkOptions(es.URL), counts)

			if check.Results[0].Estimate != tt.estimate {
				t.Errorf("estimate = %d", check.Results[0].Estimate)
			}
			if check.Results[0].InRange != tt.inRange {
				t.Errorf("in range = %v, want %v", check.Results[0].InRange, tt.inRange)
			}
			if got := check.InRange + check.OutOfRange; got != 1 || check.Failed != 0 {
				t.Errorf("check = %+v", check)
			}
		})
	}
}

func TestCheckEstimatesRetries503(t *testing.T) {
	es := newFakeES(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 0 {
			reject503(w)
			return
		}
		fmt.Fprint(w, `{"estimate":1000}`)
	})
	counts := []segmentCounts{{Segment: "seg-0", userCounts: userCounts{Acknowledged: 1000, Attempted: 1000}}}

	check := checkEstimates(context.Background(), newClient(), checkOptions(es.URL), counts)

	if check.InRange != 1 {
		t.Errorf("check = %+v", check)
	}
}

func TestCheckEstimatesCountsAFailureApart(t *testing.T) {
	es := newFakeES(t, func(_ int, w http.ResponseWriter, _ *http.Request) { reject503(w) })
	counts := []segmentCounts{{Segment: "seg-0", userCounts: userCounts{Acknowledged: 10, Attempted: 10}}}

	check := checkEstimates(context.Background(), newClient(), checkOptions(es.URL), counts)

	if check.Failed != 1 || check.OutOfRange != 0 || check.InRange != 0 {
		t.Errorf("check = %+v", check)
	}
	if check.Results[0].Error == "" {
		t.Error("the failure has no error")
	}
}
