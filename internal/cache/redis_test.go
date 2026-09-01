//go:build integration

// These tests need a real Redis. A fake store would exercise the interface but
// not the serialisation round trip or the TTL, which are the parts that break.
//
//	docker compose up -d redis
//	make test-integration
package cache

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

func redisURL() string {
	if url := os.Getenv("REDIS_URL"); url != "" {
		return url
	}
	return "redis://localhost:6379/0"
}

func newRedisCache(t *testing.T) *Redis {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := NewRedis(ctx, redisURL())
	if err != nil {
		t.Skipf("no Redis at %s (%v). Start one with: docker compose up -d redis", redisURL(), err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func uniqueKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-%s-%d", t.Name(), time.Now().UnixNano())
}

// The whole answer has to survive the round trip, not just its text: usage
// feeds the cost metrics and the finish reason tells a client whether the
// answer was cut short.
func TestRedis_RoundTripsTheWholeAnswer(t *testing.T) {
	c := newRedisCache(t)
	key := uniqueKey(t)
	ctx := context.Background()

	want := &provider.ChatResponse{
		ID:      "chatcmpl-round",
		Object:  "chat.completion",
		Created: 1700000000,
		Model:   "gpt-4o-mini",
		Choices: []provider.Choice{{
			Index:        0,
			Message:      provider.Message{Role: "assistant", Content: "hola mundo"},
			FinishReason: "length",
		}},
		Usage: provider.Usage{PromptTokens: 11, CompletionTokens: 4, TotalTokens: 15},
	}

	if err := c.Set(ctx, key, want, time.Minute); err != nil {
		t.Fatalf("Set() failed: %v", err)
	}

	got, ok, err := c.Get(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Get() = %v, %v; want a hit", ok, err)
	}

	if got.ID != want.ID || got.Created != want.Created || got.Model != want.Model {
		t.Errorf("envelope = %+v, want %+v", got, want)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %v, want one", got.Choices)
	}
	if got.Choices[0].Message.Content != "hola mundo" {
		t.Errorf("content = %q, want it preserved", got.Choices[0].Message.Content)
	}
	if got.Choices[0].FinishReason != "length" {
		t.Errorf("finish reason = %q, want it preserved", got.Choices[0].FinishReason)
	}
	if got.Usage != want.Usage {
		t.Errorf("usage = %+v, want %+v", got.Usage, want.Usage)
	}
}

func TestRedis_MissesOnAnUnknownKey(t *testing.T) {
	c := newRedisCache(t)

	resp, ok, err := c.Get(context.Background(), uniqueKey(t))
	if err != nil {
		t.Fatalf("Get() = %v, want a clean miss rather than an error", err)
	}
	if ok || resp != nil {
		t.Errorf("Get() = %v, %v; want a miss", resp, ok)
	}
}

func TestRedis_Expires(t *testing.T) {
	c := newRedisCache(t)
	key := uniqueKey(t)
	ctx := context.Background()

	if err := c.Set(ctx, key, &provider.ChatResponse{ID: "x"}, 500*time.Millisecond); err != nil {
		t.Fatalf("Set() failed: %v", err)
	}
	if _, ok, _ := c.Get(ctx, key); !ok {
		t.Fatal("the entry was not stored")
	}

	time.Sleep(700 * time.Millisecond)

	if _, ok, _ := c.Get(ctx, key); ok {
		t.Error("an expired entry was served")
	}
}

// A corrupt entry must read as a miss: the answer this request is about to
// fetch will overwrite it, and failing instead would break a request over
// something that heals itself.
func TestRedis_CorruptEntryReadsAsAMiss(t *testing.T) {
	c := newRedisCache(t)
	key := uniqueKey(t)
	ctx := context.Background()

	if err := c.client.Set(ctx, c.prefix+key, "not json at all", time.Minute).Err(); err != nil {
		t.Fatalf("seeding a corrupt entry failed: %v", err)
	}

	resp, ok, err := c.Get(ctx, key)
	if err != nil {
		t.Errorf("Get() = %v, want a miss rather than an error", err)
	}
	if ok || resp != nil {
		t.Errorf("Get() = %v, %v; want a miss", resp, ok)
	}
}

func TestRedis_IgnoresUnusableWrites(t *testing.T) {
	c := newRedisCache(t)
	ctx := context.Background()

	tests := []struct {
		name string
		key  string
		resp *provider.ChatResponse
		ttl  time.Duration
	}{
		{"empty key", "", &provider.ChatResponse{ID: "x"}, time.Minute},
		{"nil response", uniqueKey(t), nil, time.Minute},
		{"zero ttl", uniqueKey(t), &provider.ChatResponse{ID: "x"}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := c.Set(ctx, tt.key, tt.resp, tt.ttl); err != nil {
				t.Fatalf("Set() = %v, want it ignored quietly", err)
			}
			if _, ok, _ := c.Get(ctx, tt.key); ok {
				t.Error("an unusable write was stored")
			}
		})
	}
}

// Two gateway instances share one store: an answer paid for by one replica
// must be reusable by another, which is the entire reason to put the cache in
// Redis rather than in memory.
func TestRedis_IsSharedAcrossInstances(t *testing.T) {
	a := newRedisCache(t)
	b := newRedisCache(t)
	key := uniqueKey(t)
	ctx := context.Background()

	if err := a.Set(ctx, key, &provider.ChatResponse{ID: "paid-for-by-a"}, time.Minute); err != nil {
		t.Fatalf("Set() failed: %v", err)
	}

	got, ok, err := b.Get(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Get() from the second instance = %v, %v; want a hit", ok, err)
	}
	if got.ID != "paid-for-by-a" {
		t.Errorf("ID = %q, want the entry the first instance stored", got.ID)
	}
}
