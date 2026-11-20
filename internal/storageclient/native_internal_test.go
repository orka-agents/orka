package storageclient

import (
	"testing"

	"github.com/stretchr/testify/require"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestNativeSessionSizePolicyConfiguration(t *testing.T) {
	client := &Client{}
	require.Equal(t, harnessv2.DefaultMaxNativeSessionBytes, client.nativeSessionLimit())
	for _, limit := range []int{640 << 10, harnessv2.MaxNativeSessionBytes, 0} {
		require.NoError(t, client.SetNativeSessionMaxBytes(limit))
		want := limit
		if want == 0 {
			want = harnessv2.DefaultMaxNativeSessionBytes
		}
		require.Equal(t, want, client.nativeSessionLimit())
	}
	require.NoError(t, client.SetNativeSessionMaxBytes(640<<10))
	for _, limit := range []int{-1, harnessv2.MaxNativeSessionBytes + 1} {
		require.Error(t, client.SetNativeSessionMaxBytes(limit))
		require.Equal(t, 640<<10, client.nativeSessionLimit(), "rejected configuration must not change the previous policy")
	}
}
