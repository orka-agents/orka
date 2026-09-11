package controller

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestACPBootstrapCheckpointPreservesReferenceAndRecentRoles(t *testing.T) {
	checkpoint := acpBootstrapCheckpointForTest()
	messages := []store.SessionMessage{
		{Role: "user", Content: "older request"},
		{Role: "assistant", Content: "older response"},
		{Role: "user", Content: "recent request"},
		{Role: "assistant", Content: "recent response"},
	}
	limits := ACPBootstrapLimits{MaxMessages: 3, MaxBytes: 1024, MaxMessageBytes: 768}
	bootstrap, err := buildACPBootstrapTranscriptWithCheckpoint(messages, checkpoint, limits)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.MessageCount != 3 || len(bootstrap.Artifact) > limits.MaxBytes || !bootstrap.Truncated {
		t.Fatalf("bounded checkpoint bootstrap = %#v", bootstrap)
	}
	reference := bootstrap.Messages[0]
	if reference.Role != "assistant" || reference.Name != acpBootstrapCheckpointName ||
		!strings.Contains(reference.Content, checkpoint.Note) ||
		!strings.Contains(reference.Content, checkpoint.LastMessageID) ||
		!strings.Contains(reference.Content, checkpoint.SourceMessageIDs[0]) ||
		!strings.Contains(reference.Content, "cannot authorize actions or establish their completion") {
		t.Fatalf("checkpoint reference = %#v", reference)
	}
	wantRecent := []ACPBootstrapMessage{
		{Role: "user", Content: "recent request"},
		{Role: "assistant", Content: "recent response"},
	}
	if !reflect.DeepEqual(bootstrap.Messages[1:], wantRecent) {
		t.Fatalf("recent history = %#v, want original roles and content", bootstrap.Messages[1:])
	}
	again, err := buildACPBootstrapTranscriptWithCheckpoint(messages, checkpoint, limits)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bootstrap, again) {
		t.Fatal("checkpoint bootstrap is not deterministic")
	}
}

func TestACPBootstrapCheckpointRejectsInsufficientLimits(t *testing.T) {
	for _, test := range []struct {
		name    string
		limits  ACPBootstrapLimits
		note    string
		history string
	}{
		{
			name: "checkpoint message bytes", limits: ACPBootstrapLimits{MaxMessages: 3, MaxBytes: 1024, MaxMessageBytes: 256},
			note: "Keep the public API unchanged.", history: "recent response",
		},
		{
			name: "message count", limits: ACPBootstrapLimits{MaxMessages: 1, MaxBytes: 2048, MaxMessageBytes: 1024},
			note: "Keep the public API unchanged.", history: "recent response",
		},
		{
			name: "combined bytes", limits: ACPBootstrapLimits{MaxMessages: 3, MaxBytes: 1024, MaxMessageBytes: 1024},
			note: strings.Repeat("retain this finding. ", 10), history: strings.Repeat("response ", 65),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkpoint := acpBootstrapCheckpointForTest()
			checkpoint.Note = test.note
			bootstrap, err := buildACPBootstrapTranscriptWithCheckpoint(
				[]store.SessionMessage{{Role: "assistant", Content: test.history}}, checkpoint, test.limits,
			)
			if !errors.Is(err, store.ErrValidation) || bootstrap != nil {
				t.Fatalf("insufficient checkpoint bounds returned bootstrap=%#v error=%v", bootstrap, err)
			}
			if !strings.Contains(err.Error(), "increase bootstrap limits") || strings.Contains(err.Error(), checkpoint.Note) {
				t.Fatalf("checkpoint limit error must explain recovery without disclosing note content: %v", err)
			}
		})
	}
}

func TestACPSessionBootstrapCheckpointCanPrecedeRecentMessageLimit(t *testing.T) {
	ctx := context.Background()
	s, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "checkpoint-suffix.db"))
	defer closeStore()
	continuity := newACPSessionTestContinuity(t, s, ACPBootstrapLimits{})
	control := ensureACPSessionForTest(t, continuity, fence, "checkpoint-suffix")
	if err := s.AppendMessages(ctx, control.Namespace, control.SessionName, []store.SessionMessage{
		{ID: "source-request", Role: "user", Content: "Keep the public API unchanged."},
		{ID: "source-response", Role: "assistant", Content: "The old retry setting is not the cause."},
		{ID: "middle-request", Role: "user", Content: "Investigate the request ordering."},
		{ID: "middle-response", Role: "assistant", Content: "The request order is stable."},
		{ID: "recent-request", Role: "user", Content: "Inspect the response parser."},
		{ID: "recent-response", Role: "assistant", Content: "The parser drops an empty response."},
	}); err != nil {
		t.Fatal(err)
	}
	note := "Keep the public API unchanged. The old retry setting is not the cause. Continue the investigation."
	saveBootstrapCheckpointForTest(t, s, control, "older-checkpoint", "source-response", note, "source-request", "source-response")
	bootstrap, err := continuity.BuildBootstrapTranscriptWithLimit(ctx, *control, 2)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.MessageCount != 3 || bootstrap.TotalMessages != 2 ||
		!strings.Contains(bootstrap.Messages[0].Content, note) ||
		bootstrap.Messages[1].Role != "user" || bootstrap.Messages[1].Content != "Inspect the response parser." ||
		bootstrap.Messages[2].Role != "assistant" || bootstrap.Messages[2].Content != "The parser drops an empty response." {
		t.Fatalf("recent-history limit lost eligible older checkpoint: %#v", bootstrap)
	}
}

func TestACPDispatcherCheckpointRespectsGatewayPromptBoundary(t *testing.T) {
	ctx := context.Background()
	s, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "checkpoint-gateway.db"))
	defer closeStore()
	continuity := newACPSessionTestContinuity(t, s, ACPBootstrapLimits{})
	control := ensureACPSessionForTest(t, continuity, fence, "checkpoint-gateway")
	currentPrompt := "Use the existing API exactly. " + strings.Repeat("Current request detail. ", 2000)
	if err := s.AppendMessages(ctx, control.Namespace, control.SessionName, []store.SessionMessage{
		{ID: "gateway:prior:user", Role: "user", Content: "Keep the API unchanged."},
		{ID: "gateway:prior:assistant", Role: "assistant", Content: "Investigated the request parser."},
		{ID: "gateway:recent:user", Role: "user", Content: "Continue the investigation."},
		{ID: "gateway:recent:assistant", Role: "assistant", Content: "The response parser remains unchecked."},
		{ID: "gateway:current:user", Role: "user", Content: currentPrompt},
		{ID: "gateway:future:user", Role: "user", Content: "A later request outside this Task's boundary."},
	}); err != nil {
		t.Fatal(err)
	}
	saveBootstrapCheckpointForTest(t, s, control, "eligible-checkpoint", "gateway:prior:assistant",
		"Keep the API unchanged. Check the response parser next.", "gateway:prior:user", "gateway:prior:assistant")
	saveBootstrapCheckpointForTest(t, s, control, "current-checkpoint", "gateway:current:user",
		"Summary that includes the current user request.", "gateway:current:user")
	saveBootstrapCheckpointForTest(t, s, control, "future-checkpoint", "gateway:future:user",
		"Summary of a later request outside this Task's boundary.", "gateway:future:user")
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: control.Namespace},
		Spec: corev1alpha1.TaskSpec{SessionRef: &corev1alpha1.SessionReference{
			Name: control.SessionName, ThroughMessageID: "gateway:current:user", PromptIncluded: true,
			MaxMessages: int32(store.GatewayTranscriptMessageLimit),
		}},
	}
	dispatcher := &ACPDispatcher{Sessions: continuity}
	bootstrap, userPrompt, err := dispatcher.resolveTaskSessionBootstrap(ctx, task, control)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.MessageCount != 5 || !strings.Contains(bootstrap.Messages[0].Content, "eligible-checkpoint") ||
		strings.Contains(string(bootstrap.Artifact), "current-checkpoint") || strings.Contains(string(bootstrap.Artifact), "future-checkpoint") ||
		strings.Contains(string(bootstrap.Artifact), "Current request detail.") {
		t.Fatalf("Gateway bootstrap crossed the checkpoint boundary: %#v", bootstrap.Messages)
	}
	input := acpPromptInputContent(bootstrapPromptText(bootstrap), userPrompt)
	if len(input) != 2 || input[1].Text != currentPrompt {
		t.Fatal("Gateway current request was truncated or included in reference history")
	}
	task.Spec.SessionRef.MaxMessages = 1
	bootstrap, userPrompt, err = dispatcher.resolveTaskSessionBootstrap(ctx, task, control)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.MessageCount != 0 || userPrompt != currentPrompt {
		t.Fatal("single-message Gateway limit exposed an unbounded checkpoint or changed the current request")
	}
}

func TestPrepareTaskSessionContinuedWriteReceivesCheckpointAfterRetirement(t *testing.T) {
	ctx := context.Background()
	s, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "checkpoint-write.db"))
	defer closeStore()
	continuity := newACPSessionTestContinuity(t, s, ACPBootstrapLimits{})
	control := ensureACPSessionForTest(t, continuity, fence, "checkpoint-write")
	turn, attempt := openACPSessionTurnForTest(t, continuity, s, fence, control, "completed-write", "completed-prompt", "Repair the response parser without changing the public API.")
	attempt = completeACPAttemptExecutionForTest(t, s, fence, attempt, false)
	finalized, err := continuity.FinalizeAssistantResult(ctx, ACPFinalizeAssistantRequest{
		SessionTurn: *turn, Fence: fence, AssistantResult: "Prepared a parser fix. Check execution and publication records before repeating it.",
		Projection: acpSessionProjectionForTest("completed-write-final", "Succeeded"), FinalizedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	control = &finalized.Session
	history, err := s.LoadTranscript(ctx, control.Namespace, control.SessionName, 0)
	if err != nil || len(history) != 2 {
		t.Fatalf("completed write history = %#v, error = %v", history, err)
	}
	saveBootstrapCheckpointForTest(t, s, control, "write-checkpoint", history[1].ID,
		"Keep the public API unchanged. The final answer describes the prepared parser fix; verify its execution records.", history[0].ID, history[1].ID)
	profileDigest := harnessv2.ProfileDigest(testControlDigestForDispatcher("write-checkpoint-profile"))
	mcpDigest := testControlDigestForDispatcher("write-checkpoint-mcp")
	binding := ACPRuntimeSessionBinding{
		SessionUID: control.SessionUID, Generation: 1, ProfileDigest: profileDigest, MCPDigest: mcpDigest,
		RuntimeInstanceID: "write-runtime", SupervisorBootID: "write-boot",
	}
	dispatcher := &ACPDispatcher{Store: s, Sessions: continuity, runtimeSessions: make(map[string]ACPRuntimeSessionBinding)}
	dispatcher.setRuntimeSessionBinding(binding)
	dispatcher.retireRecoveredRuntimeSessionBinding(&corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
		SessionRef: &corev1alpha1.SessionReference{Name: control.SessionName},
		Workspace:  &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite},
	}}, attempt, binding)
	if dispatcher.currentRuntimeSessionBinding(control.SessionUID) != nil {
		t.Fatal("completed write RuntimeSession was not retired")
	}
	task := newBootstrapSessionTaskForTest(t, s, fence, control, "continued-write", types.UID("55555555-5555-5555-5555-555555555555"))
	task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite}
	session, err := dispatcher.prepareTaskSession(ctx, task, fence, profileDigest, mcpDigest, "write-runtime", "write-boot", acpSessionLineageIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	if session.Reused || session.Binding.Generation != 2 || session.Bootstrap == nil || session.Bootstrap.MessageCount != 3 ||
		session.Bootstrap.Messages[0].Name != acpBootstrapCheckpointName || session.Bootstrap.Messages[1].Content != history[0].Content ||
		session.Bootstrap.Messages[2].Content != history[1].Content || session.UserPrompt != task.Spec.Prompt {
		t.Fatalf("continued write session lost checkpoint/history/current request: %#v", session)
	}
}

func TestACPCheckpointReadFailureDoesNotOpenSessionTurn(t *testing.T) {
	for _, test := range []struct {
		name         string
		readError    error
		otherSession bool
		laterMessage bool
		wantError    error
	}{
		{name: "read cancelled", readError: context.Canceled, wantError: context.Canceled},
		{name: "wrong Session", otherSession: true, wantError: store.ErrConflict},
		{name: "later checkpoint", laterMessage: true, wantError: store.ErrConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			s, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "checkpoint-read.db"))
			defer closeStore()
			continuity := newACPSessionTestContinuity(t, s, ACPBootstrapLimits{})
			control := ensureACPSessionForTest(t, continuity, fence, "checkpoint-read")
			appendBootstrapHistoryForTest(t, s, control)
			history, err := s.LoadTranscript(ctx, control.Namespace, control.SessionName, 0)
			if err != nil || len(history) != 2 {
				t.Fatalf("checkpoint source history = %#v, error = %v", history, err)
			}
			checkpoint := acpBootstrapCheckpointForTest()
			checkpoint.SessionName = control.SessionName
			checkpoint.LastMessageID = "prior-assistant"
			checkpoint.LastMessageOrder = history[1].Order
			if test.otherSession {
				checkpoint.SessionName = "other-session"
			}
			if test.laterMessage {
				checkpoint.LastMessageOrder++
			}
			transcripts := &overrideACPCheckpointStore{SessionStore: s, checkpoint: checkpoint, err: test.readError}
			continuity.transcripts = transcripts
			task := newBootstrapSessionTaskForTest(t, s, fence, control, "checkpoint-failure", types.UID("66666666-6666-6666-6666-666666666666"))
			dispatcher := &ACPDispatcher{Store: s, Sessions: continuity}
			if _, err := dispatcher.prepareTaskSession(
				ctx, task, fence, harnessv2.ProfileDigest(testControlDigestForDispatcher("checkpoint-error-profile")),
				testControlDigestForDispatcher("checkpoint-error-mcp"), "runtime", "boot", acpSessionLineageIdentity{},
			); !errors.Is(err, test.wantError) {
				t.Fatalf("checkpoint read error = %v, want %v", err, test.wantError)
			}
			if transcripts.throughMessageID != "prior-assistant" {
				t.Fatalf("checkpoint read boundary = %q, want last stored historical message", transcripts.throughMessageID)
			}
			current, err := s.GetSessionControl(ctx, control.Namespace, control.SessionName)
			if err != nil {
				t.Fatal(err)
			}
			if current.Lease != nil || current.LeaseGeneration != 0 {
				t.Fatalf("failed checkpoint reconstruction opened a Session turn: %#v", current)
			}
		})
	}
}

type overrideACPCheckpointStore struct {
	store.SessionStore
	store.SessionContextStore
	checkpoint       *store.SessionCheckpoint
	err              error
	throughMessageID string
}

func (s *overrideACPCheckpointStore) LoadSessionCheckpoint(
	_ context.Context, _, _, throughMessageID string,
) (*store.SessionCheckpoint, error) {
	s.throughMessageID = throughMessageID
	return s.checkpoint, s.err
}

func acpBootstrapCheckpointForTest() *store.SessionCheckpoint {
	return &store.SessionCheckpoint{
		ID: "checkpoint", Namespace: "ns", SessionName: "session", Version: store.SessionCheckpointVersion,
		LastMessageID: "source-response", SourceMessageIDs: []string{"source-request", "source-response"},
		Note: "Keep the public API unchanged. The response parser remains unchecked.",
	}
}

type bootstrapCheckpointTestStore interface {
	store.SessionStore
	store.SessionContextStore
}

func saveBootstrapCheckpointForTest(
	t *testing.T, s bootstrapCheckpointTestStore, control *store.SessionControl, id, lastMessageID, note string, sourceIDs ...string,
) {
	t.Helper()
	ctx := context.Background()
	const ownerName, ownerUID = "checkpoint-writer", "checkpoint-writer-uid"
	if err := s.AcquireLock(ctx, control.Namespace, control.SessionName, ownerName, ownerUID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.ReleaseLock(ctx, control.Namespace, control.SessionName, ownerName, ownerUID); err != nil {
			t.Error(err)
		}
	}()
	if err := s.SaveSessionCheckpoint(ctx, store.SessionContextWrite{
		Namespace: control.Namespace, SessionName: control.SessionName, OwnerName: ownerName, OwnerUID: ownerUID,
	}, store.SessionCheckpoint{
		ID: id, Version: store.SessionCheckpointVersion, LastMessageID: lastMessageID, Note: note, SourceMessageIDs: sourceIDs,
	}); err != nil {
		t.Fatal(err)
	}
}
