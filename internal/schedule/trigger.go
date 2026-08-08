package schedule

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

// LoopBuilder builds the agent loop for one fire: provider adapter, model,
// system prompt, and permission/compression middleware. The server implements
// it by reusing the chat loop factory. system is the resolved system prompt
// (empty for a free-text task); model overrides the loop's default ("" =
// inherit). Tools are NOT bound here — the session is not yet known — but via
// ToolBinder once the trigger resolves the session.
type LoopBuilder func(ctx context.Context, task Task, system, model string) *agent.Loop

// ToolBinder attaches the session-scoped, whitelist-filtered tools to a loop
// once the target session is known (design D3). It mirrors chatapi.ToolBinder
// but narrows to the task's whitelist.
type ToolBinder func(ctx context.Context, loop *agent.Loop, sessionID string, whitelist []string)

// ScopeResolver returns the scopes a user may read from (own user scope, their
// teams, system). *identity.Service satisfies it; narrowing the dependency to
// an interface keeps the trigger testable without a database.
type ScopeResolver interface {
	AccessibleScopes(ctx context.Context, userID string) ([]identity.ScopeRef, error)
}

// DefResolver resolves an agent definition by name across scopes.
// *agentdef.Store satisfies it.
type DefResolver interface {
	Resolve(name string, scopes []identity.ScopeRef) (agentdef.AgentDef, error)
}

// Trigger scans for due tasks and fires each through the run registry — the
// "unattended chatapi" (design D5). It owns the scan-claim-submit loop; the
// claim's atomic advance (design D4) makes it safe to run one per instance.
type Trigger struct {
	store     Store
	runtime   *session.Runtime
	registry  *session.RunRegistry
	defs      DefResolver
	identity  ScopeResolver
	buildLoop LoopBuilder
	bindTools ToolBinder
	// db is used only to tag the sessions a task produces (task_id/source/
	// metadata) — the session.Store interface pre-dates those columns, so the
	// trigger writes them directly. Nil disables tagging (tests with no DB).
	db  *sql.DB
	log *slog.Logger
	now func() time.Time
	// scanInterval is how often the trigger looks for due tasks.
	scanInterval time.Duration
	// fireTimeout bounds one fire's environment construction; the run itself is
	// unbounded (registry-owned), this only guards the pre-submit work.
	fireTimeout time.Duration
}

// NewTrigger wires a Trigger. scanInterval <= 0 defaults to 30s.
func NewTrigger(store Store, rt *session.Runtime, rg *session.RunRegistry, defs DefResolver, ids ScopeResolver, build LoopBuilder, db *sql.DB, scanInterval time.Duration) *Trigger {
	if scanInterval <= 0 {
		scanInterval = 30 * time.Second
	}
	return &Trigger{
		store: store, runtime: rt, registry: rg, defs: defs, identity: ids,
		buildLoop: build, db: db, log: slog.Default(),
		now: func() time.Time { return time.Now().UTC() },
		scanInterval: scanInterval, fireTimeout: 30 * time.Second,
	}
}

// WithToolBinder wires the session-scoped, whitelist-filtered tool binder. Nil
// leaves runs tool-free (tests).
func (tr *Trigger) WithToolBinder(b ToolBinder) *Trigger {
	tr.bindTools = b
	return tr
}

// SetLogger overrides the trigger's logger.
func (tr *Trigger) SetLogger(l *slog.Logger) {
	if l != nil {
		tr.log = l
	}
}

// SetClock overrides the trigger's clock (tests).
func (tr *Trigger) SetClock(now func() time.Time) {
	if now != nil {
		tr.now = now
	}
}

// Start runs the scan loop until ctx is cancelled. It fires once immediately
// (catching up anything due) then ticks on scanInterval.
func (tr *Trigger) Start(ctx context.Context) {
	tr.sweep(ctx)
	ticker := time.NewTicker(tr.scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tr.sweep(ctx)
		}
	}
}

// sweep finds due tasks and fires each. A single task's failure is logged and
// never aborts the sweep.
func (tr *Trigger) sweep(ctx context.Context) {
	due, err := tr.store.ListDue(ctx, tr.now())
	if err != nil {
		tr.log.Error("schedule: due scan failed", "err", err)
		return
	}
	for _, t := range due {
		if err := tr.fireOnce(ctx, t); err != nil {
			if errors.Is(err, ErrNotFound) {
				// Lost the claim race or the task changed under us — normal, skip.
				continue
			}
			tr.log.Error("schedule: fire failed", "task", t.ID, "err", err)
		}
	}
}

// fireOnce claims the task and, on a successful claim, submits the run.
func (tr *Trigger) fireOnce(ctx context.Context, task Task) error {
	claimed, err := tr.store.Claim(ctx, task.ID, tr.now())
	if err != nil {
		return err // ErrNotFound = another instance claimed it; skip.
	}

	fireCtx, cancel := context.WithTimeout(ctx, tr.fireTimeout)
	defer cancel()
	return tr.submit(fireCtx, claimed)
}

// FireNow runs one task immediately, out of band. Unlike a scheduled fire it
// does NOT claim the task, so next_run_at/cron are untouched — a manual run is
// independent of the cadence. It goes through the same submit path a due fire
// does. A busy target under reject/enqueue is a quiet skip, same as a sweep;
// interrupt cancels the active run first.
func (tr *Trigger) FireNow(ctx context.Context, task Task) error {
	fireCtx, cancel := context.WithTimeout(ctx, tr.fireTimeout)
	defer cancel()
	return tr.submit(fireCtx, task)
}

// submit builds the run environment for a claimed task and hands it to the
// registry. It is the construction half of the "unattended chatapi" (design:
// run construction).
func (tr *Trigger) submit(ctx context.Context, task Task) error {
	// Resolve the prompt source into a system prompt, model, and opening turn.
	system, model, kickoff, err := tr.resolvePrompt(ctx, task)
	if err != nil {
		return err
	}

	// Resolve or create the target session (design D2).
	sessID, fresh, err := tr.resolveSession(ctx, task, kickoff)
	if err != nil {
		return err
	}

	// Concurrency strategy: what to do about an already-active run on the
	// target session (design: multitask).
	proceed, err := tr.gateMultitask(ctx, task, sessID)
	if err != nil || !proceed {
		if !proceed && fresh {
			tr.cleanupFreshSession(ctx, task, sessID)
		}
		return err
	}

	// Build the whitelisted loop and submit through the shared registry — from
	// here on it is byte-identical to a human chat run.
	loop := tr.buildLoop(ctx, task, system, model)
	if tr.bindTools != nil {
		tr.bindTools(ctx, loop, sessID, task.ToolWhitelist)
	}
	userMsg := provider.TextMessage(provider.RoleUser, kickoff)
	run, err := tr.registry.Submit(ctx, sessID, session.RunWork{
		Loop:        loop,
		History:     []provider.Message{userMsg}, // the worker runs History verbatim; it does not merge UserMessage in
		UserMessage: &userMsg,
	})
	if err != nil {
		if errors.Is(err, session.ErrRunActive) {
			// multitask=reject raced an active run; skip without error.
			tr.cleanupFreshSession(ctx, task, sessID)
			return nil
		}
		tr.cleanupFreshSession(ctx, task, sessID)
		return err
	}

	tr.log.Info("schedule: fired", "task", task.ID, "session", sessID, "run", run.ID)

	// on_run_completed = delete: remove a freshly-created session once the run
	// reaches a terminal state. Watch in the background so submit stays fast.
	if fresh && task.OnRunCompleted == OnRunDelete {
		go tr.deleteWhenTerminal(task, sessID, run.ID)
	}
	return nil
}

// resolvePrompt maps the task's prompt source (design D1) to a system prompt,
// model, and the opening user turn. A free-text task has no system prompt and
// inherits the default model; an agent-def task takes both from the definition.
func (tr *Trigger) resolvePrompt(ctx context.Context, task Task) (system, model, kickoff string, err error) {
	if task.PromptSource() == SourcePrompt {
		return "", "", task.Prompt, nil
	}
	// Agent definition: resolve across the owner's scopes (user > team > system).
	scopes, serr := tr.identity.AccessibleScopes(ctx, task.UserID)
	if serr != nil {
		scopes = []identity.ScopeRef{identity.UserScope(task.UserID), identity.SystemScope()}
	}
	def, err := tr.defs.Resolve(task.AgentDefName, scopes)
	if err != nil {
		return "", "", "", err
	}
	kickoff = task.Prompt
	if kickoff == "" {
		kickoff = "Begin the scheduled task now."
	}
	return def.System, def.Model, kickoff, nil
}

// resolveSession returns the session to fire into. With a target session the
// fire appends to it (fresh=false); otherwise it creates a tagged session
// (fresh=true) recording task_id/source/metadata (design D7).
func (tr *Trigger) resolveSession(ctx context.Context, task Task, kickoff string) (sessID string, fresh bool, err error) {
	if task.TargetSessionID != "" {
		if _, err := tr.runtime.GetSession(ctx, task.TargetSessionID); err != nil {
			return "", false, err
		}
		return task.TargetSessionID, false, nil
	}
	title := truncate(kickoff, 60)
	if tr.db == nil {
		// No DB handle (tests): create untagged via the runtime.
		s, err := tr.runtime.CreateSession(ctx, task.UserID, title)
		return s.ID, true, err
	}
	return tr.createTaggedSession(ctx, task, title)
}

// createTaggedSession inserts a session carrying the task back-reference and
// provenance metadata, returning its id. It writes directly because the
// session.Store interface predates the task_id/source/metadata columns.
func (tr *Trigger) createTaggedSession(ctx context.Context, task Task, title string) (string, bool, error) {
	meta := map[string]any{"trigger": "scheduled", "task_id": task.ID}
	for k, v := range task.Metadata {
		meta[k] = v // task metadata merges in; the trigger keys win on conflict
	}
	meta["task_id"] = task.ID
	raw, err := json.Marshal(meta)
	if err != nil {
		return "", false, err
	}
	var id string
	err = tr.db.QueryRowContext(ctx, `
		INSERT INTO sessions (user_id, title, task_id, source, metadata)
		VALUES ($1, $2, $3, 'scheduled', $4) RETURNING id`,
		task.UserID, title, task.ID, raw).Scan(&id)
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// gateMultitask applies the task's concurrency strategy against an active run
// on the target session. It reports whether to proceed with the fire.
func (tr *Trigger) gateMultitask(ctx context.Context, task Task, sessID string) (bool, error) {
	_, active, err := tr.runtime.ActiveRun(ctx, sessID)
	if err != nil {
		return false, err
	}
	if !active {
		return true, nil
	}
	switch task.Multitask {
	case MultitaskInterrupt:
		if _, err := tr.runtime.CancelRun(ctx, sessID); err != nil {
			return false, err
		}
		return true, nil
	case MultitaskEnqueue:
		// The run registry enforces single-active-run; an enqueue is realised by
		// simply letting Submit fail with ErrRunActive and skipping — the next
		// occurrence fires after the active run drains. Treat as skip for now.
		tr.log.Info("schedule: enqueue skipped (busy)", "task", task.ID, "session", sessID)
		return false, nil
	default: // reject
		tr.log.Info("schedule: fire skipped, session busy", "task", task.ID, "session", sessID)
		return false, nil
	}
}

// cleanupFreshSession removes a just-created session whose fire was abandoned
// before submit (busy target under reject, or a submit error), so a rejected
// fire does not litter an empty session.
func (tr *Trigger) cleanupFreshSession(ctx context.Context, task Task, sessID string) {
	if task.TargetSessionID != "" {
		return // never delete the user's designated target session
	}
	if tr.db != nil {
		if _, err := tr.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, sessID); err != nil {
			tr.log.Warn("schedule: cleanup fresh session failed", "session", sessID, "err", err)
		}
	}
}

// deleteWhenTerminal waits for a run to reach a terminal state, then removes a
// freshly-created session (on_run_completed = delete).
func (tr *Trigger) deleteWhenTerminal(task Task, sessID, runID string) {
	if tr.db == nil {
		return
	}
	// Poll the run until terminal, bounded so a stuck run cannot leak a goroutine.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var status string
			err := tr.db.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = $1`, runID).Scan(&status)
			if err != nil {
				return
			}
			switch session.RunStatus(status) {
			case session.RunDone, session.RunFailed, session.RunCancelled:
				if _, err := tr.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, sessID); err != nil {
					tr.log.Warn("schedule: on_run_completed delete failed", "session", sessID, "err", err)
				}
				return
			}
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
