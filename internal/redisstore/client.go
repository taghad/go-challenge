package redisstore

import (
	"time"

	"github.com/redis/go-redis/v9"
)

// ClientConfig holds the Redis address and connection timeouts. Timeouts are explicit
// so that a slow Redis fails fast instead of holding requests open.
type ClientConfig struct {
	Addr         string
	DialTimeout  time.Duration // opening a connection
	ReadTimeout  time.Duration // reading a reply
	WriteTimeout time.Duration // writing a command
	PoolTimeout  time.Duration // waiting for a free connection
}

// NewClient returns a client for the Store.
//
// MaxRetries is -1 because USS retries, and client retries would amplify overload. It
// is not 0, because go-redis v9.23.0 turns 0 into its default of 3 retries and only -1
// into none (options.go).
//
// ContextTimeoutEnabled is true because otherwise go-redis v9.23.0 replaces the
// caller's context with context.Background() (baseClient.context in redis.go), which
// would ignore the operation deadline set by the Store.
func NewClient(config ClientConfig) redis.UniversalClient {
	return redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:                 []string{config.Addr},
		DialTimeout:           config.DialTimeout,
		ReadTimeout:           config.ReadTimeout,
		WriteTimeout:          config.WriteTimeout,
		PoolTimeout:           config.PoolTimeout,
		MaxRetries:            -1,
		ContextTimeoutEnabled: true,
	})
}
