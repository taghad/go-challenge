# Decisions

The final decisions and what each one costs. The details are in [DESIGN.md](DESIGN.md), and
the numbers come from its [measured results](DESIGN.md#measured-results). `[M]` is measured on
one laptop, `[calculated]` is computed from measured values, and `[E]` is an estimate.

## HyperLogLog instead of an exact count, and no Postgres

- **Decision:** count users with Redis HyperLogLog. Keep no list of users.
- **Why:** the challenge asks for an estimate, and HLL memory does not depend on the number of
  users. One key uses 14,392 bytes `[M]`. Redis provides both HLL and key expiry, so ES
  implements neither.
- **Rejected:** Redis sorted sets, at about 5 GB of RAM for 50M memberships and 10 times that at
  10 times the memberships `[E]`. Postgres, at about 5-8 GB of disk with costly writes and
  counts, plus partitions or a delete job for expiry `[E]`.
- **Cost:** the count is approximate. The largest error measured was 2.11% `[M]`. ES cannot list
  a segment's users or remove one user from a count.

## Time buckets, at 1h

- **Decision:** one HLL per segment per hour, aligned to the Unix epoch in UTC. An estimate is a
  `PFCOUNT` over the last 336 buckets.
- **Why:** an HLL cannot delete a user, so expiry has to drop a whole key. The size came from a
  rule fixed before measuring, and all four parts held: 1.27%, 6.8 ms, 2.43 GB, and 6.3% `[M]`.
- **Rejected:** 6h buckets, because the undercount in the decision scenario was 1.27%, above the
  limit of 1%, against 0.22% at 1h `[M]`. Rollups with `PFMERGE`, because they add memory and a
  background job, and nothing measured needs them.
- **Cost:** six times the memory of 6h (2.43 GB against 405.2 MB) and about five times the p99
  (6.8 ms against 1.4 ms) `[M]`. `3gb` holds about 664 fully dense segments `[calculated]`.
  Changing the bucket size later undercounts for 14 days, because the old keys are no longer read.

## An undercount over counting expired users

- **Decision:** the read window covers between 14 days minus one bucket and 14 days. A user can
  drop out up to 1h early, and is never counted after 14 days.
- **Why:** the challenge says a user must not be counted after two weeks. That is a firm rule,
  and an hour of early removal does not break it.
- **Rejected:** reading one more bucket, which would never drop a user early but would count users
  for up to 1h after they expired.
- **Cost:** a maximum undercount of 0.33% under uniform traffic and of 22.48% in the worst spiky
  case, right when a campaign of single-visit users leaves the window `[M]`. The traffic model is
  an assumption `[E]`.

## REST with batches

- **Decision:** `POST /v1/events` with JSON batches of up to 1000 events `[E]`.
- **Why:** Gin and the frontend are both HTTP, so one API serves both. A batch writes all of its
  users for a segment in one `PFADD`, and that offsets the JSON overhead. The ramp sustained
  1,024,000 events per second on one laptop with repeated users `[M]`.
- **Rejected:** gRPC, which would reduce the payload but would need a gateway for the frontend.
  Single-event requests, which would cost one request per event.
- **Cost:** larger payloads than a binary protocol. A batch with one invalid event gets `400` as a
  whole, so USS has to fix or drop the whole batch.

## `503` and no buffer, instead of Kafka

- **Decision:** ES acknowledges only after Redis accepts a write. When the limiter is full it
  returns `503` with `Retry-After` at once, and USS retries.
- **Why:** an acknowledged event is not lost when ES crashes, and ES memory stays bounded. In the
  10x burst, ES recovered 2 s after the peak, with 98.47% final acceptance and no restart `[M]`.
  `PFADD` is idempotent, so retries do not double count.
- **Rejected:** Kafka or another durable queue, which is more to run than any measured load
  needs. An in-memory buffer in ES, which would lose acknowledged events in a crash.
- **Cost:** it depends on USS retrying, which is an assumption. In the burst, 1.53% of events used
  up their 3 retries and were abandoned `[M]`. If USS cannot retry, rejected events are lost and a
  durable queue is needed.

## Boundary-limited cache and an estimate limit

- **Decision:** each ES instance caches estimates. An entry is never served after its bucket ends,
  `singleflight` allows one computation per segment and bucket, and `MAX_INFLIGHT_ESTIMATE` limits
  computations to 32 `[E]`, with `503` when it is full.
- **Why:** `PFCOUNT` over 336 keys runs on the one Redis thread, so reads could delay writes. A cache
  entry that crosses a boundary would serve expired users. During the boundary burst, `Add` had no
  `503` and a p99 of 4.23 ms, from only 30 samples `[M]`.
- **Rejected:** a plain TTL cache, which would break the expiry guarantee. No limit, which would let
  a boundary send every segment to Redis at once. A cache shared in Redis, which adds a dependency
  to the path it is meant to protect.
- **Cost:** a number can be up to one TTL old. For 15 s after a boundary, 8.82% of estimates got `503`,
  and all segments took 15.23 s to be served `[M]`, a result the fixed 1 s retry likely shapes.

## AOF without a replica

- **Decision:** one Redis with AOF and `appendfsync everysec`.
- **Why:** after a crash Redis recovers from disk, and about 1 second of acknowledged writes can be
  lost `[E]`, which is small against a 14-day window. This loss was not measured.
- **Rejected:** a replica or Sentinel, which adds parts to run, and which replicates asynchronously,
  so it can lose writes too. `appendfsync always`, which would slow every write.
- **Cost:** Redis is a single point of failure. While it is down, writes return `503` and estimates
  work only from the cache. If the disk is lost, up to 14 days of data is gone.

## No history kept

- **Decision:** ES answers only "how many users right now". A bucket is removed when it leaves the
  window, and no past estimates are stored.
- **Why:** the challenge asks only for the current count, and keeping nothing older than 14 days also
  bounds the memory.
- **Rejected:** storing estimates over time, or keeping buckets for longer, since nothing needs them.
- **Cost:** no trends, and no answer to "how many users were there last month".
- **If it is needed:** a small job that saves each segment's estimate on a schedule gives a trend for
  little memory, in Postgres for example. To ask new questions of old data, keep the buckets longer
  and move the read window, which multiplies the memory with the retention.

## What was deliberately not built

- **Decision:** none of these are built:
  - Redis Cluster, and the fixed split of a hot segment by user.
  - `PFMERGE` rollups, and migration between bucket sizes.
  - A durable queue.
  - Authentication, and an allowlist of segment names.
  - Prometheus, Nginx, and repository or DTO layers.
- **Why:** no requirement or measurement needs them on this scale, and each is more to run or to read.
  ES is stateless, and the keys use a hash tag, so scaling out and clustering stay possible.
- **Rejected:** building them ahead of a need.
- **Cost:** ES has to run on an internal network. Nothing limits the number of segment names, so many
  names can use up the memory limit. One Redis is the limit of the current deployment, and a hot segment
  would stay on one shard even in a cluster.
