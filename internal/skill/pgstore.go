package skill

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"nowhere-agent/internal/identity"
)

// PGStore persists skills in Postgres, replacing the in-memory Store (pure-PG
// decision: every read/write hits the database, no cache layer). It backs the
// same Engine surface the memory Store did — Get/List with user>team>system
// priority — plus the version-history operations the editor needs.
//
// Two tables (migration 000019): `skills` is the mutable per-(name,scope,owner)
// pointer row; `skill_versions` is the immutable revision history. A save
// appends a version and bumps the pointer, so history is never rewritten.
type PGStore struct {
	db *sql.DB
}

// NewPGStore creates a PG-backed skill store.
func NewPGStore(db *sql.DB) *PGStore { return &PGStore{db: db} }

// skillColumns is the `skills` projection, in scan order.
const skillColumns = `id, name, scope, COALESCE(user_id,''), COALESCE(team_id,''), current_version, overrides_version, needs_review, enabled, created_at, updated_at`

// ErrNotFound is returned when a skill or version does not exist.
var ErrNotFound = errors.New("skill not found")

// ErrConflict is returned when a move would collide with an existing skill of
// the same name in the destination scope (identity is (name, scope, owner)).
var ErrConflict = errors.New("skill already exists in destination")

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// Version is one immutable revision of a skill, as the editor's history view
// and rollback render it.
type Version struct {
	SkillID   string
	Version   int
	CreatedBy string
	CreatedAt time.Time
}

// scopeOwner splits a ScopeRef into its nullable owner columns for insert.
func scopeOwner(sc identity.ScopeRef) (userID, teamID any) {
	if sc.Scope == identity.ScopeUser {
		userID = nullIfEmpty(sc.UserID)
	}
	if sc.Scope == identity.ScopeTeam {
		teamID = nullIfEmpty(sc.TeamID)
	}
	return userID, teamID
}

// nullIfEmpty maps an empty scope-owner id to SQL NULL.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// scopeWhere builds an OR clause matching any of the given scope refs (the
// visibility union), mirroring memory.pgport. Placeholders are numbered starting
// at offset+1, so a caller that binds earlier params (e.g. name as $1) passes an
// offset to keep the numbering contiguous.
func scopeWhere(scopes []identity.ScopeRef, offset int) (string, []any) {
	var clauses []string
	var args []any
	for _, s := range scopes {
		switch s.Scope {
		case identity.ScopeUser:
			args = append(args, s.UserID)
			clauses = append(clauses, fmt.Sprintf("(scope = 'user' AND user_id = $%d)", offset+len(args)))
		case identity.ScopeTeam:
			args = append(args, s.TeamID)
			clauses = append(clauses, fmt.Sprintf("(scope = 'team' AND team_id = $%d)", offset+len(args)))
		case identity.ScopeSystem:
			clauses = append(clauses, "scope = 'system'")
		}
	}
	return strings.Join(clauses, " OR "), args
}

// scanSkillRow decodes the skillColumns projection plus the joined current
// version's content into a Skill.
func scanSkillRow(row rowScanner) (Skill, error) {
	var (
		sk           Skill
		resourcesRaw []byte
		scriptsRaw   []byte
	)
	err := row.Scan(
		&sk.ID, &sk.Name, &sk.Scope.Scope, &sk.Scope.UserID, &sk.Scope.TeamID,
		&sk.Version, &sk.OverridesVersion, &sk.NeedsReview, &sk.Enabled, &sk.CreatedAt, &sk.UpdatedAt,
		&sk.Description, &sk.Body, &resourcesRaw, &scriptsRaw,
	)
	if err != nil {
		return Skill{}, err
	}
	if err := json.Unmarshal(resourcesRaw, &sk.Resources); err != nil {
		return Skill{}, fmt.Errorf("decode resources: %w", err)
	}
	if err := json.Unmarshal(scriptsRaw, &sk.Scripts); err != nil {
		return Skill{}, fmt.Errorf("decode scripts: %w", err)
	}
	if sk.Resources == nil {
		sk.Resources = map[string]string{}
	}
	if sk.Scripts == nil {
		sk.Scripts = map[string]string{}
	}
	return sk, nil
}

// currentJoin is the SELECT+JOIN that resolves a skills row to its current
// version's content. The column list mirrors skillColumns, prefixed with `s.`.
const currentJoin = `
	SELECT s.id, s.name, s.scope, COALESCE(s.user_id,''), COALESCE(s.team_id,''),
	       s.current_version, s.overrides_version, s.needs_review, s.enabled, s.created_at, s.updated_at,
	       v.description, v.body, v.resources, v.scripts
	FROM skills s
	JOIN skill_versions v ON v.skill_id = s.id AND v.version = s.current_version`

// Get resolves a skill by name with scope priority user > team > system: the
// caller's scopes are searched in priority order and the first scope holding
// the name wins. Returns the resolved current-version Skill. Only ENABLED
// skills resolve — a disabled skill is invisible to the agent even if it is the
// highest-priority override (it does not shadow a lower-scope enabled skill).
func (s *PGStore) Get(ctx context.Context, name string, scopes []identity.ScopeRef) (Skill, bool, error) {
	for _, scope := range scopes {
		// name is bound as $1, so the scope placeholders start at $2 (offset 1).
		where, args := scopeWhere([]identity.ScopeRef{scope}, 1)
		args = append([]any{name}, args...)
		q := currentJoin + " WHERE s.enabled AND s.name = $1 AND (" + where + ") LIMIT 1"
		sk, err := scanSkillRow(s.db.QueryRowContext(ctx, q, args...))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return Skill{}, false, fmt.Errorf("get skill %q: %w", name, err)
		}
		return sk, true, nil
	}
	return Skill{}, false, nil
}

// List returns L0 metadata for every ENABLED skill visible in the given scopes,
// deduplicated by name with user>team>system priority applied.
func (s *PGStore) List(ctx context.Context, scopes []identity.ScopeRef) ([]L0, error) {
	where, args := scopeWhere(scopes, 0)
	if where == "" {
		return nil, nil
	}
	q := currentJoin + " WHERE s.enabled AND (" + where + ")"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}
	defer rows.Close()

	// Apply priority: walk scopes in order, first scope to claim a name wins.
	rank := map[string]int{} // scope key -> priority index
	for i, sc := range scopes {
		rank[scopeKey(sc)] = i
	}
	best := map[string]L0{}
	bestRank := map[string]int{}
	for rows.Next() {
		sk, err := scanSkillRow(rows)
		if err != nil {
			return nil, err
		}
		r := rank[scopeKey(sk.Scope)]
		if cur, ok := bestRank[sk.Name]; !ok || r < cur {
			best[sk.Name] = L0{Name: sk.Name, Description: sk.Description, Scripts: scriptNames(sk.Scripts)}
			bestRank[sk.Name] = r
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]L0, 0, len(best))
	for _, l := range best {
		out = append(out, l)
	}
	sortL0(out)
	return out, nil
}

// scopeKey identifies a scope for priority ranking (owner-qualified).
func scopeKey(sc identity.ScopeRef) string {
	return string(sc.Scope) + "|" + sc.UserID + "|" + sc.TeamID
}

// sortL0 orders the catalog by name for a stable, prompt-cache-friendly prompt.
func sortL0(l0 []L0) {
	for i := 1; i < len(l0); i++ {
		for j := i; j > 0 && l0[j].Name < l0[j-1].Name; j-- {
			l0[j], l0[j-1] = l0[j-1], l0[j]
		}
	}
}

// Put inserts or updates a skill. A new (name, scope, owner) starts at version
// 1; an update appends a new version, bumps current_version, and flags
// higher-scope overrides for review (D16). Returns the saved skill.
func (s *PGStore) Put(ctx context.Context, sk Skill, createdBy string) (Skill, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Skill{}, fmt.Errorf("begin put skill: %w", err)
	}
	defer tx.Rollback()

	resourcesRaw, err := json.Marshal(orEmpty(sk.Resources))
	if err != nil {
		return Skill{}, fmt.Errorf("encode resources: %w", err)
	}
	scriptsRaw, err := json.Marshal(orEmpty(sk.Scripts))
	if err != nil {
		return Skill{}, fmt.Errorf("encode scripts: %w", err)
	}
	userID, teamID := scopeOwner(sk.Scope)

	// Find the existing pointer row for this identity, if any. The row is
	// locked so concurrent Puts serialize on the version bump instead of
	// racing to insert the same (skill_id, version) row.
	var existingID string
	var existingVersion int
	err = tx.QueryRowContext(ctx, `
		SELECT id, current_version FROM skills
		WHERE name = $1 AND scope = $2
		  AND COALESCE(user_id,'') = COALESCE($3,'')
		  AND COALESCE(team_id,'') = COALESCE($4,'')
		FOR UPDATE`,
		sk.Name, string(sk.Scope.Scope), userID, teamID).Scan(&existingID, &existingVersion)
	notFound := errors.Is(err, sql.ErrNoRows)
	if err != nil && !notFound {
		return Skill{}, fmt.Errorf("lookup skill: %w", err)
	}

	var newVersion int
	if notFound {
		newVersion = 1
		err = tx.QueryRowContext(ctx, `
			INSERT INTO skills (name, scope, user_id, team_id, current_version, overrides_version)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id, enabled, created_at, updated_at`,
			sk.Name, string(sk.Scope.Scope), userID, teamID, newVersion, sk.OverridesVersion).
			Scan(&existingID, &sk.Enabled, &sk.CreatedAt, &sk.UpdatedAt)
		if err != nil {
			return Skill{}, fmt.Errorf("insert skill: %w", err)
		}
	} else {
		newVersion = existingVersion + 1
		err = tx.QueryRowContext(ctx, `
			UPDATE skills SET current_version = $1, updated_at = now()
			WHERE id = $2 RETURNING enabled, created_at, updated_at`,
			newVersion, existingID).Scan(&sk.Enabled, &sk.CreatedAt, &sk.UpdatedAt)
		if err != nil {
			return Skill{}, fmt.Errorf("bump skill: %w", err)
		}
		// Flag higher-scope overrides of this skill for review (D16): any skill
		// with the same name in a scope that overrides this one, whose base
		// version is now stale. Scope rank is compared application-side; the
		// UPDATE matches only same-name rows in the higher scopes.
		higher := higherScopes(sk.Scope.Scope)
		if len(higher) > 0 {
			placeholders := make([]string, len(higher))
			args := []any{sk.Name, existingID}
			for i, sc := range higher {
				args = append(args, sc)
				placeholders[i] = fmt.Sprintf("$%d", len(args))
			}
			args = append(args, newVersion)
			q := fmt.Sprintf(`
				UPDATE skills SET needs_review = true
				WHERE name = $1 AND id <> $2
				  AND scope IN (%s)
				  AND overrides_version < $%d`,
				strings.Join(placeholders, ","), len(args))
			if _, err := tx.ExecContext(ctx, q, args...); err != nil {
				return Skill{}, fmt.Errorf("flag overrides: %w", err)
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO skill_versions (skill_id, version, description, body, resources, scripts, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		existingID, newVersion, sk.Description, sk.Body, resourcesRaw, scriptsRaw, nullIfEmpty(createdBy)); err != nil {
		return Skill{}, fmt.Errorf("insert version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Skill{}, fmt.Errorf("commit put skill: %w", err)
	}

	sk.ID = existingID
	sk.Version = newVersion
	return sk, nil
}

// orEmpty normalizes a nil map to an empty one so JSONB encodes '{}', not 'null'.
func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// higherScopes returns the scopes that override the given one (user > team >
// system), for flagging downstream overrides on update (D16).
func higherScopes(s identity.Scope) []string {
	switch s {
	case identity.ScopeSystem:
		return []string{string(identity.ScopeTeam), string(identity.ScopeUser)}
	case identity.ScopeTeam:
		return []string{string(identity.ScopeUser)}
	default: // user has nothing above it
		return nil
	}
}

// ByID fetches a skill's current version by its pointer id.
func (s *PGStore) ByID(ctx context.Context, id string) (Skill, error) {
	sk, err := scanSkillRow(s.db.QueryRowContext(ctx, currentJoin+" WHERE s.id = $1", id))
	if errors.Is(err, sql.ErrNoRows) || identity.IsMalformedID(err) {
		return Skill{}, ErrNotFound
	}
	if err != nil {
		return Skill{}, fmt.Errorf("skill by id: %w", err)
	}
	return sk, nil
}

// ListByScope returns the current versions of every skill owned by one scope,
// for the editor's per-scope listing.
func (s *PGStore) ListByScope(ctx context.Context, scope identity.ScopeRef) ([]Skill, error) {
	where, args := scopeWhere([]identity.ScopeRef{scope}, 0)
	if where == "" {
		return nil, nil
	}
	q := currentJoin + " WHERE (" + where + ") ORDER BY s.name"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list skills by scope: %w", err)
	}
	defer rows.Close()
	var out []Skill
	for rows.Next() {
		sk, err := scanSkillRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// Versions lists a skill's revision history, newest first, without content.
func (s *PGStore) Versions(ctx context.Context, skillID string) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT skill_id, version, COALESCE(created_by,''), created_at
		FROM skill_versions WHERE skill_id = $1 ORDER BY version DESC`, skillID)
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.SkillID, &v.Version, &v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// VersionAt fetches one historical revision's full content.
func (s *PGStore) VersionAt(ctx context.Context, skillID string, version int) (Skill, error) {
	var (
		sk           Skill
		resourcesRaw []byte
		scriptsRaw   []byte
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT s.id, s.name, s.scope, COALESCE(s.user_id,''), COALESCE(s.team_id,''),
		       v.version, s.overrides_version, s.needs_review, s.enabled, s.created_at, s.updated_at,
		       v.description, v.body, v.resources, v.scripts
		FROM skills s
		JOIN skill_versions v ON v.skill_id = s.id AND v.version = $2
		WHERE s.id = $1`, skillID, version).
		Scan(&sk.ID, &sk.Name, &sk.Scope.Scope, &sk.Scope.UserID, &sk.Scope.TeamID,
			&sk.Version, &sk.OverridesVersion, &sk.NeedsReview, &sk.Enabled, &sk.CreatedAt, &sk.UpdatedAt,
			&sk.Description, &sk.Body, &resourcesRaw, &scriptsRaw)
	if errors.Is(err, sql.ErrNoRows) || identity.IsMalformedID(err) {
		return Skill{}, ErrNotFound
	}
	if err != nil {
		return Skill{}, fmt.Errorf("version at: %w", err)
	}
	if err := json.Unmarshal(resourcesRaw, &sk.Resources); err != nil {
		return Skill{}, fmt.Errorf("decode resources: %w", err)
	}
	if err := json.Unmarshal(scriptsRaw, &sk.Scripts); err != nil {
		return Skill{}, fmt.Errorf("decode scripts: %w", err)
	}
	return sk, nil
}

// Rollback saves an old version's content as a NEW current version (history is
// never rewritten). Returns the new current skill.
func (s *PGStore) Rollback(ctx context.Context, skillID string, version int, createdBy string) (Skill, error) {
	old, err := s.VersionAt(ctx, skillID, version)
	if err != nil {
		return Skill{}, err
	}
	return s.Put(ctx, Skill{
		Name:        old.Name,
		Scope:       old.Scope,
		Description: old.Description,
		Body:        old.Body,
		Resources:   old.Resources,
		Scripts:     old.Scripts,
	}, createdBy)
}

// Delete removes a skill and (via ON DELETE CASCADE) its version history.
func (s *PGStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM skills WHERE id = $1`, id)
	if err != nil {
		if identity.IsMalformedID(err) {
			return ErrNotFound
		}
		return fmt.Errorf("delete skill: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetEnabled flips the agent-resolution gate without touching content or
// version history: a disabled skill drops out of Get/List but stays editable in
// the management surface. Content and history are unchanged, so re-enabling
// restores exactly what the agent saw before.
func (s *PGStore) SetEnabled(ctx context.Context, id string, enabled bool) (Skill, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE skills SET enabled = $1, updated_at = now() WHERE id = $2`, enabled, id)
	if err != nil {
		if identity.IsMalformedID(err) {
			return Skill{}, ErrNotFound
		}
		return Skill{}, fmt.Errorf("set skill enabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Skill{}, ErrNotFound
	}
	return s.ByID(ctx, id)
}

// MoveToTeam relocates a user-scope skill into a team, preserving its whole
// version history (only the pointer row's scope/owner columns change — no new
// version is written). The override bookkeeping is cleared: the skill's old
// overrides_version/needs_review refer to its user-scope lineage, which does
// not carry into the team scope.
//
// A move that collides with an existing same-name skill in the destination
// team is refused with ErrConflict rather than merging or overwriting. Only a
// user-scope skill can move; any other scope returns ErrNotFound (the caller's
// scope check already reported a non-user skill as not-in-scope).
func (s *PGStore) MoveToTeam(ctx context.Context, id, teamID string) (Skill, error) {
	// Refuse the move up front when the destination team already owns a skill of
	// this name, so the UPDATE below never has to attempt a duplicate identity
	// (the unique index would reject it as a 23505; naming it ErrConflict keeps
	// the conflict a clean client error instead of a raw constraint violation).
	var name string
	err := s.db.QueryRowContext(ctx,
		`SELECT name FROM skills WHERE id = $1 AND scope = 'user'`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) || identity.IsMalformedID(err) {
		return Skill{}, ErrNotFound
	}
	if err != nil {
		return Skill{}, fmt.Errorf("lookup skill to move: %w", err)
	}
	var clash bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM skills
			WHERE name = $1 AND scope = 'team' AND COALESCE(team_id,'') = $2)`,
		name, teamID).Scan(&clash); err != nil {
		return Skill{}, fmt.Errorf("check move destination: %w", err)
	}
	if clash {
		return Skill{}, ErrConflict
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE skills
		SET scope = 'team', team_id = $1, user_id = NULL,
		    overrides_version = 0, needs_review = false, updated_at = now()
		WHERE id = $2 AND scope = 'user'`, teamID, id)
	if err != nil {
		return Skill{}, fmt.Errorf("move skill to team: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Skill{}, ErrNotFound
	}
	return s.ByID(ctx, id)
}
