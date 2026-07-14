/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type sessionManagerResultReadStore struct {
	reads int
	err   error
}

func (s *sessionManagerResultReadStore) SaveResult(context.Context, string, string, []byte) error {
	return nil
}

func (s *sessionManagerResultReadStore) GetResult(context.Context, string, string) ([]byte, error) {
	s.reads++
	return nil, s.err
}

func (s *sessionManagerResultReadStore) DeleteResult(context.Context, string, string) error {
	return nil
}

type sessionManagerNotFoundFinalizerStore struct {
	store.SessionStore
}

func (sessionManagerNotFoundFinalizerStore) FinalizeTaskSession(context.Context, string, string, string, string, []store.SessionMessage) error {
	return store.ErrNotFound
}

type sessionManagerFallbackStore struct {
	store.SessionStore
}

func setupSessionManager() (*SessionManager, *sqlite.Store) {
	db, err := sqlite.NewDB(":memory:")
	if err != nil {
		panic(err)
	}
	ss := sqlite.NewStore(db, ":memory:")
	return NewSessionManager(ss), ss
}

func TestNewSessionManager(t *testing.T) {
	sm, _ := setupSessionManager()
	if sm == nil {
		t.Fatal("NewSessionManager returned nil")
	}
}

func TestSessionManager_FinalizeTaskDeletedSessionSkipsResultRead(t *testing.T) {
	sm, _ := setupSessionManager()
	readErr := errors.New("result should not be read for a deleted session")
	resultStore := &sessionManagerResultReadStore{err: readErr}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: "default", UID: types.UID("task-uid")},
		Spec: corev1alpha1.TaskSpec{
			Prompt:     "question",
			SessionRef: &corev1alpha1.SessionReference{Name: "deleted-session", Append: true},
		},
		Status: corev1alpha1.TaskStatus{ResultRef: &corev1alpha1.ResultReference{Available: true}},
	}

	if err := sm.FinalizeTask(context.Background(), task, resultStore); err != nil {
		t.Fatalf("FinalizeTask() error = %v", err)
	}
	if resultStore.reads != 0 {
		t.Fatalf("deleted session triggered %d result reads", resultStore.reads)
	}
}

func TestSessionManager_FinalizeTaskIgnoresConcurrentSessionDeletion(t *testing.T) {
	_, ss := setupSessionManager()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: "default", UID: types.UID("task-uid")},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{Name: "deleted-during-finalize"},
		},
	}
	require.NoError(t, ss.CreateSession(context.Background(), &store.SessionRecord{
		Namespace:     task.Namespace,
		Name:          task.Spec.SessionRef.Name,
		SessionType:   taskSessionType,
		ActiveTask:    task.Name,
		ActiveTaskUID: string(task.UID),
	}))
	sm := NewSessionManager(sessionManagerNotFoundFinalizerStore{SessionStore: ss})

	if err := sm.FinalizeTask(context.Background(), task, ss); err != nil {
		t.Fatalf("FinalizeTask() error = %v, want missing-session no-op", err)
	}
}

func TestSessionManager_FinalizeTaskFallbackRejectsSameNameDifferentUID(t *testing.T) {
	_, ss := setupSessionManager()
	currentUID := "current-task-uid"
	require.NoError(t, ss.CreateSession(context.Background(), &store.SessionRecord{
		Namespace:     "default",
		Name:          "reused-session",
		SessionType:   taskSessionType,
		ActiveTask:    testTask,
		ActiveTaskUID: currentUID,
	}))
	sm := NewSessionManager(sessionManagerFallbackStore{SessionStore: ss})
	staleTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: "default", UID: types.UID("stale-task-uid")},
		Spec: corev1alpha1.TaskSpec{
			Prompt:     "stale prompt",
			SessionRef: &corev1alpha1.SessionReference{Name: "reused-session", Append: true},
		},
	}

	if err := sm.FinalizeTask(context.Background(), staleTask, ss); err != nil {
		t.Fatalf("FinalizeTask() error = %v", err)
	}
	messages, err := ss.LoadTranscript(context.Background(), staleTask.Namespace, staleTask.Spec.SessionRef.Name, 0)
	if err != nil {
		t.Fatalf("LoadTranscript() error = %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("stale Task appended fallback transcript messages: %#v", messages)
	}
	session, err := ss.GetSession(context.Background(), staleTask.Namespace, staleTask.Spec.SessionRef.Name)
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if session.ActiveTaskUID != currentUID {
		t.Fatalf("stale Task changed current session owner: %#v", session)
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

func TestSessionManager_IsLocked_SameNameDifferentTaskUID(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()

	require.NoError(t, ss.CreateSession(ctx, &store.SessionRecord{
		Namespace:     "default",
		Name:          "test-session",
		SessionType:   "task",
		ActiveTask:    testTask,
		ActiveTaskUID: "old-task-uid",
	}))

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "default",
			UID:       types.UID("new-task-uid"),
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{Name: "test-session"},
		},
	}

	locked, err := sm.IsLocked(ctx, task)
	if err != nil {
		t.Fatalf("IsLocked() error = %v", err)
	}
	if !locked {
		t.Fatal("IsLocked() should reject a same-name lock owned by a different Task UID")
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
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
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
		{Role: "user", Content: "msg1"},
		{Role: "assistant", Content: "msg2"},
		{Role: "user", Content: "msg3"},
		{Role: "assistant", Content: "msg4"},
		{Role: "user", Content: "msg5"},
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
	if msgs[0].Role != "user" || msgs[0].Content != "What is the answer?" {
		t.Errorf("user message = %v, want user/What is the answer?", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Content != "Here is the answer" {
		t.Errorf("assistant message = %v, want assistant/Here is the answer", msgs[1])
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
	if msgs[0].Role != "user" || msgs[0].Content != "What is the answer?" {
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
