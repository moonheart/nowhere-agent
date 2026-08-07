package agent

import (
	"context"
	"testing"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// stepCapture records the ordered step lifecycle (start/finish) and, for each
// finish, the StepEvent payload — so the test asserts both the frame sequence
// and the per-step reason/continued flags.
type stepCapture struct {
	memEmitter
	seq      []EventKind
	finishes []StepEvent
}

func (s *stepCapture) Emit(ctx context.Context, kind EventKind, payload any) error {
	if kind == KindStepStart || kind == KindStepFinish {
		s.seq = append(s.seq, kind)
	}
	if kind == KindStepFinish {
		if se, ok := payload.(StepEvent); ok {
			s.finishes = append(s.finishes, se)
		}
	}
	return s.memEmitter.Emit(ctx, kind, payload)
}

// TestMultiIterationRunEmitsStepSequence pins the step framing for a tool
// round-trip run (two model iterations: think→tool, then think→final). The
// first step must close continued with reason tool-calls; the second closes
// the run with reason stop. The client uses these to render per-step usage and
// to know a step is continued rather than terminal.
func TestMultiIterationRunEmitsStepSequence(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", "echo", `{"x":1}`),
		textResponse("final answer"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	emit := &stepCapture{}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	if _, err := loop.Run(context.Background(), []provider.Message{provider.TextMessage(provider.RoleUser, "hi")}, emit); err != nil {
		t.Fatal(err)
	}

	if emit.count(KindStepFinish) != 2 {
		t.Fatalf("expected 2 step finishes (one per iteration), got %d", emit.count(KindStepFinish))
	}
	if len(emit.finishes) != 2 {
		t.Fatalf("expected 2 StepEvent payloads, got %d", len(emit.finishes))
	}
	first, second := emit.finishes[0], emit.finishes[1]
	if first.FinishReason != "tool-calls" || !first.IsContinued {
		t.Errorf("step1 = reason %q continued %v, want tool-calls/continued", first.FinishReason, first.IsContinued)
	}
	if second.FinishReason != "stop" || second.IsContinued {
		t.Errorf("step2 = reason %q continued %v, want stop/terminal", second.FinishReason, second.IsContinued)
	}
}

// TestSingleTurnRunEmitsOneTerminalStep pins the degenerate case: a run with a
// single think→final iteration emits exactly one finish-step, with reason stop
// and isContinued false, and no start-step (only continued iterations open one).
func TestSingleTurnRunEmitsOneTerminalStep(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{textResponse("hello")}}
	reg := toolruntime.NewRegistry()
	emit := &stepCapture{}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	if _, err := loop.Run(context.Background(), []provider.Message{provider.TextMessage(provider.RoleUser, "hi")}, emit); err != nil {
		t.Fatal(err)
	}
	if emit.count(KindStepFinish) != 1 {
		t.Fatalf("expected 1 step finish, got %d", emit.count(KindStepFinish))
	}
	if len(emit.finishes) != 1 || emit.finishes[0].FinishReason != "stop" || emit.finishes[0].IsContinued {
		t.Errorf("finishes = %+v, want one stop/terminal step", emit.finishes)
	}
}
