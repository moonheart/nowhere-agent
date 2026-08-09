package toolruntime

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
)

// ValidateOutput checks a value against a declared JSON output schema, covering
// the subset client tools realistically declare: type, required, properties
// (recursive), items, enum, const, additionalProperties, string constraints
// (minLength/maxLength/pattern), numeric ranges (minimum/maximum and their
// exclusive variants), and array sizes (minItems/maxItems). It is deliberately
// a small validator — enough to reject malformed client output before folding —
// not a full JSON Schema implementation. Returns nil when the value conforms.
func ValidateOutput(schema map[string]any, value any) error {
	return validate(schema, value, "$")
}

// ValidateArgs checks decoded tool-call arguments against the tool's declared
// input Schema BEFORE execution, using the same subset validator as
// ValidateOutput (LangChain's _parse_input / LangGraph's ValidationNode run the
// equivalent screen). A violation is reported with the offending field path so
// the caller can feed a structured, self-correctable error back to the model
// instead of letting the tool choke on wrong-typed input. Returns nil when the
// arguments conform.
func ValidateArgs(schema map[string]any, args map[string]any) error {
	return validate(schema, args, "$")
}

func validate(schema map[string]any, value any, path string) error {
	if schema == nil {
		return nil
	}
	// const: value must equal the declared constant.
	if c, present := schema["const"]; present {
		if !jsonEqual(c, value) {
			return fmt.Errorf("%s: value %v does not equal const %v", path, value, c)
		}
	}
	// enum: value must be one of the listed constants.
	if enum := enumValues(schema["enum"]); len(enum) > 0 {
		if !inEnum(enum, value) {
			return fmt.Errorf("%s: value %v not in enum", path, value)
		}
	}
	typ, _ := schema["type"].(string)
	switch typ {
	case "":
		// No type constraint: nothing further to check at this node.
		return nil
	case "object":
		obj, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want object, got %T", path, value)
		}
		for _, req := range toStringSlice(schema["required"]) {
			if _, present := obj[req]; !present {
				return fmt.Errorf("%s: missing required property %q", path, req)
			}
		}
		props, _ := schema["properties"].(map[string]any)
		addl, _ := schema["additionalProperties"].(bool)
		for k, v := range obj {
			sub, declared := props[k]
			if !declared {
				if props != nil && !addl {
					return fmt.Errorf("%s: unexpected property %q", path, k)
				}
				continue
			}
			if subSchema, ok := sub.(map[string]any); ok {
				if err := validate(subSchema, v, path+"."+k); err != nil {
					return err
				}
			}
		}
		return nil
	case "array":
		arr, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s: want array, got %T", path, value)
		}
		if min, ok := toNumber(schema["minItems"]); ok && float64(len(arr)) < min {
			return fmt.Errorf("%s: array length %d below minItems %v", path, len(arr), min)
		}
		if max, ok := toNumber(schema["maxItems"]); ok && float64(len(arr)) > max {
			return fmt.Errorf("%s: array length %d above maxItems %v", path, len(arr), max)
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, el := range arr {
				if err := validate(items, el, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
		return nil
	case "string":
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s: want string, got %T", path, value)
		}
		n := float64(len([]rune(s)))
		if min, ok := toNumber(schema["minLength"]); ok && n < min {
			return fmt.Errorf("%s: string length %v below minLength %v", path, n, min)
		}
		if max, ok := toNumber(schema["maxLength"]); ok && n > max {
			return fmt.Errorf("%s: string length %v above maxLength %v", path, n, max)
		}
		if p, ok := schema["pattern"].(string); ok {
			re, err := regexp.Compile(p)
			if err == nil && !re.MatchString(s) {
				return fmt.Errorf("%s: string %q does not match pattern %q", path, s, p)
			}
		}
		return nil
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: want boolean, got %T", path, value)
		}
		return nil
	case "integer":
		f, ok := value.(float64)
		if !ok || f != math.Trunc(f) {
			return fmt.Errorf("%s: want integer, got %v", path, value)
		}
		return validateRange(schema, f, path)
	case "number":
		f, ok := value.(float64)
		if !ok {
			return fmt.Errorf("%s: want number, got %T", path, value)
		}
		return validateRange(schema, f, path)
	case "null":
		if value != nil {
			return fmt.Errorf("%s: want null, got %T", path, value)
		}
		return nil
	default:
		return fmt.Errorf("%s: unsupported schema type %q", path, typ)
	}
}

func inEnum(enum []any, value any) bool {
	for _, e := range enum {
		if jsonEqual(e, value) {
			return true
		}
	}
	return false
}

// validateRange enforces minimum/maximum (and their exclusive variants) on a
// numeric value.
func validateRange(schema map[string]any, f float64, path string) error {
	if min, ok := toNumber(schema["minimum"]); ok && f < min {
		return fmt.Errorf("%s: %v below minimum %v", path, f, min)
	}
	if max, ok := toNumber(schema["maximum"]); ok && f > max {
		return fmt.Errorf("%s: %v above maximum %v", path, f, max)
	}
	if min, ok := toNumber(schema["exclusiveMinimum"]); ok && f <= min {
		return fmt.Errorf("%s: %v not above exclusiveMinimum %v", path, f, min)
	}
	if max, ok := toNumber(schema["exclusiveMaximum"]); ok && f >= max {
		return fmt.Errorf("%s: %v not below exclusiveMaximum %v", path, f, max)
	}
	return nil
}

// enumValues normalizes an enum declaration. Schemas are authored both as
// decoded JSON ([]any) and as Go literals ([]string); both must be enforced.
func enumValues(x any) []any {
	switch arr := x.(type) {
	case []any:
		return arr
	case []string:
		out := make([]any, len(arr))
		for i, s := range arr {
			out[i] = s
		}
		return out
	}
	return nil
}

// jsonEqual compares two JSON values type-sensitively: the number 1 and the
// string "1" are different. Go-native literals (int, []string, ...) are
// normalized to their JSON-decoded form first so schema authors can use either.
func jsonEqual(a, b any) bool {
	return reflect.DeepEqual(normalizeJSON(a), normalizeJSON(b))
}

func normalizeJSON(v any) any {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int8:
		return float64(n)
	case int16:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case uint:
		return float64(n)
	case uint8:
		return float64(n)
	case uint16:
		return float64(n)
	case uint32:
		return float64(n)
	case uint64:
		return float64(n)
	case float32:
		return float64(n)
	case []string:
		out := make([]any, len(n))
		for i, s := range n {
			out[i] = s
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, el := range n {
			out[i] = normalizeJSON(el)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, el := range n {
			out[k] = normalizeJSON(el)
		}
		return out
	}
	return v
}

// toNumber reads a numeric schema keyword, accepting both decoded-JSON float64
// and Go-native int literals.
func toNumber(x any) (float64, bool) {
	switch n := normalizeJSON(x).(type) {
	case float64:
		return n, true
	}
	return 0, false
}

func toStringSlice(x any) []string {
	arr, ok := x.([]any)
	if !ok {
		if s, ok := x.([]string); ok {
			return s
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, el := range arr {
		if s, ok := el.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
