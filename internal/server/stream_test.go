package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

// These tests are the specification for the streaming branch of
// handleChatCompletions. They are red until it is written.

// scriptedStream replays a fixed list of chunks and then, optionally, fails.
// It stands in for a provider stream without needing a socket.
type scriptedStream struct {
	chunks   []provider.Chunk
	next     int
	failWith error
	gap      time.Duration
	ctx      context.Context
	err      error
	closed   bool
}

var _ provider.Stream = (*scriptedStream)(nil)

func (s *scriptedStream) Next() bool {
	if s.err != nil || s.closed {
		return false
	}
	if s.next >= len(s.chunks) {
		s.err = s.failWith // nil for a clean end
		return false
	}
	if s.gap > 0 {
		select {
		case <-time.After(s.gap):
		case <-s.ctx.Done():
			s.err = s.ctx.Err()
			return false
		}
	}
	s.next++
	return true
}

func (s *scriptedStream) Current() provider.Chunk { return s.chunks[s.next-1] }
func (s *scriptedStream) Err() error              { return s.err }
func (s *scriptedStream) Close() error            { s.closed = true; return nil }

func sampleChunks() []provider.Chunk {
	return []provider.Chunk{
		{ID: "chatcmpl-s", Model: "gpt-4o-mini", Content: "Hola "},
		{ID: "chatcmpl-s", Model: "gpt-4o-mini", Content: "mundo"},
		{ID: "chatcmpl-s", Model: "gpt-4o-mini", FinishReason: "stop"},
	}
}

// streamingProvider returns a stub whose ChatStream replays chunks.
func streamingProvider(chunks []provider.Chunk, failWith error, gap time.Duration) *stubProvider {
	return &stubProvider{
		chat: func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
			return okResponse(), nil
		},
		stream: func(ctx context.Context, _ provider.ChatRequest) (provider.Stream, error) {
			return &scriptedStream{chunks: chunks, failWith: failWith, gap: gap, ctx: ctx}, nil
		},
	}
}

const streamBody = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`

// sseFrames splits a recorded SSE body into its payloads, dropping the blank
// separators. Comments and non-data fields are ignored.
func sseFrames(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		out = append(out, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
	}
	return out
}

func TestStreaming_SetsSSEHeaders(t *testing.T) {
	h := newTestHandler(streamingProvider(sampleChunks(), nil, 0))
	rec := do(h, http.MethodPost, "/v1/chat/completions", streamBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	// Proxies and browsers must not cache or buffer a stream.
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	// A stream has no known length; sending one would end it early.
	if cl := rec.Header().Get("Content-Length"); cl != "" {
		t.Errorf("Content-Length = %q, want it absent on a stream", cl)
	}
}

func TestStreaming_EmitsDeltasThenDone(t *testing.T) {
	h := newTestHandler(streamingProvider(sampleChunks(), nil, 0))
	rec := do(h, http.MethodPost, "/v1/chat/completions", streamBody)

	frames := sseFrames(t, rec.Body.String())
	if len(frames) < 2 {
		t.Fatalf("got %d frames, want the deltas plus [DONE]:\n%s", len(frames), rec.Body)
	}

	// The stream must end with the sentinel OpenAI clients look for.
	if last := frames[len(frames)-1]; last != "[DONE]" {
		t.Errorf("last frame = %q, want [DONE]", last)
	}

	var text strings.Builder
	var sawFinish bool
	for _, f := range frames[:len(frames)-1] {
		var chunk struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Model   string `json:"model"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(f), &chunk); err != nil {
			t.Fatalf("frame is not JSON: %v\n%s", err, f)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Errorf("object = %q, want chat.completion.chunk", chunk.Object)
		}
		if chunk.ID == "" {
			t.Error("frame has no id")
		}
		if len(chunk.Choices) != 1 {
			t.Fatalf("choices = %d, want 1", len(chunk.Choices))
		}
		text.WriteString(chunk.Choices[0].Delta.Content)
		if chunk.Choices[0].FinishReason != "" {
			sawFinish = true
		}
	}

	if got := text.String(); got != "Hola mundo" {
		t.Errorf("assembled text = %q, want %q", got, "Hola mundo")
	}
	if !sawFinish {
		t.Error("no frame carried a finish_reason; the client cannot tell a complete answer from a truncated one")
	}
}

// TestStreaming_FlushesProgressively is the one that proves this is a stream
// at all. Against a real socket, the first frame must arrive long before the
// last chunk is produced.
func TestStreaming_FlushesProgressively(t *testing.T) {
	const gap = 150 * time.Millisecond

	srv := httptest.NewServer(newTestHandler(streamingProvider(sampleChunks(), nil, gap)))
	t.Cleanup(srv.Close)

	start := time.Now()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(streamBody)) //nolint:noctx // the test owns the lifetime
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended before the first frame: %v", err)
		}
		if strings.HasPrefix(line, "data:") {
			break
		}
	}
	firstFrame := time.Since(start)

	// Three chunks at 150ms each is 450ms of work. Seeing the first frame at
	// roughly one gap proves the response was flushed rather than buffered.
	if firstFrame > 2*gap {
		t.Errorf("first frame took %v, want under %v: the response is being buffered, not streamed",
			firstFrame, 2*gap)
	}
}

// A provider that never opens the stream can still be reported properly,
// because no status has been committed yet.
func TestStreaming_ProviderFailsToStart(t *testing.T) {
	p := &stubProvider{
		stream: func(context.Context, provider.ChatRequest) (provider.Stream, error) {
			return nil, &provider.Error{Provider: "openai", StatusCode: 429, Message: "quota exceeded"}
		},
	}

	rec := do(newTestHandler(p), http.MethodPost, "/v1/chat/completions", streamBody)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body: %s)", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json: this never became a stream", ct)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not the usual error envelope: %v", err)
	}
	if !strings.Contains(env.Error.Message, "quota exceeded") {
		t.Errorf("message = %q, want the vendor wording", env.Error.Message)
	}
}

func TestStreaming_NotSupportedByProvider(t *testing.T) {
	p := &stubProvider{
		stream: func(context.Context, provider.ChatRequest) (provider.Stream, error) {
			return nil, provider.ErrStreamingNotSupported
		},
	}

	rec := do(newTestHandler(p), http.MethodPost, "/v1/chat/completions", streamBody)

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 (body: %s)", rec.Code, rec.Body)
	}
}

// Once the 200 and the first byte are out, the status can no longer be
// changed. A failure after that has to be signalled inside the stream, and it
// must not look like a clean end.
func TestStreaming_MidStreamFailureIsSignalled(t *testing.T) {
	boom := errors.New("upstream vanished")
	h := newTestHandler(streamingProvider(sampleChunks(), boom, 0))

	rec := do(h, http.MethodPost, "/v1/chat/completions", streamBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the header was already committed", rec.Code)
	}

	body := rec.Body.String()
	frames := sseFrames(t, body)
	if len(frames) == 0 {
		t.Fatal("no frames at all")
	}
	if last := frames[len(frames)-1]; last == "[DONE]" {
		t.Error("the stream ended with [DONE] after failing; the client would treat a broken answer as complete")
	}
	if !strings.Contains(body, "error") {
		t.Errorf("the failure was never signalled to the client:\n%s", body)
	}
}

// A disconnected client must stop the work rather than leave it generating
// tokens nobody will read.
func TestStreaming_StopsWhenClientDisconnects(t *testing.T) {
	chunks := make([]provider.Chunk, 50)
	for i := range chunks {
		chunks[i] = provider.Chunk{ID: "x", Model: "m", Content: "tick "}
	}

	// A plain bool here would be written by the server goroutine and read by
	// the test one: a data race, and -race says so. Closing a channel is the
	// idiomatic way to announce "this happened" across goroutines.
	closed := make(chan struct{})
	p := &stubProvider{
		stream: func(ctx context.Context, _ provider.ChatRequest) (provider.Stream, error) {
			return &closeRecorder{
				scriptedStream: scriptedStream{chunks: chunks, gap: 50 * time.Millisecond, ctx: ctx},
				closed:         closed,
			}, nil
		},
	}

	srv := httptest.NewServer(newTestHandler(p))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(streamBody))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	// Read one frame, then walk away mid-stream.
	buf := make([]byte, 64)
	_, _ = resp.Body.Read(buf)
	cancel()
	_ = resp.Body.Close()

	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Error("the provider stream was never closed after the client disconnected: the gateway kept paying for tokens nobody would read")
	}
}

// closeRecorder announces through a channel when Close is reached. Close may
// run more than once, so the sync.Once keeps the second call from panicking on
// an already-closed channel.
type closeRecorder struct {
	scriptedStream
	closed chan struct{}
	once   sync.Once
}

func (c *closeRecorder) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.scriptedStream.Close()
}

var _ io.Closer = (*closeRecorder)(nil)

// --- what a sequential loop cannot do --------------------------------------
//
// The two tests below are the reason the streaming handler has to multiplex.
// Both need the handler to wait on the next chunk *and* on something else at
// the same time, and a loop parked inside stream.Next() can only wait on one
// thing.

// hangingStream yields its chunks and then blocks forever, the way a provider
// that stopped responding without closing the connection would.
type hangingStream struct {
	chunks []provider.Chunk
	next   int
	gap    time.Duration
	ctx    context.Context
	err    error
	closed bool
}

var _ provider.Stream = (*hangingStream)(nil)

func (h *hangingStream) Next() bool {
	if h.err != nil || h.closed {
		return false
	}
	wait := h.gap
	if h.next >= len(h.chunks) {
		wait = time.Hour // out of chunks: hang instead of ending
	}
	select {
	case <-time.After(wait):
	case <-h.ctx.Done():
		h.err = h.ctx.Err()
		return false
	}
	if h.next >= len(h.chunks) {
		return false
	}
	h.next++
	return true
}

func (h *hangingStream) Current() provider.Chunk { return h.chunks[h.next-1] }
func (h *hangingStream) Err() error              { return h.err }
func (h *hangingStream) Close() error            { h.closed = true; return nil }

func hangingProvider(chunks []provider.Chunk, gap time.Duration) *stubProvider {
	return &stubProvider{
		stream: func(ctx context.Context, _ provider.ChatRequest) (provider.Stream, error) {
			return &hangingStream{chunks: chunks, gap: gap, ctx: ctx}, nil
		},
	}
}

// handlerWithTimings builds the chain with streaming durations short enough to
// test in milliseconds.
func handlerWithTimings(p provider.Provider, idle, heartbeat time.Duration) http.Handler {
	reg := provider.NewRegistry()
	reg.Register(p)
	if err := reg.SetDefault(p.Name()); err != nil {
		panic(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(reg, logger, time.Second, "test").WithStreamTimings(idle, heartbeat).Handler()
}

// readUntilQuiet drains an SSE response until it ends or the deadline passes,
// returning what arrived and whether the server closed the stream itself.
func readUntilQuiet(t *testing.T, body io.Reader, within time.Duration) (string, bool) {
	t.Helper()

	type result struct {
		text string
		done bool
	}
	ch := make(chan result, 1)
	go func() {
		raw, err := io.ReadAll(body)
		ch <- result{string(raw), err == nil}
	}()

	select {
	case r := <-ch:
		return r.text, r.done
	case <-time.After(within):
		return "", false
	}
}

// A provider that goes quiet must not hold the connection open forever. The
// handler has to notice the silence, which means watching a clock while it
// waits for a chunk.
func TestStreaming_IdleTimeoutEndsTheStream(t *testing.T) {
	const idle = 250 * time.Millisecond

	h := handlerWithTimings(
		hangingProvider([]provider.Chunk{{ID: "x", Model: "m", Content: "first "}}, 0),
		idle, time.Hour, // heartbeats disabled for this test
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	start := time.Now()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(streamBody)) //nolint:noctx // the test owns the lifetime
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	body, finished := readUntilQuiet(t, resp.Body, 5*time.Second)
	if !finished {
		t.Fatal("the handler never ended the response: it is still parked waiting for a chunk that will never come")
	}

	elapsed := time.Since(start)
	if elapsed > 20*idle {
		t.Errorf("the stream took %v to give up, want roughly %v", elapsed, idle)
	}
	if !strings.Contains(body, "first ") {
		t.Errorf("the chunks produced before the silence were lost:\n%s", body)
	}
	if !strings.Contains(body, "error") {
		t.Errorf("the timeout was never reported to the client:\n%s", body)
	}
	if frames := sseFrames(t, body); len(frames) > 0 && frames[len(frames)-1] == "[DONE]" {
		t.Error("a timed-out stream ended with [DONE]; the client would treat a cut-off answer as complete")
	}
}

// While the model thinks, the connection must show signs of life or an
// intermediary will drop it. SSE comments are the standard keep-alive, and
// clients ignore them.
func TestStreaming_SendsHeartbeatsWhileWaiting(t *testing.T) {
	const heartbeat = 80 * time.Millisecond

	chunks := []provider.Chunk{
		{ID: "x", Model: "m", Content: "slow "},
		{ID: "x", Model: "m", Content: "answer"},
		{ID: "x", Model: "m", FinishReason: "stop"},
	}

	h := handlerWithTimings(
		streamingProvider(chunks, nil, 400*time.Millisecond),
		5*time.Second, heartbeat,
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(streamBody)) //nolint:noctx // the test owns the lifetime
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	body, finished := readUntilQuiet(t, resp.Body, 6*time.Second)
	if !finished {
		t.Fatal("the stream never ended")
	}

	var comments int
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimRight(line, "\r"), ":") {
			comments++
		}
	}
	if comments == 0 {
		t.Errorf("no keep-alive comments were sent during 400ms gaps with an %v heartbeat:\n%s", heartbeat, body)
	}

	// The keep-alives must not disturb the real content.
	var text strings.Builder
	for _, f := range sseFrames(t, body) {
		if f == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(f), &chunk); err != nil {
			t.Fatalf("frame is not JSON: %v (%s)", err, f)
		}
		if len(chunk.Choices) > 0 {
			text.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if got := text.String(); got != "slow answer" {
		t.Errorf("assembled text = %q, want %q: the heartbeats corrupted the content", got, "slow answer")
	}
}
