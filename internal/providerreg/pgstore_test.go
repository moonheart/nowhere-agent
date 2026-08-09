package providerreg

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"nowhere-agent/internal/secrets"
)

func pgTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("PROVIDERREG_PG_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("DB_DSN")
	}
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3_000_000_000)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("no postgres reachable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newPGStore(t *testing.T, enc *secrets.Encryptor) (*PGStore, *sql.DB) {
	t.Helper()
	db := pgTestDB(t)
	s := NewPGStore(db)
	if enc != nil {
		s = s.WithEncryption(enc)
	}
	return s, db
}

// seedProvider inserts a provider directly and returns its id.
func seedProvider(t *testing.T, db *sql.DB, scope, teamID, name, vendor string, isDefault, enabled bool) string {
	t.Helper()
	var id string
	err := db.QueryRow(`
		INSERT INTO providers (scope, team_id, name, vendor, is_default, enabled)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		scope, nullIfEmpty(teamID), name, vendor, isDefault, enabled).Scan(&id)
	if err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM providers WHERE id = $1`, id) })
	return id
}

func seedModel(t *testing.T, db *sql.DB, providerID, name string, vision, isDefault, enabled bool) string {
	t.Helper()
	var id string
	err := db.QueryRow(`
		INSERT INTO provider_models (provider_id, name, vision, is_default, enabled)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		providerID, name, vision, isDefault, enabled).Scan(&id)
	if err != nil {
		t.Fatalf("seed model: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM provider_models WHERE id = $1`, id) })
	return id
}

func seedUserTeam(t *testing.T, db *sql.DB) (userID, teamID string) {
	t.Helper()
	err := db.QueryRow(`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		"pr-"+randHex()+"@test.dev").Scan(&userID)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	err = db.QueryRow(`INSERT INTO teams (name) VALUES ($1) RETURNING id`, "t-"+randHex()).Scan(&teamID)
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO team_memberships (team_id, user_id, role) VALUES ($1,$2,'owner')`, teamID, userID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, userID) })
	t.Cleanup(func() { db.Exec(`DELETE FROM teams WHERE id = $1`, teamID) })
	return userID, teamID
}

func TestCreateProviderRoundTrip(t *testing.T) {
	s, db := newPGStore(t, nil)
	id, err := s.CreateProvider(context.Background(), Provider{
		Scope:   ScopeSystem,
		Name:    "openai-" + randHex(),
		Vendor:  VendorOpenAI,
		BaseURL: "https://api.openai.com/v1",
		RawKey:  "sk-test-12345678",
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM providers WHERE id = $1`, id.ID) })

	got, err := s.GetProvider(context.Background(), id.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != id.Name || got.Vendor != VendorOpenAI || got.RawKey != "sk-test-12345678" || !got.Enabled {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestScopedNameConflicts(t *testing.T) {
	s, db := newPGStore(t, nil)
	ctx := context.Background()

	name := "dup-" + randHex()
	sys, err := s.CreateProvider(ctx, Provider{Scope: ScopeSystem, Name: name, Vendor: VendorOpenAI})
	if err != nil {
		t.Fatalf("create system: %v", err)
	}
	t.Cleanup(func() { s.DeleteProvider(ctx, sys.ID) })

	if _, err := s.CreateProvider(ctx, Provider{Scope: ScopeSystem, Name: name, Vendor: VendorAnthropic}); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("system name conflict: got %v", err)
	}

	// A team provider may reuse the same name.
	_, teamID := seedUserTeam(t, db)
	tp, err := s.CreateProvider(ctx, Provider{Scope: ScopeTeam, TeamID: teamID, Name: name, Vendor: VendorOpenAI})
	if err != nil {
		t.Fatalf("create team provider with same name: %v", err)
	}
	t.Cleanup(func() { s.DeleteProvider(ctx, tp.ID) })
}

func TestDefaultInvariants(t *testing.T) {
	s, _ := newPGStore(t, nil)
	ctx := context.Background()

	a := seedProvider(t, pgTestDB(t), ScopeSystem, "", "defa-"+randHex(), VendorOpenAI, false, true)
	b := seedProvider(t, pgTestDB(t), ScopeSystem, "", "defb-"+randHex(), VendorAnthropic, false, true)

	if err := s.SetDefaultProvider(ctx, a); err != nil {
		t.Fatalf("set default a: %v", err)
	}
	// Moving the default clears the previous one: a is no longer default.
	if err := s.SetDefaultProvider(ctx, b); err != nil {
		t.Fatalf("set default b: %v", err)
	}
	pa, err := s.GetProvider(ctx, a)
	if err != nil {
		t.Fatalf("get a: %v", err)
	}
	if pa.IsDefault {
		t.Fatal("a should no longer be default after moving")
	}
	def, err := s.PlatformDefault(ctx)
	if err != nil {
		t.Fatalf("platform default: %v", err)
	}
	if def.ID != b {
		t.Fatalf("platform default = %s, want %s", def.ID, b)
	}
}

func TestDeleteConstraints(t *testing.T) {
	s, db := newPGStore(t, nil)
	ctx := context.Background()

	pid := seedProvider(t, db, ScopeSystem, "", "used-"+randHex(), VendorOpenAI, true, true)
	mid := seedModel(t, db, pid, "gpt-x", false, true, true)

	_, teamID := seedUserTeam(t, db)
	if err := s.SetTeamAssignment(ctx, teamID, pid, mid); err != nil {
		t.Fatalf("assign: %v", err)
	}
	t.Cleanup(func() { s.ClearTeamAssignment(ctx, teamID) })

	if err := s.DeleteProvider(ctx, pid); err == nil {
		t.Fatal("deleting a provider in use should be rejected")
	}
	if err := s.DeleteModel(ctx, mid); err == nil {
		t.Fatal("deleting a model a team assignment uses should be rejected")
	}
}

func TestCrossTeamInvisibility(t *testing.T) {
	s, db := newPGStore(t, nil)
	ctx := context.Background()

	_, teamA := seedUserTeam(t, db)
	_, teamB := seedUserTeam(t, db)
	tp, err := s.CreateProvider(ctx, Provider{Scope: ScopeTeam, TeamID: teamA, Name: "priv-" + randHex(), Vendor: VendorOpenAI})
	if err != nil {
		t.Fatalf("create team provider: %v", err)
	}
	t.Cleanup(func() { s.DeleteProvider(ctx, tp.ID) })

	visible, err := s.VisibleToTeam(ctx, teamB)
	if err != nil {
		t.Fatalf("visible to B: %v", err)
	}
	for _, p := range visible {
		if p.ID == tp.ID {
			t.Fatal("team A's provider leaked into team B's view")
		}
	}
}

func TestEncryptionRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	enc, err := secrets.NewSingle(key)
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	s, db := newPGStore(t, enc)
	ctx := context.Background()

	p, err := s.CreateProvider(ctx, Provider{Scope: ScopeSystem, Name: "enc-" + randHex(), Vendor: VendorOpenAI, RawKey: "sk-secret-9876"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM providers WHERE id = $1`, p.ID) })

	// At rest it is an envelope, not the plaintext.
	var stored string
	if err := db.QueryRow(`SELECT api_key FROM providers WHERE id = $1`, p.ID).Scan(&stored); err != nil {
		t.Fatalf("read stored: %v", err)
	}
	if !secrets.IsEncrypted(stored) {
		t.Fatal("stored key is not an encrypted envelope")
	}

	got, err := s.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.RawKey != "sk-secret-9876" {
		t.Fatalf("decrypted key = %q", got.RawKey)
	}
}

func TestTeamAssignmentConstraints(t *testing.T) {
	s, db := newPGStore(t, nil)
	ctx := context.Background()

	pid := seedProvider(t, db, ScopeSystem, "", "assign-"+randHex(), VendorOpenAI, true, false)
	_, teamID := seedUserTeam(t, db)

	if err := s.SetTeamAssignment(ctx, teamID, pid, ""); !errors.Is(err, ErrProviderDisabled) {
		t.Fatalf("disabled provider assignment: got %v", err)
	}

	if err := s.SetTeamAssignment(ctx, teamID, pid, "some-model"); err == nil {
		t.Fatal("assignment to a nonexistent model should fail")
	}
}

func TestEnabledModelResolution(t *testing.T) {
	s, db := newPGStore(t, nil)
	ctx := context.Background()

	pid := seedProvider(t, db, ScopeSystem, "", "em-"+randHex(), VendorOpenAI, true, true)
	seedModel(t, db, pid, "fast", false, true, true)
	seedModel(t, db, pid, "slow", false, false, false)

	if _, err := s.EnabledModel(ctx, pid, "fast"); err != nil {
		t.Fatalf("enabled model: %v", err)
	}
	if _, err := s.EnabledModel(ctx, pid, "slow"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled model should not resolve: %v", err)
	}
	if _, err := s.EnabledModel(ctx, pid, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown model: %v", err)
	}
}
