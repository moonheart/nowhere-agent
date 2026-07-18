// Package agent implements the agent-loop capability (design D1): a self-built
// think→tool→think loop. It owns orchestration, tool dispatch, streaming, and
// the in-context short-term memory, driving a provider.Adapter and emitting
// canonical events that the session runtime persists and fans out.
package agent

import (
	"context"
	"encoding/json"
	"fmt"

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
	// KindUser marks a persisted user message. It is not emitted by the loop
	// itself; the transport writes it so replay reconstructs the user side.
	KindUser EventKind = "user"
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
}

// Loop runs the think→tool→think cycle.
type Loop struct {
	provider provider.Adapter
	tools    *toolruntime.Registry
	config   Config
}

// New creates a Loop.
func New(p provider.Adapter, tools *toolruntime.Registry, cfg Config) *Loop {
	if cfg.MaxIterations == 0 {
		cfg.MaxIterations = 25
	}
	return &Loop{provider: p, tools: tools, config: cfg}
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
func (l *Loop) Run(ctx context.Context, history []provider.Message, emit Emitter) ([]provider.Message, error) {
	var produced []provider.Message

	for iter := 0; iter < l.config.MaxIterations; iter++ {
		req := provider.Request{
			Model:           l.config.Model,
			System:          l.config.System,
			Messages:        append(append([]provider.Message{}, history...), produced...),
			Tools:           l.toolDefs(),
			MaxTokens:       l.config.MaxTokens,
			CacheablePrefix: l.config.CacheablePrefix,
		}

		events, err := l.provider.Stream(req)
		if err != nil {
			_ = emit.Emit(ctx, KindError, err.Error())
			return produced, fmt.Errorf("stream: %w", err)
		}

		assistant, toolCalls, err := l.consume(ctx, events, emit)
		if err != nil {
			_ = emit.Emit(ctx, KindError, err.Error())
			return produced, err
		}
		produced = append(produced, assistant)

		// No tool calls → final answer; loop ends.
		if len(toolCalls) == 0 {
			_ = emit.Emit(ctx, KindDone, nil)
			return produced, nil
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
		produced = append(produced, toolResultMessage(toolCalls, results))
	}

	return produced, fmt.Errorf("max iterations (%d) exceeded", l.config.MaxIterations)
}

// consume reads one provider stream into an assembled assistant message and
// the list of tool calls, forwarding text/thinking to the emitter.
func (l *Loop) consume(ctx context.Context, events <-chan provider.Event, emit Emitter) (provider.Message, []toolruntime.Call, error) {
	assistant := provider.Message{Role: provider.RoleAssistant}
	var calls []toolruntime.Call

	// Track open blocks to accumulate deltas.
	open := map[int]*accumulator{}

	for ev := range events {
		switch ev.Type {
		case provider.EventError:
			return assistant, nil, ev.Err

		case provider.EventBlockStart:
			if ev.Block != nil {
				open[ev.Index] = &accumulator{block: *ev.Block}
			}

		case provider.EventBlockDelta:
			if acc, ok := open[ev.Index]; ok {
				acc.append(ev.Delta)
				// Stream text/thinking out incrementally.
				switch acc.block.Type {
				case provider.BlockText:
					_ = emit.Emit(ctx, KindText, ev.Delta)
				case provider.BlockThinking:
					_ = emit.Emit(ctx, KindThinking, ev.Delta)
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

	// Finalize any blocks still open (defensive).
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

// dispatch runs tool calls concurrently via the registry.
func (l *Loop) dispatch(ctx context.Context, calls []toolruntime.Call) []toolruntime.Result {
	return l.tools.CallAll(ctx, calls)
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
	block provider.Block
	text  string
	json  string
}

func (a *accumulator) append(delta string) {
	switch a.block.Type {
	case provider.BlockText, provider.BlockThinking:
		a.text += delta
	case provider.BlockToolUse:
		a.json += delta
	}
}

func (a *accumulator) finalize() provider.Block {
	b := a.block
	switch b.Type {
	case provider.BlockText:
		b.Text = a.text
	case provider.BlockThinking:
		b.Thinking = a.text
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
