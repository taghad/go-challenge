# Estimation Service

Estimation Service (ES) receives `(user_id, segment)` pairs from the User Segmentation Service
(USS) and estimates how many distinct users each segment had in the last 14 days. The count is
approximate, because it uses Redis HyperLogLog (HLL).

## Quick start

Only Docker is needed.

Run ES and Redis:

```bash
docker compose up --build -d
```

Open these URLs:

- Frontend: http://127.0.0.1:8080/
- Test report: http://127.0.0.1:8080/report

Run the tests:

```bash
docker compose run --rm test
```

Send simulated USS traffic, which prints a JSON report:

```bash
docker compose run --rm uss -rate 1000 -duration 30s
```

## API

| Endpoint | Purpose | Status codes |
|---|---|---|
| `POST /v1/events` | Ingest a batch of up to 1000 events `[assumption]` | `204`, `400`, `413`, `503` |
| `GET /v1/segments/:segment/estimate` | Return the estimate for a segment | `200`, `400`, `503` |
| `GET /healthz` | Process is alive | `200` |
| `GET /readyz` | Redis is reachable | `200`, `503` |
| `GET /` | Frontend | `200` |
| `GET /report` | Test report | `200` |

A `503` has a `Retry-After` header. An unknown segment returns `200` with `0`.

```bash
curl -i -X POST http://127.0.0.1:8080/v1/events -H 'Content-Type: application/json' -d '{"events":[{"user_id":"u104010","segment":"sports"}]}'
```

```bash
curl http://127.0.0.1:8080/v1/segments/sports/estimate
```

```bash
curl -i http://127.0.0.1:8080/healthz
```

```bash
curl -i http://127.0.0.1:8080/readyz
```

```bash
curl -o /dev/null -s -w '%{http_code}\n' http://127.0.0.1:8080/
```

```bash
curl -o /dev/null -s -w '%{http_code}\n' http://127.0.0.1:8080/report
```

## How it works

```text
USS ─────POST /v1/events────▶ ES (Gin, stateless) ──PFADD, EXPIREAT──▶ Redis
Browser ──GET /, estimate───▶ ES ───────────────────────PFCOUNT──────▶ Redis
```

- **Buckets:** each segment has one HLL per hour, aligned to the Unix epoch in UTC, for example
  `es:{sports}:3600:1794096000`.
- **Write:** `PFADD` into the current bucket and `EXPIREAT` run in one `TxPipeline`, so a key never
  exists without an expiry. `204` is returned only after Redis accepts the write.
- **Read:** `PFCOUNT` over the last 336 buckets counts each user once. Each ES instance caches the
  result, and a cached value is never served after its bucket ends.
- **Expiry:** the read window decides who is counted, so an event older than 14 days is never
  counted. `EXPIREAT` is the bucket start plus 14 days, and it only reclaims memory.

## Key decisions

The reasons, rejected options, and costs are in [docs/DECISIONS.md](docs/DECISIONS.md).

- [HLL, not an exact count or Postgres](docs/DECISIONS.md#hyperloglog-instead-of-an-exact-count-and-no-postgres):
  the challenge asks for an estimate, and HLL memory does not depend on the number of users.
- [Hourly buckets](docs/DECISIONS.md#time-buckets-at-1h): an HLL cannot delete a user, so expiry drops
  whole keys, and a rule fixed before measuring chose 1h over 6h.
- [Undercount over counting expired users](docs/DECISIONS.md#an-undercount-over-counting-expired-users):
  the challenge says a user must not be counted after two weeks.
- [REST with batches](docs/DECISIONS.md#rest-with-batches): one API serves USS and the frontend, and
  batching offsets the JSON overhead.
- [`503` and no buffer, not Kafka](docs/DECISIONS.md#503-and-no-buffer-instead-of-kafka): an acknowledged
  event survives an ES crash, and ES memory stays bounded.
- [Boundary-limited cache and estimate limit](docs/DECISIONS.md#boundary-limited-cache-and-an-estimate-limit):
  reads must not delay writes on the one Redis thread, and a cache must not serve expired users.
- [AOF without a replica](docs/DECISIONS.md#aof-without-a-replica): Redis recovers from disk after a crash,
  with fewer parts to run.
- [No history kept](docs/DECISIONS.md#no-history-kept): the challenge asks only for the current count.
- [Not built on purpose](docs/DECISIONS.md#what-was-deliberately-not-built): no requirement or measurement
  needs Redis Cluster, rollups, a queue, or authentication.

## Results

One laptop, repeated users, not production capacity. The full results are in
[docs/TEST_REPORT.md](docs/TEST_REPORT.md).

| Result | Value | Label |
|---|---|---|
| Bucket size | 1h, chosen by the rule fixed before measuring | measured |
| Highest sustained rate | 1,024,000 events/s | measured |
| Saturation | 2,048,000 events/s, with 31.35% first-attempt acceptance | measured |
| Burst recovery | 2 s after a 10x peak of 2,048,000 events/s, at a resolution of 1 s | calculated |
| Largest HLL error | 2.11%, at 1,000,000 users | measured |
| Memory | 2.43 GB, for 500 fully dense segments at 1h | measured |

## Known limits

- **Early removal:** a user can drop out up to 1h before 14 days. The largest undercount under uniform
  traffic is 0.33% `[measured]`, on an assumed traffic model `[assumption]`.
- **Spiky traffic:** the undercount reaches 22.48% at 1h `[measured]` when a campaign brings 30% of all
  users in one 6h slice and they never return `[assumption]`.
- **Scale tested:** up to 1,000,000 distinct users `[measured]`. Nothing above that was measured, while the
  challenge asks for millions.
- **Redis:** one instance without a replica is a single point of failure.
- **USS retry:** ES assumes that USS retries after a `503`. Otherwise rejected events are lost. In the burst,
  235,000 events `[measured]`, or 1.53% `[calculated]`, used up their retries.
- **No history:** only the current count is available, so there are no trends.
- **Memory limit:** `3gb` holds about 664 fully dense segments at 1h `[calculated]`. Beyond that, writes
  return `503`, and nothing limits the number of segment names.

## Project layout

```text
cmd/server, cmd/uss, cmd/report   ES, the USS simulator, and the report generator
internal/estimator                buckets, validation, cache, and the estimate service
internal/redisstore               Redis implementation of the store
internal/httpapi, internal/web    router and handlers, and the embedded pages
docs/                             design, decisions, test report, and raw results
```

## Links

- [Design](docs/DESIGN.md)
- [Decisions](docs/DECISIONS.md)
- [Test report](docs/TEST_REPORT.md)
- [AI usage](ai.md)

## Original challenge

# How many users? (Go Challenge)

Suppose there is a "User Segmentation Service" (USS) that segments users based on their activities.
For example, if a user visits sports news, USS classifies and tags "sport" to him.
So we have a pair: (the user_id, and the segment) for example, (u104010, "sports").

We want to develop an Estimation Service (ES) that interacts with USS directly.
ES receives the pair from USS as input and stores it.
The responsibility of ES is to answer a simple query: "How many users exist on a specific segment?".
For example, "how many users are in the sports segment?".

![](https://raw.githubusercontent.com/ArmanCreativeSolutions/go-challenge/main/Untitled%20Diagram.drawio.png?raw=true)

The query is simple, but two assumptions may make it a little challenging:
- A specific user remains just two weeks on a segment. After that,
we should not count "u104010" on the sports segment.
- There are millions of users and hundreds of segments. So your solution(s) must be scalable


## Requirements

- Implement a (REST API, RESTful API, soap, Graphql, RPC, gRPC, or whatever protocol you prefer)
interface to receive data (user_id, segment pair) from USS. 
- Implement a method to estimate the number of users in a specific segment. ( `func estimate(segment) -> number of users`)

## Implementation details

Try to write your code as reusable and readable as possible.
Also, don't forget to document your code and clear the reasons for all your decisions in the code.

If your solution is not simple enough for implementing fast, you can just describe it in your documents.

Use any tools that you prefer just explain the reason of choices in your documents.
For example explain why you choose REST API for receiving data.

It is more valuable to us that the project comes with unit tests.

Please fork this repository and add your code to that.
Don't forget that your commits are so important.
So be sure that you're committing your code often with a proper commit message.


## We all use AI. Don't be ashamed

Everyone uses AI for everything. And we expect you to do the same. AI is a big part of our development process, and we care a lot about how you use AI tools during this task.

To earn our immunity after the AI takeover of Earth, we want to make sure you're using it properly.

Add a file named "ai.md" to your project. This file should contain the following information (the more explicit, the better):

1. What AI tools and models did you use?
2. Explain the different stages of software development where you used AI help. Describe how you used your tools at each stage.
3. We want to know how you prompt. So add your prompts to this file too. For each prompt, explain which stage you used it in, how you evaluated the result, and what you did to fix any misbehavior.
4 Explain how you monitor your token usage and what you do to manage it. Link your tools or add your helper prompts.

*There's a sample.ai.md file in the project representing the expected template. Don't forget: "ai.md" is the only file we expect you to write entirely by yourself.*
