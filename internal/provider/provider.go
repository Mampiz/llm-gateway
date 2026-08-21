// Package provider defines the contract that every upstream LLM vendor
// (OpenAI, Anthropic, ...) must satisfy, together with the request and
// response types the gateway speaks internally.
//
// In phase 1 these types are a straight copy of the OpenAI wire format,
// because the gateway is a plain passthrough. From phase 2 on they become
// the *normalized* schema and each provider package is responsible for
// translating between this schema and its own vendor format.
package provider

import (
	"context"
	"fmt"
)

// Message is a single turn in a conversation.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is the body a client POSTs to /v1/chat/completions.
//
// Temperature and MaxTokens are pointers so that "absent" and "explicitly
// zero" stay distinguishable: a nil pointer is omitted from the outgoing
// JSON, while a pointer to 0 is sent as 0.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
}

// Choice is one completion alternative returned by the model.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage reports token accounting for a single call. Phase 6 turns this into
// Prometheus metrics and per-provider cost.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResponse is the non-streaming answer returned to the client.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Provider is implemented by every upstream vendor client.
//
// Chat must honour ctx: if the caller cancels it (client disconnected,
// deadline exceeded), the in-flight upstream request has to be torn down.
type Provider interface {
	// Name identifies the provider in logs, metrics and errors.
	Name() string

	// Chat performs a single non-streaming completion.
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

// Error is an upstream failure that carries enough context for the fallback
// logic in phase 5 to decide whether retrying on another provider is worth it.
type Error struct {
	Provider   string
	StatusCode int    // HTTP status returned by the vendor, 0 if the call never completed
	Message    string // vendor-supplied error message, or the transport error
}

func (e *Error) Error() string {
	if e.StatusCode == 0 {
		return fmt.Sprintf("%s: %s", e.Provider, e.Message)
	}
	return fmt.Sprintf("%s: upstream returned %d: %s", e.Provider, e.StatusCode, e.Message)
}

// Retryable reports whether the same request could plausibly succeed on a
// different provider or a later attempt.
func (e *Error) Retryable() bool {
	switch {
	case e.StatusCode == 0: // transport error: connection refused, timeout, DNS
		return true
	case e.StatusCode == 429: // rate limited
		return true
	case e.StatusCode >= 500: // upstream is having a bad day
		return true
	default: // 400s are our fault; retrying changes nothing
		return false
	}
}
