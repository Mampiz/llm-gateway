package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mampiz/llm-gateway/internal/auth"
	"github.com/Mampiz/llm-gateway/internal/provider"
)

const testSecret = "gw_testsecret"

// authenticatedHandler builds the chain with a single configured key.
func authenticatedHandler(t *testing.T, p provider.Provider) http.Handler {
	t.Helper()

	keys, err := auth.NewStaticStore("alice:" + testSecret)
	if err != nil {
		t.Fatalf("building the key store: %v", err)
	}

	reg := provider.NewRegistry()
	reg.Register(p)
	if err := reg.SetDefault(p.Name()); err != nil {
		t.Fatalf("registry: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(reg, logger, time.Second, "test").WithAuth(keys).Handler()
}

// withAuth sends a chat request carrying the given Authorization header
// verbatim, including the empty case of no header at all.
func withAuth(h http.Handler, header string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	req.Header.Set("Content-Type", "application/json")
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuth_AcceptsAValidKey(t *testing.T) {
	rec := withAuth(authenticatedHandler(t, succeedingProvider()), "Bearer "+testSecret)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
	}
}

// The scheme is case-insensitive per RFC 7235, and clients do vary.
func TestAuth_SchemeIsCaseInsensitive(t *testing.T) {
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		t.Run(scheme, func(t *testing.T) {
			rec := withAuth(authenticatedHandler(t, succeedingProvider()), scheme+" "+testSecret)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
		})
	}
}

func TestAuth_Rejects(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{"no header at all", ""},
		{"an unknown key", "Bearer gw_notthisone"},
		{"the empty string as a key", "Bearer "},
		{"a scheme we do not speak", "Basic " + testSecret},
		{"the raw secret with no scheme", testSecret},
		// A near miss must fail exactly like a wild guess.
		{"one character off", "Bearer " + testSecret + "x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &stubProvider{chat: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
				t.Error("the provider was reached by an unauthenticated request")
				return okResponse(), nil
			}}

			rec := withAuth(authenticatedHandler(t, p), tt.header)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body: %s)", rec.Code, rec.Body)
			}

			var env errorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("the 401 body is not the usual envelope: %v", err)
			}
			if env.Error.Message == "" {
				t.Error("the 401 carries no message")
			}
			// Whatever was presented must never be echoed back.
			if strings.Contains(rec.Body.String(), testSecret) {
				t.Error("the response echoed a credential")
			}
		})
	}
}

// A client that gets a 401 should be told how to authenticate.
func TestAuth_AdvertisesTheScheme(t *testing.T) {
	rec := withAuth(authenticatedHandler(t, succeedingProvider()), "")

	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want it to name the Bearer scheme", got)
	}
}

// A probe that needs a credential stops working exactly when it is needed.
func TestAuth_HealthzStaysOpen(t *testing.T) {
	rec := do(authenticatedHandler(t, succeedingProvider()), http.MethodGet, "/healthz", "")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 without a key", rec.Code)
	}
}

// The caller's name has to reach the request context: rate limiting and
// metrics are keyed on it from here on.
func TestAuth_PutsTheCallerInContext(t *testing.T) {
	var caller string
	p := &stubProvider{chat: func(ctx context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
		caller = CallerFrom(ctx)
		return okResponse(), nil
	}}

	if rec := withAuth(authenticatedHandler(t, p), "Bearer "+testSecret); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if caller != "alice" {
		t.Errorf("caller = %q, want alice", caller)
	}
}

// With no store attached the gateway serves anyone, which is what
// GATEWAY_AUTH_DISABLED buys and why config refuses to do it silently.
func TestAuth_DisabledLetsEveryoneThrough(t *testing.T) {
	rec := withAuth(newTestHandler(succeedingProvider()), "")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with authentication disabled", rec.Code)
	}
}
