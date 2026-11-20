package storageclient

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestGatewayTextTruncationPreservesUTF8(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		limit int
		want  string
	}{
		{"no limit", "aé", 0, "aé"},
		{"exact fit", "aé", 3, "aé"},
		{"two-byte partial rune", "aé", 2, "a"},
		{"three-byte partial rune", "a中", 3, "a"},
		{"four-byte partial rune", "a🙂", 4, "a"},
		{"first rune exceeds limit", "🙂", 1, ""},
		{"preserve complete final rune", "a🙂z", 5, "a🙂"},
		{"reason limit", strings.Repeat("x", gatewayReasonBytes-1) + "é", gatewayReasonBytes, strings.Repeat("x", gatewayReasonBytes-1)},
		{"identity limit", strings.Repeat("x", gatewayIdentityBytes-1) + "🙂", gatewayIdentityBytes, strings.Repeat("x", gatewayIdentityBytes-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeGatewayText(tc.value, tc.limit)
			require.Equal(t, tc.want, got)
			require.True(t, utf8.ValidString(got))
		})
	}
}
