/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	jsonSchemaTypeNumber = "number"
	kindBoolean          = "a boolean"
)

// ToolNotFoundError reports a call to a tool the registry does not hold.
type ToolNotFoundError struct {
	Name string
}

func (e *ToolNotFoundError) Error() string {
	return fmt.Sprintf("tool %q not found", e.Name)
}

// ToolArgumentError reports a top-level argument whose JSON type contradicts
// the tool schema.
type ToolArgumentError struct {
	Field string
	Want  string
	Got   string
}

func (e *ToolArgumentError) Error() string {
	return fmt.Sprintf("invalid arguments: %s must be %s, got %s", e.Field, e.Want, e.Got)
}

// normalizeArgTypes checks top-level arguments against the JSON types the tool
// schema declares, before the tool sees them. A null argument counts as
// omitted. Numeric strings become numbers for integer and number fields, and
// strconv.ParseBool strings become booleans, matching the leniency tools
// already apply. A value whose type plainly contradicts the schema is
// rejected: an object or array for a string, a non-numeric value for a number,
// a non-boolean value for a boolean, a non-array for an array, and a number,
// boolean, or array for an object. Strings stay allowed for objects, which
// some tools read as shorthand, and numbers and booleans stay allowed for
// strings. Fields the schema does not declare pass through unchanged.
func normalizeArgTypes(tool Tool, args json.RawMessage) (json.RawMessage, error) {
	var values map[string]any
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil || len(values) == 0 {
		return args, nil
	}
	var schema struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
		return args, nil
	}

	changed := false
	for name, value := range values {
		if value == nil {
			delete(values, name)
			changed = true
			continue
		}
		want, _ := schema.Properties[name].Type.(string)
		normalized, err := normalizeArgType(name, want, value)
		if err != nil {
			return nil, err
		}
		if normalized != nil {
			values[name] = normalized
			changed = true
		}
	}
	if !changed {
		return args, nil
	}
	normalized, err := json.Marshal(values)
	if err != nil {
		return args, nil
	}
	return normalized, nil
}

// normalizeArgType returns a replacement value, nil to keep the value as is,
// or an error when the value contradicts the declared type.
func normalizeArgType(name, want string, value any) (any, error) {
	switch want {
	case jsonSchemaTypeString:
		switch value.(type) {
		case map[string]any, []any:
			return nil, &ToolArgumentError{Field: name, Want: "a string", Got: jsonValueKind(value)}
		}
	case jsonSchemaTypeInteger, jsonSchemaTypeNumber:
		kind := "a number"
		if want == jsonSchemaTypeInteger {
			kind = "a whole number"
		}
		text := ""
		switch v := value.(type) {
		case json.Number:
			text = v.String()
		case string:
			text = strings.TrimSpace(v)
		default:
			return nil, &ToolArgumentError{Field: name, Want: kind, Got: jsonValueKind(value)}
		}
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return nil, &ToolArgumentError{Field: name, Want: kind, Got: "a non-numeric string"}
		}
		if want == jsonSchemaTypeInteger && parsed != math.Trunc(parsed) {
			return nil, &ToolArgumentError{Field: name, Want: kind, Got: "a fraction"}
		}
		if _, isString := value.(string); isString {
			return json.Number(text), nil
		}
	case jsonSchemaTypeBoolean:
		switch v := value.(type) {
		case bool:
		case string:
			parsed, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil {
				return nil, &ToolArgumentError{Field: name, Want: kindBoolean, Got: "a non-boolean string"}
			}
			return parsed, nil
		default:
			return nil, &ToolArgumentError{Field: name, Want: kindBoolean, Got: jsonValueKind(value)}
		}
	case jsonSchemaTypeArray:
		if _, isArray := value.([]any); !isArray {
			return nil, &ToolArgumentError{Field: name, Want: "an array", Got: jsonValueKind(value)}
		}
	case jsonSchemaTypeObject:
		switch value.(type) {
		case map[string]any, string:
		default:
			return nil, &ToolArgumentError{Field: name, Want: "an object", Got: jsonValueKind(value)}
		}
	}
	return nil, nil
}

func jsonValueKind(value any) string {
	switch value.(type) {
	case bool:
		return kindBoolean
	case json.Number, float64:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return "null"
}
