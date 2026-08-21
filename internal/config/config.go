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

	// RequestTimeout bounds a single non-streaming completion. It is applied
	// as a context deadline, never as an http.Client.Timeout.
	RequestTimeout time.Duration

	// LogLevel is one of debug, info, warn, error.
	LogLevel string
}

// Load reads the environment and validates it.
func Load() (*Config, error) {
	cfg := &Config{
		Addr:           env("GATEWAY_ADDR", ":8080"),
		Provider:       env("GATEWAY_PROVIDER", "mock"),
		OpenAIAPIKey:   env("OPENAI_API_KEY", ""),
		OpenAIBaseURL:  env("OPENAI_BASE_URL", ""),
		RequestTimeout: envDuration("GATEWAY_REQUEST_TIMEOUT", 60*time.Second),
		LogLevel:       env("GATEWAY_LOG_LEVEL", "info"),
	}

	switch cfg.Provider {
	case "mock":
	case "openai":
		if cfg.OpenAIAPIKey == "" {
			return nil, fmt.Errorf("GATEWAY_PROVIDER=openai requires OPENAI_API_KEY to be set")
		}
	default:
		return nil, fmt.Errorf("unknown GATEWAY_PROVIDER %q (want \"openai\" or \"mock\")", cfg.Provider)
	}

	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
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
