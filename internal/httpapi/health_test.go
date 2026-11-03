package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(target string) *http.Request {
	return httptest.NewRequest(http.MethodGet, target, nil)
}

func TestHealthzHasNoDependencies(t *testing.T) {
	store := &fakeStore{err: errors.New("store down")}

	recorder := serve(newTestRouter(t, store), get("/healthz"))

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if store.pings != 0 {
		t.Errorf("store pings = %d, want 0", store.pings)
	}
}

func TestReadyzWhenStoreIsReachable(t *testing.T) {
	store := &fakeStore{}

	recorder := serve(newTestRouter(t, store), get("/readyz"))

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if store.pings != 1 {
		t.Errorf("store pings = %d, want 1", store.pings)
	}
}

func TestReadyzWhenStoreIsUnreachable(t *testing.T) {
	store := &fakeStore{err: errors.New("secret internal detail")}

	recorder := serve(newTestRouter(t, store), get("/readyz"))

	wantPlainError(t, recorder, http.StatusServiceUnavailable, "store_unavailable")
	wantRetryAfter(t, recorder)
	if strings.Contains(recorder.Body.String(), "secret") {
		t.Errorf("body leaks the store error: %s", recorder.Body)
	}
}

func TestReadyzCancelledRequestGetsNoResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &fakeStore{err: context.Canceled}

	recorder := serve(newTestRouter(t, store), get("/readyz").WithContext(ctx))

	wantNoResponse(t, recorder)
}
