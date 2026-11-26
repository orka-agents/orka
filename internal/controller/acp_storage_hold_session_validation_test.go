package controller

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	kubestore "github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// Acknowledging the hold of a Session Task whose receipt was lost after
// workspace validation started settles the validation as a delivery conflict
// before recording the outcome as unknown, so the Session turn is finalized
// and later Tasks on the Session are not blocked.
//
//nolint:gocyclo // One end-to-end scenario across the takeover, the hold and its release.
func TestACPDispatcherSettlesWorkspaceValidationWhenASessionHoldIsAcknowledged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	sqliteStore := sealedStore(t, sqlite.NewStore(db, "test"))

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, corev1.AddToScheme, coordinationv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	taskUID := types.UID("55555555-5555-5555-5555-555555555555")
	promptID := "prompt-" + string(taskUID) + "-1"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "held-validation", UID: taskUID,
			Labels: map[string]string{acpRuntimeTaskPoolLabel: "pool"},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "validate this workspace",
			SessionRef: &corev1alpha1.SessionReference{Name: "held-validation", Create: true, Append: true},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseRunning, Attempts: 1,
			AgentExecutionBinding: testACPExecuteBindingForDispatcher(),
		},
	}
	rawClient := withControllerEpochLeaseUIDs(t, fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&corev1alpha1.Task{}, &corev1alpha1.ControllerEpoch{}, &corev1alpha1.PromptAttempt{},
			&corev1alpha1.RuntimeSessionControl{}, &corev1alpha1.BranchClaim{}, &corev1alpha1.Publication{},
			&corev1alpha1.ExternalEffect{},
		).
		WithObjects(task).
		Build())
	controlStore, err := kubestore.NewComposite(rawClient, "orka-system", sqliteStore)
	if err != nil {
		t.Fatal(err)
	}
	oldEpochs, stopOldEpoch := startACPRecoveryEpochManager(t, ctx, controlStore, "controller-old", sqliteStore)
	oldFence, err := oldEpochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The replaced controller ran the prompt, stored its receipt, moved the
	// attempt to Settling and started workspace validation.
	continuity := newACPRecoveryContinuity(t, controlStore, sqliteStore, "held-validation-uid")
	control, err := continuity.EnsureSession(ctx, ACPEnsureSessionRequest{
		Namespace: "default", SessionName: "held-validation", SessionType: "task",
		ExpectedSessionUID: "held-validation-uid", Fence: oldFence, CreatedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	attemptKey := store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(taskUID), Attempt: 1, PromptID: promptID}
	attempt, err := controlStore.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{
		Key: attemptKey, RequestDigest: testControlDigestForDispatcher("held-validation"),
	}), oldFence)
	if err != nil {
		t.Fatal(err)
	}
	attempt = transitionACPRecoveryAttempt(t, ctx, controlStore, oldFence, attempt, store.PromptExecutionReserved, nil)
	lease, err := continuity.AcquireMutationLease(ctx, ACPAcquireSessionLeaseRequest{
		Session: *control, Fence: oldFence, TaskUID: string(taskUID), Attempt: 1, PromptID: promptID,
		PromptRequestDigest: attempt.RequestDigest, AcquiredAt: time.Date(2026, 7, 25, 12, 1, 0, 0, time.UTC),
		NamespaceUID: "default-namespace-uid", RuntimeIdentity: "codex",
		ConfigDigest: testControlDigestForDispatcher("held-validation-profile"),
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt = transitionACPRecoveryAttempt(t, ctx, controlStore, oldFence, attempt, store.PromptExecutionSessionStarting, lease)
	turn, err := continuity.OpenTurn(ctx, ACPOpenSessionTurnRequest{
		Lease: *lease, Fence: oldFence, PromptAttemptID: attempt.ID,
		PromptRequestDigest: attempt.RequestDigest, UserPrompt: task.Spec.Prompt,
		OpenedAt: time.Date(2026, 7, 25, 12, 2, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []store.PromptExecutionState{
		store.PromptExecutionPlanned, store.PromptExecutionSubmitting, store.PromptExecutionAccepted, store.PromptExecutionRunning,
	} {
		attempt = transitionACPRecoveryAttempt(t, ctx, controlStore, oldFence, attempt, state, nil)
	}
	task.Status.Execution = &corev1alpha1.TaskExecutionStatus{
		State: corev1alpha1.TaskExecutionStateSettling, Attempt: 1, PromptID: promptID, RuntimePoolName: "pool",
		RuntimeInstanceID: "runtime-old", RuntimeSessionUID: control.SessionUID,
		RuntimeSessionGeneration: lease.Key.LeaseGeneration, RuntimeSessionSupervisorBootID: "supervisor-boot",
		ControllerEpoch: oldFence.Epoch, RequestDigest: attempt.RequestDigest,
	}
	task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateValidating}
	if err := rawClient.Status().Update(ctx, task); err != nil {
		t.Fatal(err)
	}
	replaced := &ACPDispatcher{
		Client: rawClient, APIReader: rawClient, Store: controlStore, ResultStore: sqliteStore,
		Epochs: oldEpochs, Sessions: continuity,
	}
	if err := replaced.transitionAttemptToSettlingWithResult(ctx, task, attempt.ID, oldFence, []byte(settledTestResult)); err != nil {
		t.Fatal(err)
	}
	if err := replaced.transitionDelivery(
		ctx, attempt.ID, oldFence, store.PromptDeliveryNotRequested, store.PromptDeliveryValidating, "validate-workspace", "",
	); err != nil {
		t.Fatal(err)
	}
	// The store loses the receipt and the result.
	if err := sqliteStore.DeleteResult(ctx, task.Namespace, task.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := sqliteStore.GetPromptResultReceipt(ctx, attempt.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("result receipt after loss: %v", err)
	}
	stopOldEpoch()

	restartedStore, err := kubestore.NewComposite(rawClient, "orka-system", sqliteStore)
	if err != nil {
		t.Fatal(err)
	}
	newEpochs, stopNewEpoch := startACPRecoveryEpochManager(t, ctx, restartedStore, "controller-new", sqliteStore)
	defer stopNewEpoch()
	dispatcher := &ACPDispatcher{
		Client: rawClient, APIReader: rawClient, Store: restartedStore, ResultStore: sqliteStore, EventStore: sqliteStore,
		Epochs: newEpochs, Sessions: newACPRecoveryContinuity(t, restartedStore, sqliteStore, control.SessionUID),
	}
	hold := taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord, key: attempt.ID,
	}
	if err := dispatcher.recoverStaleAttempts(ctx); !errors.Is(err, store.ErrStorageRecoveryRequired) {
		t.Fatalf("recovery error = %v, want a storage recovery hold", err)
	}

	held := &corev1alpha1.Task{}
	if err := rawClient.Get(ctx, clientObjectKey(task), held); err != nil {
		t.Fatal(err)
	}
	held.Annotations = map[string]string{corev1alpha1.StorageRecoveryAcknowledgedAnnotation: hold.id()}
	if err := rawClient.Update(ctx, held); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.recoverStaleAttempts(ctx); err != nil {
		t.Fatalf("recovery after acknowledgement: %v", err)
	}

	recovered, err := restartedStore.GetPromptAttempt(ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ExecutionState != store.PromptExecutionOutcomeUnknown || recovered.DeliveryState != store.PromptDeliveryConflict {
		t.Fatalf("acknowledged attempt = %s/%s, want %s/%s", recovered.ExecutionState, recovered.DeliveryState,
			store.PromptExecutionOutcomeUnknown, store.PromptDeliveryConflict)
	}
	finalized, err := sqliteStore.GetSessionTurn(ctx, turn.Turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finalized.State != store.SessionTurnFinalized {
		t.Fatalf("Session turn state = %s, want %s", finalized.State, store.SessionTurnFinalized)
	}
	projection, err := sqliteStore.GetOutboxProjection(ctx, finalized.ProjectionID)
	if err != nil {
		t.Fatal(err)
	}
	var payload taskTerminalProjection
	if err := json.Unmarshal(projection.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Execution.State != corev1alpha1.TaskExecutionStateOutcomeUnknown || payload.Delivery == nil ||
		payload.Delivery.State != corev1alpha1.TaskDeliveryStateDeliveryConflict {
		t.Fatalf("Session turn projection = execution %#v delivery %#v", payload.Execution, payload.Delivery)
	}
	updated := &corev1alpha1.Task{}
	if err := rawClient.Get(ctx, clientObjectKey(task), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Execution == nil || updated.Status.Execution.State != corev1alpha1.TaskExecutionStateOutcomeUnknown ||
		updated.Status.Delivery == nil || updated.Status.Delivery.State != corev1alpha1.TaskDeliveryStateDeliveryConflict {
		t.Fatalf("acknowledged Task status = execution %#v delivery %#v", updated.Status.Execution, updated.Status.Delivery)
	}
}
