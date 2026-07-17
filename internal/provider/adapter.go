package provider

// EventType discriminates streaming events. Streaming is an ordered event
// stream (not cumulative deltas) so thinking and tool_use interleave correctly.
type EventType string

const (
	EventMessageStart EventType = "message_start"
	EventBlockStart   EventType = "block_start"
	EventBlockDelta   EventType = "block_delta"
	EventBlockStop    EventType = "block_stop"
	EventMessageStop  EventType = "message_stop"
	EventError        EventType = "error"
)

// Event is one streaming event from a provider.
type Event struct {
	Type EventType

	// Index identifies which content block this event concerns (for block events).
	Index int

	// Block is set on EventBlockStart with the initial (possibly partial) block.
	Block *Block

	// Delta carries incremental text for EventBlockDelta.
	// For text blocks this is text; for thinking, thinking text; for tool_use,
	// partial JSON of the input.
	Delta string

	// Usage is set on EventMessageStop.
	Usage *Usage

	// Err is set on EventError.
	Err error
}

// Adapter is the contract each LLM provider implements. It translates between
// the canonical model and the provider-native API. Implementations must be
// safe for concurrent use.
type Adapter interface {
	// Name identifies the provider (e.g. "anthropic", "openai").
	Name() string

	// Stream starts a generation and returns a channel of events. The channel
	// is closed when generation completes or fails. The returned channel emits
	// an EventError (and closes) on failure.
	Stream(req Request) (<-chan Event, error)
}
