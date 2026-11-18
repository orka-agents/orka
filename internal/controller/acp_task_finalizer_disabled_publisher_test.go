package controller

import (
	"context"
	"reflect"
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

func TestDeletingDeniedWriteTaskWithoutPublisher(t *testing.T) {
	t.Parallel()
	task, reconciler, control := deniedWriteTaskDeletionFixture()
	if _, err := reconciler.handleDeletion(context.Background(), task); err != nil {
		t.Fatalf("delete denied write Task: %v", err)
	}
	assertDeniedWriteReclamation(t, task, control, 1)
	var stored corev1alpha1.Task
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	if err := reconciler.Get(context.Background(), key, &stored); !apierrors.IsNotFound(err) {
		t.Fatalf("Task remains after durable NoAttempt reclamation: %v", err)
	}
}

func TestDeletingDeniedWriteTaskWithoutPublisherWaitsForDurableReclamation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	task, reconciler, control := deniedWriteTaskDeletionFixture()
	control.reclaimErr = store.ErrNotReady
	result, err := reconciler.handleDeletion(ctx, task)
	if err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("durable NotReady deletion = %v, %v; want deferred retry", result, err)
	}
	assertDeniedWriteReclamation(t, task, control, 1)
	var retained corev1alpha1.Task
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	if err := reconciler.Get(ctx, key, &retained); err != nil {
		t.Fatalf("get original Task after deferred reclamation: %v", err)
	}
	if retained.UID != task.UID || !controllerutil.ContainsFinalizer(&retained, labels.TaskFinalizer) {
		t.Fatal("original Task was released before durable reclamation")
	}
	if !reflect.DeepEqual(retained.Status.Execution, task.Status.Execution) {
		t.Fatal("deferred reclamation changed the denied Task execution identity")
	}

	control.reclaimErr = nil
	if _, err := reconciler.handleDeletion(ctx, &retained); err != nil {
		t.Fatalf("retry original Task deletion: %v", err)
	}
	assertDeniedWriteReclamation(t, task, control, 2)
	if err := reconciler.Get(ctx, key, &retained); !apierrors.IsNotFound(err) {
		t.Fatalf("Task remains after durable reclamation retry: %v", err)
	}
}

func deniedWriteTaskDeletionFixture() (*corev1alpha1.Task, *TaskReconciler, *promptAttemptReclaimControlStore) {
	task := artifactRetentionTask()
	task.Finalizers = []string{labels.TaskFinalizer}
	now := metav1.Now()
	task.DeletionTimestamp = &now
	task.Status.Phase = corev1alpha1.TaskPhaseFailed
	task.Status.Execution = &corev1alpha1.TaskExecutionStatus{
		State: corev1alpha1.TaskExecutionStateFailed, Outcome: corev1alpha1.TaskExecutionOutcomeFailed,
		Reason: "InvalidRuntimeProfile",
	}
	task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{
		State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested,
	}
	// Earlier artifact retirement does not waive deleting-Task reclamation.
	meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type: conditionTypeACPArtifactsRetired, Status: metav1.ConditionTrue,
		Reason: "ArtifactReferencesReleased", LastTransitionTime: now,
	})
	events := []string{}
	control := &promptAttemptReclaimControlStore{events: &events}
	reconciler := newUnitReconciler(newTestScheme(), task)
	reconciler.DurableControlStore = control
	reconciler.ControllerEpochManager = readyPromptAttemptReclaimEpochManager()
	return task, reconciler, control
}

func assertDeniedWriteReclamation(t *testing.T, task *corev1alpha1.Task, control *promptAttemptReclaimControlStore, count int) {
	t.Helper()
	if len(control.reclaimRequests) != count {
		t.Fatalf("reclamation count = %d, want %d", len(control.reclaimRequests), count)
	}
	want := store.ReclaimPromptAttemptsRequest{
		Namespace: task.Namespace, TaskName: task.Name, TaskUID: string(task.UID),
		Mode:                              store.PromptAttemptReclamationNoAttempt,
		RelatedExternalEffectAggregateIDs: []string{publicationIDForTask(task)},
		Fence: store.ControllerEpochFence{
			Name: store.DefaultControllerEpochName, Epoch: 7, HolderID: "reclaim-controller",
		},
	}
	for _, request := range control.reclaimRequests {
		if !reflect.DeepEqual(request, want) {
			t.Fatalf("NoAttempt reclamation identity = %#v, want %#v", request, want)
		}
	}
	wantEvents := make([]string, 0, count*2)
	for range count {
		wantEvents = append(wantEvents, "prepare-prompt-reclaim", "prompt-reclaim")
	}
	if !slices.Equal(*control.events, wantEvents) {
		t.Fatalf("reclamation order = %v, want %v", *control.events, wantEvents)
	}
}
