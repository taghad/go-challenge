package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func getEstimate(segment string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/v1/segments/"+segment+"/estimate", nil)
}

func TestEstimateResponse(t *testing.T) {
	// The time is in another zone and has a fraction, to show that as_of has neither,
	// and that the fraction is dropped and not rounded up.
	now := time.Date(2026, 11, 3, 19, 30, 0, 999999999, time.FixedZone("UTC+3:30", 3*3600+1800))
	service := newService(t, &fakeStore{count: 1042311}, now, testMaxInflight)

	recorder := serve(NewRouter(service, Config{MaxBodyBytes: testMaxBody}), getEstimate("sports"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, http.StatusOK, recorder.Body)
	}
	want := `{"segment":"sports","estimate":1042311,"as_of":"2026-11-03T16:00:00Z"}`
	if got := recorder.Body.String(); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func TestEstimateUnknownSegmentIsZero(t *testing.T) {
	recorder := serve(newTestRouter(t, &fakeStore{}), getEstimate("unknown"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if !strings.Contains(recorder.Body.String(), `"estimate":0`) {
		t.Errorf("body = %s, want an estimate of 0", recorder.Body)
	}
}

func TestEstimateInvalidSegment(t *testing.T) {
	tests := []struct {
		name    string
		segment string
	}{
		{"space", "not%20valid"},
		{"too long", strings.Repeat("a", 65)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{}

			recorder := serve(newTestRouter(t, store), getEstimate(tt.segment))

			wantPlainError(t, recorder, http.StatusBadRequest, "invalid_segment")
			if len(store.countCtx) != 0 {
				t.Error("an invalid segment reached the store")
			}
		})
	}
}

func TestEstimateStoreError(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"store failure", errors.New("secret internal detail")},
		// A store timeout wraps a context error, but this client is still waiting.
		{"store timeout", fmt.Errorf("op: %w", context.DeadlineExceeded)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := serve(newTestRouter(t, &fakeStore{err: tt.err}), getEstimate("sports"))

			wantPlainError(t, recorder, http.StatusServiceUnavailable, "store_unavailable")
			wantRetryAfter(t, recorder)
			if strings.Contains(recorder.Body.String(), "secret") {
				t.Errorf("body leaks the store error: %s", recorder.Body)
			}
		})
	}
}

func TestEstimateOverloaded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &fakeStore{block: make(chan struct{})}
		service := newService(t, store, time.Unix(testBoundary, 0), 1)
		router := NewRouter(service, Config{MaxBodyBytes: testMaxBody})

		running := make(chan *httptest.ResponseRecorder, 1)
		go func() { running <- serve(router, getEstimate("sports")) }()
		synctest.Wait()

		recorder := serve(router, getEstimate("news"))
		wantPlainError(t, recorder, http.StatusServiceUnavailable, "overloaded")
		wantRetryAfter(t, recorder)

		close(store.block)
		if got := <-running; got.Code != http.StatusOK {
			t.Errorf("running request: status = %d, want %d", got.Code, http.StatusOK)
		}
	})
}

func TestEstimatePassesRequestContext(t *testing.T) {
	store := &fakeStore{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "value")

	serve(newTestRouter(t, store), getEstimate("sports").WithContext(ctx))

	if len(store.countCtx) != 1 || store.countCtx[0].Value(ctxKey{}) != "value" {
		t.Error("store did not receive the request's context")
	}
}

func TestEstimateCancelledRequestGetsNoResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &fakeStore{block: make(chan struct{})}
		router := newTestRouter(t, store)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		// The store is held, so the only way for this request to return is its context.
		recorder := serve(router, getEstimate("sports").WithContext(ctx))
		wantNoResponse(t, recorder)

		close(store.block)
		synctest.Wait()
	})
}
