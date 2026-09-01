// Package metrics exposes what the gateway is doing, in Prometheus format.
//
// Observability is half of why a gateway is worth putting in the request path
// at all: latency, error rate and spend per provider are impossible to
// assemble when every application calls the vendors directly.
package metrics

import (
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// maxLabelValues bounds how many distinct models are tracked.
//
// Label values from user input are the classic way to kill a Prometheus
// server: a caller sending a fresh model name per request creates a fresh time
// series per request until something runs out of memory. Past the cap
// everything collapses into "other", which loses detail but never the
// server.
const maxLabelValues = 100

// Label names and the placeholder used when no provider served a request.
// A failure that never reached anyone still has to be counted, or the error
// rate hides exactly the outages that matter most.
const (
	labelProvider = "provider"
	labelModel    = "model"
	providerNone  = "none"
)

// Metrics holds every collector the gateway publishes.
type Metrics struct {
	requests   *prometheus.CounterVec
	duration   *prometheus.HistogramVec
	firstToken *prometheus.HistogramVec
	tokens     *prometheus.CounterVec
	upstream   *prometheus.CounterVec
	rateLimit  prometheus.Counter
	circuits   *prometheus.GaugeVec
	inFlight   prometheus.Gauge

	// A read-write lock, not a plain one: every request consults this set and
	// almost none of them changes it, so serialising all of them behind one
	// exclusive lock would make the metrics the contention point.
	mu     sync.RWMutex
	models map[string]struct{}
}

// New registers the collectors on reg and returns them.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		models: make(map[string]struct{}),

		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_requests_total",
			Help: "Completions handled, by provider, model, outcome and mode.",
		}, []string{labelProvider, labelModel, "outcome", "streaming"}),

		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "llmgw_request_duration_seconds",
			Help: "End-to-end time per completion.",
			// Model calls run from a few hundred milliseconds to a minute, so
			// the default buckets, which top out at 10s, would put most
			// streamed answers in +Inf and tell you nothing.
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 20, 40, 80, 160},
		}, []string{labelProvider, labelModel, "streaming"}),

		firstToken: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "llmgw_stream_first_token_seconds",
			Help:    "Time from request to first streamed token, the latency a user actually feels.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2, 4, 8, 16},
		}, []string{labelProvider, labelModel}),

		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_tokens_total",
			Help: "Tokens reported by providers, by direction. The basis for cost per provider.",
		}, []string{labelProvider, labelModel, "kind"}),

		upstream: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_upstream_errors_total",
			Help: "Upstream failures, by provider and HTTP status.",
		}, []string{labelProvider, "status"}),

		rateLimit: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "llmgw_rate_limited_total",
			Help: "Requests refused by the rate limiter.",
			// Deliberately unlabelled: keying this by caller would put an
			// unbounded, attacker-controlled dimension into the metric.
		}),

		circuits: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "llmgw_circuit_state",
			Help: "Circuit breaker state per provider: 0 closed, 1 open, 2 half-open.",
		}, []string{labelProvider}),

		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "llmgw_requests_in_flight",
			Help: "Completions currently being served.",
		}),
	}

	reg.MustRegister(
		m.requests, m.duration, m.firstToken, m.tokens,
		m.upstream, m.rateLimit, m.circuits, m.inFlight,
	)
	return m
}

// model bounds the cardinality of a user-supplied model name.
func (m *Metrics) model(name string) string {
	if name == "" {
		return "unknown"
	}

	// Fast path: a name already seen needs only a read lock, which several
	// requests can hold at once.
	m.mu.RLock()
	_, seen := m.models[name]
	full := len(m.models) >= maxLabelValues
	m.mu.RUnlock()

	if seen {
		return name
	}
	if full {
		return "other"
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Re-check: another goroutine may have added it, or filled the set, while
	// the lock was being upgraded.
	if _, seen := m.models[name]; seen {
		return name
	}
	if len(m.models) >= maxLabelValues {
		return "other"
	}
	m.models[name] = struct{}{}
	return name
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// RequestStarted reports a completion entering the gateway. The returned
// function must be called when it leaves.
func (m *Metrics) RequestStarted() func() {
	m.inFlight.Inc()
	return m.inFlight.Dec
}

// RequestFinished records one completed request. outcome is a short, bounded
// word: "ok", "upstream_error", "rate_limited", "client_error", "cancelled".
func (m *Metrics) RequestFinished(provider, model, outcome string, streaming bool, seconds float64) {
	if provider == "" {
		provider = providerNone
	}
	mdl := m.model(model)
	stream := boolLabel(streaming)

	m.requests.WithLabelValues(provider, mdl, outcome, stream).Inc()
	m.duration.WithLabelValues(provider, mdl, stream).Observe(seconds)
}

// FirstToken records how long a streamed answer took to start, which is the
// latency a user actually perceives.
func (m *Metrics) FirstToken(provider, model string, seconds float64) {
	if provider == "" {
		provider = providerNone
	}
	m.firstToken.WithLabelValues(provider, m.model(model)).Observe(seconds)
}

// Tokens records the usage a provider reported.
func (m *Metrics) Tokens(provider, model string, prompt, completion int) {
	if provider == "" {
		provider = providerNone
	}
	mdl := m.model(model)

	if prompt > 0 {
		m.tokens.WithLabelValues(provider, mdl, "prompt").Add(float64(prompt))
	}
	if completion > 0 {
		m.tokens.WithLabelValues(provider, mdl, "completion").Add(float64(completion))
	}
}

// UpstreamError records a provider failure. A status of zero means the
// exchange never completed.
func (m *Metrics) UpstreamError(provider string, status int) {
	if provider == "" {
		provider = providerNone
	}
	m.upstream.WithLabelValues(provider, strconv.Itoa(status)).Inc()
}

// RateLimited records a refusal by the limiter.
func (m *Metrics) RateLimited() { m.rateLimit.Inc() }

// CircuitStates publishes the state of every known circuit.
func (m *Metrics) CircuitStates(states map[string]string) {
	for name, state := range states {
		var v float64
		switch state {
		case "open":
			v = 1
		case "half-open":
			v = 2
		}
		m.circuits.WithLabelValues(name).Set(v)
	}
}
