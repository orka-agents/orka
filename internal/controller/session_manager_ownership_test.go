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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestHandleScheduledTask_InvalidCronPreservesChatTurn(t *testing.T) {
	ctx := t.Context()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-schedule", Namespace: "default", UID: "scheduled-task-uid"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI, Schedule: "123", Prompt: "must not enter the chat transcript",
			SessionRef: &corev1alpha1.SessionReference{Name: "parent-chat", Create: true, Append: true},
		},
	}
	r := newUnitReconciler(newTestScheme(), task)
	ss := r.SessionManager.store
	turns := ss.(store.SessionTurnCommitter)
	session := &store.SessionRecord{Namespace: task.Namespace, Name: task.Spec.SessionRef.Name, SessionType: "chat"}
	_, err := turns.AcquireChatTurn(ctx, session, "parent-turn", time.Now().Add(time.Minute))
	require.NoError(t, err)

	_, err = r.handlePending(ctx, task)
	require.NoError(t, err)
	require.Equal(t, corev1alpha1.TaskPhaseFailed, task.Status.Phase)
	require.Contains(t, task.Status.Message, "invalid cron expression")
	got, err := ss.GetSession(ctx, session.Namespace, session.Name)
	require.NoError(t, err)
	require.Empty(t, got.Messages)
	require.Zero(t, got.MessageCount)
	require.Empty(t, got.ActiveTask)
	_, err = turns.AcquireChatTurn(ctx, session, "competing-turn", time.Now().Add(time.Minute))
	require.ErrorIs(t, err, store.ErrConflict)
	require.NoError(t, turns.CommitSessionTurn(ctx, session, "parent-turn", 0, []store.SessionMessage{
		{Role: "user", Content: "create a scheduled task"},
		{Role: "assistant", Content: "the schedule was rejected"},
	}, 7, 11))
}

func TestSessionManagerAppendMessagesSkipsUnownedSession(t *testing.T) {
	for _, tt := range []struct {
		name, owner, uid string
	}{
		{name: "unlocked"},
		{name: "another task", owner: "other-task", uid: "other-uid"},
		{name: "another incarnation", owner: "completed-task", uid: "replacement-uid"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			manager, ss := setupSessionManager()
			ctx := t.Context()
			require.NoError(t, ss.CreateSession(ctx, &store.SessionRecord{
				Namespace: "default", Name: "session", SessionType: "task", ActiveTask: tt.owner, ActiveTaskUID: tt.uid,
			}))
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "completed-task", Namespace: "default", UID: "completed-uid"},
				Spec: corev1alpha1.TaskSpec{
					Prompt: "unowned prompt", SessionRef: &corev1alpha1.SessionReference{Name: "session", Append: true},
				},
				Status: corev1alpha1.TaskStatus{ResultRef: &corev1alpha1.ResultReference{Available: true}},
			}
			results := &countingSessionResultStore{}
			require.NoError(t, manager.AppendMessages(ctx, task, results))
			require.Zero(t, results.reads)
			got, err := ss.GetSession(ctx, "default", "session")
			require.NoError(t, err)
			require.Empty(t, got.Messages)
			require.Zero(t, got.MessageCount)
			require.Equal(t, tt.owner, got.ActiveTask)
			require.Equal(t, tt.uid, got.ActiveTaskUID)
		})
	}
}

type sessionOwnershipChangingResultStore struct {
	store.ResultStore
	beforeRead func()
}

func (s sessionOwnershipChangingResultStore) GetResult(ctx context.Context, namespace, name string) ([]byte, error) {
	s.beforeRead()
	return s.ResultStore.GetResult(ctx, namespace, name)
}

func TestSessionManagerAppendMessagesFencesOwnershipDuringResultRead(t *testing.T) {
	manager, ss := setupSessionManager()
	ctx := t.Context()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "completed-task", Namespace: "default", UID: "completed-uid"},
		Spec: corev1alpha1.TaskSpec{
			Prompt: "stale prompt", SessionRef: &corev1alpha1.SessionReference{Name: "session", Create: true, Append: true},
		},
		Status: corev1alpha1.TaskStatus{ResultRef: &corev1alpha1.ResultReference{Available: true}},
	}
	require.NoError(t, manager.AcquireLock(ctx, task))
	require.NoError(t, ss.SaveResult(ctx, task.Namespace, task.Name, []byte("stale response")))
	session := &store.SessionRecord{Namespace: "default", Name: "session", SessionType: "task"}
	results := sessionOwnershipChangingResultStore{ResultStore: ss, beforeRead: func() {
		require.NoError(t, manager.ReleaseLock(ctx, task))
		_, err := ss.AcquireChatTurn(ctx, session, "new-chat-turn", time.Now().Add(time.Minute))
		require.NoError(t, err)
	}}
	require.ErrorIs(t, manager.AppendMessages(ctx, task, results), store.ErrConflict)
	got, err := ss.GetSession(ctx, "default", "session")
	require.NoError(t, err)
	require.Empty(t, got.Messages)
	require.Zero(t, got.MessageCount)
	require.NoError(t, ss.CommitSessionTurn(ctx, session, "new-chat-turn", 0, []store.SessionMessage{
		{Role: "user", Content: "new request"}, {Role: "assistant", Content: "new response"},
	}, 7, 11))
}
