package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/sessioncontext"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func TestTaskSessionContextPreservesNumericArguments(t *testing.T) {
	f := newTaskSessionContextAPI(t)
	source := f.message("numeric-source", "assistant", "Inspect the selected record.")
	source.Input = map[string]any{"record_id": json.Number("9007199254740993")}
	source.ToolCalls = []llm.ToolCall{{ID: "numeric-call", Name: "inspect", Arguments: json.RawMessage(`{"record_id":9007199254740993}`)}}
	status, body := f.request(t, http.MethodPost, f.path()+"/messages", []store.SessionMessage{source})
	require.Equal(t, http.StatusOK, status, string(body))
	require.Equal(t, 2, strings.Count(string(body), "9007199254740993"))
	page, err := f.store.ReadSessionHistory(context.Background(), store.SessionHistoryRead{
		Namespace: f.task.Namespace, SessionName: f.sessionName, MessageID: source.ID, Limit: 4096,
	})
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(page.Data, "9007199254740993"))
}

func TestTaskSessionContextHistoryPageDropsUnverifiedDuplicateFields(t *testing.T) {
	f := newTaskSessionContextAPI(t)
	source := f.message("history-source", "assistant", "Saved evidence.")
	status, body := f.request(t, http.MethodPost, f.path()+"/messages", []store.SessionMessage{source})
	require.Equal(t, http.StatusOK, status, string(body))
	page, err := f.store.ReadSessionHistory(context.Background(), store.SessionHistoryRead{
		Namespace: f.task.Namespace, SessionName: f.sessionName, MessageID: source.ID, Limit: 4096,
	})
	require.NoError(t, err)
	canonical, err := json.Marshal(page)
	require.NoError(t, err)
	const hiddenValue = "history-envelope-private-placeholder"
	duplicate := `{"data":"token is ` + hiddenValue + `",` + string(canonical[1:])
	message := f.message("history-page", "tool", duplicate)
	message.Name, message.ToolCallID = sessioncontext.HistoryToolName, "history-call"
	status, body = f.request(t, http.MethodPost, f.path()+"/messages", []store.SessionMessage{message})
	require.Equal(t, http.StatusOK, status, string(body))
	require.NotContains(t, string(body), hiddenValue)
	archived, err := f.store.ReadSessionHistory(context.Background(), store.SessionHistoryRead{
		Namespace: f.task.Namespace, SessionName: f.sessionName, MessageID: message.ID, Limit: 4096,
	})
	require.NoError(t, err)
	var saved store.SessionMessage
	require.NoError(t, json.Unmarshal([]byte(archived.Data), &saved))
	require.Equal(t, string(canonical), saved.Content)
}

func TestTaskSessionContextByteTrimmingKeepsToolExchangeAtomic(t *testing.T) {
	f := newTaskSessionContextAPI(t)
	arguments, err := json.Marshal(map[string]string{"path": "/workspace/large.txt", "content": strings.Repeat("x", sessioncontext.MaxBootstrapBytes+4096)})
	require.NoError(t, err)
	f.seed(t, f.task.Namespace, f.sessionName,
		store.SessionMessage{ID: "prior-user", Role: "user", Content: "Prepare the file."},
		store.SessionMessage{ID: "prior-call", Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "large-call", Name: "file_write", Arguments: arguments}}},
		store.SessionMessage{ID: "prior-result", Role: "tool", ToolCallID: "large-call", Name: "file_write", Content: "The file was saved."},
	)
	status, body := f.request(t, http.MethodGet, f.path(), nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var bootstrap sessioncontext.Bootstrap
	require.NoError(t, json.Unmarshal(body, &bootstrap))
	require.False(t, bootstrap.PromptIncluded)
	require.Empty(t, bootstrap.Messages)
}

func TestTaskSessionContextByteTrimmingRejectsIncompleteToolTail(t *testing.T) {
	f := newTaskSessionContextAPI(t)
	arguments, err := json.Marshal(map[string]string{"content": strings.Repeat("x", sessioncontext.MaxBootstrapBytes+4096)})
	require.NoError(t, err)
	f.seed(t, f.task.Namespace, f.sessionName,
		store.SessionMessage{ID: "prior-user", Role: "user", Content: "Prepare both files."},
		store.SessionMessage{ID: "prior-call", Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "first-call", Name: "file_write", Arguments: arguments},
			{ID: "second-call", Name: "file_write", Arguments: json.RawMessage(`{}`)},
		}},
		store.SessionMessage{ID: "prior-result", Role: "tool", ToolCallID: "first-call", Name: "file_write", Content: "The first file was saved."},
	)
	status, body := f.request(t, http.MethodGet, f.path(), nil)
	require.Equal(t, http.StatusUnprocessableEntity, status, string(body))
	require.Contains(t, string(body), "incomplete exchange")
}

func TestTaskSessionContextBootstrapUsesOlderCheckpointWithinTaskBoundary(t *testing.T) {
	f := newTaskSessionContextAPI(t)
	f.seed(t, f.task.Namespace, f.sessionName,
		store.SessionMessage{ID: "constraint", Role: "user", Content: "Keep the API unchanged."},
		store.SessionMessage{ID: "finding", Role: "assistant", Content: "The retry setting is not the cause."},
		store.SessionMessage{ID: "middle-user", Role: "user", Content: "Check ordering."},
		store.SessionMessage{ID: "middle-assistant", Role: "assistant", Content: "Ordering is stable."},
		store.SessionMessage{ID: "recent-user", Role: "user", Content: "Inspect the parser."},
		store.SessionMessage{ID: "allowed-tail", Role: "assistant", Content: "The parser drops an empty result."},
		store.SessionMessage{ID: "future-user", Role: "user", Content: "Later private request."},
		store.SessionMessage{ID: "future-tail", Role: "assistant", Content: "Later private answer."},
	)
	older := f.checkpoint("older-checkpoint", "finding", "Keep the API unchanged. The retry setting is not the cause.", "constraint", "finding")
	require.NoError(t, f.store.SaveSessionCheckpoint(context.Background(), f.write(), older))
	future := f.checkpoint("future-checkpoint", "future-tail", "Later private answer.", "future-tail")
	require.NoError(t, f.store.SaveSessionCheckpoint(context.Background(), f.write(), future))
	f.task.Spec.SessionRef.MaxMessages = 2
	f.task.Spec.SessionRef.ThroughMessageID = "allowed-tail"
	f.task.Spec.Prompt = "Continue without changing the current request."
	f.updateTask(t)
	status, body := f.request(t, http.MethodGet, f.path()+"?maxMessages=64&throughMessageID=future-tail&sessionName=other", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var bootstrap sessioncontext.Bootstrap
	require.NoError(t, json.Unmarshal(body, &bootstrap))
	require.Equal(t, f.sessionName, bootstrap.SessionName)
	require.Equal(t, "allowed-tail", bootstrap.ThroughMessageID)
	require.False(t, bootstrap.Writable)
	require.False(t, bootstrap.PromptIncluded)
	require.NotNil(t, bootstrap.Checkpoint)
	require.Equal(t, older.ID, bootstrap.Checkpoint.ID)
	require.Equal(t, older.Note, bootstrap.Checkpoint.Note)
	require.Equal(t, older.SourceMessageIDs, bootstrap.Checkpoint.SourceMessageIDs)
	require.Len(t, bootstrap.Messages, 2)
	require.Equal(t, "recent-user", bootstrap.Messages[0].ID)
	require.Equal(t, "user", bootstrap.Messages[0].Role)
	require.Equal(t, "allowed-tail", bootstrap.Messages[1].ID)
	require.Equal(t, "assistant", bootstrap.Messages[1].Role)
	require.NotContains(t, string(body), "Later private")
	require.NotContains(t, string(body), f.task.Spec.Prompt)
	require.LessOrEqual(t, len(body), sessioncontext.MaxBootstrapBytes)
}

func TestTaskSessionContextRestartDetectionPrecedesRecentSuffix(t *testing.T) {
	for _, finalOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("final-only=%t", finalOnly), func(t *testing.T) {
			f := newTaskSessionContextAPI(t)
			id := sessioncontext.TaskMessagePrefix(string(f.task.UID)) + "1"
			if finalOnly {
				id = sessioncontext.FinalMessageID(string(f.task.UID))
			}
			f.seed(t, f.task.Namespace, f.sessionName,
				store.SessionMessage{ID: "before-task", Role: "user", Content: "Earlier request."},
				store.SessionMessage{ID: id, Role: "assistant", Content: "Committed model output."},
				store.SessionMessage{ID: "latest", Role: "user", Content: "Later history."},
			)
			f.task.Spec.SessionRef.MaxMessages = 1
			f.updateTask(t)
			status, body := f.request(t, http.MethodGet, f.path(), nil)
			require.Equal(t, http.StatusOK, status, string(body))
			var bootstrap sessioncontext.Bootstrap
			require.NoError(t, json.Unmarshal(body, &bootstrap))
			require.True(t, bootstrap.TaskHistoryExists)
			require.Len(t, bootstrap.Messages, 1)
			require.Equal(t, "latest", bootstrap.Messages[0].ID)

			f.task.Spec.SessionRef.ThroughMessageID = "before-task"
			f.updateTask(t)
			status, body = f.request(t, http.MethodGet, f.path(), nil)
			require.Equal(t, http.StatusOK, status, string(body))
			require.NoError(t, json.Unmarshal(body, &bootstrap))
			require.False(t, bootstrap.TaskHistoryExists, "presence checks must not expose later history")
		})
	}
}

func TestTaskSessionContextBootstrapKeepsPromptOutsideCheckpoint(t *testing.T) {
	f := newTaskSessionContextAPI(t)
	current := "Current instruction must survive exactly. " + strings.Repeat("詳細 ", 10000)
	f.seed(t, f.task.Namespace, f.sessionName,
		store.SessionMessage{ID: "prior-user", Role: "user", Content: "Keep the API unchanged."},
		store.SessionMessage{ID: "prior-assistant", Role: "assistant", Content: "The retry cause was ruled out."},
		store.SessionMessage{ID: "recent-user", Role: "user", Content: "Inspect the parser."},
		store.SessionMessage{ID: "recent-assistant", Role: "assistant", Content: "Parser validation is next."},
		store.SessionMessage{ID: "current-user", Role: "user", Content: current},
		store.SessionMessage{ID: "future-user", Role: "user", Content: "A later request."},
	)
	older := f.checkpoint("older-checkpoint", "prior-assistant", "Keep the API unchanged. The retry cause was ruled out.", "prior-user", "prior-assistant")
	require.NoError(t, f.store.SaveSessionCheckpoint(context.Background(), f.write(), older))
	currentCheckpoint := f.checkpoint("current-checkpoint", "current-user", "Summary includes the current request.", "current-user")
	require.NoError(t, f.store.SaveSessionCheckpoint(context.Background(), f.write(), currentCheckpoint))
	f.task.Spec.SessionRef.ThroughMessageID = "current-user"
	f.task.Spec.SessionRef.PromptIncluded = true
	f.task.Spec.SessionRef.MaxMessages = 3
	f.updateTask(t)
	status, body := f.request(t, http.MethodGet, f.path(), nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var bootstrap sessioncontext.Bootstrap
	require.NoError(t, json.Unmarshal(body, &bootstrap))
	require.True(t, bootstrap.PromptIncluded)
	require.False(t, bootstrap.Writable)
	require.NotNil(t, bootstrap.Checkpoint)
	require.Equal(t, older.ID, bootstrap.Checkpoint.ID)
	require.Len(t, bootstrap.Messages, 3)
	require.Equal(t, "current-user", bootstrap.Messages[2].ID)
	require.Equal(t, "user", bootstrap.Messages[2].Role)
	require.Equal(t, current, bootstrap.Messages[2].Content)
	require.NotContains(t, string(body), currentCheckpoint.ID)
	require.NotContains(t, string(body), "A later request.")
	f.task.Spec.SessionRef.MaxMessages = 1
	f.updateTask(t)
	status, body = f.request(t, http.MethodGet, f.path(), nil)
	require.Equal(t, http.StatusOK, status, string(body))
	bootstrap = sessioncontext.Bootstrap{}
	require.NoError(t, json.Unmarshal(body, &bootstrap))
	require.Nil(t, bootstrap.Checkpoint)
	require.Len(t, bootstrap.Messages, 1)
	require.Equal(t, current, bootstrap.Messages[0].Content)
}

func TestTaskSessionContextBootstrapByteLimitDoesNotShortenCurrentRequest(t *testing.T) {
	for _, size := range []int{40000, sessioncontext.MaxBootstrapBytes + 1} {
		t.Run(fmt.Sprintf("current bytes %d", size), func(t *testing.T) {
			f := newTaskSessionContextAPI(t)
			current := strings.Repeat("c", size)
			f.seed(t, f.task.Namespace, f.sessionName,
				store.SessionMessage{ID: "old-user", Role: "user", Content: strings.Repeat("older ", 15000)},
				store.SessionMessage{ID: "old-assistant", Role: "assistant", Content: strings.Repeat("answer ", 13000)},
				store.SessionMessage{ID: "current", Role: "user", Content: current},
			)
			f.task.Spec.SessionRef.ThroughMessageID = "current"
			f.task.Spec.SessionRef.PromptIncluded = true
			f.updateTask(t)
			status, body := f.request(t, http.MethodGet, f.path(), nil)
			if size > sessioncontext.MaxBootstrapBytes {
				require.Equal(t, http.StatusUnprocessableEntity, status, string(body))
				require.Contains(t, string(body), "current request was not shortened")
			} else {
				require.Equal(t, http.StatusOK, status, string(body))
				require.LessOrEqual(t, len(body), sessioncontext.MaxBootstrapBytes)
				var bootstrap sessioncontext.Bootstrap
				require.NoError(t, json.Unmarshal(body, &bootstrap))
				require.NotEmpty(t, bootstrap.Messages)
				require.Equal(t, current, bootstrap.Messages[len(bootstrap.Messages)-1].Content)
			}
			saved, err := f.store.LoadTranscriptThrough(context.Background(), f.task.Namespace, f.sessionName, "current", 1)
			require.NoError(t, err)
			require.Equal(t, current, saved[0].Content)
		})
	}
}

func TestTaskSessionContextHistoryRejectsForgedSessionAndBoundary(t *testing.T) {
	f := newTaskSessionContextAPI(t)
	f.seed(t, f.task.Namespace, f.sessionName,
		store.SessionMessage{ID: "allowed", Role: "user", Content: "An allowed earlier constraint."},
		store.SessionMessage{ID: "boundary", Role: "assistant", Content: "The allowed final message."},
		store.SessionMessage{ID: "future", Role: "assistant", Content: "Private future history."},
	)
	f.createSession(t, f.task.Namespace, "other-session", "task")
	f.seed(t, f.task.Namespace, "other-session", store.SessionMessage{ID: "foreign-session", Role: "user", Content: "Private other Session history."})
	f.createSession(t, "other-namespace", f.sessionName, "task")
	f.seed(t, "other-namespace", f.sessionName, store.SessionMessage{ID: "foreign-namespace", Role: "user", Content: "Private other namespace history."})
	f.task.Spec.SessionRef.ThroughMessageID = "boundary"
	f.updateTask(t)
	for _, test := range []struct {
		name, path string
		status     int
	}{
		{"allowed", f.path() + "/history/allowed?limit=4096", http.StatusOK},
		{"future cutoff override", f.path() + "/history/future?throughMessageID=future&throughMessageId=future", http.StatusNotFound},
		{"other Session", f.path() + "/history/foreign-session?sessionName=other-session&session=other-session", http.StatusNotFound},
		{"other namespace query", f.path() + "/history/foreign-namespace?namespace=other-namespace", http.StatusNotFound},
		{"other namespace path", strings.Replace(f.path(), "/default/", "/other-namespace/", 1) + "/history/foreign-namespace", http.StatusForbidden},
		{"encoded foreign Session ID", f.path() + "/history/" + url.PathEscape("other-session/foreign-session"), http.StatusNotFound},
		{"negative offset", f.path() + "/history/allowed?offset=-1", http.StatusBadRequest},
		{"non-numeric offset", f.path() + "/history/allowed?offset=all", http.StatusBadRequest},
		{"zero limit", f.path() + "/history/allowed?limit=0", http.StatusBadRequest},
		{"oversized limit", f.path() + "/history/allowed?limit=16385", http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := f.request(t, http.MethodGet, test.path, nil)
			require.Equal(t, test.status, status, string(body))
			require.NotContains(t, string(body), "Private")
			if test.status == http.StatusOK {
				var page store.SessionHistoryResult
				require.NoError(t, json.Unmarshal(body, &page))
				require.Equal(t, "allowed", page.MessageID)
				require.Equal(t, "user", page.Role)
				require.Contains(t, page.Data, "An allowed earlier constraint.")
			}
		})
	}
}

func TestTaskSessionContextWritesRequireCurrentWorkerAndAppendingSession(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*testing.T, *taskSessionContextAPI)
		status int
	}{
		{"missing identity", func(_ *testing.T, f *taskSessionContextAPI) { f.user = nil }, http.StatusUnauthorized},
		{"namespace-only identity", func(_ *testing.T, f *taskSessionContextAPI) {
			f.user = &UserInfo{Username: "system:serviceaccount:default:worker"}
		}, http.StatusForbidden},
		{"old Pod UID", func(_ *testing.T, f *taskSessionContextAPI) {
			f.user = internalCallerAuthWorkerUser(f.pod.Name, "expired-pod-uid")
		}, http.StatusForbidden},
		{"old Job", func(t *testing.T, f *taskSessionContextAPI) {
			f.task.Status.JobName = "replacement-job"
			f.updateTask(t)
		}, http.StatusForbidden},
		{"old Task UID", func(t *testing.T, f *taskSessionContextAPI) {
			f.job.OwnerReferences[0].UID = types.UID("previous-task-uid")
			require.NoError(t, f.client.Update(context.Background(), f.job))
		}, http.StatusForbidden},
		{"old Job UID", func(t *testing.T, f *taskSessionContextAPI) {
			f.pod.OwnerReferences[0].UID = types.UID("previous-job-uid")
			require.NoError(t, f.client.Update(context.Background(), f.pod))
		}, http.StatusForbidden},
		{"completed Task", func(t *testing.T, f *taskSessionContextAPI) {
			f.task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
			f.updateTask(t)
		}, http.StatusForbidden},
		{"deleting Task", func(t *testing.T, f *taskSessionContextAPI) {
			f.task.Finalizers = []string{"test.orka.ai/retain"}
			f.updateTask(t)
			require.NoError(t, f.client.Delete(context.Background(), f.task))
		}, http.StatusForbidden},
		{"non-appending Session", func(t *testing.T, f *taskSessionContextAPI) {
			f.task.Spec.SessionRef.Append = false
			f.updateTask(t)
		}, http.StatusForbidden},
		{"pinned history", func(t *testing.T, f *taskSessionContextAPI) {
			f.task.Spec.SessionRef.ThroughMessageID = "bounded-message"
			f.updateTask(t)
		}, http.StatusForbidden},
		{"included prompt", func(t *testing.T, f *taskSessionContextAPI) {
			f.task.Spec.SessionRef.PromptIncluded = true
			f.updateTask(t)
		}, http.StatusForbidden},
		{"no Session", func(t *testing.T, f *taskSessionContextAPI) {
			f.task.Spec.SessionRef = nil
			f.updateTask(t)
		}, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newTaskSessionContextAPI(t)
			test.change(t, f)
			message := f.message("source", "user", "Retain the existing API.")
			checkpoint := f.checkpoint("checkpoint", message.ID, "Retain the existing API.", message.ID)
			for _, request := range []struct {
				path string
				body any
			}{{"/messages", []store.SessionMessage{message}}, {"/checkpoints", checkpoint}} {
				status, body := f.request(t, http.MethodPost, f.path()+request.path, request.body)
				require.Equal(t, test.status, status, string(body))
			}
			saved, err := f.store.GetSession(context.Background(), f.task.Namespace, f.sessionName)
			require.NoError(t, err)
			require.Zero(t, saved.MessageCount)
		})
	}
}

func TestTaskSessionContextMessageIdentityRolesAndIdempotency(t *testing.T) {
	f := newTaskSessionContextAPI(t)
	request := f.message("request", "user", "Keep the API unchanged.")
	call := f.message("call", "assistant", "Inspect the parser.")
	call.ToolCalls = []any{map[string]any{"id": "call-1", "name": "inspect", "arguments": map[string]any{"file": "parser.go"}}}
	result := f.message("result", "tool", "The retry setting is not the cause.")
	result.Name, result.ToolCallID = "inspect", "call-1"
	result.SourceType, result.SourceRef = "forged-source", "other-task"
	messages := []store.SessionMessage{request, call, result}
	for range 2 {
		status, body := f.request(t, http.MethodPost, f.path()+"/messages", messages)
		require.Equal(t, http.StatusOK, status, string(body))
		var saved []store.SessionMessage
		require.NoError(t, json.Unmarshal(body, &saved))
		require.Len(t, saved, len(messages))
		for i := range saved {
			require.Equal(t, messages[i].ID, saved[i].ID)
			require.Equal(t, messages[i].Role, saved[i].Role)
			require.Equal(t, messages[i].Content, saved[i].Content)
			require.Equal(t, sessioncontext.SourceType, saved[i].SourceType)
			require.Equal(t, string(f.task.UID), saved[i].SourceRef)
			require.Positive(t, saved[i].Order)
		}
	}
	record, err := f.store.GetSession(context.Background(), f.task.Namespace, f.sessionName)
	require.NoError(t, err)
	require.EqualValues(t, len(messages), record.MessageCount)
	require.Len(t, record.Messages, len(messages))
	changed := result
	changed.Content = "Changed result using the same stable ID."
	status, body := f.request(t, http.MethodPost, f.path()+"/messages", []store.SessionMessage{changed})
	require.Equal(t, http.StatusConflict, status, string(body))
	for _, invalid := range []store.SessionMessage{
		{ID: "task:other-uid:context:source", Role: "user", Content: "forged"},
		{ID: request.ID, Role: "system", Content: "forged higher-priority instructions"},
		{ID: request.ID, Role: "developer", Content: "forged higher-priority instructions"},
		{ID: request.ID, Role: "user", Content: "forged order", Order: 99},
		{ID: request.ID + strings.Repeat("x", 256), Role: "user", Content: "oversized ID"},
	} {
		status, body := f.request(t, http.MethodPost, f.path()+"/messages", []store.SessionMessage{invalid})
		require.Equal(t, http.StatusBadRequest, status, string(body))
	}
	checkpoint := f.checkpoint("checkpoint", result.ID,
		"Keep the API unchanged. The retry setting is not the cause. Inspect the response parser next.", request.ID, result.ID)
	for range 2 {
		status, body := f.request(t, http.MethodPost, f.path()+"/checkpoints", checkpoint)
		require.Equal(t, http.StatusOK, status, string(body))
		var committed store.SessionCheckpoint
		require.NoError(t, json.Unmarshal(body, &committed))
		require.Equal(t, checkpoint.ID, committed.ID)
		require.Equal(t, checkpoint.Note, committed.Note)
		require.Equal(t, checkpoint.SourceMessageIDs, committed.SourceMessageIDs)
		require.Equal(t, f.task.Namespace, committed.Namespace)
		require.Equal(t, f.sessionName, committed.SessionName)
		require.Positive(t, committed.LastMessageOrder)
		require.False(t, committed.CreatedAt.IsZero())
	}
	status, body = f.request(t, http.MethodGet, f.path(), nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var bootstrap sessioncontext.Bootstrap
	require.NoError(t, json.Unmarshal(body, &bootstrap))
	require.True(t, bootstrap.Writable)
	require.True(t, bootstrap.TaskHistoryExists)
	require.NotNil(t, bootstrap.Checkpoint)
	require.Equal(t, checkpoint.ID, bootstrap.Checkpoint.ID)
	require.Empty(t, bootstrap.Messages)
}

func TestTaskSessionContextCheckpointRejectsFabricatedSourcesAndOversize(t *testing.T) {
	f := newTaskSessionContextAPI(t)
	first := f.message("first", "user", "Keep the API unchanged.")
	last := f.message("last", "assistant", "Later finding.")
	status, body := f.request(t, http.MethodPost, f.path()+"/messages", []store.SessionMessage{first, last})
	require.Equal(t, http.StatusOK, status, string(body))
	f.createSession(t, f.task.Namespace, "other-session", "task")
	f.seed(t, f.task.Namespace, "other-session", store.SessionMessage{ID: "foreign", Role: "assistant", Content: "Another Session's finding."})
	for _, test := range []struct {
		name   string
		change func(*store.SessionCheckpoint)
		status int
	}{
		{"missing source", func(c *store.SessionCheckpoint) { c.SourceMessageIDs = []string{"missing"} }, http.StatusNotFound},
		{"foreign source", func(c *store.SessionCheckpoint) { c.SourceMessageIDs = []string{"foreign"} }, http.StatusNotFound},
		{"future source", func(c *store.SessionCheckpoint) { c.SourceMessageIDs = []string{last.ID} }, http.StatusNotFound},
		{"forged namespace", func(c *store.SessionCheckpoint) { c.Namespace = "other-namespace" }, http.StatusBadRequest},
		{"forged Session", func(c *store.SessionCheckpoint) { c.SessionName = "other-session" }, http.StatusBadRequest},
		{"forged Task ID", func(c *store.SessionCheckpoint) { c.ID = "task:other-uid:context:checkpoint" }, http.StatusBadRequest},
		{"unsupported version", func(c *store.SessionCheckpoint) { c.Version++ }, http.StatusBadRequest},
		{"oversized note", func(c *store.SessionCheckpoint) { c.Note = strings.Repeat("n", store.MaxSessionCheckpointBytes+1) }, http.StatusBadRequest},
		{"oversized request", func(c *store.SessionCheckpoint) { c.Note = strings.Repeat("n", 33*1024) }, http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkpoint := f.checkpoint("invalid-checkpoint", first.ID, "Keep the API unchanged.", first.ID)
			test.change(&checkpoint)
			status, body := f.request(t, http.MethodPost, f.path()+"/checkpoints", checkpoint)
			require.Equal(t, test.status, status, string(body))
			_, err := f.store.LoadSessionCheckpoint(context.Background(), f.task.Namespace, f.sessionName, "")
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestTaskSessionContextExpiredOwnerAndDeletedSessionCannotWrite(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted %t", deleted), func(t *testing.T) {
			f := newTaskSessionContextAPI(t)
			first := f.message("first", "user", "Saved requirement.")
			status, body := f.request(t, http.MethodPost, f.path()+"/messages", []store.SessionMessage{first})
			require.Equal(t, http.StatusOK, status, string(body))
			_, err := f.db.ExecContext(context.Background(),
				"UPDATE sessions SET active_task_expires_at = ? WHERE namespace = ? AND name = ?",
				time.Now().UTC().Add(-time.Minute), f.task.Namespace, f.sessionName)
			require.NoError(t, err)
			expected := http.StatusConflict
			if deleted {
				require.NoError(t, f.store.DeleteSession(context.Background(), f.task.Namespace, f.sessionName))
				expected = http.StatusNotFound
			}
			for _, request := range []struct {
				path string
				body any
			}{
				{"/messages", []store.SessionMessage{f.message("late", "assistant", "Late result.")}},
				{"/checkpoints", f.checkpoint("late-checkpoint", first.ID, "Saved requirement.", first.ID)},
			} {
				status, body := f.request(t, http.MethodPost, f.path()+request.path, request.body)
				require.Equal(t, expected, status, string(body))
			}
			record, err := f.store.GetSession(context.Background(), f.task.Namespace, f.sessionName)
			if deleted {
				require.ErrorIs(t, err, store.ErrNotFound)
			} else {
				require.NoError(t, err)
				require.EqualValues(t, 1, record.MessageCount)
			}
			_, err = f.store.LoadSessionCheckpoint(context.Background(), f.task.Namespace, f.sessionName, "")
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestTaskSessionContextHistoryPagesRecoverLargeStoredResult(t *testing.T) {
	f := newTaskSessionContextAPI(t)
	result := f.message("large-result", "tool", strings.Repeat("測定結果 ", 10000)+"The final finding is recoverable.")
	result.Name, result.ToolCallID = "inspect", "call-1"
	call := f.message("call", "assistant", "Inspect the parser.")
	call.ToolCalls = []any{map[string]any{"id": result.ToolCallID, "name": result.Name}}
	status, body := f.request(t, http.MethodPost, f.path()+"/messages", []store.SessionMessage{call, result})
	require.Equal(t, http.StatusOK, status, string(body))
	var saved []store.SessionMessage
	require.NoError(t, json.Unmarshal(body, &saved))
	require.Len(t, saved, 2)
	require.LessOrEqual(t, len(saved[1].Content), store.MaxSessionContextPreviewBytes)
	require.Equal(t, result.ID, saved[1].Metadata[store.SessionContextOutputRefKey])
	require.NotContains(t, saved[1].Content, "The final finding is recoverable.")
	var recovered bytes.Buffer
	offset := 0
	for {
		path := f.path() + "/history/" + url.PathEscape(result.ID) + fmt.Sprintf("?offset=%d&limit=%d", offset, store.MaxSessionHistoryReadBytes)
		status, body := f.request(t, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		var page store.SessionHistoryResult
		require.NoError(t, json.Unmarshal(body, &page))
		require.Equal(t, result.ID, page.MessageID)
		require.Equal(t, result.Role, page.Role)
		require.Equal(t, offset, page.Offset)
		require.Equal(t, len(page.Data), page.NextOffset-page.Offset)
		require.LessOrEqual(t, len(page.Data), store.MaxSessionHistoryReadBytes)
		require.True(t, utf8.ValidString(page.Data))
		recovered.WriteString(page.Data)
		if page.NextOffset == page.TotalBytes {
			break
		}
		require.Greater(t, page.NextOffset, offset)
		offset = page.NextOffset
	}
	var original store.SessionMessage
	require.NoError(t, json.Unmarshal(recovered.Bytes(), &original))
	require.Equal(t, result.Content, original.Content)
	require.Equal(t, result.Name, original.Name)
	require.Equal(t, result.ToolCallID, original.ToolCallID)
	require.Equal(t, result.Role, original.Role)
	runeOffset := bytes.Index(recovered.Bytes(), []byte("測"))
	require.NotEqual(t, -1, runeOffset)
	status, body = f.request(t, http.MethodGet, f.path()+"/history/"+url.PathEscape(result.ID)+fmt.Sprintf("?offset=%d", runeOffset+1), nil)
	require.Equal(t, http.StatusBadRequest, status, string(body))
	checkpoint := f.checkpoint("large-result-checkpoint", result.ID, "The final finding is recoverable from the stored inspection output.", result.ID)
	status, body = f.request(t, http.MethodPost, f.path()+"/checkpoints", checkpoint)
	require.Equal(t, http.StatusOK, status, string(body))
	tooLarge := f.message("oversized-result", "tool", strings.Repeat("x", store.MaxSessionContextMessageBytes+64*1024))
	status, body = f.request(t, http.MethodPost, f.path()+"/messages", []store.SessionMessage{tooLarge})
	require.Equal(t, http.StatusRequestEntityTooLarge, status, string(body))
}

func TestTaskSessionContextGatewayUsesImmutableEventOwnership(t *testing.T) {
	f := newTaskSessionContextAPI(t)
	const gatewaySession, otherGateway = "gateway-session", "other-gateway-session"
	f.createSession(t, f.task.Namespace, gatewaySession, store.SessionTypeGateway)
	f.admitGatewayEvent(t, "event-1", gatewaySession, f.task.Name, string(f.task.UID))
	currentID := store.GatewayUserMessageID("event-1")
	f.seed(t, f.task.Namespace, gatewaySession,
		store.SessionMessage{ID: "gateway-prior", Role: "assistant", Content: "Earlier authorized Gateway answer."},
		store.SessionMessage{ID: currentID, Role: "user", Content: "Current Gateway request."},
		store.SessionMessage{ID: "gateway-future", Role: "user", Content: "Private future Gateway request."},
	)
	f.createSession(t, f.task.Namespace, otherGateway, store.SessionTypeGateway)
	f.admitGatewayEvent(t, "event-2", otherGateway, "other-task", "other-task-uid")
	f.seed(t, f.task.Namespace, otherGateway, store.SessionMessage{ID: "other-gateway-message", Role: "user", Content: "Private other Gateway conversation."})
	f.task.Annotations = map[string]string{
		gatewayruntime.TaskGatewayEventAnnotation: "event-2",
		gatewayruntime.TaskGatewaySession:         otherGateway,
	}
	f.task.Spec.SessionRef.Name = otherGateway
	f.task.Spec.SessionRef.ThroughMessageID = "other-gateway-message"
	f.task.Spec.SessionRef.PromptIncluded = false
	f.task.Spec.SessionRef.MaxMessages = 1
	f.updateTask(t)
	status, body := f.request(t, http.MethodGet, f.path()+"?sessionName="+otherGateway+"&throughMessageID=gateway-future", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var bootstrap sessioncontext.Bootstrap
	require.NoError(t, json.Unmarshal(body, &bootstrap))
	require.Equal(t, gatewaySession, bootstrap.SessionName)
	require.Equal(t, currentID, bootstrap.ThroughMessageID)
	require.True(t, bootstrap.PromptIncluded)
	require.False(t, bootstrap.Writable)
	require.Len(t, bootstrap.Messages, 2)
	require.Equal(t, "Current Gateway request.", bootstrap.Messages[1].Content)
	require.NotContains(t, string(body), "Private")
	for _, id := range []string{"gateway-future", "other-gateway-message"} {
		status, body := f.request(t, http.MethodGet, f.path()+"/history/"+id+"?sessionName="+otherGateway+"&throughMessageID=gateway-future", nil)
		require.Equal(t, http.StatusNotFound, status, string(body))
		require.NotContains(t, string(body), "Private")
	}
	status, body = f.request(t, http.MethodGet, f.path()+"/history/gateway-prior", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	message := f.message("gateway-write", "assistant", "An unowned Gateway write.")
	status, body = f.request(t, http.MethodPost, f.path()+"/messages", []store.SessionMessage{message})
	require.Equal(t, http.StatusForbidden, status, string(body))
	status, body = f.request(t, http.MethodPost, f.path()+"/checkpoints", f.checkpoint("gateway-checkpoint", currentID, "Current Gateway request.", currentID))
	require.Equal(t, http.StatusForbidden, status, string(body))
}

func TestTaskSessionContextGatewayRejectsMissingOrStaleTaskOwnership(t *testing.T) {
	for _, staleEvent := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale event %t", staleEvent), func(t *testing.T) {
			f := newTaskSessionContextAPI(t)
			f.createSession(t, f.task.Namespace, "private-gateway", store.SessionTypeGateway)
			if staleEvent {
				f.admitGatewayEvent(t, "old-event", "private-gateway", f.task.Name, "old-task-uid")
			}
			f.seed(t, f.task.Namespace, "private-gateway", store.SessionMessage{ID: "private-message", Role: "user", Content: "Private Gateway conversation."})
			f.task.Spec.SessionRef.Name = "private-gateway"
			f.updateTask(t)
			for _, path := range []string{f.path(), f.path() + "/history/private-message"} {
				status, body := f.request(t, http.MethodGet, path, nil)
				require.Equal(t, http.StatusForbidden, status, string(body))
				require.NotContains(t, string(body), "Private Gateway conversation.")
			}
		})
	}
}

type taskSessionContextAPI struct {
	db          *sql.DB
	store       *sqlite.Store
	client      client.Client
	task        *corev1alpha1.Task
	job         *batchv1.Job
	pod         *corev1.Pod
	user        *UserInfo
	app         *fiber.App
	sessionName string
}

func newTaskSessionContextAPI(t *testing.T) *taskSessionContextAPI {
	t.Helper()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	f := &taskSessionContextAPI{db: db, store: sqlite.NewStore(db, ":memory:"), task: internalCallerAuthTask(), sessionName: "work-session"}
	f.task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: f.sessionName, Append: true, MaxMessages: 50}
	f.task.Status.Phase = corev1alpha1.TaskPhaseRunning
	f.job = internalCallerAuthJob(f.task, "job-a", "job-uid")
	f.pod = internalCallerAuthPod(f.task, "worker-pod", "worker-pod-uid", f.job)
	f.user = internalCallerAuthWorkerUser(f.pod.Name, string(f.pod.UID))
	f.client = fake.NewClientBuilder().WithScheme(internalCallerAuthScheme(t)).WithObjects(f.task, f.job, f.pod).Build()
	f.createSession(t, f.task.Namespace, f.sessionName, "task")
	require.NoError(t, f.store.AcquireLockUntil(context.Background(), f.task.Namespace, f.sessionName, f.task.Name, string(f.task.UID), time.Now().UTC().Add(time.Hour)))
	handlers := NewInternalHandlers(f.store, f.store, f.store, f.store, f.store, InternalHandlersConfig{Client: f.client, GatewayEventStore: f.store})
	f.app = fiber.New()
	f.app.Use(func(c fiber.Ctx) error {
		if f.user != nil {
			c.Locals(UserInfoContextKey, f.user)
		}
		return c.Next()
	})
	const route = "/internal/v1/tasks/:namespace/:taskName/session-context"
	f.app.Get(route, handlers.GetTaskSessionContext)
	f.app.Post(route+"/messages", handlers.AppendTaskContextMessages)
	f.app.Post(route+"/checkpoints", handlers.SaveTaskSessionCheckpoint)
	f.app.Get(route+"/history/:messageID", handlers.ReadTaskSessionHistory)
	return f
}

func (f *taskSessionContextAPI) path() string {
	return "/internal/v1/tasks/" + f.task.Namespace + "/" + f.task.Name + "/session-context"
}

func (f *taskSessionContextAPI) write() store.SessionContextWrite {
	return store.SessionContextWrite{Namespace: f.task.Namespace, SessionName: f.sessionName, OwnerName: f.task.Name, OwnerUID: string(f.task.UID)}
}

func (f *taskSessionContextAPI) message(id, role, content string) store.SessionMessage {
	return store.SessionMessage{ID: sessioncontext.TaskMessagePrefix(string(f.task.UID)) + id, Role: role, Content: content}
}

func (f *taskSessionContextAPI) checkpoint(id, lastID, note string, sources ...string) store.SessionCheckpoint {
	return store.SessionCheckpoint{
		ID: sessioncontext.TaskMessagePrefix(string(f.task.UID)) + id, LastMessageID: lastID,
		Version: store.SessionCheckpointVersion, Note: note, SourceMessageIDs: sources,
	}
}

func (f *taskSessionContextAPI) updateTask(t *testing.T) {
	t.Helper()
	require.NoError(t, f.client.Update(context.Background(), f.task))
}

func (f *taskSessionContextAPI) createSession(t *testing.T, namespace, name, sessionType string) {
	t.Helper()
	require.NoError(t, f.store.CreateSession(context.Background(), &store.SessionRecord{Namespace: namespace, Name: name, SessionType: sessionType}))
}

func (f *taskSessionContextAPI) seed(t *testing.T, namespace, name string, messages ...store.SessionMessage) {
	t.Helper()
	require.NoError(t, f.store.AppendMessages(context.Background(), namespace, name, messages))
}

func (f *taskSessionContextAPI) admitGatewayEvent(t *testing.T, id, sessionName, taskName, taskUID string) {
	t.Helper()
	now := time.Now().UTC()
	_, _, err := f.store.AdmitGatewayEvent(context.Background(), store.GatewayEventAdmission{Event: store.GatewayEvent{
		ID: id, Namespace: f.task.Namespace, NamespaceUID: "namespace-uid", GatewayUID: "gateway-uid", GatewayGeneration: 1, GatewayName: "chat",
		BindingName: "room", BindingUID: "binding-uid", ExternalEventID: "external-" + id,
		ProtocolVersion: "orka.gateway.v1", EventType: "text", State: store.GatewayEventTaskCreated,
		AccountID: "account", ContextID: "room", SenderID: "sender", Text: "Current Gateway request.",
		ReplyTarget: "room", SessionName: sessionName, TaskName: taskName, TaskUID: taskUID,
		ReceivedAt: now, NextAttemptAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}})
	require.NoError(t, err)
}

func (f *taskSessionContextAPI) request(t *testing.T, method, path string, value any) (int, []byte) {
	t.Helper()
	var body []byte
	if value != nil {
		var err error
		body, err = json.Marshal(value)
		require.NoError(t, err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	response, err := f.app.Test(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, data
}
