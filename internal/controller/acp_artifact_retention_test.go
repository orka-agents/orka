package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/internal/artifactcap"
	"github.com/orka-agents/orka/internal/store"
)

func TestRetireACPArtifactIdentitiesWaitsForAllAttemptsAndPublications(t *testing.T) {
	t.Parallel()
	scheme := newTestScheme()
	task := artifactRetentionTask()
	currentPublicationID := publicationIDForTask(task)
	oldAttempt := &corev1alpha1.PromptAttempt{
		ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "old-attempt"},
		Spec:       corev1alpha1.PromptAttemptSpec{ID: "old-attempt", TaskUID: string(task.UID), Attempt: 1, PromptID: "old-prompt"},
		Status: corev1alpha1.PromptAttemptStatus{
			ExecutionState: corev1alpha1.PromptAttemptExecutionState(store.PromptExecutionRunning),
			DeliveryState:  corev1alpha1.PromptAttemptDeliveryState(store.PromptDeliveryNotRequested),
		},
	}
	currentAttempt := &corev1alpha1.PromptAttempt{
		ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "current-attempt"},
		Spec:       corev1alpha1.PromptAttemptSpec{ID: "current-attempt", TaskUID: string(task.UID), Attempt: 2, PromptID: task.Status.Execution.PromptID},
		Status: corev1alpha1.PromptAttemptStatus{
			ExecutionState: corev1alpha1.PromptAttemptExecutionState(store.PromptExecutionSucceeded),
			DeliveryState:  corev1alpha1.PromptAttemptDeliveryState(store.PromptDeliveryVerifiedExact),
		},
	}
	oldPublication := &corev1alpha1.Publication{
		ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "old-publication"},
		Spec:       corev1alpha1.PublicationSpec{ID: "publication-old", TaskUID: string(task.UID)},
		Status:     corev1alpha1.PublicationStatus{State: corev1alpha1.PublicationControlState(store.PublicationPublishing)},
	}
	currentPublication := &corev1alpha1.Publication{
		ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "current-publication"},
		Spec:       corev1alpha1.PublicationSpec{ID: currentPublicationID, TaskUID: string(task.UID)},
		Status:     corev1alpha1.PublicationStatus{State: corev1alpha1.PublicationControlState(store.PublicationVerifiedExact)},
	}
	reconciler := newUnitReconciler(scheme, task, oldAttempt, currentAttempt, oldPublication, currentPublication)
	reconciler.DurableControlStore = artifactRetentionControlStore{}
	retirer := &recordingIdentityRetirer{}
	reconciler.ACPArtifactRetirer = retirer

	ready, err := reconciler.retireACPArtifactIdentities(context.Background(), task)
	if err != nil || ready || len(retirer.calls) != 0 {
		t.Fatalf("retirement with active attempt = ready:%v calls:%v err:%v", ready, retirer.calls, err)
	}
	setPromptAttemptTerminal(t, reconciler.Client, oldAttempt)
	ready, err = reconciler.retireACPArtifactIdentities(context.Background(), task)
	if err != nil || ready || len(retirer.calls) != 0 {
		t.Fatalf("retirement with active publication = ready:%v calls:%v err:%v", ready, retirer.calls, err)
	}
	setPublicationTerminal(t, reconciler.Client, oldPublication)
	ready, err = reconciler.retireACPArtifactIdentities(context.Background(), task)
	if err != nil || !ready {
		t.Fatalf("terminal retirement = ready:%v err:%v", ready, err)
	}
	want := []artifactcap.Identity{
		{Namespace: task.Namespace, TaskID: string(task.UID)},
		{Namespace: task.Namespace, PublicationID: currentPublicationID},
		{Namespace: task.Namespace, PublicationID: "publication-old"},
	}
	if len(retirer.calls) != 1 || !identitySlicesEqual(retirer.calls[0], want) {
		t.Fatalf("retired identities = %#v, want %#v", retirer.calls, want)
	}
	stored := &corev1alpha1.Task{}
	if err := reconciler.Get(context.Background(), client.ObjectKeyFromObject(task), stored); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(stored.Status.Conditions, conditionTypeACPArtifactsRetired) {
		t.Fatalf("Task conditions = %#v, want %s=True", stored.Status.Conditions, conditionTypeACPArtifactsRetired)
	}
}

func TestHandleCompletedRetiresArtifactsAfterDurableSettlement(t *testing.T) {
	t.Parallel()
	task := artifactRetentionTask()
	reconciler, retirer := newSettledArtifactRetentionReconciler(t, task)

	result, err := reconciler.handleCompleted(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("handleCompleted() requeue = %v, want none", result.RequeueAfter)
	}
	if len(retirer.calls) != 1 {
		t.Fatalf("retirement calls = %#v, want one", retirer.calls)
	}
}

func TestHandleCompletedSettlesWorkspaceOnlyAfterArtifactRetirement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	task := artifactRetentionTask()
	task.Status.Execution.RuntimePoolName = "acp-ws-terminal-order"
	workspace := acpAdapterWorkspace(t, "acp-ws-terminal-order")
	workspace.Namespace = task.Namespace
	workspace.Name = "acp-ws-terminal-order"
	workspace.UID = types.UID("acp-ws-terminal-order-uid")
	workspace.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: corev1alpha1.GroupVersion.String(),
		Kind:       taskResourceKind,
		Name:       task.Name,
		UID:        task.UID,
	}}
	workspace.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachDelete)
	task.Labels = map[string]string{acpExecutionWorkspaceLinkLabel: workspace.Name}
	task.Annotations = map[string]string{acpExecutionWorkspaceUIDAnnotation: string(workspace.UID)}

	reconciler, retirer := newSettledArtifactRetentionReconciler(t, task)
	reconciler.WorkspaceSettlementProtected = true
	if err := reconciler.Create(ctx, workspace); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	retirer.err = errors.New("artifact retirement blocked")
	result, err := reconciler.handleCompleted(ctx, task)
	if err != nil || result.RequeueAfter != 30*time.Second {
		t.Fatalf("blocked retirement result = (%#v, %v), want 30s requeue", result, err)
	}
	currentWorkspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(workspace), currentWorkspace); err != nil {
		t.Fatalf("workspace was settled before artifact retirement: %v", err)
	}

	retirer.err = nil
	currentTask := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), currentTask); err != nil {
		t.Fatalf("reload task: %v", err)
	}
	result, err = reconciler.handleCompleted(ctx, currentTask)
	if err != nil || !result.IsZero() {
		t.Fatalf("completed settlement result = (%#v, %v), want zero", result, err)
	}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(workspace), currentWorkspace); !apierrors.IsNotFound(err) {
		t.Fatalf("workspace survived post-retirement Delete settlement: %v", err)
	}
}

func TestHandleCompletedRetiresArtifactsBeforeRetryingFailedWebhook(t *testing.T) {
	t.Parallel()
	var webhookAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		webhookAttempts.Add(1)
		http.Error(w, "retry", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	task := artifactRetentionTask()
	task.Spec.WebhookURL = server.URL
	reconciler, retirer := newSettledArtifactRetentionReconciler(t, task)
	notifier := NewWebhookNotifier()
	notifier.skipURLValidation = true
	reconciler.WebhookNotifier = notifier

	for wantWebhookAttempts := int32(1); wantWebhookAttempts <= 2; wantWebhookAttempts++ {
		current := &corev1alpha1.Task{}
		if err := reconciler.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
			t.Fatal(err)
		}
		result, err := reconciler.handleCompleted(context.Background(), current)
		if err != nil {
			t.Fatal(err)
		}
		if result.RequeueAfter != 30*time.Second {
			t.Fatalf("handleCompleted() requeue = %v, want 30s", result.RequeueAfter)
		}
		if got := webhookAttempts.Load(); got != wantWebhookAttempts {
			t.Fatalf("webhook attempts = %d, want %d", got, wantWebhookAttempts)
		}
		if len(retirer.calls) != 1 {
			t.Fatalf("retirement calls after webhook attempt %d = %#v, want exactly one", wantWebhookAttempts, retirer.calls)
		}
		if !meta.IsStatusConditionTrue(current.Status.Conditions, conditionTypeACPArtifactsRetired) {
			t.Fatalf("Task conditions after webhook attempt %d = %#v, want %s=True", wantWebhookAttempts, current.Status.Conditions, conditionTypeACPArtifactsRetired)
		}
	}
}

func newSettledArtifactRetentionReconciler(
	t *testing.T,
	task *corev1alpha1.Task,
) (*TaskReconciler, *recordingIdentityRetirer) {
	t.Helper()
	scheme := newTestScheme()
	attemptKey := store.PromptAttemptKey{
		Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: int64(task.Status.Execution.Attempt), PromptID: task.Status.Execution.PromptID,
	}
	attemptID, err := attemptKey.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	attempt := &store.PromptAttempt{
		ID: attemptID, Key: attemptKey, ExecutionState: store.PromptExecutionSucceeded, DeliveryState: store.PromptDeliveryVerifiedExact,
	}
	publication := &store.Publication{ID: publicationIDForTask(task), State: store.PublicationVerifiedExact}
	projection := &store.OutboxProjection{ID: standaloneTaskTerminalProjectionID(task, task.Status.Execution.Attempt), State: store.OutboxProjectionDelivered}
	attemptObject := &corev1alpha1.PromptAttempt{
		ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "settled-attempt"},
		Spec: corev1alpha1.PromptAttemptSpec{
			ID: attemptID, TaskUID: string(task.UID), Attempt: int64(task.Status.Execution.Attempt), PromptID: task.Status.Execution.PromptID,
		},
		Status: corev1alpha1.PromptAttemptStatus{
			ExecutionState: corev1alpha1.PromptAttemptExecutionState(store.PromptExecutionSucceeded),
			DeliveryState:  corev1alpha1.PromptAttemptDeliveryState(store.PromptDeliveryVerifiedExact),
		},
	}
	publicationObject := &corev1alpha1.Publication{
		ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "settled-publication"},
		Spec:       corev1alpha1.PublicationSpec{ID: publication.ID, TaskUID: string(task.UID)},
		Status:     corev1alpha1.PublicationStatus{State: corev1alpha1.PublicationControlState(store.PublicationVerifiedExact)},
	}
	reconciler := newUnitReconciler(scheme, task, attemptObject, publicationObject)
	reconciler.DurableControlStore = artifactRetentionControlStore{attempt: attempt, publication: publication, projection: projection}
	retirer := &recordingIdentityRetirer{}
	reconciler.ACPArtifactRetirer = retirer
	return reconciler, retirer
}

func artifactRetentionTask() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "artifact-retention", UID: types.UID("11111111-2222-3333-4444-555555555555")},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Workspace: &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
			Execution: &corev1alpha1.TaskExecutionStatus{
				Attempt: 2, PromptID: "prompt-current", State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
			},
			Delivery: &corev1alpha1.TaskDeliveryStatus{
				State: corev1alpha1.TaskDeliveryStateVerifiedExact, Outcome: corev1alpha1.TaskDeliveryOutcomeVerifiedExact,
			},
		},
	}
}

type artifactRetentionControlStore struct {
	store.DurableControlStore
	attempt     *store.PromptAttempt
	publication *store.Publication
	projection  *store.OutboxProjection
}

func (s artifactRetentionControlStore) GetPromptAttempt(context.Context, string) (*store.PromptAttempt, error) {
	if s.attempt == nil {
		return nil, store.ErrNotFound
	}
	copy := *s.attempt
	return &copy, nil
}

func (s artifactRetentionControlStore) GetPublication(context.Context, string) (*store.Publication, error) {
	if s.publication == nil {
		return nil, store.ErrNotFound
	}
	copy := *s.publication
	return &copy, nil
}

func (s artifactRetentionControlStore) GetOutboxProjection(context.Context, string) (*store.OutboxProjection, error) {
	if s.projection == nil {
		return nil, store.ErrNotFound
	}
	copy := *s.projection
	return &copy, nil
}

type recordingIdentityRetirer struct {
	calls [][]artifactcap.Identity
	err   error
}

func (r *recordingIdentityRetirer) Retire(_ context.Context, identities ...artifactcap.Identity) error {
	if r.err != nil {
		return r.err
	}
	r.calls = append(r.calls, append([]artifactcap.Identity(nil), identities...))
	return nil
}

func setPromptAttemptTerminal(t *testing.T, kubeClient client.Client, object *corev1alpha1.PromptAttempt) {
	t.Helper()
	current := &corev1alpha1.PromptAttempt{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(object), current); err != nil {
		t.Fatal(err)
	}
	current.Status.ExecutionState = corev1alpha1.PromptAttemptExecutionState(store.PromptExecutionFailed)
	current.Status.DeliveryState = corev1alpha1.PromptAttemptDeliveryState(store.PromptDeliveryNoChange)
	if err := kubeClient.Status().Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
}

func setPublicationTerminal(t *testing.T, kubeClient client.Client, object *corev1alpha1.Publication) {
	t.Helper()
	current := &corev1alpha1.Publication{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(object), current); err != nil {
		t.Fatal(err)
	}
	current.Status.State = corev1alpha1.PublicationControlState(store.PublicationDeliveryConflict)
	if err := kubeClient.Status().Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
}

func identitySlicesEqual(left, right []artifactcap.Identity) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
