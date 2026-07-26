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
	"strings"

	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// errCompressionSkipped is returned by maybeCompress when the circuit breaker
// has tripped, so the caller doesn't count it as a fresh failure or a success.
var errCompressionSkipped = errors.New("compression skipped: circuit breaker tripped")

// ErrAwaitingApproval is returned by Run when a tool call is gated for human
// approval (capability-gap O2): the loop does NOT execute it and instead
// suspends the run. The run's worker parks the run (RunWaitingApproval) and
// resumes it once the user decides. Distinct from a cancel or a failure.
var ErrAwaitingApproval = errors.New("awaiting tool approval")

// ApprovalReasonPrefix marks a Permission deny-reason as "gated for human
// approval" rather than a hard deny. The server's permission callback prefixes
// the reason for calls whose policy verdict is Ask; the loop distinguishes
// those (suspend + ask the user) from a true deny (feed an error to the model).
const ApprovalReasonPrefix = "approval required: "

// IsApprovalReason reports whether a Permission deny-reason is a gate-for-
// approval marker (vs a hard deny).
func IsApprovalReason(reason string) bool {
	return strings.HasPrefix(reason, ApprovalReasonPrefix)
}

// ApprovalRequest describes the single gated tool call that suspended a run.
// It is the loop's output to the run worker, which persists it (durable
// interaction) and surfaces it to the client. Kind distinguishes a permission
// approval (a dangerous call needing a yes/no) from an ask_user question set
// (the model asking for structured input).
type ApprovalRequest struct {
	// Kind is "approval" or "ask_user". Empty means approval (the O2 default).
	Kind       string
	ToolCallID string
	ToolName   string
	Input      map[string]any
}

// AskUserToolName is the built-in tool the model calls to ask the user
// structured questions (capability O-ask). Like a permission approval, calling
// it suspends the run until the user answers.
const AskUserToolName = "ask_user"

// ResumedApproval is the human-resolved interaction a resumed run should apply.
// For a permission approval: Approved=true executes the gated call (the approval
// is its authorization), false injects a denial. For an ask_user question set:
// Answer is the user's structured response, fed back as the tool result so the
// model reads what the user said.
type ResumedApproval struct {
	// Kind is "approval" or "ask_user" (empty = approval).
	Kind       string
	ToolCallID string
	ToolName   string
	Input      map[string]any
	Approved   bool
	// Answer carries the ask_user response (e.g. {"answers":{...}}). Unused for
	// permission approvals.
	Answer map[string]any
}

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
	// KindApprovalRequest carries the gated tool call that suspended the run
	// (payload: ApprovalRequest). Live-only content (broker-routed, never
	// persisted): the run's worker separately persists the durable Approval
	// record that Resume reads.
	KindApprovalRequest EventKind = "approval_request"
)

// Emitter receives loop events (the session runtime persists + fans them out).
type Emitter interface {
	Emit(ctx context.Context, kind EventKind, payload any) error
}

// Config controls the loop.
type Config struct {
	Model           string
	System          string
	MaxTokens       int
	MaxIterations   int // guard against infinite loops
	CacheablePrefix bool
	// Images, when set, materializes BlockImage paths to base64 before each
	// send (every turn, byte-stable for prompt caching). Nil leaves image
	// blocks as-is (they degrade to text placeholders downstream).
	Images provider.ImageResolver

	// Permission, when set, authorizes each tool call before dispatch: it
	// receives the resolved tool and returns (deny, reason). A denied call is not
	// executed — it becomes an is_error tool_result carrying the reason, so the
	// model can adapt. Nil leaves all calls ungated (pre-permission behaviour).
	Permission func(toolruntime.Tool) (bool, string)

	// Approval, when non-nil, is the tool call the run is resuming after a human
	// approved it (capability-gap O2). The loop executes it WITHOUT re-checking
	// Permission (the approval IS the authorization) and dispatches it ahead of
	// any further gated calls. Set by the run worker on Resume.
	Approval *ResumedApproval

	// ContextWindow is the model's context window in tokens. Combined with
	// MaxTokens (the reserved reply space) it bounds the working view: the loop
	// compresses when the view exceeds CompressThreshold of (ContextWindow −
	// MaxTokens). Zero disables in-loop compression.
	ContextWindow int
	// Compressor summarizes dropped history when the working view is
	// compressed. Nil disables compression (the view grows unbounded).
	Compressor contextmgmt.Compressor
	// CompressThreshold is the fraction of the usable window at which
	// compression triggers (default 0.8).
	CompressThreshold float64
	// MaxCompressFailures is the circuit breaker: after this many consecutive
	// compression failures the loop stops compressing for the run (default 3).
	MaxCompressFailures int
	// MaxOverflowRetries bounds the reactive context-overflow fallback: how many
	// times the loop drops the oldest round and retries after the provider
	// rejects a request as too large (default 3).
	MaxOverflowRetries int
}

// Loop runs the think→tool→think cycle.
type Loop struct {
	provider provider.Adapter
	tools    *toolruntime.Registry
	config   Config
	// compressFailures counts consecutive compression failures in the current
	// run, for the circuit breaker (design D6).
	compressFailures int
	// PendingApproval, when set after Run returns ErrAwaitingApproval, is the
	// gated tool call that suspended the run. The run worker reads it to persist
	// the durable Approval.
	PendingApproval *ApprovalRequest
}

// New creates a Loop.
func New(p provider.Adapter, tools *toolruntime.Registry, cfg Config) *Loop {
	if cfg.MaxIterations == 0 {
		cfg.MaxIterations = 25
	}
	if cfg.CompressThreshold <= 0 {
		cfg.CompressThreshold = 0.8
	}
	if cfg.MaxCompressFailures <= 0 {
		cfg.MaxCompressFailures = 3
	}
	if cfg.MaxOverflowRetries <= 0 {
		cfg.MaxOverflowRetries = 3
	}
	return &Loop{provider: p, tools: tools, config: cfg}
}

func (l *Loop) maxOverflowRetries() int { return l.config.MaxOverflowRetries }

// attempt runs one provider call over the given working view and consumes the
// stream into an assembled assistant message + tool calls. It returns the
// provider error verbatim (no KindError emit) so the caller can distinguish a
// retriable context-overflow from a fatal failure.
func (l *Loop) attempt(ctx context.Context, view []provider.Message, emit Emitter) (provider.Message, []toolruntime.Call, turnEnd, error) {
	req := provider.Request{
		Model:           l.config.Model,
		System:          l.config.System,
		Messages:        view,
		Tools:           l.toolDefs(),
		MaxTokens:       l.config.MaxTokens,
		CacheablePrefix: l.config.CacheablePrefix,
	}
	// Materialize image blocks (path → base64) before every send so the
	// provider receives the payload; byte-stable across turns for caching.
	if l.config.Images != nil {
		req = provider.MaterializeImages(ctx, req, l.config.Images)
	}

	events, err := l.provider.Stream(ctx, req)
	if err != nil {
		return provider.Message{}, nil, turnEnd{}, fmt.Errorf("stream: %w", err)
	}
	return l.consume(ctx, events, emit)
}

// WithImages sets the image resolver used to materialize BlockImage paths to
// base64 before each send. It is called once per run, after the loop is built,
// when the session (and thus the confined resolver) is known.
func (l *Loop) WithImages(res provider.ImageResolver) *Loop {
	l.config.Images = res
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

// WithApproval marks the loop as resuming a parked run after a human tool-
// approval decision (capability-gap O2). The next Run resolves the decided call
// first (execute on approve / inject a denial on reject), then continues the
// think→tool cycle. Returns the loop for chaining.
func (l *Loop) WithApproval(ra ResumedApproval) *Loop {
	l.config.Approval = &ra
	return l
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
// produces). When the view approaches the model's context window it is
// compressed (older rounds summarized); compression rewrites only the view —
// the durable conversation record the caller keeps is never touched (design
// D1), and the split respects tool_use/tool_result pairing (D2/D3).
func (l *Loop) Run(ctx context.Context, history []provider.Message, emit Emitter) ([]provider.Message, error) {
	var produced []provider.Message

	// total accumulates token usage across the run's provider calls (one per
	// turn); it is reported once at termination via KindUsage.
	var total provider.Usage

	// A resumed run (capability-gap O2) starts by resolving the human-approved
	// tool call: execute it (Approved) or feed the denial back (rejected), then
	// continue the think→tool cycle. This runs ONCE, before the first provider
	// call of the resumed run.
	if ra := l.config.Approval; ra != nil {
		res := l.resolveApproval(ctx, *ra, emit)
		resultMsg := toolResultMessage(
			[]toolruntime.Call{{ID: ra.ToolCallID, Name: ra.ToolName, Args: ra.Input}},
			[]toolruntime.Result{res},
		)
		produced = append(produced, resultMsg)
		_ = emit.Emit(ctx, KindMessage, resultMsg)
		l.config.Approval = nil
	}

	for iter := 0; iter < l.config.MaxIterations; iter++ {
		// Honour cancellation between iterations (e.g. after a tool batch).
		if err := ctx.Err(); err != nil {
			_ = emit.Emit(ctx, KindCancelled, nil)
			return produced, err
		}

		// Assemble the working view for this turn and repair tool pairing before
		// every send (a prior cancel or a compression split can leave an unpaired
		// block, which the provider would reject).
		view := contextmgmt.EnsurePairing(append(append([]provider.Message{}, history...), produced...))
		// Compress the view when it crosses the budget. A compression failure
		// trips the circuit breaker rather than aborting the run; a success (or a
		// no-op under threshold) resets the consecutive-failure count. A skipped
		// attempt (breaker already tripped) leaves the count alone.
		compressed, err := l.maybeCompress(ctx, view)
		switch {
		case err == nil:
			view = compressed
			l.compressFailures = 0
		case errors.Is(err, errCompressionSkipped):
			// breaker already tripped — don't re-count
		default:
			l.compressFailures++
			slog.Warn("agent: compression failed; using uncompressed view", "iter", iter, "err", err, "failures", l.compressFailures)
		}

		assistant, toolCalls, end, err := l.attempt(ctx, view, emit)
		for attempts := 0; err != nil && provider.IsContextOverflow(err) && attempts < l.maxOverflowRetries(); attempts++ {
			// Reactive fallback (design D7): the threshold trigger mis-estimated
			// and the provider rejected the request as too large. Drop the oldest
			// round and retry rather than failing the run.
			shrunk, ok := contextmgmt.DropOldestRound(view)
			if !ok {
				break // nothing safe left to drop
			}
			view = shrunk
			assistant, toolCalls, end, err = l.attempt(ctx, view, emit)
		}
		if err != nil {
			if ctx.Err() != nil {
				slog.Info("agent: run cancelled", "iter", iter, "ctx_err", ctx.Err(), "attempt_err", err)
				_ = emit.Emit(ctx, KindCancelled, nil)
				return produced, ctx.Err()
			}
			slog.Error("agent: provider attempt failed; run aborting", "iter", iter, "err", err, "view_msgs", len(view))
			_ = emit.Emit(ctx, KindError, err.Error())
			return produced, err
		}
		if end.usage != nil {
			total.InputTokens += end.usage.InputTokens
			total.OutputTokens += end.usage.OutputTokens
			total.CacheReadTokens += end.usage.CacheReadTokens
			total.CacheWriteTokens += end.usage.CacheWriteTokens
		}
		produced = append(produced, assistant)
		// Expose the assembled assistant message for full-block persistence
		// (persist-raw-messages). Emit failures here don't abort the run — the
		// persistence listener drops them — so ignore the error.
		_ = emit.Emit(ctx, KindMessage, assistant)

		// No tool calls → the turn is final. But distinguish a natural finish from
		// a max_tokens truncation: a truncated turn is a cut-off answer, not a
		// clean completion, so surface it as an error instead of a silent done
		// (capability-gap L1). Without this the loop treats truncation as success.
		if len(toolCalls) == 0 {
			if end.stop == provider.StopMaxTokens {
				truncErr := fmt.Errorf("response truncated: hit the max_tokens limit (%d)", l.config.MaxTokens)
				emitUsage(ctx, emit, total)
				_ = emit.Emit(ctx, KindError, truncErr.Error())
				return produced, truncErr
			}
			slog.Info("agent: run complete", "iterations", iter+1, "input_tokens", total.InputTokens, "output_tokens", total.OutputTokens)
			emitUsage(ctx, emit, total)
			_ = emit.Emit(ctx, KindDone, nil)
			return produced, nil
		}

		// Don't start a tool batch the caller has already cancelled.
		if err := ctx.Err(); err != nil {
			_ = emit.Emit(ctx, KindCancelled, nil)
			return produced, err
		}

		// Human-interaction gate (capability-gap O2 + ask_user): if a call needs a
		// human answer — a permission approval OR an ask_user question set — do NOT
		// dispatch the batch. Emit the request and suspend the run; the worker
		// parks it (RunWaitingApproval) and resumes on the user's response.
		if gate := l.interactionGate(toolCalls); gate != nil {
			l.PendingApproval = gate
			_ = emit.Emit(ctx, KindApprovalRequest, *gate)
			return produced, ErrAwaitingApproval
		}

		// Dispatch tool calls (concurrently) and append results.
		results := l.dispatch(ctx, toolCalls)
		for i, res := range results {
			_ = emit.Emit(ctx, KindToolResult, map[string]any{
				"tool_use_id": toolCalls[i].ID,
				"name":        toolCalls[i].Name,
				"content":     res.Content,
				"is_error":    res.IsError,
			})
		}
		resultMsg := toolResultMessage(toolCalls, results)
		produced = append(produced, resultMsg)
		// Expose the assembled tool-result message for full-block persistence.
		_ = emit.Emit(ctx, KindMessage, resultMsg)
	}

	// Reached the iteration guard without a final answer. Surface it as a terminal
	// error frame rather than a silent stop: without this emit the run still flips
	// to failed (registry.execute), but the client sees the stream just end with no
	// explanation. Emit KindError so a terminal frame always accompanies the failure.
	err := fmt.Errorf("max iterations (%d) exceeded", l.config.MaxIterations)
	emitUsage(ctx, emit, total)
	_ = emit.Emit(ctx, KindError, err.Error())
	return produced, err
}

// emitUsage reports the run's accumulated token usage as a terminal KindUsage
// event so the transport can surface real counts in its finish frame. A zero
// total (no adapter reported usage) is skipped.
func emitUsage(ctx context.Context, emit Emitter, total provider.Usage) {
	if total == (provider.Usage{}) {
		return
	}
	_ = emit.Emit(ctx, KindUsage, total)
}

// turnEnd carries the terminal metadata of one provider turn: why generation
// stopped and the reported token usage. It lets Run react to a max_tokens
// truncation and accumulate usage without growing consume's return list.
type turnEnd struct {
	stop  provider.StopReason
	usage *provider.Usage
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
				// so keep the last non-empty stop reason and usage seen.
				if ev.StopReason != provider.StopUnknown {
					end.stop = ev.StopReason
				}
				if ev.Usage != nil {
					end.usage = ev.Usage
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
			return &ApprovalRequest{Kind: "ask_user", ToolCallID: c.ID, ToolName: c.Name, Input: c.Args}
		}
		// Permission approval: a dangerous call the policy gates for a yes/no.
		if l.config.Permission != nil {
			if tool, ok := l.tools.Get(c.Name); ok {
				if deny, reason := l.config.Permission(tool); deny && IsApprovalReason(reason) {
					return &ApprovalRequest{Kind: "approval", ToolCallID: c.ID, ToolName: c.Name, Input: c.Args}
				}
			}
		}
	}
	return nil
}

// resolveApproval produces the result for a human-resolved interaction on
// resume. ask_user: the user's structured answer becomes the tool result (or a
// "skipped" note when they cancelled). Permission approval: an approved call
// executes (the approval is its authorization, so Permission is not re-checked);
// a rejected one becomes an is_error result so the model learns it was denied.
// The result is streamed like a normal tool result.
func (l *Loop) resolveApproval(ctx context.Context, ra ResumedApproval, emit Emitter) toolruntime.Result {
	var res toolruntime.Result
	switch {
	case ra.Kind == "ask_user" || ra.ToolName == AskUserToolName:
		if !ra.Approved {
			// Cancelled: the user skipped the questions, but the run continues so
			// the model can decide how to proceed without the input.
			res = toolruntime.Result{Content: "the user skipped these questions (no answer given)"}
		} else if data, err := json.Marshal(ra.Answer); err == nil {
			res = toolruntime.Result{Content: string(data)}
		} else {
			res = toolruntime.Result{Content: "the user answered (unparseable response)", IsError: true}
		}
	case !ra.Approved:
		res = toolruntime.Result{Content: "the user denied permission to run " + ra.ToolName, IsError: true}
	default:
		res = l.tools.CallAll(ctx, []toolruntime.Call{{ID: ra.ToolCallID, Name: ra.ToolName, Args: ra.Input}})[0]
	}
	_ = emit.Emit(ctx, KindToolResult, map[string]any{
		"tool_use_id": ra.ToolCallID,
		"name":        ra.ToolName,
		"content":     res.Content,
		"is_error":    res.IsError,
	})
	return res
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
		if l.config.Permission != nil {
			if tool, ok := l.tools.Get(c.Name); ok {
				if deny, reason := l.config.Permission(tool); deny {
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

// maybeCompress compresses the working view when it crosses the context budget,
// returning the (possibly unchanged) view to send. Compression is skipped when
// it is not configured or the circuit breaker has tripped; the durable record
// is never touched — only the view handed to the provider shrinks (D1/D5).
func (l *Loop) maybeCompress(ctx context.Context, view []provider.Message) ([]provider.Message, error) {
	if l.config.Compressor == nil || l.config.ContextWindow <= 0 {
		return view, nil
	}
	if l.compressFailures >= l.config.MaxCompressFailures {
		// Breaker tripped: don't even call the failing summarizer. Return a
		// sentinel so the caller leaves the count alone (neither a fresh failure
		// nor a success that would reset it).
		return view, errCompressionSkipped
	}
	// Usable window reserves room for the model's reply (design D5).
	budget := l.config.ContextWindow - l.config.MaxTokens
	if budget <= 0 {
		budget = l.config.ContextWindow
	}
	policy := contextmgmt.Policy{
		MaxTokens:  budget,
		Threshold:  l.config.CompressThreshold,
		KeepRecent: 2, // recent rounds stay verbatim; older rounds are summarized
	}
	return contextmgmt.Compress(ctx, view, policy, l.config.Compressor)
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
