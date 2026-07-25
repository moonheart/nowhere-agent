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
	BlockImage      BlockType = "image"
)

// Block is one unit of message content. Only the fields relevant to Type are set.
// The JSON tags control how blocks are persisted (messages.content) and shipped
// over the wire: empty fields are omitted, and the materialized base64 payload
// (ImageData) is never serialized — it is transient, rebuilt at send time.
type Block struct {
	Type BlockType `json:"type"`

	// Text (BlockText)
	Text string `json:"text,omitempty"`

	// Thinking (BlockThinking) — must round-trip back to the provider.
	Thinking          string `json:"thinking,omitempty"`
	ThinkingSignature string `json:"thinking_signature,omitempty"`

	// Tool use (BlockToolUse) — assistant requesting a tool call.
	ToolUseID string         `json:"tool_use_id,omitempty"`
	ToolName  string         `json:"tool_name,omitempty"`
	ToolInput map[string]any `json:"tool_input,omitempty"`

	// Tool result (BlockToolResult) — the result fed back to the model.
	ToolResultID string `json:"tool_result_id,omitempty"` // matches the ToolUseID it answers
	ToolContent  string `json:"tool_content,omitempty"`
	IsError      bool   `json:"is_error,omitempty"`

	// ToolMessages carries a tool call's nested content (a subagent's thinking /
	// text / tool-call blocks), for spawn_agent results. It is persisted and used
	// to rebuild the sub-conversation in history; it is NEVER sent back to the
	// provider (the model only sees the collapsed ToolContent).
	ToolMessages []Block `json:"tool_messages,omitempty"`

	// Image (BlockImage) — a reference to an image, never the payload. The
	// image bytes live in the session workspace; the block carries a
	// workspace-relative path. It is materialized to the provider-native base64
	// source at send time (every turn, byte-stable for prompt caching).
	MediaType string `json:"media_type,omitempty"` // e.g. "image/webp"
	ImagePath string `json:"image_path,omitempty"` // workspace-relative path
	// ImageData holds base64 image bytes, populated only transiently by the
	// pre-send materialization transform. It is never persisted (json:"-").
	ImageData string `json:"-"`

	// CachePoint marks this block's prefix as cacheable when true.
	CachePoint bool `json:"cache_point,omitempty"`
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

// StopReason is the provider-neutral reason a generation ended. Each adapter
// maps its native value (Anthropic stop_reason / OpenAI finish_reason) onto
// these; an unrecognised native value passes through as StopReason(raw) so the
// loop can still log it. The empty value (StopUnknown) means the stream ended
// without the adapter reporting a reason.
type StopReason string

const (
	StopUnknown      StopReason = ""              // no reason reported
	StopEndTurn      StopReason = "end_turn"      // natural completion
	StopToolUse      StopReason = "tool_use"      // stopped to call tools
	StopMaxTokens    StopReason = "max_tokens"    // truncated at the token limit
	StopStopSequence StopReason = "stop_sequence" // hit a stop sequence
)
