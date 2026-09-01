// Package server wires the HTTP surface of the gateway: routing, middleware
// and the handlers that translate HTTP into provider calls.
package server

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"golang.org/x/sync/singleflight"

	"github.com/Mampiz/llm-gateway/internal/auth"
	"github.com/Mampiz/llm-gateway/internal/cache"
	"github.com/Mampiz/llm-gateway/internal/metrics"
	"github.com/Mampiz/llm-gateway/internal/provider"
	"github.com/Mampiz/llm-gateway/internal/ratelimit"
)

// Server holds everything the handlers need. Dependencies are injected
// rather than reached for through globals.
type Server struct {
	registry       *provider.Registry
	logger         *slog.Logger
	requestTimeout time.Duration
	version        string

	// keys authenticates clients. A nil store means authentication is
	// disabled, which config only allows when asked for explicitly.
	keys auth.Store

	// router resolves a model to the ordered providers that may serve it and
	// owns the retry policy and the circuit breakers.
	router *provider.Router

	// metricsHandler serves /metrics when one is configured.
	metricsHandler http.Handler

	// metrics publishes what the gateway is doing. Never nil: a no-op
	// registry keeps the recording calls free of nil checks.
	metrics *metrics.Metrics

	// cache stores completed answers. A nil cache means caching is off.
	cache      cache.Cache
	cacheTTL   time.Duration
	cacheScope string // "shared" or "caller"

	// inflight collapses identical concurrent requests, so a cold cache being
	// hit by a hundred copies of the same question fetches one answer rather
	// than a hundred.
	inflight singleflight.Group

	// limiter meters callers. A nil limiter means rate limiting is off.
	limiter ratelimit.Limiter

	// streamIdle and streamHeartbeat govern a streaming response. Neither can
	// be honoured by a loop that simply blocks on the next chunk, which is
	// what forces the streaming handler to multiplex.
	streamIdle      time.Duration
	streamHeartbeat time.Duration

	// draining is closed when the process is shutting down. Streamed answers
	// can outlive any sensible grace period, so they are told to wind up
	// rather than cut mid-frame when the deadline passes.
	draining  chan struct{}
	drainOnce sync.Once

	// streamMaxDuration caps a whole streamed answer. The idle timeout only
	// catches silence; an upstream that keeps emitting would otherwise hold a
	// connection, a goroutine and a paid-for call indefinitely.
	streamMaxDuration time.Duration
}

// New builds a Server. The registry, not a single provider, is injected: from
// phase 2 on the provider is chosen per request from the model name.
func New(reg *provider.Registry, logger *slog.Logger, requestTimeout time.Duration, version string) *Server {
	if version == "" {
		version = "dev"
	}
	return &Server{
		registry: reg,
		// Always present, so there is one request path rather than two.
		// Without configured fallbacks it simply retries the one provider.
		router:            provider.NewRouter(reg, nil, provider.DefaultRetryPolicy(), provider.BreakerConfig{}),
		logger:            logger,
		requestTimeout:    requestTimeout,
		version:           version,
		metrics:           metrics.New(prometheus.NewRegistry()),
		draining:          make(chan struct{}),
		streamIdle:        60 * time.Second,
		streamHeartbeat:   15 * time.Second,
		streamMaxDuration: 10 * time.Minute,
	}
}

// WithAuth attaches the key store that guards the API surface. Passing nil
// leaves the gateway unauthenticated.
func (s *Server) WithAuth(keys auth.Store) *Server {
	s.keys = keys
	return s
}

// Drain tells in-flight streamed answers to wind up.
//
// It exists because a streamed answer can legitimately run for minutes, far
// past any grace period worth giving a shutdown. Without it a rolling deploy
// cuts every stream mid-frame at the deadline, the client gets a truncated
// answer with no explanation, and http.Server.Shutdown reports a failure for
// what is an entirely normal event.
//
// Safe to call more than once: a second SIGTERM is not unusual.
func (s *Server) Drain() {
	s.drainOnce.Do(func() { close(s.draining) })
}

// WithStreamMaxDuration caps how long a single streamed answer may run. Zero
// or less leaves the current value untouched.
func (s *Server) WithStreamMaxDuration(d time.Duration) *Server {
	if d > 0 {
		s.streamMaxDuration = d
	}
	return s
}

// WithFallback configures the chain of models to try when one fails, along
// with the retry policy and the circuit breaker settings.
func (s *Server) WithFallback(fallbacks map[string][]string, policy provider.RetryPolicy, bcfg provider.BreakerConfig) *Server {
	s.router = provider.NewRouter(s.registry, fallbacks, policy, bcfg)
	return s
}

// WithMetrics attaches the collectors the gateway publishes. Without it the
// server still records, into a registry nobody scrapes.
func (s *Server) WithMetrics(m *metrics.Metrics, h http.Handler) *Server {
	if m != nil {
		s.metrics = m
	}
	s.metricsHandler = h
	return s
}

// WithCache attaches the response cache. scope is "caller" to keep entries
// private per client, anything else to share them.
func (s *Server) WithCache(c cache.Cache, ttl time.Duration, scope string) *Server {
	s.cache = c
	s.cacheTTL = ttl
	s.cacheScope = scope
	return s
}

// WithRateLimiter attaches the limiter that meters callers. Passing nil
// leaves the gateway unmetered.
func (s *Server) WithRateLimiter(l ratelimit.Limiter) *Server {
	s.limiter = l
	return s
}

// WithStreamTimings overrides the streaming durations. Zero or negative values
// leave the current one untouched.
func (s *Server) WithStreamTimings(idle, heartbeat time.Duration) *Server {
	if idle > 0 {
		s.streamIdle = idle
	}
	if heartbeat > 0 {
		s.streamHeartbeat = heartbeat
	}
	return s
}

// Handler returns the fully decorated http.Handler for the gateway.
//
// Since Go 1.22 the standard ServeMux understands method and path patterns,
// so no third-party router is needed yet. Middleware is just a function that
// wraps one http.Handler in another; chain applies them outermost-first.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	// Unauthenticated, like the health probe: a scrape endpoint that needs a
	// credential is one more thing to get wrong in a monitoring stack, and it
	// exposes counts rather than content.
	if s.metricsHandler != nil {
		mux.Handle("GET /metrics", s.metricsHandler)
	}
	// Only the API surface is guarded: a health probe that needs a credential
	// stops working exactly when it is most needed.
	// Order matters: authenticate first so the limiter can meter a caller
	// rather than an address.
	mux.Handle("POST /v1/chat/completions",
		s.requireAuth(s.rateLimit(http.HandlerFunc(s.handleChatCompletions))))

	return chain(mux,
		recoverer(s.logger), // outermost: catches panics from everything below
		requestID,
		logging(s.logger),
	)
}

// middleware is the shape every middleware in this package has.
type middleware func(http.Handler) http.Handler

// chain applies middlewares so that the first argument is the outermost
// wrapper, i.e. the first to see the request and the last to see the response.
func chain(h http.Handler, mws ...middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
