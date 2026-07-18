package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

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
	if err != nil {
		return fmt.Errorf("deprecate memory: %w", err)
	}
	return nil
}

// Forget permanently deletes a memory (GDPR erasure).
func (p *PGPort) Forget(ctx context.Context, id string) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM memories WHERE id = $1`, id)
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

func (p *PGPort) query(ctx context.Context, q string, args ...any) ([]Memory, error) {
	rows, err := p.db.QueryContext(ctx, q, args...)
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
