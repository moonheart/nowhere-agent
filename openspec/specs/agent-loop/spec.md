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
The loop SHALL treat the current conversation context as short-term memory, managing the window without persisting it to long-term memory. The loop SHALL maintain a **working view** — the message list actually sent to the model — separate from the authoritative durable conversation record (the messages table). Compression SHALL rewrite only the working view; the durable record SHALL NOT be altered by compression, so replay/resume and dreaming always see the full history.

#### Scenario: Window management
- **WHEN** accumulated context approaches the model's limit
- **THEN** the loop applies a configured context-management strategy (e.g., truncation/summarization) to stay within budget
- **AND** the strategy is applied to the working view before the next model call, leaving the durable record intact

#### Scenario: Compression rewrites only the working view
- **WHEN** the working view crosses the configured context budget
- **THEN** the loop replaces older turns in the view with a summary before the next model call
- **AND** the durable conversation record (messages table) is unchanged

#### Scenario: Budget is model-relative
- **WHEN** the loop estimates the working view size
- **THEN** compression triggers at a configured fraction of the model's context window, reserving room for the model's reply

### Requirement: Online memory recall injection
The loop SHALL inject relevant long-term memories via the read side of MemoryPort before model inference.

#### Scenario: Recall before inference
- **WHEN** a turn begins
- **THEN** the loop recalls scoped memories relevant to the input and includes them in the prompt
- **AND** the loop never writes to long-term memory directly

### Requirement: Assembled messages available for persistence
The loop SHALL expose each fully-assembled conversation message it produces — the assistant message (text, thinking incl. signature, tool_use blocks) and the tool-result message (user role, tool_result blocks) — so the session runtime can persist them in original form. The incoming user message SHALL likewise be available for persistence at run start.

#### Scenario: Assistant message assembled with all blocks
- **WHEN** the loop finishes streaming one assistant turn that contains thinking, text, and a tool_use
- **THEN** the complete assembled message (including the thinking signature) is available to be persisted as a single unit

#### Scenario: Tool-result message assembled
- **WHEN** tool calls are dispatched and results returned
- **THEN** the tool results are assembled into a user-role message with tool_result blocks available to be persisted

#### Scenario: Thinking signature preserved in the assembled message
- **WHEN** an assistant turn contains a thinking block with a provider signature
- **THEN** the assembled message carries the signature on the block (`ThinkingSignature`) separate from the thinking text

### Requirement: Content deltas emitted to a live channel
The loop's per-token content deltas (text, thinking, tool frames) SHALL be emitted to a live delivery channel rather than to a durable per-token log, so streaming throughput is not gated by a synchronous durable write. Assembled messages SHALL still be exposed for durable persistence (see "Assembled messages available for persistence").

#### Scenario: Delta emitted without durable-write latency
- **WHEN** the loop produces a streaming content delta
- **THEN** it is emitted to the live channel without waiting on a database write

#### Scenario: Assembled message still persisted
- **WHEN** the loop finishes one assistant turn
- **THEN** the assembled message is still exposed for durable persistence even though its individual deltas were not separately persisted

### Requirement: Tool-call pairing preserved in the working view
The loop SHALL guarantee every `tool_use` block is paired with its `tool_result` in the working view before each model call, since the provider API contract requires it. Compression SHALL split history by conversation round (an assistant message plus the tool_results answering it), never by raw message count, so a pair is never severed.

#### Scenario: Compression splits by round
- **WHEN** compression drops older context
- **THEN** it drops whole rounds (an assistant message and the tool_result messages answering its tool_use blocks) so no tool_use is separated from its result

#### Scenario: Orphan tool result removed
- **WHEN** the working view contains a tool_result with no matching tool_use
- **THEN** the orphan is removed before the request is sent (a placeholder keeps the message non-empty if it would otherwise be empty)

#### Scenario: Dangling tool use answered
- **WHEN** the working view contains a tool_use with no matching tool_result
- **THEN** a synthetic `is_error` tool_result is appended before the request is sent

### Requirement: Reactive context-overflow fallback
When the provider rejects a request as too large for the context window, the loop SHALL drop older rounds from the working view and retry a bounded number of times rather than failing the run.

#### Scenario: Overflow retried after dropping rounds
- **WHEN** the provider rejects a request as context-overflow
- **THEN** the loop drops the oldest round(s) from the working view and retries, up to a configured bound

#### Scenario: Non-overflow error fails the run
- **WHEN** the provider returns an error that is not context-overflow
- **THEN** the run fails with that error rather than retrying

### Requirement: Compression circuit breaker
After a configured number of consecutive compression failures, the loop SHALL stop attempting compression for the remainder of the run, relying on the reactive overflow fallback instead of repeatedly paying for a failing summarize call.

#### Scenario: Breaker trips after repeated failures
- **WHEN** compression fails the configured number of consecutive times in one run
- **THEN** the loop stops compressing for the rest of that run

