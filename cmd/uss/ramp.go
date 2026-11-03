package main

import (
	"context"
	"net/http"
)

// The saturation rule and its thresholds are fixed in docs/DESIGN.md. They are owner
// decisions and assumptions, and they must not change after results are seen.
const (
	saturationAcceptance = 0.99
	saturationP99Ms      = 500.0
	// simulatorLagP99Ms flags a step in which batches left late, because the simulator
	// itself may then be the limit [E].
	simulatorLagP99Ms = 100.0
)

// Why a ramp stopped.
const (
	stoppedBySaturation   = "saturation"
	stoppedBySimulatorLag = "simulator_lag"
	stoppedByMaxSteps     = "max_steps"
	stoppedByInterrupt    = "interrupted"
)

// rampStep is the report of one steady run, with the rule applied to it. Its distinct
// users are cumulative across the steps so far.
type rampStep struct {
	report
	Saturated         bool     `json:"saturated"`
	SaturationReasons []string `json:"saturation_reasons,omitempty"`
	SimulatorLimited  bool     `json:"simulator_limited"`
}

type rampReport struct {
	Config                          map[string]string `json:"config"`
	Steps                           []rampStep        `json:"steps"`
	StoppedBy                       string            `json:"stopped_by"`
	HighestSustainedEventsPerSecond float64           `json:"highest_sustained_events_per_second"`
	DistinctUsers                   userCounts        `json:"distinct_users"`
	EstimateCheck                   *estimateCheck    `json:"estimate_check,omitempty"`
}

// saturationReasons applies the rule: first-attempt acceptance below 99% of offered
// events, or first-attempt p99 above 500 ms. Retries do not count, because enough of
// them could hide an overload. Acceptance is of offered events, so a batch that was
// scheduled and never sent counts against it.
func saturationReasons(r report) []string {
	var reasons []string
	if share(r.FirstAttempts.AcknowledgedEvents, r.OfferedEvents) < saturationAcceptance {
		reasons = append(reasons, "first_attempt_acceptance")
	}
	if r.FirstAttempts.LatencyMs.P99 > saturationP99Ms {
		reasons = append(reasons, "first_attempt_p99")
	}
	return reasons
}

func newRampStep(r report) rampStep {
	reasons := saturationReasons(r)
	return rampStep{
		report:            r,
		Saturated:         len(reasons) > 0,
		SaturationReasons: reasons,
		SimulatorLimited:  r.SendLagMs.P99 > simulatorLagP99Ms,
	}
}

// runRamp starts at the rate of the options and doubles it at each step. Each step is
// one steady run, and it waits for the retries of the step before it, so a step's load
// is its own. The steps share the same segments, and each draws new events, so the
// final check compares each segment with all the users the ramp sent to it.
//
// A step limited by the simulator ends the ramp, because later steps could not say
// anything about ES.
func runRamp(ctx context.Context, o options, client *http.Client) rampReport {
	users := newUserTracker(o.users, o.segments)
	result := rampReport{Config: o.flags, StoppedBy: stoppedByMaxSteps}
	var firstBatch uint64
	step := o
	for range o.rampMaxSteps {
		run := newStepSimulator(step, client, users, firstBatch).run(ctx)
		firstBatch += uint64(run.ScheduledBatches)
		run.Config = nil
		run.Segments = nil
		current := newRampStep(run)
		result.Steps = append(result.Steps, current)
		if run.Interrupted {
			result.StoppedBy = stoppedByInterrupt
			break
		}
		if current.SimulatorLimited {
			result.StoppedBy = stoppedBySimulatorLag
			break
		}
		if current.Saturated {
			result.StoppedBy = stoppedBySaturation
			break
		}
		result.HighestSustainedEventsPerSecond = run.OfferedEventsPerSecond
		step.rate *= 2
	}
	result.DistinctUsers = users.total()
	if o.checkEstimates {
		check := checkEstimates(ctx, client, o, users.perSegment(o.segmentPrefix))
		result.EstimateCheck = &check
	}
	return result
}
