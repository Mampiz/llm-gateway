package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mampiz/llm-gateway/internal/auth"
	"github.com/Mampiz/llm-gateway/internal/provider"
	"github.com/Mampiz/llm-gateway/internal/ratelimit"
)

// limitedHandler builds the full chain with authentication and a limiter.
func limitedHandler(t *testing.T, cfg ratelimit.Config) http.Handler {
	t.Helper()

	limiter, err := ratelimit.NewMemory(cfg)
	if err != nil {
		t.Fatalf("NewMemory() failed: %v", err)
	}

	keys, err := auth.NewStaticStore("alice:" + testSecret)
	if err != nil {
		t.Fatalf("building the key store: %v", err)
	}

	reg := provider.NewRegistry()
	reg.Register(succeedingProvider())
	if err := reg.SetDefault("stub"); err != nil {
		t.Fatalf("registry: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(reg, logger, time.Second, "test").
		WithAuth(keys).
		WithRateLimiter(limiter).
		Handler()
}

func chatAs(h http.Handler, secret string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRateLimit_AllowsTheBurstThenDenies(t *testing.T) {
	cfg := ratelimit.Config{Rate: 0.001, Burst: 3} // refill slow enough to be irrelevant
	h := limitedHandler(t, cfg)

	for i := range cfg.Burst {
		if rec := chatAs(h, testSecret); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 within the burst", i+1, rec.Code)
		}
	}

	rec := chatAs(h, testSecret)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 past the burst (body: %s)", rec.Code, rec.Body)
	}

	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("the 429 body is not the usual envelope: %v", err)
	}
	if env.Error.Type != "rate_limit_error" {
		t.Errorf("error type = %q, want rate_limit_error", env.Error.Type)
	}
}

// A client cannot back off sensibly without being told how long to wait.
func TestRateLimit_AdvertisesTheLimit(t *testing.T) {
	cfg := ratelimit.Config{Rate: 0.001, Burst: 2}
	h := limitedHandler(t, cfg)

	first := chatAs(h, testSecret)
	if got := first.Header().Get("X-RateLimit-Limit"); got != strconv.Itoa(cfg.Burst) {
		t.Errorf("X-RateLimit-Limit = %q, want %d", got, cfg.Burst)
	}
	if got := first.Header().Get("X-RateLimit-Remaining"); got != "1" {
		t.Errorf("X-RateLimit-Remaining = %q, want 1 after one of two", got)
	}

	chatAs(h, testSecret)
	denied := chatAs(h, testSecret)

	if denied.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", denied.Code)
	}
	retry := denied.Header().Get("Retry-After")
	if retry == "" {
		t.Fatal("no Retry-After on a 429")
	}
	// Whole seconds, and never zero: a client told to retry immediately spins.
	n, err := strconv.Atoi(retry)
	if err != nil {
		t.Fatalf("Retry-After = %q, want whole seconds: %v", retry, err)
	}
	if n < 1 {
		t.Errorf("Retry-After = %d, want at least 1", n)
	}
}

// The bucket is keyed on the authenticated caller, so one noisy client must
// not spend anyone else's allowance.
func TestRateLimit_IsPerCaller(t *testing.T) {
	limiter, err := ratelimit.NewMemory(ratelimit.Config{Rate: 0.001, Burst: 1})
	if err != nil {
		t.Fatalf("NewMemory() failed: %v", err)
	}
	keys, err := auth.NewStaticStore("alice:gw_alice,bob:gw_bob")
	if err != nil {
		t.Fatalf("building the key store: %v", err)
	}

	reg := provider.NewRegistry()
	reg.Register(succeedingProvider())
	if err := reg.SetDefault("stub"); err != nil {
		t.Fatalf("registry: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(reg, logger, time.Second, "test").WithAuth(keys).WithRateLimiter(limiter).Handler()

	if rec := chatAs(h, "gw_alice"); rec.Code != http.StatusOK {
		t.Fatalf("alice's first request: status = %d, want 200", rec.Code)
	}
	if rec := chatAs(h, "gw_alice"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("alice's second request: status = %d, want 429", rec.Code)
	}
	if rec := chatAs(h, "gw_bob"); rec.Code != http.StatusOK {
		t.Errorf("bob was rate limited because alice spent her allowance: status = %d", rec.Code)
	}
}

// A limiter outage must not become a gateway outage.
func TestRateLimit_FailsOpenWhenTheLimiterBreaks(t *testing.T) {
	keys, err := auth.NewStaticStore("alice:" + testSecret)
	if err != nil {
		t.Fatalf("building the key store: %v", err)
	}
	reg := provider.NewRegistry()
	reg.Register(succeedingProvider())
	if err := reg.SetDefault("stub"); err != nil {
		t.Fatalf("registry: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(reg, logger, time.Second, "test").
		WithAuth(keys).
		WithRateLimiter(brokenLimiter{}).
		Handler()

	if rec := chatAs(h, testSecret); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: briefly over-serving beats refusing everything "+
			"because a side channel is down", rec.Code)
	}
}

// brokenLimiter stands in for a Redis that is down.
type brokenLimiter struct{}

func (brokenLimiter) Allow(context.Context, string) (ratelimit.Decision, error) {
	return ratelimit.Decision{}, io.ErrUnexpectedEOF
}
func (brokenLimiter) Close() error { return nil }

// Health probes must not consume anyone's allowance.
func TestRateLimit_LeavesHealthzAlone(t *testing.T) {
	h := limitedHandler(t, ratelimit.Config{Rate: 0.001, Burst: 1})

	for range 5 {
		if rec := do(h, http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
			t.Fatalf("healthz status = %d, want 200 regardless of the limit", rec.Code)
		}
	}
}
