-- Replay protection for inbound webhook triggers: a client-supplied nonce is
-- folded into the HMAC signature ("<ts>.<nonce>.<body>") and deduplicated
-- here, so the same signed event can only start one run within the signature
-- window. Rows expire with the signature window (5 minutes + skew); the
-- trigger path prunes opportunistically on each claim.

CREATE TABLE inbound_webhook_nonces (
    webhook_id UUID NOT NULL REFERENCES inbound_webhooks(id) ON DELETE CASCADE,
    nonce      TEXT NOT NULL,
    seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (webhook_id, nonce)
);
