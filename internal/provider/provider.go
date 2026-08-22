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
	"errors"
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

	// Extra carries request fields the gateway does not model itself, so that
	// vendor-specific parameters survive the trip instead of being silently
	// dropped at the door. It never appears in the canonical JSON: providers
	// decide individually whether any of it applies to their own API.
	Extra map[string]any `json:"-"`
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

// Chunk is one incremental piece of a streamed completion, already translated
// out of whatever dialect the vendor speaks.
//
// Most chunks carry nothing but a Content delta. The lifecycle ones carry no
// text at all: a vendor typically reports the finish reason and the token
// counts in their own final chunks, after the last word.
type Chunk struct {
	// ID and Model identify the completion. Vendors repeat them on the wire in
	// every chunk, and so does this type: a Chunk is self-contained.
	ID    string
	Model string

	// Content is the text delta. Empty on lifecycle-only chunks.
	Content string

	// FinishReason is set on the chunk that closes the completion, in the
	// canonical vocabulary: stop, length, tool_calls.
	FinishReason string

	// Usage is set only when the vendor reports token counts. Many report them
	// once at the very end, and some not at all while streaming, so a nil here
	// means "not reported", not "zero".
	Usage *Usage
}

// Stream is a forward-only cursor over a streamed completion.
//
// It follows the shape of bufio.Scanner and sql.Rows, which is the idiom Go
// uses for "a sequence that can fail halfway through":
//
//	stream, err := p.ChatStream(ctx, req)
//	if err != nil {
//		return err
//	}
//	defer stream.Close()
//
//	for stream.Next() {
//		chunk := stream.Current()
//		...
//	}
//	if err := stream.Err(); err != nil {
//		return err
//	}
//
// The shape matters: Next reports only whether another chunk exists, so a
// clean end and a mid-stream failure both end the loop, and Err afterwards is
// what tells the two apart. Forgetting that check turns a truncated answer
// into a silently successful one.
//
// Nothing here requires a goroutine. When the caller needs to wait on a chunk
// and on something else at the same time, it wraps the stream itself -- once,
// in one place, rather than in every vendor package.
type Stream interface {
	// Next advances to the next chunk, reporting whether one arrived. It
	// returns false at the end of the stream and on error alike.
	Next() bool

	// Current returns the chunk Next just advanced to. It is only valid after
	// Next returned true.
	Current() Chunk

	// Err returns the error that ended the stream, or nil if it ended cleanly.
	Err() error

	// Close releases the underlying connection. It is safe to call more than
	// once and must be called even when the loop ran to completion, which is
	// what makes `defer stream.Close()` the right reflex.
	Close() error
}

// ErrStreamingNotSupported is returned by providers that cannot stream.
var ErrStreamingNotSupported = errors.New("this provider does not support streaming")

// Provider is implemented by every upstream vendor client.
//
// Both methods must honour ctx: if the caller cancels it (client
// disconnected, deadline exceeded), the in-flight upstream request has to be
// torn down rather than left generating tokens nobody will read.
type Provider interface {
	// Name identifies the provider in logs, metrics and errors.
	Name() string

	// Chat performs a single non-streaming completion.
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)

	// ChatStream starts a streamed completion. The error it returns covers
	// only the failure to *start*; anything that goes wrong afterwards
	// surfaces through Stream.Err.
	ChatStream(ctx context.Context, req ChatRequest) (Stream, error)
}

// Error is an upstream failure that carries enough context for the fallback
// logic in phase 5 to decide whether retrying on another provider is worth it.
type Error struct {
	Provider   string
	StatusCode int    // HTTP status returned by the vendor, 0 if the call never completed
	Message    string // vendor-supplied error message, or the transport error
	Err        error  // underlying cause, if any; may be nil
}

// Unwrap exposes the cause to errors.Is and errors.As, so that wrapping a
// context error in an *Error does not hide it from callers testing for
// context.Canceled or context.DeadlineExceeded.
func (e *Error) Unwrap() error { return e.Err }

func (e *Error) Error() string {
	if e.StatusCode == 0 {
		return fmt.Sprintf("%s: %s", e.Provider, e.Message)
	}
	return fmt.Sprintf("%s: upstream returned %d: %s", e.Provider, e.StatusCode, e.Message)
}

// Retryable reports whether the same request could plausibly succeed on a
// different provider or a later attempt.
func (e *Error) Retryable() bool {
	// The caller gave up or ran out of time: another attempt helps nobody.
	if errors.Is(e.Err, context.Canceled) || errors.Is(e.Err, context.DeadlineExceeded) {
		return false
	}
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
