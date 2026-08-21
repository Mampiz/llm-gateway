package anthropic

import (
	"strings"
	"testing"

	"github.com/Mampiz/llm-gateway/internal/provider"
)

// These tests are the specification for translate.go. They are red until the
// two translation functions are implemented; making them green is the task.
//
// Every assertion encodes a decision that was argued for explicitly. If you
// disagree with one, change the test and say why -- but change it on purpose,
// never to make a failure go away.

const testDefaultMaxTokens = 4096

func userMsg(text string) provider.Message {
	return provider.Message{Role: "user", Content: text}
}

// --- toAnthropic -----------------------------------------------------------

func TestToAnthropic_CopiesTheBasics(t *testing.T) {
	temp := 0.3
	got, err := toAnthropic(provider.ChatRequest{
		Model:       "claude-sonnet-5",
		Messages:    []provider.Message{userMsg("hello")},
		Temperature: &temp,
	}, testDefaultMaxTokens)
	if err != nil {
		t.Fatalf("toAnthropic() failed: %v", err)
	}

	if got.Model != "claude-sonnet-5" {
		t.Errorf("Model = %q, want claude-sonnet-5", got.Model)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("len(Messages) = %d, want 1", len(got.Messages))
	}
	if got.Messages[0].Role != roleUser || got.Messages[0].Content != "hello" {
		t.Errorf("Messages[0] = %+v, want the original user turn", got.Messages[0])
	}
	if got.Temperature == nil || *got.Temperature != temp {
		t.Errorf("Temperature = %v, want %v", got.Temperature, temp)
	}
}

// A system prompt is a message in the canonical schema but a top-level field
// in Anthropic's. It has to be lifted out of the list entirely.
func TestToAnthropic_HoistsSystemPrompt(t *testing.T) {
	got, err := toAnthropic(provider.ChatRequest{
		Model: "claude-sonnet-5",
		Messages: []provider.Message{
			{Role: "system", Content: "be brief"},
			userMsg("hello"),
		},
	}, testDefaultMaxTokens)
	if err != nil {
		t.Fatalf("toAnthropic() failed: %v", err)
	}

	if got.System != "be brief" {
		t.Errorf("System = %q, want %q", got.System, "be brief")
	}
	if len(got.Messages) != 1 {
		t.Fatalf("len(Messages) = %d, want 1: the system turn must not remain in the list", len(got.Messages))
	}
	if got.Messages[0].Role != roleUser {
		t.Errorf("Messages[0].Role = %q, want user", got.Messages[0].Role)
	}
}

// Several system turns are joined rather than dropped or fought over. They are
// independent instruction blocks, so they get a blank line between them.
func TestToAnthropic_ConcatenatesSystemPrompts(t *testing.T) {
	got, err := toAnthropic(provider.ChatRequest{
		Model: "claude-sonnet-5",
		Messages: []provider.Message{
			{Role: "system", Content: "be brief"},
			userMsg("hello"),
			{Role: "system", Content: "answer in Spanish"},
			{Role: "assistant", Content: "hola"},
			userMsg("again"),
		},
	}, testDefaultMaxTokens)
	if err != nil {
		t.Fatalf("toAnthropic() failed: %v", err)
	}

	want := "be brief\n\nanswer in Spanish"
	if got.System != want {
		t.Errorf("System = %q, want %q", got.System, want)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("len(Messages) = %d, want 3 non-system turns in their original order", len(got.Messages))
	}
	if got.Messages[0].Content != "hello" || got.Messages[2].Content != "again" {
		t.Errorf("messages lost their order: %+v", got.Messages)
	}
}

func TestToAnthropic_NoSystemPrompt(t *testing.T) {
	got, err := toAnthropic(provider.ChatRequest{
		Model:    "claude-sonnet-5",
		Messages: []provider.Message{userMsg("hello")},
	}, testDefaultMaxTokens)
	if err != nil {
		t.Fatalf("toAnthropic() failed: %v", err)
	}
	if got.System != "" {
		t.Errorf("System = %q, want empty so the field is omitted entirely", got.System)
	}
}

// Anthropic rejects any request without max_tokens. Rather than let that
// requirement reach our clients -- and break phase 5's fallback for every
// request that omits it -- the gateway supplies a default.
func TestToAnthropic_MaxTokens(t *testing.T) {
	limit := 128

	tests := []struct {
		name string
		req  provider.ChatRequest
		want int
	}{
		{
			name: "caller supplied a limit",
			req:  provider.ChatRequest{Model: "m", Messages: []provider.Message{userMsg("hi")}, MaxTokens: &limit},
			want: 128,
		},
		{
			name: "caller supplied none",
			req:  provider.ChatRequest{Model: "m", Messages: []provider.Message{userMsg("hi")}},
			want: testDefaultMaxTokens,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := toAnthropic(tt.req, testDefaultMaxTokens)
			if err != nil {
				t.Fatalf("toAnthropic() failed: %v", err)
			}
			if got.MaxTokens != tt.want {
				t.Errorf("MaxTokens = %d, want %d", got.MaxTokens, tt.want)
			}
		})
	}
}

func TestToAnthropic_Rejects(t *testing.T) {
	tests := []struct {
		name string
		req  provider.ChatRequest
	}{
		{
			name: "nothing left after removing the system turns",
			req: provider.ChatRequest{
				Model:    "m",
				Messages: []provider.Message{{Role: "system", Content: "be brief"}},
			},
		},
		{
			// Tool results have no representation in this phase. Forwarding
			// them silently would produce a confusing upstream error.
			name: "a role Anthropic cannot represent",
			req: provider.ChatRequest{
				Model:    "m",
				Messages: []provider.Message{userMsg("hi"), {Role: "tool", Content: "{}"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := toAnthropic(tt.req, testDefaultMaxTokens); err == nil {
				t.Error("toAnthropic() returned nil error, want a rejection")
			}
		})
	}
}

// --- fromAnthropic ---------------------------------------------------------

func textBlock(s string) apiContentBlock {
	return apiContentBlock{Type: blockTypeText, Text: s}
}

func TestFromAnthropic_BuildsASingleChoice(t *testing.T) {
	got, err := fromAnthropic(apiResponse{
		ID:         "msg_123",
		Type:       "message",
		Role:       roleAssistant,
		Model:      "claude-sonnet-5",
		Content:    []apiContentBlock{textBlock("hi there")},
		StopReason: stopEndTurn,
		Usage:      apiUsage{InputTokens: 9, OutputTokens: 2},
	})
	if err != nil {
		t.Fatalf("fromAnthropic() failed: %v", err)
	}

	if got.ID != "msg_123" {
		t.Errorf("ID = %q, want msg_123", got.ID)
	}
	if got.Model != "claude-sonnet-5" {
		t.Errorf("Model = %q, want claude-sonnet-5", got.Model)
	}
	// "object" is part of the canonical schema; Anthropic never sends it.
	if got.Object != "chat.completion" {
		t.Errorf("Object = %q, want chat.completion", got.Object)
	}
	// Anthropic sends no timestamp either, so one has to be supplied.
	if got.Created == 0 {
		t.Error("Created = 0, want a timestamp the vendor does not provide")
	}
	if len(got.Choices) != 1 {
		t.Fatalf("len(Choices) = %d, want exactly 1: Anthropic has no notion of n", len(got.Choices))
	}
	if got.Choices[0].Message.Role != "assistant" {
		t.Errorf("role = %q, want assistant", got.Choices[0].Message.Role)
	}
	if got.Choices[0].Message.Content != "hi there" {
		t.Errorf("content = %q, want %q", got.Choices[0].Message.Content, "hi there")
	}
}

// content is an array whose blocks are not all text. Assuming Content[0].Text
// works right up until a tool_use or thinking block arrives first.
func TestFromAnthropic_FlattensContentBlocks(t *testing.T) {
	got, err := fromAnthropic(apiResponse{
		ID:    "msg_1",
		Model: "claude-sonnet-5",
		Content: []apiContentBlock{
			{Type: "thinking", Text: "internal reasoning, not for the client"},
			textBlock("first part "),
			{Type: "tool_use", Text: ""},
			textBlock("second part"),
		},
		StopReason: stopEndTurn,
	})
	if err != nil {
		t.Fatalf("fromAnthropic() failed: %v", err)
	}

	want := "first part second part"
	if got.Choices[0].Message.Content != want {
		t.Errorf("content = %q, want %q: only text blocks, joined in order",
			got.Choices[0].Message.Content, want)
	}
}

func TestFromAnthropic_MapsStopReason(t *testing.T) {
	tests := []struct {
		anthropic string
		want      string
	}{
		{stopEndTurn, "stop"},
		{stopStopSequence, "stop"},
		{stopMaxTokens, "length"},
		{stopToolUse, "tool_calls"},
		// An unknown reason is passed through rather than blanked: a value we
		// do not recognise is still more useful than nothing.
		{"some_future_reason", "some_future_reason"},
	}

	for _, tt := range tests {
		t.Run(tt.anthropic, func(t *testing.T) {
			got, err := fromAnthropic(apiResponse{
				ID:         "msg_1",
				Content:    []apiContentBlock{textBlock("x")},
				StopReason: tt.anthropic,
			})
			if err != nil {
				t.Fatalf("fromAnthropic() failed: %v", err)
			}
			if got.Choices[0].FinishReason != tt.want {
				t.Errorf("FinishReason = %q, want %q", got.Choices[0].FinishReason, tt.want)
			}
		})
	}
}

func TestFromAnthropic_MapsUsage(t *testing.T) {
	got, err := fromAnthropic(apiResponse{
		ID:         "msg_1",
		Content:    []apiContentBlock{textBlock("x")},
		StopReason: stopEndTurn,
		Usage:      apiUsage{InputTokens: 40, OutputTokens: 2},
	})
	if err != nil {
		t.Fatalf("fromAnthropic() failed: %v", err)
	}

	if got.Usage.PromptTokens != 40 {
		t.Errorf("PromptTokens = %d, want 40", got.Usage.PromptTokens)
	}
	if got.Usage.CompletionTokens != 2 {
		t.Errorf("CompletionTokens = %d, want 2", got.Usage.CompletionTokens)
	}
	// Anthropic sends no total, so it has to be computed.
	if got.Usage.TotalTokens != 42 {
		t.Errorf("TotalTokens = %d, want 42", got.Usage.TotalTokens)
	}
}

// Validate at the boundary so no consumer downstream ever has to wonder
// whether Choices[0] exists or holds anything.
func TestFromAnthropic_RejectsUnusableResponses(t *testing.T) {
	tests := []struct {
		name string
		resp apiResponse
	}{
		{"no content at all", apiResponse{ID: "msg_1", StopReason: stopEndTurn}},
		{"only non-text blocks", apiResponse{
			ID:         "msg_1",
			Content:    []apiContentBlock{{Type: "tool_use"}},
			StopReason: stopToolUse,
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fromAnthropic(tt.resp)
			if err == nil {
				t.Fatalf("fromAnthropic() = %+v, nil; want an error", got)
			}
			if got != nil {
				t.Errorf("response = %+v, want nil alongside an error", got)
			}
		})
	}
}

// A round trip is the clearest statement of what the layer is for: what the
// client sent must survive translation to the vendor and back.
func TestTranslate_RoundTrip(t *testing.T) {
	req := provider.ChatRequest{
		Model: "claude-sonnet-5",
		Messages: []provider.Message{
			{Role: "system", Content: "be brief"},
			userMsg("what is 2+2?"),
		},
	}

	apiReq, err := toAnthropic(req, testDefaultMaxTokens)
	if err != nil {
		t.Fatalf("toAnthropic() failed: %v", err)
	}

	// What a healthy upstream would answer to that.
	apiResp := apiResponse{
		ID:         "msg_round",
		Role:       roleAssistant,
		Model:      apiReq.Model,
		Content:    []apiContentBlock{textBlock("4")},
		StopReason: stopEndTurn,
		Usage:      apiUsage{InputTokens: 12, OutputTokens: 1},
	}

	resp, err := fromAnthropic(apiResp)
	if err != nil {
		t.Fatalf("fromAnthropic() failed: %v", err)
	}

	if resp.Model != req.Model {
		t.Errorf("Model = %q, want the requested %q", resp.Model, req.Model)
	}
	if !strings.Contains(resp.Choices[0].Message.Content, "4") {
		t.Errorf("content = %q, want the answer preserved", resp.Choices[0].Message.Content)
	}
	if resp.Usage.TotalTokens != 13 {
		t.Errorf("TotalTokens = %d, want 13", resp.Usage.TotalTokens)
	}
}
