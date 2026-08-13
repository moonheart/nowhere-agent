# provider-abstraction Specification (delta)

## MODIFIED Requirements

### Requirement: Image blocks materialized on send
The canonical model SHALL support an image content block that references an image by a workspace-relative path rather than embedding the payload. When a request is built for the provider, each image block SHALL be materialized into the provider-native base64 image source, byte-stably, on every request so prompt caching is preserved. The stored block SHALL NOT contain the base64 payload. Each adapter SHALL serialize image blocks to its provider-native image source form (Anthropic `image`/`base64`, OpenAI `image_url` content parts); an adapter SHALL only degrade an image to a text placeholder when the provider or its configured gateway does not accept image parts for the model being called.

#### Scenario: Path materialized to base64 every turn
- **WHEN** a request is built that includes a prior image block carrying a workspace path
- **THEN** the block is rewritten to the provider-native image source with the image bytes as base64, identically on every request

#### Scenario: Stored block holds no payload
- **WHEN** an image block is persisted
- **THEN** it records the media type and workspace-relative path, not the base64 data

#### Scenario: Missing image file
- **WHEN** a referenced image path no longer resolves to a file
- **THEN** the block is replaced with a text placeholder rather than failing the request

#### Scenario: OpenAI serializes images natively
- **WHEN** the OpenAI adapter builds a request for a vision-capable model and a message contains an image block
- **THEN** the block is serialized as an `image_url` content part carrying the base64 data, not as text

#### Scenario: Non-vision OpenAI model degrades
- **WHEN** the OpenAI adapter builds a request for a model whose profile reports no `ImageInput`
- **THEN** the image block degrades to a text placeholder so the request still sends
