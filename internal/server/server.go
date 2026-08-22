// Package server wires the HTTP surface of the gateway: routing, middleware
// and the handlers that translate HTTP into provider calls.
package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

// Server holds everything the handlers need. Dependencies are injected
// rather than reached for through globals.
type Server struct {
	registry       *provider.Registry
	logger         *slog.Logger
	requestTimeout time.Duration
	version        string

	// streamIdle and streamHeartbeat govern a streaming response. Neither can
	// be honoured by a loop that simply blocks on the next chunk, which is
	// what forces the streaming handler to multiplex.
	streamIdle      time.Duration
	streamHeartbeat time.Duration
}

// New builds a Server. The registry, not a single provider, is injected: from
// phase 2 on the provider is chosen per request from the model name.
func New(reg *provider.Registry, logger *slog.Logger, requestTimeout time.Duration, version string) *Server {
	if version == "" {
		version = "dev"
	}
	return &Server{
		registry:        reg,
		logger:          logger,
		requestTimeout:  requestTimeout,
		version:         version,
		streamIdle:      60 * time.Second,
		streamHeartbeat: 15 * time.Second,
	}
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
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)

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
