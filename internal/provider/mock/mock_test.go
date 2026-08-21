package mock

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

func TestChat_EchoesLastMessage(t *testing.T) {
	c := &Client{Latency: 0}

	resp, err := c.Chat(context.Background(), provider.ChatRequest{
		Model: "gpt-4o-mini",
		Messages: []provider.Message{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "middle"},
			{Role: "user", Content: "last one"},
		},
	})
	if err != nil {
		t.Fatalf("Chat() failed: %v", err)
	}

	if len(resp.Choices) != 1 {
		t.Fatalf("len(Choices) = %d, want 1", len(resp.Choices))
	}
	if got := resp.Choices[0].Message.Content; got != "mock reply to: last one" {
		t.Errorf("content = %q, want the last user message echoed", got)
	}
	if resp.Model != "gpt-4o-mini" {
		t.Errorf("Model = %q, want the requested model echoed back", resp.Model)
	}
}

func TestChat_HandlesEmptyConversation(t *testing.T) {
	c := &Client{Latency: 0}

	// No messages at all must not panic on Messages[len-1].
	if _, err := c.Chat(context.Background(), provider.ChatRequest{Model: "m"}); err != nil {
		t.Fatalf("Chat() failed on an empty conversation: %v", err)
	}
}

// TestChat_CancellationBeatsLatency is the point of the mock's select: an
// uninterruptible time.Sleep would make this test take a full second.
func TestChat_CancellationBeatsLatency(t *testing.T) {
	c := &Client{Latency: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call even starts

	start := time.Now()
	_, err := c.Chat(ctx, provider.ChatRequest{Model: "m"})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Errorf("Chat() took %v to honour a cancelled context, want it immediate", elapsed)
	}
}
