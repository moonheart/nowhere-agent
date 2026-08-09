// Package agent implements the agent-loop capability (design D1): a self-built
// think→tool→think loop. It owns orchestration, tool dispatch, streaming, and
// the in-context short-term memory, driving a provider.Adapter and emitting
// canonical events that the session runtime persists and fans out.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"

	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// ApprovalReasonPrefix marks a Permission deny-reason as "gated for human
// approval" rather than a hard deny. The server's permission callback prefixes
// the reason for calls whose policy verdict is Ask; the loop distinguishes
// those (end the run + ask the user) from a true deny (feed an error to the
// model).
const ApprovalReasonPrefix = "approval required: "

// IsApprovalReason reports whether a Permission deny-reason is a gate-for-
// approval marker (vs a hard deny).
func IsApprovalReason(reason string) bool {
	return strings.HasPrefix(reason, ApprovalReasonPrefix)
}

// Interaction describes the single client-interaction tool call that ended a
// run (general interrupt, capability O2 + O-ask + client-side tools). The loop
// emits it (KindInterrupt) and finishes; the run's worker persists it as a
// durable Interaction (thread state) and surfaces it to the client. The run
// does NOT suspend — a fresh run applies the result later. Kind distinguishes
// the interaction kinds (a permission approval, an ask_user question set, a
// client-side tool).
type Interaction struct {
	// ID is the durable interaction's id, generated the moment the gate is
	// detected (LangGraph-style: the interrupt's id is known before it is
	// surfaced). The loop emits it on the KindInterrupt frame and the run worker
	// persists the Interaction row with the SAME id, so the client's card can POST
	// its verdict without a refresh or a store lookup.
	ID string
	// Kind is the open interaction kind ("approval" | "ask_user" | "client_tool" |
	// ...). Empty means approval (the O2 default).
	Kind       string
	ToolCallID string
	ToolName   string
	Input      map[string]any
	// Batch is the FULL tool-call batch this interaction belongs to (gated and
	// ungated siblings alike), in assistant-message block order. The run worker
	// persists it as the suspended-batch snapshot, so a later fold resolves the
	// batch from durable state rather than re-deriving it from history.
	Batch []toolruntime.Call
}

// ApprovalRequest is retained as an alias of Interaction for source
// compatibility during the transition to the general-interrupt model.
type ApprovalRequest = Interaction

// AskUserToolName is the built-in tool the model calls to ask the user
// structured questions (capability O-ask). Like a permission approval, calling
// it ends the run for human input; the answer arrives via a later run.
const AskUserToolName = "ask_user"

// EventKind classifies loop events persisted by the session runtime.
type EventKind string

const (
	KindText       EventKind = "text"
	KindThinking   EventKind = "thinking"
	KindToolUse    EventKind = "tool_use"
	KindToolResult EventKind = "tool_result"
	// KindToolArgs carries one incremental fragment of a tool call's arguments
	// (payload: {id, name, delta}) as the model streams them, so the client can
	// render a large tool input (e.g. a 10k-token write_file) while it generates
	// instead of only at block-stop. Live-only content (broker-routed, never
	// persisted): the durable record keeps the COMPLETE args on KindToolUse, so
	// history replay is unaffected.
	KindToolArgs EventKind = "tool_args"
	KindError    EventKind = "error"
	KindDone     EventKind = "done"
	// KindMessage carries a fully-assembled conversation message (payload:
	// provider.Message) so the run path can persist it in original block form.
	// It is emitted once per completed message: each assistant message and each
	// tool-result message. It is a persistence signal, not a render frame.
	KindMessage EventKind = "message"
	// KindCancelled marks a run stopped early (client Stop / server cancel). It
	// is persisted so replay/history can tell a cancelled run from a finished
	// one. The loop emits it detached from the cancelled ctx (emitCancelled),
	// so the frame itself lands rather than relying on a downstream
	// compensation; the session registry re-publishes only if that emit was
	// dropped.
	KindCancelled EventKind = "cancelled"
	// KindUser marks a persisted user message. It is not emitted by the loop
	// itself; the transport writes it so replay reconstructs the user side.
	KindUser EventKind = "user"
	// KindRunning marks the moment a run starts. The transport persists it as the
	// run's first event so an attached client (a second tab/device on the same
	// session) learns a run began — both live (via fan-out) and on replay.
	KindRunning EventKind = "running"
	// KindSubagent carries a subagent activity signal (a child loop's phase/tool)
	// for the run panel. It is a live-only content event (broker-routed, never
	// persisted): a UI progress hint, not part of the conversation record.
	KindSubagent EventKind = "subagent"
	// KindUsage carries a run's accumulated token usage (payload: provider.Usage)
	// once, at natural termination. It is persisted and fanned out so the
	// transport's finish frame can report real token counts instead of zeros.
	KindUsage EventKind = "usage"
	// KindInterrupt carries the client-interaction tool call that ended the run
	// (payload: Interaction). Live-only content (broker-routed, never persisted):
	// the run's worker separately persists the durable Interaction record the
	// decision endpoint reads. One unified frame for every interaction kind —
	// approval, ask_user, client-side tool.
	KindInterrupt EventKind = "interrupt"
	// KindApprovalRequest is the pre-generalization name for KindInterrupt, kept
	// as an alias so existing emitters/consumers keep working during the
	// transition. Emit and match on KindInterrupt; treat them interchangeably.
	KindApprovalRequest = KindInterrupt
	// KindStepStart opens a new think→tool step (one model iteration after the
	// first). Live-only content (broker-routed, never persisted): a render hint so
	// the transport can emit a start-step frame for multi-iteration runs.
	KindStepStart EventKind = "step_start"
	// KindStepFinish closes a step (payload: StepEvent) carrying that iteration's
	// stop reason, usage, and whether another step follows. Live-only content, so
	// the transport can emit a finish-step frame with real per-step usage.
	KindStepFinish EventKind = "step_finish"
)

// Emitter receives loop events (the session runtime persists + fans them out).
type Emitter interface {
	Emit(ctx context.Context, kind EventKind, payload any) error
}

// Config controls the loop. It holds true configuration only; cross-cutting
// concerns (compression, memory injection, image materialization, overflow
// retry, usage, tool authorization) are middleware registered via Use.
type Config struct {
	Model           string
	System          string
	MaxTokens       int
	MaxIterations   int // guard against infinite loops
	CacheablePrefix bool
}

// Loop runs the think→tool→think cycle.
type Loop struct {
	provider provider.Adapter
	tools    *toolruntime.Registry
	config   Config
	// middleware is the registered cross-cutting chain, in registration order.
	// It is partitioned into the hook slices below at Use time.
	middleware []Middleware
	before     []BeforeModelHook
	afterModel []AfterModelHook
	afterRun   []AfterRunHook
	modelWrap  []ModelCallMiddleware
	toolWrap   []ToolCallMiddleware
	// gateInteraction/gateExecute authorize tool calls at the interaction gate
	// (end the run for human input) and the execution gate (block dispatch).
	// First registered wins; both default to allow-all when no middleware
	// registers the hook.
	gateInteraction GateFunc
	gateExecute     GateFunc
	// PendingInteraction, when set after Run returns, is the client-interaction
	// tool call that ended the run. The run worker reads it to persist the durable
	// Interaction. With a multi-call gated batch this is the FIRST (queue head);
	// PendingInteractions carries the whole batch.
	PendingInteraction *Interaction
	// PendingInteractions, when non-empty after Run returns, is the full gated
	// batch that ended the run (in order). Each element is persisted as its own
	// durable Interaction row; the run resumes only once all are resolved.
	PendingInteractions []*Interaction
	// PendingApproval is the pre-generalization name for PendingInteraction. It
	// points at the same value (set alongside it) for source compatibility.
	PendingApproval *Interaction
}

// New creates a Loop. UsageMW is registered by default so each run reports its
// accumulated token usage via KindUsage at termination (the pre-middleware
// behaviour); callers add further middleware with Use.
func New(p provider.Adapter, tools *toolruntime.Registry, cfg Config) *Loop {
	if cfg.MaxIterations == 0 {
		cfg.MaxIterations = 25
	}
	l := &Loop{provider: p, tools: tools, config: cfg}
	return l.Use(&UsageMW{})
}

// Use registers middleware in order. Per turn the chain runs: BeforeModel
// first→last, WrapModelCall nested (first registered outermost), AfterModel
// last→first; WrapToolCall nests likewise; AfterRun runs last→first once at run
// end. A GateFuncProvider supplies the tool-authorization policy — the FIRST
// registered provider wins (later ones are ignored); its single func governs
// both the interaction and execution gates. Use returns the loop for chaining.
func (l *Loop) Use(mw ...Middleware) *Loop {
	for _, m := range mw {
		if m == nil {
			continue
		}
		l.middleware = append(l.middleware, m)
		if h, ok := m.(BeforeModelHook); ok {
			l.before = append(l.before, h)
		}
		if h, ok := m.(AfterModelHook); ok {
			l.afterModel = append(l.afterModel, h)
		}
		if h, ok := m.(AfterRunHook); ok {
			l.afterRun = append(l.afterRun, h)
		}
		if h, ok := m.(ModelCallMiddleware); ok {
			l.modelWrap = append(l.modelWrap, h)
		}
		if h, ok := m.(ToolCallMiddleware); ok {
			l.toolWrap = append(l.toolWrap, h)
		}
		if h, ok := m.(GateFuncProvider); ok {
			if l.gateInteraction != nil {
				// First registered wins; a later provider would otherwise be
				// silently dropped, hiding a wiring mistake.
				slog.Warn("agent: GateFuncProvider ignored; a policy is already registered",
					"middleware", m.MiddlewareName())
				continue
			}
			gate := h.GateCheck()
			l.gateInteraction = gate
			l.gateExecute = gate
		}
	}
	return l
}

// WithTools replaces the loop's tool registry. It is called once per run, after
// the loop is built, when the session (and thus its sandbox-bound tools) is
// known. Nil is ignored, leaving the existing registry in place.
func (l *Loop) WithTools(reg *toolruntime.Registry) *Loop {
	if reg != nil {
		l.tools = reg
	}
	return l
}

// Tools returns the loop's tool registry, so a caller that needs to execute a
// tool directly (e.g. an approved HITL call, executed by the decision path
// rather than the loop) can share the same session-bound registry.
func (l *Loop) Tools() *toolruntime.Registry {
	return l.tools
}

// Gate returns the tool-authorization policy registered via GateFuncProvider
// middleware (nil when none). The suspended-batch fold path re-applies it to
// un-gated sibling calls so a hard-deny that the dispatch screen would have
// enforced applies identically on resume (the two execution paths must agree).
func (l *Loop) Gate() GateFunc {
	return l.gateExecute
}

// Model returns the model the loop was configured with, for stamping on the run
// so usage reports can break down by model (enterprise-readiness P1-3).
func (l *Loop) Model() string { return l.config.Model }

// RegisterTool adds a tool to the loop's registry for this run. Used to attach
// client-declared tools (parsed from the request body) after the loop is built.
func (l *Loop) RegisterTool(t toolruntime.Tool) {
	l.tools.Register(t)
}

// toolDefs converts registered tools to provider tool definitions.
func (l *Loop) toolDefs() []provider.ToolDefinition {
	all := l.tools.All()
	defs := make([]provider.ToolDefinition, 0, len(all))
	for _, t := range all {
		defs = append(defs, provider.ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.Schema(),
		})
	}
	return defs
}

// Run executes the loop for a conversation history, streaming output to the
// emitter. It returns the final assembled assistant messages produced. The
// history is the short-term memory; it is not persisted to long-term memory.
//
// The returned tail is NOT guaranteed provider-sendable on every path: when
// the run ends on the interaction gate, the last produced message is the
// assistant message carrying the unanswered tool_use batch — its tool_results
// arrive only when a later run folds the resolved interactions back into
// history. Callers reusing the return value as a sendable history must run it
// through contextmgmt.EnsurePairing first (the registry, the primary caller,
// discards it and rebuilds history from the durable record).
//
// The loop works over a working view of the conversation (history + what it
// produces). Cross-cutting concerns (compression, memory injection, image
// materialization, overflow retry, usage) run as middleware around each model
// call, rewriting only the transient view — never the durable record (D1).
func (l *Loop) Run(ctx context.Context, history []provider.Message, emit Emitter) ([]provider.Message, error) {
	state := &RunState{Emit: emit}

	// Install the run tree's usage scope when the incoming context has none:
	// that makes this run the root, whose terminal KindUsage folds in every
	// descendant subagent loop's usage (see UsageScope). Nested loops inherit
	// the same scope and report only their own usage.
	if UsageScopeFrom(ctx) == nil {
		ctx = WithUsageScope(ctx, &UsageScope{root: true})
	}

	// Repair tool pairing ONCE, up front: a prior run's cancel or a persisted
	// compression split can leave history with an unpaired block, which the
	// provider rejects outright. Re-running the repair every iteration is wasted
	// work — the loop keeps the tail it produces paired by construction (every
	// tool batch is answered by recordToolResults on every path that continues
	// or ends the run: dispatch, pre-dispatch cancel, truncation, unreported
	// stop, early-end stop, interrupt-emit failure), so
	// the per-iteration rebuild only re-copied every block without ever
	// changing anything (O(iterations × messages) over a long agentic run).
	base := contextmgmt.EnsurePairing(append(append([]provider.Message{}, history...), state.Produced...))

	for iter := 0; iter < l.config.MaxIterations; iter++ {
		state.Iteration = iter
		// Honour cancellation between iterations (e.g. after a tool batch).
		if err := ctx.Err(); err != nil {
			l.reportTerminal(ctx, state)
			l.emitCancelled(ctx, emit)
			return state.Produced, err
		}
		// Open a new step for every iteration after the first, so the transport can
		// render a start-step frame between the tool batch and the next model call.
		if iter > 0 {
			_ = emit.Emit(ctx, KindStepStart, nil)
		}

		// Assemble the working view for this turn: the once-repaired base plus
		// the tail this run produced (paired by construction, per the invariant
		// above). A prefix the overflow fallback already dropped this run stays
		// dropped (viewDropped), or the next attempt would overflow on the
		// same rounds again.
		view := append(append([]provider.Message{}, base...), state.Produced...)
		if state.viewDropped > 0 && state.viewDropped < len(view) {
			view = view[state.viewDropped:]
		}
		state.View = view

		// Node hooks: observation before the model call (registration order).
		// A hook error is logged and skipped, EXCEPT a wrapped ErrAbortRun,
		// which aborts the run (settled like a provider failure).
		for _, h := range l.before {
			if err := h.BeforeModel(ctx, state); err != nil {
				if errors.Is(err, ErrAbortRun) {
					slog.Error("agent: BeforeModel hook aborted the run", "iter", iter, "err", err)
					l.emitStepFinish(ctx, emit, ModelResult{}, "error", false)
					l.runAfterRun(ctx, state)
					_ = emit.Emit(ctx, KindError, err.Error())
					return state.Produced, err
				}
				slog.Warn("agent: BeforeModel hook failed", "err", err)
			}
		}

		res, err := l.attempt(ctx, state, emit)
		if err != nil {
			if ctx.Err() != nil {
				slog.Info("agent: run cancelled", "iter", iter, "ctx_err", ctx.Err(), "attempt_err", err)
				// Close the step opened at iter>0 before the terminal frame,
				// symmetric with the error path below: a step must not dangle.
				// Detach from the cancelled ctx so the frame survives the
				// emitter's ctx guard (same rationale as reportTerminal);
				// "other" is the reason the transport maps cancellation to.
			l.emitStepFinish(context.WithoutCancel(ctx), emit, res, "other", false)
			l.reportTerminal(ctx, state)
			l.emitCancelled(ctx, emit)
			return state.Produced, ctx.Err()
			}
			slog.Error("agent: provider attempt failed; run aborting", "iter", iter, "err", err, "view_msgs", len(state.View))
			// Close the step before the terminal error frame, symmetric with
			// every other exit: a step opened at iter>0 must not dangle.
			l.emitStepFinish(ctx, emit, res, "error", false)
			l.runAfterRun(ctx, state)
			_ = emit.Emit(ctx, KindError, err.Error())
			return state.Produced, err
		}
		if res.Usage != nil {
			state.Usage.InputTokens += res.Usage.InputTokens
			state.Usage.OutputTokens += res.Usage.OutputTokens
			state.Usage.CacheReadTokens += res.Usage.CacheReadTokens
			state.Usage.CacheWriteTokens += res.Usage.CacheWriteTokens
		}
		// Drop content blocks that carry nothing (an empty text block, or a
		// thinking block with neither text nor signature): one serializes as an
		// empty block, which providers reject with a 400 on the next send — and
		// a turn whose ONLY block is empty would slip past the 0-length
		// empty-response guard below and persist as a hollow assistant message.
		res.Assistant.Content = dropEmptyBlocks(res.Assistant.Content)
		// Empty-response guard: a turn with no content blocks and no tool calls
		// carries nothing — no answer to show, no call to answer. Persisting it
		// would write an assistant message with empty content, which
		// OpenAI-compatible gateways reject with a 400 on the next send (the
		// up-front EnsurePairing repair patches the send view, but the empty row
		// stays durable). Fail loudly instead, same philosophy as the stop-reason
		// guards below.
		if len(res.Calls) == 0 && len(res.Assistant.Content) == 0 {
			emptyErr := fmt.Errorf("provider returned an empty response: no content, no tool calls (stop reason: %s)", stopReasonText(res.Stop))
			l.emitStepFinish(ctx, emit, res, "error", false)
			l.runAfterRun(ctx, state)
			_ = emit.Emit(ctx, KindError, emptyErr.Error())
			return state.Produced, emptyErr
		}
		state.Produced = append(state.Produced, res.Assistant)
		// Expose the assembled assistant message for full-block persistence
		// (persist-raw-messages), paired with the usage of the LLM call that
		// produced it. Emit failures here don't abort the run — the persistence
		// listener drops them — so ignore the error.
		//
		// This emit is commit-class: once the model's reply is assembled, the
		// tool_use blocks it carries WILL be answered (a cancel before the batch
		// records synthetic cancelled results — see the guard below), so the
		// assistant message must land too or the durable record keeps orphaned
		// tool_results. Detach from cancellation, symmetric with
		// recordToolResults.
		_ = emit.Emit(context.WithoutCancel(ctx), KindMessage, MessageWithUsage{Message: res.Assistant, Usage: res.Usage})

		// Node hooks: observation after the model call (reverse order).
		// ErrAbortRun aborts the run; the assistant message is already durable
		// (KindMessage above), so its tool batch must be answered first — same
		// pairing rationale as the pre-dispatch cancel guard below.
		for i := len(l.afterModel) - 1; i >= 0; i-- {
			if err := l.afterModel[i].AfterModel(ctx, state); err != nil {
				if errors.Is(err, ErrAbortRun) {
					slog.Error("agent: AfterModel hook aborted the run", "iter", iter, "err", err)
					if len(res.Calls) > 0 {
						l.recordToolResults(ctx, emit, state, res.Calls, abortedCallResults(res.Calls))
					}
					l.emitStepFinish(ctx, emit, res, "error", false)
					l.runAfterRun(ctx, state)
					_ = emit.Emit(ctx, KindError, err.Error())
					return state.Produced, err
				}
				slog.Warn("agent: AfterModel hook failed", "err", err)
			}
		}

		// No tool calls → the turn is final ONLY on a natural end_turn. Every
		// other stop reason is a failure shape: a max_tokens truncation, an
		// unreported stop reason (a cut-off answer or an unverifiable one), or
		// any other early end — stop_sequence, content_filter, pause_turn,
		// refusal, provider passthroughs — whose output likewise ended before
		// the model chose to stop. Surface all of them as errors instead of a
		// silent done (capability-gap L1): without this the loop treats
		// truncation as success.
		if len(res.Calls) == 0 {
			if res.Stop == provider.StopMaxTokens {
				truncErr := fmt.Errorf("response truncated: hit %s", maxTokensLimitText(l.config.MaxTokens))
				l.emitStepFinish(ctx, emit, res, "length", false)
				l.runAfterRun(ctx, state)
				_ = emit.Emit(ctx, KindError, truncErr.Error())
				return state.Produced, truncErr
			}
			if res.Stop == provider.StopUnknown {
				// The provider closed the stream without reporting a finish
				// reason: a natural end is indistinguishable from a truncation
				// the adapter failed to flag, which would silently defeat the
				// max_tokens guard above. Fail loudly rather than pass a
				// possibly cut-off answer off as complete. Both shipped
				// adapters always report a reason.
				stopErr := fmt.Errorf("provider closed the stream without a finish reason; the response may be incomplete")
				l.emitStepFinish(ctx, emit, res, "error", false)
				l.runAfterRun(ctx, state)
				_ = emit.Emit(ctx, KindError, stopErr.Error())
				return state.Produced, stopErr
			}
			if res.Stop != provider.StopEndTurn {
				// Any other reported reason means the output ended early
				// (stop_sequence truncates at the stop token; content_filter
				// cuts the answer; pause_turn/refusal are not completions).
				// Treating it as done would pass a cut-off answer off as
				// complete — the same silent-truncation class the two guards
				// above exist to catch.
				stopErr := fmt.Errorf("response ended early (stop reason: %s); the output may be incomplete", res.Stop)
				l.emitStepFinish(ctx, emit, res, "error", false)
				l.runAfterRun(ctx, state)
				_ = emit.Emit(ctx, KindError, stopErr.Error())
				return state.Produced, stopErr
			}
			// Honour a cancellation that landed while the final answer streamed: the
		// answer is complete and already durable, but the caller asked to stop —
		// report the run cancelled, symmetric with the pre-dispatch guard below
		// and the iteration guard above.
	if err := ctx.Err(); err != nil {
		l.emitStepFinish(context.WithoutCancel(ctx), emit, res, "other", false)
		l.reportTerminal(ctx, state)
		l.emitCancelled(ctx, emit)
		return state.Produced, err
	}
		slog.Info("agent: run complete", "iterations", iter+1, "input_tokens", state.Usage.InputTokens, "output_tokens", state.Usage.OutputTokens,
				"cache_read_tokens", state.Usage.CacheReadTokens, "cache_write_tokens", state.Usage.CacheWriteTokens, "cache_hit_pct", cacheHitPct(state.Usage))
			l.emitStepFinish(ctx, emit, res, "stop", false)
			l.runAfterRun(ctx, state)
			_ = emit.Emit(ctx, KindDone, nil)
			return state.Produced, nil
		}

		// Don't start a tool batch the caller has already cancelled. The
		// assistant message carrying this batch is already produced (persisted
		// via KindMessage above), so it must still be answered: record synthetic
		// cancelled results, or the durable record keeps a tool_use batch with
		// no tool_result and every later send papers over the gap with the
		// transient EnsurePairing repair while history consumers (and any
		// durable scan) see the dangling call.
		if err := ctx.Err(); err != nil {
		l.recordToolResults(ctx, emit, state, res.Calls, cancelledCallResults(res.Calls))
		// Close the step opened at iter>0, symmetric with every other exit.
		l.emitStepFinish(context.WithoutCancel(ctx), emit, res, "other", false)
		l.reportTerminal(ctx, state)
		l.emitCancelled(ctx, emit)
		return state.Produced, err
		}

		// Truncation guard (capability-gap L1, tool-call half). Reaching here with
		// StopMaxTokens means the message was cut off at the output-token limit
		// WHILE emitting tool calls (the no-calls case returned above). The batch is
		// not trustworthy: the model was still writing, so the plan behind these
		// calls is only half-expressed and the calls it had not reached yet are
		// missing. Fail every call in the batch instead of dispatching it.
		//
		// The malformed-arguments check does not cover this. A message can be cut
		// off just after a complete tool_use block, leaving arguments that parse
		// cleanly — only the stop reason reveals that the turn was truncated.
		//
		// This must run BEFORE the interaction gate: otherwise such a call suspends
		// the run, parking a durable Interaction and putting a card in front of the
		// user for an action the model had not finished specifying.
		//
		// Unlike the no-calls case this does NOT fail the run — the model is told
		// exactly what happened and can re-issue with complete arguments, so the
		// loop continues. MaxIterations bounds a model that keeps truncating.
		if res.Stop == provider.StopMaxTokens {
			slog.Warn("agent: tool batch truncated at max_tokens; failing the batch for re-issue",
				"iter", iter, "calls", len(res.Calls), "max_tokens", l.config.MaxTokens)
			l.recordToolResults(ctx, emit, state, res.Calls, truncatedCallResults(res.Calls, l.config.MaxTokens))
			l.emitStepFinish(ctx, emit, res, "length", true)
			continue
		}

		// Unreported stop reason with tool calls: the same signal the no-calls
		// case fails the run on above. The stream closed without saying why, so
		// this batch may be truncated without the adapter flagging it — dispatch
		// would execute a possibly half-written plan. Fail every call for
		// re-issue, exactly like the max_tokens case, and let the loop continue
		// (MaxIterations bounds a provider that never reports a reason).
		if res.Stop == provider.StopUnknown {
			slog.Warn("agent: provider reported no stop reason for a tool batch; failing the batch for re-issue",
				"iter", iter, "calls", len(res.Calls))
			l.recordToolResults(ctx, emit, state, res.Calls, unknownStopCallResults(res.Calls))
			l.emitStepFinish(ctx, emit, res, "error", true)
			continue
		}

		// Any other early-end reason with tool calls (stop_sequence,
		// content_filter, provider passthroughs) is the same truncation class as
		// the two guards above: the message ended before the model chose to
		// stop, so this batch may be only part of the plan — a cut right after a
		// complete tool_use block leaves arguments that parse cleanly, and only
		// the stop reason reveals the turn was truncated. Fail every call for
		// re-issue rather than executing a possibly half-written plan.
		// Tool-call turns legitimately report StopToolUse (or StopEndTurn from
		// providers that don't distinguish), so those two pass to dispatch.
		if res.Stop != provider.StopEndTurn && res.Stop != provider.StopToolUse {
			slog.Warn("agent: tool batch ended on an early-end stop reason; failing the batch for re-issue",
				"iter", iter, "calls", len(res.Calls), "stop", res.Stop)
			l.recordToolResults(ctx, emit, state, res.Calls, earlyStopCallResults(res.Calls, res.Stop))
			l.emitStepFinish(ctx, emit, res, "error", true)
			continue
		}

		// Unified suspend point (general interrupt): if any call needs the client
		// before the run can continue — a permission approval, an ask_user question
		// set, or a client-side tool — do NOT dispatch it. Emit one interrupt frame
		// per gated call (the whole batch) and end the run cleanly: the assistant
		// message (with the suspended tool_use blocks) is already produced/persisted,
		// the run worker records each durable Interaction, and a LATER run folds all
		// the results back once the batch is fully resolved. The run is stateless —
		// it ends here; there is no suspend/resume.
		if gated := l.interactionGate(ctx, res.Calls); len(gated) > 0 {
			l.PendingInteractions = gated
			l.PendingInteraction = gated[0]
			l.PendingApproval = gated[0] // alias for source compatibility
			// Emitting each interrupt also records its durable Interaction row (the
			// run worker's emitter persists it BEFORE the frame is published, so a
			// fast client can't POST a verdict on a row that doesn't exist yet). If
			// any emit fails — or the client disconnected — fail the run rather than
			// ending it "done" with prompts the client can act on but nothing backing
			// them (which would 404 on resume).
		// Settle the run like every other failure path — close the step, fire
		// AfterRun (usage), and surface a terminal error frame — rather than
		// returning bare. Any Interaction rows the earlier emits already
		// persisted are voided by the run worker (a failed run never waits on
		// client input), so the session's pending-interaction gate clears.
		//
		// Answer the batch BEFORE failing: the assistant message carrying these
		// tool_use blocks is already durable (KindMessage above), and the
		// suspended calls will never be dispatched or verdict-folded now, so
		// without a recorded tool_result the durable record keeps a permanently
		// unpaired tool_use and every later send papers over the gap with the
		// transient EnsurePairing repair (same rationale as the pre-dispatch
		// cancel guard above).
		for _, gate := range gated {
			if err := emit.Emit(ctx, KindInterrupt, *gate); err != nil {
				l.recordToolResults(ctx, emit, state, res.Calls, interruptFailedCallResults(res.Calls))
				l.emitStepFinish(ctx, emit, res, "error", false)
				l.runAfterRun(ctx, state)
				_ = emit.Emit(ctx, KindError, err.Error())
				return state.Produced, err
			}
		}
			slog.Info("agent: run ended awaiting client interactions", "batch", len(gated), "first_tool", gated[0].ToolName, "first_kind", gated[0].Kind)
			l.emitStepFinish(ctx, emit, res, "tool-calls", false)
			l.runAfterRun(ctx, state)
			_ = emit.Emit(ctx, KindDone, nil)
			return state.Produced, nil
		}

		// Dispatch tool calls (concurrently) and append results.
		l.recordToolResults(ctx, emit, state, res.Calls, l.dispatch(ctx, res.Calls))
		l.emitStepFinish(ctx, emit, res, "tool-calls", true)
	}

	// Reached the iteration guard without a final answer. Surface it as a terminal
	// error frame rather than a silent stop: without this emit the run still flips
	// to failed (registry.execute), but the client sees the stream just end with no
	// explanation. Emit KindError so a terminal frame always accompanies the failure.
	// Close the step first, symmetric with every other exit.
	err := fmt.Errorf("max iterations (%d) exceeded", l.config.MaxIterations)
	l.emitStepFinish(ctx, emit, ModelResult{}, "error", false)
	l.runAfterRun(ctx, state)
	_ = emit.Emit(ctx, KindError, err.Error())
	return state.Produced, err
}

// reportTerminal fires AfterRun hooks on the cancellation paths, detached from
// the cancelled ctx: terminal signals (UsageMW's KindUsage → the runs-row usage
// record) are commit-class — a cancelled run still consumed tokens, and the
// emitter's ctx guard would silently drop a usage report sent on the dead ctx.
func (l *Loop) reportTerminal(ctx context.Context, state *RunState) {
	l.runAfterRun(context.WithoutCancel(ctx), state)
}

// emitCancelled emits the run's terminal KindCancelled frame, detached from
// the cancelled ctx for the same commit-class reason as reportTerminal: the
// frame must land in the durable log and reach attached clients even though
// the run ctx is dead (an emitter with a ctx guard would drop it otherwise).
// The session registry keeps an idempotent compensation for the narrow case
// where a cancellation lands on a path that never reaches this emit.
func (l *Loop) emitCancelled(ctx context.Context, emit Emitter) {
	_ = emit.Emit(context.WithoutCancel(ctx), KindCancelled, nil)
}

// runAfterRun fires AfterRun hooks once (reverse registration order).
func (l *Loop) runAfterRun(ctx context.Context, state *RunState) {
	for i := len(l.afterRun) - 1; i >= 0; i-- {
		if err := l.afterRun[i].AfterRun(ctx, state); err != nil {
			if errors.Is(err, ErrAbortRun) {
				// The run is already ending; abort can only stop the
				// remaining AfterRun hooks.
				slog.Warn("agent: AfterRun hook requested abort; skipping remaining AfterRun hooks", "err", err)
				return
			}
			slog.Warn("agent: AfterRun hook failed", "err", err)
		}
	}
}

// emitStepFinish emits the KindStepFinish event closing one think→tool step.
// reason is the ui-message-stream step finish reason ("stop" | "length" |
// "tool-calls" | "error"); continued is true when the loop iterates again (tool
// calls were dispatched). Emit failures are ignored — step frames are render hints, and the
// run must not abort on a broker hiccup.
func (l *Loop) emitStepFinish(ctx context.Context, emit Emitter, res ModelResult, reason string, continued bool) {
	_ = emit.Emit(ctx, KindStepFinish, StepEvent{FinishReason: reason, Usage: res.Usage, IsContinued: continued})
}

// attempt runs one provider call through the wrap chain and returns the
// assembled result. It returns the provider error verbatim (no KindError emit)
// so the caller can distinguish a retriable context-overflow from a fatal
// failure.
func (l *Loop) attempt(ctx context.Context, state *RunState, emit Emitter) (ModelResult, error) {
	// Per-attempt copy down to block granularity: middleware (compression,
	// memory injection, image materialization) rewrites the transient view —
	// image materialization mutates block structs IN PLACE (filling ImageData)
	// — and none of that may reach the durable record, whose messages share
	// Content slices with this view. Nested reference values inside a block
	// (ToolInput maps) stay shared; middleware must treat them as read-only.
	messages := copyMessageContents(state.View)
	call := &ModelCall{
		Request: provider.Request{
			Model:           l.config.Model,
			System:          l.config.System,
			Messages:        messages,
			Tools:           l.toolDefs(),
			MaxTokens:       l.config.MaxTokens,
			CacheablePrefix: l.config.CacheablePrefix,
		},
		View:  messages,
		State: state,
	}
	return chainModel(l.modelWrap, l.realAttempt(emit))(ctx, call)
}

// copyMessageContents copies messages down to block granularity: the message
// structs and their Content slices are fresh, so in-place block mutation
// touches only the copy. Nested reference values inside a block (ToolInput
// maps, byte slices) are still shared with the source.
func copyMessageContents(msgs []provider.Message) []provider.Message {
	out := make([]provider.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		if m.Content != nil {
			out[i].Content = append([]provider.Block{}, m.Content...)
		}
	}
	return out
}

// realAttempt is the innermost model-call handler: it streams the request and
// consumes the events into an assembled assistant message + tool calls.
func (l *Loop) realAttempt(emit Emitter) ModelHandler {
	return func(ctx context.Context, c *ModelCall) (ModelResult, error) {
		events, err := l.provider.Stream(ctx, c.Request)
		if err != nil {
			return ModelResult{}, fmt.Errorf("stream: %w", err)
		}
		track := &emissionTracker{inner: emit}
		assistant, calls, end, err := l.consume(ctx, events, track)
		if err != nil {
			if track.emitted {
				// The failure surfaced AFTER content frames already reached the
				// client: retrying the call (e.g. the overflow fallback) would
				// re-emit them. Mark it so retry middleware declines.
				return ModelResult{}, &midStreamError{err: err}
			}
			return ModelResult{}, err
		}
		return ModelResult{Assistant: assistant, Calls: calls, Stop: end.stop, Usage: end.usage}, nil
	}
}

// emissionTracker records whether any frame was successfully forwarded to the
// client during one provider stream.
type emissionTracker struct {
	inner   Emitter
	emitted bool
}

func (t *emissionTracker) Emit(ctx context.Context, kind EventKind, payload any) error {
	err := t.inner.Emit(ctx, kind, payload)
	if err == nil {
		t.emitted = true
	}
	return err
}

// midStreamError marks a provider failure that surfaced after content frames
// were already forwarded to the client, so a retry would duplicate output. It
// unwraps to the underlying error, so classifiers (IsContextOverflow) still
// see through it.
type midStreamError struct{ err error }

func (e *midStreamError) Error() string { return e.err.Error() }
func (e *midStreamError) Unwrap() error { return e.err }

// cacheHitPct returns the prompt-prefix cache hit rate as a whole percentage.
// Semantics differ by provider: DeepSeek/OpenAI's InputTokens (prompt_tokens)
// already INCLUDES the cached prefix (hit + miss = total), so the hit share is
// CacheRead / Input. Anthropic's input_tokens EXCLUDES the cached prefix, where
// the share would be CacheRead / (Input + CacheRead). We report the DeepSeek/
// OpenAI form (the deployed provider); the >100 clamp guards an Anthropic-style
// payload from showing a nonsense rate. 0 when no input.
func cacheHitPct(u provider.Usage) int {
	if u.InputTokens == 0 {
		return 0
	}
	pct := u.CacheReadTokens * 100 / u.InputTokens
	if pct > 100 {
		return 100
	}
	return pct
}

// MessageWithUsage pairs an assembled assistant message with the token usage
// of the single LLM call that produced it (one assistant message == one LLM
// call). The loop emits it as the KindMessage payload for assistant messages so
// the persistence path can record per-call usage on that message's row. Tool
// results are not LLM calls, so they keep the bare provider.Message payload
// (no usage). The field is a pointer: nil means no usage was reported.
type MessageWithUsage struct {
	Message provider.Message
	Usage   *provider.Usage
}

// StepEvent is the KindStepFinish payload: how one think→tool step ended.
// FinishReason is a ui-message-stream finish reason for the step ("stop" |
// "length" | "tool-calls" | "error"); Usage is the per-LLM-call usage (nil if
// unreported); IsContinued is true when another step follows in this run (tool
// calls were dispatched, so the loop iterates again) and false at a
// run-terminal step.
type StepEvent struct {
	FinishReason string          `json:"finish_reason"`
	Usage        *provider.Usage `json:"usage,omitempty"`
	IsContinued  bool            `json:"is_continued"`
}

// turnEnd carries the terminal metadata of one provider turn: why generation
// stopped and the reported token usage. It lets Run react to a max_tokens
// truncation and accumulate usage without growing consume's return list.
type turnEnd struct {
	stop  provider.StopReason
	usage *provider.Usage
}

// mergeUsage keeps the more informative of two usage reports. A proxy (newapi)
// can inject a placeholder usage chunk (empty id/model, shrunken prompt_tokens,
// cache cleared to 0) AFTER the real model chunk; naive last-wins would erase
// the real cache-hit count. Take the max per field so the placeholder never
// clobbers the real numbers.
func mergeUsage(old, new *provider.Usage) *provider.Usage {
	if old == nil {
		return new
	}
	if new == nil {
		return old
	}
	return &provider.Usage{
		InputTokens:      max(old.InputTokens, new.InputTokens),
		OutputTokens:     max(old.OutputTokens, new.OutputTokens),
		CacheReadTokens:  max(old.CacheReadTokens, new.CacheReadTokens),
		CacheWriteTokens: max(old.CacheWriteTokens, new.CacheWriteTokens),
	}
}

// consume reads one provider stream into an assembled assistant message and
// the list of tool calls, forwarding text/thinking to the emitter. It stops at
// once when ctx is cancelled (client Stop / server cancel) so a run can be
// interrupted mid-stream rather than only between iterations.
func (l *Loop) consume(ctx context.Context, events <-chan provider.Event, emit Emitter) (provider.Message, []toolruntime.Call, turnEnd, error) {
	assistant := provider.Message{Role: provider.RoleAssistant}
	var calls []toolruntime.Call
	var end turnEnd

	// Track open blocks to accumulate deltas.
	open := map[int]*accumulator{}

	for {
		select {
		case <-ctx.Done():
			return assistant, calls, end, ctx.Err()
		case ev, ok := <-events:
			if !ok {
				// The provider closes its channel when the request is cancelled
				// (aborting the HTTP body) as well as at natural end. A cancelled
				// run must surface as such, not look like a clean finish.
				if err := ctx.Err(); err != nil {
					return assistant, calls, end, err
				}
				// Stream closed naturally: finalize any blocks still open, in
				// provider index order — map iteration order is random, and the
				// persisted assistant message must keep the stream's block order.
				idxs := make([]int, 0, len(open))
				for idx := range open {
					idxs = append(idxs, idx)
				}
				sort.Ints(idxs)
				for _, idx := range idxs {
					l.appendFinalized(ctx, open[idx], &assistant, &calls, emit)
					delete(open, idx)
				}
				return assistant, calls, end, nil
			}

			switch ev.Type {
			case provider.EventError:
				return assistant, calls, end, ev.Err

			case provider.EventBlockStart:
				if ev.Block != nil {
					if _, dup := open[ev.Index]; dup {
						// Adapter contract violation: a second block start on an
						// open index would silently discard the deltas already
						// accumulated for it. Fail loudly.
						err := fmt.Errorf("provider stream contract violation: duplicate block start at index %d", ev.Index)
						slog.Warn("agent: "+err.Error())
						return assistant, calls, end, err
					}
					open[ev.Index] = &accumulator{block: *ev.Block}
					// A tool_use block starts with its id/name already known (both
					// adapters supply them at block start), so surface the call —
					// and its name — immediately, before any arguments stream in.
					// The empty delta opens the client block without adding args.
					if ev.Block.Type == provider.BlockToolUse {
						if err := emit.Emit(ctx, KindToolArgs, map[string]any{
							"id": ev.Block.ToolUseID, "name": ev.Block.ToolName, "delta": "",
						}); err != nil {
							return assistant, calls, end, err
						}
					}
				}

			case provider.EventBlockDelta:
				acc, ok := open[ev.Index]
				if !ok {
					// Adapter contract violation: a delta for a block that never
					// started (or already stopped) would be silently dropped,
					// truncating the output with no trace. Fail loudly.
					err := fmt.Errorf("provider stream contract violation: block delta at unopened index %d", ev.Index)
					slog.Warn("agent: "+err.Error())
					return assistant, calls, end, err
				}
				acc.append(ev.Delta)
				acc.appendSignature(ev.SignatureDelta)
				// Stream text/thinking out incrementally. An emit failure
				// (e.g. client disconnected) aborts the run so the loop
				// unwinds and the run settles instead of leaking.
				var emitErr error
				switch acc.block.Type {
				case provider.BlockText:
					emitErr = emit.Emit(ctx, KindText, ev.Delta)
				case provider.BlockThinking:
					emitErr = emit.Emit(ctx, KindThinking, ev.Delta)
				case provider.BlockToolUse:
					// Stream the argument fragment too, so a large tool input
					// renders as it generates rather than appearing whole at
					// block-stop. An empty provider delta carries no args.
					if ev.Delta != "" {
						emitErr = emit.Emit(ctx, KindToolArgs, map[string]any{
							"id": acc.block.ToolUseID, "name": acc.block.ToolName, "delta": ev.Delta,
						})
					}
				}
				if emitErr != nil {
					return assistant, calls, end, emitErr
				}

			case provider.EventBlockStop:
				if acc, ok := open[ev.Index]; ok {
					l.appendFinalized(ctx, acc, &assistant, &calls, emit)
					delete(open, ev.Index)
				} else {
					// A stop for a block that never started loses no data, but
					// it is a contract violation worth a trace.
					slog.Warn("agent: provider stream contract violation: block stop at unopened index", "index", ev.Index)
				}

			case provider.EventMessageStop:
				// Terminal metadata. It can arrive across several stop events
				// (OpenAI reports the finish reason and usage in separate chunks),
				// so keep the last non-empty stop reason and merge usage (a proxy
				// placeholder chunk must not clobber the real cache-hit count).
				if ev.StopReason != provider.StopUnknown {
					end.stop = ev.StopReason
				}
				if ev.Usage != nil {
					end.usage = mergeUsage(end.usage, ev.Usage)
				}
			}
		}
	}
}

// appendFinalized finalizes an accumulator, appends its block to the assistant
// message, and — for a tool_use block — records the tool call (carrying any
// argument-parse error) and emits the tool-use frame. Shared by the natural
// stream-close path and EventBlockStop so both handle malformed args the same.
func (l *Loop) appendFinalized(ctx context.Context, acc *accumulator, assistant *provider.Message, calls *[]toolruntime.Call, emit Emitter) {
	blk, argErr := acc.finalize()
	if argErr != nil {
		// Persist the parse failure on the block itself: a suspended-batch fold
		// later rebuilds calls from the durable message and must still know this
		// call was never meant to execute.
		blk.ArgsError = argErr.Error()
	}
	assistant.Content = append(assistant.Content, blk)
	if blk.Type != provider.BlockToolUse {
		return
	}
	call := toolruntime.Call{ID: blk.ToolUseID, Name: blk.ToolName, Args: blk.ToolInput}
	if argErr != nil {
		call.ArgsError = argErr.Error()
	}
	*calls = append(*calls, call)
	// Emit the tool-use event so a provider that closes the stream without a
	// block-stop for a tool call (e.g. OpenAI's finish) still shows a tool-call
	// before its result, rather than orphaning the result on the client.
	_ = emit.Emit(ctx, KindToolUse, map[string]any{
		"id": blk.ToolUseID, "name": blk.ToolName, "input": blk.ToolInput,
	})
}

// interactionGate is the loop's unified suspend point: it scans the batch for
// every call that needs the client before the run can continue and returns them
// ALL, in order. Three triggers, one check per call: (1) a permission approval
// — a tool whose Permission callback denies with the ApprovalReasonPrefix
// marker; (2) an ask_user question set — the model calling the built-in
// ask_user tool; (3) a client-side tool — one implementing toolruntime.ClientTool.
// Every gated call becomes its own pending interaction (multi-approval queue):
// the run ends on the batch and a fresh run resumes only once the WHOLE batch is
// resolved, so the model never sees a partial conversation (LangGraph-style).
// Returns nil when nothing needs the client.
func (l *Loop) interactionGate(ctx context.Context, calls []toolruntime.Call) []*Interaction {
	var gated []*Interaction
	for _, c := range calls {
		if c.ArgsError != "" {
			continue
		}
		tool, registered := l.tools.Get(c.Name)
		if registered {
			// Arguments that would fail the dispatch schema screen must not
			// park an approval / question set / client round-trip: the run
			// would suspend (and put a card in front of the user) for a call
			// that can never execute. Dispatch answers the violation inline.
			if verr := toolruntime.ValidateArgs(tool.Schema(), c.Args); verr != nil {
				continue
			}
		}
		var in *Interaction
		switch {
		// ask_user: the model is explicitly asking the user for structured input.
		case c.Name == AskUserToolName:
			in = &Interaction{ID: uuid.NewString(), Kind: "ask_user", ToolCallID: c.ID, ToolName: c.Name, Input: c.Args}
		// Client-side tool: executes in the client, not the server — suspend.
		case registered && toolruntime.IsClientTool(tool):
			in = &Interaction{ID: uuid.NewString(), Kind: "client_tool", ToolCallID: c.ID, ToolName: c.Name, Input: c.Args}
		// Permission approval: a dangerous call the policy gates for a yes/no.
		case registered && l.gateInteraction != nil:
			if deny, reason := l.gateInteraction(ctx, tool); deny && IsApprovalReason(reason) {
				in = &Interaction{ID: uuid.NewString(), Kind: "approval", ToolCallID: c.ID, ToolName: c.Name, Input: c.Args}
			}
		}
		if in != nil {
			// Every gated interaction carries the full batch so the run worker
			// can persist the suspended-batch snapshot with the first row.
			in.Batch = calls
			gated = append(gated, in)
		}
	}
	return gated
}

// dispatch runs tool calls concurrently, each through the tool-middleware chain.
// Calls are first screened sequentially — malformed arguments, an unregistered
// name, a schema violation, or an execution-permission deny become is_error
// results inline (so the model can self-correct) and never reach the chain.
// Results stay index-aligned with calls for tool_use/tool_result pairing.
//
// The loop owns the fan-out rather than delegating to Registry.CallAll, because
// each call must be wrapped by WrapToolCall middleware and toolruntime cannot
// import this package. The screen resolves every live call's Tool up front, so
// middleware is guaranteed a non-nil ToolCall.Tool.
func (l *Loop) dispatch(ctx context.Context, calls []toolruntime.Call) []toolruntime.Result {
	results := make([]toolruntime.Result, len(calls))
	live := make([]*ToolCall, 0, len(calls))
	liveIdx := make([]int, 0, len(calls))
	for i, c := range calls {
		if c.ArgsError != "" {
			results[i] = toolruntime.Result{Content: "invalid tool arguments: " + c.ArgsError, IsError: true}
			continue
		}
		// Resolve the tool here rather than inside the registry so the middleware
		// chain always sees a real Tool. Registry.Call keeps its own unknown-tool
		// guard for direct callers; this mirrors its message.
		tool, ok := l.tools.Get(c.Name)
		if !ok {
			results[i] = toolruntime.Result{Content: fmt.Sprintf("unknown tool: %s", c.Name), IsError: true}
			continue
		}
		// Schema screen (LangChain _parse_input parity): arguments that parsed
		// but violate the tool's declared input schema (wrong-typed fields,
		// missing required) never execute — the structured error names the
		// offending field so the model can re-issue with corrected arguments.
		if verr := toolruntime.ValidateArgs(tool.Schema(), c.Args); verr != nil {
			results[i] = toolruntime.Result{Content: "invalid tool arguments: " + verr.Error(), IsError: true}
			continue
		}
		// Execution-permission gate (D10): authorize the call by the tool's risk
		// before dispatch. A denied call is not executed; the reason is fed back as
		// an error result so the model can adapt.
		if l.gateExecute != nil {
			if deny, reason := l.gateExecute(ctx, tool); deny {
				results[i] = toolruntime.Result{Content: "permission denied: " + reason, IsError: true}
				continue
			}
		}
		live = append(live, &ToolCall{Call: c, Tool: tool})
		liveIdx = append(liveIdx, i)
	}
	if len(live) == 0 {
		return results
	}
	// One chain for the batch: the middleware slice is fixed for the run, so the
	// composed handler is shared and must be safe for concurrent use (the same
	// contract the tools themselves carry).
	handler := chainTool(l.toolWrap, l.realToolCall())
	var wg sync.WaitGroup
	for j, tc := range live {
		wg.Add(1)
		go func(j int, tc *ToolCall) {
			defer wg.Done()
			results[liveIdx[j]] = handler(ctx, tc)
		}(j, tc)
	}
	wg.Wait()
	return results
}

// realToolCall is the innermost tool-call handler: it executes the call through
// the registry, which applies the tool's timeout and converts errors/panics into
// error results. The call id rides the ctx so a tool that emits progress frames
// can tag them with the call they belong to (nesting them correctly in the UI
// when several run in parallel).
func (l *Loop) realToolCall() ToolHandler {
	return func(ctx context.Context, c *ToolCall) toolruntime.Result {
		return l.tools.Call(toolruntime.ContextWithCallID(ctx, c.Call.ID), c.Call.Name, c.Call.Args)
	}
}

// recordToolResults completes one tool batch: it streams each result to the
// client, assembles the tool-result message, and appends it to the run's
// produced messages for full-block persistence. Shared by the normal dispatch
// path and the truncated-batch path so both produce an identical event shape —
// a batch that was failed rather than executed must still be a well-formed
// tool_result for every tool_use, or the next request is unpaired.
func (l *Loop) recordToolResults(ctx context.Context, emit Emitter, state *RunState, calls []toolruntime.Call, results []toolruntime.Result) {
	// Recording a batch's results is commit-class: the assistant tool_use is
	// already durable, so its results must land too — including when the run
	// was cancelled mid-batch or right before it (a cancelled ctx would make
	// the persistence emit fail silently and leave the durable record
	// unpaired). Detach from cancellation; result frames for a departed client
	// are harmless.
	ctx = context.WithoutCancel(ctx)
	for i, r := range results {
		_ = emit.Emit(ctx, KindToolResult, map[string]any{
			"tool_use_id": calls[i].ID,
			"name":        calls[i].Name,
			"content":     r.Content,
			"is_error":    r.IsError,
		})
	}
	msg := toolResultMessage(calls, results)
	state.Produced = append(state.Produced, msg)
	_ = emit.Emit(ctx, KindMessage, msg)
}

// cancelledCallResults answers every call in a batch that never dispatched
// because the run was cancelled between the model's reply and the tool batch.
// Naming the non-execution explicitly matters: a later model turn reads these
// results and must not assume the side effects happened.
func cancelledCallResults(calls []toolruntime.Call) []toolruntime.Result {
	results := make([]toolruntime.Result, len(calls))
	for i := range results {
		results[i] = toolruntime.Result{
			Content: "not executed: the run was cancelled before this tool call could be dispatched",
			IsError: true,
		}
	}
	return results
}

// truncatedCallResults fails every call in a batch that arrived on a message cut
// off at the output-token limit. The message names the real cause. Strict JSON
// parsing already rejects a call whose arguments were sliced mid-object, but it
// reports that as malformed JSON — sending the model looking for a syntax error
// it did not make; and a call completed just before the cut parses fine, so
// nothing else flags it at all. Telling the model it was truncated is what makes
// re-issuing the obvious next move.
func truncatedCallResults(calls []toolruntime.Call, maxTokens int) []toolruntime.Result {
	content := fmt.Sprintf(
		"not executed: the response hit %s while this tool call was being written, so its arguments may be incomplete. Re-issue the call with complete arguments, keeping the response shorter.",
		maxTokensLimitText(maxTokens))
	results := make([]toolruntime.Result, len(calls))
	for i := range results {
		results[i] = toolruntime.Result{Content: content, IsError: true}
	}
	return results
}

// maxTokensLimitText renders the configured output-token limit for truncation
// messages, tolerating an unset (0) config — "limit (0)" would read as nonsense.
func maxTokensLimitText(maxTokens int) string {
	if maxTokens <= 0 {
		return "the max_tokens limit"
	}
	return fmt.Sprintf("the max_tokens limit (%d)", maxTokens)
}

// unknownStopCallResults fails every call in a batch whose stream closed without
// a reported finish reason: the batch may be truncated without the adapter
// flagging it, so — as with the max_tokens cut — the safe move is re-issue, not
// executing a possibly half-written plan.
func unknownStopCallResults(calls []toolruntime.Call) []toolruntime.Result {
	results := make([]toolruntime.Result, len(calls))
	for i := range results {
		results[i] = toolruntime.Result{
			Content: "not executed: the provider closed the stream without reporting a finish reason, so this tool call may be incomplete. Re-issue the call with complete arguments.",
			IsError: true,
		}
	}
	return results
}

// earlyStopCallResults fails every call in a batch whose message ended on an
// early-end stop reason (stop_sequence, content_filter, provider passthroughs):
// the same truncation class as max_tokens — the batch may be only part of the
// plan, so re-issue, don't dispatch.
func earlyStopCallResults(calls []toolruntime.Call, stop provider.StopReason) []toolruntime.Result {
	content := fmt.Sprintf(
		"not executed: the response ended early (stop reason: %s) while these tool calls were being written, so the batch may be incomplete. Re-issue the call with complete arguments.",
		stopReasonText(stop))
	results := make([]toolruntime.Result, len(calls))
	for i := range results {
		results[i] = toolruntime.Result{Content: content, IsError: true}
	}
	return results
}

// dropEmptyBlocks removes content blocks that carry no payload: a text block
// with no text, or a thinking block with neither thinking text nor a
// signature (the signature must round-trip, so a signed block is kept even
// when its text is empty). Cache-point blocks are kept regardless — dropping
// one would move the byte-stable caching boundary. All other block types
// (tool_use, image, tool_result) always carry content by construction.
func dropEmptyBlocks(blocks []provider.Block) []provider.Block {
	kept := blocks[:0]
	for _, b := range blocks {
		if b.CachePoint {
			kept = append(kept, b)
			continue
		}
		switch b.Type {
		case provider.BlockText:
			if b.Text == "" {
				continue
			}
		case provider.BlockThinking:
			if b.Thinking == "" && b.ThinkingSignature == "" {
				continue
			}
		}
		kept = append(kept, b)
	}
	return kept
}

// stopReasonText renders a stop reason for error messages, naming the
// no-reason case explicitly so "stop reason: " never reads as a blank.
func stopReasonText(stop provider.StopReason) string {
	if stop == provider.StopUnknown {
		return "none reported"
	}
	return string(stop)
}

// interruptFailedCallResults answers every call in a gated batch whose
// interrupt frame failed to persist/publish: the run ends abnormally and no
// client verdict will ever arrive, so the durable tool_use must still be
// paired with a tool_result naming the non-execution — the model reads these
// on a later turn and must not assume the gated side effects happened.
func interruptFailedCallResults(calls []toolruntime.Call) []toolruntime.Result {
	results := make([]toolruntime.Result, len(calls))
	for i := range results {
		results[i] = toolruntime.Result{
			Content: "not executed: the run ended abnormally while requesting client input for this tool call. Re-issue the call to try again.",
			IsError: true,
		}
	}
	return results
}

// abortedCallResults answers every call in a batch left undispatched because
// an AfterModel hook aborted the run (ErrAbortRun). The assistant tool_use is
// already durable, so each call needs a tool_result naming the non-execution —
// a later model turn must not assume the side effects happened.
func abortedCallResults(calls []toolruntime.Call) []toolruntime.Result {
	results := make([]toolruntime.Result, len(calls))
	for i := range results {
		results[i] = toolruntime.Result{
			Content: "not executed: the run was aborted by middleware before this tool call could be dispatched",
			IsError: true,
		}
	}
	return results
}

// toolResultMessage builds the user-role message carrying tool results back.
// A tool that returns empty content (e.g. list_dir on an empty workspace) is
// given a placeholder: an empty tool_result serializes to a `role:"tool"`
// message with no content field, which providers (OpenAI/deepseek gateway)
// reject with a 400 — aborting the run right after the tool batch.
func toolResultMessage(calls []toolruntime.Call, results []toolruntime.Result) provider.Message {
	msg := provider.Message{Role: provider.RoleUser}
	for i, res := range results {
		content := res.Content
		if content == "" {
			content = emptyToolResultPlaceholder
		}
		msg.Content = append(msg.Content, provider.Block{
			Type:         provider.BlockToolResult,
			ToolResultID: calls[i].ID,
			ToolContent:  content,
			IsError:      res.IsError,
			ToolMessages: res.Nested,
		})
	}
	return msg
}

// emptyToolResultPlaceholder stands in for a tool that produced no output, so
// the serialized tool message always carries a non-empty content field.
const emptyToolResultPlaceholder = "(no output)"

// accumulator assembles a block from streaming deltas.
type accumulator struct {
	block     provider.Block
	text      string
	json      string
	signature string
}

func (a *accumulator) append(delta string) {
	switch a.block.Type {
	case provider.BlockText, provider.BlockThinking:
		a.text += delta
	case provider.BlockToolUse:
		a.json += delta
	}
}

// appendSignature collects a thinking-block signature fragment. Signatures
// stream as their own delta kind and must be kept off the text so the block's
// ThinkingSignature round-trips cleanly.
func (a *accumulator) appendSignature(sig string) {
	a.signature += sig
}

func (a *accumulator) finalize() (provider.Block, error) {
	b := a.block
	var argErr error
	switch b.Type {
	case provider.BlockText:
		b.Text = a.text
	case provider.BlockThinking:
		b.Thinking = a.text
		if a.signature != "" {
			b.ThinkingSignature = a.signature
		}
	case provider.BlockToolUse:
		if a.json != "" {
			var input map[string]any
			if err := json.Unmarshal([]byte(a.json), &input); err != nil {
				argErr = fmt.Errorf("tool arguments are not valid JSON: %w", err)
			} else {
				b.ToolInput = input
			}
		}
	}
	return b, argErr
}
