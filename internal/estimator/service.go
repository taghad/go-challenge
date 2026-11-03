package estimator

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"golang.org/x/sync/singleflight"
)

// ErrOverloaded is returned when an estimate must be recomputed and the limit on
// concurrent recomputations is full. The caller can retry.
var ErrOverloaded = errors.New("too many estimate recomputations in flight")

// Estimate is the number of users in a segment at one time.
type Estimate struct {
	Value uint64
	AsOf  time.Time // in UTC; for a cached value, the time it was computed
}

// Service records events and answers estimates. It knows nothing about HTTP:
// validation errors are returned as they are, store errors are wrapped with %w, and
// overload is ErrOverloaded, so the caller can tell them apart with errors.Is and
// errors.As and choose a response. A cancelled caller gets its context's error.
//
// The cache, singleflight, and estimate limit live here, in the path of Estimate,
// because PFCOUNT across many keys runs on the single Redis thread, so a read spike
// could delay writes. Only this path knows whether a response needs the store: a
// limit in the HTTP layer would also reject responses that the cache can serve. The
// ingestion limiter belongs to the HTTP layer, and timeouts to the Redis store.
type Service struct {
	store   Store
	window  Window
	now     func() time.Time
	cache   *estimateCache
	flights singleflight.Group
	// slots is a semaphore on recomputations. singleflight allows one flight per
	// segment and bucket, but arbitrary segment names, or a boundary that invalidates
	// every entry at once, can still start many flights.
	slots chan struct{}
}

// NewService returns a Service. The clock is injected so that tests can control
// bucket boundaries. cacheTTL is the upper limit on the age of a cached estimate, and
// maxInflightEstimate is the limit on concurrent recomputations.
func NewService(store Store, window Window, now func() time.Time, cacheTTL time.Duration, maxInflightEstimate int) *Service {
	return &Service{
		store:  store,
		window: window,
		now:    now,
		cache:  newEstimateCache(cacheTTL),
		slots:  make(chan struct{}, maxInflightEstimate),
	}
}

// Add validates a batch and records it. A rejected batch reaches neither the clock
// nor the store.
//
// The clock is read once, so a whole batch goes into one bucket even if a boundary
// passes while it is handled.
func (s *Service) Add(ctx context.Context, events []Event) error {
	if err := ValidateBatch(events); err != nil {
		return err
	}
	if err := s.store.Add(ctx, s.window.Current(s.now()), events); err != nil {
		return fmt.Errorf("add events: %w", err)
	}
	return nil
}

// Estimate returns the number of users in a segment. An unknown segment returns 0,
// because it cannot be told apart from a segment with no recent events.
//
// The clock is read once, so the window that was counted and the reported time
// agree, even if a boundary passes while the store is read.
//
// The singleflight key is the segment plus the bucket start, so a request after a
// boundary cannot join a flight that began before it and is counting an expired
// bucket. The segment pattern excludes ":", so two keys cannot collide.
//
// The flight is shared, so it must not depend on the context of the caller that
// happened to start it. It runs with context.WithoutCancel through DoChan, and each
// caller waits on its own context, so one cancelled request does not fail the others,
// and the result is still cached.
func (s *Service) Estimate(ctx context.Context, segment string) (Estimate, error) {
	if err := ValidateSegment(segment); err != nil {
		return Estimate{}, err
	}
	now := s.now()
	current := s.window.Current(now)
	if cached, ok := s.cache.get(segment, current, now); ok {
		return cached, nil
	}
	key := segment + ":" + strconv.FormatInt(current.Start, 10)
	flight := s.flights.DoChan(key, func() (any, error) {
		return s.recompute(context.WithoutCancel(ctx), segment, now)
	})
	select {
	case <-ctx.Done():
		return Estimate{}, ctx.Err()
	case result := <-flight:
		if result.Err != nil {
			return Estimate{}, result.Err
		}
		return result.Val.(Estimate), nil
	}
}

// recompute reads the store and caches the result.
//
// The semaphore is non-blocking, because ES builds no internal queue: when it is full
// the caller gets ErrOverloaded at once. It is taken here, inside the flight, so it is
// held only during recomputation: cached responses never touch it, and a caller that
// joins a running flight needs no slot of its own.
//
// Errors are not cached, so the next request tries the store again instead of a short
// outage being served for a whole TTL. The semaphore bounds those retries.
func (s *Service) recompute(ctx context.Context, segment string, now time.Time) (Estimate, error) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return Estimate{}, ErrOverloaded
	}
	value, err := s.store.Count(ctx, segment, s.window.Buckets(now))
	if err != nil {
		return Estimate{}, fmt.Errorf("count segment %s: %w", segment, err)
	}
	estimate := Estimate{Value: value, AsOf: now.UTC()}
	s.cache.put(segment, s.window.Current(now), estimate)
	return estimate, nil
}
