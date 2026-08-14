package schedule

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/agentdef"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/quota"
	"nowhere-agent/internal/session"
)

// LoopBuilder builds the agent loop for one fire: provider adapter, model,
// system prompt, and permission/compression middleware. The server implements
// it by reusing the chat loop factory. system is the resolved system prompt
// (empty for a free-text task); model overrides the loop's default ("" =
// inherit). The error return lets a fire fail with a clear log when the
// provider or model cannot be resolved (e.g. a task naming a model the
// resolved provider does not serve). Tools are NOT bound here — the session is
// not yet known — but via ToolBinder once the trigger resolves the session.
type LoopBuilder func(ctx context.Context, task Task, system, model string) (*agent.Loop, error)

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

// TeamAttributor resolves which team's provider key bills a task owner's runs
// (enterprise-readiness P1-3): the team id when a team key applies, "" when the
// platform key does. Nil leaves scheduled runs unattributed; an error is
// treated as "unattributed" so a lookup hiccup never blocks a firing.
type TeamAttributor func(ctx context.Context, userID string) string

// BudgetChecker reports whether a task owner's run (billed to teamID, or the
// platform when "") may proceed under the monthly token budget
// (enterprise-readiness P1-1). Nil leaves scheduled runs ungated.
type BudgetChecker func(ctx context.Context, userID, teamID string) error

// DefResolver resolves an agent definition by name across scopes.
// *agentdef.Store satisfies it.
type DefResolver interface {
	Resolve(name string, scopes []identity.ScopeRef) (agentdef.AgentDef, error)
}

// Trigger scans for due tasks and fires each through the run registry — the
// "unattended chatapi" (design D5). It owns the scan-claim-submit loop; the
// claim's atomic advance (design D4) makes it safe to run one per instance.
type Trigger struct {
	store      Store
	runtime    *session.Runtime
	registry   *session.RunRegistry
	defs       DefResolver
	identity   ScopeResolver
	buildLoop  LoopBuilder
	bindTools  ToolBinder
	attributor TeamAttributor
	// budgetGate, when set, enforces the monthly token budget before a firing
	// starts spending (P1-1). Nil leaves scheduled runs ungated.
	budgetGate BudgetChecker
	// db is used only to tag the sessions a task produces (task_id/source/
	// metadata) — the session.Store interface pre-dates those columns, so the
	// trigger writes them directly. Nil disables tagging (tests with no DB).
	db  *sql.DB
	log *slog.Logger
	now func() time.Time
	// scanInterval is how often the trigger looks for due tasks. Mutable via
	// SetScanInterval; Start re-reads it every tick.
	scanInterval time.Duration
	// mu guards scanInterval against concurrent SetScanInterval reads.
	mu sync.Mutex
	// enabledFunc, when set, gates sweeps (live schedule_enabled switch).
	enabledFunc func() bool
	// fireTimeout bounds one fire's environment construction; the run itself is
	// unbounded (registry-owned), this only guards the pre-submit work.
	fireTimeout time.Duration
	// deleteOnDone maps a scheduled run's id to the fresh session it created
	// when on_run_completed = delete. watchRunDelete adds an entry at fire
	// time; the registry RunDoneHook (onRunDone) removes the session once the
	// run settles, so the delete follows the run's actual lifetime instead of
	// a fixed polling timeout. In-memory and per-process: a crashed process
	// loses the entry (the session stays, and the workspace retention sweep
	// still reclaims its images).
	deleteOnDone map[string]string
	// ddMu guards deleteOnDone.
	ddMu sync.Mutex
}

// NewTrigger wires a Trigger. scanInterval <= 0 defaults to 30s.
func NewTrigger(store Store, rt *session.Runtime, rg *session.RunRegistry, defs DefResolver, ids ScopeResolver, build LoopBuilder, db *sql.DB, scanInterval time.Duration) *Trigger {
	if scanInterval <= 0 {
		scanInterval = 30 * time.Second
	}
	tr := &Trigger{
		store: store, runtime: rt, registry: rg, defs: defs, identity: ids,
		buildLoop: build, db: db, log: slog.Default(),
		now:          func() time.Time { return time.Now().UTC() },
		scanInterval: scanInterval, fireTimeout: 30 * time.Second,
		deleteOnDone: map[string]string{},
	}
	// on_run_completed = delete: the registry's RunDoneHook performs the
	// session deletion when a fired run settles (see onRunDone), so a run
	// that outlives any fixed poll window still gets its session cleaned up.
	// Registered before any Submit; the hook fires only for terminal runs and
	// is a no-op for every session not registered via watchRunDelete.
	if rg != nil {
		rg.WithRunDoneHook(tr.onRunDone)
	}
	return tr
}

// WithToolBinder wires the session-scoped, whitelist-filtered tool binder. Nil
// leaves runs tool-free (tests).
func (tr *Trigger) WithToolBinder(b ToolBinder) *Trigger {
	tr.bindTools = b
	return tr
}

// WithTeamAttributor wires run billing attribution (P1-3): each fired run is
// stamped with the team whose provider key pays for it. Nil leaves runs
// unattributed.
func (tr *Trigger) WithTeamAttributor(a TeamAttributor) *Trigger {
	tr.attributor = a
	return tr
}

// WithBudgetGate wires monthly token-budget enforcement (P1-1): before a firing
// starts spending, the gate checks the task owner's (and billing team's)
// current-month usage and skips over-budget firings. A skipped firing is logged
// and requeued so it retries on the next scan once the window rolls or the
// budget is raised — it is not a hard failure of the trigger.
func (tr *Trigger) WithBudgetGate(g BudgetChecker) *Trigger {
	tr.budgetGate = g
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
// (catching up anything due) then ticks on scanInterval. The interval is
// re-read on every tick, so a SetScanInterval retune takes effect at the next
// tick (the ticker is rebuilt when it changes).
func (tr *Trigger) Start(ctx context.Context) {
	tr.sweep(ctx)
	ticker := time.NewTicker(tr.scanInterval)
	applied := tr.scanInterval
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tr.mu.Lock()
			cur := tr.scanInterval
			tr.mu.Unlock()
			if cur != applied {
				applied = cur
				ticker.Reset(applied)
			}
			tr.sweep(ctx)
		}
	}
}

// SetScanInterval retunes the scan cadence live (admin console). A
// non-positive value is ignored.
func (tr *Trigger) SetScanInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	tr.mu.Lock()
	tr.scanInterval = d
	tr.mu.Unlock()
}

// WithEnabledFunc wires a live auto-trigger switch (admin console's
// schedule_enabled): when it returns false, sweeps skip firing entirely (task
// CRUD and run-now stay available). Nil leaves the trigger always on.
func (tr *Trigger) WithEnabledFunc(f func() bool) *Trigger {
	tr.enabledFunc = f
	return tr
}

// maxConcurrentFires bounds how many due tasks one sweep fires in parallel.
// Claim is atomic per task (design D4), so parallel fires keep the
// single-instance claim semantics while removing the serial worst case: N
// tasks due together used to start the last one up to N×fireTimeout late.
const maxConcurrentFires = 4

// sweep finds due tasks and fires each through a bounded worker pool. A
// single task's failure is logged and never aborts the sweep. Each fire keeps
// its own timeout ctx (fireOnce's context.WithTimeout), so one slow fire never
// delays its siblings' pre-submit work.
func (tr *Trigger) sweep(ctx context.Context) {
	if tr.enabledFunc != nil && !tr.enabledFunc() {
		return // auto-trigger switched off from the admin console
	}
	due, err := tr.store.ListDue(ctx, tr.now())
	if err != nil {
		tr.log.Error("schedule: due scan failed", "err", err)
		return
	}
	fires := make(chan Task)
	var wg sync.WaitGroup
	for range min(len(due), maxConcurrentFires) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range fires {
				if err := tr.fireOnce(ctx, t); err != nil {
					if errors.Is(err, ErrNotFound) {
						// Lost the claim race or the task changed under us — normal, skip.
						continue
					}
					tr.log.Error("schedule: fire failed", "task", t.ID, "err", err)
				}
			}
		}()
	}
	for _, t := range due {
		fires <- t
	}
	close(fires)
	wg.Wait()
}

// fireOnce claims the task and, on a successful claim, submits the run.
func (tr *Trigger) fireOnce(ctx context.Context, task Task) error {
	claimed, err := tr.store.Claim(ctx, task.ID, tr.now())
	if err != nil {
		return err // ErrNotFound = another instance claimed it; skip.
	}

	fireCtx, cancel := context.WithTimeout(ctx, tr.fireTimeout)
	defer cancel()
	return tr.submit(fireCtx, claimed, true)
}

// FireNow runs one task immediately, out of band. Unlike a scheduled fire it
// does NOT claim the task, so next_run_at/cron are untouched — a manual run is
// independent of the cadence. It goes through the same submit path a due fire
// does (with claimed=false, so a transient skip is never requeued — requeueing
// an unclaimed task would rewrite next_run_at=now and make the next scan fire
// the task early). A busy target under reject/enqueue is a quiet skip, same as
// a sweep; interrupt cancels the active run first.
func (tr *Trigger) FireNow(ctx context.Context, task Task) error {
	fireCtx, cancel := context.WithTimeout(ctx, tr.fireTimeout)
	defer cancel()
	return tr.submit(fireCtx, task, false)
}

// submit builds the run environment for a claimed task and hands it to the
// registry. It is the construction half of the "unattended chatapi" (design:
// run construction). Failure policy splits transient from persistent: a
// transient skip (pending interaction, budget exceeded) requeues so the next
// scan retries — but ONLY when the fire claimed the task (claimed=true); an
// unclaimed FireNow must never touch next_run_at, or the manual run would
// rewrite the cadence. A persistent failure (unresolvable prompt source or
// loop build) leaves the claimed slot advanced and lets the NEXT CRON
// OCCURRENCE retry — requeueing a misconfigured task would hot-loop the scan
// (create/delete session churn every interval) until an operator fixes it.
func (tr *Trigger) submit(ctx context.Context, task Task, claimed bool) error {
	// Resolve the prompt source into a system prompt, model, and opening turn.
	// A resolution failure (unknown agent def) is persistent — the claimed slot
	// stays advanced for the next cron occurrence (see the failure policy
	// above), never requeued.
	system, model, kickoff, err := tr.resolvePrompt(ctx, task)
	if err != nil {
		return err
	}

	// Billing attribution (P1-3): the team whose key pays for this task owner's
	// run, and the model the loop runs — same stamp a human chat run gets.
	var teamID string
	if tr.attributor != nil {
		teamID = tr.attributor(ctx, task.UserID)
	}
	// Budget gate (P1-1): skip this firing when the owner's (or billing team's)
	// monthly budget is met. Fail-open inside the checker means an error here is
	// a real limit: the budget-exceeded skip is requeued so the next scan
	// retries once the window rolls or the budget is raised — one over-budget
	// task must not stall the others, and the claimed slot must not burn a day.
	// A non-budget error surfaces as a fire failure (sweep logs it) and is left
	// claimed: an unhealthy checker must not hot-loop the scan.
	//
	// The gate runs BEFORE the session is resolved: an over-budget firing is
	// requeued every scan (next_run_at = now), and without this a fresh task
	// would create and delete a tagged session on every 30s scan until the
	// window rolls or the budget is raised — churn for zero work.
	if tr.budgetGate != nil {
		if err := tr.budgetGate(ctx, task.UserID, teamID); err != nil {
			if errors.Is(err, quota.ErrBudgetExceeded) {
				tr.log.Warn("schedule: budget exceeded, skipping firing", "task", task.ID, "user", task.UserID, "team", teamID, "err", err)
				if claimed {
					tr.requeue(ctx, task)
				}
				return nil
			}
			return err
		}
	}

	// Resolve or create the target session (design D2).
	sessID, fresh, err := tr.resolveSession(ctx, task, kickoff)
	if err != nil {
		return err
	}

	// Concurrency strategy: what to do about an already-active run on the
	// target session (design: multitask).
	proceed, interrupted, err := tr.gateMultitask(ctx, task, sessID)
	if err != nil || !proceed {
		if !proceed && fresh {
			tr.cleanupFreshSession(ctx, task, sessID)
		}
		return err
	}

	// Pending-interaction gate (capability suspend-batch-snapshot): a session
	// with undecided interactions rejects new submissions — a scheduled firing
	// must not bury a human's pending approval either. Skip this firing and
	// requeue it so the next scan retries (a claim already advanced the
	// schedule). Fail-open on a store error, mirroring the budget gate.
	if pending, err := tr.registry.PendingApprovalsForSession(ctx, sessID); err == nil && len(pending) > 0 {
		tr.log.Info("schedule: session has pending interactions, skipping firing", "task", task.ID, "session", sessID, "pending", len(pending))
		tr.cleanupFreshSession(ctx, task, sessID)
		tr.requeue(ctx, task)
		return nil
	}

	// Build the whitelisted loop and submit through the shared registry — from
	// here on it is byte-identical to a human chat run. A loop build failure
	// (unresolvable provider/model) is a PERSISTENT misconfiguration: leave the
	// claimed slot advanced and let the next cron occurrence retry once the
	// operator fixes the reference — requeueing would hot-loop the scan (build
	// fails again 30s later, session churn every interval).
	loop, err := tr.buildLoop(ctx, task, system, model)
	if err != nil {
		if fresh {
			tr.cleanupFreshSession(ctx, task, sessID)
		}
		tr.log.Warn("schedule: loop build failed", "task", task.ID, "err", err)
		return err
	}
	if tr.bindTools != nil {
		tr.bindTools(ctx, loop, sessID, task.ToolWhitelist)
	}
	userMsg := provider.TextMessage(provider.RoleUser, kickoff)
	run, err := tr.registry.Submit(ctx, sessID, session.RunWork{
		Loop:        loop,
		History:     []provider.Message{userMsg}, // the worker runs History verbatim; it does not merge UserMessage in
		UserMessage: &userMsg,
		TeamID:      teamID,
		Model:       loop.Model(),
	})
	if err != nil {
		if errors.Is(err, session.ErrRunActive) {
			// multitask=reject raced an active run; skip without error. When the
			// fire was an INTERRUPT that timed out (CancelAndWait's bound), the
			// pre-empted worker is still unwinding and the claim already
			// advanced — requeue so the next scan retries, instead of silently
			// dropping this firing until the next cron occurrence. An
			// unclaimed FireNow is never requeued: its schedule was never
			// touched, so rewriting next_run_at would fire the task early.
			if interrupted {
				tr.log.Warn("schedule: interrupt timed out, worker still active; requeueing firing", "task", task.ID, "session", sessID)
				if claimed {
					tr.requeue(ctx, task)
				}
			}
			tr.cleanupFreshSession(ctx, task, sessID)
			return nil
		}
		tr.cleanupFreshSession(ctx, task, sessID)
		return err
	}

	tr.log.Info("schedule: fired", "task", task.ID, "session", sessID, "run", run.ID)

	// on_run_completed = delete: remove a freshly-created session once the run
	// reaches a terminal state. The registry's RunDoneHook performs the
	// deletion when the run settles, so submit stays fast and a run that
	// outlives any fixed poll window still gets its session cleaned up.
	if fresh && task.OnRunCompleted == OnRunDelete {
		tr.watchRunDelete(run.ID, sessID)
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
		sess, err := tr.runtime.GetSession(ctx, task.TargetSessionID)
		if err != nil {
			return "", false, err
		}
		// Ownership gate (IDOR, mirror of inbound.Dispatcher): a task may only
		// fire into its OWNER's sessions. A task pointing at someone else's
		// session would read/write that user's conversation and workspace —
		// and MultitaskInterrupt could even cancel their running job.
		if sess.UserID != task.UserID {
			return "", false, fmt.Errorf("target session %s belongs to user %s, not task owner %s", task.TargetSessionID, sess.UserID, task.UserID)
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

// interruptWaitTimeout bounds how long the interrupt branch waits for the
// pre-empted run's worker to unwind before submitting the new run.
const interruptWaitTimeout = 3 * time.Second

// gateMultitask applies the task's concurrency strategy against an active run
// on the target session. It reports whether to proceed with the fire, and —
// when the fire chose to interrupt — that an interrupt cancel was issued
// (interrupted=true), so submit can requeue an interrupt that races a worker
// which did not unwind within the wait bound.
func (tr *Trigger) gateMultitask(ctx context.Context, task Task, sessID string) (proceed bool, interrupted bool, err error) {
	_, active, err := tr.runtime.ActiveRun(ctx, sessID)
	if err != nil {
		return false, false, err
	}
	if !active {
		return true, false, nil
	}
	switch task.Multitask {
	case MultitaskInterrupt:
		// A REAL interrupt: cancel the worker's run context (the registry's
		// own path — the runtime's lock alone would leave the old worker
		// executing, interleaving its frames with the new run's stream) and
		// wait for the worker goroutine to unwind before submitting. The
		// worker settles the interrupted run cancelled itself; one that fails
		// to exit within the timeout is left to unwind on its own, and the
		// submit below races it (ErrRunActive → requeue).
		tr.registry.CancelAndWait(sessID, interruptWaitTimeout)
		return true, true, nil
	case MultitaskEnqueue:
		// The run registry enforces single-active-run; an enqueue is realised by
		// simply letting Submit fail with ErrRunActive and skipping — the next
		// occurrence fires after the active run drains. Treat as skip for now.
		tr.log.Info("schedule: enqueue skipped (busy)", "task", task.ID, "session", sessID)
		return false, false, nil
	default: // reject
		tr.log.Info("schedule: fire skipped, session busy", "task", task.ID, "session", sessID)
		return false, false, nil
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

// requeue pushes a claimed-but-skipped task's next_run_at back to now so the
// next scan retries it. Claim already advanced the schedule; a transient
// pre-submit skip (budget gate, pending interaction) must not burn the slot
// the claim consumed — a daily task would otherwise wait 24h for its next
// chance. The store re-checks the claimed state (task still enabled,
// unexpired, and its next_run_at untouched by an operator edit since the
// claim), so a requeue is a no-op if the schedule changed underneath us.
// Best-effort: a requeue failure is logged, never allowed to mask the skip
// (the sweep already logged its reason).
func (tr *Trigger) requeue(ctx context.Context, task Task) {
	if err := tr.store.RequeueDue(ctx, task, tr.now()); err != nil {
		tr.log.Warn("schedule: requeue skipped task failed", "task", task.ID, "err", err)
	}
}

// watchRunDelete records a freshly-created session whose run's completion
// must delete it (on_run_completed = delete). The RunDoneHook registered in
// NewTrigger performs the deletion when the run settles, so submit stays fast
// and the delete follows the run's actual lifetime.
func (tr *Trigger) watchRunDelete(runID, sessID string) {
	tr.ddMu.Lock()
	defer tr.ddMu.Unlock()
	tr.deleteOnDone[runID] = sessID
}

// onRunDone is the registry's RunDoneHook: a settled run whose session is
// registered for delete-on-completion removes the session. It runs on its own
// goroutine, is fire-and-forget (a delete failure logs, never propagates),
// and must tolerate the run context being cancelled — so the delete runs on
// an uncancelled view of the context. A non-terminal notification (the hook
// contract is terminal-only) leaves the entry registered.
func (tr *Trigger) onRunDone(ctx context.Context, sessionID string, run session.Run, status session.RunStatus) {
	switch status {
	case session.RunDone, session.RunFailed, session.RunCancelled:
	default:
		return
	}
	tr.ddMu.Lock()
	sessID, ok := tr.deleteOnDone[run.ID]
	if ok {
		delete(tr.deleteOnDone, run.ID)
	}
	tr.ddMu.Unlock()
	if !ok || tr.db == nil {
		return
	}
	if _, err := tr.db.ExecContext(context.WithoutCancel(ctx), `DELETE FROM sessions WHERE id = $1`, sessID); err != nil {
		tr.log.Warn("schedule: on_run_completed delete failed", "session", sessID, "err", err)
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
