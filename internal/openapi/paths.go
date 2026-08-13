package openapi

// paths lists the embeddable endpoints. Chat is the heart of the platform:
// POST /api/chat streams the ui-message-stream protocol (data: <json>\n\n …
// data: [DONE]) and accepts attach/resume via the session_id/history payloads.
func paths() map[string]PathItem {
	bearer := []map[string][]any{{"bearerAuth": {}}}

	jsonBody := func(schema any) *RequestBody {
		return &RequestBody{
			Required: true,
			Content:  map[string]Content{"application/json": {Schema: schema}},
		}
	}
	jsonResp := func(desc string, schema any) map[string]Response {
		return map[string]Response{
			"200": {Description: desc, Content: map[string]Content{"application/json": {Schema: schema}}},
			"401": {Description: "invalid or missing bearer token"},
		}
	}
	textResp := func(desc string) map[string]Response {
		return map[string]Response{
			"200": {Description: desc},
			"401": {Description: "invalid or missing bearer token"},
		}
	}

	return map[string]PathItem{
		// ---- auth (open) ----
		"/api/auth/signup": {
			"post": Operation{
				Summary: "Create an account and receive a bearer token",
				Tags:    []string{"auth"},
				RequestBody: jsonBody(map[string]any{
					"type":     "object",
					"required": []string{"email", "password"},
					"properties": map[string]any{
						"email":        map[string]any{"type": "string", "format": "email"},
						"password":     map[string]any{"type": "string"},
						"display_name": map[string]any{"type": "string"},
					},
				}),
				Responses: jsonResp("created; token + user", ref("AuthResponse")),
			},
		},
		"/api/auth/login": {
			"post": Operation{
				Summary:     "Sign in with email + password",
				Tags:        []string{"auth"},
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"email", "password"}, "properties": map[string]any{"email": map[string]any{"type": "string"}, "password": map[string]any{"type": "string"}}}),
				Responses:   jsonResp("ok; token + user", ref("AuthResponse")),
			},
		},
		"/api/auth/logout": {
			"post": Operation{
				Summary:  "Invalidate the current bearer token",
				Tags:     []string{"auth"},
				Security: bearer,
				Responses: map[string]Response{
					"204": {Description: "token revoked"},
					"401": {Description: "invalid or missing bearer token"},
				},
			},
		},
		"/api/auth/totp/verify": {
			"post": Operation{
				Summary:     "Complete a password+TOTP login: redeem the one-shot challenge with the authenticator code",
				Tags:        []string{"auth"},
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"totp_token", "code"}, "properties": map[string]any{"totp_token": map[string]any{"type": "string"}, "code": map[string]any{"type": "string"}}}),
				Responses:   jsonResp("token + user", ref("AuthResponse")),
			},
		},
		"/api/auth/phone/request-code": {
			"post": Operation{
				Summary:     "Send a verification code to a phone number",
				Tags:        []string{"auth"},
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"phone"}, "properties": map[string]any{"phone": map[string]any{"type": "string"}}}),
				Responses: map[string]Response{
					"204": {Description: "code sent"},
					"429": {Description: "code sent too recently or daily quota exceeded"},
				},
			},
		},
		"/api/auth/phone/verify": {
			"post": Operation{
				Summary:     "Verify a phone code and receive a bearer token",
				Tags:        []string{"auth"},
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"phone", "code"}, "properties": map[string]any{"phone": map[string]any{"type": "string"}, "code": map[string]any{"type": "string"}}}),
				Responses:   jsonResp("token + user", ref("AuthResponse")),
			},
		},

		// ---- chat (the core embedding surface) ----
		"/api/chat": {
			"post": Operation{
				Summary:     "Run a chat turn; streams ui-message-stream frames",
				Description: "Sends a message and streams the assistant's turn as SSE frames of ui-message-stream (data: <json>\\n\\n, terminated by data: [DONE]). Include session_id to continue an existing conversation (the server rebuilds history from its own durable record); omit it to create a new session. resume_mode/fold batch controls the interaction path.",
				Tags:        []string{"chat"},
				Security:    bearer,
				RequestBody: jsonBody(ref("ChatRequest")),
				Responses: map[string]Response{
					"200": {Description: "ui-message-stream SSE (data: <json>\\n\\n … data: [DONE])"},
					"401": {Description: "invalid or missing bearer token"},
					"429": {Description: "monthly token budget exceeded; Retry-After set"},
				},
			},
		},
		"/api/chat/cancel": {
			"post": Operation{
				Summary:     "Cancel the active run on a session",
				Tags:        []string{"chat"},
				Security:    bearer,
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"session_id"}, "properties": map[string]any{"session_id": map[string]any{"type": "string"}}}),
				Responses:   textResp("cancelled"),
			},
		},
		"/api/chat/history": {
			"get": Operation{
				Summary:    "Fetch a session's durable message history",
				Tags:       []string{"chat"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "session_id", In: "query", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  jsonResp("messages", map[string]any{"type": "array", "items": ref("Message")}),
			},
		},
		"/api/chat/sessions": {
			"get": Operation{
				Summary:  "List the caller's conversations (keyset pagination)",
				Tags:     []string{"chat"},
				Security: bearer,
				Parameters: []Parameter{
					{Name: "limit", In: "query", Schema: map[string]any{"type": "integer"}},
					{Name: "cursor", In: "query", Description: "updated_at+id cursor from the previous page", Schema: map[string]any{"type": "string"}},
				},
				Responses: jsonResp("sessions + next cursor", ref("SessionPage")),
			},
		},
		"/api/chat/resume": {
			"post": Operation{
				Summary:     "Re-stream an in-flight run from an offset",
				Description: "Attaches to the session's active run and streams its ui-message-stream continuation from `after`; a settled run has nothing to re-stream.",
				Tags:        []string{"chat"},
				Security:    bearer,
				Parameters: []Parameter{
					{Name: "threadId", In: "query", Required: true, Schema: map[string]any{"type": "string"}},
					{Name: "after", In: "query", Schema: map[string]any{"type": "integer", "description": "broker offset to resume from (0 = from the start)"}},
				},
				Responses: map[string]Response{
					"200": {Description: "ui-message-stream SSE (data: <json>\\n\\n … data: [DONE])"},
					"401": {Description: "invalid or missing bearer token"},
				},
			},
		},
		"/api/chat/sessions/{id}/active": {
			"get": Operation{
				Summary:    "Report whether the session has a run in flight",
				Tags:       []string{"chat"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  jsonResp("active flag", map[string]any{"type": "object", "properties": map[string]any{"active": map[string]any{"type": "boolean"}}}),
			},
		},
		"/api/chat/sessions/{id}/state": {
			"post": Operation{
				Summary:     "Write one client-settable session-state key",
				Tags:        []string{"chat"},
				Security:    bearer,
				Parameters:  []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"key"}, "properties": map[string]any{"key": map[string]any{"type": "string"}, "value": map[string]any{"type": "object"}}}),
				Responses: map[string]Response{
					"200": {Description: "written", Content: map[string]Content{"application/json": {Schema: map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}, "key": map[string]any{"type": "string"}}}}}},
					"403": {Description: "key not client-settable"},
				},
			},
		},

		// ---- scheduled tasks (self-service automation) ----
		"/api/me/scheduled-tasks": {
			"get": Operation{
				Summary:  "List the caller's scheduled tasks",
				Tags:     []string{"scheduled-tasks"},
				Security: bearer,
				Responses: jsonResp("tasks", map[string]any{
					"type":       "object",
					"properties": map[string]any{"tasks": map[string]any{"type": "array", "items": ref("ScheduledTask")}},
				}),
			},
			"post": Operation{
				Summary:     "Create a scheduled task (cron agent run)",
				Tags:        []string{"scheduled-tasks"},
				Security:    bearer,
				RequestBody: jsonBody(ref("ScheduledTaskRequest")),
				Responses: map[string]Response{
					"201": {Description: "created", Content: map[string]Content{"application/json": {Schema: map[string]any{"type": "object", "properties": map[string]any{"task": ref("ScheduledTask")}}}}},
					"400": {Description: "invalid task (bad cron/timezone/webhook_url)"},
					"401": {Description: "invalid or missing bearer token"},
				},
			},
		},
		"/api/me/scheduled-tasks/{id}": {
			"get": Operation{
				Summary:    "Fetch one task",
				Tags:       []string{"scheduled-tasks"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  jsonResp("task", map[string]any{"type": "object", "properties": map[string]any{"task": ref("ScheduledTask")}}),
			},
			"put": Operation{
				Summary:     "Update a task",
				Tags:        []string{"scheduled-tasks"},
				Security:    bearer,
				Parameters:  []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				RequestBody: jsonBody(ref("ScheduledTaskRequest")),
				Responses:   jsonResp("updated", map[string]any{"type": "object", "properties": map[string]any{"task": ref("ScheduledTask")}}),
			},
			"delete": Operation{
				Summary:    "Delete a task",
				Tags:       []string{"scheduled-tasks"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses: map[string]Response{
					"204": {Description: "deleted"},
					"401": {Description: "invalid or missing bearer token"},
					"404": {Description: "task not found"},
				},
			},
		},
		"/api/me/scheduled-tasks/{id}/run": {
			"post": Operation{
				Summary:    "Fire one task immediately",
				Tags:       []string{"scheduled-tasks"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses: jsonResp("accepted; started + session_id", map[string]any{
					"type": "object",
					"properties": map[string]any{
						"started":    map[string]any{"type": "boolean"},
						"session_id": map[string]any{"type": "string"},
					},
				}),
			},
		},

		// ---- agent definitions & skills ----
		"/api/me/agentdefs": {
			"get": Operation{
				Summary:   "List the caller's agent definitions",
				Tags:      []string{"agent-defs"},
				Security:  bearer,
				Responses: jsonResp("definitions", map[string]any{"type": "array", "items": ref("AgentDef")}),
			},
			"post": Operation{
				Summary:     "Create an agent definition",
				Tags:        []string{"agent-defs"},
				Security:    bearer,
				RequestBody: jsonBody(ref("AgentDefRequest")),
				Responses: map[string]Response{
					"201": {Description: "created", Content: map[string]Content{"application/json": {Schema: ref("AgentDef")}}},
					"400": {Description: "invalid definition"},
					"401": {Description: "invalid or missing bearer token"},
				},
			},
		},
		"/api/me/skills": {
			"get": Operation{
				Summary:  "List the caller-visible skills (user/team/system scope)",
				Tags:     []string{"skills"},
				Security: bearer,
				Responses: jsonResp("skills", map[string]any{
					"type":       "object",
					"required":   []string{"skills"},
					"properties": map[string]any{"skills": map[string]any{"type": "array", "items": ref("Skill")}},
				}),
			},
		},
		"/api/me/usage": {
			"get": Operation{
				Summary:    "The caller's token usage",
				Tags:       []string{"me"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "from", In: "query", Schema: map[string]any{"type": "string", "description": "RFC3339 range start (empty = since the beginning)"}}, {Name: "to", In: "query", Schema: map[string]any{"type": "string", "description": "RFC3339 range end (empty = now)"}}},
				Responses: jsonResp("total + daily buckets", map[string]any{
					"type": "object",
					"properties": map[string]any{
						"total": map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "integer"}, "output": map[string]any{"type": "integer"}, "cache_read": map[string]any{"type": "integer"}, "cache_write": map[string]any{"type": "integer"}, "runs": map[string]any{"type": "integer"}}},
						"daily": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
					},
				}),
			},
		},

		// ---- self-service ----
		"/api/me": {
			"get": Operation{
				Summary:   "Current account profile",
				Tags:      []string{"me"},
				Security:  bearer,
				Responses: jsonResp("profile", ref("User")),
			},
			"patch": Operation{
				Summary:     "Update display name",
				Tags:        []string{"me"},
				Security:    bearer,
				RequestBody: jsonBody(map[string]any{"type": "object", "properties": map[string]any{"display_name": map[string]any{"type": "string"}}}),
				Responses:   jsonResp("updated", ref("User")),
			},
		},
		"/api/me/tokens": {
			"get": Operation{
				Summary:   "List the caller's active session tokens",
				Tags:      []string{"me"},
				Security:  bearer,
				Responses: jsonResp("tokens", map[string]any{"type": "array", "items": ref("Token")}),
			},
		},

		// ---- administration (platform admins; service keys live here) ----
		"/api/admin/users": {
			"get": Operation{
				Summary:   "List platform users",
				Tags:      []string{"admin"},
				Security:  bearer,
				Responses: jsonResp("users", map[string]any{"type": "array", "items": ref("User")}),
			},
		},
		"/api/admin/service-keys": {
			"get": Operation{
				Summary:  "List service keys (programmatic credentials)",
				Tags:     []string{"admin"},
				Security: bearer,
				Parameters: []Parameter{
					{Name: "user_id", In: "query", Description: "filter to one owner", Schema: map[string]any{"type": "string"}},
					{Name: "revoked", In: "query", Description: "1 to include revoked keys", Schema: map[string]any{"type": "string"}},
				},
				Responses: jsonResp("service_keys", map[string]any{"type": "object", "properties": map[string]any{"service_keys": map[string]any{"type": "array", "items": ref("ServiceKey")}}}),
			},
			"post": Operation{
				Summary:     "Issue a service key (raw token returned once)",
				Description: "Creates an sk_ credential bound to a user; ttl_days 0/omitted = never expires. The raw token appears in the response exactly once.",
				Tags:        []string{"admin"},
				Security:    bearer,
				RequestBody: jsonBody(ref("ServiceKeyRequest")),
				Responses: map[string]Response{
					"201": {Description: "created; token visible once", Content: map[string]Content{"application/json": {Schema: map[string]any{"type": "object", "properties": map[string]any{"service_key": ref("ServiceKey")}}}}},
					"400": {Description: "invalid request"},
					"401": {Description: "invalid or missing bearer token"},
					"403": {Description: "not a platform admin"},
				},
			},
		},
		"/api/admin/service-keys/{id}": {
			"delete": Operation{
				Summary:    "Revoke a service key",
				Tags:       []string{"admin"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses: map[string]Response{
					"204": {Description: "revoked"},
					"401": {Description: "invalid or missing bearer token"},
					"403": {Description: "not a platform admin"},
					"404": {Description: "key not found"},
				},
			},
		},
		"/api/admin/quotas": {
			"get": Operation{
				Summary:  "Read a monthly token budget",
				Tags:     []string{"admin"},
				Security: bearer,
				Parameters: []Parameter{
					{Name: "scope", In: "query", Required: true, Schema: map[string]any{"type": "string", "enum": []string{"user", "team"}}},
					{Name: "owner_id", In: "query", Required: true, Schema: map[string]any{"type": "string"}},
				},
				Responses: jsonResp("budget (null = no limit)", map[string]any{"type": "object", "properties": map[string]any{"budget": ref("QuotaBudget")}}),
			},
			"put": Operation{
				Summary:     "Set a monthly token budget",
				Tags:        []string{"admin"},
				Security:    bearer,
				RequestBody: jsonBody(ref("QuotaRequest")),
				Responses:   jsonResp("budget", map[string]any{"type": "object", "properties": map[string]any{"budget": ref("QuotaBudget")}}),
			},
		},
		"/api/admin/audit": {
			"get": Operation{
				Summary:  "Query the append-only audit trail",
				Tags:     []string{"admin"},
				Security: bearer,
				Parameters: []Parameter{
					{Name: "action", In: "query", Schema: map[string]any{"type": "string"}},
					{Name: "actor", In: "query", Schema: map[string]any{"type": "string"}},
					{Name: "from", In: "query", Schema: map[string]any{"type": "string"}},
					{Name: "to", In: "query", Schema: map[string]any{"type": "string"}},
					{Name: "limit", In: "query", Schema: map[string]any{"type": "integer"}},
				},
				Responses: jsonResp("audit entries", map[string]any{"type": "array", "items": ref("AuditEntry")}),
			},
		},

		// ---- inbound webhooks (enterprise integration) ----
		// The trigger endpoint is public; every management route requires auth.
		"/api/inbound/{id}": {
			"post": Operation{
				Summary:     "Trigger an agent run from an external system",
				Description: "Starts an asynchronous agent run owned by the webhook's user. The request must carry X-Nowhere-Timestamp (unix seconds, within 5 minutes), X-Nowhere-Nonce (client-generated, deduplicated — replaying the same timestamp+nonce+signature starts no second run), and X-Nowhere-Signature: sha256=<hex HMAC-SHA256 over \"<ts>.<nonce>.<body>\" with the webhook secret>. The run executes on the platform's shared registry and completion is delivered to the webhook's notify_url (or the global WEBHOOK_URL).",
				Tags:        []string{"inbound"},
				Parameters: []Parameter{
					{Name: "id", In: "path", Required: true, Description: "webhook id from /api/me/inbound", Schema: map[string]any{"type": "string"}},
					{Name: "X-Nowhere-Timestamp", In: "header", Required: true, Description: "unix seconds; accepted within a 5-minute window", Schema: map[string]any{"type": "integer"}},
					{Name: "X-Nowhere-Nonce", In: "header", Required: true, Description: "client-generated idempotency nonce (<= 128 chars), folded into the signature and deduplicated", Schema: map[string]any{"type": "string"}},
					{Name: "X-Nowhere-Signature", In: "header", Required: true, Description: "sha256=<hex HMAC-SHA256 over \"<timestamp>.<nonce>.<body>\" with the webhook secret>", Schema: map[string]any{"type": "string"}},
				},
				RequestBody: jsonBody(ref("InboundTriggerRequest")),
				Responses: map[string]Response{
					"202": {Description: "run started; {run_id, session_id, status}", Content: map[string]Content{"application/json": {Schema: map[string]any{"type": "object", "properties": map[string]any{"run_id": map[string]any{"type": "string"}, "session_id": map[string]any{"type": "string"}, "status": map[string]any{"type": "string"}}}}}},
					"400": {Description: "invalid payload"},
					"401": {Description: "invalid signature, expired timestamp, or disabled webhook"},
					"403": {Description: "target session belongs to another user"},
					"409": {Description: "replayed nonce, or target session has pending human interactions"},
					"413": {Description: "payload too large"},
					"429": {Description: "monthly token budget exceeded"},
				},
			},
		},
		"/api/me/inbound": {
			"get": Operation{
				Summary:   "List my inbound webhooks",
				Tags:      []string{"inbound"},
				Security:  bearer,
				Responses: jsonResp("inbound_webhooks", map[string]any{"type": "object", "properties": map[string]any{"inbound_webhooks": map[string]any{"type": "array", "items": ref("InboundWebhook")}}}),
			},
			"post": Operation{
				Summary:     "Create an inbound webhook (secret returned once)",
				Description: "Creates a trigger endpoint. agent_def and system_prompt are mutually exclusive. The plaintext secret appears in the response exactly once; it is stored AES-256-GCM encrypted at rest.",
				Tags:        []string{"inbound"},
				Security:    bearer,
				RequestBody: jsonBody(ref("InboundWebhookRequest")),
				Responses: map[string]Response{
					"201": {Description: "created; secret visible once", Content: map[string]Content{"application/json": {Schema: map[string]any{"type": "object", "properties": map[string]any{"inbound_webhook": ref("InboundWebhook"), "secret": map[string]any{"type": "string"}}}}}},
					"400": {Description: "invalid request"},
					"401": {Description: "invalid or missing bearer token"},
				},
			},
		},
		"/api/me/inbound/{id}": {
			"patch": Operation{
				Summary:     "Enable or disable an inbound webhook",
				Tags:        []string{"inbound"},
				Security:    bearer,
				Parameters:  []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"enabled"}, "properties": map[string]any{"enabled": map[string]any{"type": "boolean"}}}),
				Responses: map[string]Response{
					"204": {Description: "updated"},
					"401": {Description: "invalid or missing bearer token"},
					"404": {Description: "webhook not found"},
				},
			},
			"delete": Operation{
				Summary:    "Delete an inbound webhook",
				Tags:       []string{"inbound"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses: map[string]Response{
					"204": {Description: "deleted"},
					"401": {Description: "invalid or missing bearer token"},
					"404": {Description: "webhook not found"},
				},
			},
		},
		"/api/me/inbound/{id}/rotate": {
			"post": Operation{
				Summary:    "Rotate an inbound webhook secret (old secret dies immediately)",
				Tags:       []string{"inbound"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses: map[string]Response{
					"200": {Description: "new secret (visible once)", Content: map[string]Content{"application/json": {Schema: map[string]any{"type": "object", "properties": map[string]any{"secret": map[string]any{"type": "string"}}}}}},
					"401": {Description: "invalid or missing bearer token"},
					"404": {Description: "webhook not found"},
				},
			},
		},

		// ---- meta (open) ----
		"/healthz": {
			"get": Operation{
				Summary:   "Liveness: AND of dependency probes",
				Tags:      []string{"meta"},
				Responses: map[string]Response{"200": {Description: "healthy"}, "503": {Description: "a dependency is down"}},
			},
		},
		"/openapi.json": {
			"get": Operation{
				Summary:   "This OpenAPI document",
				Tags:      []string{"meta"},
				Responses: map[string]Response{"200": {Description: "OpenAPI 3.0 JSON"}},
			},
		},
	}
}

// schemas holds the shared component definitions.
func schemas() map[string]any {
	return map[string]any{
		"AuthResponse": map[string]any{
			"type":     "object",
			"required": []string{"token", "user"},
			"properties": map[string]any{
				"token": map[string]any{"type": "string"},
				"user":  ref("User"),
			},
		},
		"User": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":            map[string]any{"type": "string"},
				"email":         map[string]any{"type": "string"},
				"display_name":  map[string]any{"type": "string"},
				"platform_role": map[string]any{"type": "string", "enum": []string{"user", "admin"}},
			},
		},
		"ChatRequest": map[string]any{
			"type":     "object",
			"required": []string{"message"},
			"properties": map[string]any{
				"message":       map[string]any{"type": "string", "description": "the user's text turn"},
				"session_id":    map[string]any{"type": "string", "description": "continue an existing session (empty = new)"},
				"mode":          map[string]any{"type": "string", "description": "response_mode: chat | resume", "enum": []string{"chat", "resume"}},
				"system_prompt": map[string]any{"type": "string", "description": "override the system prompt for this run"},
				"model":         map[string]any{"type": "string", "description": "model override on the resolved provider"},
				"images": map[string]any{
					"type":        "array",
					"description": "images attached to the current user turn, pre-uploaded to the session workspace",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":      map[string]any{"type": "string", "description": "session-relative workspace path (the upload endpoint's response)"},
							"mediaType": map[string]any{"type": "string", "description": "image media type; defaults to image/webp when empty"},
						},
					},
				},
			},
		},
		"Message": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"role":    map[string]any{"type": "string", "enum": []string{"user", "assistant", "tool"}},
				"content": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			},
		},
		"SessionPage": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sessions":    map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"}, "updated_at": map[string]any{"type": "string"}}}},
				"next_cursor": map[string]any{"type": "string", "nullable": true},
			},
		},
		"ScheduledTask": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":                 map[string]any{"type": "string"},
				"prompt":             map[string]any{"type": "string"},
				"agent_def_name":     map[string]any{"type": "string"},
				"cron":               map[string]any{"type": "string"},
				"timezone":           map[string]any{"type": "string"},
				"tool_whitelist":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"target_session_id":  map[string]any{"type": "string"},
				"on_run_completed":   map[string]any{"type": "string", "enum": []string{"keep", "delete"}},
				"multitask_strategy": map[string]any{"type": "string", "enum": []string{"reject", "interrupt", "enqueue"}},
				"webhook_url":        map[string]any{"type": "string", "description": "POSTed a run-completion notification when this task's run ends"},
				"enabled":            map[string]any{"type": "boolean"},
				"next_run_at":        map[string]any{"type": "string"},
			},
		},
		"ScheduledTaskRequest": map[string]any{
			"type":     "object",
			"required": []string{"cron"},
			"properties": map[string]any{
				"prompt":             map[string]any{"type": "string"},
				"agent_def_name":     map[string]any{"type": "string"},
				"cron":               map[string]any{"type": "string"},
				"timezone":           map[string]any{"type": "string"},
				"tool_whitelist":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"on_run_completed":   map[string]any{"type": "string", "enum": []string{"keep", "delete"}},
				"multitask_strategy": map[string]any{"type": "string", "enum": []string{"reject", "interrupt", "enqueue"}},
				"webhook_url":        map[string]any{"type": "string"},
				"end_time":           map[string]any{"type": "string"},
			},
		},
		"AgentDef": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":            map[string]any{"type": "string"},
				"name":          map[string]any{"type": "string"},
				"description":   map[string]any{"type": "string"},
				"system_prompt": map[string]any{"type": "string"},
				"model":         map[string]any{"type": "string"},
				"tools":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"max_turns":     map[string]any{"type": "integer"},
			},
		},
		"AgentDefRequest": map[string]any{
			"type":     "object",
			"required": []string{"name", "system_prompt"},
			"properties": map[string]any{
				"name":          map[string]any{"type": "string"},
				"description":   map[string]any{"type": "string"},
				"system_prompt": map[string]any{"type": "string"},
				"model":         map[string]any{"type": "string"},
				"tools":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"max_turns":     map[string]any{"type": "integer"},
			},
		},
		"Skill": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":          map[string]any{"type": "string"},
				"name":        map[string]any{"type": "string"},
				"description": map[string]any{"type": "string"},
				"scope":       map[string]any{"type": "string", "enum": []string{"user", "team", "system"}},
			},
		},
		"Token": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":         map[string]any{"type": "string"},
				"expires_at": map[string]any{"type": "string"},
				"created_at": map[string]any{"type": "string"},
			},
		},
		"ServiceKey": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":           map[string]any{"type": "string"},
				"name":         map[string]any{"type": "string"},
				"user_id":      map[string]any{"type": "string"},
				"created_at":   map[string]any{"type": "string"},
				"expires_at":   map[string]any{"type": "string", "nullable": true},
				"last_used_at": map[string]any{"type": "string", "nullable": true},
				"revoked_at":   map[string]any{"type": "string", "nullable": true},
				"token":        map[string]any{"type": "string", "description": "present ONLY in the create response"},
			},
		},
		"ServiceKeyRequest": map[string]any{
			"type":     "object",
			"required": []string{"name", "user_id"},
			"properties": map[string]any{
				"name":     map[string]any{"type": "string"},
				"user_id":  map[string]any{"type": "string"},
				"ttl_days": map[string]any{"type": "integer", "description": "0/omitted = never expires"},
			},
		},
		"InboundTriggerRequest": map[string]any{
			"type":     "object",
			"required": []string{"prompt"},
			"properties": map[string]any{
				"prompt":   map[string]any{"type": "string", "description": "the user turn that starts the run"},
				"metadata": map[string]any{"type": "object", "description": "free-form provenance carried into the session (ticket id, source system, ...)"},
			},
		},
		"InboundWebhookRequest": map[string]any{
			"type":     "object",
			"required": []string{"name"},
			"properties": map[string]any{
				"name":              map[string]any{"type": "string"},
				"agent_def":         map[string]any{"type": "string", "description": "agent definition name; mutually exclusive with system_prompt"},
				"system_prompt":     map[string]any{"type": "string", "description": "fixed system prompt override; mutually exclusive with agent_def"},
				"target_session_id": map[string]any{"type": "string", "description": "reuse an existing session; empty = new tagged session per trigger"},
				"notify_url":        map[string]any{"type": "string", "description": "run-completion notification target; empty = global WEBHOOK_URL"},
			},
		},
		"InboundWebhook": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":                map[string]any{"type": "string"},
				"name":              map[string]any{"type": "string"},
				"agent_def":         map[string]any{"type": "string"},
				"system_prompt":     map[string]any{"type": "string"},
				"target_session_id": map[string]any{"type": "string"},
				"notify_url":        map[string]any{"type": "string"},
				"enabled":           map[string]any{"type": "boolean"},
				"last_used_at":      map[string]any{"type": "string", "nullable": true},
				"created_at":        map[string]any{"type": "string"},
			},
		},
		"QuotaBudget": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"scope":          map[string]any{"type": "string", "enum": []string{"user", "team"}},
				"owner_id":       map[string]any{"type": "string"},
				"monthly_tokens": map[string]any{"type": "integer"},
			},
		},
		"QuotaRequest": map[string]any{
			"type":     "object",
			"required": []string{"scope", "owner_id", "monthly_tokens"},
			"properties": map[string]any{
				"scope":          map[string]any{"type": "string", "enum": []string{"user", "team"}},
				"owner_id":       map[string]any{"type": "string"},
				"monthly_tokens": map[string]any{"type": "integer"},
			},
		},
		"AuditEntry": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":         map[string]any{"type": "string"},
				"actor":      map[string]any{"type": "string"},
				"action":     map[string]any{"type": "string"},
				"outcome":    map[string]any{"type": "string"},
				"target":     map[string]any{"type": "string"},
				"created_at": map[string]any{"type": "string"},
			},
		},
	}
}
