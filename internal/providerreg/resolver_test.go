package providerreg

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// resolverEnv seeds a default system provider + a team with members.
func resolverEnv(t *testing.T) (*Resolver, *PGStore, *sql.DB) {
	t.Helper()
	s, db := newPGStore(t, nil)
	ctx := context.Background()

	sysID := seedProvider(t, db, ScopeSystem, "", "res-sys-"+randHex(), VendorOpenAI, true, true)
	seedModel(t, db, sysID, "sys-model", true, true, true)

	_, teamID := seedUserTeam(t, db)
	t.Cleanup(func() { s.ClearTeamAssignment(ctx, teamID) })

	r := NewResolver(s)
	return r, s, db
}

func TestResolveTeamAssignmentOverSystemProvider(t *testing.T) {
	r, s, db := resolverEnv(t)
	ctx := context.Background()
	_, teamID := seedUserTeam(t, db)

	// The default system provider is the platform default; the team picks it.
	def, err := s.PlatformDefault(ctx)
	if err != nil {
		t.Fatalf("platform default: %v", err)
	}
	if err := s.SetTeamAssignment(ctx, teamID, def.ID, ""); err != nil {
		t.Fatalf("assign: %v", err)
	}
	tg, err := r.ResolveForTeam(ctx, teamID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tg.ProviderID != def.ID || tg.Model != "sys-model" {
		t.Fatalf("target = %+v", tg)
	}
}

func TestResolveTeamAssignmentOverTeamProvider(t *testing.T) {
	r, _, db := resolverEnv(t)
	ctx := context.Background()
	_, teamID := seedUserTeam(t, db)

	tp, err := r.store.CreateProvider(ctx, Provider{Scope: ScopeTeam, TeamID: teamID, Name: "res-own-" + randHex(), Vendor: VendorAnthropic, RawKey: "sk-team", Enabled: true})
	if err != nil {
		t.Fatalf("create team provider: %v", err)
	}
	t.Cleanup(func() { r.store.DeleteProvider(ctx, tp.ID) })
	tm := seedModel(t, db, tp.ID, "claude-t", true, true, true)

	if err := r.store.SetTeamAssignment(ctx, teamID, tp.ID, tm); err != nil {
		t.Fatalf("assign: %v", err)
	}
	tg, err := r.Resolve(ctx, memberOf(t, db, teamID))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tg.ProviderID != tp.ID || tg.Model != "claude-t" || tg.APIKey != "sk-team" || tg.Vendor != VendorAnthropic {
		t.Fatalf("target = %+v", tg)
	}
}

func TestResolvePlatformDefaultFallback(t *testing.T) {
	r, s, db := resolverEnv(t)
	ctx := context.Background()
	_, teamID := seedUserTeam(t, db)

	// No assignment → platform default.
	def, err := s.PlatformDefault(ctx)
	if err != nil {
		t.Fatalf("platform default: %v", err)
	}
	tg, err := r.ResolveForTeam(ctx, teamID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tg.ProviderID != def.ID || tg.Model != "sys-model" {
		t.Fatalf("target = %+v", tg)
	}
	_ = db
}

func TestResolveNoProvider(t *testing.T) {
	// A store with nothing enabled resolves to ErrNoProvider. The shared dev
	// registry may already hold enabled system providers (bootstrap fallback),
	// so temporarily disable all of them and restore the exact prior state —
	// this test must pass on a fresh database and on a dev database alike.
	s, db := newPGStore(t, nil)
	restore := disableSystemProviders(t, db)
	defer restore()

	ctx := context.Background()
	r := NewResolver(s)

	if _, err := r.ResolveForTeam(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("resolve on empty registry: %v", err)
	}
	_ = db
}

// disableSystemProviders flips every system provider to disabled + non-default
// so a resolver sees an empty registry, returning a restore func that puts
// back the exact prior state of each row.
func disableSystemProviders(t *testing.T, db *sql.DB) func() {
	t.Helper()
	type row struct {
		id      string
		def     bool
		enabled bool
	}
	rows, err := db.Query(`SELECT id::text, is_default, enabled FROM providers WHERE scope = 'system'`)
	if err != nil {
		t.Fatalf("snapshot system providers: %v", err)
	}
	var snap []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.def, &r.enabled); err != nil {
			rows.Close()
			t.Fatalf("scan provider snapshot: %v", err)
		}
		snap = append(snap, r)
	}
	rows.Close()
	if _, err := db.Exec(`UPDATE providers SET is_default = false, enabled = false WHERE scope = 'system'`); err != nil {
		t.Fatalf("disable system providers: %v", err)
	}
	return func() {
		for _, r := range snap {
			if _, err := db.Exec(`UPDATE providers SET is_default = $2, enabled = $3 WHERE id = $1`, r.id, r.def, r.enabled); err != nil {
				t.Errorf("restore provider %s: %v", r.id, err)
			}
		}
	}
}

func TestResolveModelFailClosed(t *testing.T) {
	r, s, db := resolverEnv(t)
	ctx := context.Background()
	_, teamID := seedUserTeam(t, db)

	if _, err := s.PlatformDefault(ctx); err != nil {
		t.Fatalf("platform default: %v", err)
	}
	tg, err := r.ResolveForTeam(ctx, teamID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Empty reference → the resolved default model.
	m, err := r.ResolveModel(ctx, tg, "")
	if err != nil || m != "sys-model" {
		t.Fatalf("empty reference: %q %v", m, err)
	}
	// Known name → itself.
	m, err = r.ResolveModel(ctx, tg, "sys-model")
	if err != nil || m != "sys-model" {
		t.Fatalf("known reference: %q %v", m, err)
	}
	// Unknown name → fail-closed.
	if _, err := r.ResolveModel(ctx, tg, "nope"); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("unknown reference: %v", err)
	}
	_ = db
}

// TestEnabledModelsListsOnlyEnabled verifies the picker read: enabled models
// of the resolved provider are listed, disabled ones are not.
func TestEnabledModelsListsOnlyEnabled(t *testing.T) {
	r, s, db := resolverEnv(t)
	ctx := context.Background()
	_, teamID := seedUserTeam(t, db)

	if _, err := s.PlatformDefault(ctx); err != nil {
		t.Fatalf("platform default: %v", err)
	}
	tg, err := r.ResolveForTeam(ctx, teamID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// resolverEnv seeded "sys-model" (enabled). Add a disabled model: it must
	// not appear in the picker.
	seedModel(t, db, tg.ProviderID, "disabled-model", false, false, false)
	names, err := r.EnabledModels(ctx, tg)
	if err != nil {
		t.Fatalf("enabled models: %v", err)
	}
	if len(names) != 1 || names[0] != "sys-model" {
		t.Fatalf("models = %v, want [sys-model]", names)
	}
}

func TestVisionModelPick(t *testing.T) {	r, s, db := resolverEnv(t)
	ctx := context.Background()
	_, teamID := seedUserTeam(t, db)

	if _, err := s.PlatformDefault(ctx); err != nil {
		t.Fatalf("platform default: %v", err)
	}
	tg, err := r.ResolveForTeam(ctx, teamID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The default system provider has a vision model (resolverEnv seeds one).
	if name, ok := r.VisionModel(ctx, tg); !ok || name != "sys-model" {
		t.Fatalf("vision model = %q %v", name, ok)
	}

	// A provider with no vision model reports none.
	plain := seedProvider(t, db, ScopeSystem, "", "res-plain-"+randHex(), VendorOpenAI, false, true)
	seedModel(t, db, plain, "text-only", false, true, true)
	plainTarget := Target{ProviderID: plain, Vendor: VendorOpenAI}
	if _, ok := r.VisionModel(ctx, plainTarget); ok {
		t.Fatal("provider without vision model reported one")
	}
}

// memberOf inserts a member into an existing team and returns their id.
func memberOf(t *testing.T, db *sql.DB, teamID string) string {
	t.Helper()
	var userID string
	if err := db.QueryRow(`INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id`,
		"res-"+randHex()+"@test.dev").Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, userID) })
	if _, err := db.Exec(`INSERT INTO team_memberships (team_id, user_id, role) VALUES ($1,$2,'member')`, teamID, userID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	return userID
}
