package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ResolveReplyEligibility resolves durable origin before tool configuration is
// frozen. Readiness and interim capability are admission gates, not origin:
// temporary withdrawal must not freeze a tool-less execution. It does not require
// Running. ErrNotReady means dispatch created the Task but has not yet linked its
// UID: callers must defer, not freeze a tool-less Job/session. Metadata can locate
// a pending event but can never grant access.
func (s *Service) ResolveReplyEligibility(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	event, err := s.resolveReplyOrigin(ctx, task)
	return event != nil, err
}

func (s *Service) resolveReplyOrigin(ctx context.Context, task *corev1alpha1.Task) (*store.GatewayEvent, error) {
	if task == nil || task.UID == "" || (task.Spec.Type != corev1alpha1.TaskTypeAI && task.Spec.Type != corev1alpha1.TaskTypeAgent) || labels.ParentTaskName(task.Labels, task.Annotations) != "" {
		return nil, nil
	}
	switch task.Status.Phase {
	case "", corev1alpha1.TaskPhasePending, corev1alpha1.TaskPhaseRunning:
	default:
		return nil, nil
	}
	if _, owned := TaskOwner(task); !owned {
		return nil, nil
	}
	if s == nil || s.EventStore == nil || s.DeliveryStore == nil || s.freshReader() == nil {
		return nil, store.ErrNotReady
	}
	if !s.Config.Enabled {
		return nil, nil
	}
	event, err := s.EventStore.GetGatewayEventForTask(ctx, task.Namespace, task.Name, string(task.UID))
	if errors.Is(err, store.ErrNotFound) {
		event, err = s.EventStore.GetGatewayEvent(ctx, task.Namespace, task.Annotations[TaskGatewayEventAnnotation])
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrValidation) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if event.TaskUID == "" {
			if event.State == store.GatewayEventDispatching && event.ExpiresAt.After(time.Now()) && gatewayTaskCorrelatesWithEvent(task, event) {
				return nil, store.ErrNotReady
			}
			return nil, nil
		}
		// Dispatch may have linked the Task between reads. Apply the same durable
		// ownership and live identity checks as a successful task-index lookup.
	}
	if err != nil {
		return nil, err
	}
	if !activeReplyOriginEvent(task, event) {
		return nil, nil
	}
	if _, err := s.liveMessageGatewayIdentity(ctx, event); err != nil {
		return nil, err
	}
	return event, nil
}

func activeReplyOriginEvent(task *corev1alpha1.Task, event *store.GatewayEvent) bool {
	return event.TaskUID == string(task.UID) && gatewayTaskCorrelatesWithEvent(task, event) &&
		event.State == store.GatewayEventTaskCreated && event.DeliveryID == "" && event.ExpiresAt.After(time.Now()) &&
		task.Status.ExecutionOutcome == nil && task.DeletionTimestamp.IsZero()
}

// TaskReplyOrigin authenticates origin only. It contains no admission grant,
// routing, content or quota; every Budget and Enqueue still authorizes afresh.
type TaskReplyOrigin struct {
	TaskUID string `json:"taskUID"`
}

// PreparedTaskReplyOrigin must be consumed in the caller's authorized task-data
// transaction, with its revocation fence. Never retain it across requests.
type PreparedTaskReplyOrigin struct {
	service *Service
	task    *corev1alpha1.Task
	event   *store.GatewayEvent
}

// PrepareTaskReplyOrigin performs live identity checks outside the SQLite writer.
func (s *Service) PrepareTaskReplyOrigin(ctx context.Context, namespace, taskName, taskUID string) (*PreparedTaskReplyOrigin, error) {
	if s == nil || !s.Config.Enabled || s.EventStore == nil || s.DeliveryStore == nil || s.freshReader() == nil {
		return nil, &HTTPError{Code: http.StatusServiceUnavailable, Message: "gateway reply origin is unavailable"}
	}
	task := &corev1alpha1.Task{}
	if err := s.freshReader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: taskName}, task); err != nil {
		return nil, messageIdentityReadError(err)
	}
	if taskUID == "" || string(task.UID) != taskUID {
		return nil, &HTTPError{Code: http.StatusForbidden, Message: "gateway reply task identity changed"}
	}
	event, err := s.resolveReplyOrigin(ctx, task)
	if errors.Is(err, store.ErrNotReady) {
		return nil, &HTTPError{Code: http.StatusServiceUnavailable, Message: "gateway reply origin is not yet linked"}
	}
	if err != nil {
		return nil, err
	}
	if event == nil {
		return nil, &HTTPError{Code: http.StatusForbidden, Message: "task does not own an active gateway event"}
	}
	return &PreparedTaskReplyOrigin{service: s, task: task, event: event}, nil
}

// ReadPreparedTaskReplyOrigin rechecks durable ownership and lifecycle inside the
// authorized writer. No Kubernetes reads or budget/admission gates run here.
func (s *Service) ReadPreparedTaskReplyOrigin(ctx context.Context, prepared *PreparedTaskReplyOrigin) (*TaskReplyOrigin, error) {
	if s == nil || prepared == nil || prepared.service != s || !s.Config.Enabled || s.EventStore == nil {
		return nil, &HTTPError{Code: http.StatusServiceUnavailable, Message: "gateway reply origin is unavailable"}
	}
	task, before := prepared.task, prepared.event
	event, err := s.EventStore.GetGatewayEventForTask(ctx, task.Namespace, task.Name, string(task.UID))
	if err != nil {
		return nil, taskMessageStoreError(err)
	}
	if event.ID != before.ID || event.NamespaceUID != before.NamespaceUID || event.GatewayUID != before.GatewayUID || event.GatewayGeneration != before.GatewayGeneration || !activeReplyOriginEvent(task, event) {
		return nil, &HTTPError{Code: http.StatusForbidden, Message: "gateway reply origin is no longer active"}
	}
	return &TaskReplyOrigin{TaskUID: string(task.UID)}, nil
}
