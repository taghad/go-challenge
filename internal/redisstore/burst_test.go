package redisstore

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/taghad/go-challenge/internal/estimator"
)

// The method of the boundary burst, fixed in docs/DESIGN.md. Every value is [E].
const (
	burstRequestInterval = 100 * time.Millisecond
	burstRetryAfter      = time.Second // the fixed value of Retry-After in ES
	burstWarmup          = 20 * time.Second
	burstWindow          = 15 * time.Second
	burstServedLimit     = 60 * time.Second
	burstWriterBatch     = 500
	burstWriterInterval  = 500 * time.Millisecond // 1000 events per second
	burstCacheTTL        = 30 * time.Second       // the default of ES
	burstMaxInflight     = 32                     // the default of ES
)

type burstSample struct {
	offset  time.Duration // from the boundary, when the request started
	latency time.Duration
	err     error
}

// burstRun holds the samples of one bucket size.
type burstRun struct {
	mu       sync.Mutex
	reads    []burstSample
	writes   []burstSample
	served   map[int]time.Duration // first success after the boundary, per segment
	allDone  chan struct{}
	doneOnce sync.Once
}

func (r *burstRun) addRead(segment int, sample burstSample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads = append(r.reads, sample)
	if sample.err != nil || sample.offset < 0 {
		return
	}
	if _, ok := r.served[segment]; !ok {
		r.served[segment] = sample.offset + sample.latency
		if len(r.served) == benchSegments {
			r.doneOnce.Do(func() { close(r.allDone) })
		}
	}
}

func (r *burstRun) addWrite(sample burstSample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes = append(r.writes, sample)
}

// summarizeBurst logs the samples whose request started in [from, to).
func summarizeBurst(t *testing.T, label string, samples []burstSample, from, to time.Duration) {
	t.Helper()
	latencies := make([]time.Duration, 0, len(samples))
	overloaded, storeFailed := 0, 0
	for _, sample := range samples {
		if sample.offset < from || sample.offset >= to {
			continue
		}
		latencies = append(latencies, sample.latency)
		switch {
		case errors.Is(sample.err, estimator.ErrOverloaded):
			overloaded++
		case sample.err != nil:
			storeFailed++
		}
	}
	slices.Sort(latencies)
	var worst time.Duration
	if len(latencies) > 0 {
		worst = latencies[len(latencies)-1]
	}
	failed := overloaded + storeFailed
	rate := 0.0
	if len(latencies) > 0 {
		rate = float64(failed) / float64(len(latencies)) * 100
	}
	t.Logf("%s requests=%d would_be_503=%d (overloaded=%d store_failed=%d) rate_503=%.3f%% latency_ms p50=%.3f p99=%.3f max=%.3f",
		label, len(latencies), failed, overloaded, storeFailed, rate,
		ms(nearestRank(latencies, 50)), ms(nearestRank(latencies, 99)), ms(worst))
}

// TestMeasureBoundaryBurst queries all segments across a bucket boundary, when every
// cache entry becomes invalid at once, while a writer keeps ingesting. It runs in process
// with real Redis, dense keys, the default timeouts and limits of ES, and an injected
// clock, so it does not wait for a real boundary.
//
// The clock runs at real speed and reaches the boundary after the warm-up, which fills
// the cache. Each segment has one querier, which sends one request per interval and waits
// for Retry-After after a failure. Any error of Estimate or Add would be a 503 from ES.
func TestMeasureBoundaryBurst(t *testing.T) {
	admin := newBenchAdmin(t)

	for _, size := range []time.Duration{6 * time.Hour, time.Hour} {
		window, err := estimator.NewWindow(size)
		if err != nil {
			t.Fatalf("NewWindow: %v", err)
		}
		boundary := time.Unix(window.Current(time.Now()).Start, 0)
		populateDense(t, admin, window.Buckets(boundary.Add(-time.Second)))

		store := newBenchStore(t)
		run := &burstRun{served: make(map[int]time.Duration), allDone: make(chan struct{})}
		var started time.Time
		clock := func() time.Time { return boundary.Add(time.Since(started) - burstWarmup) }
		service := estimator.NewService(store, window, clock, burstCacheTTL, burstMaxInflight)

		ctx, stop := context.WithCancel(context.Background())
		var running sync.WaitGroup
		started = time.Now()

		for segment := range benchSegments {
			running.Go(func() {
				generator := rand.New(rand.NewPCG(simBurstSeed, uint64(segment)))
				name := benchSegment(segment)
				time.Sleep(time.Duration(generator.Int64N(int64(burstRequestInterval))))
				for ctx.Err() == nil {
					offset := clock().Sub(boundary)
					begin := time.Now()
					_, err := service.Estimate(context.Background(), name)
					run.addRead(segment, burstSample{offset: offset, latency: time.Since(begin), err: err})
					if err != nil {
						time.Sleep(burstRetryAfter)
						continue
					}
					time.Sleep(burstRequestInterval)
				}
			})
		}
		running.Go(func() {
			generator := rand.New(rand.NewPCG(simBurstSeed, benchSegments))
			ticker := time.NewTicker(burstWriterInterval)
			defer ticker.Stop()
			var batches sync.WaitGroup
			defer batches.Wait()
			for sent := 0; ; sent++ {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				batch := make([]estimator.Event, burstWriterBatch)
				for i := range batch {
					batch[i] = estimator.Event{
						UserID:  fmt.Sprintf("w%d-%d", sent, i),
						Segment: benchSegment(generator.IntN(benchSegments)),
					}
				}
				batches.Go(func() {
					offset := clock().Sub(boundary)
					begin := time.Now()
					err := service.Add(context.Background(), batch)
					run.addWrite(burstSample{offset: offset, latency: time.Since(begin), err: err})
				})
			}
		})

		time.Sleep(burstWarmup + burstWindow)
		select {
		case <-run.allDone:
		case <-time.After(burstServedLimit - burstWindow):
		}
		stop()
		running.Wait()

		run.mu.Lock()
		served := make([]time.Duration, 0, len(run.served))
		for _, at := range run.served {
			served = append(served, at)
		}
		run.mu.Unlock()
		slices.Sort(served)
		t.Logf("bucket=%s buckets_read=%d segments=%d cache_ttl=%s max_inflight_estimate=%d writer_events_per_second=%d",
			size, len(window.Buckets(boundary)), benchSegments, burstCacheTTL, burstMaxInflight, burstWriterBatch*int(time.Second/burstWriterInterval))
		summarizeBurst(t, fmt.Sprintf("bucket=%s estimate before_boundary", size), run.reads, -burstWarmup, 0)
		summarizeBurst(t, fmt.Sprintf("bucket=%s estimate after_boundary_%s", size, burstWindow), run.reads, 0, burstWindow)
		summarizeBurst(t, fmt.Sprintf("bucket=%s add before_boundary", size), run.writes, -burstWarmup, 0)
		summarizeBurst(t, fmt.Sprintf("bucket=%s add after_boundary_%s", size, burstWindow), run.writes, 0, burstWindow)
		t.Logf("bucket=%s segments_served_after_boundary=%d of %d limit=%s first_ms=%.3f median_ms=%.3f all_served_ms=%.3f",
			size, len(served), benchSegments, burstServedLimit,
			ms(nearestRank(served, 1)), ms(nearestRank(served, 50)), ms(nearestRank(served, 100)))
		if len(served) < benchSegments {
			t.Logf("bucket=%s not all segments were served within %s; all_served_ms is the last one served", size, burstServedLimit)
		}
	}
	if err := admin.FlushAll(context.Background()).Err(); err != nil {
		t.Errorf("flush: %v", err)
	}
}

const simBurstSeed = 1
