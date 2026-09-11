package controller

import (
	"context"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestSessionCleanupRecoveryManagerResumesWithCurrentFence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fence := store.ControllerEpochFence{Name: store.DefaultControllerEpochName, Epoch: 7, HolderID: "controller-a"}
	cleanup := &recordingSessionCleanupRecovery{calls: make(chan store.ControllerEpochFence, 1)}
	manager := &SessionCleanupRecoveryManager{
		cleanup:  cleanup,
		fences:   staticSessionCleanupFenceSource{fence: fence},
		interval: time.Hour,
	}
	done := make(chan error, 1)
	go func() { done <- manager.Start(ctx) }()
	select {
	case got := <-cleanup.calls:
		if got != fence {
			t.Fatalf("recovery fence = %#v, want %#v", got, fence)
		}
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("Session cleanup recovery did not run")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Session cleanup recovery did not stop")
	}
	if !manager.NeedLeaderElection() {
		t.Fatal("Session cleanup recovery must require leader election")
	}
}

type recordingSessionCleanupRecovery struct {
	calls chan store.ControllerEpochFence
}

func (r *recordingSessionCleanupRecovery) ResumeSessionCleanups(_ context.Context, fence store.ControllerEpochFence) error {
	r.calls <- fence
	return nil
}

type staticSessionCleanupFenceSource struct {
	fence store.ControllerEpochFence
	err   error
}

func (s staticSessionCleanupFenceSource) CurrentFence(context.Context) (store.ControllerEpochFence, error) {
	return s.fence, s.err
}
