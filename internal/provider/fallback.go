package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

// RetryPolicy governs how hard one provider is tried before moving on.
type RetryPolicy struct {
	// Attempts is the total number of tries per provider, including the
	// first. One means no retrying.
	Attempts int

	// BaseDelay is the wait before the second attempt. Each further attempt
	// doubles it.
	BaseDelay time.Duration

	// MaxDelay caps the backoff, so a long chain cannot make a client wait
	// minutes for a decision.
	MaxDelay time.Duration
}

// DefaultRetryPolicy is deliberately modest: a gateway that retries hard turns
// one struggling upstream into a self-inflicted denial of service.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{Attempts: 2, BaseDelay: 200 * time.Millisecond, MaxDelay: 2 * time.Second}
}

// backoff reports how long to wait before the given attempt, counting from 1.
//
// Jitter matters more than the exponential curve: without it, every client
// that failed at the same instant retries at the same instant, and the
// upstream is hit by a synchronised wave exactly when it is least able to
// take one.
func (p RetryPolicy) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := float64(p.BaseDelay) * math.Pow(2, float64(attempt-1))
	if max := float64(p.MaxDelay); p.MaxDelay > 0 && d > max {
		d = max
	}
	// Full jitter: anywhere in [0, d).
	return time.Duration(rand.Float64() * d) //nolint:gosec // scheduling, not security
}

// Attempt is one provider-and-model pair to try.
type Attempt struct {
	Provider Provider
	Model    string
}

// Router turns a requested model into the ordered list of attempts that may
// serve it, and remembers which providers are currently unwell.
//
// It is the piece that makes fallback possible without any provider knowing
// another exists.
type Router struct {
	reg    *Registry
	policy RetryPolicy
	bcfg   BreakerConfig

	// fallbacks maps a requested model to the models to try after it, in
	// order. Falling back means changing model as well as provider: the same
	// model rarely exists on two vendors.
	fallbacks map[string][]string

	mu       sync.Mutex
	breakers map[string]*Breaker
}

// NewRouter builds a router over a registry.
func NewRouter(reg *Registry, fallbacks map[string][]string, policy RetryPolicy, bcfg BreakerConfig) *Router {
	if policy.Attempts < 1 {
		policy = DefaultRetryPolicy()
	}
	return &Router{
		reg:       reg,
		policy:    policy,
		bcfg:      bcfg,
		fallbacks: fallbacks,
		breakers:  make(map[string]*Breaker),
	}
}

// ParseFallbacks reads a specification of the form
//
//	gpt-4o-mini:claude-sonnet-5,claude-sonnet-5:gpt-4o-mini|gpt-4o
//
// where each entry maps a requested model to a pipe-separated ordered list of
// models to fall back to.
func ParseFallbacks(spec string) (map[string][]string, error) {
	out := make(map[string][]string)

	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		model, rest, ok := strings.Cut(entry, ":")
		model = strings.TrimSpace(model)
		if !ok || model == "" || strings.TrimSpace(rest) == "" {
			return nil, fmt.Errorf("malformed fallback entry %q: want model:fallback[|fallback]", entry)
		}

		var chain []string
		for _, f := range strings.Split(rest, "|") {
			if f = strings.TrimSpace(f); f != "" {
				chain = append(chain, f)
			}
		}
		if len(chain) == 0 {
			return nil, fmt.Errorf("fallback entry %q lists no targets", entry)
		}
		out[model] = chain
	}
	return out, nil
}

// breakerFor returns the circuit for a provider, creating it on first use.
func (r *Router) breakerFor(name string) *Breaker {
	r.mu.Lock()
	defer r.mu.Unlock()

	b, ok := r.breakers[name]
	if !ok {
		b = NewBreaker(r.bcfg)
		r.breakers[name] = b
	}
	return b
}

// Breakers reports the state of every circuit seen so far, for /healthz and
// metrics.
func (r *Router) Breakers() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make(map[string]string, len(r.breakers))
	for name, b := range r.breakers {
		out[name] = b.State().String()
	}
	return out
}

// attemptsFor resolves the requested model and its fallbacks into providers.
// Unresolvable fallbacks are skipped rather than fatal: a chain that names a
// model this deployment does not serve should degrade, not break.
func (r *Router) attemptsFor(model string) ([]Attempt, error) {
	p, err := r.reg.For(model)
	if err != nil {
		return nil, err
	}

	attempts := []Attempt{{Provider: p, Model: model}}
	for _, alt := range r.fallbacks[model] {
		if altProvider, err := r.reg.For(alt); err == nil {
			attempts = append(attempts, Attempt{Provider: altProvider, Model: alt})
		}
	}
	return attempts, nil
}

// ErrAllProvidersFailed reports that every attempt was exhausted.
var ErrAllProvidersFailed = errors.New("every provider failed")

// Do runs fn against each attempt in turn until one succeeds.
//
// An attempt is retried in place while its error is retryable and the policy
// allows, then the next provider is tried. A non-retryable error stops
// everything: a 400 is the caller's fault and asking a second vendor the same
// malformed question only wastes their time and the caller's.
//
// The generic parameter keeps one implementation serving both the buffered and
// the streaming paths, which must fail over identically or the two diverge.
func do[T any](ctx context.Context, r *Router, req ChatRequest, fn func(context.Context, Provider, ChatRequest) (T, error)) (T, string, error) {
	var zero T

	attempts, err := r.attemptsFor(req.Model)
	if err != nil {
		return zero, "", err
	}

	var lastErr error
	var skipped []string

	for _, attempt := range attempts {
		name := attempt.Provider.Name()
		breaker := r.breakerFor(name)

		allowed, state := breaker.Allow()
		if !allowed {
			skipped = append(skipped, fmt.Sprintf("%s (circuit %s)", name, state))
			continue
		}

		call := req
		call.Model = attempt.Model

		for try := 1; try <= r.policy.Attempts; try++ {
			result, err := fn(ctx, attempt.Provider, call)
			if err == nil {
				breaker.Success()
				return result, name, nil
			}
			lastErr = err

			// The caller giving up is not the provider's failure, and must not
			// count against its circuit or trigger a fallback.
			if ctx.Err() != nil {
				return zero, name, err
			}

			var pErr *Error
			retryable := errors.As(err, &pErr) && pErr.Retryable()
			if !retryable {
				// A rejection on our side, not an outage. Do not blame the
				// provider and do not ask anyone else the same question.
				return zero, name, err
			}

			breaker.Failure()

			if try == r.policy.Attempts {
				break
			}
			select {
			case <-time.After(r.policy.backoff(try)):
			case <-ctx.Done():
				return zero, name, ctx.Err()
			}
		}
	}

	if lastErr == nil {
		// Every provider was skipped by an open circuit.
		return zero, "", fmt.Errorf("%w: %s", ErrAllProvidersFailed, strings.Join(skipped, ", "))
	}
	return zero, "", fmt.Errorf("%w: %w", ErrAllProvidersFailed, lastErr)
}

// Chat runs a buffered completion through the fallback chain, reporting which
// provider ended up serving it.
func (r *Router) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, string, error) {
	return do(ctx, r, req, func(ctx context.Context, p Provider, req ChatRequest) (*ChatResponse, error) {
		return p.Chat(ctx, req)
	})
}

// ChatStream opens a streamed completion through the fallback chain.
//
// Only the failure to *start* can fail over: once frames have reached the
// client the response is committed, and silently switching vendors mid-answer
// would splice two different completions together.
func (r *Router) ChatStream(ctx context.Context, req ChatRequest) (Stream, string, error) {
	return do(ctx, r, req, func(ctx context.Context, p Provider, req ChatRequest) (Stream, error) {
		return p.ChatStream(ctx, req)
	})
}
