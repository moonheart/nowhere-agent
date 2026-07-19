// Package agent implements the agent-loop capability (design D1): a self-built
// think→tool→think loop. It owns orchestration, tool dispatch, streaming, and
// the in-context short-term memory, driving a provider.Adapter and emitting
// canonical events that the session runtime persists and fans out.
package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

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
}

// Loop runs the think→tool→think cycle.
type Loop struct {
	provider provider.Adapter
	tools    *toolruntime.Registry
	config   Config
	// compressFailures counts consecutive compression failures in the current
	// run, for the circuit breaker (design D6).
	compressFailures int
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
	return &Loop{provider: p, tools: tools, config: cfg}
}

// WithImages sets the image resolver used to materialize BlockImage paths to
// base64 before each send. It is called once per run, after the loop is built,
// when the session (and thus the confined resolver) is known.
func (l *Loop) WithImages(res provider.ImageResolver) *Loop {
	l.config.Images = res
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
		// no-op under threshold) resets the consecutive-failure count.
		if compressed, err := l.maybeCompress(ctx, view); err == nil {
			view = compressed
			l.compressFailures = 0
		} else {
			l.compressFailures++
		}

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
			_ = emit.Emit(ctx, KindError, err.Error())
			return produced, fmt.Errorf("stream: %w", err)
		}

		assistant, toolCalls, err := l.consume(ctx, events, emit)
		if err != nil {
			if ctx.Err() != nil {
				_ = emit.Emit(ctx, KindCancelled, nil)
				return produced, ctx.Err()
			}
			_ = emit.Emit(ctx, KindError, err.Error())
			return produced, err
		}
		produced = append(produced, assistant)
		// Expose the assembled assistant message for full-block persistence
		// (persist-raw-messages). Emit failures here don't abort the run — the
		// persistence listener drops them — so ignore the error.
		_ = emit.Emit(ctx, KindMessage, assistant)

		// No tool calls → final answer; loop ends.
		if len(toolCalls) == 0 {
			_ = emit.Emit(ctx, KindDone, nil)
			return produced, nil
		}

		// Don't start a tool batch the caller has already cancelled.
		if err := ctx.Err(); err != nil {
			_ = emit.Emit(ctx, KindCancelled, nil)
			return produced, err
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

	return produced, fmt.Errorf("max iterations (%d) exceeded", l.config.MaxIterations)
}

// consume reads one provider stream into an assembled assistant message and
// the list of tool calls, forwarding text/thinking to the emitter. It stops at
// once when ctx is cancelled (client Stop / server cancel) so a run can be
// interrupted mid-stream rather than only between iterations.
func (l *Loop) consume(ctx context.Context, events <-chan provider.Event, emit Emitter) (provider.Message, []toolruntime.Call, error) {
	assistant := provider.Message{Role: provider.RoleAssistant}
	var calls []toolruntime.Call

	// Track open blocks to accumulate deltas.
	open := map[int]*accumulator{}

	for {
		select {
		case <-ctx.Done():
			return assistant, calls, ctx.Err()
		case ev, ok := <-events:
			if !ok {
				// The provider closes its channel when the request is cancelled
				// (aborting the HTTP body) as well as at natural end. A cancelled
				// run must surface as such, not look like a clean finish.
				if err := ctx.Err(); err != nil {
					return assistant, calls, err
				}
				// Stream closed naturally: finalize any blocks still open.
				for idx, acc := range open {
					blk := acc.finalize()
					assistant.Content = append(assistant.Content, blk)
					if blk.Type == provider.BlockToolUse {
						calls = append(calls, toolruntime.Call{ID: blk.ToolUseID, Name: blk.ToolName, Args: blk.ToolInput})
					}
					delete(open, idx)
				}
				return assistant, calls, nil
			}

			switch ev.Type {
			case provider.EventError:
				return assistant, calls, ev.Err

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
						return assistant, calls, emitErr
					}
				}

			case provider.EventBlockStop:
				if acc, ok := open[ev.Index]; ok {
					blk := acc.finalize()
					assistant.Content = append(assistant.Content, blk)
					if blk.Type == provider.BlockToolUse {
						calls = append(calls, toolruntime.Call{ID: blk.ToolUseID, Name: blk.ToolName, Args: blk.ToolInput})
						_ = emit.Emit(ctx, KindToolUse, map[string]any{
							"id": blk.ToolUseID, "name": blk.ToolName, "input": blk.ToolInput,
						})
					}
					delete(open, ev.Index)
				}

			case provider.EventMessageStop:
				// usage could be recorded here for observability.
			}
		}
	}
}

// dispatch runs tool calls concurrently via the registry.
func (l *Loop) dispatch(ctx context.Context, calls []toolruntime.Call) []toolruntime.Result {
	return l.tools.CallAll(ctx, calls)
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
		return view, nil // breaker tripped: stop paying for a failing summarizer
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
func toolResultMessage(calls []toolruntime.Call, results []toolruntime.Result) provider.Message {
	msg := provider.Message{Role: provider.RoleUser}
	for i, res := range results {
		msg.Content = append(msg.Content, provider.Block{
			Type:         provider.BlockToolResult,
			ToolResultID: calls[i].ID,
			ToolContent:  res.Content,
			IsError:      res.IsError,
		})
	}
	return msg
}

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

func (a *accumulator) finalize() provider.Block {
	b := a.block
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
			if err := json.Unmarshal([]byte(a.json), &input); err == nil {
				b.ToolInput = input
			}
		}
	}
	return b
}
