-- Phone + SMS-OTP authentication (domestic enterprise account convention):
-- employees register/sign in with a mobile number + one-time code instead of
-- (or in addition to) email+password. users.phone stores the normalized
-- number; phone-only accounts keep email = 'phone:<number>' (satisfying the
-- NOT NULL UNIQUE email constraint) with an unusable password sentinel, the
-- same provisioning shape OIDC accounts use. phone_otps holds pending codes:
-- hashed at rest, 10-minute TTL, attempt-capped, single-use.

ALTER TABLE users ADD COLUMN IF NOT EXISTS phone TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_phone ON users (phone) WHERE phone IS NOT NULL;

CREATE TABLE IF NOT EXISTS phone_otps (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    phone       TEXT NOT NULL,
    code_hash   TEXT NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    attempts    INT NOT NULL DEFAULT 0,
    consumed_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_phone_otps_phone ON phone_otps (phone, created_at DESC);
