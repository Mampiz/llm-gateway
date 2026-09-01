package anthropic

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

// This file is the anti-corruption layer: the only place where the canonical
// schema and Anthropic's wire format meet. Both functions are pure -- no
// network, no context, no state -- which is exactly why they are the easiest
// thing in the project to test and the hardest thing to get right.

// Roles as they arrive in the canonical schema. They overlap with Anthropic's
// for user and assistant, but the two vocabularies are distinct: "system" and
// "tool" exist on this side and have no equivalent on the other. Naming both
// keeps the translation explicit even where the strings happen to match.
const (
	canonRoleSystem    = "system"
	canonRoleUser      = "user"
	canonRoleAssistant = "assistant"
)

// toAnthropic converts a canonical request into an Anthropic one.
//
// defaultMaxTokens is used when the caller did not ask for a limit, because
// Anthropic requires the field on every request and refusing the call would
// leak that vendor quirk to our clients.
func toAnthropic(req provider.ChatRequest, defaultMaxTokens int) (apiRequest, error) {

	out := apiRequest{
		Model:       req.Model,
		Stream:      req.Stream,
		Temperature: req.Temperature,
		MaxTokens:   defaultMaxTokens,
	}
	if req.MaxTokens != nil {
		out.MaxTokens = *req.MaxTokens
	}

	var systemParts []string

	for _, msg := range req.Messages {
		switch msg.Role {
		case canonRoleSystem:
			systemParts = append(systemParts, msg.Content)

		case canonRoleUser:
			out.Messages = append(out.Messages, apiMessage{Role: roleUser, Content: msg.Content})

		case canonRoleAssistant:
			out.Messages = append(out.Messages, apiMessage{Role: roleAssistant, Content: msg.Content})

		default:
			return apiRequest{}, fmt.Errorf("role %q has no equivalent in the Messages API", msg.Role)
		}
	}

	out.System = strings.Join(systemParts, "\n\n")

	if len(out.Messages) == 0 {
		return apiRequest{}, errors.New("request has no messages left once system turns are hoisted")
	}

	return out, nil
}

// fromAnthropic converts an Anthropic response into a canonical one.
func fromAnthropic(resp apiResponse) (*provider.ChatResponse, error) {

	var tmpChoice provider.Choice

	out := &provider.ChatResponse{
		ID:      resp.ID,
		Model:   resp.Model,
		Created: time.Now().Unix(),
		Object:  "chat.completion",
	}

	tmpChoice.Index = 0
	tmpChoice.Message.Role = canonRoleAssistant
	out.Usage.CompletionTokens = resp.Usage.OutputTokens
	out.Usage.PromptTokens = resp.Usage.InputTokens
	out.Usage.TotalTokens = resp.Usage.InputTokens + resp.Usage.OutputTokens

	for _, msg := range resp.Content {
		if msg.Type == blockTypeText {
			tmpChoice.Message.Content += msg.Text
		}
	}

	// An empty result covers both a missing content array and an array
	// carrying no text at all.
	if tmpChoice.Message.Content == "" {
		return nil, errors.New("response carries no text content")
	}

	tmpChoice.FinishReason = mapStopReason(resp.StopReason)

	out.Choices = append(out.Choices, tmpChoice)

	return out, nil

}

// mapStopReason translates this vendor's stop reason vocabulary into the
// canonical one. It is shared by the buffered and the streaming paths, which
// receive the same values through different events.
//
// Unknown values pass through untouched: a reason we do not recognise is more
// informative than an empty string, and claiming a truncated answer stopped
// normally would be a lie.
func mapStopReason(reason string) string {
	switch reason {
	case stopEndTurn, stopStopSequence:
		return "stop"
	case stopMaxTokens:
		return "length"
	case stopToolUse:
		return "tool_calls"
	default:
		return reason
	}
}
