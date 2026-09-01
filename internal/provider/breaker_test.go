package provider

import (
	"sync"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestBreaker(t *testing.T, cfg BreakerConfig) (*Breaker, *fakeClock) {
	t.Helper()
	b := NewBreaker(cfg)
	c := &fakeClock{t: time.Now()}
	b.now = c.now
	return b, c
}

func TestBreaker_StartsClosed(t *testing.T) {
	b, _ := newTestBreaker(t, BreakerConfig{Threshold: 3, Cooldown: time.Minute})

	if allowed, state := b.Allow(); !allowed || state != BreakerClosed {
		t.Errorf("Allow() = %v, %v; want true, closed", allowed, state)
	}
}

// Only consecutive failures count: an intermittent blip is not an outage.
func TestBreaker_SuccessResetsTheCount(t *testing.T) {
	b, _ := newTestBreaker(t, BreakerConfig{Threshold: 3, Cooldown: time.Minute})

	b.Failure()
	b.Failure()
	b.Success()
	b.Failure()
	b.Failure()

	if allowed, _ := b.Allow(); !allowed {
		t.Error("the circuit tripped on non-consecutive failures")
	}
}

func TestBreaker_TripsAtTheThreshold(t *testing.T) {
	b, _ := newTestBreaker(t, BreakerConfig{Threshold: 3, Cooldown: time.Minute})

	for range 3 {
		b.Failure()
	}

	allowed, state := b.Allow()
	if allowed {
		t.Error("Allow() = true after reaching the threshold")
	}
	if state != BreakerOpen {
		t.Errorf("state = %v, want open", state)
	}
}

// After the cooldown exactly one probe gets through: letting a crowd in would
// put the provider straight back under the load that broke it.
func TestBreaker_HalfOpensAfterTheCooldown(t *testing.T) {
	b, clk := newTestBreaker(t, BreakerConfig{Threshold: 1, Cooldown: 30 * time.Second})

	b.Failure()
	if allowed, _ := b.Allow(); allowed {
		t.Fatal("the circuit did not open")
	}

	clk.add(29 * time.Second)
	if allowed, _ := b.Allow(); allowed {
		t.Error("a probe was let through before the cooldown elapsed")
	}

	clk.add(2 * time.Second)

	allowed, state := b.Allow()
	if !allowed {
		t.Fatal("no probe was let through after the cooldown")
	}
	if state != BreakerHalfOpen {
		t.Errorf("state = %v, want half-open", state)
	}

	// The probe is in flight: nobody else gets through until it reports back.
	if allowed, _ := b.Allow(); allowed {
		t.Error("a second request was let through while a probe was in flight")
	}
}

func TestBreaker_ProbeSuccessCloses(t *testing.T) {
	b, clk := newTestBreaker(t, BreakerConfig{Threshold: 1, Cooldown: time.Second})

	b.Failure()
	clk.add(2 * time.Second)
	if allowed, _ := b.Allow(); !allowed {
		t.Fatal("no probe after the cooldown")
	}

	b.Success()

	if allowed, state := b.Allow(); !allowed || state != BreakerClosed {
		t.Errorf("Allow() = %v, %v; want true, closed after a successful probe", allowed, state)
	}
}

// One failed probe is enough to reopen: the provider had its chance.
func TestBreaker_ProbeFailureReopensImmediately(t *testing.T) {
	b, clk := newTestBreaker(t, BreakerConfig{Threshold: 5, Cooldown: time.Second})

	for range 5 {
		b.Failure()
	}
	clk.add(2 * time.Second)
	if allowed, _ := b.Allow(); !allowed {
		t.Fatal("no probe after the cooldown")
	}

	b.Failure()

	if allowed, state := b.Allow(); allowed || state != BreakerOpen {
		t.Errorf("Allow() = %v, %v; want false, open after a failed probe", allowed, state)
	}
	// And the cooldown restarts from the failed probe, not from the original
	// trip.
	clk.add(500 * time.Millisecond)
	if allowed, _ := b.Allow(); allowed {
		t.Error("the cooldown did not restart after the failed probe")
	}
}

func TestBreaker_State(t *testing.T) {
	b, clk := newTestBreaker(t, BreakerConfig{Threshold: 1, Cooldown: time.Second})

	if got := b.State(); got != BreakerClosed {
		t.Errorf("State() = %v, want closed", got)
	}

	b.Failure()
	if got := b.State(); got != BreakerOpen {
		t.Errorf("State() = %v, want open", got)
	}

	// A circuit whose cooldown elapsed is reported as half-open, not as a
	// stale open that no caller would actually meet.
	clk.add(2 * time.Second)
	if got := b.State(); got != BreakerHalfOpen {
		t.Errorf("State() = %v, want half-open once the cooldown elapsed", got)
	}
}

func TestBreaker_DefaultsAreSane(t *testing.T) {
	b := NewBreaker(BreakerConfig{})
	if b.cfg.Threshold < 1 || b.cfg.Cooldown <= 0 {
		t.Errorf("zero config produced %+v, want usable defaults", b.cfg)
	}
}

// Every request consults the breaker, and they arrive on different goroutines.
func TestBreaker_IsSafeUnderConcurrency(t *testing.T) {
	b, _ := newTestBreaker(t, BreakerConfig{Threshold: 100, Cooldown: time.Minute})

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if allowed, _ := b.Allow(); allowed {
				b.Failure()
			}
			_ = b.State()
		}()
	}
	wg.Wait()

	if allowed, _ := b.Allow(); allowed {
		t.Error("100 concurrent failures did not trip a threshold of 100")
	}
}

func TestBreakerState_String(t *testing.T) {
	for state, want := range map[BreakerState]string{
		BreakerClosed:   "closed",
		BreakerOpen:     "open",
		BreakerHalfOpen: "half-open",
	} {
		if got := state.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}
