package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/provider"
)

// registryEmitter adapts agent.Emitter to the registry's persist+publish write
// path. The loop emits through it; each event is persisted to the durable log and
// fanned out to attached clients by the Runtime's AppendEvent.
type registryEmitter struct {
	rg        *RunRegistry
	sessionID string
	runID     string
	// pending carries the pre-provisioned message ids the step-intent
	// middlewares wrote before their effects; the KindMessage persist path
	// consumes them so messages land with exactly the provisioned ids.
	pending *stepIntentQueue
	// toolMW is the tool-intent middleware of this run, so the tool-result
	// persist path can reset its shared batch id after the batch's message.
	toolMW *toolIntentMW
	// cancelledPersisted records that this emitter landed the terminal
	// KindCancelled frame, so the run worker's compensation (which covers the
	// paths that never emitted it) stays single-written rather than
	// duplicating the frame in the durable log.
	cancelledPersisted atomic.Bool
	// terminalErr latches the run's terminal KindError text (the exact string
	// the live client saw), so the run worker can attach it to the run's last
	// assistant message after the loop returns — run_events is not consulted
	// by history rebuild, so without this a failed run's error would vanish on
	// client reload. atomic.Value because Emit may be reached concurrently.
	terminalErr atomic.Value // string
}

// Emit persists the event (and fans it out). It honours ctx cancellation so a
// cancelled run unwinds rather than blocking on a dead write path.
func (e *registryEmitter) Emit(ctx context.Context, kind agent.EventKind, payload any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Assembled conversation messages go to the MessageStore for full-block
	// persistence. They are NOT written to run_events: KindMessage is a
	// persistence signal, not a render frame, and the durable run log already
	// carries the text/thinking/tool frames the UI replays.
	if kind == agent.KindMessage {
		e.persistMessage(ctx, payload)
		return nil
	}
	// The overflow-recovery signal records the once-per-input guard: an
	// overflow_compact step intent. Best-effort: the in-memory guard already
	// bounds the loop; the row is the durable audit trail and future resume
	// input. A failure is logged, not fatal — the run continues.
	if kind == agent.KindOverflowRecovery {
		// The discarded response's tokens were consumed (and reported live via
		// KindUsage) but no message persisted, so record them on the usage
		// ledger — the only durable accounting for the truncated attempt. Its
		// step intent is popped with it (the retry's BeforeModel provisions a
		// fresh id; the discarded attempt's provisioned id must not linger in
		// the pending queue).
		in := e.pending.popAssistant()
		if u, ok := payload.(*provider.Usage); ok && u != nil {
			if err := e.rg.rt.store.AppendUsageRecord(context.Background(), UsageRecord{
				RunID: e.runID, Cause: UsageOverflow, Attempt: in.attempt, Usage: *u,
			}); err != nil {
				slog.Warn("record overflow usage ledger", "run", e.runID, "err", err)
			}
		}
		if _, err := e.rg.appendStep(context.Background(), e.sessionID, e.runID, StepOverflowCompact, "", nil); err != nil {
			slog.Warn("record overflow recovery", "run", e.runID, "err", err)
		}
		return nil
	}
	// The interrupt frame carries the interaction id the client POSTs its verdict
	// with. Persist the durable Interaction row BEFORE publishing the frame, so a
	// fast client (an instant client-tool auto-run) can never learn an id it can't
	// yet resolve (which would 404 on resume). Synchronous, and ahead of the
	// append below: once any client can see the frame, the row exists. A failure
	// is returned so the loop fails the run instead of surfacing a phantom prompt.
	if kind == agent.KindInterrupt {
		if err := e.persistInteraction(payload); err != nil {
			return err
		}
	}
	// The run's aggregate usage is recorded on the runs row, recomputed from
	// the usage ledger (SumUsage); the event still flows to the durable log.
	if kind == agent.KindUsage {
		// Descendant (subagent) usage never reaches the ledger: child loops
		// emit into black-box emitters, so no message rows exist under this
		// run for them. The root's terminal KindUsage payload already folds
		// the descendants' total in (UsageMW), so record that subtree
		// complement here as a UsageRun row. The payload itself is NOT
		// written — it also contains this run's own usage, which the ledger
		// already carries via the assistant/tool/overflow rows, so writing it
		// would double count. Only the ROOT run does this (mirroring
		// UsageMW's scoped fold), so nested levels are never counted twice.
		if sc := agent.UsageScopeFrom(ctx); sc != nil && sc.IsRoot() {
			if u := sc.Total(); u != (provider.Usage{}) {
				if err := e.rg.rt.store.AppendUsageRecord(context.Background(), UsageRecord{
					RunID: e.runID, Cause: UsageRun, Usage: u,
				}); err != nil {
					slog.Warn("record descendant usage ledger", "run", e.runID, "err", err)
				}
			}
		}
		e.persistRunUsage(payload)
	}
	// The terminal cancelled frame must be KNOWN-landed: the run worker
	// compensates only when this persist failed, so record a successful append
	// to keep the durable log single-written.
	if kind == agent.KindCancelled {
		if err := e.rg.appendEvent(ctx, e.sessionID, e.runID, kind, payload); err == nil {
			e.cancelledPersisted.Store(true)
		}
		return nil
	}
	if kind == agent.KindError {
		if s, ok := payload.(string); ok && s != "" {
			e.terminalErr.Store(s)
		}
	}
	e.rg.append(ctx, e.sessionID, e.runID, kind, payload)
	return nil
}

// terminalErrorText returns the run's terminal error text the loop emitted (the
// exact string attached clients saw), or "" for a run that did not end on an
// error.
func (e *registryEmitter) terminalErrorText() string {
	if v := e.terminalErr.Load(); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// persistInteraction writes the durable Interaction row for a KindInterrupt
// frame BEFORE the frame is published, closing the resume race (a client must
// never learn an interaction id it cannot yet resolve). The payload is the
// loop's agent.Interaction; its id was generated when the gate was detected, so
// it matches the id the frame carries. A persistence failure is returned so the
// emit — and thus the run — fails, rather than a prompt reaching the client with
// no backing row.
func (e *registryEmitter) persistInteraction(payload any) error {
	var in agent.Interaction
	switch v := payload.(type) {
	case agent.Interaction:
		in = v
	case *agent.Interaction:
		if v == nil {
			return fmt.Errorf("interrupt payload is a nil *agent.Interaction")
		}
		in = *v
	default:
		return fmt.Errorf("interrupt payload is not an agent.Interaction: %T", payload)
	}
	input, err := json.Marshal(in.Input)
	if err != nil {
		input = []byte("{}")
	}
	kind := in.Kind
	if kind == "" {
		kind = "approval"
	}
	// The suspended-batch snapshot: the full ordered batch the gate suspended on,
	// persisted in the same transaction as the first interaction row (idempotent
	// across the batch's frames). A hand-constructed payload without Batch
	// degenerates to the single gated call.
	batchIDs := make([]string, 0, len(in.Batch))
	for _, c := range in.Batch {
		if c.ID != "" {
			batchIDs = append(batchIDs, c.ID)
		}
	}
	if len(batchIDs) == 0 && in.ToolCallID != "" {
		batchIDs = []string{in.ToolCallID}
	}
	ap, err := e.rg.rt.store.CreateInteractionBatch(context.Background(), SuspendedBatch{
		RunID: e.runID, SessionID: e.sessionID, ToolCallIDs: batchIDs,
	}, Interaction{
		ID: in.ID, RunID: e.runID, SessionID: e.sessionID,
		ToolCallID: in.ToolCallID, ToolName: in.ToolName,
		Payload: input, Kind: kind,
	})
	if err != nil {
		slog.Error("persist interaction failed; failing run", "session", e.sessionID, "run", e.runID, "err", err)
		return err
	}
	slog.Info("run ended awaiting client interaction", "session", e.sessionID, "run", e.runID, "interaction", ap.ID, "kind", kind, "tool", ap.ToolName)
	return nil
}

// persistRunUsage recomputes the run's aggregate usage from the usage ledger
// (SumUsage) and records it on the runs row — the per-request ledger rows were
// written at settle time, so this recomputation never loses spend. Best-effort:
// failures are logged, not fatal.
func (e *registryEmitter) persistRunUsage(_ any) {
	u, err := e.rg.rt.store.SumUsage(context.Background(), e.runID)
	if err != nil {
		slog.Warn("sum run usage", "run", e.runID, "err", err)
		return
	}
	if err := e.rg.rt.store.SetRunUsage(context.Background(), e.runID, u); err != nil {
		slog.Warn("record run usage", "run", e.runID, "err", err)
	}
}

// persistMessage stores one assembled message in the MessageStore. The payload
// is either an agent.MessageWithUsage (an assistant message paired with the
// usage of the LLM call that produced it) or a bare provider.Message (a tool
// result, which is not an LLM call and has no usage). For assistant messages
// the usage ledger row is written FIRST (bound to the step intent's
// pre-provisioned message id), then the message row with exactly that id — the
// ledger's cost durability never depends on the message's. Persistence is
// best-effort: a failure is logged but does not abort the run (the run's render
// stream is unaffected).
func (e *registryEmitter) persistMessage(ctx context.Context, payload any) {
	if e.rg.msgStore == nil {
		return
	}
	var msg provider.Message
	var usage *provider.Usage
	switch v := payload.(type) {
	case agent.MessageWithUsage:
		msg, usage = v.Message, v.Usage
	case provider.Message:
		msg = v
	default:
		// Tolerate a marshalled-then-decoded message (map shape) too.
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			return
		}
	}
	// Use a background context: by the time the final assistant message is
	// emitted the run ctx may already be cancelled, and we still want the
	// message persisted (mirrors the terminal-event re-publish on a live ctx).
	bg := context.Background()

	// Assistant messages: consume the step intent's provisioned id (if any),
	// write the usage ledger row bound to it, then insert the message with
	// exactly that id.
	if msg.Role == provider.RoleAssistant {
		in := e.pending.popAssistant()
		if usage != nil {
			rec := UsageRecord{RunID: e.runID, Cause: UsageAssistant, Attempt: in.attempt, Usage: *usage}
			if in.messageID != nil {
				rec.ResultMessageID = in.messageID
			}
			if err := e.rg.rt.store.AppendUsageRecord(bg, rec); err != nil {
				slog.Warn("record usage ledger", "run", e.runID, "err", err)
			}
		}
		stored := StoredMessage{
			SessionID: e.sessionID,
			RunID:     e.runID,
			Role:      msg.Role,
			Content:   contextmgmt.TruncateBlocksForPersistence(msg.Content),
			Usage:     usage,
		}
		if in.messageID != nil {
			stored.ID = *in.messageID
		}
		if _, err := e.rg.msgStore.AppendMessage(bg, stored); err != nil {
			slog.Warn("persist assistant message", "session", e.sessionID, "run", e.runID, "role", msg.Role, "err", err)
		}
		return
	}

	// Tool-result messages: the batch's tool intents shared one provisioned id;
	// insert with it. The batch's shared id resets for the next batch.
	toolID := e.pending.popTools()
	if e.toolMW != nil {
		e.toolMW.resetBatch()
	}
	stored := StoredMessage{
		SessionID: e.sessionID,
		RunID:     e.runID,
		Role:      msg.Role,
		Content:   contextmgmt.TruncateBlocksForPersistence(msg.Content),
	}
	if toolID != nil {
		stored.ID = *toolID
	}
	if _, err := e.rg.msgStore.AppendMessage(bg, stored); err != nil {
		slog.Warn("persist tool message", "session", e.sessionID, "run", e.runID, "role", msg.Role, "err", err)
	}
}
