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
	"regexp"
	"strconv"
	"strings"

	"github.com/orka-agents/orka/internal/gateway/protocol"
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

// jsonNumberPattern matches a JSON number literal.
var jsonNumberPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

// normalizeArgTypes checks top-level arguments against the JSON types the tool
// schema declares, before the tool sees them. A null value for a declared
// field counts as omitted. Numeric strings become numbers for integer and number fields, and
// strconv.ParseBool strings become booleans, matching the leniency tools
// already apply. A value whose type plainly contradicts the schema is
// rejected: an object or array for a string, a non-numeric value for a number,
// a non-boolean value for a boolean, a non-array for an array, and a number,
// boolean, or array for an object. Strings stay allowed for objects, which
// some tools read as shorthand, and numbers and booleans stay allowed for
// strings. Fields the schema does not declare pass through unchanged.
func normalizeArgTypes(tool Tool, args json.RawMessage) (json.RawMessage, error) {
	values, err := decodeArgObject(args)
	if err != nil || len(values) == 0 {
		return args, err
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
			// Undeclared fields, null or not, are left for the tool to judge,
			// as reply_in_conversation does when it rejects extra fields.
			if _, declared := schema.Properties[name]; declared {
				delete(values, name)
				changed = true
			}
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

// decodeArgObject decodes a JSON object of arguments. It returns nil values
// for invalid JSON, trailing data, or a non-object, which the tool rejects
// itself. A duplicate key is an error: the tool would see only one of the
// values, and the others would skip the type check. So is invalid Unicode
// (invalid UTF-8 or an unpaired surrogate escape), which decoding would
// silently replace.
func decodeArgObject(args json.RawMessage) (map[string]any, error) {
	if !json.Valid(args) {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.UseNumber()
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, nil
	}
	values := map[string]any{}
	for decoder.More() {
		token, err := decoder.Token()
		key, isKey := token.(string)
		if err != nil || !isKey {
			return nil, nil
		}
		if _, duplicate := values[key]; duplicate {
			return nil, &ToolArgumentError{Field: key, Want: "sent once", Got: "a duplicate"}
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, nil
		}
		if !protocol.ValidJSONUnicode(raw) {
			return nil, &ToolArgumentError{Field: key, Want: "valid Unicode text", Got: "invalid Unicode"}
		}
		value, err := decodeArgValue(raw)
		if err != nil {
			return nil, nil
		}
		values[key] = value
	}
	if !protocol.ValidJSONUnicode(args) {
		return nil, &ToolArgumentError{Field: "every field name", Want: "valid Unicode text", Got: "invalid Unicode"}
	}
	return values, nil
}

// decodeArgValue decodes one argument value, keeping numbers as json.Number.
func decodeArgValue(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	err := decoder.Decode(&value)
	return value, err
}

// canonicalJSONInteger returns the plain spelling of an integral JSON number
// literal, such as "10" for "10.0" or "1e3", which tools decoding into Go
// integers accept. It works on the text, so float64 rounding cannot hide a
// fraction. got describes the value when it is not a whole number that fits in
// an int64.
func canonicalJSONInteger(text string) (canonical, got string) {
	const outOfRange = "a number out of range"
	// Clamp the exponent so the arithmetic below cannot overflow. A value this
	// far out is a fraction or out of range either way.
	const maxExponent = 1 << 20
	mantissa, exponent, _ := strings.Cut(strings.ToLower(text), "e")
	exp := 0
	if exponent != "" {
		parsed, err := strconv.Atoi(exponent)
		if err != nil {
			parsed = maxExponent
			if strings.HasPrefix(exponent, "-") {
				parsed = -maxExponent
			}
		}
		exp = max(-maxExponent, min(parsed, maxExponent))
	}
	sign := ""
	if rest, negative := strings.CutPrefix(mantissa, "-"); negative {
		sign, mantissa = "-", rest
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	fraction = strings.TrimRight(fraction, "0")
	digits := strings.TrimLeft(whole+fraction, "0")
	if digits == "" {
		return "0", ""
	}
	// The value is digits * 10^-scale.
	switch scale := len(fraction) - exp; {
	case scale > 0:
		if len(digits)-len(strings.TrimRight(digits, "0")) < scale {
			return "", "a fraction"
		}
		digits = digits[:len(digits)-scale]
	case scale < 0:
		// Bound the zeros before writing them; an int64 has at most 19 digits.
		if len(digits)-scale > 19 {
			return "", outOfRange
		}
		digits += strings.Repeat("0", -scale)
	}
	if _, err := strconv.ParseInt(sign+digits, 10, 64); err != nil {
		return "", outOfRange
	}
	return sign + digits, ""
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
		if !jsonNumberPattern.MatchString(text) {
			return nil, &ToolArgumentError{Field: name, Want: kind, Got: "a non-numeric string"}
		}
		_, isString := value.(string)
		if want == jsonSchemaTypeInteger {
			canonical, got := canonicalJSONInteger(text)
			if got != "" {
				return nil, &ToolArgumentError{Field: name, Want: kind, Got: got}
			}
			if isString || canonical != text {
				return json.Number(canonical), nil
			}
			return nil, nil
		}
		if parsed, err := strconv.ParseFloat(text, 64); err != nil || math.IsInf(parsed, 0) {
			return nil, &ToolArgumentError{Field: name, Want: kind, Got: "a number out of range"}
		}
		if isString {
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
