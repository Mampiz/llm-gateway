package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

// stubProvider is a test double: the behaviour of every case is injected as a
// function, so one type covers success, every failure mode and even a panic.
type stubProvider struct {
	name string
	chat func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error)
}

func (s *stubProvider) Name() string {
	if s.name == "" {
		return "stub"
	}
	return s.name
}

func (s *stubProvider) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return s.chat(ctx, req)
}

var _ provider.Provider = (*stubProvider)(nil)

func okResponse() *provider.ChatResponse {
	return &provider.ChatResponse{
		ID:      "chatcmpl-test",
		Object:  "chat.completion",
		Model:   "gpt-4o-mini",
		Choices: []provider.Choice{{Message: provider.Message{Role: "assistant", Content: "hi"}}},
		Usage:   provider.Usage{TotalTokens: 11},
	}
}

// newTestHandler builds the full handler chain, middleware included, with a
// logger that goes nowhere so test output stays readable.
func newTestHandler(p provider.Provider) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(p, logger, time.Second, "test").Handler()
}

// do sends one request through the handler and returns the recorded response.
func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const validBody = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`

func succeedingProvider() *stubProvider {
	return &stubProvider{chat: func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
		return okResponse(), nil
	}}
}

func TestHealthz(t *testing.T) {
	rec := do(newTestHandler(&stubProvider{name: "openai"}), http.MethodGet, "/healthz", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if got["status"] != "ok" {
		t.Errorf("status field = %q, want ok", got["status"])
	}
	if got["provider"] != "openai" {
		t.Errorf("provider field = %q, want openai", got["provider"])
	}
	if got["version"] != "test" {
		t.Errorf("version field = %q, want test", got["version"])
	}
}

func TestChatCompletions_Success(t *testing.T) {
	rec := do(newTestHandler(succeedingProvider()), http.MethodPost, "/v1/chat/completions", validBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got provider.ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not a ChatResponse: %v", err)
	}
	if got.ID != "chatcmpl-test" {
		t.Errorf("ID = %q, want chatcmpl-test", got.ID)
	}
}

// TestChatCompletions_PassesRequestThrough guards the contract between the
// HTTP layer and the provider: what the client sent is what the provider sees.
func TestChatCompletions_PassesRequestThrough(t *testing.T) {
	var got provider.ChatRequest
	p := &stubProvider{chat: func(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		got = req
		return okResponse(), nil
	}}

	body := `{"model":"gpt-4o","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],"temperature":0.2,"max_tokens":64}`
	if rec := do(newTestHandler(p), http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
	}

	if got.Model != "gpt-4o" {
		t.Errorf("Model = %q, want gpt-4o", got.Model)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("len(Messages) = %d, want 2", len(got.Messages))
	}
	if got.Temperature == nil || *got.Temperature != 0.2 {
		t.Errorf("Temperature = %v, want 0.2", got.Temperature)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 64 {
		t.Errorf("MaxTokens = %v, want 64", got.MaxTokens)
	}
}

// TestChatCompletions_AppliesRequestTimeout proves the handler puts a deadline
// on the context it hands to the provider, rather than trusting it to behave.
func TestChatCompletions_AppliesRequestTimeout(t *testing.T) {
	var hasDeadline bool
	p := &stubProvider{chat: func(ctx context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
		_, hasDeadline = ctx.Deadline()
		return okResponse(), nil
	}}

	do(newTestHandler(p), http.MethodPost, "/v1/chat/completions", validBody)

	if !hasDeadline {
		t.Error("provider received a context with no deadline, want the configured request timeout applied")
	}
}

func TestChatCompletions_RejectsBadRequests(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"malformed JSON", `{"model":`, http.StatusBadRequest},
		{"empty body", ``, http.StatusBadRequest},
		{"missing model", `{"messages":[{"role":"user","content":"hi"}]}`, http.StatusBadRequest},
		{"empty messages", `{"model":"gpt-4o-mini","messages":[]}`, http.StatusBadRequest},
		{"no messages field", `{"model":"gpt-4o-mini"}`, http.StatusBadRequest},
		{"unknown field", `{"model":"m","messages":[{"role":"user","content":"hi"}],"nope":1}`, http.StatusBadRequest},
		{"streaming not implemented yet", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`, http.StatusNotImplemented},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &stubProvider{chat: func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
				t.Error("provider was called, want the request rejected first")
				return okResponse(), nil
			}}

			rec := do(newTestHandler(p), http.MethodPost, "/v1/chat/completions", tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body)
			}

			// Every error must come back in the OpenAI envelope so client SDKs
			// can parse it without special-casing this gateway.
			var env errorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("error response is not JSON: %v", err)
			}
			if env.Error.Message == "" {
				t.Error("error message is empty, want something actionable")
			}
			if env.Error.Type == "" {
				t.Error("error type is empty")
			}
		})
	}
}

func TestChatCompletions_RejectsOversizedBody(t *testing.T) {
	huge := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 2*maxRequestBody) + `"}]}`

	rec := do(newTestHandler(succeedingProvider()), http.MethodPost, "/v1/chat/completions", huge)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a body over the %d byte cap", rec.Code, maxRequestBody)
	}
}

// TestChatCompletions_MapsProviderErrors is where the error taxonomy earns its
// keep: each upstream failure has to become the right status for the client.
func TestChatCompletions_MapsProviderErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{
			name:       "client disconnected",
			err:        context.Canceled,
			wantStatus: 499,
		},
		{
			name:       "upstream timed out",
			err:        context.DeadlineExceeded,
			wantStatus: http.StatusGatewayTimeout,
		},
		{
			name:       "rate limit is propagated verbatim",
			err:        &provider.Error{Provider: "openai", StatusCode: 429, Message: "quota exceeded"},
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name:       "vendor rejected the request",
			err:        &provider.Error{Provider: "openai", StatusCode: 400, Message: "unknown model"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "transport failure becomes a bad gateway",
			err:        &provider.Error{Provider: "openai", StatusCode: 0, Message: "connection refused"},
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "unclassified error becomes a bad gateway",
			err:        errors.New("something went sideways"),
			wantStatus: http.StatusBadGateway,
		},
		{
			// A cancellation wrapped in a provider.Error must still be seen
			// through the chain, which is exactly what Unwrap is for.
			name:       "wrapped cancellation is still a cancellation",
			err:        &provider.Error{Provider: "openai", Message: "ctx", Err: context.Canceled},
			wantStatus: 499,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &stubProvider{chat: func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
				return nil, tt.err
			}}

			rec := do(newTestHandler(p), http.MethodPost, "/v1/chat/completions", validBody)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body)
			}
		})
	}
}

func TestChatCompletions_PropagatesUpstreamMessage(t *testing.T) {
	p := &stubProvider{chat: func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, &provider.Error{Provider: "openai", StatusCode: 429, Message: "quota exceeded, check billing"}
	}}

	rec := do(newTestHandler(p), http.MethodPost, "/v1/chat/completions", validBody)

	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error response is not JSON: %v", err)
	}
	if !strings.Contains(env.Error.Message, "quota exceeded") {
		t.Errorf("message = %q, want the vendor's own wording preserved", env.Error.Message)
	}
}

func TestRouting(t *testing.T) {
	tests := []struct {
		method, path string
		wantStatus   int
	}{
		{http.MethodGet, "/v1/chat/completions", http.StatusMethodNotAllowed},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/nope", http.StatusNotFound},
	}

	h := newTestHandler(succeedingProvider())
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			if rec := do(h, tt.method, tt.path, ""); rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}
