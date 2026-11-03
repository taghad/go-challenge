package main

import (
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/taghad/go-challenge/internal/estimator"
)

func testOptions(url string) options {
	return options{
		url:           url,
		rate:          1000,
		duration:      time.Second,
		batch:         10,
		users:         1000,
		segments:      3,
		segmentPrefix: "seg-",
		seed:          1,
		profile:       profileSteady,
		timeout:       time.Second,
		maxRetries:    3,
	}
}

// fakeES answers the nth request, from 0, with the given handler, and records the bodies.
type fakeES struct {
	*httptest.Server
	mu      sync.Mutex
	times   []time.Time
	bodies  [][]byte
	respond func(n int, w http.ResponseWriter, r *http.Request)
}

func newFakeES(t *testing.T, respond func(n int, w http.ResponseWriter, r *http.Request)) *fakeES {
	t.Helper()
	f := &fakeES{respond: respond}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		n := len(f.times)
		f.times = append(f.times, time.Now())
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		f.respond(n, w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeES) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.times)
}

func accept(_ int, w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// reject503 asks for no wait, so that a test of the retry count stays fast.
func reject503(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "0")
	w.WriteHeader(http.StatusServiceUnavailable)
}

// sendOne runs one batch, due now, through the real code path, and returns the report.
func sendOne(o options) report {
	s := newSimulator(o, newClient())
	now := time.Now()
	s.sendBatch(0, now, now)
	return s.recorder.report(o, 1, false)
}

func TestAcknowledgedOnFirstAttempt(t *testing.T) {
	es := newFakeES(t, accept)

	r := sendOne(testOptions(es.URL))

	if r.FirstAttempts.Attempts != 1 || r.FirstAttempts.AcknowledgedEvents != 10 {
		t.Errorf("first attempts = %+v", r.FirstAttempts)
	}
	if r.FirstAttempts.Acceptance != 1 {
		t.Errorf("first-attempt acceptance = %f, want 1", r.FirstAttempts.Acceptance)
	}
	if r.Retries.Attempts != 0 {
		t.Errorf("retries = %d, want 0", r.Retries.Attempts)
	}
	if r.Final.Acceptance != 1 {
		t.Errorf("final acceptance = %f, want 1", r.Final.Acceptance)
	}
	if r.DistinctUsers.Acknowledged == 0 || r.DistinctUsers.Acknowledged != r.DistinctUsers.Attempted {
		t.Errorf("distinct users = %+v, want them equal and above 0", r.DistinctUsers)
	}
}

func TestRetryIsReportedSeparatelyFromFirstAttempt(t *testing.T) {
	es := newFakeES(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 0 {
			reject503(w)
			return
		}
		accept(n, w, r)
	})

	r := sendOne(testOptions(es.URL))

	if r.FirstAttempts.Attempts != 1 || r.FirstAttempts.Rejected503 != 1 {
		t.Errorf("first attempts = %+v", r.FirstAttempts)
	}
	if r.FirstAttempts.Acceptance != 0 {
		t.Errorf("first-attempt acceptance = %f, want 0", r.FirstAttempts.Acceptance)
	}
	if r.Retries.Attempts != 1 || r.Retries.AcknowledgedEvents != 10 {
		t.Errorf("retries = %+v", r.Retries)
	}
	if r.Final.Acceptance != 1 {
		t.Errorf("final acceptance = %f, want 1", r.Final.Acceptance)
	}
	if !slices.Equal(es.bodies[0], es.bodies[1]) {
		t.Error("a retry changed the body")
	}
}

func TestTimelineCountsAttemptsBySecondOfTheRun(t *testing.T) {
	r := newRecorder(newUserTracker(10, 1))

	r.attempt(true, rejected, 10, 20*time.Millisecond, 100*time.Millisecond)
	r.attempt(true, acknowledged, 10, 30*time.Millisecond, 900*time.Millisecond)
	r.attempt(false, rejected, 10, 5*time.Millisecond, 2100*time.Millisecond)
	r.attempt(false, acknowledged, 10, 6*time.Millisecond, 2200*time.Millisecond)

	want := []timelineSecond{
		{Second: 0, FirstAttempts: 2, FirstAcknowledged: 1, Rejected503: 1, MaxLatencyMs: 30},
		{Second: 1},
		{Second: 2, Retries: 2, RetriesAcknowledged: 1, Rejected503: 1, MaxLatencyMs: 6},
	}
	got := r.report(testOptions("http://unused"), 1, false).Timeline
	if !reflect.DeepEqual(got, want) {
		t.Errorf("timeline = %+v, want %+v", got, want)
	}
}

func TestRetriesStopAtTheLimit(t *testing.T) {
	es := newFakeES(t, func(_ int, w http.ResponseWriter, _ *http.Request) { reject503(w) })

	r := sendOne(testOptions(es.URL))

	if got := es.requests(); got != 4 {
		t.Errorf("requests = %d, want a first attempt and 3 retries", got)
	}
	if r.FirstAttempts.Attempts != 1 || r.Retries.Attempts != 3 {
		t.Errorf("attempts = %d and %d, want 1 and 3", r.FirstAttempts.Attempts, r.Retries.Attempts)
	}
	if r.Final.AbandonedEvents != 10 {
		t.Errorf("abandoned events = %d, want 10", r.Final.AbandonedEvents)
	}
	if r.Final.Acceptance != 0 {
		t.Errorf("final acceptance = %f, want 0", r.Final.Acceptance)
	}
}

func TestTimeoutIsCountedSeparatelyAndRetried(t *testing.T) {
	es := newFakeES(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 0 {
			<-r.Context().Done()
			return
		}
		accept(n, w, r)
	})
	o := testOptions(es.URL)
	o.timeout = 50 * time.Millisecond

	s := newSimulator(o, newClient())
	now := time.Now()
	// Without a Retry-After, the wait after a timeout is the 1 second default.
	s.sendBatch(0, now, now)
	r := s.recorder.report(o, 1, false)

	if r.FirstAttempts.Timeouts != 1 || r.FirstAttempts.Rejected503 != 0 || r.FirstAttempts.NetworkErrors != 0 {
		t.Errorf("first attempts = %+v", r.FirstAttempts)
	}
	if r.Retries.AcknowledgedEvents != 10 {
		t.Errorf("retries = %+v", r.Retries)
	}
}

// A timed-out event may have been written, so its users are attempted but not
// acknowledged. Together the two counts bound the true number.
func TestTimedOutUsersAreAttemptedButNotAcknowledged(t *testing.T) {
	es := newFakeES(t, func(_ int, _ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	o := testOptions(es.URL)
	o.timeout = 20 * time.Millisecond
	o.maxRetries = 0

	r := sendOne(o)

	if r.DistinctUsers.Acknowledged != 0 {
		t.Errorf("acknowledged users = %d, want 0", r.DistinctUsers.Acknowledged)
	}
	if r.DistinctUsers.Attempted == 0 {
		t.Error("attempted users = 0, want the users of the batch")
	}
	var attempted int64
	for _, segment := range r.Segments {
		if segment.Acknowledged != 0 {
			t.Errorf("%s acknowledged = %d, want 0", segment.Segment, segment.Acknowledged)
		}
		attempted += segment.Attempted
	}
	if attempted == 0 {
		t.Error("no segment has attempted users")
	}
}

func TestOtherResponseIsNotRetried(t *testing.T) {
	es := newFakeES(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})

	r := sendOne(testOptions(es.URL))

	if got := es.requests(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
	if r.FirstAttempts.OtherErrors != 1 {
		t.Errorf("other errors = %d, want 1", r.FirstAttempts.OtherErrors)
	}
	if r.Final.FailedEvents != 10 {
		t.Errorf("failed events = %d, want 10", r.Final.FailedEvents)
	}
}

func TestNetworkErrorIsCountedSeparatelyAndRetried(t *testing.T) {
	es := httptest.NewServer(http.NotFoundHandler())
	url := es.URL
	es.Close()
	o := testOptions(url)
	o.maxRetries = 1

	r := sendOne(o)

	if r.FirstAttempts.NetworkErrors != 1 {
		t.Errorf("first attempts = %+v", r.FirstAttempts)
	}
	if r.Retries.NetworkErrors != 1 {
		t.Errorf("retries = %+v", r.Retries)
	}
	if r.Final.AbandonedEvents != 10 {
		t.Errorf("abandoned events = %d, want 10", r.Final.AbandonedEvents)
	}
}

func TestHonorsRetryAfter(t *testing.T) {
	es := newFakeES(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 0 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		accept(n, w, r)
	})

	sendOne(testOptions(es.URL))

	if gap := es.times[1].Sub(es.times[0]); gap < time.Second {
		t.Errorf("retry came after %s, want at least 1s", gap)
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		value string
		want  time.Duration
	}{
		{"0", 0},
		{"1", time.Second},
		{"30", 30 * time.Second},
		{"", defaultRetryWait},
		{"-1", defaultRetryWait},
		{"soon", defaultRetryWait},
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.value); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", tt.value, got, tt.want)
		}
	}
}

func TestRetryWaitAddsJitterWithoutGoingBelowRetryAfter(t *testing.T) {
	generator := rand.New(rand.NewPCG(1, 0))
	seen := make(map[time.Duration]bool)
	for range 100 {
		wait := retryWait(time.Second, 100*time.Millisecond, generator)
		if wait < time.Second || wait >= 1100*time.Millisecond {
			t.Fatalf("wait = %s, want it in [1s, 1.1s)", wait)
		}
		seen[wait] = true
	}
	if len(seen) < 2 {
		t.Error("the jitter did not vary")
	}
	if got := retryWait(time.Second, 0, generator); got != time.Second {
		t.Errorf("wait without jitter = %s, want 1s", got)
	}
}

// A send that starts late must count as latency, or the report would hide the time a
// batch spent waiting for the simulator.
func TestLatencyRunsFromScheduledTime(t *testing.T) {
	es := newFakeES(t, accept)
	o := testOptions(es.URL)
	s := newSimulator(o, newClient())
	late := time.Now().Add(-time.Second)

	s.sendBatch(0, late, late)
	r := s.recorder.report(o, 1, false)

	if r.FirstAttempts.LatencyMs.Max < 1000 {
		t.Errorf("first-attempt latency = %f ms, want at least 1000", r.FirstAttempts.LatencyMs.Max)
	}
	if r.SendLagMs.Max < 1000 {
		t.Errorf("send lag = %f ms, want at least 1000", r.SendLagMs.Max)
	}
}

// A retry is measured from the end of its own wait, and the final latency from the
// schedule of the batch, so the wait shows in the final latency only.
func TestRetryLatencyRunsFromItsOwnSchedule(t *testing.T) {
	es := newFakeES(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 0 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		accept(n, w, r)
	})

	r := sendOne(testOptions(es.URL))

	if r.Retries.LatencyMs.Max >= 1000 {
		t.Errorf("retry latency = %f ms, want it without the 1s wait", r.Retries.LatencyMs.Max)
	}
	if r.Final.LatencyMs.Max < 1000 {
		t.Errorf("final latency = %f ms, want it to include the 1s wait", r.Final.LatencyMs.Max)
	}
}

// The core property of open loop: every batch is sent on schedule while no earlier one
// has been answered.
func TestRunIsOpenLoop(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	const batches = 5
	arrived := make(chan struct{}, batches)
	es := newFakeES(t, func(n int, w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		accept(n, w, r)
	})
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	o := testOptions(es.URL)
	o.batch = 10
	o.rate = 500 // one batch every 20 ms
	o.duration = batches * 20 * time.Millisecond
	o.timeout = 10 * time.Second

	done := make(chan report, 1)
	go func() { done <- newSimulator(o, newClient()).run(context.Background()) }()

	deadline := time.After(5 * time.Second)
	for i := range batches {
		select {
		case <-arrived:
		case <-deadline:
			t.Fatalf("only %d of %d batches were sent while none was answered", i, batches)
		}
	}
	once.Do(func() { close(release) })

	r := <-done
	if r.FirstAttempts.Attempts != batches {
		t.Errorf("first attempts = %d, want %d", r.FirstAttempts.Attempts, batches)
	}
	if r.ScheduledBatches != batches {
		t.Errorf("scheduled batches = %d, want %d", r.ScheduledBatches, batches)
	}
	if r.Interrupted {
		t.Error("the run reports an interruption")
	}
	if want := 500.0; r.OfferedEventsPerSecond != want {
		t.Errorf("offered rate = %f, want %f", r.OfferedEventsPerSecond, want)
	}
	if want := 500.0; r.AchievedEventsPerSecond != want {
		t.Errorf("achieved rate = %f, want %f", r.AchievedEventsPerSecond, want)
	}
}

func TestInterruptedRunStopsSchedulingAndReportsIt(t *testing.T) {
	es := newFakeES(t, accept)
	o := testOptions(es.URL)
	o.rate = 10 // one batch of 10 every second
	o.duration = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := newSimulator(o, newClient()).run(ctx)

	if !r.Interrupted {
		t.Error("Interrupted is false")
	}
	if r.ScheduledBatches != 60 {
		t.Errorf("scheduled batches = %d, want 60", r.ScheduledBatches)
	}
	if r.FirstAttempts.Attempts >= 2 {
		t.Errorf("first attempts = %d, want the run to stop at once", r.FirstAttempts.Attempts)
	}
}

func eventsOf(t *testing.T, o options, index uint64) []estimator.Event {
	t.Helper()
	s := newSimulator(o, newClient())
	b := s.newBatch(rand.New(rand.NewPCG(o.seed, index)))
	var body struct {
		Events []estimator.Event `json:"events"`
	}
	if err := json.Unmarshal(b.body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Events
}

func TestSameSeedGivesSameEvents(t *testing.T) {
	o := testOptions("http://unused")
	first := eventsOf(t, o, 7)

	if !reflect.DeepEqual(first, eventsOf(t, o, 7)) {
		t.Error("the same seed and batch gave different events")
	}
	if reflect.DeepEqual(first, eventsOf(t, o, 8)) {
		t.Error("two batches of one run have the same events")
	}
	o.seed = 2
	if reflect.DeepEqual(first, eventsOf(t, o, 7)) {
		t.Error("two seeds gave the same events")
	}
}

func TestEventsAreValidForES(t *testing.T) {
	o := testOptions("http://unused")
	if err := estimator.ValidateBatch(eventsOf(t, o, 0)); err != nil {
		t.Errorf("ValidateBatch: %v", err)
	}
}

func TestSegmentCountsMatchTheSentEvents(t *testing.T) {
	es := newFakeES(t, accept)
	o := testOptions(es.URL)

	r := sendOne(o)

	var body struct {
		Events []estimator.Event `json:"events"`
	}
	if err := json.Unmarshal(es.bodies[0], &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	users := make(map[string]bool)
	perSegment := make(map[string]map[string]bool)
	for _, event := range body.Events {
		users[event.UserID] = true
		if perSegment[event.Segment] == nil {
			perSegment[event.Segment] = make(map[string]bool)
		}
		perSegment[event.Segment][event.UserID] = true
	}
	if got := r.DistinctUsers.Acknowledged; got != int64(len(users)) {
		t.Errorf("acknowledged users = %d, want %d", got, len(users))
	}
	if len(r.Segments) != o.segments {
		t.Fatalf("segments = %d, want %d", len(r.Segments), o.segments)
	}
	for _, segment := range r.Segments {
		want := int64(len(perSegment[segment.Segment]))
		if segment.Acknowledged != want || segment.Attempted != want {
			t.Errorf("%s = %+v, want %d", segment.Segment, segment.userCounts, want)
		}
	}
}

func TestPercentileUsesNearestRank(t *testing.T) {
	samples := make([]time.Duration, 100)
	for i := range samples {
		samples[i] = time.Duration(i + 1)
	}
	tests := []struct {
		p    int
		want time.Duration
	}{{50, 50}, {90, 90}, {99, 99}, {100, 100}, {1, 1}}
	for _, tt := range tests {
		if got := percentile(samples, tt.p); got != tt.want {
			t.Errorf("p%d = %d, want %d", tt.p, got, tt.want)
		}
	}
	if got := percentile(samples[:1], 99); got != 1 {
		t.Errorf("p99 of one sample = %d, want 1", got)
	}
}

func TestSummarizeWithoutSamples(t *testing.T) {
	if got := summarize(nil); got != (latencySummary{}) {
		t.Errorf("summarize(nil) = %+v", got)
	}
}

func TestBitsetCountsDistinct(t *testing.T) {
	b := newBitset(130)
	for _, i := range []int{0, 0, 63, 64, 129} {
		b.set(i)
	}
	if got := b.count(); got != 4 {
		t.Errorf("count = %d, want 4", got)
	}
}
