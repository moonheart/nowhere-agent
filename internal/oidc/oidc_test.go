package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// fakeIdP is an in-memory OIDC provider: it serves the discovery document and a
// JWKS built from a throwaway RSA key, and signs id_tokens with that key. It
// lets the whole exchange+verify path run without a real IdP.
type fakeIdP struct {
	t       *testing.T
	key     *rsa.PrivateKey
	kid     string
	srv     *httptest.Server
	lastSub string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	f := &fakeIdP{t: t, key: key, kid: "test-key-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("/jwks", f.jwksHandler)
	mux.HandleFunc("/token", f.token)
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		// Not exercised: the flow builds the URL but tests never drive a browser.
		w.WriteHeader(http.StatusOK)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) issuer() string { return f.srv.URL }

func (f *fakeIdP) discovery(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]any{
		"issuer":                 f.issuer(),
		"authorization_endpoint": f.issuer() + "/authorize",
		"token_endpoint":         f.issuer() + "/token",
		"jwks_uri":               f.issuer() + "/jwks",
	})
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (f *fakeIdP) jwksHandler(w http.ResponseWriter, r *http.Request) {
	pub := f.key.PublicKey
	json.NewEncoder(w).Encode(map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"kid": f.kid,
			"alg": "RS256",
			"use": "sig",
			"n":   b64url(pub.N.Bytes()),
			"e":   b64url(big.NewInt(int64(pub.E)).Bytes()),
		}},
	})
}

// signIDToken issues a signed id_token for the given subject/audience.
func (f *fakeIdP) signIDToken(sub, aud, iss string, exp time.Time) string {
	claims := jwt.MapClaims{
		"iss":   iss,
		"sub":   sub,
		"aud":   aud,
		"exp":   exp.Unix(),
		"iat":   time.Now().Add(-time.Minute).Unix(),
		"email": sub + "@corp.test",
		"name":  "User " + sub,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = f.kid
	signed, err := tok.SignedString(f.key)
	if err != nil {
		f.t.Fatalf("sign id_token: %v", err)
	}
	return signed
}

// token serves the code exchange: any code yields a signed id_token for the
// subject recorded in lastSub (set by each test via the client id it passes).
func (f *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	sub := f.lastSub
	if sub == "" {
		sub = "default-sub"
	}
	idTok := f.signIDToken(sub, "test-client", f.issuer(), time.Now().Add(time.Hour))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": "at-" + sub,
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idTok,
	})
}

func newProviderFor(t *testing.T, f *fakeIdP) *Provider {
	t.Helper()
	p, err := NewProvider(context.Background(), Config{
		Issuer:      f.issuer(),
		ClientID:    "test-client",
		RedirectURL: "http://localhost:8080/auth/oidc/callback",
	}, nil)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

func TestNewProviderDiscoversEndpoints(t *testing.T) {
	f := newFakeIdP(t)
	p := newProviderFor(t, f)
	if p.issuer != f.issuer() {
		t.Fatalf("issuer = %q, want %q", p.issuer, f.issuer())
	}
	if p.authEndpoint == "" || p.tokenURL == "" || p.jwksURI == "" {
		t.Fatalf("endpoints not discovered: %+v", p)
	}
	if len(p.keys) != 1 {
		t.Fatalf("jwks should have loaded 1 key, got %d", len(p.keys))
	}
}

func TestNewProviderRejectsIssuerMismatch(t *testing.T) {
	f := newFakeIdP(t)
	_, err := NewProvider(context.Background(), Config{
		Issuer:      f.issuer() + "/wrong",
		ClientID:    "test-client",
		RedirectURL: "http://x/cb",
	}, nil)
	if err == nil {
		t.Fatal("a discovered issuer that does not match the configured one must be rejected")
	}
}

func TestExchangeYieldsVerifiedClaims(t *testing.T) {
	f := newFakeIdP(t)
	f.lastSub = "alice"
	p := newProviderFor(t, f)

	claims, err := p.Exchange(context.Background(), "any-code")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if claims.Subject != "alice" || claims.Issuer != f.issuer() {
		t.Fatalf("claims = %+v", claims)
	}
	if claims.Email != "alice@corp.test" || claims.Name != "User alice" {
		t.Fatalf("profile claims = %+v", claims)
	}
}

func TestVerifyIDTokenRejectsWrongAudience(t *testing.T) {
	f := newFakeIdP(t)
	p := newProviderFor(t, f)
	// A token minted for a DIFFERENT client must not verify for this provider.
	tok := f.signIDToken("mallory", "some-other-client", f.issuer(), time.Now().Add(time.Hour))
	if _, err := p.verifyIDToken(context.Background(), tok); err == nil {
		t.Fatal("wrong-audience id_token must be rejected")
	}
}

func TestVerifyIDTokenRejectsWrongIssuer(t *testing.T) {
	f := newFakeIdP(t)
	p := newProviderFor(t, f)
	tok := f.signIDToken("mallory", "test-client", "https://evil.example", time.Now().Add(time.Hour))
	if _, err := p.verifyIDToken(context.Background(), tok); err == nil {
		t.Fatal("wrong-issuer id_token must be rejected")
	}
}

func TestVerifyIDTokenRejectsExpired(t *testing.T) {
	f := newFakeIdP(t)
	p := newProviderFor(t, f)
	tok := f.signIDToken("bob", "test-client", f.issuer(), time.Now().Add(-time.Hour))
	if _, err := p.verifyIDToken(context.Background(), tok); err == nil {
		t.Fatal("expired id_token must be rejected")
	}
}

func TestVerifyIDTokenRejectsForgedSignature(t *testing.T) {
	f := newFakeIdP(t)
	p := newProviderFor(t, f)
	// Sign with a DIFFERENT key than the JWKS publishes.
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	claims := jwt.MapClaims{
		"iss": f.issuer(), "sub": "mallory", "aud": "test-client",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = f.kid
	forged, err := tok.SignedString(other)
	if err != nil {
		t.Fatalf("sign forged: %v", err)
	}
	if _, err := p.verifyIDToken(context.Background(), forged); err == nil {
		t.Fatal("a token signed by an unknown key must be rejected")
	}
}

func TestPublicKeyRefreshesOnUnknownKID(t *testing.T) {
	f := newFakeIdP(t)
	p := newProviderFor(t, f)
	// Empty the cache, then a lookup must refresh from the JWKS and find the key.
	p.mu.Lock()
	p.keys = map[string]*rsa.PublicKey{}
	p.mu.Unlock()
	key, err := p.publicKey(context.Background(), f.kid)
	if err != nil {
		t.Fatalf("refresh on unknown kid: %v", err)
	}
	if key.N.Cmp(f.key.PublicKey.N) != 0 {
		t.Fatal("refreshed key does not match the IdP key")
	}
}

func TestRSAPublicKeyRoundTrip(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pub, err := rsaPublicKey(b64url(key.PublicKey.N.Bytes()), b64url(big.NewInt(int64(key.PublicKey.E)).Bytes()))
	if err != nil {
		t.Fatalf("rsaPublicKey: %v", err)
	}
	if pub.N.Cmp(key.PublicKey.N) != 0 || pub.E != key.PublicKey.E {
		t.Fatal("decoded key mismatch")
	}
}

func TestAuthURLCarriesStateAndClient(t *testing.T) {
	f := newFakeIdP(t)
	p := newProviderFor(t, f)
	u := p.AuthURL("state-123")
	if u == "" {
		t.Fatal("empty auth url")
	}
	for _, want := range []string{"state-123", "test-client", "openid"} {
		if !strings.Contains(u, want) {
			t.Fatalf("auth url %q missing %q", u, want)
		}
	}
}
