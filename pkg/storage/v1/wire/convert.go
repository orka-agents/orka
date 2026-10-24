package wire

import (
	"encoding/json"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// These helpers convert the Go types that have no direct protobuf scalar.
// The generated mapping calls them; field names a value for its error.

// timeToWire encodes t. The zero time is an absent field, and a time outside
// the Timestamp range (0001-01-01 to 9999-12-31) is a validation error.
func timeToWire(t time.Time, field string) (*timestamppb.Timestamp, error) {
	if t.IsZero() {
		return nil, nil
	}
	encoded := timestamppb.New(t)
	if err := encoded.CheckValid(); err != nil {
		return nil, storagev1.ValidationErrorf("%s is outside the supported time range", field)
	}
	return encoded, nil
}

// timeFromWire decodes a time in UTC. An absent field is the zero time.
func timeFromWire(encoded *timestamppb.Timestamp, field string) (time.Time, error) {
	if encoded == nil {
		return time.Time{}, nil
	}
	if err := encoded.CheckValid(); err != nil {
		return time.Time{}, storagev1.ValidationErrorf("%s is outside the supported time range", field)
	}
	return encoded.AsTime(), nil
}

// nullableTimeToWire encodes a *time.Time: nil is an absent message, and a
// pointer to the zero time is a message with no value.
func nullableTimeToWire(t *time.Time, field string) (*storagepb.NullableTime, error) {
	if t == nil {
		return nil, nil
	}
	value, err := timeToWire(*t, field)
	if err != nil {
		return nil, err
	}
	return &storagepb.NullableTime{Value: value}, nil
}

func nullableTimeFromWire(encoded *storagepb.NullableTime, field string) (*time.Time, error) {
	if encoded == nil {
		return nil, nil
	}
	value, err := timeFromWire(encoded.GetValue(), field)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func durationToWire(d time.Duration) *durationpb.Duration {
	if d == 0 {
		return nil
	}
	return durationpb.New(d)
}

func durationFromWire(encoded *durationpb.Duration, field string) (time.Duration, error) {
	if encoded == nil {
		return 0, nil
	}
	if err := encoded.CheckValid(); err != nil {
		return 0, storagev1.ValidationErrorf("%s is not a valid duration", field)
	}
	// AsDuration saturates at the time.Duration range; refuse instead.
	const (
		maxSeconds = math.MaxInt64 / int64(time.Second)
		minSeconds = math.MinInt64 / int64(time.Second)
		maxNanos   = int32(math.MaxInt64 % int64(time.Second))
		minNanos   = int32(math.MinInt64 % int64(time.Second))
	)
	seconds, nanos := encoded.GetSeconds(), encoded.GetNanos()
	if seconds > maxSeconds || seconds < minSeconds ||
		(seconds == maxSeconds && nanos > maxNanos) || (seconds == minSeconds && nanos < minNanos) {
		return 0, storagev1.ValidationErrorf("%s is outside the supported duration range", field)
	}
	return encoded.AsDuration(), nil
}

// jsonToWire encodes a dynamic value as its encoding/json bytes. A nil value
// is an absent field.
func jsonToWire(value any, field string) (*storagepb.JSONValue, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, storagev1.ValidationErrorf("%s cannot be encoded as JSON", field)
	}
	return &storagepb.JSONValue{Json: encoded}, nil
}

// jsonMapToWire encodes a map[string]any. Nil and empty maps are equivalent,
// so both are an absent field.
func jsonMapToWire(value map[string]any, field string) (*storagepb.JSONValue, error) {
	if len(value) == 0 {
		return nil, nil
	}
	return jsonToWire(value, field)
}

// rawJSONToWire carries json.RawMessage bytes unchanged. Empty bytes are an
// absent field.
func rawJSONToWire(value json.RawMessage, field string) (*storagepb.JSONValue, error) {
	if len(value) == 0 {
		return nil, nil
	}
	if !json.Valid(value) {
		return nil, storagev1.ValidationErrorf("%s is not valid JSON", field)
	}
	return &storagepb.JSONValue{Json: append([]byte(nil), value...)}, nil
}

// jsonFromWire decodes a dynamic value into T (any or map[string]any). The
// decoded types are those encoding/json produces, as a provider that stores
// JSON text returns them in process.
func jsonFromWire[T any](encoded *storagepb.JSONValue, field string) (T, error) {
	var value T
	if encoded == nil || len(encoded.GetJson()) == 0 {
		return value, nil
	}
	if err := json.Unmarshal(encoded.GetJson(), &value); err != nil {
		return value, storagev1.ValidationErrorf("%s is not valid JSON", field)
	}
	return value, nil
}

func rawJSONFromWire(encoded *storagepb.JSONValue, field string) (json.RawMessage, error) {
	if encoded == nil || len(encoded.GetJson()) == 0 {
		return nil, nil
	}
	if !json.Valid(encoded.GetJson()) {
		return nil, storagev1.ValidationErrorf("%s is not valid JSON", field)
	}
	return json.RawMessage(append([]byte(nil), encoded.GetJson()...)), nil
}

// sanitizeText bounds text that crosses a trust boundary into status, logs or
// errors: invalid UTF-8 and control characters are dropped, and the result is
// at most limit bytes.
func sanitizeText(text string, limit int) string {
	var builder strings.Builder
	for _, r := range text {
		if r == utf8.RuneError || unicode.IsControl(r) {
			continue
		}
		if builder.Len()+utf8.RuneLen(r) > limit {
			break
		}
		builder.WriteRune(r)
	}
	return builder.String()
}
