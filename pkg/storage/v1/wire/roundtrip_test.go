package wire

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
)

// roundTrip encodes value to wire bytes and decodes it back.
func roundTrip(t *testing.T, codec recordCodec, value any) (any, error) {
	t.Helper()
	encoded, err := codec.toWire(value)
	if err != nil {
		return nil, err
	}
	raw, err := proto.Marshal(encoded)
	require.NoError(t, err, "%s marshals", codec.name)
	decoded := codec.newWire()
	require.NoError(t, proto.Unmarshal(raw, decoded), "%s unmarshals", codec.name)
	return codec.fromWire(decoded)
}

// Every record crosses the wire with every field, including fields tagged
// json:"-", nested records, non-nil pointers and times with nanoseconds in a
// non-UTC zone.
func TestEveryRecordRoundTrips(t *testing.T) {
	require.NotEmpty(t, recordCodecs)
	for _, codec := range recordCodecs {
		t.Run(codec.name, func(t *testing.T) {
			value := FillValue(codec.goType, codec.name)
			require.Empty(t, zeroFields(value, codec.name), "the fill reaches every field")
			back, err := roundTrip(t, codec, value.Interface())
			require.NoError(t, err)
			require.Equal(t, NormalizeValue(value.Interface()), NormalizeValue(back))

			zero := reflect.Zero(codec.goType).Interface()
			back, err = roundTrip(t, codec, zero)
			require.NoError(t, err)
			require.Equal(t, NormalizeValue(zero), NormalizeValue(back), "the zero value round-trips")
		})
	}
}

// Random fills either round-trip exactly or are refused as validation errors
// before any bytes are written; they never decode to a different value.
func FuzzRecordRoundTrip(f *testing.F) {
	for seed := range uint64(8) {
		f.Add(seed, uint8(seed))
	}
	f.Fuzz(func(t *testing.T, seed uint64, index uint8) {
		codec := recordCodecs[int(index)%len(recordCodecs)]
		fill := &filler{random: rand.New(rand.NewPCG(seed, uint64(index)))}
		value := fill.fill(codec.goType, codec.name)
		back, err := roundTrip(t, codec, value.Interface())
		if err != nil {
			require.ErrorIs(t, err, storagev1.ErrValidation)
			return
		}
		require.Equal(t, NormalizeValue(value.Interface()), NormalizeValue(back))
	})
}

func TestTimesCrossTheWireExactlyInUTC(t *testing.T) {
	zone := time.FixedZone("east", 9*3600)
	for _, value := range []time.Time{
		time.Date(2026, 10, 21, 1, 2, 3, 999999999, zone),
		time.Date(1, 1, 1, 0, 0, 0, 1, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
		time.Date(1969, 12, 31, 23, 59, 59, 1, time.UTC),
	} {
		encoded, err := timeToWire(value, "T")
		require.NoError(t, err)
		decoded, err := timeFromWire(encoded, "T")
		require.NoError(t, err)
		require.True(t, value.Equal(decoded), "%s", value)
		require.Equal(t, time.UTC, decoded.Location())
	}

	encoded, err := timeToWire(time.Time{}, "T")
	require.NoError(t, err)
	require.Nil(t, encoded, "the zero time is an absent field")
	decoded, err := timeFromWire(nil, "T")
	require.NoError(t, err)
	require.True(t, decoded.IsZero())

	for _, outside := range []time.Time{
		time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(0, 12, 31, 23, 59, 59, 0, time.UTC),
	} {
		_, err := timeToWire(outside, "Record.At")
		require.ErrorIs(t, err, storagev1.ErrValidation)
		require.ErrorContains(t, err, "Record.At")
	}
	_, err = timeFromWire(&timestamppb.Timestamp{Seconds: 253402300800}, "T")
	require.ErrorIs(t, err, storagev1.ErrValidation, "a server cannot send a time outside the range")
}

func TestNullableTimesKeepNilApartFromTheZeroTime(t *testing.T) {
	encoded, err := nullableTimeToWire(nil, "T")
	require.NoError(t, err)
	require.Nil(t, encoded)
	decoded, err := nullableTimeFromWire(nil, "T")
	require.NoError(t, err)
	require.Nil(t, decoded)

	zero := time.Time{}
	encoded, err = nullableTimeToWire(&zero, "T")
	require.NoError(t, err)
	require.NotNil(t, encoded)
	raw, err := proto.Marshal(encoded)
	require.NoError(t, err)
	var back storagepb.NullableTime
	require.NoError(t, proto.Unmarshal(raw, &back))
	decoded, err = nullableTimeFromWire(&back, "T")
	require.NoError(t, err)
	require.NotNil(t, decoded, "a pointer to the zero time stays a pointer")
	require.True(t, decoded.IsZero())
}

func TestDurationsRefuseValuesOutsideTheGoRange(t *testing.T) {
	for _, value := range []time.Duration{time.Nanosecond, -time.Hour, 1<<63 - 1, -1 << 63} {
		decoded, err := durationFromWire(durationToWire(value), "D")
		require.NoError(t, err)
		require.Equal(t, value, decoded)
	}
	require.Nil(t, durationToWire(0))
	_, err := durationFromWire(&durationpb.Duration{Seconds: 9223372036, Nanos: 854775808}, "D")
	require.ErrorIs(t, err, storagev1.ErrValidation)
	_, err = durationFromWire(&durationpb.Duration{Seconds: 315576000000}, "D")
	require.ErrorIs(t, err, storagev1.ErrValidation)
}

// Dynamic values carry their encoding/json bytes, so the decoded types are
// the ones a JSON text column returns in process.
func TestJSONValuesCarryTheirEncodingJSONBytes(t *testing.T) {
	type call struct {
		Name string `json:"name"`
		Args []int  `json:"args"`
	}
	encoded, err := jsonToWire([]call{{Name: "tool", Args: []int{1, 2}}}, "J")
	require.NoError(t, err)
	decoded, err := jsonFromWire[any](encoded, "J")
	require.NoError(t, err)
	require.Equal(t, []any{map[string]any{"name": "tool", "args": []any{1.0, 2.0}}}, decoded)

	encoded, err = jsonMapToWire(map[string]any{}, "J")
	require.NoError(t, err)
	require.Nil(t, encoded, "an empty map is an absent field")
	asMap, err := jsonFromWire[map[string]any](&storagepb.JSONValue{Json: []byte(`{"a":{"b":null}}`)}, "J")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"a": map[string]any{"b": nil}}, asMap)

	raw := json.RawMessage(`{"keep":  "spacing"}`)
	encoded, err = rawJSONToWire(raw, "J")
	require.NoError(t, err)
	back, err := rawJSONFromWire(encoded, "J")
	require.NoError(t, err)
	require.Equal(t, raw, back, "raw JSON bytes are carried unchanged")

	_, err = rawJSONToWire(json.RawMessage(`{`), "J")
	require.ErrorIs(t, err, storagev1.ErrValidation)
	_, err = jsonToWire(func() {}, "J")
	require.ErrorIs(t, err, storagev1.ErrValidation)
	_, err = jsonFromWire[any](&storagepb.JSONValue{Json: []byte(`nope`)}, "J")
	require.ErrorIs(t, err, storagev1.ErrValidation)
	_, err = rawJSONFromWire(&storagepb.JSONValue{Json: []byte(`nope`)}, "J")
	require.ErrorIs(t, err, storagev1.ErrValidation)
	value, err := jsonToWire(nil, "J")
	require.NoError(t, err)
	require.Nil(t, value)
}

func TestSanitizeTextDropsControlsAndBoundsLength(t *testing.T) {
	require.Equal(t, "line one two", sanitizeText("line\n one\x00 two\xff", 64))
	require.Equal(t, "ééé", sanitizeText("éééé", 7), "truncation keeps whole runes")
}

func TestServerInfoIsSanitized(t *testing.T) {
	long := string(make([]byte, 300))
	info := serverInfoFromWire(serverInfoToWire(ServerInfo{
		ProtocolRevision: 3, LibraryVersion: "v1.2.3", AdapterName: "postgres\n", AdapterVersion: "1.0" + long,
	}))
	require.Equal(t, ServerInfo{ProtocolRevision: 3, LibraryVersion: "v1.2.3", AdapterName: "postgres", AdapterVersion: "1.0"}, info)
	require.NotEmpty(t, libraryVersion())
}

func TestUnknownFieldsAreFoundAnywhereInTheRequest(t *testing.T) {
	unknown := []byte{0xf8, 0x07, 0x01} // field 127, varint 1
	request := &storagepb.SavePlanRequest{Namespace: "n", Plan: &storagepb.PlanState{Summary: "s"}}
	require.False(t, hasUnknownFields(request.ProtoReflect()))
	request.Plan.ProtoReflect().SetUnknown(unknown)
	require.True(t, hasUnknownFields(request.ProtoReflect()), "an unknown field in a nested record")

	list := &storagepb.GetMessagesResponse{Messages: []*storagepb.Message{{}, {}}}
	require.False(t, hasUnknownFields(list.ProtoReflect()))
	list.Messages[1].ProtoReflect().SetUnknown(unknown)
	require.True(t, hasUnknownFields(list.ProtoReflect()), "an unknown field in a list element")

	top := &storagepb.DeletePlanRequest{}
	top.ProtoReflect().SetUnknown(unknown)
	require.True(t, hasUnknownFields(top.ProtoReflect()))
}

func TestInvalidUTF8IsFoundAnywhereInAMessage(t *testing.T) {
	message := &storagepb.GetMessagesResponse{Messages: []*storagepb.Message{{Content: "ok"}, {Content: "bad\xff"}}}
	path, invalid := firstInvalidString(message.ProtoReflect())
	require.True(t, invalid)
	require.Equal(t, "orka.storage.v1.Message.content", path)
	info := &storagepb.ProviderInfo{Contracts: []string{"state.v1", "\xfe"}}
	_, invalid = firstInvalidString(info.ProtoReflect())
	require.True(t, invalid, "a list element")
	_, invalid = firstInvalidString((&storagepb.ProviderInfo{Contracts: []string{"state.v1"}}).ProtoReflect())
	require.False(t, invalid)
}

// The client refuses an RPC the server's revision does not have, without
// sending it.
func TestClientRefusesRPCsNewerThanTheServer(t *testing.T) {
	client := &Client{serverRevision: 1}
	newer := &method{name: "FutureWrite", fullName: "/orka.storage.v1.Future/FutureWrite", kind: KindWrite, since: 2}
	called := false
	err := client.invoke(t.Context(), newer, &storagepb.DeletePlanRequest{}, func(ctx context.Context, _ ...grpc.CallOption) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, storagev1.ErrValidation)
	require.ErrorIs(t, err, ErrIncompatible)
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown))
	require.False(t, called, "nothing is sent")
}
