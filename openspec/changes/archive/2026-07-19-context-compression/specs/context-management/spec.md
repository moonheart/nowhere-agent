# Spec: context-management (delta for context-compression)

## MODIFIED Requirements

### Requirement: Sliding-window summary strategy
Compression SHALL reduce older context to a summary while retaining recent turns. History SHALL be split by conversation round (an assistant message plus the tool_results answering its tool_use blocks), not by raw message count, so a tool_use/tool_result pair is never severed. Recent rounds SHALL be kept verbatim and older rounds summarized.

#### Scenario: Recent turns preserved
- **WHEN** compression runs
- **THEN** recent rounds are kept verbatim and older rounds are summarized

#### Scenario: Split never severs a tool pair
- **WHEN** compression chooses a split point
- **THEN** it falls on a round boundary so no tool_use block is separated from its tool_result

### Requirement: Compression may use an LLM
Compression SHALL produce summaries via an LLM call that uses the same provider adapter and model as the loop, invoked with tools disabled so the model can only emit text. A heuristic/truncation summary SHALL be used only as a fallback when the LLM call fails.

#### Scenario: LLM-based summary
- **WHEN** a summary is needed
- **THEN** an LLM is invoked to generate it, subject to cost controls

#### Scenario: Summarizer cannot call tools
- **WHEN** the LLM summarizer runs
- **THEN** the request is made with no tools available so the model can only emit text

## ADDED Requirements

### Requirement: Tool-call pairing repair
The compression layer SHALL provide a repair pass that normalizes a message list so every tool_use has a matching tool_result and no tool_result is orphaned, independent of whether compression actually ran.

#### Scenario: Orphan tool result stripped
- **WHEN** a message list contains a tool_result whose tool_use is absent
- **THEN** the repair pass removes it (inserting a placeholder if the message would become empty)

#### Scenario: Dangling tool use answered with error
- **WHEN** a message list contains a tool_use with no matching tool_result
- **THEN** the repair pass appends a synthetic `is_error` tool_result

#### Scenario: Duplicate ids deduplicated
- **WHEN** a message list contains duplicate tool_use or tool_result ids
- **THEN** the repair pass keeps only the first occurrence
