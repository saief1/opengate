package budget

import (
	"math"
	"sync"
	"time"
)

// RateLimiter is an in-memory token bucket per scope (requests per minute).
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
	rpm    int
}

// NewRateLimiter returns an empty limiter.
func NewRateLimiter() *RateLimiter { return &RateLimiter{buckets: map[string]*bucket{}} }

// Allow consumes one token for key. The bucket holds up to rpm tokens and
// refills at rpm/60 per second. When denied it returns the wait time.
func (r *RateLimiter) Allow(key string, rpm int, now time.Time) (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.buckets[key]
	if !ok || b.rpm != rpm {
		b = &bucket{tokens: float64(rpm), last: now, rpm: rpm}
		r.buckets[key] = b
	}
	rate := float64(rpm) / 60.0
	b.tokens = math.Min(float64(rpm), b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / rate * float64(time.Second))
	return false, wait.Round(time.Second) + time.Second
}
