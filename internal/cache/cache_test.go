package cache

import (
	"context"
	"testing"
	"time"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

func req(model, content string) provider.ChatRequest {
	return provider.ChatRequest{
		Model:    model,
		Messages: []provider.Message{{Role: "user", Content: content}},
	}
}

func resp(id string) *provider.ChatResponse {
	return &provider.ChatResponse{
		ID:      id,
		Object:  "chat.completion",
		Model:   "gpt-4o-mini",
		Choices: []provider.Choice{{Message: provider.Message{Role: "assistant", Content: "hi"}}},
		Usage:   provider.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
	}
}

func TestKey_IsStableAndDistinguishing(t *testing.T) {
	base := req("gpt-4o-mini", "hello")

	// Two separate calls, so this is a real determinism check rather than an
	// expression the compiler could fold away.
	first, second := Key("", base), Key("", base)
	if first != second {
		t.Errorf("the same request produced two keys: %q and %q", first, second)
	}
	if first == "" {
		t.Error("Key() returned an empty string for a valid request")
	}

	temp := 0.5
	withTemp := base
	withTemp.Temperature = &temp

	limit := 100
	withLimit := base
	withLimit.MaxTokens = &limit

	different := map[string]provider.ChatRequest{
		"another model":   req("gpt-4o", "hello"),
		"another message": req("gpt-4o-mini", "goodbye"),
		"a temperature":   withTemp,
		"a token limit":   withLimit,
	}

	for name, other := range different {
		t.Run(name, func(t *testing.T) {
			if Key("", base) == Key("", other) {
				t.Errorf("%s did not change the key, so a cache hit would return the wrong answer", name)
			}
		})
	}
}

// A vendor parameter can change the answer completely, so it belongs in the
// key even though the gateway does not model it.
func TestKey_IncludesVendorExtras(t *testing.T) {
	base := req("gpt-4o-mini", "hello")

	withExtra := base
	withExtra.Extra = map[string]any{"top_p": 0.1}

	if Key("", base) == Key("", withExtra) {
		t.Error("an unmodelled parameter did not change the key")
	}
}

// Map iteration order must not leak into the digest.
func TestKey_IsOrderIndependentForExtras(t *testing.T) {
	a := req("gpt-4o-mini", "hello")
	a.Extra = map[string]any{"top_p": 0.1, "user": "u1", "seed": 7}

	b := req("gpt-4o-mini", "hello")
	b.Extra = map[string]any{"seed": 7, "user": "u1", "top_p": 0.1}

	if Key("", a) != Key("", b) {
		t.Error("the same extras in a different order produced different keys")
	}
}

// Field boundaries must stay unambiguous, or "a"+"bc" collides with "ab"+"c".
func TestKey_DoesNotCollideAcrossFields(t *testing.T) {
	a := req("gpt", "4o-hello")
	b := req("gpt-4o", "hello")

	if Key("", a) == Key("", b) {
		t.Error("two different requests share a key")
	}
}

func TestKey_ScopeIsolates(t *testing.T) {
	r := req("gpt-4o-mini", "hello")

	if Key("alice", r) == Key("bob", r) {
		t.Error("two callers share a key under a per-caller scope")
	}
	if Key("", r) == Key("alice", r) {
		t.Error("the shared and scoped keys are the same")
	}
}

func TestMemory_StoresAndReturns(t *testing.T) {
	c := NewMemory(10)
	key := Key("", req("gpt-4o-mini", "hello"))

	if _, ok, err := c.Get(context.Background(), key); err != nil || ok {
		t.Fatalf("Get() on an empty cache = %v, %v; want a clean miss", ok, err)
	}

	if err := c.Set(context.Background(), key, resp("chatcmpl-1"), time.Minute); err != nil {
		t.Fatalf("Set() failed: %v", err)
	}

	got, ok, err := c.Get(context.Background(), key)
	if err != nil || !ok {
		t.Fatalf("Get() = %v, %v; want a hit", ok, err)
	}
	if got.ID != "chatcmpl-1" {
		t.Errorf("ID = %q, want the stored answer", got.ID)
	}
}

func TestMemory_Expires(t *testing.T) {
	c := NewMemory(10)
	clk := time.Now()
	c.now = func() time.Time { return clk }

	key := "k"
	if err := c.Set(context.Background(), key, resp("chatcmpl-1"), time.Minute); err != nil {
		t.Fatalf("Set() failed: %v", err)
	}

	clk = clk.Add(59 * time.Second)
	if _, ok, _ := c.Get(context.Background(), key); !ok {
		t.Error("the entry vanished before its TTL")
	}

	clk = clk.Add(2 * time.Second)
	if _, ok, _ := c.Get(context.Background(), key); ok {
		t.Error("an expired entry was served")
	}
}

// A bounded cache is the difference between a cache and a memory leak.
func TestMemory_StaysBounded(t *testing.T) {
	const max = 5
	c := NewMemory(max)

	for i := range max * 4 {
		key := Key("", req("gpt-4o-mini", string(rune('a'+i))))
		if err := c.Set(context.Background(), key, resp("x"), time.Minute); err != nil {
			t.Fatalf("Set() failed: %v", err)
		}
	}

	c.mu.Lock()
	size := len(c.entries)
	c.mu.Unlock()

	if size > max {
		t.Errorf("cache holds %d entries, want at most %d", size, max)
	}
}

func TestMemory_IgnoresUnusableWrites(t *testing.T) {
	c := NewMemory(10)
	ctx := context.Background()

	tests := []struct {
		name string
		key  string
		resp *provider.ChatResponse
		ttl  time.Duration
	}{
		{"empty key", "", resp("x"), time.Minute},
		{"nil response", "k", nil, time.Minute},
		{"zero ttl", "k", resp("x"), 0},
		{"negative ttl", "k", resp("x"), -time.Second},
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

func TestMemory_Close(t *testing.T) {
	c := NewMemory(10)
	if err := c.Set(context.Background(), "k", resp("x"), time.Minute); err != nil {
		t.Fatalf("Set() failed: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	if _, ok, _ := c.Get(context.Background(), "k"); ok {
		t.Error("an entry survived Close()")
	}
}
