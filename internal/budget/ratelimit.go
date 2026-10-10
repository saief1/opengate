package budget

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/saief1/opengate/internal/store"
)

// RateStore is a pluggable token-bucket backend (requests per minute).
// Implementations: MemoryRates (single instance, lost on restart) and
// DBRates (SQLite/PostgreSQL, survives restarts and is shared by every
// OpenGate replica pointing at the same PostgreSQL database).
type RateStore interface {
	// Allow consumes one token for key. The bucket holds up to rpm tokens and
	// refills at rpm/60 per second. When denied it returns the wait time.
	Allow(ctx context.Context, key string, rpm int, now time.Time) (bool, time.Duration, error)
}

// MemoryRates is an in-process token bucket store.
type MemoryRates struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
	rpm    int
}

// NewRateLimiter returns an empty in-memory limiter.
func NewRateLimiter() *MemoryRates { return &MemoryRates{buckets: map[string]*bucket{}} }

// Allow implements RateStore.
func (r *MemoryRates) Allow(_ context.Context, key string, rpm int, now time.Time) (bool, time.Duration, error) {
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
		return true, 0, nil
	}
	return false, waitFor(b.tokens, rate), nil
}

func waitFor(tokens, ratePerSec float64) time.Duration {
	wait := time.Duration((1 - tokens) / ratePerSec * float64(time.Second))
	return wait.Round(time.Second) + time.Second
}

// DBRates keeps buckets in the database with a single atomic upsert per
// request, so state survives restarts and is shared across replicas.
type DBRates struct{ st *store.Store }

// NewDBRates returns a database-backed RateStore.
func NewDBRates(st *store.Store) *DBRates { return &DBRates{st: st} }

// Allow implements RateStore.
func (d *DBRates) Allow(ctx context.Context, key string, rpm int, now time.Time) (bool, time.Duration, error) {
	rate := float64(rpm) / 60.0
	ok, tokens, err := d.st.TakeToken(ctx, key, float64(rpm), rate/1000.0, now.UnixMilli())
	if err != nil || ok {
		return ok, 0, err
	}
	return false, waitFor(tokens, rate), nil
}
