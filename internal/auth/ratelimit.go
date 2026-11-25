package auth

import (
	"sync"
	"time"
)

// RateLimiter is a per-key token bucket (used for per-token MCP limits).
type RateLimiter struct {
	// Rate is tokens added per second; Burst the bucket size.
	Rate  float64
	Burst float64
	Now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter creates a limiter allowing perMinute calls/minute on average.
func NewRateLimiter(perMinute, burst int) *RateLimiter {
	return &RateLimiter{Rate: float64(perMinute) / 60, Burst: float64(burst), Now: time.Now, buckets: map[string]*bucket{}}
}

// Allow consumes one token; on refusal it returns the wait until the next.
func (r *RateLimiter) Allow(key string) (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.Now()
	b := r.buckets[key]
	if b == nil {
		b = &bucket{tokens: r.Burst, last: now}
		r.buckets[key] = b
		if len(r.buckets) > 10000 { // bound memory
			for k, v := range r.buckets {
				if now.Sub(v.last) > time.Hour {
					delete(r.buckets, k)
				}
			}
		}
	}
	b.tokens = min(r.Burst, b.tokens+now.Sub(b.last).Seconds()*r.Rate)
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / r.Rate * float64(time.Second))
	}
	b.tokens--
	return true, 0
}
