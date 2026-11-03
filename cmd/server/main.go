// Command server runs the Estimation Service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/taghad/go-challenge/internal/estimator"
	"github.com/taghad/go-challenge/internal/httpapi"
	"github.com/taghad/go-challenge/internal/redisstore"
)

func main() {
	cfg, err := loadConfig(os.LookupEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration:\n%v\n", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, cfg); err != nil {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}

// run serves until ctx ends. It does not wait for Redis: the client connects on first
// use, and until then /readyz fails, so a Redis outage at startup is handled like any
// other Redis outage.
func run(ctx context.Context, cfg config) error {
	client := redisstore.NewClient(cfg.redis)
	defer client.Close()

	server := newServer(cfg, newHandler(cfg, redisstore.New(client, cfg.redisOpTimeout)))
	listener, err := net.Listen("tcp", cfg.httpAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	slog.Info("server started", "addr", listener.Addr().String())
	return serve(ctx, server, listener, cfg.shutdownTimeout)
}

// newHandler sets release mode before the router is built, because gin prints its
// debug output while routes are registered.
func newHandler(cfg config, store estimator.Store) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	service := estimator.NewService(store, cfg.window, time.Now, cfg.estimateCacheTTL, cfg.maxInflightEstimate)
	return httpapi.NewRouter(service, httpapi.Config{
		MaxBodyBytes:      cfg.maxBodyBytes,
		MaxInflightIngest: cfg.maxInflightIngest,
	})
}

func newServer(cfg config, handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: cfg.readHeaderTimeout,
		ReadTimeout:       cfg.readTimeout,
		WriteTimeout:      cfg.writeTimeout,
		IdleTimeout:       cfg.idleTimeout,
	}
}

// serve runs the server until ctx ends, then lets in-flight requests finish. It uses
// its own context for the shutdown, because ctx has already ended by then. Requests
// still running after shutdownTimeout are cut off and serve returns an error.
func serve(ctx context.Context, server *http.Server, listener net.Listener, shutdownTimeout time.Duration) error {
	failed := make(chan error, 1)
	go func() { failed <- server.Serve(listener) }()

	select {
	case err := <-failed:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return errors.Join(fmt.Errorf("shutdown: %w", err), server.Close())
	}
	return nil
}
