package ratelimit

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

func testConfig() Config {
	// Two per second, room for five at once.
	return Config{Rate: 2, Burst: 5}
}

// clock lets a test move time without sleeping through it.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(t *testing.T, cfg Config) (*Memory, *clock) {
	t.Helper()
	m, err := NewMemory(cfg)
	if err != nil {
		t.Fatalf("NewMemory() failed: %v", err)
	}
	c := &clock{t: time.Now()}
	m.now = c.now
	return m, c
}

func TestConfig_Rejects(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"zero rate", Config{Rate: 0, Burst: 5}},
		{"negative rate", Config{Rate: -1, Burst: 5}},
		{"zero burst", Config{Rate: 1, Burst: 0}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewMemory(tt.cfg); !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("error = %v, want ErrInvalidConfig: this bucket could never allow anything", err)
			}
		})
	}
}

// A caller's first request must not be the one that gets refused.
func TestMemory_StartsFull(t *testing.T) {
	m, _ := newTestLimiter(t, testConfig())

	for i := range testConfig().Burst {
		d, err := m.Allow(context.Background(), "alice")
		if err != nil {
			t.Fatalf("Allow() failed: %v", err)
		}
		if !d.Allowed {
			t.Fatalf("request %d denied, want the full burst allowed from cold", i+1)
		}
	}
}

func TestMemory_DeniesOnceTheBucketIsEmpty(t *testing.T) {
	cfg := testConfig()
	m, _ := newTestLimiter(t, cfg)

	for range cfg.Burst {
		if d, _ := m.Allow(context.Background(), "alice"); !d.Allowed {
			t.Fatal("the burst was refused")
		}
	}

	d, err := m.Allow(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Allow() failed: %v", err)
	}
	if d.Allowed {
		t.Error("a request past the burst was allowed")
	}
	if d.Remaining != 0 {
		t.Errorf("Remaining = %d, want 0", d.Remaining)
	}
	// A retry hint of zero invites a hot loop.
	if d.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v, want a positive hint", d.RetryAfter)
	}
	// Two tokens a second means about half a second for the next one.
	if d.RetryAfter > time.Second {
		t.Errorf("RetryAfter = %v, want roughly 500ms at %v/s", d.RetryAfter, cfg.Rate)
	}
}

// Refill is continuous, not a window that resets: after half a second at two
// per second exactly one more request fits.
func TestMemory_RefillsContinuously(t *testing.T) {
	cfg := testConfig()
	m, clk := newTestLimiter(t, cfg)

	for range cfg.Burst {
		m.Allow(context.Background(), "alice") //nolint:errcheck // drained on purpose
	}
	if d, _ := m.Allow(context.Background(), "alice"); d.Allowed {
		t.Fatal("the bucket was not empty")
	}

	clk.add(500 * time.Millisecond)

	if d, _ := m.Allow(context.Background(), "alice"); !d.Allowed {
		t.Error("no token after half a second at 2/s, want exactly one")
	}
	if d, _ := m.Allow(context.Background(), "alice"); d.Allowed {
		t.Error("a second token after half a second, want only one")
	}
}

// Refill must not accrue past the bucket size, or an idle caller could bank a
// whole day and spend it at once.
func TestMemory_DoesNotAccrueBeyondBurst(t *testing.T) {
	cfg := testConfig()
	m, clk := newTestLimiter(t, cfg)

	for range cfg.Burst {
		m.Allow(context.Background(), "alice") //nolint:errcheck // drained on purpose
	}

	clk.add(time.Hour)

	allowed := 0
	for range cfg.Burst * 3 {
		if d, _ := m.Allow(context.Background(), "alice"); d.Allowed {
			allowed++
		}
	}
	if allowed != cfg.Burst {
		t.Errorf("allowed %d after an hour idle, want the burst size %d", allowed, cfg.Burst)
	}
}

// Buckets are per caller: one noisy client must not starve the rest.
func TestMemory_IsolatesCallers(t *testing.T) {
	cfg := testConfig()
	m, _ := newTestLimiter(t, cfg)

	for range cfg.Burst {
		m.Allow(context.Background(), "noisy") //nolint:errcheck // drained on purpose
	}
	if d, _ := m.Allow(context.Background(), "noisy"); d.Allowed {
		t.Fatal("the noisy caller was not exhausted")
	}

	if d, _ := m.Allow(context.Background(), "quiet"); !d.Allowed {
		t.Error("a second caller was denied because the first spent its allowance")
	}
}

// Every in-flight request goes through Allow, and they arrive on different
// goroutines by construction.
func TestMemory_IsSafeUnderConcurrency(t *testing.T) {
	const goroutines = 50
	cfg := Config{Rate: 1, Burst: 10}
	m, _ := newTestLimiter(t, cfg)

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, err := m.Allow(context.Background(), "shared"); err == nil && d.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// The clock is frozen, so no refill can happen: exactly the burst fits.
	if allowed != cfg.Burst {
		t.Errorf("allowed %d of %d concurrent requests, want exactly the burst %d",
			allowed, goroutines, cfg.Burst)
	}
}

func TestMemory_Close(t *testing.T) {
	m, _ := newTestLimiter(t, testConfig())
	if err := m.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
}

// Buckets are created per caller and, with authentication disabled, the caller
// is an address. Without eviction that map grows for as long as the process
// runs, which makes the limiter a memory leak with a rate limit attached.
func TestMemory_DoesNotGrowWithoutBound(t *testing.T) {
	cfg := Config{Rate: 1000, Burst: 5} // refills fast, so buckets go idle at once
	m, clk := newTestLimiter(t, cfg)

	const callers = maxBuckets + sweepEvery*3
	for i := range callers {
		if _, err := m.Allow(context.Background(), "ip:10.0.0."+strconv.Itoa(i)); err != nil {
			t.Fatalf("Allow() failed: %v", err)
		}
		// Move on enough for the bucket just used to refill completely, which
		// is what makes it safe to forget.
		clk.add(time.Second)
	}

	m.mu.Lock()
	size := len(m.buckets)
	m.mu.Unlock()

	// Sweeps are spaced out, so the map may run up to one interval past the
	// bound before being trimmed. Anything beyond that is unbounded growth.
	if size > maxBuckets+sweepEvery {
		t.Errorf("the limiter holds %d buckets after %d one-shot callers, want at most %d",
			size, callers, maxBuckets+sweepEvery)
	}
}

// Forgetting a bucket must never hand back an allowance that was already
// spent, or a caller could reset its own limit by going quiet for an instant.
func TestMemory_EvictionNeverRefundsAnActiveCaller(t *testing.T) {
	cfg := Config{Rate: 0.001, Burst: 2} // refill slow enough to be irrelevant
	m, _ := newTestLimiter(t, cfg)

	const busy = "alice"
	for range cfg.Burst {
		if d, _ := m.Allow(context.Background(), busy); !d.Allowed {
			t.Fatal("the burst was refused")
		}
	}

	// Flood the limiter with one-shot callers to force eviction.
	for i := range maxBuckets + sweepEvery*2 {
		m.Allow(context.Background(), "ip:10.0.0."+strconv.Itoa(i)) //nolint:errcheck // filling on purpose
	}

	if d, _ := m.Allow(context.Background(), busy); d.Allowed {
		t.Error("an exhausted caller got its allowance back through eviction")
	}
}
