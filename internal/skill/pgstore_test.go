package skill

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"nowhere-agent/internal/identity"
)

// These tests run against the shared development Postgres (skipping when none
// is reachable), the repo's convention. The test database IS the dev database,
// so every row uses a unique random name and cleanup deletes only the skills
// this test created, by ID — never an unscoped DELETE/UPDATE.

func skillTestDSN() string {
	if v := os.Getenv("SKILL_PG_TEST_DSN"); v != "" {
		return v
	}
	if v := os.Getenv("DB_DSN"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"
}

func pgSkillDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", skillTestDSN())
	if err != nil {
		t.Skipf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("no postgres reachable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func skillSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// cleanupSkill deletes exactly the skill row (and cascaded versions) created by
// a test. It is scoped by ID, so it can never touch another tenant's data.
func cleanupSkill(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM skills WHERE id = $1`, id); err != nil {
			t.Logf("cleanup skill %s: %v", id, err)
		}
	})
}

func putSkill(t *testing.T, st *PGStore, sk Skill) Skill {
	t.Helper()
	saved, err := st.Put(context.Background(), sk, "test")
	if err != nil {
		t.Fatalf("put skill %q: %v", sk.Name, err)
	}
	return saved
}

// TestPGStorePutAndGetPriority: a name resolves user > team > system, and the
// version increments on each save.
func TestPGStorePutAndGetPriority(t *testing.T) {
	db := pgSkillDB(t)
	st := NewPGStore(db)
	ctx := context.Background()
	name := "prio-" + skillSuffix()
	userID := "u-" + skillSuffix()
	teamID := "t-" + skillSuffix()

	// Seed all three scopes with the same name; track ids for scoped cleanup.
	sys := putSkill(t, st, Skill{Name: name, Description: "system", Body: "sys", Scope: identity.SystemScope()})
	cleanupSkill(t, db, sys.ID)
	team := putSkill(t, st, Skill{Name: name, Description: "team", Body: "tm", Scope: identity.TeamScope(teamID)})
	cleanupSkill(t, db, team.ID)
	usr := putSkill(t, st, Skill{Name: name, Description: "user", Body: "usr", Scope: identity.UserScope(userID)})
	cleanupSkill(t, db, usr.ID)

	scopes := []identity.ScopeRef{identity.UserScope(userID), identity.TeamScope(teamID), identity.SystemScope()}

	got, ok, err := st.Get(ctx, name, scopes)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Body != "usr" {
		t.Errorf("priority resolution = body %q, want user override", got.Body)
	}

	// With no user skill visible, the team one wins over system.
	got, ok, err = st.Get(ctx, name, []identity.ScopeRef{identity.TeamScope(teamID), identity.SystemScope()})
	if err != nil || !ok {
		t.Fatalf("get team-only: ok=%v err=%v", ok, err)
	}
	if got.Body != "tm" {
		t.Errorf("team scope resolution = body %q, want team", got.Body)
	}

	// Saving again bumps the version.
	v2 := putSkill(t, st, Skill{Name: name, Description: "user2", Body: "usr2", Scope: identity.UserScope(userID)})
	if v2.Version != 2 {
		t.Errorf("second save version = %d, want 2", v2.Version)
	}
	if v2.ID != usr.ID {
		t.Errorf("second save id = %q, want same pointer row %q", v2.ID, usr.ID)
	}
}

// TestPGStoreListDedup: List returns one L0 entry per name, priority-resolved.
func TestPGStoreListDedup(t *testing.T) {
	db := pgSkillDB(t)
	st := NewPGStore(db)
	ctx := context.Background()
	name := "dedup-" + skillSuffix()
	userID := "u-" + skillSuffix()

	sys := putSkill(t, st, Skill{Name: name, Description: "system desc", Scope: identity.SystemScope()})
	cleanupSkill(t, db, sys.ID)
	usr := putSkill(t, st, Skill{Name: name, Description: "user desc", Scope: identity.UserScope(userID)})
	cleanupSkill(t, db, usr.ID)

	scopes := []identity.ScopeRef{identity.UserScope(userID), identity.SystemScope()}
	l0, err := st.List(ctx, scopes)
	if err != nil {
		t.Fatal(err)
	}
	var found []L0
	for _, e := range l0 {
		if e.Name == name {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 catalog entry for %q, got %d", name, len(found))
	}
	if found[0].Description != "user desc" {
		t.Errorf("catalog description = %q, want the user (higher-priority) one", found[0].Description)
	}
}

// TestPGStoreDisabledHiddenFromAgent: SetEnabled(false) drops a skill from the
// agent's Get/List resolution while keeping it visible in the management reads
// (ListByScope/ByID); re-enabling restores it.
func TestPGStoreDisabledHiddenFromAgent(t *testing.T) {
	db := pgSkillDB(t)
	st := NewPGStore(db)
	ctx := context.Background()
	name := "enab-" + skillSuffix()
	userID := "u-" + skillSuffix()
	scopes := []identity.ScopeRef{identity.UserScope(userID)}

	sk := putSkill(t, st, Skill{Name: name, Description: "d", Body: "b", Scope: identity.UserScope(userID)})
	cleanupSkill(t, db, sk.ID)
	if !sk.Enabled {
		t.Fatalf("new skill enabled = false, want true by default")
	}

	// Disable: agent resolution loses the skill entirely.
	dis, err := st.SetEnabled(ctx, sk.ID, false)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if dis.Enabled {
		t.Errorf("after disable enabled = true, want false")
	}
	if _, ok, err := st.Get(ctx, name, scopes); err != nil || ok {
		t.Errorf("Get after disable: ok=%v err=%v, want ok=false", ok, err)
	}
	l0, err := st.List(ctx, scopes)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range l0 {
		if e.Name == name {
			t.Errorf("disabled skill %q still in the L0 catalog", name)
		}
	}

	// The management reads still see it (so it can be reviewed and re-enabled).
	if _, err := st.ByID(ctx, sk.ID); err != nil {
		t.Errorf("ByID after disable: %v, want the skill still readable", err)
	}
	mgmt, err := st.ListByScope(ctx, identity.UserScope(userID))
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, m := range mgmt {
		if m.ID == sk.ID {
			seen = true
		}
	}
	if !seen {
		t.Errorf("disabled skill missing from management ListByScope")
	}

	// Re-enable restores agent resolution.
	if _, err := st.SetEnabled(ctx, sk.ID, true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if got, ok, err := st.Get(ctx, name, scopes); err != nil || !ok || got.Body != "b" {
		t.Errorf("Get after re-enable: ok=%v body=%q err=%v", ok, got.Body, err)
	}
}

// TestPGStoreDisabledOverrideFallsThrough: a disabled user override does not
// shadow the lower-scope enabled skill — resolution falls through to system.
func TestPGStoreDisabledOverrideFallsThrough(t *testing.T) {
	db := pgSkillDB(t)
	st := NewPGStore(db)
	ctx := context.Background()
	name := "fall-" + skillSuffix()
	userID := "u-" + skillSuffix()
	scopes := []identity.ScopeRef{identity.UserScope(userID), identity.SystemScope()}

	sys := putSkill(t, st, Skill{Name: name, Description: "sys", Body: "sys-body", Scope: identity.SystemScope()})
	cleanupSkill(t, db, sys.ID)
	usr := putSkill(t, st, Skill{Name: name, Description: "usr", Body: "usr-body", Scope: identity.UserScope(userID)})
	cleanupSkill(t, db, usr.ID)

	if _, err := st.SetEnabled(ctx, usr.ID, false); err != nil {
		t.Fatalf("disable override: %v", err)
	}
	got, ok, err := st.Get(ctx, name, scopes)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Body != "sys-body" {
		t.Errorf("disabled override should fall through to system, got body %q", got.Body)
	}
}

// TestPGStoreMoveToTeam: a user-scope skill moves into a team with its version
// history intact and override bookkeeping cleared.
func TestPGStoreMoveToTeam(t *testing.T) {
	db := pgSkillDB(t)
	st := NewPGStore(db)
	ctx := context.Background()
	name := "move-" + skillSuffix()
	userID := "u-" + skillSuffix()
	teamID := "t-" + skillSuffix()

	sk := putSkill(t, st, Skill{Name: name, Description: "d", Body: "v1", Scope: identity.UserScope(userID)})
	cleanupSkill(t, db, sk.ID)
	// A second save gives it a history worth preserving.
	if v2 := putSkill(t, st, Skill{Name: name, Description: "d", Body: "v2", Scope: identity.UserScope(userID)}); v2.Version != 2 {
		t.Fatalf("setup: expected v2, got %d", v2.Version)
	}

	moved, err := st.MoveToTeam(ctx, sk.ID, teamID)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if moved.Scope.Scope != identity.ScopeTeam || moved.Scope.TeamID != teamID || moved.Scope.UserID != "" {
		t.Errorf("moved scope = %+v, want team %q", moved.Scope, teamID)
	}
	if moved.ID != sk.ID {
		t.Errorf("move changed the pointer id: %q -> %q", sk.ID, moved.ID)
	}
	if moved.Version != 2 || moved.Body != "v2" {
		t.Errorf("moved content = v%d body %q, want v2", moved.Version, moved.Body)
	}
	if moved.OverridesVersion != 0 || moved.NeedsReview {
		t.Errorf("override bookkeeping not cleared: ov=%d review=%v", moved.OverridesVersion, moved.NeedsReview)
	}

	// History survived the move: both revisions are still listed under the same id.
	vs, err := st.Versions(ctx, sk.ID)
	if err != nil || len(vs) != 2 {
		t.Errorf("versions after move = %v (n=%d), want 2", err, len(vs))
	}

	// It now resolves under the team scope, not the old user scope.
	if _, ok, _ := st.Get(ctx, name, []identity.ScopeRef{identity.UserScope(userID)}); ok {
		t.Errorf("moved skill still resolves in the old user scope")
	}
	if _, ok, _ := st.Get(ctx, name, []identity.ScopeRef{identity.TeamScope(teamID)}); !ok {
		t.Errorf("moved skill does not resolve in the destination team scope")
	}
}

// TestPGStoreMoveToTeamConflict: moving onto an existing same-name team skill is
// refused with ErrConflict (no merge, no overwrite).
func TestPGStoreMoveToTeamConflict(t *testing.T) {
	db := pgSkillDB(t)
	st := NewPGStore(db)
	ctx := context.Background()
	name := "clash-" + skillSuffix()
	userID := "u-" + skillSuffix()
	teamID := "t-" + skillSuffix()

	mine := putSkill(t, st, Skill{Name: name, Body: "mine", Scope: identity.UserScope(userID)})
	cleanupSkill(t, db, mine.ID)
	existing := putSkill(t, st, Skill{Name: name, Body: "theirs", Scope: identity.TeamScope(teamID)})
	cleanupSkill(t, db, existing.ID)

	if _, err := st.MoveToTeam(ctx, mine.ID, teamID); !errors.Is(err, ErrConflict) {
		t.Errorf("move onto existing = %v, want ErrConflict", err)
	}
	// The team skill is untouched and mine is still a user skill.
	if got, err := st.ByID(ctx, existing.ID); err != nil || got.Body != "theirs" {
		t.Errorf("existing team skill disturbed: body=%q err=%v", got.Body, err)
	}
	if got, err := st.ByID(ctx, mine.ID); err != nil || got.Scope.Scope != identity.ScopeUser {
		t.Errorf("source skill moved despite conflict: scope=%v err=%v", got.Scope.Scope, err)
	}
}

// TestPGStoreMoveToTeamOnlyUserScope: only a user-scope skill can move; a team
// or system skill reports ErrNotFound.
func TestPGStoreMoveToTeamOnlyUserScope(t *testing.T) {
	db := pgSkillDB(t)
	st := NewPGStore(db)
	ctx := context.Background()
	teamID := "t-" + skillSuffix()

	team := putSkill(t, st, Skill{Name: "tmv-" + skillSuffix(), Scope: identity.TeamScope(teamID)})
	cleanupSkill(t, db, team.ID)
	if _, err := st.MoveToTeam(ctx, team.ID, "t-other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("move team skill = %v, want ErrNotFound", err)
	}

	sys := putSkill(t, st, Skill{Name: "smv-" + skillSuffix(), Scope: identity.SystemScope()})
	cleanupSkill(t, db, sys.ID)
	if _, err := st.MoveToTeam(ctx, sys.ID, teamID); !errors.Is(err, ErrNotFound) {
		t.Errorf("move system skill = %v, want ErrNotFound", err)
	}

	if _, err := st.MoveToTeam(ctx, "00000000-0000-0000-0000-000000000000", teamID); !errors.Is(err, ErrNotFound) {
		t.Errorf("move unknown id = %v, want ErrNotFound", err)
	}
}

// TestPGStoreOverrideReview: bumping a system skill flags a same-name user
// override whose base version is now stale (D16).
func TestPGStoreOverrideReview(t *testing.T) {
	db := pgSkillDB(t)
	st := NewPGStore(db)
	ctx := context.Background()
	name := "review-" + skillSuffix()
	userID := "u-" + skillSuffix()

	// System v1; the user overrides it based on system v1.
	sys := putSkill(t, st, Skill{Name: name, Body: "sys v1", Scope: identity.SystemScope()})
	cleanupSkill(t, db, sys.ID)
	usr := putSkill(t, st, Skill{Name: name, Body: "user override", Scope: identity.UserScope(userID), OverridesVersion: 1})
	cleanupSkill(t, db, usr.ID)

	// A fresh override based on current upstream is not flagged.
	if usr.NeedsReview {
		t.Fatalf("fresh override should not need review")
	}

	// Update the system skill to v2: the user's override (base v1) is now stale.
	putSkill(t, st, Skill{Name: name, Body: "sys v2", Scope: identity.SystemScope()})

	reviewed, err := st.ByID(ctx, usr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reviewed.NeedsReview {
		t.Errorf("user override should be flagged needs_review after upstream v2")
	}
}

// TestPGStoreVersionHistoryAndRollback: history is append-only and rollback
// restores old content as a NEW version, never rewriting the past.
func TestPGStoreVersionHistoryAndRollback(t *testing.T) {
	db := pgSkillDB(t)
	st := NewPGStore(db)
	ctx := context.Background()
	name := "hist-" + skillSuffix()
	userID := "u-" + skillSuffix()

	v1 := putSkill(t, st, Skill{Name: name, Description: "d1", Body: "body one", Scope: identity.UserScope(userID), Resources: map[string]string{"a.txt": "A"}})
	cleanupSkill(t, db, v1.ID)
	putSkill(t, st, Skill{Name: name, Description: "d2", Body: "body two", Scope: identity.UserScope(userID), Resources: map[string]string{"b.txt": "B"}})
	putSkill(t, st, Skill{Name: name, Description: "d3", Body: "body three", Scope: identity.UserScope(userID)})

	versions, err := st.Versions(ctx, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 {
		t.Fatalf("versions = %d, want 3", len(versions))
	}
	// Newest first.
	if versions[0].Version != 3 || versions[2].Version != 1 {
		t.Errorf("version order = %v, want [3 2 1]", []int{versions[0].Version, versions[1].Version, versions[2].Version})
	}

	// VersionAt returns the historical content, not the current.
	at, err := st.VersionAt(ctx, v1.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if at.Body != "body one" || at.Resources["a.txt"] != "A" {
		t.Errorf("version 1 = body %q resources %v", at.Body, at.Resources)
	}
	if _, err := st.VersionAt(ctx, v1.ID, 99); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing version should be ErrNotFound, got %v", err)
	}

	// Rollback to v1 produces a v4 whose content matches v1.
	rb, err := st.Rollback(ctx, v1.ID, 1, "test")
	if err != nil {
		t.Fatal(err)
	}
	if rb.Version != 4 {
		t.Errorf("rollback version = %d, want 4 (a new revision)", rb.Version)
	}
	if rb.Body != "body one" || rb.Resources["a.txt"] != "A" {
		t.Errorf("rolled-back content = body %q resources %v", rb.Body, rb.Resources)
	}
	// History still holds all four revisions.
	versions, err = st.Versions(ctx, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 4 {
		t.Errorf("after rollback versions = %d, want 4", len(versions))
	}
}

// TestPGStoreDelete: Delete removes the skill and its cascade of versions;
// deleting again is ErrNotFound.
func TestPGStoreDelete(t *testing.T) {
	db := pgSkillDB(t)
	st := NewPGStore(db)
	ctx := context.Background()
	userID := "u-" + skillSuffix()

	sk := putSkill(t, st, Skill{Name: "del-" + skillSuffix(), Scope: identity.UserScope(userID)})
	// No cleanupSkill here: the test itself deletes the row.

	if err := st.Delete(ctx, sk.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.ByID(ctx, sk.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("by id after delete should be ErrNotFound, got %v", err)
	}
	if err := st.Delete(ctx, sk.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete should be ErrNotFound, got %v", err)
	}
	// Versions are gone via cascade.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM skill_versions WHERE skill_id = $1`, sk.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("orphaned versions after delete = %d, want 0", n)
	}
}
