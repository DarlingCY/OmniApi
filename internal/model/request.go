package model

// Message is one protocol-neutral conversation turn.
type Message struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"toolCallId,omitempty"`
	Name       string `json:"name,omitempty"`
}

// Request is the canonical inbound request shared by all protocols.
type Request struct {
	Protocol        Protocol
	Raw             map[string]any
	RequestID       string
	Model           string
	System          string
	Messages        []Message
	Tools           []any
	MaxOutputTokens int
	Temperature     *float64
	TopP            *float64
	Stream          bool
}
