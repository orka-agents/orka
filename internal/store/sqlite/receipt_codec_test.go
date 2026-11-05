package sqlite

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

type receiptCodecInner struct {
	Note  string
	Count *int
}

type receiptCodecSample struct {
	Zero        *int
	Absent      *int
	Flag        *bool
	When        time.Time
	WhenLocal   time.Time
	MaybeWhen   *time.Time
	ZeroWhen    *time.Time
	Raw         json.RawMessage
	Data        []byte
	EmptyData   []byte
	NilData     []byte
	Items       []receiptCodecInner
	EmptyItems  []string
	NilItems    []string
	Labels      map[string]string
	EmptyLabels map[string]string
	NilLabels   map[string]string
	Nested      **receiptCodecInner
	Big         int64
	Small       int64
	Huge        uint64
	Ratio       float64
	NotANumber  float64
	Array       [2]string
}

func TestReceiptCodecReplaysExactValues(t *testing.T) {
	zero := 0
	no := false
	when := time.Date(2026, 9, 1, 12, 0, 0, 123456789, time.UTC)
	inner := &receiptCodecInner{Note: "inner", Count: &zero}
	sample := receiptCodecSample{
		Zero: &zero, Flag: &no,
		When: when, WhenLocal: when.In(time.FixedZone("AEST", 10*60*60)), MaybeWhen: &when, ZeroWhen: &time.Time{},
		Raw: json.RawMessage(`{"n":1.0}`), Data: []byte{0, 1, 2}, EmptyData: []byte{},
		Items:      []receiptCodecInner{{Note: "a"}, {Note: "b", Count: &zero}},
		EmptyItems: []string{}, Labels: map[string]string{"k": "v"}, EmptyLabels: map[string]string{},
		Nested: &inner, Big: math.MaxInt64, Small: math.MinInt64, Huge: math.MaxUint64,
		Ratio: 0.1, NotANumber: math.NaN(), Array: [2]string{"x", ""},
	}
	encoded, err := marshalReceiptValue(&sample)
	require.NoError(t, err)
	var decoded *receiptCodecSample
	require.NoError(t, unmarshalReceiptValue(encoded, &decoded))
	require.True(t, math.IsNaN(decoded.NotANumber))
	decoded.NotANumber, sample.NotANumber = 0, 0
	require.True(t, decoded.WhenLocal.Equal(sample.WhenLocal))
	_, offset := decoded.WhenLocal.Zone()
	require.Equal(t, 10*60*60, offset, "a time keeps its offset")
	decoded.WhenLocal, sample.WhenLocal = time.Time{}, time.Time{}
	require.Equal(t, sample, *decoded, "pointers to zero values, nil and empty collections and times survive exactly")
	require.NotNil(t, decoded.Zero)
	require.Nil(t, decoded.Absent)
	require.NotNil(t, decoded.EmptyItems)
	require.Nil(t, decoded.NilItems)

	var nilResult *receiptCodecSample
	encoded, err = marshalReceiptValue(nilResult)
	require.NoError(t, err)
	decoded = &receiptCodecSample{}
	require.NoError(t, unmarshalReceiptValue(encoded, &decoded))
	require.Nil(t, decoded)

	// A string cut inside a multibyte character, as byte-bounded sanitizing
	// can leave it, keeps its exact bytes, as a value and as a map key.
	truncated := strings.Repeat("a", 255) + "\xf0\x9f"
	encodedStrings, err := marshalReceiptValue(map[string]string{truncated: truncated, "ok": "\u00e9"})
	require.NoError(t, err)
	var decodedStrings map[string]string
	require.NoError(t, unmarshalReceiptValue(encodedStrings, &decodedStrings))
	require.Equal(t, map[string]string{truncated: truncated, "ok": "\u00e9"}, decodedStrings)
	require.False(t, utf8.ValidString(decodedStrings[truncated]))

	_, err = marshalReceiptValue(struct{ Value any }{Value: 1})
	require.Error(t, err, "an interface cannot be replayed exactly")
	_, err = marshalReceiptValue(struct{ hidden int }{})
	require.Error(t, err, "an unexported field cannot be replayed")
}
