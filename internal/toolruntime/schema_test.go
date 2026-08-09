package toolruntime

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestValidateOutput covers the schema subset the client_tool fold relies on:
// type checks, required properties, nested properties, arrays, and enums.
func TestValidateOutput(t *testing.T) {
	textObj := map[string]any{
		"type":       "object",
		"properties": map[string]any{"text": map[string]any{"type": "string"}},
		"required":   []string{"text"},
	}
	cases := []struct {
		name    string
		schema  map[string]any
		value   any
		wantErr bool
	}{
		{"nil schema accepts", nil, map[string]any{"x": 1}, false},
		{"valid object", textObj, map[string]any{"text": "hi"}, false},
		{"missing required", textObj, map[string]any{"other": 1.0}, true},
		{"wrong property type", textObj, map[string]any{"text": 5.0}, true},
		{"non-object", textObj, "a string", true},
		{"string ok", map[string]any{"type": "string"}, "s", false},
		{"string wrong", map[string]any{"type": "string"}, 3.0, true},
		{"integer ok", map[string]any{"type": "integer"}, 4.0, false},
		{"integer non-integer", map[string]any{"type": "integer"}, 4.5, true},
		{"number ok", map[string]any{"type": "number"}, 4.5, false},
		{"boolean ok", map[string]any{"type": "boolean"}, true, false},
		{"enum match", map[string]any{"enum": []any{"a", "b"}}, "b", false},
		{"enum miss", map[string]any{"enum": []any{"a", "b"}}, "c", true},
		{"array items ok", map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, []any{"x", "y"}, false},
		{"array items bad", map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, []any{"x", 2.0}, true},
		{"nested ok", map[string]any{"type": "object", "properties": map[string]any{"inner": textObj}}, map[string]any{"inner": map[string]any{"text": "v"}}, false},
		{"nested bad", map[string]any{"type": "object", "properties": map[string]any{"inner": textObj}}, map[string]any{"inner": map[string]any{"text": 9.0}}, true},
		{"no type passes", map[string]any{"properties": map[string]any{}}, map[string]any{"a": 1.0}, false},
		{"enum number/string not conflated", map[string]any{"enum": []any{1.0}}, "1", true},
		{"enum []string match", map[string]any{"enum": []string{"a", "b"}}, "a", false},
		{"enum []string miss", map[string]any{"enum": []string{"a", "b"}}, "c", true},
		{"const match", map[string]any{"const": "v1"}, "v1", false},
		{"const miss", map[string]any{"const": "v1"}, "v2", true},
		{"const number vs string", map[string]any{"const": 1}, "1", true},
		{"const go int vs json float", map[string]any{"const": 1}, 1.0, false},
		{"minLength ok", map[string]any{"type": "string", "minLength": 2}, "ab", false},
		{"minLength short", map[string]any{"type": "string", "minLength": 2}, "a", true},
		{"maxLength long", map[string]any{"type": "string", "maxLength": 2}, "abc", true},
		{"pattern ok", map[string]any{"type": "string", "pattern": `^a+$`}, "aaa", false},
		{"pattern miss", map[string]any{"type": "string", "pattern": `^a+$`}, "ab", true},
		{"minimum ok", map[string]any{"type": "number", "minimum": 0}, 0.0, false},
		{"minimum low", map[string]any{"type": "number", "minimum": 0}, -1.0, true},
		{"maximum high", map[string]any{"type": "integer", "maximum": 10}, 11.0, true},
		{"exclusiveMinimum edge", map[string]any{"type": "number", "exclusiveMinimum": 1}, 1.0, true},
		{"exclusiveMaximum edge", map[string]any{"type": "number", "exclusiveMaximum": 1}, 1.0, true},
		{"minItems short", map[string]any{"type": "array", "minItems": 1}, []any{}, true},
		{"maxItems long", map[string]any{"type": "array", "maxItems": 1}, []any{1.0, 2.0}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateOutput(c.schema, c.value)
			if c.wantErr && err == nil {
				t.Errorf("want error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Errorf("want no error, got %v", err)
			}
		})
	}
}

// TestValidateArgs pins the pre-execution input screen the agent loop and the
// suspended-batch fold rely on: the error names the offending field path (so
// the model can self-correct), and a property the schema never declares is
// rejected when the schema declares properties but not additionalProperties.
func TestValidateArgs(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
		"required":   []string{"path"},
	}
	if err := ValidateArgs(schema, map[string]any{"path": "/tmp/x"}); err != nil {
		t.Errorf("conforming args rejected: %v", err)
	}
	if err := ValidateArgs(nil, map[string]any{"anything": 1.0}); err != nil {
		t.Errorf("nil schema must accept: %v", err)
	}
	err := ValidateArgs(schema, map[string]any{"path": 123.0})
	if err == nil || !strings.Contains(err.Error(), "$.path") {
		t.Errorf("wrong-typed arg: err = %v, want it to name $.path", err)
	}
	err = ValidateArgs(schema, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), `"path"`) {
		t.Errorf("missing required: err = %v, want it to name the property", err)
	}
	err = ValidateArgs(schema, map[string]any{"path": "/tmp/x", "extra": true})
	if err == nil || !strings.Contains(err.Error(), `"extra"`) {
		t.Errorf("undeclared property: err = %v, want it to name the property", err)
	}
}

// TestIsClientTool verifies the optional-interface detection.
func TestIsClientTool(t *testing.T) {
	if IsClientTool(clientStub{side: false}) {
		t.Error("ClientSide()=false should not be a client tool")
	}
	if !IsClientTool(clientStub{side: true}) {
		t.Error("ClientSide()=true should be a client tool")
	}
	if IsClientTool(plainStub{}) {
		t.Error("a tool without the ClientTool interface is not a client tool")
	}
}

type clientStub struct{ side bool }

func (clientStub) Name() string               { return "c" }
func (clientStub) Description() string        { return "d" }
func (clientStub) Schema() map[string]any     { return nil }
func (clientStub) Risk() Risk                 { return RiskReadOnly }
func (clientStub) Timeout() (d time.Duration) { return }
func (c clientStub) Call(context.Context, map[string]any) (Result, error) {
	return Result{}, nil
}
func (c clientStub) ClientSide() bool           { return c.side }
func (clientStub) OutputSchema() map[string]any { return nil }

type plainStub struct{}

func (plainStub) Name() string               { return "p" }
func (plainStub) Description() string        { return "d" }
func (plainStub) Schema() map[string]any     { return nil }
func (plainStub) Risk() Risk                 { return RiskReadOnly }
func (plainStub) Timeout() (d time.Duration) { return }
func (plainStub) Call(context.Context, map[string]any) (Result, error) {
	return Result{}, nil
}
