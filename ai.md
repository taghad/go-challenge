# AI Usage Disclosure

**How this file was written.** The challenge asks that this file be written by the candidate. I
wrote my notes in Persian. GPT translated parts of them into English. The Claude consultation
session extracted the prompts and usage from the session history, listed facts for me, and applied
the final edit to this file at my request. Treat this file as AI-assisted.

## 1. AI Tools and Models

  -----------------------------------------------------------------------
  Tool / Product    Model             Purpose           Frequency
  ----------------- ----------------- ----------------- -----------------
  Claude Code --    Claude Opus 5.5   Reviewing         Throughout
  Consultation                        proposals,        development
  Sessions                            inspecting the    
                                      repository,       
                                      technical         
                                      decisions, and    
                                      reviewing         
                                      iterations        

  Claude Code --    Claude Opus 5.5   Design, coding,   Most of the work
  Project Sessions                    testing, Docker,  
                                      measurements, and 
                                      documentation     

  ChatGPT           GPT-6             Initial planning, Several times,
                                      HLL evaluation,   mostly at the
                                      and suggesting    beginning and
                                      prompts           occasionally for
                                                        reviews

  ChatGPT           GPT-6             Translating my    At the end
                                      Persian notes for
                                      this file

  Claude Code --    Claude Opus 5.5   Extracting prompts At the end
  Consultation                        and usage, and the
  Sessions                            final edit of this
                                      file
  -----------------------------------------------------------------------

## 2. Stages of Development

### Planning / Requirements Analysis

-   ChatGPT proposed a PostgreSQL + Redis ZSET + Outbox + Worker
    architecture.
-   The Claude consultation session pointed out that the challenge
    requires an `estimate` operation and suggested using HLL with
    time-based buckets.
-   Each model's output was shared with the other model for review. GPT
    found two real problems in Claude's HLL proposal: the "0.3% boundary
    error" claim held only under uniform traffic, and a hash tag does not
    spread a hot segment across shards.
-   Independently, the coding session suggested HLL without having
    access to the consultation session's discussion, proposing six-hour
    buckets. Both sessions used the same model, so this agreement is not
    independent proof.
-   **Kept:** HLL, REST with batches, Gin, and `503` with no buffer.
    **Rejected:** PostgreSQL, Outbox, Kafka, and a live dashboard.
-   I looked at another candidate's public fork, which used PostgreSQL,
    Redis sorted sets, and an outbox. The consultation session reviewed it,
    and no code was taken from it.
-   The bucket size was selected according to a rule established before
    benchmarking. The project session proposed the thresholds and I
    confirmed them, so `DESIGN.md` records them as owner decisions and
    assumptions. The resulting bucket size was one hour.
-   Technical decisions were reviewed in the consultation session before
    implementation prompts were sent to the coding session.

### Scaffolding / Boilerplate

Decisions made before coding:

-   **Language and framework:** Go and Gin.
-   **Running:** everything runs with Docker and Compose. Go was not
    installed on the host, so tests also run in containers.
-   **API:** REST and JSON, with `POST /v1/events` for batches of up to 1000
    events and `GET /v1/segments/:segment/estimate`. REST was chosen because
    Gin and the frontend are both HTTP, and gRPC would need a gateway for the
    frontend.
-   **Storage:** Redis with HLL only.
-   **Frontend:** one page embedded with `go:embed`, with no separate service.
-   **Assumptions:** time is the time of receipt at ES, a repeat event renews
    the two weeks, USS retries after `503`, and authentication is out of scope.

Claude Code generated `docs/DESIGN.md`, `CLAUDE.md`, `go.mod`, the ignore
files, and `docker-compose.yml`. `docs/DESIGN.md` was the first commit, so the
design would not be lost when using `/clear`. `CLAUDE.md` holds the project
rules, including that technical decisions belong to the owner.

Changed after review:

-   `maxmemory` was added, because `noeviction` has no effect without it.
-   A separate `redis-test` service was added, because tests shared the main
    Redis.
-   Tests moved to `go test -p 1 -race ./...`.

### Implementation

-   The code was implemented by Claude Code across 24 planned stages and
    45 commits.
-   Prompts generally specified the decisions and the reasoning behind
    them, while the model handled implementation. However, the
    implementation model also made some decisions and reported them
    afterward.
-   The consultation session calculated that `3gb` holds about 620 fully
    dense segments at one hour, using 3,000,000,000 bytes. The project
    session noted that Redis reads `3gb` as 3,221,225,472 bytes, so the
    corrected figure is about 664 `[calculated]`.

### Debugging

-   **Cache:** The initial design could cross bucket boundaries and
    count expired users. This was corrected by making the key and entry
    validity dependent on the bucket.
-   **`noeviction`:** it had no effect without `maxmemory`, so a limit was
    added.
-   **go-redis:** `MaxRetries: 0` means the default of 3 retries, so `-1` is
    used. `ContextTimeoutEnabled` is needed for context deadlines. Both were
    verified in the library source.
-   **Burst:** a peak of 5.12M events/s killed the simulator with an
    out-of-memory error. The run was repeated at a base of 204,800 events/s.
-   **Vulnerabilities:** `govulncheck` found 5 reachable issues, and dependency
    updates brought that to 0. One module-level issue remains, which the code
    does not call and which has no tagged fix.
-   **My own process error:** comparing reports against the repository showed
    that my correction prompt had not been sent to the project session, and it
    took several rounds before I sent it.

### Testing

-   Tests were written by Claude Code. The latest test run completed
    successfully with 419 tests passing under `-race`.
-   Mutation testing was used for critical logic. In Step 7, it revealed
    that the test for the 10,000-item limit was weak, and the test was
    subsequently improved.
-   The consultation session ran the tests twice without being explicitly
    asked to do so, and they passed. After that, the results reported by the
    coding session were trusted, and only code and reports were compared.

Rules fixed before measuring:

-   **Bucket rule:** keep 6h unless all four hold: the worst 6h undercount is
    above 1%, uncached estimate p99 at 1h is at most 20 ms, Redis memory at 1h
    is at most 2.5 GB, and recomputation at 1h uses at most 20% of Redis time.
-   **Saturation:** first-attempt acceptance below 99%, or first-attempt p99
    above 500 ms. p99 is used because an average hides the slowest requests,
    and first attempts are used because retries would hide saturation.
-   **After a spike:** the estimate must be between acknowledged and attempted
    users, with a 2.43% margin, because a timed-out event may have been written.
-   **Simulator:** open-loop with a fixed seed, and latency is measured from the
    scheduled send time.

Results, on the machine described in `docs/TEST_REPORT.md`:

-   **HLL accuracy:** 5 independent sets at 1k, 100k, and 1M users; the largest
    error was 2.11%.
-   **Boundary undercount:** 8 simulated cases; the decision scenario gave 1.27%
    at 6h and 0.22% at 1h, and the worst spiky case at 1h was 22.48%.
-   **Benchmark at 1h:** p99 of 6.8 ms, memory of 2.43 GB, and a calculated 6.26%
    of Redis time. All four rules held, so the default became 1h.
-   **Ramp:** sustained 1,024,000 events/s, and saturated at 2,048,000 events/s
    with 31.35% first-attempt acceptance and a p99 of 120.8 ms.
-   **Burst:** a 10x peak of 2,048,000 events/s gave 98.47% final acceptance and
    recovery in 2 s at a resolution of 1 s, with no restart.
-   **Boundary burst at 1h:** 8.82% of estimates got `503`, and write p99 was
    4.2 ms.

### Testing Limitations

-   All testing was performed on a single laptop and one VM; these
    results do not represent production capacity.
-   Users were repeated, and nothing above 1,000,000 distinct users was
    measured, while the challenge asks for millions.
-   The p99 write latency in the boundary burst test was based on only 30
    samples, and the test's fixed 1 s retry likely shapes its 15 s result.
-   Some measurements were taken before dependency upgrades and were not
    repeated afterward.

### Documentation

The following documentation and reporting components were written by
Claude Code:

-   `DESIGN.md`
-   `DECISIONS.md`
-   `TEST_REPORT.md`
-   The `/report` page
-   `README.md`

### Code Review / Refactoring

-   A version check against the Go proxy and Docker Hub showed Redis 8.2.3
    behind the 8.2 line, so it was upgraded to 8.2.11.
-   The consultation session read key code and compared project-session
    reports against the repository.
-   The project session reported two of its own mistakes: stopping my running
    stack, and merging before its final check had run. That check then passed
    from a fresh clean clone.
-   The project session's final review changed only explanations and documented
    limitations, and it reported no code bugs.

## 3. Prompts

The following table summarizes the 19 prompts sent in the Claude Code
project session. Usage figures were extracted from the session's JSONL
history files. "Input" includes input tokens and cache writes.

  ----------------------------------------------------------------------------------------------
              \# Prompt          API calls / Result           Evaluation        Fix
                                 Input /                      (evidence)        
                                 Cache read                                     
                                 / Output                                       
  -------------- --------------- ----------- ---------------- ----------------- ----------------
               1 Architecture    5 / 41k /   HLL design with  Cache             Prompt 2
                 proposal        340k / 21k  6h buckets       inconsistency and 
                                                              time-percentage   
                                                              error found       

               2 Approve design  7 / 26k /   `DESIGN.md`      Incorrect author  Prompt 3
                 and Step 1      713k / 19k                   identified        

               3 Step 2 and      9 / 26k /   Traffic model,   36 runs were      Prompt 5
                 author          1.15M / 14k `CLAUDE.md`, and excessive         
                 correction                  `go.mod`                           

               4 `/cont`         0 / 0 / 0 / Failed; typo and ---               Run `/context`
                                 0           incomplete                         again
                                             command                            

               5 Confirm         5 / 17k /   8 measurement    No issue found    Not needed
                 thresholds      739k / 7k   cases                              

               6 Step 3          10 / 48k /  Compose with     Missing           Prompt 11
                                 868k / 8k   Redis and tests  `maxmemory`,      
                                                              shared Redis, and 
                                                              `-race`           

               7 Complete        9 / 23k /   Bucket logic     `-race` run; some Prompt 11
                 document and    1.03M / 19k                  fixes had not     
                 Step 4                                       been applied      

               8 Step 5          6 / 22k /   Validation       Decisions made    Approval and 413
                                 831k / 11k                   without asking    

               9 Step 6          6 / 19k /   Store and        Checked against   Prompt 11
                                 941k / 16k  Service          repository        

              10 Step 7          11 / 84k /  Cache and        `-race` and       Limit test fixed
                                 1.18M / 43k estimate limit   mutation check    in the same
                                                                                session

              11 Fixes and Steps 11 / 53k /  Redis store      go-redis options  Prompt 12
                 8--9            1.79M / 40k                  verified in       
                                                              source            

              12 Steps 10--13    8 / 48k /   API and limiter  Six choices made  Approved with a
                                 1.70M / 38k                  without asking    reminder

              13 Steps 14--17    3 / 41k /   Stopped with 4   Timeout issue     Prompt 14
                                 202k / 14k  questions        identified        

              14 Answers to      26 / 112k / Server,          Clean checkout    Prompt 15
                 questions       4.12M / 74k frontend,        and XSS test      
                                             Docker, and USS                    

              15 UI and method   12 / 48k /  UI and stopped   Missing           Prompt 16
                 decisions       2.86M / 32k for a decision   concurrent writes 
                                                              identified        

              16 Redis and Steps 18 / 110k / Measurements     5 vulnerabilities Prompt 17
                 18--21          5.66M / 73k                  and OOM during    
                                                              burst             

              17 Dependencies,   25 / 154k / Results and      Test report and   Prompt 18
                 1h, and         3.73M / 46k `DECISIONS.md`   620 figure had    
                 documentation                                issues            

              18 Final prompt    27 / 119k / 1h ramp, report, Two of its own    Repeat
                                 7.46M / 80k and merge        mistakes were     clean-checkout
                                                              reported          run

              19 Rewrite README  5 / 15k /   Commit `9f82f0e` Compared with    ---
                                 1.67M / 11k                  the test report  

       **Total**                 **203 /                                        
                                 1.01M /                                        
                                 36.98M /                                       
                                 564k**                                         
  ----------------------------------------------------------------------------------------------

Stages: prompts 1, 2, and 5 are Planning; 3, 4, and 6 are Scaffolding; 7 to
15 and 17 are Implementation; 16 is Testing; 18 and 19 are Documentation. The
Evaluation column lists the evidence available from the session history and
reviews.

The prompt texts below are reproduced from the project-session history, with
blank lines removed. Prompt 11 is two prompts sent in one message. The prompts
were prepared with input from the project owner, Claude, and GPT.

### Prompt 1

``` text
Read README.md in this repository carefully. It contains the challenge.
Propose an architecture for it. This step is planning only: do not write code,
create files, or commit anything.
Constraints:
- Go with the Gin framework.
- The whole project must run with Docker (Dockerfile and docker-compose).
- It must include a simple frontend. Keep it minimal, and include in your
  proposal what it does and how it is served.
- Do not read, create, edit, or commit ai.md at any point. I write it myself.
In your proposal:
1. Summarize the requirements and list any assumptions or ambiguities.
2. Compare at least 2-3 realistic approaches for storage and counting, and
   recommend one. Explain the trade-offs, including how the two-week expiry and
   scale to millions of users are handled.
3. Justify the ingestion protocol and every major tool.
4. Describe failure modes and how the system behaves under a sudden traffic spike.
5. Propose the folder structure, the test plan, and a step-by-step plan where
   each step can be a small commit.
6. Label every number as either measured or estimated.
Prefer the simplest design that meets the requirements. Avoid unnecessary
layers and infrastructure.
Rules for the implementation steps later:
- Make small, meaningful commits with clear messages. Each commit does one thing.
- Report token usage before and after each commit. Use a real source, and tell me
  which one. If you cannot read real usage, say so; never present a guess as measured.
- All code, comments, commits, and docs must be in English.
Reply to me in Persian, keeping technical terms in English.
Ask me about anything you cannot decide on your own, then stop and wait for my approval.
```

### Prompt 2

``` text
Approved with these answers and changes. Update the plan, then start with step 1 only.
Answers:
1. Yes, a new event renews the two weeks.
2. Yes, about 0.81% standard error is acceptable.
3. Start with 6h, but make bucket size configurable. Benchmark 1h and 6h, then
   choose the final default from measured results. Document that changing it on
   existing data is not safe.
4. Use feat/estimation-service. Do not push. At the end, merge into main with
   --ff-only and no squash, only after I approve.
Changes:
- Estimate cache TTL must never cross the next bucket boundary, otherwise the
  "expired users are never counted" claim is false.
- Do not equate 0.9% of time with 0.9% of users. Measure boundary error under
  uniform and spiky traffic. Early removal is always an undercount.
- Add a USS simulator in cmd/uss and a spike test that finds the saturation point.
- Add explicit Redis timeouts.
- Write PFADD and expiry atomically with TxPipeline.
- Define a small Store interface. Put the Redis implementation in its own package.
- Rename the read endpoint to GET /v1/segments/:segment/estimate.
- Make docs/DESIGN.md the first commit. Update it with measured numbers at the end.
For usage, report tokensUsed and plan percentages in one short line per commit,
and label the difference as estimated. Keep ai.md untouched.
```

### Prompt 3

``` text
Approved, with these changes before and during step 2:
1. In this repository only, set git user.name to "taghad" and user.email to
   "mhtaghad99@gmail.com". Then amend commit 21b7b82 with --reset-author. Do not push.
2. Bucket rule: before measuring, define the traffic model in docs/DESIGN.md,
   including the share of users who return during the window. Report the spiky
   traffic results too, even though they are not part of the decision rule.
   Mark all thresholds as my decision after I confirm them.
3. Add a short CLAUDE.md in step 2 containing the project rules: follow
   docs/DESIGN.md, one small commit per change, tests with the code, never touch
   ai.md, all project text in English, and the one-line usage report.
   I will clear this session afterwards, so these rules must survive in the files.
Then do step 2 only and stop.
```

### Prompt 4

``` text
/cont
```

### Prompt 5

``` text
I confirm the proposed thresholds as my decision, recorded as assumptions because
we have no real USS data. Mark them as such in docs/DESIGN.md.
Reduce the measurement scope:
- Run boundary error as a simple simulation, without Redis.
- Use 8 cases: uniform and spiky traffic, 0% and 50% returning users, 1h and 6h buckets.
- Use real Redis only for the accuracy test, PFCOUNT benchmark, memory and spike test.
Commit this change, report usage, and stop. Do not start step 3.
```

### Prompt 6

``` text
Read docs/DESIGN.md, then do step 3 only.
```

### Prompt 7

``` text
Add these to docs/DESIGN.md in one small commit, without building them:
- Scaling limit: a hot segment stays on one Redis Cluster shard because of its
  hash tag. The next step would be a fixed split by hash(user_id) into disjoint
  sub-keys, where the estimate is the sum of their counts.
- Rollups as the fallback option if PFCOUNT over many buckets proves too slow.
- Also state the exact EXPIREAT value for a bucket.
Then do step 4 only.
```

### Prompt 8

``` text
Do step 5 only, with these decisions and reasons recorded in code comments:
- user_id is opaque: 1 to 128 bytes, with no trimming. Its meaning belongs to USS.
- segment must match the DESIGN.md regex, case-sensitive, with no trimming.
- A batch must have 1 to 1000 events. Use a fixed constant in the estimator package,
  and check the size before validating events.
- Return a ValidationError with Index, Field, and Reason for the first invalid event.
```

### Prompt 9

``` text
Do step 6 only, recording these decisions and reasons in code comments:
- Add(ctx, events) reads now once from an injected clock, so a whole batch goes
  into one bucket, and calls ValidateBatch first.
- Estimate(ctx, segment) reads now once and returns a struct with Value (uint64)
  and AsOf (that time, in UTC), so the window and the reported time agree.
- Add a separate segment validation function returning ErrInvalidSegment, because
  an event index has no meaning for a read. Use the same pattern as ValidateBatch.
- Wrap store errors with %w. The service knows nothing about HTTP.
- Grouping events by segment belongs to the Store implementation, not the service.
- Tests use a fake store in the test file, with no Redis.
- No cache, singleflight, limiter, or timeouts in this step.
```

### Prompt 10

``` text
Read docs/DESIGN.md, then do step 7 only, recording these decisions and reasons in
code comments and updating the Service comment:
- The cache, singleflight, and estimate limit live in the estimator package, in
  the path of Service.Estimate. Add golang.org/x/sync for singleflight.
- An entry is valid only while its computed bucket is the current bucket and its
  age is below the TTL, using the injected clock.
- The singleflight key is segment plus bucket start.
- MAX_INFLIGHT_ESTIMATE is a non-blocking semaphore held only during recomputation.
  When full, return ErrOverloaded. Cached responses never touch it.
- Run the computation with context.WithoutCancel through DoChan, and each caller
  waits on its own context, so one cancelled request does not fail the others. The
  store operation deadline comes in step 9.
- Do not cache store errors.
- At most 10,000 entries; clear the whole cache when full. Protect it with sync.RWMutex.
- AsOf of a cached value is its computation time.
Tests: TTL expiry, no serving after a boundary within TTL, a request after a
boundary not joining an earlier flight, limit rejection, cancellation isolation,
errors not cached, size limit. Run with -race and run a mutation check.
```

### Prompt 11

``` text
Apply exactly, in small commits. Make no other technical decisions; if one is
needed, stop and ask.
CLAUDE.md: tidy and commit my uncommitted rule. Add: "Technical decisions are made
by the owner. If one is needed, stop and ask."
docker-compose.yml: maxmemory from REDIS_MAXMEMORY, default 3gb [E]; a redis-test
service with no volume or AOF, used by the test service; go test -race ./...
docs/DESIGN.md: MAX_INFLIGHT_ESTIMATE default 32 [E], 503 with Retry-After when
full, also covering the boundary burst (add it to the spike test); BUCKET_SIZE in
whole seconds; batch over 1000 returns 413.
Validate in Docker, report usage, stop.
Read docs/DESIGN.md, then do steps 8 and 9, one commit each:
- Group by segment; one variadic PFADD plus EXPIREAT from Bucket.ExpireAt() per key,
  in one TxPipeline. Use redis.UniversalClient.
- MaxRetries -1 and ContextTimeoutEnabled true. Verify both in the installed go-redis.
- Apply REDIS_OP_TIMEOUT inside each store operation. Remove the "step 9" comment
  from Service.
- Integration tests on redis-test: idempotency, union across buckets, every key has
  the expected expiry, excluded bucket not counted, and a timeout frees the slot.
No mutation check. Report usage, stop.
```

### Prompt 12

``` text
First, one small commit each:
- tmpfs on /data for redis-test; run tests with go test -p 1 -race ./...
- DESIGN.md: add REDIS_MAXMEMORY; Retry-After is a fixed 1s [E]; the boundary-burst
  test runs in process with real Redis and an injected clock.
Then do steps 10 to 13, one commit each:
- Ignore unknown JSON fields. Use http.MaxBytesReader; oversized body or batch: 413.
- 400: malformed JSON, empty batch, ValidationError, ErrInvalidSegment.
- 503 with Retry-After: 1: store errors, ErrOverloaded, full ingestion limiter.
- Error body: {"error":{"code","message","index","field"}}, last two optional.
- as_of: UTC RFC3339 in whole seconds.
- Non-blocking ingestion limiter as middleware on POST only, before reading the body.
- /healthz has no dependencies. /readyz uses a new Service.Ping.
- Pass c.Request.Context(). No extra response for a cancelled request.
- Handler tests use httptest and a fake, with no Redis.
No mutation check. If another decision is needed, stop and ask. Report usage, stop.
```

### Prompt 13

``` text
Your six choices in steps 10-13 are approved. Next time, ask first.
Do steps 14 to 17, one commit each:
14. gin.New with Recovery, no Logger, release mode. http.Server timeouts:
    ReadHeader 5s, Read 10s, Write 10s, Idle 60s; shutdown 10s [E]. Config from env.
    Any invalid or non-positive value: exit at startup with a clear error.
15. Frontend as in DESIGN.md. Render all output with textContent.
16. Multi-stage Dockerfile, CGO_ENABLED=0, final image distroless nonroot. Compose:
    restart on-failure, wait for healthy Redis, ESTIMATE_CACHE_TTL=5s, no app
    healthcheck. Verify docker compose up --build from a clean checkout, then POST,
    estimate, and the frontend.
17. cmd/uss per DESIGN.md: open-loop, fixed seed, in its own container under profile
    "tools". Measure latency from scheduled send time. Report first attempts
    separately from retries, the achieved rate, and distinct acknowledged users.
    Smoke test only; the spike test stays in step 21.
Update DESIGN.md where these add detail. No mutation check. If another decision is
needed, stop and ask. Report usage, stop.
```

### Prompt 14

``` text
1. Profiles: steady (constant rate) and burst (rate multiplier for a short window).
   uniform and spiky stay in the step 19 simulation only. Update DESIGN.md.
2. Retry: at most 3 retries, honoring Retry-After with jitter. Saturation uses
   first-attempt acceptance only. Report final acceptance separately.
3. Request timeout: 2s [E]. Count timeouts separately and retry them.
4. Publish ES on 127.0.0.1:8080:8080 only. cmd/uss connects through the compose network.
A timed-out event may have been written. So also report distinct attempted users.
After a spike, the estimate must be between acknowledged and attempted users, with
the 2.43% margin. Update DESIGN.md.
```

### Prompt 15

``` text
Your choices in steps 14-17 are approved. Ask first next time.
Small commits first:
- Compose: restart on-failure:5. DESIGN.md: static-debian12 base image; saturation
  p99 uses first attempts; ramp starts at 1000 events/s, batch 500, 30s per step [E].
- Improve the frontend design. Keep both forms and their behavior. The estimate is
  the main part of the page; rename the other form "Send a test event".
  Keep one embedded file, with no framework, CDN, or external fonts. Support light
  and dark themes and mobile width, and keep textContent for all output.
- Repeat the clean-checkout check, including the frontend in a browser.
Then do steps 18 to 21, one commit each, as DESIGN.md defines them.
Report raw results and machine details. Do not choose the final bucket size or edit
the results section; that is steps 22-24. If another decision is needed, stop and ask.
No mutation check. Report usage, stop.
```

### Prompt 16

``` text
Approved, with these changes:
- First: update both Redis images to redis:8.2.11, run tests, and run govulncheck ./...
  in Docker. Report findings without changing anything else.
- Item 9: check only after the last ramp step and after the burst.
- Item 11: no cmd/spikecheck. Put ramp and the saturation rule in cmd/uss with unit
  tests. scripts/spike.sh only collects docker stats.
- Item 12: also run a concurrent writer at 1000 events/s [E] during the burst. Report
  Add p99, the 503 rate, and how long until all segments are served.
Then do steps 18 to 21, one commit each. Report raw results and machine details. Do
not choose the bucket size or fill the results section. Stop if another decision is
needed.
```

### Prompt 17

``` text
Decisions:
1. Dependencies: update x/net to at least v0.62.0, quic-go to at least v0.59.1, and
   x/text to a fixed version. Rerun tests and govulncheck. Report anything left.
2. Burst: base 204,800, 10x peak (2,048,000 events/s). Report acceptance, 503 rate,
   time to recover after the peak, and the after-burst estimate check.
3. Step 22: all four rules hold (1.27%, 6.8 ms, 2.43 GB, 6.3%), so the default
   becomes 1h. Update code, compose, and docs.
Then steps 23 and 24:
- Fill the results section. Replace estimates with measured values.
- State these limits: 3gb holds about 620 fully dense segments at 1h [calculated];
  throughput comes from one laptop with repeated users and is not production
  capacity; the boundary test's fixed 1s retry likely shapes the 15s result; Add p99
  there has only 30 samples.
- Add docs/DECISIONS.md, each with Decision, Why, Rejected, Cost: HLL instead of an
  exact count and no Postgres; time buckets and 1h; undercount over counting expired
  users; REST with batches; 503 and no buffer instead of Kafka; boundary-limited
  cache and estimate limit; AOF without replica; no history kept and what to add if
  needed; what was deliberately not built.
- README: a few lines at the top with run commands and links. Keep the challenge text.
One commit each. Do not touch ai.md, push, or merge. Stop if another decision is needed.
```

### Prompt 18

``` text
This is the final prompt for this session. Read CLAUDE.md and docs/DESIGN.md first.
Do all of the following, one commit each. Do not touch ai.md and do not push.
1. Rerun the ramp and the burst on 1h with the current dependencies, using the
   approved method. Update DESIGN.md and DECISIONS.md if the numbers change.
2. Fix the capacity number: 3gb holds about 664 fully dense segments [calculated].
3. Add a static test report, built only from real results. No live dashboard or polling.
   - Commit raw summaries as docs/results/*.json, with machine details. Results that
     no longer exist as raw files come from the DESIGN.md measured values; say so.
   - A small Go generator produces docs/TEST_REPORT.md and SVG charts that render on
     GitHub.
   - Serve the same report at GET /report as one embedded page, linked from the
     frontend. No framework, CDN, or external fonts. Support light, dark, and mobile.
   - Charts: ramp acceptance and p99 with saturation, burst timeline, boundary
     undercount for all 8 cases, 1h vs 6h with the four rules, HLL error, and test,
     coverage, race, and govulncheck results.
   - Show the machine and "not production capacity" at the top. Label measured and
     calculated values. Never invent or interpolate points.
   - Test the generator and the route. Check the page in a browser.
4. Link the report from the README.
5. Final review: check the code and docs against the original README line by line.
   Fix bugs and wrong claims. Do not add features or change decisions.
6. Final checks from a clean checkout: go test -p 1 -race ./..., gofmt, go vet,
   govulncheck, docker compose up --build, POST, estimate, frontend, and /report.
7. Merge feat/estimation-service into main with --ff-only, without squashing. I approve
   this merge. Do not push. Leave the stack running.
If a real blocker needs my decision, stop and ask. Otherwise finish everything. End
with a short report: commits, final numbers, anything changed by the final review,
known limits, and total usage from the first and last get_usage readings.
```

### Prompt 19

``` text
Rewrite README.md for a reviewer who has five minutes. Base every claim on the code,
docs/DESIGN.md, docs/DECISIONS.md, and docs/TEST_REPORT.md. Add nothing new.
Order:
1. What it is, in two sentences: it estimates users per segment over 14 days, and the
   count is approximate.
2. Quick start with Docker only: run, frontend, report, tests, and simulator. Say which
   URLs to open. Run every command you write.
3. API: endpoints, status codes, and one curl example each.
4. How it works: a small diagram, hourly HyperLogLog buckets, and how expiry works.
5. Key decisions: one line each, with why, linking to docs/DECISIONS.md.
6. Results: a small table with bucket size, highest sustained rate, saturation, burst
   recovery, largest HLL error, and memory. State "one laptop, repeated users, not
   production capacity".
7. Known limits: at most 1h early removal, spiky traffic undercount, up to 1M distinct
   users tested, Redis as a single point of failure, reliance on USS retry, no history
   kept, about 664 dense segments at 3gb.
8. Project layout, in a few lines.
9. Links: design, decisions, test report, and ai.md.
10. The original challenge text, unchanged, under "Original challenge" at the end.
Keep it short, with no marketing language. Label numbers as in the report. Do not create
or read ai.md. Do not change any other file. Commit on main in one commit. Do not push.
```

## 4. Usage Tracking and Cost Management

### Usage Tracking

A usage-reporting rule was added to `CLAUDE.md`. The project session was
instructed to read usage before and after each commit using `get_usage`
and report the results in a single line.

The reporting format was:

``` text
<hash>: before <tokens>, 5h <n>%, weekly <n>% [measured] -> after <tokens>, 5h <n>%, weekly <n>% [measured] | diff +<tokens> [estimated]
```

-   Values directly returned by `get_usage` were labelled as
    **measured**.
-   The difference between readings was labelled as **estimated**,
    because `tokensUsed` represents context size rather than cumulative
    token consumption.
-   `/context` was used to inspect what was occupying the context.
-   After development, the actual prompt usage figures were extracted
    from Claude Code's JSONL session history files.

The extracted project-session figures were:

  Metric                             Result
  ------------------------------ ----------
  Project prompts                        19
  API calls                             203
  Input and cache-write tokens      \~1.01M
  Cache-read tokens                \~36.98M
  Output tokens                      \~564K

These figures cover the project session prompts and do not include the
consultation session or context outside those records.

### References and Guidance

The following resources were used as guidance:

-   [Maximizing the value of your Claude Code
    sessions](https://claude.com/resources/articles/maximizing-the-value-of-your-claude-code-sessions)
    --- I shared this article with the consultation session and asked it
    to summarize the recommendations and identify practical actions for
    the project.
-   [Claude Code Cost Management
    Documentation](https://code.claude.com/docs/en/costs) --- the
    official documentation was consulted for usage-monitoring tools and
    cost-management options.

### Actions Taken to Reduce Usage

-   **Separate sessions:** Technical consultation and decision-making
    were kept in a separate session from implementation.
-   **Clearing context:** `/clear` was used several times between groups
    of implementation steps. After clearing, project-session context
    usage dropped from approximately 150K--360K to around 70K--80K.
-   **Persistent project documentation:** Important decisions and
    project rules were stored in `CLAUDE.md` and `docs/DESIGN.md`,
    allowing work to continue after `/clear` without relying on the
    previous conversation.
-   **Smaller prompts:** Prompts were made more focused by assigning
    specific steps rather than asking for several unrelated tasks at
    once. For example: `Read docs/DESIGN.md, then do step 3 only.`
-   **Grouping steps:** Some related steps were grouped into a single
    prompt to reduce unnecessary back-and-forth.
-   **Limiting mutation testing:** Step 7 as a whole, including its mutation
    check, grew the context by about 54K tokens `[estimated]`. The share of the
    check was not measured separately. After that, later prompts did not ask
    for mutation testing.
-   **Reducing benchmark scope:** The measurement plan was reduced from
    36 runs to 8 cases.
-   **Moving work into files:** Reports and JSON files were generated
    through scripts. Only the grouped report was sent to the
    consultation session for review.

### Helper Prompts

``` text
Report tokensUsed and plan percentages in one short line per commit, and label the difference as estimated.
```

``` text
Read docs/DESIGN.md, then do step 3 only.
```

### Limitations and Observations

-   Usage remained significant. The last recorded plan readings were 88%
    of the five-hour allowance and 14% of the weekly allowance.
-   The consultation session also accumulated a large context. One
    `/context` reading showed 230.5K. This session is not included in
    the project-session usage figures above.
-   The extracted token figures should not be interpreted as total
    billing or total usage across all sessions.
