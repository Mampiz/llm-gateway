// Package mock provides a fake provider so the gateway can be exercised
// end to end without an API key and without spending money.
package mock

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

// Client is a canned provider. Latency simulates upstream think time so
// timeouts and cancellation can be tested for real.
type Client struct {
	Latency time.Duration
}

var _ provider.Provider = (*Client)(nil)

// New returns a mock provider with a small artificial latency.
func New() *Client { return &Client{Latency: 300 * time.Millisecond} }

// Name implements provider.Provider.
func (c *Client) Name() string { return "mock" }

// Chat implements provider.Provider. It echoes the last user message back.
//
// The select is the point of interest: instead of time.Sleep, which is
// uninterruptible, it races the artificial latency against ctx.Done() so a
// cancelled request returns immediately with the context's error.
func (c *Client) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	select {
	case <-time.After(c.Latency):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	var last string
	if n := len(req.Messages); n > 0 {
		last = req.Messages[n-1].Content
	}

	return &provider.ChatResponse{
		ID:      "chatcmpl-mock-" + fmt.Sprint(time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []provider.Choice{{
			Index:        0,
			Message:      provider.Message{Role: "assistant", Content: "mock reply to: " + last},
			FinishReason: "stop",
		}},
		Usage: provider.Usage{PromptTokens: 10, CompletionTokens: 8, TotalTokens: 18},
	}, nil
}

// ChatStream implements provider.Provider. It emits the same answer as Chat,
// one word at a time.
//
// This is also the reference implementation of the Stream contract: the
// smallest thing that satisfies it correctly, worth reading before writing a
// real one.
func (c *Client) ChatStream(ctx context.Context, req provider.ChatRequest) (provider.Stream, error) {
	var last string
	if n := len(req.Messages); n > 0 {
		last = req.Messages[n-1].Content
	}

	id := "chatcmpl-mock-" + fmt.Sprint(time.Now().UnixNano())

	var chunks []provider.Chunk
	for _, word := range strings.Fields("mock reply to: " + last) {
		chunks = append(chunks, provider.Chunk{
			ID:      id,
			Model:   req.Model,
			Content: word + " ",
		})
	}
	// The closing chunk carries no text: the finish reason and the token
	// counts arrive after the last word, exactly as real vendors send them.
	chunks = append(chunks, provider.Chunk{
		ID:           id,
		Model:        req.Model,
		FinishReason: "stop",
		Usage:        &provider.Usage{PromptTokens: 10, CompletionTokens: len(chunks), TotalTokens: 10 + len(chunks)},
	})

	return &stream{ctx: ctx, chunks: chunks, delay: c.Latency / 4}, nil
}

// stream is a slice-backed provider.Stream.
type stream struct {
	ctx     context.Context
	chunks  []provider.Chunk
	next    int
	current provider.Chunk
	delay   time.Duration
	err     error
}

var _ provider.Stream = (*stream)(nil)

// Next advances to the next chunk. Note the two ways it returns false: the
// slice ran out (clean end, Err stays nil) or the context was cancelled
// (Err is set). Distinguishing them is the caller's job, via Err.
func (s *stream) Next() bool {
	if s.err != nil || s.next >= len(s.chunks) {
		return false
	}
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-s.ctx.Done():
			s.err = s.ctx.Err()
			return false
		}
	}
	s.current = s.chunks[s.next]
	s.next++
	return true
}

func (s *stream) Current() provider.Chunk { return s.current }

func (s *stream) Err() error { return s.err }

// Close has nothing to release here, but it still has to exist and stay safe
// to call twice: callers are meant to `defer stream.Close()` unconditionally.
func (s *stream) Close() error {
	s.next = len(s.chunks)
	return nil
}
