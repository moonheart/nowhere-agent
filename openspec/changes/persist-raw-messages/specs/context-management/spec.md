# Spec: context-management (delta for persist-raw-messages)

## ADDED Requirements

### Requirement: Faithful history available for compression
A faithful, full-block conversation history (text, thinking incl. signature, tool_use, tool_result) SHALL be available as the input to online context compression, so that a future compressor can summarize the real conversation rather than a degraded text-only projection. This change establishes the availability of that history; it does not itself add compression behaviour.

#### Scenario: Compressor reads full blocks
- **WHEN** online compression is later wired into the loop (task 4.4)
- **THEN** the history it consumes contains thinking (with signature), tool_use, and tool_result blocks, not just text

#### Scenario: Cross-run history is complete
- **WHEN** a session spans multiple runs
- **THEN** the history available for compression covers all runs' messages in order, rebuilt from the authoritative store
