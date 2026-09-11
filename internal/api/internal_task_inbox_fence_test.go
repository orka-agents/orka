package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func TestInternalInboxRejectsTaskReuseDuringAuthorization(t *testing.T) {
	for _, markRead := range []bool{false, true} {
		t.Run("markRead="+strconv.FormatBool(markRead), func(t *testing.T) {
			h, _, _ := setupTestInternalHandlers()
			dbPath := filepath.Join(t.TempDir(), "inbox.db")
			writerDB, err := sqlite.NewDB(dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { _ = writerDB.Close() })
			cleanupDB, err := sqlite.NewDB(dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { _ = cleanupDB.Close() })
			_, err = cleanupDB.ExecContext(t.Context(), `PRAGMA busy_timeout=0`)
			require.NoError(t, err)
			writer, cleanup := sqlite.NewStore(writerDB, dbPath), sqlite.NewStore(cleanupDB, dbPath)
			h.messageStore = writer
			message := &store.Message{
				Namespace: "default", FromTask: "worker-b", ToTask: "my-task", ParentTask: "coordinator", Content: "original Task message",
			}
			require.NoError(t, writer.SendMessage(t.Context(), message))

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
				message.Content = "replacement Task message"
				return cleanup.SendMessage(ctx, message)
			}
			var cleanupErr error
			attemptedCleanup := false
			baseClient, ok := h.k8sClient.(client.WithWatch)
			require.True(t, ok)
			h.apiReader = interceptor.NewClient(baseClient, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}
					if key.Name == "coordinator" && !attemptedCleanup {
						attemptedCleanup = true
						// Race finalizer cleanup against the last authorization lookup.
						// If cleanup succeeds, reuse the name before GetMessages runs.
						cleanupErr = cleanup.DeleteTaskMessages(ctx, "default", "my-task")
						if cleanupErr == nil {
							return recreateTask(ctx)
						}
					}
					return nil
				},
			})
			app := newTaskScopedInternalApp(h, internalCallerAuthWorkerUser("my-task-pod", "my-task-pod-uid"))
			request := taskScopedRequest{
				method: http.MethodGet,
				path:   "/internal/v1/messages/default/my-task?parentTask=coordinator&markRead=" + strconv.FormatBool(markRead),
			}
			resp := doTaskScopedInternalRequest(t, app, request)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)
			require.True(t, attemptedCleanup)
			require.NoError(t, cleanupErr)

			// Cleanup can finish during network authorization. Its generation
			// change forces a fresh identity check before reading or marking data.
			stale := doTaskScopedInternalRequest(t, app, request)
			require.Equal(t, http.StatusForbidden, stale.StatusCode)
			messages, err := cleanup.GetMessages(t.Context(), "default", "my-task", "coordinator", false)
			require.NoError(t, err)
			require.Len(t, messages, 1, "stale worker must not mark replacement messages read")
			require.Equal(t, "replacement Task message", messages[0].Content)
		})
	}
}
