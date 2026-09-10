/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/sessioncontext"
	"github.com/orka-agents/orka/internal/store"
)

func sessionManagerContextTask() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "context-task", Namespace: defaultNS, UID: "context-task-uid"},
		Spec: corev1alpha1.TaskSpec{
			Prompt: "Keep the public API unchanged.",
			SessionRef: &corev1alpha1.SessionReference{
				Name: "context-session", Create: true, Append: true,
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}
}

func TestSessionManagerAppendMessagesPreservesCommittedWorkerContext(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()
	task := sessionManagerContextTask()
	task.Spec.AI = &corev1alpha1.AISpec{Prompt: "The completion path must preserve the worker's saved request."}
	require.NoError(t, sm.AcquireLock(ctx, task))
	uid := string(task.UID)
	_, err := ss.AppendContextMessages(ctx, store.SessionContextWrite{
		Namespace: task.Namespace, SessionName: task.Spec.SessionRef.Name, OwnerName: task.Name, OwnerUID: uid,
	}, []store.SessionMessage{
		{
			ID: sessioncontext.PromptMessageID(uid), Role: sessionTestRoleUser, Content: "The worker's canonical request.",
			SourceType: sessioncontext.SourceType, SourceRef: task.Name,
		},
		{
			ID: sessioncontext.TaskMessagePrefix(uid) + "1", Role: sessionTestRoleAssistant,
			ToolCalls: []any{map[string]any{"id": "inspect-call", "name": "inspect"}},
		},
		{
			ID: sessioncontext.TaskMessagePrefix(uid) + "2", Role: "tool", ToolCallID: "inspect-call",
			Content: strings.Repeat("A saved inspection finding. ", 500),
		},
		{
			ID: sessioncontext.FinalMessageID(uid), Role: sessionTestRoleAssistant,
			Content:    strings.Repeat("The worker's final answer with supporting evidence. ", 300),
			SourceType: sessioncontext.SourceType, SourceRef: task.Name,
		},
	})
	require.NoError(t, err)
	require.NoError(t, ss.SaveResult(ctx, task.Namespace, task.Name, []byte("A separate completion result must not overwrite saved worker context.")))
	before, err := ss.GetSession(ctx, task.Namespace, task.Spec.SessionRef.Name)
	require.NoError(t, err)
	require.Len(t, before.Messages, 4)
	require.Equal(t, sessioncontext.FinalMessageID(uid), before.Messages[3].Metadata[store.SessionContextOutputRefKey])

	for range 3 {
		require.NoError(t, sm.AppendMessages(ctx, task, ss))
	}
	after, err := ss.GetSession(ctx, task.Namespace, task.Spec.SessionRef.Name)
	require.NoError(t, err)
	require.Equal(t, before.MessageCount, after.MessageCount)
	require.Equal(t, before.Messages, after.Messages, "completion must preserve canonical previews, IDs, roles, and tool relationships")
}

func TestSessionManagerAppendMessagesRepeatedCompletionIsIdempotent(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()
	task := sessionManagerContextTask()
	require.NoError(t, sm.AcquireLock(ctx, task))
	require.NoError(t, ss.SaveResult(ctx, task.Namespace, task.Name, []byte("The public API is unchanged.")))
	require.NoError(t, sm.AppendMessages(ctx, task, ss))
	first, err := ss.GetSession(ctx, task.Namespace, task.Spec.SessionRef.Name)
	require.NoError(t, err)
	require.Len(t, first.Messages, 2)
	require.Equal(t, 2, first.MessageCount)
	require.Equal(t, sessioncontext.PromptMessageID(string(task.UID)), first.Messages[0].ID)
	require.Equal(t, sessionTestRoleUser, first.Messages[0].Role)
	require.Equal(t, task.Spec.Prompt, first.Messages[0].Content)
	require.Equal(t, sessioncontext.FinalMessageID(string(task.UID)), first.Messages[1].ID)
	require.Equal(t, sessionTestRoleAssistant, first.Messages[1].Role)
	require.Equal(t, "The public API is unchanged.", first.Messages[1].Content)

	for range 3 {
		require.NoError(t, sm.AppendMessages(ctx, task, ss))
	}
	repeated, err := ss.GetSession(ctx, task.Namespace, task.Spec.SessionRef.Name)
	require.NoError(t, err)
	require.Equal(t, first.MessageCount, repeated.MessageCount)
	require.Equal(t, first.Messages, repeated.Messages)
}

func TestSessionManagerAppendMessagesCompletesPartiallySavedWorkerContext(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()
	task := sessionManagerContextTask()
	require.NoError(t, sm.AcquireLock(ctx, task))
	_, err := ss.AppendContextMessages(ctx, store.SessionContextWrite{
		Namespace: task.Namespace, SessionName: task.Spec.SessionRef.Name, OwnerName: task.Name, OwnerUID: string(task.UID),
	}, []store.SessionMessage{{
		ID: sessioncontext.PromptMessageID(string(task.UID)), Role: sessionTestRoleUser,
		Content:    "The worker already saved this canonical request.",
		SourceType: sessioncontext.SourceType, SourceRef: task.Name,
	}})
	require.NoError(t, err)
	before, err := ss.GetSession(ctx, task.Namespace, task.Spec.SessionRef.Name)
	require.NoError(t, err)
	require.Len(t, before.Messages, 1)
	require.NoError(t, ss.SaveResult(ctx, task.Namespace, task.Name, []byte("Final answer recovered from the result store.")))

	for range 2 {
		require.NoError(t, sm.AppendMessages(ctx, task, ss))
	}
	after, err := ss.GetSession(ctx, task.Namespace, task.Spec.SessionRef.Name)
	require.NoError(t, err)
	require.Len(t, after.Messages, 2)
	require.Equal(t, 2, after.MessageCount)
	require.Equal(t, before.Messages[0], after.Messages[0], "saving a final response must preserve the existing prompt")
	require.Equal(t, sessioncontext.FinalMessageID(string(task.UID)), after.Messages[1].ID)
	require.Equal(t, sessionTestRoleAssistant, after.Messages[1].Role)
	require.Equal(t, "Final answer recovered from the result store.", after.Messages[1].Content)
}

func TestSessionManagerAppendMessagesSeparatesRecreatedTaskUIDs(t *testing.T) {
	sm, ss := setupSessionManager()
	ctx := context.Background()
	firstTask := sessionManagerContextTask()
	require.NoError(t, sm.AcquireLock(ctx, firstTask))
	require.NoError(t, ss.SaveResult(ctx, firstTask.Namespace, firstTask.Name, []byte("First Task answer.")))
	require.NoError(t, sm.AppendMessages(ctx, firstTask, ss))
	first, err := ss.GetSession(ctx, firstTask.Namespace, firstTask.Spec.SessionRef.Name)
	require.NoError(t, err)
	require.Len(t, first.Messages, 2)
	require.NoError(t, sm.ReleaseLock(ctx, firstTask))

	recreated := firstTask.DeepCopy()
	recreated.UID = "replacement-task-uid"
	recreated.Spec.Prompt = "Continue with the parser investigation."
	require.NoError(t, sm.AcquireLock(ctx, recreated))
	require.NoError(t, ss.SaveResult(ctx, recreated.Namespace, recreated.Name, []byte("Replacement Task answer.")))
	for range 2 {
		require.NoError(t, sm.AppendMessages(ctx, recreated, ss))
	}
	// A delayed completion retry for the old UID must also leave both turns intact.
	require.NoError(t, sm.AppendMessages(ctx, firstTask, ss))
	after, err := ss.GetSession(ctx, recreated.Namespace, recreated.Spec.SessionRef.Name)
	require.NoError(t, err)
	require.Len(t, after.Messages, 4)
	require.Equal(t, 4, after.MessageCount)
	require.Equal(t, first.Messages, after.Messages[:2])
	require.Equal(t, sessioncontext.PromptMessageID(string(recreated.UID)), after.Messages[2].ID)
	require.Equal(t, sessionTestRoleUser, after.Messages[2].Role)
	require.Equal(t, recreated.Spec.Prompt, after.Messages[2].Content)
	require.Equal(t, sessioncontext.FinalMessageID(string(recreated.UID)), after.Messages[3].ID)
	require.Equal(t, sessionTestRoleAssistant, after.Messages[3].Role)
	require.Equal(t, "Replacement Task answer.", after.Messages[3].Content)
}
