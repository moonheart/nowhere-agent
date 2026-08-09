package builtin

import (
	"context"
	"testing"
)

// TestTestUIToolShape pins the generative-UI smoke test tool: metadata for the
// model, and a fixed spec the client's allowlist renders.
func TestTestUIToolShape(t *testing.T) {
	tool := NewTestUI()
	if tool.Name() != TestUIToolName {
		t.Errorf("name = %q want %q", tool.Name(), TestUIToolName)
	}
	if tool.Risk() != "read_only" {
		t.Errorf("risk = %q want read_only", tool.Risk())
	}
	if _, ok := tool.Schema()["type"]; !ok {
		t.Error("schema missing type")
	}

	res, err := tool.Call(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.GenerativeUI == nil {
		t.Fatal("Call returned no GenerativeUI spec")
	}
	if len(res.GenerativeUI.Root) != 1 {
		t.Fatalf("spec root has %d nodes, want 1", len(res.GenerativeUI.Root))
	}
	card := res.GenerativeUI.Root[0]
	if card.Component != "test-ui-card" {
		t.Errorf("root component = %q want test-ui-card", card.Component)
	}
	if card.Props["title"] != "Generative UI works" {
		t.Errorf("title prop = %v", card.Props["title"])
	}
	if len(card.Children) != 3 {
		t.Errorf("card has %d children, want 3 bullets", len(card.Children))
	}
	for _, c := range card.Children {
		if c.Component != "test-ui-bullet" {
			t.Errorf("child component = %q want test-ui-bullet", c.Component)
		}
	}
}
