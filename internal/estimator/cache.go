package estimator

import (
	"sync"
	"time"
)

// maxCacheEntries limits the cache to 10,000 segments [E]. Any caller can query an
// arbitrary valid segment name, so without a limit those queries could grow the
// cache without bound.
const maxCacheEntries = 10000

type cacheEntry struct {
	bucket   Bucket
	estimate Estimate
}

// estimateCache holds the last estimate of each segment.
//
// It uses sync.RWMutex because most requests are hits, which only read, so they do
// not wait for one another.
type estimateCache struct {
	ttl     time.Duration
	mu      sync.RWMutex
	entries map[string]cacheEntry
}

func newEstimateCache(ttl time.Duration) *estimateCache {
	return &estimateCache{ttl: ttl, entries: make(map[string]cacheEntry)}
}

// get returns a segment's estimate if its entry is still valid at now, in the given
// current bucket.
//
// An entry is valid only while its computed bucket is the current bucket and its age
// is below the TTL. The bucket check is what protects the expiry guarantee: the TTL
// alone would keep serving an expired bucket's users after a boundary. Within a
// bucket the set of keys is fixed and only grows, so a cached value can only miss
// recent events. The age comes from the AsOf of the estimate, so it is measured with
// the clock injected into the service.
func (c *estimateCache) get(segment string, current Bucket, now time.Time) (Estimate, bool) {
	c.mu.RLock()
	entry, ok := c.entries[segment]
	c.mu.RUnlock()
	if !ok || entry.bucket != current || now.Sub(entry.estimate.AsOf) >= c.ttl {
		return Estimate{}, false
	}
	return entry.estimate, true
}

// put stores a segment's estimate. When a new segment finds the cache full, the whole
// cache is cleared: this needs no eviction bookkeeping, and the only cost is
// recomputations, which singleflight and the estimate limit already bound. Replacing
// a segment does not grow the cache, so it clears nothing.
//
// A flight that finishes after a boundary can replace a newer entry. The bucket check
// rejects what it stored, so the cost is one extra recomputation.
func (c *estimateCache) put(segment string, bucket Bucket, estimate Estimate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[segment]; !exists && len(c.entries) >= maxCacheEntries {
		clear(c.entries)
	}
	c.entries[segment] = cacheEntry{bucket: bucket, estimate: estimate}
}
