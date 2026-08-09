package upload

import (
	"context"
	"database/sql"
	"errors"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PGStore is the Postgres-backed upload metadata store.
type PGStore struct {
	db *sql.DB
}

// NewPGStore creates a Postgres-backed Store.
func NewPGStore(db *sql.DB) *PGStore { return &PGStore{db: db} }

var _ Store = (*PGStore)(nil)

const cols = `id::text, user_id::text, filename, size, media_type, created_at`

func scanUpload(row interface{ Scan(...any) error }) (Upload, error) {
	var u Upload
	var createdAt time.Time
	err := row.Scan(&u.ID, &u.UserID, &u.Filename, &u.Size, &u.MediaType, &createdAt)
	u.CreatedAt = createdAt
	return u, err
}

func (s *PGStore) Create(ctx context.Context, u Upload) (Upload, error) {
	return scanUpload(s.db.QueryRowContext(ctx, `
		INSERT INTO uploads (id, user_id, filename, size, media_type)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+cols, u.ID, u.UserID, u.Filename, u.Size, u.MediaType))
}

func (s *PGStore) ListByUser(ctx context.Context, userID string) ([]Upload, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+cols+` FROM uploads
		WHERE user_id = $1
		ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Upload
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *PGStore) Get(ctx context.Context, id string) (Upload, error) {
	u, err := scanUpload(s.db.QueryRowContext(ctx, `
		SELECT `+cols+` FROM uploads WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Upload{}, ErrNotFound
	}
	return u, err
}

func (s *PGStore) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM uploads WHERE id = $1`, id)
	return err
}

// ReferencedByMessage scans message content for the upload reference. Message
// content is a JSON document, not a normalized relation, so the check is a text
// scan — a deletion-time cost, not a hot path (see design: reference protection).
func (s *PGStore) ReferencedByMessage(ctx context.Context, id string) (bool, error) {
	pattern := "%uploads/" + id + ".webp%"
	var ref bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM messages WHERE content::text LIKE $1
		)`, pattern).Scan(&ref)
	return ref, err
}
