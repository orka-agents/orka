package controller

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	types "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func TestACPOutboxProjectorRepairsTerminalTaskStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "task", UID: types.UID("task-uid")},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Execution: &corev1alpha1.TaskExecutionStatus{
			Attempt: 1, PromptID: "prompt-1", State: corev1alpha1.TaskExecutionStateSettling,
			RuntimePoolName: "pool", RuntimePoolUID: "pool-uid", RuntimeInstanceID: "runtime-1",
			RuntimeSessionUID: "session-1", RuntimeSessionGeneration: 3,
			RequestDigest: "sha256:" + strings.Repeat("a", 64), ControllerEpoch: 7,
			ReadCredentialResourceVersion: "11", PublicationReadCredentialResourceVersion: "12",
			PublicationCredentialResourceVersion: "13", ForgeCredentialResourceVersion: "14",
		}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	controlStore := sqlite.NewStore(db, "test")
	epochs := NewControllerEpochManager(controlStore, "controller")
	epochCtx, cancelEpoch := context.WithCancel(context.Background())
	epochDone := make(chan error, 1)
	go func() { epochDone <- epochs.Start(epochCtx) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(taskTerminalProjection{
		Namespace: "default", Task: "task", TaskUID: "task-uid", Attempt: 1, Phase: corev1alpha1.TaskPhaseSucceeded,
		Execution: corev1alpha1.TaskExecutionStatus{Attempt: 1, State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded},
	})
	projection := &store.OutboxProjection{
		ID: store.CanonicalControlID("outbox", "turn", "TaskTerminalStatus"), AggregateKind: "SessionTurn", AggregateID: "turn",
		ProjectionKind: "TaskTerminalStatus", Payload: payload, PayloadDigest: canonicalACPPayloadDigest(payload), AvailableAt: time.Now().UTC(),
	}
	if _, err := controlStore.EnqueueOutboxProjection(ctx, projection, fence); err != nil {
		t.Fatal(err)
	}
	projector := &ACPOutboxProjector{Client: kubeClient, Store: controlStore, Epochs: epochs, WorkerID: "worker"}
	if err := projector.projectOnce(ctx); err != nil {
		t.Fatal(err)
	}
	updated := &corev1alpha1.Task{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "task"}, updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded || updated.Status.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeSucceeded {
		t.Fatalf("updated status = %#v", updated.Status)
	}
	gotExecution := updated.Status.Execution
	if gotExecution.RuntimePoolUID != "pool-uid" || gotExecution.RuntimeInstanceID != "runtime-1" ||
		gotExecution.RuntimeSessionUID != "session-1" || gotExecution.RuntimeSessionGeneration != 3 ||
		gotExecution.ControllerEpoch != 7 || gotExecution.RequestDigest == "" ||
		gotExecution.ReadCredentialResourceVersion != "11" || gotExecution.PublicationReadCredentialResourceVersion != "12" ||
		gotExecution.PublicationCredentialResourceVersion != "13" || gotExecution.ForgeCredentialResourceVersion != "14" {
		t.Fatalf("terminal projection erased execution identity: %#v", gotExecution)
	}
	stored, err := controlStore.GetOutboxProjection(ctx, projection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != store.OutboxProjectionDelivered || stored.DeliveryDigest == "" {
		t.Fatalf("stored projection = %#v", stored)
	}
	cancelEpoch()
	if err := <-epochDone; err != nil {
		t.Fatal(err)
	}
}

func TestACPOutboxProjectorAppliesHarnessV1ResultReference(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	bindingDigest := "sha256:" + strings.Repeat("a", 64)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "task", UID: types.UID("task-uid")},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseRunning,
			AgentExecutionBinding: &corev1alpha1.AgentExecutionBinding{
				ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV1,
				BindingDigest:   bindingDigest,
			},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	payload, err := json.Marshal(taskTerminalProjection{
		Namespace: "default", Task: "task", TaskUID: "task-uid", Attempt: 1,
		Phase: corev1alpha1.TaskPhaseSucceeded, BindingDigest: bindingDigest,
		HarnessRuntime: &corev1alpha1.HarnessRuntimeStatus{
			Attempt: 1, State: corev1alpha1.TaskExecutionStateSucceeded,
			Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
		},
		ResultRef: &corev1alpha1.ResultReference{Available: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	projection := store.OutboxProjection{
		ID: "session-turn-projection", AggregateKind: "SessionTurn", AggregateID: "turn",
		ProjectionKind: "TaskTerminalStatus", Payload: payload, PayloadDigest: canonicalACPPayloadDigest(payload),
	}
	projector := &ACPOutboxProjector{Client: kubeClient}
	if _, err := projector.deliver(context.Background(), projection); err != nil {
		t.Fatal(err)
	}
	updated := &corev1alpha1.Task{}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "task"}, updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.ResultRef == nil || !updated.Status.ResultRef.Available {
		t.Fatalf("Session outbox result reference = %#v, want available", updated.Status.ResultRef)
	}
}
