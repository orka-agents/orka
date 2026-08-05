package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	kubestore "github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

const acpTestModel = "gpt-test"

type missingRecoveryPromptAttemptStore struct {
	store.DurableControlStore
	calls atomic.Int32
}

func (s *missingRecoveryPromptAttemptStore) GetPromptAttempt(context.Context, string) (*store.PromptAttempt, error) {
	s.calls.Add(1)
	return nil, store.ErrNotFound
}

func TestACPDispatcherRecoverySkipsTaskDeletedAfterCachedList(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionPlanned)
	defer fixture.close(t)

	missingStore := &missingRecoveryPromptAttemptStore{DurableControlStore: fixture.controlStore}
	fixture.dispatcher.Store = missingStore
	getCalls := 0
	watchClient, ok := fixture.kubeClient.(client.WithWatch)
	if !ok {
		t.Fatal("recovery fake client does not implement client.WithWatch")
	}
	fixture.dispatcher.APIReader = interceptor.NewClient(watchClient, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isTask := obj.(*corev1alpha1.Task); !isTask {
				return c.Get(ctx, key, obj, opts...)
			}
			getCalls++
			if getCalls == 1 {
				return c.Get(ctx, key, obj, opts...)
			}
			return apierrors.NewNotFound(
				schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "tasks"},
				key.Name,
			)
		},
	})

	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recoverStaleAttempts() error = %v, want deleted Task race ignored", err)
	}
	if getCalls != 2 {
		t.Fatalf("Task reads = %d, want initial refresh plus post-not-found recheck", getCalls)
	}
	if missingStore.calls.Load() != 1 {
		t.Fatalf("GetPromptAttempt calls = %d, want 1", missingStore.calls.Load())
	}
}

func TestACPDispatcherRecoveryFailsClosedWhenAttemptMissingForLiveTask(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionPlanned)
	defer fixture.close(t)

	missingStore := &missingRecoveryPromptAttemptStore{DurableControlStore: fixture.controlStore}
	fixture.dispatcher.Store = missingStore

	err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx)
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("recoverStaleAttempts() error = %v, want ErrNotFound for a still-live Task", err)
	}
	if missingStore.calls.Load() != 1 {
		t.Fatalf("GetPromptAttempt calls = %d, want 1", missingStore.calls.Load())
	}
}

func TestACPDispatcherRecoversPreSubmissionAttemptUnderNewEpoch(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionPlanned)
	defer fixture.close(t)

	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionReserved || attempt.ControllerEpoch != fixture.fence.Epoch || attempt.RuntimeInstanceID != "" {
		t.Fatalf("recovered attempt = %#v", attempt)
	}
	task := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, types.NamespacedName{Namespace: "default", Name: "task"}, task); err != nil {
		t.Fatal(err)
	}
	if task.Status.Execution == nil || task.Status.Execution.State != corev1alpha1.TaskExecutionStateReserved || task.Status.Execution.ControllerEpoch != fixture.fence.Epoch {
		t.Fatalf("recovered task = %#v", task.Status.Execution)
	}
}

func TestACPDispatcherMakesAcceptedOldEpochAttemptOutcomeUnknown(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionAccepted)
	defer fixture.close(t)

	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionOutcomeUnknown || attempt.OutcomeMarker == "" {
		t.Fatalf("recovered attempt = %#v", attempt)
	}
	task := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, types.NamespacedName{Namespace: "default", Name: "task"}, task); err != nil {
		t.Fatal(err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseFailed || task.Status.Execution == nil || task.Status.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeOutcomeUnknown {
		t.Fatalf("recovered task status = %#v", task.Status)
	}
}

//nolint:gocyclo // This intentionally exercises the full production recovery boundary in one scenario.
func TestACPDispatcherRecoveryReusesExistingTaskScopedTerminalProjection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "task-projection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	controlStore := sqlite.NewStore(db, "test")
	oldEpochs, stopOld := startACPRecoveryEpochManager(t, ctx, controlStore, "controller-old-task-projection")
	oldFence, err := oldEpochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}

	uid := types.UID("77777777-7777-7777-7777-777777777777")
	promptID := "prompt-" + string(uid) + "-1"
	fixedTransition := metav1.NewTime(time.Date(2026, 7, 25, 12, 30, 0, 0, time.UTC))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "task-projection", UID: uid},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "test"},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded, Attempts: 1, Message: "ACP task completed",
			Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
				Attempt: 1, PromptID: promptID, RuntimePoolName: "pool", RuntimePoolUID: "pool-uid", ControllerEpoch: oldFence.Epoch,
				RequestDigest: testControlDigestForDispatcher("task-projection-request"), LastTransitionTime: &fixedTransition,
			},
			Delivery: &corev1alpha1.TaskDeliveryStatus{
				State: corev1alpha1.TaskDeliveryStateReadValidated, Outcome: corev1alpha1.TaskDeliveryOutcomeReadValidated,
				StartingSHA: "740310bf8ecfbce4963628a51b6a11e26d7ee7be", LastTransitionTime: &fixedTransition,
			},
		},
	}
	key := store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: promptID}
	attempt, err := controlStore.CreatePromptAttempt(ctx, &store.PromptAttempt{
		Key: key, RequestDigest: task.Status.Execution.RequestDigest,
	}, oldFence)
	if err != nil {
		t.Fatal(err)
	}
	attempt = completeACPAttemptExecutionForTest(t, controlStore, oldFence, attempt, false)
	for _, next := range []store.PromptDeliveryState{store.PromptDeliveryValidating, store.PromptDeliveryReadValidated} {
		op := "task-projection-delivery-" + string(next)
		attempt, err = controlStore.TransitionPromptAttemptDelivery(ctx, store.PromptAttemptDeliveryTransition{
			ID: attempt.ID, Fence: oldFence, ExpectedVersion: attempt.Version, ExpectedState: attempt.DeliveryState,
			NewState: next, OperationID: op, OperationDigest: testControlDigestForDispatcher(op), UpdatedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	oldDispatcher := &ACPDispatcher{Store: controlStore, Epochs: oldEpochs}
	payload := taskTerminalProjection{
		Namespace: task.Namespace, Task: task.Name, TaskUID: string(task.UID), Attempt: 1,
		Phase: corev1alpha1.TaskPhaseSucceeded, Message: "ACP task completed",
		Execution: corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
			Attempt: 1, PromptID: promptID,
		},
		Delivery: task.Status.Delivery.DeepCopy(),
	}
	if err := oldDispatcher.enqueueStandaloneTaskProjection(ctx, task, payload); err != nil {
		t.Fatal(err)
	}
	projectionID := standaloneTaskTerminalProjectionID(task, 1)

	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	projector := &ACPOutboxProjector{Client: kubeClient, Store: controlStore, Epochs: oldEpochs, WorkerID: "task-projection-test"}
	if err := projector.projectOnce(ctx); err != nil {
		t.Fatal(err)
	}
	original, err := controlStore.GetOutboxProjection(ctx, projectionID)
	if err != nil {
		t.Fatal(err)
	}
	if original.State != store.OutboxProjectionDelivered || original.DeliveryDigest == "" || original.DeliveredAt == nil {
		t.Fatalf("projection was not delivered before restart: %#v", original)
	}
	stopOld()
	newEpochs, stopNew := startACPRecoveryEpochManager(t, ctx, controlStore, "controller-new-task-projection")
	defer stopNew()
	newFence, err := newEpochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if newFence.Epoch <= oldFence.Epoch {
		t.Fatalf("new epoch = %d, want > %d", newFence.Epoch, oldFence.Epoch)
	}
	dispatcher := &ACPDispatcher{Client: kubeClient, APIReader: kubeClient, Store: controlStore, ResultStore: controlStore, Epochs: newEpochs}
	if err := dispatcher.recoverStaleAttempts(ctx); err != nil {
		t.Fatal(err)
	}
	recoveredTask := &corev1alpha1.Task{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, recoveredTask); err != nil {
		t.Fatal(err)
	}
	if recoveredTask.Status.Execution == nil || recoveredTask.Status.Execution.ControllerEpoch != newFence.Epoch || recoveredTask.Status.Message != "ACP task completed" || recoveredTask.Status.Delivery == nil || recoveredTask.Status.Delivery.StartingSHA != task.Status.Delivery.StartingSHA {
		var recoveredEpoch int64
		if recoveredTask.Status.Execution != nil {
			recoveredEpoch = recoveredTask.Status.Execution.ControllerEpoch
		}
		var recoveredSHA string
		if recoveredTask.Status.Delivery != nil {
			recoveredSHA = recoveredTask.Status.Delivery.StartingSHA
		}
		t.Fatalf("terminal Task identity changed during projection recovery: epoch=%d want=%d message=%q sha=%q wantSHA=%q", recoveredEpoch, newFence.Epoch, recoveredTask.Status.Message, recoveredSHA, task.Status.Delivery.StartingSHA)
	}
	if err := dispatcher.recoverStaleAttempts(ctx); err != nil {
		t.Fatalf("same-epoch recovery retry: %v", err)
	}
	recovered, err := controlStore.GetOutboxProjection(ctx, projectionID)
	if err != nil {
		t.Fatal(err)
	}
	deliveredAtMatches := recovered.DeliveredAt != nil && original.DeliveredAt != nil && recovered.DeliveredAt.Equal(*original.DeliveredAt)
	if string(recovered.Payload) != string(original.Payload) || recovered.PayloadDigest != original.PayloadDigest ||
		!recovered.InitialAvailableAt.Equal(original.InitialAvailableAt) || !recovered.AvailableAt.Equal(original.AvailableAt) ||
		recovered.State != original.State || recovered.Version != original.Version || recovered.Attempts != original.Attempts ||
		recovered.DeliveryDigest != original.DeliveryDigest || !deliveredAtMatches || recovered.ControllerEpoch != original.ControllerEpoch {
		t.Fatalf("recovery changed existing task projection: before=%#v after=%#v", original, recovered)
	}
}

//nolint:gocyclo // The restart, drain, retry, receipt, and projection assertions stay in one scenario.
func TestRecoveredTaskScopedRuntimeSessionCleanupRetriesBeforeEpochAdvance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "recovered-cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	controlStore := sqlite.NewStore(db, "test")
	epochs, stopEpoch := startACPRecoveryEpochManager(t, ctx, controlStore, "recovered-cleanup")
	defer stopEpoch()
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	profile := harnessProfileForTest()
	profile.Model = acpTestModel
	profileDigest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	var deleteCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == harnessv2.CapabilitiesPath:
			writeDispatcherJSON(w, harnessv2.CapabilitiesResponse{
				Protocol: harnessv2.ProtocolVersion, Transport: "http+ndjson", ACPVersion: harnessv2.ACPProfileV1,
				RuntimeProfileDigest: profileDigest, ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
				AdapterDigests: profile.AdapterDigests, Limits: harnessv2.DefaultProtocolLimits(),
				Provider:            harnessv2.ProviderCapabilities{ProviderKinds: []string{profile.ProviderKind}, Models: []string{profile.Model}, SupportsCancel: true, SupportsPermissions: true, SupportsTools: true},
				WorkspaceGovernance: harnessv2.StrictWorkspaceGovernanceCapabilities(), SupportsDrain: true,
			})
		case r.Method == http.MethodGet && r.URL.Path == harnessv2.StatusPath:
			now := time.Now().UTC()
			writeDispatcherJSON(w, harnessv2.StatusResponse{
				Protocol: harnessv2.ProtocolVersion,
				Fence: harnessv2.Fence{
					RuntimeInstanceID: "pod-uid.boot-id", SupervisorBootID: "boot-id", ControllerEpoch: uint64(fence.Epoch),
					RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 1, RuntimeProfileDigest: profileDigest,
					ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
				},
				Lifecycle: harnessv2.SupervisorLifecycleReady, Drain: harnessv2.DrainStatus{AcceptingNewSessions: true},
				Sessions: []harnessv2.RuntimeSessionStatus{{
					RuntimeSessionID: "runtime-session-recovered-cleanup", RuntimeSessionUID: "session-recovered-cleanup",
					Generation: 1, State: harnessv2.RuntimeSessionStateIdle, LastTransitionAt: now,
				}},
				Pressure: harnessv2.PressureMetadata{ResidentSessions: 1}, Timestamp: now,
			})
		case r.Method == http.MethodDelete:
			var request harnessv2.DeleteRuntimeSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode delete: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if deleteCalls.Add(1) == 1 {
				writeDispatcherJSONStatus(w, http.StatusInternalServerError, harnessv2.ErrorResponse{
					Protocol: harnessv2.ProtocolVersion, Code: harnessv2.ErrorCodeSessionPoisoned,
					Message: "runtime descendant cleanup could not be proven", Retryable: false,
				})
				return
			}
			writeDispatcherJSON(w, harnessv2.DeleteRuntimeSessionResponse{
				Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh}, State: harnessv2.RuntimeSessionStateDeleted,
				Tombstone: testDeleteTombstone(request, time.Now().UTC()),
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "recovered-cleanup", UID: types.UID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseCancelled, Attempts: 1,
			Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionStateCancelled, Outcome: corev1alpha1.TaskExecutionOutcomeCancelled,
				Attempt: 1, PromptID: "prompt-recovered-cleanup", RuntimePoolName: "pool", RuntimePoolUID: "pool-uid",
				RuntimeInstanceID: "pod-uid.boot-id", RuntimeSessionUID: "session-recovered-cleanup", RuntimeSessionGeneration: 1,
				ControllerEpoch: fence.Epoch, RequestDigest: testControlDigestForDispatcher("recovered-cleanup"),
			},
			Delivery: &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested},
		},
	}
	pool := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "pool", UID: types.UID("pool-uid"), Generation: 1},
		Spec: corev1alpha1.RuntimePoolSpec{RuntimeNamespace: "orka-runtimes", Runtime: corev1alpha1.RuntimePoolRuntimeSpec{
			Image:   "docker.io/example/acp@sha256:" + strings.Repeat("a", 64),
			Profile: RuntimePoolProfileFromPlan(ACPRuntimePlan{Profile: profile, Digest: profileDigest}),
		}},
		Status: corev1alpha1.RuntimePoolStatus{ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{
			PodNamespace: "orka-runtimes", PodName: "runtime-pod", PodAddress: parsed.Host, PodUID: "pod-uid", BootID: "boot-id",
			RuntimeInstanceID: task.Status.Execution.RuntimeInstanceID, ControllerEpoch: fence.Epoch - 1,
			ProtocolVersion: corev1alpha1.RuntimePoolProtocolHarnessV2, ProfileDigest: string(profileDigest), ProfileDigestSchemaVersion: strconv.FormatUint(uint64(harnessv2.ProfileDigestSchemaVersion), 10),
		}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "orka-runtimes", Name: "pool-auth", Labels: map[string]string{
			runtimePoolAuthLabel: "true", runtimePoolUIDLabel: string(pool.UID),
		}},
		Data: map[string][]byte{runtimePoolControllerTokenKey: []byte(strings.Repeat("t", 32)), runtimePoolCapabilitySecretKey: []byte(strings.Repeat("s", 32))},
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task, pool, secret).Build()
	attemptKey := store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID}
	attempt, err := controlStore.CreatePromptAttempt(ctx, &store.PromptAttempt{Key: attemptKey, RequestDigest: task.Status.Execution.RequestDigest}, fence)
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []store.PromptExecutionState{
		store.PromptExecutionReserved, store.PromptExecutionSessionStarting, store.PromptExecutionPlanned,
		store.PromptExecutionSubmitting, store.PromptExecutionAccepted, store.PromptExecutionRunning, store.PromptExecutionCancelled,
	} {
		op := "recovered-cleanup-" + string(next)
		attempt, err = controlStore.TransitionPromptAttemptExecution(ctx, store.PromptAttemptExecutionTransition{
			ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
			NewState: next, OperationID: op, OperationDigest: testControlDigestForDispatcher(op), UpdatedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	gate := NewACPAdmissionGate()
	gate.Close("planned drain", time.Now().UTC())
	dispatcher := &ACPDispatcher{
		Client: kubeClient, APIReader: kubeClient, Store: controlStore, Epochs: epochs, AdmissionGate: gate,
		sem: make(chan struct{}, 1), active: make(map[types.UID]struct{}),
	}
	if complete, err := dispatcher.cleanupRecoveredTaskScopedRuntimeSession(ctx, task); err != nil || complete {
		t.Fatalf("stale-epoch cleanup = complete:%v err:%v, want pending without error", complete, err)
	}
	if got := deleteCalls.Load(); got != 0 {
		t.Fatalf("stale-epoch cleanup issued %d DELETE calls, want 0", got)
	}
	currentPool := &corev1alpha1.RuntimePool{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, currentPool); err != nil {
		t.Fatal(err)
	}
	currentPool.Status.ActiveInstance.ControllerEpoch = fence.Epoch
	if err := kubeClient.Update(ctx, currentPool); err != nil {
		t.Fatal(err)
	}
	if complete, err := dispatcher.cleanupRecoveredTaskScopedRuntimeSession(ctx, task); err == nil || complete {
		t.Fatalf("first cleanup = complete:%v err:%v, want incomplete error", complete, err)
	}
	if err := dispatcher.dispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var recoveredTask corev1alpha1.Task
	projectionReady := false
	projectionID := standaloneTaskTerminalProjectionID(task, 1)
	for range 100 {
		if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, &recoveredTask); err != nil {
			t.Fatal(err)
		}
		if _, err := controlStore.GetOutboxProjection(ctx, projectionID); err == nil {
			projectionReady = true
		} else if !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		if deleteCalls.Load() == 2 && taskScopedRuntimeSessionCleanupComplete(&recoveredTask) && projectionReady {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := deleteCalls.Load(); got != 2 || !taskScopedRuntimeSessionCleanupComplete(&recoveredTask) || !projectionReady {
		t.Fatalf("dispatcher retry = DELETE calls:%d cleanupComplete:%v projectionReady:%v", got, taskScopedRuntimeSessionCleanupComplete(&recoveredTask), projectionReady)
	}
}

func TestRecoveredWriteSessionPublicationCleanupByRuntimeState(t *testing.T) {
	for _, test := range []struct {
		name       string
		state      harnessv2.RuntimeSessionState
		operations []string
	}{
		{name: "publication prepared", state: harnessv2.RuntimeSessionStatePublicationPrepared, operations: []string{"finalize", "delete"}},
		{name: "finalizing", state: harnessv2.RuntimeSessionStateFinalizing, operations: []string{"delete"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRecoveredWriteCleanupFixture(t, recoveredWriteCleanupOptions{state: test.state})
			complete, err := fixture.dispatcher.cleanupRecoveredTaskScopedRuntimeSession(fixture.ctx, fixture.task.DeepCopy())
			if err != nil || !complete {
				t.Fatalf("cleanup = complete:%v err:%v, want complete", complete, err)
			}
			operations, finalizations, deletes := fixture.trace.snapshot()
			if fmt.Sprint(operations) != fmt.Sprint(test.operations) {
				t.Fatalf("cleanup operations = %v, want %v", operations, test.operations)
			}
			if len(deletes) != 1 {
				t.Fatalf("delete requests = %d, want 1", len(deletes))
			}
			if test.state == harnessv2.RuntimeSessionStatePublicationPrepared {
				if len(finalizations) != 1 {
					t.Fatalf("publication finalization requests = %d, want 1", len(finalizations))
				}
				request := finalizations[0]
				if request.WorkspaceDeltaID != harnessv2.WorkspaceDeltaID("delta-"+fixture.task.Status.Execution.PromptID) ||
					request.PublicationID != publicationIDForTask(fixture.task) ||
					request.PublicationGeneration != uint64(fixture.publication.Generation) ||
					request.PublicationVersion != uint64(fixture.publication.Version) ||
					request.TerminalState != harnessv2.PublicationTerminalVerifiedExact || request.TerminalReceiptDigest == "" {
					t.Fatalf("recovered publication finalization request = %#v", request)
				}
			} else if len(finalizations) != 0 {
				t.Fatalf("finalizing RuntimeSession was finalized again: %#v", finalizations)
			}
			fixture.assertCleanupReceipt(t)
		})
	}
}

func TestRecoveredWriteSessionDeleteResponseLossUsesStatusAbsenceReceipt(t *testing.T) {
	fixture := newRecoveredWriteCleanupFixture(t, recoveredWriteCleanupOptions{
		state: harnessv2.RuntimeSessionStateFinalizing, abortFirstDeleteResponse: true, omitDeletedSessionFromStatus: true,
	})
	complete, err := fixture.dispatcher.cleanupRecoveredTaskScopedRuntimeSession(fixture.ctx, fixture.task.DeepCopy())
	if err == nil || complete {
		t.Fatalf("first cleanup after lost DELETE response = complete:%v err:%v, want incomplete error", complete, err)
	}
	var afterLoss corev1alpha1.Task
	if err := fixture.kubeClient.Get(fixture.ctx, types.NamespacedName{Namespace: fixture.task.Namespace, Name: fixture.task.Name}, &afterLoss); err != nil {
		t.Fatal(err)
	}
	if afterLoss.Status.Execution == nil || afterLoss.Status.Execution.RuntimeSessionCleanupDigest != "" {
		t.Fatalf("cleanup receipt was persisted before deletion was proven: %#v", afterLoss.Status.Execution)
	}
	complete, err = fixture.dispatcher.cleanupRecoveredTaskScopedRuntimeSession(fixture.ctx, &afterLoss)
	if err != nil || !complete {
		t.Fatalf("status-absence recovery = complete:%v err:%v, want complete", complete, err)
	}
	operations, finalizations, deletes := fixture.trace.snapshot()
	if fmt.Sprint(operations) != "[delete]" || len(finalizations) != 0 || len(deletes) != 1 {
		t.Fatalf("response-loss recovery operations = %v finalizations=%d deletes=%d", operations, len(finalizations), len(deletes))
	}
	fixture.assertCleanupReceipt(t)
}

func TestACPDispatcherRecoveryAcceptsDeadLetteredStandaloneProjection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "dead-letter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	controlStore := sqlite.NewStore(db, "test")
	epochs, stopEpoch := startACPRecoveryEpochManager(t, ctx, controlStore, "dead-letter")
	defer stopEpoch()
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "dead-letter", UID: types.UID("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
			Attempt: 1, PromptID: "prompt-dead-letter",
		}, Delivery: &corev1alpha1.TaskDeliveryStatus{
			State: corev1alpha1.TaskDeliveryStateReadValidated, Outcome: corev1alpha1.TaskDeliveryOutcomeReadValidated,
		}},
	}
	attempt := &store.PromptAttempt{
		Key:            store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID},
		ExecutionState: store.PromptExecutionSucceeded, DeliveryState: store.PromptDeliveryReadValidated,
	}
	dispatcher := &ACPDispatcher{Store: controlStore, Epochs: epochs}
	if err := dispatcher.enqueueStandaloneTaskProjection(ctx, task, taskTerminalProjection{
		Namespace: task.Namespace, Task: task.Name, TaskUID: string(task.UID), Attempt: 1,
		Phase: corev1alpha1.TaskPhaseSucceeded, Message: "ACP task completed",
		Execution: *task.Status.Execution.DeepCopy(), Delivery: task.Status.Delivery.DeepCopy(),
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := controlStore.ClaimOutboxProjections(ctx, store.ClaimOutboxProjectionsRequest{
		Fence: fence, WorkerID: "dead-letter-worker", Limit: 1, LeaseDuration: time.Minute, Now: time.Now().UTC(),
	})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim projection: count=%d err=%v", len(claimed), err)
	}
	if _, err := controlStore.CompleteOutboxProjection(ctx, store.CompleteOutboxProjectionRequest{
		ID: claimed[0].ID, Fence: fence, ExpectedVersion: claimed[0].Version, LeaseOwner: "dead-letter-worker",
		OperationID: "dead-letter", OperationDigest: testControlDigestForDispatcher("dead-letter"),
		NewState: store.OutboxProjectionDeadLetter, LastError: "delivery attempts exhausted", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	exists, err := dispatcher.validateExistingStandaloneTaskProjection(ctx, task, attempt)
	if err != nil || !exists {
		t.Fatalf("dead-letter projection validation = exists:%v err:%v", exists, err)
	}
}

func TestACPDispatcherRecoveryCreatesMissingStandaloneNonSuccessProjection(t *testing.T) {
	tests := []struct {
		name       string
		state      store.PromptExecutionState
		phase      corev1alpha1.TaskPhase
		outcome    corev1alpha1.TaskExecutionOutcome
		sessionRef bool
	}{
		{name: "failed", state: store.PromptExecutionFailed, phase: corev1alpha1.TaskPhaseFailed, outcome: corev1alpha1.TaskExecutionOutcomeFailed},
		{name: "cancelled", state: store.PromptExecutionCancelled, phase: corev1alpha1.TaskPhaseCancelled, outcome: corev1alpha1.TaskExecutionOutcomeCancelled},
		{name: "cancelled before session binding", state: store.PromptExecutionCancelled, phase: corev1alpha1.TaskPhaseCancelled, outcome: corev1alpha1.TaskExecutionOutcomeCancelled, sessionRef: true},
		{name: "outcome unknown", state: store.PromptExecutionOutcomeUnknown, phase: corev1alpha1.TaskPhaseFailed, outcome: corev1alpha1.TaskExecutionOutcomeOutcomeUnknown},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "missing-terminal.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close() //nolint:errcheck
			controlStore := sqlite.NewStore(db, "test")
			epochs, stopEpoch := startACPRecoveryEpochManager(t, ctx, controlStore, "missing-terminal-"+tc.name)
			defer stopEpoch()
			uid := types.UID(fmt.Sprintf("88888888-8888-8888-8888-%012d", i+1))
			promptID := "prompt-" + string(uid) + "-1"
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "missing-" + tc.name, UID: uid},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
				Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Attempts: 1,
					Execution: &corev1alpha1.TaskExecutionStatus{State: corev1alpha1.TaskExecutionState(tc.state), Outcome: tc.outcome, Attempt: 1, PromptID: promptID},
					Delivery:  &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested},
				},
			}
			if tc.sessionRef {
				task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "unbound-session", Create: true, Append: true}
			}
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
			dispatcher := &ACPDispatcher{Client: kubeClient, APIReader: kubeClient, Store: controlStore, Epochs: epochs}
			attempt := &store.PromptAttempt{
				Key:            store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: promptID},
				ExecutionState: tc.state, DeliveryState: store.PromptDeliveryNotRequested,
			}
			if err := dispatcher.recoverMissingStandaloneTerminalProjection(ctx, task, attempt); err != nil {
				t.Fatal(err)
			}
			projection, err := controlStore.GetOutboxProjection(ctx, standaloneTaskTerminalProjectionID(task, 1))
			if err != nil {
				t.Fatal(err)
			}
			var payload taskTerminalProjection
			if err := json.Unmarshal(projection.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Phase != tc.phase || payload.Execution.State != corev1alpha1.TaskExecutionState(tc.state) || payload.Execution.Outcome != tc.outcome || payload.Delivery == nil || payload.Delivery.State != corev1alpha1.TaskDeliveryStateNotRequested {
				t.Fatalf("terminal projection payload = %#v", payload)
			}
			updated := &corev1alpha1.Task{}
			if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, updated); err != nil {
				t.Fatal(err)
			}
			if updated.Status.Phase != tc.phase || updated.Status.Execution == nil || updated.Status.Execution.Outcome != tc.outcome {
				t.Fatalf("recovered Task status = %#v", updated.Status)
			}
		})
	}
}

func TestRecoveredTerminalDeliveryStatusPreservesTaskEvidence(t *testing.T) {
	transition := metav1.NewTime(time.Date(2026, 7, 25, 13, 0, 0, 0, time.UTC))
	task := &corev1alpha1.Task{Status: corev1alpha1.TaskStatus{Delivery: &corev1alpha1.TaskDeliveryStatus{
		State: corev1alpha1.TaskDeliveryStateReadValidated, Outcome: corev1alpha1.TaskDeliveryOutcomeReadValidated,
		StartingSHA: "740310bf8ecfbce4963628a51b6a11e26d7ee7be", LastTransitionTime: &transition,
	}}}
	attempt := &store.PromptAttempt{DeliveryState: store.PromptDeliveryReadValidated}
	status, err := (&ACPDispatcher{}).recoveredTerminalDeliveryStatus(context.Background(), task, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if status.StartingSHA != task.Status.Delivery.StartingSHA || status.LastTransitionTime == nil || !status.LastTransitionTime.Equal(&transition) {
		t.Fatalf("recovered delivery evidence = %#v, want %#v", status, task.Status.Delivery)
	}
	if status == task.Status.Delivery {
		t.Fatal("recovered delivery status aliased Task status")
	}
}

//nolint:gocyclo // The restart-boundary assertions intentionally stay in one end-to-end scenario.
func TestACPDispatcherRecoveryResumesCommittedSessionTurnFinalization(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	sqliteStore := sqlite.NewStore(db, "test")

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
	taskUID := types.UID("44444444-4444-4444-4444-444444444444")
	promptID := "prompt-" + string(taskUID) + "-1"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "session-crash", UID: taskUID,
			Labels: map[string]string{acpRuntimeTaskPoolLabel: "pool"},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "persist this failed turn",
			SessionRef: &corev1alpha1.SessionReference{Name: "session-crash", Create: true, Append: true},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Attempts: 1},
	}
	rawClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&corev1alpha1.Task{}, &corev1alpha1.ControllerEpoch{}, &corev1alpha1.PromptAttempt{},
			&corev1alpha1.RuntimeSessionControl{}, &corev1alpha1.BranchClaim{}, &corev1alpha1.Publication{},
			&corev1alpha1.ExternalEffect{},
		).
		WithObjects(task).
		Build()
	controlStore, err := kubestore.NewComposite(rawClient, "orka-system", sqliteStore)
	if err != nil {
		t.Fatal(err)
	}
	oldEpochs, stopOldEpoch := startACPRecoveryEpochManager(t, ctx, controlStore, "controller-old")
	oldFence, err := oldEpochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	continuity := newACPRecoveryContinuity(t, controlStore, sqliteStore, "session-crash-uid")
	control, err := continuity.EnsureSession(ctx, ACPEnsureSessionRequest{
		Namespace: "default", SessionName: "session-crash", SessionType: "task",
		ExpectedSessionUID: "session-crash-uid", Fence: oldFence, CreatedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	attemptKey := store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(taskUID), Attempt: 1, PromptID: promptID}
	attempt, err := controlStore.CreatePromptAttempt(ctx, &store.PromptAttempt{Key: attemptKey, RequestDigest: testControlDigestForDispatcher("session-crash")}, oldFence)
	if err != nil {
		t.Fatal(err)
	}
	attempt = transitionACPRecoveryAttempt(t, ctx, controlStore, oldFence, attempt, store.PromptExecutionReserved, nil)
	lease, err := continuity.AcquireMutationLease(ctx, ACPAcquireSessionLeaseRequest{
		Session: *control, Fence: oldFence, TaskUID: string(taskUID), Attempt: 1, PromptID: promptID,
		PromptRequestDigest: attempt.RequestDigest, AcquiredAt: time.Date(2026, 7, 25, 12, 1, 0, 0, time.UTC),
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
		store.PromptExecutionPlanned, store.PromptExecutionSubmitting, store.PromptExecutionAccepted,
		store.PromptExecutionRunning, store.PromptExecutionFailed,
	} {
		attempt = transitionACPRecoveryAttempt(t, ctx, controlStore, oldFence, attempt, state, nil)
	}
	task.Status.Execution = &corev1alpha1.TaskExecutionStatus{
		State: corev1alpha1.TaskExecutionStateFailed, Outcome: corev1alpha1.TaskExecutionOutcomeFailed,
		Attempt: 1, PromptID: promptID, RuntimePoolName: "pool", RuntimeInstanceID: "runtime-old",
		RuntimeSessionUID: control.SessionUID, RuntimeSessionGeneration: lease.Key.LeaseGeneration,
		ControllerEpoch: oldFence.Epoch, RequestDigest: attempt.RequestDigest,
	}
	if err := rawClient.Status().Update(ctx, task); err != nil {
		t.Fatal(err)
	}

	failSessionStatusOnce := true
	failingClient := interceptor.NewClient(rawClient, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if failSessionStatusOnce {
				if _, isSession := obj.(*corev1alpha1.RuntimeSessionControl); isSession {
					failSessionStatusOnce = false
					return apierrors.NewConflict(schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "runtimesessioncontrols"}, obj.GetName(), errors.New("simulated controller crash"))
				}
			}
			return c.SubResource(subresource).Update(ctx, obj, opts...)
		},
	})
	failingStore, err := kubestore.NewComposite(failingClient, "orka-system", sqliteStore)
	if err != nil {
		t.Fatal(err)
	}
	failingContinuity := newACPRecoveryContinuity(t, failingStore, sqliteStore, control.SessionUID)
	projectionPayload, err := json.Marshal(taskTerminalProjection{
		Namespace: task.Namespace, Task: task.Name, TaskUID: string(task.UID), Attempt: 1,
		Phase: corev1alpha1.TaskPhaseFailed,
		Execution: corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateFailed, Outcome: corev1alpha1.TaskExecutionOutcomeFailed,
			Attempt: 1, PromptID: promptID, Reason: "PromptFailed", Message: "prompt failed",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	finalizedAt := time.Date(2026, 7, 25, 12, 3, 0, 0, time.UTC)
	_, err = failingContinuity.FinalizeOutcomeMarker(ctx, ACPFinalizeOutcomeMarkerRequest{
		SessionTurn: *turn, Fence: oldFence, Kind: "Failed", Reason: "prompt failed",
		Projection: ACPFinalizationProjection{
			ProjectionKind: "TaskTerminalStatus", Payload: projectionPayload, AvailableAt: finalizedAt,
		},
		FinalizedAt: finalizedAt,
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("partial finalization error = %v, want simulated conflict", err)
	}
	committedTurn, err := sqliteStore.GetSessionTurn(ctx, turn.Turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if committedTurn.State != store.SessionTurnFinalized || committedTurn.FinalizedAt == nil {
		t.Fatalf("SQLite turn was not finalized before crash: %#v", committedTurn)
	}
	deferred, err := sqliteStore.GetOutboxProjection(ctx, committedTurn.ProjectionID)
	if err != nil {
		t.Fatal(err)
	}
	if deferred.AvailableAt.Year() != 9999 {
		t.Fatalf("outbox was activated before Kubernetes completion: %s", deferred.AvailableAt)
	}

	stopOldEpoch()
	restartedStore, err := kubestore.NewComposite(rawClient, "orka-system", sqliteStore)
	if err != nil {
		t.Fatal(err)
	}
	newEpochs, stopNewEpoch := startACPRecoveryEpochManager(t, ctx, restartedStore, "controller-new")
	defer stopNewEpoch()
	newContinuity := newACPRecoveryContinuity(t, restartedStore, sqliteStore, control.SessionUID)
	dispatcher := &ACPDispatcher{
		Client: rawClient, APIReader: rawClient, Store: restartedStore, ResultStore: sqliteStore,
		Epochs: newEpochs, Sessions: newContinuity,
	}
	if err := dispatcher.recoverStaleAttempts(ctx); err != nil {
		t.Fatal(err)
	}

	recoveredTurn, err := sqliteStore.GetSessionTurn(ctx, turn.Turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredTurn.FinalizationDigest != committedTurn.FinalizationDigest || recoveredTurn.FinalizedAt == nil ||
		!recoveredTurn.FinalizedAt.Equal(*committedTurn.FinalizedAt) ||
		!recoveredTurn.ProjectionAvailableAt.Equal(committedTurn.ProjectionAvailableAt) {
		t.Fatalf("recovery changed persisted finalization identity: before=%#v after=%#v", committedTurn, recoveredTurn)
	}
	recoveredControl, err := restartedStore.GetSessionControl(ctx, control.Namespace, control.SessionName)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredControl.Lease != nil || recoveredControl.LastOperationID != "finalize:"+turn.Turn.ID || recoveredControl.LastOperationDigest != committedTurn.FinalizationDigest {
		t.Fatalf("SessionControl finalization tail was not completed: %#v", recoveredControl)
	}
	activated, err := sqliteStore.GetOutboxProjection(ctx, committedTurn.ProjectionID)
	if err != nil {
		t.Fatal(err)
	}
	if !activated.AvailableAt.Equal(committedTurn.ProjectionAvailableAt) || activated.PayloadDigest != committedTurn.ProjectionDigest {
		t.Fatalf("outbox activation did not reuse persisted receipt: %#v", activated)
	}
	transcript, err := sqliteStore.LoadTranscript(ctx, control.Namespace, control.SessionName, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) != 2 {
		t.Fatalf("recovery duplicated transcript entries: %d", len(transcript))
	}
}

type recoveredWriteCleanupOptions struct {
	state                        harnessv2.RuntimeSessionState
	abortFirstDeleteResponse     bool
	omitDeletedSessionFromStatus bool
}

type recoveredWriteCleanupTrace struct {
	mu            sync.Mutex
	operations    []string
	finalizations []harnessv2.FinalizeRuntimeSessionPublicationRequest
	deletes       []harnessv2.DeleteRuntimeSessionRequest
	deleted       bool
}

func (t *recoveredWriteCleanupTrace) snapshot() (
	[]string,
	[]harnessv2.FinalizeRuntimeSessionPublicationRequest,
	[]harnessv2.DeleteRuntimeSessionRequest,
) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.operations...),
		append([]harnessv2.FinalizeRuntimeSessionPublicationRequest(nil), t.finalizations...),
		append([]harnessv2.DeleteRuntimeSessionRequest(nil), t.deletes...)
}

type recoveredWriteCleanupFixture struct {
	ctx         context.Context
	task        *corev1alpha1.Task
	publication *store.Publication
	kubeClient  client.Client
	dispatcher  *ACPDispatcher
	trace       *recoveredWriteCleanupTrace
}

func (f *recoveredWriteCleanupFixture) assertCleanupReceipt(t *testing.T) {
	t.Helper()
	var recovered corev1alpha1.Task
	if err := f.kubeClient.Get(f.ctx, types.NamespacedName{Namespace: f.task.Namespace, Name: f.task.Name}, &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.Status.Execution == nil {
		t.Fatal("recovered task execution status is missing")
	}
	wantDigest, err := taskScopedRuntimeSessionCleanupDigest(
		recovered.UID, recovered.Status.Execution.Attempt, recovered.Status.Execution.RuntimeInstanceID,
		recovered.Status.Execution.RuntimeSessionUID, recovered.Status.Execution.RuntimeSessionGeneration,
	)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status.Execution.RuntimeSessionCleanupDigest != wantDigest || !taskScopedRuntimeSessionCleanupComplete(&recovered) {
		t.Fatalf("write SessionRef cleanup receipt = %q, want %q", recovered.Status.Execution.RuntimeSessionCleanupDigest, wantDigest)
	}
}

type recoveredPublicationStore struct {
	store.DurableControlStore
	publication    *store.Publication
	sessionControl *store.SessionControl
}

func (s *recoveredPublicationStore) GetSessionControl(ctx context.Context, namespace, sessionName string) (*store.SessionControl, error) {
	if s.sessionControl != nil && namespace == s.sessionControl.Namespace && sessionName == s.sessionControl.SessionName {
		copy := *s.sessionControl
		return &copy, nil
	}
	return s.DurableControlStore.GetSessionControl(ctx, namespace, sessionName)
}

func (s *recoveredPublicationStore) GetPublication(ctx context.Context, id string) (*store.Publication, error) {
	if s.publication != nil && id == s.publication.ID {
		copy := *s.publication
		return &copy, nil
	}
	return s.DurableControlStore.GetPublication(ctx, id)
}

//nolint:gocyclo // This fixture keeps the authenticated recovery boundary explicit and auditable.
func newRecoveredWriteCleanupFixture(t *testing.T, options recoveredWriteCleanupOptions) *recoveredWriteCleanupFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "recovered-write-cleanup.db"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	t.Cleanup(cancel)
	controlStore := sqlite.NewStore(db, "test")
	epochs, stopEpoch := startACPRecoveryEpochManager(t, ctx, controlStore, "recovered-write-cleanup")
	t.Cleanup(stopEpoch)
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	profile := harnessProfileForTest()
	profile.Model = acpTestModel
	profile.WorkspaceIntent = harnessv2.WorkspaceIntentWrite
	profileDigest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	trace := &recoveredWriteCleanupTrace{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == harnessv2.CapabilitiesPath:
			writeDispatcherJSON(w, harnessv2.CapabilitiesResponse{
				Protocol: harnessv2.ProtocolVersion, Transport: "http+ndjson", ACPVersion: harnessv2.ACPProfileV1,
				RuntimeProfileDigest: profileDigest, ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
				AdapterDigests: profile.AdapterDigests, Limits: harnessv2.DefaultProtocolLimits(),
				Provider: harnessv2.ProviderCapabilities{
					ProviderKinds: []string{profile.ProviderKind}, Models: []string{profile.Model},
					SupportsCancel: true, SupportsPermissions: true, SupportsTools: true,
				},
				WorkspaceGovernance:             harnessv2.StrictWorkspaceGovernanceCapabilities(),
				SupportsDrain:                   true,
				SupportsPublicationFinalization: true,
			})
		case r.Method == http.MethodGet && r.URL.Path == harnessv2.StatusPath:
			now := time.Now().UTC()
			trace.mu.Lock()
			deleted := trace.deleted
			trace.mu.Unlock()
			sessions := []harnessv2.RuntimeSessionStatus(nil)
			resident := uint32(0)
			if !deleted || !options.omitDeletedSessionFromStatus {
				sessions = []harnessv2.RuntimeSessionStatus{{
					RuntimeSessionID: "runtime-session-recovered-write", RuntimeSessionUID: "session-recovered-write",
					Generation: 1, State: options.state,
					ReservedForFinalization: options.state == harnessv2.RuntimeSessionStateFinalizing,
					LastTransitionAt:        now,
				}}
				resident = 1
			}
			writeDispatcherJSON(w, harnessv2.StatusResponse{
				Protocol: harnessv2.ProtocolVersion,
				Fence: harnessv2.Fence{
					RuntimeInstanceID: "pod-uid.boot-id", SupervisorBootID: "boot-id", ControllerEpoch: uint64(fence.Epoch),
					RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 1, RuntimeProfileDigest: profileDigest,
					ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
				},
				Lifecycle: harnessv2.SupervisorLifecycleReady, Drain: harnessv2.DrainStatus{AcceptingNewSessions: true},
				Sessions: sessions, Pressure: harnessv2.PressureMetadata{ResidentSessions: resident}, Timestamp: now,
			})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/publication-finalization"):
			var request harnessv2.FinalizeRuntimeSessionPublicationRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode recovered publication finalization: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			trace.mu.Lock()
			trace.operations = append(trace.operations, "finalize")
			trace.finalizations = append(trace.finalizations, request)
			trace.mu.Unlock()
			now := time.Now().UTC()
			writeDispatcherJSON(w, harnessv2.FinalizeRuntimeSessionPublicationResponse{
				Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
				Session: harnessv2.RuntimeSessionDescriptor{
					RuntimeSessionID:  harnessv2.RuntimeSessionID(runtimeSessionID(request.Metadata.Fence)),
					RuntimeSessionUID: request.Metadata.Fence.RuntimeSessionUID, Generation: request.Metadata.Fence.RuntimeSessionGeneration,
					RuntimeInstanceID: request.Metadata.Fence.RuntimeInstanceID, SupervisorBootID: request.Metadata.Fence.SupervisorBootID,
					RuntimeProfileDigest: request.Metadata.Fence.RuntimeProfileDigest, State: harnessv2.RuntimeSessionStateFinalizing,
					ProviderSessionID: "provider-session",
					WorkspaceBaseline: harnessv2.WorkspaceBaseline{
						RepositoryIdentity: "github.com/orka-agents/orka", Revision: strings.Repeat("1", 40),
						TreeDigest: "sha256:" + strings.Repeat("2", 64),
					},
					CreatedAt: now.Add(-time.Minute), LastTransitionAt: now,
				},
				Finalization: harnessv2.PublicationFinalizationReceipt{
					WorkspaceDeltaID: request.WorkspaceDeltaID, PublicationID: request.PublicationID,
					PublicationGeneration: request.PublicationGeneration, PublicationVersion: request.PublicationVersion,
					TerminalState: request.TerminalState, TerminalReceiptDigest: request.TerminalReceiptDigest, AppliedAt: now,
				},
			})
		case r.Method == http.MethodDelete:
			var request harnessv2.DeleteRuntimeSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode recovered delete: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			trace.mu.Lock()
			trace.operations = append(trace.operations, "delete")
			trace.deletes = append(trace.deletes, request)
			deleteCall := len(trace.deletes)
			trace.deleted = true
			trace.mu.Unlock()
			if options.abortFirstDeleteResponse && deleteCall == 1 {
				panic(http.ErrAbortHandler)
			}
			writeDispatcherJSON(w, harnessv2.DeleteRuntimeSessionResponse{
				Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
				State:     harnessv2.RuntimeSessionStateDeleted,
				Tombstone: testDeleteTombstone(request, time.Now().UTC()),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "recovered-write", UID: types.UID("abababab-abab-abab-abab-abababababab")},
		Spec: corev1alpha1.TaskSpec{
			Type:       corev1alpha1.TaskTypeAgent,
			SessionRef: &corev1alpha1.SessionReference{Name: "recovered-write", Create: true, Append: true},
			Workspace:  &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded, Attempts: 1,
			Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
				Attempt: 1, PromptID: "prompt-recovered-write", RuntimePoolName: "pool", RuntimePoolUID: "pool-uid",
				RuntimeInstanceID: "pod-uid.boot-id", RuntimeSessionUID: "session-recovered-write", RuntimeSessionGeneration: 1,
				ControllerEpoch: fence.Epoch, RequestDigest: testControlDigestForDispatcher("recovered-write"),
			},
			Delivery: &corev1alpha1.TaskDeliveryStatus{
				State: corev1alpha1.TaskDeliveryStateVerifiedExact, Outcome: corev1alpha1.TaskDeliveryOutcomeVerifiedExact,
				StartingSHA: strings.Repeat("1", 40), ExpectedCommitSHA: strings.Repeat("3", 40), VerifiedRemoteSHA: strings.Repeat("3", 40),
			},
		},
	}
	pool := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "pool", UID: types.UID("pool-uid"), Generation: 1},
		Spec: corev1alpha1.RuntimePoolSpec{RuntimeNamespace: "orka-runtimes", Runtime: corev1alpha1.RuntimePoolRuntimeSpec{
			Image:   "docker.io/example/acp@sha256:" + strings.Repeat("a", 64),
			Profile: RuntimePoolProfileFromPlan(ACPRuntimePlan{Profile: profile, Digest: profileDigest}),
		}},
		Status: corev1alpha1.RuntimePoolStatus{ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{
			PodNamespace: "orka-runtimes", PodName: "runtime-pod", PodAddress: parsed.Host, PodUID: "pod-uid", BootID: "boot-id",
			RuntimeInstanceID: task.Status.Execution.RuntimeInstanceID, ControllerEpoch: fence.Epoch,
			ProtocolVersion: corev1alpha1.RuntimePoolProtocolHarnessV2, ProfileDigest: string(profileDigest), ProfileDigestSchemaVersion: strconv.FormatUint(uint64(harnessv2.ProfileDigestSchemaVersion), 10),
		}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "orka-runtimes", Name: "pool-auth", Labels: map[string]string{
			runtimePoolAuthLabel: "true", runtimePoolUIDLabel: string(pool.UID),
		}},
		Data: map[string][]byte{runtimePoolControllerTokenKey: []byte(strings.Repeat("t", 32)), runtimePoolCapabilitySecretKey: []byte(strings.Repeat("s", 32))},
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task, pool, secret).Build()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	publication := &store.Publication{
		ID: publicationIDForTask(task), Namespace: task.Namespace, Generation: 1, Version: 7,
		TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID,
		State: store.PublicationVerifiedExact,
		PreparedReceipt: &store.PreparedPublicationReceipt{
			OperationID: "prepare", RequestDigest: testControlDigestForDispatcher("recovered-prepare"),
			TreeSHA: strings.Repeat("2", 40), CommitSHA: strings.Repeat("3", 40),
			ManifestDigest: "sha256:" + strings.Repeat("4", 64), PreparedAt: now.Add(-2 * time.Minute),
		},
		PublishReceipt: &store.PublishOperationReceipt{
			OperationID: "publish", RequestDigest: testControlDigestForDispatcher("recovered-publish"),
			ExpectedCommitSHA: strings.Repeat("3", 40), PublishedAt: now.Add(-time.Minute),
		},
		VerificationReceipt: &store.PublicationVerificationReceipt{
			OperationID: "verify", RequestDigest: testControlDigestForDispatcher("recovered-verify"),
			Outcome: store.PublicationVerifiedExact, ExpectedCommitSHA: strings.Repeat("3", 40),
			ObservedRemote: store.RemoteRefState{SHA: strings.Repeat("3", 40)}, VerifiedAt: now,
		},
	}
	publicationStore := &recoveredPublicationStore{DurableControlStore: controlStore, publication: publication}
	dispatcher := &ACPDispatcher{Client: kubeClient, APIReader: kubeClient, Store: publicationStore, Epochs: epochs}
	return &recoveredWriteCleanupFixture{
		ctx: ctx, task: task, publication: publication, kubeClient: kubeClient, dispatcher: dispatcher, trace: trace,
	}
}

func transitionACPRecoveryAttempt(
	t *testing.T,
	ctx context.Context,
	controlStore store.PromptAttemptStore,
	fence store.ControllerEpochFence,
	attempt *store.PromptAttempt,
	to store.PromptExecutionState,
	lease *ACPSessionLease,
) *store.PromptAttempt {
	t.Helper()
	transition := store.PromptAttemptExecutionTransition{
		ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
		NewState: to, OperationID: "recovery-" + string(to) + "-" + strconv.FormatInt(attempt.Version, 10),
		OperationDigest: testControlDigestForDispatcher("recovery-" + string(to) + "-" + strconv.FormatInt(attempt.Version, 10)),
		UpdatedAt:       time.Now().UTC(),
	}
	if lease != nil {
		transition.SessionUID = lease.Key.SessionUID
		transition.SessionLeaseGeneration = lease.Key.LeaseGeneration
		transition.RuntimeInstanceID = "runtime-old"
	}
	updated, err := controlStore.TransitionPromptAttemptExecution(ctx, transition)
	if err != nil {
		t.Fatalf("transition PromptAttempt to %s: %v", to, err)
	}
	return updated
}

func newACPRecoveryContinuity(t *testing.T, controls store.DurableControlStore, transcripts store.SessionStore, sessionUID string) *ACPSessionContinuity {
	t.Helper()
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: controls, Transcripts: transcripts,
		Publications: controls, BranchClaims: controls,
		NewSessionUID: func() (string, error) { return sessionUID, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return continuity
}

func startACPRecoveryEpochManager(t *testing.T, ctx context.Context, epochStore store.ControllerEpochStore, holder string) (*ControllerEpochManager, func()) {
	t.Helper()
	manager := NewControllerEpochManager(epochStore, holder)
	epochCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- manager.Start(epochCtx) }()
	if _, err := manager.CurrentFence(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	return manager, func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("stop epoch manager: %v", err)
		}
	}
}

type recoveryFixture struct {
	ctx          context.Context
	cancel       context.CancelFunc
	db           *sql.DB
	epochCancel  context.CancelFunc
	epochDone    chan error
	fence        store.ControllerEpochFence
	controlStore *sqlite.Store
	kubeClient   client.Client
	dispatcher   *ACPDispatcher
	attemptID    string
}

func newACPRecoveryFixture(t *testing.T, target store.PromptExecutionState) *recoveryFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	controlStore := sqlite.NewStore(db, "test")
	first := NewControllerEpochManager(controlStore, "controller-old")
	oldCtx, stopOld := context.WithCancel(context.Background())
	oldDone := make(chan error, 1)
	go func() { oldDone <- first.Start(oldCtx) }()
	oldFence, err := first.CurrentFence(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}

	uid := types.UID("33333333-3333-3333-3333-333333333333")
	promptID := "prompt-" + string(uid) + "-1"
	key := store.PromptAttemptKey{Namespace: "default", TaskUID: string(uid), Attempt: 1, PromptID: promptID}
	attemptID, err := key.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := controlStore.CreatePromptAttempt(ctx, &store.PromptAttempt{
		ID: attemptID, Key: key, RequestDigest: testControlDigestForDispatcher("recovery"),
	}, oldFence)
	if err != nil {
		t.Fatal(err)
	}
	path := []store.PromptExecutionState{store.PromptExecutionReserved, store.PromptExecutionSessionStarting, store.PromptExecutionPlanned}
	if target == store.PromptExecutionAccepted {
		path = append(path, store.PromptExecutionSubmitting, store.PromptExecutionAccepted)
	}
	for _, next := range path {
		if attempt.ExecutionState == target {
			break
		}
		attempt, err = controlStore.TransitionPromptAttemptExecution(ctx, store.PromptAttemptExecutionTransition{
			ID: attempt.ID, Fence: oldFence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState, NewState: next,
			OperationID: "recover-" + string(next), OperationDigest: testControlDigestForDispatcher("recover-" + string(next)), UpdatedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	stopOld()
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}

	second := NewControllerEpochManager(controlStore, "controller-new")
	epochCtx, epochCancel := context.WithCancel(context.Background())
	epochDone := make(chan error, 1)
	go func() { epochDone <- second.Start(epochCtx) }()
	fence, err := second.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "task", UID: uid, Labels: map[string]string{acpRuntimeTaskPoolLabel: "pool"}},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "test"},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Attempts: 1, Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionState(target), Attempt: 1, PromptID: promptID, RuntimePoolName: "pool",
			ControllerEpoch: oldFence.Epoch, RequestDigest: attempt.RequestDigest,
		}},
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	return &recoveryFixture{
		ctx: ctx, cancel: cancel, db: db, epochCancel: epochCancel, epochDone: epochDone, fence: fence,
		controlStore: controlStore, kubeClient: kubeClient,
		dispatcher: &ACPDispatcher{Client: kubeClient, APIReader: kubeClient, Store: controlStore, ResultStore: controlStore, Epochs: second},
		attemptID:  attemptID,
	}
}

func (f *recoveryFixture) close(t *testing.T) {
	t.Helper()
	f.epochCancel()
	if err := <-f.epochDone; err != nil {
		t.Fatal(err)
	}
	f.cancel()
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveredTerminalDeliveryStatusUsesTaskReceiptWithoutPublication(t *testing.T) {
	storeValue, _, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "recovery-no-publication.db"))
	defer closeStore()
	dispatcher := &ACPDispatcher{Store: storeValue}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "no-publication", UID: types.UID("no-publication-uid")},
		Status: corev1alpha1.TaskStatus{
			Execution: &corev1alpha1.TaskExecutionStatus{Attempt: 1, PromptID: "prompt-no-publication"},
			Delivery: &corev1alpha1.TaskDeliveryStatus{
				State: corev1alpha1.TaskDeliveryStateDeliveryConflict, Outcome: corev1alpha1.TaskDeliveryOutcomeDeliveryConflict,
				Reason: "BranchMoved", Message: "branch moved before publication",
			},
		},
	}
	attempt := &store.PromptAttempt{DeliveryState: store.PromptDeliveryConflict}
	status, err := dispatcher.recoveredTerminalDeliveryStatus(context.Background(), task, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if status.Outcome != corev1alpha1.TaskDeliveryOutcomeDeliveryConflict || status.Reason != task.Status.Delivery.Reason {
		t.Fatalf("status = %#v", status)
	}
}
