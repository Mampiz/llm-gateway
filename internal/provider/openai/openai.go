// Package openai implements provider.Provider on top of the OpenAI
// Chat Completions API.
package openai

import (
	"context"
	"errors"
	"net/http"
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

// Chat implements provider.Provider.
//
// TODO(phase 1): this is your mission. Build the outgoing request from req,
// send it, and turn the response into a *provider.ChatResponse (or a
// *provider.Error when the upstream rejects the call).
func (c *Client) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	_ = ctx
	_ = req
	return nil, errors.New("openai: Chat is not implemented yet")
}
