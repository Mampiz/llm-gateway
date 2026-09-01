// Package cache stores completed answers so an identical question is not paid
// for twice.
//
// The cache is exact-match: the key is a digest of everything that determines
// the answer. Semantic caching, where "similar" prompts share an entry, needs
// embeddings and a similarity threshold, and gets a request wrong the moment
// the threshold is off. This one either matches or it does not.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

// Cache stores and retrieves completed answers.
//
// A miss is reported as ok=false with a nil error. An error means the cache
// itself failed, which the caller treats as a miss: a broken cache must slow
// the gateway down, never break it.
type Cache interface {
	Get(ctx context.Context, key string) (*provider.ChatResponse, bool, error)
	Set(ctx context.Context, key string, resp *provider.ChatResponse, ttl time.Duration) error
	Close() error
}

// Key digests everything that determines an answer.
//
// scope isolates entries; passing the caller's name makes the cache private
// per client, and passing "" shares it across all of them. Sharing raises the
// hit rate and is what most gateways do, at the cost of a subtle oracle: a
// caller can learn that *somebody* asked a given question by noticing an
// unusually fast reply.
//
// Fields the answer does not depend on are deliberately excluded, and anything
// unrecognised in Extra is included, since a vendor parameter can change the
// answer completely.
func Key(scope string, req provider.ChatRequest) string {
	// A struct rather than concatenation: field boundaries stay unambiguous,
	// so a model named "a" with a message "b" cannot collide with a model
	// named "ab".
	payload := struct {
		Scope       string             `json:"scope"`
		Model       string             `json:"model"`
		Messages    []provider.Message `json:"messages"`
		Temperature *float64           `json:"temperature,omitempty"`
		MaxTokens   *int               `json:"max_tokens,omitempty"`
		Extra       map[string]any     `json:"extra,omitempty"`
	}{
		Scope:       scope,
		Model:       req.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Extra:       req.Extra,
	}

	// json.Marshal sorts map keys, so Extra hashes the same whatever order it
	// arrived in.
	raw, err := json.Marshal(payload)
	if err != nil {
		// Unhashable input cannot be cached, and a key that could collide is
		// worse than no key at all.
		return ""
	}

	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// --- in-memory --------------------------------------------------------------

// Memory is a per-process cache with a bounded number of entries.
//
// Like the in-process rate limiter it is correct for one instance and merely
// unhelpful for several: replicas simply miss on each other's entries, which
// costs money rather than correctness.
type Memory struct {
	max int

	mu      sync.Mutex
	entries map[string]memEntry
	now     func() time.Time
}

type memEntry struct {
	resp      *provider.ChatResponse
	expiresAt time.Time
}

var _ Cache = (*Memory)(nil)

// NewMemory builds a cache holding at most max entries.
func NewMemory(max int) *Memory {
	if max < 1 {
		max = 1000
	}
	return &Memory{max: max, entries: make(map[string]memEntry), now: time.Now}
}

// Get implements Cache.
func (m *Memory) Get(_ context.Context, key string) (*provider.ChatResponse, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.entries[key]
	if !ok {
		return nil, false, nil
	}
	if m.now().After(e.expiresAt) {
		delete(m.entries, key)
		return nil, false, nil
	}
	return e.resp, true, nil
}

// Set implements Cache.
func (m *Memory) Set(_ context.Context, key string, resp *provider.ChatResponse, ttl time.Duration) error {
	if key == "" || resp == nil || ttl <= 0 {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.entries) >= m.max {
		m.evictLocked()
	}
	m.entries[key] = memEntry{resp: resp, expiresAt: m.now().Add(ttl)}
	return nil
}

// evictLocked makes room. Expired entries go first; if none have expired it
// drops an arbitrary one, which map iteration order supplies for free.
//
// This is not an LRU. Tracking recency would mean a linked list and a write on
// every read, and for a cache whose entries expire on a timer anyway the extra
// machinery buys very little.
func (m *Memory) evictLocked() {
	now := m.now()
	for k, e := range m.entries {
		if now.After(e.expiresAt) {
			delete(m.entries, k)
		}
	}
	if len(m.entries) < m.max {
		return
	}
	for k := range m.entries {
		delete(m.entries, k)
		return
	}
}

// Close implements Cache.
func (m *Memory) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = make(map[string]memEntry)
	return nil
}
