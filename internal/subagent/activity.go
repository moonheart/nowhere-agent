package subagent

import (
	"context"

	"nowhere-agent/internal/agent"
)

// Activity is a subagent progress signal surfaced to the run panel and, for
// streamed output, to the chat thread. It is a UI hint only — never part of the
// conversation the model sees, and never persisted (delivered as a live-only
// content event).
type Activity struct {
	AgentType string `json:"agentType"`
	Depth     int    `json:"depth"`
	Phase     string `json:"phase"`          // "start" | "stream" | "tool" | "result" | "interrupted" | "done" | "error"
	Tool      string `json:"tool,omitempty"` // tool name, for phase "tool"/"result"
	// ToolCallID ties the signal to the spawn_agent tool call that launched this
	// child, so the UI can nest output under the right card when several
	// subagents run in parallel.
	ToolCallID string `json:"toolCallId,omitempty"`
	// Kind classifies a Phase "stream" payload: "text" | "thinking".
	Kind string `json:"kind,omitempty"`
	// Text is one streamed delta of the child's own output (Phase "stream").
	Text string `json:"text,omitempty"`
	// SubToolCallID is the child's own tool call's id (Phase "tool"/"result"),
	// so the UI can render that tool invocation (and match its result) exactly
	// like a top-level call.
	SubToolCallID string `json:"subToolCallId,omitempty"`
	// Args is the child tool call's input (Phase "tool"); Result its output and
	// IsError its error flag (Phase "result").
	Args    any    `json:"args,omitempty"`
	Result  string `json:"result,omitempty"`
	IsError bool   `json:"isError,omitempty"`
}

// Sink receives subagent activity for forwarding to the UI. The run worker
// installs one via WithSink; a nil sink (no runtime, e.g. tests/dev) means
// subagents run black-box.
type Sink func(Activity)

type sinkKey struct{}

// WithSink returns ctx carrying a subagent activity sink. Because the sink rides
// in the run context, nested subagents at any depth report to the same run
// stream without extra plumbing.
func WithSink(ctx context.Context, s Sink) context.Context {
	return context.WithValue(ctx, sinkKey{}, s)
}

func sinkFrom(ctx context.Context) Sink {
	s, _ := ctx.Value(sinkKey{}).(Sink)
	return s
}

// activityEmitter forwards a child loop's events to the run's activity sink as
// compact Activity signals. It stands in for the discard emitter when a sink is
// present. The child's collapsed result still reaches the parent as the tool
// result; this emitter only mirrors progress + a live copy of the child's
// streamed text/thinking so the UI can show the subagent working in real time.
type activityEmitter struct {
	sink       Sink
	agentType  string
	depth      int
	toolCallID string
	// interrupted records that the child ended awaiting client interactions
	// (approval/ask_user/client_tool). The loop still emits KindDone after its
	// interrupt frames; that trailing "done" is suppressed so the UI doesn't
	// show a gated child as finished.
	interrupted bool
}

func (e *activityEmitter) Emit(_ context.Context, kind agent.EventKind, payload any) error {
	if e.sink == nil {
		return nil
	}
	base := Activity{AgentType: e.agentType, Depth: e.depth, ToolCallID: e.toolCallID}
	switch kind {
	case agent.KindText:
		if s, ok := payload.(string); ok && s != "" {
			base.Phase, base.Kind, base.Text = "stream", "text", s
			e.sink(base)
		}
	case agent.KindThinking:
		if s, ok := payload.(string); ok && s != "" {
			base.Phase, base.Kind, base.Text = "stream", "thinking", s
			e.sink(base)
		}
	case agent.KindToolUse:
		base.Phase = "tool"
		if m, ok := payload.(map[string]any); ok {
			base.Tool, _ = m["name"].(string)
			base.SubToolCallID, _ = m["id"].(string)
			base.Args = m["input"]
		}
		e.sink(base)
	case agent.KindToolResult:
		base.Phase = "result"
		if m, ok := payload.(map[string]any); ok {
			base.Tool, _ = m["name"].(string)
			base.SubToolCallID, _ = m["tool_use_id"].(string)
			base.Result, _ = m["content"].(string)
			base.IsError, _ = m["is_error"].(bool)
		}
		e.sink(base)
	case agent.KindInterrupt:
		// The child hit a gate it cannot resolve (subagents can't suspend for
		// human input). Surface it so the UI shows the child stalled on an
		// approval/question rather than silently finishing.
		e.interrupted = true
		base.Phase = "interrupted"
		if g, ok := payload.(agent.Interaction); ok {
			base.Tool = g.ToolName
			base.SubToolCallID = g.ToolCallID
			base.Kind = g.Kind
		}
		e.sink(base)
	case agent.KindDone:
		if e.interrupted {
			return nil
		}
		base.Phase = "done"
		e.sink(base)
	case agent.KindError:
		base.Phase = "error"
		e.sink(base)
	}
	return nil
}
