package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"nowhere-agent/internal/identity"
)

// PGPort is a Postgres-backed Port. Recall does cosine similarity over the
// query embedding when provided and pgvector is installed, falling back to
// Postgres full-text keyword matching otherwise. Embeddings are sent as
// "[a,b,c]" literals, which both the pgvector `vector` type and a `jsonb`
// array accept, so no vector driver dependency is required.
type PGPort struct {
	db *sql.DB
}

// NewPGPort creates a Postgres-backed Port.
func NewPGPort(db *sql.DB) *PGPort { return &PGPort{db: db} }

// Store persists a new memory, assigning ID and timestamps.
func (p *PGPort) Store(ctx context.Context, m Memory) (Memory, error) {
	if m.ID == "" {
		m.ID = uuid.NewString()
	}
	emb, err := encodeEmbedding(m.Embedding)
	if err != nil {
		return Memory{}, err
	}
	err = p.db.QueryRowContext(ctx, `
		INSERT INTO memories (id, scope, user_id, team_id, kind, content, embedding, deprecated)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING created_at, updated_at`,
		m.ID, string(m.Scope.Scope), nullStr(m.Scope.UserID), nullStr(m.Scope.TeamID),
		string(m.Kind), m.Content, emb, m.Deprecated,
	).Scan(&m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return Memory{}, fmt.Errorf("store memory: %w", err)
	}
	return m, nil
}

// Recall returns non-deprecated memories in scope, ranked by relevance. With a
// non-empty query it ranks by full-text keyword match; an empty query returns
// the most recent memories in scope. (Vector recall is provided by
// RecallVector once an embedding generator is wired — the agent loop has no
// embedder today, so the online read path is keyword-based.)
func (p *PGPort) Recall(ctx context.Context, query string, scopes []identity.ScopeRef, limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 10
	}
	where, args := scopeWhere(scopes)
	if where == "" {
		return nil, nil
	}

	var orderBy string
	if strings.TrimSpace(query) != "" {
		args = append(args, query)
		orderBy = fmt.Sprintf("ts_rank(to_tsvector('simple', content), plainto_tsquery('simple', $%d)) DESC", len(args))
	} else {
		orderBy = "created_at DESC"
	}

	args = append(args, limit)
	q := fmt.Sprintf(`
		SELECT id, scope, user_id, team_id, kind, content, embedding, deprecated, created_at, updated_at
		FROM memories
		WHERE NOT deprecated AND (%s)
		ORDER BY %s
		LIMIT $%d`, where, orderBy, len(args))

	return p.query(ctx, q, args...)
}

// RecallSince returns non-deprecated in-scope memories created after `since`,
// optionally filtered to `kinds`, ranked by relevance (or recency when the
// query is empty). A zero `since` disables the time lower bound. Kind filters
// expand inline (the project uses pgx stdlib, not lib/pq, so no array param).
func (p *PGPort) RecallSince(ctx context.Context, since time.Time, query string, scopes []identity.ScopeRef, kinds []Kind, limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 10
	}
	where, args := scopeWhere(scopes)
	if where == "" {
		return nil, nil
	}

	if len(kinds) > 0 {
		ph := make([]string, 0, len(kinds))
		for _, k := range kinds {
			args = append(args, string(k))
			ph = append(ph, fmt.Sprintf("$%d", len(args)))
		}
		where = fmt.Sprintf("(%s) AND kind IN (%s)", where, strings.Join(ph, ","))
	}
	if !since.IsZero() {
		args = append(args, since)
		where = fmt.Sprintf("%s AND created_at > $%d", where, len(args))
	}

	var orderBy string
	if strings.TrimSpace(query) != "" {
		args = append(args, query)
		orderBy = fmt.Sprintf("ts_rank(to_tsvector('simple', content), plainto_tsquery('simple', $%d)) DESC", len(args))
	} else {
		orderBy = "created_at DESC"
	}

	args = append(args, limit)
	q := fmt.Sprintf(`
		SELECT id, scope, user_id, team_id, kind, content, embedding, deprecated, created_at, updated_at
		FROM memories
		WHERE NOT deprecated AND (%s)
		ORDER BY %s
		LIMIT $%d`, where, orderBy, len(args))

	return p.query(ctx, q, args...)
}

// RecallVector ranks in-scope memories by cosine distance to a query embedding
// (requires pgvector). Used by consumers that generate their own embeddings —
// e.g. the dreaming worker with an embedding provider configured.
func (p *PGPort) RecallVector(ctx context.Context, queryEmbedding []float32, scopes []identity.ScopeRef, limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 10
	}
	if len(queryEmbedding) == 0 {
		return nil, fmt.Errorf("query embedding required for vector recall")
	}
	where, args := scopeWhere(scopes)
	if where == "" {
		return nil, nil
	}
	emb, err := encodeEmbedding(queryEmbedding)
	if err != nil {
		return nil, err
	}
	args = append(args, emb)
	orderBy := fmt.Sprintf("embedding <=> $%d", len(args))
	args = append(args, limit)
	q := fmt.Sprintf(`
		SELECT id, scope, user_id, team_id, kind, content, embedding, deprecated, created_at, updated_at
		FROM memories
		WHERE NOT deprecated AND (%s)
		ORDER BY %s
		LIMIT $%d`, where, orderBy, len(args))
	return p.query(ctx, q, args...)
}

// Deprecate marks a memory superseded (excluded from recall).
func (p *PGPort) Deprecate(ctx context.Context, id string) error {
	_, err := p.db.ExecContext(ctx, `
		UPDATE memories SET deprecated = true, updated_at = now() WHERE id = $1`, id)
	if identity.IsMalformedID(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deprecate memory: %w", err)
	}
	return nil
}

// Update rewrites a memory's content in place. It clears the embedding, which
// was derived from the old text and would otherwise rank the memory by content
// it no longer holds. A malformed id names nothing, so it lands on ErrNotFound
// like any other miss rather than surfacing a database fault.
func (p *PGPort) Update(ctx context.Context, id, content string) error {
	res, err := p.db.ExecContext(ctx, `
		UPDATE memories
		SET content = $2, embedding = NULL, updated_at = now()
		WHERE id = $1`, id, content)
	if identity.IsMalformedID(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("update memory: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update memory: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PurgeDeprecated deletes memories deprecated before the cutoff.
//
// It dates a deprecation by `updated_at`: Deprecate stamps it, and nothing
// touches a memory after it is deprecated (consolidation only ever sees live
// ones), so on a deprecated row that column IS the deprecation time. A separate
// deprecated_at column would carry the same value at the cost of a migration.
func (p *PGPort) PurgeDeprecated(ctx context.Context, before time.Time) (int, error) {
	res, err := p.db.ExecContext(ctx, `
		DELETE FROM memories WHERE deprecated AND updated_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("purge deprecated memories: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purge deprecated memories: %w", err)
	}
	return int(n), nil
}

// Forget permanently deletes a memory (GDPR erasure).
func (p *PGPort) Forget(ctx context.Context, id string) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM memories WHERE id = $1`, id)
	if identity.IsMalformedID(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("forget memory: %w", err)
	}
	return nil
}

// ListByScope returns all memories (incl. deprecated) in a scope.
func (p *PGPort) ListByScope(ctx context.Context, scope identity.ScopeRef) ([]Memory, error) {
	where, args := scopeWhere([]identity.ScopeRef{scope})
	q := fmt.Sprintf(`
		SELECT id, scope, user_id, team_id, kind, content, embedding, deprecated, created_at, updated_at
		FROM memories
		WHERE %s
		ORDER BY created_at`, where)
	return p.query(ctx, q, args...)
}

// GetByID returns one memory, or ErrNotFound. A malformed id names nothing, so
// query resolves it to an empty result and it lands on ErrNotFound too — ids
// arrive from URL path segments, and a typo is a miss, not a server fault.
func (p *PGPort) GetByID(ctx context.Context, id string) (Memory, error) {
	out, err := p.query(ctx, `
		SELECT id, scope, user_id, team_id, kind, content, embedding, deprecated, created_at, updated_at
		FROM memories
		WHERE id = $1`, id)
	if err != nil {
		return Memory{}, err
	}
	if len(out) == 0 {
		return Memory{}, ErrNotFound
	}
	return out[0], nil
}

func (p *PGPort) query(ctx context.Context, q string, args ...any) ([]Memory, error) {
	rows, err := p.db.QueryContext(ctx, q, args...)
	if identity.IsMalformedID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query memories: %w", err)
	}
	defer rows.Close()

	var out []Memory
	for rows.Next() {
		var m Memory
		var scope string
		var userID, teamID sql.NullString
		var emb sql.NullString
		if err := rows.Scan(&m.ID, &scope, &userID, &teamID, &m.Kind, &m.Content, &emb, &m.Deprecated, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}
		m.Scope = identity.ScopeRef{Scope: identity.Scope(scope), UserID: userID.String, TeamID: teamID.String}
		m.Embedding = decodeEmbedding(emb)
		out = append(out, m)
	}
	return out, rows.Err()
}

// nullStr wraps an optional scope-owner id as NULL when empty.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// scopeWhere builds an OR clause matching any of the given scope refs.
func scopeWhere(scopes []identity.ScopeRef) (string, []any) {
	var clauses []string
	var args []any
	for _, s := range scopes {
		switch s.Scope {
		case identity.ScopeUser:
			args = append(args, s.UserID)
			clauses = append(clauses, fmt.Sprintf("(scope = 'user' AND user_id = $%d)", len(args)))
		case identity.ScopeTeam:
			args = append(args, s.TeamID)
			clauses = append(clauses, fmt.Sprintf("(scope = 'team' AND team_id = $%d)", len(args)))
		case identity.ScopeSystem:
			clauses = append(clauses, "scope = 'system'")
		}
	}
	return strings.Join(clauses, " OR "), args
}

// encodeEmbedding renders a float slice as "[a,b,c]", accepted by both the
// pgvector vector type and a jsonb array. Nil/empty → NULL.
func encodeEmbedding(v []float32) (sql.NullString, error) {
	if len(v) == 0 {
		return sql.NullString{}, nil
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return sql.NullString{String: b.String(), Valid: true}, nil
}

// decodeEmbedding parses a stored embedding back into a float slice. It
// tolerates both a JSON array (jsonb column) and pgvector's "[a,b,c]" text.
func decodeEmbedding(s sql.NullString) []float32 {
	if !s.Valid || s.String == "" {
		return nil
	}
	var out []float32
	if err := json.Unmarshal([]byte(s.String), &out); err == nil {
		return out
	}
	// Fall back to splitting the "[...]" literal.
	trimmed := strings.Trim(s.String, "[]")
	if trimmed == "" {
		return nil
	}
	for _, part := range strings.Split(trimmed, ",") {
		f, err := strconv.ParseFloat(strings.TrimSpace(part), 32)
		if err != nil {
			return nil
		}
		out = append(out, float32(f))
	}
	return out
}
