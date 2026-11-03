package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/taghad/go-challenge/internal/estimator"
)

// runMainEnv makes the test binary run main instead of the tests, so a test can check
// the real process: its exit code, its output, and how it handles a signal.
const runMainEnv = "ES_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func serverCommand(env ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), runMainEnv+"=1")
	cmd.Env = append(cmd.Env, env...)
	return cmd
}

func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

// startServer runs the real process and waits until it answers.
func startServer(t *testing.T) (*exec.Cmd, string) {
	t.Helper()
	addr := freeAddr(t)
	cmd := serverCommand("HTTP_ADDR=" + addr)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })

	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			response.Body.Close()
			return cmd, "http://" + addr
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not answer: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProcessExitsWithClearErrorOnInvalidConfig(t *testing.T) {
	var stderr bytes.Buffer
	cmd := serverCommand("MAX_INFLIGHT_INGEST=0", "SHUTDOWN_TIMEOUT=-1s")
	cmd.Stderr = &stderr

	err := cmd.Run()

	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("Run = %v, want exit code 1", err)
	}
	for _, name := range []string{"MAX_INFLIGHT_INGEST", "SHUTDOWN_TIMEOUT"} {
		if !strings.Contains(stderr.String(), name) {
			t.Errorf("stderr %q does not name %s", stderr.String(), name)
		}
	}
}

func TestProcessExitsCleanlyOnSIGTERM(t *testing.T) {
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(signal.String(), func(t *testing.T) {
			cmd, _ := startServer(t)
			if err := cmd.Process.Signal(signal); err != nil {
				t.Fatalf("signal: %v", err)
			}
			if err := cmd.Wait(); err != nil {
				t.Errorf("Wait = %v, want a clean exit", err)
			}
		})
	}
}

// This checks that the parts are wired to each other and to a real Redis.
func TestProcessServesEventsFromRedis(t *testing.T) {
	if os.Getenv("REDIS_ADDR") == "" {
		t.Fatal("REDIS_ADDR is not set; run the tests with: docker compose run --rm test")
	}
	_, base := startServer(t)
	segment := fmt.Sprintf("server-test-%d", time.Now().UnixNano())
	body := fmt.Sprintf(`{"events":[{"user_id":"u1","segment":%q}]}`, segment)

	response, err := http.Post(base+"/v1/events", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("POST status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}

	response, err = http.Get(base + "/v1/segments/" + segment + "/estimate")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	var estimate struct {
		Estimate uint64 `json:"estimate"`
	}
	if err := json.NewDecoder(response.Body).Decode(&estimate); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if estimate.Estimate != 1 {
		t.Errorf("estimate = %d, want 1", estimate.Estimate)
	}
}

type nopStore struct{}

func (nopStore) Add(context.Context, estimator.Bucket, []estimator.Event) error { return nil }
func (nopStore) Count(context.Context, string, []estimator.Bucket) (uint64, error) {
	return 0, nil
}
func (nopStore) Ping(context.Context) error { return nil }

func TestNewHandlerUsesReleaseMode(t *testing.T) {
	previous := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(previous) })
	cfg, err := loadConfig(lookupFrom(nil))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	newHandler(cfg, nopStore{})

	if gin.Mode() != gin.ReleaseMode {
		t.Errorf("gin mode = %q, want %q", gin.Mode(), gin.ReleaseMode)
	}
}

func TestNewServerSetsTimeouts(t *testing.T) {
	cfg, err := loadConfig(lookupFrom(map[string]string{
		"HTTP_READ_HEADER_TIMEOUT": "1s",
		"HTTP_READ_TIMEOUT":        "2s",
		"HTTP_WRITE_TIMEOUT":       "3s",
		"HTTP_IDLE_TIMEOUT":        "4s",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	server := newServer(cfg, http.NewServeMux())

	if server.ReadHeaderTimeout != 1*time.Second {
		t.Errorf("ReadHeaderTimeout = %s", server.ReadHeaderTimeout)
	}
	if server.ReadTimeout != 2*time.Second {
		t.Errorf("ReadTimeout = %s", server.ReadTimeout)
	}
	if server.WriteTimeout != 3*time.Second {
		t.Errorf("WriteTimeout = %s", server.WriteTimeout)
	}
	if server.IdleTimeout != 4*time.Second {
		t.Errorf("IdleTimeout = %s", server.IdleTimeout)
	}
}

// blockingServer has a handler that waits until release is closed. It closes started
// when the request has arrived.
func blockingServer(t *testing.T) (server *http.Server, listener net.Listener, started, release chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	started, release = make(chan struct{}), make(chan struct{})
	server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusTeapot)
	})}
	return server, listener, started, release
}

func TestServeFinishesInFlightRequestBeforeReturning(t *testing.T) {
	server, listener, started, release := blockingServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, server, listener, 10*time.Second) }()

	status := make(chan int, 1)
	go func() {
		response, err := http.Get("http://" + listener.Addr().String())
		if err != nil {
			t.Errorf("get: %v", err)
			status <- 0
			return
		}
		defer response.Body.Close()
		io.Copy(io.Discard, response.Body)
		status <- response.StatusCode
	}()
	<-started

	cancel()
	select {
	case err := <-served:
		t.Fatalf("serve returned %v while a request was in flight", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if got := <-status; got != http.StatusTeapot {
		t.Errorf("status = %d, want %d", got, http.StatusTeapot)
	}
	if err := <-served; err != nil {
		t.Errorf("serve = %v, want nil", err)
	}
}

func TestServeCutsOffRequestAfterShutdownTimeout(t *testing.T) {
	server, listener, started, release := blockingServer(t)
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, server, listener, 50*time.Millisecond) }()

	failed := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			response.Body.Close()
		}
		failed <- err
	}()
	<-started

	cancel()
	if err := <-served; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("serve = %v, want %v", err, context.DeadlineExceeded)
	}
	if err := <-failed; err == nil {
		t.Error("the request succeeded, want it cut off")
	}
}

func TestServeReturnsServeError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	listener.Close()

	err = serve(context.Background(), &http.Server{}, listener, time.Second)

	if !errors.Is(err, net.ErrClosed) {
		t.Errorf("serve = %v, want %v", err, net.ErrClosed)
	}
}
