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
//	go run ./cmd/fakeupstream                 # happy path on :9000
//	go run ./cmd/fakeupstream -fail 429       # every call is rate limited
//	go run ./cmd/fakeupstream -latency 5s     # slow enough to test timeouts
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"
)

func main() {
	addr := flag.String("addr", ":9000", "address to listen on")
	fail := flag.Int("fail", 0, "if non-zero, answer every request with this HTTP status")
	latency := flag.Duration("latency", 0, "artificial delay before answering")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", handler(*fail, *latency, openAIReply))
	mux.HandleFunc("POST /v1/messages", handler(*fail, *latency, anthropicReply))

	log.Printf("fake upstream listening on %s (fail=%d latency=%s)", *addr, *fail, *latency)
	log.Printf("  OpenAI    POST %s/v1/chat/completions", *addr)
	log.Printf("  Anthropic POST %s/v1/messages", *addr)

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// reply builds a response body from the decoded request.
type reply func(body map[string]any) string

func handler(fail int, latency time.Duration, r reply) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)

		log.Printf("%s %s  auth=%q anthropic-version=%q",
			req.Method, req.URL.Path,
			firstNonEmpty(req.Header.Get("Authorization"), req.Header.Get("X-Api-Key")),
			req.Header.Get("Anthropic-Version"))
		if pretty, err := json.Marshal(body); err == nil {
			log.Printf("  body: %s", pretty)
		}

		if latency > 0 {
			time.Sleep(latency)
		}

		w.Header().Set("Content-Type", "application/json")

		if fail != 0 {
			w.WriteHeader(fail)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"fake upstream was told to fail with %d","type":"fake_error"}}`, fail)
			return
		}

		_, _ = fmt.Fprint(w, r(body))
	}
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

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
