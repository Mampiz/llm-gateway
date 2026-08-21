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

	"github.com/Mampiz/llm-gateway/internal/config"
	"github.com/Mampiz/llm-gateway/internal/provider"
	"github.com/Mampiz/llm-gateway/internal/provider/anthropic"
	"github.com/Mampiz/llm-gateway/internal/provider/mock"
	"github.com/Mampiz/llm-gateway/internal/provider/openai"
	"github.com/Mampiz/llm-gateway/internal/server"
)

// version is stamped at build time with -ldflags "-X main.version=...".
// It stays "dev" for local builds.
var version = "dev"

func main() {
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

	reg, err := buildRegistry(cfg)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: server.New(reg, logger, cfg.RequestTimeout, version).Handler(),

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
