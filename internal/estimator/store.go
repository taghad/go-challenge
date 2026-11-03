package estimator

import "context"

// Store keeps the users of each segment per bucket. The service defines it so that
// it depends on no storage package, and its tests can use a fake.
type Store interface {
	// Add records the batch in one bucket and sets the expiry of each key. A batch
	// can hold several segments. Grouping them belongs here and not in the service,
	// because how to group depends on how the implementation writes.
	Add(ctx context.Context, bucket Bucket, events []Event) error
	// Count returns the distinct users of a segment across the given buckets.
	Count(ctx context.Context, segment string, buckets []Bucket) (uint64, error)
	// Ping reports whether the store is reachable.
	Ping(ctx context.Context) error
}
