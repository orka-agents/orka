package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const provisioningHandshakeTaskUID = "provisioning-task-uid"

func provisioningHandshakeObjects(t *testing.T) (*runtime.Scheme, *workspacev1alpha1.ExecutionWorkspace, *corev1alpha1.RuntimePool) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	workspace := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: corev1.NamespaceDefault, Name: "resuming-workspace", UID: types.UID("resuming-workspace-uid"),
			Generation: 1, CreationTimestamp: metav1.NewTime(now.Add(-time.Minute)),
			Annotations: map[string]string{acpExecutionWorkspacePoolAnnotation: "resuming-pool", acpWorkspaceResumedLineageAnnotation: booleanTrueValue},
		},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
			DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredReady, AttachmentEpoch: 3,
			Attachment: &workspacev1alpha1.ExecutionWorkspaceAttachment{
				TaskRef: workspacev1alpha1.ObjectIdentityReference{UID: types.UID(provisioningHandshakeTaskUID)},
				Epoch:   3, ExpiresAt: metav1.NewTime(now.Add(time.Hour)),
			},
			Lifecycle: workspacev1alpha1.ExecutionWorkspaceLifecycle{MaxLifetime: &metav1.Duration{Duration: 2 * time.Hour}},
		},
	}
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateProvisioning
	workspace.Status.AttachedEpoch = 0
	workspace.Status.Conditions = append(workspace.Status.Conditions, metav1.Condition{
		Type: string(workspacev1alpha1.ConditionWorkspaceAttached), Status: metav1.ConditionFalse,
		Reason: string(workspacev1alpha1.ReasonProgressing), ObservedGeneration: workspace.Generation,
	})
	pool := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: workspace.Namespace, Name: "resuming-pool", UID: types.UID("original-data-pool-uid"),
			Labels:      map[string]string{acpExecutionWorkspaceLinkLabel: workspace.Name},
			Annotations: map[string]string{acpExecutionWorkspaceUIDAnnotation: string(workspace.UID)},
		},
		Spec: corev1alpha1.RuntimePoolSpec{DesiredReplicas: 1, ExecutionWorkspace: &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{Provider: corev1alpha1.WorkspaceProviderAgentSandbox}},
	}
	return scheme, workspace, pool
}

func TestWorkspacePoolHandshakeWaitsForProvisioning(t *testing.T) {
	t.Parallel()
	scheme, workspace, pool := provisioningHandshakeObjects(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace).WithObjects(workspace, pool).Build()
	r := &TaskReconciler{Client: c, APIReader: c, Scheme: scheme}
	err := r.verifyACPWorkspaceReadyForPool(t.Context(), pool, workspace.Name, string(workspace.UID), provisioningHandshakeTaskUID)
	if !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("provisioning handshake = %v, want retryable readiness without deleting the original pool", err)
	}
	current := &corev1alpha1.RuntimePool{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("original pool was lost during reversible provisioning: %v", err)
	}
	if current.UID != pool.UID || !current.DeletionTimestamp.IsZero() || !reflect.DeepEqual(current.Spec, pool.Spec) {
		t.Fatal("waiting for readiness altered original pool ownership or demand")
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(workspace), workspace); err != nil {
		t.Fatal(err)
	}
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateAttached
	workspace.Status.AttachedEpoch = workspace.Spec.Attachment.Epoch
	for i := range workspace.Status.Conditions {
		if workspace.Status.Conditions[i].Type == string(workspacev1alpha1.ConditionWorkspaceAttached) {
			workspace.Status.Conditions[i].Status = metav1.ConditionTrue
			workspace.Status.Conditions[i].Reason = string(workspacev1alpha1.ReasonReady)
		}
	}
	if err := c.Status().Update(t.Context(), workspace); err != nil {
		t.Fatal(err)
	}
	if err := r.verifyACPWorkspaceReadyForPool(t.Context(), pool, workspace.Name, string(workspace.UID), provisioningHandshakeTaskUID); err != nil {
		t.Fatalf("same original pool did not become admissible after exact attachment: %v", err)
	}
}

func TestWorkspacePoolHandshakeProvisioningKeepsWithdrawalGuards(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*workspacev1alpha1.ExecutionWorkspace)
	}{
		{"replaced UID", func(w *workspacev1alpha1.ExecutionWorkspace) { w.UID = "replacement" }},
		{"deleting", func(w *workspacev1alpha1.ExecutionWorkspace) {
			now := metav1.Now()
			w.DeletionTimestamp = &now
			w.Finalizers = []string{"test/retain"}
		}},
		{"different pool", func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpExecutionWorkspacePoolAnnotation] = "other-pool"
		}},
		{"quarantined", func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
		}},
		{"core admission withdrawn", func(w *workspacev1alpha1.ExecutionWorkspace) { w.Spec.CoreAdmission.AdmittedGeneration = 0 }},
		{"attachment removed", func(w *workspacev1alpha1.ExecutionWorkspace) { w.Spec.Attachment = nil }},
		{"different task", func(w *workspacev1alpha1.ExecutionWorkspace) { w.Spec.Attachment.TaskRef.UID = "other-task" }},
		{"invalid epoch", func(w *workspacev1alpha1.ExecutionWorkspace) { w.Spec.Attachment.Epoch = 0 }},
		{"changed epoch", func(w *workspacev1alpha1.ExecutionWorkspace) { w.Spec.AttachmentEpoch++ }},
		{"expired attachment", func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.Attachment.ExpiresAt = metav1.NewTime(time.Now().Add(-time.Second))
		}},
		{"revocation", func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceRevocationStartedAnnotation] = "3 2026-09-07T00:00:00Z"
		}},
		{"maximum lifetime elapsed", func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.CreationTimestamp = metav1.NewTime(time.Now().Add(-3 * time.Hour))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scheme, w, pool := provisioningHandshakeObjects(t)
			expectedUID := string(w.UID)
			tc.mutate(w)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(w, pool).Build()
			r := &TaskReconciler{Client: c, APIReader: c, Scheme: scheme}
			err := r.verifyACPWorkspaceReadyForPool(context.Background(), pool, w.Name, expectedUID, provisioningHandshakeTaskUID)
			if err == nil || errors.Is(err, store.ErrNotReady) {
				t.Fatalf("withdrawn authority treated as pending: %v", err)
			}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(pool), &corev1alpha1.RuntimePool{}); !apierrors.IsNotFound(err) {
				t.Fatalf("withdrawn workspace retained pool demand: %v", err)
			}
		})
	}
}
