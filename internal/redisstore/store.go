// Package redisstore keeps the users of each segment in Redis, in one HyperLogLog
// per segment and bucket.
package redisstore

import (
	"context"
	"strconv"

	"github.com/redis/go-redis/v9"

	"github.com/taghad/go-challenge/internal/estimator"
)

var _ estimator.Store = (*Store)(nil)

// Store implements estimator.Store. It takes a redis.UniversalClient, so the same
// code works if Redis is later clustered.
type Store struct {
	client redis.UniversalClient
}

// New returns a Store that uses the given client.
func New(client redis.UniversalClient) *Store {
	return &Store{client: client}
}

// key holds the segment, the bucket size in seconds, and the bucket start in Unix
// seconds. The hash tag keeps all of a segment's keys in one Redis Cluster slot, so a
// multi-key PFCOUNT keeps working if Redis is later clustered. The bucket size is part
// of the key, so a key written with another size is never misread.
func key(segment string, bucket estimator.Bucket) string {
	return "es:{" + segment + "}:" + strconv.FormatInt(bucket.Size, 10) + ":" + strconv.FormatInt(bucket.Start, 10)
}

// Add groups the batch by segment and writes one variadic PFADD plus one EXPIREAT per
// key in one TxPipeline, so a key never exists without an expiry. The expiry is
// absolute, so repeated writes set the same value. On a Redis Cluster, a batch that
// spans several slots is atomic per slot, which still keeps each key with its expiry.
func (s *Store) Add(ctx context.Context, bucket estimator.Bucket, events []estimator.Event) error {
	users := make(map[string][]any)
	for _, event := range events {
		users[event.Segment] = append(users[event.Segment], event.UserID)
	}
	_, err := s.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		for segment, ids := range users {
			k := key(segment, bucket)
			pipe.PFAdd(ctx, k, ids...)
			pipe.ExpireAt(ctx, k, bucket.ExpireAt())
		}
		return nil
	})
	return err
}

// Count returns the distinct users of a segment across the given buckets. A user in
// several buckets is counted once, because PFCOUNT over several keys counts their
// union. A key that does not exist counts as empty.
func (s *Store) Count(ctx context.Context, segment string, buckets []estimator.Bucket) (uint64, error) {
	keys := make([]string, len(buckets))
	for i, bucket := range buckets {
		keys[i] = key(segment, bucket)
	}
	count, err := s.client.PFCount(ctx, keys...).Result()
	if err != nil {
		return 0, err
	}
	return uint64(count), nil
}

// Ping reports whether Redis is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}
