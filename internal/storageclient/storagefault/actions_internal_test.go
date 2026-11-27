package storagefault

import (
	"context"
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func TestHangAbandonsAnEndedCallReleasedAtTheSameMoment(t *testing.T) {
	hang := NewHang()
	close(hang.release)
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	for range 50 {
		injected, err := hang.apply(ended, "GetResult", func(context.Context) error {
			t.Fatal("a call whose context ended reached the provider")
			return nil
		})
		require.True(t, injected)
		require.ErrorIs(t, err, storagev1.ErrUnavailable)
		require.ErrorIs(t, err, context.Canceled)
	}
	require.Zero(t, hang.Held())
}
