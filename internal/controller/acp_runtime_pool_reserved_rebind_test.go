package controller

import (
	"context"
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

//nolint:gocyclo // The table verifies rebind and no-rebind durable lifecycle invariants together.
func TestQueueACPRuntimeTaskRebindsOnlyQuiescentReservedAttemptAfterRuntimeImageRotation(t *testing.T) {
	for _, tc := range []struct {
		name              string
		activeReservation bool
		wantRebound       bool
	}{
		{name: "recovered without active reservation", wantRebound: true},
		{name: "active old-pool reservation blocks rebind", activeReservation: true, wantRebound: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default", Name: "reserved-image-rotation",
					UID: types.UID("91d91425-e317-4f86-9c46-ee63ae111c42"), Generation: 2,
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
					UID: types.UID("33220495-a4a7-4f3e-9e07-8f3901048de5"), Generation: 3,
				},
				Spec: corev1alpha1.AgentSpec{
					Model:   &corev1alpha1.ModelConfig{Name: acpTestModel},
					Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex},
				},
			}
			oldImage := "docker.io/example/codex@sha256:" + strings.Repeat("a", 64)
			newImage := "docker.io/example/codex@sha256:" + strings.Repeat("b", 64)
			oldPlan, err := PlanACPRuntime(task, agent, ACPRuntimeImages{Codex: oldImage})
			if err != nil {
				t.Fatal(err)
			}
			newPlan, err := PlanACPRuntime(task, agent, ACPRuntimeImages{Codex: newImage})
			if err != nil {
				t.Fatal(err)
			}
			oldPoolFixture := runtimePoolForImageRotationTest(
				task.Namespace, types.UID("101a20ba-2f11-44b5-8983-a912dc97800f"), oldPlan,
			)
			newPoolFixture := runtimePoolForImageRotationTest(
				task.Namespace, types.UID("1d863930-2412-4c27-9f6c-bdf55ca10ac0"), newPlan,
			)
			kubeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.Task{}, &corev1alpha1.RuntimePool{}).
				WithObjects(task, oldPoolFixture, newPoolFixture).
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
			fence, err := epochs.CurrentFence(ctx)
			if err != nil {
				t.Fatal(err)
			}
			reconciler := &TaskReconciler{
				Client: kubeClient, Scheme: scheme, DurableControlStore: controlStore,
				ControllerEpochManager: epochs, ACPRuntimeEnabled: true, ACPRuntimeNamespace: "orka-runtimes",
				ACPRuntimeImages: ACPRuntimeImages{Codex: oldImage},
			}
			if _, err := reconciler.queueACPRuntimeTask(ctx, task.DeepCopy(), agent); err != nil {
				t.Fatalf("initial queue: %v", err)
			}

			reserved := &corev1alpha1.Task{}
			if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, reserved); err != nil {
				t.Fatal(err)
			}
			attemptID, err := promptAttemptIDFromTask(reserved)
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := controlStore.GetPromptAttempt(ctx, attemptID)
			if err != nil {
				t.Fatal(err)
			}
			attempt = transitionPromptAttemptForImageRotationTest(
				t, ctx, controlStore, fence, attempt, store.PromptExecutionReserved, "recover-reserved-before-image-rotation",
			)
			base := reserved.DeepCopy()
			reserved.Status.Execution.State = corev1alpha1.TaskExecutionStateReserved
			reserved.Status.Execution.ControllerEpoch = fence.Epoch
			reserved.Status.Execution.Reason = acpControllerRestartRecoveredReason
			reserved.Status.Execution.Message = acpControllerRestartRecoveredMessage
			reserved.Status.Execution.LastTransitionTime = nowMeta()
			if err := kubeClient.Status().Patch(ctx, reserved, client.MergeFrom(base)); err != nil {
				t.Fatal(err)
			}
			if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, reserved); err != nil {
				t.Fatal(err)
			}
			beforeExecution := reserved.Status.Execution.DeepCopy()
			beforeAttempt := *attempt

			oldPool := &corev1alpha1.RuntimePool{}
			if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: oldPlan.PoolName}, oldPool); err != nil {
				t.Fatal(err)
			}
			oldPool.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
			oldPool.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			if tc.activeReservation {
				oldPool.Status.Capacity.Reservations = []corev1alpha1.RuntimePoolCapacityReservationStatus{{
					PoolUID: string(oldPool.UID), TaskUID: string(task.UID), Attempt: beforeExecution.Attempt,
					ControllerEpoch: fence.Epoch, RuntimeInstanceID: "old-pool-instance",
					ResidentSlots: 1, PromptSlots: 1, ReservedAt: metav1.Now(),
					ExpiresAt: metav1.NewTime(time.Now().UTC().Add(time.Minute)),
				}}
				updateRuntimePoolReservationCounters(&oldPool.Status.Capacity)
			}
			if err := kubeClient.Status().Update(ctx, oldPool); err != nil {
				t.Fatal(err)
			}

			reconciler.ACPRuntimeImages = ACPRuntimeImages{Codex: newImage}
			if _, err := reconciler.queueACPRuntimeTask(ctx, reserved.DeepCopy(), agent); err != nil {
				t.Fatalf("queue after image rotation: %v", err)
			}
			current := &corev1alpha1.Task{}
			if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Execution == nil || current.Status.Execution.State != corev1alpha1.TaskExecutionStateReserved {
				t.Fatalf("reserved execution status = %#v", current.Status.Execution)
			}
			if tc.wantRebound {
				if current.Status.Execution.RuntimePoolName != newPlan.PoolName || current.Status.Execution.RuntimePoolUID != string(newPoolFixture.UID) {
					t.Fatalf("reserved attempt was not rebound to replacement pool: %#v", current.Status.Execution)
				}
				wantExecution := beforeExecution.DeepCopy()
				wantExecution.RuntimePoolName = newPlan.PoolName
				wantExecution.RuntimePoolUID = string(newPoolFixture.UID)
				wantExecution.Reason = ""
				wantExecution.Message = ""
				wantExecution.LastTransitionTime = current.Status.Execution.LastTransitionTime
				if !reflect.DeepEqual(current.Status.Execution, wantExecution) {
					t.Fatalf("reserved rebind changed frozen attempt fields:\n got: %#v\nwant: %#v", current.Status.Execution, wantExecution)
				}
				if current.Labels[acpRuntimeTaskPoolLabel] != newPlan.PoolName {
					t.Fatalf("pool label = %q, want %q", current.Labels[acpRuntimeTaskPoolLabel], newPlan.PoolName)
				}
			} else if current.Status.Execution.RuntimePoolName != beforeExecution.RuntimePoolName ||
				current.Status.Execution.RuntimePoolUID != beforeExecution.RuntimePoolUID {
				t.Fatalf("active old-pool reservation was rebound: before=%#v after=%#v", beforeExecution, current.Status.Execution)
			}
			persisted, err := controlStore.GetPromptAttempt(ctx, attemptID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantRebound {
				wantOperationID := store.CanonicalControlID(
					"rebind-reserved-runtime-pool", beforeAttempt.ID, beforeExecution.RuntimePoolUID, string(newPoolFixture.UID),
				)
				if persisted.ExecutionState != store.PromptExecutionReserved || persisted.Version != beforeAttempt.Version+1 ||
					persisted.LastOperationID != wantOperationID || persisted.RequestDigest != beforeAttempt.RequestDigest ||
					persisted.RuntimeInstanceID != "" || persisted.SessionUID != "" || persisted.SessionLeaseGeneration != 0 {
					t.Fatalf("reserved rebind did not durably fence the old dispatcher: before=%#v after=%#v", beforeAttempt, persisted)
				}
			} else if !reflect.DeepEqual(persisted, &beforeAttempt) {
				t.Fatalf("blocked reserved rebind mutated durable attempt: before=%#v after=%#v", beforeAttempt, persisted)
			}
		})
	}
}

func TestRebindPreSubmissionACPRuntimeTaskRejectsPostSubmissionStates(t *testing.T) {
	pool := &corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{Name: "replacement", Namespace: "default", UID: "replacement-uid"}}
	for _, state := range []corev1alpha1.TaskExecutionState{
		corev1alpha1.TaskExecutionStateSubmitting,
		corev1alpha1.TaskExecutionStateAccepted,
		corev1alpha1.TaskExecutionStateRunning,
	} {
		t.Run(string(state), func(t *testing.T) {
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "post-submission", UID: "task-uid"},
				Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
					State: state, Attempt: 1, PromptID: "prompt-1",
					RequestDigest:   "sha256:" + strings.Repeat("a", 64),
					RuntimePoolName: "old", RuntimePoolUID: "old-uid",
				}},
			}
			rebound, err := (&TaskReconciler{}).rebindQueuedACPRuntimeTask(
				context.Background(), task, &corev1alpha1.Agent{}, ACPRuntimePlan{}, pool,
			)
			if err != nil {
				t.Fatal(err)
			}
			if rebound {
				t.Fatalf("post-submission state %s was rebound", state)
			}
		})
	}
}
