package controller

import (
	"context"
	"testing"

	"github.com/orka-agents/orka/internal/store"
)

func syncTestControllerEpochMirror(t *testing.T, epochs store.ControllerEpochStore, mirror store.ControllerEpochMirror) {
	t.Helper()
	epoch, err := epochs.GetControllerEpoch(context.Background(), store.DefaultControllerEpochName)
	if err != nil {
		t.Fatalf("read controller epoch: %v", err)
	}
	if err := mirror.SyncControllerEpochMirror(context.Background(), *epoch); err != nil {
		t.Fatalf("sync controller epoch mirror: %v", err)
	}
}
