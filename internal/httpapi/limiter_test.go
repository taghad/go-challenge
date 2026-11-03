package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func newLimitedRouter(t *testing.T, store *fakeStore, maxInflight int) http.Handler {
	t.Helper()
	service := newService(t, store, time.Unix(testBoundary, 0), testMaxInflight)
	return NewRouter(service, Config{MaxBodyBytes: testMaxBody, MaxInflightIngest: maxInflight})
}

// startPost serves a valid batch in its own goroutine.
func startPost(router http.Handler) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serve(router, postEvents(batchOf(1))) }()
	return done
}

// countingBody records whether anything read the request body.
type countingBody struct {
	io.Reader
	reads int
}

func (b *countingBody) Read(p []byte) (int, error) {
	b.reads++
	return b.Reader.Read(p)
}

func TestIngestRejectedWhenFullWithoutReadingBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &fakeStore{block: make(chan struct{})}
		router := newLimitedRouter(t, store, 1)

		running := startPost(router)
		synctest.Wait()

		body := &countingBody{Reader: strings.NewReader(batchOf(1))}
		recorder := serve(router, httptest.NewRequest(http.MethodPost, "/v1/events", body))

		wantPlainError(t, recorder, http.StatusServiceUnavailable, "overloaded")
		wantRetryAfter(t, recorder)
		if body.reads != 0 {
			t.Errorf("body reads = %d, want 0", body.reads)
		}
		store.mu.Lock()
		batches := len(store.added)
		store.mu.Unlock()
		if batches != 1 {
			t.Errorf("store batches = %d, want only the running one", batches)
		}

		close(store.block)
		if got := <-running; got.Code != http.StatusNoContent {
			t.Errorf("running request: status = %d, want %d", got.Code, http.StatusNoContent)
		}
	})
}

func TestIngestLimitAllowsUpToMax(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &fakeStore{block: make(chan struct{})}
		router := newLimitedRouter(t, store, 2)

		first := startPost(router)
		second := startPost(router)
		synctest.Wait()
		close(store.block)

		if got := <-first; got.Code != http.StatusNoContent {
			t.Errorf("first: status = %d, want %d", got.Code, http.StatusNoContent)
		}
		if got := <-second; got.Code != http.StatusNoContent {
			t.Errorf("second: status = %d, want %d", got.Code, http.StatusNoContent)
		}
	})
}

// With one slot, each request here would be rejected if an earlier one kept it.
func TestIngestSlotFreedAfterEveryOutcome(t *testing.T) {
	store := &fakeStore{}
	router := newLimitedRouter(t, store, 1)

	if got := serve(router, postEvents(`not JSON`)); got.Code != http.StatusBadRequest {
		t.Errorf("malformed request: status = %d, want %d", got.Code, http.StatusBadRequest)
	}
	store.err = errors.New("store down")
	wantPlainError(t, serve(router, postEvents(batchOf(1))), http.StatusServiceUnavailable, "store_unavailable")
	store.err = nil
	if got := serve(router, postEvents(batchOf(1))); got.Code != http.StatusNoContent {
		t.Errorf("valid request: status = %d, want %d", got.Code, http.StatusNoContent)
	}
}

func TestIngestLimitDoesNotApplyToEstimates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &fakeStore{count: 7}
		router := newLimitedRouter(t, store, 1)

		// The value is cached first, so the estimate below needs no store while the
		// store is held.
		if got := serve(router, getEstimate("sports")); got.Code != http.StatusOK {
			t.Fatalf("first estimate: status = %d, want %d", got.Code, http.StatusOK)
		}
		store.block = make(chan struct{})
		running := startPost(router)
		synctest.Wait()

		if got := serve(router, getEstimate("sports")); got.Code != http.StatusOK {
			t.Errorf("estimate with a full limiter: status = %d, want %d", got.Code, http.StatusOK)
		}

		close(store.block)
		<-running
	})
}

func TestIngestRejectionOfCancelledRequestGetsNoResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &fakeStore{block: make(chan struct{})}
		router := newLimitedRouter(t, store, 1)

		running := startPost(router)
		synctest.Wait()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		wantNoResponse(t, serve(router, postEvents(batchOf(1)).WithContext(ctx)))

		close(store.block)
		<-running
	})
}
