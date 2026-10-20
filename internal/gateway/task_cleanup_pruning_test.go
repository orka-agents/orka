package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/gateway/protocol"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func compactTaskReceiptForPruning(t *testing.T, db *sqlite.Store, namespace, taskName, taskUID string) store.GatewayEvent {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	event := store.GatewayEvent{
		ID: "event-" + taskUID, Namespace: namespace, NamespaceUID: "namespace-uid",
		GatewayUID: "gateway-uid", GatewayGeneration: 1, GatewayName: "chat",
		BindingName: "room", BindingUID: "binding-uid", BindingGeneration: 1,
		AgentName: "assistant", AgentUID: "agent-uid", ExternalEventID: "external-" + taskUID,
		ProtocolVersion: protocol.Version, EventType: protocol.EventTypeText, AccountID: "acct", ContextID: taskUID,
		SenderID: "user-1", Text: "old prompt", ReplyTarget: "room", SessionName: "session-" + taskUID,
		TaskName: taskName, ReceivedAt: old, NextAttemptAt: old,
		ExpiresAt: old.Add(time.Hour), CreatedAt: old, UpdatedAt: old,
	}
	_, created, err := db.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: event, AppendUserMessage: true})
	require.NoError(t, err)
	require.True(t, created)
	claimed, err := db.ClaimNextGatewayEvent(ctx, namespace, "pruning-test", old, time.Minute)
	require.NoError(t, err)
	require.Equal(t, event.ID, claimed.ID)
	require.NoError(t, db.MarkGatewayEventTaskCreated(ctx, namespace, event.ID, taskName, taskUID, "pruning-test", old))
	event.TaskUID = taskUID
	attachDeliveredExpiryDelivery(t, db, ctx, event, old.Add(time.Minute))
	_, err = db.MaintainGatewayRecords(ctx, namespace, now, now.Add(-time.Hour))
	require.NoError(t, err)
	_, err = db.GetGatewayTaskCleanupReceipt(ctx, namespace, taskName, taskUID)
	require.NoError(t, err)
	return event
}

func TestGatewayTaskReceiptPruningRequiresAuthoritativeAbsence(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "orka.ai", Resource: "tasks"}, "old-task", errors.New("unavailable"))
	for _, tt := range []struct {
		name         string
		apiUID       types.UID
		exists       bool
		deleting     bool
		cached       bool
		noReader     bool
		readError    error
		wantRetained bool
	}{
		{name: "live Task missing from cache", exists: true, apiUID: "task-uid", wantRetained: true},
		{name: "deleting Task", exists: true, apiUID: "task-uid", deleting: true, wantRetained: true},
		{name: "absent Task"},
		{name: "absent Task still cached", cached: true},
		{name: "replacement Task", exists: true, apiUID: "replacement-uid", cached: true},
		{name: "missing reader cannot trust empty cache", noReader: true, wantRetained: true},
		{name: "forbidden", readError: forbidden, wantRetained: true},
		{name: "transport error", readError: errors.New("API unavailable"), wantRetained: true},
		{name: "incomplete API identity", exists: true, wantRetained: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			service, db, _ := newGatewayServiceFixture(t)
			event := compactTaskReceiptForPruning(t, db, "default", "old-task", "task-uid")
			original, err := db.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID)
			require.NoError(t, err)
			completion, err := db.GetSessionCleanupCompletion(ctx, event.Namespace, event.SessionName)
			require.NoError(t, err)
			if tt.cached {
				require.NoError(t, service.Client.Create(ctx, &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
					Namespace: event.Namespace, Name: event.TaskName, UID: types.UID(event.TaskUID),
				}}))
			}
			builder := fake.NewClientBuilder().WithScheme(service.Client.Scheme())
			if tt.exists {
				task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: event.Namespace, Name: event.TaskName, UID: tt.apiUID}}
				if tt.deleting {
					task.Finalizers = []string{"orka.ai/cleanup"}
					deletingAt := metav1.NewTime(time.Now().Add(-24 * time.Hour))
					task.DeletionTimestamp = &deletingAt
				}
				builder.WithObjects(task)
			}
			if !tt.noReader {
				service.APIReader = builder.WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if tt.readError != nil {
							return tt.readError
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}).Build()
			}
			err = service.pruneGatewayTaskCleanupReceipts(ctx)
			if tt.readError != nil {
				require.ErrorIs(t, err, tt.readError)
			} else {
				require.NoError(t, err)
			}
			retained, err := db.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID)
			if tt.wantRetained {
				require.NoError(t, err)
				require.Equal(t, original, retained)
			} else {
				require.ErrorIs(t, err, store.ErrNotFound)
			}
			retainedCompletion, err := db.GetSessionCleanupCompletion(ctx, event.Namespace, event.SessionName)
			require.NoError(t, err)
			require.Equal(t, completion, retainedCompletion)
			duplicate, err := db.GetGatewayEventDuplicate(ctx, &event, time.Now())
			require.NoError(t, err, "Task receipt pruning must preserve event replay protection")
			require.Equal(t, event.ID, duplicate.ID)
			if tt.exists {
				var task corev1alpha1.Task
				require.NoError(t, service.APIReader.Get(ctx, client.ObjectKey{Namespace: event.Namespace, Name: event.TaskName}, &task))
				require.Equal(t, tt.apiUID, task.UID)
				require.Equal(t, tt.deleting, !task.DeletionTimestamp.IsZero())
			}
		})
	}
}

type failingReceiptMaintenanceStore struct {
	*sqlite.Store
	listError   error
	deleteError error
}

func (s *failingReceiptMaintenanceStore) ListGatewayTaskCleanupReceipts(ctx context.Context, filter store.GatewayTaskCleanupReceiptFilter) ([]store.GatewayTaskCleanupReceipt, error) {
	if s.listError != nil {
		return nil, s.listError
	}
	return s.Store.ListGatewayTaskCleanupReceipts(ctx, filter)
}

func (s *failingReceiptMaintenanceStore) DeleteGatewayTaskCleanupReceipt(ctx context.Context, namespace, taskName, taskUID string) error {
	if taskUID == "a-uid" && s.deleteError != nil {
		return s.deleteError
	}
	return s.Store.DeleteGatewayTaskCleanupReceipt(ctx, namespace, taskName, taskUID)
}

func TestGatewayTaskReceiptPruningAdvancesAndRetriesBoundedPages(t *testing.T) {
	for _, failure := range []string{"live Task", "read error", "delete error"} {
		t.Run(failure, func(t *testing.T) {
			ctx := t.Context()
			service, db, _ := newGatewayServiceFixture(t)
			service.Config.Namespace = "default"
			service.Config.BatchSize = 1
			first := compactTaskReceiptForPruning(t, db, "default", "first", "a-uid")
			second := compactTaskReceiptForPruning(t, db, "default", "second", "b-uid")
			other := compactTaskReceiptForPruning(t, db, "other", "other", "other-uid")
			unavailable := errors.New("temporarily unavailable")
			events := &failingReceiptMaintenanceStore{Store: db, listError: unavailable}
			service.EventStore = events
			reads := 0
			blocked := true
			service.APIReader = fake.NewClientBuilder().WithScheme(service.Client.Scheme()).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					reads++
					if key.Name == first.TaskName && blocked {
						switch failure {
						case "live Task":
							obj.SetUID(types.UID(first.TaskUID))
							return nil
						case "read error":
							return unavailable
						}
					}
					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()
			require.ErrorIs(t, service.pruneGatewayTaskCleanupReceipts(ctx), unavailable)
			require.Zero(t, reads)
			events.listError = nil
			if failure == "delete error" {
				events.deleteError = unavailable
			}
			err := service.pruneGatewayTaskCleanupReceipts(ctx)
			if failure == "live Task" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, unavailable)
			}
			require.Equal(t, 1, reads, "one pass must inspect at most one configured receipt")
			_, err = db.GetGatewayTaskCleanupReceipt(ctx, first.Namespace, first.TaskName, first.TaskUID)
			require.NoError(t, err)
			_, err = db.GetGatewayTaskCleanupReceipt(ctx, second.Namespace, second.TaskName, second.TaskUID)
			require.NoError(t, err)
			require.NoError(t, service.pruneGatewayTaskCleanupReceipts(ctx))
			require.Equal(t, 2, reads)
			_, err = db.GetGatewayTaskCleanupReceipt(ctx, second.Namespace, second.TaskName, second.TaskUID)
			require.ErrorIs(t, err, store.ErrNotFound, "a blocked earlier receipt must not starve later receipts")
			events.deleteError, blocked = nil, false
			require.NoError(t, service.pruneGatewayTaskCleanupReceipts(ctx)) // End of scan resets the cursor.
			require.NoError(t, service.pruneGatewayTaskCleanupReceipts(ctx))
			_, err = db.GetGatewayTaskCleanupReceipt(ctx, first.Namespace, first.TaskName, first.TaskUID)
			require.ErrorIs(t, err, store.ErrNotFound, "the retained receipt must be retried after the scan wraps")
			_, err = db.GetGatewayTaskCleanupReceipt(ctx, other.Namespace, other.TaskName, other.TaskUID)
			require.NoError(t, err, "maintenance must not cross its namespace")
		})
	}
}
