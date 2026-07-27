package agent

import (
	"context"
)

// SessionStateWriter persists one key of the session's generic key/value state
// (capability-gap O1) and fans the change out to attached clients. It is the
// low-coupling seam between a tool that produces session-level state (plan_write
// and any future session-state producer) and the session runtime that owns the
// storage: the tool depends only on this func signature, never on the session
// store, SQL, or the broker. The server injects an implementation that calls
// session.Runtime.SetSessionStateKV (persist via jsonb_set + live broker push).
//
// key is the feature's namespace ("plan", ...); value is marshalled to JSON for
// storage. Implementations must be safe for concurrent use.
type SessionStateWriter func(ctx context.Context, key string, value any) error
