package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/taghad/go-challenge/internal/estimator"
)

func batchOf(n int) string {
	events := make([]string, n)
	for i := range events {
		events[i] = fmt.Sprintf(`{"user_id":"u%d","segment":"sports"}`, i)
	}
	return `{"events":[` + strings.Join(events, ",") + `]}`
}

func TestAddEventsAccepted(t *testing.T) {
	store := &fakeStore{}
	router := newTestRouter(t, store)

	recorder := serve(router, postEvents(`{"events":[
		{"user_id":"u1","segment":"sports"},
		{"user_id":"u2","segment":"news"}
	]}`))

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, http.StatusNoContent, recorder.Body)
	}
	if recorder.Body.Len() != 0 {
		t.Errorf("body = %q, want it empty", recorder.Body)
	}
	want := []estimator.Event{{UserID: "u1", Segment: "sports"}, {UserID: "u2", Segment: "news"}}
	if len(store.added) != 1 || !slices.Equal(store.added[0], want) {
		t.Errorf("store received %+v, want one batch %+v", store.added, want)
	}
}

func TestAddEventsIgnoresUnknownFields(t *testing.T) {
	store := &fakeStore{}
	router := newTestRouter(t, store)

	recorder := serve(router, postEvents(`{"source":"uss","events":[{"user_id":"u1","segment":"sports","ts":1}]}`))

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, http.StatusNoContent, recorder.Body)
	}
	want := []estimator.Event{{UserID: "u1", Segment: "sports"}}
	if !slices.Equal(store.added[0], want) {
		t.Errorf("store received %+v, want %+v", store.added[0], want)
	}
}

func TestAddEventsMalformedJSON(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty body", ``},
		{"not JSON", `events`},
		{"truncated", `{"events":[`},
		{"data after the value", `{"events":[{"user_id":"u1","segment":"sports"}]} x`},
		{"second value", `{"events":[]} {"events":[]}`},
		{"events of the wrong type", `{"events":"u1"}`},
		{"field of the wrong type", `{"events":[{"user_id":1,"segment":"sports"}]}`},
		{"body of the wrong type", `[]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{}

			recorder := serve(newTestRouter(t, store), postEvents(tt.body))

			wantPlainError(t, recorder, http.StatusBadRequest, "malformed_json")
			if len(store.added) != 0 {
				t.Errorf("store received %+v, want nothing", store.added)
			}
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("connection broke")
}

func TestAddEventsUnreadableBody(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/events", io.NopCloser(failingReader{}))

	recorder := serve(newTestRouter(t, &fakeStore{}), request)

	wantPlainError(t, recorder, http.StatusBadRequest, "malformed_json")
}

func TestAddEventsEmptyBatch(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty list", `{"events":[]}`},
		{"null list", `{"events":null}`},
		{"no list", `{}`},
		{"null body", `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := serve(newTestRouter(t, &fakeStore{}), postEvents(tt.body))

			wantPlainError(t, recorder, http.StatusBadRequest, "empty_batch")
		})
	}
}

func TestAddEventsInvalidEvent(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantIndex float64
		wantField string
	}{
		{"first event", `{"events":[{"user_id":"","segment":"sports"}]}`, 0, "user_id"},
		{
			"later event",
			`{"events":[{"user_id":"u1","segment":"sports"},{"user_id":"u2","segment":"not valid"}]}`,
			1, "segment",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{}

			recorder := serve(newTestRouter(t, store), postEvents(tt.body))

			fields := wantError(t, recorder, http.StatusBadRequest, "invalid_event")
			if fields["index"] != tt.wantIndex {
				t.Errorf("index = %v, want %v", fields["index"], tt.wantIndex)
			}
			if fields["field"] != tt.wantField {
				t.Errorf("field = %v, want %q", fields["field"], tt.wantField)
			}
			if len(store.added) != 0 {
				t.Errorf("store received %+v, want nothing", store.added)
			}
		})
	}
}

func TestAddEventsBodyLimit(t *testing.T) {
	const limit = 200
	valid := `{"events":[{"user_id":"u1","segment":"sports"}]}`
	atLimit := valid + strings.Repeat(" ", limit-len(valid))
	tests := []struct {
		name string
		body string
		want int
	}{
		{"at the limit", atLimit, http.StatusNoContent},
		{"one byte over", atLimit + " ", http.StatusRequestEntityTooLarge},
		{"over and not JSON", strings.Repeat("x", limit+1), http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{}
			service := newService(t, store, time.Unix(testBoundary, 0), testMaxInflight)
			router := NewRouter(service, Config{MaxBodyBytes: limit, MaxInflightIngest: testMaxInflight})

			recorder := serve(router, postEvents(tt.body))

			if tt.want == http.StatusNoContent {
				if recorder.Code != tt.want {
					t.Errorf("status = %d, want %d", recorder.Code, tt.want)
				}
				return
			}
			wantPlainError(t, recorder, tt.want, "body_too_large")
			if len(store.added) != 0 {
				t.Errorf("store received %+v, want nothing", store.added)
			}
		})
	}
}

func TestAddEventsBatchLimit(t *testing.T) {
	store := &fakeStore{}
	router := newTestRouter(t, store)

	recorder := serve(router, postEvents(batchOf(estimator.MaxBatchSize)))
	if recorder.Code != http.StatusNoContent {
		t.Errorf("batch at the limit: status = %d, want %d", recorder.Code, http.StatusNoContent)
	}

	recorder = serve(router, postEvents(batchOf(estimator.MaxBatchSize+1)))
	wantPlainError(t, recorder, http.StatusRequestEntityTooLarge, "batch_too_large")
	if len(store.added) != 1 {
		t.Errorf("store batches = %d, want only the batch at the limit", len(store.added))
	}
}

func TestAddEventsStoreError(t *testing.T) {
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
			recorder := serve(newTestRouter(t, &fakeStore{err: tt.err}), postEvents(batchOf(1)))

			wantPlainError(t, recorder, http.StatusServiceUnavailable, "store_unavailable")
			wantRetryAfter(t, recorder)
			if strings.Contains(recorder.Body.String(), "secret") {
				t.Errorf("body leaks the store error: %s", recorder.Body)
			}
		})
	}
}

func TestAddEventsPassesRequestContext(t *testing.T) {
	store := &fakeStore{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "value")

	serve(newTestRouter(t, store), postEvents(batchOf(1)).WithContext(ctx))

	if len(store.addCtx) != 1 || store.addCtx[0].Value(ctxKey{}) != "value" {
		t.Error("store did not receive the request's context")
	}
}

func TestAddEventsCancelledRequestGetsNoResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &fakeStore{err: fmt.Errorf("add events: %w", context.Canceled)}

	recorder := serve(newTestRouter(t, store), postEvents(batchOf(1)).WithContext(ctx))

	wantNoResponse(t, recorder)
}
