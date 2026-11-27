package v1

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"
	"time"
)

const requestDigestDomain = "orka.storage.v1 request digest 1\x00"

const maxRequestDigestDepth = 64

const canonicalNaNBits = 0x7ff8000000000001

const (
	digestTagZero   = 'z'
	digestTagBool   = 'b'
	digestTagInt    = 'i'
	digestTagUint   = 'u'
	digestTagFloat  = 'f'
	digestTagTime   = 't'
	digestTagString = 's'
	digestTagBytes  = 'y'
	digestTagJSON   = 'j'
	digestTagPtr    = 'p'
	digestTagList   = 'l'
	digestTagMap    = 'm'
	digestTagStruct = 'r'
)

var (
	rawMessageType  = reflect.TypeFor[json.RawMessage]()
	errDigestNested = errors.New("value is nested too deeply")
)

func RequestDigest(method string, request ...any) (string, error) {
	if err := ValidateControlIdentifier("request digest method", method); err != nil {
		return "", err
	}
	nodes := make([]digestNode, 0, len(request))
	for i, value := range request {
		node, err := newDigestNode(reflect.ValueOf(value), 0)
		if err != nil {
			return "", ValidationErrorf("request digest argument %d: %v", i+1, err)
		}
		nodes = append(nodes, node)
	}
	hash := sha256.New()
	w := digestWriter{w: hash}
	w.raw([]byte(requestDigestDomain))
	w.text(method)
	w.uvarint(uint64(len(nodes)))
	for _, node := range nodes {
		w.node(node)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

type digestNode struct {
	tag   byte
	text  string
	raw   []byte
	items []digestNode
}

func (n digestNode) isZero() bool {
	return n.tag == digestTagZero
}

var zeroDigestNode = digestNode{tag: digestTagZero}

func newDigestNode(v reflect.Value, depth int) (digestNode, error) {
	if depth > maxRequestDigestDepth {
		return zeroDigestNode, errDigestNested
	}
	if !v.IsValid() {
		return zeroDigestNode, nil
	}
	switch v.Type() {
	case timeType:
		return timeDigestNode(v)
	case rawMessageType:
		return jsonDigestNode(v.Bytes())
	}
	switch v.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.String:
		return scalarDigestNode(v), nil
	case reflect.Interface:
		if v.IsNil() {
			return zeroDigestNode, nil
		}
		return dynamicDigestNode(v)
	case reflect.Pointer:
		if v.IsNil() {
			return zeroDigestNode, nil
		}
		elem, err := newDigestNode(v.Elem(), depth+1)
		if err != nil {
			return zeroDigestNode, err
		}
		return digestNode{tag: digestTagPtr, items: []digestNode{elem}}, nil
	case reflect.Slice, reflect.Array:
		return sequenceDigestNode(v, depth)
	case reflect.Map:
		if v.Len() == 0 {
			return zeroDigestNode, nil
		}
		if v.Type().Key().Kind() == reflect.Interface || v.Type().Elem().Kind() == reflect.Interface {
			return dynamicDigestNode(v)
		}
		return mapDigestNode(v, depth)
	case reflect.Struct:
		return structDigestNode(v, depth)
	default:
		if v.IsZero() {
			return zeroDigestNode, nil
		}
		return zeroDigestNode, fmt.Errorf("%s values cannot be digested", v.Kind())
	}
}

func scalarDigestNode(v reflect.Value) digestNode {
	if v.IsZero() {
		return zeroDigestNode
	}
	switch v.Kind() {
	case reflect.Bool:
		return digestNode{tag: digestTagBool}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return digestNode{tag: digestTagInt, raw: binary.AppendVarint(nil, v.Int())}
	case reflect.Float32, reflect.Float64:
		value := v.Float()
		if value == 0 {
			return zeroDigestNode
		}
		bits := math.Float64bits(value)
		if math.IsNaN(value) {
			bits = canonicalNaNBits
		}
		return digestNode{tag: digestTagFloat, raw: binary.BigEndian.AppendUint64(nil, bits)}
	case reflect.String:
		return digestNode{tag: digestTagString, text: v.String()}
	default:
		return digestNode{tag: digestTagUint, raw: binary.AppendUvarint(nil, v.Uint())}
	}
}

func timeDigestNode(v reflect.Value) (digestNode, error) {
	if !v.CanInterface() {
		return zeroDigestNode, errors.New("time value cannot be read")
	}
	at, _ := reflect.TypeAssert[time.Time](v)
	if at.IsZero() {
		return zeroDigestNode, nil
	}
	raw := binary.AppendVarint(nil, at.Unix())
	raw = binary.AppendUvarint(raw, uint64(at.Nanosecond()))
	return digestNode{tag: digestTagTime, raw: raw}, nil
}

func dynamicDigestNode(v reflect.Value) (digestNode, error) {
	if !v.CanInterface() {
		return zeroDigestNode, errors.New("dynamic value cannot be read")
	}
	encoded, err := json.Marshal(v.Interface())
	if err != nil {
		return zeroDigestNode, fmt.Errorf("dynamic value cannot be encoded as JSON: %w", err)
	}
	return jsonDigestNode(encoded)
}

func jsonDigestNode(encoded []byte) (digestNode, error) {
	if len(bytes.Trim(encoded, " \t\r\n")) == 0 {
		return zeroDigestNode, nil
	}
	canonical, err := canonicalJSON(encoded)
	if err != nil {
		return zeroDigestNode, err
	}
	if string(canonical) == "null" {
		return zeroDigestNode, nil
	}
	return digestNode{tag: digestTagJSON, raw: canonical}, nil
}

func canonicalJSON(encoded []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid JSON: data after the value")
	}
	return json.Marshal(value)
}

func sequenceDigestNode(v reflect.Value, depth int) (digestNode, error) {
	if v.Len() == 0 {
		return zeroDigestNode, nil
	}
	if v.Type().Elem().Kind() == reflect.Uint8 {
		if v.Kind() == reflect.Slice {
			return digestNode{tag: digestTagBytes, raw: v.Bytes()}, nil
		}
		raw := make([]byte, v.Len())
		for i := range raw {
			raw[i] = byte(v.Index(i).Uint())
		}
		if !slices.ContainsFunc(raw, func(b byte) bool { return b != 0 }) {
			return zeroDigestNode, nil
		}
		return digestNode{tag: digestTagBytes, raw: raw}, nil
	}
	items := make([]digestNode, v.Len())
	allZero := true
	for i := range items {
		item, err := newDigestNode(v.Index(i), depth+1)
		if err != nil {
			return zeroDigestNode, fmt.Errorf("element %d: %w", i, err)
		}
		items[i] = item
		allZero = allZero && item.isZero()
	}
	if v.Kind() == reflect.Array && allZero {
		return zeroDigestNode, nil
	}
	return digestNode{tag: digestTagList, items: items}, nil
}

func mapDigestNode(v reflect.Value, depth int) (digestNode, error) {
	type entry struct {
		key, value digestNode
		keyBytes   []byte
	}
	entries := make([]entry, 0, v.Len())
	iter := v.MapRange()
	for iter.Next() {
		key, err := newDigestNode(iter.Key(), depth+1)
		if err != nil {
			return zeroDigestNode, fmt.Errorf("map key: %w", err)
		}
		value, err := newDigestNode(iter.Value(), depth+1)
		if err != nil {
			return zeroDigestNode, fmt.Errorf("map value: %w", err)
		}
		entries = append(entries, entry{key: key, value: value, keyBytes: encodeDigestNode(key)})
	}
	slices.SortFunc(entries, func(a, b entry) int {
		if c := bytes.Compare(a.keyBytes, b.keyBytes); c != 0 {
			return c
		}
		return bytes.Compare(encodeDigestNode(a.value), encodeDigestNode(b.value))
	})
	items := make([]digestNode, 0, 2*len(entries))
	for _, e := range entries {
		items = append(items, e.key, e.value)
	}
	return digestNode{tag: digestTagMap, items: items}, nil
}

func structDigestNode(v reflect.Value, depth int) (digestNode, error) {
	t := v.Type()
	var items []digestNode
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			return zeroDigestNode, fmt.Errorf("%s has unexported field %s", t, field.Name)
		}
		value, err := newDigestNode(v.Field(i), depth+1)
		if err != nil {
			return zeroDigestNode, fmt.Errorf("%s: %w", field.Name, err)
		}
		if value.isZero() {
			continue
		}
		items = append(items, digestNode{tag: digestTagString, text: field.Name}, value)
	}
	if len(items) == 0 {
		return zeroDigestNode, nil
	}
	return digestNode{tag: digestTagStruct, items: items}, nil
}

func encodeDigestNode(node digestNode) []byte {
	var buf bytes.Buffer
	w := digestWriter{w: &buf}
	w.node(node)
	return buf.Bytes()
}

type digestWriter struct {
	w       io.Writer
	scratch [binary.MaxVarintLen64]byte
}

func (w *digestWriter) raw(b []byte) {
	_, _ = w.w.Write(b)
}

func (w *digestWriter) uvarint(value uint64) {
	w.raw(binary.AppendUvarint(w.scratch[:0], value))
}

func (w *digestWriter) text(value string) {
	w.uvarint(uint64(len(value)))
	_, _ = io.WriteString(w.w, value)
}

func (w *digestWriter) node(node digestNode) {
	w.raw([]byte{node.tag})
	switch node.tag {
	case digestTagZero, digestTagBool:
	case digestTagString:
		w.text(node.text)
	case digestTagPtr:
		w.node(node.items[0])
	case digestTagList:
		w.uvarint(uint64(len(node.items)))
		for _, item := range node.items {
			w.node(item)
		}
	case digestTagMap, digestTagStruct:
		w.uvarint(uint64(len(node.items) / 2))
		for _, item := range node.items {
			w.node(item)
		}
	default:
		w.uvarint(uint64(len(node.raw)))
		w.raw(node.raw)
	}
}
