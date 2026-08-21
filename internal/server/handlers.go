package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

// maxRequestBody caps how much JSON a client may send. Without it a single
// caller can make the process allocate until it dies.
const maxRequestBody = 1 << 20 // 1 MiB

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":   "ok",
		"provider": s.provider.Name(),
		"version":  s.version,
	})
}

// handleChatCompletions is the gateway's only real endpoint for now: decode,
// call the provider, encode.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	var req provider.ChatRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // loud about typos now; relax it if it gets annoying
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed JSON body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "field \"model\" is required")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "field \"messages\" must not be empty")
		return
	}
	if req.Stream {
		writeError(w, http.StatusNotImplemented, "not_implemented", "streaming arrives in phase 3")
		return
	}

	// r.Context() is already cancelled when the client disconnects. Layering a
	// deadline on top bounds how long a hung upstream can hold this handler.
	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()

	resp, err := s.provider.Chat(ctx, req)
	if err != nil {
		s.writeProviderError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// writeProviderError maps an upstream failure onto a client-facing status.
func (s *Server) writeProviderError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("provider call failed",
		"provider", s.provider.Name(),
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
