# provider-abstraction Specification

## Purpose
TBD - created by archiving change init-nowhere-agent. Update Purpose after archive.
## Requirements
### Requirement: Canonical message model
The system SHALL define a provider-neutral Message model whose content is a structured array of blocks, not a plain string. Supported block types SHALL include Text, ToolUse, ToolResult, Thinking, and CachePoint.

#### Scenario: Structured content
- **WHEN** a message is constructed
- **THEN** its content is an ordered list of typed blocks capable of mixing text, tool use, tool result, and thinking

#### Scenario: Rich tool result
- **WHEN** a tool returns an image or file
- **THEN** the ToolResult block can carry that non-text payload

### Requirement: Thinking round-trip
The canonical model SHALL preserve assistant thinking blocks so they can be returned to the provider on subsequent turns.

#### Scenario: Multi-turn thinking
- **WHEN** an assistant message contains thinking and the conversation continues
- **THEN** the thinking blocks are included in the next request to providers that require them

### Requirement: Prompt caching
The canonical model SHALL support CachePoint markers indicating cacheable prompt prefixes.

#### Scenario: Cache marking
- **WHEN** a stable prompt prefix is assembled
- **THEN** a CachePoint can be attached so supporting providers cache it and reduce input-token cost

### Requirement: Event-based streaming
Provider adapters SHALL expose streaming as an ordered event stream (block_start, block_delta, block_stop, message_stop), not cumulative deltas.

#### Scenario: Interleaved thinking and tool use
- **WHEN** a provider streams thinking and tool-use blocks
- **THEN** the adapter emits ordered events preserving that interleaving

### Requirement: Per-provider adapters
The system SHALL translate between the canonical model and each provider's native API via a thin adapter. Initial adapters SHALL include Anthropic and OpenAI.

#### Scenario: Adapter conformance
- **WHEN** any adapter is exercised against the canonical contract
- **THEN** it round-trips all supported block types and streaming events without loss for the features that provider supports

