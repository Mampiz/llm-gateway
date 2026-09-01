package metrics

import (
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newTestMetrics(t *testing.T) (*Metrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	return New(reg), reg
}

func TestRequestFinished(t *testing.T) {
	m, reg := newTestMetrics(t)

	m.RequestFinished("openai", "gpt-4o-mini", "ok", false, 0.42)
	m.RequestFinished("openai", "gpt-4o-mini", "ok", false, 1.1)
	m.RequestFinished("anthropic", "claude-sonnet-5", "upstream_error", true, 0.2)

	if got := testutil.ToFloat64(m.requests.WithLabelValues("openai", "gpt-4o-mini", "ok", "false")); got != 2 {
		t.Errorf("requests = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("anthropic", "claude-sonnet-5", "upstream_error", "true")); got != 1 {
		t.Errorf("streaming failure count = %v, want 1", got)
	}
	if n := testutil.CollectAndCount(reg, "llmgw_request_duration_seconds"); n == 0 {
		t.Error("no duration series were recorded")
	}
}

// A failure with no provider still has to be counted, or the error rate hides
// exactly the outages that never reached anyone.
func TestRequestFinished_HandlesAnEmptyProvider(t *testing.T) {
	m, _ := newTestMetrics(t)

	m.RequestFinished("", "gpt-4o-mini", "error", false, 0.1)

	if got := testutil.ToFloat64(m.requests.WithLabelValues("none", "gpt-4o-mini", "error", "false")); got != 1 {
		t.Errorf("count = %v, want the request recorded under \"none\"", got)
	}
}

func TestTokens(t *testing.T) {
	m, _ := newTestMetrics(t)

	m.Tokens("openai", "gpt-4o-mini", 100, 50)
	m.Tokens("openai", "gpt-4o-mini", 10, 5)

	if got := testutil.ToFloat64(m.tokens.WithLabelValues("openai", "gpt-4o-mini", "prompt")); got != 110 {
		t.Errorf("prompt tokens = %v, want 110", got)
	}
	if got := testutil.ToFloat64(m.tokens.WithLabelValues("openai", "gpt-4o-mini", "completion")); got != 55 {
		t.Errorf("completion tokens = %v, want 55", got)
	}
}

// Providers often report only one side, and a zero must not create a series
// that suggests a real measurement of zero.
func TestTokens_SkipsZeroes(t *testing.T) {
	m, reg := newTestMetrics(t)

	m.Tokens("openai", "gpt-4o-mini", 0, 0)

	if n := testutil.CollectAndCount(reg, "llmgw_tokens_total"); n != 0 {
		t.Errorf("recorded %d token series for a report of nothing, want 0", n)
	}
}

// Label values taken from user input are the classic way to kill a Prometheus
// server: one fresh model name per request is one fresh time series per
// request.
func TestModelLabel_IsBounded(t *testing.T) {
	m, _ := newTestMetrics(t)

	for i := range maxLabelValues {
		if got := m.model("model-" + strconv.Itoa(i)); got == "other" {
			t.Fatalf("model %d collapsed early, want the first %d kept", i, maxLabelValues)
		}
	}

	if got := m.model("one-model-too-many"); got != "other" {
		t.Errorf("model = %q, want it collapsed into \"other\" past the cap", got)
	}
	// Names already seen keep their identity even once the cap is reached.
	if got := m.model("model-0"); got != "model-0" {
		t.Errorf("model = %q, want a known name to survive the cap", got)
	}
}

func TestModelLabel_HandlesEmpty(t *testing.T) {
	m, _ := newTestMetrics(t)
	if got := m.model(""); got != "unknown" {
		t.Errorf("model = %q, want unknown", got)
	}
}

func TestUpstreamError(t *testing.T) {
	m, _ := newTestMetrics(t)

	m.UpstreamError("openai", 429)
	m.UpstreamError("openai", 429)
	m.UpstreamError("openai", 0) // never completed

	if got := testutil.ToFloat64(m.upstream.WithLabelValues("openai", "429")); got != 2 {
		t.Errorf("429 count = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.upstream.WithLabelValues("openai", "0")); got != 1 {
		t.Errorf("transport failure count = %v, want 1", got)
	}
}

func TestCircuitStates(t *testing.T) {
	m, _ := newTestMetrics(t)

	m.CircuitStates(map[string]string{
		"openai":    "open",
		"anthropic": "closed",
		"mock":      "half-open",
	})

	for provider, want := range map[string]float64{"openai": 1, "anthropic": 0, "mock": 2} {
		if got := testutil.ToFloat64(m.circuits.WithLabelValues(provider)); got != want {
			t.Errorf("circuit %s = %v, want %v", provider, got, want)
		}
	}
}

func TestInFlight(t *testing.T) {
	m, _ := newTestMetrics(t)

	done := m.RequestStarted()
	if got := testutil.ToFloat64(m.inFlight); got != 1 {
		t.Errorf("in flight = %v, want 1", got)
	}
	done()
	if got := testutil.ToFloat64(m.inFlight); got != 0 {
		t.Errorf("in flight = %v, want 0 after the request finished", got)
	}
}

func TestRateLimited(t *testing.T) {
	m, _ := newTestMetrics(t)

	m.RateLimited()
	m.RateLimited()

	if got := testutil.ToFloat64(m.rateLimit); got != 2 {
		t.Errorf("count = %v, want 2", got)
	}
}

// The names are the public contract: dashboards and alerts break when they
// change, so a rename should have to be deliberate.
func TestPublishedNames(t *testing.T) {
	m, reg := newTestMetrics(t)

	m.RequestFinished("openai", "gpt-4o-mini", "ok", false, 0.1)
	m.FirstToken("openai", "gpt-4o-mini", 0.2)
	m.Tokens("openai", "gpt-4o-mini", 1, 1)
	m.UpstreamError("openai", 500)
	m.RateLimited()
	m.CircuitStates(map[string]string{"openai": "closed"})

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}

	found := make(map[string]bool)
	for _, f := range families {
		found[f.GetName()] = true
		if !strings.HasPrefix(f.GetName(), "llmgw_") {
			t.Errorf("metric %q is missing the llmgw_ prefix", f.GetName())
		}
		if f.GetHelp() == "" {
			t.Errorf("metric %q has no help text", f.GetName())
		}
	}

	for _, want := range []string{
		"llmgw_requests_total",
		"llmgw_request_duration_seconds",
		"llmgw_stream_first_token_seconds",
		"llmgw_tokens_total",
		"llmgw_upstream_errors_total",
		"llmgw_rate_limited_total",
		"llmgw_circuit_state",
		"llmgw_requests_in_flight",
	} {
		if !found[want] {
			t.Errorf("metric %q is not published", want)
		}
	}
}

// Every request goes through the label bound, on its own goroutine.
func TestModelLabel_IsSafeUnderConcurrency(t *testing.T) {
	m, _ := newTestMetrics(t)

	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.RequestFinished("openai", "model-"+strconv.Itoa(i%50), "ok", false, 0.1)
			_ = m.model("model-" + strconv.Itoa(i))
		}()
	}
	wg.Wait()

	m.mu.RLock()
	size := len(m.models)
	m.mu.RUnlock()

	if size > maxLabelValues {
		t.Errorf("tracked %d model names, want at most %d", size, maxLabelValues)
	}
}
