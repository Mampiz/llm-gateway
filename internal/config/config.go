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

		APIKeys:      env("GATEWAY_API_KEYS", ""),
		AuthDisabled: envBool("GATEWAY_AUTH_DISABLED", false),

		RateLimitRPS:   envFloat("GATEWAY_RATE_LIMIT_RPS", 10),
		RateLimitBurst: envInt("GATEWAY_RATE_LIMIT_BURST", 20),
		RedisURL:       env("GATEWAY_REDIS_URL", ""),

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
