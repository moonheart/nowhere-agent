package identity

import (
	"context"
	"fmt"
	"time"
)

// SweepExpired deletes credentials that can never authenticate again and are
// past the retention grace, returning how many rows were removed:
//
//   - auth_tokens whose expires_at passed more than a day before cutoff — an
//     expired session token is rejected by UserIDByTokenHash, so its row is
//     pure garbage; the 1-day grace keeps a just-expired token's audit trail;
//   - phone_otps past expiry or consumed more than a day before cutoff — a
//     consumed code is single-use, its row exists only for audit;
//   - service_keys that expired more than a day before cutoff (revoked keys
//     are deliberately kept: the admin console's revoked=1 list shows them).
//
// One statement per table (a mid-sweep failure leaves the remaining tables
// untouched for the next hourly pass). Best-effort by design — the hourly
// sweep in cmd/server logs and keeps going.
func (s *Store) SweepExpired(ctx context.Context, cutoff time.Time) (int64, error) {
	var total int64
	for _, q := range []string{
		`DELETE FROM auth_tokens WHERE expires_at < $1`,
		`DELETE FROM phone_otps WHERE expires_at < $1 OR (consumed_at IS NOT NULL AND consumed_at < $1)`,
		`DELETE FROM service_keys WHERE expires_at IS NOT NULL AND expires_at < $1 AND revoked_at IS NULL`,
	} {
		res, err := s.db.ExecContext(ctx, q, cutoff)
		if err != nil {
			return total, fmt.Errorf("sweep expired credentials: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
