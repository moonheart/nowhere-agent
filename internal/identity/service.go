package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	// tokenTTL is how long an issued auth token remains valid.
	tokenTTL = 30 * 24 * time.Hour
)

// Service provides authentication and team operations over the Store.
type Service struct {
	store *Store
	now   func() time.Time
}

func NewService(store *Store) *Service {
	return &Service{store: store, now: time.Now}
}

// Signup registers a new user and returns it.
func (s *Service) Signup(ctx context.Context, email, password, displayName string) (User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, fmt.Errorf("hash password: %w", err)
	}
	return s.store.CreateUser(ctx, email, string(hash), displayName)
}

// Login verifies credentials and returns a fresh bearer token plus the user.
func (s *Service) Login(ctx context.Context, email, password string) (token string, u User, err error) {
	u, err = s.store.UserByEmail(ctx, email)
	if err != nil {
		return "", User{}, ErrInvalidCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return "", User{}, ErrInvalidCredentials
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

// Authenticate resolves a bearer token to its user.
func (s *Service) Authenticate(ctx context.Context, token string) (User, error) {
	userID, err := s.store.UserIDByTokenHash(ctx, hashToken(token), s.now())
	if err != nil {
		return User{}, err
	}
	return s.store.UserByID(ctx, userID)
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
