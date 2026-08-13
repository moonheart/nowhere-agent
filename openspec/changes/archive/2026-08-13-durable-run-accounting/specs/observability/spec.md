# observability Specification (delta)

## MODIFIED Requirements

### Requirement: LLM cost metering
Every LLM call SHALL record input/output/cached tokens and computed cost, attributed to both user and team. Recording SHALL happen at request settle time into the durable usage ledger (see usage-ledger), before any classification, retry, or discard decision — including failed attempts and discarded overflow responses. The run's aggregate is the ledger sum; message-level usage columns remain immutable display snapshots.

#### Scenario: Per-call metering
- **WHEN** an LLM call completes
- **THEN** token counts and cost are recorded against the calling user and their team

#### Scenario: Cost feeds quota
- **WHEN** accumulated cost is queried
- **THEN** it reflects metered usage and is available to quota enforcement

#### Scenario: Failed call still metered
- **WHEN** an LLM call fails or its response is discarded after spending tokens
- **THEN** the spend is still recorded in the usage ledger, so totals do not lose failed requests

#### Scenario: Run aggregate from the ledger
- **WHEN** a run's usage totals are queried
- **THEN** they equal the sum of its ledger rows, recomputed rather than accumulated
