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
Thinking blocks SHALL carry their provider signature as a distinct, preserved value so it can be persisted and sent back to the provider on subsequent turns. The signature SHALL NOT be merged into the thinking text.

#### Scenario: Multi-turn thinking
- **WHEN** an assistant message contains thinking and the conversation continues
- **THEN** the thinking blocks are included in the next request to providers that require them

#### Scenario: Signature captured separately
- **WHEN** the provider streams a thinking block followed by a signature delta
- **THEN** the assembled block has the reasoning text in `Thinking` and the signature in `ThinkingSignature`, not concatenated together

#### Scenario: Signature round-trips
- **WHEN** a persisted thinking block is sent back to the provider on a later turn
- **THEN** its signature is included so the provider can validate the thinking block

#### Scenario: Provider without signatures
- **WHEN** the provider does not emit thinking signatures (e.g. OpenAI-compatible reasoning)
- **THEN** `ThinkingSignature` is empty and the thinking text is unaffected

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

### Requirement: Image blocks materialized on send
The canonical model SHALL support an image content block that references an image by a workspace-relative path rather than embedding the payload. When a request is built for the provider, each image block SHALL be materialized into the provider-native base64 image source, byte-stably, on every request so prompt caching is preserved. The stored block SHALL NOT contain the base64 payload.

#### Scenario: Path materialized to base64 every turn
- **WHEN** a request is built that includes a prior image block carrying a workspace path
- **THEN** the block is rewritten to the provider-native image source with the image bytes as base64, identically on every request

#### Scenario: Stored block holds no payload
- **WHEN** an image block is persisted
- **THEN** it records the media type and workspace-relative path, not the base64 data

#### Scenario: Missing image file
- **WHEN** a referenced image path no longer resolves to a file
- **THEN** the block is replaced with a text placeholder rather than failing the request

