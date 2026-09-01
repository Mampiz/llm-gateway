// Package config loads the gateway configuration from the environment.
//
// Everything is read once at startup and passed down explicitly, so no other
// package ever calls os.Getenv. That keeps components testable: a test builds
// a Config literal instead of mutating global process state.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	// Addr is the TCP address the HTTP server listens on, e.g. ":8080".
	Addr string

	// Provider selects the upstream backend: "openai" or "mock".
	Provider string

	// OpenAIAPIKey is required when Provider is "openai".
	OpenAIAPIKey string

	// OpenAIBaseURL overrides the OpenAI API root. Empty means the default.
	OpenAIBaseURL string

	// AnthropicAPIKey enables the Anthropic provider when set.
	AnthropicAPIKey string

	// AnthropicBaseURL overrides the Anthropic API root.
	AnthropicBaseURL string

	// AnthropicMaxTokens is the limit sent when a request does not specify
	// one. Anthropic requires the field; the gateway supplies a default so a
	// request valid for an OpenAI model stays valid for a Claude one.
	AnthropicMaxTokens int

	// RequestTimeout bounds a single non-streaming completion. It is applied
	// as a context deadline, never as an http.Client.Timeout.
	RequestTimeout time.Duration

	// StreamIdleTimeout bounds how long a stream may go without producing a
	// single chunk before the gateway gives up. Unlike RequestTimeout it is
	// not a budget for the whole answer: it resets on every chunk, so a long
	// generation is fine as long as it keeps moving.
	StreamIdleTimeout time.Duration

	// StreamMaxDuration caps a whole streamed answer. StreamIdleTimeout
	// catches an upstream that goes quiet; this one catches an upstream that
	// never stops talking.
	StreamMaxDuration time.Duration

	// StreamHeartbeat is how often an idle stream emits an SSE comment, so
	// that proxies and load balancers do not drop a connection that is merely
	// waiting for the model to think.
	StreamHeartbeat time.Duration

	// APIKeys is the raw specification of the gateway's own client keys, as
	// `name:secret` pairs separated by commas.
	APIKeys string

	// AuthDisabled turns off client authentication. It exists so local
	// development against the fake upstream needs no setup, and it has to be
	// asked for explicitly: an unauthenticated gateway is never the default.
	AuthDisabled bool

	// RateLimitRPS is the sustained per-caller allowance in requests per
	// second. Zero or less disables rate limiting entirely.
	RateLimitRPS float64

	// RateLimitBurst is how many requests a caller may make at once after
	// being idle.
	RateLimitBurst int

	// RedisURL points at the Redis that backs the distributed limiter. When
	// empty the limiter lives in this process, which is correct for a single
	// instance and wrong for several.
	RedisURL string

	// FallbackModels maps a requested model to the models to try after it,
	// in order:  gpt-4o-mini:claude-sonnet-5,claude-sonnet-5:gpt-4o-mini
	// Falling back changes the model as well as the provider, because the
	// same model rarely exists on two vendors.
	FallbackModels string

	// RetryAttempts is how many times one provider is tried before the chain
	// moves on, the first attempt included.
	RetryAttempts int

	// RetryBaseDelay is the wait before a second attempt; each further one
	// doubles it, with full jitter applied.
	RetryBaseDelay time.Duration

	// BreakerThreshold is how many consecutive failures take a provider out
	// of rotation.
	BreakerThreshold int

	// BreakerCooldown is how long a tripped provider is left alone before a
	// single probe is allowed through.
	BreakerCooldown time.Duration

	// CacheTTL is how long a completed answer is reused. Zero or less
	// disables caching. Streaming responses are never cached.
	CacheTTL time.Duration

	// CacheScope is "shared" to reuse entries across callers, or "caller" to
	// keep them private. Sharing raises the hit rate at the cost of a subtle
	// oracle: a caller can tell that somebody asked a given question by
	// noticing an unusually fast reply.
	CacheScope string

	// CacheMaxEntries bounds the in-process cache. Ignored when Redis backs
	// it, since Redis does its own eviction.
	CacheMaxEntries int

	// LogLevel is one of debug, info, warn, error.
	LogLevel string
}

// Load reads the environment and validates it.
func Load() (*Config, error) {
	cfg := &Config{
		Addr:          env("GATEWAY_ADDR", ":8080"),
		Provider:      env("GATEWAY_PROVIDER", "mock"),
		OpenAIAPIKey:  env("OPENAI_API_KEY", ""),
		OpenAIBaseURL: env("OPENAI_BASE_URL", ""),

		AnthropicAPIKey:    env("ANTHROPIC_API_KEY", ""),
		AnthropicBaseURL:   env("ANTHROPIC_BASE_URL", ""),
		AnthropicMaxTokens: envInt("ANTHROPIC_DEFAULT_MAX_TOKENS", 4096),

		RequestTimeout: envDuration("GATEWAY_REQUEST_TIMEOUT", 60*time.Second),

		StreamIdleTimeout: envDuration("GATEWAY_STREAM_IDLE_TIMEOUT", 60*time.Second),
		StreamHeartbeat:   envDuration("GATEWAY_STREAM_HEARTBEAT", 15*time.Second),
		StreamMaxDuration: envDuration("GATEWAY_STREAM_MAX_DURATION", 10*time.Minute),

		APIKeys:      env("GATEWAY_API_KEYS", ""),
		AuthDisabled: envBool("GATEWAY_AUTH_DISABLED", false),

		RateLimitRPS:   envFloat("GATEWAY_RATE_LIMIT_RPS", 10),
		RateLimitBurst: envInt("GATEWAY_RATE_LIMIT_BURST", 20),
		RedisURL:       env("GATEWAY_REDIS_URL", ""),

		FallbackModels:   env("GATEWAY_FALLBACK_MODELS", ""),
		RetryAttempts:    envInt("GATEWAY_RETRY_ATTEMPTS", 2),
		RetryBaseDelay:   envDuration("GATEWAY_RETRY_BASE_DELAY", 200*time.Millisecond),
		BreakerThreshold: envInt("GATEWAY_BREAKER_THRESHOLD", 5),
		BreakerCooldown:  envDuration("GATEWAY_BREAKER_COOLDOWN", 30*time.Second),

		CacheTTL:        envDuration("GATEWAY_CACHE_TTL", 0),
		CacheScope:      env("GATEWAY_CACHE_SCOPE", "shared"),
		CacheMaxEntries: envInt("GATEWAY_CACHE_MAX_ENTRIES", 1000),

		LogLevel: env("GATEWAY_LOG_LEVEL", "info"),
	}

	switch cfg.Provider {
	case "mock":
	case "openai":
		if cfg.OpenAIAPIKey == "" {
			return nil, fmt.Errorf("GATEWAY_PROVIDER=openai requires OPENAI_API_KEY to be set")
		}
	case "anthropic":
		if cfg.AnthropicAPIKey == "" {
			return nil, fmt.Errorf("GATEWAY_PROVIDER=anthropic requires ANTHROPIC_API_KEY to be set")
		}
	default:
		return nil, fmt.Errorf("unknown GATEWAY_PROVIDER %q (want \"openai\", \"anthropic\" or \"mock\")", cfg.Provider)
	}

	// A duration of zero parses cleanly and then makes the gateway useless: a
	// request timeout of zero expires every request as it starts. Substituting
	// a default here would hide the mistake; naming it does not.
	for _, d := range []struct {
		name  string
		value time.Duration
	}{
		{"GATEWAY_REQUEST_TIMEOUT", cfg.RequestTimeout},
		{"GATEWAY_STREAM_IDLE_TIMEOUT", cfg.StreamIdleTimeout},
		{"GATEWAY_STREAM_HEARTBEAT", cfg.StreamHeartbeat},
		{"GATEWAY_STREAM_MAX_DURATION", cfg.StreamMaxDuration},
		{"GATEWAY_BREAKER_COOLDOWN", cfg.BreakerCooldown},
	} {
		if d.value <= 0 {
			return nil, fmt.Errorf("%s must be a positive duration, got %v", d.name, d.value)
		}
	}

	// A heartbeat that never arrives before the idle timeout is a heartbeat
	// that cannot do its job, and one that fires after the cap is dead weight.
	if cfg.StreamHeartbeat >= cfg.StreamIdleTimeout {
		return nil, fmt.Errorf("GATEWAY_STREAM_HEARTBEAT (%v) must be shorter than GATEWAY_STREAM_IDLE_TIMEOUT (%v)",
			cfg.StreamHeartbeat, cfg.StreamIdleTimeout)
	}

	if cfg.CacheTTL < 0 {
		return nil, fmt.Errorf("GATEWAY_CACHE_TTL must not be negative, got %v", cfg.CacheTTL)
	}
	if cfg.RetryBaseDelay < 0 {
		return nil, fmt.Errorf("GATEWAY_RETRY_BASE_DELAY must not be negative, got %v", cfg.RetryBaseDelay)
	}

	if !cfg.AuthDisabled && strings.TrimSpace(cfg.APIKeys) == "" {
		return nil, fmt.Errorf("GATEWAY_API_KEYS is empty: set it, or set GATEWAY_AUTH_DISABLED=true to run without authentication")
	}

	return cfg, nil
}

func envBool(key string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if f, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(key)), 64); err == nil {
		return f
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n > 0 {
		return n
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	// Accept both "45s" and a bare number of seconds.
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return fallback
}
