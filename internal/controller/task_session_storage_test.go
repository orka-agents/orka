package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// lossySessionStore fails transcript appends in order: ErrOutcomeUnknown
// after committing, as a lost response would, and any other error without
// reaching the store.
type lossySessionStore struct {
	*sqlite.Store
	appendErrs []error
}

func (s *lossySessionStore) AppendMessagesWithLock(
	ctx context.Context, namespace, name, ownerName, ownerUID string, messages []store.SessionMessage,
) error {
	if len(s.appendErrs) == 0 {
		return s.Store.AppendMessagesWithLock(ctx, namespace, name, ownerName, ownerUID, messages)
	}
	err := s.appendErrs[0]
	s.appendErrs = s.appendErrs[1:]
	if errors.Is(err, store.ErrOutcomeUnknown) {
		if appendErr := s.Store.AppendMessagesWithLock(ctx, namespace, name, ownerName, ownerUID, messages); appendErr != nil {
			return appendErr
		}
	}
	return err
}

// A session Task whose transcript the store has not accepted yet, or may have
// saved, keeps the Session and is completed again; the turn is appended once.
func TestCompleteTaskRetriesASessionAppendTheStoreHasNotAccepted(t *testing.T) {
	ctx := context.Background()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "append-task", Namespace: "default", UID: "append-task-uid"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI, Prompt: "summarize the release",
			SessionRef: &corev1alpha1.SessionReference{Name: "append-session", Append: true},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	r := newUnitReconciler(newTestScheme(), task)
	backend := r.SessionManager.store.(*sqlite.Store)
	sessions := &lossySessionStore{Store: backend, appendErrs: []error{
		fmt.Errorf("append: %w", store.ErrNotReady),
		fmt.Errorf("append: %w", store.ErrOutcomeUnknown),
	}}
	r.SessionManager = NewSessionManager(sessions)
	if err := backend.CreateSession(ctx, &store.SessionRecord{
		Namespace: "default", Name: "append-session", SessionType: "task",
		ActiveTask: task.Name, ActiveTaskUID: string(task.UID),
	}); err != nil {
		t.Fatal(err)
	}
	requireSession := func(locked bool, messages int) {
		t.Helper()
		session, err := backend.GetSession(ctx, "default", "append-session")
		if err != nil {
			t.Fatal(err)
		}
		transcript, err := backend.LoadTranscript(ctx, "default", "append-session", 0)
		if err != nil {
			t.Fatal(err)
		}
		if (session.ActiveTask == task.Name) != locked || len(transcript) != messages || session.MessageCount != messages {
			t.Fatalf("session active task %q, %d transcript messages (count %d); want locked=%v with %d",
				session.ActiveTask, len(transcript), session.MessageCount, locked, messages)
		}
	}

	for _, want := range []error{store.ErrNotReady, store.ErrOutcomeUnknown} {
		if _, err := r.completeTask(ctx, task.DeepCopy(), corev1alpha1.TaskPhaseSucceeded, "done"); !errors.Is(err, want) {
			t.Fatalf("completion = %v, want %v so the Task is retried", err, want)
		}
	}
	requireSession(true, 1)

	if _, err := r.completeTask(ctx, task.DeepCopy(), corev1alpha1.TaskPhaseSucceeded, "done"); err != nil {
		t.Fatalf("completion after the store accepts writes: %v", err)
	}
	requireSession(false, 1)
}
