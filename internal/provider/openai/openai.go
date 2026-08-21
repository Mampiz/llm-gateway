// Package openai implements provider.Provider on top of the OpenAI
// Chat Completions API.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

// DefaultBaseURL is the public OpenAI API root. It is configurable so the
// gateway can be pointed at a local mock or at an OpenAI-compatible vendor
// (Groq, Together, Ollama, ...) without code changes.
const DefaultBaseURL = "https://api.openai.com/v1"

// Client is an OpenAI-compatible chat completions client.
type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// Compile-time proof that *Client satisfies the interface. If Chat's
// signature ever drifts, the build breaks here instead of at the call site.
var _ provider.Provider = (*Client)(nil)

// New builds a Client. An empty baseURL falls back to DefaultBaseURL, and a
// nil httpClient gets a sane default.
//
// Note the deliberate absence of an http.Client.Timeout: that field bounds
// the *entire* exchange including reading the response body, which would
// truncate long generations and make streaming impossible in phase 3.
// Per-request deadlines belong on the context instead.
func New(apiKey, baseURL string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}
	return &Client{apiKey: apiKey, baseURL: baseURL, http: httpClient}
}

// Name implements provider.Provider.
func (c *Client) Name() string { return "openai" }

// maxErrorBody caps how much of a failed response we read. A misbehaving
// upstream must not be able to make the gateway allocate without bound.
const maxErrorBody = 8 << 10 // 8 KiB

// errorEnvelope is the shape OpenAI wraps its failures in. It stays
// unexported because it is vendor-specific wire format: in phase 2 Anthropic
// will bring its own, and neither belongs in the shared provider package.
type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// Chat implements provider.Provider.
func (c *Client) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	// Do returns an error only when the exchange never completed
	// (DNS, refused connection, cancelled context). An HTTP 429 or 500 is a
	// *successful* Do with an unhappy status code, handled below.
	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, &provider.Error{
			Provider: c.Name(),
			Message:  err.Error(),
			Err:      err,
		}
	}

	// close on every return path, or the TCP connection never goes
	// back to the Transport's pool.
	defer httpResp.Body.Close()


	if httpResp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, maxErrorBody))

		msg := strings.TrimSpace(string(raw))
		var env errorEnvelope
		if err := json.Unmarshal(raw, &env); err == nil && env.Error.Message != "" {
			msg = env.Error.Message
		}
		if msg == "" {
			msg = httpResp.Status 
		}

		return nil, &provider.Error{
			Provider:   c.Name(),
			StatusCode: httpResp.StatusCode,
			Message:    msg,
		}
	}

	var out provider.ChatResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&out); err != nil {
		return nil, &provider.Error{
			Provider:   c.Name(),
			StatusCode: httpResp.StatusCode,
			Message:    "malformed response body: " + err.Error(),
			Err:        err,
		}
	}

	if len(out.Choices) == 0 {
		return nil, &provider.Error{
			Provider:   c.Name(),
			StatusCode: httpResp.StatusCode,
			Message:    "upstream returned no choices",
		}
	}

	return &out, nil
}
