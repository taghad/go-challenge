// Package redisstore keeps the users of each segment in Redis, in one HyperLogLog
// per segment and bucket.
package redisstore

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/taghad/go-challenge/internal/estimator"
)

var _ estimator.Store = (*Store)(nil)

// Store implements estimator.Store. It takes a redis.UniversalClient, so the same
// code works if Redis is later clustered.
//
// Every operation applies its own deadline inside the Store, whatever context the
// caller passes. The estimate path needs this: it calls Count with a context that has
// no deadline, so that one cancelled request does not fail the others, and this
// deadline is what ends a slow flight and frees its slot.
type Store struct {
	client    redis.UniversalClient
	opTimeout time.Duration
}

// New returns a Store that uses the given client, with opTimeout as the deadline of
// each store operation. The client must come from NewClient, or otherwise respect
// context deadlines.
func New(client redis.UniversalClient, opTimeout time.Duration) *Store {
	return &Store{client: client, opTimeout: opTimeout}
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
	ctx, cancel := context.WithTimeout(ctx, s.opTimeout)
	defer cancel()
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
	ctx, cancel := context.WithTimeout(ctx, s.opTimeout)
	defer cancel()
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
	ctx, cancel := context.WithTimeout(ctx, s.opTimeout)
	defer cancel()
	return s.client.Ping(ctx).Err()
}
