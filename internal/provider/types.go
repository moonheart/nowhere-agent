// Package provider defines the canonical provider-neutral message model and
// the adapter contract that each LLM provider implements. The canonical model
// follows Anthropic's block shape (design D2): content is a structured block
// array, thinking round-trips, and cache points mark cacheable prefixes.
package provider

// Role is the speaker of a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockType discriminates the kinds of content blocks.
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
	BlockThinking   BlockType = "thinking"
)

// Block is one unit of message content. Only the fields relevant to Type are set.
type Block struct {
	Type BlockType

	// Text (BlockText)
	Text string

	// Thinking (BlockThinking) — must round-trip back to the provider.
	Thinking          string
	ThinkingSignature string

	// Tool use (BlockToolUse) — assistant requesting a tool call.
	ToolUseID string
	ToolName  string
	ToolInput map[string]any

	// Tool result (BlockToolResult) — the result fed back to the model.
	ToolResultID string // matches the ToolUseID it answers
	ToolContent  string
	IsError      bool

	// CachePoint marks this block's prefix as cacheable when true.
	CachePoint bool
}

// Message is one turn in a conversation. Content is an ordered block array.
type Message struct {
	Role    Role
	Content []Block
}

// TextMessage is a convenience constructor for a single-text-block message.
func TextMessage(role Role, text string) Message {
	return Message{Role: role, Content: []Block{{Type: BlockText, Text: text}}}
}

// ToolDefinition describes a callable tool for function calling.
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema map[string]any // JSON Schema
}

// Request is a provider-agnostic generation request.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []ToolDefinition
	MaxTokens int
	// CacheablePrefix, when true, asks the adapter to place a cache point on
	// the stable system/tool prefix to enable prompt caching where supported.
	CacheablePrefix bool
}

// Usage reports token consumption for a completed generation.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}
