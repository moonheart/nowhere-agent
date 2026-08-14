// Package oidc implements single-sign-on via the OIDC authorization-code flow
// (enterprise-readiness P1-2). It is the bridge between an external identity
// provider (钉钉 / 企业微信 / 飞书 / any standard OIDC IdP) and the platform's
// own account layer: the browser is redirected to the IdP to authenticate, the
// IdP returns an authorization code, and this package exchanges it for an
// id_token, verifies that token against the IdP's published keys, and hands the
// verified (issuer, subject, email, name) to the identity layer to provision or
// resolve a platform account and issue the platform's own bearer token.
//
// The platform's session model is unchanged: SSO is only a sign-in MECHANISM.
// Everything downstream (RequireAuth, teams, quotas) still keys off the platform
// account and its opaque bearer token; the IdP is consulted only at login.
//
// Trust boundaries: the (issuer, subject) pair comes from a cryptographically
// verified id_token, never from a query param or client body. The state cookie
// guards the callback against CSRF/login-confusion. The IdP's client secret and
// the id_token are never logged or written to the audit trail.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
)

// Claims is the verified identity an IdP asserts about the signing-in user.
// Subject+Issuer together are the stable external key; Email/Name are profile.
type Claims struct {
	Issuer  string
	Subject string
	Email   string
	Name    string
}

// discoveryDoc is the subset of the OIDC discovery metadata the flow uses.
type discoveryDoc struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// jwks is the IdP's public key set (RSA keys only — the universal case for the
// target IdPs). Each key's n/e build the rsa.PublicKey used to verify id_tokens.
type jwks struct {
	Keys []struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		Alg string `json:"alg"`
		Use string `json:"use"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

// Provider is one configured IdP: its discovered endpoints, its signing keys
// (refreshed lazily), and the oauth2 config for the code exchange.
type Provider struct {
	issuer       string
	oauth2       oauth2.Config
	authEndpoint string
	tokenURL     string
	jwksURI      string
	// pkce makes the flow attach an RFC 7636 S256 challenge to the
	// authorization request and a code_verifier to the exchange.
	pkce bool

	httpClient *http.Client

	mu   sync.Mutex
	keys map[string]*rsa.PublicKey // kid -> public key, from the JWKS
}

// Config carries the OIDC settings the server passes in (mirrors config.OIDC
// without importing the config package, keeping this package reusable).
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
	// PKCE enables RFC 7636 proof-of-key exchange on the flow (S256). Off by
	// default for IdP compatibility; see config.OIDC.PKCE.
	PKCE bool
}

// NewProvider discovers the IdP from its issuer and builds a Provider. It makes
// one network call (the .well-known document) and one more (the initial JWKS);
// both must succeed for SSO to be considered configured, so a misconfigured
// issuer fails fast at startup rather than on the first login attempt. httpClient
// may be nil to use a default with a sane timeout.
func NewProvider(ctx context.Context, cfg Config, httpClient *http.Client) (*Provider, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" {
		return nil, errors.New("oidc: issuer and client id are required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"openid", "profile", "email"}
	}

	var disco discoveryDoc
	if err := getJSON(ctx, httpClient, strings.TrimSuffix(cfg.Issuer, "/")+"/.well-known/openid-configuration", &disco); err != nil {
		return nil, fmt.Errorf("oidc: discover %s: %w", cfg.Issuer, err)
	}
	if disco.AuthorizationEndpoint == "" || disco.TokenEndpoint == "" || disco.JWKSURI == "" {
		return nil, fmt.Errorf("oidc: discovery document missing endpoints: %+v", disco)
	}
	// The discovered issuer must match the configured one; a mismatch means the
	// URL we fetched is not the IdP we meant (a redirect or a typo), and id_token
	// iss validation would otherwise trust the wrong authority.
	iss := disco.Issuer
	if iss == "" {
		iss = cfg.Issuer
	}
	if !strings.EqualFold(strings.TrimSuffix(iss, "/"), strings.TrimSuffix(cfg.Issuer, "/")) {
		return nil, fmt.Errorf("oidc: issuer mismatch: configured %q, discovered %q", cfg.Issuer, iss)
	}

	p := &Provider{
		issuer:       iss,
		authEndpoint: disco.AuthorizationEndpoint,
		tokenURL:     disco.TokenEndpoint,
		jwksURI:      disco.JWKSURI,
		pkce:         cfg.PKCE,
		httpClient:   httpClient,
		keys:         make(map[string]*rsa.PublicKey),
		oauth2: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint: oauth2.Endpoint{
				AuthURL:  disco.AuthorizationEndpoint,
				TokenURL: disco.TokenEndpoint,
			},
			RedirectURL: cfg.RedirectURL,
			Scopes:      cfg.Scopes,
		},
	}
	if err := p.refreshKeys(ctx); err != nil {
		return nil, fmt.Errorf("oidc: fetch jwks: %w", err)
	}
	return p, nil
}

// AuthURL builds the IdP authorization URL the browser is sent to. state is the
// CSRF/login-confusion token the callback must echo back (see StateCookie).
func (p *Provider) AuthURL(state string) string {
	return p.oauth2.AuthCodeURL(state)
}

// AuthURLPKCE builds the authorization URL for a PKCE flow: it adds the RFC
// 7636 S256 challenge derived from verifier (code_challenge +
// code_challenge_method). Only the handler's PKCE-enabled login path calls it.
func (p *Provider) AuthURLPKCE(state, verifier string) string {
	return p.oauth2.AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", verifierChallenge(verifier)),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"))
}

// PKCEEnabled reports whether this provider's flow carries PKCE.
func (p *Provider) PKCEEnabled() bool { return p.pkce }

// Exchange trades an authorization code for tokens and verifies the id_token,
// returning the asserted identity. This is the security core: the code is
// redeemed server-side (never exposed to the browser beyond the redirect), and
// the id_token's signature, issuer, audience, and expiry are all validated
// before any claim is trusted.
func (p *Provider) Exchange(ctx context.Context, code string) (Claims, error) {
	return p.exchange(ctx, code)
}

// ExchangePKCE is Exchange for a PKCE flow: it additionally sends the
// code_verifier the authorization request's challenge was derived from. The
// IdP proves the code was redeemed by the same party that started the flow.
func (p *Provider) ExchangePKCE(ctx context.Context, code, verifier string) (Claims, error) {
	return p.exchange(ctx, code, oauth2.SetAuthURLParam("code_verifier", verifier))
}

func (p *Provider) exchange(ctx context.Context, code string, opts ...oauth2.AuthCodeOption) (Claims, error) {
	// The oauth2 package would otherwise use http.DefaultClient with no
	// timeout; route the token-endpoint call through the same bounded client
	// as discovery/JWKS so a hung IdP cannot block the login handler forever.
	ctx = context.WithValue(ctx, oauth2.HTTPClient, p.httpClient)
	tok, err := p.oauth2.Exchange(ctx, code, opts...)
	if err != nil {
		return Claims{}, fmt.Errorf("oidc: exchange code: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return Claims{}, errors.New("oidc: no id_token in token response")
	}
	return p.verifyIDToken(ctx, raw)
}

// NewVerifier returns a fresh RFC 7636 PKCE verifier: 32 random bytes,
// base64url-encoded without padding — the spec's 43-char form (between 43 and
// 128 chars). The handler stores it in an HttpOnly cookie alongside the state
// so the callback can present it at the exchange.
func NewVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oidc: rand verifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// verifierChallenge derives the RFC 7636 S256 code_challenge for a verifier:
// base64url(sha256(verifier)) without padding.
func verifierChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// verifyIDToken validates the id_token's RS256 signature against the IdP's JWKS
// and checks iss/aud/exp, returning the asserted claims.
func (p *Provider) verifyIDToken(ctx context.Context, raw string) (Claims, error) {
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("oidc: unexpected signing method %v", t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		key, err := p.publicKey(ctx, kid)
		if err != nil {
			return nil, err
		}
		return key, nil
	}, jwt.WithAudience(p.oauth2.ClientID), jwt.WithIssuer(p.issuer), jwt.WithExpirationRequired())
	if err != nil {
		return Claims{}, fmt.Errorf("oidc: verify id_token: %w", err)
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return Claims{}, errors.New("oidc: id_token missing sub")
	}
	email, _ := claims["email"].(string)
	name, _ := claims["name"].(string)
	if name == "" {
		name, _ = claims["preferred_username"].(string)
	}
	return Claims{Issuer: p.issuer, Subject: sub, Email: email, Name: name}, nil
}

// publicKey returns the verification key for a key id, refreshing the JWKS once
// on an unknown kid (key rotation) before giving up.
func (p *Provider) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	p.mu.Lock()
	key, ok := p.keys[kid]
	p.mu.Unlock()
	if ok {
		return key, nil
	}
	// Unknown kid: the IdP may have rotated keys. Refresh once and retry.
	if err := p.refreshKeys(ctx); err != nil {
		return nil, fmt.Errorf("oidc: refresh jwks: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if key, ok := p.keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("oidc: no verification key for kid %q", kid)
}

// refreshKeys fetches the IdP's JWKS and rebuilds the kid->key map.
func (p *Provider) refreshKeys(ctx context.Context) error {
	var set jwks
	if err := getJSON(ctx, p.httpClient, p.jwksURI, &set); err != nil {
		return err
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.N == "" || k.E == "" {
			continue
		}
		pub, err := rsaPublicKey(k.N, k.E)
		if err != nil {
			continue // skip a malformed key rather than fail the whole set
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return errors.New("oidc: jwks contained no usable RSA keys")
	}
	p.mu.Lock()
	p.keys = keys
	p.mu.Unlock()
	return nil
}

// rsaPublicKey decodes a JWK RSA modulus/exponent (base64url, no padding).
func rsaPublicKey(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, err
	}
	n := new(big.Int).SetBytes(nBytes)
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	if e == 0 {
		return nil, errors.New("oidc: zero exponent")
	}
	return &rsa.PublicKey{N: n, E: e}, nil
}

// getJSON fetches url and decodes the JSON body into v.
func getJSON(ctx context.Context, c *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// NewState returns a random CSRF/login-confusion state token for one flow.
func NewState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oidc: rand state: %w", err)
	}
	return hex.EncodeToString(b), nil
}
