package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type nativeDeliveryFailureStore struct {
	store.DurableControlStore
	fail bool
}

func (s *nativeDeliveryFailureStore) TransitionPromptAttemptDelivery(ctx context.Context, request store.PromptAttemptDeliveryTransition) (*store.PromptAttempt, error) {
	if s.fail && (request.NewState == store.PromptDeliveryNoChange || request.NewState == store.PromptDeliveryReadValidated) {
		return nil, errors.New("injected terminal delivery transition failure")
	}
	return s.DurableControlStore.TransitionPromptAttemptDelivery(ctx, request)
}

func TestACPDispatcherFirstNativeCaptureRecoveryPreservesExactEvidence(t *testing.T) {
	for _, takeover := range []bool{false, true} {
		name := "same-epoch-restart"
		if takeover {
			name = "epoch-takeover"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			snapshot := storetest.NativeSessionSnapshot(t, "first native checkpoint interrupted before capture intent")
			captures, deletes, prompts := 0, 0, 0
			var persistence *sqlite.Store
			fixture := newTaskScopedCreateConflictFixture(t, ctx, "native-recovery-"+name, "77777777-7777-7777-7777-777777777777",
				func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
					return newDispatcherRuntimeServerWithOptions(t, profile, digest, dispatcherRuntimeServerOptions{
						nativeSnapshot: &snapshot,
						onNativeCapture: func(request harnessv2.CaptureNativeSessionRequest) error {
							captures++
							require.Empty(t, request.OriginalOperationID, "the first same-epoch recovery starts one exact capture")
							return nil
						},
						onDelete: func(harnessv2.DeleteRuntimeSessionRequest) { deletes++ },
						onPrompt: func(harnessv2.StartPromptRequest) { prompts++ },
					}, func(request harnessv2.CreateRuntimeSessionRequest) {
						require.NoError(t, persistence.BindSessionCleanupIdentity(ctx, "default", "first-native-recovery", string(request.Metadata.Fence.RuntimeSessionUID)))
					})
				}, func(task *corev1alpha1.Task) {
					task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "first-native-recovery", Create: true, Append: true}
				})
			defer func() { fixture.stop() }()
			persistence = fixture.dispatcher.ResultStore.(*sqlite.Store)
			failure := &nativeDeliveryFailureStore{DurableControlStore: persistence, fail: true}
			fixture.dispatcher.Store = failure
			fixture.dispatcher.EventStore = persistence
			continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
				SessionControls: failure, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
			})
			require.NoError(t, err)
			fixture.dispatcher.Sessions = continuity
			reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
			require.NoError(t, err)
			require.NotNil(t, reserved)
			require.ErrorContains(t, fixture.dispatcher.executeReservedTask(ctx, reserved, target), "injected terminal delivery transition failure")
			current := fixture.currentTask(t, ctx)
			require.Empty(t, current.Annotations[nativeCaptureIntentAnnotation])
			require.Zero(t, captures)
			require.Zero(t, deletes, "accepted runtime evidence must survive a delivery error before capture intent")
			require.Equal(t, 1, prompts)
			control, err := persistence.GetSessionControl(ctx, "default", "first-native-recovery")
			require.NoError(t, err)
			require.NotNil(t, control.Lease)
			_, err = persistence.GetNativeSession(ctx, control.Namespace, control.SessionName, control.SessionUID)
			require.ErrorIs(t, err, store.ErrNotFound)
			failure.fail = false
			fence, err := fixture.dispatcher.Epochs.CurrentFence(ctx)
			require.NoError(t, err)
			require.NoError(t, fixture.dispatcher.transitionDelivery(ctx, fixture.attemptID, fence, store.PromptDeliveryValidating, store.PromptDeliveryNoChange, "recover-terminal-delivery", ""))
			epochs := fixture.dispatcher.Epochs
			if takeover {
				fixture.stop()
				fixture.stop = func() {}
				var stopEpoch func()
				epochs, stopEpoch = startACPRecoveryEpochManager(t, ctx, persistence, "native-recovery-takeover")
				defer stopEpoch()
				fence, err = epochs.CurrentFence(ctx)
				require.NoError(t, err)
				require.Greater(t, fence.Epoch, current.Status.Execution.ControllerEpoch)
			}
			restarted := &ACPDispatcher{Client: fixture.kubeClient, APIReader: fixture.kubeClient, Store: failure,
				ResultStore: persistence, EventStore: persistence, Snapshots: persistence, Sessions: continuity, Epochs: epochs}
			err = restarted.recoverStaleTask(ctx, current, fence)
			if takeover {
				require.ErrorIs(t, err, store.ErrNotReady, "an old pool authority cannot authorize retirement or a new capture after takeover")
				require.Zero(t, captures)
				require.Zero(t, deletes)
				retained, err := persistence.GetSessionControl(ctx, control.Namespace, control.SessionName)
				require.NoError(t, err)
				require.Equal(t, control.Lease, retained.Lease)
				require.Empty(t, fixture.currentTask(t, ctx).Annotations[nativeCaptureIntentAnnotation])
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, captures)
			require.Equal(t, 1, deletes)
			require.Equal(t, 1, prompts, "recovery must not replay an accepted prompt")
			record, err := persistence.GetNativeSession(ctx, control.Namespace, control.SessionName, control.SessionUID)
			require.NoError(t, err)
			require.Equal(t, snapshot.DataDigest, record.Snapshot.DataDigest)
			require.Equal(t, 2, record.MessageCount)
			settled, err := persistence.GetSessionControl(ctx, control.Namespace, control.SessionName)
			require.NoError(t, err)
			require.Nil(t, settled.Lease)
		})
	}
}

type nativeRecoveryPoolReadFailure struct {
	client.Reader
}

func (r nativeRecoveryPoolReadFailure) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if _, isPool := object.(*corev1alpha1.RuntimePool); isPool {
		return errors.New("injected transient pool read failure")
	}
	return r.Reader.Get(ctx, key, object, opts...)
}

func TestACPDispatcherFirstNativeCaptureRecoveryAfterPoolRetirement(t *testing.T) {
	tests := []struct {
		name       string
		poolChange string
		native     bool
		pending    bool
		settles    bool
	}{
		{name: "deleted-pool", poolChange: "deleted", settles: true},
		{name: "replaced-pool", poolChange: "uid", settles: true},
		{name: "replaced-instance", poolChange: "instance", settles: true},
		{name: "missing-active-status", poolChange: "nil-active"},
		{name: "empty-instance", poolChange: "empty-instance"},
		{name: "same-instance-changed-boot", poolChange: "boot"},
		{name: "same-instance-changed-profile", poolChange: "profile"},
		{name: "transient-pool-read", poolChange: "read-error"},
		{name: "transient-runtime-probe", poolChange: "probe-error"},
		{name: "native-checkpoint-deleted-pool", poolChange: "deleted", native: true},
		{name: "native-checkpoint-replaced-pool", poolChange: "uid", native: true},
		{name: "pending-capture-deleted-pool", poolChange: "deleted", pending: true},
		{name: "pending-capture-replaced-instance", poolChange: "instance", pending: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			snapshot := storetest.NativeSessionSnapshot(t, "native recovery after exact pool retirement")
			captures, deletes, prompts := 0, 0, 0
			probeFailure := false
			var persistence *sqlite.Store
			fixture := newTaskScopedCreateConflictFixture(t, ctx, "native-retired-"+test.name, "77777777-7777-7777-7777-777777777777",
				func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
					runtimeServer := newDispatcherRuntimeServerWithOptions(t, profile, digest, dispatcherRuntimeServerOptions{
						nativeSnapshot: &snapshot,
						onNativeCapture: func(harnessv2.CaptureNativeSessionRequest) error {
							captures++
							return &harnessv2.ClientError{Code: harnessv2.ErrorCodeSessionPoisoned, StatusCode: http.StatusConflict}
						},
						onDelete: func(harnessv2.DeleteRuntimeSessionRequest) { deletes++ },
						onPrompt: func(harnessv2.StartPromptRequest) { prompts++ },
					}, func(harnessv2.CreateRuntimeSessionRequest) {
						require.NoError(t, persistence.BindSessionCleanupIdentity(ctx, "default", "retired-native-recovery", "retired-native-owner"))
					})
					t.Cleanup(runtimeServer.Close)
					return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
						if probeFailure && (request.URL.Path == harnessv2.StatusPath || request.URL.Path == harnessv2.CapabilitiesPath) {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						runtimeServer.Config.Handler.ServeHTTP(w, request)
					}))
				}, func(task *corev1alpha1.Task) {
					task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "retired-native-recovery", Create: true, Append: true}
				})
			defer fixture.stop()
			persistence = fixture.dispatcher.ResultStore.(*sqlite.Store)
			failure := &nativeDeliveryFailureStore{DurableControlStore: persistence, fail: true}
			fixture.dispatcher.Store, fixture.dispatcher.EventStore = failure, persistence
			continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
				SessionControls: failure, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
				NewSessionUID: func() (string, error) { return "retired-native-owner", nil },
			})
			require.NoError(t, err)
			fixture.dispatcher.Sessions = continuity
			if test.native {
				staged := snapshot
				staged.RuntimeSessionUID, staged.RuntimeProfileDigest, staged.WorkingDirectory = "", "", ""
				_, err = persistence.StageNativeSessionImport(ctx, store.NativeSessionImport{
					Namespace: "default", SessionName: "retired-native-recovery", OperationID: "import-retired-native",
					RequestDigest: store.NativeSessionImportDigest("default", "retired-native-recovery", snapshot.DataDigest), Snapshot: staged,
				})
				require.NoError(t, err)
				require.NoError(t, persistence.BindSessionCleanupIdentity(ctx, "default", "retired-native-recovery", "retired-native-owner"))
			}
			reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
			require.NoError(t, err)
			require.NotNil(t, reserved)
			require.ErrorContains(t, fixture.dispatcher.executeReservedTask(ctx, reserved, target), "injected terminal delivery transition failure")
			current := fixture.currentTask(t, ctx)
			require.Empty(t, current.Annotations[nativeCaptureIntentAnnotation])
			require.Zero(t, captures)
			require.Zero(t, deletes)
			failure.fail = false
			fence, err := fixture.dispatcher.Epochs.CurrentFence(ctx)
			require.NoError(t, err)
			require.NoError(t, fixture.dispatcher.transitionDelivery(ctx, fixture.attemptID, fence, store.PromptDeliveryValidating, store.PromptDeliveryNoChange, "recover-retired-delivery", ""))
			if test.pending {
				require.ErrorContains(t, fixture.dispatcher.recoverStaleTask(ctx, current, fence), "capture private native Session")
				current = fixture.currentTask(t, ctx)
				require.NotEmpty(t, current.Annotations[nativeCaptureIntentAnnotation])
				require.Equal(t, 1, captures)
			}
			control, err := persistence.GetSessionControl(ctx, "default", "retired-native-recovery")
			require.NoError(t, err)
			require.NotNil(t, control.Lease)
			before, err := persistence.LoadTranscript(ctx, control.Namespace, control.SessionName, 100)
			require.NoError(t, err)
			pool := &corev1alpha1.RuntimePool{}
			require.NoError(t, fixture.kubeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: current.Status.Execution.RuntimePoolName}, pool))
			// The lifecycle owner proves exact authenticated quiescence and
			// persists its receipt before forgetting or replacing this pool.
			drained := dispatcherRuntimeStatusResponseForPool(harnessv2.ProfileDigest(pool.Spec.Runtime.Profile.Digest), string(pool.UID), harnessv2.RuntimeSessionDescriptor{})
			drained.Lifecycle, drained.Drain = harnessv2.SupervisorLifecycleDraining, harnessv2.DrainStatus{Requested: true}
			reconciler := &RuntimePoolReconciler{Client: fixture.kubeClient, APIReader: fixture.kubeClient}
			require.NoError(t, reconciler.recordDrainedRuntimePoolTaskCleanup(ctx, pool, pool.Status.ActiveInstance, drained))
			current = fixture.currentTask(t, ctx)
			require.True(t, runtimeSessionCleanupCompleteForUID(current, current.UID))
			switch test.poolChange {
			case "deleted":
				require.NoError(t, fixture.kubeClient.Delete(ctx, pool))
			case "uid":
				require.NoError(t, fixture.kubeClient.Delete(ctx, pool))
				pool.UID, pool.ResourceVersion = "replacement-pool-uid", ""
				require.NoError(t, fixture.kubeClient.Create(ctx, pool))
			case "instance":
				pool.Status.ActiveInstance.RuntimeInstanceID = "replacement-pod.replacement-boot"
				require.NoError(t, fixture.kubeClient.Status().Update(ctx, pool))
			case "nil-active":
				pool.Status.ActiveInstance = nil
				require.NoError(t, fixture.kubeClient.Status().Update(ctx, pool))
			case "empty-instance":
				pool.Status.ActiveInstance.RuntimeInstanceID = ""
				require.NoError(t, fixture.kubeClient.Status().Update(ctx, pool))
			case "boot":
				pool.Status.ActiveInstance.BootID = "unverified-boot"
				require.NoError(t, fixture.kubeClient.Status().Update(ctx, pool))
			case "profile":
				pool.Spec.Runtime.Profile.Digest = "sha256:" + strings.Repeat("b", 64)
				require.NoError(t, fixture.kubeClient.Update(ctx, pool))
			case "probe-error":
				probeFailure = true
			}
			restarted := &ACPDispatcher{Client: fixture.kubeClient, APIReader: fixture.kubeClient, Store: failure,
				ResultStore: persistence, EventStore: persistence, Snapshots: persistence, Sessions: continuity, Epochs: fixture.dispatcher.Epochs}
			if test.poolChange == "read-error" {
				restarted.APIReader = nativeRecoveryPoolReadFailure{Reader: fixture.kubeClient}
			}
			err = restarted.recoverStaleTask(ctx, current, fence)
			settled, controlErr := persistence.GetSessionControl(ctx, control.Namespace, control.SessionName)
			require.NoError(t, controlErr)
			history, historyErr := persistence.LoadTranscript(ctx, control.Namespace, control.SessionName, 100)
			require.NoError(t, historyErr)
			if test.settles {
				require.NoError(t, err)
				require.Nil(t, settled.Lease, "confirmed retirement before the first capture must allow canonical settlement")
				require.Len(t, history, len(before)+2)
				require.Equal(t, corev1alpha1.TaskPhaseSucceeded, fixture.currentTask(t, ctx).Status.Phase)
			} else {
				require.Error(t, err)
				require.Equal(t, control.Lease, settled.Lease)
				require.Equal(t, before, history, "native obligations and ambiguous runtime loss must retain canonical history")
			}
			if test.native {
				record, recordErr := persistence.GetNativeSession(ctx, control.Namespace, control.SessionName, control.SessionUID)
				require.NoError(t, recordErr)
				require.Equal(t, snapshot.DataDigest, record.Snapshot.DataDigest)
			} else {
				_, recordErr := persistence.GetNativeSession(ctx, control.Namespace, control.SessionName, control.SessionUID)
				require.ErrorIs(t, recordErr, store.ErrNotFound)
			}
			captureCount := 0
			if test.pending {
				captureCount = 1
			}
			require.Equal(t, captureCount, captures, "recovery must not recapture or create an intent for a lost or ambiguous runtime")
			require.Zero(t, deletes, "durable retirement proof must prevent a DELETE against the replacement runtime")
			require.Equal(t, 1, prompts, "accepted prompts must never replay")
		})
	}
}

func TestACPDispatcherPoisonedWorkspaceFailureDoesNotAttemptNativeCapture(t *testing.T) {
	for _, validationError := range []bool{false, true} {
		name := "read-only-modified"
		if validationError {
			name = "validation-error"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			snapshot := storetest.NativeSessionSnapshot(t, "unsupported poisoned workspace has no native checkpoint")
			captures, deletes := 0, 0
			fixture := newTaskScopedCreateConflictFixture(t, ctx, "native-workspace-"+name, "77777777-7777-7777-7777-777777777777",
				func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
					options := dispatcherRuntimeServerOptions{
						nativeSnapshot: &snapshot, workspaceDeltaState: harnessv2.WorkspaceDeltaReadOnlyModified,
						onDelete: func(harnessv2.DeleteRuntimeSessionRequest) { deletes++ },
						onNativeCapture: func(harnessv2.CaptureNativeSessionRequest) error {
							captures++
							return &harnessv2.ClientError{Code: harnessv2.ErrorCodeSessionPoisoned, StatusCode: http.StatusConflict}
						},
					}
					if validationError {
						options.workspaceDeltaFailure = &harnessv2.ErrorResponse{Protocol: harnessv2.ProtocolVersion,
							Code: harnessv2.ErrorCodeSessionPoisoned, Message: "workspace validation failed"}
					}
					return newDispatcherRuntimeServerWithOptions(t, profile, digest, options)
				}, func(task *corev1alpha1.Task) {
					task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "poisoned-workspace", Create: true, Append: true}
				})
			defer fixture.stop()
			persistence := fixture.dispatcher.ResultStore.(*sqlite.Store)
			fixture.dispatcher.EventStore = persistence
			continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
				SessionControls: persistence, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
			})
			require.NoError(t, err)
			fixture.dispatcher.Sessions = continuity
			reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
			require.NoError(t, err)
			require.NotNil(t, reserved)
			if err := fixture.dispatcher.executeReservedTask(ctx, reserved, target); err != nil {
				require.ErrorIs(t, err, store.ErrNotReady, "the existing deferred cleanup can await reconstructed terminal settlement")
			}
			current := fixture.currentTask(t, ctx)
			require.Equal(t, corev1alpha1.TaskPhaseFailed, current.Status.Phase)
			require.Equal(t, corev1alpha1.TaskExecutionOutcomeSucceeded, current.Status.Execution.Outcome)
			if validationError {
				require.Equal(t, corev1alpha1.TaskDeliveryStateDeliveryConflict, current.Status.Delivery.State)
			} else {
				require.Equal(t, corev1alpha1.TaskDeliveryStateReadOnlyWorkspaceModified, current.Status.Delivery.State)
			}
			fence, err := fixture.dispatcher.Epochs.CurrentFence(ctx)
			require.NoError(t, err)
			require.NoError(t, fixture.dispatcher.recoverStaleTask(ctx, current, fence))
			require.Empty(t, current.Annotations[nativeCaptureIntentAnnotation])
			require.Zero(t, captures)
			require.LessOrEqual(t, deletes, 1)
			control, err := persistence.GetSessionControl(ctx, "default", "poisoned-workspace")
			require.NoError(t, err)
			require.Nil(t, control.Lease)
			_, err = persistence.GetNativeSession(ctx, control.Namespace, control.SessionName, control.SessionUID)
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestACPDispatcherNativeContinuityRetainsUnsupportedTerminalState(t *testing.T) {
	for _, test := range []struct {
		name            string
		terminal        harnessv2.EventType
		workspaceState  harnessv2.WorkspaceDeltaState
		validationError bool
		wantExecution   store.PromptExecutionState
	}{
		{name: "read-only-modified", workspaceState: harnessv2.WorkspaceDeltaReadOnlyModified, wantExecution: store.PromptExecutionSucceeded},
		{name: "validation-error", validationError: true, wantExecution: store.PromptExecutionSucceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			snapshot := storetest.NativeSessionSnapshot(t, "native continuity preceding an unsupported terminal state")
			captures, deletes, prompts := 0, 0, 0
			fixture := newTaskScopedCreateConflictFixture(t, ctx, "native-terminal-"+test.name, "77777777-7777-7777-7777-777777777777",
				func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
					options := dispatcherRuntimeServerOptions{
						nativeSnapshot: &snapshot, workspaceDeltaState: test.workspaceState,
						terminalEvents: map[harnessv2.PromptID]harnessv2.EventType{"prompt-77777777-7777-7777-7777-777777777777-1": test.terminal},
						onPrompt: func(request harnessv2.StartPromptRequest) {
							prompts++
							require.Len(t, request.Input.Content, 1, "restored prompts must omit canonical bootstrap")
						},
						onDelete: func(harnessv2.DeleteRuntimeSessionRequest) { deletes++ },
						onNativeCapture: func(harnessv2.CaptureNativeSessionRequest) error {
							captures++
							return &harnessv2.ClientError{Code: harnessv2.ErrorCodeSessionPoisoned, StatusCode: http.StatusConflict}
						},
					}
					if test.validationError {
						options.workspaceDeltaFailure = &harnessv2.ErrorResponse{Protocol: harnessv2.ProtocolVersion,
							Code: harnessv2.ErrorCodeSessionPoisoned, Message: "workspace validation failed"}
					}
					return newDispatcherRuntimeServerWithOptions(t, profile, digest, options, func(request harnessv2.CreateRuntimeSessionRequest) {
						require.NotNil(t, request.NativeRestore)
					})
				}, func(task *corev1alpha1.Task) {
					task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "native-terminal", Create: true, Append: true}
				})
			defer fixture.stop()
			persistence := fixture.dispatcher.ResultStore.(*sqlite.Store)
			fixture.dispatcher.EventStore = persistence
			continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
				SessionControls: persistence, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
				NewSessionUID: func() (string, error) { return "native-terminal-owner", nil },
			})
			require.NoError(t, err)
			fixture.dispatcher.Sessions = continuity
			staged := snapshot
			staged.RuntimeSessionUID, staged.RuntimeProfileDigest, staged.WorkingDirectory = "", "", ""
			_, err = persistence.StageNativeSessionImport(ctx, store.NativeSessionImport{
				Namespace: "default", SessionName: "native-terminal", OperationID: "import-native-terminal",
				RequestDigest: store.NativeSessionImportDigest("default", "native-terminal", snapshot.DataDigest), Snapshot: staged,
			})
			require.NoError(t, err)
			require.NoError(t, persistence.BindSessionCleanupIdentity(ctx, "default", "native-terminal", "native-terminal-owner"))
			before, err := persistence.GetNativeSession(ctx, "default", "native-terminal", "native-terminal-owner")
			require.NoError(t, err)
			transcript, err := persistence.LoadTranscript(ctx, "default", "native-terminal", 100)
			require.NoError(t, err)
			reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
			require.NoError(t, err)
			require.NotNil(t, reserved)
			require.ErrorIs(t, fixture.dispatcher.executeReservedTask(ctx, reserved, target), errNativeSessionCheckpointRequired)
			current := fixture.currentTask(t, ctx)
			attempt, err := persistence.GetPromptAttempt(ctx, fixture.attemptID)
			require.NoError(t, err)
			require.Equal(t, test.wantExecution, attempt.ExecutionState)
			control, err := persistence.GetSessionControl(ctx, "default", "native-terminal")
			require.NoError(t, err)
			require.NotNil(t, control.Lease)
			key := store.SessionTurnKey{SessionUID: attempt.SessionUID, LeaseGeneration: attempt.SessionLeaseGeneration,
				TaskUID: attempt.Key.TaskUID, Attempt: attempt.Key.Attempt, PromptID: attempt.Key.PromptID}
			turnID, err := key.CanonicalID()
			require.NoError(t, err)
			fence, err := fixture.dispatcher.Epochs.CurrentFence(ctx)
			require.NoError(t, err)
			restarted := &ACPDispatcher{Client: fixture.kubeClient, APIReader: fixture.kubeClient, Store: persistence,
				ResultStore: persistence, EventStore: persistence, Snapshots: persistence, Sessions: continuity, Epochs: fixture.dispatcher.Epochs}
			for range 2 {
				require.ErrorIs(t, restarted.recoverStaleTask(ctx, current, fence), errNativeSessionCheckpointRequired)
				retained, err := persistence.GetSessionControl(ctx, control.Namespace, control.SessionName)
				require.NoError(t, err)
				require.Equal(t, control.Lease, retained.Lease)
				turn, err := persistence.GetSessionTurn(ctx, turnID)
				require.NoError(t, err)
				require.Equal(t, store.SessionTurnOpen, turn.State)
				after, err := persistence.GetNativeSession(ctx, control.Namespace, control.SessionName, control.SessionUID)
				require.NoError(t, err)
				require.Equal(t, before, after)
				history, err := persistence.LoadTranscript(ctx, control.Namespace, control.SessionName, 100)
				require.NoError(t, err)
				require.Equal(t, transcript, history, "unsupported settlement must not advance canonical history")
			}
			require.Zero(t, captures, "failed or poisoned runtime state cannot start a native capture")
			require.Zero(t, deletes, "the original runtime evidence must remain available")
			require.Equal(t, 1, prompts, "accepted prompts must never replay during recovery")
		})
	}
}

func TestACPDispatcherNonAppendingNativeRuntimeRetirementSurvivesRestart(t *testing.T) {
	for _, beforeDelete := range []bool{false, true} {
		name := "delete-failed"
		if beforeDelete {
			name = "before-delete"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			snapshot := storetest.NativeSessionSnapshot(t, "non-appending Task must retire native runtime")
			captures, deletes, prompts := 0, 0, 0
			failDelete := !beforeDelete
			fixture := newTaskScopedCreateConflictFixture(t, ctx, "native-nonappend-"+name, "77777777-7777-7777-7777-777777777777",
				func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
					return newDispatcherRuntimeServerWithOptions(t, profile, digest, dispatcherRuntimeServerOptions{
						nativeSnapshot:  &snapshot,
						onPrompt:        func(harnessv2.StartPromptRequest) { prompts++ },
						onNativeCapture: func(harnessv2.CaptureNativeSessionRequest) error { captures++; return nil },
						onDelete:        func(harnessv2.DeleteRuntimeSessionRequest) { deletes++ },
						deleteFailure: func() *harnessv2.ErrorResponse {
							if failDelete {
								return &harnessv2.ErrorResponse{Protocol: harnessv2.ProtocolVersion, Code: harnessv2.ErrorCodeSessionPoisoned, Message: "injected retirement failure"}
							}
							return nil
						},
					})
				}, func(task *corev1alpha1.Task) {
					task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "native-nonappend", Create: true, Append: false}
				})
			defer fixture.stop()
			persistence := fixture.dispatcher.ResultStore.(*sqlite.Store)
			failure := &nativeDeliveryFailureStore{DurableControlStore: persistence, fail: beforeDelete}
			fixture.dispatcher.Store, fixture.dispatcher.EventStore = failure, persistence
			continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
				SessionControls: failure, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
			})
			require.NoError(t, err)
			fixture.dispatcher.Sessions = continuity
			reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
			require.NoError(t, err)
			require.NotNil(t, reserved)
			err = fixture.dispatcher.executeReservedTask(ctx, reserved, target)
			if beforeDelete {
				require.ErrorContains(t, err, "injected terminal delivery transition failure")
				require.Zero(t, deletes)
			} else {
				require.ErrorContains(t, err, "delete RuntimeSession")
				require.Positive(t, deletes)
			}
			current := fixture.currentTask(t, ctx)
			require.True(t, current.Status.Execution.RuntimeSessionRecreationPending, "accepted native runtime retirement must survive interruption before DELETE")
			binding := fixture.dispatcher.currentRuntimeSessionBinding(current.Status.Execution.RuntimeSessionUID)
			require.NotNil(t, binding)
			require.True(t, binding.RecreationRequired, "the live binding must forbid reuse before canonical lease release")
			require.Empty(t, current.Annotations[nativeCaptureIntentAnnotation])
			require.False(t, taskScopedRuntimeSessionCleanupComplete(current))
			failure.fail, failDelete = false, false
			fence, err := fixture.dispatcher.Epochs.CurrentFence(ctx)
			require.NoError(t, err)
			if beforeDelete {
				require.NoError(t, fixture.dispatcher.transitionDelivery(ctx, fixture.attemptID, fence, store.PromptDeliveryValidating, store.PromptDeliveryReadValidated, "recover-nonappend-delivery", ""))
			}
			priorDeletes := deletes
			restarted := &ACPDispatcher{Client: fixture.kubeClient, APIReader: fixture.kubeClient, Store: failure,
				ResultStore: persistence, EventStore: persistence, Snapshots: persistence, Sessions: continuity, Epochs: fixture.dispatcher.Epochs}
			require.NoError(t, restarted.recoverStaleTask(ctx, current, fence))
			settled := fixture.currentTask(t, ctx)
			require.True(t, taskScopedRuntimeSessionCleanupComplete(settled))
			require.Equal(t, priorDeletes+1, deletes)
			require.NoError(t, restarted.recoverStaleTask(ctx, settled, fence), "committed settlement and retirement retries must remain idempotent")
			require.Equal(t, priorDeletes+1, deletes)
			require.Equal(t, 1, prompts)
			require.Zero(t, captures, "append:false must not checkpoint unrecorded prompt history")
			control, err := persistence.GetSessionControl(ctx, "default", "native-nonappend")
			require.NoError(t, err)
			require.Nil(t, control.Lease)
			transcript, err := persistence.LoadTranscript(ctx, control.Namespace, control.SessionName, 100)
			require.NoError(t, err)
			require.Empty(t, transcript)
		})
	}
}

type nativeAcceptedStatusFailureClient struct {
	client.Client
	failed bool
}

func (c *nativeAcceptedStatusFailureClient) Status() client.SubResourceWriter {
	return &nativeAcceptedStatusFailureWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type nativeAcceptedStatusFailureWriter struct {
	client.SubResourceWriter
	parent *nativeAcceptedStatusFailureClient
}

func (w *nativeAcceptedStatusFailureWriter) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
	if task, ok := object.(*corev1alpha1.Task); ok && task.Status.Execution != nil &&
		task.Status.Execution.State == corev1alpha1.TaskExecutionStateRunning && !w.parent.failed {
		w.parent.failed = true
		return errors.New("injected accepted status write failure")
	}
	return w.SubResourceWriter.Patch(ctx, object, patch, options...)
}

func TestACPDispatcherNonAppendingNativeRetirementSurvivesAcceptedStatusWriteFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	snapshot := storetest.NativeSessionSnapshot(t, "native retirement survives acceptance status failure")
	prompts, captures, deletes := 0, 0, 0
	fixture := newTaskScopedCreateConflictFixture(t, ctx, "native-accepted-status-failure", "77777777-7777-7777-7777-777777777777",
		func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, kubeClient *client.Client) *httptest.Server {
			return newDispatcherRuntimeServerWithOptions(t, profile, digest, dispatcherRuntimeServerOptions{
				nativeSnapshot: &snapshot,
				onPrompt: func(harnessv2.StartPromptRequest) {
					prompts++
					current := &corev1alpha1.Task{}
					require.NoError(t, (*kubeClient).Get(ctx, client.ObjectKey{Namespace: "default", Name: "native-accepted-status-failure"}, current))
					require.Equal(t, corev1alpha1.TaskExecutionStateSubmitting, current.Status.Execution.State)
					require.True(t, current.Status.Execution.RuntimeSessionRecreationPending, "retirement must be durable before the provider can accept")
				},
				onNativeCapture: func(harnessv2.CaptureNativeSessionRequest) error { captures++; return nil },
				onDelete:        func(harnessv2.DeleteRuntimeSessionRequest) { deletes++ },
			})
		}, func(task *corev1alpha1.Task) {
			task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "native-status-failure", Create: true, Append: false}
		})
	defer fixture.stop()
	persistence := fixture.dispatcher.ResultStore.(*sqlite.Store)
	fixture.dispatcher.EventStore = persistence
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: persistence, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
	})
	require.NoError(t, err)
	fixture.dispatcher.Sessions = continuity
	failedStatus := &nativeAcceptedStatusFailureClient{Client: fixture.kubeClient}
	fixture.dispatcher.Client = failedStatus
	reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	require.ErrorIs(t, fixture.dispatcher.executeReservedTask(ctx, reserved, target), store.ErrNotReady)
	require.True(t, failedStatus.failed)
	current := fixture.currentTask(t, ctx)
	require.True(t, current.Status.Execution.RuntimeSessionRecreationPending)
	require.Zero(t, deletes)
	fence, err := fixture.dispatcher.Epochs.CurrentFence(ctx)
	require.NoError(t, err)
	restarted := &ACPDispatcher{Client: fixture.kubeClient, APIReader: fixture.kubeClient, Store: persistence,
		ResultStore: persistence, EventStore: persistence, Snapshots: persistence, Sessions: continuity, Epochs: fixture.dispatcher.Epochs}
	require.NoError(t, restarted.recoverStaleTask(ctx, current, fence))
	require.True(t, runtimeSessionCleanupCompleteForUID(fixture.currentTask(t, ctx), current.UID))
	require.Equal(t, 1, deletes)
	require.Equal(t, 1, prompts, "an accepted prompt must not replay after a status write failure")
	require.Zero(t, captures)
}

func TestNativeCapturePersistsUnderCanonicalSessionOwner(t *testing.T) {
	ctx := t.Context()
	persistence, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "native-owner.db"))
	defer closeStore()
	cipher, err := sqlite.NewAgentExecutionSnapshotCipher([]byte(strings.Repeat("k", 32)))
	require.NoError(t, err)
	require.NoError(t, persistence.SetAgentExecutionSnapshotCipher(cipher))
	continuity := newACPSessionTestContinuity(t, persistence, ACPBootstrapLimits{})
	control := ensureACPSessionForTest(t, continuity, fence, "canonical-owner")
	require.NoError(t, persistence.BindSessionCleanupIdentity(ctx, control.Namespace, control.SessionName, control.SessionUID))
	taskUID := "77777777-7777-7777-7777-777777777777"
	turn, attempt := openACPSessionTurnForTest(t, continuity, persistence, fence, control, taskUID, "native-owner-prompt", "remember this")
	completeACPAttemptExecutionForTest(t, persistence, fence, attempt, false)
	snapshot := storetest.NativeSessionSnapshot(t, "native capture belongs to the canonical turn")
	snapshot.RuntimeSessionUID = "separate-runtime-incarnation"
	runtimeFence := harnessv2.Fence{
		RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 1, RuntimeInstanceID: "pod-uid.boot-id", SupervisorBootID: "boot-id",
		ControllerEpoch: uint64(fence.Epoch), RuntimeProfileDigest: snapshot.RuntimeProfileDigest,
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
		RuntimeSessionUID:          snapshot.RuntimeSessionUID, RuntimeSessionGeneration: 1,
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: control.Namespace, Name: "native-owner-task", UID: types.UID(taskUID)},
		Spec:       corev1alpha1.TaskSpec{SessionRef: &corev1alpha1.SessionReference{Name: control.SessionName, Append: true}},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			Attempt: 1, PromptID: attempt.Key.PromptID, RuntimeSessionUID: string(runtimeFence.RuntimeSessionUID),
			RuntimeSessionGeneration: 1, RuntimeInstanceID: string(runtimeFence.RuntimeInstanceID),
			RuntimeSessionSupervisorBootID: string(runtimeFence.SupervisorBootID), RuntimeSessionProfileDigest: string(runtimeFence.RuntimeProfileDigest),
		}},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	dispatcher := &ACPDispatcher{Client: kubeClient, APIReader: kubeClient, Store: persistence, Sessions: continuity}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request harnessv2.CaptureNativeSessionRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v2/runtime-sessions/"+runtimeSessionID(runtimeFence)+"/native-session", r.URL.Path)
		require.Equal(t, runtimeFence, request.Metadata.Fence)
		now := time.Now().UTC()
		writeDispatcherJSON(w, harnessv2.CaptureNativeSessionResponse{
			Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
			Snapshot: snapshot, Session: harnessv2.RuntimeSessionDescriptor{
				RuntimeSessionID: harnessv2.RuntimeSessionID(runtimeSessionID(runtimeFence)), RuntimeSessionUID: runtimeFence.RuntimeSessionUID,
				Generation: 1, RuntimeInstanceID: runtimeFence.RuntimeInstanceID, SupervisorBootID: runtimeFence.SupervisorBootID,
				RuntimeProfileDigest: runtimeFence.RuntimeProfileDigest, State: harnessv2.RuntimeSessionStatePoisoned,
				ProviderSessionID: snapshot.ProviderSessionID, CreatedAt: now, LastTransitionAt: now,
				WorkspaceBaseline: harnessv2.WorkspaceBaseline{RepositoryIdentity: "workspace:native-owner", Revision: "empty", TreeDigest: testControlDigestForDispatcher("native-owner-tree")},
			},
		})
	}))
	defer server.Close()
	runtimeClient, err := harnessv2.NewClient(server.URL, harnessv2.WithControllerBearerToken(strings.Repeat("t", 32)), harnessv2.WithOperationCapabilitySecret([]byte(strings.Repeat("s", 32))))
	require.NoError(t, err)
	session := &acpTaskSession{Turn: turn, Binding: ACPRuntimeSessionBinding{SessionUID: control.SessionUID, Generation: 1}}
	require.NoError(t, dispatcher.captureTaskNativeSession(ctx, runtimeClient, task, runtimeFence, session))
	require.Equal(t, control.SessionUID, session.NativeCapture.SessionUID)
	require.Equal(t, runtimeFence.RuntimeSessionUID, session.NativeCapture.Snapshot.RuntimeSessionUID)
	finalized, err := continuity.FinalizeAssistantResult(ctx, ACPFinalizeAssistantRequest{
		SessionTurn: *turn, Fence: fence, AssistantResult: "known native result", NativeSession: session.NativeCapture,
		Projection: acpSessionProjectionForTest("native-owner", "Succeeded"),
	})
	require.NoError(t, err)
	require.Nil(t, finalized.Session.Lease)
	record, err := persistence.GetNativeSession(ctx, control.Namespace, control.SessionName, control.SessionUID)
	require.NoError(t, err)
	require.Equal(t, 2, record.MessageCount)
	require.Equal(t, runtimeFence.RuntimeSessionUID, record.Snapshot.RuntimeSessionUID)
	_, err = persistence.GetNativeSession(ctx, control.Namespace, control.SessionName, string(runtimeFence.RuntimeSessionUID))
	require.ErrorIs(t, err, store.ErrConflict)
}

func TestACPDispatcherRejectsIncompatibleNativeRuntimeBeforeSessionMutation(t *testing.T) {
	for _, runtimeType := range []corev1alpha1.AgentRuntimeType{corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude} {
		t.Run(string(runtimeType), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			snapshot := storetest.NativeSessionSnapshot(t, "import must survive an incompatible Task")
			creates, deletes, prompts := 0, 0, 0
			fixture := newTaskScopedCreateConflictFixtureForRuntime(t, ctx, "incompatible-native-"+string(runtimeType), "77777777-7777-7777-7777-777777777777", runtimeType,
				func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
					options := dispatcherRuntimeServerOptions{
						onDelete: func(harnessv2.DeleteRuntimeSessionRequest) { deletes++ },
						onPrompt: func(harnessv2.StartPromptRequest) { prompts++ },
					}
					// Codex without the capability and a non-Codex runtime advertising
					// it must both reject the import before Session mutation.
					if runtimeType == corev1alpha1.AgentRuntimeClaude {
						options.nativeSnapshot = &snapshot
					}
					return newDispatcherRuntimeServerWithOptions(t, profile, digest, options, func(harnessv2.CreateRuntimeSessionRequest) { creates++ })
				}, func(task *corev1alpha1.Task) {
					task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "staged-native", Create: true, Append: true}
				})
			defer fixture.stop()
			persistence := fixture.dispatcher.ResultStore.(*sqlite.Store)
			fixture.dispatcher.EventStore = persistence
			continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
				SessionControls: persistence, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
			})
			require.NoError(t, err)
			fixture.dispatcher.Sessions = continuity
			staged := snapshot
			staged.RuntimeSessionUID, staged.RuntimeProfileDigest, staged.WorkingDirectory = "", "", ""
			_, err = persistence.StageNativeSessionImport(ctx, store.NativeSessionImport{
				Namespace: "default", SessionName: "staged-native", OperationID: "stage-native",
				RequestDigest: store.NativeSessionImportDigest("default", "staged-native", snapshot.DataDigest), Snapshot: staged,
			})
			require.NoError(t, err)
			before, err := persistence.GetNativeSession(ctx, "default", "staged-native", "")
			require.NoError(t, err)
			reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
			require.NoError(t, err)
			require.NotNil(t, reserved)
			require.NoError(t, fixture.dispatcher.executeReservedTask(ctx, reserved, target))
			current := fixture.currentTask(t, ctx)
			require.Equal(t, corev1alpha1.TaskPhaseFailed, current.Status.Phase)
			require.Equal(t, corev1alpha1.TaskExecutionReason("NativeSessionRuntimeUnsupported"), current.Status.Execution.Reason)
			attempt, err := persistence.GetPromptAttempt(ctx, fixture.attemptID)
			require.NoError(t, err)
			require.Equal(t, store.PromptExecutionFailed, attempt.ExecutionState)
			require.Empty(t, attempt.SessionUID)
			require.Zero(t, attempt.SessionLeaseGeneration)
			_, err = persistence.GetSessionControl(ctx, "default", "staged-native")
			require.ErrorIs(t, err, store.ErrNotFound, "incompatible admission must not create or lease a Session control")
			after, err := persistence.GetNativeSession(ctx, "default", "staged-native", "")
			require.NoError(t, err)
			require.Equal(t, before, after, "the staged import must remain readable and unchanged")
			transcript, err := persistence.LoadTranscript(ctx, "default", "staged-native", 100)
			require.NoError(t, err)
			require.Empty(t, transcript)
			require.Zero(t, creates)
			require.Zero(t, deletes)
			require.Zero(t, prompts)
		})
	}
}

// A failed, cancelled, or lost prompt yields no new native state and the
// supervisor retires that runtime, so the Session settles its terminal marker
// with the existing checkpoint carried forward instead of holding the lease.
func TestACPDispatcherNativeContinuityCarriesCheckpointAcrossNonSuccess(t *testing.T) {
	for _, test := range []struct {
		name          string
		terminal      harnessv2.EventType
		wantExecution store.PromptExecutionState
	}{
		{name: "failed", terminal: harnessv2.EventFailed, wantExecution: store.PromptExecutionFailed},
		{name: "cancelled", terminal: harnessv2.EventCancelled, wantExecution: store.PromptExecutionCancelled},
		{name: "unknown", terminal: harnessv2.EventOutcomeUnknown, wantExecution: store.PromptExecutionOutcomeUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			snapshot := storetest.NativeSessionSnapshot(t, "native continuity preceding a non-success prompt")
			captures, prompts := 0, 0
			fixture := newTaskScopedCreateConflictFixture(t, ctx, "native-carried-"+test.name, "78787878-7878-7878-7878-787878787878",
				func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
					options := dispatcherRuntimeServerOptions{
						nativeSnapshot: &snapshot,
						terminalEvents: map[harnessv2.PromptID]harnessv2.EventType{"prompt-78787878-7878-7878-7878-787878787878-1": test.terminal},
						onPrompt: func(request harnessv2.StartPromptRequest) {
							prompts++
							require.Len(t, request.Input.Content, 1, "restored prompts must omit canonical bootstrap")
						},
						onNativeCapture: func(harnessv2.CaptureNativeSessionRequest) error {
							captures++
							return &harnessv2.ClientError{Code: harnessv2.ErrorCodeSessionPoisoned, StatusCode: http.StatusConflict}
						},
					}
					return newDispatcherRuntimeServerWithOptions(t, profile, digest, options, func(request harnessv2.CreateRuntimeSessionRequest) {
						require.NotNil(t, request.NativeRestore)
					})
				}, func(task *corev1alpha1.Task) {
					task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "native-carried", Create: true, Append: true}
				})
			defer fixture.stop()
			persistence := fixture.dispatcher.ResultStore.(*sqlite.Store)
			fixture.dispatcher.EventStore = persistence
			continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
				SessionControls: persistence, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
				NewSessionUID: func() (string, error) { return "native-carried-owner", nil },
			})
			require.NoError(t, err)
			fixture.dispatcher.Sessions = continuity
			staged := snapshot
			staged.RuntimeSessionUID, staged.RuntimeProfileDigest, staged.WorkingDirectory = "", "", ""
			_, err = persistence.StageNativeSessionImport(ctx, store.NativeSessionImport{
				Namespace: "default", SessionName: "native-carried", OperationID: "import-native-carried",
				RequestDigest: store.NativeSessionImportDigest("default", "native-carried", snapshot.DataDigest), Snapshot: staged,
			})
			require.NoError(t, err)
			require.NoError(t, persistence.BindSessionCleanupIdentity(ctx, "default", "native-carried", "native-carried-owner"))
			before, err := persistence.GetNativeSession(ctx, "default", "native-carried", "native-carried-owner")
			require.NoError(t, err)
			transcript, err := persistence.LoadTranscript(ctx, "default", "native-carried", 100)
			require.NoError(t, err)
			reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
			require.NoError(t, err)
			require.NotNil(t, reserved)
			require.NoError(t, fixture.dispatcher.executeReservedTask(ctx, reserved, target))
			attempt, err := persistence.GetPromptAttempt(ctx, fixture.attemptID)
			require.NoError(t, err)
			require.Equal(t, test.wantExecution, attempt.ExecutionState)
			control, err := persistence.GetSessionControl(ctx, "default", "native-carried")
			require.NoError(t, err)
			require.Nil(t, control.Lease, "a settled non-success prompt must release the Session lease")
			key := store.SessionTurnKey{SessionUID: attempt.SessionUID, LeaseGeneration: attempt.SessionLeaseGeneration,
				TaskUID: attempt.Key.TaskUID, Attempt: attempt.Key.Attempt, PromptID: attempt.Key.PromptID}
			turnID, err := key.CanonicalID()
			require.NoError(t, err)
			turn, err := persistence.GetSessionTurn(ctx, turnID)
			require.NoError(t, err)
			require.Equal(t, store.SessionTurnFinalized, turn.State)
			require.Equal(t, store.SessionTurnOutcomeMarker, turn.TerminalKind)
			require.Equal(t, store.NativeSessionCaptureDigest(before), turn.NativeSessionDigest)
			history, err := persistence.LoadTranscript(ctx, control.Namespace, control.SessionName, 100)
			require.NoError(t, err)
			require.Greater(t, len(history), len(transcript), "the terminal marker advances canonical history")
			after, err := persistence.GetNativeSession(ctx, control.Namespace, control.SessionName, control.SessionUID)
			require.NoError(t, err, "the carried checkpoint must remain loadable for the next Task")
			require.Equal(t, before.Snapshot, after.Snapshot, "a non-success prompt must not change native bytes")
			require.Equal(t, before.SourceOperationID, after.SourceOperationID)
			require.Greater(t, after.MessageCount, before.MessageCount)
			require.Zero(t, captures, "failed or lost runtime state cannot start a native capture")
			require.Equal(t, 1, prompts, "accepted prompts must never replay")
		})
	}
}

// A capture intent that expires before the runtime ever records it must not
// reconcile forever: the same runtime incarnation reports that no capture
// started, and the controller begins one fresh capture under a superseding
// intent.
func TestACPDispatcherExpiredCaptureIntentStartsFreshCaptureWhenNeverStarted(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	snapshot := storetest.NativeSessionSnapshot(t, "first native checkpoint captured after an expired intent")
	var persistence *sqlite.Store
	var requests []harnessv2.CaptureNativeSessionRequest
	failFresh := true
	fixture := newTaskScopedCreateConflictFixture(t, ctx, "native-expired-intent", "79797979-7979-7979-7979-797979797979",
		func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
			return newDispatcherRuntimeServerWithOptions(t, profile, digest, dispatcherRuntimeServerOptions{
				nativeSnapshot: &snapshot,
				onNativeCapture: func(request harnessv2.CaptureNativeSessionRequest) error {
					requests = append(requests, request)
					if request.OriginalOperationID != "" {
						return &harnessv2.ClientError{Code: harnessv2.ErrorCodeNativeCaptureNotStarted, StatusCode: http.StatusConflict}
					}
					if failFresh {
						return &harnessv2.ClientError{Code: harnessv2.ErrorCodeAlreadyAccepted, StatusCode: http.StatusConflict, Retryable: true}
					}
					return nil
				},
			}, func(request harnessv2.CreateRuntimeSessionRequest) {
				require.NoError(t, persistence.BindSessionCleanupIdentity(ctx, "default", "native-expired-intent", string(request.Metadata.Fence.RuntimeSessionUID)))
			})
		}, func(task *corev1alpha1.Task) {
			task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "native-expired-intent", Create: true, Append: true}
		})
	defer fixture.stop()
	persistence = fixture.dispatcher.ResultStore.(*sqlite.Store)
	failure := &nativeDeliveryFailureStore{DurableControlStore: persistence, fail: true}
	fixture.dispatcher.Store = failure
	fixture.dispatcher.EventStore = persistence
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: failure, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
	})
	require.NoError(t, err)
	fixture.dispatcher.Sessions = continuity
	reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	require.ErrorContains(t, fixture.dispatcher.executeReservedTask(ctx, reserved, target), "injected terminal delivery transition failure")
	failure.fail = false
	fence, err := fixture.dispatcher.Epochs.CurrentFence(ctx)
	require.NoError(t, err)
	require.NoError(t, fixture.dispatcher.transitionDelivery(ctx, fixture.attemptID, fence, store.PromptDeliveryValidating, store.PromptDeliveryNoChange, "recover-terminal-delivery", ""))
	restarted := &ACPDispatcher{Client: fixture.kubeClient, APIReader: fixture.kubeClient, Store: failure,
		ResultStore: persistence, EventStore: persistence, Snapshots: persistence, Sessions: continuity, Epochs: fixture.dispatcher.Epochs}

	// The first recovery persists the intent, but the runtime never records it.
	require.Error(t, restarted.recoverStaleTask(ctx, fixture.currentTask(t, ctx), fence))
	require.Len(t, requests, 1)
	original := requests[0]
	require.Empty(t, original.OriginalOperationID)
	current := fixture.currentTask(t, ctx)
	var intent nativeCaptureIntent
	require.NoError(t, json.Unmarshal([]byte(current.Annotations[nativeCaptureIntentAnnotation]), &intent))
	require.Equal(t, original.Metadata.RequestDigest, intent.Request.Metadata.RequestDigest)
	intent.Request.Metadata.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	expired, err := json.Marshal(intent)
	require.NoError(t, err)
	current.Annotations[nativeCaptureIntentAnnotation] = string(expired)
	require.NoError(t, fixture.kubeClient.Update(ctx, current))

	failFresh = false
	require.NoError(t, restarted.recoverStaleTask(ctx, fixture.currentTask(t, ctx), fence))
	require.Len(t, requests, 3, "one reconciliation attempt, then exactly one fresh capture")
	require.Equal(t, original.Metadata.OperationID, requests[1].OriginalOperationID)
	require.Equal(t, original.Metadata.RequestDigest, requests[1].OriginalRequestDigest)
	fresh := requests[2]
	require.Empty(t, fresh.OriginalOperationID)
	require.NotEqual(t, original.Metadata.OperationID, fresh.Metadata.OperationID)
	var superseding nativeCaptureIntent
	require.NoError(t, json.Unmarshal([]byte(fixture.currentTask(t, ctx).Annotations[nativeCaptureIntentAnnotation]), &superseding))
	require.Equal(t, fresh.Metadata.RequestDigest, superseding.Request.Metadata.RequestDigest)
	require.Equal(t, original.Metadata.RequestDigest, superseding.Supersedes)
	require.Nil(t, superseding.Reconciliation)
	control, err := persistence.GetSessionControl(ctx, "default", "native-expired-intent")
	require.NoError(t, err)
	require.Nil(t, control.Lease, "the fresh capture settles the Session")
	record, err := persistence.GetNativeSession(ctx, control.Namespace, control.SessionName, control.SessionUID)
	require.NoError(t, err)
	require.Equal(t, snapshot.DataDigest, record.Snapshot.DataDigest)
	require.Equal(t, string(fresh.Metadata.OperationID), record.SourceOperationID)
}
