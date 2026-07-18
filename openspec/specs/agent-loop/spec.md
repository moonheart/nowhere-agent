# agent-loop Specification

## Purpose
TBD - created by archiving change init-nowhere-agent. Update Purpose after archive.
## Requirements
### Requirement: Think-tool-think orchestration
The system SHALL run a self-built loop that alternates model inference and tool execution until the assistant produces a final response or a stop condition is met.

#### Scenario: Single-turn response
- **WHEN** the model returns a message with no tool-use blocks
- **THEN** the loop emits that message as the final assistant response and stops

#### Scenario: Tool-use round trip
- **WHEN** the model returns one or more tool-use blocks
- **THEN** the loop dispatches each tool, appends tool-result blocks, and invokes the model again

#### Scenario: Loop guard
- **WHEN** the number of iterations exceeds a configured maximum
- **THEN** the loop stops and returns a termination notice instead of looping forever

### Requirement: Streaming output
The loop SHALL stream assistant output to the gateway as an ordered event stream, not as a single buffered payload.

#### Scenario: Incremental delivery
- **WHEN** the model produces output
- **THEN** consumers receive block-start/delta/stop events incrementally over WS/SSE

### Requirement: Short-term memory is in-context
The loop SHALL treat the current conversation context as short-term memory, managing the window without persisting it to long-term memory.

#### Scenario: Window management
- **WHEN** accumulated context approaches the model's limit
- **THEN** the loop applies a configured context-management strategy (e.g., truncation/summarization) to stay within budget

### Requirement: Online memory recall injection
The loop SHALL inject relevant long-term memories via the read side of MemoryPort before model inference.

#### Scenario: Recall before inference
- **WHEN** a turn begins
- **THEN** the loop recalls scoped memories relevant to the input and includes them in the prompt
- **AND** the loop never writes to long-term memory directly

