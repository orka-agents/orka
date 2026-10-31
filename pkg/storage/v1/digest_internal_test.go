package v1

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCanonicalJSONIsPinned pins canonical JSON byte for byte: keys sorted,
// encoding/json's escaping, and every number exactly as written, so a change
// in either shows up here rather than as silently different request digests.
func TestCanonicalJSONIsPinned(t *testing.T) {
	for input, want := range map[string]string{
		` { "b" : [1.0, 2.50, 1e2, -0.0, 1E+21, 1e-7], "a" : null } `: `{"a":null,"b":[1.0,2.50,1e2,-0.0,1E+21,1e-7]}`,
		"\"<tag> & \\u2028 \\u00e9 \u00e9\"":                          "\"\\u003ctag\\u003e \\u0026 \\u2028 \u00e9 \u00e9\"",
		`1152921504606846976`:                                         `1152921504606846976`,
		`1152921504606847000`:                                         `1152921504606847000`,
		`18446744073709551617`:                                        `18446744073709551617`,
		`0.1000000000000000055511151231257827021181583404541015625`:   `0.1000000000000000055511151231257827021181583404541015625`,
		`1e400`:                      `1e400`,
		`0e99999999999999999999`:     `0e99999999999999999999`,
		`{"k":{"z":true,"y":false}}`: `{"k":{"y":false,"z":true}}`,
	} {
		got, err := canonicalJSON([]byte(input))
		require.NoError(t, err, input)
		require.Equal(t, want, string(got), input)
	}
}
