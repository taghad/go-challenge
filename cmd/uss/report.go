package main

import (
	"slices"
	"sync"
	"time"
)

// report is the output of one run. All of its numbers are measured on the machine that
// ran it, which also runs ES and Redis if the run uses local compose.
type report struct {
	Config                  map[string]string `json:"config,omitempty"`
	StartedAt               string            `json:"started_at"`
	FinishedAt              string            `json:"finished_at"`
	Interrupted             bool              `json:"interrupted"`
	ScheduledBatches        int               `json:"scheduled_batches"`
	OfferedEvents           int64             `json:"offered_events"`
	OfferedEventsPerSecond  float64           `json:"offered_events_per_second"`
	AchievedEventsPerSecond float64           `json:"achieved_events_per_second"`
	SendLagMs               latencySummary    `json:"send_lag_ms"`
	FirstAttempts           attemptReport     `json:"first_attempts"`
	Retries                 attemptReport     `json:"retries"`
	Final                   finalReport       `json:"final"`
	DistinctUsers           userCounts        `json:"distinct_users"`
	Segments                []segmentCounts   `json:"segments,omitempty"`
	Timeline                []timelineSecond  `json:"timeline,omitempty"`
	EstimateCheck           *estimateCheck    `json:"estimate_check,omitempty"`
}

// attemptReport covers one kind of attempt. Acceptance is the share of the events in
// these attempts that were acknowledged by them.
type attemptReport struct {
	Attempts           int64          `json:"attempts"`
	Events             int64          `json:"events"`
	AcknowledgedEvents int64          `json:"acknowledged_events"`
	Rejected503        int64          `json:"rejected_503"`
	Timeouts           int64          `json:"timeouts"`
	NetworkErrors      int64          `json:"network_errors"`
	OtherErrors        int64          `json:"other_errors"`
	Acceptance         float64        `json:"acceptance"`
	LatencyMs          latencySummary `json:"latency_ms"`
}

// finalReport covers each batch once, after its last attempt. Abandoned events used up
// their retries. Failed events got a response that a retry cannot fix.
type finalReport struct {
	Events             int64          `json:"events"`
	AcknowledgedEvents int64          `json:"acknowledged_events"`
	AbandonedEvents    int64          `json:"abandoned_events"`
	FailedEvents       int64          `json:"failed_events"`
	Acceptance         float64        `json:"acceptance"`
	LatencyMs          latencySummary `json:"latency_ms"`
}

// userCounts holds distinct users. Attempted includes every user sent at least once. A
// timed-out event may have been written, so the true count lies between the two.
type userCounts struct {
	Acknowledged int64 `json:"acknowledged"`
	Attempted    int64 `json:"attempted"`
}

type segmentCounts struct {
	Segment string `json:"segment"`
	userCounts
}

// timelineSecond counts the attempts that finished in one second of a run, counted from
// its start. It shows when a run recovers after a burst, which the totals cannot show.
type timelineSecond struct {
	Second              int     `json:"second"`
	FirstAttempts       int64   `json:"first_attempts"`
	FirstAcknowledged   int64   `json:"first_acknowledged"`
	Retries             int64   `json:"retries"`
	RetriesAcknowledged int64   `json:"retries_acknowledged"`
	Rejected503         int64   `json:"rejected_503"`
	MaxLatencyMs        float64 `json:"max_latency_ms"`
}

type latencySummary struct {
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

// summarize sorts its input.
func summarize(samples []time.Duration) latencySummary {
	if len(samples) == 0 {
		return latencySummary{}
	}
	slices.Sort(samples)
	return latencySummary{
		P50: milliseconds(percentile(samples, 50)),
		P90: milliseconds(percentile(samples, 90)),
		P99: milliseconds(percentile(samples, 99)),
		Max: milliseconds(samples[len(samples)-1]),
	}
}

// percentile uses the nearest rank on sorted samples.
func percentile(sorted []time.Duration, p int) time.Duration {
	rank := (len(sorted)*p + 99) / 100
	return sorted[max(rank, 1)-1]
}

func milliseconds(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func share(part, whole int64) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

// bitset marks user indexes. It uses one bit per possible user, so its size is known at
// the start of the run and does not grow with the number of events.
type bitset []uint64

func newBitset(size int) bitset {
	return make(bitset, (size+63)/64)
}

func (b bitset) set(i int) {
	b[i/64] |= 1 << (i % 64)
}

func (b bitset) count() int64 {
	var total int64
	for _, word := range b {
		for ; word != 0; word &= word - 1 {
			total++
		}
	}
	return total
}

type attemptCounters struct {
	report  attemptReport
	samples []time.Duration
}

func (c *attemptCounters) add(result outcome, events int, latency time.Duration) {
	c.report.Attempts++
	c.report.Events += int64(events)
	c.samples = append(c.samples, latency)
	switch result {
	case acknowledged:
		c.report.AcknowledgedEvents += int64(events)
	case rejected:
		c.report.Rejected503++
	case timedOut:
		c.report.Timeouts++
	case networkError:
		c.report.NetworkErrors++
	case otherError:
		c.report.OtherErrors++
	}
}

func (c *attemptCounters) finish() attemptReport {
	c.report.Acceptance = share(c.report.AcknowledgedEvents, c.report.Events)
	c.report.LatencyMs = summarize(c.samples)
	return c.report
}

// userTracker holds the distinct users of a run. It is separate from the recorder,
// because the steps of a ramp each have a recorder but send to the same segments, so
// the true number of users of a segment builds up across them.
type userTracker struct {
	mu               sync.Mutex
	acked            bitset
	attempted        bitset
	segmentAcked     []bitset
	segmentAttempted []bitset
}

func newUserTracker(users, segments int) *userTracker {
	u := &userTracker{
		acked:            newBitset(users),
		attempted:        newBitset(users),
		segmentAcked:     make([]bitset, segments),
		segmentAttempted: make([]bitset, segments),
	}
	for i := range segments {
		u.segmentAcked[i] = newBitset(users)
		u.segmentAttempted[i] = newBitset(users)
	}
	return u
}

func (u *userTracker) markAttempted(b batch) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i, user := range b.users {
		u.attempted.set(user)
		u.segmentAttempted[b.segments[i]].set(user)
	}
}

func (u *userTracker) markAcknowledged(b batch) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i, user := range b.users {
		u.acked.set(user)
		u.segmentAcked[b.segments[i]].set(user)
	}
}

func (u *userTracker) total() userCounts {
	u.mu.Lock()
	defer u.mu.Unlock()
	return userCounts{Acknowledged: u.acked.count(), Attempted: u.attempted.count()}
}

func (u *userTracker) perSegment(prefix string) []segmentCounts {
	u.mu.Lock()
	defer u.mu.Unlock()
	segments := make([]segmentCounts, len(u.segmentAcked))
	for i := range segments {
		segments[i] = segmentCounts{
			Segment: segmentName(prefix, i),
			userCounts: userCounts{
				Acknowledged: u.segmentAcked[i].count(),
				Attempted:    u.segmentAttempted[i].count(),
			},
		}
	}
	return segments
}

// recorder collects the results of all batches of one run. One mutex is enough, because
// it is held only after a response, and so it adds nothing to a measured latency.
type recorder struct {
	mu           sync.Mutex
	users        *userTracker
	first        attemptCounters
	retries      attemptCounters
	final        finalReport
	finalSamples []time.Duration
	sendLag      []time.Duration
	lastSend     time.Duration
	timeline     []timelineSecond
}

func newRecorder(users *userTracker) *recorder {
	return &recorder{users: users}
}

// firstSend records a batch just before its first attempt.
func (r *recorder) firstSend(b batch, start, scheduled time.Time) {
	now := time.Now()
	r.users.markAttempted(b)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sendLag = append(r.sendLag, now.Sub(scheduled))
	r.lastSend = max(r.lastSend, now.Sub(start))
	r.final.Events += int64(len(b.users))
}

// attempt records one finished attempt. finishedAt is counted from the start of the run.
func (r *recorder) attempt(first bool, result outcome, events int, latency, finishedAt time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if first {
		r.first.add(result, events, latency)
	} else {
		r.retries.add(result, events, latency)
	}
	second := int(finishedAt / time.Second)
	for len(r.timeline) <= second {
		r.timeline = append(r.timeline, timelineSecond{Second: len(r.timeline)})
	}
	t := &r.timeline[second]
	switch {
	case first:
		t.FirstAttempts++
		if result == acknowledged {
			t.FirstAcknowledged++
		}
	default:
		t.Retries++
		if result == acknowledged {
			t.RetriesAcknowledged++
		}
	}
	if result == rejected {
		t.Rejected503++
	}
	t.MaxLatencyMs = max(t.MaxLatencyMs, milliseconds(latency))
}

func (r *recorder) acknowledge(b batch, latency time.Duration) {
	r.users.markAcknowledged(b)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.final.AcknowledgedEvents += int64(len(b.users))
	r.finalSamples = append(r.finalSamples, latency)
}

func (r *recorder) abandon(events int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.final.AbandonedEvents += int64(events)
}

func (r *recorder) fail(events int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.final.FailedEvents += int64(events)
}

// report must be called after all batches have finished.
//
// The offered rate is what the schedule planned. The achieved rate is what was actually
// sent over the same time, or over a longer time if sends fell behind. An interrupted
// run is measured up to its last send, so the part that never ran does not count.
func (r *recorder) report(o options, scheduledBatches int, interrupted bool) report {
	r.mu.Lock()
	defer r.mu.Unlock()
	sendWindow := max(o.duration, r.lastSend)
	if interrupted {
		sendWindow = r.lastSend
	}
	var achieved float64
	if sendWindow > 0 {
		achieved = float64(r.final.Events) / sendWindow.Seconds()
	}
	r.final.Acceptance = share(r.final.AcknowledgedEvents, r.final.Events)
	r.final.LatencyMs = summarize(r.finalSamples)
	return report{
		Config:                  o.flags,
		Interrupted:             interrupted,
		ScheduledBatches:        scheduledBatches,
		OfferedEvents:           int64(scheduledBatches * o.batch),
		OfferedEventsPerSecond:  float64(scheduledBatches*o.batch) / o.duration.Seconds(),
		AchievedEventsPerSecond: achieved,
		SendLagMs:               summarize(r.sendLag),
		FirstAttempts:           r.first.finish(),
		Retries:                 r.retries.finish(),
		Final:                   r.final,
		DistinctUsers:           r.users.total(),
		Segments:                r.users.perSegment(o.segmentPrefix),
		Timeline:                r.timeline,
	}
}
