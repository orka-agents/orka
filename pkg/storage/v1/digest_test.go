package v1_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"maps"
	"math"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func requestDigest(t *testing.T, method string, request ...any) string {
	t.Helper()
	digest, err := storagev1.RequestDigest(method, request...)
	require.NoError(t, err)
	require.NoError(t, storagev1.ValidateCanonicalDigest("request digest", digest))
	return digest
}

func TestRequestDigestEncodingIsTheDocumentedOne(t *testing.T) {
	// One string argument: the domain, the method, the argument count, then a
	// string node (tag, length, bytes). Spelling the bytes out keeps the
	// encoding from changing by accident.
	var want bytes.Buffer
	want.WriteString("orka.storage.v1 request digest 1\x00")
	want.Write(binary.AppendUvarint(nil, uint64(len("SaveResult"))))
	want.WriteString("SaveResult")
	want.Write(binary.AppendUvarint(nil, 2))
	want.WriteByte('s')
	want.Write(binary.AppendUvarint(nil, 2))
	want.WriteString("ns")
	want.WriteByte('z')
	sum := sha256.Sum256(want.Bytes())
	require.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), requestDigest(t, "SaveResult", "ns", ""))

	// A struct: present fields only, each as a name node and a value node.
	type record struct {
		Name    string
		Skipped string
		Count   *int64
		At      time.Time
		N       int64
	}
	zero := int64(0)
	text := func(b *bytes.Buffer, value string) {
		b.WriteByte('s')
		b.Write(binary.AppendUvarint(nil, uint64(len(value))))
		b.WriteString(value)
	}
	scalar := func(b *bytes.Buffer, tag byte, raw []byte) {
		b.WriteByte(tag)
		b.Write(binary.AppendUvarint(nil, uint64(len(raw))))
		b.Write(raw)
	}
	want.Reset()
	want.WriteString("orka.storage.v1 request digest 1\x00")
	want.Write(binary.AppendUvarint(nil, 1))
	want.WriteString("M")
	want.Write(binary.AppendUvarint(nil, 1))
	want.WriteByte('r')
	want.Write(binary.AppendUvarint(nil, 4))
	text(&want, "Name")
	text(&want, "x")
	text(&want, "Count")
	want.WriteString("pz")
	text(&want, "At")
	scalar(&want, 't', binary.AppendUvarint(binary.AppendVarint(nil, 5), 7))
	text(&want, "N")
	scalar(&want, 'i', binary.AppendVarint(nil, -3))
	sum = sha256.Sum256(want.Bytes())
	require.Equal(t, "sha256:"+hex.EncodeToString(sum[:]),
		requestDigest(t, "M", record{Name: "x", Count: &zero, At: time.Unix(5, 7), N: -3}))

	// The remaining node kinds: bool, unsigned, float, bytes, list, map and
	// canonical JSON.
	type kinds struct {
		B    bool
		U    uint32
		F    float64
		Y    []byte
		L    []string
		M    map[string]int
		JSON json.RawMessage
	}
	want.Reset()
	want.WriteString("orka.storage.v1 request digest 1\x00")
	want.Write(binary.AppendUvarint(nil, 1))
	want.WriteString("M")
	want.Write(binary.AppendUvarint(nil, 1))
	want.WriteByte('r')
	want.Write(binary.AppendUvarint(nil, 7))
	text(&want, "B")
	want.WriteByte('b')
	text(&want, "U")
	scalar(&want, 'u', binary.AppendUvarint(nil, 300))
	text(&want, "F")
	scalar(&want, 'f', binary.BigEndian.AppendUint64(nil, math.Float64bits(-1.5)))
	text(&want, "Y")
	scalar(&want, 'y', []byte{0, 1})
	text(&want, "L")
	want.WriteByte('l')
	want.Write(binary.AppendUvarint(nil, 2))
	want.WriteByte('z')
	text(&want, "a")
	text(&want, "M")
	want.WriteByte('m')
	want.Write(binary.AppendUvarint(nil, 2))
	text(&want, "a")
	want.WriteByte('z')
	text(&want, "b")
	scalar(&want, 'i', binary.AppendVarint(nil, 2))
	text(&want, "JSON")
	scalar(&want, 'j', []byte(`{"k":[1,"\u003c"]}`))
	sum = sha256.Sum256(want.Bytes())
	require.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), requestDigest(t, "M", kinds{
		B: true, U: 300, F: -1.5, Y: []byte{0, 1}, L: []string{"", "a"}, M: map[string]int{"b": 2, "a": 0},
		JSON: json.RawMessage(` {"k": [1.0, "<"]} `),
	}))
}

func TestRequestDigestIsStable(t *testing.T) {
	at := time.Date(2026, time.March, 4, 5, 6, 7, 123456789, time.FixedZone("AEST", 10*60*60))
	input, output := int64(0), int64(7)
	digest := requestDigest(t, "RecordUsage", storagev1.UsageObservation{
		ID: "observation-1", Namespace: "tenant", TaskUID: "task-uid", InputTokens: &input, OutputTokens: &output,
		ObservedAt: at, Status: "running",
	})
	// A changed value here means every stored receipt digest changed meaning.
	require.Equal(t, "sha256:41667d5c039db43b4ad3aaa4014f31d414717abe72def3ef734bb8daaca302b1", digest)

	nested := int64(0)
	digest = requestDigest(t, "AppendMessages", "tenant", "session", []storagev1.SessionMessage{{
		ID: "m1", Role: "assistant", Content: "<b>&</b> \u2028", Timestamp: at,
		ToolCalls: []any{map[string]any{"arguments": json.RawMessage(`{"n":1.0,"r":0.25}`), "id": "call-1"}},
		Metadata:  map[string]string{"z": "", "a": "x"},
	}}, map[string]any{"score": 0.75, "nested": &nested, "flag": false}, []float64{math.NaN(), 0, 2.5}, uint8(9))
	require.Equal(t, "sha256:7a27b3f441db3c75e0ece6cac38818f91fa6ab8a072795d3a34a9d952621129c", digest)
}

func TestRequestDigestSeparatesMethodsArgumentsAndFields(t *testing.T) {
	base := requestDigest(t, "SaveResult", "ns", "task", []byte("data"))
	require.Equal(t, base, requestDigest(t, "SaveResult", "ns", "task", []byte("data")), "deterministic")
	require.NotEqual(t, base, requestDigest(t, "SavePlan", "ns", "task", []byte("data")), "the method is covered")
	require.NotEqual(t, base, requestDigest(t, "SaveResult", "task", "ns", []byte("data")), "argument order counts")
	require.NotEqual(t, base, requestDigest(t, "SaveResult", "ns", "task", []byte("datA")))
	require.NotEqual(t, requestDigest(t, "M", "", "a"), requestDigest(t, "M", "a", ""), "a zero argument keeps its position")
	require.NotEqual(t, requestDigest(t, "M", "a"), requestDigest(t, "M", "a", ""), "the argument count is covered")

	type pair struct{ A, B string }
	require.NotEqual(t, requestDigest(t, "M", pair{A: "ab", B: "c"}), requestDigest(t, "M", pair{A: "a", B: "bc"}),
		"values are length-delimited")
	type renamed struct{ A, C string }
	require.NotEqual(t, requestDigest(t, "M", pair{A: "a", B: "b"}), requestDigest(t, "M", renamed{A: "a", C: "b"}),
		"field names are covered")
}

func TestRequestDigestOmitsZeroFields(t *testing.T) {
	type before struct {
		Name string
	}
	type after struct {
		Name     string
		Limit    int
		Tags     []string
		Labels   map[string]string
		Deadline time.Time
		Owner    *string
		Extra    any
		Nested   struct{ Flag bool }
		Payload  json.RawMessage
	}
	require.Equal(t, requestDigest(t, "M", before{Name: "x"}), requestDigest(t, "M", after{Name: "x"}),
		"a field added later and left unset does not change the digest")
	require.Equal(t, requestDigest(t, "M", after{Name: "x", Tags: []string{}, Labels: map[string]string{}}),
		requestDigest(t, "M", after{Name: "x"}), "nil and empty slices and maps are equal")
	require.Equal(t, requestDigest(t, "M", after{Name: "x", Payload: json.RawMessage(" null ")}),
		requestDigest(t, "M", after{Name: "x"}), "a JSON null is zero")
	require.NotEqual(t, requestDigest(t, "M", after{Name: "x", Extra: []int{}}), requestDigest(t, "M", after{Name: "x"}),
		"an empty dynamic list is a value, as it is after decoding")
	require.NotEqual(t, requestDigest(t, "M", after{Name: "x", Payload: json.RawMessage(`[]`)}),
		requestDigest(t, "M", after{Name: "x", Payload: json.RawMessage(`{}`)}), "an empty JSON array is not an empty object")
	require.NotEqual(t, requestDigest(t, "M", []any{[]any{}}), requestDigest(t, "M", []any{map[string]any{}}),
		"empty dynamic lists and objects keep their place in a list")
	require.NotEqual(t, requestDigest(t, "M", []any{[]any{}}), requestDigest(t, "M", []any{nil}))
	var missing *int
	require.Equal(t, requestDigest(t, "M", []any{nil}), requestDigest(t, "M", []any{missing}),
		"a dynamic null is zero, as it is after decoding")

	empty := ""
	require.NotEqual(t, requestDigest(t, "M", after{Name: "x", Owner: &empty}), requestDigest(t, "M", after{Name: "x"}),
		"a pointer to a zero value is not absent")
	require.NotEqual(t, requestDigest(t, "M", []string{"", "a"}), requestDigest(t, "M", []string{"a"}),
		"a zero element keeps its position")
	require.NotEqual(t, requestDigest(t, "M", map[string]string{"a": ""}), requestDigest(t, "M", map[string]string{}),
		"a map entry with a zero value is kept")
}

func TestRequestDigestCoversFieldsHiddenFromJSON(t *testing.T) {
	event := storagev1.GatewayEvent{ID: "event-1", Namespace: "tenant"}
	claimed := event
	claimed.ClaimOwner = "dispatcher-1"
	require.NotEqual(t, requestDigest(t, "RenewGatewayEventClaim", event), requestDigest(t, "RenewGatewayEventClaim", claimed))
}

func TestRequestDigestEncodesTimesByInstant(t *testing.T) {
	type stamped struct{ At time.Time }
	at := time.Date(2026, time.March, 4, 5, 6, 7, 123456789, time.UTC)
	digest := requestDigest(t, "M", stamped{At: at})
	require.Equal(t, digest, requestDigest(t, "M", stamped{At: at.In(time.FixedZone("AEST", 10*60*60))}),
		"the location does not matter")
	require.Equal(t, digest, requestDigest(t, "M", stamped{At: time.Unix(at.Unix(), int64(at.Nanosecond()))}))
	require.NotEqual(t, digest, requestDigest(t, "M", stamped{At: at.Add(time.Nanosecond)}), "nanoseconds count")
	require.Equal(t, requestDigest(t, "M", stamped{}), requestDigest(t, "M", struct{}{}), "the zero time is zero")

	// UnixNano is undefined outside 1678 to 2262; seconds and nanoseconds are not.
	ancient := time.Date(1, time.January, 1, 0, 0, 0, 1, time.UTC)
	require.NotEqual(t, requestDigest(t, "M", stamped{At: ancient}), requestDigest(t, "M", stamped{At: ancient.Add(time.Nanosecond)}))
	future := time.Date(3000, time.January, 1, 0, 0, 0, 0, time.UTC)
	require.NotEqual(t, requestDigest(t, "M", stamped{At: future}), requestDigest(t, "M", stamped{At: future.Add(time.Nanosecond)}))
}

func TestRequestDigestCanonicalizesDynamicValues(t *testing.T) {
	type call struct {
		Name string         `json:"name"`
		Args map[string]int `json:"args,omitempty"`
	}
	type message struct {
		ToolCalls any
		Payload   json.RawMessage
		Metadata  map[string]any
	}
	typed := message{
		ToolCalls: []call{{Name: "search", Args: map[string]int{"b": 2, "a": 1}}},
		Payload:   json.RawMessage(`{"b":1,"a":[1,2]}`),
		Metadata:  map[string]any{"count": 3, "labels": []string{"x"}},
	}
	decoded := message{
		ToolCalls: []any{map[string]any{"args": map[string]any{"a": 1.0, "b": 2.0}, "name": "search"}},
		Payload:   json.RawMessage("{ \"a\": [1, 2],\n  \"b\": 1 }"),
		Metadata:  map[string]any{"labels": []any{"x"}, "count": float64(3)},
	}
	require.Equal(t, requestDigest(t, "M", typed), requestDigest(t, "M", decoded),
		"a typed value and its decoded JSON form digest the same")

	// Numbers written in another form, such as raw tool-call arguments, match
	// the float64 copy a server decodes.
	type toolCall struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	written := message{ToolCalls: []toolCall{{Name: "f", Arguments: json.RawMessage(`{"a":1.0,"b":1e2,"c":0.50,"d":-0.0,"e":2.5E-3}`)}}}
	decodedCopy := message{ToolCalls: []any{map[string]any{"name": "f", "arguments": map[string]any{
		"a": 1.0, "b": 100.0, "c": 0.5, "d": math.Copysign(0, -1), "e": 0.0025,
	}}}}
	require.Equal(t, requestDigest(t, "M", written), requestDigest(t, "M", decodedCopy))
	for _, same := range [][2]string{{`1.0`, `1`}, {`1e2`, `100`}, {`0.50`, `0.5`}, {`1E+21`, `1e21`}, {`0.1`, `1e-1`}, {`1152921504606846976`, `1152921504606847000`}, {`18446744073709551616`, `18446744073709552000`}} {
		require.Equal(t, requestDigest(t, "M", message{Payload: json.RawMessage(same[0])}),
			requestDigest(t, "M", message{Payload: json.RawMessage(same[1])}), "%s and %s are forms of the same float64", same[0], same[1])
	}
	for _, different := range [][2]string{
		{`1152921504606846977`, `1152921504606846976`},
		{`0.1000000000000000055511151231257827`, `0.1`},
		{`1e400`, `1e401`},
		{`1`, `-1`},
	} {
		require.NotEqual(t, requestDigest(t, "M", message{Payload: json.RawMessage(different[0])}),
			requestDigest(t, "M", message{Payload: json.RawMessage(different[1])}), "%s and %s differ", different[0], different[1])
	}
	big := int64(1)<<60 + 1
	require.Equal(t, requestDigest(t, "M", message{ToolCalls: int64(1) << 60}),
		requestDigest(t, "M", message{ToolCalls: float64(int64(1) << 60)}),
		"an integer a float64 holds exactly matches its float64 copy")
	require.NotEqual(t, requestDigest(t, "M", message{ToolCalls: big}),
		requestDigest(t, "M", message{ToolCalls: float64(big)}),
		"a number with more precision than a float64 does not match its rounded copy")

	_, err := storagev1.RequestDigest("M", message{Payload: json.RawMessage(`{"a":`)})
	require.ErrorIs(t, err, storagev1.ErrValidation)
	_, err = storagev1.RequestDigest("M", message{Payload: json.RawMessage(`{} {}`)})
	require.ErrorIs(t, err, storagev1.ErrValidation)
	_, err = storagev1.RequestDigest("M", message{ToolCalls: math.Inf(1)})
	require.ErrorIs(t, err, storagev1.ErrValidation)
}

func TestRequestDigestCanonicalizesNumbersAndMaps(t *testing.T) {
	type number struct{ F float64 }
	require.Equal(t, requestDigest(t, "M", number{F: math.Copysign(0, -1)}), requestDigest(t, "M", number{}),
		"negative zero is zero")
	require.Equal(t, requestDigest(t, "M", number{F: math.NaN()}),
		requestDigest(t, "M", number{F: math.Float64frombits(0x7ff8000000000abc)}), "every NaN is the same")
	require.NotEqual(t, requestDigest(t, "M", number{F: math.NaN()}), requestDigest(t, "M", number{}))

	first := map[string]int{}
	second := map[string]int{}
	for i := range 64 {
		first[string(rune('a'+i%26))+string(rune('A'+i/26))] = i
	}
	maps.Copy(second, first)
	require.Equal(t, requestDigest(t, "M", first), requestDigest(t, "M", second))
	require.NotEqual(t, requestDigest(t, "M", map[string]string{"a": "b"}), requestDigest(t, "M", map[string]string{"b": "a"}))
}

func TestRequestDigestRefusesWhatItCannotEncode(t *testing.T) {
	for name, request := range map[string]any{
		"channel":          struct{ C chan int }{C: make(chan int)},
		"function":         struct{ F func() }{F: func() {}},
		"complex":          struct{ C complex128 }{C: 1i},
		"unexported field": struct{ hidden string }{hidden: "x"},
		"unencodable JSON": struct{ V any }{V: make(chan int)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := storagev1.RequestDigest("M", request)
			require.ErrorIs(t, err, storagev1.ErrValidation)
		})
	}

	type node struct{ Next *node }
	cycle := &node{}
	cycle.Next = cycle
	_, err := storagev1.RequestDigest("M", cycle)
	require.ErrorIs(t, err, storagev1.ErrValidation, "a pointer cycle is refused, not followed forever")

	_, err = storagev1.RequestDigest("", "x")
	require.ErrorIs(t, err, storagev1.ErrValidation)

	require.Equal(t, requestDigest(t, "M", struct{ F func() }{}), requestDigest(t, "M", struct{}{}),
		"a nil function is zero")
}

func FuzzRequestDigest(f *testing.F) {
	f.Add("a", "b", int64(1), uint64(2), int64(1_700_000_000), uint32(5), []byte(`{"k":[1,2]}`), 1.5)
	f.Add("", "", int64(0), uint64(0), int64(0), uint32(0), []byte(nil), 0.0)
	f.Add("ab", "", int64(-1), uint64(1<<63), int64(-62135596800), uint32(999_999_999), []byte("not json"), math.NaN())

	type sample struct {
		A, B  string
		N     int64
		U     uint64
		At    time.Time
		Raw   json.RawMessage
		F     float64
		Tags  []string
		Owner *string
	}
	f.Fuzz(func(t *testing.T, a, b string, n int64, u uint64, sec int64, nsec uint32, raw []byte, value float64) {
		at := time.Unix(sec%(1<<40), int64(nsec%1_000_000_000))
		build := func() sample {
			return sample{A: a, B: b, N: n, U: u, At: at, F: value}
		}
		digest := requestDigest(t, "M", build())
		require.Equal(t, digest, requestDigest(t, "M", build()), "deterministic")

		zoned := build()
		zoned.At = at.In(time.FixedZone("offset", 5*60*60+30*60))
		require.Equal(t, digest, requestDigest(t, "M", zoned), "the time zone does not matter")

		empty := build()
		empty.Tags = []string{}
		require.Equal(t, digest, requestDigest(t, "M", empty), "an empty slice equals a nil one")

		withOwner := build()
		withOwner.Owner = &a
		require.NotEqual(t, digest, requestDigest(t, "M", withOwner), "a present pointer differs from an absent one")

		changed := build()
		changed.A = a + "x"
		require.NotEqual(t, digest, requestDigest(t, "M", changed))
		if b != "" {
			shifted := build()
			shifted.A, shifted.B = a+b, ""
			require.NotEqual(t, digest, requestDigest(t, "M", shifted), "values are length-delimited")
		}

		withRaw := build()
		withRaw.Raw = raw
		got, err := storagev1.RequestDigest("M", withRaw)
		switch {
		case len(bytes.Trim(raw, " \t\r\n")) == 0:
			require.NoError(t, err)
			require.Equal(t, digest, got, "empty JSON is zero")
		case !json.Valid(raw):
			require.ErrorIs(t, err, storagev1.ErrValidation)
		default:
			require.NoError(t, err)
			var indented bytes.Buffer
			require.NoError(t, json.Indent(&indented, raw, " ", "\t"))
			reformatted := build()
			reformatted.Raw = indented.Bytes()
			require.Equal(t, got, requestDigest(t, "M", reformatted), "JSON layout does not matter")
		}
	})
}

func BenchmarkRequestDigestLargeArtifact(b *testing.B) {
	request := storagev1.SaveArtifactRequest{
		Owner:    storagev1.ArtifactOwner{Namespace: "tenant", TaskName: "task", TaskUID: "task-uid"},
		Filename: "report.bin",
		Data:     bytes.Repeat([]byte{0x5a}, storagev1.MaxArtifactBytes),
	}
	b.SetBytes(int64(len(request.Data)))
	for b.Loop() {
		if _, err := storagev1.RequestDigest("SaveArtifact", request); err != nil {
			b.Fatal(err)
		}
	}
}
