package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAgentExecutionBindingPreservesCheckpointRestore(t *testing.T) {
	for _, reuse := range []corev1alpha1.WorkspaceReusePolicy{
		corev1alpha1.WorkspaceReusePolicyNone,
		corev1alpha1.WorkspaceReusePolicySession,
	} {
		t.Run(string(reuse), func(t *testing.T) {
			ctx := context.Background()
			fixture := suspendableSubstrateFixture(t)
			fixture.provider.Status.SupportedFeatures = append(fixture.provider.Status.SupportedFeatures,
				workspacev1alpha1.WorkspaceFeatureRestore)
			task := bindingTestTask()
			checkpoint := &corev1alpha1.WorkspaceCheckpointReference{
				Name: "saved-workspace", UID: "checkpoint-uid", Digest: "sha256:" + strings.Repeat("b", 64),
			}
			task.Spec.Execution = &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
				ClassRef:    &corev1alpha1.WorkspaceClassReference{Name: fixture.class.Name},
				ReusePolicy: reuse,
				OnDetach:    corev1alpha1.WorkspaceOnDetachDelete,
				RestoreFrom: checkpoint.DeepCopy(),
			}}
			sessionUID := ""
			if reuse == corev1alpha1.WorkspaceReusePolicySession {
				task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "restored-session", Create: true}
				sessionUID = "restored-session-uid"
			}
			classReconciler := acpClassTestReconciler(t, append(fixture.objects(), task, bindingTestNamespace())...)
			r, _ := newBindingTestReconciler(t)
			r.Client, r.APIReader, r.Scheme = classReconciler.Client, classReconciler.Client, classReconciler.Scheme
			r.WorkspaceProviderAPIEnabled = true
			candidate, err := r.resolveAgentExecutionCandidateWithWorkspaceSessionUID(ctx, task, bindingTestAgent(), sessionUID)
			if err != nil {
				t.Fatalf("resolve restore Task: %v", err)
			}
			if err := r.persistAgentExecutionSnapshot(ctx, task, candidate); err != nil {
				t.Fatal(err)
			}
			if _, err := r.persistAgentExecutionBinding(ctx, task, candidate); err != nil {
				t.Fatal(err)
			}
			bound := &corev1alpha1.Task{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(task), bound); err != nil {
				t.Fatal(err)
			}

			// Every queue reconcile reloads this immutable plan before creating
			// workspace demand. The retained checkpoint must survive that boundary.
			verified, err := r.loadVerifiedBoundExecution(ctx, bound, bound.Status.AgentExecutionBinding)
			if err != nil {
				t.Fatalf("reload checkpoint restore for dispatch: %v", err)
			}
			if verified.plan.Workspace == nil || !reflect.DeepEqual(verified.plan.Workspace.RestoreFrom, checkpoint) {
				t.Fatalf("restored execution plan lost checkpoint identity: %+v", verified.plan.Workspace)
			}
			for _, tamper := range []struct {
				name   string
				change func(*agentExecutionSnapshotWorkspaceBinding)
			}{
				{name: "removed", change: func(ws *agentExecutionSnapshotWorkspaceBinding) { ws.RestoreFrom = nil }},
				{name: "name", change: func(ws *agentExecutionSnapshotWorkspaceBinding) { ws.RestoreFrom.Name = "another-checkpoint" }},
				{name: "uid", change: func(ws *agentExecutionSnapshotWorkspaceBinding) { ws.RestoreFrom.UID = "replacement-uid" }},
				{name: "digest", change: func(ws *agentExecutionSnapshotWorkspaceBinding) {
					ws.RestoreFrom.Digest = "sha256:" + strings.Repeat("c", 64)
				}},
			} {
				t.Run(tamper.name, func(t *testing.T) {
					body := verified.body
					workspace := *body.ExecutionWorkspace
					workspace.RestoreFrom = checkpoint.DeepCopy()
					tamper.change(&workspace)
					body.ExecutionWorkspace = &workspace
					if _, err := verifiedSnapshotWorkspaceBinding(bound.Status.AgentExecutionBinding, body); err == nil {
						t.Fatal("changed checkpoint identity passed frozen binding verification")
					}
				})
			}
		})
	}
}
