// Package anthropic implements provider.Provider on top of the Anthropic
// Messages API.
//
// Anthropic's request and response shapes differ structurally from the
// canonical schema the gateway speaks, so this package owns an anti-corruption
// layer: every value crossing its boundary is translated in translate.go, and
// no Anthropic-specific vocabulary is visible from the outside.
package anthropic

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

const (
	// DefaultBaseURL is the public Anthropic API root.
	DefaultBaseURL = "https://api.anthropic.com/v1"

	// APIVersion is sent on every call. Anthropic pins breaking changes behind
	// this header, so a fixed value here means the vendor cannot change the
	// wire format under our feet.
	APIVersion = "2023-06-01"

	// DefaultMaxTokens is used when the caller does not specify a limit.
	// Anthropic rejects requests without max_tokens, but leaking that
	// requirement to our clients would mean a request valid for an OpenAI
	// model becomes invalid for a Claude one purely because of the vendor
	// behind it -- and phase 5's fallback would break for every request that
	// omits the field.
	DefaultMaxTokens = 4096

	// maxErrorBody caps how much of a failed response is read.
	maxErrorBody = 8 << 10
)

// Client is an Anthropic Messages API client.
type Client struct {
	apiKey           string
	baseURL          string
	defaultMaxTokens int
	http             *http.Client
}

var _ provider.Provider = (*Client)(nil)

// New builds a Client. Empty or zero arguments fall back to the package
// defaults. As with the OpenAI client, no http.Client.Timeout is set: bounding
// the whole exchange would truncate long generations and make phase 3's
// streaming impossible. Deadlines belong on the context.
func New(apiKey, baseURL string, defaultMaxTokens int, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if defaultMaxTokens <= 0 {
		defaultMaxTokens = DefaultMaxTokens
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
	return &Client{
		apiKey:           apiKey,
		baseURL:          baseURL,
		defaultMaxTokens: defaultMaxTokens,
		http:             httpClient,
	}
}

// Name implements provider.Provider.
func (c *Client) Name() string { return "anthropic" }

// Chat implements provider.Provider. The HTTP mechanics are identical to the
// OpenAI client; everything interesting happens in the two translation calls
// at the top and the bottom.
func (c *Client) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	apiReq, err := toAnthropic(req, c.defaultMaxTokens)
	if err != nil {
		return nil, fmt.Errorf("anthropic: translate request: %w", err)
	}

	body, err := json.Marshal(apiReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}

	// Anthropic authenticates with its own header rather than a bearer token,
	// and refuses any request without a pinned API version.
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Api-Key", c.apiKey)
	httpReq.Header.Set("Anthropic-Version", APIVersion)

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, &provider.Error{
			Provider: c.Name(),
			Message:  err.Error(),
			Err:      err,
		}
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, maxErrorBody))

		msg := strings.TrimSpace(string(raw))
		var env apiError
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

	var apiResp apiResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&apiResp); err != nil {
		return nil, &provider.Error{
			Provider:   c.Name(),
			StatusCode: httpResp.StatusCode,
			Message:    "malformed response body: " + err.Error(),
			Err:        err,
		}
	}

	resp, err := fromAnthropic(apiResp)
	if err != nil {
		return nil, &provider.Error{
			Provider:   c.Name(),
			StatusCode: httpResp.StatusCode,
			Message:    "unusable response: " + err.Error(),
			Err:        err,
		}
	}
	return resp, nil
}
