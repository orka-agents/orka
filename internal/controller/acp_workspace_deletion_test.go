package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

func TestDeletingSessionTaskSuspendsBeforeRetirementAndArchival(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, task, workspace, control, retirer := deletingSessionWorkspaceFixture(t)
	var reclamationEvents []string
	control.events = &reclamationEvents
	frozenBinding := task.Status.AgentExecutionBinding.DeepCopy()
	attachmentEpoch := workspace.Spec.Attachment.Epoch

	for range 4 {
		result, err := r.handleDeletion(ctx, task)
		require.NoError(t, err)
		require.Positive(t, result.RequeueAfter, "Task reclamation must wait for retirement and Session archival")
		releaseTestACPEnforcedEpoch(t, r, workspace.Namespace, workspace.Name)
		require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(task), task))
	}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(workspace), workspace))
	require.Nil(t, workspace.Spec.Attachment)
	require.Equal(t, workspacev1alpha1.ExecutionWorkspaceDesiredSuspended, workspace.Spec.DesiredState)
	require.Equal(t, booleanTrueValue, task.Annotations[acpTaskWorkspaceSettledAnnotation])
	require.Equal(t, attachmentEpoch, acpTaskRecordedAttachmentEpoch(task))
	require.Len(t, retirer.calls, 1)
	require.True(t, controllerutil.ContainsFinalizer(task, labels.TaskFinalizer))
	require.Equal(t, frozenBinding, task.Status.AgentExecutionBinding, "Session cleanup authority must survive detach")
	require.Empty(t, task.Status.Execution.RuntimeSessionCleanupDigest)
	require.Empty(t, reclamationEvents, "detach must not even prepare durable PromptAttempt reclamation")
	require.Empty(t, control.reclaimRequests)
	var attempts corev1alpha1.PromptAttemptList
	require.NoError(t, r.List(ctx, &attempts, client.InNamespace(task.Namespace)))
	require.Len(t, attempts.Items, 1)
	require.Equal(t, control.attempt.ID, attempts.Items[0].Spec.ID)

	// Physical retirement alone must not release the Task's archival authority.
	digest, err := taskScopedRuntimeSessionCleanupDigest(task.UID, task.Status.Execution.Attempt,
		task.Status.Execution.RuntimeInstanceID, task.Status.Execution.RuntimeSessionUID,
		task.Status.Execution.RuntimeSessionGeneration)
	require.NoError(t, err)
	task.Status.Execution.RuntimeSessionCleanupDigest = digest
	require.NoError(t, r.Status().Update(ctx, task))
	result, err := r.handleDeletion(ctx, task)
	require.NoError(t, err)
	require.Positive(t, result.RequeueAfter)
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(task), task))
	require.True(t, controllerutil.ContainsFinalizer(task, labels.TaskFinalizer))
	require.Empty(t, reclamationEvents)
	require.Empty(t, control.reclaimRequests)
}

func TestDeletingSessionTaskKeepsAttachmentUntilTerminalSettlement(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *TaskReconciler, *corev1alpha1.Task, *sessionCleanupReceiptControlStore, *recordingIdentityRetirer)
	}{
		{"running prompt", func(_ *testing.T, _ *TaskReconciler, _ *corev1alpha1.Task, s *sessionCleanupReceiptControlStore, _ *recordingIdentityRetirer) {
			s.attempt.ExecutionState = store.PromptExecutionRunning
		}},
		{"unsettled delivery", func(_ *testing.T, _ *TaskReconciler, _ *corev1alpha1.Task, s *sessionCleanupReceiptControlStore, _ *recordingIdentityRetirer) {
			s.attempt.DeliveryState = store.PromptDeliveryValidating
		}},
		{"unfinalized SessionTurn", func(_ *testing.T, _ *TaskReconciler, _ *corev1alpha1.Task, s *sessionCleanupReceiptControlStore, _ *recordingIdentityRetirer) {
			s.turn.State = store.SessionTurnOpen
		}},
		{"different SessionTurn", func(_ *testing.T, _ *TaskReconciler, _ *corev1alpha1.Task, s *sessionCleanupReceiptControlStore, _ *recordingIdentityRetirer) {
			s.turn.ID = "another-turn"
		}},
		{"undelivered projection", func(_ *testing.T, _ *TaskReconciler, _ *corev1alpha1.Task, s *sessionCleanupReceiptControlStore, _ *recordingIdentityRetirer) {
			s.projection.State = store.OutboxProjectionPending
		}},
		{"different projection", func(_ *testing.T, _ *TaskReconciler, _ *corev1alpha1.Task, s *sessionCleanupReceiptControlStore, _ *recordingIdentityRetirer) {
			s.projection.ID = "another-projection"
		}},
		{"unsettled external effect", func(t *testing.T, r *TaskReconciler, task *corev1alpha1.Task, _ *sessionCleanupReceiptControlStore, _ *recordingIdentityRetirer) {
			require.NoError(t, r.Create(context.Background(), &corev1alpha1.ExternalEffect{
				ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "pending-effect"},
				Spec:       corev1alpha1.ExternalEffectSpec{AggregateID: task.Status.Execution.RuntimeSessionUID},
			}))
		}},
		{"older attempt unsettled", func(t *testing.T, r *TaskReconciler, task *corev1alpha1.Task, _ *sessionCleanupReceiptControlStore, _ *recordingIdentityRetirer) {
			require.NoError(t, r.Create(context.Background(), &corev1alpha1.PromptAttempt{
				ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "older-attempt"},
				Spec:       corev1alpha1.PromptAttemptSpec{ID: "older-attempt", TaskUID: string(task.UID)},
				Status:     corev1alpha1.PromptAttemptStatus{ExecutionState: corev1alpha1.PromptAttemptExecutionState(store.PromptExecutionRunning)},
			}))
		}},
		{"publication unsettled", func(t *testing.T, r *TaskReconciler, task *corev1alpha1.Task, _ *sessionCleanupReceiptControlStore, _ *recordingIdentityRetirer) {
			require.NoError(t, r.Create(context.Background(), &corev1alpha1.Publication{
				ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "pending-publication"},
				Spec:       corev1alpha1.PublicationSpec{ID: "pending-publication", TaskUID: string(task.UID)},
				Status:     corev1alpha1.PublicationStatus{State: corev1alpha1.PublicationControlState(store.PublicationPublishing)},
			}))
		}},
		{"artifact retirement unavailable", func(_ *testing.T, _ *TaskReconciler, _ *corev1alpha1.Task, _ *sessionCleanupReceiptControlStore, r *recordingIdentityRetirer) {
			r.err = errors.New("retirement unavailable")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, task, workspace, control, retirer := deletingSessionWorkspaceFixture(t)
			attachment := workspace.Spec.Attachment.DeepCopy()
			tc.mutate(t, r, task, control, retirer)
			result, err := r.handleDeletion(context.Background(), task)
			require.True(t, err != nil || result.RequeueAfter > 0, "unsettled deletion must retry")
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(workspace), workspace))
			require.Equal(t, attachment, workspace.Spec.Attachment)
			require.Equal(t, workspacev1alpha1.ExecutionWorkspaceDesiredReady, workspace.Spec.DesiredState)
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(task), task))
			require.True(t, controllerutil.ContainsFinalizer(task, labels.TaskFinalizer))
			require.Empty(t, control.reclaimRequests)
		})
	}
}

func deletingSessionWorkspaceFixture(t *testing.T) (*TaskReconciler, *corev1alpha1.Task, *workspacev1alpha1.ExecutionWorkspace, *sessionCleanupReceiptControlStore, *recordingIdentityRetirer) {
	t.Helper()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	task := suspendableSessionTask()
	task.Finalizers = []string{labels.TaskFinalizer}
	r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
	task = bindSuspendableSessionTaskForSettlement(t, r, task)
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	require.NoError(t, err)
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", false, suspendTestSessionUID, resolved)
	require.NoError(t, err)
	plan := ACPRuntimePlan{PoolName: suspendTestRuntimePoolName, Workspace: binding}
	_, _, err = r.ensureACPClassWorkspace(ctx, task, plan)
	require.NoError(t, err)
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	workspaceKey := client.ObjectKey{Namespace: task.Namespace, Name: acpClassWorkspaceName(task, binding)}
	require.NoError(t, r.Get(ctx, workspaceKey, workspace))
	admitTestACPWorkspace(t, r, workspace)
	_, ready := attachTestACPWorkspace(t, r, task, plan, workspace.Name)
	require.True(t, ready)
	require.NoError(t, r.Get(ctx, workspaceKey, workspace))
	base := workspace.DeepCopy()
	workspace.Annotations[acpWorkspaceDurableSessionCommittedAnnotation] = "1"
	require.NoError(t, r.Patch(ctx, workspace, client.MergeFrom(base)))
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(task), task))
	task.Status.Phase = corev1alpha1.TaskPhaseCancelled
	task.Status.Execution = &corev1alpha1.TaskExecutionStatus{
		Attempt: 1, PromptID: "cancelled-prompt", State: corev1alpha1.TaskExecutionStateCancelled,
		Outcome:         corev1alpha1.TaskExecutionOutcomeCancelled,
		RuntimePoolName: plan.PoolName, RuntimeInstanceID: "runtime-instance",
		RuntimeSessionUID: suspendTestSessionUID, RuntimeSessionGeneration: 1,
	}
	task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{
		State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested,
	}
	require.NoError(t, r.Status().Update(ctx, task))
	require.NoError(t, r.Delete(ctx, task))
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(task), task))
	key := store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID}
	id, err := key.CanonicalID()
	require.NoError(t, err)
	attempt := &store.PromptAttempt{
		ID: id, Key: key, ExecutionState: store.PromptExecutionCancelled, DeliveryState: store.PromptDeliveryNotRequested,
		SessionUID: suspendTestSessionUID, SessionLeaseGeneration: 4, RuntimeInstanceID: task.Status.Execution.RuntimeInstanceID,
	}
	turnKey := store.SessionTurnKey{SessionUID: attempt.SessionUID, LeaseGeneration: attempt.SessionLeaseGeneration,
		TaskUID: string(task.UID), Attempt: 1, PromptID: key.PromptID}
	turnID, err := turnKey.CanonicalID()
	require.NoError(t, err)
	control := &sessionCleanupReceiptControlStore{
		promptAttemptReclaimControlStore: &promptAttemptReclaimControlStore{attempt: attempt,
			projection: &store.OutboxProjection{ID: store.CanonicalControlID("outbox", turnID, "TaskTerminalStatus"), State: store.OutboxProjectionDelivered}},
		turn: &store.SessionTurn{ID: turnID, Key: turnKey, PromptAttemptID: id, State: store.SessionTurnFinalized},
	}
	require.NoError(t, r.Create(ctx, &corev1alpha1.PromptAttempt{
		ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "cancelled-attempt"},
		Spec:       corev1alpha1.PromptAttemptSpec{ID: id, TaskUID: string(task.UID), Attempt: 1, PromptID: key.PromptID},
		Status: corev1alpha1.PromptAttemptStatus{
			ExecutionState: corev1alpha1.PromptAttemptExecutionState(attempt.ExecutionState),
			DeliveryState:  corev1alpha1.PromptAttemptDeliveryState(attempt.DeliveryState),
		},
	}))
	r.DurableControlStore = control
	retirer := &recordingIdentityRetirer{}
	r.ACPArtifactRetirer = retirer
	return r, task, workspace, control, retirer
}
