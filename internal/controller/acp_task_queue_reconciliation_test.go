package controller

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	types "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

//nolint:gocyclo // The table verifies first settlement, replay, and durable identity preservation together.
func TestQueueACPRuntimeTaskSettlesExistingAttemptBeforePermanentPlanningFailure(t *testing.T) {
	for _, state := range []struct {
		name       string
		taskState  corev1alpha1.TaskExecutionState
		storeState store.PromptExecutionState
	}{
		{name: "queued", taskState: corev1alpha1.TaskExecutionStateQueued, storeState: store.PromptExecutionQueued},
		{name: "reserved", taskState: corev1alpha1.TaskExecutionStateReserved, storeState: store.PromptExecutionReserved},
	} {
		t.Run(state.name, func(t *testing.T) {
			fixture := newACPQueuePlanningFailureFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			queued, attempt := fixture.queueValidTask(t, ctx)
			if state.storeState == store.PromptExecutionReserved {
				fence, err := fixture.epochs.CurrentFence(ctx)
				if err != nil {
					t.Fatal(err)
				}
				attempt = transitionPromptAttemptForImageRotationTest(
					t, ctx, fixture.controlStore, fence, attempt, store.PromptExecutionReserved, "reserve-before-configuration-drift",
				)
				base := queued.DeepCopy()
				queued.Status.Execution.State = state.taskState
				queued.Status.Execution.ControllerEpoch = fence.Epoch
				queued.Status.Execution.Reason = acpControllerRestartRecoveredReason
				queued.Status.Execution.Message = acpControllerRestartRecoveredMessage
				if err := fixture.kubeClient.Status().Patch(ctx, queued, client.MergeFrom(base)); err != nil {
					t.Fatal(err)
				}
				if err := fixture.kubeClient.Get(ctx, types.NamespacedName{Namespace: queued.Namespace, Name: queued.Name}, queued); err != nil {
					t.Fatal(err)
				}
			}

			beforeExecution := queued.Status.Execution.DeepCopy()
			beforeAttempt := *attempt
			invalidAgent := fixture.agent.DeepCopy()
			invalidAgent.Spec.Skills = []corev1alpha1.SkillReference{{Name: "added-after-queue"}}
			if _, err := fixture.reconciler.queueACPRuntimeTask(ctx, queued.DeepCopy(), invalidAgent); err != nil {
				t.Fatalf("queue after permanent Agent configuration failure: %v", err)
			}

			failed := &corev1alpha1.Task{}
			if err := fixture.kubeClient.Get(ctx, types.NamespacedName{Namespace: queued.Namespace, Name: queued.Name}, failed); err != nil {
				t.Fatal(err)
			}
			if failed.Status.Phase != corev1alpha1.TaskPhaseFailed || failed.Status.Execution == nil {
				t.Fatalf("planning failure status = %#v", failed.Status)
			}
			wantExecution := beforeExecution.DeepCopy()
			wantExecution.State = corev1alpha1.TaskExecutionStateFailed
			wantExecution.Outcome = corev1alpha1.TaskExecutionOutcomeFailed
			wantExecution.Reason = corev1alpha1.TaskExecutionReason("InvalidRuntimeProfile")
			wantExecution.Message = unsupportedACPAgentSkillsMessage
			wantExecution.LastTransitionTime = failed.Status.Execution.LastTransitionTime
			if !reflect.DeepEqual(failed.Status.Execution, wantExecution) {
				t.Fatalf("terminal projection did not preserve attempt identity/fences:\n got: %#v\nwant: %#v", failed.Status.Execution, wantExecution)
			}
			if failed.Status.Attempts != queued.Status.Attempts {
				t.Fatalf("Task attempts changed from %d to %d", queued.Status.Attempts, failed.Status.Attempts)
			}
			if failed.Status.Delivery == nil || failed.Status.Delivery.State != corev1alpha1.TaskDeliveryStateNotRequested ||
				failed.Status.Delivery.Outcome != corev1alpha1.TaskDeliveryOutcomeNotRequested {
				t.Fatalf("planning failure delivery status = %#v", failed.Status.Delivery)
			}

			terminalAttempt, err := fixture.controlStore.GetPromptAttempt(ctx, attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if terminalAttempt.ExecutionState != store.PromptExecutionFailed ||
				terminalAttempt.TerminalReason != "InvalidRuntimeProfile" ||
				terminalAttempt.ID != beforeAttempt.ID || terminalAttempt.Key != beforeAttempt.Key ||
				terminalAttempt.RequestDigest != beforeAttempt.RequestDigest ||
				!reflect.DeepEqual(terminalAttempt.CredentialBindings, beforeAttempt.CredentialBindings) ||
				terminalAttempt.RuntimeInstanceID != beforeAttempt.RuntimeInstanceID ||
				terminalAttempt.SessionUID != beforeAttempt.SessionUID ||
				terminalAttempt.SessionLeaseGeneration != beforeAttempt.SessionLeaseGeneration ||
				terminalAttempt.DeliveryState != beforeAttempt.DeliveryState ||
				terminalAttempt.Version != beforeAttempt.Version+1 {
				t.Fatalf("durable planning failure settlement did not preserve the attempt: before=%#v after=%#v", beforeAttempt, terminalAttempt)
			}

			if _, err := fixture.reconciler.queueACPRuntimeTask(ctx, failed.DeepCopy(), invalidAgent); err != nil {
				t.Fatalf("replayed planning failure reconciliation: %v", err)
			}
			projection, err := fixture.controlStore.GetOutboxProjection(
				ctx, standaloneTaskTerminalProjectionID(failed, failed.Status.Execution.Attempt),
			)
			if err != nil {
				t.Fatalf("get durable planning failure projection: %v", err)
			}
			if projection.State != store.OutboxProjectionPending || projection.AggregateID != string(failed.UID) {
				t.Fatalf("planning failure projection = %#v", projection)
			}

			replayedAttempt, err := fixture.controlStore.GetPromptAttempt(ctx, attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if replayedAttempt.Version != terminalAttempt.Version || replayedAttempt.ExecutionState != store.PromptExecutionFailed {
				t.Fatalf("planning failure replay changed durable attempt: before=%#v after=%#v", terminalAttempt, replayedAttempt)
			}
		})
	}
}

var errPlanningFailureTransition = errors.New("injected planning failure transition error")

type failingPlanningFailureTransitionStore struct {
	store.DurableControlStore
}

func (s *failingPlanningFailureTransitionStore) TransitionPromptAttemptExecution(
	ctx context.Context,
	transition store.PromptAttemptExecutionTransition,
) (*store.PromptAttempt, error) {
	if transition.NewState == store.PromptExecutionFailed && transition.TerminalReason == "InvalidRuntimeProfile" {
		return nil, errPlanningFailureTransition
	}
	return s.DurableControlStore.TransitionPromptAttemptExecution(ctx, transition)
}

func TestQueueACPRuntimeTaskDoesNotProjectPlanningFailureBeforeAttemptSettlement(t *testing.T) {
	fixture := newACPQueuePlanningFailureFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	queued, attempt := fixture.queueValidTask(t, ctx)
	fixture.reconciler.DurableControlStore = &failingPlanningFailureTransitionStore{DurableControlStore: fixture.controlStore}

	invalidAgent := fixture.agent.DeepCopy()
	invalidAgent.Spec.Skills = []corev1alpha1.SkillReference{{Name: "added-after-queue"}}
	_, err := fixture.reconciler.queueACPRuntimeTask(ctx, queued.DeepCopy(), invalidAgent)
	if !errors.Is(err, errPlanningFailureTransition) {
		t.Fatalf("queue error = %v, want injected durable transition error", err)
	}

	current := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(ctx, types.NamespacedName{Namespace: queued.Namespace, Name: queued.Name}, current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current.Status.Execution, queued.Status.Execution) || current.Status.Phase != queued.Status.Phase {
		t.Fatalf("Task was projected terminal before durable settlement: before=%#v after=%#v", queued.Status, current.Status)
	}
	persisted, err := fixture.controlStore.GetPromptAttempt(ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ExecutionState != store.PromptExecutionQueued || persisted.Version != attempt.Version {
		t.Fatalf("durable attempt changed after failed settlement: before=%#v after=%#v", attempt, persisted)
	}
}

type acpQueuePlanningFailureFixture struct {
	task         *corev1alpha1.Task
	agent        *corev1alpha1.Agent
	kubeClient   client.Client
	controlStore store.DurableControlStore
	epochs       *ControllerEpochManager
	reconciler   *TaskReconciler
}

func newACPQueuePlanningFailureFixture(t *testing.T) *acpQueuePlanningFailureFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "post-queue-configuration-failure",
			UID: types.UID("c72243b6-8b2f-4466-a321-644bff8b21c1"), Generation: 1,
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "inspect the repository",
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{},
			Workspace: &corev1alpha1.WorkspaceConfig{
				Intent: corev1alpha1.WorkspaceIntentRead, GitRepo: "https://github.com/orka-agents/orka.git",
			},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "codex",
			UID: types.UID("4fba4a25-3c83-465c-9105-ded0b9c8029a"), Generation: 1,
		},
		Spec: corev1alpha1.AgentSpec{
			Model:   &corev1alpha1.ModelConfig{Name: acpTestModel},
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex},
		},
	}
	runtimeImage := "docker.io/example/codex@sha256:" + strings.Repeat("a", 64)
	plan, err := PlanACPRuntime(task, agent, ACPRuntimeImages{Codex: runtimeImage})
	if err != nil {
		t.Fatal(err)
	}
	pool := runtimePoolForImageRotationTest(
		task.Namespace, types.UID("73ab20fa-2e2b-4f46-a410-ae4ed9c57751"), plan,
	)
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}, &corev1alpha1.RuntimePool{}).
		WithObjects(task, pool).
		Build()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close durable control database: %v", err)
		}
	})
	controlStore := sqlite.NewStore(db, "test")
	epochs := NewControllerEpochManager(controlStore, "controller-test")
	epochCtx, cancelEpoch := context.WithCancel(context.Background())
	epochDone := make(chan error, 1)
	go func() { epochDone <- epochs.Start(epochCtx) }()
	t.Cleanup(func() {
		cancelEpoch()
		if err := <-epochDone; err != nil {
			t.Errorf("controller epoch manager shutdown: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := epochs.CurrentFence(ctx); err != nil {
		t.Fatal(err)
	}
	reconciler := &TaskReconciler{
		Client: kubeClient, Scheme: scheme, DurableControlStore: controlStore,
		ControllerEpochManager: epochs, ACPRuntimeEnabled: true, ACPRuntimeNamespace: "orka-runtimes",
		ACPRuntimeImages: ACPRuntimeImages{Codex: runtimeImage},
	}
	return &acpQueuePlanningFailureFixture{
		task: task, agent: agent, kubeClient: kubeClient, controlStore: controlStore, epochs: epochs, reconciler: reconciler,
	}
}

func (f *acpQueuePlanningFailureFixture) queueValidTask(t *testing.T, ctx context.Context) (*corev1alpha1.Task, *store.PromptAttempt) {
	t.Helper()
	if _, err := f.reconciler.queueACPRuntimeTask(ctx, f.task.DeepCopy(), f.agent.DeepCopy()); err != nil {
		t.Fatalf("initial queue: %v", err)
	}
	queued := &corev1alpha1.Task{}
	if err := f.reconciler.Get(ctx, types.NamespacedName{Namespace: f.task.Namespace, Name: f.task.Name}, queued); err != nil {
		t.Fatal(err)
	}
	attemptID, err := promptAttemptIDFromTask(queued)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := f.controlStore.GetPromptAttempt(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	return queued, attempt
}

func TestFailACPPlanningTaskIsIdempotentBeforeDurableAttempt(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "pre-attempt-planning-failure",
			UID: types.UID("da90c496-1f82-44a4-a610-8b46c9a90f55"),
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(task).
		Build()
	reconciler := &TaskReconciler{Client: kubeClient, Scheme: scheme}
	reason := corev1alpha1.TaskExecutionReason("InvalidRuntimeProfile")
	message := "agent configuration is unsupported"

	for attempt := 1; attempt <= 2; attempt++ {
		current := &corev1alpha1.Task{}
		if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
			t.Fatal(err)
		}
		if _, err := reconciler.failACPPlanningTask(context.Background(), current, reason, message); err != nil {
			t.Fatalf("failACPPlanningTask() call %d error = %v", attempt, err)
		}
	}

	current := &corev1alpha1.Task{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != corev1alpha1.TaskPhaseFailed || current.Status.Execution == nil ||
		current.Status.Execution.State != corev1alpha1.TaskExecutionStateFailed ||
		current.Status.Execution.Reason != reason || current.Status.Execution.Attempt != 0 ||
		current.Status.Execution.PromptID != "" || current.Status.Execution.RequestDigest != "" {
		t.Fatalf("idempotent planning failure status = %#v", current.Status)
	}
}

var errPlanningFailureProjectionEnqueue = errors.New("injected planning failure projection enqueue error")

type failingPlanningFailureProjectionStore struct {
	store.DurableControlStore
	failed bool
}

func (s *failingPlanningFailureProjectionStore) EnqueueOutboxProjection(
	ctx context.Context,
	projection *store.OutboxProjection,
	fence store.ControllerEpochFence,
) (*store.OutboxProjection, error) {
	if !s.failed {
		s.failed = true
		return nil, errPlanningFailureProjectionEnqueue
	}
	return s.DurableControlStore.EnqueueOutboxProjection(ctx, projection, fence)
}

func TestQueueACPRuntimeTaskRecoversPlanningFailureAfterProjectionEnqueueError(t *testing.T) {
	fixture := newACPQueuePlanningFailureFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	queued, attempt := fixture.queueValidTask(t, ctx)
	failingStore := &failingPlanningFailureProjectionStore{DurableControlStore: fixture.controlStore}
	fixture.reconciler.DurableControlStore = failingStore

	invalidAgent := fixture.agent.DeepCopy()
	invalidAgent.Spec.Skills = []corev1alpha1.SkillReference{{Name: "added-after-queue"}}
	_, err := fixture.reconciler.queueACPRuntimeTask(ctx, queued.DeepCopy(), invalidAgent)
	if !errors.Is(err, errPlanningFailureProjectionEnqueue) {
		t.Fatalf("queue planning failure error = %v, want injected projection error", err)
	}
	settled, err := fixture.controlStore.GetPromptAttempt(ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.ExecutionState != store.PromptExecutionFailed || settled.OutcomeMarker != unsupportedACPAgentSkillsMessage {
		t.Fatalf("settled planning failure attempt = %#v", settled)
	}
	stillQueued := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(ctx, client.ObjectKeyFromObject(queued), stillQueued); err != nil {
		t.Fatal(err)
	}
	if stillQueued.Status.Execution == nil || stillQueued.Status.Execution.State != corev1alpha1.TaskExecutionStateQueued {
		t.Fatalf("Task projected terminal despite failed outbox enqueue: %#v", stillQueued.Status)
	}

	fixture.reconciler.DurableControlStore = fixture.controlStore
	if _, err := fixture.reconciler.queueACPRuntimeTask(ctx, stillQueued.DeepCopy(), fixture.agent.DeepCopy()); err != nil {
		t.Fatalf("recover durable planning failure after configuration correction: %v", err)
	}
	failed := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(ctx, client.ObjectKeyFromObject(queued), failed); err != nil {
		t.Fatal(err)
	}
	if failed.Status.Phase != corev1alpha1.TaskPhaseFailed || failed.Status.Execution == nil ||
		failed.Status.Execution.State != corev1alpha1.TaskExecutionStateFailed ||
		failed.Status.Execution.Reason != corev1alpha1.TaskExecutionReason("InvalidRuntimeProfile") {
		t.Fatalf("recovered planning failure Task status = %#v", failed.Status)
	}
	if _, err := fixture.controlStore.GetOutboxProjection(
		ctx, standaloneTaskTerminalProjectionID(failed, failed.Status.Execution.Attempt),
	); err != nil {
		t.Fatalf("get recovered planning failure projection: %v", err)
	}
}
