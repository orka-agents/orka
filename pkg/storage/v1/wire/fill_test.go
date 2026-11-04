package wire

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
)

// recordCodec is one generated record conversion, listed by
// zz_generated.codecs_test.go.
type recordCodec struct {
	name     string
	goType   reflect.Type
	newWire  func() proto.Message
	toWire   func(any) (proto.Message, error)
	fromWire func(proto.Message) (any, error)
}

var (
	timeType       = reflect.TypeFor[time.Time]()
	durationType   = reflect.TypeFor[time.Duration]()
	rawMessageType = reflect.TypeFor[json.RawMessage]()
	anyMapType     = reflect.TypeFor[map[string]any]()
)

// filler sets every field of a value, recursively, to a distinct non-zero
// value. With a random source it fills randomly for fuzzing.
type filler struct {
	random *rand.Rand
	next   int64
}

// FillValue returns a value of t with every field set. It is exported to the
// external tests.
func FillValue(t reflect.Type, path string) reflect.Value {
	return (&filler{}).fill(t, path)
}

// NormalizeValue returns value in the form the wire preserves: times in UTC,
// nil and empty lists and maps equal, and dynamic values as their JSON
// decoding.
func NormalizeValue(value any) any {
	if value == nil {
		return nil
	}
	return normalize(reflect.ValueOf(value)).Interface()
}

func (f *filler) fill(t reflect.Type, path string) reflect.Value {
	value := reflect.New(t).Elem()
	f.set(value, path)
	return value
}

func (f *filler) number() int64 {
	if f.random != nil {
		return f.random.Int64N(1<<53) - 1<<52
	}
	f.next++
	return f.next
}

func (f *filler) text(path string) string {
	if f.random == nil {
		f.next++
		return fmt.Sprintf("%s-%d-é", path, f.next)
	}
	var builder strings.Builder
	for range f.random.IntN(24) {
		r := rune(f.random.IntN(0x2FFFF))
		if !utf8.ValidRune(r) {
			r = 'x'
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

func (f *filler) time() time.Time {
	zone := time.FixedZone("wire+05:30", 5*3600+30*60)
	if f.random == nil {
		f.next++
		return time.Date(2026, 10, 21, 13, 14, 15, int(123456789+f.next), zone)
	}
	// Mostly in the Timestamp range, sometimes outside it.
	seconds := f.random.Int64N(2*400000*365*24*3600) - 400000*365*24*3600
	return time.Unix(seconds, f.random.Int64N(1e9)).In(zone)
}

func (f *filler) set(value reflect.Value, path string) {
	switch value.Type() {
	case timeType:
		value.Set(reflect.ValueOf(f.time()))
		return
	case durationType:
		value.SetInt(f.number()*int64(time.Millisecond) + 7)
		return
	case rawMessageType:
		value.SetBytes([]byte(fmt.Sprintf(`{"path":%q,"n":%d}`, path, f.number())))
		return
	case anyMapType:
		value.Set(reflect.ValueOf(map[string]any{"path": path, "n": float64(f.number()), "nested": []any{true, "x"}}))
		return
	}
	switch value.Kind() {
	case reflect.String:
		value.SetString(f.text(path))
	case reflect.Int, reflect.Int32, reflect.Int64:
		n := f.number()
		if value.Kind() == reflect.Int32 {
			n = int64(int32(n))
		}
		if n == 0 {
			n = 1
		}
		value.SetInt(n)
	case reflect.Uint64:
		value.SetUint(uint64(f.number()) | 1)
	case reflect.Bool:
		value.SetBool(f.random == nil || f.random.IntN(2) == 0)
	case reflect.Float64:
		value.SetFloat(float64(f.number()) + 0.5)
	case reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			value.SetBytes([]byte(f.text(path) + "\x00\xff"))
			return
		}
		slice := reflect.MakeSlice(value.Type(), 2, 2)
		for index := range 2 {
			f.set(slice.Index(index), fmt.Sprintf("%s[%d]", path, index))
		}
		value.Set(slice)
	case reflect.Map:
		entries := reflect.MakeMap(value.Type())
		for index := range 2 {
			key := reflect.New(value.Type().Key()).Elem()
			f.set(key, fmt.Sprintf("%s.key%d", path, index))
			element := reflect.New(value.Type().Elem()).Elem()
			f.set(element, fmt.Sprintf("%s.value%d", path, index))
			entries.SetMapIndex(key, element)
		}
		value.Set(entries)
	case reflect.Pointer:
		element := reflect.New(value.Type().Elem())
		if value.Type().Elem() != timeType || f.number()%3 != 0 {
			f.set(element.Elem(), path)
		}
		value.Set(element)
	case reflect.Struct:
		for index := range value.NumField() {
			field := value.Type().Field(index)
			if field.IsExported() {
				f.set(value.Field(index), path+"."+field.Name)
			}
		}
	case reflect.Interface:
		value.Set(reflect.ValueOf(map[string]any{"path": path, "n": float64(f.number())}))
	default:
		panic("filler: unsupported kind " + value.Kind().String() + " at " + path)
	}
}

func normalize(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}
	switch value.Type() {
	case timeType:
		return reflect.ValueOf(value.Interface().(time.Time).UTC().Round(0))
	case rawMessageType:
		if value.Len() == 0 {
			return reflect.Zero(value.Type())
		}
		return value
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return value
		}
		return reflect.ValueOf(jsonRoundTrip(value.Interface()))
	case reflect.Slice:
		if value.Len() == 0 {
			return reflect.Zero(value.Type())
		}
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return value
		}
		out := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := range value.Len() {
			out.Index(index).Set(normalize(value.Index(index)))
		}
		return out
	case reflect.Map:
		if value.Len() == 0 {
			return reflect.Zero(value.Type())
		}
		if value.Type() == anyMapType {
			return reflect.ValueOf(jsonRoundTrip(value.Interface()))
		}
		out := reflect.MakeMap(value.Type())
		for _, key := range value.MapKeys() {
			out.SetMapIndex(key, normalize(value.MapIndex(key)))
		}
		return out
	case reflect.Pointer:
		if value.IsNil() {
			return value
		}
		out := reflect.New(value.Type().Elem())
		out.Elem().Set(normalize(value.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(value.Type()).Elem()
		for index := range value.NumField() {
			if value.Type().Field(index).IsExported() {
				out.Field(index).Set(normalize(value.Field(index)))
			}
		}
		return out
	default:
		return value
	}
}

func jsonRoundTrip(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		panic(err)
	}
	return decoded
}

// zeroFields lists the paths of fields left at their zero value, so a test
// can prove its fill reached every field.
func zeroFields(value reflect.Value, path string) []string {
	if value.IsZero() {
		return []string{path}
	}
	var zero []string
	switch value.Kind() {
	case reflect.Pointer:
		if value.Type().Elem() != timeType {
			zero = append(zero, zeroFields(value.Elem(), path)...)
		}
	case reflect.Struct:
		if value.Type() == timeType {
			return nil
		}
		for index := range value.NumField() {
			if value.Type().Field(index).IsExported() {
				zero = append(zero, zeroFields(value.Field(index), path+"."+value.Type().Field(index).Name)...)
			}
		}
	case reflect.Slice:
		if value.Type().Elem().Kind() != reflect.Uint8 {
			for index := range value.Len() {
				zero = append(zero, zeroFields(value.Index(index), fmt.Sprintf("%s[%d]", path, index))...)
			}
		}
	}
	return zero
}
