package inbound

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/agentdef"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
)

// LoopBuilder builds the agent loop for one trigger: provider adapter, model,
// system prompt, and permission/compression middleware. It mirrors
// schedule.LoopBuilder's contract — system is the resolved system prompt
// ("" means the server's default), model overrides the loop's ("" = inherit).
type LoopBuilder func(ctx context.Context, userID, teamID, system, model string) (*agent.Loop, error)

// ToolBinder attaches the session-scoped tools to a loop once the target
// session is known (the same binder a chat run gets).
type ToolBinder func(ctx context.Context, loop *agent.Loop, sessionID string)

// TeamAttributor resolves which team's provider key bills the webhook owner's
// runs (P1-3): the team id when a team key applies, "" when the platform key
// does. Nil leaves runs unattributed.
type TeamAttributor func(ctx context.Context, userID string) string

// BudgetChecker reports whether the webhook owner's run (billed to teamID, or
// the platform when "") may proceed under the monthly token budget (P1-1).
// Nil leaves triggered runs ungated.
type BudgetChecker func(ctx context.Context, userID, teamID string) error

// DefResolver resolves an agent definition by name across scopes.
type DefResolver interface {
	Resolve(name string, scopes []identity.ScopeRef) (agentdef.AgentDef, error)
}

// ScopeResolver returns the scopes a user may read from.
type ScopeResolver interface {
	AccessibleScopes(ctx context.Context, userID string) ([]identity.ScopeRef, error)
}

// RunCompleter is not needed here: triggered runs flow through the shared
// RunRegistry, whose RunDoneHook already delivers run-completion notifications
// (the server's notifyTarget resolves the inbound webhook's notify_url from
// the session's provenance metadata).

// Dispatcher turns a verified trigger request into a run, byte-identical to a
// scheduled firing once submitted: resolve the prompt source, resolve or
// create the session, gate budget, build the loop, submit to the shared
// registry.
type Dispatcher struct {
	store     *Store
	runtime   *session.Runtime
	registry  *session.RunRegistry
	defs      DefResolver
	identity  ScopeResolver
	buildLoop LoopBuilder
	bindTools ToolBinder
	// baseSystem resolves the platform default system prompt per dispatch,
	// used when the webhook carries neither an agent_def nor a fixed
	// system_prompt — a func so a runtime language change applies to the next
	// triggered run without a restart.
	baseSystem func() string
	attributor TeamAttributor
	budgetGate BudgetChecker
	// db is used to tag the sessions a trigger produces (source/metadata).
	db  *sql.DB
	log *slog.Logger
	now func() time.Time
}

// NewDispatcher wires a Dispatcher over a Store, the shared runtime and
// registry, and the server's loop builder.
func NewDispatcher(st *Store, rt *session.Runtime, rg *session.RunRegistry, defs DefResolver, ids ScopeResolver, build LoopBuilder, baseSystem func() string, db *sql.DB) *Dispatcher {
	return &Dispatcher{
		store: st, runtime: rt, registry: rg, defs: defs, identity: ids,
		buildLoop: build, baseSystem: baseSystem, db: db, log: slog.Default(),
		now: func() time.Time { return time.Now().UTC() },
	}
}

// WithToolBinder wires the session tool binder.
func (d *Dispatcher) WithToolBinder(b ToolBinder) *Dispatcher {
	d.bindTools = b
	return d
}

// WithTeamAttributor wires run billing attribution.
func (d *Dispatcher) WithTeamAttributor(a TeamAttributor) *Dispatcher {
	d.attributor = a
	return d
}

// WithBudgetGate wires monthly token-budget enforcement.
func (d *Dispatcher) WithBudgetGate(g BudgetChecker) *Dispatcher {
	d.budgetGate = g
	return d
}

// SetLogger overrides the dispatcher's logger.
func (d *Dispatcher) SetLogger(l *slog.Logger) {
	if l != nil {
		d.log = l
	}
}

// SetClock overrides the dispatcher's clock (tests).
func (d *Dispatcher) SetClock(now func() time.Time) {
	if now != nil {
		d.now = now
	}
}

// ErrDisabled is returned when a trigger hits a disabled webhook.
var ErrDisabled = errors.New("inbound webhook disabled")

// ErrPendingInteraction is returned when the target session holds undecided
// approvals — a trigger must not bury a human's pending decision.
var ErrPendingInteraction = errors.New("session has pending interactions")

// ErrNotOwner is returned when a trigger targets a session the webhook's
// owner does not own (cross-tenant boundary, IDOR guard).
var ErrNotOwner = errors.New("webhook owner does not own the target session")

// Dispatch is the trigger request's call into run construction. It verifies
// the webhook is enabled, resolves the prompt source, resolves or creates the
// target session, gates the budget, builds the loop, and submits the run. It
// returns the run id and session id; the run executes on the registry's own
// goroutine and completes asynchronously.
func (d *Dispatcher) Dispatch(ctx context.Context, wh Webhook, prompt string, metadata map[string]any) (runID, sessionID string, err error) {
	if !wh.Enabled {
		return "", "", ErrDisabled
	}

	// Resolve the prompt source (mirrors schedule.Trigger.resolvePrompt):
	// an agent definition takes precedence, then a fixed system prompt, then
	// the platform default. agent_def and system_prompt cannot both be set
	// (create enforces it), so the order is deterministic.
	system, model := d.resolvePrompt(ctx, wh)
	if system == "" && d.baseSystem != nil {
		system = d.baseSystem()
	}

	// Resolve or create the target session.
	sessID, _, err := d.resolveSession(ctx, wh, prompt, metadata)
	if err != nil {
		return "", "", err
	}

	// Pending-interaction gate: a session with undecided approvals rejects new
	// submissions — a trigger must not bury a human's pending decision.
	if pending, err := d.registry.PendingApprovalsForSession(ctx, sessID); err == nil && len(pending) > 0 {
		d.cleanupFreshSession(ctx, wh, sessID)
		return "", "", ErrPendingInteraction
	}

	// Build the loop through the shared builder.
	loop, err := d.buildLoop(ctx, wh.UserID, wh.TeamID, system, model)
	if err != nil {
		d.cleanupFreshSession(ctx, wh, sessID)
		return "", "", err
	}
	if d.bindTools != nil {
		d.bindTools(ctx, loop, sessID)
	}
	userMsg := provider.TextMessage(provider.RoleUser, prompt)

	// Billing attribution + budget gate, same contract as a human chat run.
	var teamID string
	if d.attributor != nil {
		teamID = d.attributor(ctx, wh.UserID)
	}
	if d.budgetGate != nil {
		if err := d.budgetGate(ctx, wh.UserID, teamID); err != nil {
			d.cleanupFreshSession(ctx, wh, sessID)
			return "", "", err
		}
	}

	run, err := d.registry.Submit(ctx, sessID, session.RunWork{
		Loop:        loop,
		History:     []provider.Message{userMsg},
		UserMessage: &userMsg,
		TeamID:      teamID,
		Model:       loop.Model(),
	})
	if err != nil {
		d.cleanupFreshSession(ctx, wh, sessID)
		return "", "", err
	}

	// Provenance stamp for a REUSED target session — deliberately AFTER the
	// submit: a fire that never became a run must not retag the session, and
	// the tag must not overwrite one already held by a different webhook
	// (last-write-wins would misdeliver A's run-completion notification to
	// webhook B). See stampReusedSession.
	d.stampReusedSession(ctx, wh, sessID)

	d.log.Info("inbound: run started", "webhook", wh.ID, "session", sessID, "run", run.ID)
	return run.ID, sessID, nil
}

// resolvePrompt maps the webhook's prompt source to a system prompt and model.
// An agent definition supplies both; a fixed system_prompt supplies only the
// system prompt (model stays the resolved default). A missing def resolver
// (tests) or unresolvable definition falls back to the fixed prompt — the
// trigger must never die on a def-lookup hiccup.
func (d *Dispatcher) resolvePrompt(ctx context.Context, wh Webhook) (system, model string) {
	if wh.AgentDef == "" || d.defs == nil {
		return wh.SystemPrompt, ""
	}
	scopes, err := d.identity.AccessibleScopes(ctx, wh.UserID)
	if err != nil {
		scopes = []identity.ScopeRef{identity.UserScope(wh.UserID), identity.SystemScope()}
	}
	def, err := d.defs.Resolve(wh.AgentDef, scopes)
	if err != nil {
		d.log.Warn("inbound: agent def resolve failed, falling back to default prompt", "webhook", wh.ID, "agent_def", wh.AgentDef, "err", err)
		return wh.SystemPrompt, ""
	}
	return def.System, def.Model
}

// resolveSession reuses the webhook's target session when set, else creates a
// tagged session recording the trigger provenance.
func (d *Dispatcher) resolveSession(ctx context.Context, wh Webhook, prompt string, metadata map[string]any) (sessID string, fresh bool, err error) {
	if wh.TargetSessionID != "" {
		sess, err := d.runtime.GetSession(ctx, wh.TargetSessionID)
		if err != nil {
			return "", false, err
		}
		// Ownership gate (IDOR): a webhook may only inject runs into the
		// owner's own sessions. A webhook-owned run in someone else's session
		// would read/write that user's conversation and workspace — the same
		// cross-tenant boundary chatapi.enforceSessionVisibleTo guards.
		if sess.UserID != wh.UserID {
			return "", false, ErrNotOwner
		}
		// Provenance tagging for a REUSED target session happens after the
		// submit succeeds (Dispatch → stampReusedSession): a fire abandoned
		// before submit (pending interaction, budget, loop build, active run)
		// must not retag the session, and the tag must never overwrite another
		// webhook's tag.
		return wh.TargetSessionID, false, nil
	}
	title := truncate(prompt, 60)
	if d.db == nil {
		s, err := d.runtime.CreateSession(ctx, wh.UserID, title)
		return s.ID, true, err
	}
	meta := map[string]any{"trigger": "inbound", "webhook_id": wh.ID}
	for k, v := range metadata {
		meta[k] = v
	}
	meta["webhook_id"] = wh.ID
	raw, err := json.Marshal(meta)
	if err != nil {
		return "", false, err
	}
	var id string
	err = d.db.QueryRowContext(ctx, `
		INSERT INTO sessions (user_id, title, source, metadata)
		VALUES ($1, $2, 'inbound', $3) RETURNING id`,
		wh.UserID, title, raw).Scan(&id)
	return id, true, err
}

// stampReusedSession tags a reused target session with the webhook
// back-reference so run-completion notifications can resolve this webhook's
// notify_url (target.go matches on source = 'inbound' AND
// metadata->>'webhook_id'). The tag must not clobber sibling metadata keys,
// so it goes through jsonb_set. The WHERE makes it conditional: the tag is
// only written when the session is untagged or already carries THIS webhook's
// id, so a second webhook firing into the same session cannot overwrite the
// first's tag and misdeliver its run-completion notification. Best-effort —
// the run is already submitted, so a failure is logged, never fatal. The
// residual race (a run completing after another webhook legitimately tagged
// the session first) is not solved here; it would require run-level
// provenance rather than a session-level tag.
func (d *Dispatcher) stampReusedSession(ctx context.Context, wh Webhook, sessID string) {
	if d.db == nil {
		return
	}
	if _, err := d.db.ExecContext(ctx, `
		UPDATE sessions SET source = 'inbound',
			metadata = jsonb_set(metadata, '{webhook_id}', to_jsonb($1::text), true)
		WHERE id = $2
		  AND (metadata->>'webhook_id' IS NULL OR metadata->>'webhook_id' = $1)`,
		wh.ID, sessID); err != nil {
		d.log.Warn("inbound: stamp reused session failed", "session", sessID, "err", err)
	}
}

// cleanupFreshSession removes a just-created session whose trigger was
// abandoned before submit, so a rejected trigger does not litter an empty
// session.
func (d *Dispatcher) cleanupFreshSession(ctx context.Context, wh Webhook, sessID string) {
	if wh.TargetSessionID != "" {
		return // never delete the owner's designated target session
	}
	if d.db != nil {
		if _, err := d.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, sessID); err != nil {
			d.log.Warn("inbound: cleanup fresh session failed", "session", sessID, "err", err)
		}
	}
}

// truncate shortens s to n runes for a session title.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
