//go:build integration

// These tests need a real Redis. The Lua script is the whole point of the
// distributed limiter and a fake would not exercise what makes it correct, so
// there is nothing to gain from mocking it.
//
//	docker compose up -d redis
//	make test-integration
package ratelimit

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func redisURL() string {
	if url := os.Getenv("REDIS_URL"); url != "" {
		return url
	}
	return "redis://localhost:6379/0"
}

func newRedisLimiter(t *testing.T, cfg Config) *Redis {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r, err := NewRedis(ctx, redisURL(), cfg)
	if err != nil {
		t.Skipf("no Redis at %s (%v). Start one with: docker compose up -d redis", redisURL(), err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// uniqueKey keeps runs from interfering with each other and with leftovers.
func uniqueKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-%s-%d", t.Name(), time.Now().UnixNano())
}

func TestRedis_StartsFullThenDenies(t *testing.T) {
	cfg := Config{Rate: 2, Burst: 5, TTL: time.Minute}
	r := newRedisLimiter(t, cfg)
	key := uniqueKey(t)

	for i := range cfg.Burst {
		d, err := r.Allow(context.Background(), key)
		if err != nil {
			t.Fatalf("Allow() failed: %v", err)
		}
		if !d.Allowed {
			t.Fatalf("request %d denied, want the full burst from cold", i+1)
		}
		if d.Limit != cfg.Burst {
			t.Errorf("Limit = %d, want %d", d.Limit, cfg.Burst)
		}
	}

	d, err := r.Allow(context.Background(), key)
	if err != nil {
		t.Fatalf("Allow() failed: %v", err)
	}
	if d.Allowed {
		t.Error("a request past the burst was allowed")
	}
	if d.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v, want a positive hint", d.RetryAfter)
	}
}

func TestRedis_RefillsOverTime(t *testing.T) {
	cfg := Config{Rate: 20, Burst: 2, TTL: time.Minute}
	r := newRedisLimiter(t, cfg)
	key := uniqueKey(t)

	for range cfg.Burst {
		r.Allow(context.Background(), key) //nolint:errcheck // drained on purpose
	}
	if d, _ := r.Allow(context.Background(), key); d.Allowed {
		t.Fatal("the bucket was not empty")
	}

	// At 20 per second, 150ms is comfortably more than one token.
	time.Sleep(150 * time.Millisecond)

	if d, _ := r.Allow(context.Background(), key); !d.Allowed {
		t.Error("no token after 150ms at 20/s")
	}
}

// This is the test the whole Lua script exists for.
//
// Two limiters against the same Redis stand in for two gateway replicas. If
// check-and-consume were read-modify-write in Go, both would read the same
// token count and both would allow, and the effective limit would multiply by
// the number of instances. Running the bucket inside Redis makes the operation
// indivisible however many are asking.
func TestRedis_SharesOneBucketAcrossInstances(t *testing.T) {
	cfg := Config{Rate: 1, Burst: 10, TTL: time.Minute}

	a := newRedisLimiter(t, cfg)
	b := newRedisLimiter(t, cfg)
	key := uniqueKey(t)

	const attemptsEach = 40

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0

	for _, limiter := range []*Redis{a, b} {
		for range attemptsEach {
			wg.Add(1)
			go func(l *Redis) {
				defer wg.Done()
				if d, err := l.Allow(context.Background(), key); err == nil && d.Allowed {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}(limiter)
		}
	}
	wg.Wait()

	// One second of refill at 1/s could add at most a token or two while the
	// goroutines run, so allow a small margin over the burst -- but nothing
	// close to twice it, which is what a non-atomic implementation gives.
	if allowed < cfg.Burst || allowed > cfg.Burst+2 {
		t.Errorf("allowed %d of %d across two instances, want about the burst %d: "+
			"the bucket is not shared atomically", allowed, attemptsEach*2, cfg.Burst)
	}
}

func TestRedis_IsolatesCallers(t *testing.T) {
	cfg := Config{Rate: 1, Burst: 3, TTL: time.Minute}
	r := newRedisLimiter(t, cfg)

	noisy, quiet := uniqueKey(t)+"-noisy", uniqueKey(t)+"-quiet"

	for range cfg.Burst {
		r.Allow(context.Background(), noisy) //nolint:errcheck // drained on purpose
	}
	if d, _ := r.Allow(context.Background(), noisy); d.Allowed {
		t.Fatal("the noisy caller was not exhausted")
	}

	if d, _ := r.Allow(context.Background(), quiet); !d.Allowed {
		t.Error("a second caller was denied because the first spent its allowance")
	}
}

// Buckets must expire, or Redis grows one key per caller forever.
func TestRedis_BucketsExpire(t *testing.T) {
	cfg := Config{Rate: 1, Burst: 1, TTL: 2 * time.Second}
	r := newRedisLimiter(t, cfg)
	key := uniqueKey(t)

	if _, err := r.Allow(context.Background(), key); err != nil {
		t.Fatalf("Allow() failed: %v", err)
	}

	ttl, err := r.client.PTTL(context.Background(), r.prefix+key).Result()
	if err != nil {
		t.Fatalf("PTTL failed: %v", err)
	}
	if ttl <= 0 {
		t.Errorf("PTTL = %v, want the bucket to carry an expiry", ttl)
	}
	if ttl > cfg.TTL {
		t.Errorf("PTTL = %v, want at most the configured %v", ttl, cfg.TTL)
	}
}
