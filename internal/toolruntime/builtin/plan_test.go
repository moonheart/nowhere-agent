package builtin

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/toolruntime"
)

func TestPlanWriteRiskAndSchema(t *testing.T) {
	tl := NewPlanWrite(func(context.Context, string, any) error { return nil })
	if tl.Risk() != toolruntime.RiskReadOnly {
		t.Errorf("risk = %q want read_only", tl.Risk())
	}
	if tl.Name() != PlanWriteToolName {
		t.Errorf("name = %q", tl.Name())
	}
}

func TestPlanWriteCallsWriterWithPlan(t *testing.T) {
	var gotKey string
	var gotVal any
	tl := NewPlanWrite(func(_ context.Context, key string, value any) error {
		gotKey, gotVal = key, value
		return nil
	})
	res, err := tl.Call(context.Background(), map[string]any{
		"items": []any{
			map[string]any{"content": "Read config", "status": "completed"},
			map[string]any{"content": "Edit file", "status": "in_progress", "activeForm": "Editing file"},
			map[string]any{"content": "Run tests", "status": "pending"},
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if gotKey != PlanStateKey {
		t.Errorf("writer key = %q want %q", gotKey, PlanStateKey)
	}
	p, ok := gotVal.(plan)
	if !ok {
		t.Fatalf("writer value type = %T", gotVal)
	}
	if len(p.Items) != 3 || p.Items[1].Status != "in_progress" || p.Items[1].ActiveForm != "Editing file" {
		t.Errorf("plan = %+v", p)
	}
	if !strings.Contains(res.Content, "(1/3 completed)") {
		t.Errorf("result = %q", res.Content)
	}
}

func TestPlanWriteValidation(t *testing.T) {
	tl := NewPlanWrite(func(context.Context, string, any) error { return nil })
	cases := map[string]map[string]any{
		"missing items": {},
		"empty content": {"items": []any{
			map[string]any{"content": "  ", "status": "pending"},
		}},
		"bad status": {"items": []any{
			map[string]any{"content": "x", "status": "done"},
		}},
		"two in_progress": {"items": []any{
			map[string]any{"content": "a", "status": "in_progress"},
			map[string]any{"content": "b", "status": "in_progress"},
		}},
	}
	for name, args := range cases {
		res, _ := tl.Call(context.Background(), args)
		if !res.IsError {
			t.Errorf("%s: expected IsError, got %q", name, res.Content)
		}
	}
}

func TestPlanWriteNilWriter(t *testing.T) {
	tl := NewPlanWrite(nil)
	res, _ := tl.Call(context.Background(), map[string]any{
		"items": []any{map[string]any{"content": "x", "status": "pending"}},
	})
	if !res.IsError {
		t.Error("expected IsError when writer is nil")
	}
}
