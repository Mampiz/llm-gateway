package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// bucketScript is the whole token bucket, evaluated inside Redis.
//
// It runs there rather than in Go for one reason: read-modify-write from N
// replicas over a shared counter is a race. Two gateways reading "1 token
// left" at the same instant would both allow their request and the limit would
// be exactly as leaky as the number of replicas. Redis executes a script
// atomically, so check-and-consume becomes indivisible however many instances
// are asking.
//
// KEYS[1]  bucket key
// ARGV[1]  rate, tokens per second
// ARGV[2]  burst, bucket size
// ARGV[3]  now, milliseconds
// ARGV[4]  ttl, milliseconds
//
// Returns: {allowed, remaining, retry_after_ms}.
const bucketScript = `
local key    = KEYS[1]
local rate   = tonumber(ARGV[1])
local burst  = tonumber(ARGV[2])
local now    = tonumber(ARGV[3])
local ttl    = tonumber(ARGV[4])

local state  = redis.call('HMGET', key, 'tokens', 'last')
local tokens = tonumber(state[1])
local last   = tonumber(state[2])

-- An unseen caller starts with a full bucket: the first request must not be
-- the one that gets refused.
if tokens == nil then
  tokens = burst
  last   = now
end

-- Refill for the time that passed, capped at the bucket size.
local elapsed = math.max(0, now - last) / 1000
tokens = math.min(burst, tokens + elapsed * rate)

local allowed = 0
local retry   = 0

if tokens >= 1 then
  allowed = 1
  tokens  = tokens - 1
else
  -- Round up so a client is never told to retry in 0 ms while still empty.
  retry = math.max(1, math.ceil((1 - tokens) / rate * 1000))
end

redis.call('HSET', key, 'tokens', tokens, 'last', now)
redis.call('PEXPIRE', key, ttl)

return {allowed, math.floor(tokens), retry}
`

// Redis is a token bucket shared by every instance of the gateway.
type Redis struct {
	cfg    Config
	client redis.UniversalClient
	script *redis.Script
	prefix string
}

var _ Limiter = (*Redis)(nil)

// NewRedis builds a distributed limiter against the given Redis URL, e.g.
// redis://localhost:6379/0.
func NewRedis(ctx context.Context, url string, cfg Config) (*Redis, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.TTL <= 0 {
		// Long enough that a bucket outlives its own refill window, short
		// enough that idle callers stop occupying memory.
		cfg.TTL = 10 * time.Minute
	}

	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parsing redis url: %w", err)
	}

	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connecting to redis: %w", err)
	}

	return &Redis{
		cfg:    cfg,
		client: client,
		script: redis.NewScript(bucketScript),
		prefix: "llmgw:ratelimit:",
	}, nil
}

// Allow implements Limiter.
func (r *Redis) Allow(ctx context.Context, key string) (Decision, error) {
	// The clock comes from the caller, not from Redis, so the script stays
	// deterministic and replicable. Gateway instances must therefore agree on
	// the time to within a refill interval, which NTP handles comfortably.
	now := time.Now().UnixMilli()

	raw, err := r.script.Run(ctx, r.client,
		[]string{r.prefix + key},
		r.cfg.Rate, r.cfg.Burst, now, r.cfg.TTL.Milliseconds(),
	).Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("running rate limit script: %w", err)
	}
	if len(raw) != 3 {
		return Decision{}, errors.New("rate limit script returned an unexpected shape")
	}

	allowed, _ := raw[0].(int64)
	remaining, _ := raw[1].(int64)
	retryMS, _ := raw[2].(int64)

	return Decision{
		Allowed:    allowed == 1,
		Limit:      r.cfg.Burst,
		Remaining:  int(remaining),
		RetryAfter: time.Duration(retryMS) * time.Millisecond,
	}, nil
}

// Close implements Limiter.
func (r *Redis) Close() error { return r.client.Close() }
