package main

import (
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/taghad/go-challenge/internal/estimator"
	"github.com/taghad/go-challenge/internal/redisstore"
)

// config holds everything ES reads from the environment. Every default is an
// estimate [E] until a test validates it.
type config struct {
	httpAddr            string
	readHeaderTimeout   time.Duration
	readTimeout         time.Duration
	writeTimeout        time.Duration
	idleTimeout         time.Duration
	shutdownTimeout     time.Duration
	redis               redisstore.ClientConfig
	redisOpTimeout      time.Duration
	window              estimator.Window
	estimateCacheTTL    time.Duration
	maxInflightIngest   int
	maxInflightEstimate int
	maxBodyBytes        int64
}

// loadConfig reads the configuration through lookup, which has the signature of
// os.LookupEnv. An unset variable takes its default. A variable that is set must hold
// a valid, positive value, so a set but empty one is an error too.
//
// It reports every invalid variable at once, so a bad deployment is fixed in one try.
func loadConfig(lookup func(string) (string, bool)) (config, error) {
	r := envReader{lookup: lookup}
	cfg := config{
		httpAddr:          r.addr("HTTP_ADDR", ":8080"),
		readHeaderTimeout: r.duration("HTTP_READ_HEADER_TIMEOUT", "5s"),
		readTimeout:       r.duration("HTTP_READ_TIMEOUT", "10s"),
		writeTimeout:      r.duration("HTTP_WRITE_TIMEOUT", "10s"),
		idleTimeout:       r.duration("HTTP_IDLE_TIMEOUT", "60s"),
		shutdownTimeout:   r.duration("SHUTDOWN_TIMEOUT", "10s"),
		redis: redisstore.ClientConfig{
			Addr:         r.addr("REDIS_ADDR", "redis:6379"),
			DialTimeout:  r.duration("REDIS_DIAL_TIMEOUT", "500ms"),
			ReadTimeout:  r.duration("REDIS_READ_TIMEOUT", "250ms"),
			WriteTimeout: r.duration("REDIS_WRITE_TIMEOUT", "250ms"),
			PoolTimeout:  r.duration("REDIS_POOL_TIMEOUT", "100ms"),
		},
		redisOpTimeout:      r.duration("REDIS_OP_TIMEOUT", "500ms"),
		estimateCacheTTL:    r.duration("ESTIMATE_CACHE_TTL", "30s"),
		maxInflightIngest:   r.positiveInt("MAX_INFLIGHT_INGEST", "256"),
		maxInflightEstimate: r.positiveInt("MAX_INFLIGHT_ESTIMATE", "32"),
		maxBodyBytes:        r.byteSize("MAX_BODY_BYTES", "512KiB"),
	}
	// The default of 1h comes from the decision rule and measurements in docs/DESIGN.md.
	if size := r.duration("BUCKET_SIZE", "1h"); size > 0 {
		window, err := estimator.NewWindow(size)
		if err != nil {
			r.fail("BUCKET_SIZE", size.String(), err)
		}
		cfg.window = window
	}
	if err := errors.Join(r.errs...); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// envReader reads variables and collects what is wrong with them. A variable that
// fails reads as the zero value, which is never used because loadConfig returns the
// collected errors.
type envReader struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (r *envReader) fail(name, value string, err error) {
	r.errs = append(r.errs, fmt.Errorf("%s=%q: %w", name, value, err))
}

func (r *envReader) value(name, fallback string) string {
	if value, ok := r.lookup(name); ok {
		return value
	}
	return fallback
}

// addr reads a host:port address. The host can be empty, which means all interfaces
// when listening. It is not checked further, because a name may not resolve yet at
// startup. Port 0 is rejected as a non-positive value.
func (r *envReader) addr(name, fallback string) string {
	value := r.value(name, fallback)
	_, port, err := net.SplitHostPort(value)
	if err != nil {
		r.fail(name, value, errors.New("must be host:port"))
		return ""
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		r.fail(name, value, errors.New("port must be from 1 to 65535"))
		return ""
	}
	return value
}

func (r *envReader) duration(name, fallback string) time.Duration {
	value := r.value(name, fallback)
	d, err := time.ParseDuration(value)
	if err != nil {
		r.fail(name, value, errors.New("must be a duration such as 500ms or 6h"))
		return 0
	}
	if d <= 0 {
		r.fail(name, value, errors.New("must be positive"))
		return 0
	}
	return d
}

func (r *envReader) positiveInt(name, fallback string) int {
	value := r.value(name, fallback)
	n, err := strconv.Atoi(value)
	if err != nil {
		r.fail(name, value, errors.New("must be an integer"))
		return 0
	}
	if n <= 0 {
		r.fail(name, value, errors.New("must be positive"))
		return 0
	}
	return n
}

// byteSize reads a size in bytes, with an optional KiB or MiB suffix.
func (r *envReader) byteSize(name, fallback string) int64 {
	value := r.value(name, fallback)
	digits, unit := value, int64(1)
	if trimmed, ok := strings.CutSuffix(value, "KiB"); ok {
		digits, unit = trimmed, 1<<10
	} else if trimmed, ok := strings.CutSuffix(value, "MiB"); ok {
		digits, unit = trimmed, 1<<20
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		r.fail(name, value, errors.New("must be an integer, with an optional KiB or MiB suffix"))
		return 0
	}
	if n <= 0 {
		r.fail(name, value, errors.New("must be positive"))
		return 0
	}
	if n > math.MaxInt64/unit {
		r.fail(name, value, errors.New("is too large"))
		return 0
	}
	return n * unit
}
