package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

// sampleRequest is the request every test sends unless it needs something else.
func sampleRequest() provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}
}

// newTestClient spins up an in-memory HTTP server running h and returns a
// Client pointed at it. httptest.NewServer listens on a real loopback port, so
// the whole net/http stack is exercised: no mocking of transports, no
// pretending. The server is torn down when the test ends.
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New("sk-test-key", srv.URL, srv.Client())
}

// jsonResponse writes a canned successful completion.
const okBody = `{
	"id": "chatcmpl-abc",
	"object": "chat.completion",
	"created": 1700000000,
	"model": "gpt-4o-mini",
	"choices": [{"index":0,"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],
	"usage": {"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}
}`

func TestChat_SendsWellFormedRequest(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotType   string
		gotBody   provider.ChatRequest
	)

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okBody)
	})

	temp := 0.7
	req := sampleRequest()
	req.Temperature = &temp

	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat() returned unexpected error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if want := "Bearer sk-test-key"; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotType)
	}
	if gotBody.Model != req.Model {
		t.Errorf("upstream model = %q, want %q", gotBody.Model, req.Model)
	}
	if len(gotBody.Messages) != 1 || gotBody.Messages[0].Content != "hello" {
		t.Errorf("upstream messages = %+v, want the original one", gotBody.Messages)
	}
	if gotBody.Temperature == nil || *gotBody.Temperature != temp {
		t.Errorf("upstream temperature = %v, want %v", gotBody.Temperature, temp)
	}
}

func TestChat_OmitsUnsetOptionalFields(t *testing.T) {
	var raw map[string]any

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = io.WriteString(w, okBody)
	})

	if _, err := c.Chat(context.Background(), sampleRequest()); err != nil {
		t.Fatalf("Chat() returned unexpected error: %v", err)
	}

	// Temperature and MaxTokens are nil pointers: they must not reach the
	// vendor at all, or we would be overriding its defaults with zeroes.
	for _, field := range []string{"temperature", "max_tokens"} {
		if _, present := raw[field]; present {
			t.Errorf("unset field %q was sent to the upstream: %v", field, raw[field])
		}
	}
}

func TestChat_ParsesSuccessfulResponse(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, okBody)
	})

	got, err := c.Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat() returned unexpected error: %v", err)
	}

	if got.ID != "chatcmpl-abc" {
		t.Errorf("ID = %q, want chatcmpl-abc", got.ID)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("len(Choices) = %d, want 1", len(got.Choices))
	}
	if want := "hi there"; got.Choices[0].Message.Content != want {
		t.Errorf("content = %q, want %q", got.Choices[0].Message.Content, want)
	}
	if got.Usage.TotalTokens != 11 {
		t.Errorf("TotalTokens = %d, want 11", got.Usage.TotalTokens)
	}
}

// TestChat_UpstreamErrors is table-driven: one test function, many cases.
// This is the idiomatic Go shape for "same logic, different inputs" and it
// makes adding a case a two-line change.
func TestChat_UpstreamErrors(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantStatus    int
		wantMessage   string
		wantRetryable bool
	}{
		{
			name:          "rate limited with vendor envelope",
			status:        http.StatusTooManyRequests,
			body:          `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota"}}`,
			wantStatus:    429,
			wantMessage:   "You exceeded your current quota",
			wantRetryable: true,
		},
		{
			name:          "bad request is not retryable",
			status:        http.StatusBadRequest,
			body:          `{"error":{"message":"Unknown model","type":"invalid_request_error"}}`,
			wantStatus:    400,
			wantMessage:   "Unknown model",
			wantRetryable: false,
		},
		{
			name:          "server error is retryable",
			status:        http.StatusInternalServerError,
			body:          `{"error":{"message":"The server had an error","type":"server_error"}}`,
			wantStatus:    500,
			wantMessage:   "The server had an error",
			wantRetryable: true,
		},
		{
			// A load balancer or proxy in front of the vendor may answer with
			// HTML instead of JSON. We must still surface something useful.
			name:          "non-JSON error body falls back to raw text",
			status:        http.StatusBadGateway,
			body:          "<html><body>502 Bad Gateway</body></html>",
			wantStatus:    502,
			wantMessage:   "<html><body>502 Bad Gateway</body></html>",
			wantRetryable: true,
		},
		{
			name:          "empty error body falls back to the status line",
			status:        http.StatusServiceUnavailable,
			body:          "",
			wantStatus:    503,
			wantMessage:   "503 Service Unavailable",
			wantRetryable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})

			resp, err := c.Chat(context.Background(), sampleRequest())
			if resp != nil {
				t.Errorf("response = %+v, want nil alongside an error", resp)
			}

			var pErr *provider.Error
			if !errors.As(err, &pErr) {
				t.Fatalf("error = %v (%T), want a *provider.Error", err, err)
			}
			if pErr.StatusCode != tt.wantStatus {
				t.Errorf("StatusCode = %d, want %d", pErr.StatusCode, tt.wantStatus)
			}
			if pErr.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", pErr.Message, tt.wantMessage)
			}
			if pErr.Retryable() != tt.wantRetryable {
				t.Errorf("Retryable() = %v, want %v", pErr.Retryable(), tt.wantRetryable)
			}
			if pErr.Provider != "openai" {
				t.Errorf("Provider = %q, want openai", pErr.Provider)
			}
		})
	}
}

func TestChat_ErrorBodyIsCapped(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		// Ten times the cap, and not valid JSON, so it lands in the raw-text
		// fallback path where the length is observable.
		_, _ = io.WriteString(w, strings.Repeat("x", maxErrorBody*10))
	})

	_, err := c.Chat(context.Background(), sampleRequest())

	var pErr *provider.Error
	if !errors.As(err, &pErr) {
		t.Fatalf("error = %v, want a *provider.Error", err)
	}
	if len(pErr.Message) > maxErrorBody {
		t.Errorf("message length = %d, want at most %d", len(pErr.Message), maxErrorBody)
	}
}

func TestChat_RejectsUnusableSuccess(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"malformed JSON", `{"id": "x", "choices": [`},
		{"no choices", `{"id":"x","object":"chat.completion","choices":[]}`},
		{"null choices", `{"id":"x","object":"chat.completion"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tt.body)
			})

			resp, err := c.Chat(context.Background(), sampleRequest())
			if err == nil {
				t.Fatalf("Chat() = %+v, nil; want an error", resp)
			}
			if resp != nil {
				t.Errorf("response = %+v, want nil alongside an error", resp)
			}
		})
	}
}

// TestChat_HonoursContextCancellation is the important one: it proves the
// context actually reaches the socket. The upstream never answers, so the
// only thing that can end this call is the cancellation.
func TestChat_HonoursContextCancellation(t *testing.T) {
	released := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-released // block until the test lets go
	})
	defer close(released)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := c.Chat(ctx, sampleRequest())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Chat() returned nil error, want a cancellation error")
	}
	// errors.Is walks the chain through provider.Error.Unwrap. If Unwrap ever
	// disappears, this assertion is what catches it.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false for %v, want true", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Chat() took %v to notice cancellation, want well under 2s", elapsed)
	}

	var pErr *provider.Error
	if errors.As(err, &pErr) && pErr.Retryable() {
		t.Error("Retryable() = true for a cancelled request, want false: nobody is waiting for the answer")
	}
}

func TestChat_DeadlineExceeded(t *testing.T) {
	released := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-released
	})
	defer close(released)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.Chat(ctx, sampleRequest())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false for %v, want true", err)
	}
}

// TestChat_TransportFailure covers the case where no HTTP exchange happens at
// all: the connection is refused. StatusCode must stay 0 and the failure must
// be retryable, because another provider might well be up.
func TestChat_TransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening any more

	c := New("sk-test-key", url, srv.Client())

	_, err := c.Chat(context.Background(), sampleRequest())

	var pErr *provider.Error
	if !errors.As(err, &pErr) {
		t.Fatalf("error = %v (%T), want a *provider.Error", err, err)
	}
	if pErr.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0 for a transport failure", pErr.StatusCode)
	}
	if !pErr.Retryable() {
		t.Error("Retryable() = false, want true for a transport failure")
	}
}

func TestChat_RejectsUnserializableRequest(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream was called, want the request rejected before sending")
	})

	nan := math.NaN()
	req := sampleRequest()
	req.Temperature = &nan

	if _, err := c.Chat(context.Background(), req); err == nil {
		t.Fatal("Chat() returned nil error for a NaN temperature, want a marshal error")
	} else if !strings.Contains(err.Error(), "openai:") {
		t.Errorf("error = %q, want it prefixed with the package name", err)
	}
}

func TestNew_Defaults(t *testing.T) {
	c := New("k", "", nil)
	if c.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, DefaultBaseURL)
	}
	if c.http == nil {
		t.Fatal("http client is nil, want a default one")
	}
	// A non-zero Timeout here would break streaming in phase 3.
	if c.http.Timeout != 0 {
		t.Errorf("http.Client.Timeout = %v, want 0", c.http.Timeout)
	}
}

func TestChat_ForwardsExtraFields(t *testing.T) {
	var raw map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = io.WriteString(w, okBody)
	})

	req := sampleRequest()
	req.Extra = map[string]any{"top_p": 0.9, "user": "u-42"}

	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat() failed: %v", err)
	}

	// This vendor's API is the canonical schema, so unmodelled parameters can
	// be passed through instead of dropped.
	if raw["top_p"] != 0.9 {
		t.Errorf("top_p = %v, want it forwarded", raw["top_p"])
	}
	if raw["user"] != "u-42" {
		t.Errorf("user = %v, want it forwarded", raw["user"])
	}
}

// TestChat_ExtraCannotOverrideControlledFields is a security-shaped test: the
// passthrough must never let a caller rewrite a field the gateway owns.
func TestChat_ExtraCannotOverrideControlledFields(t *testing.T) {
	var raw map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = io.WriteString(w, okBody)
	})

	req := sampleRequest()
	req.Extra = map[string]any{
		"model":    "smuggled-model",
		"messages": []any{map[string]any{"role": "user", "content": "smuggled"}},
	}

	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat() failed: %v", err)
	}

	if raw["model"] != "gpt-4o-mini" {
		t.Errorf("model = %v, want the real one to survive the merge", raw["model"])
	}
	msgs, _ := raw["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want the original single message", raw["messages"])
	}
	if first, _ := msgs[0].(map[string]any); first["content"] != "hello" {
		t.Errorf("messages[0].content = %v, want the original", first["content"])
	}
}
