// Package estimator estimates how many distinct users a segment has had in the
// last 14 days.
package estimator

import (
	"fmt"
	"time"
)

// Retention is how long an event counts toward a segment's estimate.
const Retention = 14 * 24 * time.Hour

// Bucket is one fixed slice of time, aligned to the Unix epoch.
type Bucket struct {
	Start int64 // Unix seconds
	Size  int64 // seconds
}

// ExpireAt returns when the bucket leaves the read window. Before this time it
// is still read, so its storage must not expire earlier.
func (b Bucket) ExpireAt() time.Time {
	return time.Unix(b.Start+int64(Retention/time.Second), 0).UTC()
}

// Window calculates buckets of one size. It holds no clock, so a caller can use
// one time for all of its calculations.
type Window struct {
	size  int64
	count int
}

// NewWindow returns a Window for the given bucket size. The size must be a whole
// number of seconds, because keys store it in seconds, and must divide Retention
// exactly.
func NewWindow(size time.Duration) (Window, error) {
	if size <= 0 || size%time.Second != 0 || Retention%size != 0 {
		return Window{}, fmt.Errorf("bucket size %s must be a whole number of seconds that divides %s", size, Retention)
	}
	return Window{size: int64(size / time.Second), count: int(Retention / size)}, nil
}

// Current returns the bucket that contains now.
func (w Window) Current(now time.Time) Bucket {
	sec := now.Unix()
	return Bucket{Start: sec - sec%w.size, Size: w.size}
}

// Buckets returns the buckets to read at now, newest first. The first one is the
// current bucket.
func (w Window) Buckets(now time.Time) []Bucket {
	current := w.Current(now)
	buckets := make([]Bucket, w.count)
	for i := range buckets {
		buckets[i] = Bucket{Start: current.Start - int64(i)*w.size, Size: w.size}
	}
	return buckets
}
