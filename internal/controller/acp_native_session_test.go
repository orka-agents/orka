package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/events"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestNativeRestoreRequiresExactResumeAndRebindsCurrentPolicy(t *testing.T) {
	snapshot := storetest.NativeSessionSnapshot(t, "existing private context")
	session := &acpTaskSession{NativeSession: &store.NativeSessionRecord{Snapshot: snapshot}, Bootstrap: &ACPBootstrapTranscript{}}
	request := harnessv2.CreateRuntimeSessionRequest{Profile: harnessv2.RuntimeProfile{ProviderKind: "codex"}, Metadata: harnessv2.MutationMetadata{Fence: harnessv2.Fence{
		RuntimeSessionUID: "new-owner", RuntimeProfileDigest: harnessv2.ProfileDigest("sha256:" + strings.Repeat("b", 64)),
	}}}
	_, err := nativeRestoreForTask(session, request, false)
	require.ErrorIs(t, err, store.ErrConflict)
	restore, err := nativeRestoreForTask(session, request, true)
	require.NoError(t, err)
	require.Equal(t, request.Metadata.Fence.RuntimeSessionUID, restore.Snapshot.RuntimeSessionUID)
	require.Equal(t, request.Metadata.Fence.RuntimeProfileDigest, restore.Snapshot.RuntimeProfileDigest)
	require.Equal(t, "/workspace", restore.Snapshot.WorkingDirectory)
	require.Equal(t, snapshot.Data, restore.Snapshot.Data)
	require.Equal(t, snapshot.ProviderSessionID, restore.Snapshot.ProviderSessionID)
	require.Equal(t, snapshot.RuntimeProfileDigest, session.NativeSession.Snapshot.RuntimeProfileDigest)
	require.ErrorIs(t, verifyTaskNativeRestoration(session, nil), store.ErrConflict)
	require.NotNil(t, session.Bootstrap)
	proof := &harnessv2.NativeSessionRestoration{DataDigest: snapshot.DataDigest, ProviderSessionID: snapshot.ProviderSessionID, Loaded: true}
	wrong := *proof
	wrong.DataDigest = "sha256:" + strings.Repeat("c", 64)
	require.ErrorIs(t, verifyTaskNativeRestoration(session, &wrong), store.ErrConflict)
	require.NotNil(t, session.Bootstrap)
	require.NoError(t, verifyTaskNativeRestoration(session, proof))
	require.Nil(t, session.Bootstrap)
}

type nativeFinalizationFailureStore struct {
	store.DurableControlStore
	fail bool
}

func (s *nativeFinalizationFailureStore) FinalizeSessionTurn(ctx context.Context, request store.FinalizeSessionTurnRequest) (*store.SessionTurn, error) {
	if s.fail && request.NativeSession != nil {
		return nil, errors.New("injected atomic checkpoint persistence failure")
	}
	return s.DurableControlStore.FinalizeSessionTurn(ctx, request)
}

// This executes normal Task admission and prompt settlement. The runtime's
// deletion hook observes the private store, proving persistence precedes any
// cleanup through the production controller path.
func TestACPDispatcherCapturesNativeSessionBeforeDeletion(t *testing.T) {
	for _, failSave := range []bool{false, true} {
		name := "saved"
		if failSave {
			name = "save-failure"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			snapshot := storetest.NativeSessionSnapshot(t, "private completed native conversation")
			var persistence *sqlite.Store
			var firstCapture *harnessv2.CaptureNativeSessionRequest
			captureCalls, deleteCalls := 0, 0
			var continuationCreate *harnessv2.CreateRuntimeSessionRequest
			var continuationPrompt *harnessv2.StartPromptRequest
			fixture := newTaskScopedCreateConflictFixture(t, ctx, "native-"+name, types.UID("77777777-7777-7777-7777-777777777777"),
				func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
					return newDispatcherRuntimeServerWithOptions(t, profile, digest, dispatcherRuntimeServerOptions{
						nativeSnapshot: &snapshot,
						onPrompt: func(request harnessv2.StartPromptRequest) {
							if request.Metadata.TaskUID != "77777777-7777-7777-7777-777777777777" {
								copyRequest := request
								continuationPrompt = &copyRequest
							}
						},
						onNativeCapture: func(request harnessv2.CaptureNativeSessionRequest) error {
							captureCalls++
							if firstCapture == nil {
								copyRequest := request
								firstCapture = &copyRequest
							} else if firstCapture.Metadata.TaskUID == request.Metadata.TaskUID {
								require.Equal(t, firstCapture.Metadata.OperationID, request.Metadata.OperationID)
								require.Equal(t, firstCapture.Metadata.RequestDigest, request.Metadata.RequestDigest)
							}
							control, err := persistence.GetSessionControl(ctx, "default", "native-session")
							require.NoError(t, err)
							require.NotNil(t, control.Lease, "capture must hold the real Task Session lease")
							return nil
						},
						onDelete: func(request harnessv2.DeleteRuntimeSessionRequest) {
							deleteCalls++
							record, err := persistence.GetNativeSession(ctx, "default", "native-session", string(request.Metadata.Fence.RuntimeSessionUID))
							require.NoError(t, err, "cleanup must follow native persistence")
							require.Equal(t, snapshot.DataDigest, record.Snapshot.DataDigest)
							require.Equal(t, 2*deleteCalls, record.MessageCount)
						},
					}, func(request harnessv2.CreateRuntimeSessionRequest) {
						require.NoError(t, persistence.BindSessionCleanupIdentity(ctx, "default", "native-session", string(request.Metadata.Fence.RuntimeSessionUID)))
						if request.NativeRestore != nil {
							copyRequest := request
							continuationCreate = &copyRequest
						}
					})
				}, func(task *corev1alpha1.Task) {
					task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "native-session", Create: true, Append: true}
				})
			defer fixture.stop()
			persistence = fixture.dispatcher.ResultStore.(*sqlite.Store)
			failing := &nativeFinalizationFailureStore{DurableControlStore: persistence, fail: failSave}
			fixture.dispatcher.Store = failing
			fixture.dispatcher.EventStore = persistence
			continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
				SessionControls: failing, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
			})
			require.NoError(t, err)
			fixture.dispatcher.Sessions = continuity
			reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
			require.NoError(t, err)
			require.NotNil(t, reserved)
			err = fixture.dispatcher.executeReservedTask(ctx, reserved, target)
			if !failSave {
				require.NoError(t, err)
				require.Equal(t, 1, captureCalls)
				require.Equal(t, 1, deleteCalls)
				require.True(t, taskScopedRuntimeSessionCleanupComplete(fixture.currentTask(t, ctx)))
				current := fixture.currentTask(t, ctx)
				recoveryFence, err := fixture.dispatcher.Epochs.CurrentFence(ctx)
				require.NoError(t, err)
				restarted := &ACPDispatcher{Client: fixture.kubeClient, APIReader: fixture.kubeClient, Store: failing,
					ResultStore: persistence, EventStore: persistence, Snapshots: persistence, Sessions: continuity, Epochs: fixture.dispatcher.Epochs}
				require.NoError(t, restarted.recoverStaleTask(ctx, current, recoveryFence), "the committed native settlement receipt must resume without an in-memory capture")
				require.Equal(t, 1, captureCalls, "committed native settlement must not recapture")
				require.Equal(t, 1, deleteCalls)
				// A subsequent real Task receives native continuation only once,
				// with no canonical conversation replay in its prompt.
				continued := fixture.task.DeepCopy()
				continued.Name = "native-continued"
				continued.UID = "88888888-8888-8888-8888-888888888888"
				continued.ResourceVersion = ""
				continued.Spec.Prompt = "continue the native conversation"
				continued.Spec.SessionRef.Create = false
				continued.Status = corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: 1, Execution: &corev1alpha1.TaskExecutionStatus{
					State: corev1alpha1.TaskExecutionStateQueued, Attempt: 1, PromptID: "prompt-native-continuation",
					RuntimePoolName: fixture.task.Status.Execution.RuntimePoolName, RuntimePoolUID: "pool-uid", ControllerEpoch: 1,
					RequestDigest: testControlDigestForDispatcher("native-continuation"),
				}}
				require.NoError(t, fixture.kubeClient.Create(ctx, continued))
				agent := &corev1alpha1.Agent{}
				require.NoError(t, fixture.kubeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "agent"}, agent))
				continued = prepareBoundACPDispatcherTaskForTest(t, ctx, fixture.kubeClient, fixture.kubeClient.Scheme(), persistence, continued, agent, ACPRuntimeImages{Codex: "docker.io/example/acp@sha256:" + strings.Repeat("a", 64)})
				fence, err := fixture.dispatcher.Epochs.CurrentFence(ctx)
				require.NoError(t, err)
				key := store.PromptAttemptKey{Namespace: continued.Namespace, TaskUID: string(continued.UID), Attempt: 1, PromptID: continued.Status.Execution.PromptID}
				id, err := key.CanonicalID()
				require.NoError(t, err)
				_, err = persistence.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{ID: id, Key: key,
					RequestDigest: continued.Status.Execution.RequestDigest, BindingDigest: continued.Status.AgentExecutionBinding.BindingDigest, SnapshotDigest: continued.Status.AgentExecutionBinding.Snapshot.Digest,
					ExecutionState: store.PromptExecutionQueued, DeliveryState: store.PromptDeliveryNotRequested,
				}), fence)
				require.NoError(t, err)
				reserved, target, err := fixture.dispatcher.reserveTask(ctx, continued)
				require.NoError(t, err)
				require.NotNil(t, reserved)
				require.NoError(t, fixture.dispatcher.executeReservedTask(ctx, reserved, target))
				require.NotNil(t, continuationCreate)
				require.NotNil(t, continuationPrompt)
				require.Equal(t, snapshot.DataDigest, continuationCreate.NativeRestore.Snapshot.DataDigest)
				require.Greater(t, continuationCreate.Metadata.Fence.RuntimeSessionGeneration, uint64(1))
				require.Len(t, continuationPrompt.Input.Content, 1)
				require.Equal(t, continued.Spec.Prompt, continuationPrompt.Input.Content[0].Text)
				require.Equal(t, 2, deleteCalls)
				return
			}
			require.Error(t, err)
			require.GreaterOrEqual(t, captureCalls, 1)
			require.Zero(t, deleteCalls, "capture evidence must survive failed canonical checkpoint persistence")
			control, err := persistence.GetSessionControl(ctx, "default", "native-session")
			require.NoError(t, err)
			require.NotNil(t, control.Lease, "a newer Task must remain fenced out")
			current := fixture.currentTask(t, ctx)
			require.NotEmpty(t, current.Annotations[nativeCaptureIntentAnnotation])
			_, err = persistence.GetNativeSession(ctx, "default", "native-session", control.SessionUID)
			require.ErrorIs(t, err, store.ErrNotFound)
			// Retry terminal recovery using the durable capture identity. No
			// accepted prompt is submitted again.
			failing.fail = false
			attempt, err := persistence.GetPromptAttempt(ctx, fixture.attemptID)
			require.NoError(t, err)
			fence, err := fixture.dispatcher.Epochs.CurrentFence(ctx)
			require.NoError(t, err)
			require.NoError(t, fixture.dispatcher.finalizeRecoveredTerminalSession(ctx, current, attempt, fence))
			ready, err := fixture.dispatcher.cleanupRecoveredTaskScopedRuntimeSession(ctx, current)
			require.NoError(t, err)
			require.True(t, ready)
			require.Equal(t, 1, deleteCalls)
		})
	}
}

func TestACPDispatcherUnsupportedNativeCapturePreservesContinuityPolicy(t *testing.T) {
	for _, tc := range []struct{ imported, failWarning bool }{{}, {failWarning: true}, {imported: true}} {
		imported := tc.imported
		name := "canonical-session"
		if imported {
			name = "native-session"
		}
		t.Run(name+fmt.Sprint(tc.failWarning), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			snapshot := storetest.NativeSessionSnapshot(t, "native imported context")
			captures, deletes := 0, 0
			fixture := newTaskScopedCreateConflictFixture(t, ctx, "unsupported-"+name, "77777777-7777-7777-7777-777777777777",
				func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
					return newDispatcherRuntimeServerWithOptions(t, profile, digest, dispatcherRuntimeServerOptions{
						nativeSnapshot: &snapshot,
						onNativeCapture: func(harnessv2.CaptureNativeSessionRequest) error {
							captures++
							return &harnessv2.ClientError{StatusCode: http.StatusUnprocessableEntity, Code: harnessv2.ErrorCodeNativeCaptureUnsupported}
						},
						onDelete: func(harnessv2.DeleteRuntimeSessionRequest) { deletes++ },
					})
				}, func(task *corev1alpha1.Task) {
					task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: name, Create: true, Append: true}
				})
			defer fixture.stop()
			persistence := fixture.dispatcher.ResultStore.(*sqlite.Store)
			warningStore := &nativeWarningFailureStore{Store: persistence, fail: tc.failWarning}
			fixture.dispatcher.EventStore = warningStore
			continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
				SessionControls: persistence, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
				NewSessionUID: func() (string, error) { return "fixed-native-owner", nil },
			})
			require.NoError(t, err)
			fixture.dispatcher.Sessions = continuity
			if imported {
				staged := snapshot
				staged.RuntimeSessionUID, staged.RuntimeProfileDigest, staged.WorkingDirectory = "", "", ""
				_, err := persistence.StageNativeSessionImport(ctx, store.NativeSessionImport{
					Namespace: "default", SessionName: name, OperationID: "import-native",
					RequestDigest: store.NativeSessionImportDigest("default", name, snapshot.DataDigest), Snapshot: staged,
				})
				require.NoError(t, err)
				require.NoError(t, persistence.BindSessionCleanupIdentity(ctx, "default", name, "fixed-native-owner"))
			}
			reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
			require.NoError(t, err)
			require.NotNil(t, reserved)
			err = fixture.dispatcher.executeReservedTask(ctx, reserved, target)
			if tc.failWarning {
				require.ErrorContains(t, err, "injected warning persistence failure")
				current := fixture.currentTask(t, ctx)
				var intent nativeCaptureIntent
				require.NoError(t, json.Unmarshal([]byte(current.Annotations[nativeCaptureIntentAnnotation]), &intent))
				require.True(t, intent.Unsupported, "annotation may commit before warning")
				fence, fenceErr := fixture.dispatcher.Epochs.CurrentFence(ctx)
				require.NoError(t, fenceErr)
				restarted := &ACPDispatcher{Client: fixture.kubeClient, APIReader: fixture.kubeClient,
					Store: fixture.dispatcher.Store, ResultStore: persistence, EventStore: warningStore,
					Snapshots: persistence, Sessions: continuity, Epochs: fixture.dispatcher.Epochs}
				err = restarted.recoverStaleTask(ctx, current, fence)
			}
			control, controlErr := persistence.GetSessionControl(ctx, "default", name)
			require.NoError(t, controlErr)
			if imported {
				require.Error(t, err)
				require.Zero(t, deletes, "imported native history cannot fall back silently")
				require.NotNil(t, control.Lease)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, deletes, "proven stopped unsupported state may be retired")
				require.Nil(t, control.Lease)
				require.Equal(t, corev1alpha1.TaskPhaseSucceeded, fixture.currentTask(t, ctx).Status.Phase)
				fence, err := fixture.dispatcher.Epochs.CurrentFence(ctx)
				require.NoError(t, err)
				require.NoError(t, fixture.dispatcher.recoverStaleTask(ctx, fixture.currentTask(t, ctx), fence))
				require.Equal(t, 1, captures, "the durable unsupported fallback must not retry capture")
				require.Equal(t, 1, deletes)
			}
			listed, eventErr := persistence.ListExecutionEvents(ctx, store.ExecutionEventFilter{Namespace: "default", StreamType: events.ExecutionEventStreamTypeTask, StreamID: fixture.task.Name, Limit: 1000})
			require.NoError(t, eventErr)
			warnings := 0
			for _, event := range listed {
				if event.Type == events.ExecutionEventTypeNativeSessionCaptureSkipped {
					warnings++
					require.Equal(t, events.ExecutionEventSeverityWarning, event.Severity)
					require.JSONEq(t, `{"reason":"native_capture_unsupported","fallback":"transcript"}`, string(event.Content))
					require.Contains(t, event.Summary, "canonical transcript")
					require.NotContains(t, event.Summary, snapshot.ProviderSessionID)
				}
			}
			if imported {
				require.Zero(t, warnings)
			} else {
				require.Equal(t, 1, warnings)
			}

		})
	}
}

func TestACPDispatcherUnknownNativeInstallRetainsExactGenerationAndLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	snapshot := storetest.NativeSessionSnapshot(t, "native import awaiting install receipt")
	creates, deletes := 0, 0
	fixture := newTaskScopedCreateConflictFixture(t, ctx, "unknown-native-install", "77777777-7777-7777-7777-777777777777",
		func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
			return newDispatcherRuntimeServerWithOptions(t, profile, digest, dispatcherRuntimeServerOptions{
				nativeSnapshot: &snapshot, nativeInstallUnresolved: true,
				rejectCreate: func(request harnessv2.CreateRuntimeSessionRequest) (int, *harnessv2.ErrorResponse, bool) {
					creates++
					require.NotNil(t, request.NativeRestore)
					return http.StatusConflict, &harnessv2.ErrorResponse{Protocol: harnessv2.ProtocolVersion,
						Code: harnessv2.ErrorCodeCleanupUnproven, Message: "native install receipt is unresolved"}, true
				},
				onDelete: func(harnessv2.DeleteRuntimeSessionRequest) { deletes++ },
			})
		}, func(task *corev1alpha1.Task) {
			task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "unknown-import", Create: true, Append: true}
		})
	defer fixture.stop()
	persistence := fixture.dispatcher.ResultStore.(*sqlite.Store)
	fixture.dispatcher.EventStore = persistence
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: persistence, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
		NewSessionUID: func() (string, error) { return "unknown-native-owner", nil },
	})
	require.NoError(t, err)
	fixture.dispatcher.Sessions = continuity
	staged := snapshot
	staged.RuntimeSessionUID, staged.RuntimeProfileDigest, staged.WorkingDirectory = "", "", ""
	_, err = persistence.StageNativeSessionImport(ctx, store.NativeSessionImport{Namespace: "default", SessionName: "unknown-import",
		OperationID: "unknown-import-operation", RequestDigest: store.NativeSessionImportDigest("default", "unknown-import", snapshot.DataDigest), Snapshot: staged})
	require.NoError(t, err)
	require.NoError(t, persistence.BindSessionCleanupIdentity(ctx, "default", "unknown-import", "unknown-native-owner"))
	reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	require.ErrorIs(t, fixture.dispatcher.executeReservedTask(ctx, reserved, target), errNativeSessionInstallUnresolved)
	current := fixture.currentTask(t, ctx)
	require.NotEmpty(t, current.Annotations[nativeInstallUnresolvedAnnotation])
	require.Equal(t, int64(1), current.Status.Execution.RuntimeSessionGeneration)
	control, err := persistence.GetSessionControl(ctx, "default", "unknown-import")
	require.NoError(t, err)
	require.NotNil(t, control.Lease)
	attempt, err := persistence.GetPromptAttempt(ctx, fixture.attemptID)
	require.NoError(t, err)
	require.Equal(t, store.PromptExecutionPlanned, attempt.ExecutionState, "unknown install must not mint/requeue a prompt attempt")
	fence, err := fixture.dispatcher.Epochs.CurrentFence(ctx)
	require.NoError(t, err)
	require.ErrorIs(t, fixture.dispatcher.recoverStaleTask(ctx, current, fence), errNativeSessionInstallUnresolved)
	withoutMarker := current.DeepCopy()
	delete(withoutMarker.Annotations, nativeInstallUnresolvedAnnotation)
	require.ErrorIs(t, fixture.dispatcher.recoverStaleTask(ctx, withoutMarker, fence), errNativeSessionInstallUnresolved,
		"authenticated unresolved status must also protect recovery when the marker write was interrupted")
	runtimeClient, runtimeFence, _, _, err := fixture.dispatcher.runtimeClient(ctx, target, harnessv2.MCPPolicyConfiguration{}, false)
	require.NoError(t, err)
	runtimeFence.RuntimeSessionUID, runtimeFence.RuntimeSessionGeneration = "unknown-native-owner", 1
	session, err := fixture.dispatcher.recoveredTaskSession(ctx, current, attempt)
	require.NoError(t, err)
	require.NotNil(t, session)
	_, err = fixture.dispatcher.reconcilePlannedRuntimeSession(ctx, runtimeClient, current, fixture.attemptID, fence, session, &runtimeFence)
	require.ErrorIs(t, err, errNativeSessionInstallUnresolved)
	require.Equal(t, uint64(1), session.Binding.Generation)
	_, err = waitForRuntimeSessionAdmission(ctx, runtimeClient, harnessv2.RuntimeSessionID(runtimeSessionID(runtimeFence)), runtimeFence.RuntimeSessionUID, 1, time.Second)
	require.ErrorIs(t, err, errNativeSessionInstallUnresolved)
	_, err = fixture.dispatcher.reconcileRecoveredRuntimeSession(ctx, current, current.UID, true, nil)
	require.ErrorIs(t, err, errNativeSessionInstallUnresolved)
	require.Equal(t, 1, creates)
	require.Zero(t, deletes)
	retained, err := persistence.GetSessionControl(ctx, "default", "unknown-import")
	require.NoError(t, err)
	require.Equal(t, control.Lease, retained.Lease)
}

type nativeWarningFailureStore struct {
	*sqlite.Store
	fail bool
}

func (s *nativeWarningFailureStore) AppendExecutionEventIfAbsent(ctx context.Context, event *store.ExecutionEvent, key string) (*store.ExecutionEvent, bool, error) {
	if s.fail && event.Type == events.ExecutionEventTypeNativeSessionCaptureSkipped {
		s.fail = false
		return nil, false, errors.New("injected warning persistence failure")
	}
	return s.Store.AppendExecutionEventIfAbsent(ctx, event, key)
}

func TestLegacyTaskRejectsNativeSessionBeforeLockAndAppend(t *testing.T) {
	ctx := t.Context()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "native-legacy.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	persistence := sqlite.NewStore(db, "")
	cipher, err := sqlite.NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	require.NoError(t, persistence.SetAgentExecutionSnapshotCipher(cipher))
	snapshot := storetest.NativeSessionSnapshot(t, "staged private history")
	snapshot.RuntimeSessionUID, snapshot.RuntimeProfileDigest, snapshot.WorkingDirectory = "", "", ""
	_, err = persistence.StageNativeSessionImport(ctx, store.NativeSessionImport{
		Namespace: "default", SessionName: "native-import", OperationID: "stage-native",
		RequestDigest: store.NativeSessionImportDigest("default", "native-import", snapshot.DataDigest), Snapshot: snapshot,
	})
	require.NoError(t, err)
	before, err := persistence.GetNativeSession(ctx, "default", "native-import", "")
	require.NoError(t, err)
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "legacy-task", UID: "legacy-uid"},
		Spec: corev1alpha1.TaskSpec{Prompt: "unrelated legacy work", SessionRef: &corev1alpha1.SessionReference{Name: "native-import", Append: true}}}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	manager := NewSessionManager(persistence)
	reconciler := &TaskReconciler{Client: kube, SessionManager: manager, ExecutionEventStore: persistence}
	_, err, rejected := reconciler.acquireSessionLock(ctx, task)
	require.NoError(t, err)
	require.True(t, rejected)
	current := &corev1alpha1.Task{}
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(task), current))
	require.Equal(t, corev1alpha1.TaskPhaseFailed, current.Status.Phase)
	require.Contains(t, current.Status.Message, "native continuity")
	require.NoError(t, manager.AppendMessages(ctx, current, persistence), "a rejected Task has no transcript ownership")
	session, err := persistence.GetSession(ctx, "default", "native-import")
	require.NoError(t, err)
	require.Empty(t, session.ActiveTask)
	require.Zero(t, session.MessageCount)
	after, err := persistence.GetNativeSession(ctx, "default", "native-import", "")
	require.NoError(t, err)
	require.Equal(t, before, after)
}
