# Spec: provider-abstraction (delta for persist-raw-messages)

## MODIFIED Requirements

### Requirement: Thinking round-trips with its signature
Thinking blocks SHALL carry their provider signature as a distinct, preserved value so it can be persisted and sent back to the provider on subsequent turns. The signature SHALL NOT be merged into the thinking text.

#### Scenario: Signature captured separately
- **WHEN** the provider streams a thinking block followed by a signature delta
- **THEN** the assembled block has the reasoning text in `Thinking` and the signature in `ThinkingSignature`, not concatenated together

#### Scenario: Signature round-trips
- **WHEN** a persisted thinking block is sent back to the provider on a later turn
- **THEN** its signature is included so the provider can validate the thinking block

#### Scenario: Provider without signatures
- **WHEN** the provider does not emit thinking signatures (e.g. OpenAI-compatible reasoning)
- **THEN** `ThinkingSignature` is empty and the thinking text is unaffected

## ADDED Requirements

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
