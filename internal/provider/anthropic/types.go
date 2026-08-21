package anthropic

// The types in this file mirror the Anthropic Messages API wire format. They
// are all unexported on purpose: Anthropic's vocabulary must never escape this
// package. Everything that leaves goes through translate.go and speaks the
// canonical provider schema instead.

// apiRequest is the body of POST /v1/messages.
//
// Two differences from the canonical schema are visible right here: System is
// a top-level field rather than a message in the list, and MaxTokens has no
// omitempty because Anthropic requires it on every call.
type apiRequest struct {
	Model       string       `json:"model"`
	MaxTokens   int          `json:"max_tokens"`
	Messages    []apiMessage `json:"messages"`
	System      string       `json:"system,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
	Stream      bool         `json:"stream,omitempty"`
}

// apiMessage is one turn. Anthropic accepts either a plain string or an array
// of content blocks here; the gateway always sends the string form.
type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Roles accepted by the Messages API. Note the absence of "system" and "tool".
const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

// apiResponse is the body of a successful POST /v1/messages.
//
// Content is always an array, even for a plain text answer, and its blocks are
// not necessarily all text. There is no "created" timestamp and no total token
// count: both have to be supplied by the translation.
type apiResponse struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	Role       string            `json:"role"`
	Model      string            `json:"model"`
	Content    []apiContentBlock `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      apiUsage          `json:"usage"`
}

// apiContentBlock is one piece of the answer. Type is "text" for prose, but
// other kinds exist ("tool_use", "thinking", ...) and may be interleaved.
type apiContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// blockTypeText is the only block kind this phase knows how to render.
const blockTypeText = "text"

// apiUsage is Anthropic's token accounting. The field names differ from the
// canonical ones and there is no total.
type apiUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Stop reasons reported by the Messages API.
const (
	stopEndTurn      = "end_turn"
	stopMaxTokens    = "max_tokens"
	stopStopSequence = "stop_sequence"
	stopToolUse      = "tool_use"
)

// apiError is Anthropic's failure envelope. Note the shape differs from
// OpenAI's: there is a discriminator at the top level as well as inside.
type apiError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}
