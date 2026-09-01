package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRequestID_GeneratedWhenAbsent(t *testing.T) {
	var seen string
	h := requestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Fatal("no request id in the context, want a generated one")
	}
	if got := rec.Header().Get("X-Request-ID"); got != seen {
		t.Errorf("X-Request-ID header = %q, want it to match the context value %q", got, seen)
	}
}

func TestRequestID_PreservedFromHeader(t *testing.T) {
	const incoming = "trace-from-the-caller"

	var seen string
	h := requestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", incoming)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen != incoming {
		t.Errorf("request id = %q, want the caller's own %q so traces stitch together", seen, incoming)
	}
}

func TestRecoverer_TurnsPanicIntoJSON500(t *testing.T) {
	h := recoverer(discardLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("panic response is not JSON: %v", err)
	}
	if env.Error.Message == "" {
		t.Error("panic response has no message")
	}
}

func TestLogging_RecordsStatusAndBytes(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}

	sr.WriteHeader(http.StatusTeapot)
	if _, err := sr.Write([]byte("hello")); err != nil {
		t.Fatalf("Write() failed: %v", err)
	}

	if sr.status != http.StatusTeapot {
		t.Errorf("recorded status = %d, want 418", sr.status)
	}
	if sr.bytes != 5 {
		t.Errorf("recorded bytes = %d, want 5", sr.bytes)
	}
}

// TestStatusRecorder_StaysFlushable is a guard for phase 3. Wrapping the
// ResponseWriter to record the status must not cost us the ability to flush,
// or SSE would silently buffer and stream nothing. http.NewResponseController
// finds the real writer through Unwrap; delete that method and this fails.
func TestStatusRecorder_StaysFlushable(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}

	if _, err := sr.Write([]byte("data: hi\n\n")); err != nil {
		t.Fatalf("Write() failed: %v", err)
	}
	if err := http.NewResponseController(sr).Flush(); err != nil {
		t.Fatalf("Flush() through the wrapper failed: %v (is statusRecorder.Unwrap still there?)", err)
	}
	if !rec.Flushed {
		t.Error("the underlying ResponseWriter was never flushed")
	}
}

func TestChain_AppliesOutermostFirst(t *testing.T) {
	var order []string

	mw := func(name string) middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}

	h := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
	}), mw("first"), mw("second"))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"first", "second", "handler"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// A panic after the response has started cannot be turned into a 500: the
// status is already on the wire. Writing one anyway logs a superfluous
// WriteHeader and splices an error object into whatever was being streamed,
// corrupting it for the client.
func TestRecoverer_DoesNotRewriteACommittedResponse(t *testing.T) {
	h := recoverer(discardLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: first\n\n"))
		panic("boom halfway through")
	}))

	rec := httptest.NewRecorder()

	// The recovery re-panics with ErrAbortHandler, which net/http treats as
	// "drop this connection quietly". A truncated stream is the honest signal
	// that the answer is incomplete; a spliced error object is not.
	func() {
		defer func() {
			got := recover()
			err, _ := got.(error)
			if !errors.Is(err, http.ErrAbortHandler) {
				t.Errorf("re-panicked with %v, want http.ErrAbortHandler", got)
			}
		}()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	}()

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want the committed 200 left alone", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: first") {
		t.Errorf("body = %q, want what was already sent preserved", body)
	}
	if strings.Contains(body, "internal_error") {
		t.Errorf("body = %q, want no error object spliced into a started response", body)
	}
}

// Before anything is written the panic still has to become a clean 500.
func TestRecoverer_StillAnswersAnUncommittedRequest(t *testing.T) {
	h := recoverer(discardLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom before writing")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("panic response is not JSON: %v", err)
	}
}
