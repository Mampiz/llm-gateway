// Package ratelimit meters how fast each caller may spend the gateway's
// upstream budget.
//
// The algorithm is a token bucket: a caller holds up to Burst tokens, refilled
// continuously at Rate tokens per second, and every request costs one. Bursts
// are absorbed up to the bucket's size while the sustained rate stays capped,
// which is what a fixed window per minute cannot do -- there, a caller can
// spend a whole minute's allowance in the last second of one window and again
// in the first second of the next.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Decision is the outcome of one metering call.
type Decision struct {
	// Allowed reports whether the request may proceed.
	Allowed bool

	// Limit is the bucket size, reported to clients as X-RateLimit-Limit.
	Limit int

	// Remaining is how many whole tokens are left after this call.
	Remaining int

	// RetryAfter is how long until one token is available again. Zero when
	// the request was allowed.
	RetryAfter time.Duration
}

// Limiter meters one caller at a time.
//
// Allow must be safe for concurrent use: every in-flight request goes through
// it, and they arrive on different goroutines by construction.
type Limiter interface {
	// Allow consumes one token for key and reports what happened. An error
	// means the limiter itself failed, which is different from a denial: the
	// caller decides whether to fail open or closed.
	Allow(ctx context.Context, key string) (Decision, error)

	// Close releases whatever the limiter holds.
	Close() error
}

// Config describes a bucket.
type Config struct {
	// Rate is the sustained allowance in requests per second.
	Rate float64

	// Burst is the bucket size: the most a caller may spend at once after
	// being idle. Must be at least 1 or nothing is ever allowed.
	Burst int

	// TTL is how long an idle bucket is remembered. It only matters for the
	// distributed limiter, where forgetting is what keeps Redis from growing
	// one key per caller forever.
	TTL time.Duration
}

// ErrInvalidConfig reports a bucket that could never allow anything.
var ErrInvalidConfig = errors.New("invalid rate limit configuration")

func (c Config) validate() error {
	if c.Rate <= 0 {
		return fmt.Errorf("%w: rate must be positive, got %v", ErrInvalidConfig, c.Rate)
	}
	if c.Burst < 1 {
		return fmt.Errorf("%w: burst must be at least 1, got %d", ErrInvalidConfig, c.Burst)
	}
	return nil
}

// --- in-memory --------------------------------------------------------------

// Memory is a token bucket held in this process.
//
// It is correct for a single instance and useless for several: three replicas
// each allowing the configured rate means three times the intended limit. It
// exists for local development and for tests that should not need a database.
type Memory struct {
	cfg Config

	mu      sync.Mutex
	buckets map[string]*bucket

	// now is swappable so tests can move time without sleeping.
	now func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

var _ Limiter = (*Memory)(nil)

// NewMemory builds an in-process limiter.
func NewMemory(cfg Config) (*Memory, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Memory{
		cfg:     cfg,
		buckets: make(map[string]*bucket),
		now:     time.Now,
	}, nil
}

// Allow implements Limiter.
func (m *Memory) Allow(_ context.Context, key string) (Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()

	b, seen := m.buckets[key]
	if !seen {
		// A caller starts with a full bucket rather than an empty one: the
		// first request must not be the one that gets refused.
		b = &bucket{tokens: float64(m.cfg.Burst), last: now}
		m.buckets[key] = b
	}

	refill(b, m.cfg, now)

	if b.tokens < 1 {
		return Decision{
			Limit:      m.cfg.Burst,
			Remaining:  0,
			RetryAfter: waitFor(b.tokens, m.cfg.Rate),
		}, nil
	}

	b.tokens--
	return Decision{
		Allowed:   true,
		Limit:     m.cfg.Burst,
		Remaining: int(b.tokens),
	}, nil
}

// Close implements Limiter. It drops every bucket, which is all this
// implementation holds.
func (m *Memory) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buckets = make(map[string]*bucket)
	return nil
}

// refill adds the tokens that accrued since the bucket was last touched,
// capped at the bucket's size. Continuous refill is what makes this a token
// bucket rather than a fixed window: there is no boundary to game.
func refill(b *bucket, cfg Config, now time.Time) {
	elapsed := now.Sub(b.last).Seconds()
	if elapsed <= 0 {
		return
	}
	b.last = now
	b.tokens += elapsed * cfg.Rate
	if max := float64(cfg.Burst); b.tokens > max {
		b.tokens = max
	}
}

// waitFor reports how long until the bucket holds one whole token.
func waitFor(tokens, rate float64) time.Duration {
	missing := 1 - tokens
	if missing <= 0 {
		return 0
	}
	d := time.Duration(missing / rate * float64(time.Second))
	// Round up: telling a client to retry in 0s when it cannot yet succeed
	// invites a hot loop.
	if d < time.Millisecond {
		d = time.Millisecond
	}
	return d
}
