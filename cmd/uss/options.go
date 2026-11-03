package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/taghad/go-challenge/internal/estimator"
)

const (
	profileSteady = "steady" // constant rate
	profileBurst  = "burst"  // constant rate, multiplied for a short window
)

// options holds one run of the simulator. Every default is an estimate [E].
type options struct {
	url             string
	rate            float64 // offered events per second
	duration        time.Duration
	batch           int
	users           int
	segments        int
	segmentPrefix   string
	seed            uint64
	profile         string
	burstMultiplier float64
	burstStart      time.Duration
	burstDuration   time.Duration
	timeout         time.Duration // one attempt
	maxRetries      int
	retryJitter     time.Duration // upper limit on the time added to Retry-After
	flags           map[string]string
}

func parseOptions(args []string) (options, error) {
	var o options
	flags := flag.NewFlagSet("uss", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&o.url, "url", "http://es:8080", "base URL of ES")
	flags.Float64Var(&o.rate, "rate", 1000, "offered events per second")
	flags.DurationVar(&o.duration, "duration", 30*time.Second, "length of the schedule")
	flags.IntVar(&o.batch, "batch", 500, "events per request")
	flags.IntVar(&o.users, "users", 1000000, "number of user IDs to draw from")
	flags.IntVar(&o.segments, "segments", 500, "number of segments to draw from")
	flags.StringVar(&o.segmentPrefix, "segment-prefix", "uss-", "prefix of segment names, so that one run can be told from another")
	flags.Uint64Var(&o.seed, "seed", 1, "seed of the events and the jitter")
	flags.StringVar(&o.profile, "profile", profileSteady, "traffic profile: steady or burst")
	flags.Float64Var(&o.burstMultiplier, "burst-multiplier", 10, "burst: rate multiplier during the window")
	flags.DurationVar(&o.burstStart, "burst-start", 10*time.Second, "burst: start of the window")
	flags.DurationVar(&o.burstDuration, "burst-duration", 5*time.Second, "burst: length of the window")
	flags.DurationVar(&o.timeout, "timeout", 2*time.Second, "timeout of one attempt")
	flags.IntVar(&o.maxRetries, "max-retries", 3, "retries per batch after the first attempt")
	flags.DurationVar(&o.retryJitter, "retry-jitter", time.Second, "upper limit on the random time added to Retry-After")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	o.flags = make(map[string]string)
	flags.VisitAll(func(f *flag.Flag) { o.flags[f.Name] = f.Value.String() })
	if err := o.validate(); err != nil {
		return options{}, err
	}
	return o, nil
}

func (o options) validate() error {
	var errs []error
	check := func(ok bool, message string) {
		if !ok {
			errs = append(errs, errors.New(message))
		}
	}
	check(o.url != "", "-url must not be empty")
	check(o.rate > 0, "-rate must be positive")
	check(o.duration > 0, "-duration must be positive")
	check(o.batch > 0, "-batch must be positive")
	check(o.users > 0, "-users must be positive")
	check(o.segments > 0, "-segments must be positive")
	check(o.timeout > 0, "-timeout must be positive")
	check(o.maxRetries >= 0, "-max-retries must not be negative")
	check(o.retryJitter >= 0, "-retry-jitter must not be negative")
	if o.segments > 0 {
		last := segmentName(o.segmentPrefix, o.segments-1)
		check(estimator.ValidateSegment(last) == nil, "-segment-prefix makes the invalid segment name "+last)
	}
	switch o.profile {
	case profileSteady:
	case profileBurst:
		check(o.burstMultiplier > 1, "-burst-multiplier must be above 1")
		check(o.burstStart >= 0, "-burst-start must not be negative")
		check(o.burstDuration > 0, "-burst-duration must be positive")
		check(o.burstStart+o.burstDuration <= o.duration, "the burst window must end within -duration")
	default:
		check(false, "-profile must be steady or burst")
	}
	return errors.Join(errs...)
}

// rateAt returns the offered events per second at time t of the run.
func (o options) rateAt(t time.Duration) float64 {
	if o.profile == profileBurst && t >= o.burstStart && t < o.burstStart+o.burstDuration {
		return o.rate * o.burstMultiplier
	}
	return o.rate
}

// schedule returns when each batch is due, from the start of the run. It is computed
// before the run and never depends on a response, which is what makes the load open
// loop: a slow ES does not slow the offered rate.
func (o options) schedule() []time.Duration {
	var offsets []time.Duration
	for t := 0.0; t < float64(o.duration); t += float64(o.batch) / o.rateAt(time.Duration(t)) * float64(time.Second) {
		offsets = append(offsets, time.Duration(t))
	}
	return offsets
}

func segmentName(prefix string, index int) string {
	return fmt.Sprintf("%s%d", prefix, index)
}
