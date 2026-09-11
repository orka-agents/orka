package api

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type taskReadRaceStore struct {
	*sqlite.Store
	beforeRead func(context.Context) error
}

func (s *taskReadRaceStore) GetSessionType(ctx context.Context, namespace, name string) (string, error) {
	sessionType, err := s.Store.GetSessionType(ctx, namespace, name)
	if err != nil {
		return "", err
	}
	return sessionType, s.beforeRead(ctx)
}

func (s *taskReadRaceStore) SearchTranscript(ctx context.Context, filter store.TranscriptSearchFilter) ([]store.TranscriptSearchResult, error) {
	if err := s.beforeRead(ctx); err != nil {
		return nil, err
	}
	return s.Store.SearchTranscript(ctx, filter)
}

func (s *taskReadRaceStore) GetPlan(ctx context.Context, namespace, taskName string) (*store.PlanState, error) {
	if err := s.beforeRead(ctx); err != nil {
		return nil, err
	}
	return s.Store.GetPlan(ctx, namespace, taskName)
}

func TestInternalReadsSerializeAuthorizationWithTaskReuse(t *testing.T) {
	for _, test := range []struct {
		name    string
		path    string
		through bool
	}{
		{name: "plan", path: "/internal/v1/plans/default/my-task"},
		{name: "transcript", path: "/internal/v1/sessions/default/my-session/transcript"},
		{name: "transcript cutoff", path: "/internal/v1/sessions/default/my-session/transcript", through: true},
		{name: "explicit session search", path: "/internal/v1/sessions/default/search?query=history&sessionName=my-session"},
		{name: "coordination search", path: "/internal/v1/sessions/default/search?query=history"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, _, _ := setupTestInternalHandlers()
			dbPath := filepath.Join(t.TempDir(), "reads.db")
			readerDB, err := sqlite.NewDB(dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { _ = readerDB.Close() })
			cleanupDB, err := sqlite.NewDB(dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { _ = cleanupDB.Close() })
			_, err = cleanupDB.ExecContext(t.Context(), `PRAGMA busy_timeout=0`)
			require.NoError(t, err)
			reader, cleanup := sqlite.NewStore(readerDB, dbPath), sqlite.NewStore(cleanupDB, dbPath)
			seed := func(ctx context.Context, data *sqlite.Store, content string) error {
				if err := data.CreateSession(ctx, &store.SessionRecord{
					Namespace: "default", Name: "my-session", SessionType: "task",
				}); err != nil {
					return err
				}
				if err := data.AppendMessages(ctx, "default", "my-session", []store.SessionMessage{{
					ID: "read-cutoff", Role: "assistant", Content: content,
				}}); err != nil {
					return err
				}
				return data.SavePlan(ctx, "default", "my-task", &store.PlanState{Summary: content})
			}
			require.NoError(t, seed(t.Context(), reader, "original Task history"))
			if test.through {
				current := &corev1alpha1.Task{}
				require.NoError(t, h.k8sClient.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "my-task"}, current))
				current.Spec.SessionRef.ThroughMessageID = "read-cutoff"
				require.NoError(t, h.k8sClient.Update(t.Context(), current))
				require.NoError(t, reader.AppendMessages(t.Context(), "default", "my-session", []store.SessionMessage{{
					ID: "after-cutoff", Role: "assistant", Content: "later Task history",
				}}))
			}

			recreateTask := func(ctx context.Context) error {
				current := &corev1alpha1.Task{}
				if err := h.k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "my-task"}, current); err != nil {
					return err
				}
				if err := h.k8sClient.Delete(ctx, current); err != nil {
					return err
				}
				current.UID, current.ResourceVersion = "replacement-task-uid", ""
				if err := h.k8sClient.Create(ctx, current); err != nil {
					return err
				}
				if err := cleanup.DeleteSession(ctx, "default", "my-session"); err != nil {
					return err
				}
				return seed(ctx, cleanup, "replacement Task history")
			}
			var cleanupErr error
			attemptedCleanup := false
			racing := &taskReadRaceStore{Store: reader, beforeRead: func(ctx context.Context) error {
				if attemptedCleanup {
					return nil
				}
				attemptedCleanup = true
				// Finalizer cleanup must finish before the Task name can be reused.
				// Race it after authorization and before reading the protected data.
				cleanupErr = cleanup.DeletePlan(ctx, "default", "my-task")
				if cleanupErr == nil {
					return recreateTask(ctx)
				}
				return nil
			}}
			h.sessionStore, h.planStore, h.gatewayEventStore = racing, racing, reader
			app := newTaskScopedInternalApp(h, internalCallerAuthWorkerUser("my-task-pod", "my-task-pod-uid"))
			request := taskScopedRequest{method: http.MethodGet, path: test.path}
			resp := doTaskScopedInternalRequest(t, app, request)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Contains(t, string(body), "original Task history")
			require.NotContains(t, string(body), "replacement Task history")
			if test.through {
				require.NotContains(t, string(body), "later Task history")
			}
			require.True(t, attemptedCleanup)
			require.ErrorContains(t, cleanupErr, "locked")

			// Once the read finishes, cleanup can complete and stale workers lose access.
			require.NoError(t, cleanup.DeletePlan(t.Context(), "default", "my-task"))
			require.NoError(t, recreateTask(t.Context()))
			stale := doTaskScopedInternalRequest(t, app, request)
			require.Equal(t, http.StatusForbidden, stale.StatusCode)
		})
	}
}
