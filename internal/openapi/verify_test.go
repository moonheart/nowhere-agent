package openapi

import (
	"strings"
	"testing"
)

// recordedAll builds the recorded-pattern set a fully-synced assembly would
// produce: every contract route except the open mux routes, in ServeMux
// registration form (uppercase method, {path...} wildcards).
func recordedAll() []string {
	var out []string
	for path, methods := range registeredRoutes {
		for _, m := range methods {
			if routeIn(openRoutes, route{path: path, method: m}) {
				continue
			}
			out = append(out, strings.ToUpper(m)+" "+path)
		}
	}
	return out
}

// TestVerifyAuthedRoutesAcceptsFullSet: the recorded set derived from the
// contract passes — the baseline against which every drift test is measured.
func TestVerifyAuthedRoutesAcceptsFullSet(t *testing.T) {
	if err := VerifyAuthedRoutes(recordedAll()); err != nil {
		t.Fatalf("full recorded set must pass: %v", err)
	}
	if err := VerifyAuthedRoutes(nil); err == nil {
		t.Fatal("empty recorded set must fail: every authed contract route would be unregistered")
	}
}

// TestVerifyAuthedRoutesCatchesUnsyncedCodeRoute pins direction 1: a route
// registered in code but forgotten in routes.go/paths.go fails the check
// naming the route — the drift this test exists to catch.
func TestVerifyAuthedRoutesCatchesUnsyncedCodeRoute(t *testing.T) {
	recorded := append(recordedAll(), "GET /api/chat/brand-new")
	err := VerifyAuthedRoutes(recorded)
	if err == nil {
		t.Fatal("an unsynced code route must fail the check")
	}
	if !strings.Contains(err.Error(), "brand-new") {
		t.Errorf("error %q must name the offending route", err)
	}
}

// TestVerifyAuthedRoutesCatchesStaleContractEntry pins direction 2: a
// routes.go entry no handler registers anymore fails the check naming the
// route.
func TestVerifyAuthedRoutesCatchesStaleContractEntry(t *testing.T) {
	// Drop one authed route (PATCH /api/me is registered only by adminapi)
	// from the recorded set to simulate a route removed in code.
	var recorded []string
	for _, p := range recordedAll() {
		if p == "PATCH /api/me" {
			continue
		}
		recorded = append(recorded, p)
	}
	err := VerifyAuthedRoutes(recorded)
	if err == nil {
		t.Fatal("a stale contract entry must fail the check")
	}
	if !strings.Contains(err.Error(), "patch /api/me") {
		t.Errorf("error %q must name the stale route", err)
	}
}

// TestVerifyAuthedRoutesNormalizesWildcards: the recorded {path...} wildcard
// matches the contract's {path} form (chatapi registers the files route with
// {path...}; routes.go spells it {path}).
func TestVerifyAuthedRoutesNormalizesWildcards(t *testing.T) {
	recorded := recordedAll()
	for i, p := range recorded {
		if p == "GET /api/chat/sessions/{id}/files/{path}" {
			recorded[i] = "GET /api/chat/sessions/{id}/files/{path...}"
			break
		}
	}
	if err := VerifyAuthedRoutes(recorded); err != nil {
		t.Fatalf("wildcard normalization must pass: %v", err)
	}
}
