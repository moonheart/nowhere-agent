package toolruntime

import (
	"fmt"
)

// ValidateOutput checks a value against a declared JSON output schema, covering
// the subset client tools realistically declare: type, required, properties
// (recursive), items, enum, and additionalProperties. It is deliberately a
// small validator — enough to reject malformed client output before folding —
// not a full JSON Schema implementation. Returns nil when the value conforms.
func ValidateOutput(schema map[string]any, value any) error {
	return validate(schema, value, "$")
}

func validate(schema map[string]any, value any, path string) error {
	if schema == nil {
		return nil
	}
	// enum: value must be one of the listed constants.
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
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
		if items, ok := schema["items"].(map[string]any); ok {
			for i, el := range arr {
				if err := validate(items, el, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
		return nil
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s: want string, got %T", path, value)
		}
		return nil
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: want boolean, got %T", path, value)
		}
		return nil
	case "integer":
		f, ok := value.(float64)
		if !ok || f != float64(int64(f)) {
			return fmt.Errorf("%s: want integer, got %v", path, value)
		}
		return nil
	case "number":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("%s: want number, got %T", path, value)
		}
		return nil
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
		if fmt.Sprintf("%v", e) == fmt.Sprintf("%v", value) {
			return true
		}
	}
	return false
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
