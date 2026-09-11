package api

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/tools"
)

func TestBrokeredTranscriptSearchEnforcesSessionScopeAndHistory(t *testing.T) {
	for _, protected := range []bool{false, true} {
		for _, test := range []struct {
			name      string
			args      string
			own       bool
			peer      bool
			forbidden bool
		}{
			{name: "broad search", args: `{"query":"needle"}`, own: true, peer: true},
			{name: "own session", args: `{"query":"needle","session_name":"own-session"}`, own: true},
			{name: "peer session", args: `{"query":"needle","sessionName":"peer-session"}`, peer: true},
			{name: "unrelated session", args: `{"query":"needle","session_name":"unrelated-session"}`, forbidden: true},
			{name: "gateway session", args: `{"query":"needle","session_name":"gateway-session"}`, forbidden: true},
			{name: "role filter follows history bound", args: `{"query":"needle","session_name":"own-session","roles":["assistant"]}`},
			{name: "excluded own session", args: `{"query":"needle","exclude_session_name":"own-session"}`, peer: true},
		} {
			t.Run(test.name+map[bool]string{false: "/unprotected", true: "/protected"}[protected], func(t *testing.T) {
				task, kube, data := setupBrokeredTranscriptSearch(t)
				ctx := brokeredTranscriptSearchContext(t, task, NewTaskTranscriptSearcher(kube, data, data, client.ObjectKeyFromObject(task), string(task.UID), protected, allowBrokeredTaskData))
				result, err := tools.NewSearchTranscriptTool().Execute(ctx, json.RawMessage(test.args))
				if test.forbidden || (!protected && test.name == "peer session") {
					require.Error(t, err)
					require.Empty(t, result)
					return
				}
				require.NoError(t, err)
				var hits []store.TranscriptSearchResult
				require.NoError(t, json.Unmarshal([]byte(result), &hits))
				var want, got []string
				if test.own {
					want = append(want, "own-session/cutoff")
				}
				if test.peer && protected {
					want = append(want, "peer-session/future")
				}
				for _, hit := range hits {
					got = append(got, hit.SessionName+"/"+hit.StableMessageID)
				}
				require.ElementsMatch(t, want, got)
			})
		}
	}
}

func TestBrokeredTranscriptSearchRejectsStaleOrUnavailableIdentity(t *testing.T) {
	for _, failure := range []string{"replaced", "completed", "execution outcome", "deleted", "reader unavailable", "transactions unavailable", "namespace mismatch", "missing UID", "missing cutoff", "prompt guard unavailable"} {
		t.Run(failure, func(t *testing.T) {
			task, kube, data := setupBrokeredTranscriptSearch(t)
			capture := &countingTranscriptSearchStore{SessionStore: data, TaskDataTransactionStore: data}
			var sessions store.SessionStore = capture
			var reader client.Reader = kube
			uid := string(task.UID)
			if failure == "reader unavailable" {
				reader = interceptor.NewClient(kube, interceptor.Funcs{
					Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
						return errors.New("API reader unavailable")
					},
				})
			}
			if failure == "transactions unavailable" {
				sessions = struct{ store.SessionStore }{data}
			}
			if failure == "missing UID" {
				uid = ""
			}
			guard := allowBrokeredTaskData
			if failure == "prompt guard unavailable" {
				guard = nil
			}
			ctx := brokeredTranscriptSearchContext(t, task, NewTaskTranscriptSearcher(reader, sessions, data, client.ObjectKeyFromObject(task), uid, true, guard))
			switch failure {
			case "replaced":
				require.NoError(t, kube.Delete(t.Context(), task))
				task.UID, task.ResourceVersion = "replacement-uid", ""
				require.NoError(t, kube.Create(t.Context(), task))
			case "completed":
				task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
				require.NoError(t, kube.Update(t.Context(), task))
			case "execution outcome":
				task.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{Phase: corev1alpha1.TaskPhaseSucceeded}
				require.NoError(t, kube.Update(t.Context(), task))
			case "deleted":
				require.NoError(t, kube.Delete(t.Context(), task))
			case "namespace mismatch":
				tools.GetToolContext(ctx).Namespace = "another-namespace"
			case "missing cutoff":
				task.Spec.SessionRef.ThroughMessageID = "missing"
				require.NoError(t, kube.Update(t.Context(), task))
			}
			result, err := tools.NewSearchTranscriptTool().Execute(ctx, json.RawMessage(`{"query":"needle","session_name":"own-session"}`))
			if failure == "missing cutoff" {
				require.NoError(t, err)
				require.JSONEq(t, `[]`, result)
				return
			}
			require.Error(t, err)
			require.Empty(t, result)
			require.Zero(t, capture.calls)
		})
	}
}

func TestBrokeredTranscriptSearchRejectsGatewayCaller(t *testing.T) {
	task, kube, data := setupBrokeredTranscriptSearch(t)
	now := time.Now().UTC()
	_, _, err := data.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{Event: store.GatewayEvent{
		ID: "search-event", Namespace: task.Namespace, NamespaceUID: "namespace-uid", GatewayUID: "gateway-uid", GatewayGeneration: 1, GatewayName: "chat",
		BindingName: "room", BindingUID: "binding-uid", ExternalEventID: "search-event",
		ProtocolVersion: "orka.gateway.v1", EventType: "text", State: store.GatewayEventTaskCreated,
		AccountID: "acct", ContextID: "room", SenderID: "sender", Text: "current",
		ReplyTarget: "room", SessionName: "gateway-session", TaskName: task.Name, TaskUID: string(task.UID),
		ReceivedAt: now, NextAttemptAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}})
	require.NoError(t, err)
	// The caller's ordinary SessionRef cannot override durable gateway ownership.
	ctx := brokeredTranscriptSearchContext(t, task, NewTaskTranscriptSearcher(kube, data, data, client.ObjectKeyFromObject(task), string(task.UID), true, allowBrokeredTaskData))
	for _, args := range []string{`{"query":"needle"}`, `{"query":"needle","session_name":"own-session"}`} {
		result, err := tools.NewSearchTranscriptTool().Execute(ctx, json.RawMessage(args))
		require.ErrorContains(t, err, "gateway session transcript search is unavailable")
		require.Empty(t, result)
	}
}

func TestBrokeredTranscriptSearchReauthorizesAfterNamespaceCleanup(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "broad", true: "explicit"}[explicit], func(t *testing.T) {
			task, kube, data := setupBrokeredTranscriptSearch(t)
			lists := 0
			reader := interceptor.NewClient(kube, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, objects client.ObjectList, opts ...client.ListOption) error {
					if err := c.List(ctx, objects, opts...); err != nil {
						return err
					}
					lists++
					if lists != 1 {
						return nil
					}
					storeCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
					defer cancel()
					// This write proves Kubernetes authorization holds no SQLite connection.
					if err := data.SaveResult(storeCtx, task.Namespace, "unrelated", []byte("progress")); err != nil {
						return err
					}
					peer := &corev1alpha1.Task{}
					if err := c.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: "peer"}, peer); err != nil {
						return err
					}
					peer.OwnerReferences, peer.Labels, peer.Annotations = nil, nil, nil
					if err := c.Update(ctx, peer); err != nil {
						return err
					}
					// Cleanup of a different Task's session must invalidate the old list.
					if err := data.DeleteSession(storeCtx, task.Namespace, "peer-session"); err != nil {
						return err
					}
					if err := data.CreateSession(storeCtx, &store.SessionRecord{Namespace: task.Namespace, Name: "peer-session", SessionType: "task"}); err != nil {
						return err
					}
					return data.AppendMessages(storeCtx, task.Namespace, "peer-session", []store.SessionMessage{{Role: "assistant", Content: "needle in replacement history"}})
				},
			})
			ctx := brokeredTranscriptSearchContext(t, task, NewTaskTranscriptSearcher(reader, data, data, client.ObjectKeyFromObject(task), string(task.UID), true, allowBrokeredTaskData))
			args := `{"query":"needle"}`
			if explicit {
				args = `{"query":"needle","session_name":"peer-session"}`
			}
			result, err := tools.NewSearchTranscriptTool().Execute(ctx, json.RawMessage(args))
			require.Equal(t, 2, lists, "namespace cleanup must trigger fresh authorization")
			if explicit {
				require.ErrorContains(t, err, "not authorized for this session")
				require.Empty(t, result)
				return
			}
			require.NoError(t, err)
			var hits []store.TranscriptSearchResult
			require.NoError(t, json.Unmarshal([]byte(result), &hits))
			require.Len(t, hits, 1)
			require.Equal(t, "own-session", hits[0].SessionName)
		})
	}
}

func TestBrokeredTranscriptSearchSerializesReadWithCleanup(t *testing.T) {
	task, kube, _ := setupBrokeredTranscriptSearch(t)
	dbPath := filepath.Join(t.TempDir(), "search.db")
	readDB, err := sqlite.NewDB(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = readDB.Close() })
	cleanupDB, err := sqlite.NewDB(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cleanupDB.Close() })
	_, err = cleanupDB.ExecContext(t.Context(), `PRAGMA busy_timeout=0`)
	require.NoError(t, err)
	data, cleanup := sqlite.NewStore(readDB, dbPath), sqlite.NewStore(cleanupDB, dbPath)
	require.NoError(t, data.CreateSession(t.Context(), &store.SessionRecord{Namespace: task.Namespace, Name: "own-session", SessionType: "task"}))
	require.NoError(t, data.AppendMessages(t.Context(), task.Namespace, "own-session", []store.SessionMessage{{ID: "cutoff", Role: "user", Content: "needle in original history"}}))
	var cleanupErr error
	attempted := false
	racing := &taskReadRaceStore{Store: data, beforeRead: func(ctx context.Context) error {
		attempted = true
		cleanupErr = cleanup.DeleteSession(ctx, task.Namespace, "own-session")
		return nil
	}}
	ctx := brokeredTranscriptSearchContext(t, task, NewTaskTranscriptSearcher(kube, racing, data, client.ObjectKeyFromObject(task), string(task.UID), true, allowBrokeredTaskData))
	result, err := tools.NewSearchTranscriptTool().Execute(ctx, json.RawMessage(`{"query":"needle"}`))
	require.NoError(t, err)
	require.Contains(t, result, "original history")
	require.True(t, attempted)
	require.ErrorContains(t, cleanupErr, "locked")
	require.NoError(t, cleanup.DeleteSession(t.Context(), task.Namespace, "own-session"))
}

func brokeredTranscriptSearchContext(t *testing.T, task *corev1alpha1.Task, searcher tools.TranscriptSearcher) context.Context {
	t.Helper()
	return tools.WithToolContext(t.Context(), &tools.ToolContext{
		Brokered: true, Namespace: task.Namespace, TaskID: task.Name, TaskUID: string(task.UID), TranscriptSearcher: searcher,
	})
}

func setupBrokeredTranscriptSearch(t *testing.T) (*corev1alpha1.Task, client.WithWatch, *sqlite.Store) {
	t.Helper()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	data := sqlite.NewStore(db, ":memory:")
	task := internalCallerAuthTaskObject("caller", "caller-uid", "", "root", "own-session")
	task.Spec.Type = corev1alpha1.TaskTypeAgent
	task.Spec.SessionRef.MaxMessages, task.Spec.SessionRef.ThroughMessageID = 1, "cutoff"
	peer := internalCallerAuthTaskObject("peer", "peer-uid", "", "root", "peer-session")
	peer.Spec.SessionRef.MaxMessages = 1
	kube := fake.NewClientBuilder().WithScheme(internalCallerAuthScheme(t)).WithObjects(
		task, peer,
		internalCallerAuthTaskObject("root", "root-uid", "", "", ""),
		internalCallerAuthTaskObject("unrelated", "unrelated-uid", "", "", "unrelated-session"),
		internalCallerAuthTaskObject("gateway", "gateway-uid", "", "root", "gateway-session"),
		internalCallerAuthTaskObject("unbounded", "unbounded-uid", "", "root", "own-session"),
	).Build()
	for _, name := range []string{"own-session", "peer-session", "unrelated-session", "gateway-session"} {
		sessionType := "task"
		if name == "gateway-session" {
			sessionType = store.SessionTypeGateway
		}
		require.NoError(t, data.CreateSession(t.Context(), &store.SessionRecord{Namespace: task.Namespace, Name: name, SessionType: sessionType}))
		require.NoError(t, data.AppendMessages(t.Context(), task.Namespace, name, []store.SessionMessage{
			{ID: "old", Order: 1, Role: "assistant", Content: "needle in old history"},
			{ID: "cutoff", Order: 2, Role: "user", Content: "needle at cutoff"},
			{ID: "future", Order: 3, Role: "assistant", Content: "needle in future history"},
		}))
	}
	return task, kube, data
}
