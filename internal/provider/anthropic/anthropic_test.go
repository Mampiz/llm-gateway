package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New("sk-ant-test", srv.URL, testDefaultMaxTokens, srv.Client())
}

func sampleRequest() provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "claude-sonnet-5",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}
}

const okBody = `{
	"id": "msg_abc",
	"type": "message",
	"role": "assistant",
	"model": "claude-sonnet-5",
	"content": [{"type":"text","text":"hi there"}],
	"stop_reason": "end_turn",
	"usage": {"input_tokens":9,"output_tokens":2}
}`

func TestChat_SendsVendorSpecificHeaders(t *testing.T) {
	var gotPath, gotKey, gotVersion, gotAuth string

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-Api-Key")
		gotVersion = r.Header.Get("Anthropic-Version")
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, okBody)
	})

	if _, err := c.Chat(context.Background(), sampleRequest()); err != nil {
		t.Fatalf("Chat() failed: %v", err)
	}

	// A different endpoint, a different auth scheme and a mandatory version
	// header: none of this leaks out of the package.
	if gotPath != "/messages" {
		t.Errorf("path = %q, want /messages", gotPath)
	}
	if gotKey != "sk-ant-test" {
		t.Errorf("X-Api-Key = %q, want the key", gotKey)
	}
	if gotVersion != APIVersion {
		t.Errorf("Anthropic-Version = %q, want %q", gotVersion, APIVersion)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want it absent: this vendor does not use bearer tokens", gotAuth)
	}
}

func TestChat_SendsTranslatedBody(t *testing.T) {
	var raw map[string]any

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = io.WriteString(w, okBody)
	})

	req := sampleRequest()
	req.Messages = append([]provider.Message{{Role: "system", Content: "be brief"}}, req.Messages...)

	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat() failed: %v", err)
	}

	if raw["system"] != "be brief" {
		t.Errorf("system = %v, want it hoisted to a top-level field", raw["system"])
	}
	if _, present := raw["max_tokens"]; !present {
		t.Error("max_tokens is absent, want the default supplied: this vendor requires it")
	}
	msgs, _ := raw["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("messages = %v, want only the non-system turn", raw["messages"])
	}
}

func TestChat_ReturnsCanonicalResponse(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, okBody)
	})

	got, err := c.Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat() failed: %v", err)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != "hi there" {
		t.Errorf("choices = %+v, want the flattened text", got.Choices)
	}
	if got.Usage.TotalTokens != 11 {
		t.Errorf("TotalTokens = %d, want 11", got.Usage.TotalTokens)
	}
}

// The failure envelope is shaped differently from OpenAI's, which is exactly
// why each provider parses its own.
func TestChat_ParsesVendorErrorEnvelope(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit"}}`)
	})

	_, err := c.Chat(context.Background(), sampleRequest())

	var pErr *provider.Error
	if !errors.As(err, &pErr) {
		t.Fatalf("error = %v (%T), want a *provider.Error", err, err)
	}
	if pErr.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", pErr.Provider)
	}
	if pErr.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", pErr.StatusCode)
	}
	if !strings.Contains(pErr.Message, "rate limit") {
		t.Errorf("Message = %q, want the vendor's own wording", pErr.Message)
	}
	if !pErr.Retryable() {
		t.Error("Retryable() = false, want true for a rate limit")
	}
}

func TestChat_HonoursContextCancellation(t *testing.T) {
	released := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-released
	})
	defer close(released)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := c.Chat(ctx, sampleRequest()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded through the chain", err)
	}
}

func TestNew_Defaults(t *testing.T) {
	c := New("k", "", 0, nil)

	if c.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, DefaultBaseURL)
	}
	if c.defaultMaxTokens != DefaultMaxTokens {
		t.Errorf("defaultMaxTokens = %d, want %d", c.defaultMaxTokens, DefaultMaxTokens)
	}
	if c.http.Timeout != 0 {
		t.Errorf("http.Client.Timeout = %v, want 0: a timeout here would break streaming", c.http.Timeout)
	}
}
