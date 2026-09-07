package kube

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestControllerEpochMutationSerializesTakeoverAndReleasesOnError(t *testing.T) {
	kubeStore, _, fence := newTestStoreWithEpoch(t)
	ctx := t.Context()
	sentinel := errors.New("mutation failed")
	change := store.ControllerEpochCAS{ExpectedVersion: 1, ExpectedEpoch: 1, NewEpoch: 2,
		HolderID: "successor", RequestDigest: testDigest("successor"), UpdatedAt: time.Now().UTC()}
	err := kubeStore.WithControllerEpochMutation(ctx, fence, func(mutationCtx context.Context) error {
		deadline, bounded := mutationCtx.Deadline()
		if !bounded || time.Until(deadline) > 21*time.Second {
			t.Fatal("external mutation did not receive a bounded context")
		}
		// Takeover uses a distinct store instance so this checks the durable
		// Lease authority, not just a per-instance synchronization primitive.
		successor, err := New(kubeStore.client, testControlNamespace, WithAPIReader(kubeStore.readClient()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := successor.CompareAndSwapControllerEpoch(ctx, change); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("takeover passed an active mutation fence: %v", err)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("callback error was lost: %v", err)
	}
	if _, err := kubeStore.CompareAndSwapControllerEpoch(ctx, change); err != nil {
		t.Fatalf("mutation lock was not released on failure: %v", err)
	}
	called := false
	err = kubeStore.WithControllerEpochMutation(ctx, fence, func(context.Context) error { called = true; return nil })
	if !errors.Is(err, store.ErrConflict) || called {
		t.Fatalf("stale owner callback called=%t, err=%v", called, err)
	}
}

func TestControllerEpochMutationRejectsMissingClientAndCallback(t *testing.T) {
	var missing *Store
	if err := missing.WithControllerEpochMutation(t.Context(), store.ControllerEpochFence{}, func(context.Context) error { t.Fatal("callback ran"); return nil }); err == nil {
		t.Fatal("nil control store accepted a mutation")
	}
	s, _, fence := newTestStoreWithEpoch(t)
	if err := s.WithControllerEpochMutation(t.Context(), fence, nil); err == nil {
		t.Fatal("nil callback was accepted")
	}
}
