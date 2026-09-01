package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// ctxKey is unexported so no other package can collide with our context keys.
// This is the standard way to store values in a context safely.
type ctxKey string

const (
	requestIDKey ctxKey = "request_id"
	callerKey    ctxKey = "caller"
)

// CallerFrom returns the authenticated caller's name, or "" when the request
// was not authenticated (which only happens with auth explicitly disabled).
func CallerFrom(ctx context.Context) string {
	name, _ := ctx.Value(callerKey).(string)
	return name
}

// RequestIDFrom returns the request id attached by the requestID middleware,
// or "" when there is none.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// requestID attaches a random id to every request so a single call can be
// followed across log lines, and echoes it back in a response header.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			var b [8]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// statusRecorder remembers the status code and byte count so the logging
// middleware can report them. Handlers that never call WriteHeader still
// produce 200, which is why status is pre-seeded.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int

	// committed records whether a status has reached the wire. Once it has,
	// nothing further can change it, which is what the panic handler needs to
	// know before trying to turn a failure into a 500.
	committed bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.committed {
		// Go would log a superfluous WriteHeader here and ignore it anyway.
		return
	}
	r.committed = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	// An implicit 200, the same one net/http would send.
	r.committed = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Unwrap lets http.NewResponseController reach the underlying
// ResponseWriter, which is how flushing keeps working through this wrapper.
// Without it, streaming in phase 3 would silently buffer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// logging emits one structured line per request once it completes.
func logging(logger *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			logger.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", RequestIDFrom(r.Context()),
				"caller", CallerFrom(r.Context()),
			)
		})
	}
}

// recoverer turns a panic in any handler into a 500 instead of killing the
// connection. net/http recovers panics itself, but it does so silently and
// without a response body; this makes the failure visible and well formed.
func recoverer(logger *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Wrapped so the recovery can tell whether the response already
			// started. A panic partway through a stream cannot be turned into
			// a 500: the status is on the wire, and writing an error object
			// after it would splice JSON into whatever was being streamed.
			rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			//nolint:contextcheck // writeError performs no I/O that could be cancelled
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}

				logger.Error("panic recovered",
					"panic", rec,
					"path", r.URL.Path,
					"committed", rw.committed,
					"request_id", RequestIDFrom(r.Context()),
				)

				if rw.committed {
					// Nothing can be said to the client any more. Cutting the
					// connection is the honest signal that the answer is
					// incomplete, and it is what net/http does with a
					// re-panic of ErrAbortHandler.
					panic(http.ErrAbortHandler)
				}
				writeError(rw, http.StatusInternalServerError, "internal_error", "internal server error")
			}()

			next.ServeHTTP(rw, r)
		})
	}
}

// requireAuth rejects requests without a valid gateway API key.
//
// It guards the API surface only: /healthz and /metrics stay open, because a
// probe that needs a credential is a probe that stops working exactly when
// something is wrong.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.keys == nil { // authentication explicitly disabled
			next.ServeHTTP(w, r)
			return
		}

		secret, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			// The header tells a well-behaved client how to authenticate
			// rather than leaving it to guess.
			w.Header().Set("WWW-Authenticate", `Bearer realm="llm-gateway"`)
			writeError(w, http.StatusUnauthorized, "invalid_request_error",
				"missing or malformed Authorization header, want: Bearer <key>")
			return
		}

		key, found := s.keys.Lookup(secret)
		if !found {
			// Deliberately identical wording for an unknown key and a
			// malformed one at the log level, and no echo of what was sent.
			s.logger.Warn("rejected an unknown API key",
				"request_id", RequestIDFrom(r.Context()),
				"remote", r.RemoteAddr,
			)
			writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid API key")
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey, key.Name)))
	})
}

// bearerToken extracts the credential from an Authorization header. The scheme
// is matched case-insensitively, as RFC 7235 requires.
func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}
