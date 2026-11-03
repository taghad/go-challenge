package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/taghad/go-challenge/internal/estimator"
)

const (
	testBoundary    = 1794096000
	testTTL         = 30 * time.Second
	testMaxInflight = 4
	testMaxBody     = 1 << 20
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

type ctxKey struct{}

// fakeStore records its calls and returns the configured count and error. When block
// is set, Add and Count wait until it is closed, so a test can hold a request open.
type fakeStore struct {
	mu       sync.Mutex
	addCtx   []context.Context
	added    [][]estimator.Event
	countCtx []context.Context
	pings    int
	count    uint64
	err      error
	block    chan struct{}
}

func (f *fakeStore) Add(ctx context.Context, _ estimator.Bucket, events []estimator.Event) error {
	f.mu.Lock()
	f.addCtx = append(f.addCtx, ctx)
	f.added = append(f.added, events)
	block, err := f.block, f.err
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return err
}

func (f *fakeStore) Count(ctx context.Context, _ string, _ []estimator.Bucket) (uint64, error) {
	f.mu.Lock()
	f.countCtx = append(f.countCtx, ctx)
	block, count, err := f.block, f.count, f.err
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return count, err
}

func (f *fakeStore) Ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pings++
	return f.err
}

func newService(t *testing.T, store estimator.Store, now time.Time, maxInflight int) *estimator.Service {
	t.Helper()
	window, err := estimator.NewWindow(6 * time.Hour)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	return estimator.NewService(store, window, func() time.Time { return now }, testTTL, maxInflight)
}

func newTestRouter(t *testing.T, store estimator.Store) http.Handler {
	t.Helper()
	service := newService(t, store, time.Unix(testBoundary, 0), testMaxInflight)
	return NewRouter(service, Config{MaxBodyBytes: testMaxBody})
}

func serve(router http.Handler, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func postEvents(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
}

// errorFields decodes an error response into a map, so a test can tell an omitted
// field from a zero value.
func errorFields(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", recorder.Body.String(), err)
	}
	return body.Error
}

// wantError checks the status, the code, and the fields every error shares. Index and
// field must be absent unless a test expects them.
func wantError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if recorder.Code != status {
		t.Errorf("status = %d, want %d", recorder.Code, status)
	}
	fields := errorFields(t, recorder)
	if fields["code"] != code {
		t.Errorf("code = %v, want %q", fields["code"], code)
	}
	if message, _ := fields["message"].(string); message == "" {
		t.Error("message is empty")
	}
	return fields
}

// wantPlainError also checks that an error about no single event has no index or field.
func wantPlainError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	fields := wantError(t, recorder, status, code)
	if _, ok := fields["index"]; ok {
		t.Errorf("index is present: %v", fields["index"])
	}
	if _, ok := fields["field"]; ok {
		t.Errorf("field is present: %v", fields["field"])
	}
}

func wantRetryAfter(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want %q", got, "1")
	}
}

// wantNoResponse checks that a cancelled request got nothing written for it. Gin still
// sends its default status when the handler returns, to a client that has gone.
func wantNoResponse(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Body.Len() != 0 {
		t.Errorf("body = %q, want it empty", recorder.Body.String())
	}
	if got := recorder.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want it absent", got)
	}
}
