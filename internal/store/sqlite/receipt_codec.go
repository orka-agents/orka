package sqlite

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"time"
	"unicode/utf8"
)

// The receipt codec stores a write's result exactly as the write returned
// it, so a replay returns an equal value. It walks Go values rather than
// their JSON form, so fields Orka's API leaves out are kept, and it keeps
// what gob and JSON lose: a non-nil pointer to a zero value, a nil slice or
// map apart from an empty one, and the bytes of a string that is not valid
// UTF-8. Decoding is directed by the result type.
//
// A value encodes as JSON: a nil pointer, slice or map as null; a non-nil
// pointer as {"*": value}; a struct as an object of every exported field
// under its Go name; a time.Time as {"t": its MarshalBinary bytes}; a []byte
// as {"b": base64}; a string that is not valid UTF-8 as {"x": base64}; a map
// with string keys as an array of [key, value] pairs; integers as JSON
// numbers; and floats as strings, so NaN and infinities survive. A type the
// codec cannot represent exactly, such as an interface or an unexported
// field, fails the write rather than storing a result that replays wrongly.

var timeType = reflect.TypeFor[time.Time]()

func marshalReceiptValue(value any) ([]byte, error) {
	node, err := receiptNode(reflect.ValueOf(value))
	if err != nil {
		return nil, err
	}
	return json.Marshal(node)
}

func unmarshalReceiptValue(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var node any
	if err := decoder.Decode(&node); err != nil {
		return err
	}
	return setReceiptNode(node, reflect.ValueOf(target).Elem())
}

func receiptNode(v reflect.Value) (any, error) {
	if !v.IsValid() {
		return nil, nil
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return nil, nil
		}
		elem, err := receiptNode(v.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"*": elem}, nil
	case reflect.Struct:
		if v.Type() == timeType {
			encoded, err := v.Interface().(time.Time).MarshalBinary()
			if err != nil {
				return nil, err
			}
			return map[string]any{"t": encoded}, nil
		}
		fields := make(map[string]any, v.NumField())
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if !field.IsExported() {
				return nil, fmt.Errorf("%s has unexported field %s", v.Type(), field.Name)
			}
			node, err := receiptNode(v.Field(i))
			if err != nil {
				return nil, err
			}
			fields[field.Name] = node
		}
		return fields, nil
	case reflect.Slice:
		if v.IsNil() {
			return nil, nil
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return map[string]any{"b": base64.StdEncoding.EncodeToString(v.Bytes())}, nil
		}
		return receiptSequenceNode(v)
	case reflect.Array:
		return receiptSequenceNode(v)
	case reflect.Map:
		if v.IsNil() {
			return nil, nil
		}
		if v.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map key type %s is not a string", v.Type().Key())
		}
		// Entries are [key, value] pairs, so a key that is not valid UTF-8
		// keeps its bytes as a value would.
		entries := make([]any, 0, v.Len())
		iter := v.MapRange()
		for iter.Next() {
			node, err := receiptNode(iter.Value())
			if err != nil {
				return nil, err
			}
			entries = append(entries, []any{receiptStringNode(iter.Key().String()), node})
		}
		return entries, nil
	case reflect.Bool:
		return v.Bool(), nil
	case reflect.String:
		return receiptStringNode(v.String()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint(), nil
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'g', -1, 64), nil
	default:
		return nil, fmt.Errorf("%s values are not supported", v.Type())
	}
}

func receiptSequenceNode(v reflect.Value) (any, error) {
	items := make([]any, v.Len())
	for i := range v.Len() {
		node, err := receiptNode(v.Index(i))
		if err != nil {
			return nil, err
		}
		items[i] = node
	}
	return items, nil
}

func setReceiptNode(node any, v reflect.Value) error {
	if node == nil {
		switch v.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map:
			v.SetZero()
			return nil
		default:
			return fmt.Errorf("null for %s", v.Type())
		}
	}
	switch v.Kind() {
	case reflect.Pointer:
		wrapper, ok := node.(map[string]any)
		if !ok {
			return fmt.Errorf("pointer %s is not wrapped", v.Type())
		}
		elem := reflect.New(v.Type().Elem())
		if err := setReceiptNode(wrapper["*"], elem.Elem()); err != nil {
			return err
		}
		v.Set(elem)
		return nil
	case reflect.Struct:
		return setReceiptStruct(node, v)
	case reflect.Slice, reflect.Array:
		return setReceiptSequence(node, v)
	case reflect.Map:
		return setReceiptMap(node, v)
	default:
		return setReceiptScalar(node, v)
	}
}

func setReceiptStruct(node any, v reflect.Value) error {
	fields, ok := node.(map[string]any)
	if !ok {
		return fmt.Errorf("%s is not an object", v.Type())
	}
	if v.Type() == timeType {
		encoded, err := receiptBytes(fields["t"])
		if err != nil {
			return err
		}
		var t time.Time
		if err := t.UnmarshalBinary(encoded); err != nil {
			return err
		}
		v.Set(reflect.ValueOf(t))
		return nil
	}
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		if err := setReceiptNode(fields[name], v.Field(i)); err != nil {
			return fmt.Errorf("%s.%s: %w", v.Type(), name, err)
		}
	}
	return nil
}

func setReceiptSequence(node any, v reflect.Value) error {
	if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
		wrapper, ok := node.(map[string]any)
		if !ok {
			return fmt.Errorf("bytes %s are not wrapped", v.Type())
		}
		decoded, err := receiptBytes(wrapper["b"])
		if err != nil {
			return err
		}
		v.SetBytes(decoded)
		return nil
	}
	items, ok := node.([]any)
	if !ok {
		return fmt.Errorf("%s is not an array", v.Type())
	}
	if v.Kind() == reflect.Array {
		if len(items) != v.Len() {
			return fmt.Errorf("%s does not have %d items", v.Type(), v.Len())
		}
	} else {
		v.Set(reflect.MakeSlice(v.Type(), len(items), len(items)))
	}
	for i, item := range items {
		if err := setReceiptNode(item, v.Index(i)); err != nil {
			return err
		}
	}
	return nil
}

func setReceiptMap(node any, v reflect.Value) error {
	entries, ok := node.([]any)
	if !ok {
		return fmt.Errorf("%s is not an array of entries", v.Type())
	}
	m := reflect.MakeMapWithSize(v.Type(), len(entries))
	for _, entry := range entries {
		pair, ok := entry.([]any)
		if !ok || len(pair) != 2 {
			return fmt.Errorf("%s entry is not a key and value", v.Type())
		}
		key, err := receiptString(pair[0])
		if err != nil {
			return err
		}
		value := reflect.New(v.Type().Elem()).Elem()
		if err := setReceiptNode(pair[1], value); err != nil {
			return err
		}
		m.SetMapIndex(reflect.ValueOf(key).Convert(v.Type().Key()), value)
	}
	v.Set(m)
	return nil
}

// receiptStringNode encodes a string as itself when it is valid UTF-8, which
// JSON keeps exactly, and otherwise as {"x": base64 of its bytes}, which JSON
// would otherwise replace with U+FFFD.
func receiptStringNode(s string) any {
	if utf8.ValidString(s) {
		return s
	}
	return map[string]any{"x": base64.StdEncoding.EncodeToString([]byte(s))}
}

func receiptString(node any) (string, error) {
	switch value := node.(type) {
	case string:
		return value, nil
	case map[string]any:
		decoded, err := receiptBytes(value["x"])
		return string(decoded), err
	default:
		return "", fmt.Errorf("not a string")
	}
}

func setReceiptScalar(node any, v reflect.Value) error {
	switch v.Kind() {
	case reflect.Bool:
		b, ok := node.(bool)
		if !ok {
			return fmt.Errorf("%s is not a boolean", v.Type())
		}
		v.SetBool(b)
	case reflect.String:
		s, err := receiptString(node)
		if err != nil {
			return fmt.Errorf("%s: %w", v.Type(), err)
		}
		v.SetString(s)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		number, ok := node.(json.Number)
		if !ok {
			return fmt.Errorf("%s is not a number", v.Type())
		}
		i, err := strconv.ParseInt(number.String(), 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(i)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		number, ok := node.(json.Number)
		if !ok {
			return fmt.Errorf("%s is not a number", v.Type())
		}
		u, err := strconv.ParseUint(number.String(), 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetUint(u)
	case reflect.Float32, reflect.Float64:
		s, ok := node.(string)
		if !ok {
			return fmt.Errorf("%s is not a float string", v.Type())
		}
		f, err := strconv.ParseFloat(s, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetFloat(f)
	default:
		return fmt.Errorf("%s values are not supported", v.Type())
	}
	return nil
}

func receiptBytes(node any) ([]byte, error) {
	encoded, ok := node.(string)
	if !ok {
		return nil, fmt.Errorf("encoded bytes are not a string")
	}
	return base64.StdEncoding.DecodeString(encoded)
}
