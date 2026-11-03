# Estimation Service: Design

The Estimation Service (ES) receives `(user_id, segment)` pairs from the User
Segmentation Service (USS) and answers one question: how many users are in a
segment right now?

**Status:** design approved, implementation not started. Every capacity and
accuracy number below is an estimate until the [results](#measured-results)
section is filled in.

**Number labels**

- `[M]` measured on a stated machine.
- `[E]` estimated. This includes calculations, values taken from Redis
  documentation that have not been verified here, and configuration defaults
  that have not been validated by a test yet.

## Requirements

- Receive `(user_id, segment)` pairs from USS through an API.
- Implement `estimate(segment)`, returning the number of users in a segment.
- A user stays in a segment for two weeks. After that the user is not counted.
- Handle millions of users and hundreds of segments.
- Use Go and Gin, run everything with Docker, and include a simple frontend.

## Decisions on ambiguous requirements

| Question | Decision | Reason |
|---|---|---|
| Does a repeat event renew the two weeks? | Yes | Approved. The estimate is the number of distinct users with an event in the last 14 days. |
| Is an approximate count acceptable? | Yes | Approved. About 0.81% standard error `[E]`. |
| Which time does an event use? | Time of receipt at ES | USS sends no timestamp. A retried event renews from the retry time. |
| Does USS retry after a `503`? | Assumed yes | If it does not, rejected events are lost and a durable queue is needed. |
| Is authentication in scope? | No | ES is assumed to run on an internal network. |
| What does an unknown segment return? | `200` with `0` | It cannot be told apart from a segment with no recent events. |
| What happens to a batch with one invalid event? | The whole batch gets `400` | Partial acceptance makes retries harder to reason about. |

## Storage and counting

Comparison scenario `[E]`: 10M users, 5 segments per user on average (50M
memberships), and 500 segments.

| | Redis sorted sets | Postgres | **Time-bucketed Redis HLL (chosen)** |
|---|---|---|---|
| Write | `ZADD seg now user` | Upsert `(segment, user_id, last_seen)` | `PFADD` into the current bucket |
| Accuracy | Exact | Exact | About 0.81% standard error `[E]` |
| Size in scenario | About 5 GB RAM `[E]` | About 5-8 GB disk `[E]` | At most 344 MB RAM `[E]` |
| Size at 10x memberships | About 50 GB RAM `[E]` | About 50-80 GB disk `[E]` | At most 344 MB RAM `[E]` |
| Expiry | Periodic cleanup | Partitions or a delete job | Key expiry |
| Main weakness | RAM grows with memberships | Costly writes and counts | Approximate; cannot list users |

HyperLogLog is chosen because its memory does not depend on the number of users
and the challenge only asks for an estimate. Redis provides it, along with key
expiry, so ES does not implement either one.

### How a count works

Time is divided into fixed buckets aligned to the Unix epoch in UTC. Each
segment has one HLL per bucket.

- **Key:** `es:{sports}:21600:1794096000`, which holds the segment, the bucket
  size in seconds, and the bucket start in Unix seconds. The `{sports}` hash tag
  keeps all of a segment's keys in one Redis Cluster slot, so a multi-key
  `PFCOUNT` keeps working if Redis is later clustered.
- **Write:** `PFADD` into the current bucket and `EXPIREAT` in one `TxPipeline`.
  The expiry is absolute, so repeated writes set the same value.
- **Read:** `PFCOUNT` over the current bucket and the previous ones, for a total
  of `14 days / bucket size` buckets.

### Expiry guarantee

The read window decides who is counted. Key expiry only reclaims memory.

**Guarantee:** an event older than 14 days is never counted, measured from
receipt at ES. It holds up to clock skew between ES instances. A response can
also reach the client after a bucket boundary if the Redis call was in flight
when the boundary passed, bounded by the operation deadline.

With bucket size `S`, the window is `N = 14d / S` buckets including the current
one. An event in the oldest bucket is at most `(N - 1) * S` plus the time elapsed
in the current bucket, which is less than 14 days.

### Boundary error

The window covers between `14d - S` and `14d`. It is shortest right after a
boundary and grows until the next one, so a user can drop out up to `S` early.

- Early removal is always an **undercount**. It never adds users.
- The measured total error can still go either way, because HLL noise is
  two-sided and sits on top of the boundary undercount.
- **The share of time lost is not the share of users lost.** `S / 14d` is 1.8%
  for 6h, but the undercount depends on how many users have their only event in
  the dropped slice. If a campaign brought 30% of a segment's users within one
  6h slice and they never returned, the estimate can be about 30% low for up to
  6h before they would have expired `[E]`.

The real undercount is therefore measured, not derived from time. See the
[test plan](#test-plan).

### Bucket size

`BUCKET_SIZE` is configurable and defaults to `6h`. It must divide 14 days
exactly, or ES refuses to start. The final default is chosen from measurements,
using the rule below.

| Bucket size | Buckets | Max memory, 500 segments `[E]` | Uncached estimate `[E]` | Time not covered, worst case |
|---|---:|---:|---:|---:|
| 6h | 56 | 344 MB | About 2 ms | 1.8% of the window |
| 1h | 336 | 2.06 GB | About 11 ms | 0.3% of the window |

**Decision rule, fixed before measuring.** Keep 6h unless all of these hold, in
which case use 1h. The thresholds are judgment calls, not measurements.

1. The worst 6h undercount under uniform traffic exceeds 1%.
2. Uncached estimate p99 at 1h is at most 20 ms.
3. Redis memory at 1h for 500 dense segments is at most 2.5 GB.
4. Estimate recomputation at 1h uses at most 20% of Redis time when all 500
   segments are queried continuously.

**Changing the bucket size on existing data is not safe.** The bucket size is
part of the key, so old keys are never misread, but they are also no longer
read. Estimates undercount until a full 14 days of new data has accumulated. No
migration is implemented. Old keys are reclaimed by their expiry.

## API

| Endpoint | Purpose | Responses |
|---|---|---|
| `POST /v1/events` | Ingest a batch of up to 1000 events `[E]` | `204`, `400`, `413`, `503` |
| `GET /v1/segments/:segment/estimate` | Return the estimate for a segment | `200`, `400`, `503` |
| `GET /healthz` | Process is alive | `200` |
| `GET /readyz` | Redis is reachable | `200`, `503` |
| `GET /` | Frontend | `200` |

```http
POST /v1/events
{"events":[{"user_id":"u104010","segment":"sports"}]}

GET /v1/segments/sports/estimate
{"segment":"sports","estimate":1042311,"as_of":"2026-11-03T19:30:00Z"}
```

Validation:

- `segment` must match `^[a-zA-Z0-9_-]{1,64}$`. It is case-sensitive.
- `user_id` must be 1 to 128 bytes.

`204` is returned only after Redis accepts the write. ES keeps no in-memory write
buffer, so an acknowledged event is not lost when ES crashes.

### Why REST

Gin and the frontend are both HTTP, so one API serves both. gRPC would reduce
payload size, but it would need a gateway for the frontend. Batching offsets the
JSON overhead.

## Components

```text
USS or cmd/uss ──POST /v1/events──▶ ES (Gin, stateless) ──TxPipeline──▶ Redis
Browser ──GET /, GET .../estimate─▶ ES ──────────────────PFCOUNT──────▶ Redis
```

### Store interface

The service depends on a small interface, which it defines itself. The Redis
implementation lives in its own package, and service tests use a fake.

```go
type Store interface {
	// Add records the batch in one bucket and sets the expiry of each key.
	Add(ctx context.Context, bucket Bucket, events []Event) error
	// Count returns the distinct users of a segment across the given buckets.
	Count(ctx context.Context, segment string, buckets []Bucket) (uint64, error)
	// Ping reports whether the store is reachable.
	Ping(ctx context.Context) error
}
```

### Atomic writes

`PFADD` and `EXPIREAT` for a key run in one `TxPipeline`, so a key never exists
without an expiry.

- `PFADD` receives all of a batch's users for a segment in one call.
- A lost `EXEC` reply is safe: USS retries and `PFADD` is idempotent.
- On a Redis Cluster, a batch that spans several slots is atomic per slot, not
  as a whole. The guarantee that matters, one key with its expiry, still holds.
- A transaction blocks Redis while it runs, which is one reason batches are
  capped.

### Estimate cache

`PFCOUNT` across many keys runs on the single Redis thread, so a read spike could
delay writes. Each ES instance caches estimates in memory.

- **An entry never crosses a bucket boundary.** Each entry stores the bucket it
  was computed in, and it is valid only while that is still the current bucket
  and `ESTIMATE_CACHE_TTL` has not passed. Otherwise an expired bucket's users
  would still be served, which would break the expiry guarantee.
- `singleflight` is keyed by segment and bucket start, so a request after a
  boundary cannot join a computation that began before it.
- Within a bucket the set of keys is fixed and only grows, so a cached value can
  only miss recent events. It does not add expired users.
- The cache holds at most 10,000 entries `[E]` and is cleared when full, so
  queries for arbitrary segment names cannot grow it without bound.
- All entries become invalid at a boundary. Recomputation is limited to one
  flight per queried segment, per instance.

Estimated Redis load `[E]`: 500 segments with a 30s TTL is about 17 recomputes
per second per instance. At about 2 ms each, that is about 3% of Redis time for
6h buckets, and about 18% for 1h buckets.

### Timeouts

Redis timeouts are explicit, so a slow Redis fails fast instead of holding
requests open. All are configurable, and the defaults are validated by the spike
test.

| Setting | Default `[E]` | Purpose |
|---|---:|---|
| `REDIS_DIAL_TIMEOUT` | 500 ms | Opening a connection |
| `REDIS_READ_TIMEOUT` | 250 ms | Reading a reply |
| `REDIS_WRITE_TIMEOUT` | 250 ms | Writing a command |
| `REDIS_POOL_TIMEOUT` | 100 ms | Waiting for a free connection |
| `REDIS_OP_TIMEOUT` | 500 ms | Context deadline per store operation |
| Client retries | 0 | USS retries; client retries would amplify overload |

### Other configuration

| Setting | Default `[E]` | Purpose |
|---|---:|---|
| `HTTP_ADDR` | `:8080` | Listen address |
| `REDIS_ADDR` | `redis:6379` | Redis address |
| `BUCKET_SIZE` | `6h` | Bucket size; must divide 14 days |
| `ESTIMATE_CACHE_TTL` | `30s` (`5s` in local compose) | Upper limit on cache age |
| `MAX_INFLIGHT_INGEST` | `256` | Concurrent ingestion requests |
| `MAX_BODY_BYTES` | `512KiB` | Ingestion body limit |

### Tools

| Tool | Reason |
|---|---|
| Go and Gin | Required |
| Redis with AOF | Provides HLL and key expiry; `everysec` bounds crash loss to about 1 second of writes `[E]` |
| `go-redis` v9 | Pipelines, transactions, context support, and Cluster support |
| `x/sync/singleflight` | One computation per segment and bucket |
| `testing`, `httptest` | Sufficient without an assertion library |
| `log/slog` | Structured logs without a dependency |
| `embed` | The frontend ships in the binary, without Nginx, a build step, or CORS |
| Docker multi-stage, Compose | Small image; tests also run in containers, so Go is not needed on the host |

Left out because no requirement needs them: Kafka, Prometheus, Nginx, and
repository or DTO layers.

### Frontend

One `index.html` with plain JavaScript, embedded in the binary and served from
`GET /` on the same port. It has two forms: submit a `(user_id, segment)` pair,
and get an estimate. It shows `as_of` and states that the number is approximate.
A number can be up to one cache TTL old, which is why local compose uses `5s`.

## Failure modes

| Failure | Behavior |
|---|---|
| Redis unreachable or slow | Writes fail within the timeouts and return `503` with `Retry-After`. `/readyz` fails. Reads use the cache while entries are valid, otherwise `503`. |
| Redis crash | Recovers from AOF. About 1 second of acknowledged writes can be lost `[E]`. |
| Redis out of memory | `noeviction` keeps data from being dropped silently. Writes return `503`. |
| ES crash | No state is lost. USS retries unacknowledged requests and compose restarts ES. |
| Duplicate events or retries | `PFADD` is idempotent, so users are not double counted. |
| Clock skew between ES instances | Events can land in a neighboring bucket. The effect is limited to the skew. |
| Too many segment names | Each segment can use up to 688 KB at 6h `[E]`. The regex limits format, not count, so this risk remains. An allowlist could be added later. |
| Shutdown | On `SIGTERM`, in-flight requests finish and then the process exits. |

### Traffic spikes

**Writes.** Ingestion passes through a non-blocking limiter. When it is full, ES
immediately returns `503` with `Retry-After`. ES builds no internal queue, so its
memory stays bounded, and USS absorbs the spike through retries.

Estimated load `[E]`: 5 events per user per day for 10M users is about 580 events
per second, with a peak of about 5.8k. A further 10x spike is 58k events per
second, which is about 116 requests per second at 500 events per batch. A single
Redis is estimated to handle about 100k operations per second `[E]`. The actual
limit is found by the spike test.

**Reads.** The cache and `singleflight` limit how much read traffic reaches Redis.

**Limit of this design.** If USS cannot retry, rejected events are lost.

### Scaling

ES is stateless, so it scales by adding instances. Redis can move to Cluster
without changing the key format. Clustering is not implemented here.

## Project layout

```text
.
├── cmd/
│   ├── server/main.go       # config, wiring, graceful shutdown
│   └── uss/main.go          # USS traffic simulator
├── internal/
│   ├── estimator/
│   │   ├── window.go        # pure bucket calculation
│   │   ├── validate.go      # event validation
│   │   ├── store.go         # Store interface
│   │   ├── service.go       # Add and Estimate
│   │   └── cache.go         # boundary-limited cache
│   ├── redisstore/          # Redis implementation of Store
│   ├── httpapi/             # router, handlers, ingestion limiter
│   └── web/                 # embedded index.html
├── docs/DESIGN.md
├── Dockerfile
├── docker-compose.yml
├── .dockerignore
├── .gitignore
├── go.mod
└── go.sum
```

## Test plan

All tests run in Docker.

| Type | Coverage |
|---|---|
| Unit | Bucket boundaries and window length with an injected clock; rejection of a bucket size that does not divide 14 days; validation |
| Cache unit | Entry expires at TTL; **entry is not served after a boundary, even within TTL**; a request after a boundary does not join an earlier flight; size limit |
| HTTP (`httptest`) | Status codes, batch and body limits, limiter rejection, store errors |
| Integration, real Redis | Idempotency; union across buckets; every key has an expiry; reading a window that excludes an old bucket |
| HLL accuracy | Error at 1k, 100k, and 1M distinct users |
| Boundary error | See below |
| 1h and 6h benchmark | See below |
| Spike | See below |

### Boundary error experiment

This runs with an injected clock and exact per-bucket sets, without HLL, so that
it isolates boundary error from HLL error.

- **Ground truth:** distinct users with an event in the exact last 14 days.
- **Compared with:** distinct users in the bucket window.
- **Bucket sizes:** 1h and 6h.
- **Traffic profiles:** uniform, and spiky with a large share of users arriving
  in one short slice. Each runs with single-visit and returning users.
- **Sampling:** throughout several bucket cycles, including right after boundaries.
- **Reported:** mean and maximum undercount, with fixed and recorded seeds.

### 1h and 6h benchmark

- Uncached estimate latency (p50 and p99) and ingestion throughput.
- Redis memory for fully dense keys. A benchmark-only Redis uses
  `hll-sparse-max-bytes 0` to force the worst case, and the dense encoding is
  checked with `PFDEBUG ENCODING`.
- Redis time used by recomputation with all segments queried.
- The default is then chosen with the [decision rule](#bucket-size).

### Spike test

`cmd/uss` simulates USS. It takes a rate, batch size, user count, segment count,
and traffic profile. It honors `Retry-After` and reports accepted and rejected
events and latency percentiles.

- **Ramp:** the offered rate doubles at each step until saturation.
- **Saturation point:** the first step where accepted events fall below 99% of
  offered events, or ingestion p99 exceeds 500 ms `[E]`.
- **Reported:** highest sustained rate, `503` rate, Redis and ES resource use.
- **After the spike:** a segment's estimate is compared with the distinct users
  whose events were acknowledged. It should be within 3 standard errors, which
  is about 2.43% `[E]`.

Results describe the test machine only, and the simulator shares that machine
with ES and Redis. They are not a production capacity prediction.

## Implementation plan

Each step is one small commit, with its tests.

1. `docs: add design and implementation plan`
2. `chore: initialize Go module and ignore files`
3. `build: add compose services for Redis and Go tests`
4. `feat(estimator): add configurable time bucket window`
5. `feat(estimator): add event validation`
6. `feat(estimator): add Store interface and estimate service`
7. `feat(estimator): add boundary-limited estimate cache`
8. `feat(redisstore): add atomic HyperLogLog store`
9. `feat(redisstore): add explicit client timeouts`
10. `feat(api): add event ingestion endpoint`
11. `feat(api): add segment estimate endpoint`
12. `feat(api): limit in-flight ingestion requests`
13. `feat(api): add health and readiness endpoints`
14. `feat(server): wire config and graceful shutdown`
15. `feat(web): add embedded frontend`
16. `build: add application Dockerfile and compose service`
17. `feat(uss): add USS traffic simulator`
18. `test: measure HyperLogLog accuracy`
19. `test: measure boundary error under uniform and spiky traffic`
20. `test: benchmark 1h and 6h bucket sizes`
21. `test: add spike test to find the saturation point`
22. `feat(server): set the bucket size default from measurements`, only if the
    decision rule selects 1h
23. `docs: add measured results and final decisions`
24. `docs: link design and run instructions from README`

Work happens on `feat/estimation-service` and is not pushed. After approval, it
is merged into `main` with `--ff-only`, without squashing.

## Measured results

Not measured yet. This section is filled in by step 23, along with the machine
used, and the matching `[E]` values above are replaced.

| Result | Value |
|---|---|
| Test machine | Not measured |
| HLL error at 1k, 100k, 1M | Not measured |
| Boundary undercount, uniform, 1h and 6h | Not measured |
| Boundary undercount, spiky, 1h and 6h | Not measured |
| Uncached estimate p50 and p99, 1h and 6h | Not measured |
| Redis memory, 500 dense segments, 1h and 6h | Not measured |
| Saturation point | Not measured |
| Final default bucket size | Not decided |
