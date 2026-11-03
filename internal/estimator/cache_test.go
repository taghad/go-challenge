package estimator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

const (
	testTTL         = 30 * time.Second
	testMaxInflight = 4
)

// manualClock returns the same time until a test moves it, and is safe for the
// concurrent callers in these tests.
type manualClock struct {
	mu      sync.Mutex
	current time.Time
}

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *manualClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = t
}

type countResult struct {
	value uint64
	err   error
}

// gatedCall is one Count call, waiting for a test to send its result.
type gatedCall struct {
	countCall
	release chan countResult
}

// gatedStore blocks every Count until the test releases that call, so a test can
// hold a flight open. Tests that use it run in a synctest bubble, where
// synctest.Wait returns only once every caller is blocked, so no test needs a sleep.
type gatedStore struct {
	mu     sync.Mutex
	calls  []*gatedCall
	closed chan struct{}
}

// newGatedStore must be called inside the bubble. It frees blocked calls when the
// test ends, so that a test that stops early leaves no flight running.
func newGatedStore(t *testing.T) *gatedStore {
	t.Helper()
	store := &gatedStore{closed: make(chan struct{})}
	t.Cleanup(func() { close(store.closed) })
	return store
}

func (s *gatedStore) Add(context.Context, Bucket, []Event) error {
	return nil
}

func (s *gatedStore) Count(ctx context.Context, segment string, buckets []Bucket) (uint64, error) {
	call := &gatedCall{countCall{ctx, segment, buckets}, make(chan countResult)}
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
	select {
	case result := <-call.release:
		return result.value, result.err
	case <-s.closed:
		return 0, errors.New("test ended")
	}
}

func (s *gatedStore) Ping(context.Context) error {
	return nil
}

func (s *gatedStore) started() []*gatedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*gatedCall(nil), s.calls...)
}

type estimateResult struct {
	estimate Estimate
	err      error
}

// startEstimate calls Estimate in its own goroutine. The result channel is buffered,
// so the goroutine ends even if the test stops early.
func startEstimate(ctx context.Context, service *Service, segment string) <-chan estimateResult {
	result := make(chan estimateResult, 1)
	go func() {
		estimate, err := service.Estimate(ctx, segment)
		result <- estimateResult{estimate, err}
	}()
	return result
}

func newGatedService(t *testing.T, store Store, maxInflight int) (*Service, *manualClock) {
	t.Helper()
	clock := &manualClock{current: justBeforeBoundary}
	return NewService(store, mustWindow(t, 6*time.Hour), clock.now, testTTL, maxInflight), clock
}

func wantValue(t *testing.T, got estimateResult, want uint64) {
	t.Helper()
	if got.err != nil {
		t.Fatalf("Estimate: %v", got.err)
	}
	if got.estimate.Value != want {
		t.Errorf("Value = %d, want %d", got.estimate.Value, want)
	}
}

func TestCacheEntryExpiresAtTTL(t *testing.T) {
	computed := time.Unix(boundary, 0)
	bucket := mustWindow(t, 6*time.Hour).Current(computed)
	tests := []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"new", 0, true},
		{"just below TTL", testTTL - time.Nanosecond, true},
		{"at TTL", testTTL, false},
		{"above TTL", testTTL + time.Nanosecond, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := newEstimateCache(testTTL)
			cache.put("sports", bucket, Estimate{Value: 7, AsOf: computed})

			got, ok := cache.get("sports", bucket, computed.Add(tt.age))
			if ok != tt.want {
				t.Fatalf("ok = %t, want %t", ok, tt.want)
			}
			if ok && got.Value != 7 {
				t.Errorf("Value = %d, want 7", got.Value)
			}
		})
	}
}

func TestCacheEntryInvalidInAnotherBucket(t *testing.T) {
	window := mustWindow(t, 6*time.Hour)
	computed := time.Unix(boundary, -1)
	cache := newEstimateCache(testTTL)
	cache.put("sports", window.Current(computed), Estimate{Value: 7, AsOf: computed})

	after := time.Unix(boundary, 0)
	if _, ok := cache.get("sports", window.Current(after), after); ok {
		t.Error("entry is served in the next bucket")
	}
}

func TestCacheMissesUnknownSegment(t *testing.T) {
	now := time.Unix(boundary, 0)
	bucket := mustWindow(t, 6*time.Hour).Current(now)
	cache := newEstimateCache(testTTL)
	cache.put("sports", bucket, Estimate{Value: 7, AsOf: now})

	if _, ok := cache.get("news", bucket, now); ok {
		t.Error("segment that was never stored is served")
	}
}

func fillCache(cache *estimateCache, bucket Bucket, now time.Time, n int) {
	for i := range n {
		cache.put(fmt.Sprintf("s%d", i), bucket, Estimate{Value: uint64(i), AsOf: now})
	}
}

func TestCacheHoldsAtMostMaxEntries(t *testing.T) {
	now := time.Unix(boundary, 0)
	bucket := mustWindow(t, 6*time.Hour).Current(now)
	cache := newEstimateCache(testTTL)

	// The limit is written out, so that changing the constant fails this test.
	const limit = 10000
	fillCache(cache, bucket, now, limit)
	if len(cache.entries) != limit {
		t.Fatalf("entries = %d, want %d: the cache was cleared before it was full", len(cache.entries), limit)
	}
	if _, ok := cache.get("s0", bucket, now); !ok {
		t.Error("oldest entry is gone before the cache is full")
	}

	cache.put("new", bucket, Estimate{Value: 1, AsOf: now})
	if len(cache.entries) != 1 {
		t.Errorf("entries = %d, want 1: a full cache must be cleared", len(cache.entries))
	}
	if _, ok := cache.get("new", bucket, now); !ok {
		t.Error("entry that triggered the clear was not stored")
	}
}

func TestCacheReplacingEntryWhenFullClearsNothing(t *testing.T) {
	now := time.Unix(boundary, 0)
	bucket := mustWindow(t, 6*time.Hour).Current(now)
	cache := newEstimateCache(testTTL)
	fillCache(cache, bucket, now, maxCacheEntries)

	cache.put("s0", bucket, Estimate{Value: 99, AsOf: now})

	if len(cache.entries) != maxCacheEntries {
		t.Errorf("entries = %d, want %d", len(cache.entries), maxCacheEntries)
	}
	if got, ok := cache.get("s0", bucket, now); !ok || got.Value != 99 {
		t.Errorf("get = %+v, %t, want the new value", got, ok)
	}
}

// TestCacheConcurrentAccess finds its failures only under -race.
func TestCacheConcurrentAccess(t *testing.T) {
	now := time.Unix(boundary, 0)
	bucket := mustWindow(t, 6*time.Hour).Current(now)
	cache := newEstimateCache(testTTL)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i := range 1000 {
				segment := fmt.Sprintf("s%d", i%10)
				cache.put(segment, bucket, Estimate{Value: uint64(i), AsOf: now})
				cache.get(segment, bucket, now)
			}
		})
	}
	wg.Wait()
}

func TestEstimateCachedUntilTTL(t *testing.T) {
	store := &fakeStore{count: 1}
	clock := &manualClock{current: time.Unix(boundary, 0)}
	service := NewService(store, mustWindow(t, 6*time.Hour), clock.now, testTTL, testMaxInflight)
	computed := clock.now()

	if _, err := service.Estimate(context.Background(), "sports"); err != nil {
		t.Fatalf("Estimate: %v", err)
	}

	// Just below the TTL the first value is served, with its computation time.
	store.count = 2
	clock.set(computed.Add(testTTL - time.Second))
	got, err := service.Estimate(context.Background(), "sports")
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if len(store.countCalls) != 1 {
		t.Errorf("store Count calls = %d, want 1", len(store.countCalls))
	}
	if got.Value != 1 {
		t.Errorf("Value = %d, want the cached 1", got.Value)
	}
	if !got.AsOf.Equal(computed) {
		t.Errorf("AsOf = %s, want the computation time %s", got.AsOf, computed)
	}

	// At the TTL it is recomputed, and AsOf moves.
	expired := computed.Add(testTTL)
	clock.set(expired)
	got, err = service.Estimate(context.Background(), "sports")
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if len(store.countCalls) != 2 {
		t.Errorf("store Count calls = %d, want 2", len(store.countCalls))
	}
	if got.Value != 2 {
		t.Errorf("Value = %d, want the new 2", got.Value)
	}
	if !got.AsOf.Equal(expired) {
		t.Errorf("AsOf = %s, want %s", got.AsOf, expired)
	}
}

func TestEstimateCacheIsPerSegment(t *testing.T) {
	store := &fakeStore{count: 1}
	clock := &manualClock{current: time.Unix(boundary, 0)}
	service := NewService(store, mustWindow(t, 6*time.Hour), clock.now, testTTL, testMaxInflight)

	if _, err := service.Estimate(context.Background(), "sports"); err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	store.count = 2
	got, err := service.Estimate(context.Background(), "news")
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if got.Value != 2 {
		t.Errorf("Value = %d, want 2 from the store", got.Value)
	}
}

func TestEstimateNotServedAfterBoundaryWithinTTL(t *testing.T) {
	store := &fakeStore{count: 1}
	clock := &manualClock{current: justBeforeBoundary}
	service := NewService(store, mustWindow(t, 6*time.Hour), clock.now, testTTL, testMaxInflight)

	if _, err := service.Estimate(context.Background(), "sports"); err != nil {
		t.Fatalf("Estimate: %v", err)
	}

	// One second later the entry is far below the TTL, but its bucket has ended.
	store.count = 2
	clock.set(time.Unix(boundary, 0))
	got, err := service.Estimate(context.Background(), "sports")
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if got.Value != 2 {
		t.Errorf("Value = %d, want the new 2", got.Value)
	}
	if len(store.countCalls) != 2 {
		t.Fatalf("store Count calls = %d, want 2", len(store.countCalls))
	}
	if start := store.countCalls[1].buckets[0].Start; start != boundary {
		t.Errorf("recomputed from bucket %d, want %d", start, boundary)
	}
}

func TestEstimateAfterBoundaryDoesNotJoinEarlierFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newGatedStore(t)
		service, clock := newGatedService(t, store, testMaxInflight)
		ctx := context.Background()

		before := startEstimate(ctx, service, "sports")
		synctest.Wait()
		clock.set(time.Unix(boundary, 0))
		after := startEstimate(ctx, service, "sports")
		synctest.Wait()

		calls := store.started()
		if len(calls) != 2 {
			t.Fatalf("store Count calls = %d, want 2: the later request joined the earlier flight", len(calls))
		}
		if start := calls[1].buckets[0].Start; start != boundary {
			t.Errorf("later flight starts at bucket %d, want %d", start, boundary)
		}

		// The later request does not wait for the earlier flight.
		calls[1].release <- countResult{value: 2}
		wantValue(t, <-after, 2)
		calls[0].release <- countResult{value: 1}
		wantValue(t, <-before, 1)
	})
}

func TestEstimateConcurrentCallersShareOneFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newGatedStore(t)
		service, _ := newGatedService(t, store, testMaxInflight)

		var results []<-chan estimateResult
		for range 10 {
			results = append(results, startEstimate(context.Background(), service, "sports"))
		}
		synctest.Wait()

		calls := store.started()
		if len(calls) != 1 {
			t.Fatalf("store Count calls = %d, want 1", len(calls))
		}
		calls[0].release <- countResult{value: 5}
		for _, result := range results {
			wantValue(t, <-result, 5)
		}
	})
}

func TestEstimateRejectedWhenLimitIsFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newGatedStore(t)
		service, _ := newGatedService(t, store, 1)
		ctx := context.Background()

		running := startEstimate(ctx, service, "sports")
		synctest.Wait()

		// The rejection is immediate, while the running flight still holds the slot.
		_, err := service.Estimate(ctx, "news")
		if !errors.Is(err, ErrOverloaded) {
			t.Errorf("Estimate = %v, want %v", err, ErrOverloaded)
		}
		if calls := store.started(); len(calls) != 1 {
			t.Errorf("store Count calls = %d, want 1: a rejected request reached the store", len(calls))
		}

		store.started()[0].release <- countResult{value: 1}
		wantValue(t, <-running, 1)

		// The slot is free again, and the rejection was not cached.
		retry := startEstimate(ctx, service, "news")
		synctest.Wait()
		calls := store.started()
		if len(calls) != 2 {
			t.Fatalf("store Count calls = %d, want 2", len(calls))
		}
		calls[1].release <- countResult{value: 2}
		wantValue(t, <-retry, 2)
	})
}

func TestEstimateLimitAllowsUpToMax(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newGatedStore(t)
		service, _ := newGatedService(t, store, 2)
		ctx := context.Background()

		first := startEstimate(ctx, service, "sports")
		second := startEstimate(ctx, service, "news")
		synctest.Wait()

		calls := store.started()
		if len(calls) != 2 {
			t.Fatalf("store Count calls = %d, want 2", len(calls))
		}
		calls[0].release <- countResult{value: 1}
		calls[1].release <- countResult{value: 1}
		wantValue(t, <-first, 1)
		wantValue(t, <-second, 1)
	})
}

func TestEstimateCachedResponseIgnoresFullLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newGatedStore(t)
		service, _ := newGatedService(t, store, 1)
		ctx := context.Background()

		primed := startEstimate(ctx, service, "news")
		synctest.Wait()
		store.started()[0].release <- countResult{value: 3}
		wantValue(t, <-primed, 3)

		running := startEstimate(ctx, service, "sports")
		synctest.Wait()

		got, err := service.Estimate(ctx, "news")
		wantValue(t, estimateResult{got, err}, 3)

		store.started()[1].release <- countResult{value: 1}
		wantValue(t, <-running, 1)
	})
}

func TestEstimateJoiningFlightNeedsNoSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newGatedStore(t)
		service, _ := newGatedService(t, store, 1)
		ctx := context.Background()

		first := startEstimate(ctx, service, "sports")
		synctest.Wait()
		second := startEstimate(ctx, service, "sports")
		synctest.Wait()

		store.started()[0].release <- countResult{value: 5}
		wantValue(t, <-first, 5)
		wantValue(t, <-second, 5)
	})
}

func TestEstimateSlotFreedAfterStoreError(t *testing.T) {
	store := &fakeStore{err: errors.New("store down")}
	service, _ := newGatedService(t, store, 1)

	if _, err := service.Estimate(context.Background(), "sports"); errors.Is(err, ErrOverloaded) || err == nil {
		t.Fatalf("first Estimate = %v, want the store error", err)
	}
	if _, err := service.Estimate(context.Background(), "sports"); errors.Is(err, ErrOverloaded) || err == nil {
		t.Errorf("second Estimate = %v, want the store error: the slot was not freed", err)
	}
}

func TestEstimateCancelledCallerDoesNotAffectOthers(t *testing.T) {
	tests := []struct {
		name          string
		cancelStarter bool // whether the cancelled caller is the one that started the flight
	}{
		{"caller that started the flight", true},
		{"caller that joined the flight", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := newGatedStore(t)
				service, _ := newGatedService(t, store, testMaxInflight)
				cancelled, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "value"))
				defer cancel()

				firstCtx, secondCtx := cancelled, context.Background()
				if !tt.cancelStarter {
					firstCtx, secondCtx = secondCtx, firstCtx
				}
				first := startEstimate(firstCtx, service, "sports")
				synctest.Wait()
				second := startEstimate(secondCtx, service, "sports")
				synctest.Wait()

				gone, staying := first, second
				if !tt.cancelStarter {
					gone, staying = second, first
				}

				// The cancelled caller returns while the flight is still running.
				cancel()
				if got := <-gone; !errors.Is(got.err, context.Canceled) {
					t.Errorf("cancelled caller got %v, want %v", got.err, context.Canceled)
				}

				calls := store.started()
				if len(calls) != 1 {
					t.Fatalf("store Count calls = %d, want 1", len(calls))
				}
				if err := calls[0].ctx.Err(); err != nil {
					t.Errorf("store context was cancelled: %v", err)
				}
				if tt.cancelStarter && calls[0].ctx.Value(ctxKey{}) != "value" {
					t.Error("store context lost the values of the caller's context")
				}

				calls[0].release <- countResult{value: 5}
				wantValue(t, <-staying, 5)

				// The result is cached even though one caller has gone.
				got, err := service.Estimate(context.Background(), "sports")
				wantValue(t, estimateResult{got, err}, 5)
				if calls := store.started(); len(calls) != 1 {
					t.Errorf("store Count calls = %d, want 1", len(calls))
				}
			})
		})
	}
}

func TestEstimateStoreErrorNotCached(t *testing.T) {
	storeErr := errors.New("store down")
	store := &fakeStore{count: 5, err: storeErr}
	service, _ := newGatedService(t, store, testMaxInflight)

	if _, err := service.Estimate(context.Background(), "sports"); !errors.Is(err, storeErr) {
		t.Fatalf("Estimate = %v, want it to wrap %v", err, storeErr)
	}

	store.err = nil
	got, err := service.Estimate(context.Background(), "sports")
	if err != nil {
		t.Fatalf("Estimate after the store recovered: %v", err)
	}
	if got.Value != 5 {
		t.Errorf("Value = %d, want 5", got.Value)
	}
	if len(store.countCalls) != 2 {
		t.Errorf("store Count calls = %d, want 2", len(store.countCalls))
	}
}
