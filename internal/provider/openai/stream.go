package openai

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
	// ssePrefix marks the only SSE field this vendor uses.
	ssePrefix = "data:"

	// sseDone is OpenAI's end-of-stream sentinel. It is not JSON.
	sseDone = "[DONE]"

	// maxStreamLine caps one SSE line. bufio.Scanner refuses tokens over
	// 64 KiB by default, and a single frame can exceed that.
	maxStreamLine = 1 << 20 // 1 MiB
)

// This file holds everything about streaming completions. Two things live
// here, and both are yours to write:
//
//  1. ChatStream, below: build the request, send it, and on success hand back
//     a cursor over the events coming down the wire.
//
//  2. An unexported type implementing provider.Stream, with these four
//     methods:
//
//     func (s *stream) Next() bool
//     func (s *stream) Current() provider.Chunk
//     func (s *stream) Err() error
//     func (s *stream) Close() error
//
//     Name it what you like. Add the compile-time assertion
//     `var _ provider.Stream = (*stream)(nil)` so a wrong signature breaks the
//     build here instead of at the call site.
//
// The expected behaviour of both is written down as tests in stream_test.go.
// internal/provider/mock/mock.go has a working reference implementation of the
// same contract, backed by a slice instead of a socket.

// ChatStream implements provider.Provider.
func (c *Client) ChatStream(ctx context.Context, req provider.ChatRequest) (provider.Stream, error) {

	var outReq = req
	outReq.Stream = true

	body, err := marshalWithExtra(outReq)
	if err != nil {
		return nil, fmt.Errorf("stream marshal request: %w", err)
	}

	httpStreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("stream new request fail: %w", err)
	}

	httpStreamReq.Header.Set("Content-Type", "application/json")
	httpStreamReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpStreamReq.Header.Set("Accept", "text/event-stream")

	// bodyclose cannot see through provider.DrainAndClose, which both
	// drains and closes; every path below goes through it.
	//nolint:bodyclose // closed via provider.DrainAndClose
	httpStreamResp, err := c.http.Do(httpStreamReq)
	if err != nil {
		return nil, &provider.Error{
			Provider:   c.Name(),
			Message:    err.Error(),
			Err:        err,
			StatusCode: 0,
		}
	}

	if httpStreamResp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(httpStreamResp.Body, maxErrorBody))

		msg := strings.TrimSpace(string(raw))
		var env errorEnvelope
		if err := json.Unmarshal(raw, &env); err == nil && env.Error.Message != "" {
			msg = env.Error.Message
		}
		if msg == "" {
			msg = httpStreamResp.Status
		}

		provider.DrainAndClose(httpStreamResp.Body)

		return nil, &provider.Error{
			Provider:   c.Name(),
			StatusCode: httpStreamResp.StatusCode,
			Message:    msg,
		}
	}

	scanner := bufio.NewScanner(httpStreamResp.Body)
	scanner.Buffer(nil, maxStreamLine)

	// Ownership of the body passes to the stream, which closes it in Close.
	return &stream{
		ctx:     ctx,
		body:    httpStreamResp.Body,
		scanner: scanner,
	}, nil
}

// streamFrame is the JSON payload of one `data:` line. Unexported, like
// errorEnvelope: it is this vendor's wire format and must not leave the
// package.
//
// Usage reuses provider.Usage because the field names happen to match; it is a
// pointer so an absent "usage" stays distinguishable from one reporting zeros.
type streamFrame struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *provider.Usage `json:"usage"`
}

// stream is a cursor over the SSE frames of one streamed completion.
//
// Holding a context in a struct is normally discouraged in Go, but an iterator
// whose Next takes no arguments has nowhere else to put it. http.Request does
// the same for the same reason.
type stream struct {
	ctx     context.Context
	body    io.ReadCloser
	scanner *bufio.Scanner
	current provider.Chunk
	err     error
	done    bool // the [DONE] sentinel has been seen
	closed  bool // Close has been called
}

var _ provider.Stream = (*stream)(nil)

// Next advances to the next chunk worth reporting.
//
// Most lines on the wire produce nothing -- blank separators, comments, frames
// whose delta is empty -- which is why this is a loop and not a single read.
// It resumes where the previous call stopped, because the scanner holds the
// position.
func (s *stream) Next() bool {
	// Once the stream has ended for any reason, it stays ended.
	if s.err != nil || s.done || s.closed {
		return false
	}

	for s.scanner.Scan() {
		line := s.scanner.Text()

		// Blank lines delimit events; lines starting with ":" are comments,
		// which is how vendors send keep-alives. Neither carries content.
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		// Any other SSE field (event:, id:, retry:) is not used by this vendor.
		if !strings.HasPrefix(line, ssePrefix) {
			continue
		}

		// The spec allows a single optional space after the colon.
		payload := strings.TrimPrefix(strings.TrimPrefix(line, ssePrefix), " ")

		if payload == sseDone {
			// A clean end: err stays nil, which is what tells the caller this
			// was a complete answer rather than a truncated one.
			s.done = true
			return false
		}

		var frame streamFrame
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			s.err = fmt.Errorf("openai: malformed stream frame: %w", err)
			return false
		}

		chunk := provider.Chunk{
			ID:    frame.ID,
			Model: frame.Model,
			Usage: frame.Usage,
		}
		if len(frame.Choices) > 0 {
			chunk.Content = frame.Choices[0].Delta.Content
			chunk.FinishReason = frame.Choices[0].FinishReason
		}

		// The opening frame carries only a role. Reporting it would make the
		// consumer handle chunks that say nothing.
		if chunk.Content == "" && chunk.FinishReason == "" && chunk.Usage == nil {
			continue
		}

		s.current = chunk
		return true
	}

	// Scan returned false: there is no more data. Three different reasons,
	// and the order matters -- a cancelled context also breaks the scanner,
	// but with a vaguer error than the context's own.
	switch {
	case s.ctx.Err() != nil:
		s.err = s.ctx.Err()
	case s.scanner.Err() != nil:
		s.err = fmt.Errorf("openai: reading stream: %w", s.scanner.Err())
	default:
		s.err = errors.New("openai: stream ended without a [DONE] marker")
	}
	return false
}

// Current returns the chunk Next just advanced to.
func (s *stream) Current() provider.Chunk { return s.current }

// Err reports why the stream ended, or nil if it ended cleanly.
func (s *stream) Err() error { return s.err }

// Close releases the upstream connection. Callers are told to defer it
// unconditionally, so it has to survive being called after the loop already
// drained the stream, and twice.
func (s *stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.body.Close()
}
