package config

import (
	"strings"
	"testing"
	"time"
)

// clearEnv unsets every variable Load reads, so a test never inherits the
// developer's shell. t.Setenv restores the previous value automatically when
// the test ends, and marks the test as unsafe for t.Parallel.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GATEWAY_ADDR", "GATEWAY_PROVIDER", "OPENAI_API_KEY",
		"OPENAI_BASE_URL", "GATEWAY_REQUEST_TIMEOUT", "GATEWAY_LOG_LEVEL",
	} {
		t.Setenv(k, "")
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed on an empty environment: %v", err)
	}

	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr)
	}
	// The default must be the provider that costs nothing and needs no key,
	// so a fresh clone runs with zero setup.
	if cfg.Provider != "mock" {
		t.Errorf("Provider = %q, want mock", cfg.Provider)
	}
	if cfg.RequestTimeout != 60*time.Second {
		t.Errorf("RequestTimeout = %v, want 60s", cfg.RequestTimeout)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
}

func TestLoad_ReadsEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv("GATEWAY_ADDR", ":9000")
	t.Setenv("GATEWAY_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "sk-live")
	t.Setenv("OPENAI_BASE_URL", "http://localhost:1234/v1")
	t.Setenv("GATEWAY_LOG_LEVEL", "debug")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.Addr != ":9000" {
		t.Errorf("Addr = %q, want :9000", cfg.Addr)
	}
	if cfg.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", cfg.Provider)
	}
	if cfg.OpenAIAPIKey != "sk-live" {
		t.Errorf("OpenAIAPIKey = %q, want sk-live", cfg.OpenAIAPIKey)
	}
	if cfg.OpenAIBaseURL != "http://localhost:1234/v1" {
		t.Errorf("OpenAIBaseURL = %q, want the override", cfg.OpenAIBaseURL)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
}

func TestLoad_Validation(t *testing.T) {
	tests := []struct {
		name        string
		env         map[string]string
		wantErrPart string
	}{
		{
			name:        "openai without a key",
			env:         map[string]string{"GATEWAY_PROVIDER": "openai"},
			wantErrPart: "OPENAI_API_KEY",
		},
		{
			name:        "unknown provider",
			env:         map[string]string{"GATEWAY_PROVIDER": "gemini"},
			wantErrPart: "unknown GATEWAY_PROVIDER",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			cfg, err := Load()
			if err == nil {
				t.Fatalf("Load() = %+v, nil; want an error", cfg)
			}
			if !strings.Contains(err.Error(), tt.wantErrPart) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErrPart)
			}
		})
	}
}

func TestEnvDuration(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"go duration syntax", "45s", 45 * time.Second},
		{"minutes", "2m", 2 * time.Minute},
		{"bare seconds", "30", 30 * time.Second},
		{"unset falls back", "", 60 * time.Second},
		{"garbage falls back", "soon", 60 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("GATEWAY_REQUEST_TIMEOUT", tt.value)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() failed: %v", err)
			}
			if cfg.RequestTimeout != tt.want {
				t.Errorf("RequestTimeout = %v, want %v", cfg.RequestTimeout, tt.want)
			}
		})
	}
}
