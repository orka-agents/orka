package controller

import (
	"context"
	"encoding/json"
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
