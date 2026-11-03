package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/taghad/go-challenge/internal/estimator"
)

// defaultRetryWait is used when there is no valid Retry-After, as after a timeout. It
// matches the fixed value ES sends [E].
const defaultRetryWait = time.Second

type outcome int

const (
	acknowledged outcome = iota
	rejected             // 503: retried
	timedOut             // no response in time; the events may have been written: retried
	networkError         // connection failed: retried
	otherError           // any other response, such as 400: not retried
)

// retriable is true for every outcome that leaves the events unacknowledged and that a
// repeat could fix. PFADD is idempotent, so a repeat cannot double count a user.
func (o outcome) retriable() bool {
	return o == rejected || o == timedOut || o == networkError
}

// batch is one request. It holds the drawn indexes as well as the body, so that users
// can be recorded without decoding the body.
type batch struct {
	users    []int
	segments []int
	body     []byte
}

// simulator sends the scheduled batches and records what happens to them.
type simulator struct {
	options  options
	endpoint string
	client   *http.Client
	recorder *recorder
	// firstBatch is added to each batch index, so that the steps of a ramp draw new
	// events instead of repeating the same ones.
	firstBatch uint64
}

func newSimulator(o options, client *http.Client) *simulator {
	return newStepSimulator(o, client, newUserTracker(o.users, o.segments), 0)
}

func newStepSimulator(o options, client *http.Client, users *userTracker, firstBatch uint64) *simulator {
	return &simulator{
		options:    o,
		endpoint:   strings.TrimSuffix(o.url, "/") + "/v1/events",
		client:     client,
		recorder:   newRecorder(users),
		firstBatch: firstBatch,
	}
}

// newClient has no limit on connections, because a limit would make a request wait for
// an earlier one, and the load would no longer be open loop. Idle connections are kept
// so that the run measures ES and not connection setup.
func newClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 0
	transport.MaxIdleConnsPerHost = 4096
	return &http.Client{Transport: transport}
}

// run starts each batch at its scheduled time, in its own goroutine, without waiting
// for earlier batches. The goroutines are bounded by the rate, the request timeout, and
// the retry limit. When ctx ends no new batch starts, and those in flight finish.
func (s *simulator) run(ctx context.Context) report {
	offsets := s.options.schedule()
	start := time.Now()
	timer := time.NewTimer(0)
	defer timer.Stop()
	var running sync.WaitGroup
	interrupted := false
schedule:
	for i, offset := range offsets {
		scheduled := start.Add(offset)
		timer.Reset(time.Until(scheduled))
		select {
		case <-ctx.Done():
			interrupted = true
			break schedule
		case <-timer.C:
		}
		running.Go(func() { s.sendBatch(i, start, scheduled) })
	}
	running.Wait()
	result := s.recorder.report(s.options, len(offsets), interrupted)
	result.StartedAt = start.UTC().Format(time.RFC3339Nano)
	result.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return result
}

// sendBatch sends one batch with its retries. Each batch has its own generator, seeded
// by its index, so its events and jitter do not depend on timing or goroutine order.
//
// An attempt's latency runs from its own scheduled time, so a late send counts as
// latency and is not hidden. The scheduled time of a retry is when its wait ends. The
// final latency runs from the scheduled time of the batch to its acknowledgement.
func (s *simulator) sendBatch(index int, start, scheduled time.Time) {
	generator := rand.New(rand.NewPCG(s.options.seed, s.firstBatch+uint64(index)))
	b := s.newBatch(generator)
	s.recorder.firstSend(b, start, scheduled)

	attemptScheduled := scheduled
	for attempt := 0; ; attempt++ {
		result, retryAfter := s.send(b.body)
		end := time.Now()
		s.recorder.attempt(attempt == 0, result, len(b.users), end.Sub(attemptScheduled), end.Sub(start))
		switch {
		case result == acknowledged:
			s.recorder.acknowledge(b, end.Sub(scheduled))
			return
		case !result.retriable():
			s.recorder.fail(len(b.users))
			return
		case attempt == s.options.maxRetries:
			s.recorder.abandon(len(b.users))
			return
		}
		wait := retryWait(retryAfter, s.options.retryJitter, generator)
		attemptScheduled = end.Add(wait)
		time.Sleep(wait)
	}
}

func (s *simulator) newBatch(generator *rand.Rand) batch {
	b := batch{
		users:    make([]int, s.options.batch),
		segments: make([]int, s.options.batch),
	}
	events := make([]estimator.Event, s.options.batch)
	for i := range events {
		b.users[i] = generator.IntN(s.options.users)
		b.segments[i] = generator.IntN(s.options.segments)
		events[i] = estimator.Event{
			UserID:  "u" + strconv.Itoa(b.users[i]),
			Segment: segmentName(s.options.segmentPrefix, b.segments[i]),
		}
	}
	// Marshalling a struct of strings cannot fail.
	b.body, _ = json.Marshal(struct {
		Events []estimator.Event `json:"events"`
	}{events})
	return b
}

// send makes one attempt, and returns its outcome and how long the server asked to wait.
func (s *simulator) send(body []byte) (outcome, time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), s.options.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return otherError, 0
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			return timedOut, defaultRetryWait
		}
		return networkError, defaultRetryWait
	}
	defer response.Body.Close()
	// Reading the body to its end lets the connection be used again.
	io.Copy(io.Discard, response.Body)
	switch response.StatusCode {
	case http.StatusNoContent:
		return acknowledged, 0
	case http.StatusServiceUnavailable:
		return rejected, parseRetryAfter(response.Header.Get("Retry-After"))
	default:
		return otherError, 0
	}
}

// parseRetryAfter reads a whole number of seconds, the only form ES sends.
func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return defaultRetryWait
	}
	return time.Duration(seconds) * time.Second
}

// retryWait never returns less than Retry-After. The jitter only adds to it, so that
// batches rejected together do not all come back at the same moment.
func retryWait(retryAfter, jitter time.Duration, generator *rand.Rand) time.Duration {
	if jitter <= 0 {
		return retryAfter
	}
	return retryAfter + time.Duration(generator.Int64N(int64(jitter)))
}
