package storagefault

import (
	"context"
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// TestHangAbandonsAnEndedCallReleasedAtTheSameMoment makes the release and
// the context's end both ready before a held call selects, so the release
// branch is taken about half the time; the call must still be abandoned.
func TestHangAbandonsAnEndedCallReleasedAtTheSameMoment(t *testing.T) {
	hang := NewHang()
	// Close the release channel without marking the hang released, as when a
	// release arrives while a call is about to wait, so apply reaches select.
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
