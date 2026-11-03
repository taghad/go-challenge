package estimator

import (
	"slices"
	"testing"
	"time"
)

// boundary is a Unix time that starts a bucket of every size used in these
// tests except 7 and 14 days.
const boundary = 1794096000

func mustWindow(t *testing.T, size time.Duration) Window {
	t.Helper()
	w, err := NewWindow(size)
	if err != nil {
		t.Fatalf("NewWindow(%s): %v", size, err)
	}
	return w
}

func TestNewWindowAcceptsSizesThatDivideRetention(t *testing.T) {
	sizes := []time.Duration{
		time.Second,
		time.Hour,
		6 * time.Hour,
		24 * time.Hour,
		7 * 24 * time.Hour,
		Retention,
	}
	for _, size := range sizes {
		if _, err := NewWindow(size); err != nil {
			t.Errorf("NewWindow(%s): %v", size, err)
		}
	}
}

func TestNewWindowRejectsInvalidSizes(t *testing.T) {
	sizes := []time.Duration{
		0,
		-6 * time.Hour,
		5 * time.Hour,                 // does not divide 14 days
		Retention + time.Hour,         // larger than 14 days
		2 * Retention,                 // larger than 14 days
		500 * time.Millisecond,        // divides 14 days, but not whole seconds
		time.Nanosecond,               // divides 14 days, but not whole seconds
		6*time.Hour + time.Nanosecond, // does not divide 14 days
	}
	for _, size := range sizes {
		if _, err := NewWindow(size); err == nil {
			t.Errorf("NewWindow(%s): got nil error", size)
		}
	}
}

func TestCurrentAtBoundaries(t *testing.T) {
	const size = int64(6 * 3600)
	w := mustWindow(t, 6*time.Hour)

	tests := []struct {
		name string
		now  time.Time
		want int64
	}{
		{"at boundary", time.Unix(boundary, 0), boundary},
		{"just after boundary", time.Unix(boundary, 1), boundary},
		{"just before boundary", time.Unix(boundary, -1), boundary - size},
		{"just before next boundary", time.Unix(boundary+size, -1), boundary},
		{"at next boundary", time.Unix(boundary+size, 0), boundary + size},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := Bucket{Start: tt.want, Size: size}
			if got := w.Current(tt.now); got != want {
				t.Errorf("Current = %+v, want %+v", got, want)
			}
		})
	}
}

func TestCurrentIsAlignedToUnixEpoch(t *testing.T) {
	// time.Truncate aligns to the zero time, which is not a multiple of 7 days
	// from the Unix epoch, so this size tells the two apart.
	const size = int64(7 * 24 * 3600)
	w := mustWindow(t, 7*24*time.Hour)

	if got := w.Current(time.Unix(boundary, 0)).Start; got%size != 0 {
		t.Errorf("Start = %d is not a multiple of %d", got, size)
	}
	if got := w.Current(time.Unix(size-1, 0)).Start; got != 0 {
		t.Errorf("Start in the first bucket = %d, want 0", got)
	}
}

func TestCurrentIgnoresTimeZone(t *testing.T) {
	w := mustWindow(t, 6*time.Hour)
	now := time.Unix(boundary+100, 0)
	zone := time.FixedZone("UTC+3:30", 3*3600+1800)

	if got, want := w.Current(now.In(zone)), w.Current(now.UTC()); got != want {
		t.Errorf("Current in zone = %+v, in UTC = %+v", got, want)
	}
}

func TestBucketsWindowLength(t *testing.T) {
	tests := []struct {
		size time.Duration
		want int
	}{
		{6 * time.Hour, 56},
		{time.Hour, 336},
		{Retention, 1},
	}
	for _, tt := range tests {
		t.Run(tt.size.String(), func(t *testing.T) {
			got := mustWindow(t, tt.size).Buckets(time.Unix(boundary, 0))
			if len(got) != tt.want {
				t.Errorf("len = %d, want %d", len(got), tt.want)
			}
		})
	}
}

func TestBucketsAreContiguousNewestFirst(t *testing.T) {
	w := mustWindow(t, 6*time.Hour)
	now := time.Unix(boundary+100, 0)

	got := w.Buckets(now)
	if got[0] != w.Current(now) {
		t.Errorf("first = %+v, want the current bucket %+v", got[0], w.Current(now))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Start != got[i-1].Start-got[i].Size {
			t.Errorf("bucket %d starts at %d, after %d", i, got[i].Start, got[i-1].Start)
		}
	}
}

func TestBucketsCoverBetweenRetentionMinusSizeAndRetention(t *testing.T) {
	for _, size := range []time.Duration{time.Hour, 6 * time.Hour} {
		t.Run(size.String(), func(t *testing.T) {
			w := mustWindow(t, size)
			coverage := func(now time.Time) time.Duration {
				buckets := w.Buckets(now)
				oldest := time.Unix(buckets[len(buckets)-1].Start, 0)
				return now.Sub(oldest)
			}

			// Shortest right after a boundary.
			atBoundary := time.Unix(boundary, 0)
			if got, want := coverage(atBoundary), Retention-size; got != want {
				t.Errorf("at boundary = %s, want %s", got, want)
			}
			// Longest right before the next one, and still under 14 days.
			beforeNext := atBoundary.Add(size - time.Nanosecond)
			if got, want := coverage(beforeNext), Retention-time.Nanosecond; got != want {
				t.Errorf("before next boundary = %s, want %s", got, want)
			}
		})
	}
}

func TestExpireAtValue(t *testing.T) {
	b := Bucket{Start: boundary, Size: 6 * 3600}
	if got, want := b.ExpireAt().Unix(), int64(1795305600); got != want {
		t.Errorf("ExpireAt = %d, want %d", got, want)
	}
}

// An event must be read until it is 14 days past its bucket start and never
// afterward, and the bucket must not expire while it is still read.
func TestEventLeavesWindowExactlyAtExpireAt(t *testing.T) {
	for _, size := range []time.Duration{time.Hour, 6 * time.Hour, Retention} {
		t.Run(size.String(), func(t *testing.T) {
			w := mustWindow(t, size)
			received := time.Unix(boundary+1234, 0)
			bucket := w.Current(received)
			expire := bucket.ExpireAt()

			if !slices.Contains(w.Buckets(received), bucket) {
				t.Error("bucket is not read when the event is received")
			}
			if !slices.Contains(w.Buckets(expire.Add(-time.Nanosecond)), bucket) {
				t.Error("bucket is not read just before ExpireAt, so the window is shorter than ExpireAt assumes")
			}
			if slices.Contains(w.Buckets(expire), bucket) {
				t.Error("bucket is still read at ExpireAt, after its key has expired")
			}
		})
	}
}

// An event that is 14 days old or more is never read.
func TestEventOlderThanRetentionIsNeverRead(t *testing.T) {
	for _, size := range []time.Duration{time.Hour, 6 * time.Hour} {
		t.Run(size.String(), func(t *testing.T) {
			w := mustWindow(t, size)
			received := time.Unix(boundary+1234, 0)
			bucket := w.Current(received)

			for age := Retention; age < Retention+2*size; age += time.Minute {
				if slices.Contains(w.Buckets(received.Add(age)), bucket) {
					t.Fatalf("event of age %s is still read", age)
				}
			}
		})
	}
}
