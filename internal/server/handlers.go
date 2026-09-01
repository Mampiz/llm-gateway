package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

// maxRequestBody caps how much JSON a client may send. Without it a single
// caller can make the process allocate until it dies.
const maxRequestBody = 1 << 20 // 1 MiB

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"version":   s.version,
		"providers": s.registry.Names(),
		// The first thing worth looking at when a gateway starts answering
		// slowly or from the wrong vendor.
		"circuits": s.router.Breakers(),
	})
}

// handleChatCompletions decodes the request, resolves which provider serves
// the requested model, and encodes whatever comes back.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	var req provider.ChatRequest
	// Unknown fields are tolerated rather than rejected: real SDKs send
	// top_p, presence_penalty, stream_options and more, and a gateway that
	// 400s on them is a gateway nobody can use. What we do not model, we keep
	// in Extra so providers that understand it can forward it.
	raw, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed JSON body: "+err.Error())
		return
	}
	req.Extra = unknownFields(raw)

	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "field \"model\" is required")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "field \"messages\" must not be empty")
		return
	}
	// A model nobody serves is the caller's mistake, not an upstream failure.
	if _, err := s.registry.For(req.Model); errors.Is(err, provider.ErrNoProvider) {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	if req.Stream {
		s.streamChatCompletions(w, r, req)
		return
	}

	// r.Context() is already cancelled when the client disconnects. Layering a
	// deadline on top bounds how long a hung upstream can hold this handler.
	// The budget covers the whole chain, retries and fallbacks included.
	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()

	done := s.metrics.RequestStarted()
	defer done()
	started := time.Now()

	resp, served, cached, err := s.cachedChat(ctx, r, req)
	if err != nil {
		s.metrics.RequestFinished(served, req.Model, outcomeFor(err), false, time.Since(started).Seconds())
		s.recordUpstreamError(served, err)
		s.writeProviderError(w, r, served, err)
		return
	}

	if cached {
		w.Header().Set(cacheHeader, cacheHit)
		s.metrics.RequestFinished(served, req.Model, "cache_hit", false, time.Since(started).Seconds())
	} else {
		w.Header().Set(cacheHeader, cacheMiss)
		s.metrics.RequestFinished(served, req.Model, "ok", false, time.Since(started).Seconds())
		// Only a real call spends tokens. Counting a cache hit again would
		// inflate the cost the dashboard reports.
		s.metrics.Tokens(served, resp.Model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	}
	s.metrics.CircuitStates(s.router.Breakers())

	writeJSON(w, http.StatusOK, resp)
}

// readBody reads the (already capped) request body and reports an empty one as
// an error, since json.Unmarshal's message for it is unhelpful.
func readBody(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, errors.New("could not read request body: " + err.Error())
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, errors.New("request body is empty")
	}
	return raw, nil
}

// knownFields is the set of JSON keys the canonical schema models, derived
// from the struct tags themselves so the two can never drift apart.
// sync.OnceValue computes it on first use and caches it forever.
var knownFields = sync.OnceValue(func() map[string]struct{} {
	fields := make(map[string]struct{})
	t := reflect.TypeFor[provider.ChatRequest]()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			fields[name] = struct{}{}
		}
	}
	return fields
})

// unknownFields returns the top-level keys of raw that the canonical schema
// does not model. Returns nil when there are none, so the common case costs no
// allocation.
func unknownFields(raw []byte) map[string]any {
	var all map[string]any
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil
	}

	var extra map[string]any
	known := knownFields()
	for k, v := range all {
		if _, ok := known[k]; ok {
			continue
		}
		if extra == nil {
			extra = make(map[string]any)
		}
		extra[k] = v
	}
	return extra
}

// writeProviderError maps an upstream failure onto a client-facing status.
func (s *Server) writeProviderError(w http.ResponseWriter, r *http.Request, providerName string, err error) {
	s.logger.Error("provider call failed",
		"provider", providerName,
		"error", err,
		"request_id", RequestIDFrom(r.Context()),
	)

	switch {
	case errors.Is(err, context.Canceled):
		// The client hung up; nobody is left to read a response body.
		// 499 is nginx's non-standard "client closed request".
		w.WriteHeader(499)
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "timeout", "upstream provider timed out")
	default:
		var pErr *provider.Error
		if errors.As(err, &pErr) && pErr.StatusCode >= 400 && pErr.StatusCode < 600 {
			writeError(w, pErr.StatusCode, "upstream_error", pErr.Message)
			return
		}
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
	}
}

// outcomeFor reduces an error to one of a small, bounded set of words. The
// set has to stay small: it is a metric label, and an unbounded one would
// multiply every series by the number of distinct failure messages.
func outcomeFor(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}

	var pErr *provider.Error
	if errors.As(err, &pErr) {
		if pErr.StatusCode >= 400 && pErr.StatusCode < 500 {
			return "client_error"
		}
		return "upstream_error"
	}
	return "error"
}

// recordUpstreamError publishes the status a provider returned, when there
// was one.
func (s *Server) recordUpstreamError(served string, err error) {
	var pErr *provider.Error
	if errors.As(err, &pErr) {
		s.metrics.UpstreamError(served, pErr.StatusCode)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status) // must come after the headers, before the body
	_ = json.NewEncoder(w).Encode(v)
}

// errorEnvelope mirrors the shape OpenAI uses, so existing client SDKs can
// parse the gateway's errors without special-casing.
type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, kind, msg string) {
	var env errorEnvelope
	env.Error.Message = msg
	env.Error.Type = kind
	writeJSON(w, status, env)
}
