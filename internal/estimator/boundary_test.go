package estimator

import (
	"math"
	"math/bits"
	"math/rand/v2"
	"os"
	"slices"
	"testing"
	"time"
)

// The traffic model of the boundary error experiment, fixed in docs/DESIGN.md. Every
// value is an assumption [E], because ES has no real USS data.
const (
	simDay         = int64(24 * 60 * 60)
	simStart       = int64(1794096000) // aligned to a whole day, so also to 6h and 1h
	simEnd         = simStart + 56*simDay
	simSampleFrom  = simStart + 42*simDay
	simSampleEvery = int64(15 * 60)
	simSpikeStart  = simStart + 28*simDay
	simSpikeLength = 6 * 60 * 60
	simSpikeShare  = 0.30
	simMeanGap     = 3 * float64(simDay)
	simMaxReturns  = 8
	simSeed        = 1
	simRetention   = int64(Retention / time.Second)
)

type simEvent struct {
	at   int64 // Unix seconds
	user int32
}

// simTraffic is every event of one traffic profile, sorted by time.
type simTraffic struct {
	users          int
	events         []simEvent
	spikeUsers     int
	assignedReturn int // users assigned to make at least one return visit
	returnedAny    int // users with at least one further event before day 56
	returnedWithin int // users with a further event within 14 days of the one before it
}

// generateTraffic follows the traffic model. A returning user makes 1 to 8 further
// events with exponential gaps, and an event after day 56 is dropped.
func generateTraffic(users int, spiky bool, returnShare float64, seed uint64) simTraffic {
	generator := rand.New(rand.NewPCG(seed, 0))
	traffic := simTraffic{users: users}
	if spiky {
		traffic.spikeUsers = int(float64(users) * simSpikeShare)
	}
	for user := range users {
		first := simStart + generator.Int64N(simEnd-simStart)
		if user < traffic.spikeUsers {
			first = simSpikeStart + generator.Int64N(simSpikeLength)
		}
		traffic.events = append(traffic.events, simEvent{at: first, user: int32(user)})
		if generator.Float64() >= returnShare {
			continue
		}
		traffic.assignedReturn++
		previous := first
		kept, within := 0, false
		for range 1 + generator.IntN(simMaxReturns) {
			gap := int64(generator.ExpFloat64() * simMeanGap)
			at := previous + gap
			if at >= simEnd {
				break
			}
			traffic.events = append(traffic.events, simEvent{at: at, user: int32(user)})
			kept++
			within = within || gap < simRetention
			previous = at
		}
		if kept > 0 {
			traffic.returnedAny++
		}
		if within {
			traffic.returnedWithin++
		}
	}
	slices.SortFunc(traffic.events, func(a, b simEvent) int {
		if a.at != b.at {
			if a.at < b.at {
				return -1
			}
			return 1
		}
		return int(a.user - b.user)
	})
	return traffic
}

// simSamples returns every sampling instant, from day 42 to day 56 inclusive.
func simSamples() []int64 {
	var samples []int64
	for at := simSampleFrom; at <= simEnd; at += simSampleEvery {
		samples = append(samples, at)
	}
	return samples
}

type simSet []uint64

func newSimSet(users int) simSet {
	return make(simSet, (users+63)/64)
}

func (s simSet) add(user int32) {
	s[user/64] |= 1 << (user % 64)
}

func (s simSet) union(other simSet) {
	for i, word := range other {
		s[i] |= word
	}
}

func (s simSet) count() int {
	total := 0
	for _, word := range s {
		total += bits.OnesCount64(word)
	}
	return total
}

// undercountResult is the undercount of one bucket size at every sample.
type undercountResult struct {
	size    time.Duration
	samples int
	mean    float64
	max     float64
	maxAt   int64
}

// simulateBoundary compares, at each sample, the exact ground truth with the bucket
// window. It uses exact sets, and neither Redis nor HLL, so it isolates boundary error.
//
//   - Ground truth: users with an event in the exact last 14 days.
//   - Window: users in the union of the buckets that Window.Buckets returns.
//
// Events are applied in time order, so the current bucket holds only what has happened
// by the sample. A window count above the truth would break the expiry guarantee, so
// the test fails on one.
func simulateBoundary(t *testing.T, traffic simTraffic, sizes []time.Duration, samples []int64) []undercountResult {
	t.Helper()
	windows := make([]Window, len(sizes))
	sets := make([]map[int64]simSet, len(sizes))
	results := make([]undercountResult, len(sizes))
	for i, size := range sizes {
		window, err := NewWindow(size)
		if err != nil {
			t.Fatalf("NewWindow: %v", err)
		}
		windows[i] = window
		sets[i] = make(map[int64]simSet)
		results[i].size = size
	}
	lastSeen := make([]int64, traffic.users)
	for user := range lastSeen {
		lastSeen[user] = math.MinInt64
	}
	union := newSimSet(traffic.users)
	next := 0
	for _, at := range samples {
		for ; next < len(traffic.events) && traffic.events[next].at <= at; next++ {
			event := traffic.events[next]
			lastSeen[event.user] = event.at
			for i, window := range windows {
				start := window.Current(time.Unix(event.at, 0)).Start
				set, ok := sets[i][start]
				if !ok {
					set = newSimSet(traffic.users)
					sets[i][start] = set
				}
				set.add(event.user)
			}
		}
		truth := 0
		for _, seen := range lastSeen {
			if seen > at-simRetention {
				truth++
			}
		}
		for i, window := range windows {
			clear(union)
			for _, bucket := range window.Buckets(time.Unix(at, 0)) {
				if set, ok := sets[i][bucket.Start]; ok {
					union.union(set)
				}
			}
			counted := union.count()
			if counted > truth {
				t.Fatalf("size %s at %d: window %d is above truth %d", sizes[i], at, counted, truth)
			}
			if truth == 0 {
				continue
			}
			undercount := float64(truth-counted) / float64(truth)
			results[i].samples++
			results[i].mean += undercount
			if undercount > results[i].max {
				results[i].max = undercount
				results[i].maxAt = at
			}
		}
	}
	for i := range results {
		if results[i].samples > 0 {
			results[i].mean /= float64(results[i].samples)
		}
	}
	return results
}

func TestMeasureBoundaryError(t *testing.T) {
	users := 2000
	if os.Getenv("ES_MEASURE") != "" {
		users = 1000000
	}
	sizes := []time.Duration{6 * time.Hour, time.Hour}
	samples := simSamples()
	t.Logf("users=%d seed=%d samples=%d", users, simSeed, len(samples))

	for _, spiky := range []bool{false, true} {
		profile := "uniform"
		if spiky {
			profile = "spiky"
		}
		for _, returnShare := range []float64{0, 0.5} {
			traffic := generateTraffic(users, spiky, returnShare, simSeed)
			t.Logf("profile=%s r=%.0f%% events=%d spike_users=%d assigned_return=%.4f%% returned_any=%.4f%% returned_within_14d=%.4f%%",
				profile, returnShare*100, len(traffic.events), traffic.spikeUsers,
				simShare(traffic.assignedReturn, users), simShare(traffic.returnedAny, users), simShare(traffic.returnedWithin, users))
			for _, result := range simulateBoundary(t, traffic, sizes, samples) {
				t.Logf("profile=%s r=%.0f%% bucket=%s mean_undercount=%.4f%% max_undercount=%.4f%% max_at_day=%.4f",
					profile, returnShare*100, result.size, result.mean*100, result.max*100,
					float64(result.maxAt-simStart)/float64(simDay))
			}
		}
	}
}

func simShare(part, whole int) float64 {
	return float64(part) / float64(whole) * 100
}

func TestSimSamplesIncludeEveryBoundary(t *testing.T) {
	samples := simSamples()
	if len(samples) != 14*24*4+1 {
		t.Errorf("samples = %d, want %d", len(samples), 14*24*4+1)
	}
	for at := simSampleFrom; at <= simEnd; at += 3600 {
		if !slices.Contains(samples, at) {
			t.Errorf("no sample at the 1h boundary %d", at)
		}
	}
}

func TestGenerateTrafficFollowsTheModel(t *testing.T) {
	const users = 10000
	spiky := generateTraffic(users, true, 0.5, simSeed)
	if spiky.spikeUsers != 3000 {
		t.Errorf("spike users = %d, want 3000", spiky.spikeUsers)
	}
	if !slices.IsSortedFunc(spiky.events, func(a, b simEvent) int { return int(a.at - b.at) }) {
		t.Error("events are not sorted")
	}
	first := make(map[int32]int64)
	extra := make(map[int32]int)
	for _, event := range spiky.events {
		if event.at < simStart || event.at >= simEnd {
			t.Fatalf("event at %d is outside the period", event.at)
		}
		if _, seen := first[event.user]; seen {
			extra[event.user]++
		} else {
			first[event.user] = event.at
		}
	}
	if len(first) != users {
		t.Errorf("users with events = %d, want %d", len(first), users)
	}
	for user := range int32(3000) {
		if at := first[user]; at < simSpikeStart || at >= simSpikeStart+simSpikeLength {
			t.Fatalf("spike user %d starts at %d, outside the slice", user, at)
		}
	}
	for user, count := range extra {
		if count > simMaxReturns {
			t.Fatalf("user %d has %d further events", user, count)
		}
	}
	if spiky.returnedAny != len(extra) {
		t.Errorf("returnedAny = %d, want %d", spiky.returnedAny, len(extra))
	}
	if spiky.returnedAny > spiky.assignedReturn || spiky.returnedWithin > spiky.returnedAny {
		t.Errorf("assigned %d, any %d, within %d: want them not to increase",
			spiky.assignedReturn, spiky.returnedAny, spiky.returnedWithin)
	}

	uniform := generateTraffic(users, false, 0, simSeed)
	if len(uniform.events) != users {
		t.Errorf("r=0 events = %d, want one per user", len(uniform.events))
	}
	if uniform.spikeUsers != 0 || uniform.assignedReturn != 0 {
		t.Errorf("uniform with r=0 = %+v", uniform.spikeUsers)
	}
}

func TestGenerateTrafficIsReproducible(t *testing.T) {
	a := generateTraffic(100, true, 0.5, 1)
	b := generateTraffic(100, true, 0.5, 1)
	if !slices.Equal(a.events, b.events) {
		t.Error("the same seed gave different events")
	}
	if slices.Equal(a.events, generateTraffic(100, true, 0.5, 2).events) {
		t.Error("two seeds gave the same events")
	}
}

// An event one hour into the bucket that has just left the window is still in the exact
// last 14 days, so it is the undercount. The other user keeps the truth at two.
func TestSimulateBoundaryFindsTheDroppedUser(t *testing.T) {
	at := simStart + 21*simDay // aligned to a 6h boundary
	traffic := simTraffic{users: 2, events: []simEvent{
		{at: at - simRetention + 3600, user: 0},
		{at: at - 60, user: 1},
	}}

	results := simulateBoundary(t, traffic, []time.Duration{6 * time.Hour, time.Hour}, []int64{at})

	if got := results[0].max; got != 0.5 {
		t.Errorf("6h undercount = %f, want 0.5", got)
	}
	// With 1h buckets, the same event is in the oldest bucket that is still read.
	if got := results[1].max; got != 0 {
		t.Errorf("1h undercount = %f, want 0", got)
	}
}

func TestSimulateBoundaryLeavesOutExpiredUsers(t *testing.T) {
	at := simStart + 21*simDay
	traffic := simTraffic{users: 2, events: []simEvent{
		{at: at - simRetention, user: 0}, // exactly 14 days old
		{at: at - 60, user: 1},
	}}

	results := simulateBoundary(t, traffic, []time.Duration{6 * time.Hour}, []int64{at})

	if got := results[0].max; got != 0 {
		t.Errorf("undercount = %f, want 0: the expired user is in neither count", got)
	}
}

func TestSimulateBoundaryIgnoresEventsAfterTheSample(t *testing.T) {
	at := simStart + 21*simDay + 3600
	traffic := simTraffic{users: 2, events: []simEvent{
		{at: at - 60, user: 0},
		{at: at + 60, user: 1}, // same bucket, but later than the sample
	}}

	results := simulateBoundary(t, traffic, []time.Duration{6 * time.Hour}, []int64{at})

	if got := results[0].max; got != 0 {
		t.Errorf("undercount = %f, want 0", got)
	}
}
