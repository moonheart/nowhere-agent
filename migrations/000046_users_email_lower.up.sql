-- Case-insensitive user emails (identity consistency): the users.email unique
-- constraint is case-sensitive, so "User@X.com" and "user@x.com" were two
-- accounts and the SSO email merge could silently create a second account when
-- the IdP case differed. All entry points now normalize (trim + lowercase)
-- before storage/lookup; this migration brings existing rows in line and pins
-- the invariant with a unique index on lower(email).

-- 1) Normalize stored emails: trim and lowercase. Colliding rows (the same
--    address stored with different casing) are deduplicated afterwards, so the
--    index below can be created.
UPDATE users
SET email = lower(trim(email))
WHERE email <> lower(trim(email));

-- 2) Deduplicate mixed-case copies of the same address: keep the EARLIEST
--    account (it is the one the user created first), and give later copies a
--    deterministic +dup- suffix so no row is deleted — their data (sessions,
--    messages, team links) survives and the suffixed address still identifies
--    the row uniquely. The tuple (created_at, id) comparison orders the group
--    even when two accounts share a timestamp.
UPDATE users u
SET email = u.email || '+dup-' || replace(u.id::text, '-', '')
FROM users d
WHERE u.email = d.email
  AND (u.created_at, u.id) > (d.created_at, d.id);

-- 3) Pin the invariant: no two accounts may share a case-insensitive email.
CREATE UNIQUE INDEX IF NOT EXISTS users_email_lower_uniq ON users (lower(email));
