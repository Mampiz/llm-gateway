// Command gateway runs the LLM gateway HTTP server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Mampiz/llm-gateway/internal/auth"
	"github.com/Mampiz/llm-gateway/internal/config"
	"github.com/Mampiz/llm-gateway/internal/provider"
	"github.com/Mampiz/llm-gateway/internal/provider/anthropic"
	"github.com/Mampiz/llm-gateway/internal/provider/mock"
	"github.com/Mampiz/llm-gateway/internal/provider/openai"
	"github.com/Mampiz/llm-gateway/internal/ratelimit"
	"github.com/Mampiz/llm-gateway/internal/server"
)

// version is stamped at build time with -ldflags "-X main.version=...".
// It stays "dev" for local builds.
var version = "dev"

func main() {
	// A tiny escape hatch so issuing a key needs no other tooling installed.
	if len(os.Args) > 1 && os.Args[1] == "-genkey" {
		key, err := auth.Generate()
		if err != nil {
			slog.Error("fatal", "error", err)
			os.Exit(1)
		}
		fmt.Println(key)
		return
	}

	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// run exists so every exit path can return an error instead of calling
// os.Exit deep inside the program, which would skip deferred cleanup.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)

	// A short budget for the dependencies that must be reachable at startup.
	ctxStartup, cancelStartup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStartup()

	reg, err := buildRegistry(cfg)
	if err != nil {
		return err
	}

	var keys auth.Store
	if cfg.AuthDisabled {
		logger.Warn("client authentication is DISABLED: anyone who can reach this port can spend the budget")
	} else {
		keys, err = auth.NewStaticStore(cfg.APIKeys)
		if err != nil {
			return fmt.Errorf("loading API keys: %w", err)
		}
		logger.Info("client authentication enabled", "keys", keys.Len())
	}

	limiter, err := buildLimiter(ctxStartup, cfg, logger)
	if err != nil {
		return err
	}
	if limiter != nil {
		defer func() { _ = limiter.Close() }()
	}

	fallbacks, err := provider.ParseFallbacks(cfg.FallbackModels)
	if err != nil {
		return fmt.Errorf("parsing GATEWAY_FALLBACK_MODELS: %w", err)
	}
	if len(fallbacks) > 0 {
		logger.Info("automatic fallback enabled", "chains", len(fallbacks))
	}

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: server.New(reg, logger, cfg.RequestTimeout, version).
			WithAuth(keys).
			WithRateLimiter(limiter).
			WithFallback(fallbacks,
				provider.RetryPolicy{
					Attempts:  cfg.RetryAttempts,
					BaseDelay: cfg.RetryBaseDelay,
					MaxDelay:  5 * time.Second,
				},
				provider.BreakerConfig{
					Threshold: cfg.BreakerThreshold,
					Cooldown:  cfg.BreakerCooldown,
				}).
			WithStreamTimings(cfg.StreamIdleTimeout, cfg.StreamHeartbeat).
			Handler(),

		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout is deliberately left at zero. It caps the time from the
		// end of the request headers to the end of the response, which would
		// cut long completions short and make SSE streaming impossible in
		// phase 3. Per-request deadlines live on the context instead.
	}

	// signal.NotifyContext gives a context that is cancelled on SIGINT/SIGTERM,
	// which is how a container tells us it is about to be stopped.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ListenAndServe blocks, so it runs in its own goroutine and reports its
	// outcome through a buffered channel. The buffer of 1 matters: if main
	// returns via the shutdown path instead, nobody ever receives from this
	// channel, and an unbuffered send would block that goroutine forever.
	errCh := make(chan error, 1)
	go func() {
		logger.Info("gateway listening", "addr", cfg.Addr, "providers", reg.Names(), "version", version)
		errCh <- srv.ListenAndServe()
	}()

	// Wait for whichever happens first: the server dying, or a shutdown signal.
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining connections")
	}

	// Shutdown stops accepting new connections and waits for in-flight
	// handlers to finish, up to this grace period.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	logger.Info("gateway stopped cleanly")
	return nil
}

// buildLimiter picks the rate limiter that matches the deployment: Redis when
// one is configured, in-process otherwise, and none at all when the rate is
// zero or less.
//
// The in-process one is correct for a single instance and wrong for several:
// three replicas each allowing the configured rate allow three times the
// intended limit. That is why a Redis URL is what turns the limit real.
func buildLimiter(ctx context.Context, cfg *config.Config, logger *slog.Logger) (ratelimit.Limiter, error) {
	if cfg.RateLimitRPS <= 0 {
		logger.Warn("rate limiting is disabled: any caller can spend the whole budget")
		return nil, nil
	}

	rlCfg := ratelimit.Config{Rate: cfg.RateLimitRPS, Burst: cfg.RateLimitBurst}

	if cfg.RedisURL == "" {
		logger.Warn("rate limiting is in-process: correct for one instance, too permissive for several",
			"rps", cfg.RateLimitRPS, "burst", cfg.RateLimitBurst)
		return ratelimit.NewMemory(rlCfg)
	}

	l, err := ratelimit.NewRedis(ctx, cfg.RedisURL, rlCfg)
	if err != nil {
		return nil, fmt.Errorf("connecting the rate limiter: %w", err)
	}
	logger.Info("rate limiting is distributed",
		"rps", cfg.RateLimitRPS, "burst", cfg.RateLimitBurst)
	return l, nil
}

// buildRegistry wires the providers that are configured and declares which
// model names route to each of them.
//
// The prefixes live here, in the composition root, rather than inside the
// registry or the provider packages: routing policy is a deployment decision,
// not something a vendor client should hardcode.
func buildRegistry(cfg *config.Config) (*provider.Registry, error) {
	reg := provider.NewRegistry()
	reg.Register(mock.New(), "mock-")

	if cfg.OpenAIAPIKey != "" {
		reg.Register(
			openai.New(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, nil),
			"gpt-", "o1-", "o3-", "o4-", "chatgpt-",
		)
	}

	if cfg.AnthropicAPIKey != "" {
		reg.Register(
			anthropic.New(cfg.AnthropicAPIKey, cfg.AnthropicBaseURL, cfg.AnthropicMaxTokens, nil),
			"claude-",
		)
	}

	// GATEWAY_PROVIDER names the fallback for models that match no prefix.
	if err := reg.SetDefault(cfg.Provider); err != nil {
		return nil, fmt.Errorf("%w (is its API key set?)", err)
	}
	return reg, nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
