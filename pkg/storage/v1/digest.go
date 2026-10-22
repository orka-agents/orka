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
	"math/big"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

// requestDigestDomain separates request digests from every other digest Orka
// computes. Changing the encoding changes this value.
const requestDigestDomain = "orka.storage.v1 request digest 1\x00"

// maxRequestDigestDepth bounds how deeply nested a request may be, which also
// stops a pointer cycle.
const maxRequestDigestDepth = 64

// canonicalNaNBits is the one encoding every NaN shares.
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
	timeType        = reflect.TypeFor[time.Time]()
	rawMessageType  = reflect.TypeFor[json.RawMessage]()
	errDigestNested = errors.New("value is nested too deeply")
)

// RequestDigest returns the canonical "sha256:<hex>" digest of one call of a
// provider method: the method name, then each argument after the context, in
// order. A [WriteEnvelope] carries it so that an operation ID reused with a
// different request is refused (see [WriteEnvelope.RequestDigest] for who
// computes and checks it). The envelope itself is never an argument.
//
// The encoding walks Go values rather than their JSON form, so fields tagged
// json:"-" are covered (except inside dynamic values, described below, which
// follow encoding/json), and it is stable across additive contract changes
// and across a process boundary:
//
//   - A struct is encoded as its exported fields in declaration order, under
//     their Go names. A field holding a zero value is left out, so a field
//     added later and left unset does not change the digest. Zero values are
//     zero numbers, false, empty strings, nil or empty slices and maps
//     (including maps of interfaces), arrays whose elements are all zero, nil
//     pointers and interfaces, dynamic values that encode as JSON null, the
//     zero time, and structs whose fields are all left out.
//   - In a slice, an array, a map value or an argument position, a zero value
//     still takes its place, so positions keep their meaning.
//   - A non-nil pointer is encoded even when it points to a zero value, so an
//     absent value and a zero value stay distinct.
//   - Maps are encoded in the order of their encoded keys.
//   - A time is encoded as UTC seconds and nanoseconds since the Unix epoch;
//     its location and monotonic reading do not matter. Inside a dynamic
//     value a time is part of the JSON text, so there its zone offset counts.
//   - Every NaN encodes the same, and a float field holding negative zero is
//     zero. (Inside dynamic JSON, -0 stays -0, as encoding/json writes it.)
//   - Dynamic values (interface values, non-empty maps whose keys or values
//     are interfaces, and json.RawMessage) are encoded as canonical JSON: the
//     encoding/json form, decoded and encoded again with object keys sorted.
//     An empty or whitespace-only json.RawMessage is zero.
//     Each number is written as encoding/json writes its float64 value when
//     the number is written in that form (ignoring zeros and exponent
//     notation) or is a plain integer that the float64
//     holds exactly, so 1.0, 1e2 and 0.50 become 1, 100 and 0.5; any other
//     number keeps its text. Decoding replaces invalid UTF-8 and unpaired
//     surrogate escapes with U+FFFD and keeps the last of duplicate object
//     keys, so JSON texts that differ only in those ways digest the same. A
//     JSON null is zero; an empty JSON array or object is a value like any
//     other. A typed value and a copy decoded into map[string]any and float64
//     therefore digest the same, unless it holds an integer that a float64
//     cannot hold exactly and that is not written as the float64's shortest
//     text; then the digests differ rather than matching another request.
//     Raw JSON that writes a number in any other form, such
//     as 1152921504606846976.0, keeps its text and likewise differs from a
//     float64 copy. The price of that match is that an integer a float64
//     holds exactly and the float64's shortest text, such as
//     1152921504606846976 and 1152921504606847000, digest the same even where
//     they are distinct integers. A server that recomputes the digest from a
//     decoded request matches exactly when it decodes dynamic values with
//     json.Decoder.UseNumber or keeps json.RawMessage bytes, and matches for
//     typed values whenever it decodes them into float64, with the same
//     exception.
//
// A typed request that holds an unexported struct field, a channel, a
// function, a complex number, invalid JSON, a dynamic value encoding/json
// cannot encode (such as a NaN), or a typed value nested more than 64 levels
// deep returns [ErrValidation]. Inside a dynamic value, encoding/json's rules
// apply instead: it leaves out unexported and json:"-" fields, and bounds
// nesting itself.
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

// digestNode is the canonical form of one value before it is written.
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

// canonicalJSON decodes one JSON value and encodes it again with object keys
// sorted and every number in canonical form.
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
	return json.Marshal(canonicalJSONNumbers(value))
}

// canonicalJSONNumbers rewrites every number in a decoded JSON value with
// canonicalJSONNumber.
func canonicalJSONNumbers(value any) any {
	switch v := value.(type) {
	case json.Number:
		return canonicalJSONNumber(v)
	case []any:
		for i, item := range v {
			v[i] = canonicalJSONNumbers(item)
		}
	case map[string]any:
		for key, item := range v {
			v[key] = canonicalJSONNumbers(item)
		}
	}
	return value
}

// canonicalJSONNumber returns the text encoding/json writes for the number's
// float64 value when the number is written in that form (ignoring zeros and
// exponent notation), or is a plain integer that the
// float64 holds exactly, so 1.0, 1e2, 0.50 and 1152921504606846976 become 1,
// 100, 0.5 and 1152921504606847000, as they do in a copy decoded into float64.
// Any other number keeps its text.
func canonicalJSONNumber(number json.Number) json.Number {
	parsed, err := strconv.ParseFloat(string(number), 64)
	if err != nil {
		return number
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return number
	}
	if !sameDecimal(string(number), string(encoded)) && !exactInteger(string(number), parsed) {
		return number
	}
	return json.Number(encoded)
}

// exactInteger reports whether number is a plain JSON integer whose value
// parsed holds exactly. ParseFloat has already accepted number, so a plain
// integer here has at most 309 digits.
func exactInteger(number string, parsed float64) bool {
	text := strings.TrimPrefix(number, "-")
	if magnitude, err := strconv.ParseUint(text, 10, 64); err == nil {
		// 2^64 is the first float64 that does not fit a uint64.
		return math.Abs(parsed) < 1<<64 && uint64(math.Abs(parsed)) == magnitude
	}
	written, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return false
	}
	held, accuracy := new(big.Float).SetFloat64(math.Abs(parsed)).Int(nil)
	return accuracy == big.Exact && held.Cmp(written) == 0
}

// maxDecimalExponent bounds the exponents sameDecimal compares, far beyond
// the float64 range, so the arithmetic cannot overflow.
const maxDecimalExponent = 1 << 30

// sameDecimal reports whether two JSON numbers denote the same decimal value.
func sameDecimal(a, b string) bool {
	aNegative, aDigits, aExponent, aOK := decimalParts(a)
	bNegative, bDigits, bExponent, bOK := decimalParts(b)
	return aOK && bOK && aNegative == bNegative && aDigits == bDigits && aExponent == bExponent
}

// decimalParts splits a JSON number into its sign, its significant digits
// without leading or trailing zeros, and the exponent e for which the value is
// 0.digits times 10 to the e. Zero has no digits and exponent 0, whatever
// exponent it is written with.
func decimalParts(number string) (negative bool, digits string, exponent int, ok bool) {
	negative = strings.HasPrefix(number, "-")
	number = strings.TrimPrefix(number, "-")
	mantissa, written := number, ""
	if i := strings.IndexAny(number, "eE"); i >= 0 {
		mantissa, written = number[:i], number[i+1:]
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	all := whole + fraction
	trimmed := strings.TrimLeft(all, "0")
	digits = strings.TrimRight(trimmed, "0")
	if digits == "" {
		return negative, "", 0, true
	}
	power := 0
	if written != "" {
		parsed, err := strconv.Atoi(written)
		if err != nil || parsed > maxDecimalExponent || parsed < -maxDecimalExponent {
			return false, "", 0, false
		}
		power = parsed
	}
	return negative, digits, len(whole) - (len(all) - len(trimmed)) + power, true
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
		// Only keys that encode the same, such as NaN keys, reach this.
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

// digestWriter writes the self-delimiting canonical form of digest nodes.
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
