/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const sessionRoleUser = "user"

type countingSessionResultStore struct {
	reads int
}

func (s *countingSessionResultStore) SaveResult(context.Context, string, string, []byte) error {
	return nil
}

func (s *countingSessionResultStore) GetResult(context.Context, string, string) ([]byte, error) {
	s.reads++
	return []byte("generic finalization must not project this result"), nil
}

func (s *countingSessionResultStore) DeleteResult(context.Context, string, string) error {
	return nil
}

func setupSessionManager() (*SessionManager, *sqlite.Store) {
	db, err := sqlite.NewDB(":memory:")
	if err != nil {
		panic(err)
	}
	ss := sqlite.NewStore(db, ":memory:")
	return NewSessionManager(ss), ss
}

const (
	sessionTestRoleUser      = "user"
	sessionTestRoleAssistant = "assistant"
)

func TestNewSessionManager(t *testing.T) {
	sm, _ := setupSessionManager()
	if sm == nil {
		t.Fatal("NewSessionManager returned nil")
	}
}

func TestSessionManager_IsLocked_NoSessionRef(t *testing.T) {
	sm, _ := setupSessionManager()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			SessionRef: nil,
		},
	}

	locked, err := sm.IsLocked(context.Background(), task)
	if err != nil {
		t.Fatalf("IsLocked() error = %v", err)
	}
	if locked {
		t.Error("IsLocked() should return false for task without session ref")
	}
}

func TestSessionManager_IsLocked_SessionNotFound(t *testing.T) {
	sm, _ := setupSessionManager()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "nonexistent-session",
			},
		},
	}

	locked, err := sm.IsLocked(context.Background(), task)
	if err != nil {
		t.Fatalf("IsLocked() error = %v", err)
	}
	if locked {
		t.Error("IsLocked() should return false for nonexistent session")
	}
}

func TestSessionManager_IsLocked_NotLocked(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()

	// Create session with no active task
	require.NoError(t, ss.CreateSession(ctx, &store.SessionRecord{
		Namespace:   "default",
		Name:        "test-session",
		SessionType: "task",
	}))

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "test-session",
			},
		},
	}

	locked, err := sm.IsLocked(ctx, task)
	if err != nil {
		t.Fatalf("IsLocked() error = %v", err)
	}
	if locked {
		t.Error("IsLocked() should return false when no active task")
	}
}

func TestSessionManager_IsLocked_LockedByOther(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()

	// Create session locked by another task
	require.NoError(t, ss.CreateSession(ctx, &store.SessionRecord{
		Namespace:   "default",
		Name:        "test-session",
		SessionType: "task",
		ActiveTask:  "other-task",
	}))

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "test-session",
			},
		},
	}

	locked, err := sm.IsLocked(ctx, task)
	if err != nil {
		t.Fatalf("IsLocked() error = %v", err)
	}
	if !locked {
		t.Error("IsLocked() should return true when locked by another task")
	}
}

func TestSessionManager_IsLocked_LockedBySelf(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()

	// Create session locked by this task
	require.NoError(t, ss.CreateSession(ctx, &store.SessionRecord{
		Namespace:   "default",
		Name:        "test-session",
		SessionType: "task",
		ActiveTask:  testTask,
	}))

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "test-session",
			},
		},
	}

	locked, err := sm.IsLocked(ctx, task)
	if err != nil {
		t.Fatalf("IsLocked() error = %v", err)
	}
	if locked {
		t.Error("IsLocked() should return false when locked by self")
	}
}

func TestSessionManager_AcquireLock_NoSessionRef(t *testing.T) {
	sm, _ := setupSessionManager()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			SessionRef: nil,
		},
	}

	err := sm.AcquireLock(context.Background(), task)
	if err != nil {
		t.Errorf("AcquireLock() error = %v", err)
	}
}

func TestSessionManager_AcquireLock_CreateSession(t *testing.T) {
	sm, _ := setupSessionManager()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "new-session",
				Create: true,
			},
		},
	}

	err := sm.AcquireLock(context.Background(), task)
	if err != nil {
		t.Fatalf("AcquireLock() error = %v", err)
	}

	// Verify session was created with lock
	session, err := sm.GetSession(context.Background(), "default", "new-session")
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if session.ActiveTask != testTask {
		t.Errorf("ActiveTask = %s, want test-task", session.ActiveTask)
	}
}

func TestSessionManager_AcquireLock_SessionNotFound_NoCreate(t *testing.T) {
	sm, _ := setupSessionManager()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "nonexistent",
				Create: false,
			},
		},
	}

	err := sm.AcquireLock(context.Background(), task)
	if err == nil {
		t.Error("AcquireLock() expected error for nonexistent session with create=false")
	}
}

func TestSessionManager_AcquireLock_AlreadyLockedByOther(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()

	// Create session locked by another task
	ss.CreateSession(ctx, &store.SessionRecord{ //nolint:errcheck
		Namespace:   "default",
		Name:        "test-session",
		SessionType: "task",
		ActiveTask:  "other-task",
	})

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "test-session",
			},
		},
	}

	err := sm.AcquireLock(ctx, task)
	if err == nil {
		t.Error("AcquireLock() expected error when locked by another task")
	}
}

func TestSessionManager_ReleaseLock_NoSessionRef(t *testing.T) {
	sm, _ := setupSessionManager()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			SessionRef: nil,
		},
	}

	err := sm.ReleaseLock(context.Background(), task)
	if err != nil {
		t.Errorf("ReleaseLock() error = %v", err)
	}
}

func TestSessionManager_ReleaseLock_SessionNotFound(t *testing.T) {
	sm, _ := setupSessionManager()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "nonexistent",
			},
		},
	}

	err := sm.ReleaseLock(context.Background(), task)
	if err != nil {
		t.Errorf("ReleaseLock() error = %v", err)
	}
}

func TestSessionManager_ReleaseLock_NotOwner(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()

	// Create session locked by another task
	ss.CreateSession(ctx, &store.SessionRecord{ //nolint:errcheck
		Namespace:   "default",
		Name:        "test-session",
		SessionType: "task",
		ActiveTask:  "other-task",
	})

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "test-session",
			},
		},
	}

	err := sm.ReleaseLock(ctx, task)
	if err != nil {
		t.Errorf("ReleaseLock() error = %v", err)
	}

	// Verify lock was not released (still held by other-task)
	session, _ := sm.GetSession(ctx, "default", "test-session")
	if session.ActiveTask != "other-task" {
		t.Error("Lock should not be released by non-owner")
	}
}

func TestSessionManager_LoadTranscript_NoSessionRef(t *testing.T) {
	sm, _ := setupSessionManager()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			SessionRef: nil,
		},
	}

	msgs, err := sm.LoadTranscript(context.Background(), task)
	if err != nil {
		t.Fatalf("LoadTranscript() error = %v", err)
	}
	if msgs != nil {
		t.Error("LoadTranscript() should return nil for task without session ref")
	}
}

func TestSessionManager_LoadTranscript_SessionNotFound(t *testing.T) {
	sm, _ := setupSessionManager()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "nonexistent",
			},
		},
	}

	msgs, err := sm.LoadTranscript(context.Background(), task)
	if err != nil {
		t.Fatalf("LoadTranscript() error = %v", err)
	}
	if msgs != nil {
		t.Error("LoadTranscript() should return nil for nonexistent session")
	}
}

func TestSessionManager_LoadTranscript_WithMessages(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()

	// Create session with messages
	require.NoError(t, ss.CreateSession(ctx, &store.SessionRecord{
		Namespace:   "default",
		Name:        "test-session",
		SessionType: "task",
	}))
	require.NoError(t, ss.AppendMessages(ctx, "default", "test-session", []store.SessionMessage{
		{Role: sessionTestRoleUser, Content: "hello"},
		{Role: sessionTestRoleAssistant, Content: "hi"},
	}))

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "test-session",
			},
		},
	}

	msgs, err := sm.LoadTranscript(ctx, task)
	if err != nil {
		t.Fatalf("LoadTranscript() error = %v", err)
	}
	if len(msgs) != 2 {
		t.Errorf("LoadTranscript() returned %d messages, want 2", len(msgs))
	}
}

func TestSessionManager_LoadTranscript_MaxMessages(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()

	// Create session with messages
	require.NoError(t, ss.CreateSession(ctx, &store.SessionRecord{
		Namespace:   "default",
		Name:        "test-session",
		SessionType: "task",
	}))
	require.NoError(t, ss.AppendMessages(ctx, "default", "test-session", []store.SessionMessage{
		{Role: sessionTestRoleUser, Content: "msg1"},
		{Role: sessionTestRoleAssistant, Content: "msg2"},
		{Role: sessionTestRoleUser, Content: "msg3"},
		{Role: sessionTestRoleAssistant, Content: "msg4"},
		{Role: sessionTestRoleUser, Content: "msg5"},
	}))

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name:        "test-session",
				MaxMessages: 3,
			},
		},
	}

	msgs, err := sm.LoadTranscript(ctx, task)
	if err != nil {
		t.Fatalf("LoadTranscript() error = %v", err)
	}
	if len(msgs) != 3 {
		t.Errorf("LoadTranscript() returned %d messages, want 3", len(msgs))
	}
}

func TestSessionManager_GetSession(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()

	ss.CreateSession(ctx, &store.SessionRecord{ //nolint:errcheck
		Namespace:   "default",
		Name:        "test-session",
		SessionType: "task",
	})

	session, err := sm.GetSession(ctx, "default", "test-session")
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if session.Name != "test-session" {
		t.Errorf("GetSession() returned wrong session")
	}
}

func TestSessionManager_DeleteSession(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()

	ss.CreateSession(ctx, &store.SessionRecord{ //nolint:errcheck
		Namespace:   "default",
		Name:        "test-session",
		SessionType: "task",
	})

	err := sm.DeleteSession(ctx, "default", "test-session")
	if err != nil {
		t.Fatalf("DeleteSession() error = %v", err)
	}

	// Verify session was deleted
	_, err = sm.GetSession(ctx, "default", "test-session")
	if err == nil {
		t.Error("Session should be deleted")
	}
}

func TestSessionManager_ListSessions(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()

	ss.CreateSession(ctx, &store.SessionRecord{ //nolint:errcheck
		Namespace:   "default",
		Name:        "s1",
		SessionType: "task",
	})
	ss.CreateSession(ctx, &store.SessionRecord{ //nolint:errcheck
		Namespace:   "default",
		Name:        "s2",
		SessionType: "task",
	})

	sessions, err := sm.ListSessions(ctx, "default")
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(sessions) != 2 {
		t.Errorf("ListSessions() returned %d items, want 2", len(sessions))
	}
}

func TestSessionManager_AppendMessages_NoSessionRef(t *testing.T) {
	sm, ss := setupSessionManager()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			SessionRef: nil,
		},
	}

	err := sm.AppendMessages(context.Background(), task, ss)
	if err != nil {
		t.Errorf("AppendMessages() error = %v", err)
	}
}

func TestSessionManager_AppendMessages_AppendFalse(t *testing.T) {
	sm, ss := setupSessionManager()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "test-session",
				Append: false,
			},
		},
	}

	err := sm.AppendMessages(context.Background(), task, ss)
	if err != nil {
		t.Errorf("AppendMessages() error = %v", err)
	}
}

func TestSessionManager_AppendMessages_MissingSessionNoops(t *testing.T) {
	sm, ss := setupSessionManager()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Prompt: "What is the answer?",
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "missing-session",
				Append: true,
			},
		},
	}

	err := sm.AppendMessages(context.Background(), task, ss)
	if err != nil {
		t.Fatalf("AppendMessages() error = %v", err)
	}
}

func TestSessionManager_AppendMessages_WithPromptAndResult(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()

	// Create session
	require.NoError(t, ss.CreateSession(ctx, &store.SessionRecord{
		Namespace:   "default",
		Name:        "test-session",
		SessionType: "task",
	}))

	// Save result
	require.NoError(t, ss.SaveResult(ctx, defaultNS, testTask, []byte("Here is the answer")))

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "default",
			UID:       "completed-task-uid",
		},
		Spec: corev1alpha1.TaskSpec{
			Prompt: "What is the answer?",
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "test-session",
				Append: true,
			},
		},
		Status: corev1alpha1.TaskStatus{
			ResultRef: &corev1alpha1.ResultReference{
				Available: true,
			},
		},
	}

	require.NoError(t, sm.AcquireLock(ctx, task))
	err := sm.AppendMessages(ctx, task, ss)
	if err != nil {
		t.Fatalf("AppendMessages() error = %v", err)
	}

	// Verify messages were appended
	msgs, err := ss.LoadTranscript(ctx, "default", "test-session", 0)
	if err != nil {
		t.Fatalf("LoadTranscript() error = %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != sessionTestRoleUser || msgs[0].Content != "What is the answer?" {
		t.Errorf("user message = %v, want user/What is the answer?", msgs[0])
	}
	if msgs[1].Role != sessionTestRoleAssistant || msgs[1].Content != "Here is the answer" {
		t.Errorf("assistant message = %v, want assistant/Here is the answer", msgs[1])
	}
}

func TestSessionManager_AppendMessages_PromptIncludedSkipsDuplicateUserMessage(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()
	require.NoError(t, ss.CreateSession(ctx, &store.SessionRecord{
		Namespace: "default", Name: "prompt-included-session", SessionType: "task",
	}))
	require.NoError(t, ss.AppendMessages(ctx, "default", "prompt-included-session", []store.SessionMessage{{
		Role: sessionRoleUser, Content: "canonical transcript prompt", Timestamp: time.Now(),
	}}))
	require.NoError(t, ss.SaveResult(ctx, defaultNS, testTask, []byte("assistant result")))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Prompt: "must not be appended again",
			SessionRef: &corev1alpha1.SessionReference{
				Name: "prompt-included-session", Append: true, PromptIncluded: true,
				ThroughMessageID: "canonical:user",
			},
		},
		Status: corev1alpha1.TaskStatus{ResultRef: &corev1alpha1.ResultReference{Available: true}},
	}
	require.NoError(t, sm.AcquireLock(ctx, task))
	if err := sm.AppendMessages(ctx, task, ss); err != nil {
		t.Fatal(err)
	}
	messages, err := ss.LoadTranscript(ctx, "default", "prompt-included-session", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Content != "canonical transcript prompt" ||
		messages[1].Role != "assistant" || messages[1].Content != "assistant result" {
		t.Fatalf("messages = %#v, want canonical user plus assistant only", messages)
	}
}

func TestSessionManager_AppendMessages_NilResultStore(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()

	require.NoError(t, ss.CreateSession(ctx, &store.SessionRecord{
		Namespace:   "default",
		Name:        "test-session",
		SessionType: "task",
	}))

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Prompt: "What is the answer?",
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "test-session",
				Append: true,
			},
		},
		Status: corev1alpha1.TaskStatus{
			ResultRef: &corev1alpha1.ResultReference{
				Available: true,
			},
		},
	}

	require.NoError(t, sm.AcquireLock(ctx, task))
	err := sm.AppendMessages(ctx, task, nil)
	if err != nil {
		t.Fatalf("AppendMessages() error = %v", err)
	}

	msgs, err := ss.LoadTranscript(ctx, "default", "test-session", 0)
	if err != nil {
		t.Fatalf("LoadTranscript() error = %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected only the user message to be appended, got %d messages", len(msgs))
	}
	if msgs[0].Role != sessionTestRoleUser || msgs[0].Content != "What is the answer?" {
		t.Errorf("user message = %v, want user/What is the answer?", msgs[0])
	}
}

func TestSessionManager_AppendMessages_NoPromptNoResult(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()

	ss.CreateSession(ctx, &store.SessionRecord{ //nolint:errcheck
		Namespace:   "default",
		Name:        "test-session",
		SessionType: "task",
	})

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "test-session",
				Append: true,
			},
		},
	}

	err := sm.AppendMessages(ctx, task, ss)
	if err != nil {
		t.Fatalf("AppendMessages() error = %v", err)
	}

	// No messages should have been appended
	msgs, err := ss.LoadTranscript(ctx, "default", "test-session", 0)
	if err != nil {
		t.Fatalf("LoadTranscript() error = %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages, got %d", len(msgs))
	}
}

func TestSessionManagerNativeAIGatewayFinalization(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ss := sqlite.NewStore(db, ":memory:")
	manager := NewSessionManager(ss)
	manager.SetGatewayEventStore(ss)
	now := time.Now().UTC()
	event := store.GatewayEvent{
		ID: "native-event", Namespace: "default", NamespaceUID: "namespace-uid",
		GatewayUID: "gateway-uid", GatewayGeneration: 1, GatewayName: "chat",
		BindingName: "room", BindingUID: "binding-uid", ExternalEventID: "external-event",
		ProtocolVersion: "orka.gateway.v1", EventType: "text",
		AccountID: "acct", ContextID: "room", SenderID: "sender", Text: "question",
		ReplyTarget: "room", SessionName: "native-session", TaskName: "native-task",
		ReceivedAt: now, NextAttemptAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}
	admitted, created, err := ss.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: event, AppendUserMessage: true})
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, store.GatewayEventQueued, admitted.State)
	var canonicalSessionType, canonicalOwnerType, canonicalOwnerRef string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT session_type, owner_type, owner_ref FROM sessions WHERE namespace = ? AND name = ?`,
		event.Namespace, event.SessionName,
	).Scan(&canonicalSessionType, &canonicalOwnerType, &canonicalOwnerRef))
	require.Equal(t, "gateway", canonicalSessionType)
	require.Equal(t, "gateway", canonicalOwnerType)
	require.Equal(t, "gateway-uid/binding-uid", canonicalOwnerRef)
	claimed, err := ss.ClaimNextGatewayEvent(ctx, event.Namespace, "dispatcher", now, time.Minute)
	require.NoError(t, err)
	require.Equal(t, event.ID, claimed.ID)
	require.NoError(t, ss.SaveResult(ctx, "default", "native-task", []byte("native answer")))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "native-task", Namespace: "default", UID: "native-task-uid"},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, SessionRef: &corev1alpha1.SessionReference{
			Name: "native-session", MaxMessages: store.GatewayTranscriptMessageLimit,
			ThroughMessageID: store.GatewayUserMessageID("native-event"), PromptIncluded: true,
		}},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded, ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}
	require.NoError(t, ss.MarkGatewayEventTaskCreated(ctx, event.Namespace, event.ID, task.Name, string(task.UID), "dispatcher", now))
	require.NoError(t, manager.AcquireLock(ctx, task))
	nextEvent := event
	nextEvent.ID = "native-event-next"
	nextEvent.ExternalEventID = "external-event-next"
	nextEvent.TaskName = "native-task-next"
	nextEvent.Text = "follow-up"
	queued, created, err := ss.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: nextEvent, AppendUserMessage: true})
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, store.GatewayEventQueued, queued.State)
	// A non-nil interface with no implementation panics if generic finalization reads the result.
	unreadResultStore := struct{ store.ResultStore }{}
	for _, appendEnabled := range []bool{false, true} {
		task.Spec.SessionRef.Append = appendEnabled
		require.NoError(t, manager.AppendMessages(ctx, task, ss))
		require.NoError(t, manager.AppendMessages(ctx, task, unreadResultStore))
		require.NoError(t, manager.ReleaseLock(ctx, task))
		session, err := ss.GetSession(ctx, "default", "native-session")
		require.NoError(t, err)
		require.Equal(t, "native-task", session.ActiveTask)
		require.Equal(t, "native-task-uid", session.ActiveTaskUID)
		require.Len(t, session.Messages, 2, "only gateway terminal projection may append the native result")
		require.Equal(t, "gateway:native-event:user", session.Messages[0].ID)
		require.Equal(t, "question", session.Messages[0].Content)
		require.Equal(t, "gateway:native-event-next:user", session.Messages[1].ID)
		require.Equal(t, "follow-up", session.Messages[1].Content)
		_, err = ss.ClaimNextGatewayEvent(ctx, event.Namespace, "next-dispatcher", now, time.Minute)
		require.ErrorIs(t, err, store.ErrNotFound, "queued event must wait for gateway terminal projection")
	}

	projection := store.GatewayTerminalProjection{
		EventID: event.ID,
		Message: store.SessionMessage{
			ID: store.GatewayAssistantMessageID(event.ID), Role: "assistant", Content: "native answer",
			SourceType: "gateway-task", SourceRef: task.Name, Timestamp: now,
		},
		Delivery: store.GatewayDelivery{
			ID: "native-delivery", IdempotencyID: "native-delivery", Namespace: event.Namespace,
			NamespaceUID: event.NamespaceUID, GatewayUID: event.GatewayUID, GatewayGeneration: event.GatewayGeneration,
			GatewayName: event.GatewayName, BindingName: event.BindingName,
			EventID: event.ID, TaskName: task.Name, SessionName: event.SessionName,
			Kind: "final", State: store.GatewayDeliveryPending, AccountID: event.AccountID,
			ContextID: event.ContextID, ReplyTarget: event.ReplyTarget, Text: "native answer", MaxAttempts: 10,
			NextAttemptAt: now, ExpiresAt: event.ExpiresAt, CreatedAt: now, UpdatedAt: now,
		},
		CompletedAt: now,
	}
	for _, wantCreated := range []bool{true, false} {
		delivery, created, err := ss.ProjectGatewayTerminal(ctx, projection)
		require.NoError(t, err)
		require.Equal(t, wantCreated, created)
		require.Equal(t, "native-delivery", delivery.ID)
	}
	session, err := ss.GetSession(ctx, event.Namespace, event.SessionName)
	require.NoError(t, err)
	require.Empty(t, session.ActiveTask)
	require.Empty(t, session.ActiveTaskUID)
	require.Len(t, session.Messages, 3, "projection must append exactly one canonical assistant message")
	require.Equal(t, "gateway:native-event:user", session.Messages[0].ID)
	require.Equal(t, "gateway:native-event:assistant", session.Messages[1].ID)
	require.Equal(t, "assistant", session.Messages[1].Role)
	require.Equal(t, "native answer", session.Messages[1].Content)
	require.Equal(t, "gateway:native-event-next:user", session.Messages[2].ID)
	deliveries, err := ss.ListGatewayDeliveries(ctx, store.GatewayDeliveryFilter{Namespace: event.Namespace, EventID: event.ID})
	require.NoError(t, err)
	require.Len(t, deliveries, 1, "repeated projection must not duplicate the delivery")
	require.Equal(t, "native-delivery", deliveries[0].ID)
	completed, err := ss.GetGatewayEvent(ctx, event.Namespace, event.ID)
	require.NoError(t, err)
	require.Equal(t, store.GatewayEventCompleted, completed.State)
	claimed, err = ss.ClaimNextGatewayEvent(ctx, event.Namespace, "next-dispatcher", now, time.Minute)
	require.NoError(t, err)
	require.Equal(t, nextEvent.ID, claimed.ID)
}

func TestSessionManagerLoadsTranscriptThroughStableMessageID(t *testing.T) {
	db, err := sqlite.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ss := sqlite.NewStore(db, ":memory:")
	ctx := context.Background()
	now := time.Now().UTC()
	if err := ss.CreateSession(ctx, &store.SessionRecord{
		Namespace: "default", Name: "cutoff-session", SessionType: "gateway", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ss.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: store.GatewayEvent{
		ID: "gateway-event", Namespace: "default", NamespaceUID: "namespace-uid", GatewayUID: "gateway-uid", GatewayGeneration: 1, GatewayName: "chat",
		BindingName: "room", BindingUID: "binding-uid", ExternalEventID: "external-event",
		ProtocolVersion: "orka.gateway.v1", EventType: "text", State: store.GatewayEventTaskCreated,
		AccountID: "acct", ContextID: "room", SenderID: "sender", Text: "current",
		ReplyTarget: "room", SessionName: "cutoff-session", TaskName: "task", TaskUID: "task-uid",
		ReceivedAt: now, NextAttemptAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := ss.AppendMessages(ctx, "default", "cutoff-session", []store.SessionMessage{
		{ID: "prior-user", Order: 2, Role: sessionRoleUser, Content: "first"},
		{ID: "prior-assistant", Order: 3, Role: "assistant", Content: "reply"},
		{ID: store.GatewayUserMessageID("gateway-event"), Order: 4, Role: sessionRoleUser, Content: "current"},
		{ID: "future-user", Order: 6, Role: sessionRoleUser, Content: "future"},
	}); err != nil {
		t.Fatal(err)
	}
	manager := NewSessionManager(ss)
	manager.SetGatewayEventStore(ss)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid"},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, SessionRef: &corev1alpha1.SessionReference{
			Name: "mutated-session", MaxMessages: 1, ThroughMessageID: "future-user",
		}},
	}
	messages, err := manager.LoadTranscript(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[0].Content != "first" || messages[1].Content != "reply" || messages[2].Content != "current" {
		t.Fatalf("cutoff transcript = %#v", messages)
	}
}

func TestSessionManagerDefersGatewayTranscriptAndLockFinalization(t *testing.T) {
	db, err := sqlite.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ss := sqlite.NewStore(db, ":memory:")
	ctx := context.Background()
	now := time.Now().UTC()
	event := store.GatewayEvent{
		ID: "gateway-finalize-event", Namespace: "default", NamespaceUID: "namespace-uid",
		GatewayUID: "gateway-uid", GatewayGeneration: 1, GatewayName: "chat",
		BindingName: "room", BindingUID: "binding-uid", ExternalEventID: "gateway-finalize-external",
		ProtocolVersion: "orka.gateway.v1", EventType: "text", AccountID: "acct", ContextID: "room",
		SenderID: "sender", Text: "current", ReplyTarget: "room", SessionName: "gateway-finalize-session",
		TaskName: "gateway-finalize-task", ReceivedAt: now, NextAttemptAt: now,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := ss.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{
		Event: event, AppendUserMessage: true, PendingLimit: 100,
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := ss.ClaimNextGatewayEvent(ctx, event.Namespace, "dispatcher", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	const taskUID = "gateway-finalize-task-uid"
	if err := ss.MarkGatewayEventTaskCreated(
		ctx, event.Namespace, event.ID, claimed.TaskName, taskUID, "dispatcher", now,
	); err != nil {
		t.Fatal(err)
	}

	manager := NewSessionManager(ss)
	manager.SetGatewayEventStore(ss)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: claimed.TaskName, Namespace: event.Namespace, UID: types.UID(taskUID),
		},
		// Use an admitted but malformed policy to prove generic finalization does
		// not revalidate the policy before deferring to Gateway projection.
		Spec: corev1alpha1.TaskSpec{SessionRef: &corev1alpha1.SessionReference{
			Name: event.SessionName, Create: true, Append: true, MaxMessages: 1,
		}},
		Status: corev1alpha1.TaskStatus{ResultRef: &corev1alpha1.ResultReference{Available: true}},
	}
	results := &countingSessionResultStore{}
	if err := manager.AppendMessages(ctx, task, results); err != nil {
		t.Fatalf("AppendMessages() error = %v", err)
	}
	if err := manager.ReleaseLock(ctx, task); err != nil {
		t.Fatalf("ReleaseLock() error = %v", err)
	}
	if results.reads != 0 {
		t.Fatalf("generic finalization read the Gateway result %d times", results.reads)
	}

	session, err := ss.GetSession(ctx, event.Namespace, event.SessionName)
	if err != nil {
		t.Fatal(err)
	}
	if session.ActiveTask != task.Name || session.ActiveTaskUID != string(task.UID) {
		t.Fatalf(
			"gateway session lock = (%q, %q), want (%q, %q)",
			session.ActiveTask, session.ActiveTaskUID, task.Name, task.UID,
		)
	}
	if len(session.Messages) != 1 || session.Messages[0].Role != sessionRoleUser {
		t.Fatalf("gateway transcript was generically finalized: %#v", session.Messages)
	}
}
