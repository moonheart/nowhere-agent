// Package agent implements the agent-loop capability (design D1): a self-built
// think→tool→think loop. It owns orchestration, tool dispatch, streaming, and
// the in-context short-term memory, driving a provider.Adapter and emitting
// canonical events that the session runtime persists and fans out.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

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

// ApprovalRequest describes the single gated tool call that ended a run for
// human input (capability-gap O2). The loop emits it (KindApprovalRequest) and
// finishes; the run's worker persists it as a durable Approval (thread state)
// and surfaces it to the client. The run does NOT suspend — a fresh run applies
// the verdict later. Kind distinguishes a permission approval (a dangerous call
// needing a yes/no) from an ask_user question set.
type ApprovalRequest struct {
	// ID is the durable approval's id, generated the moment the gate is detected
	// (LangGraph-style: the interrupt's id is known before it is surfaced). The
	// loop emits it on the KindApprovalRequest frame and the run worker persists
	// the Approval row with the SAME id, so the client's card can POST its verdict
	// without a refresh or a store lookup.
	ID string
	// Kind is "approval" or "ask_user". Empty means approval (the O2 default).
	Kind       string
	ToolCallID string
	ToolName   string
	Input      map[string]any
}

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
	KindError      EventKind = "error"
	KindDone       EventKind = "done"
	// KindMessage carries a fully-assembled conversation message (payload:
	// provider.Message) so the run path can persist it in original block form.
	// It is emitted once per completed message: each assistant message and each
	// tool-result message. It is a persistence signal, not a render frame.
	KindMessage EventKind = "message"
	// KindCancelled marks a run stopped early (client Stop / server cancel). It
	// is persisted so replay/history can tell a cancelled run from a finished one.
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
	// KindApprovalRequest carries the gated tool call that ended the run for
	// human input (payload: ApprovalRequest). Live-only content (broker-routed,
	// never persisted): the run's worker separately persists the durable Approval
	// record the decision endpoint reads.
	KindApprovalRequest EventKind = "approval_request"
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
	// PendingApproval, when set after Run returns, is the gated tool call that
	// ended the run for human input. The run worker reads it to persist the
	// durable Approval.
	PendingApproval *ApprovalRequest
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
// end. Gate hooks (interaction/execute) go to the FIRST middleware that
// registers each — later registrations for the same gate are ignored. Use
// returns the loop for chaining.
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
		if h, ok := m.(InteractionGateHook); ok && l.gateInteraction == nil {
			l.gateInteraction = h.GateInteraction()
		}
		if h, ok := m.(ExecuteGateHook); ok && l.gateExecute == nil {
			l.gateExecute = h.GateExecute()
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
// The loop works over a working view of the conversation (history + what it
// produces). Cross-cutting concerns (compression, memory injection, image
// materialization, overflow retry, usage) run as middleware around each model
// call, rewriting only the transient view — never the durable record (D1).
func (l *Loop) Run(ctx context.Context, history []provider.Message, emit Emitter) ([]provider.Message, error) {
	state := &RunState{Emit: emit}

	for iter := 0; iter < l.config.MaxIterations; iter++ {
		state.Iteration = iter
		// Honour cancellation between iterations (e.g. after a tool batch).
		if err := ctx.Err(); err != nil {
			_ = emit.Emit(ctx, KindCancelled, nil)
			return state.Produced, err
		}

		// Assemble the working view for this turn and repair tool pairing before
		// every send (a prior cancel or a compression split can leave an unpaired
		// block, which the provider would reject).
		state.View = contextmgmt.EnsurePairing(append(append([]provider.Message{}, history...), state.Produced...))

		// Node hooks: observation before the model call (registration order).
		for _, h := range l.before {
			if err := h.BeforeModel(ctx, state); err != nil {
				slog.Warn("agent: BeforeModel hook failed", "err", err)
			}
		}

		res, err := l.attempt(ctx, state.View, emit)
		if err != nil {
			if ctx.Err() != nil {
				slog.Info("agent: run cancelled", "iter", iter, "ctx_err", ctx.Err(), "attempt_err", err)
				_ = emit.Emit(ctx, KindCancelled, nil)
				return state.Produced, ctx.Err()
			}
			slog.Error("agent: provider attempt failed; run aborting", "iter", iter, "err", err, "view_msgs", len(state.View))
			_ = emit.Emit(ctx, KindError, err.Error())
			l.runAfterRun(ctx, state)
			return state.Produced, err
		}
		if res.Usage != nil {
			state.Usage.InputTokens += res.Usage.InputTokens
			state.Usage.OutputTokens += res.Usage.OutputTokens
			state.Usage.CacheReadTokens += res.Usage.CacheReadTokens
			state.Usage.CacheWriteTokens += res.Usage.CacheWriteTokens
		}
		state.Produced = append(state.Produced, res.Assistant)
		// Expose the assembled assistant message for full-block persistence
		// (persist-raw-messages), paired with the usage of the LLM call that
		// produced it. Emit failures here don't abort the run — the persistence
		// listener drops them — so ignore the error.
		_ = emit.Emit(ctx, KindMessage, MessageWithUsage{Message: res.Assistant, Usage: res.Usage})

		// Node hooks: observation after the model call (reverse order).
		for i := len(l.afterModel) - 1; i >= 0; i-- {
			if err := l.afterModel[i].AfterModel(ctx, state); err != nil {
				slog.Warn("agent: AfterModel hook failed", "err", err)
			}
		}

		// No tool calls → the turn is final. But distinguish a natural finish from
		// a max_tokens truncation: a truncated turn is a cut-off answer, not a
		// clean completion, so surface it as an error instead of a silent done
		// (capability-gap L1). Without this the loop treats truncation as success.
		if len(res.Calls) == 0 {
			if res.Stop == provider.StopMaxTokens {
				truncErr := fmt.Errorf("response truncated: hit the max_tokens limit (%d)", l.config.MaxTokens)
				l.runAfterRun(ctx, state)
				_ = emit.Emit(ctx, KindError, truncErr.Error())
				return state.Produced, truncErr
			}
			slog.Info("agent: run complete", "iterations", iter+1, "input_tokens", state.Usage.InputTokens, "output_tokens", state.Usage.OutputTokens,
				"cache_read_tokens", state.Usage.CacheReadTokens, "cache_write_tokens", state.Usage.CacheWriteTokens, "cache_hit_pct", cacheHitPct(state.Usage))
			l.runAfterRun(ctx, state)
			_ = emit.Emit(ctx, KindDone, nil)
			return state.Produced, nil
		}

		// Don't start a tool batch the caller has already cancelled.
		if err := ctx.Err(); err != nil {
			_ = emit.Emit(ctx, KindCancelled, nil)
			return state.Produced, err
		}

		// Human-interaction gate (capability-gap O2 + ask_user): if a call needs a
		// human answer — a permission approval OR an ask_user question set — do NOT
		// dispatch it. Emit the request and end the run cleanly: the assistant
		// message (with the gated tool_use) is already produced/persisted, the run
		// worker records the durable Approval, and a LATER run applies the verdict.
		// The run is stateless — it ends here; there is no suspend/resume.
		if gate := l.interactionGate(res.Calls); gate != nil {
			l.PendingApproval = gate
			_ = emit.Emit(ctx, KindApprovalRequest, *gate)
			slog.Info("agent: run ended awaiting human input", "tool", gate.ToolName, "kind", gate.Kind)
			l.runAfterRun(ctx, state)
			_ = emit.Emit(ctx, KindDone, nil)
			return state.Produced, nil
		}

		// Dispatch tool calls (concurrently) and append results.
		results := l.dispatch(ctx, res.Calls)
		for i, r := range results {
			_ = emit.Emit(ctx, KindToolResult, map[string]any{
				"tool_use_id": res.Calls[i].ID,
				"name":        res.Calls[i].Name,
				"content":     r.Content,
				"is_error":    r.IsError,
			})
		}
		resultMsg := toolResultMessage(res.Calls, results)
		state.Produced = append(state.Produced, resultMsg)
		// Expose the assembled tool-result message for full-block persistence.
		_ = emit.Emit(ctx, KindMessage, resultMsg)
	}

	// Reached the iteration guard without a final answer. Surface it as a terminal
	// error frame rather than a silent stop: without this emit the run still flips
	// to failed (registry.execute), but the client sees the stream just end with no
	// explanation. Emit KindError so a terminal frame always accompanies the failure.
	err := fmt.Errorf("max iterations (%d) exceeded", l.config.MaxIterations)
	l.runAfterRun(ctx, state)
	_ = emit.Emit(ctx, KindError, err.Error())
	return state.Produced, err
}

// runAfterRun fires AfterRun hooks once (reverse registration order).
func (l *Loop) runAfterRun(ctx context.Context, state *RunState) {
	for i := len(l.afterRun) - 1; i >= 0; i-- {
		if err := l.afterRun[i].AfterRun(ctx, state); err != nil {
			slog.Warn("agent: AfterRun hook failed", "err", err)
		}
	}
}

// attempt runs one provider call through the wrap chain and returns the
// assembled result. It returns the provider error verbatim (no KindError emit)
// so the caller can distinguish a retriable context-overflow from a fatal
// failure.
func (l *Loop) attempt(ctx context.Context, view []provider.Message, emit Emitter) (ModelResult, error) {
	call := &ModelCall{
		Request: provider.Request{
			Model:           l.config.Model,
			System:          l.config.System,
			Messages:        view,
			Tools:           l.toolDefs(),
			MaxTokens:       l.config.MaxTokens,
			CacheablePrefix: l.config.CacheablePrefix,
		},
		// Per-attempt copy: middleware (compression, memory injection, image
		// materialization) rewrites this without touching the durable record.
		View: append([]provider.Message{}, view...),
	}
	return chainModel(l.modelWrap, l.realAttempt(emit))(ctx, call)
}

// realAttempt is the innermost model-call handler: it streams the request and
// consumes the events into an assembled assistant message + tool calls.
func (l *Loop) realAttempt(emit Emitter) ModelHandler {
	return func(ctx context.Context, c *ModelCall) (ModelResult, error) {
		events, err := l.provider.Stream(ctx, c.Request)
		if err != nil {
			return ModelResult{}, fmt.Errorf("stream: %w", err)
		}
		assistant, calls, end, err := l.consume(ctx, events, emit)
		if err != nil {
			return ModelResult{}, err
		}
		return ModelResult{Assistant: assistant, Calls: calls, Stop: end.stop, Usage: end.usage}, nil
	}
}

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
				// Stream closed naturally: finalize any blocks still open.
				for idx, acc := range open {
					l.appendFinalized(ctx, acc, &assistant, &calls, emit)
					delete(open, idx)
				}
				return assistant, calls, end, nil
			}

			switch ev.Type {
			case provider.EventError:
				return assistant, calls, end, ev.Err

			case provider.EventBlockStart:
				if ev.Block != nil {
					open[ev.Index] = &accumulator{block: *ev.Block}
				}

			case provider.EventBlockDelta:
				if acc, ok := open[ev.Index]; ok {
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
					}
					if emitErr != nil {
						return assistant, calls, end, emitErr
					}
				}

			case provider.EventBlockStop:
				if acc, ok := open[ev.Index]; ok {
					l.appendFinalized(ctx, acc, &assistant, &calls, emit)
					delete(open, ev.Index)
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

// interactionGate scans the batch for a call that needs a human answer before
// the run can continue. Two kinds: (1) a permission approval — a tool whose
// Permission callback denies with the ApprovalReasonPrefix marker; (2) an
// ask_user question set — the model calling the built-in ask_user tool. Only
// the first such call is returned — the run suspends on it. Returns nil when
// nothing needs a human.
func (l *Loop) interactionGate(calls []toolruntime.Call) *ApprovalRequest {
	for _, c := range calls {
		if c.ArgsError != "" {
			continue
		}
		// ask_user: the model is explicitly asking the user for structured input.
		if c.Name == AskUserToolName {
			return &ApprovalRequest{ID: uuid.NewString(), Kind: "ask_user", ToolCallID: c.ID, ToolName: c.Name, Input: c.Args}
		}
		// Permission approval: a dangerous call the policy gates for a yes/no.
		if l.gateInteraction != nil {
			if tool, ok := l.tools.Get(c.Name); ok {
				if deny, reason := l.gateInteraction(tool); deny && IsApprovalReason(reason) {
					return &ApprovalRequest{ID: uuid.NewString(), Kind: "approval", ToolCallID: c.ID, ToolName: c.Name, Input: c.Args}
				}
			}
		}
	}
	return nil
}

// dispatch runs tool calls concurrently via the registry. A call whose
// arguments failed to parse (ArgsError set) is not dispatched: it becomes an
// is_error result inline so the model can retry with valid JSON, while results
// stay index-aligned with calls for tool_use/tool_result pairing.
func (l *Loop) dispatch(ctx context.Context, calls []toolruntime.Call) []toolruntime.Result {
	results := make([]toolruntime.Result, len(calls))
	live := make([]toolruntime.Call, 0, len(calls))
	liveIdx := make([]int, 0, len(calls))
	for i, c := range calls {
		if c.ArgsError != "" {
			results[i] = toolruntime.Result{Content: "invalid tool arguments: " + c.ArgsError, IsError: true}
			continue
		}
		// Execution-permission gate (D10): authorize the call by the tool's risk
		// before dispatch. A denied call is not executed; the reason is fed back as
		// an error result so the model can adapt.
		if l.gateExecute != nil {
			if tool, ok := l.tools.Get(c.Name); ok {
				if deny, reason := l.gateExecute(tool); deny {
					results[i] = toolruntime.Result{Content: "permission denied: " + reason, IsError: true}
					continue
				}
			}
		}
		live = append(live, c)
		liveIdx = append(liveIdx, i)
	}
	if len(live) > 0 {
		for j, r := range l.tools.CallAll(ctx, live) {
			results[liveIdx[j]] = r
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
