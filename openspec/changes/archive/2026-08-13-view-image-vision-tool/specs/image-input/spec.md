# image-input Specification (delta)

## ADDED Requirements

### Requirement: Authenticated image upload
The system SHALL provide an authenticated upload endpoint that accepts an image, stores it via `ImageStore.Save` (re-encoded to WebP, confined to the owning session's workspace), and returns the resulting session-relative path. Only the session owner (or a principal authorized for the session) SHALL be able to upload into a session's workspace.

#### Scenario: Image uploaded to session workspace
- **WHEN** the session owner POSTs an image to the upload endpoint with the session id
- **THEN** the bytes are written to the session workspace as WebP and the response carries the workspace-relative path

#### Scenario: Non-owner denied upload
- **WHEN** a principal that does not own the session attempts to upload
- **THEN** the upload is denied

#### Scenario: Unsupported payload rejected
- **WHEN** the uploaded payload is not a supported image type or is malformed
- **THEN** the upload is rejected with an error and no workspace file is written

### Requirement: Image parts in the chat wire format
The chat request format SHALL accept an `image` message part carrying a workspace-relative path (and media type), alongside text parts. The assembled user turn SHALL contain a `provider.BlockImage` for each image part, so history, persistence, and the provider serialization all use the canonical image block.

#### Scenario: User message with image part
- **WHEN** a chat request contains a message with text and image parts
- **THEN** the resulting user turn carries the text as a text block and each image as a `BlockImage` with its media type and path

#### Scenario: Image path confined to the session
- **WHEN** an image part references a path outside the session's workspace
- **THEN** the request is rejected or the image degrades to a placeholder rather than being read from outside the session

### Requirement: Vision gate on the main model
The loop SHALL choose between native image blocks and the `view_image` tool based on the main model's capability profile. When `LookupProfile` reports `ImageInput` for the main model, image blocks SHALL be sent to the provider natively. When it reports no `ImageInput`, image blocks in the outgoing (transient) view SHALL be replaced with a text hint identifying the attached image and the `view_image` tool SHALL be available.

#### Scenario: Vision-capable main model
- **WHEN** the main model's profile reports `ImageInput`
- **THEN** image blocks materialize and serialize to the provider-native image source

#### Scenario: Non-vision main model
- **WHEN** the main model's profile reports no `ImageInput`
- **THEN** the outgoing view shows a text hint for each image instead of the image block, and `view_image` is registered

#### Scenario: Unknown model profile
- **WHEN** the main model is not in the profile table
- **THEN** the system keeps a conservative default (no native images; `view_image` available when a vision model is configured)

### Requirement: view_image tool
The system SHALL provide a built-in `view_image` tool that lets the agent inspect an attached image through a separately configured vision model. The tool SHALL take the workspace-relative image path (and an optional question), resolve the image bytes through the session-scoped `ImageResolver`, call the vision model with the image and the question, and return the model's text response as a tool result.

#### Scenario: Model inspects an image
- **WHEN** the model calls `view_image` with a valid workspace-relative path
- **THEN** the image is sent to the configured vision model and the returned description is folded back as the tool result text

#### Scenario: Tool call includes a question
- **WHEN** the model supplies a question with the image path
- **THEN** the vision model receives the question alongside the image and answers it

#### Scenario: Missing image file
- **WHEN** the referenced path does not resolve to a file
- **THEN** the tool returns an error result and the image is not sent to the vision model

#### Scenario: Not registered without a vision model
- **WHEN** no vision model is configured
- **THEN** `view_image` is not registered and image blocks degrade to placeholders as today

### Requirement: Vision model configuration
The system SHALL allow a dedicated vision model to be configured independently of the main model, via provider/model/API key/base URL settings. The vision adapter SHALL be built from these settings, falling back to the platform key semantics used for other providers. When unset, the vision capability is disabled (no `view_image` tool).

#### Scenario: Dedicated vision model configured
- **WHEN** `VISION_*` settings configure a vision model
- **THEN** the `view_image` tool uses that model for non-vision main models

#### Scenario: Unset vision config
- **WHEN** no vision model is configured
- **THEN** no vision adapter is built, `view_image` is absent, and existing non-vision behavior is unchanged

### Requirement: Frontend image attachment and rendering
The chat composer SHALL support attaching images; attachments SHALL be uploaded to the session and rendered in the message history (e.g. via the authenticated file endpoint). Image-bearing messages SHALL round-trip through resume/history so attached images persist across turns.

#### Scenario: User attaches an image
- **WHEN** the user selects an image in the composer
- **THEN** it is uploaded and appears as an attachment preview, and is included in the next chat request as an image part

#### Scenario: Image renders in history
- **WHEN** a message with an image part is displayed (including after resume)
- **THEN** the image is rendered from the authenticated file endpoint rather than dropped
