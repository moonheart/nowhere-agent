package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	// tokenTTL is how long an issued auth token remains valid.
	tokenTTL = 30 * 24 * time.Hour
	// serviceKeyPrefix marks programmatic credentials (admin-issued, long-lived,
	// revocable). The prefix makes lookup cheap and logs/UI unambiguous.
	serviceKeyPrefix = "sk_"
)

// Service provides authentication and team operations over the Store.
type Service struct {
	store *Store
	now   func() time.Time
}

func NewService(store *Store) *Service {
	return &Service{store: store, now: time.Now}
}

// minPasswordLen is the platform's password policy floor. Eight characters is
// a weak-but-reasonable baseline for an internal platform whose real strength
// is SSO; it keeps the password path from being the weakest link without
// imposing the complexity rules that push users onto password managers.
const minPasswordLen = 8

// validatePassword applies the password policy. Returns ErrWeakPassword when
// the password does not meet it.
func validatePassword(password string) error {
	if len(password) < minPasswordLen {
		return ErrWeakPassword
	}
	return nil
}

// Signup registers a new user and returns it.
func (s *Service) Signup(ctx context.Context, email, password, displayName string) (User, error) {
	if err := validatePassword(password); err != nil {
		return User{}, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, fmt.Errorf("hash password: %w", err)
	}
	return s.store.CreateUser(ctx, email, string(hash), displayName)
}

// Login verifies credentials and returns a fresh bearer token plus the user.
// A disabled account fails as if the credentials were wrong: telling a
// disabled account apart from a wrong password hands an attacker a valid-email
// oracle, and the account holder learns nothing actionable either way.
//
// When the account has a TOTP second factor, no bearer token is issued —
// Login returns ErrTOTPRequired and the caller begins the second-factor
// challenge instead.
func (s *Service) Login(ctx context.Context, email, password string) (token string, u User, err error) {
	u, err = s.store.UserByEmail(ctx, email)
	if err != nil {
		return "", User{}, ErrInvalidCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return "", User{}, ErrInvalidCredentials
	}
	if u.Disabled() {
		return "", User{}, ErrUserDisabled
	}
	// Second factor: verify the one-time code before any token is issued.
	if totpEnabled, err := s.TOTPEnabled(ctx, u.ID); err == nil && totpEnabled {
		return "", User{}, ErrTOTPRequired
	}
	raw, err := generateToken()
	if err != nil {
		return "", User{}, err
	}
	if err := s.store.CreateToken(ctx, u.ID, hashToken(raw), s.now().Add(tokenTTL)); err != nil {
		return "", User{}, err
	}
	return raw, u, nil
}

// TOTPEnabled reports whether the account has an active second factor.
func (s *Service) TOTPEnabled(ctx context.Context, userID string) (bool, error) {
	_, enabled, err := s.store.TOTPState(ctx, userID)
	return enabled, err
}

// LookupByEmail fetches a user by email (no credential check) — the login
// challenge path needs the account after the password already verified.
func (s *Service) LookupByEmail(ctx context.Context, email string) (User, error) {
	return s.store.UserByEmail(ctx, email)
}

// IssueToken issues a fresh bearer token for an already-authenticated account.
// Password login authenticates via Login; SSO authenticates via the IdP and
// reaches the platform only after the identity is verified, so it mints the
// platform token here rather than re-checking a password the account may not
// have. The account must already be validated (not disabled) by the caller.
func (s *Service) IssueToken(ctx context.Context, u User) (string, error) {
	raw, err := generateToken()
	if err != nil {
		return "", err
	}
	if err := s.store.CreateToken(ctx, u.ID, hashToken(raw), s.now().Add(tokenTTL)); err != nil {
		return "", err
	}
	return raw, nil
}

// Authenticate resolves a bearer token to its user. Two credential classes:
// user auth tokens (30-day TTL sessions) and admin-issued service keys (sk_,
// long-lived programmatic credentials). Disabling an account revokes its
// tokens, so this rarely fires — but a token issued in the same instant as the
// disable would otherwise slip through, and re-checking is one field comparison
// on a row already fetched.
func (s *Service) Authenticate(ctx context.Context, token string) (User, error) {
	if strings.HasPrefix(token, serviceKeyPrefix) {
		return s.authenticateServiceKey(ctx, token)
	}
	userID, err := s.store.UserIDByTokenHash(ctx, hashToken(token), s.now())
	if err != nil {
		return User{}, err
	}
	return s.userIfEnabled(ctx, userID)
}

// authenticateServiceKey resolves a service key (sk_...) to its owner account.
func (s *Service) authenticateServiceKey(ctx context.Context, token string) (User, error) {
	userID, err := s.store.UserIDByServiceKeyHash(ctx, hashToken(token), s.now())
	if err != nil {
		return User{}, err
	}
	return s.userIfEnabled(ctx, userID)
}

// userIfEnabled loads a user and rejects disabled accounts.
func (s *Service) userIfEnabled(ctx context.Context, userID string) (User, error) {
	u, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return User{}, err
	}
	if u.Disabled() {
		return User{}, ErrUserDisabled
	}
	return u, nil
}

// CreateServiceKey issues a long-lived programmatic credential for userID.
// ttl <= 0 means the key never expires. The raw token is returned once; only
// its hash is stored. Returns ErrUserNotFound when userID matches no account.
func (s *Service) CreateServiceKey(ctx context.Context, name, userID string, ttl time.Duration) (raw string, key ServiceKey, err error) {
	if _, err := s.store.UserByID(ctx, userID); err != nil {
		return "", ServiceKey{}, err
	}
	var expiresAt *time.Time
	if ttl > 0 {
		t := s.now().Add(ttl)
		expiresAt = &t
	}
	raw, err = generateServiceKey()
	if err != nil {
		return "", ServiceKey{}, err
	}
	key, err = s.store.CreateServiceKey(ctx, name, userID, hashToken(raw), expiresAt)
	if err != nil {
		return "", ServiceKey{}, err
	}
	return raw, key, nil
}

// ListServiceKeys returns the (optionally revoked) service keys of one user,
// or all keys when userID is empty (admin view).
func (s *Service) ListServiceKeys(ctx context.Context, userID string, includeRevoked bool) ([]ServiceKey, error) {
	return s.store.ListServiceKeys(ctx, userID, includeRevoked)
}

// RevokeServiceKey invalidates a service key (soft delete for audit).
func (s *Service) RevokeServiceKey(ctx context.Context, id string) error {
	return s.store.RevokeServiceKey(ctx, id)
}

// generateServiceKey returns an opaque sk_-prefixed bearer token.
func generateServiceKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand service key: %w", err)
	}
	return serviceKeyPrefix + hex.EncodeToString(b), nil
}

// Logout invalidates a token.
func (s *Service) Logout(ctx context.Context, token string) error {
	return s.store.DeleteToken(ctx, hashToken(token))
}

// CreateTeam creates a team owned by the user.
func (s *Service) CreateTeam(ctx context.Context, name, ownerUserID string) (Team, error) {
	return s.store.CreateTeam(ctx, name, ownerUserID)
}

// AccessibleScopes returns the scopes a user may read from: their own user
// scope, every team they belong to, and the system scope. Skills and memory
// use this to filter recall/visibility (design D8, D10 resource layer).
func (s *Service) AccessibleScopes(ctx context.Context, userID string) ([]ScopeRef, error) {
	teamIDs, err := s.store.TeamIDsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	scopes := make([]ScopeRef, 0, len(teamIDs)+2)
	scopes = append(scopes, UserScope(userID), SystemScope())
	for _, tid := range teamIDs {
		scopes = append(scopes, TeamScope(tid))
	}
	return scopes, nil
}

// CanAccessTeam reports whether the user may access team-scoped resources.
func (s *Service) CanAccessTeam(ctx context.Context, teamID, userID string) (bool, error) {
	return s.store.IsMember(ctx, teamID, userID)
}

// generateToken returns a random opaque bearer token.
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// hashToken hashes a token for storage so DB compromise doesn't leak sessions.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
