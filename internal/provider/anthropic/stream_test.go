package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

// sse writes one named Server-Sent Event and pushes it out. This vendor always
// sends the event name as well as the payload, so the fixtures do too.
func sse(t *testing.T, w http.ResponseWriter, name, data string) {
	t.Helper()
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return
	}
	_ = http.NewResponseController(w).Flush()
}

func streamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
}

// happyStream writes a complete, well-formed message: metadata, two text
// deltas with a ping and block boundaries in between, then the closing events.
func happyStream(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	streamHeaders(w)
	sse(t, w, "message_start", `{"type":"message_start","message":{"id":"msg_abc","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":11,"output_tokens":0}}}`)
	sse(t, w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	sse(t, w, "ping", `{"type":"ping"}`)
	sse(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hola "}}`)
	sse(t, w, "ping", `{"type":"ping"}`)
	sse(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"mundo"}}`)
	sse(t, w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
	sse(t, w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":4}}`)
	sse(t, w, "message_stop", `{"type":"message_stop"}`)
}

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
		happyStream(t, w)
	})

	req := sampleRequest()
	req.Stream = false // the method name is the intent, not this field

	s, err := c.ChatStream(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatStream() failed: %v", err)
	}
	defer s.Close()
	collect(t, s)

	if body["stream"] != true {
		t.Errorf("outgoing body has stream=%v, want true", body["stream"])
	}
	if _, ok := body["max_tokens"]; !ok {
		t.Error("max_tokens is missing: this vendor requires it on streaming calls too")
	}
}

func TestChatStream_SendsVendorHeaders(t *testing.T) {
	var gotPath, gotKey, gotVersion, gotAuth, gotAccept string

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.Header.Get("X-Api-Key")
		gotVersion, gotAuth = r.Header.Get("Anthropic-Version"), r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		happyStream(t, w)
	})

	s, err := c.ChatStream(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("ChatStream() failed: %v", err)
	}
	defer s.Close()
	collect(t, s)

	if gotPath != "/messages" {
		t.Errorf("path = %q, want /messages", gotPath)
	}
	if gotKey != "sk-ant-test" {
		t.Errorf("X-Api-Key = %q, want the key", gotKey)
	}
	if gotVersion != APIVersion {
		t.Errorf("Anthropic-Version = %q, want %q", gotVersion, APIVersion)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want it absent for this vendor", gotAuth)
	}
	if !strings.Contains(gotAccept, "text/event-stream") {
		t.Errorf("Accept = %q, want text/event-stream", gotAccept)
	}
}

// The vendor spreads one answer across several event types and interleaves
// keep-alives and block boundaries that carry nothing.
func TestChatStream_YieldsTextAndClosingMetadata(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		happyStream(t, w)
	})

	s, err := c.ChatStream(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("ChatStream() failed: %v", err)
	}
	defer s.Close()

	text, chunks := collect(t, s)

	if err := s.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil after message_stop", err)
	}
	if text != "Hola mundo" {
		t.Errorf("assembled text = %q, want %q", text, "Hola mundo")
	}
	// Two text deltas plus the closing metadata. Pings and block boundaries
	// must not reach the consumer.
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3: %+v", len(chunks), chunks)
	}

	for _, c := range chunks {
		if c.ID != "msg_abc" {
			t.Errorf("chunk ID = %q, want it carried from message_start", c.ID)
		}
		if c.Model != "claude-sonnet-5" {
			t.Errorf("chunk Model = %q, want it carried from message_start", c.Model)
		}
	}

	last := chunks[len(chunks)-1]
	if last.Content != "" {
		t.Errorf("the closing chunk carries text %q, want none", last.Content)
	}
	// end_turn is this vendor's wording; the client must see the canonical one.
	if last.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", last.FinishReason)
	}
	if last.Usage == nil {
		t.Fatal("the closing chunk carries no usage")
	}
	// input_tokens arrives in message_start, output_tokens in message_delta,
	// and the total exists in neither.
	if last.Usage.PromptTokens != 11 || last.Usage.CompletionTokens != 4 || last.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v, want 11/4/15 stitched from two events", *last.Usage)
	}
}

func TestChatStream_MapsStopReasons(t *testing.T) {
	tests := []struct {
		vendor string
		want   string
	}{
		{"end_turn", "stop"},
		{"stop_sequence", "stop"},
		{"max_tokens", "length"},
		{"tool_use", "tool_calls"},
		{"some_future_reason", "some_future_reason"},
	}

	for _, tt := range tests {
		t.Run(tt.vendor, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				streamHeaders(w)
				sse(t, w, "message_start", `{"type":"message_start","message":{"id":"m","model":"claude","usage":{"input_tokens":1}}}`)
				sse(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`)
				sse(t, w, "message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":1}}`, tt.vendor))
				sse(t, w, "message_stop", `{"type":"message_stop"}`)
			})

			s, err := c.ChatStream(context.Background(), sampleRequest())
			if err != nil {
				t.Fatalf("ChatStream() failed: %v", err)
			}
			defer s.Close()

			_, chunks := collect(t, s)
			if len(chunks) == 0 {
				t.Fatal("no chunks")
			}
			if got := chunks[len(chunks)-1].FinishReason; got != tt.want {
				t.Errorf("FinishReason = %q, want %q", got, tt.want)
			}
		})
	}
}

// Non-text deltas belong to tool use, which the canonical schema does not
// model yet. Letting them through would corrupt the answer.
func TestChatStream_IgnoresNonTextDeltas(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		streamHeaders(w)
		sse(t, w, "message_start", `{"type":"message_start","message":{"id":"m","model":"claude","usage":{"input_tokens":1}}}`)
		sse(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`)
		sse(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"real text"}}`)
		sse(t, w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`)
		sse(t, w, "message_stop", `{"type":"message_stop"}`)
	})

	s, err := c.ChatStream(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("ChatStream() failed: %v", err)
	}
	defer s.Close()

	text, _ := collect(t, s)
	if text != "real text" {
		t.Errorf("assembled text = %q, want only the text deltas", text)
	}
}

// This vendor reports mid-stream failures as an event, not as a status code:
// the response already began with a 200.
func TestChatStream_ErrorEventEndsTheStream(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		streamHeaders(w)
		sse(t, w, "message_start", `{"type":"message_start","message":{"id":"m","model":"claude","usage":{"input_tokens":1}}}`)
		sse(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half an ans"}}`)
		sse(t, w, "error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	})

	s, err := c.ChatStream(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("ChatStream() failed to start: %v", err)
	}
	defer s.Close()

	text, _ := collect(t, s)
	if text != "half an ans" {
		t.Errorf("text = %q, want what arrived before the error", text)
	}
	if s.Err() == nil {
		t.Fatal("Err() = nil after an error event, want an error")
	}
	if !strings.Contains(s.Err().Error(), "Overloaded") {
		t.Errorf("Err() = %v, want the vendor wording preserved", s.Err())
	}
}

// There is no [DONE] here: a stream that stops without message_stop was cut
// short, and saying otherwise would turn a broken answer into a complete one.
func TestChatStream_MissingMessageStopIsAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		streamHeaders(w)
		sse(t, w, "message_start", `{"type":"message_start","message":{"id":"m","model":"claude","usage":{"input_tokens":1}}}`)
		sse(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half"}}`)
		// connection just ends
	})

	s, err := c.ChatStream(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("ChatStream() failed: %v", err)
	}
	defer s.Close()

	collect(t, s)
	if s.Err() == nil {
		t.Error("Err() = nil after a truncated stream, want an error")
	}
}

func TestChatStream_UpstreamRejectsTheRequest(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests exceeded"}}`)
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
	if !pErr.Retryable() {
		t.Error("Retryable() = false, want true for a rate limit")
	}
}

func TestChatStream_MalformedFrameIsAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		streamHeaders(w)
		sse(t, w, "message_start", `{"type":"message_start","message":{"id":"m","model":"claude"}}`)
		sse(t, w, "content_block_delta", `{"type":"content_block_delta","delta":{"type":`)
		sse(t, w, "message_stop", `{"type":"message_stop"}`)
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

// A single frame can exceed bufio.Scanner's 64 KiB default.
func TestChatStream_HandlesVeryLongFrames(t *testing.T) {
	huge := strings.Repeat("a", 128*1024)

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		streamHeaders(w)
		sse(t, w, "message_start", `{"type":"message_start","message":{"id":"m","model":"claude","usage":{"input_tokens":1}}}`)
		text, _ := json.Marshal(huge)
		sse(t, w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","delta":{"type":"text_delta","text":%s}}`, text))
		sse(t, w, "message_stop", `{"type":"message_stop"}`)
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

func TestChatStream_HonoursCancellation(t *testing.T) {
	release := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		streamHeaders(w)
		sse(t, w, "message_start", `{"type":"message_start","message":{"id":"m","model":"claude"}}`)
		sse(t, w, "content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"first "}}`)
		<-release
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

func TestChatStream_CloseIsIdempotent(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		happyStream(t, w)
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
