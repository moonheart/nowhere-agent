-- OIDC / SSO login (enterprise-readiness P1-2): link an external identity to a
-- platform account. Until now the only credential was a local email+password;
-- an enterprise rolling out SSO needs accounts provisioned from its IdP (钉钉 /
-- 企业微信 / 飞书 / any standard OIDC provider) instead of a per-platform
-- password. One row per (issuer, subject) pair — the pair OIDC guarantees is
-- stable and unique per external identity.

-- user_id has ON DELETE CASCADE so removing the account drops its links; the
-- link is meaningless without the account. (issuer, subject) is the natural
-- unique key. password_hash on the linked users row stays whatever it was (for
-- an SSO-provisioned account it is unusable — sign-in goes through the IdP), so
-- SSO and password can coexist on one account.
CREATE TABLE IF NOT EXISTS user_identities (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- issuer is the OIDC iss claim (the IdP's identifier URL); subject is the
    -- sub claim. Both come from a verified id_token, never from client input.
    issuer     TEXT NOT NULL,
    subject    TEXT NOT NULL,
    -- email/display_name are the profile the IdP asserted at last login, kept
    -- for the admin console and audit; they are informational, not the key.
    email      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (issuer, subject)
);

-- Resolve an external identity to its account on every SSO callback.
CREATE INDEX IF NOT EXISTS idx_user_identities_user ON user_identities(user_id);
