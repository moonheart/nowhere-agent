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
	// ok204 builds a 204 response plus any extra statuses (401/403/404…).
	ok204 := func(extra ...map[string]Response) map[string]Response {
		out := map[string]Response{"204": {Description: "ok"}}
		for _, e := range extra {
			for k, v := range e {
				out[k] = v
			}
		}
		return out
	}
	// pathParam builds one required path parameter.
	pathParam := func(name, desc string) []Parameter {
		return []Parameter{{Name: name, In: "path", Required: true, Description: desc, Schema: map[string]any{"type": "string"}}}
	}
	unauthorized := map[string]Response{"401": {Description: "invalid or missing bearer token"}}
	forbidden := map[string]Response{"403": {Description: "forbidden"}}
	notFound := map[string]Response{"404": {Description: "not found"}}

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
		"/api/auth/phone/reset-password": {
			"post": Operation{
				Summary:     "Reset a password with an SMS code for the account's bound phone",
				Description: "Verifies the code delivered to the phone number bound to the account, then sets the new password and signs every session out. The phone must already be bound to an account (self-service phone binding lives at /api/me/phone/bind).",
				Tags:        []string{"auth"},
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"phone", "code", "password"}, "properties": map[string]any{"phone": map[string]any{"type": "string"}, "code": map[string]any{"type": "string"}, "password": map[string]any{"type": "string"}}}),
				Responses: map[string]Response{
					"204": {Description: "password reset"},
					"400": {Description: "invalid phone number or weak password"},
					"401": {Description: "invalid verification code"},
					"404": {Description: "no account bound to this phone"},
					"429": {Description: "too many failed attempts; Retry-After set"},
				},
			},
		},
		"/api/auth/phone/enabled": {
			"get": Operation{
				Summary:   "Whether phone (SMS) login is enabled",
				Tags:      []string{"auth"},
				Responses: jsonResp("enabled flag", map[string]any{"type": "object", "properties": map[string]any{"enabled": map[string]any{"type": "boolean"}}}),
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
				Description: "Rebuilds the conversation from the durable message store. Without a limit the FULL conversation is returned (legacy contract); with limit, only the newest limit messages come back, keyset-paged backwards via before (the previous page's first message id) — hasMore reports whether older messages exist.",
				Tags:       []string{"chat"},
				Security:   bearer,
				Parameters: []Parameter{
					{Name: "threadId", In: "query", Required: true, Schema: map[string]any{"type": "string"}},
					{Name: "limit", In: "query", Schema: map[string]any{"type": "integer", "description": "max messages to return (newest; 1..500). Absent = full conversation."}},
					{Name: "before", In: "query", Schema: map[string]any{"type": "integer", "description": "keyset cursor: return only messages with id strictly below this (the previous page's first message id)"}},
				},
				Responses: jsonResp("messages", map[string]any{"type": "array", "items": ref("Message")}),
			},
		},
		"/api/chat/models": {
			"get": Operation{
				Summary:    "List the caller's chat model picker",
				Description: "Returns the default model the caller's chat runs resolve to (team assignment → platform default) plus every enabled model name on that provider. Empty list when no provider serves the caller; names only, never credentials.",
				Tags:       []string{"chat"},
				Security:   bearer,
				Responses: jsonResp("model list", map[string]any{
					"type": "object",
					"properties": map[string]any{
						"default": map[string]any{"type": "string"},
						"models":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
				}),
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
		"/api/chat/sessions/{id}": {
			"delete": Operation{
				Summary:    "Delete a session (and its run record)",
				Tags:       []string{"chat"},
				Security:   bearer,
				Parameters: pathParam("id", "session id"),
				Responses:  ok204(unauthorized, notFound),
			},
		},
		"/api/chat/sessions/{id}/images": {
			"post": Operation{
				Summary:     "Upload an image to the session workspace",
				Description: "Stores the raw image payload (PNG/JPEG/GIF/WebP, up to 10 MiB) WebP-normalized under the session dir and returns the session-relative path to include in the next message's images array. Enforced against the per-session image quota (413 when over).",
				Tags:        []string{"chat"},
				Security:    bearer,
				Parameters:  pathParam("id", "session id"),
				Responses: map[string]Response{
					"200": {Description: "stored", Content: map[string]Content{"application/json": {Schema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string", "description": "session-relative path"}}}}}},
					"401": {Description: "invalid or missing bearer token"},
					"404": {Description: "session not found"},
					"413": {Description: "payload too large or session image quota exceeded"},
					"415": {Description: "unsupported or malformed image"},
				},
			},
		},
		"/api/chat/sessions/{id}/files/{path}": {
			"get": Operation{
				Summary:     "Stream a session image to its owner",
				Description: "Resolves a session-relative image path (as stored in a message block) and streams the WebP bytes. Confined to the session dir; ownership is the session-ownership check.",
				Tags:        []string{"chat"},
				Security:    bearer,
				Parameters: []Parameter{
					{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}},
					{Name: "path", In: "path", Required: true, Description: "session-relative image path (wildcard)", Schema: map[string]any{"type": "string"}},
				},
				Responses: map[string]Response{
					"200": {Description: "image/webp bytes"},
					"401": {Description: "invalid or missing bearer token"},
					"404": {Description: "session not found or image missing"},
				},
			},
		},
		"/api/chat/uploads": {
			"post": Operation{
				Summary:     "Upload a session-independent user-level image",
				Description: "Stores the image WebP-normalized under the caller's upload scope and returns the \"uploads/<id>.webp\" reference for a message image part — including a brand-new conversation's first message, which has no session yet. Per-user quota enforced (413 when over).",
				Tags:        []string{"chat"},
				Security:    bearer,
				Responses: map[string]Response{
					"200": {Description: "stored", Content: map[string]Content{"application/json": {Schema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string", "description": "uploads/<id>.webp"}}}}}},
					"401": {Description: "invalid or missing bearer token"},
					"413": {Description: "payload too large or per-user quota exceeded"},
					"415": {Description: "unsupported or malformed image"},
				},
			},
		},
		"/api/chat/uploads/{id}": {
			"get": Operation{
				Summary:     "Stream a user-level upload blob to its owner",
				Description: "Resolves an \"uploads/<id>.webp\" message reference under the caller's own upload scope (a foreign id 404s) and streams the WebP bytes.",
				Tags:        []string{"chat"},
				Security:    bearer,
				Parameters:  pathParam("id", "upload id (with .webp suffix)"),
				Responses: map[string]Response{
					"200": {Description: "image/webp bytes"},
					"401": {Description: "invalid or missing bearer token"},
					"404": {Description: "not found"},
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
				Parameters: pathParam("id", "task id"),
				Responses: jsonResp("accepted; started + session_id", map[string]any{
					"type": "object",
					"properties": map[string]any{
						"started":    map[string]any{"type": "boolean"},
						"session_id": map[string]any{"type": "string"},
					},
				}),
			},
		},
		"/api/me/scheduled-tasks/{id}/enable": {
			"post": Operation{
				Summary:    "Enable a scheduled task",
				Tags:       []string{"scheduled-tasks"},
				Security:   bearer,
				Parameters: pathParam("id", "task id"),
				Responses:  ok204(unauthorized, notFound),
			},
		},
		"/api/me/scheduled-tasks/{id}/disable": {
			"post": Operation{
				Summary:    "Disable a scheduled task",
				Tags:       []string{"scheduled-tasks"},
				Security:   bearer,
				Parameters: pathParam("id", "task id"),
				Responses:  ok204(unauthorized, notFound),
			},
		},
		"/api/me/scheduled-tasks/{id}/sessions": {
			"get": Operation{
				Summary:    "List the sessions a scheduled task has started",
				Tags:       []string{"scheduled-tasks"},
				Security:   bearer,
				Parameters: pathParam("id", "task id"),
				Responses: jsonResp("sessions", map[string]any{
					"type":       "object",
					"properties": map[string]any{"sessions": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}},
				}),
			},
		},
		"/api/me/scheduled-tasks/{id}/sessions/clear": {
			"post": Operation{
				Summary:    "Clear the sessions a scheduled task has started",
				Tags:       []string{"scheduled-tasks"},
				Security:   bearer,
				Parameters: pathParam("id", "task id"),
				Responses:  ok204(unauthorized, notFound),
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
		"/api/me/agentdefs/{name}": {
			"get": Operation{
				Summary:    "Fetch one of the caller's agent definitions",
				Tags:       []string{"agent-defs"},
				Security:   bearer,
				Parameters: pathParam("name", "definition name"),
				Responses:  jsonResp("definition", ref("AgentDef")),
			},
			"put": Operation{
				Summary:     "Update one of the caller's agent definitions",
				Tags:        []string{"agent-defs"},
				Security:    bearer,
				Parameters:  pathParam("name", "definition name"),
				RequestBody: jsonBody(ref("AgentDefRequest")),
				Responses:   jsonResp("updated", ref("AgentDef")),
			},
			"delete": Operation{
				Summary:    "Delete one of the caller's agent definitions",
				Tags:       []string{"agent-defs"},
				Security:   bearer,
				Parameters: pathParam("name", "definition name"),
				Responses:  ok204(unauthorized, notFound),
			},
		},
		"/api/teams/{id}/agentdefs": {
			"get": Operation{
				Summary:    "List a team's agent definitions",
				Tags:       []string{"agent-defs"},
				Security:   bearer,
				Parameters: pathParam("id", "team id"),
				Responses:  jsonResp("definitions", map[string]any{"type": "array", "items": ref("AgentDef")}),
			},
			"post": Operation{
				Summary:     "Create a team agent definition (team admin)",
				Tags:        []string{"agent-defs"},
				Security:    bearer,
				Parameters:  pathParam("id", "team id"),
				RequestBody: jsonBody(ref("AgentDefRequest")),
				Responses: map[string]Response{
					"201": {Description: "created", Content: map[string]Content{"application/json": {Schema: ref("AgentDef")}}},
					"401": {Description: "invalid or missing bearer token"},
					"403": {Description: "not a team admin"},
				},
			},
		},
		"/api/teams/{id}/agentdefs/{name}": {
			"get": Operation{
				Summary:    "Fetch a team's agent definition",
				Tags:       []string{"agent-defs"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "name", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  jsonResp("definition", ref("AgentDef")),
			},
			"put": Operation{
				Summary:     "Update a team agent definition (team admin)",
				Tags:        []string{"agent-defs"},
				Security:    bearer,
				Parameters:  []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "name", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				RequestBody: jsonBody(ref("AgentDefRequest")),
				Responses:   jsonResp("updated", ref("AgentDef")),
			},
			"delete": Operation{
				Summary:    "Delete a team agent definition (team admin)",
				Tags:       []string{"agent-defs"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "name", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/admin/agentdefs": {
			"get": Operation{
				Summary:   "List system agent definitions",
				Tags:      []string{"agent-defs"},
				Security:  bearer,
				Responses: jsonResp("definitions", map[string]any{"type": "array", "items": ref("AgentDef")}),
			},
			"post": Operation{
				Summary:     "Create a system agent definition (platform admin)",
				Tags:        []string{"agent-defs"},
				Security:    bearer,
				RequestBody: jsonBody(ref("AgentDefRequest")),
				Responses: map[string]Response{
					"201": {Description: "created", Content: map[string]Content{"application/json": {Schema: ref("AgentDef")}}},
					"400": {Description: "invalid definition"},
					"401": {Description: "invalid or missing bearer token"},
					"403": {Description: "not a platform admin"},
				},
			},
		},
		"/api/admin/agentdefs/{name}": {
			"get": Operation{
				Summary:    "Fetch a system agent definition",
				Tags:       []string{"agent-defs"},
				Security:   bearer,
				Parameters: pathParam("name", "definition name"),
				Responses:  jsonResp("definition", ref("AgentDef")),
			},
			"put": Operation{
				Summary:     "Update a system agent definition (platform admin)",
				Tags:        []string{"agent-defs"},
				Security:    bearer,
				Parameters:  pathParam("name", "definition name"),
				RequestBody: jsonBody(ref("AgentDefRequest")),
				Responses:   jsonResp("updated", ref("AgentDef")),
			},
			"delete": Operation{
				Summary:    "Delete a system agent definition (platform admin)",
				Tags:       []string{"agent-defs"},
				Security:   bearer,
				Parameters: pathParam("name", "definition name"),
				Responses:  ok204(unauthorized, forbidden, notFound),
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
			"post": Operation{
				Summary:     "Create a user-scope skill",
				Tags:        []string{"skills"},
				Security:    bearer,
				RequestBody: jsonBody(ref("SkillRequest")),
				Responses: map[string]Response{
					"201": {Description: "created", Content: map[string]Content{"application/json": {Schema: ref("Skill")}}},
					"400": {Description: "invalid skill"},
					"401": {Description: "invalid or missing bearer token"},
				},
			},
		},
		"/api/me/skills/{id}": {
			"get": Operation{
				Summary:    "Fetch one skill with its content",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: pathParam("id", "skill id"),
				Responses:  jsonResp("skill", ref("Skill")),
			},
			"put": Operation{
				Summary:     "Update a user-scope skill",
				Tags:        []string{"skills"},
				Security:    bearer,
				Parameters:  pathParam("id", "skill id"),
				RequestBody: jsonBody(ref("SkillRequest")),
				Responses:   jsonResp("updated", ref("Skill")),
			},
			"delete": Operation{
				Summary:    "Delete a user-scope skill",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: pathParam("id", "skill id"),
				Responses:  ok204(unauthorized, notFound),
			},
		},
		"/api/me/skills/{id}/versions": {
			"get": Operation{
				Summary:    "List a skill's versions",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: pathParam("id", "skill id"),
				Responses: jsonResp("versions", map[string]any{
					"type":       "object",
					"properties": map[string]any{"versions": map[string]any{"type": "array", "items": ref("SkillVersion")}},
				}),
			},
		},
		"/api/me/skills/{id}/versions/{v}": {
			"get": Operation{
				Summary:    "Fetch one skill version's content",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "v", In: "path", Required: true, Description: "version number", Schema: map[string]any{"type": "integer"}}},
				Responses:  jsonResp("version", ref("SkillVersion")),
			},
		},
		"/api/me/skills/{id}/rollback/{v}": {
			"post": Operation{
				Summary:    "Roll a skill back to an earlier version",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "v", In: "path", Required: true, Schema: map[string]any{"type": "integer"}}},
				Responses:  jsonResp("rolled back", ref("Skill")),
			},
		},
		"/api/me/skills/{id}/enable": {
			"post": Operation{
				Summary:    "Enable a user-scope skill",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: pathParam("id", "skill id"),
				Responses:  ok204(unauthorized, notFound),
			},
		},
		"/api/me/skills/{id}/disable": {
			"post": Operation{
				Summary:    "Disable a user-scope skill",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: pathParam("id", "skill id"),
				Responses:  ok204(unauthorized, notFound),
			},
		},
		"/api/me/skills/{id}/move": {
			"post": Operation{
				Summary:     "Move a skill to another scope (user/team/system)",
				Tags:        []string{"skills"},
				Security:    bearer,
				Parameters:  pathParam("id", "skill id"),
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"scope"}, "properties": map[string]any{"scope": map[string]any{"type": "string", "enum": []string{"user", "team", "system"}}}}),
				Responses:   jsonResp("moved", ref("Skill")),
			},
		},
		"/api/teams/{id}/skills": {
			"get": Operation{
				Summary:    "List a team's skills",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: pathParam("id", "team id"),
				Responses: jsonResp("skills", map[string]any{
					"type":       "object",
					"properties": map[string]any{"skills": map[string]any{"type": "array", "items": ref("Skill")}},
				}),
			},
			"post": Operation{
				Summary:     "Create a team-scope skill (team admin)",
				Tags:        []string{"skills"},
				Security:    bearer,
				Parameters:  pathParam("id", "team id"),
				RequestBody: jsonBody(ref("SkillRequest")),
				Responses: map[string]Response{
					"201": {Description: "created", Content: map[string]Content{"application/json": {Schema: ref("Skill")}}},
					"401": {Description: "invalid or missing bearer token"},
					"403": {Description: "not a team admin"},
				},
			},
		},
		"/api/teams/{id}/skills/{sid}": {
			"get": Operation{
				Summary:    "Fetch a team skill",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "sid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  jsonResp("skill", ref("Skill")),
			},
			"put": Operation{
				Summary:     "Update a team skill (team admin)",
				Tags:        []string{"skills"},
				Security:    bearer,
				Parameters:  []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "sid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				RequestBody: jsonBody(ref("SkillRequest")),
				Responses:   jsonResp("updated", ref("Skill")),
			},
			"delete": Operation{
				Summary:    "Delete a team skill (team admin)",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "sid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/teams/{id}/skills/{sid}/versions": {
			"get": Operation{
				Summary:    "List a team skill's versions",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "sid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses: jsonResp("versions", map[string]any{
					"type":       "object",
					"properties": map[string]any{"versions": map[string]any{"type": "array", "items": ref("SkillVersion")}},
				}),
			},
		},
		"/api/teams/{id}/skills/{sid}/versions/{v}": {
			"get": Operation{
				Summary:    "Fetch one team skill version",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "sid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "v", In: "path", Required: true, Schema: map[string]any{"type": "integer"}}},
				Responses:  jsonResp("version", ref("SkillVersion")),
			},
		},
		"/api/teams/{id}/skills/{sid}/rollback/{v}": {
			"post": Operation{
				Summary:    "Roll a team skill back (team admin)",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "sid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "v", In: "path", Required: true, Schema: map[string]any{"type": "integer"}}},
				Responses:  jsonResp("rolled back", ref("Skill")),
			},
		},
		"/api/teams/{id}/skills/{sid}/enable": {
			"post": Operation{
				Summary:    "Enable a team skill (team admin)",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "sid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/teams/{id}/skills/{sid}/disable": {
			"post": Operation{
				Summary:    "Disable a team skill (team admin)",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "sid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/admin/skills": {
			"get": Operation{
				Summary:  "List system skills (platform admin)",
				Tags:     []string{"skills"},
				Security: bearer,
				Responses: jsonResp("skills", map[string]any{
					"type":       "object",
					"properties": map[string]any{"skills": map[string]any{"type": "array", "items": ref("Skill")}},
				}),
			},
			"post": Operation{
				Summary:     "Create a system-scope skill (platform admin)",
				Tags:        []string{"skills"},
				Security:    bearer,
				RequestBody: jsonBody(ref("SkillRequest")),
				Responses: map[string]Response{
					"201": {Description: "created", Content: map[string]Content{"application/json": {Schema: ref("Skill")}}},
					"401": {Description: "invalid or missing bearer token"},
					"403": {Description: "not a platform admin"},
				},
			},
		},
		"/api/admin/skills/{id}": {
			"get": Operation{
				Summary:    "Fetch a system skill",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: pathParam("id", "skill id"),
				Responses:  jsonResp("skill", ref("Skill")),
			},
			"put": Operation{
				Summary:     "Update a system skill (platform admin)",
				Tags:        []string{"skills"},
				Security:    bearer,
				Parameters:  pathParam("id", "skill id"),
				RequestBody: jsonBody(ref("SkillRequest")),
				Responses:   jsonResp("updated", ref("Skill")),
			},
			"delete": Operation{
				Summary:    "Delete a system skill (platform admin)",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: pathParam("id", "skill id"),
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/admin/skills/{id}/versions": {
			"get": Operation{
				Summary:    "List a system skill's versions",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: pathParam("id", "skill id"),
				Responses: jsonResp("versions", map[string]any{
					"type":       "object",
					"properties": map[string]any{"versions": map[string]any{"type": "array", "items": ref("SkillVersion")}},
				}),
			},
		},
		"/api/admin/skills/{id}/versions/{v}": {
			"get": Operation{
				Summary:    "Fetch one system skill version",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "v", In: "path", Required: true, Schema: map[string]any{"type": "integer"}}},
				Responses:  jsonResp("version", ref("SkillVersion")),
			},
		},
		"/api/admin/skills/{id}/rollback/{v}": {
			"post": Operation{
				Summary:    "Roll a system skill back (platform admin)",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "v", In: "path", Required: true, Schema: map[string]any{"type": "integer"}}},
				Responses:  jsonResp("rolled back", ref("Skill")),
			},
		},
		"/api/admin/skills/{id}/enable": {
			"post": Operation{
				Summary:    "Enable a system skill (platform admin)",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: pathParam("id", "skill id"),
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/admin/skills/{id}/disable": {
			"post": Operation{
				Summary:    "Disable a system skill (platform admin)",
				Tags:       []string{"skills"},
				Security:   bearer,
				Parameters: pathParam("id", "skill id"),
				Responses:  ok204(unauthorized, forbidden, notFound),
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
			"delete": Operation{
				Summary:   "Permanently delete the caller's account and data",
				Tags:      []string{"me"},
				Security:  bearer,
				Responses: ok204(unauthorized),
			},
		},
		"/api/me/password": {
			"post": Operation{
				Summary:     "Change the caller's password",
				Tags:        []string{"me"},
				Security:    bearer,
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"current_password", "new_password"}, "properties": map[string]any{"current_password": map[string]any{"type": "string"}, "new_password": map[string]any{"type": "string"}}}),
				Responses: map[string]Response{
					"200": {Description: "password changed", Content: map[string]Content{"application/json": {Schema: map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}}}},
					"401": {Description: "invalid or missing bearer token"},
					"403": {Description: "current password wrong or policy violation"},
				},
			},
		},
		"/api/me/phone/bind": {
			"post": Operation{
				Summary:     "Bind a mobile number to the caller's account (SMS-OTP verified)",
				Description: "Verifies the code delivered to the phone (issued via /api/auth/phone/request-code) and binds it to the caller's account, enabling phone-based password recovery. A phone already bound to another account is rejected.",
				Tags:        []string{"me"},
				Security:    bearer,
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"phone", "code"}, "properties": map[string]any{"phone": map[string]any{"type": "string"}, "code": map[string]any{"type": "string"}}}),
				Responses: map[string]Response{
					"200": {Description: "phone bound"},
					"400": {Description: "invalid phone number"},
					"401": {Description: "invalid verification code"},
					"409": {Description: "phone is bound to another account"},
					"429": {Description: "too many failed attempts; Retry-After set"},
				},
			},
		},
		"/api/me/totp/enable": {
			"post": Operation{
				Summary:   "Start TOTP enrollment (returns a QR-secret challenge)",
				Tags:      []string{"me"},
				Security:  bearer,
				Responses: jsonResp("enrollment challenge", map[string]any{"type": "object", "properties": map[string]any{"secret": map[string]any{"type": "string"}, "otpauth_url": map[string]any{"type": "string"}}}),
			},
		},
		"/api/me/totp/confirm": {
			"post": Operation{
				Summary:     "Confirm TOTP enrollment with a code",
				Tags:        []string{"me"},
				Security:    bearer,
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"code"}, "properties": map[string]any{"code": map[string]any{"type": "string"}}}),
				Responses:   jsonResp("enabled", map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}),
			},
		},
		"/api/me/totp/disable": {
			"post": Operation{
				Summary:     "Disable TOTP (password required)",
				Tags:        []string{"me"},
				Security:    bearer,
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"password"}, "properties": map[string]any{"password": map[string]any{"type": "string"}}}),
				Responses:   ok204(unauthorized),
			},
		},
		"/api/me/tokens": {
			"get": Operation{
				Summary:   "List the caller's active session tokens",
				Tags:      []string{"me"},
				Security:  bearer,
				Responses: jsonResp("tokens", map[string]any{"type": "array", "items": ref("Token")}),
			},
			"delete": Operation{
				Summary:   "Revoke every session token except the current one",
				Tags:      []string{"me"},
				Security:  bearer,
				Responses: ok204(unauthorized),
			},
		},
		"/api/me/tokens/{id}": {
			"delete": Operation{
				Summary:    "Revoke one session token",
				Tags:       []string{"me"},
				Security:   bearer,
				Parameters: pathParam("id", "token id"),
				Responses:  ok204(unauthorized, notFound),
			},
		},
		"/api/me/memories": {
			"get": Operation{
				Summary:   "List the caller's long-term memories",
				Tags:      []string{"me"},
				Security:  bearer,
				Responses: jsonResp("memories", map[string]any{"type": "object", "properties": map[string]any{"memories": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}}}),
			},
		},
		"/api/me/memories/{id}": {
			"delete": Operation{
				Summary:    "Delete one of the caller's memories",
				Tags:       []string{"me"},
				Security:   bearer,
				Parameters: pathParam("id", "memory id"),
				Responses:  ok204(unauthorized, notFound),
			},
		},
		"/api/me/dream": {
			"get": Operation{
				Summary:   "Dreaming status (enabled, last pass, pending)",
				Tags:      []string{"me"},
				Security:  bearer,
				Responses: jsonResp("status", map[string]any{"type": "object", "properties": map[string]any{"enabled": map[string]any{"type": "boolean"}, "status": map[string]any{"type": "string"}, "last_run_at": map[string]any{"type": "string", "nullable": true}}}),
			},
			"post": Operation{
				Summary:   "Trigger a dreaming pass now",
				Tags:      []string{"me"},
				Security:  bearer,
				Responses: map[string]Response{"202": {Description: "accepted"}, "401": {Description: "invalid or missing bearer token"}},
			},
		},
		"/api/me/uploads": {
			"get": Operation{
				Summary:   "List the caller's user-level image uploads",
				Tags:      []string{"me"},
				Security:  bearer,
				Responses: jsonResp("uploads", map[string]any{"type": "object", "properties": map[string]any{"uploads": map[string]any{"type": "array", "items": ref("Upload")}}}),
			},
		},
		"/api/me/uploads/{id}": {
			"delete": Operation{
				Summary:    "Delete one of the caller's uploads (rejected while referenced by a message)",
				Tags:       []string{"me"},
				Security:   bearer,
				Parameters: pathParam("id", "upload id"),
				Responses:  ok204(unauthorized, notFound),
			},
		},
		"/api/me/export": {
			"get": Operation{
				Summary:   "Export the caller's data (JSON)",
				Tags:      []string{"me"},
				Security:  bearer,
				Responses: map[string]Response{"200": {Description: "JSON export"}, "401": {Description: "invalid or missing bearer token"}},
			},
		},

		// ---- teams (self-service + team-scoped resources) ----
		"/api/teams": {
			"get": Operation{
				Summary:   "List the caller's teams",
				Tags:      []string{"teams"},
				Security:  bearer,
				Responses: jsonResp("teams", map[string]any{"type": "object", "properties": map[string]any{"teams": map[string]any{"type": "array", "items": ref("Team")}}}),
			},
			"post": Operation{
				Summary:     "Create a team",
				Tags:        []string{"teams"},
				Security:    bearer,
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"name"}, "properties": map[string]any{"name": map[string]any{"type": "string"}}}),
				Responses: map[string]Response{
					"201": {Description: "created", Content: map[string]Content{"application/json": {Schema: ref("Team")}}},
					"401": {Description: "invalid or missing bearer token"},
				},
			},
		},
		"/api/teams/{id}": {
			"get": Operation{
				Summary:    "Fetch a team",
				Tags:       []string{"teams"},
				Security:   bearer,
				Parameters: pathParam("id", "team id"),
				Responses:  jsonResp("team", ref("Team")),
			},
			"patch": Operation{
				Summary:     "Rename a team (team admin)",
				Tags:        []string{"teams"},
				Security:    bearer,
				Parameters:  pathParam("id", "team id"),
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"name"}, "properties": map[string]any{"name": map[string]any{"type": "string"}}}),
				Responses:   jsonResp("renamed", ref("Team")),
			},
			"delete": Operation{
				Summary:    "Delete a team (team owner)",
				Tags:       []string{"teams"},
				Security:   bearer,
				Parameters: pathParam("id", "team id"),
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/teams/{id}/members": {
			"get": Operation{
				Summary:    "List a team's members",
				Tags:       []string{"teams"},
				Security:   bearer,
				Parameters: pathParam("id", "team id"),
				Responses:  jsonResp("members", map[string]any{"type": "object", "properties": map[string]any{"members": map[string]any{"type": "array", "items": ref("TeamMember")}}}),
			},
			"post": Operation{
				Summary:     "Add a member (team admin)",
				Tags:        []string{"teams"},
				Security:    bearer,
				Parameters:  pathParam("id", "team id"),
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"user_id", "role"}, "properties": map[string]any{"user_id": map[string]any{"type": "string"}, "role": map[string]any{"type": "string", "enum": []string{"owner", "admin", "member"}}}}),
				Responses: map[string]Response{
					"201": {Description: "added", Content: map[string]Content{"application/json": {Schema: ref("TeamMember")}}},
					"401": {Description: "invalid or missing bearer token"},
					"403": {Description: "not a team admin"},
				},
			},
		},
		"/api/teams/{id}/members/{userId}": {
			"patch": Operation{
				Summary:     "Change a member's role (team owner)",
				Tags:        []string{"teams"},
				Security:    bearer,
				Parameters:  []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "userId", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"role"}, "properties": map[string]any{"role": map[string]any{"type": "string", "enum": []string{"owner", "admin", "member"}}}}),
				Responses:   jsonResp("updated", ref("TeamMember")),
			},
			"delete": Operation{
				Summary:    "Remove a member (team admin/owner)",
				Tags:       []string{"teams"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "userId", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/teams/{id}/usage": {
			"get": Operation{
				Summary:    "A team's token usage (team admin)",
				Tags:       []string{"teams"},
				Security:   bearer,
				Parameters: pathParam("id", "team id"),
				Responses: jsonResp("usage", map[string]any{
					"type":       "object",
					"properties": map[string]any{"total": map[string]any{"type": "object"}, "daily": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}},
				}),
			},
		},
		"/api/teams/{id}/memories": {
			"get": Operation{
				Summary:    "List a team's shared memories",
				Tags:       []string{"teams"},
				Security:   bearer,
				Parameters: pathParam("id", "team id"),
				Responses:  jsonResp("memories", map[string]any{"type": "object", "properties": map[string]any{"memories": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}}}),
			},
		},
		"/api/teams/{id}/memories/{mid}": {
			"delete": Operation{
				Summary:    "Delete a team memory (team admin)",
				Tags:       []string{"teams"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "mid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/teams/{id}/memories/{mid}/deprecate": {
			"post": Operation{
				Summary:    "Deprecate a team memory (team admin)",
				Tags:       []string{"teams"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "mid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/teams/{id}/providers": {
			"get": Operation{
				Summary:    "List a team's LLM providers",
				Tags:       []string{"teams"},
				Security:   bearer,
				Parameters: pathParam("id", "team id"),
				Responses:  jsonResp("providers", map[string]any{"type": "object", "properties": map[string]any{"providers": map[string]any{"type": "array", "items": ref("Provider")}}}),
			},
			"post": Operation{
				Summary:     "Create a team provider (team admin)",
				Tags:        []string{"teams"},
				Security:    bearer,
				Parameters:  pathParam("id", "team id"),
				RequestBody: jsonBody(ref("ProviderRequest")),
				Responses: map[string]Response{
					"201": {Description: "created", Content: map[string]Content{"application/json": {Schema: ref("Provider")}}},
					"401": {Description: "invalid or missing bearer token"},
					"403": {Description: "not a team admin"},
				},
			},
		},
		"/api/teams/{id}/providers/{pid}": {
			"patch": Operation{
				Summary:     "Update a team provider (team admin)",
				Tags:        []string{"teams"},
				Security:    bearer,
				Parameters:  []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "pid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				RequestBody: jsonBody(ref("ProviderRequest")),
				Responses:   jsonResp("updated", ref("Provider")),
			},
			"delete": Operation{
				Summary:    "Delete a team provider (team admin)",
				Tags:       []string{"teams"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "pid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/teams/{id}/providers/{pid}/models": {
			"get": Operation{
				Summary:    "List a provider's models",
				Tags:       []string{"teams"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "pid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  jsonResp("models", map[string]any{"type": "object", "properties": map[string]any{"models": map[string]any{"type": "array", "items": ref("ProviderModel")}}}),
			},
			"post": Operation{
				Summary:     "Create a provider model (team admin)",
				Tags:        []string{"teams"},
				Security:    bearer,
				Parameters:  []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "pid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				RequestBody: jsonBody(ref("ProviderModelRequest")),
				Responses: map[string]Response{
					"201": {Description: "created", Content: map[string]Content{"application/json": {Schema: ref("ProviderModel")}}},
					"401": {Description: "invalid or missing bearer token"},
					"403": {Description: "not a team admin"},
				},
			},
		},
		"/api/teams/{id}/providers/{pid}/models/fetch": {
			"post": Operation{
				Summary:    "Discover a provider's models remotely (team admin)",
				Tags:       []string{"teams"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "pid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  jsonResp("fetched models", map[string]any{"type": "object", "properties": map[string]any{"models": map[string]any{"type": "array", "items": ref("ProviderModel")}}}),
			},
		},
		"/api/teams/{id}/providers/{pid}/models/{mid}": {
			"patch": Operation{
				Summary:     "Update a provider model (team admin)",
				Tags:        []string{"teams"},
				Security:    bearer,
				Parameters:  []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "pid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "mid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				RequestBody: jsonBody(ref("ProviderModelRequest")),
				Responses:   jsonResp("updated", ref("ProviderModel")),
			},
			"delete": Operation{
				Summary:    "Delete a provider model (team admin)",
				Tags:       []string{"teams"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "pid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "mid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/teams/{id}/providers/{pid}/models/{mid}/default": {
			"post": Operation{
				Summary:    "Set a provider model as default (team admin)",
				Tags:       []string{"teams"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "pid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "mid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/teams/{id}/provider-assignment": {
			"put": Operation{
				Summary:     "Set the team's provider assignment (team admin)",
				Tags:        []string{"teams"},
				Security:    bearer,
				Parameters:  pathParam("id", "team id"),
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"provider_id"}, "properties": map[string]any{"provider_id": map[string]any{"type": "string"}}}),
				Responses:   ok204(unauthorized, forbidden),
			},
			"delete": Operation{
				Summary:    "Clear the team's provider assignment (team admin)",
				Tags:       []string{"teams"},
				Security:   bearer,
				Parameters: pathParam("id", "team id"),
				Responses:  ok204(unauthorized, forbidden),
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
			"post": Operation{
				Summary:     "Create a user (platform admin)",
				Tags:        []string{"admin"},
				Security:    bearer,
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"email", "password"}, "properties": map[string]any{"email": map[string]any{"type": "string", "format": "email"}, "password": map[string]any{"type": "string"}, "display_name": map[string]any{"type": "string"}, "platform_role": map[string]any{"type": "string", "enum": []string{"user", "admin"}}}}),
				Responses: map[string]Response{
					"201": {Description: "created", Content: map[string]Content{"application/json": {Schema: ref("User")}}},
					"401": {Description: "invalid or missing bearer token"},
					"403": {Description: "not a platform admin"},
				},
			},
		},
		"/api/admin/users/{id}": {
			"patch": Operation{
				Summary:     "Update a user (platform admin)",
				Tags:        []string{"admin"},
				Security:    bearer,
				Parameters:  pathParam("id", "user id"),
				RequestBody: jsonBody(map[string]any{"type": "object", "properties": map[string]any{"display_name": map[string]any{"type": "string"}, "platform_role": map[string]any{"type": "string", "enum": []string{"user", "admin"}}}}),
				Responses:   jsonResp("updated", ref("User")),
			},
			"delete": Operation{
				Summary:    "Permanently delete a user and their data (platform admin)",
				Tags:       []string{"admin"},
				Security:   bearer,
				Parameters: pathParam("id", "user id"),
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/admin/users/{id}/password": {
			"post": Operation{
				Summary:     "Reset a user's password (platform admin)",
				Tags:        []string{"admin"},
				Security:    bearer,
				Parameters:  pathParam("id", "user id"),
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"new_password"}, "properties": map[string]any{"new_password": map[string]any{"type": "string"}}}),
				Responses:   ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/admin/sessions/{id}": {
			"delete": Operation{
				Summary:    "Delete a session and its data (platform admin)",
				Tags:       []string{"admin"},
				Security:   bearer,
				Parameters: pathParam("id", "session id"),
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/admin/teams": {
			"get": Operation{
				Summary:   "List all teams (platform admin)",
				Tags:      []string{"admin"},
				Security:  bearer,
				Responses: jsonResp("teams", map[string]any{"type": "object", "properties": map[string]any{"teams": map[string]any{"type": "array", "items": ref("Team")}}}),
			},
			"post": Operation{
				Summary:     "Create a team owned by a user (platform admin)",
				Tags:        []string{"admin"},
				Security:    bearer,
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"name", "owner_id"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "owner_id": map[string]any{"type": "string"}}}),
				Responses: map[string]Response{
					"201": {Description: "created", Content: map[string]Content{"application/json": {Schema: ref("Team")}}},
					"401": {Description: "invalid or missing bearer token"},
					"403": {Description: "not a platform admin"},
				},
			},
		},
		"/api/admin/usage": {
			"get": Operation{
				Summary:   "Platform-wide token usage (platform admin)",
				Tags:      []string{"admin"},
				Security:  bearer,
				Responses: jsonResp("usage", map[string]any{"type": "object", "properties": map[string]any{"total": map[string]any{"type": "object"}, "daily": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}}}),
			},
		},
		"/api/admin/stats": {
			"get": Operation{
				Summary:   "Platform counters (users, sessions, …; platform admin)",
				Tags:      []string{"admin"},
				Security:  bearer,
				Responses: jsonResp("stats", map[string]any{"type": "object"}),
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
				Parameters: pathParam("id", "key id"),
				Responses: map[string]Response{
					"204": {Description: "revoked"},
					"401": {Description: "invalid or missing bearer token"},
					"403": {Description: "not a platform admin"},
					"404": {Description: "key not found"},
				},
			},
		},
		"/api/admin/webhook-deliveries": {
			"get": Operation{
				Summary:   "List webhook delivery attempts (platform admin)",
				Tags:      []string{"admin"},
				Security:  bearer,
				Responses: jsonResp("deliveries", map[string]any{"type": "object", "properties": map[string]any{"deliveries": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}}}),
			},
		},
		"/api/admin/webhook-deliveries/{id}/retry": {
			"post": Operation{
				Summary:    "Requeue a failed webhook delivery (platform admin)",
				Tags:       []string{"admin"},
				Security:   bearer,
				Parameters: pathParam("id", "delivery id"),
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/admin/settings": {
			"get": Operation{
				Summary:   "List runtime settings with their effective values (platform admin)",
				Tags:      []string{"admin"},
				Security:  bearer,
				Responses: jsonResp("settings", map[string]any{"type": "object", "properties": map[string]any{"settings": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}}}),
			},
		},
		"/api/admin/settings/{key}": {
			"put": Operation{
				Summary:     "Set one runtime setting (platform admin)",
				Tags:        []string{"admin"},
				Security:    bearer,
				Parameters:  pathParam("key", "setting key"),
				RequestBody: jsonBody(map[string]any{"type": "object", "required": []string{"value"}, "properties": map[string]any{"value": map[string]any{"type": "string"}}}),
				Responses:   ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/admin/providers": {
			"get": Operation{
				Summary:   "List platform (system) LLM providers",
				Tags:      []string{"admin"},
				Security:  bearer,
				Responses: jsonResp("providers", map[string]any{"type": "object", "properties": map[string]any{"providers": map[string]any{"type": "array", "items": ref("Provider")}}}),
			},
			"post": Operation{
				Summary:     "Create a platform provider (platform admin)",
				Tags:        []string{"admin"},
				Security:    bearer,
				RequestBody: jsonBody(ref("ProviderRequest")),
				Responses: map[string]Response{
					"201": {Description: "created", Content: map[string]Content{"application/json": {Schema: ref("Provider")}}},
					"401": {Description: "invalid or missing bearer token"},
					"403": {Description: "not a platform admin"},
				},
			},
		},
		"/api/admin/providers/{pid}": {
			"patch": Operation{
				Summary:     "Update a platform provider (platform admin)",
				Tags:        []string{"admin"},
				Security:    bearer,
				Parameters:  pathParam("pid", "provider id"),
				RequestBody: jsonBody(ref("ProviderRequest")),
				Responses:   jsonResp("updated", ref("Provider")),
			},
			"delete": Operation{
				Summary:    "Delete a platform provider (platform admin)",
				Tags:       []string{"admin"},
				Security:   bearer,
				Parameters: pathParam("pid", "provider id"),
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/admin/providers/{pid}/default": {
			"post": Operation{
				Summary:    "Set a platform provider as default (platform admin)",
				Tags:       []string{"admin"},
				Security:   bearer,
				Parameters: pathParam("pid", "provider id"),
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/admin/providers/{pid}/models": {
			"get": Operation{
				Summary:    "List a platform provider's models",
				Tags:       []string{"admin"},
				Security:   bearer,
				Parameters: pathParam("pid", "provider id"),
				Responses:  jsonResp("models", map[string]any{"type": "object", "properties": map[string]any{"models": map[string]any{"type": "array", "items": ref("ProviderModel")}}}),
			},
			"post": Operation{
				Summary:     "Create a platform provider model (platform admin)",
				Tags:        []string{"admin"},
				Security:    bearer,
				Parameters:  pathParam("pid", "provider id"),
				RequestBody: jsonBody(ref("ProviderModelRequest")),
				Responses: map[string]Response{
					"201": {Description: "created", Content: map[string]Content{"application/json": {Schema: ref("ProviderModel")}}},
					"401": {Description: "invalid or missing bearer token"},
					"403": {Description: "not a platform admin"},
				},
			},
		},
		"/api/admin/providers/{pid}/models/fetch": {
			"post": Operation{
				Summary:    "Discover a platform provider's models remotely (platform admin)",
				Tags:       []string{"admin"},
				Security:   bearer,
				Parameters: pathParam("pid", "provider id"),
				Responses:  jsonResp("fetched models", map[string]any{"type": "object", "properties": map[string]any{"models": map[string]any{"type": "array", "items": ref("ProviderModel")}}}),
			},
		},
		"/api/admin/providers/{pid}/models/{mid}": {
			"patch": Operation{
				Summary:     "Update a platform provider model (platform admin)",
				Tags:        []string{"admin"},
				Security:    bearer,
				Parameters:  []Parameter{{Name: "pid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "mid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				RequestBody: jsonBody(ref("ProviderModelRequest")),
				Responses:   jsonResp("updated", ref("ProviderModel")),
			},
			"delete": Operation{
				Summary:    "Delete a platform provider model (platform admin)",
				Tags:       []string{"admin"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "pid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "mid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/admin/providers/{pid}/models/{mid}/default": {
			"post": Operation{
				Summary:    "Set a platform provider model as default (platform admin)",
				Tags:       []string{"admin"},
				Security:   bearer,
				Parameters: []Parameter{{Name: "pid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, {Name: "mid", In: "path", Required: true, Schema: map[string]any{"type": "string"}}},
				Responses:  ok204(unauthorized, forbidden, notFound),
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
			"delete": Operation{
				Summary:  "Clear a monthly token budget (lift the limit)",
				Tags:     []string{"admin"},
				Security: bearer,
				Parameters: []Parameter{
					{Name: "scope", In: "query", Required: true, Schema: map[string]any{"type": "string", "enum": []string{"user", "team"}}},
					{Name: "owner_id", In: "query", Required: true, Schema: map[string]any{"type": "string"}},
				},
				Responses: ok204(unauthorized, forbidden),
			},
		},
		"/api/admin/memories": {
			"get": Operation{
				Summary:   "List memories across users (platform admin)",
				Tags:      []string{"admin"},
				Security:  bearer,
				Responses: jsonResp("memories", map[string]any{"type": "object", "properties": map[string]any{"memories": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}}}),
			},
		},
		"/api/admin/memories/{id}": {
			"delete": Operation{
				Summary:    "Delete any user's memory (platform admin)",
				Tags:       []string{"admin"},
				Security:   bearer,
				Parameters: pathParam("id", "memory id"),
				Responses:  ok204(unauthorized, forbidden, notFound),
			},
		},
		"/api/admin/memories/{id}/deprecate": {
			"post": Operation{
				Summary:    "Deprecate any user's memory (platform admin)",
				Tags:       []string{"admin"},
				Security:   bearer,
				Parameters: pathParam("id", "memory id"),
				Responses:  ok204(unauthorized, forbidden, notFound),
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
		"/metrics": {
			"get": Operation{
				Summary:   "Prometheus metrics",
				Tags:      []string{"meta"},
				Responses: map[string]Response{"200": {Description: "Prometheus text format"}},
			},
		},
		"/openapi.json": {
			"get": Operation{
				Summary:   "This OpenAPI document",
				Tags:      []string{"meta"},
				Responses: map[string]Response{"200": {Description: "OpenAPI 3.0 JSON"}},
			},
		},

		// ---- SSO (open; enabled only when OIDC_ISSUER is set) ----
		"/auth/oidc/enabled": {
			"get": Operation{
				Summary:   "Whether OIDC SSO is configured",
				Tags:      []string{"auth"},
				Responses: jsonResp("enabled flag", map[string]any{"type": "object", "properties": map[string]any{"enabled": map[string]any{"type": "boolean"}}}),
			},
		},
		"/auth/oidc/login": {
			"get": Operation{
				Summary:   "Start an OIDC authorization-code flow (302 to the IdP)",
				Tags:      []string{"auth"},
				Responses: map[string]Response{"302": {Description: "redirect to the identity provider"}},
			},
		},
		"/auth/oidc/callback": {
			"get": Operation{
				Summary:   "OIDC callback: exchanges the code and redirects to /#token=...",
				Tags:      []string{"auth"},
				Responses: map[string]Response{"302": {Description: "redirect to the SPA with #token=... or #totp_required=..."}},
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
				"phone":         map[string]any{"type": "string", "description": "bound mobile number, masked (\"****8000\"); empty when unbound"},
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
				"model":         map[string]any{"type": "string", "description": "model override on the resolved provider; an unknown/disabled name falls back to the provider's default"},
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
		"SkillRequest": map[string]any{
			"type":     "object",
			"required": []string{"name"},
			"properties": map[string]any{
				"name":        map[string]any{"type": "string"},
				"description": map[string]any{"type": "string"},
				"content":     map[string]any{"type": "string"},
			},
		},
		"SkillVersion": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"version":    map[string]any{"type": "integer"},
				"created_at": map[string]any{"type": "string"},
				"content":    map[string]any{"type": "string"},
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
		"Upload": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":         map[string]any{"type": "string"},
				"filename":   map[string]any{"type": "string"},
				"size":       map[string]any{"type": "integer"},
				"media_type": map[string]any{"type": "string"},
				"created_at": map[string]any{"type": "string"},
			},
		},
		"Team": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":         map[string]any{"type": "string"},
				"name":       map[string]any{"type": "string"},
				"created_at": map[string]any{"type": "string"},
			},
		},
		"TeamMember": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"user_id": map[string]any{"type": "string"},
				"role":    map[string]any{"type": "string", "enum": []string{"owner", "admin", "member"}},
			},
		},
		"Provider": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":         map[string]any{"type": "string"},
				"vendor":     map[string]any{"type": "string"},
				"label":      map[string]any{"type": "string"},
				"is_default": map[string]any{"type": "boolean"},
			},
		},
		"ProviderRequest": map[string]any{
			"type":     "object",
			"required": []string{"vendor", "label"},
			"properties": map[string]any{
				"vendor":     map[string]any{"type": "string"},
				"label":      map[string]any{"type": "string"},
				"base_url":   map[string]any{"type": "string"},
				"api_key":    map[string]any{"type": "string", "description": "plaintext on create/update; never echoed back"},
				"is_default": map[string]any{"type": "boolean"},
			},
		},
		"ProviderModel": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":         map[string]any{"type": "string"},
				"model":      map[string]any{"type": "string"},
				"is_default": map[string]any{"type": "boolean"},
			},
		},
		"ProviderModelRequest": map[string]any{
			"type":     "object",
			"required": []string{"model"},
			"properties": map[string]any{
				"model":      map[string]any{"type": "string"},
				"is_default": map[string]any{"type": "boolean"},
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
