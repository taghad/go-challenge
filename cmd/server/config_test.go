package main

import (
	"strings"
	"testing"
	"time"

	"github.com/taghad/go-challenge/internal/estimator"
)

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(lookupFrom(nil))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	durations := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"readHeaderTimeout", cfg.readHeaderTimeout, 5 * time.Second},
		{"readTimeout", cfg.readTimeout, 10 * time.Second},
		{"writeTimeout", cfg.writeTimeout, 10 * time.Second},
		{"idleTimeout", cfg.idleTimeout, 60 * time.Second},
		{"shutdownTimeout", cfg.shutdownTimeout, 10 * time.Second},
		{"redis dial", cfg.redis.DialTimeout, 500 * time.Millisecond},
		{"redis read", cfg.redis.ReadTimeout, 250 * time.Millisecond},
		{"redis write", cfg.redis.WriteTimeout, 250 * time.Millisecond},
		{"redis pool", cfg.redis.PoolTimeout, 100 * time.Millisecond},
		{"redis op", cfg.redisOpTimeout, 500 * time.Millisecond},
		{"cache TTL", cfg.estimateCacheTTL, 30 * time.Second},
	}
	for _, d := range durations {
		if d.got != d.want {
			t.Errorf("%s = %s, want %s", d.name, d.got, d.want)
		}
	}
	if cfg.httpAddr != ":8080" {
		t.Errorf("httpAddr = %q", cfg.httpAddr)
	}
	if cfg.redis.Addr != "redis:6379" {
		t.Errorf("redis addr = %q", cfg.redis.Addr)
	}
	if cfg.maxInflightIngest != 256 {
		t.Errorf("maxInflightIngest = %d", cfg.maxInflightIngest)
	}
	if cfg.maxInflightEstimate != 32 {
		t.Errorf("maxInflightEstimate = %d", cfg.maxInflightEstimate)
	}
	if cfg.maxBodyBytes != 512*1024 {
		t.Errorf("maxBodyBytes = %d", cfg.maxBodyBytes)
	}
	want, err := estimator.NewWindow(time.Hour)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	if cfg.window != want {
		t.Errorf("window = %+v, want %+v", cfg.window, want)
	}
}

func TestLoadConfigReadsEveryVariable(t *testing.T) {
	cfg, err := loadConfig(lookupFrom(map[string]string{
		"HTTP_ADDR":                "127.0.0.1:9000",
		"HTTP_READ_HEADER_TIMEOUT": "1s",
		"HTTP_READ_TIMEOUT":        "2s",
		"HTTP_WRITE_TIMEOUT":       "3s",
		"HTTP_IDLE_TIMEOUT":        "4s",
		"SHUTDOWN_TIMEOUT":         "5s",
		"REDIS_ADDR":               "other:7000",
		"REDIS_DIAL_TIMEOUT":       "6s",
		"REDIS_READ_TIMEOUT":       "7s",
		"REDIS_WRITE_TIMEOUT":      "8s",
		"REDIS_POOL_TIMEOUT":       "9s",
		"REDIS_OP_TIMEOUT":         "11s",
		"ESTIMATE_CACHE_TTL":       "12s",
		"MAX_INFLIGHT_INGEST":      "13",
		"MAX_INFLIGHT_ESTIMATE":    "14",
		"MAX_BODY_BYTES":           "15",
		"BUCKET_SIZE":              "6h",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	durations := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"readHeaderTimeout", cfg.readHeaderTimeout, 1 * time.Second},
		{"readTimeout", cfg.readTimeout, 2 * time.Second},
		{"writeTimeout", cfg.writeTimeout, 3 * time.Second},
		{"idleTimeout", cfg.idleTimeout, 4 * time.Second},
		{"shutdownTimeout", cfg.shutdownTimeout, 5 * time.Second},
		{"redis dial", cfg.redis.DialTimeout, 6 * time.Second},
		{"redis read", cfg.redis.ReadTimeout, 7 * time.Second},
		{"redis write", cfg.redis.WriteTimeout, 8 * time.Second},
		{"redis pool", cfg.redis.PoolTimeout, 9 * time.Second},
		{"redis op", cfg.redisOpTimeout, 11 * time.Second},
		{"cache TTL", cfg.estimateCacheTTL, 12 * time.Second},
	}
	for _, d := range durations {
		if d.got != d.want {
			t.Errorf("%s = %s, want %s", d.name, d.got, d.want)
		}
	}
	if cfg.httpAddr != "127.0.0.1:9000" {
		t.Errorf("httpAddr = %q", cfg.httpAddr)
	}
	if cfg.redis.Addr != "other:7000" {
		t.Errorf("redis addr = %q", cfg.redis.Addr)
	}
	if cfg.maxInflightIngest != 13 {
		t.Errorf("maxInflightIngest = %d", cfg.maxInflightIngest)
	}
	if cfg.maxInflightEstimate != 14 {
		t.Errorf("maxInflightEstimate = %d", cfg.maxInflightEstimate)
	}
	if cfg.maxBodyBytes != 15 {
		t.Errorf("maxBodyBytes = %d", cfg.maxBodyBytes)
	}
	want, err := estimator.NewWindow(6 * time.Hour)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	if cfg.window != want {
		t.Errorf("window = %+v, want %+v", cfg.window, want)
	}
}

func TestLoadConfigByteSizeSuffixes(t *testing.T) {
	tests := []struct {
		value string
		want  int64
	}{
		{"1", 1},
		{"1KiB", 1024},
		{"2MiB", 2 << 20},
	}
	for _, tt := range tests {
		cfg, err := loadConfig(lookupFrom(map[string]string{"MAX_BODY_BYTES": tt.value}))
		if err != nil {
			t.Errorf("%q: %v", tt.value, err)
			continue
		}
		if cfg.maxBodyBytes != tt.want {
			t.Errorf("%q = %d, want %d", tt.value, cfg.maxBodyBytes, tt.want)
		}
	}
}

// Each variable must reject its own invalid values, and the error must name it.
func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	durations := []string{
		"HTTP_READ_HEADER_TIMEOUT", "HTTP_READ_TIMEOUT", "HTTP_WRITE_TIMEOUT",
		"HTTP_IDLE_TIMEOUT", "SHUTDOWN_TIMEOUT", "REDIS_DIAL_TIMEOUT",
		"REDIS_READ_TIMEOUT", "REDIS_WRITE_TIMEOUT", "REDIS_POOL_TIMEOUT",
		"REDIS_OP_TIMEOUT", "ESTIMATE_CACHE_TTL", "BUCKET_SIZE",
	}
	cases := map[string][]string{
		"HTTP_ADDR":             {"", "8080", "host:", ":0", ":-1", ":65536", ":http"},
		"REDIS_ADDR":            {"", "redis", "redis:", "redis:0", "redis:65536"},
		"MAX_INFLIGHT_INGEST":   {"", "0", "-1", "1.5", "many"},
		"MAX_INFLIGHT_ESTIMATE": {"", "0", "-1", "1.5", "many"},
		"MAX_BODY_BYTES":        {"", "0", "-1", "0KiB", "-1KiB", "KiB", "1GiB", "1.5KiB", "9007199254740992MiB"},
	}
	for _, name := range durations {
		cases[name] = []string{"", "0", "0s", "-1s", "5", "soon"}
	}
	// These are valid durations but do not divide 14 days into whole seconds.
	cases["BUCKET_SIZE"] = append(cases["BUCKET_SIZE"], "5h", "1500ms")

	for name, values := range cases {
		for _, value := range values {
			t.Run(name+"="+value, func(t *testing.T) {
				_, err := loadConfig(lookupFrom(map[string]string{name: value}))
				if err == nil {
					t.Fatal("loadConfig returned no error")
				}
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error %q does not name %s", err, name)
				}
			})
		}
	}
}

func TestLoadConfigReportsEveryInvalidVariable(t *testing.T) {
	_, err := loadConfig(lookupFrom(map[string]string{
		"HTTP_READ_TIMEOUT":   "-1s",
		"MAX_INFLIGHT_INGEST": "0",
		"BUCKET_SIZE":         "5h",
	}))
	if err == nil {
		t.Fatal("loadConfig returned no error")
	}
	for _, name := range []string{"HTTP_READ_TIMEOUT", "MAX_INFLIGHT_INGEST", "BUCKET_SIZE"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
}
