package controller

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	kubestore "github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

//nolint:gocyclo // The regression keeps rotation/deletion settlement invariants in one lifecycle.
func TestReserveTaskSettlesFrozenWorkspaceCredentialBlocked(t *testing.T) {
	for _, test := range []struct {
		name         string
		mutate       func(*corev1.Secret)
		deleteSecret bool
		added        bool
	}{
		{
			name: "rotated",
			mutate: func(secret *corev1.Secret) {
				secret.Data[defaultACPWorkspaceCredentialKey] = []byte("rotated-secret-value")
			},
		},
		{name: "deleted", deleteSecret: true},
		{name: "added after queue", added: true},
		{
			name: "missing key",
			mutate: func(secret *corev1.Secret) {
				delete(secret.Data, defaultACPWorkspaceCredentialKey)
			},
		},
		{
			name: "empty key",
			mutate: func(secret *corev1.Secret) {
				secret.Data[defaultACPWorkspaceCredentialKey] = []byte(" \n\t")
			},
		},
		{
			name: "oversized key",
			mutate: func(secret *corev1.Secret) {
				secret.Data[defaultACPWorkspaceCredentialKey] = []byte(strings.Repeat("x", maxACPWorkspaceCredentialBytes+1))
			},
		},
		{
			name: "invalid key",
			mutate: func(secret *corev1.Secret) {
				secret.Data[defaultACPWorkspaceCredentialKey] = []byte("invalid\x00secret-value")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := coordinationv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(
					&corev1alpha1.Task{}, &corev1alpha1.RuntimePool{},
					&corev1alpha1.ControllerEpoch{}, &corev1alpha1.PromptAttempt{},
				).
				Build()

			db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "credential-blocked.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close() //nolint:errcheck

			sqliteStore := sqlite.NewStore(db, "credential-blocked-test")
			controlStore, err := kubestore.NewComposite(kubeClient, "orka-system", sqliteStore)
			if err != nil {
				t.Fatal(err)
			}
			epochs, stopEpoch := startACPRecoveryEpochManager(t, ctx, controlStore, "credential-blocked-controller")
			defer stopEpoch()
			fence, err := epochs.CurrentFence(ctx)
			if err != nil {
				t.Fatal(err)
			}

			const (
				originalSecretValue = "original-secret-value"
				wantOperation       = "credential-blocked"
				wantExecutionReason = corev1alpha1.TaskExecutionReason("CredentialBlocked")
				wantMessage         = "workspace credential changed or became unavailable after queue; refusing to change frozen authority"
			)
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default", Name: "workspace-credential", UID: types.UID("workspace-credential-uid"),
				},
				Data: map[string][]byte{defaultACPWorkspaceCredentialKey: []byte(originalSecretValue)},
			}
			if err := kubeClient.Create(ctx, secret); err != nil {
				t.Fatal(err)
			}
			frozenSecret := &corev1.Secret{}
			if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(secret), frozenSecret); err != nil {
				t.Fatal(err)
			}
			frozenResourceVersion := frozenSecret.ResourceVersion
			if frozenResourceVersion == "" {
				t.Fatal("fake Kubernetes API did not assign a Secret resourceVersion")
			}
			if test.added {
				frozenResourceVersion = ""
			} else if test.deleteSecret {
				if err := kubeClient.Delete(ctx, frozenSecret); err != nil {
					t.Fatal(err)
				}
			} else {
				test.mutate(frozenSecret)
				if err := kubeClient.Update(ctx, frozenSecret); err != nil {
					t.Fatal(err)
				}
				if frozenSecret.ResourceVersion == frozenResourceVersion {
					t.Fatalf("Secret mutation did not advance resourceVersion %q", frozenResourceVersion)
				}
			}
			task := runtimePoolReservationTestTask("credential-blocked", "credential-blocked-uid", "pool-uid")
			task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{
				GitRepo: "https://github.com/orka-agents/orka.git",
				ReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{
					Name: "workspace-credential",
				},
			}
			task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "credential-blocked-session", Create: true, Append: true}
			task.Status.Execution.ControllerEpoch = fence.Epoch
			task.Status.Execution.ReadCredentialResourceVersion = frozenResourceVersion
			task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{
				State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested,
			}
			pool := &corev1alpha1.RuntimePool{
				ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "pool", UID: types.UID("pool-uid"), Generation: 1},
				Status: corev1alpha1.RuntimePoolStatus{
					Lifecycle:      corev1alpha1.RuntimePoolLifecycleServing,
					AdmissionState: corev1alpha1.RuntimePoolAdmissionAccepting,
					ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{
						RuntimeInstanceID: "pod-uid.boot-id", ControllerEpoch: fence.Epoch,
					},
					Capacity: corev1alpha1.RuntimePoolCapacityStatus{MaxResidentSessions: 1, MaxRunningPrompts: 1},
				},
			}

			taskForCreate := task.DeepCopy()
			taskForCreate.Status = corev1alpha1.TaskStatus{}
			if err := kubeClient.Create(ctx, taskForCreate); err != nil {
				t.Fatal(err)
			}
			currentTask := &corev1alpha1.Task{}
			if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(task), currentTask); err != nil {
				t.Fatal(err)
			}
			currentTask.Status = task.Status
			if err := kubeClient.Status().Update(ctx, currentTask); err != nil {
				t.Fatal(err)
			}
			poolForCreate := pool.DeepCopy()
			poolForCreate.Status = corev1alpha1.RuntimePoolStatus{}
			if err := kubeClient.Create(ctx, poolForCreate); err != nil {
				t.Fatal(err)
			}
			currentPool := &corev1alpha1.RuntimePool{}
			if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(pool), currentPool); err != nil {
				t.Fatal(err)
			}
			currentPool.Status = pool.Status
			if err := kubeClient.Status().Update(ctx, currentPool); err != nil {
				t.Fatal(err)
			}
			key := store.PromptAttemptKey{
				Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID,
			}
			bindings := []store.PromptCredentialBinding(nil)
			if !test.added {
				bindings = []store.PromptCredentialBinding{{
					Role: store.PromptCredentialSourceRead, Namespace: task.Namespace,
					SecretName: "workspace-credential", SecretKey: defaultACPWorkspaceCredentialKey,
					SecretUID: "workspace-credential-uid", ResourceVersion: frozenResourceVersion,
				}}
			}
			attempt, err := controlStore.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{
				Key: key, RequestDigest: task.Status.Execution.RequestDigest, CredentialBindings: bindings,
			}), fence)
			if err != nil {
				t.Fatal(err)
			}
			dispatcher := &ACPDispatcher{
				Client: kubeClient, APIReader: kubeClient, Store: controlStore, ResultStore: sqliteStore, Epochs: epochs,
			}

			reserved, _, err := dispatcher.reserveTask(ctx, task.DeepCopy())
			if err != nil {
				t.Fatalf("reserveTask() error = %v, want terminal credential-blocked settlement", err)
			}
			if reserved != nil {
				t.Fatalf("reserveTask() returned reserved Task %#v, want terminal credential-blocked settlement", reserved)
			}

			attempt, err = controlStore.GetPromptAttempt(ctx, attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.ExecutionState != store.PromptExecutionFailed || attempt.TerminalReason != wantOperation {
				t.Fatalf("PromptAttempt settlement = state %s reason %q, want %s/%q", attempt.ExecutionState, attempt.TerminalReason, store.PromptExecutionFailed, wantOperation)
			}
			wantOperationID := store.CanonicalControlID(
				wantOperation, attempt.ID, strconv.FormatInt(attempt.Version-1, 10), string(store.PromptCredentialSourceRead),
			)
			if attempt.LastOperationID != wantOperationID {
				t.Fatalf("credential-blocked operation ID = %q, want %q", attempt.LastOperationID, wantOperationID)
			}

			updatedTask := &corev1alpha1.Task{}
			if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(task), updatedTask); err != nil {
				t.Fatal(err)
			}
			if updatedTask.Status.Phase != corev1alpha1.TaskPhaseFailed || updatedTask.Status.Execution == nil ||
				updatedTask.Status.Execution.State != corev1alpha1.TaskExecutionStateFailed ||
				updatedTask.Status.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeFailed ||
				updatedTask.Status.Execution.Reason != wantExecutionReason ||
				updatedTask.Status.Execution.Message != wantMessage {
				t.Fatalf("Task settlement = %#v", updatedTask.Status)
			}
			if updatedTask.Status.Execution.ReadCredentialResourceVersion != frozenResourceVersion {
				t.Fatalf("frozen credential resourceVersion = %q, want %q", updatedTask.Status.Execution.ReadCredentialResourceVersion, frozenResourceVersion)
			}

			projection, err := controlStore.GetOutboxProjection(ctx, standaloneTaskTerminalProjectionID(task, 1))
			if err != nil {
				t.Fatalf("get terminal Task projection: %v", err)
			}
			var payload taskTerminalProjection
			if err := json.Unmarshal(projection.Payload, &payload); err != nil {
				t.Fatalf("decode terminal Task projection: %v", err)
			}
			if payload.Execution.Reason != wantExecutionReason || payload.Execution.Message != wantMessage {
				t.Fatalf("terminal Task projection = %#v", payload.Execution)
			}

			updatedPool := &corev1alpha1.RuntimePool{}
			if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(pool), updatedPool); err != nil {
				t.Fatal(err)
			}
			if len(updatedPool.Status.Capacity.Reservations) != 0 || updatedPool.Status.Capacity.ReservedSessions != 0 || updatedPool.Status.Capacity.ReservedPrompts != 0 {
				t.Fatalf("credential-blocked settlement leaked RuntimePool reservation: %#v", updatedPool.Status.Capacity)
			}

			attemptJSON, err := json.Marshal(attempt)
			if err != nil {
				t.Fatal(err)
			}
			for _, secretValue := range []string{originalSecretValue, "rotated-secret-value"} {
				if strings.Contains(string(attemptJSON), secretValue) || strings.Contains(string(projection.Payload), secretValue) {
					t.Fatalf("credential value %q leaked into durable settlement or Task projection", secretValue)
				}
			}
		})
	}
}
