package subagent

import (
	"context"

	"nowhere-agent/internal/agent"
)

// Activity is a subagent progress signal surfaced to the run panel. It is a UI
// hint only — never part of the conversation the model sees, and never
// persisted (delivered as a live-only content event).
type Activity struct {
	AgentType string `json:"agentType"`
	Depth     int    `json:"depth"`
	Phase     string `json:"phase"`          // "start" | "tool" | "done" | "error"
	Tool      string `json:"tool,omitempty"` // tool name, for phase "tool"
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
// present, so the child stays black-box for content (only the collapsed result
// reaches the parent) while its progress is visible in the run panel.
type activityEmitter struct {
	sink      Sink
	agentType string
	depth     int
}

func (e activityEmitter) Emit(_ context.Context, kind agent.EventKind, payload any) error {
	if e.sink == nil {
		return nil
	}
	switch kind {
	case agent.KindToolUse:
		name := ""
		if m, ok := payload.(map[string]any); ok {
			name, _ = m["name"].(string)
		}
		e.sink(Activity{AgentType: e.agentType, Depth: e.depth, Phase: "tool", Tool: name})
	case agent.KindDone:
		e.sink(Activity{AgentType: e.agentType, Depth: e.depth, Phase: "done"})
	case agent.KindError:
		e.sink(Activity{AgentType: e.agentType, Depth: e.depth, Phase: "error"})
	}
	return nil
}
