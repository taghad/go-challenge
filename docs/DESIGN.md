# Estimation Service: Design

The Estimation Service (ES) receives `(user_id, segment)` pairs from the User
Segmentation Service (USS) and answers one question: how many users are in a
segment right now?

**Status:** implemented and measured. The results and their limits are under
[measured results](#measured-results), and the reasons are in
[DECISIONS.md](DECISIONS.md). `[E]` remains only where nothing has measured a value.

**Number labels**

- `[M]` measured on the machine stated under [measured results](#measured-results).
- `[calculated]` computed from `[M]` values, without a run of its own.
- `[E]` estimated. This includes calculations from estimates, values taken from Redis
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
| Is an approximate count acceptable? | Yes | Approved. About 0.81% standard error `[E]`, from Redis documentation. The largest error measured was 2.11% `[M]`. |
| Which time does an event use? | Time of receipt at ES | USS sends no timestamp. A retried event renews from the retry time. |
| Does USS retry after a `503`? | Assumed yes, at most 3 times `[E]` in `cmd/uss` | If it does not, rejected events are lost and a durable queue is needed. |
| Is authentication in scope? | No | ES is assumed to run on an internal network. |
| What does an unknown segment return? | `200` with `0` | It cannot be told apart from a segment with no recent events. |
| What happens to a batch with one invalid event? | The whole batch gets `400` | Partial acceptance makes retries harder to reason about. |

## Storage and counting

Comparison scenario `[E]`: 10M users, 5 segments per user on average (50M
memberships), and 500 segments.

| | Redis sorted sets | Postgres | **Time-bucketed Redis HLL (chosen)** |
|---|---|---|---|
| Write | `ZADD seg now user` | Upsert `(segment, user_id, last_seen)` | `PFADD` into the current bucket |
| Accuracy | Exact | Exact | About 0.81% standard error `[E]`; largest measured error 2.11% `[M]` |
| Size in scenario | About 5 GB RAM `[E]` | About 5-8 GB disk `[E]` | At most 2.43 GB RAM at 1h `[M]` |
| Size at 10x memberships | About 50 GB RAM `[E]` | About 50-80 GB disk `[E]` | At most 2.43 GB RAM at 1h `[M]` |
| Expiry | Periodic cleanup | Partitions or a delete job | Key expiry |
| Main weakness | RAM grows with memberships | Costly writes and counts | Approximate; cannot list users |

HyperLogLog is chosen because its memory does not depend on the number of users
and the challenge only asks for an estimate. Redis provides it, along with key
expiry, so ES does not implement either one.

### How a count works

Time is divided into fixed buckets aligned to the Unix epoch in UTC. Each
segment has one HLL per bucket.

- **Key:** `es:{sports}:3600:1794096000`, which holds the segment, the bucket
  size in seconds, and the bucket start in Unix seconds. The `{sports}` hash tag
  keeps all of a segment's keys in one Redis Cluster slot, so a multi-key
  `PFCOUNT` keeps working if Redis is later clustered.
- **Write:** `PFADD` into the current bucket and `EXPIREAT` in one `TxPipeline`.
  The expiry is absolute, so repeated writes set the same value. The value is
  given under [expiry guarantee](#expiry-guarantee).
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

**Expiry value:** `EXPIREAT` is the bucket start plus 14 days, in Unix seconds.
For bucket `1794096000`, that is `1794096000 + 1209600 = 1795305600`.

A bucket is last read just before `start + N * S`, which equals `start + 14 days`,
so this is the earliest value that never removes a key still in the window, and
at most `N` buckets of a segment are live at once. Redis applies the expiry with
its own clock. If that clock runs ahead of ES, a key can expire early by the skew,
which is an undercount limited to the skew. If it runs behind, the key only uses
memory for longer, because the read window has already stopped counting it.

### Boundary error

The window covers between `14d - S` and `14d`. It is shortest right after a
boundary and grows until the next one, so a user can drop out up to `S` early.

- Early removal is always an **undercount**. It never adds users.
- The measured total error can still go either way, because HLL noise is
  two-sided and sits on top of the boundary undercount.
- **The share of time lost is not the share of users lost.** `S / 14d` is 0.3%
  for 1h, but the undercount depends on how many users have their only event in
  the dropped slice. If a campaign brought 30% of all users within one 6h slice and
  they never returned, the largest undercount at 1h is 22.48%, and at 6h it is 63.86%
  `[M]`. It exceeds 30% because those users are a larger share of the true count at
  that moment.

The real undercount is therefore measured, not derived from time. See the
[test plan](#test-plan).

### Bucket size

`BUCKET_SIZE` is configurable and defaults to `1h`. It must be a whole number of
seconds and divide 14 days exactly, or ES refuses to start. The default was chosen
from measurements, using the rule below. All four rules held, with 1.27%, 6.8 ms,
2.43 GB, and 6.3% `[M]`, so the default is 1h.

| Bucket size | Buckets | Max memory, 500 segments `[M]` | Uncached estimate p99 `[M]` | Time not covered, worst case |
|---|---:|---:|---:|---:|
| 6h | 56 | 405.2 MB | 1.4 ms | 1.8% of the window |
| 1h | 336 | 2.43 GB | 6.8 ms | 0.3% of the window |

**Decision rule, fixed before measuring.** Keep 6h unless all of these hold, in
which case use 1h. The thresholds are judgment calls, not measurements, and their
status is recorded under [thresholds](#thresholds).

1. The worst 6h undercount in the decision scenario exceeds 1%. The scenario is
   defined in the [traffic model](#traffic-model).
2. Uncached estimate p99 at 1h is at most 20 ms.
3. Redis memory at 1h for 500 dense segments is at most 2.5 GB.
4. Estimate recomputation at 1h uses at most 20% of Redis time when all 500
   segments are queried continuously.

Spiky traffic results are reported as well, but they are not part of this rule.

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
| `GET /report` | Static [test report](TEST_REPORT.md) | `200` |

```http
POST /v1/events
{"events":[{"user_id":"u104010","segment":"sports"}]}

GET /v1/segments/sports/estimate
{"segment":"sports","estimate":1042311,"as_of":"2026-11-03T19:30:00Z"}
```

Validation:

- `segment` must match `^[a-zA-Z0-9_-]{1,64}$`. It is case-sensitive.
- `user_id` must be 1 to 128 bytes.
- A batch of more than 1000 events returns `413`.

`204` is returned only after Redis accepts the write. ES keeps no in-memory write
buffer, so an acknowledged event is not lost when ES crashes.

`Retry-After` is a fixed 1 second `[E]`.

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
  flight per queried segment, per instance, and to `MAX_INFLIGHT_ESTIMATE` flights
  per instance. When that limit is full, the estimate returns `503` with
  `Retry-After`. Cached responses never touch the limit.

Redis load: 500 segments with a 30s TTL is 16.7 recomputes per second per instance.
With the measured time inside Redis, that is 1.44% of Redis time for 6h buckets and
6.26% for 1h buckets `[calculated]`.

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

### HTTP server

The router is `gin.New` with `Recovery` and no request `Logger`, in release mode. A
panic in one request returns `500` and the process keeps serving. These are all
configurable.

| Setting | Default `[E]` | Purpose |
|---|---:|---|
| `HTTP_READ_HEADER_TIMEOUT` | 5 s | Reading the request headers |
| `HTTP_READ_TIMEOUT` | 10 s | Reading the whole request |
| `HTTP_WRITE_TIMEOUT` | 10 s | Writing the response |
| `HTTP_IDLE_TIMEOUT` | 60 s | A keep-alive connection between requests |
| `SHUTDOWN_TIMEOUT` | 10 s | In-flight requests finishing after a signal |

### Other configuration

All configuration comes from the environment. An unset variable takes its default.
Any other value that is invalid or not positive, including an empty one, stops ES at
startup with exit code 1 and an error that names every invalid variable.

| Setting | Default `[E]` | Purpose |
|---|---:|---|
| `HTTP_ADDR` | `:8080` | Listen address; host and port, port 1 to 65535 |
| `REDIS_ADDR` | `redis:6379` | Redis address; host and port, port 1 to 65535 |
| `REDIS_MAXMEMORY` | `3gb` | Redis memory limit, set in compose |
| `BUCKET_SIZE` | `1h` `[M]` | Bucket size; a whole number of seconds that divides 14 days |
| `ESTIMATE_CACHE_TTL` | `30s` (`5s` in local compose) | Upper limit on cache age |
| `MAX_INFLIGHT_INGEST` | `256` | Concurrent ingestion requests |
| `MAX_INFLIGHT_ESTIMATE` | `32` | Concurrent estimate recomputations; `503` with `Retry-After` when full |
| `MAX_BODY_BYTES` | `512KiB` | Ingestion body limit; bytes, with an optional `KiB` or `MiB` suffix |

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

### Running with Compose

`docker compose up --build` starts ES and Redis. The ES image is a static binary
(`CGO_ENABLED=0`) on `gcr.io/distroless/static-debian12:nonroot`, running as a
non-root user.

- ES starts only after Redis is healthy, and restarts on failure at most 5 times `[E]`.
  An invalid configuration therefore stops after those restarts, with the error in
  `docker compose logs es`.
- ES has no compose healthcheck, because the image has no shell or client to run one
  with. `/healthz` and `/readyz` are for whatever runs ES.
- ES has no authentication, so it is published on `127.0.0.1:8080` only.
- The stop grace period is 15 s `[E]`, longer than `SHUTDOWN_TIMEOUT`, so that docker
  does not kill ES during a graceful shutdown.

### Frontend

One `index.html` with plain JavaScript, embedded in the binary and served from
`GET /` on the same port. It has no framework, CDN, or external fonts, and it follows
the system light or dark theme and fits a mobile width. The estimate is the main part
of the page, with a second form to send a test event. It shows `as_of` and states that
the number is approximate. A number can be up to one cache TTL old, which is why local
compose uses `5s`.

All output is rendered with `textContent`, so nothing a user typed or the server
returned can become markup. The segment is encoded with `encodeURIComponent` in the
URL path. Serving the page does not touch Redis, so it loads during a Redis outage.

## Failure modes

| Failure | Behavior |
|---|---|
| Redis unreachable or slow | Writes fail within the timeouts and return `503` with `Retry-After`. `/readyz` fails. Reads use the cache while entries are valid, otherwise `503`. |
| Redis crash | Recovers from AOF. About 1 second of acknowledged writes can be lost `[E]`. |
| Redis out of memory | `noeviction` keeps data from being dropped silently. Writes return `503`. |
| ES crash | No state is lost. USS retries unacknowledged requests and compose restarts ES. |
| Duplicate events or retries | `PFADD` is idempotent, so users are not double counted. |
| Clock skew between ES instances | Events can land in a neighboring bucket. The effect is limited to the skew. |
| Too many segment names | Each segment can use up to 4.84 MB at 1h: 336 keys of 14,392 bytes `[M]`. The regex limits format, not count, so this risk remains. An allowlist could be added later. |
| Shutdown | On `SIGTERM` or `SIGINT`, in-flight requests finish and then the process exits. Requests still running after `SHUTDOWN_TIMEOUT` are cut off and the exit code is 1. |
| Invalid configuration | ES exits at startup with an error that names every invalid variable. |

### Traffic spikes

**Writes.** Ingestion passes through a non-blocking limiter. When it is full, ES
immediately returns `503` with `Retry-After`. ES builds no internal queue, so its
memory stays bounded, and USS absorbs the spike through retries.

Estimated load `[E]`: 5 events per user per day for 10M users is about 580 events
per second, with a peak of about 5.8k. A further 10x spike is 58k events per
second, which is about 116 requests per second at 500 events per batch. The ramp
sustained 1,024,000 events per second `[M]`, which is far above this. That number comes
from one laptop with repeated users, as stated under [limits](#limits-of-these-results).

**Reads.** The cache and `singleflight` limit how much read traffic reaches Redis.
`MAX_INFLIGHT_ESTIMATE` limits concurrent recomputations, and when it is full ES
immediately returns `503` with `Retry-After`. This also covers the boundary burst:
all cache entries become invalid at a boundary, so every queried segment recomputes
at once.

**Limit of this design.** If USS cannot retry, rejected events are lost.

### Scaling

ES is stateless, so it scales by adding instances. Redis can move to Cluster
without changing the key format. Clustering is not implemented here.

**Limit: a hot segment stays on one shard.** The `{sports}` hash tag puts all of
a segment's keys in one slot. Clustering spreads different segments across shards,
but it cannot spread the load of one hot segment.

**Next step, not implemented: a fixed split by user.** Each user goes to one of
`K` sub-keys by `hash(user_id) mod K`, for example `es:{sports:3}:3600:1794096000`.
The estimate is the sum of the `K` counts.

- The sum is valid only because the sub-keys are disjoint, so no user is counted
  twice. A single `PFCOUNT` could not span them anyway, because they sit in
  different slots.
- `K` must stay fixed. Changing it moves users to other sub-keys, so the sub-keys
  are no longer disjoint and users are double counted for up to 14 days.
- The split hash must be independent of the hash HLL uses internally. Otherwise
  each sub-key sees a skewed share of its registers and accuracy suffers.
- Memory grows by a factor of `K`. With an even split, standard error falls to
  about `0.81% / sqrt(K)` `[E]`.

### Rollups

**Fallback, not implemented:** if `PFCOUNT` over many buckets proves too slow,
sealed buckets can be merged with `PFMERGE` into coarser rollup keys, and a read
then uses a few rollups plus the recent buckets.

- **An HLL cannot subtract.** A rollup still counts its oldest bucket's users
  until the whole rollup is dropped, so it must leave the read window when its
  oldest bucket would expire, or the expiry guarantee breaks.
- The buckets a dropped rollup covered must still be read, so they have to be
  kept. Rollups therefore add memory and a background merge job to save read time.
- No threshold triggers this option. Rules 2 and 4 of the
  [decision rule](#bucket-size) only choose between 6h and 1h, so using rollups
  would be a separate owner decision based on the measured latency.

## Project layout

```text
.
├── cmd/
│   ├── server/
│   │   ├── config.go        # configuration from the environment
│   │   └── main.go          # wiring, graceful shutdown
│   ├── report/              # generates the test report from docs/results
│   └── uss/                 # USS traffic simulator
├── internal/
│   ├── estimator/
│   │   ├── window.go        # pure bucket calculation
│   │   ├── validate.go      # event validation
│   │   ├── store.go         # Store interface
│   │   ├── service.go       # Add and Estimate
│   │   └── cache.go         # boundary-limited cache
│   ├── redisstore/          # Redis implementation of Store
│   ├── httpapi/             # router, handlers, ingestion limiter
│   └── web/                 # embedded index.html and report.html
├── scripts/spike.sh         # collects docker stats during the spike test
├── docs/
│   ├── DESIGN.md
│   ├── DECISIONS.md         # each decision, why, what was rejected, and its cost
│   ├── TEST_REPORT.md       # generated, with its charts in report/
│   └── results/             # raw result summaries, with the machine
├── CLAUDE.md                # project rules for Claude Code
├── Dockerfile
├── docker-compose.yml
├── .dockerignore
├── .gitignore
├── go.mod
└── go.sum
```

## Test plan

All tests run in Docker. Measurements run at full size with
`docker compose run --rm measure`, without `-race`. The default test run uses the
same code at a small size.

| Type | Coverage |
|---|---|
| Unit | Bucket boundaries and window length with an injected clock; rejection of a bucket size that does not divide 14 days; validation |
| Cache unit | Entry expires at TTL; **entry is not served after a boundary, even within TTL**; a request after a boundary does not join an earlier flight; size limit |
| HTTP (`httptest`) | Status codes, batch and body limits, limiter rejection, store errors |
| Integration, real Redis | Idempotency; union across buckets; every key has an expiry; reading a window that excludes an old bucket |
| HLL accuracy | Error at 1k, 100k, and 1M distinct users, in 5 independent sets of users at each size `[E]`, on real Redis through the Store. The test fails above 2.43%. |
| Boundary error | See below |
| 1h and 6h benchmark | See below |
| Spike | See below |

### Boundary error experiment

This is a simple in-process simulation with exact per-bucket sets. It uses neither
Redis nor HLL, so it isolates boundary error from HLL error.

- **Ground truth:** distinct users with an event in the exact last 14 days.
- **Compared with:** distinct users in the bucket window.
- **Undercount:** `(truth - window count) / truth`, which is never negative.
- **Reported:** mean and maximum undercount for each of the 8 cases below,
  including spiky traffic, along with the seed and the realized return share.

#### Traffic model

The model is fixed before any measurement. ES has no real USS data, so every
parameter is an assumption `[E]`. If real USS data becomes available, it should
replace these values.

| Parameter | Value `[E]` |
|---|---|
| Distinct users | 1,000,000 |
| Simulated period | 56 days: 42 days of warm-up, then 14 days of sampling |
| Sampling | Every 15 minutes, which includes the instant of each 1h and 6h boundary |
| First event, uniform profile | Uniformly random across the simulated period |
| First event, spiky profile | As uniform, except 30% of users have their first event in one 6h slice |
| Spike position | Aligned to a 6h bucket, on day 28, so that it leaves the window during sampling |
| Return share `r` | 0% and 50% |
| Single-visit user, share `1 - r` | One event only |
| Returning user, share `r` | 1 to 8 further events (uniform), with exponential gaps of mean 3 days |
| Events after day 56 | Dropped |
| Seed | 1 |

`r` is the share of users assigned to make at least one return visit. Because
gaps are random and events after day 56 are dropped, a few of those users do not
return within 14 days, so the experiment also reports the realized share: users with
a further event within 14 days of the event before it. It also reports the users with
any further event before day 56.

The spike slice is `[day 28, day 28 + 6h)`. Sampling includes both day 42 and day 56,
which makes 1345 samples. Each combination of profile and `r` has one set of events
with seed 1, and both bucket sizes run on that same set.

**Cases:** both profiles, both return shares, and both bucket sizes, which makes 8
cases.

**Decision scenario:** uniform profile, `r = 50%`, 6h buckets, taking the maximum
undercount. Every other case is reported, but only this one is used by rule 1 of
the [decision rule](#bucket-size).

**Expected result, not a measurement** `[E]`: under uniform arrivals, the maximum
6h undercount is about 1.8% at `r = 0%` and about 1.2% at `r = 50%`, right after a
boundary. The time-averaged undercount is about half of that. If measurements
agree, rule 1 will hold, and the choice will depend on rules 2 to 4. The measured values
are 1.84% and 1.27%, with means of 0.93% and 0.63% `[M]`, so rule 1 held.

### 1h and 6h benchmark

It runs with `docker compose run --rm bench`, on 500 segments.

- Uncached `PFCOUNT` latency (p50 and p99), on real Redis, from 2000 calls `[E]` to
  `Store.Count` per bucket size, one at a time, with the default client timeouts.
- Redis memory for fully dense keys, on real Redis. A benchmark-only Redis uses
  `hll-sparse-max-bytes 0` to force the worst case, and the dense encoding of every
  key is checked with `PFDEBUG ENCODING`.
- The registers hold real values. Each bucket has one template with 20,000 users
  `[E]`, which `COPY` gives to all 500 segments, because writing every user to every
  segment would take too long.
- The recomputation share of Redis time is calculated from the measured `PFCOUNT`
  latency and the recompute rate. It is not run as a separate experiment. It is 16.7
  recomputes per second (500 segments with a 30 s TTL, per instance) times the mean time
  inside Redis, from `usec_per_call` in `INFO commandstats`. That excludes the network,
  so it is the Redis time itself. The share from the client latency is reported as well,
  but rule 4 uses the Redis-side value.
- The default is then chosen with the [decision rule](#bucket-size).

Measurements use real Redis only for HLL accuracy, this benchmark, and the spike
test. Ingestion throughput comes from the spike test.

### Spike test

`cmd/uss` simulates USS and is described [below](#uss-simulator).

- **Ramp:** the offered rate starts at 1000 events per second and doubles at each step
  until saturation, with batches of 500 and 30 s per step `[E]`, at most 12 steps `[E]`.
  `cmd/uss -ramp` runs it and applies the saturation rule. Each step is one `steady`
  run, which draws new events into the same segments. If the p99 send lag of a step is
  above 100 ms `[E]`, the simulator itself may be the limit, so the ramp stops and the
  step is flagged.
- **Saturation point:** the first step where the first-attempt acceptance falls below
  99% of offered events, or the p99 latency of first attempts exceeds 500 ms `[E]`.
  Retries do not count here, because enough retries could hide an overload. Final
  acceptance is reported separately.
- **Reported:** highest sustained rate, `503` rate, Redis and ES resource use.
  `scripts/spike.sh` only collects `docker stats`, for the same period.
- **Burst run:** after the ramp, one `burst` run at a base of 204,800 events per second,
  10x for 5 s in a 30 s run `[E]`, which peaks at 2,048,000. The base is an owner decision.
  It is not half of the highest sustained rate, because that peak of 5.12M events per
  second ended the simulator with an out-of-memory kill on the test machine. It is reported,
  and it plays no part in saturation. The time to recover runs from the end of the peak to
  the start of the first second of the report's `timeline` from which every attempt, first
  or retry, is acknowledged.
- **Boundary burst:** segments are queried continuously across a bucket boundary,
  when all cache entries become invalid at once. This test runs in process with
  real Redis and an injected clock, so it does not wait for a real boundary. It runs
  with `docker compose run --rm bench go test -count=1 -v -run TestMeasureBoundaryBurst
  ./internal/redisstore/`, at both bucket sizes, on 500 dense segments, with the default
  cache TTL, estimate limit, and Redis timeouts.
  - After a 20 s warm-up `[E]` that fills the cache, one querier per segment sends one
    request per 100 ms `[E]` and waits for the 1 s `Retry-After` after a failure.
  - A concurrent writer calls `Add` with 1000 events per second `[E]`, in batches of 500.
  - Reported for the 15 s `[E]` after the boundary, and for the warm-up: the estimate
    `503` rate and latency percentiles, and the `Add` p99 and `503` rate. Also reported:
    how long until all segments have been served, with a limit of 60 s `[E]`.
- **After the spike:** a timed-out event may have been written, so the true number of
  users of a segment lies between its distinct acknowledged users and its distinct
  attempted users. The estimate must lie in that range, widened by 3 standard errors,
  which is about 2.43% `[E]`:
  `acknowledged * (1 - 0.0243) <= estimate <= attempted * (1 + 0.0243)`.
  `cmd/uss -check-estimates` checks all segments, after the last ramp step and after the
  burst run only.

Results describe the test machine only, and the simulator shares that machine
with ES and Redis. They are not a production capacity prediction.

### USS simulator

`cmd/uss` runs in its own container, under the compose profile `tools`, and reaches ES
through the compose network:
`docker compose run --rm uss -rate 1000 -duration 30s -profile steady`.

- **Open loop.** The schedule is computed before the run, and each batch starts at its
  scheduled time whether or not earlier ones were answered. A slow ES therefore cannot
  slow the offered rate. Connections are not limited, for the same reason.
- **Fixed seed.** The seed defaults to 1. Each batch has a generator seeded by its
  index, so its events and jitter do not depend on timing. Users and segments are drawn
  uniformly.
- **Profiles.** `steady` is a constant rate. `burst` multiplies the rate during a short
  window. The `uniform` and `spiky` profiles belong only to the
  [boundary error experiment](#traffic-model), which simulates 56 days and cannot run
  in real time.

| Flag | Default `[E]` | Purpose |
|---|---:|---|
| `-url` | `http://es:8080` | ES through the compose network |
| `-rate` | 1000 | Offered events per second |
| `-duration` | 30 s | Length of the schedule |
| `-batch` | 500 | Events per request |
| `-users` | 1,000,000 | User IDs to draw from |
| `-segments` | 500 | Segments to draw from |
| `-segment-prefix` | `uss-` | Keeps the segments of one run apart from another |
| `-seed` | 1 | Seed of the events and the jitter |
| `-profile` | `steady` | `steady` or `burst` |
| `-burst-multiplier` | 10 | `burst`: rate multiplier in the window |
| `-burst-start` | 10 s | `burst`: start of the window |
| `-burst-duration` | 5 s | `burst`: length of the window |
| `-timeout` | 2 s | Timeout of one attempt |
| `-max-retries` | 3 | Retries per batch after the first attempt |
| `-retry-jitter` | 1 s | Upper limit on the random time added to `Retry-After` |
| `-ramp` | off | Double `-rate` at each step of `-duration` until saturation |
| `-ramp-max-steps` | 12 | Stop the ramp after this many steps |
| `-check-estimates` | off | At the end, compare each segment's estimate with its distinct users |
| `-check-delay` | 6 s | Wait before the check, longer than the cache TTL of local compose |

**Retries.** A `503`, a timeout, and a network error are each counted separately and
retried, at most 3 times `[E]`, with the same body. A batch that uses them up is
abandoned. Any other response, such as a `400`, is not retried. The wait is
`Retry-After` plus a random time below `-retry-jitter`, so it is never shorter than ES
asked for. A timeout or a network error has no `Retry-After`, so it uses 1 s `[E]`,
the value ES sends.

**Report.** One JSON document on standard output, measured on the machine that ran it:

- The offered rate from the schedule, the achieved rate of first attempts, and the send
  lag, which is how late batches left. A growing lag means the simulator itself is
  saturated, and the results of that run are not valid.
- First attempts and retries separately: attempts, events, acknowledged events, `503`s,
  timeouts, network errors, other errors, acceptance, and latency percentiles.
- Final results per batch: acknowledged, abandoned and failed events, final acceptance,
  and the latency to the acknowledgement.
- Distinct acknowledged users and distinct attempted users, in total and per segment.
- A timeline of the attempts that finished in each second of the run, except in a ramp.

**Latency runs from the scheduled send time**, so a late send counts as latency and is
not hidden. A retry is scheduled at the end of its wait, so its latency excludes that
wait. The final latency runs from the schedule of the batch and includes every wait.

Tracking uses one bit per user, for acknowledged and for attempted, in total and per
segment. That is about 125 MB `[E]` at 1,000,000 users and 500 segments, and it grows
with `users * segments`.

### Thresholds

These values decide outcomes, so they must not change after results are seen. The
project owner confirmed all of them as their decision. They are assumptions,
because no real USS data exists to derive them from.

| Threshold | Value | Status |
|---|---:|---|
| Rule 1: worst 6h undercount, decision scenario | above 1% | Owner decision, assumption |
| Rule 2: uncached estimate p99 at 1h | at most 20 ms | Owner decision, assumption |
| Rule 3: Redis memory at 1h, 500 dense segments | at most 2.5 GB | Owner decision, assumption |
| Rule 4: recomputation share of Redis time at 1h | at most 20% | Owner decision, assumption |
| Saturation: first-attempt acceptance of offered events | below 99% | Owner decision, assumption |
| Saturation: ingestion p99 of first attempts | above 500 ms | Owner decision, assumption |
| After a spike: estimate between acknowledged and attempted users | margin of 2.43% | Owner decision, assumption |

## Implementation plan

Each step is one small commit, with its tests.

1. `docs: add design and implementation plan`
2. Project setup, in three commits:
   - `docs: define traffic model and threshold status`
   - `docs: add project rules for Claude Code`
   - `chore: initialize Go module and ignore files`
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
19. `test: simulate boundary error under uniform and spiky traffic`
20. `test: benchmark PFCOUNT and Redis memory at 1h and 6h`
21. `test: add spike test to find the saturation point`
22. `feat(server): set the bucket size default from measurements`, only if the
    decision rule selects 1h
23. `docs: add measured results and final decisions`
24. `docs: link design and run instructions from README`

Work happens on `feat/estimation-service` and is not pushed. After approval, it
is merged into `main` with `--ff-only`, without squashing.

## Measured results

Every value in this section is `[M]` unless it is marked otherwise. The
[test report](TEST_REPORT.md) shows the same results with charts. `cmd/report` generates it
and the `GET /report` page from `docs/results`, and uses no clock, so a test can check that
the committed report matches them:
`docker compose run --rm test go run ./cmd/report -collect-quality` updates the test, coverage,
and govulncheck results as well.

**Machine:** Mac17,3, Apple M5, 10 CPUs, 16 GB, macOS 26.6.2. Docker 29.7.2 on linux/arm64,
with 6 CPUs and 3.825 GiB, kernel 7.0.12-linuxkit. Redis 8.2.11 with jemalloc-5.3.0, and
Go 1.27.3. The simulator, ES, and Redis all ran in that one VM.

**Code:** the ramp and the burst ran at 1h with the current dependencies, at commit
`cfabc4f`. A separate, idle ES and Redis also ran in the VM during them. Everything else ran
before the dependency update and was not repeated.

### Bucket size decision

| Rule | Limit | Measured | Holds |
|---|---:|---:|---|
| 1. Worst 6h undercount, decision scenario | above 1% | 1.27% | Yes |
| 2. Uncached estimate p99 at 1h | at most 20 ms | 6.8 ms | Yes |
| 3. Redis memory at 1h, 500 dense segments | at most 2.5 GB | 2.43 GB | Yes |
| 4. Recomputation share of Redis time at 1h | at most 20% | 6.3% `[calculated]` | Yes |

All four hold, so the final default bucket size is **1h**.

### HyperLogLog accuracy

On real Redis through the Store, with 5 independent sets of users at each size.

| Users | Error in each set | Largest absolute error |
|---:|---|---:|
| 1,000 | -0.40, -0.20, -0.10, -0.50, +0.60% | 0.60% |
| 100,000 | +1.01, +0.70, +0.33, -0.58, -0.28% | 1.01% |
| 1,000,000 | +0.57, -2.11, -0.57, +0.61, -0.85% | 2.11% |

### Boundary undercount

In process with exact sets, with 1,000,000 users, seed 1, and 1345 samples. The traffic
model is an assumption `[E]`, so these values are measured only for that model.

| Profile | `r` | Realized return share, within 14 days | 6h mean / max | 1h mean / max |
|---|---:|---:|---:|---:|
| uniform | 0% | 0% | 0.93% / 1.84% | 0.19% / 0.33% |
| uniform | 50% | 47.21% | 0.63% / 1.27% | 0.13% / 0.22% |
| spiky | 0% | 0% | 1.69% / 63.86% | 0.36% / 22.48% |
| spiky | 50% | 48.05% | 0.96% / 27.90% | 0.19% / 6.02% |

### Benchmark at 6h and 1h

500 fully dense segments on real Redis, with 2000 uncached calls at each size.

| | 6h | 1h |
|---|---:|---:|
| Keys | 28,000 | 168,000 |
| `used_memory` | 405.2 MB | 2.43 GB (2.26 GiB) |
| `MEMORY USAGE` of one key | 14,392 bytes | 14,392 bytes |
| Client latency p50 / p99 | 0.9 / 1.4 ms | 3.6 / 6.8 ms |
| Time inside Redis, `usec_per_call` | 866 | 3758 |
| Recomputation share of Redis time `[calculated]` | 1.44% | 6.26% |
| The same share from client mean latency `[calculated]` | 1.53% | 6.44% |

### Saturation

Ramp with batches of 500, 30 s per step, 1,000,000 users, and 500 segments.

| Offered events/s | First-attempt acceptance | First-attempt p99 | Mean CPU, ES / Redis / simulator |
|---:|---:|---:|---:|
| 1,000 to 256,000, 9 steps | 100% | at most 31.7 ms | at most 36% / 21% / 11% |
| 512,000 | 100% | 29.4 ms | 58% / 37% / 20% |
| 1,024,000 | 100% | 11.4 ms | 107% / 69% / 35% |
| 2,048,000 | 31.35% | 120.8 ms | 188% / 94% / 83% |

- **Saturation point:** 2,048,000 events per second, by first-attempt acceptance. The
  highest sustained rate is 1,024,000.
- **In the saturated step:** `503` on 84,352 of 122,880 first attempts (68.65%), with no
  timeouts at the simulator. Final acceptance was 74.76%, with 15,509,500 events abandoned.
  The send lag p99 was 3.95 ms, so the simulator was not the limit.
- **Peak memory in that step:** ES 287 MiB, Redis 37 MiB, simulator 1150 MiB. ES logged 228
  store failures, all Redis pool timeouts, and it did not restart.
- **After the ramp:** all 500 segments were in range, with the estimate from -2.04% to
  +2.40% of acknowledged users.

### Burst

A base of 204,800 events per second, 10x from 10 s to 15 s, in a 30 s run, right after the ramp
on the same Redis. The peak is 2,048,000. There were 15,360,000 offered events.

| Result | Value |
|---|---:|
| First-attempt acceptance, whole run | 65.86% |
| First-attempt acceptance, in the five seconds from 10 s | 49.83% `[calculated]` |
| Final acceptance | 98.47% |
| Abandoned events | 235,000 (1.53%) |
| `503` rate, first attempts | 34.14% |
| `503` rate, retries | 42.79% |
| Timeouts and network errors at the simulator | 0 |
| Final latency p99, including retry waits | 4949 ms |
| **Time to recover after the peak** | **2 s**, at a resolution of 1 s |
| Last retry finished | in the second from 18 s, which is 3 to 4 s after the peak |
| Estimate check after the burst | 500 of 500 segments in range, -1.69% to +1.85% of acknowledged users |
| Peak memory: ES / Redis / simulator | 239 / 44 / 772 MiB |
| Peak CPU: ES / Redis / simulator | 191% / 110% / 80% |

ES logged 6 store failures, all Redis pool timeouts. No container restarted. The run's send
lag p99 was 2.3 ms.

### Boundary burst

In process with real Redis, on 500 dense segments, with a writer at 1000 events per second.

| For the 15 s after the boundary | 6h | 1h |
|---|---:|---:|
| Estimate `503` rate | 8.92% | 8.82% |
| Estimate `503` rate during the warm-up | 1.09% | 4.63% |
| Estimate latency p50 / p99 | 0.001 / 51.8 ms | 0.001 / 144.7 ms |
| `Add` `503` | 0 of 30 | 0 of 30 |
| `Add` p99 | 4.98 ms | 4.23 ms |
| Time until all segments were served | 15.15 s | 15.23 s |

### Dependencies

`govulncheck` v1.9.0 after the update reports 0 vulnerabilities that the code reaches and 0 in
imported packages. One remains at module level: GO-2026-5932, for the unmaintained
`x/crypto/openpgp` package, which the code does not call. Its fix has no tagged version yet.

### Limits of these results

- **Memory limit.** A fully dense segment at 1h uses 4.85 MB. Redis reads `3gb` as
  3,221,225,472 bytes `[M]`, so it holds about 664 fully dense segments at 1h
  `[calculated]`. Beyond that, writes return
  `503`, and `noeviction` keeps the data. The challenge asks for hundreds of segments, and
  sparse keys use less, but nothing limits the number of segment names.
- **Throughput is not production capacity.** It comes from one laptop, with the simulator in
  the same VM as ES and Redis. It also uses repeated users: distinct users reached the
  1,000,000 limit by the 256,000 step, so later steps mostly repeated users, and `PFADD` on
  new users could be slower `[E]`.
- **The 15 s boundary result is likely shaped by the test.** The queriers wait a fixed 1 s
  after a `503`, and 32 flights fit each wave, so 500 segments need about 15.6 s
  `[calculated]`. The 6h and 1h results are nearly equal although `PFCOUNT` differs by about
  4x, which fits this explanation. It was not tested on its own.
- **`Add` p99 in the boundary burst has only 30 samples**, so it equals the maximum.
- **Accuracy has 5 sets at each size.** One error at 1,000,000 users was 2.11%, close to the
  test's limit of 2.43%.
- **The recomputation share is calculated**, from the measured time per call. All 500 segments
  were not queried continuously in a separate run.
- **Undercount depends on an assumed traffic model.** Real USS data should replace it.
