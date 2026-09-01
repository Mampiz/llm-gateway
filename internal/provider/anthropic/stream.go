package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

const (
	// ssePrefix is the only SSE field this package reads. Anthropic also sends
	// an `event:` line before every frame, but its value is duplicated in the
	// payload's own "type" field, so keying off the JSON is both simpler and
	// harder to get out of step with.
	ssePrefix = "data:"

	// maxStreamLine caps one SSE line. bufio.Scanner refuses tokens over
	// 64 KiB by default and a single frame can exceed that.
	maxStreamLine = 1 << 20 // 1 MiB
)

// Event types on the Messages API stream. Unlike OpenAI there is no [DONE]
// sentinel: message_stop marks a complete answer, and a stream that ends
// without one was truncated.
const (
	evtMessageStart = "message_start"
	evtContentDelta = "content_block_delta"
	evtMessageDelta = "message_delta"
	evtMessageStop  = "message_stop"
	evtError        = "error"
	deltaTypeText   = "text_delta"
)

// streamEvent is one `data:` payload. The fields are a union of every event
// type; which ones are populated depends on Type.
type streamEvent struct {
	Type string `json:"type"`

	// message_start
	Message struct {
		ID    string   `json:"id"`
		Model string   `json:"model"`
		Usage apiUsage `json:"usage"`
	} `json:"message"`

	// content_block_delta
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`

		// message_delta reuses this object for the stop reason.
		StopReason string `json:"stop_reason"`
	} `json:"delta"`

	// message_delta
	Usage apiUsage `json:"usage"`

	// error
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ChatStream implements provider.Provider.
func (c *Client) ChatStream(ctx context.Context, req provider.ChatRequest) (provider.Stream, error) {
	req.Stream = true

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

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("X-Api-Key", c.apiKey)
	httpReq.Header.Set("Anthropic-Version", APIVersion)

	// bodyclose cannot see through provider.DrainAndClose, which both
	// drains and closes; every path below goes through it.
	//nolint:bodyclose // closed via provider.DrainAndClose
	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, &provider.Error{Provider: c.Name(), Message: err.Error(), Err: err}
	}

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

		// No stream is handed back, so nobody will ever call Close.
		provider.DrainAndClose(httpResp.Body)

		return nil, &provider.Error{
			Provider:   c.Name(),
			StatusCode: httpResp.StatusCode,
			Message:    msg,
		}
	}

	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, maxStreamLine)

	// Ownership of the body passes to the stream, which closes it in Close.
	return &stream{ctx: ctx, body: httpResp.Body, scanner: scanner}, nil
}

// stream is a cursor over the SSE frames of one streamed message.
//
// It carries more state than the OpenAI one because this vendor spreads a
// single answer's metadata across several event types: the id and the input
// token count arrive in message_start, the stop reason and the output token
// count in message_delta, and the text in between.
type stream struct {
	ctx     context.Context
	body    io.ReadCloser
	scanner *bufio.Scanner
	current provider.Chunk
	err     error
	done    bool // message_stop has been seen
	closed  bool

	id          string
	model       string
	inputTokens int
}

var _ provider.Stream = (*stream)(nil)

// Next advances to the next chunk worth reporting. Most events on this wire
// produce nothing: pings, block boundaries and the message envelope itself.
func (s *stream) Next() bool {
	if s.err != nil || s.done || s.closed {
		return false
	}

	for s.scanner.Scan() {
		line := s.scanner.Text()

		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, ssePrefix) {
			// The `event:` line, whose value the payload repeats.
			continue
		}

		payload := strings.TrimPrefix(strings.TrimPrefix(line, ssePrefix), " ")

		var evt streamEvent
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			s.err = fmt.Errorf("anthropic: malformed stream frame: %w", err)
			return false
		}

		switch evt.Type {
		case evtMessageStart:
			// Metadata only. Held for the chunks that follow, which is why
			// this type needs state at all.
			s.id = evt.Message.ID
			s.model = evt.Message.Model
			s.inputTokens = evt.Message.Usage.InputTokens

		case evtContentDelta:
			// Only text deltas carry prose; other delta kinds belong to tool
			// use and are not represented in the canonical schema yet.
			if evt.Delta.Type != deltaTypeText || evt.Delta.Text == "" {
				continue
			}
			s.current = provider.Chunk{ID: s.id, Model: s.model, Content: evt.Delta.Text}
			return true

		case evtMessageDelta:
			// The closing metadata: stop reason and the output token count.
			// Emitted as a textless chunk, the same shape the OpenAI stream
			// uses for its final frame.
			s.current = provider.Chunk{
				ID:           s.id,
				Model:        s.model,
				FinishReason: mapStopReason(evt.Delta.StopReason),
				Usage: &provider.Usage{
					PromptTokens:     s.inputTokens,
					CompletionTokens: evt.Usage.OutputTokens,
					TotalTokens:      s.inputTokens + evt.Usage.OutputTokens,
				},
			}
			return true

		case evtMessageStop:
			// A clean end. err stays nil, which is what distinguishes a
			// complete answer from a truncated one.
			s.done = true
			return false

		case evtError:
			msg := evt.Error.Message
			if msg == "" {
				msg = "upstream reported an error mid-stream"
			}
			s.err = &provider.Error{Provider: "anthropic", Message: msg}
			return false

		default:
			// ping, content_block_start, content_block_stop and anything the
			// vendor adds later.
			continue
		}
	}

	switch {
	case s.ctx.Err() != nil:
		s.err = s.ctx.Err()
	case s.scanner.Err() != nil:
		s.err = fmt.Errorf("anthropic: reading stream: %w", s.scanner.Err())
	default:
		s.err = errors.New("anthropic: stream ended without a message_stop event")
	}
	return false
}

// Current returns the chunk Next just advanced to.
func (s *stream) Current() provider.Chunk { return s.current }

// Err reports why the stream ended, or nil if it ended cleanly.
func (s *stream) Err() error { return s.err }

// Close releases the upstream connection. Safe to call more than once.
func (s *stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.body.Close()
}
