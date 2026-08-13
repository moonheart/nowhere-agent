package identity

import (
	"context"
	"testing"
	"time"
)

// TestSweepExpired pins the credential reaper: rows whose expiry (or, for OTPs,
// consumption) passed the cutoff are deleted; still-valid rows and revoked
// service keys survive. The sweep is inherently a conditional delete, so it
// runs in a throwaway database (freshDB) — never against the shared dev DB,
// where it could eat another test's leftovers.
func TestSweepExpired(t *testing.T) {
	db := freshDB(t)
	s := NewStore(db)
	ctx := context.Background()
	u := mkUser(t, db)
	suffix := randSuffix()
	cutoff := time.Now().UTC().Add(-24 * time.Hour)

	// auth_tokens: one expired (past the grace), one still valid.
	var expiredTokenID string
	if err := db.QueryRow(`INSERT INTO auth_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3) RETURNING id`, u.ID, "tok-exp-"+suffix, cutoff.Add(-time.Hour)).Scan(&expiredTokenID); err != nil {
		t.Fatalf("insert expired token: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM auth_tokens WHERE id = $1`, expiredTokenID) })
	liveToken := "tok-live-" + suffix
	var liveTokenID string
	if err := db.QueryRow(`INSERT INTO auth_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3) RETURNING id`, u.ID, liveToken, time.Now().UTC().Add(time.Hour)).Scan(&liveTokenID); err != nil {
		t.Fatalf("insert live token: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM auth_tokens WHERE id = $1`, liveTokenID) })

	// phone_otps: one expired, one consumed past the grace, one fresh.
	insertOTP := func(expires time.Time, consumed *time.Time) string {
		var id string
		err := db.QueryRow(`INSERT INTO phone_otps (phone, code_hash, expires_at, consumed_at)
			VALUES ($1, $2, $3, $4) RETURNING id`,
			"139"+suffix, "h-"+suffix, expires, consumed).Scan(&id)
		if err != nil {
			t.Fatalf("insert otp: %v", err)
		}
		t.Cleanup(func() { db.Exec(`DELETE FROM phone_otps WHERE id = $1`, id) })
		return id
	}
	insertOTP(cutoff.Add(-time.Hour), nil)
	consumed := cutoff.Add(-2 * time.Hour)
	insertOTP(time.Now().UTC().Add(time.Hour), &consumed)
	freshOTP := insertOTP(time.Now().UTC().Add(time.Hour), nil)

	// service_keys: one expired, one revoked (must survive), one live.
	insertKey := func(hash string, expires *time.Time, revoked *time.Time) string {
		var id string
		err := db.QueryRow(`INSERT INTO service_keys (name, token_hash, user_id, expires_at, revoked_at)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			"k-"+suffix, hash, u.ID, expires, revoked).Scan(&id)
		if err != nil {
			t.Fatalf("insert service key: %v", err)
		}
		t.Cleanup(func() { db.Exec(`DELETE FROM service_keys WHERE id = $1`, id) })
		return id
	}
	expPast := cutoff.Add(-time.Hour)
	insertKey("kh-exp-"+suffix, &expPast, nil)
	revPast := cutoff.Add(-2 * time.Hour)
	revokedKeyID := insertKey("kh-rev-"+suffix, &expPast, &revPast)
	liveKey := insertKey("kh-live-"+suffix, nil, nil)

	removed, err := s.SweepExpired(ctx, cutoff)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 4 {
		t.Errorf("removed = %d, want 4 (one token, two otps, one key)", removed)
	}

	// Survivors still resolve / exist.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM auth_tokens WHERE id = $1`, liveTokenID).Scan(&n); err != nil || n != 1 {
		t.Errorf("live token gone (n=%d err=%v), want kept", n, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM phone_otps WHERE id = $1`, freshOTP).Scan(&n); err != nil || n != 1 {
		t.Errorf("fresh otp gone (n=%d err=%v), want kept", n, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM service_keys WHERE id = $1`, liveKey).Scan(&n); err != nil || n != 1 {
		t.Errorf("live service key gone (n=%d err=%v), want kept", n, err)
	}
	// Revoked keys are deliberately NOT purged (admin console revoked list).
	var kept string
	if err := db.QueryRow(`SELECT id FROM service_keys WHERE id = $1`, revokedKeyID).Scan(&kept); err != nil {
		t.Errorf("revoked service key gone (err=%v), want kept", err)
	}
}
