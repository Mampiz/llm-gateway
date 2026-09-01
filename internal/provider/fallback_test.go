package provider

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// scripted is a provider whose every call is answered from a script.
type scripted struct {
	name string

	mu    sync.Mutex
	calls int
	errs  []error // errs[i] answers call i+1; nil means success
}

func (s *scripted) Name() string { return s.name }

func (s *scripted) next() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls <= len(s.errs) {
		return s.errs[s.calls-1]
	}
	return nil
}

func (s *scripted) Chat(_ context.Context, req ChatRequest) (*ChatResponse, error) {
	if err := s.next(); err != nil {
		return nil, err
	}
	return &ChatResponse{ID: s.name, Model: req.Model}, nil
}

func (s *scripted) ChatStream(context.Context, ChatRequest) (Stream, error) {
	if err := s.next(); err != nil {
		return nil, err
	}
	return nil, nil //nolint:nilnil // the fallback tests only care about the error
}

func (s *scripted) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

var _ Provider = (*scripted)(nil)

func retryable() error { return &Error{Provider: "x", StatusCode: 503, Message: "overloaded"} }
func permanent() error { return &Error{Provider: "x", StatusCode: 400, Message: "bad model"} }
func fastPolicy() RetryPolicy {
	return RetryPolicy{Attempts: 2, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}
}

// routerWith wires a primary on gpt- and a secondary on claude-, with the
// former falling back to the latter.
func routerWith(t *testing.T, primary, secondary *scripted) *Router {
	t.Helper()

	reg := NewRegistry()
	reg.Register(primary, "gpt-")
	reg.Register(secondary, "claude-")

	fallbacks, err := ParseFallbacks("gpt-4o-mini:claude-sonnet-5")
	if err != nil {
		t.Fatalf("ParseFallbacks() failed: %v", err)
	}
	return NewRouter(reg, fallbacks, fastPolicy(), BreakerConfig{Threshold: 10, Cooldown: time.Minute})
}

func chatReq() ChatRequest {
	return ChatRequest{Model: "gpt-4o-mini", Messages: []Message{{Role: "user", Content: "hi"}}}
}

func TestRouter_UsesThePrimaryWhenItWorks(t *testing.T) {
	primary := &scripted{name: "openai"}
	secondary := &scripted{name: "anthropic"}

	resp, served, err := routerWith(t, primary, secondary).Chat(context.Background(), chatReq())
	if err != nil {
		t.Fatalf("Chat() failed: %v", err)
	}
	if served != "openai" {
		t.Errorf("served by %q, want openai", served)
	}
	if resp.Model != "gpt-4o-mini" {
		t.Errorf("model = %q, want the requested one untouched", resp.Model)
	}
	if secondary.count() != 0 {
		t.Errorf("the secondary was called %d times, want 0", secondary.count())
	}
}

// A retryable error deserves another try on the same provider before the
// request is handed to a different vendor with a different model.
func TestRouter_RetriesTheSameProviderFirst(t *testing.T) {
	primary := &scripted{name: "openai", errs: []error{retryable()}}
	secondary := &scripted{name: "anthropic"}

	_, served, err := routerWith(t, primary, secondary).Chat(context.Background(), chatReq())
	if err != nil {
		t.Fatalf("Chat() failed: %v", err)
	}
	if served != "openai" {
		t.Errorf("served by %q, want the primary on its second attempt", served)
	}
	if primary.count() != 2 {
		t.Errorf("primary called %d times, want 2", primary.count())
	}
	if secondary.count() != 0 {
		t.Errorf("the secondary was used even though a retry succeeded")
	}
}

// Falling back changes the model as well as the provider: the same model
// rarely exists on two vendors.
func TestRouter_FallsBackAndRewritesTheModel(t *testing.T) {
	primary := &scripted{name: "openai", errs: []error{retryable(), retryable()}}
	secondary := &scripted{name: "anthropic"}

	resp, served, err := routerWith(t, primary, secondary).Chat(context.Background(), chatReq())
	if err != nil {
		t.Fatalf("Chat() failed: %v", err)
	}
	if served != "anthropic" {
		t.Errorf("served by %q, want the fallback", served)
	}
	if resp.Model != "claude-sonnet-5" {
		t.Errorf("model = %q, want it rewritten for the fallback provider", resp.Model)
	}
	if primary.count() != 2 {
		t.Errorf("primary called %d times, want the policy's 2 before moving on", primary.count())
	}
}

// A 400 is the caller's fault. Asking a second vendor the same malformed
// question wastes their time and the caller's.
func TestRouter_DoesNotFallBackOnAClientError(t *testing.T) {
	primary := &scripted{name: "openai", errs: []error{permanent(), permanent()}}
	secondary := &scripted{name: "anthropic"}

	_, _, err := routerWith(t, primary, secondary).Chat(context.Background(), chatReq())
	if err == nil {
		t.Fatal("Chat() succeeded, want the client error surfaced")
	}
	if primary.count() != 1 {
		t.Errorf("primary called %d times, want 1: a 400 is not worth retrying", primary.count())
	}
	if secondary.count() != 0 {
		t.Errorf("the secondary was asked the same malformed question")
	}

	var pErr *Error
	if !errors.As(err, &pErr) || pErr.StatusCode != 400 {
		t.Errorf("error = %v, want the original 400 rather than a wrapped chain failure", err)
	}
}

func TestRouter_ReportsWhenEveryProviderFails(t *testing.T) {
	primary := &scripted{name: "openai", errs: []error{retryable(), retryable()}}
	secondary := &scripted{name: "anthropic", errs: []error{retryable(), retryable()}}

	_, _, err := routerWith(t, primary, secondary).Chat(context.Background(), chatReq())
	if !errors.Is(err, ErrAllProvidersFailed) {
		t.Fatalf("error = %v, want ErrAllProvidersFailed", err)
	}
	// The last cause must survive: "everything failed" alone is undebuggable.
	var pErr *Error
	if !errors.As(err, &pErr) {
		t.Errorf("error = %v, want the underlying provider error still reachable", err)
	}
}

// The caller giving up is not the provider's fault and must not spend the
// rest of the chain.
func TestRouter_StopsWhenTheCallerCancels(t *testing.T) {
	primary := &scripted{name: "openai", errs: []error{retryable(), retryable()}}
	secondary := &scripted{name: "anthropic"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := routerWith(t, primary, secondary).Chat(ctx, chatReq())
	if err == nil {
		t.Fatal("Chat() succeeded with a cancelled context")
	}
	if secondary.count() != 0 {
		t.Errorf("the secondary was tried after the caller gave up")
	}
}

// Once a provider is known to be down, requests should not queue behind it.
func TestRouter_SkipsProvidersWithAnOpenCircuit(t *testing.T) {
	primary := &scripted{name: "openai", errs: []error{retryable(), retryable(), retryable(), retryable()}}
	secondary := &scripted{name: "anthropic"}

	reg := NewRegistry()
	reg.Register(primary, "gpt-")
	reg.Register(secondary, "claude-")
	fallbacks, _ := ParseFallbacks("gpt-4o-mini:claude-sonnet-5")
	// Two consecutive failures are enough to trip it, which one call reaches.
	r := NewRouter(reg, fallbacks, fastPolicy(), BreakerConfig{Threshold: 2, Cooldown: time.Minute})

	if _, served, err := r.Chat(context.Background(), chatReq()); err != nil || served != "anthropic" {
		t.Fatalf("first call: served=%q err=%v, want the fallback to take over", served, err)
	}
	before := primary.count()

	if _, served, err := r.Chat(context.Background(), chatReq()); err != nil || served != "anthropic" {
		t.Fatalf("second call: served=%q err=%v", served, err)
	}
	if primary.count() != before {
		t.Errorf("the primary was called again with its circuit open: %d then %d", before, primary.count())
	}
	if got := r.Breakers()["openai"]; got != "open" {
		t.Errorf("circuit for openai = %q, want open", got)
	}
}

func TestRouter_StreamFailsOverTheSameWay(t *testing.T) {
	primary := &scripted{name: "openai", errs: []error{retryable(), retryable()}}
	secondary := &scripted{name: "anthropic"}

	_, served, err := routerWith(t, primary, secondary).ChatStream(context.Background(), chatReq())
	if err != nil {
		t.Fatalf("ChatStream() failed: %v", err)
	}
	if served != "anthropic" {
		t.Errorf("served by %q, want the fallback: both paths must fail over identically", served)
	}
}

// A chain naming a model this deployment does not serve should degrade, not
// break the request that could still be served.
func TestRouter_SkipsUnresolvableFallbacks(t *testing.T) {
	primary := &scripted{name: "openai", errs: []error{retryable(), retryable()}}

	reg := NewRegistry()
	reg.Register(primary, "gpt-")
	fallbacks, _ := ParseFallbacks("gpt-4o-mini:llama-3-not-configured")
	r := NewRouter(reg, fallbacks, fastPolicy(), BreakerConfig{Threshold: 10, Cooldown: time.Minute})

	if _, _, err := r.Chat(context.Background(), chatReq()); !errors.Is(err, ErrAllProvidersFailed) {
		t.Errorf("error = %v, want the chain to end cleanly rather than panic on an unknown model", err)
	}
}

func TestParseFallbacks(t *testing.T) {
	got, err := ParseFallbacks(" gpt-4o-mini : claude-sonnet-5 | gpt-4o , claude-sonnet-5:gpt-4o-mini ")
	if err != nil {
		t.Fatalf("ParseFallbacks() failed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d entries, want 2: %v", len(got), got)
	}
	want := []string{"claude-sonnet-5", "gpt-4o"}
	for i, w := range want {
		if got["gpt-4o-mini"][i] != w {
			t.Errorf("fallback %d = %q, want %q in order", i, got["gpt-4o-mini"][i], w)
		}
	}
}

func TestParseFallbacks_Rejects(t *testing.T) {
	for _, spec := range []string{"gpt-4o-mini", "gpt-4o-mini:", ":claude", "gpt-4o-mini:|"} {
		t.Run(spec, func(t *testing.T) {
			if _, err := ParseFallbacks(spec); err == nil {
				t.Errorf("ParseFallbacks(%q) accepted a malformed entry", spec)
			}
		})
	}
}

func TestRetryPolicy_BackoffGrowsAndIsCapped(t *testing.T) {
	p := RetryPolicy{Attempts: 5, BaseDelay: 100 * time.Millisecond, MaxDelay: 300 * time.Millisecond}

	for attempt := 1; attempt <= 5; attempt++ {
		d := p.backoff(attempt)
		if d < 0 {
			t.Fatalf("attempt %d: backoff = %v, want non-negative", attempt, d)
		}
		// Full jitter picks anywhere below the ceiling, so the cap is the only
		// property that can be asserted deterministically.
		if d > p.MaxDelay {
			t.Errorf("attempt %d: backoff = %v, want at most %v", attempt, d, p.MaxDelay)
		}
	}
}

// Jitter is the point of the backoff: without it every client that failed at
// the same instant retries at the same instant.
func TestRetryPolicy_BackoffIsJittered(t *testing.T) {
	p := RetryPolicy{Attempts: 3, BaseDelay: time.Second, MaxDelay: time.Second}

	seen := make(map[time.Duration]bool)
	for range 20 {
		seen[p.backoff(1)] = true
	}
	if len(seen) < 2 {
		t.Error("every backoff was identical, want jitter")
	}
}

func TestErrAllProvidersFailed_MentionsSkippedCircuits(t *testing.T) {
	primary := &scripted{name: "openai"}
	reg := NewRegistry()
	reg.Register(primary, "gpt-")
	r := NewRouter(reg, nil, fastPolicy(), BreakerConfig{Threshold: 1, Cooldown: time.Minute})

	// Trip the circuit directly, then confirm the message names it.
	r.breakerFor("openai").Failure()

	_, _, err := r.Chat(context.Background(), chatReq())
	if err == nil {
		t.Fatal("Chat() succeeded with the only circuit open")
	}
	if !strings.Contains(err.Error(), "circuit") {
		t.Errorf("error = %q, want it to say why nothing was tried", err)
	}
}
