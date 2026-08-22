package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

// streamFrame is one `chat.completion.chunk` in the shape OpenAI clients
// expect. No translation happens here: the canonical schema already is this
// format, so the mapping is field for field.
type streamFrame struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices []streamChoice  `json:"choices"`
	Usage   *provider.Usage `json:"usage,omitempty"`
}

type streamChoice struct {
	Index int         `json:"index"`
	Delta streamDelta `json:"delta"`
	// A pointer so an absent reason serializes as null rather than "", which
	// is what real OpenAI sends and what clients check against.
	FinishReason *string `json:"finish_reason"`
}

type streamDelta struct {
	Content string `json:"content,omitempty"`
}

const streamObject = "chat.completion.chunk"

// streamChatCompletions answers a streaming request by tunnelling the
// provider's chunks to the client as Server-Sent Events.
func (s *Server) streamChatCompletions(w http.ResponseWriter, r *http.Request, p provider.Provider, req provider.ChatRequest) {
	// Open the stream before touching the response. Until the first byte goes
	// out the status is still ours to choose, so a provider that refuses to
	// start can be reported as a proper HTTP error instead of an empty stream.
	//
	// No deadline is layered on r.Context() here, unlike the non-streaming
	// path: a total timeout would cut a long generation short. What this
	// wants instead is an idle timeout, which a sequential loop cannot
	// express.
	streamCtx, cancel := context.WithCancel(r.Context())
	defer cancel()

	stream, err := p.ChatStream(streamCtx, req)
	if err != nil {
		if errors.Is(err, provider.ErrStreamingNotSupported) {
			writeError(w, http.StatusNotImplemented, "not_implemented",
				"provider "+p.Name()+" does not support streaming")
			return
		}
		s.writeProviderError(w, r, p.Name(), err)
		return
	}

	// Past this point the status is committed and cannot be revised.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Tell nginx and friends not to buffer, which would defeat the whole
	// exercise without any error to show for it.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	// Push the headers out now so the client sees an open connection
	// immediately, before the model has produced anything.
	_ = rc.Flush()

	created := time.Now().Unix()

	ch := make(chan provider.Chunk, 8)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		// The stream is owned by this goroutine alone. Next, Current and
		// Close all run here, so there is nothing to synchronise: the handler
		// stops it by cancelling the context, never by touching it directly.
		defer func() { _ = stream.Close() }()

		for stream.Next() {
			select {
			case ch <- stream.Current():
			case <-streamCtx.Done():
				errCh <- streamCtx.Err()
				return
			}
		}
		errCh <- stream.Err()
	}()

	idleTimer := time.NewTimer(s.streamIdle)
	defer idleTimer.Stop()
	heartbeat := time.NewTicker(s.streamHeartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				if err := <-errCh; err != nil {
					writeSSEError(w, rc, err)
					return
				}
				writeSSEDone(w, rc)
				return
			}

			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(s.streamIdle)

			frame := toStreamFrame(chunk, created)

			payload, err := json.Marshal(frame)
			if err != nil {
				s.logger.Error("encoding stream frame", "error", err,
					"request_id", RequestIDFrom(r.Context()))
				return
			}
			if !writeSSE(w, rc, payload) {
				// The client is gone. Returning cancels the context, which is
				// what tears down the upstream call and stops the meter.
				return
			}

		case <-idleTimer.C:
			cancel()
			writeSSEError(w, rc, context.DeadlineExceeded)
			return

		case <-heartbeat.C:
			if !writeSSEComment(w, rc) {
				return
			}
		}
	}

}

// toStreamFrame maps a canonical chunk onto the wire frame.
func toStreamFrame(chunk provider.Chunk, created int64) streamFrame {
	choice := streamChoice{
		Index: 0,
		Delta: streamDelta{Content: chunk.Content},
	}
	if chunk.FinishReason != "" {
		reason := chunk.FinishReason
		choice.FinishReason = &reason
	}

	return streamFrame{
		ID:      chunk.ID,
		Object:  streamObject,
		Created: created,
		Model:   chunk.Model,
		Choices: []streamChoice{choice},
		Usage:   chunk.Usage,
	}
}

// writeSSE emits one event and pushes it out. The two newlines matter: the
// first ends the line, the second ends the event. With only one the client
// waits forever for an event that never completes.
//
// It reports false once writing fails, which is how a client that walked away
// makes itself known.
func writeSSE(w io.Writer, rc *http.ResponseController, payload []byte) bool {
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return false
	}
	// A failed flush is not worth acting on: the next write will fail too.
	_ = rc.Flush()
	return true
}

// writeSSEError reports a mid-stream failure in the same envelope the
// non-streaming path uses, so clients need only one error parser.
func writeSSEError(w io.Writer, rc *http.ResponseController, err error) {
	var env errorEnvelope
	env.Error.Type = "upstream_error"
	env.Error.Message = err.Error()

	var pErr *provider.Error
	if errors.As(err, &pErr) && pErr.Message != "" {
		env.Error.Message = pErr.Message
	}

	payload, mErr := json.Marshal(env)
	if mErr != nil {
		return
	}
	writeSSE(w, rc, payload)
}

func writeSSEDone(w io.Writer, rc *http.ResponseController) {
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return
	}
	_ = rc.Flush()
}

// writeSSEComment emits an SSE comment, the standard keep-alive. Clients
// ignore it by specification, so it never reaches the application: its only
// job is to prove the connection is alive to whatever sits in between.
func writeSSEComment(w io.Writer, rc *http.ResponseController) bool {
	if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
		return false
	}
	// A failed flush is not worth acting on: the next write will fail too.
	_ = rc.Flush()
	return true
}
