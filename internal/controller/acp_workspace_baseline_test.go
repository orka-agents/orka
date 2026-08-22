/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// Every turn of one logical Session must present the exact repo-less protocol
// baseline the runtime session was created with: the supervisor rejects a
// workspace delta whose VerifiedBaseline differs from the creation baseline,
// so a task-scoped identity fails every repo-less continuation with a digest
// conflict.
func TestRepoLessWorkspaceBaselineIsStableAcrossSessionTurns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := &ACPDispatcher{}
	session := &acpTaskSession{Binding: ACPRuntimeSessionBinding{SessionUID: "acp-session-stable"}}

	first := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "turn-1", Namespace: "orka-system", UID: "task-uid-turn-1"}}
	first.Spec.Type = corev1alpha1.TaskTypeAgent
	second := first.DeepCopy()
	second.Name, second.UID = "turn-2", "task-uid-turn-2"

	firstPrepared, err := d.prepareRuntimeWorkspace(ctx, first, store.ControllerEpochFence{}, session)
	if err != nil {
		t.Fatalf("prepare first turn: %v", err)
	}
	secondPrepared, err := d.prepareRuntimeWorkspace(ctx, second, store.ControllerEpochFence{}, session)
	if err != nil {
		t.Fatalf("prepare second turn: %v", err)
	}
	if firstPrepared.baseline != secondPrepared.baseline {
		t.Fatalf("session turns derived different repo-less baselines:\n%+v\n%+v",
			firstPrepared.baseline, secondPrepared.baseline)
	}
	if firstPrepared.bindingDigest != secondPrepared.bindingDigest {
		t.Fatalf("session turns derived different workspace binding digests")
	}

	standalone, err := d.prepareRuntimeWorkspace(ctx, first, store.ControllerEpochFence{}, nil)
	if err != nil {
		t.Fatalf("prepare standalone: %v", err)
	}
	if standalone.baseline == firstPrepared.baseline {
		t.Fatal("a standalone Task must keep its task-scoped repo-less baseline")
	}
}
