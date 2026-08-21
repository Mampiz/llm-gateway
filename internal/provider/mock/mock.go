// Package mock provides a fake provider so the gateway can be exercised
// end to end without an API key and without spending money.
package mock

import (
	"context"
	"fmt"
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
