package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/taghad/go-challenge/internal/estimator"
	"github.com/taghad/go-challenge/internal/web"
)

type panicStore struct{ fakeStore }

func (*panicStore) Add(context.Context, estimator.Bucket, []estimator.Event) error {
	panic("store bug")
}

func TestPanicReturns500AndFreesIngestionSlot(t *testing.T) {
	writer := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = io.Discard
	t.Cleanup(func() { gin.DefaultErrorWriter = writer })

	router := newTestRouter(t, &panicStore{})
	body := `{"events":[{"user_id":"u1","segment":"sports"}]}`

	// More requests than slots: a slot that a panic kept would turn a later 500 into a 503.
	for range testMaxInflight + 1 {
		recorder := serve(router, postEvents(body))
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
		}
	}
}

func TestIndexServesFrontendWithoutStore(t *testing.T) {
	store := &fakeStore{err: errors.New("store down")}

	recorder := serve(newTestRouter(t, store), get("/"))

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if !bytes.Equal(recorder.Body.Bytes(), web.Index) {
		t.Error("body is not the embedded page")
	}
	if store.pings != 0 {
		t.Errorf("store pings = %d, want 0", store.pings)
	}
}

func TestReportServesTheEmbeddedPageWithoutStore(t *testing.T) {
	store := &fakeStore{err: errors.New("store down")}

	recorder := serve(newTestRouter(t, store), get("/report"))

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if !bytes.Equal(recorder.Body.Bytes(), web.Report) || len(web.Report) == 0 {
		t.Error("body is not the embedded report")
	}
}

func TestRouterLogsNoRequests(t *testing.T) {
	var output bytes.Buffer
	writer := gin.DefaultWriter
	gin.DefaultWriter = &output
	t.Cleanup(func() { gin.DefaultWriter = writer })

	recorder := serve(newTestRouter(t, &fakeStore{}), get("/healthz"))

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if output.Len() != 0 {
		t.Errorf("router logged %q", output.String())
	}
}
