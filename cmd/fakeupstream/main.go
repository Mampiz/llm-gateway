// Command fakeupstream serves the OpenAI and Anthropic APIs well enough to
// develop against without an API key, a network connection or a bill.
//
// It answers on both dialects at once:
//
//	POST /v1/chat/completions   OpenAI shape
//	POST /v1/messages           Anthropic shape
//
// The Anthropic replies deliberately interleave block types the gateway has to
// filter out, so a translation bug shows up immediately rather than in
// production.
//
// A request carrying "stream": true is answered with a real SSE stream in the
// matching dialect, emitted token by token with a flush after each event.
//
//	go run ./cmd/fakeupstream                    # happy path on :9000
//	go run ./cmd/fakeupstream -fail 429          # every call is rate limited
//	go run ./cmd/fakeupstream -latency 5s        # slow enough to test timeouts
//	go run ./cmd/fakeupstream -token-delay 500ms # slow motion streaming
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", ":9000", "address to listen on")
	fail := flag.Int("fail", 0, "if non-zero, answer every request with this HTTP status")
	latency := flag.Duration("latency", 0, "artificial delay before answering")
	tokenDelay := flag.Duration("token-delay", 80*time.Millisecond, "pause between streamed tokens")
	flag.Parse()

	cfg := config{fail: *fail, latency: *latency, tokenDelay: *tokenDelay}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", handler(cfg, openAIReply, openAIStream))
	mux.HandleFunc("POST /v1/messages", handler(cfg, anthropicReply, anthropicStream))

	log.Printf("fake upstream listening on %s (fail=%d latency=%s)", *addr, *fail, *latency)
	log.Printf("  OpenAI    POST %s/v1/chat/completions", *addr)
	log.Printf("  Anthropic POST %s/v1/messages", *addr)

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

type config struct {
	fail       int
	latency    time.Duration
	tokenDelay time.Duration
}

// reply builds a whole response body from the decoded request.
type reply func(body map[string]any) string

// streamer writes an SSE stream for the decoded request. It receives a
// pre-configured emitter that handles framing and flushing.
type streamer func(e *emitter, body map[string]any)

func handler(cfg config, r reply, s streamer) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)

		stream, _ := body["stream"].(bool)

		log.Printf("%s %s  stream=%v auth=%q anthropic-version=%q",
			req.Method, req.URL.Path, stream,
			firstNonEmpty(req.Header.Get("Authorization"), req.Header.Get("X-Api-Key")),
			req.Header.Get("Anthropic-Version"))
		if pretty, err := json.Marshal(body); err == nil {
			log.Printf("  body: %s", pretty)
		}

		if cfg.latency > 0 {
			time.Sleep(cfg.latency)
		}

		if cfg.fail != 0 {
			// Vendors report a failed streaming request as a plain JSON error
			// with the right status, not as an SSE event.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(cfg.fail)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"fake upstream was told to fail with %d","type":"fake_error"}}`, cfg.fail)
			return
		}

		if !stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, r(body))
			return
		}

		// SSE: no Content-Length, so the client reads until the connection
		// closes. The headers must be set before the first byte of the body.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		s(&emitter{w: w, ctx: req.Context(), delay: cfg.tokenDelay}, body)
	}
}

// emitter writes SSE frames and flushes after each one, which is what makes a
// stream a stream. It stops early once the client goes away.
type emitter struct {
	w     http.ResponseWriter
	ctx   context.Context
	delay time.Duration
}

// send writes one event. name may be empty for an unnamed event. It reports
// whether the stream should continue.
func (e *emitter) send(name, data string) bool {
	if e.ctx.Err() != nil {
		log.Printf("  client went away mid-stream, stopping")
		return false
	}
	if name != "" {
		_, _ = fmt.Fprintf(e.w, "event: %s\n", name)
	}
	// The blank line is the frame delimiter: without it the client never
	// considers the event complete.
	_, _ = fmt.Fprintf(e.w, "data: %s\n\n", data)

	// Without this the bytes sit in the response writer's buffer and the
	// client sees nothing until the handler returns.
	_ = http.NewResponseController(e.w).Flush()

	if e.delay > 0 {
		select {
		case <-time.After(e.delay):
		case <-e.ctx.Done():
			log.Printf("  client went away mid-stream, stopping")
			return false
		}
	}
	return true
}

// words splits the answer into chunks that stand in for tokens.
func words(s string) []string {
	parts := strings.Fields(s)
	out := make([]string, 0, len(parts))
	for i, p := range parts {
		if i < len(parts)-1 {
			p += " "
		}
		out = append(out, p)
	}
	return out
}

// lastUserMessage digs the most recent user turn out of either dialect, so the
// answer visibly derives from the request instead of being a fixed string.
func lastUserMessage(body map[string]any) string {
	messages, _ := body["messages"].([]any)
	for i := len(messages) - 1; i >= 0; i-- {
		msg, _ := messages[i].(map[string]any)
		if msg["role"] == "user" {
			if content, ok := msg["content"].(string); ok {
				return content
			}
		}
	}
	return "(nothing)"
}

func openAIReply(body map[string]any) string {
	model, _ := body["model"].(string)
	answer := "openai heard: " + lastUserMessage(body)

	resp := map[string]any{
		"id":      "chatcmpl-fake",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": answer},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 9, "completion_tokens": 4, "total_tokens": 13},
	}
	out, _ := json.Marshal(resp)
	return string(out)
}

func anthropicReply(body map[string]any) string {
	model, _ := body["model"].(string)

	resp := map[string]any{
		"id":    "msg_fake",
		"type":  "message",
		"role":  "assistant",
		"model": model,
		// Three block kinds on purpose: only the text ones may survive the
		// translation, and they must be joined with nothing between them.
		"content": []any{
			map[string]any{"type": "thinking", "text": "THIS MUST NOT REACH THE CLIENT"},
			map[string]any{"type": "text", "text": "anthropic heard: "},
			map[string]any{"type": "tool_use"},
			map[string]any{"type": "text", "text": lastUserMessage(body)},
		},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 11, "output_tokens": 5},
	}
	out, _ := json.Marshal(resp)
	return string(out)
}

// openAIStream emits the OpenAI dialect: unnamed events carrying deltas, and
// the literal [DONE] sentinel to mark the end.
func openAIStream(e *emitter, body map[string]any) {
	model, _ := body["model"].(string)
	answer := "openai streamed: " + lastUserMessage(body)

	header := fmt.Sprintf(`"id":"chatcmpl-fake","object":"chat.completion.chunk","created":%d,"model":%q`,
		time.Now().Unix(), model)

	if !e.send("", fmt.Sprintf(`{%s,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`, header)) {
		return
	}
	for _, w := range words(answer) {
		chunk, _ := json.Marshal(w)
		if !e.send("", fmt.Sprintf(`{%s,"choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, header, chunk)) {
			return
		}
	}
	if !e.send("", fmt.Sprintf(`{%s,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, header)) {
		return
	}
	e.send("", "[DONE]")
}

// anthropicStream emits the Anthropic dialect: named events wrapped in a
// message lifecycle, with pings in between and no [DONE] sentinel. The stream
// simply ends.
func anthropicStream(e *emitter, body map[string]any) {
	model, _ := body["model"].(string)
	answer := "anthropic streamed: " + lastUserMessage(body)

	if !e.send("message_start", fmt.Sprintf(
		`{"type":"message_start","message":{"id":"msg_fake","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"usage":{"input_tokens":11,"output_tokens":0}}}`,
		model)) {
		return
	}
	if !e.send("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) {
		return
	}
	// A keep-alive the client must ignore rather than treat as content.
	if !e.send("ping", `{"type":"ping"}`) {
		return
	}

	chunks := words(answer)
	for i, w := range chunks {
		text, _ := json.Marshal(w)
		if !e.send("content_block_delta", fmt.Sprintf(
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, text)) {
			return
		}
		if i == len(chunks)/2 {
			if !e.send("ping", `{"type":"ping"}`) {
				return
			}
		}
	}

	if !e.send("content_block_stop", `{"type":"content_block_stop","index":0}`) {
		return
	}
	if !e.send("message_delta", fmt.Sprintf(
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":%d}}`,
		len(chunks))) {
		return
	}
	e.send("message_stop", `{"type":"message_stop"}`)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
