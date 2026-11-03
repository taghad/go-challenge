package redisstore

import (
	"context"
	"fmt"
	"net"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/taghad/go-challenge/internal/estimator"
)

// testAddr returns the address of redis-test. A missing address fails the test
// instead of skipping it, so these tests cannot silently stop running.
func testAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Fatal("REDIS_ADDR is not set; run the tests with: docker compose run --rm test")
	}
	return addr
}

func newTestClient(t *testing.T) redis.UniversalClient {
	t.Helper()
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{testAddr(t)}})
	t.Cleanup(func() { client.Close() })
	return client
}

// newSegment returns a segment name no other run has used, and deletes its keys when
// the test ends. Tests never flush the database, so a wrong REDIS_ADDR cannot erase
// real data. The cleanup runs before the client closes, because cleanups run in
// reverse order.
func newSegment(t *testing.T, client redis.UniversalClient) string {
	t.Helper()
	segment := fmt.Sprintf("test-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		ctx := context.Background()
		keys, err := client.Keys(ctx, "es:{"+segment+"}:*").Result()
		if err != nil {
			t.Errorf("list test keys: %v", err)
			return
		}
		if len(keys) > 0 {
			if err := client.Del(ctx, keys...).Err(); err != nil {
				t.Errorf("delete test keys: %v", err)
			}
		}
	})
	return segment
}

func newWindow(t *testing.T) estimator.Window {
	t.Helper()
	window, err := estimator.NewWindow(6 * time.Hour)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	return window
}

func events(segment string, users ...string) []estimator.Event {
	batch := make([]estimator.Event, len(users))
	for i, user := range users {
		batch[i] = estimator.Event{UserID: user, Segment: segment}
	}
	return batch
}

func mustAdd(t *testing.T, store *Store, bucket estimator.Bucket, batch []estimator.Event) {
	t.Helper()
	if err := store.Add(context.Background(), bucket, batch); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

func wantCount(t *testing.T, store *Store, segment string, buckets []estimator.Bucket, want uint64) {
	t.Helper()
	got, err := store.Count(context.Background(), segment, buckets)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != want {
		t.Errorf("Count = %d, want %d", got, want)
	}
}

func TestKeyFormat(t *testing.T) {
	got := key("sports", estimator.Bucket{Start: 1794096000, Size: 21600})
	if want := "es:{sports}:21600:1794096000"; got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
}

func TestAddIsIdempotent(t *testing.T) {
	client := newTestClient(t)
	store := New(client)
	segment := newSegment(t, client)
	bucket := newWindow(t).Current(time.Now())
	buckets := []estimator.Bucket{bucket}

	// The batch repeats a user, and the whole batch is then sent again, as a USS
	// retry would.
	batch := events(segment, "u1", "u2", "u1")
	mustAdd(t, store, bucket, batch)
	wantCount(t, store, segment, buckets, 2)
	mustAdd(t, store, bucket, batch)
	wantCount(t, store, segment, buckets, 2)
}

func TestCountIsUnionAcrossBuckets(t *testing.T) {
	client := newTestClient(t)
	store := New(client)
	segment := newSegment(t, client)
	buckets := newWindow(t).Buckets(time.Now())

	// u2 is in both buckets and must be counted once.
	mustAdd(t, store, buckets[0], events(segment, "u1", "u2"))
	mustAdd(t, store, buckets[1], events(segment, "u2", "u3"))

	wantCount(t, store, segment, buckets, 3)
}

func TestCountKeepsSegmentsApart(t *testing.T) {
	client := newTestClient(t)
	store := New(client)
	sports := newSegment(t, client)
	news := sports + "-news"
	bucket := newWindow(t).Current(time.Now())
	t.Cleanup(func() { client.Del(context.Background(), key(news, bucket)) })

	mustAdd(t, store, bucket, append(events(sports, "u1", "u2"), events(news, "u3")...))

	wantCount(t, store, sports, []estimator.Bucket{bucket}, 2)
	wantCount(t, store, news, []estimator.Bucket{bucket}, 1)
}

func TestCountOfUnknownSegmentIsZero(t *testing.T) {
	client := newTestClient(t)
	segment := newSegment(t, client)

	wantCount(t, New(client), segment, newWindow(t).Buckets(time.Now()), 0)
}

func TestAddSetsExpectedExpiryOnEveryKey(t *testing.T) {
	client := newTestClient(t)
	store := New(client)
	ctx := context.Background()
	first := newSegment(t, client)
	second := first + "-second"
	buckets := newWindow(t).Buckets(time.Now())
	t.Cleanup(func() { client.Del(ctx, key(second, buckets[0]), key(second, buckets[1])) })

	// Two segments in each of two buckets make four keys. Each expiry depends only on
	// the bucket, so the second bucket's keys expire earlier than the first's.
	for _, bucket := range buckets[:2] {
		mustAdd(t, store, bucket, append(events(first, "u1"), events(second, "u2")...))
	}

	for _, bucket := range buckets[:2] {
		for _, segment := range []string{first, second} {
			k := key(segment, bucket)
			got, err := client.ExpireTime(ctx, k).Result()
			if err != nil {
				t.Fatalf("EXPIRETIME %s: %v", k, err)
			}
			if want := time.Duration(bucket.ExpireAt().Unix()) * time.Second; got != want {
				t.Errorf("%s expires at %d, want %d", k, got/time.Second, want/time.Second)
			}
		}
	}
}

func TestCountExcludesBucketOutsideWindow(t *testing.T) {
	client := newTestClient(t)
	store := New(client)
	ctx := context.Background()
	segment := newSegment(t, client)
	window := newWindow(t)
	oldest := window.Current(time.Now())
	next := estimator.Bucket{Start: oldest.Start + oldest.Size, Size: oldest.Size}

	mustAdd(t, store, oldest, events(segment, "old"))
	mustAdd(t, store, next, events(segment, "new"))

	// The read window is moved instead of the clock: 14 days after the start of the
	// oldest bucket, it has just left the window. Its key still exists, because it
	// expires at that same real time, which is still far away. So only the read window
	// can have excluded it, not expiry.
	leftAt := time.Unix(oldest.Start, 0).Add(estimator.Retention)
	wantCount(t, store, segment, window.Buckets(leftAt.Add(-time.Second)), 2)
	wantCount(t, store, segment, window.Buckets(leftAt), 1)

	exists, err := client.Exists(ctx, key(segment, oldest)).Result()
	if err != nil {
		t.Fatalf("EXISTS: %v", err)
	}
	if exists != 1 {
		t.Error("the excluded bucket's key does not exist, so the test proves nothing")
	}
}

// pipelineRecorder records the commands of every pipeline that writes a HyperLogLog,
// leaving out what the client sends by itself on a new connection.
type pipelineRecorder struct {
	pipelines [][]redis.Cmder
}

func (r *pipelineRecorder) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

func (r *pipelineRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

func (r *pipelineRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if slices.ContainsFunc(cmds, func(cmd redis.Cmder) bool { return cmd.Name() == "pfadd" }) {
			r.pipelines = append(r.pipelines, cmds)
		}
		return next(ctx, cmds)
	}
}

func TestAddWritesEachSegmentOnceInOneTransaction(t *testing.T) {
	client := newTestClient(t)
	recorder := &pipelineRecorder{}
	client.AddHook(recorder)
	first := newSegment(t, client)
	second := first + "-second"
	bucket := newWindow(t).Current(time.Now())
	t.Cleanup(func() { client.Del(context.Background(), key(second, bucket)) })

	batch := []estimator.Event{
		{UserID: "u1", Segment: first},
		{UserID: "u2", Segment: second},
		{UserID: "u3", Segment: first},
	}
	mustAdd(t, New(client), bucket, batch)

	if len(recorder.pipelines) != 1 {
		t.Fatalf("pipelines = %d, want 1", len(recorder.pipelines))
	}
	cmds := recorder.pipelines[0]
	if len(cmds) != 6 {
		t.Fatalf("commands = %d, want MULTI, 2 PFADD, 2 EXPIREAT, EXEC", len(cmds))
	}
	if cmds[0].Name() != "multi" || cmds[5].Name() != "exec" {
		t.Errorf("transaction is %s ... %s, want multi ... exec", cmds[0].Name(), cmds[5].Name())
	}

	users := make(map[string][]any)
	for i := 1; i < 5; i += 2 {
		pfadd, expireat := cmds[i], cmds[i+1]
		if pfadd.Name() != "pfadd" || expireat.Name() != "expireat" {
			t.Fatalf("commands = %s, %s, want pfadd, expireat", pfadd.Name(), expireat.Name())
		}
		if pfadd.Args()[1] != expireat.Args()[1] {
			t.Errorf("PFADD on %v but EXPIREAT on %v", pfadd.Args()[1], expireat.Args()[1])
		}
		users[pfadd.Args()[1].(string)] = pfadd.Args()[2:]
	}
	if got := users[key(first, bucket)]; !slices.Equal(got, []any{"u1", "u3"}) {
		t.Errorf("first segment users = %v, want [u1 u3]", got)
	}
	if got := users[key(second, bucket)]; !slices.Equal(got, []any{"u2"}) {
		t.Errorf("second segment users = %v, want [u2]", got)
	}
}

func TestPing(t *testing.T) {
	if err := New(newTestClient(t)).Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}
