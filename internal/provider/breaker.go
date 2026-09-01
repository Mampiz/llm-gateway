package provider

import (
	"sync"
	"time"
)

// BreakerState is where a circuit stands.
type BreakerState int

const (
	// BreakerClosed is the healthy state: requests flow through.
	BreakerClosed BreakerState = iota

	// BreakerOpen means the provider is failing and is being left alone.
	// Skipping it is the point: hammering a struggling upstream makes its
	// recovery slower, and every doomed attempt costs the caller latency.
	BreakerOpen

	// BreakerHalfOpen lets a single probe through to see whether the provider
	// recovered. One success closes the circuit; one failure reopens it.
	BreakerHalfOpen
)

func (s BreakerState) String() string {
	switch s {
	case BreakerClosed:
		return "closed"
	case BreakerOpen:
		return "open"
	case BreakerHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// BreakerConfig tunes one circuit.
type BreakerConfig struct {
	// Threshold is how many consecutive failures trip the circuit.
	Threshold int

	// Cooldown is how long an open circuit stays open before letting a probe
	// through.
	Cooldown time.Duration
}

// Breaker is a per-provider circuit breaker.
//
// It is safe for concurrent use: every request consults it, and they arrive on
// different goroutines by construction.
type Breaker struct {
	cfg BreakerConfig

	mu         sync.Mutex
	state      BreakerState
	failures   int
	openedAt   time.Time
	probeInUse bool

	// now is swappable so tests can move time without sleeping.
	now func() time.Time
}

// NewBreaker builds a closed circuit. A threshold below 1 or a cooldown of
// zero fall back to values that are conservative rather than surprising.
func NewBreaker(cfg BreakerConfig) *Breaker {
	if cfg.Threshold < 1 {
		cfg.Threshold = 5
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 30 * time.Second
	}
	return &Breaker{cfg: cfg, now: time.Now}
}

// Allow reports whether a request may be attempted, and in which state the
// circuit was when it decided.
//
// A caller that gets true must report the outcome with Success, Failure or
// Release. Half-open lets exactly one probe through, so a caller that takes
// the slot and never reports would strand the circuit: it would refuse every
// request from then on, and a provider that recovered would never come back.
// Release exists for the paths where neither outcome applies.
func (b *Breaker) Allow() (bool, BreakerState) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case BreakerClosed:
		return true, BreakerClosed

	case BreakerOpen:
		if b.now().Sub(b.openedAt) < b.cfg.Cooldown {
			return false, BreakerOpen
		}
		// The cooldown elapsed: promote to half-open and let this caller be
		// the probe.
		b.state = BreakerHalfOpen
		b.probeInUse = true
		return true, BreakerHalfOpen

	case BreakerHalfOpen:
		// Exactly one probe at a time. Letting a crowd through would put the
		// provider straight back under the load that broke it.
		if b.probeInUse {
			return false, BreakerHalfOpen
		}
		b.probeInUse = true
		return true, BreakerHalfOpen

	default:
		return true, b.state
	}
}

// Success records a completed request.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures = 0
	b.probeInUse = false
	b.state = BreakerClosed
}

// Failure records a failed request and trips the circuit once the threshold is
// reached.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.probeInUse = false

	if b.state == BreakerHalfOpen {
		// The probe failed: straight back to open, with a fresh cooldown.
		b.state = BreakerOpen
		b.openedAt = b.now()
		return
	}

	b.failures++
	if b.failures >= b.cfg.Threshold {
		b.state = BreakerOpen
		b.openedAt = b.now()
	}
}

// Release gives back a probe slot without recording an outcome.
//
// It is for the cases where the attempt says nothing about the provider: the
// caller cancelled, or the request was rejected before it could be judged. A
// deferred Release is also what keeps a panic from stranding the circuit.
// Calling it after Success or Failure is a no-op.
func (b *Breaker) Release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probeInUse = false
}

// State reports the current state, for logs and metrics.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Report the state a caller would actually meet, rather than a stale
	// "open" for a circuit whose cooldown has already elapsed.
	if b.state == BreakerOpen && b.now().Sub(b.openedAt) >= b.cfg.Cooldown {
		return BreakerHalfOpen
	}
	return b.state
}
