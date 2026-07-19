# Spec: agent-loop (delta for context-compression)

## MODIFIED Requirements

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

## ADDED Requirements

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
