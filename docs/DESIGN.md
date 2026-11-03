# Estimation Service: Design

The Estimation Service (ES) receives `(user_id, segment)` pairs from the User
Segmentation Service (USS) and answers one question: how many users are in a
segment right now?

**Status:** design approved, implementation in progress. Every capacity and
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
- **The share of time lost is not the share of users lost.** `S / 14d` is 1.8%
  for 6h, but the undercount depends on how many users have their only event in
  the dropped slice. If a campaign brought 30% of a segment's users within one
  6h slice and they never returned, the estimate can be about 30% low for up to
  6h before they would have expired `[E]`.

The real undercount is therefore measured, not derived from time. See the
[test plan](#test-plan).

### Bucket size

`BUCKET_SIZE` is configurable and defaults to `6h`. It must be a whole number of
seconds and divide 14 days exactly, or ES refuses to start. The final default is
chosen from measurements, using the rule below.

| Bucket size | Buckets | Max memory, 500 segments `[E]` | Uncached estimate `[E]` | Time not covered, worst case |
|---|---:|---:|---:|---:|
| 6h | 56 | 344 MB | About 2 ms | 1.8% of the window |
| 1h | 336 | 2.06 GB | About 11 ms | 0.3% of the window |

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
| `BUCKET_SIZE` | `6h` | Bucket size; a whole number of seconds that divides 14 days |
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
| Too many segment names | Each segment can use up to 688 KB at 6h `[E]`. The regex limits format, not count, so this risk remains. An allowlist could be added later. |
| Shutdown | On `SIGTERM` or `SIGINT`, in-flight requests finish and then the process exits. Requests still running after `SHUTDOWN_TIMEOUT` are cut off and the exit code is 1. |
| Invalid configuration | ES exits at startup with an error that names every invalid variable. |

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
`K` sub-keys by `hash(user_id) mod K`, for example `es:{sports:3}:21600:1794096000`.
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
│   └── web/                 # embedded index.html
├── docs/DESIGN.md
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
agree, rule 1 will hold, and the choice will depend on rules 2 to 4.

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
  until saturation, with batches of 500 and 30 s per step `[E]`. Each step is one
  `steady` run of `cmd/uss`.
- **Saturation point:** the first step where the first-attempt acceptance falls below
  99% of offered events, or the p99 latency of first attempts exceeds 500 ms `[E]`.
  Retries do not count here, because enough retries could hide an overload. Final
  acceptance is reported separately.
- **Reported:** highest sustained rate, `503` rate, Redis and ES resource use.
- **Boundary burst:** segments are queried continuously across a bucket boundary,
  when all cache entries become invalid at once. This test runs in process with
  real Redis and an injected clock, so it does not wait for a real boundary. The
  estimate `503` rate and latency percentiles are reported.
- **After the spike:** a timed-out event may have been written, so the true number of
  users of a segment lies between its distinct acknowledged users and its distinct
  attempted users. The estimate must lie in that range, widened by 3 standard errors,
  which is about 2.43% `[E]`:
  `acknowledged * (1 - 0.0243) <= estimate <= attempted * (1 + 0.0243)`.

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

Not measured yet. This section is filled in by step 23, along with the machine
used, and the matching `[E]` values above are replaced.

| Result | Value |
|---|---|
| Test machine | Not measured |
| HLL error at 1k, 100k, 1M | Not measured |
| Boundary undercount, uniform, 1h and 6h, each return share | Not measured |
| Boundary undercount, spiky, 1h and 6h, each return share | Not measured |
| Realized return share | Not measured |
| Uncached estimate p50 and p99, 1h and 6h | Not measured |
| Redis memory, 500 dense segments, 1h and 6h | Not measured |
| Recomputation share of Redis time, 1h and 6h (calculated) | Not measured |
| Saturation point | Not measured |
| Final default bucket size | Not decided |
