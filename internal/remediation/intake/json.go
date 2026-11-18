package intake

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	maxInputBytes = 8 << 20
	maxJSONDepth  = 32
	maxJSONValues = 131072
)

type jsonBudget struct {
	values int
}

func decodeJSON(data []byte, budget *jsonBudget) (any, error) {
	if len(data) > maxInputBytes {
		return nil, errors.New("report JSON exceeds the 8 MiB input limit")
	}
	if !utf8.Valid(data) || !validUnicodeEscapes(data) {
		return nil, errors.New("report JSON must contain valid UTF-8 and Unicode escapes")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := readJSONValue(decoder, 0, budget)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("report must contain exactly one JSON value")
	}
	return value, nil
}

func readJSONValue(decoder *json.Decoder, depth int, budget *jsonBudget) (any, error) {
	if depth > maxJSONDepth {
		return nil, errors.New("report JSON exceeds the nesting limit")
	}
	budget.values++
	if budget.values > maxJSONValues {
		return nil, errors.New("report JSON exceeds the value count limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, errors.New("invalid report JSON")
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		keys := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, errors.New("invalid report JSON object")
			}
			name, ok := key.(string)
			// Match the request decoder's case-collision safeguard without
			// including untrusted field names in diagnostics.
			canonical := strings.ToLower(name)
			if !ok || keys[canonical] {
				return nil, errors.New("report JSON contains duplicate or case-colliding fields")
			}
			keys[canonical] = true
			budget.values++
			value, err := readJSONValue(decoder, depth+1, budget)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
			return nil, errors.New("invalid report JSON object")
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			value, err := readJSONValue(decoder, depth+1, budget)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
			return nil, errors.New("invalid report JSON array")
		}
		return array, nil
	default:
		return nil, errors.New("invalid report JSON delimiter")
	}
}

// encoding/json replaces unpaired UTF-16 surrogates instead of rejecting them.
// Reject those escapes before decoding so provenance never describes repaired
// source text as if it had been supplied by the caller.
func validUnicodeEscapes(data []byte) bool {
	inString := false
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || i+1 >= len(data) {
				continue
			}
			if data[i+1] != 'u' {
				i++
				continue
			}
			unit, ok := unicodeUnit(data, i+2)
			if !ok {
				return false
			}
			i += 5
			if unit >= 0xdc00 && unit <= 0xdfff {
				return false
			}
			if unit >= 0xd800 && unit <= 0xdbff {
				if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
					return false
				}
				low, ok := unicodeUnit(data, i+3)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return false
				}
				i += 6
			}
		}
	}
	return true
}

func unicodeUnit(data []byte, offset int) (uint16, bool) {
	if offset+4 > len(data) {
		return 0, false
	}
	var unit uint16
	for _, digit := range data[offset : offset+4] {
		unit <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			unit += uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			unit += uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			unit += uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return unit, true
}
