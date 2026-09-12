package controller

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPrepareTaskSessionSkipsBootstrapForLiveRuntimeSessionReuse(t *testing.T) {
	ctx := context.Background()
	controlStore, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "live-reuse.db"))
	defer closeStore()
	continuity, transcriptStore := newBootstrapTrackingSessionContinuity(t, controlStore, "live-reuse-session-uid")
	control := ensureACPSessionForTest(t, continuity, fence, "live-reuse")
	appendBootstrapHistoryForTest(t, controlStore, control)

	profileDigest := harnessv2.ProfileDigest(testControlDigestForDispatcher("live-reuse-profile"))
	mcpDigest := testControlDigestForDispatcher("live-reuse-mcp")
	runtimeInstanceID := harnessv2.RuntimeInstanceID("live-runtime")
	supervisorBootID := harnessv2.SupervisorBootID("live-boot")
	task := newBootstrapSessionTaskForTest(
		t, controlStore, fence, control, "live-reuse-task", types.UID("11111111-1111-1111-1111-111111111111"),
	)
	dispatcher := &ACPDispatcher{
		Store: controlStore, Sessions: continuity, runtimeSessions: make(map[string]ACPRuntimeSessionBinding),
	}
	dispatcher.setRuntimeSessionBinding(ACPRuntimeSessionBinding{
		SessionUID: control.SessionUID, Generation: 1, ProfileDigest: profileDigest, MCPDigest: mcpDigest,
		RuntimeInstanceID: runtimeInstanceID, SupervisorBootID: supervisorBootID,
	})

	session, err := dispatcher.prepareTaskSession(
		ctx, task, fence, profileDigest, mcpDigest, runtimeInstanceID, supervisorBootID,
		acpSessionLineageIdentity{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil || !session.Reused {
		t.Fatalf("live RuntimeSession preparation = %#v, want reused session", session)
	}
	if session.Bootstrap != nil || bootstrapPromptText(session.Bootstrap) != "" {
		t.Fatalf("live RuntimeSession reuse attached canonical bootstrap history: %#v", session.Bootstrap)
	}
	if transcriptStore.loadTranscriptCalls != 0 || transcriptStore.loadTranscriptThroughCalls != 0 {
		t.Fatalf(
			"live RuntimeSession reuse loaded canonical transcript: LoadTranscript=%d LoadTranscriptThrough=%d",
			transcriptStore.loadTranscriptCalls, transcriptStore.loadTranscriptThroughCalls,
		)
	}
}

func TestPrepareTaskSessionRetainsBootstrapWhenRuntimeSessionRecreationIsRequired(t *testing.T) {
	ctx := context.Background()
	controlStore, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "recreate.db"))
	defer closeStore()
	continuity, transcriptStore := newBootstrapTrackingSessionContinuity(t, controlStore, "recreate-session-uid")
	control := ensureACPSessionForTest(t, continuity, fence, "recreate")
	appendBootstrapHistoryForTest(t, controlStore, control)

	profileDigest := harnessv2.ProfileDigest(testControlDigestForDispatcher("recreate-profile"))
	mcpDigest := testControlDigestForDispatcher("recreate-mcp")
	task := newBootstrapSessionTaskForTest(
		t, controlStore, fence, control, "recreate-task", types.UID("22222222-2222-2222-2222-222222222222"),
	)
	dispatcher := &ACPDispatcher{
		Store: controlStore, Sessions: continuity, runtimeSessions: make(map[string]ACPRuntimeSessionBinding),
	}
	dispatcher.setRuntimeSessionBinding(ACPRuntimeSessionBinding{
		SessionUID: control.SessionUID, Generation: 1, ProfileDigest: profileDigest, MCPDigest: mcpDigest,
		RuntimeInstanceID: "lost-runtime", SupervisorBootID: "lost-boot",
	})

	session, err := dispatcher.prepareTaskSession(
		ctx, task, fence, profileDigest, mcpDigest, "replacement-runtime", "replacement-boot",
		acpSessionLineageIdentity{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil || session.Reused {
		t.Fatalf("recreated RuntimeSession preparation = %#v, want non-reused session", session)
	}
	if session.Binding.Generation != 2 {
		t.Fatalf("recreated RuntimeSession generation = %d, want 2", session.Binding.Generation)
	}
	if session.Bootstrap == nil || session.Bootstrap.MessageCount != 2 {
		t.Fatalf("recreated RuntimeSession bootstrap = %#v, want retained two-message history", session.Bootstrap)
	}
	bootstrapText := bootstrapPromptText(session.Bootstrap)
	if !strings.Contains(bootstrapText, "prior request") || !strings.Contains(bootstrapText, "prior response") {
		t.Fatalf("recreated RuntimeSession bootstrap text = %q, want canonical history", bootstrapText)
	}
	if transcriptStore.loadTranscriptCalls != 1 || transcriptStore.loadTranscriptThroughCalls != 0 {
		t.Fatalf(
			"recreated RuntimeSession transcript loads: LoadTranscript=%d LoadTranscriptThrough=%d, want 1 and 0",
			transcriptStore.loadTranscriptCalls, transcriptStore.loadTranscriptThroughCalls,
		)
	}
}

func TestPrepareTaskSessionAdvancesPastDurableWorkspaceGenerationWithoutCachedBinding(t *testing.T) {
	ctx := context.Background()
	controlStore, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "durable-floor.db"))
	defer closeStore()
	continuity, transcriptStore := newBootstrapTrackingSessionContinuity(t, controlStore, "durable-floor-session-uid")
	control := ensureACPSessionForTest(t, continuity, fence, "durable-floor")
	appendBootstrapHistoryForTest(t, controlStore, control)

	profileDigest := harnessv2.ProfileDigest(testControlDigestForDispatcher("durable-floor-profile"))
	mcpDigest := testControlDigestForDispatcher("durable-floor-mcp")
	task := newBootstrapSessionTaskForTest(
		t, controlStore, fence, control, "durable-floor-task", types.UID("33333333-3333-3333-3333-333333333333"),
	)
	workspace := &workspacev1alpha1.ExecutionWorkspace{ObjectMeta: metav1.ObjectMeta{
		Namespace: task.Namespace, Name: "durable-floor-workspace", UID: types.UID("durable-floor-workspace-uid"),
		Annotations: map[string]string{acpWorkspaceDurableSessionCommittedAnnotation: "3"},
	}}
	task.Labels = map[string]string{acpExecutionWorkspaceLinkLabel: workspace.Name}
	task.Annotations = map[string]string{acpExecutionWorkspaceUIDAnnotation: string(workspace.UID)}
	kubeClient := fake.NewClientBuilder().WithScheme(bindingTestScheme(t)).WithObjects(workspace).Build()
	dispatcher := &ACPDispatcher{
		Client: kubeClient, APIReader: kubeClient, Store: controlStore, Sessions: continuity,
		runtimeSessions: make(map[string]ACPRuntimeSessionBinding),
	}

	session, err := dispatcher.prepareTaskSession(
		ctx, task, fence, profileDigest, mcpDigest, "replacement-runtime", "replacement-boot",
		acpSessionLineageIdentity{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil || session.Reused {
		t.Fatalf("durable-floor RuntimeSession preparation = %#v, want recreated session", session)
	}
	if session.Binding.Generation != 4 {
		t.Fatalf("durable-floor RuntimeSession generation = %d, want 4", session.Binding.Generation)
	}
	if session.Bootstrap == nil || session.Bootstrap.MessageCount != 2 {
		t.Fatalf("durable-floor RuntimeSession bootstrap = %#v, want retained two-message history", session.Bootstrap)
	}
	if transcriptStore.loadTranscriptCalls != 1 || transcriptStore.loadTranscriptThroughCalls != 0 {
		t.Fatalf(
			"durable-floor RuntimeSession transcript loads: LoadTranscript=%d LoadTranscriptThrough=%d, want 1 and 0",
			transcriptStore.loadTranscriptCalls, transcriptStore.loadTranscriptThroughCalls,
		)
	}
}

func TestPrepareTaskSessionAdvancesPastDurableSessionGenerationWithoutCachedBinding(t *testing.T) {
	ctx := context.Background()
	baseStore, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "durable-session-floor.db"))
	defer closeStore()
	controlStore := &runtimeGenerationSessionStore{bootstrapSessionTestStore: baseStore}
	continuity, transcriptStore := newBootstrapTrackingSessionContinuity(t, controlStore, "durable-session-floor-uid")
	control := ensureACPSessionForTest(t, continuity, fence, "durable-session-floor")
	appendBootstrapHistoryForTest(t, controlStore, control)
	controlStore.generation = 3

	profileDigest := harnessv2.ProfileDigest(testControlDigestForDispatcher("durable-session-floor-profile"))
	mcpDigest := testControlDigestForDispatcher("durable-session-floor-mcp")
	task := newBootstrapSessionTaskForTest(
		t, controlStore, fence, control, "durable-session-floor-task", types.UID("44444444-4444-4444-4444-444444444444"),
	)
	dispatcher := &ACPDispatcher{
		Store: controlStore, Sessions: continuity, runtimeSessions: make(map[string]ACPRuntimeSessionBinding),
	}

	session, err := dispatcher.prepareTaskSession(
		ctx, task, fence, profileDigest, mcpDigest, "replacement-runtime", "replacement-boot",
		acpSessionLineageIdentity{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil || session.Reused {
		t.Fatalf("durable Session floor preparation = %#v, want recreated session", session)
	}
	if session.Binding.Generation != 4 {
		t.Fatalf("durable Session floor RuntimeSession generation = %d, want 4", session.Binding.Generation)
	}
	if session.Bootstrap == nil || session.Bootstrap.MessageCount != 2 {
		t.Fatalf("durable Session floor bootstrap = %#v, want retained two-message history", session.Bootstrap)
	}
	if transcriptStore.loadTranscriptCalls != 1 || transcriptStore.loadTranscriptThroughCalls != 0 {
		t.Fatalf(
			"durable Session floor transcript loads: LoadTranscript=%d LoadTranscriptThrough=%d, want 1 and 0",
			transcriptStore.loadTranscriptCalls, transcriptStore.loadTranscriptThroughCalls,
		)
	}
}

type bootstrapTrackingSessionStore struct {
	store.SessionStore
	loadTranscriptCalls        int
	loadTranscriptThroughCalls int
}

func (s *bootstrapTrackingSessionStore) LoadTranscript(
	ctx context.Context, namespace, name string, maxMessages int,
) ([]store.SessionMessage, error) {
	s.loadTranscriptCalls++
	return s.SessionStore.LoadTranscript(ctx, namespace, name, maxMessages)
}

func (s *bootstrapTrackingSessionStore) LoadTranscriptThrough(
	ctx context.Context, namespace, name, throughMessageID string, maxMessages int,
) ([]store.SessionMessage, error) {
	s.loadTranscriptThroughCalls++
	return s.SessionStore.LoadTranscriptThrough(ctx, namespace, name, throughMessageID, maxMessages)
}

type bootstrapSessionTestStore interface {
	store.DurableControlStore
	store.SessionStore
}

type runtimeGenerationSessionStore struct {
	bootstrapSessionTestStore
	generation int64
}

func (s *runtimeGenerationSessionStore) GetSessionControl(
	ctx context.Context, namespace, sessionName string,
) (*store.SessionControl, error) {
	control, err := s.bootstrapSessionTestStore.GetSessionControl(ctx, namespace, sessionName)
	if err != nil {
		return nil, err
	}
	copy := *control
	copy.RuntimeSessionGeneration = s.generation
	return &copy, nil
}

func newBootstrapTrackingSessionContinuity(
	t *testing.T,
	controlStore bootstrapSessionTestStore,
	sessionUID string,
) (*ACPSessionContinuity, *bootstrapTrackingSessionStore) {
	t.Helper()
	transcriptStore := &bootstrapTrackingSessionStore{SessionStore: controlStore}
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: controlStore,
		Transcripts:     transcriptStore,
		Publications:    controlStore,
		BranchClaims:    controlStore,
		NewSessionUID:   func() (string, error) { return sessionUID, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return continuity, transcriptStore
}

func appendBootstrapHistoryForTest(
	t *testing.T, sessionStore store.SessionStore, control *store.SessionControl,
) {
	t.Helper()
	if err := sessionStore.AppendMessages(context.Background(), control.Namespace, control.SessionName, []store.SessionMessage{
		{ID: "prior-user", Role: "user", Content: "prior request", Timestamp: time.Now().UTC()},
		{ID: "prior-assistant", Role: "assistant", Content: "prior response", Timestamp: time.Now().UTC()},
	}); err != nil {
		t.Fatal(err)
	}
}

func newBootstrapSessionTaskForTest(
	t *testing.T,
	controlStore store.DurableControlStore,
	fence store.ControllerEpochFence,
	control *store.SessionControl,
	taskName string,
	taskUID types.UID,
) *corev1alpha1.Task {
	t.Helper()
	promptID := "prompt-" + string(taskUID) + "-1"
	requestDigest := testControlDigestForDispatcher(taskName)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: control.Namespace, Name: taskName, UID: taskUID},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "continue without replaying history",
			SessionRef: &corev1alpha1.SessionReference{
				Name: control.SessionName, Create: false, Append: true, MaxMessages: 50,
			},
		},
		Status: corev1alpha1.TaskStatus{Attempts: 1, Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateReserved, Attempt: 1, PromptID: promptID,
			RequestDigest: requestDigest, ControllerEpoch: fence.Epoch,
		}},
	}
	attempt, err := controlStore.CreatePromptAttempt(context.Background(), boundPromptAttemptForTest(&store.PromptAttempt{
		Key: store.PromptAttemptKey{
			Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: promptID,
		},
		RequestDigest: requestDigest,
	}), fence)
	if err != nil {
		t.Fatal(err)
	}
	operationID := "reserve-" + taskName
	if _, err := controlStore.TransitionPromptAttemptExecution(context.Background(), store.PromptAttemptExecutionTransition{
		ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
		NewState: store.PromptExecutionReserved, OperationID: operationID,
		OperationDigest: testControlDigestForDispatcher(operationID), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return task
}
