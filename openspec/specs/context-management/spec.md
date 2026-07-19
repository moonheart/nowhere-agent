# context-management Specification

## Purpose
TBD - created by archiving change init-nowhere-agent. Update Purpose after archive.
## Requirements
### Requirement: Threshold-triggered compression
The system SHALL compress short-term context online when it approaches a configurable fraction of the model's context window.

#### Scenario: Compression on threshold
- **WHEN** the accumulated context crosses the configured threshold
- **THEN** compression runs before the next model call so the conversation can continue within budget

### Requirement: Sliding-window summary strategy
Compression SHALL reduce older context to a summary while retaining recent turns, using a sliding-window strategy.

#### Scenario: Recent turns preserved
- **WHEN** compression runs
- **THEN** recent turns are kept verbatim and older turns are summarized

### Requirement: Separation from dreaming
Online compression SHALL govern only the current session's context; it SHALL NOT write to long-term memory, which is exclusively dreaming's role.

#### Scenario: No double-write
- **WHEN** online compression summarizes context
- **THEN** nothing is persisted to long-term memory by the compression path

#### Scenario: Offline recovery of dropped detail
- **WHEN** detail dropped by online compression is later deemed important
- **THEN** it is recovered into long-term memory by the dreaming worker offline, not by compression

### Requirement: Compression may use an LLM
Compression SHALL be permitted to call an LLM to produce summaries.

#### Scenario: LLM-based summary
- **WHEN** a summary is needed
- **THEN** an LLM may be invoked to generate it, subject to cost controls

### Requirement: Faithful history available for compression
A faithful, full-block conversation history (text, thinking incl. signature, tool_use, tool_result) SHALL be available as the input to online context compression, so that a future compressor can summarize the real conversation rather than a degraded text-only projection. This change establishes the availability of that history; it does not itself add compression behaviour.

#### Scenario: Compressor reads full blocks
- **WHEN** online compression is later wired into the loop (task 4.4)
- **THEN** the history it consumes contains thinking (with signature), tool_use, and tool_result blocks, not just text

#### Scenario: Cross-run history is complete
- **WHEN** a session spans multiple runs
- **THEN** the history available for compression covers all runs' messages in order, rebuilt from the authoritative store

