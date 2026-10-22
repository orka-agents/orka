package v1

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCanonicalJSONIsPinned pins canonical JSON byte for byte, including the
// escaping and number forms encoding/json produces, so a change in either
// shows up here rather than as silently different request digests.
func TestCanonicalJSONIsPinned(t *testing.T) {
	for input, want := range map[string]string{
		` { "b" : [1.0, 2.50, 1e2, -0.0, 1E+21, 1e-7], "a" : null } `: `{"a":null,"b":[1,2.5,100,-0,1e+21,1e-7]}`,
		"\"<tag> & \\u2028 \\u00e9 \u00e9\"":                          "\"\\u003ctag\\u003e \\u0026 \\u2028 \u00e9 \u00e9\"",
		`1152921504606846977`:                                         `1152921504606846977`,
		`0.1000000000000000055511151231257827`:                        `0.1000000000000000055511151231257827`,
		`1e400`:                                                       `1e400`,
		`1152921504606846976`:                                         `1152921504606847000`,
		`-9223372036854775808`:                                        `-9223372036854776000`,
		`18446744073709551616`:                                        `18446744073709552000`,
		`-340282366920938463463374607431768211456`:                    `-3.402823669209385e+38`,
		`18446744073709551617`:                                        `18446744073709551617`,
		`18446744073709551615`:                                        `18446744073709551615`,
		`0.1000000000000000055511151231257827021181583404541015625`:   `0.1000000000000000055511151231257827021181583404541015625`,
		`0e99999999999999999999`:                                      `0`,
		`-0E-5`:                                                       `-0`,
		`{"k":{"z":true,"y":false}}`:                                  `{"k":{"y":false,"z":true}}`,
	} {
		got, err := canonicalJSON([]byte(input))
		require.NoError(t, err, input)
		require.Equal(t, want, string(got), input)
	}
}

func TestDecimalParts(t *testing.T) {
	type parts struct {
		negative bool
		digits   string
		exponent int
	}
	for number, want := range map[string]parts{
		"0":                         {},
		"-0.000":                    {negative: true},
		"0e999999":                  {},
		"-0.0e99999999999999999999": {negative: true},
		"123.45":                    {digits: "12345", exponent: 3},
		"0.0012":                    {digits: "12", exponent: -2},
		"100":                       {digits: "1", exponent: 3},
		"1e2":                       {digits: "1", exponent: 3},
		"1.50E-3":                   {digits: "15", exponent: -2},
		"-2e+21":                    {negative: true, digits: "2", exponent: 22},
	} {
		negative, digits, exponent, ok := decimalParts(number)
		require.True(t, ok, number)
		require.Equal(t, want, parts{negative, digits, exponent}, number)
	}
	_, _, _, ok := decimalParts("1e99999999999999999999")
	require.False(t, ok, "an exponent beyond int is not compared")
	_, _, _, ok = decimalParts("1e2000000000")
	require.False(t, ok, "an exponent beyond the compared range is not compared")
}
