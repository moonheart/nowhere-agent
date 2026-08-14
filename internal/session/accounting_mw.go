package session

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/toolruntime"
)

// stepIntentMW writes the durable assistant step intent BEFORE each provider
// request (change durable-run-accounting): the intent row carries the durable
// attempt count and the pre-provisioned id of the message the request is
// expected to produce. The registry installs it on every run's loop; the id
// rides to the emitter's pending queue, which the KindMessage persist path
// consumes. A failed intent write aborts the run (ErrAbortRun) rather than
// letting an effect start with no durable intent.
type stepIntentMW struct {
	rg        *RunRegistry
	sessionID string
	runID     string
	pending   *stepIntentQueue
}

var _ agent.BeforeModelHook = (*stepIntentMW)(nil)

// MiddlewareName identifies the middleware in the chain.
func (m *stepIntentMW) MiddlewareName() string { return "step-intent" }

// BeforeModel writes the assistant step intent for the upcoming provider call.
func (m *stepIntentMW) BeforeModel(ctx context.Context, _ *agent.RunState) error {
	st, err := m.rg.appendStep(ctx, m.sessionID, m.runID, StepAssistant, "", nil)
	if err != nil {
		return fmt.Errorf("write assistant step intent: %w", err)
	}
	m.pending.push(stepIntent{kind: StepAssistant, messageID: st.ResultMessageID, attempt: st.Attempt})
	return nil
}

// toolIntentMW writes the durable tool step intent BEFORE each tool call
// executes (change durable-run-accounting). A parallel batch's calls share one
// provisioned id — their results land in one tool-result message — so the
// first call provisions and the rest reuse. A blocked or invalid call never
// executes, so it never passes through here and writes no intent (matching the
// rule that only real effects carry intents). A failed intent write skips the
// call with an error result: the effect must not start without its intent.
type toolIntentMW struct {
	rg        *RunRegistry
	sessionID string
	runID     string
	pending   *stepIntentQueue

	mu      sync.Mutex
	batchID *int64 // shared provisioned id for the current parallel batch
}

var _ agent.ToolCallMiddleware = (*toolIntentMW)(nil)

// MiddlewareName identifies the middleware in the chain.
func (m *toolIntentMW) MiddlewareName() string { return "tool-intent" }

// WrapToolCall writes the intent and then executes. The batch's shared id is
// decided atomically: exactly ONE caller sees batchID == nil and provisions it
// (appendStep, holding the lock across the provision); every later caller
// reuses the decided id. Racing parallel dispatches must never each provision
// (two ids would leave a dangling second intent that recovery misreads as an
// interrupted step), and the deciding caller must not append twice (a duplicate
// step row for one call). A failed provision leaves batchID nil so the next
// caller takes the first-caller role.
func (m *toolIntentMW) WrapToolCall(ctx context.Context, c *agent.ToolCall, next agent.ToolHandler) toolruntime.Result {
	m.mu.Lock()
	shared := m.batchID
	if shared == nil {
		st, err := m.rg.appendStep(ctx, m.sessionID, m.runID, StepTool, c.Call.ID, nil)
		if err != nil {
			m.mu.Unlock()
			slog.Warn("write tool step intent failed; skipping tool", "run", m.runID, "tool", c.Call.Name, "err", err)
			return toolruntime.Result{
				Content: "not executed: the run could not record this tool call's intent (durable accounting failure)",
				IsError: true,
			}
		}
		m.batchID = st.ResultMessageID
		m.mu.Unlock()
		m.pending.push(stepIntent{kind: StepTool, messageID: st.ResultMessageID, attempt: st.Attempt})
		return next(ctx, c)
	}
	m.mu.Unlock()
	st, err := m.rg.appendStep(ctx, m.sessionID, m.runID, StepTool, c.Call.ID, shared)
	if err != nil {
		slog.Warn("write tool step intent failed; skipping tool", "run", m.runID, "tool", c.Call.Name, "err", err)
		return toolruntime.Result{
			Content: "not executed: the run could not record this tool call's intent (durable accounting failure)",
			IsError: true,
		}
	}
	m.pending.push(stepIntent{kind: StepTool, messageID: st.ResultMessageID, attempt: st.Attempt})
	return next(ctx, c)
}

// resetBatch clears the shared batch id after the batch's result message is
// persisted, so the next batch provisions a fresh id.
func (m *toolIntentMW) resetBatch() {
	m.mu.Lock()
	m.batchID = nil
	m.mu.Unlock()
}

// stepIntent is one pending provisioned result id, FIFO-ordered.
type stepIntent struct {
	kind      StepKind
	messageID *int64
	attempt   int
}

// stepIntentQueue is the emitter-side FIFO of provisioned ids written by the
// step-intent middlewares. The loop and the persist path run on the same
// goroutine (except parallel tool intents, which the tool middleware
// serializes through the store), so a mutex is belt-and-braces.
type stepIntentQueue struct {
	mu  sync.Mutex
	ids []stepIntent
}

func (q *stepIntentQueue) push(in stepIntent) {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.ids = append(q.ids, in)
	q.mu.Unlock()
}

// popAssistant takes the newest assistant intent (each assistant step writes
// exactly one and its message follows immediately).
func (q *stepIntentQueue) popAssistant() stepIntent {
	if q == nil {
		return stepIntent{}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := len(q.ids) - 1; i >= 0; i-- {
		if q.ids[i].kind == StepAssistant {
			in := q.ids[i]
			q.ids = append(q.ids[:i], q.ids[i+1:]...)
			return in
		}
	}
	return stepIntent{}
}

// popTools drains every pending tool intent (one parallel batch) and returns
// the batch's shared id (the first provisioned one), or nil.
func (q *stepIntentQueue) popTools() *int64 {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	var shared *int64
	kept := q.ids[:0]
	for _, in := range q.ids {
		if in.kind == StepTool {
			if shared == nil {
				shared = in.messageID
			}
		} else {
			kept = append(kept, in)
		}
	}
	q.ids = kept
	return shared
}
