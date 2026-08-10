// Package openapi serves the platform's OpenAPI 3.0 contract at
// /openapi.json (enterprise integration): the embeddable surface — auth, chat,
// sessions, scheduled tasks, agent definitions, skills, and the admin console —
// so external systems can generate clients instead of reverse-engineering the
// API. The spec is hand-maintained alongside the routes; the canonical wiring
// truth remains cmd/server/main.go.
package openapi

import (
	"encoding/json"
)

// Spec is the OpenAPI 3.0 document. Served at GET /openapi.json (no auth:
// it describes the API, it does not expose it).
type Spec struct {
	OpenAPI    string               `json:"openapi"`
	Info       Info                 `json:"info"`
	Servers    []Server             `json:"servers,omitempty"`
	Paths      map[string]PathItem  `json:"paths"`
	Components Components           `json:"components"`
}

// Info is the OpenAPI info object.
type Info struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Version     string `json:"version"`
}

// Server is one base URL for the API.
type Server struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

// PathItem maps an HTTP method to an operation.
type PathItem map[string]Operation

// Operation is one endpoint.
type Operation struct {
	Summary     string              `json:"summary,omitempty"`
	Description string              `json:"description,omitempty"`
	Tags        []string            `json:"tags,omitempty"`
	Security    []map[string][]any  `json:"security,omitempty"`
	Parameters  []Parameter         `json:"parameters,omitempty"`
	RequestBody *RequestBody        `json:"requestBody,omitempty"`
	Responses   map[string]Response `json:"responses"`
}

// Parameter is one query/path/header parameter.
type Parameter struct {
	Name        string `json:"name"`
	In          string `json:"in"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
	Schema      any    `json:"schema,omitempty"`
}

// RequestBody is one request body.
type RequestBody struct {
	Required bool               `json:"required,omitempty"`
	Content  map[string]Content `json:"content"`
}

// Content is a media type mapping to a schema.
type Content struct {
	Schema any `json:"schema"`
}

// Response is one operation response.
type Response struct {
	Description string               `json:"description"`
	Content     map[string]Content   `json:"content,omitempty"`
}

// Components holds shared schemas and security schemes.
type Components struct {
	Schemas        map[string]any           `json:"schemas,omitempty"`
	SecuritySchemes map[string]SecurityScheme `json:"securitySchemes,omitempty"`
}

// SecurityScheme is the bearer auth scheme.
type SecurityScheme struct {
	Type   string `json:"type"`
	Scheme string `json:"scheme"`
}

// ref renders a #/components/schemas/... reference.
func ref(name string) map[string]any { return map[string]any{"$ref": "#/components/schemas/" + name} }

// Build assembles the document.
func Build() Spec {
	return Spec{
		OpenAPI: "3.0.3",
		Info: Info{
			Title:       "nowhere-agent API",
			Description: "Self-hosted agent platform: chat (ui-message-stream SSE), sessions, scheduled tasks, skills, agent definitions, and administration. Authenticate with a bearer token (user session or an admin-issued sk_ service key) via Authorization: Bearer <token>.",
			Version:     "0.1.0",
		},
		Servers: []Server{{URL: "/"}},
		Paths:   paths(),
		Components: Components{
			Schemas:         schemas(),
			SecuritySchemes: map[string]SecurityScheme{"bearerAuth": {Type: "http", Scheme: "bearer"}},
		},
	}
}

// JSON renders the spec as bytes.
func JSON() ([]byte, error) {
	return json.MarshalIndent(Build(), "", "  ")
}
