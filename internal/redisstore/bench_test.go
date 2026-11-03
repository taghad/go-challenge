package redisstore

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/taghad/go-challenge/internal/estimator"
)

const (
	benchSegments = 500
	// benchUsersPerBucket fills the registers of each template, so that a merge reads
	// realistic values and not mostly zeros [E].
	benchUsersPerBucket = 20000
	benchLatencySamples = 2000 // [E]
	benchKeyTTL         = 24 * time.Hour
	// benchAdminTimeout allows for slow admin commands such as FLUSHALL on 2 GB.
	benchAdminTimeout = 2 * time.Minute
	// recomputesPerSecond is 500 segments with the default cache TTL of 30s, per instance.
	recomputesPerSecond = float64(benchSegments) / 30
)

// The default timeouts of ES [E], so that the benchmark uses the same client as ES.
var benchClientConfig = ClientConfig{
	DialTimeout:  500 * time.Millisecond,
	ReadTimeout:  250 * time.Millisecond,
	WriteTimeout: 250 * time.Millisecond,
	PoolTimeout:  100 * time.Millisecond,
}

const benchOpTimeout = 500 * time.Millisecond

// newBenchAdmin connects to redis-bench. These tests flush the database, so they run
// only when asked to, and they refuse any Redis that is not configured for the benchmark.
func newBenchAdmin(t *testing.T) redis.UniversalClient {
	t.Helper()
	addr := os.Getenv("REDIS_BENCH_ADDR")
	if addr == "" {
		t.Skip("REDIS_BENCH_ADDR is not set; run with: docker compose run --rm bench")
	}
	admin := NewClient(ClientConfig{
		Addr:         addr,
		DialTimeout:  testClientTimeout,
		ReadTimeout:  benchAdminTimeout,
		WriteTimeout: benchAdminTimeout,
		PoolTimeout:  benchAdminTimeout,
	})
	t.Cleanup(func() { admin.Close() })
	config, err := admin.ConfigGet(context.Background(), "hll-sparse-max-bytes").Result()
	if err != nil {
		t.Fatalf("config get: %v", err)
	}
	if config["hll-sparse-max-bytes"] != "0" {
		t.Fatalf("hll-sparse-max-bytes = %q at %s; refusing to flush a Redis that is not redis-bench", config["hll-sparse-max-bytes"], addr)
	}
	return admin
}

func benchSegment(i int) string {
	return fmt.Sprintf("seg-%03d", i)
}

func newBenchStore(t *testing.T) *Store {
	t.Helper()
	config := benchClientConfig
	config.Addr = os.Getenv("REDIS_BENCH_ADDR")
	client := NewClient(config)
	t.Cleanup(func() { client.Close() })
	return New(client, benchOpTimeout)
}

// populateDense writes every key of 500 segments for the window at the given time.
//
// Each bucket has one template, filled with its own users. Writing every user to all
// 500 segments would take too long, so the templates are copied to the segments. COPY
// keeps the encoding and the expiry. The expiry is a fixed time and not the one ES
// sets, so a key cannot expire during the benchmark. The memory of an expiry is the same.
//
// Every key is then checked to be dense, because a sparse key would make the memory
// result meaningless.
func populateDense(t *testing.T, admin redis.UniversalClient, buckets []estimator.Bucket) {
	t.Helper()
	ctx := context.Background()
	if err := admin.FlushAll(ctx).Err(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	templates := make([]string, len(buckets))
	for i := range buckets {
		templates[i] = fmt.Sprintf("template:%d", i)
		pipe := admin.Pipeline()
		users := make([]any, 0, estimator.MaxBatchSize)
		for j := range benchUsersPerBucket {
			users = append(users, fmt.Sprintf("b%d-u%d", i, j))
			if len(users) == cap(users) {
				pipe.PFAdd(ctx, templates[i], users...)
				users = make([]any, 0, estimator.MaxBatchSize)
			}
		}
		pipe.Expire(ctx, templates[i], benchKeyTTL)
		if _, err := pipe.Exec(ctx); err != nil {
			t.Fatalf("fill template: %v", err)
		}
	}
	for s := range benchSegments {
		pipe := admin.Pipeline()
		for i, bucket := range buckets {
			pipe.Copy(ctx, templates[i], key(benchSegment(s), bucket), 0, false)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			t.Fatalf("copy templates: %v", err)
		}
	}
	if err := admin.Del(ctx, templates...).Err(); err != nil {
		t.Fatalf("delete templates: %v", err)
	}

	size, err := admin.DBSize(ctx).Result()
	if err != nil {
		t.Fatalf("dbsize: %v", err)
	}
	if want := int64(benchSegments * len(buckets)); size != want {
		t.Fatalf("keys = %d, want %d", size, want)
	}
	for s := range benchSegments {
		pipe := admin.Pipeline()
		encodings := make([]*redis.Cmd, len(buckets))
		for i, bucket := range buckets {
			encodings[i] = pipe.Do(ctx, "PFDEBUG", "ENCODING", key(benchSegment(s), bucket))
		}
		if _, err := pipe.Exec(ctx); err != nil {
			t.Fatalf("pfdebug: %v", err)
		}
		for i, encoding := range encodings {
			if got := encoding.Val(); got != "dense" {
				t.Fatalf("%s bucket %d has encoding %v, want dense", benchSegment(s), i, got)
			}
		}
	}
	ttl, err := admin.TTL(ctx, key(benchSegment(0), buckets[0])).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("ttl = %s, want the copied key to keep its expiry", ttl)
	}
}

// infoField reads one "name:value" line of an INFO section.
func infoField(t *testing.T, admin redis.UniversalClient, section, name string) string {
	t.Helper()
	info, err := admin.Info(context.Background(), section).Result()
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	for line := range strings.Lines(info) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), name+":"); ok {
			return value
		}
	}
	t.Fatalf("info %s has no %s", section, name)
	return ""
}

// pfcountUsecPerCall returns the mean time PFCOUNT ran inside Redis. It excludes the
// network and any wait in the queue of the single Redis thread, so it is the Redis time
// a recomputation uses.
func pfcountUsecPerCall(t *testing.T, admin redis.UniversalClient) float64 {
	t.Helper()
	stats := infoField(t, admin, "commandstats", "cmdstat_pfcount")
	for field := range strings.SplitSeq(stats, ",") {
		if value, ok := strings.CutPrefix(field, "usec_per_call="); ok {
			usec, err := strconv.ParseFloat(value, 64)
			if err != nil {
				t.Fatalf("parse %q: %v", field, err)
			}
			return usec
		}
	}
	t.Fatalf("no usec_per_call in %q", stats)
	return 0
}

// nearestRank returns a percentile of sorted samples.
func nearestRank(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := (len(sorted)*p + 99) / 100
	return sorted[max(rank, 1)-1]
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// TestMeasureBucketBenchmark reports what the decision rule needs for each bucket size,
// on real Redis with fully dense keys: memory, uncached estimate latency through the
// real Store and client, and the share of Redis time that recomputation would use. It
// runs one client, one request at a time, so that the latency has no queue in it.
func TestMeasureBucketBenchmark(t *testing.T) {
	admin := newBenchAdmin(t)
	store := newBenchStore(t)
	ctx := context.Background()
	t.Logf("redis_version=%s allocator=%s", infoField(t, admin, "server", "redis_version"), infoField(t, admin, "memory", "mem_allocator"))

	for _, size := range []time.Duration{6 * time.Hour, time.Hour} {
		window, err := estimator.NewWindow(size)
		if err != nil {
			t.Fatalf("NewWindow: %v", err)
		}
		buckets := window.Buckets(time.Now())
		if err := admin.FlushAll(ctx).Err(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		if err := admin.Do(ctx, "MEMORY", "PURGE").Err(); err != nil {
			t.Fatalf("purge: %v", err)
		}
		baseline := infoField(t, admin, "memory", "used_memory")

		started := time.Now()
		populateDense(t, admin, buckets)
		populated := time.Since(started)
		if err := admin.Do(ctx, "MEMORY", "PURGE").Err(); err != nil {
			t.Fatalf("purge: %v", err)
		}
		keyBytes, err := admin.MemoryUsage(ctx, key(benchSegment(0), buckets[0])).Result()
		if err != nil {
			t.Fatalf("memory usage: %v", err)
		}
		t.Logf("bucket=%s keys=%d encoding=dense(all checked) populate=%s baseline_used_memory=%s used_memory=%s used_memory_human=%s used_memory_rss=%s fragmentation=%s memory_usage_one_key=%d",
			size, benchSegments*len(buckets), populated.Round(time.Millisecond), baseline,
			infoField(t, admin, "memory", "used_memory"), infoField(t, admin, "memory", "used_memory_human"),
			infoField(t, admin, "memory", "used_memory_rss"), infoField(t, admin, "memory", "mem_fragmentation_ratio"), keyBytes)

		if err := admin.ConfigResetStat(ctx).Err(); err != nil {
			t.Fatalf("resetstat: %v", err)
		}
		samples := make([]time.Duration, 0, benchLatencySamples)
		failures := 0
		var total time.Duration
		for i := range benchLatencySamples {
			start := time.Now()
			_, err := store.Count(ctx, benchSegment(i%benchSegments), buckets)
			elapsed := time.Since(start)
			if err != nil {
				failures++
				continue
			}
			samples = append(samples, elapsed)
			total += elapsed
		}
		slices.Sort(samples)
		if len(samples) == 0 {
			t.Fatalf("bucket=%s: every Count failed", size)
		}
		mean := total / time.Duration(len(samples))
		t.Logf("bucket=%s buckets_read=%d samples=%d failures=%d client_ms mean=%.3f p50=%.3f p90=%.3f p99=%.3f max=%.3f",
			size, len(buckets), len(samples), failures, ms(mean), ms(nearestRank(samples, 50)), ms(nearestRank(samples, 90)), ms(nearestRank(samples, 99)), ms(samples[len(samples)-1]))

		usec := pfcountUsecPerCall(t, admin)
		t.Logf("bucket=%s recomputes_per_second=%.2f usec_per_call=%.2f recomputation_share_of_redis_time=%.3f%% (client mean: %.3f%%, client p99: %.3f%%)",
			size, recomputesPerSecond, usec, recomputesPerSecond*usec/1e6*100,
			recomputesPerSecond*mean.Seconds()*100, recomputesPerSecond*nearestRank(samples, 99).Seconds()*100)
	}
	if err := admin.FlushAll(ctx).Err(); err != nil {
		t.Errorf("flush: %v", err)
	}
}
