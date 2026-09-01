package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mampiz/llm-gateway/internal/cache"
	"github.com/Mampiz/llm-gateway/internal/provider"
)

// countingProvider records how many times an upstream call actually happened,
// which is the only thing a cache is judged on.
func countingProvider(calls *atomic.Int32, delay time.Duration) *stubProvider {
	return &stubProvider{
		chat: func(ctx context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
			calls.Add(1)
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return okResponse(), nil
		},
	}
}

func cachingHandler(t *testing.T, p provider.Provider, ttl time.Duration, scope string) http.Handler {
	t.Helper()

	reg := provider.NewRegistry()
	reg.Register(p)
	if err := reg.SetDefault(p.Name()); err != nil {
		t.Fatalf("registry: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(reg, logger, 5*time.Second, "test").
		WithCache(cache.NewMemory(100), ttl, scope).
		Handler()
}

func TestCache_SecondIdenticalRequestIsServedFromTheCache(t *testing.T) {
	var calls atomic.Int32
	h := cachingHandler(t, countingProvider(&calls, 0), time.Minute, "shared")

	first := do(h, http.MethodPost, "/v1/chat/completions", validBody)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.Code)
	}
	if got := first.Header().Get(cacheHeader); got != cacheMiss {
		t.Errorf("first %s = %q, want MISS", cacheHeader, got)
	}

	second := do(h, http.MethodPost, "/v1/chat/completions", validBody)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", second.Code)
	}
	if got := second.Header().Get(cacheHeader); got != cacheHit {
		t.Errorf("second %s = %q, want HIT", cacheHeader, got)
	}

	if calls.Load() != 1 {
		t.Errorf("the provider was called %d times, want 1", calls.Load())
	}
	if first.Body.String() != second.Body.String() {
		t.Error("the cached answer differs from the original")
	}
}

func TestCache_DifferentRequestsMiss(t *testing.T) {
	var calls atomic.Int32
	h := cachingHandler(t, countingProvider(&calls, 0), time.Minute, "shared")

	do(h, http.MethodPost, "/v1/chat/completions", validBody)
	do(h, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"something else"}]}`)

	if calls.Load() != 2 {
		t.Errorf("the provider was called %d times, want 2 for two different questions", calls.Load())
	}
}

// A cold cache under a burst of the same question should fetch one answer,
// not one per request. That is what singleflight buys.
func TestCache_CollapsesIdenticalConcurrentRequests(t *testing.T) {
	var calls atomic.Int32
	// Slow enough that every goroutine is waiting before the first returns.
	h := cachingHandler(t, countingProvider(&calls, 150*time.Millisecond), time.Minute, "shared")

	const concurrent = 20
	var wg sync.WaitGroup
	codes := make([]int, concurrent)

	for i := range concurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = do(h, http.MethodPost, "/v1/chat/completions", validBody).Code
		}()
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, code)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("the provider was called %d times for %d identical concurrent requests, want 1",
			got, concurrent)
	}
}

// Turning the cache off must leave one code path, not a half-configured one.
func TestCache_DisabledAlwaysCallsTheProvider(t *testing.T) {
	var calls atomic.Int32

	reg := provider.NewRegistry()
	reg.Register(countingProvider(&calls, 0))
	if err := reg.SetDefault("stub"); err != nil {
		t.Fatalf("registry: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(reg, logger, time.Second, "test").Handler()

	do(h, http.MethodPost, "/v1/chat/completions", validBody)
	do(h, http.MethodPost, "/v1/chat/completions", validBody)

	if calls.Load() != 2 {
		t.Errorf("the provider was called %d times with caching off, want 2", calls.Load())
	}
}

// A broken cache must make the gateway slower, never broken.
func TestCache_FailsOpenWhenTheStoreBreaks(t *testing.T) {
	var calls atomic.Int32

	reg := provider.NewRegistry()
	reg.Register(countingProvider(&calls, 0))
	if err := reg.SetDefault("stub"); err != nil {
		t.Fatalf("registry: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(reg, logger, time.Second, "test").
		WithCache(brokenCache{}, time.Minute, "shared").
		Handler()

	for i := range 2 {
		if rec := do(h, http.MethodPost, "/v1/chat/completions", validBody); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 despite the broken cache", i+1, rec.Code)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("the provider was called %d times, want every request served", calls.Load())
	}
}

// brokenCache stands in for a store that is down.
type brokenCache struct{}

func (brokenCache) Get(context.Context, string) (*provider.ChatResponse, bool, error) {
	return nil, false, io.ErrUnexpectedEOF
}
func (brokenCache) Set(context.Context, string, *provider.ChatResponse, time.Duration) error {
	return io.ErrUnexpectedEOF
}
func (brokenCache) Close() error { return nil }

// Streamed answers are not cached: replaying one would need the frame timing
// too, and accumulating while forwarding adds a failure mode to the hot path.
func TestCache_LeavesStreamingAlone(t *testing.T) {
	var calls atomic.Int32

	p := streamingProvider(sampleChunks(), nil, 0)
	p.chat = func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
		calls.Add(1)
		return okResponse(), nil
	}

	reg := provider.NewRegistry()
	reg.Register(p)
	if err := reg.SetDefault("stub"); err != nil {
		t.Fatalf("registry: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(reg, logger, time.Second, "test").
		WithCache(cache.NewMemory(100), time.Minute, "shared").
		Handler()

	for range 2 {
		if rec := do(h, http.MethodPost, "/v1/chat/completions", streamBody); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	}
	// Both requests went upstream; neither carried a cache header.
	if rec := do(h, http.MethodPost, "/v1/chat/completions", streamBody); rec.Header().Get(cacheHeader) != "" {
		t.Errorf("%s = %q on a stream, want it absent", cacheHeader, rec.Header().Get(cacheHeader))
	}
}

// singleflight collapses several callers onto one upstream call, and that call
// runs with whichever context happened to arrive first. If the leader walks
// away, everyone waiting behind it must not be dragged down with it: they are
// still connected and still waiting for an answer.
func TestCache_LeaderCancellationDoesNotFailTheFollowers(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})

	p := &stubProvider{
		chat: func(ctx context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
			calls.Add(1)
			select {
			case <-release:
				return okResponse(), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}

	reg := provider.NewRegistry()
	reg.Register(p)
	if err := reg.SetDefault("stub"); err != nil {
		t.Fatalf("registry: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(reg, logger, 5*time.Second, "test").
		WithCache(cache.NewMemory(100), time.Minute, "shared").
		Handler()

	// The leader, whose client gives up mid-flight.
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody)).
			WithContext(leaderCtx)
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()

	// A follower that arrives while the leader is still in flight and stays.
	time.Sleep(50 * time.Millisecond)
	followerCode := make(chan int, 1)
	go func() {
		followerCode <- do(h, http.MethodPost, "/v1/chat/completions", validBody).Code
	}()

	// The leader gives up, then the upstream answers. Releasing before waiting
	// on the leader matters: it is still parked inside the shared call, so
	// waiting first would deadlock the test rather than the code.
	time.Sleep(50 * time.Millisecond)
	cancelLeader()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-leaderDone

	select {
	case code := <-followerCode:
		if code != http.StatusOK {
			t.Errorf("the follower got %d after the leader walked away, want 200: "+
				"it was still connected and still waiting", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the follower never finished")
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("the provider was called %d times, want 1", got)
	}
}
