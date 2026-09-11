package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

func nativeACPRecoveryFixture(t *testing.T) (*nativeRuntimeTestHarness, *TaskReconciler, *corev1alpha1.Task, *workspacev1alpha1.ExecutionWorkspace) {
	t.Helper()
	h := newNativeRuntimeTestHarness(t)
	// Materialize the native test pool in the class fixture's tenant namespace.
	initialPool := runtimePoolTestGetPool(t, h.r, h.pool)
	require.NoError(t, h.r.Delete(t.Context(), &initialPool))
	initialPool.Namespace, initialPool.ResourceVersion = acpTestNamespace, ""
	require.NoError(t, h.r.Create(t.Context(), &initialPool))
	h.pool = &initialPool
	require.NoError(t, acpworkspacev1alpha1.AddToScheme(h.r.Scheme))
	require.NoError(t, storagev1.AddToScheme(h.r.Scheme))
	require.NoError(t, coordinationv1.AddToScheme(h.r.Scheme))
	r := &TaskReconciler{
		Client: h.r.Client, APIReader: h.r.Client, Scheme: h.r.Scheme,
		ControllerNamespace:         h.r.ControllerNamespace,
		WorkspaceProviderAPIEnabled: true, WorkspaceSettlementProtected: true,
	}
	fixture := suspendableSubstrateFixture(t)
	for _, obj := range fixture.objects() {
		require.NoError(t, r.Create(t.Context(), obj))
	}
	task := suspendableSessionTask()
	require.NoError(t, r.Create(t.Context(), task))
	task = bindSuspendableSessionTaskForSettlement(t, r, task)
	resolved, err := r.resolveACPWorkspaceClass(t.Context(), task)
	require.NoError(t, err)
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", false, suspendTestSessionUID, resolved)
	require.NoError(t, err)
	plan := ACPRuntimePlan{PoolName: h.pool.Name, Workspace: binding}
	_, _, err = r.ensureACPClassWorkspace(t.Context(), task, plan)
	require.NoError(t, err)
	name := acpClassWorkspaceName(task, binding)
	ws := &workspacev1alpha1.ExecutionWorkspace{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: task.Namespace, Name: name}, ws))
	admitTestACPWorkspace(t, r, ws)
	_, ready := attachTestACPWorkspace(t, r, task, plan, name)
	require.True(t, ready)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(ws), ws))
	ws.Finalizers = append(ws.Finalizers, executionWorkspaceFinalizer)
	require.NoError(t, r.Update(t.Context(), ws))
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	pool.Labels[acpExecutionWorkspaceLinkLabel] = ws.Name
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	pool.Annotations[acpExecutionWorkspaceUIDAnnotation] = string(ws.UID)
	require.NoError(t, h.r.Update(t.Context(), &pool))
	h.until(t, nativeTestServing)
	h.api.data[h.record(t).Attempt.Name] = "last verified workspace bytes"
	substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
	h.until(t, nativeTestSuspended)
	substrateSuspendTestPoolIntent(t, h.r, h.pool, false)
	h.until(t, nativeTestServing)
	h.api.data[h.record(t).Attempt.Name] = "later uncertain workspace bytes"
	delete(h.api.actors, h.record(t).Attempt.Name)
	h.step(t)
	require.Equal(t, substrateNativeFailed, h.record(t).Phase)
	ws.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
	require.NoError(t, r.Status().Update(t.Context(), ws))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	task.Status.Phase = corev1alpha1.TaskPhaseFailed
	task.Status.Execution = &corev1alpha1.TaskExecutionStatus{RuntimePoolName: h.pool.Name, State: "OutcomeUnknown", Outcome: "OutcomeUnknown", Attempt: 1}
	require.NoError(t, r.Status().Update(t.Context(), task))
	return h, r, task, ws
}

func TestSettleACPClassWorkspaceRetainsNativeRecoveryDataAfterActorLoss(t *testing.T) {
	h, r, task, ws := nativeACPRecoveryFixture(t)
	digest, resumes := h.record(t).Checkpoint.Digest, h.api.resumes
	worker := *h.record(t).Attempt.Worker
	done, err := r.settleACPClassWorkspace(t.Context(), task)
	require.NoError(t, err)
	require.False(t, done)
	adapter := &ACPExecutionWorkspaceAdapterReconciler{Client: r.Client, APIReader: r.APIReader}
	_, err = adapter.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ws)})
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(ws), ws))
	require.Equal(t, workspacev1alpha1.ExecutionWorkspaceStateFailed, ws.Status.State)
	settleTestACPClassWorkspace(t, r, task, ws.Name)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(ws), ws))
	require.True(t, ws.DeletionTimestamp.IsZero())
	require.Equal(t, workspacev1alpha1.ExecutionWorkspaceStateFailed, ws.Status.State)
	require.Nil(t, ws.Spec.Attachment)
	require.True(t, workspaceConsumesSuspendedQuota(ws))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.NotEmpty(t, task.Annotations[acpTaskWorkspaceSettledAnnotation])

	h.until(t, func(pool *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
		return record.Phase == substrateNativeFailed && record.Attempt == nil && pool.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionClosed
	})
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKey{Namespace: worker.Namespace, Name: worker.Pod}, &corev1.Pod{})))
	require.Empty(t, h.api.actors)
	require.Equal(t, resumes, h.api.resumes)
	cp := nativeExportCheckpoint(t, h, ws, true)
	require.Equal(t, digest, cp.Status.Digest)
	require.Equal(t, "last verified workspace bytes", h.api.tagData[h.record(t).Checkpoint.Name])
	// A repeated settlement cannot delete the failed source after export.
	settleTestACPClassWorkspace(t, r, task, ws.Name)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(ws), ws))

	// The existing hard retention deadline still releases the source. The
	// exported checkpoint owns its independent reference before cleanup.
	retention := &ACPWorkspaceRetentionReconciler{Client: r.Client, APIReader: r.APIReader,
		Now: func() time.Time {
			return ws.CreationTimestamp.Add(ws.Spec.Lifecycle.MaxLifetime.Duration + time.Second)
		}}
	_, err = retention.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ws)})
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(ws), ws))
	require.False(t, ws.DeletionTimestamp.IsZero())
	nativeDeletePool(t, h, h.pool)
	require.Len(t, h.api.tags, 1)
	_, artifact, err := h.r.readSubstrateCheckpointArtifact(t.Context(), digest)
	require.NoError(t, err)
	require.True(t, artifact.Owners[substratePublicCheckpointOwner(cp)])
}

func TestSettleACPClassWorkspaceRecoveryHonorsDeleteAndLifetime(t *testing.T) {
	for _, reason := range []string{"Delete", "maxLifetime"} {
		t.Run(reason, func(t *testing.T) {
			h, r, task, ws := nativeACPRecoveryFixture(t)
			if reason == "Delete" {
				ws.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachDelete)
			} else {
				ws.CreationTimestamp = metav1.NewTime(time.Now().Add(-24 * time.Hour))
			}
			require.NoError(t, r.Update(t.Context(), ws))
			settleTestACPClassWorkspace(t, r, task, ws.Name)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(ws), ws))
			require.False(t, ws.DeletionTimestamp.IsZero())
			nativeDeletePool(t, h, h.pool)
			require.Empty(t, h.api.tags)
		})
	}
}

func TestFailedACPWorkspaceNativeRecoveryRequiresExactRetainedReference(t *testing.T) {
	for _, mismatch := range []string{"pool lifetime", "catalog owner", "class", "runtime", "unavailable journal"} {
		t.Run(mismatch, func(t *testing.T) {
			h, r, _, ws := nativeACPRecoveryFixture(t)
			pool := runtimePoolTestGetPool(t, h.r, h.pool)
			cm, artifact, err := h.r.readSubstrateCheckpointArtifact(t.Context(), h.record(t).Checkpoint.Digest)
			require.NoError(t, err)
			switch mismatch {
			case "pool lifetime":
				pool.Annotations[acpExecutionWorkspaceUIDAnnotation] = "another-workspace-uid"
				require.NoError(t, r.Update(t.Context(), &pool))
			case "catalog owner":
				delete(artifact.Owners, substratePoolCheckpointOwner(&pool))
			case "class":
				artifact.ClassBinding.UID = "another-class-uid"
			case "runtime":
				artifact.Runtime.Image = "another-runtime-image"
			case "unavailable journal":
				r.APIReader = recoveryJournalUnavailableReader{Reader: r.Client}
			}
			require.NoError(t, h.r.saveSubstrateCheckpointArtifact(t.Context(), cm, artifact))
			retained, err := r.failedACPWorkspaceHasNativeCheckpoint(t.Context(), ws)
			if mismatch == "unavailable journal" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.False(t, retained)
		})
	}
}

type recoveryJournalUnavailableReader struct{ client.Reader }

func (r recoveryJournalUnavailableReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, journal := obj.(*corev1.ConfigMap); journal {
		return apierrors.NewServiceUnavailable("injected journal read failure")
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}
