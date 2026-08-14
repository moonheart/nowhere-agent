package openapi

// registeredRoutes is the wire-format contract as the handlers actually
// register it: path → methods, mirrored from the "METHOD /path" patterns in
// every package's Register / RegisterAuthed call (identity, chatapi, inbound,
// agentdefapi, skillapi, scheduleapi, adminapi, oidc) plus the meta routes
// mounted in cmd/server/main.go. It is the authoritative input for
// TestSpecRoutesMatchRegisteredRoutes, which asserts BOTH directions against
// the OpenAPI document:
//
//  1. every registered (path, method) is documented, and
//  2. every documented path is registered (a spec-only route is a lie).
//
// Keep this list in sync when a route is added, renamed, or removed — the
// test's whole point is to fail loudly when the spec drifts from reality.
// Boot enforces it for the protected tier: cmd/server compares the patterns
// the httpx.Router group actually registered (VerifyAuthedRoutes) against
// this list and refuses to start on drift. Wildcards normalize to the OpenAPI
// form ({path...} → {path}).
var registeredRoutes = map[string][]string{
	// ---- meta (cmd/server/main.go) ----
	"/healthz":            {"get"},
	"/metrics":            {"get"},
	"/openapi.json":       {"get"},
	"/auth/oidc/enabled":  {"get"},
	"/auth/oidc/login":    {"get"},
	"/auth/oidc/callback": {"get"},

	// ---- identity (internal/identity/http.go, phonehttp.go) ----
	"/api/auth/signup":                  {"post"},
	"/api/auth/login":                   {"post"},
	"/api/auth/logout":                  {"post"},
	"/api/auth/totp/verify":             {"post"},
	"/api/auth/phone/request-code":      {"post"},
	"/api/auth/phone/verify":            {"post"},
	"/api/auth/phone/reset-password":    {"post"},
	"/api/auth/phone/enabled":           {"get"},
	"/api/me":                           {"get", "patch", "delete"},
	"/api/me/phone/bind":                {"post"},

	// ---- chatapi (internal/chatapi/handler.go Register + RegisterAuthed) ----
	"/api/chat":                            {"post"},
	"/api/chat/models":                     {"get"},
	"/api/chat/history":                    {"get"},
	"/api/chat/resume":                     {"post"},
	"/api/chat/cancel":                     {"post"},
	"/api/chat/sessions":                   {"get"},
	"/api/chat/sessions/{id}":              {"delete"},
	"/api/chat/sessions/{id}/active":       {"get"},
	"/api/chat/sessions/{id}/state":        {"post"},
	"/api/chat/sessions/{id}/files/{path}": {"get"}, // registered as {path...}
	"/api/chat/sessions/{id}/images":       {"post"},
	"/api/chat/uploads":                    {"post"},
	"/api/chat/uploads/{id}":               {"get"},

	// ---- agentdefapi (internal/agentdefapi/handler.go) ----
	"/api/me/agentdefs":                {"get", "post"},
	"/api/me/agentdefs/{name}":         {"get", "put", "delete"},
	"/api/teams/{id}/agentdefs":        {"get", "post"},
	"/api/teams/{id}/agentdefs/{name}": {"get", "put", "delete"},
	"/api/admin/agentdefs":             {"get", "post"},
	"/api/admin/agentdefs/{name}":      {"get", "put", "delete"},

	// ---- skillapi (internal/skillapi/handler.go) ----
	"/api/me/skills":                            {"get", "post"},
	"/api/me/skills/{id}":                       {"get", "put", "delete"},
	"/api/me/skills/{id}/versions":              {"get"},
	"/api/me/skills/{id}/versions/{v}":          {"get"},
	"/api/me/skills/{id}/rollback/{v}":          {"post"},
	"/api/me/skills/{id}/enable":                {"post"},
	"/api/me/skills/{id}/disable":               {"post"},
	"/api/me/skills/{id}/move":                  {"post"},
	"/api/teams/{id}/skills":                    {"get", "post"},
	"/api/teams/{id}/skills/{sid}":              {"get", "put", "delete"},
	"/api/teams/{id}/skills/{sid}/versions":     {"get"},
	"/api/teams/{id}/skills/{sid}/versions/{v}": {"get"},
	"/api/teams/{id}/skills/{sid}/rollback/{v}": {"post"},
	"/api/teams/{id}/skills/{sid}/enable":       {"post"},
	"/api/teams/{id}/skills/{sid}/disable":      {"post"},
	"/api/admin/skills":                         {"get", "post"},
	"/api/admin/skills/{id}":                    {"get", "put", "delete"},
	"/api/admin/skills/{id}/versions":           {"get"},
	"/api/admin/skills/{id}/versions/{v}":       {"get"},
	"/api/admin/skills/{id}/rollback/{v}":       {"post"},
	"/api/admin/skills/{id}/enable":             {"post"},
	"/api/admin/skills/{id}/disable":            {"post"},

	// ---- scheduleapi (internal/scheduleapi/handler.go) ----
	"/api/me/scheduled-tasks":                     {"get", "post"},
	"/api/me/scheduled-tasks/{id}":                {"get", "put", "delete"},
	"/api/me/scheduled-tasks/{id}/enable":         {"post"},
	"/api/me/scheduled-tasks/{id}/disable":        {"post"},
	"/api/me/scheduled-tasks/{id}/run":            {"post"},
	"/api/me/scheduled-tasks/{id}/sessions":       {"get"},
	"/api/me/scheduled-tasks/{id}/sessions/clear": {"post"},

	// ---- inbound (internal/inbound/handler.go RegisterPublic + RegisterAuthed) ----
	"/api/inbound/{id}":           {"post"},
	"/api/me/inbound":             {"get", "post"},
	"/api/me/inbound/{id}":        {"patch", "delete"},
	"/api/me/inbound/{id}/rotate": {"post"},

	// ---- adminapi (internal/adminapi/handler.go) ----
	"/api/me/password":                                     {"post"},
	"/api/me/totp/enable":                                  {"post"},
	"/api/me/totp/confirm":                                 {"post"},
	"/api/me/totp/disable":                                 {"post"},
	"/api/me/usage":                                        {"get"},
	"/api/me/memories":                                     {"get"},
	"/api/me/memories/{id}":                                {"delete"},
	"/api/me/dream":                                        {"get", "post"},
	"/api/me/tokens":                                       {"get", "delete"},
	"/api/me/tokens/{id}":                                  {"delete"},
	"/api/me/uploads":                                      {"get"},
	"/api/me/uploads/{id}":                                 {"delete"},
	"/api/me/export":                                       {"get"},
	"/api/teams":                                           {"get", "post"},
	"/api/teams/{id}":                                      {"get", "patch", "delete"},
	"/api/teams/{id}/members":                              {"get", "post"},
	"/api/teams/{id}/members/{userId}":                     {"patch", "delete"},
	"/api/teams/{id}/usage":                                {"get"},
	"/api/teams/{id}/memories":                             {"get"},
	"/api/teams/{id}/memories/{mid}":                       {"delete"},
	"/api/teams/{id}/memories/{mid}/deprecate":             {"post"},
	"/api/teams/{id}/providers":                            {"get", "post"},
	"/api/teams/{id}/providers/{pid}":                      {"patch", "delete"},
	"/api/teams/{id}/providers/{pid}/models":               {"get", "post"},
	"/api/teams/{id}/providers/{pid}/models/fetch":         {"post"},
	"/api/teams/{id}/providers/{pid}/models/{mid}":         {"patch", "delete"},
	"/api/teams/{id}/providers/{pid}/models/{mid}/default": {"post"},
	"/api/teams/{id}/provider-assignment":                  {"put", "delete"},
	"/api/admin/stats":                                     {"get"},
	"/api/admin/users":                                     {"get", "post"},
	"/api/admin/users/{id}":                                {"patch", "delete"},
	"/api/admin/users/{id}/password":                       {"post"},
	"/api/admin/sessions/{id}":                             {"delete"},
	"/api/admin/teams":                                     {"get", "post"},
	"/api/admin/usage":                                     {"get"},
	"/api/admin/quotas":                                    {"get", "put", "delete"},
	"/api/admin/memories":                                  {"get"},
	"/api/admin/memories/{id}":                             {"delete"},
	"/api/admin/memories/{id}/deprecate":                   {"post"},
	"/api/admin/audit":                                     {"get"},
	"/api/admin/service-keys":                              {"get", "post"},
	"/api/admin/service-keys/{id}":                         {"delete"},
	"/api/admin/webhook-deliveries":                        {"get"},
	"/api/admin/webhook-deliveries/{id}/retry":             {"post"},
	"/api/admin/settings":                                  {"get"},
	"/api/admin/settings/{key}":                            {"put"},
	"/api/admin/providers":                                 {"get", "post"},
	"/api/admin/providers/{pid}":                           {"patch", "delete"},
	"/api/admin/providers/{pid}/default":                   {"post"},
	"/api/admin/providers/{pid}/models":                    {"get", "post"},
	"/api/admin/providers/{pid}/models/fetch":              {"post"},
	"/api/admin/providers/{pid}/models/{mid}":              {"patch", "delete"},
	"/api/admin/providers/{pid}/models/{mid}/default":      {"post"},
}
