package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type taskCompactionReceiptEvents struct {
	store.GatewayEventStore
	receipt *store.GatewayTaskCleanupReceipt
	err     error
}

func (s taskCompactionReceiptEvents) GetGatewayTaskCleanupReceipt(context.Context, string, string, string) (*store.GatewayTaskCleanupReceipt, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.receipt == nil {
		return nil, store.ErrNotFound
	}
	return s.receipt, nil
}

func TestGatewayTaskRetentionUsesExactCompactionReceipt(t *testing.T) {
	storageFailure := errors.New("receipt storage unavailable")
	tests := []struct {
		name          string
		mutateTask    func(*corev1alpha1.Task)
		mutateReceipt func(*store.GatewayTaskCleanupReceipt)
		missing       bool
		readerError   error
		removeOwners  bool
		wantRetained  bool
		wantError     bool
	}{
		{name: "failed before admission"},
		{name: "queued attempt without a SessionTurn", mutateTask: func(task *corev1alpha1.Task) {
			task.Status.Phase = corev1alpha1.TaskPhaseCancelled
			task.Status.Execution.Attempt = 1
			task.Status.Execution.PromptID = "queued-prompt"
		}},
		{name: "owners already deleted", removeOwners: true},
		{name: "no surviving authority", missing: true, wantRetained: true},
		{name: "receipt read failure", readerError: storageFailure, wantRetained: true, wantError: true},
		{name: "replacement Task UID", mutateTask: func(task *corev1alpha1.Task) { task.UID = "replacement-task" }, wantRetained: true, wantError: true},
		{name: "wrong namespace", mutateReceipt: func(r *store.GatewayTaskCleanupReceipt) { r.Namespace = "other" }, wantRetained: true, wantError: true},
		{name: "recreated namespace", mutateReceipt: func(r *store.GatewayTaskCleanupReceipt) { r.NamespaceUID = "replacement-namespace" }, wantRetained: true, wantError: true},
		{name: "wrong Gateway name", mutateReceipt: func(r *store.GatewayTaskCleanupReceipt) { r.GatewayName = "other" }, wantRetained: true, wantError: true},
		{name: "replaced Gateway", mutateReceipt: func(r *store.GatewayTaskCleanupReceipt) { r.GatewayUID = "replacement-gateway" }, wantRetained: true, wantError: true},
		{name: "wrong binding", mutateReceipt: func(r *store.GatewayTaskCleanupReceipt) { r.BindingName = "other" }, wantRetained: true, wantError: true},
		{name: "missing binding UID", mutateReceipt: func(r *store.GatewayTaskCleanupReceipt) { r.BindingUID = "" }, wantRetained: true, wantError: true},
		{name: "wrong event", mutateReceipt: func(r *store.GatewayTaskCleanupReceipt) { r.EventID = "other-event" }, wantRetained: true, wantError: true},
		{name: "wrong Task name", mutateReceipt: func(r *store.GatewayTaskCleanupReceipt) { r.TaskName = "other-task" }, wantRetained: true, wantError: true},
		{name: "wrong physical Session", mutateReceipt: func(r *store.GatewayTaskCleanupReceipt) { r.SessionName = "replacement-session" }, wantRetained: true, wantError: true},
		{name: "wrong transcript fence", mutateTask: func(task *corev1alpha1.Task) { task.Spec.SessionRef.ThroughMessageID = "gateway:other:user" }, wantRetained: true, wantError: true},
		{name: "missing Session reference", mutateTask: func(task *corev1alpha1.Task) { task.Spec.SessionRef = nil }, wantRetained: true, wantError: true},
		{name: "receipt predates Task", mutateReceipt: func(r *store.GatewayTaskCleanupReceipt) { r.CompactedAt = r.CompactedAt.Add(-4 * time.Hour) }, wantRetained: true, wantError: true},
		{name: "missing compaction time", mutateReceipt: func(r *store.GatewayTaskCleanupReceipt) { r.CompactedAt = time.Time{} }, wantRetained: true, wantError: true},
		{name: "metadata alone cannot establish ownership", mutateTask: func(task *corev1alpha1.Task) { task.Spec.RequestedBy = nil }, wantRetained: true},
		{name: "running Task remains protected", mutateTask: func(task *corev1alpha1.Task) { task.Status.Phase = corev1alpha1.TaskPhaseRunning }, wantRetained: true},
		{name: "recent Task remains protected", mutateTask: func(task *corev1alpha1.Task) { task.CreationTimestamp = metav1.Now() }, wantRetained: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			service, _, _ := newGatewayServiceFixture(t)
			now := time.Now().UTC()
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "old-task", UID: "old-task-uid", CreationTimestamp: metav1.NewTime(now.Add(-3 * time.Hour)),
					Labels:      map[string]string{TaskGatewayEventLabel: "old-event"},
					Annotations: map[string]string{TaskGatewayEventAnnotation: "old-event", TaskGatewayBindingAnnotation: "room"}},
				Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent,
					RequestedBy: &corev1alpha1.RequestedBy{Issuer: "gateway.orka.ai/default/namespace-uid/chat/gateway-uid"},
					SessionRef:  &corev1alpha1.SessionReference{Name: "old-session", ThroughMessageID: "gateway:old-event:user", PromptIncluded: true},
				},
				Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFailed,
					Execution: &corev1alpha1.TaskExecutionStatus{Reason: "InvalidRuntimeProfile", State: corev1alpha1.TaskExecutionStateFailed}},
			}
			receipt := &store.GatewayTaskCleanupReceipt{Namespace: task.Namespace, NamespaceUID: "namespace-uid", GatewayName: "chat", GatewayUID: "gateway-uid",
				BindingName: "room", BindingUID: "binding-uid", EventID: "old-event", TaskName: task.Name, TaskUID: string(task.UID),
				SessionName: "old-session", CompactedAt: now.Add(-2 * time.Hour)}
			if tt.mutateTask != nil {
				tt.mutateTask(task)
			}
			if tt.mutateReceipt != nil {
				tt.mutateReceipt(receipt)
			}
			if tt.missing {
				receipt = nil
			}
			service.EventStore = taskCompactionReceiptEvents{GatewayEventStore: service.EventStore, receipt: receipt, err: tt.readerError}
			if tt.removeOwners {
				for _, obj := range []client.Object{
					&gatewayv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "chat"}},
					&gatewayv1alpha1.GatewayBinding{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "room"}},
				} {
					if err := service.Client.Delete(ctx, obj); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := service.Client.Create(ctx, task); err != nil {
				t.Fatal(err)
			}
			err := service.cleanupRetainedGatewayTasks(ctx, now.Add(-time.Hour))
			if (err != nil) != tt.wantError {
				t.Fatalf("cleanup error = %v, want error %t", err, tt.wantError)
			}
			var current corev1alpha1.Task
			err = service.Client.Get(ctx, client.ObjectKeyFromObject(task), &current)
			if tt.wantRetained {
				if err != nil || !current.DeletionTimestamp.IsZero() {
					t.Fatalf("protected Task was deleted: %+v, %v", current, err)
				}
			} else if !apierrors.IsNotFound(err) {
				t.Fatalf("ordinary Task deletion did not complete: %v", err)
			}
		})
	}
}

func TestGatewayCompactionReceiptDoesNotBypassTaskFinalizer(t *testing.T) {
	ctx := context.Background()
	service, _, _ := newGatewayServiceFixture(t)
	now := time.Now().UTC()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "finalizing-task", UID: "finalizing-task-uid", CreationTimestamp: metav1.NewTime(now.Add(-3 * time.Hour)),
			Finalizers: []string{"orka.ai/cleanup"}, Labels: map[string]string{TaskGatewayEventLabel: "old-event"},
			Annotations: map[string]string{TaskGatewayEventAnnotation: "old-event", TaskGatewayBindingAnnotation: "room"}},
		Spec: corev1alpha1.TaskSpec{RequestedBy: &corev1alpha1.RequestedBy{Issuer: "gateway.orka.ai/default/namespace-uid/chat/gateway-uid"},
			SessionRef: &corev1alpha1.SessionReference{Name: "old-session", ThroughMessageID: "gateway:old-event:user"}},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFailed},
	}
	receipt := &store.GatewayTaskCleanupReceipt{Namespace: task.Namespace, NamespaceUID: "namespace-uid", GatewayName: "chat", GatewayUID: "gateway-uid", BindingName: "room", BindingUID: "binding-uid",
		EventID: "old-event", TaskName: task.Name, TaskUID: string(task.UID), SessionName: "old-session", CompactedAt: now.Add(-2 * time.Hour)}
	service.EventStore = taskCompactionReceiptEvents{GatewayEventStore: service.EventStore, receipt: receipt}
	if err := service.Client.Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := service.cleanupRetainedGatewayTasks(ctx, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	var current corev1alpha1.Task
	if err := service.Client.Get(ctx, client.ObjectKeyFromObject(task), &current); err != nil {
		t.Fatal(err)
	}
	if current.DeletionTimestamp.IsZero() || len(current.Finalizers) != 1 || current.Finalizers[0] != "orka.ai/cleanup" {
		t.Fatalf("compaction receipt bypassed ordinary cleanup finalization: %+v", current.ObjectMeta)
	}
}
