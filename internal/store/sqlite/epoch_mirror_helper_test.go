package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func mirrorTestFence(t *testing.T, s *Store, fence store.ControllerEpochFence) {
	t.Helper()
	at := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := s.SyncControllerEpochMirror(context.Background(), store.ControllerEpoch{
		Name: fence.Name, Epoch: fence.Epoch, HolderID: fence.HolderID,
		RequestDigest: controlTestDigest("mirror-" + fence.HolderID), Version: fence.Epoch,
		AcquiredAt: at, UpdatedAt: at,
	}); err != nil {
		t.Fatalf("mirror controller epoch fence: %v", err)
	}
}
