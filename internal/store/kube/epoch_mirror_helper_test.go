package kube

import (
	"context"
	"errors"
	"testing"

	controlstore "github.com/orka-agents/orka/internal/store"
)

func syncTestEpochMirror(t *testing.T, kubeStore *Store, persistence any) {
	t.Helper()
	epoch, err := kubeStore.GetControllerEpoch(context.Background(), controlstore.DefaultControllerEpochName)
	if errors.Is(err, controlstore.ErrNotFound) {
		return
	}
	if err != nil {
		t.Fatalf("read controller epoch: %v", err)
	}
	mirror, ok := persistence.(controlstore.ControllerEpochMirror)
	if !ok {
		t.Fatalf("SQLite persistence %T has no controller epoch mirror", persistence)
	}
	if err := mirror.SyncControllerEpochMirror(context.Background(), *epoch); err != nil {
		t.Fatalf("sync controller epoch mirror: %v", err)
	}
}
