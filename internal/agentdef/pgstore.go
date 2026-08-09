package agentdef

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

// PGStore persists agent definitions in Postgres. It mirrors the skill store's
// two-table versioning model (migration 000027): `agent_defs` is the mutable
// per-(name,scope,owner) pointer row; `agent_def_versions` is the immutable
// revision history. A save appends a version and bumps the pointer, so history
// is never rewritten. The built-in definitions stay in code (Store); PGStore
// holds only authored definitions.
type PGStore struct {
	db *sql.DB
}

// NewPGStore creates a PG-backed agent definition store.
func NewPGStore(db *sql.DB) *PGStore { return &PGStore{db: db} }

// ErrNotFound is returned when a definition does not exist at the given scope.
var ErrNotFound = errors.New("agent definition not found")

// StoredDef is a persisted definition: the parsed AgentDef plus its pointer
// metadata and the raw source document (retained for the editor).
type StoredDef struct {
	AgentDef
	ID          string
	Version     int
	RawDocument string
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// nullIfEmpty maps an empty scope-owner id to SQL NULL.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
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

// scopeWhere builds an OR clause matching any of the given scope refs,
// mirroring skill.pgstore. Placeholders are numbered starting at offset+1.
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

// currentJoin resolves an agent_defs row to its current version's content.
const currentJoin = `
	SELECT s.id, s.name, s.scope, COALESCE(s.user_id,''), COALESCE(s.team_id,''),
	       s.current_version, s.created_at, s.updated_at,
	       v.when_to_use, v.tools, v.disallowed_tools, v.skills, v.model, v.max_turns, v.system,
	       v.raw_document, COALESCE(v.created_by,'')
	FROM agent_defs s
	JOIN agent_def_versions v ON v.def_id = s.id AND v.version = s.current_version`

// scanDefRow decodes the currentJoin projection into a StoredDef.
func scanDefRow(row rowScanner) (StoredDef, error) {
	var (
		sd                      StoredDef
		toolsRaw, disRaw, skRaw []byte
	)
	err := row.Scan(
		&sd.ID, &sd.Name, &sd.Scope.Scope, &sd.Scope.UserID, &sd.Scope.TeamID,
		&sd.Version, &sd.CreatedAt, &sd.UpdatedAt,
		&sd.WhenToUse, &toolsRaw, &disRaw, &skRaw, &sd.Model, &sd.MaxTurns, &sd.System,
		&sd.RawDocument, &sd.CreatedBy,
	)
	if err != nil {
		return StoredDef{}, err
	}
	if err := json.Unmarshal(toolsRaw, &sd.Tools); err != nil {
		return StoredDef{}, fmt.Errorf("decode tools: %w", err)
	}
	if err := json.Unmarshal(disRaw, &sd.DisallowedTools); err != nil {
		return StoredDef{}, fmt.Errorf("decode disallowed_tools: %w", err)
	}
	if err := json.Unmarshal(skRaw, &sd.Skills); err != nil {
		return StoredDef{}, fmt.Errorf("decode skills: %w", err)
	}
	return sd, nil
}

func queryDefs(ctx context.Context, db *sql.DB, q string, args ...any) ([]StoredDef, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredDef
	for rows.Next() {
		sd, err := scanDefRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sd)
	}
	return out, rows.Err()
}

// ListVisible returns the current version of every authored definition visible
// in any of the given scopes. Scope priority is NOT applied here — callers
// (the Resolver) overlay them in priority order.
func (s *PGStore) ListVisible(ctx context.Context, scopes []identity.ScopeRef) ([]StoredDef, error) {
	where, args := scopeWhere(scopes, 0)
	if where == "" {
		return nil, nil
	}
	defs, err := queryDefs(ctx, s.db, currentJoin+" WHERE ("+where+")", args...)
	if err != nil {
		return nil, fmt.Errorf("list visible agent defs: %w", err)
	}
	return defs, nil
}

// ListByScope returns the current versions of every definition owned by one
// scope, for the management API's per-tier listing.
func (s *PGStore) ListByScope(ctx context.Context, scope identity.ScopeRef) ([]StoredDef, error) {
	where, args := scopeWhere([]identity.ScopeRef{scope}, 0)
	if where == "" {
		return nil, nil
	}
	defs, err := queryDefs(ctx, s.db, currentJoin+" WHERE ("+where+") ORDER BY s.name", args...)
	if err != nil {
		return nil, fmt.Errorf("list agent defs by scope: %w", err)
	}
	return defs, nil
}

// Get returns the current version of one definition at one exact scope.
func (s *PGStore) Get(ctx context.Context, name string, scope identity.ScopeRef) (StoredDef, error) {
	where, args := scopeWhere([]identity.ScopeRef{scope}, 1)
	args = append([]any{name}, args...)
	sd, err := scanDefRow(s.db.QueryRowContext(ctx,
		currentJoin+" WHERE s.name = $1 AND ("+where+") LIMIT 1", args...))
	if errors.Is(err, sql.ErrNoRows) {
		return StoredDef{}, ErrNotFound
	}
	if err != nil {
		return StoredDef{}, fmt.Errorf("get agent def %q: %w", name, err)
	}
	return sd, nil
}

// Put validates and saves a definition document at the given scope: parse +
// validate the markdown, then in one transaction insert-or-bump the pointer
// row and append the new immutable version. createdBy records the author.
func (s *PGStore) Put(ctx context.Context, document string, scope identity.ScopeRef, createdBy string) (StoredDef, error) {
	d, err := Validate(document)
	if err != nil {
		return StoredDef{}, err
	}
	d.Scope = scope
	userID, teamID := scopeOwner(scope)

	toolsRaw, _ := json.Marshal(listOrEmpty(d.Tools))
	disRaw, _ := json.Marshal(listOrEmpty(d.DisallowedTools))
	skillsRaw, _ := json.Marshal(listOrEmpty(d.Skills))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StoredDef{}, fmt.Errorf("begin put agent def: %w", err)
	}
	defer tx.Rollback()

	var defID string
	var version int
	err = tx.QueryRowContext(ctx, `
		SELECT id, current_version FROM agent_defs
		WHERE name = $1 AND scope = $2
		  AND COALESCE(user_id,'') = COALESCE($3,'')
		  AND COALESCE(team_id,'') = COALESCE($4,'')
		FOR UPDATE`,
		d.Name, string(scope.Scope), userID, teamID).Scan(&defID, &version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		version = 1
		if err = tx.QueryRowContext(ctx, `
			INSERT INTO agent_defs (name, scope, user_id, team_id, current_version)
			VALUES ($1, $2, $3, $4, 1)
			RETURNING id`,
			d.Name, string(scope.Scope), userID, teamID).Scan(&defID); err != nil {
			return StoredDef{}, fmt.Errorf("insert agent def: %w", err)
		}
	case err != nil:
		return StoredDef{}, fmt.Errorf("lock agent def: %w", err)
	default:
		version++
		if _, err = tx.ExecContext(ctx, `
			UPDATE agent_defs SET current_version = $1, updated_at = now() WHERE id = $2`,
			version, defID); err != nil {
			return StoredDef{}, fmt.Errorf("bump agent def: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agent_def_versions
			(def_id, version, when_to_use, tools, disallowed_tools, skills, model, max_turns, system, raw_document, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		defID, version, d.WhenToUse, toolsRaw, disRaw, skillsRaw,
		d.Model, d.MaxTurns, d.System, document, nullIfEmpty(createdBy)); err != nil {
		return StoredDef{}, fmt.Errorf("insert agent def version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return StoredDef{}, fmt.Errorf("commit put agent def: %w", err)
	}
	return s.Get(ctx, d.Name, scope)
}

// Delete removes one definition (and, by cascade, its versions) at one exact
// scope. Deleting a name that only exists as a built-in returns ErrNotFound —
// built-ins are not stored here and cannot be deleted.
func (s *PGStore) Delete(ctx context.Context, name string, scope identity.ScopeRef) error {
	userID, teamID := scopeOwner(scope)
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM agent_defs
		WHERE name = $1 AND scope = $2
		  AND COALESCE(user_id,'') = COALESCE($3,'')
		  AND COALESCE(team_id,'') = COALESCE($4,'')`,
		name, string(scope.Scope), userID, teamID)
	if err != nil {
		return fmt.Errorf("delete agent def %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete agent def %q: %w", name, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// listOrEmpty normalizes a nil list to an empty one so JSONB encodes '[]'.
func listOrEmpty(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}
