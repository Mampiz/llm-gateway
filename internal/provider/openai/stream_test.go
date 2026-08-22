package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

// These tests are the specification for ChatStream. They are red until it is
// implemented; making them green is the task.

// sse writes one Server-Sent Event and pushes it out immediately. Without the
// flush the whole "stream" would arrive in one lump when the handler returns.
func sse(t *testing.T, w http.ResponseWriter, data string) {
	t.Helper()
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return
	}
	_ = http.NewResponseController(w).Flush()
}

// streamHeaders sets what a vendor sends before the first event.
func streamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
}

// chunkJSON builds one OpenAI streaming frame.
func chunkJSON(content, finish string) string {
	delta := `{}`
	if content != "" {
		encoded, _ := json.Marshal(content)
		delta = fmt.Sprintf(`{"content":%s}`, encoded)
	}
	finishField := "null"
	if finish != "" {
		finishField = fmt.Sprintf("%q", finish)
	}
	return fmt.Sprintf(
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`,
		delta, finishField)
}

// collect drains a stream into the text it produced plus the chunks seen.
func collect(t *testing.T, s provider.Stream) (string, []provider.Chunk) {
	t.Helper()
	var text strings.Builder
	var chunks []provider.Chunk
	for s.Next() {
		c := s.Current()
		text.WriteString(c.Content)
		chunks = append(chunks, c)
	}
	return text.String(), chunks
}

func TestChatStream_AsksTheVendorToStream(t *testing.T) {
	var body map[string]any

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		streamHeaders(w)
		sse(t, w, chunkJSON("hi", ""))
		sse(t, w, "[DONE]")
	})

	req := sampleRequest()
	req.Stream = false // the method name is the intent; it must not depend on this

	s, err := c.ChatStream(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatStream() failed: %v", err)
	}
	defer s.Close()
	collect(t, s)

	if body["stream"] != true {
		t.Errorf(`outgoing body has stream=%v, want true`, body["stream"])
	}
}

func TestChatStream_YieldsDeltasInOrder(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		streamHeaders(w)
		sse(t, w, chunkJSON("", ""))      // the role-only opener carries no text
		sse(t, w, chunkJSON("Hola ", "")) //
		sse(t, w, chunkJSON("mundo", "")) //
		sse(t, w, chunkJSON("", "stop"))  // the closer carries no text either
		sse(t, w, "[DONE]")
	})

	s, err := c.ChatStream(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("ChatStream() failed: %v", err)
	}
	defer s.Close()

	text, chunks := collect(t, s)

	if text != "Hola mundo" {
		t.Errorf("assembled text = %q, want %q", text, "Hola mundo")
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err() = %v, want nil after a clean stream", err)
	}

	// Chunks with neither text, nor a finish reason, nor usage tell the
	// consumer nothing; they must not reach it.
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3: two deltas and the closer (%+v)", len(chunks), chunks)
	}
	if chunks[len(chunks)-1].FinishReason != "stop" {
		t.Errorf("last chunk FinishReason = %q, want stop", chunks[len(chunks)-1].FinishReason)
	}
	for _, c := range chunks {
		if c.ID != "chatcmpl-x" {
			t.Errorf("chunk ID = %q, want it carried from the wire", c.ID)
		}
		if c.Model != "gpt-4o-mini" {
			t.Errorf("chunk Model = %q, want it carried from the wire", c.Model)
		}
	}
}

// The sentinel ends the stream and is never content.
func TestChatStream_StopsAtDone(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		streamHeaders(w)
		sse(t, w, chunkJSON("only this", ""))
		sse(t, w, "[DONE]")
		// Anything after the sentinel must be ignored.
		sse(t, w, chunkJSON("MUST NOT APPEAR", ""))
	})

	s, err := c.ChatStream(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("ChatStream() failed: %v", err)
	}
	defer s.Close()

	text, _ := collect(t, s)
	if strings.Contains(text, "MUST NOT APPEAR") {
		t.Errorf("text = %q, want everything after [DONE] ignored", text)
	}
	if text != "only this" {
		t.Errorf("text = %q, want %q", text, "only this")
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err() = %v, want nil: [DONE] is a clean end, not a failure", err)
	}
}

// A request that never becomes a stream fails at ChatStream, not at Err: there
// is no stream to fail halfway through.
func TestChatStream_UpstreamRejectsTheRequest(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"quota exceeded","type":"insufficient_quota"}}`)
	})

	s, err := c.ChatStream(context.Background(), sampleRequest())
	if err == nil {
		_ = s.Close()
		t.Fatal("ChatStream() returned nil error for a 429")
	}

	var pErr *provider.Error
	if !errors.As(err, &pErr) {
		t.Fatalf("error = %v (%T), want a *provider.Error", err, err)
	}
	if pErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want 429", pErr.StatusCode)
	}
	if !strings.Contains(pErr.Message, "quota exceeded") {
		t.Errorf("Message = %q, want the vendor wording", pErr.Message)
	}
}

// A stream that dies halfway must not look like a stream that ended.
func TestChatStream_TruncatedStreamIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamHeaders(w)
		sse(t, w, chunkJSON("half an ans", ""))
		// No [DONE]: the connection is torn down mid-answer.
		http.NewResponseController(w).SetWriteDeadline(time.Now())
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)

	c := New("sk-test-key", srv.URL, srv.Client())

	s, err := c.ChatStream(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("ChatStream() failed to start: %v", err)
	}
	defer s.Close()

	collect(t, s)
	if s.Err() == nil {
		t.Error("Err() = nil after a truncated stream, want an error: silence here turns a cut-off answer into a successful one")
	}
}

func TestChatStream_MalformedFrameIsAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		streamHeaders(w)
		sse(t, w, chunkJSON("fine so far", ""))
		sse(t, w, `{"choices":[{"delta":{"content":`) // truncated JSON
		sse(t, w, "[DONE]")
	})

	s, err := c.ChatStream(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("ChatStream() failed: %v", err)
	}
	defer s.Close()

	collect(t, s)
	if s.Err() == nil {
		t.Error("Err() = nil after an unparseable frame, want an error")
	}
}

// bufio.Scanner refuses tokens over 64 KiB by default, and a single SSE line
// can exceed that. Left unhandled, this fails only on large payloads.
func TestChatStream_HandlesVeryLongFrames(t *testing.T) {
	huge := strings.Repeat("a", 128*1024)

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		streamHeaders(w)
		sse(t, w, chunkJSON(huge, ""))
		sse(t, w, chunkJSON("", "stop"))
		sse(t, w, "[DONE]")
	})

	s, err := c.ChatStream(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("ChatStream() failed: %v", err)
	}
	defer s.Close()

	text, _ := collect(t, s)
	if err := s.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil: a 128 KiB frame is large, not invalid", err)
	}
	if len(text) != len(huge) {
		t.Errorf("assembled %d bytes, want %d", len(text), len(huge))
	}
}

// Cancelling must stop the read, not wait for the vendor to finish. Without
// it the gateway keeps paying for tokens nobody will ever see.
func TestChatStream_HonoursCancellation(t *testing.T) {
	release := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		streamHeaders(w)
		sse(t, w, chunkJSON("first ", ""))
		<-release // then hang forever
	})
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())

	s, err := c.ChatStream(ctx, sampleRequest())
	if err != nil {
		t.Fatalf("ChatStream() failed: %v", err)
	}
	defer s.Close()

	if !s.Next() {
		t.Fatalf("Next() = false on the first chunk: %v", s.Err())
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for s.Next() { //nolint:revive // draining is the point
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream ignored cancellation and blocked")
	}

	if !errors.Is(s.Err(), context.Canceled) {
		t.Errorf("Err() = %v, want it to wrap context.Canceled", s.Err())
	}
}

// Callers are told to `defer stream.Close()` unconditionally, so Close has to
// survive being called after the loop already drained the stream.
func TestChatStream_CloseIsIdempotent(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		streamHeaders(w)
		sse(t, w, chunkJSON("hi", ""))
		sse(t, w, "[DONE]")
	})

	s, err := c.ChatStream(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("ChatStream() failed: %v", err)
	}
	collect(t, s)

	if err := s.Close(); err != nil {
		t.Errorf("first Close() = %v, want nil", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close() = %v, want nil", err)
	}
	if s.Next() {
		t.Error("Next() = true after Close(), want false")
	}
}
