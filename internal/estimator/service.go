package estimator

import (
	"context"
	"fmt"
	"time"
)

// Estimate is the number of users in a segment at one time.
type Estimate struct {
	Value uint64
	AsOf  time.Time // in UTC
}

// Service records events and answers estimates. It knows nothing about HTTP:
// validation errors are returned as they are, and store errors are wrapped with %w,
// so the caller can tell them apart with errors.Is and errors.As and choose a
// response.
//
// It calls the store directly. The cache and singleflight are added in the next
// step, the limiter belongs to the HTTP layer, and timeouts to the Redis store.
type Service struct {
	store  Store
	window Window
	now    func() time.Time
}

// NewService returns a Service. The clock is injected so that tests can control
// bucket boundaries.
func NewService(store Store, window Window, now func() time.Time) *Service {
	return &Service{store: store, window: window, now: now}
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
func (s *Service) Estimate(ctx context.Context, segment string) (Estimate, error) {
	if err := ValidateSegment(segment); err != nil {
		return Estimate{}, err
	}
	now := s.now()
	value, err := s.store.Count(ctx, segment, s.window.Buckets(now))
	if err != nil {
		return Estimate{}, fmt.Errorf("count segment %s: %w", segment, err)
	}
	return Estimate{Value: value, AsOf: now.UTC()}, nil
}
