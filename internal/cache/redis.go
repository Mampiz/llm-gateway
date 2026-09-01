package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

// Redis is a cache shared by every instance of the gateway.
//
// Unlike the rate limiter this needs no Lua: storing an answer twice is
// harmless, so a plain GET and SET are enough. The limiter needed atomicity
// because two replicas both deciding "one token left" is wrong; two replicas
// both caching the same answer is merely redundant.
type Redis struct {
	client redis.UniversalClient
	prefix string
}

var _ Cache = (*Redis)(nil)

// NewRedis connects to the Redis at url, e.g. redis://localhost:6379/0.
func NewRedis(ctx context.Context, url string) (*Redis, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parsing redis url: %w", err)
	}

	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connecting to redis: %w", err)
	}

	return &Redis{client: client, prefix: "llmgw:cache:"}, nil
}

// Get implements Cache.
func (r *Redis) Get(ctx context.Context, key string) (*provider.ChatResponse, bool, error) {
	raw, err := r.client.Get(ctx, r.prefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading cache: %w", err)
	}

	var resp provider.ChatResponse
	if decodeErr := json.Unmarshal(raw, &resp); decodeErr != nil {
		// A corrupt entry is a miss, not a failure: the answer this request is
		// about to fetch will overwrite it. Reporting an error instead would
		// only add noise to a situation that heals itself.
		return nil, false, nil //nolint:nilerr // a corrupt entry is a miss by design
	}
	return &resp, true, nil
}

// Set implements Cache.
func (r *Redis) Set(ctx context.Context, key string, resp *provider.ChatResponse, ttl time.Duration) error {
	if key == "" || resp == nil || ttl <= 0 {
		return nil
	}

	raw, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("encoding cache entry: %w", err)
	}
	if err := r.client.Set(ctx, r.prefix+key, raw, ttl).Err(); err != nil {
		return fmt.Errorf("writing cache: %w", err)
	}
	return nil
}

// Close implements Cache.
func (r *Redis) Close() error { return r.client.Close() }
