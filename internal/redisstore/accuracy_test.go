package redisstore

import (
	"context"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/taghad/go-challenge/internal/estimator"
)

// measureEnv selects the full size of a measurement. Without it, the default test run
// uses the same code at a small size, because the full size is too slow under -race.
const measureEnv = "ES_MEASURE"

// accuracyMargin is 3 standard errors of about 0.81% [E].
const accuracyMargin = 0.0243

// accuracyTrials is how many independent sets of users are measured at each size [E].
// HyperLogLog is deterministic for one set of users, so one set is one sample.
const accuracyTrials = 5

// TestMeasureHLLAccuracy writes known numbers of distinct users through the Store into
// real Redis and reports the error of each count. Each trial has its own user IDs and
// segment, and it writes in batches of the largest size ES accepts.
func TestMeasureHLLAccuracy(t *testing.T) {
	sizes := []int{1000}
	if os.Getenv(measureEnv) != "" {
		sizes = []int{1000, 100000, 1000000}
	}
	client := newTestClient(t)
	store := New(client, testOpTimeout)
	ctx := context.Background()
	bucket := newWindow(t).Current(time.Now())

	for _, size := range sizes {
		var worst float64
		for trial := range accuracyTrials {
			segment := newSegment(t, client)
			batch := make([]estimator.Event, 0, estimator.MaxBatchSize)
			for i := range size {
				batch = append(batch, estimator.Event{UserID: fmt.Sprintf("t%d-u%d", trial, i), Segment: segment})
				if len(batch) == cap(batch) || i == size-1 {
					mustAdd(t, store, bucket, batch)
					batch = batch[:0]
				}
			}
			count, err := store.Count(ctx, segment, []estimator.Bucket{bucket})
			if err != nil {
				t.Fatalf("Count: %v", err)
			}
			relative := (float64(count) - float64(size)) / float64(size)
			worst = max(worst, math.Abs(relative))
			t.Logf("users=%d trial=%d estimate=%d error=%+.4f%%", size, trial, count, relative*100)
		}
		t.Logf("users=%d trials=%d largest_absolute_error=%.4f%%", size, accuracyTrials, worst*100)
		if worst > accuracyMargin {
			t.Errorf("users=%d: largest error %.4f%% is above %.2f%%", size, worst*100, accuracyMargin*100)
		}
	}
}
