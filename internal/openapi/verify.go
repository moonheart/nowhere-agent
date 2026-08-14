package openapi

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// openRoutes are the contract entries registered directly on the outer mux
// (meta, identity, phone, oidc, inbound public) rather than on the protected
// httpx.Router group, so the recorded-pattern check (VerifyAuthedRoutes)
// cannot see them. They are exempted from the "every contract route must be
// recorded" direction; the spec test still pins them against the document.
// Keep in sync with the open sections of registeredRoutes above.
var openRoutes = map[string][]string{
	"/healthz":            {"get"},
	"/metrics":            {"get"},
	"/openapi.json":       {"get"},
	"/auth/oidc/enabled":  {"get"},
	"/auth/oidc/login":    {"get"},
	"/auth/oidc/callback": {"get"},
	"/api/auth/signup":             {"post"},
	"/api/auth/login":              {"post"},
	"/api/auth/logout":             {"post"},
	"/api/auth/totp/verify":        {"post"},
	"/api/auth/phone/request-code": {"post"},
	"/api/auth/phone/verify":       {"post"},
	"/api/auth/phone/enabled":      {"get"},
	"/api/me":                      {"get"},
	"/api/inbound/{id}":            {"post"},
}

// VerifyAuthedRoutes cross-checks the patterns a real route assembly recorded
// (httpx.Router.Patterns) against the registeredRoutes contract, in both
// directions:
//
//  1. every recorded (path, method) must be in the contract — a route added in
//     code without syncing openapi/routes.go fails here;
//  2. every contract (path, method) that is not one of the openRoutes (which
//     the recording cannot see) must have been recorded — a route removed
//     from code leaves routes.go stale and fails here.
//
// The check runs at boot (cmd/server), because the full route assembly lives
// in run() where no test can reach it; a drift fails startup instead of
// silently shipping a spec that lies. Wildcards normalize to the OpenAPI form
// ({path...} → {path}), mirroring registeredRoutes.
func VerifyAuthedRoutes(recorded []string) error {
	seen := make(map[route]bool, len(recorded))
	for _, p := range recorded {
		path, method, ok := splitPattern(p)
		if !ok {
			return fmt.Errorf("recorded route %q is not a METHOD /path pattern", p)
		}
		seen[route{path: path, method: method}] = true
	}

	// Direction 1: every recorded route must be in the contract.
	var missing []string
	for r := range seen {
		if !routeIn(registeredRoutes, r) {
			missing = append(missing, r.method+" "+r.path)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("routes registered in code are missing from the OpenAPI contract (openapi/routes.go + paths.go are stale): %s", strings.Join(missing, ", "))
	}

	// Direction 2: every contract route that is not open must be recorded.
	var stale []string
	for path, methods := range registeredRoutes {
		for _, m := range methods {
			if routeIn(openRoutes, route{path: path, method: m}) {
				continue
			}
			if !seen[route{path: path, method: m}] {
				stale = append(stale, m+" "+path)
			}
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		return fmt.Errorf("routes listed in the OpenAPI contract are not registered by any handler (routes.go is stale): %s", strings.Join(stale, ", "))
	}
	return nil
}

type route struct{ path, method string }

// routeIn reports whether (path, method) appears in a contract map.
func routeIn(routes map[string][]string, want route) bool {
	for _, m := range routes[want.path] {
		if m == want.method {
			return true
		}
	}
	return false
}

// wildcardRe matches a trailing-ellipsis wildcard segment ({path...}); the
// OpenAPI contract spells every wildcard without the ellipsis.
var wildcardRe = regexp.MustCompile(`\{([^}/]+)\.\.\.\}`)

// splitPattern splits one registered ServeMux pattern ("GET /api/chat",
// "GET /api/chat/sessions/{id}/files/{path...}") into its path and
// lowercased method, normalizing wildcards to the OpenAPI form.
func splitPattern(pattern string) (path, method string, ok bool) {
	m, p, found := strings.Cut(pattern, " ")
	if !found || m == "" || p == "" {
		return "", "", false
	}
	return wildcardRe.ReplaceAllString(p, "{$1}"), strings.ToLower(m), true
}
