-- TOTP second factor (MFA, 等保-alignment): admin/enterprise accounts can
-- enroll an authenticator-app one-time password (RFC 6238). The shared
-- secret is stored base32; totp_enabled gates the login challenge. A blank
-- secret with enabled=false means "no second factor".

ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_secret TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_enabled BOOLEAN NOT NULL DEFAULT false;
