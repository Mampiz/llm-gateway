//go:build e2e

// Package e2e exercises the gateway the way a client does: a real binary, a
// real TCP port, real HTTP requests. The unit suites test the pieces; this
// tests that the assembled program works.
//
// It is behind a build tag so `go test ./...` stays fast. Run it with
// `make e2e`.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var binary string

// TestMain compiles the gateway once for every test in the package.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gateway-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mktemp:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	binary = filepath.Join(dir, "gateway")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/gateway")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// freePort asks the kernel for an unused port, so parallel CI runs never
// collide on a hardcoded one.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not reserve a port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// gateway starts the real binary with env and waits until it is serving.
func gateway(t *testing.T, env map[string]string) string {
	t.Helper()

	port := freePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("GATEWAY_ADDR=:%d", port),
		"GATEWAY_LOG_LEVEL=error",
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs

	if err := cmd.Start(); err != nil {
		t.Fatalf("could not start the gateway: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if t.Failed() && logs.Len() > 0 {
			t.Logf("gateway logs:\n%s", logs.String())
		}
	})

	waitReady(t, base+"/healthz", cmd)
	return base
}

func waitReady(t *testing.T, url string, cmd *exec.Cmd) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			t.Fatal("the gateway exited before becoming ready")
		}
		resp, err := http.Get(url) //nolint:noctx // short-lived readiness poll
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the gateway never became ready")
}

// post sends a chat completion request and returns status and decoded body.
func post(t *testing.T, base, body string) (int, map[string]any) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded
}

// --- fake upstreams --------------------------------------------------------

const openAIBody = `{"id":"chatcmpl-e2e","object":"chat.completion","created":1,"model":"gpt-4o-mini",
"choices":[{"index":0,"message":{"role":"assistant","content":"from openai"},"finish_reason":"stop"}],
"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`

// Deliberately interleaves block types the gateway must filter out.
const anthropicBody = `{"id":"msg_e2e","type":"message","role":"assistant","model":"claude-sonnet-5",
"content":[{"type":"thinking","text":"hidden"},{"type":"text","text":"from "},
{"type":"tool_use"},{"type":"text","text":"anthropic"}],
"stop_reason":"max_tokens","usage":{"input_tokens":11,"output_tokens":3}}`

func fakeOpenAI(t *testing.T, seen *map[string]any) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			_ = json.NewDecoder(r.Body).Decode(seen)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openAIBody)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func fakeAnthropic(t *testing.T, seen *map[string]any, headers *http.Header) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if headers != nil {
			*headers = r.Header.Clone()
		}
		if seen != nil {
			_ = json.NewDecoder(r.Body).Decode(seen)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicBody)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// --- tests -----------------------------------------------------------------

func TestHealthzListsConfiguredProviders(t *testing.T) {
	base := gateway(t, map[string]string{
		"OPENAI_API_KEY":     "sk-e2e",
		"OPENAI_BASE_URL":    fakeOpenAI(t, nil),
		"ANTHROPIC_API_KEY":  "sk-ant-e2e",
		"ANTHROPIC_BASE_URL": fakeAnthropic(t, nil, nil),
	})

	resp, err := http.Get(base + "/healthz") //nolint:noctx // trivial probe
	if err != nil {
		t.Fatalf("healthz failed: %v", err)
	}
	defer resp.Body.Close()

	var got struct {
		Status    string   `json:"status"`
		Providers []string `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("healthz is not JSON: %v", err)
	}
	if got.Status != "ok" {
		t.Errorf("status = %q, want ok", got.Status)
	}
	if len(got.Providers) != 3 {
		t.Errorf("providers = %v, want anthropic, mock and openai", got.Providers)
	}
}

// TestRoutesToOpenAI walks the whole OpenAI path through the real binary.
func TestRoutesToOpenAI(t *testing.T) {
	var upstream map[string]any
	base := gateway(t, map[string]string{
		"OPENAI_API_KEY":  "sk-e2e",
		"OPENAI_BASE_URL": fakeOpenAI(t, &upstream),
	})

	status, body := post(t, base, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"top_p":0.9}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}
	if body["id"] != "chatcmpl-e2e" {
		t.Errorf("id = %v, want the upstream answer", body["id"])
	}
	// A field the gateway does not model must reach a provider that speaks it.
	if upstream["top_p"] != 0.9 {
		t.Errorf("top_p = %v, want it forwarded to the vendor", upstream["top_p"])
	}
}

// TestRoutesToAnthropic is the interesting one: a request written in the
// OpenAI dialect comes out the other side as a Messages API call, and its
// answer comes back in the OpenAI dialect again.
func TestRoutesToAnthropic(t *testing.T) {
	var upstream map[string]any
	var headers http.Header

	base := gateway(t, map[string]string{
		"ANTHROPIC_API_KEY":  "sk-ant-e2e",
		"ANTHROPIC_BASE_URL": fakeAnthropic(t, &upstream, &headers),
	})

	status, body := post(t, base, `{"model":"claude-sonnet-5","messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"2+2?"},
		{"role":"system","content":"in Spanish"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}

	// What the vendor received.
	if headers.Get("X-Api-Key") != "sk-ant-e2e" {
		t.Errorf("X-Api-Key = %q, want the configured key", headers.Get("X-Api-Key"))
	}
	if headers.Get("Anthropic-Version") == "" {
		t.Error("Anthropic-Version header is missing")
	}
	if headers.Get("Authorization") != "" {
		t.Error("Authorization header was sent to a vendor that does not use it")
	}
	if want := "be brief\n\nin Spanish"; upstream["system"] != want {
		t.Errorf("system = %q, want %q hoisted out of the message list", upstream["system"], want)
	}
	if msgs, _ := upstream["messages"].([]any); len(msgs) != 1 {
		t.Errorf("messages = %v, want only the non-system turn", upstream["messages"])
	}
	if upstream["max_tokens"] == nil {
		t.Error("max_tokens is missing: the vendor requires it and the gateway must supply a default")
	}

	// What the client received.
	choices, _ := body["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v, want exactly one", body["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "from anthropic" {
		t.Errorf("content = %v, want the text blocks flattened and the others dropped", message["content"])
	}
	if choice["finish_reason"] != "length" {
		t.Errorf("finish_reason = %v, want max_tokens translated to length", choice["finish_reason"])
	}
	usage, _ := body["usage"].(map[string]any)
	if usage["total_tokens"] != float64(14) {
		t.Errorf("total_tokens = %v, want 11+3 computed by the gateway", usage["total_tokens"])
	}
}

func TestUpstreamFailureIsPropagated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"quota exceeded","type":"insufficient_quota"}}`)
	}))
	t.Cleanup(srv.Close)

	base := gateway(t, map[string]string{
		"OPENAI_API_KEY":  "sk-e2e",
		"OPENAI_BASE_URL": srv.URL,
	})

	status, body := post(t, base, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want the upstream 429 propagated", status)
	}
	errObj, _ := body["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "quota exceeded") {
		t.Errorf("message = %q, want the vendor wording preserved", msg)
	}
}

func TestBadRequestsAreRejected(t *testing.T) {
	base := gateway(t, nil) // mock only, no keys needed

	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"malformed JSON", `{"model":`, http.StatusBadRequest},
		{"no model", `{"messages":[{"role":"user","content":"hi"}]}`, http.StatusBadRequest},
		{"no messages", `{"model":"mock-1"}`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if status, body := post(t, base, tt.body); status != tt.wantStatus {
				t.Errorf("status = %d, want %d: %v", status, tt.wantStatus, body)
			}
		})
	}
}

// TestStreamsThroughTheBinary drives a real SSE stream end to end: the fake
// upstream emits vendor frames, the gateway translates them, and the client
// reads them off the socket as they arrive.
func TestStreamsThroughTheBinary(t *testing.T) {
	base := gateway(t, map[string]string{
		"OPENAI_API_KEY":  "sk-e2e",
		"OPENAI_BASE_URL": fakeStreamingOpenAI(t),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	var text strings.Builder
	var sawDone bool
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("frame is not JSON: %v (%s)", err, payload)
		}
		if len(chunk.Choices) > 0 {
			text.WriteString(chunk.Choices[0].Delta.Content)
		}
	}

	if !sawDone {
		t.Error("the stream never reached [DONE]")
	}
	if !strings.Contains(text.String(), "hello there") {
		t.Errorf("assembled text = %q, want the upstream answer", text.String())
	}
}

// fakeStreamingOpenAI emits an OpenAI SSE stream, flushing each frame.
func fakeStreamingOpenAI(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)

		const head = `"id":"chatcmpl-e2e","object":"chat.completion.chunk","created":1,"model":"gpt-4o-mini"`
		for _, word := range []string{"hello ", "there"} {
			fmt.Fprintf(w, "data: {%s,\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", head, word)
			_ = rc.Flush()
		}
		fmt.Fprintf(w, "data: {%s,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", head)
		_ = rc.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		_ = rc.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestStartupFailsOnBadConfig proves the binary refuses to run half-configured
// instead of surfacing the problem later as a confusing runtime error.
func TestStartupFailsOnBadConfig(t *testing.T) {
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(),
		"GATEWAY_ADDR=:0",
		"GATEWAY_PROVIDER=openai", // named as default but no API key configured
		"OPENAI_API_KEY=",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the gateway started with an unusable configuration:\n%s", out)
	}
}
