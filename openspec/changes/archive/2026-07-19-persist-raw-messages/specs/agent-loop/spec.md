# Spec: agent-loop (delta for persist-raw-messages)

## ADDED Requirements

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
