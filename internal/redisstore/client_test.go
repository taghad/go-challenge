package redisstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/taghad/go-challenge/internal/estimator"
)

func TestNewClientOptions(t *testing.T) {
	config := ClientConfig{
		Addr:         "redis-test:6379",
		DialTimeout:  1 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolTimeout:  4 * time.Second,
	}
	universal := NewClient(config)
	defer universal.Close()

	client, ok := universal.(*redis.Client)
	if !ok {
		t.Fatalf("client is %T, want *redis.Client for one address", universal)
	}
	got := client.Options()
	if got.Addr != config.Addr {
		t.Errorf("Addr = %q, want %q", got.Addr, config.Addr)
	}
	if got.DialTimeout != config.DialTimeout {
		t.Errorf("DialTimeout = %s, want %s", got.DialTimeout, config.DialTimeout)
	}
	if got.ReadTimeout != config.ReadTimeout {
		t.Errorf("ReadTimeout = %s, want %s", got.ReadTimeout, config.ReadTimeout)
	}
	if got.WriteTimeout != config.WriteTimeout {
		t.Errorf("WriteTimeout = %s, want %s", got.WriteTimeout, config.WriteTimeout)
	}
	if got.PoolTimeout != config.PoolTimeout {
		t.Errorf("PoolTimeout = %s, want %s", got.PoolTimeout, config.PoolTimeout)
	}
	// go-redis stores the -1 it was given as 0 retries.
	if got.MaxRetries != 0 {
		t.Errorf("MaxRetries = %d, want no retries", got.MaxRetries)
	}
	if !got.ContextTimeoutEnabled {
		t.Error("ContextTimeoutEnabled is false, so operation deadlines would be ignored")
	}
}

// pauseRedis stops redis-test from processing commands for the given time. It uses
// the real server, so a slow Redis looks the same to the client as it would in
// production. This stops the whole server, so the tests of this package must not run
// in parallel. A failed test still waits for the end of the pause, so that it cannot
// also fail the tests after it.
func pauseRedis(t *testing.T, admin redis.UniversalClient, d time.Duration) (waitForResume func()) {
	t.Helper()
	ctx := context.Background()
	waitForResume = func() {
		t.Helper()
		if err := admin.Ping(ctx).Err(); err != nil {
			t.Errorf("wait for Redis to resume: %v", err)
		}
	}
	if err := admin.Do(ctx, "CLIENT", "PAUSE", d.Milliseconds(), "ALL").Err(); err != nil {
		t.Fatalf("CLIENT PAUSE: %v", err)
	}
	t.Cleanup(waitForResume)
	return waitForResume
}

func TestOperationsStopAtTheirDeadline(t *testing.T) {
	const (
		opTimeout = 100 * time.Millisecond
		pause     = 1500 * time.Millisecond
		// The bound sits well above opTimeout and well below testClientTimeout, so only
		// the operation deadline can have ended the call.
		bound = 400 * time.Millisecond
	)
	admin := newTestClient(t)
	client := newTestClient(t)
	store := New(client, opTimeout)
	segment := newSegment(t, admin)
	bucket := newWindow(t).Current(time.Now())
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
	}{
		{"Add", func() error { return store.Add(ctx, bucket, events(segment, "u1")) }},
		{"Count", func() error {
			_, err := store.Count(ctx, segment, []estimator.Bucket{bucket})
			return err
		}},
		{"Ping", func() error { return store.Ping(ctx) }},
	}

	pauseRedis(t, admin, pause)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			err := tt.call()
			elapsed := time.Since(start)
			if err == nil {
				t.Error("want a timeout error, got nil")
			}
			if elapsed > bound {
				t.Errorf("returned after %s, want at most %s", elapsed, bound)
			}
		})
	}
}

func TestTimeoutFreesEstimateSlot(t *testing.T) {
	const (
		opTimeout = 100 * time.Millisecond
		pause     = 500 * time.Millisecond
	)
	admin := newTestClient(t)
	segment := newSegment(t, admin)
	window := newWindow(t)
	ctx := context.Background()
	mustAdd(t, New(admin, testOpTimeout), window.Current(time.Now()), events(segment, "u1"))

	// One slot, so a slot kept by the timed-out flight would reject the next request.
	store := New(newTestClient(t), opTimeout)
	service := estimator.NewService(store, window, time.Now, time.Minute, 1)

	waitForResume := pauseRedis(t, admin, pause)
	_, err := service.Estimate(ctx, segment)
	if err == nil {
		t.Fatal("Estimate during the pause returned no error")
	}
	if errors.Is(err, estimator.ErrOverloaded) {
		t.Fatalf("Estimate during the pause = %v, want a timeout", err)
	}
	waitForResume()

	got, err := service.Estimate(ctx, segment)
	if err != nil {
		t.Fatalf("Estimate after the timeout: %v", err)
	}
	if got.Value != 1 {
		t.Errorf("Value = %d, want 1", got.Value)
	}
}
