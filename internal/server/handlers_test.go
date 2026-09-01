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
	name   string
	chat   func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error)
	stream func(context.Context, provider.ChatRequest) (provider.Stream, error)
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

func (s *stubProvider) ChatStream(ctx context.Context, req provider.ChatRequest) (provider.Stream, error) {
	if s.stream == nil {
		return nil, provider.ErrStreamingNotSupported
	}
	return s.stream(ctx, req)
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
	reg := provider.NewRegistry()
	reg.Register(p)
	if err := reg.SetDefault(p.Name()); err != nil {
		panic(err)
	}
	return newTestHandlerWith(reg)
}

func newTestHandlerWith(reg *provider.Registry) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(reg, logger, time.Second, "test").Handler()
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
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{name: "openai"}, "gpt-")
	reg.Register(&stubProvider{name: "anthropic"}, "claude-")

	rec := do(newTestHandlerWith(reg), http.MethodGet, "/healthz", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got struct {
		Status    string   `json:"status"`
		Version   string   `json:"version"`
		Providers []string `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if got.Status != "ok" {
		t.Errorf("status = %q, want ok", got.Status)
	}
	if got.Version != "test" {
		t.Errorf("version = %q, want test", got.Version)
	}
	if len(got.Providers) != 2 {
		t.Errorf("providers = %v, want both registered ones listed", got.Providers)
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

// TestChatCompletions_KeepsUnknownFields is the other half of dropping
// DisallowUnknownFields: what the gateway does not model must survive in Extra
// instead of being silently discarded at the door.
func TestChatCompletions_KeepsUnknownFields(t *testing.T) {
	var got provider.ChatRequest
	p := &stubProvider{chat: func(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		got = req
		return okResponse(), nil
	}}

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"top_p":0.9,"presence_penalty":1,"user":"u-42"}`
	if rec := do(newTestHandler(p), http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: real SDKs send fields we do not model (body: %s)", rec.Code, rec.Body)
	}

	if got.Extra == nil {
		t.Fatal("Extra is nil, want the unmodelled fields preserved")
	}
	for _, k := range []string{"top_p", "presence_penalty", "user"} {
		if _, ok := got.Extra[k]; !ok {
			t.Errorf("Extra is missing %q: %v", k, got.Extra)
		}
	}
	// Fields the schema does model must not be duplicated into Extra.
	for _, k := range []string{"model", "messages"} {
		if _, ok := got.Extra[k]; ok {
			t.Errorf("Extra contains modelled field %q", k)
		}
	}
}

func TestChatCompletions_NoExtraWhenNothingUnknown(t *testing.T) {
	var got provider.ChatRequest
	p := &stubProvider{chat: func(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		got = req
		return okResponse(), nil
	}}

	do(newTestHandler(p), http.MethodPost, "/v1/chat/completions", validBody)

	if got.Extra != nil {
		t.Errorf("Extra = %v, want nil when every field is modelled", got.Extra)
	}
}

// TestChatCompletions_RoutesByModel proves the provider is chosen per request
// from the model name rather than fixed at startup.
func TestChatCompletions_RoutesByModel(t *testing.T) {
	var served string
	record := func(name string) *stubProvider {
		return &stubProvider{name: name, chat: func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
			served = name
			return okResponse(), nil
		}}
	}

	reg := provider.NewRegistry()
	reg.Register(record("openai"), "gpt-")
	reg.Register(record("anthropic"), "claude-")
	h := newTestHandlerWith(reg)

	tests := []struct{ model, want string }{
		{"gpt-4o-mini", "openai"},
		{"claude-sonnet-5", "anthropic"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			served = ""
			body := `{"model":"` + tt.model + `","messages":[{"role":"user","content":"hi"}]}`
			if rec := do(h, http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
			}
			if served != tt.want {
				t.Errorf("model %q was served by %q, want %q", tt.model, served, tt.want)
			}
		})
	}
}

func TestChatCompletions_UnroutableModel(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{name: "openai", chat: func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
		t.Error("provider was called for a model it does not serve")
		return okResponse(), nil
	}}, "gpt-")

	body := `{"model":"llama-3","messages":[{"role":"user","content":"hi"}]}`
	rec := do(newTestHandlerWith(reg), http.MethodPost, "/v1/chat/completions", body)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a model no provider serves", rec.Code)
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

// The Provider contract says exactly one of (response, error) is non-nil. A
// broken implementation must produce a diagnosable 502, not a panic that the
// recoverer flattens into an opaque 500.
func TestChatCompletions_HandlesAProviderThatReturnsNothing(t *testing.T) {
	p := &stubProvider{chat: func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, nil //nolint:nilnil // the contract violation is the point
	}}

	rec := do(newTestHandler(p), http.MethodPost, "/v1/chat/completions", validBody)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (body: %s)", rec.Code, rec.Body)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not the usual envelope: %v", err)
	}
	if env.Error.Message == "" {
		t.Error("the 502 carries no message")
	}
}
