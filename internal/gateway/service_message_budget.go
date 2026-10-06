package gateway

import (
	"context"
	"net/http"

	"github.com/orka-agents/orka/internal/store"
)

// TaskMessageBudget exposes policy and receipt existence, never text or routing.
type TaskMessageBudget struct {
	store.GatewayMessageBudget
	Limit int `json:"limit"`
}

// PreparedTaskMessageBudget is short-lived live authorization state. Read it
// under the caller's authorized task-data transaction, just like message enqueue.
type PreparedTaskMessageBudget struct {
	service            *Service
	query              store.GatewayMessageBudgetQuery
	admissionGateError error
}

func (s *Service) PrepareTaskMessageBudget(ctx context.Context, namespace, taskName, taskUID, requestID string) (*PreparedTaskMessageBudget, error) {
	event, object, err := s.prepareMessageIdentity(ctx, namespace, taskName, taskUID)
	if err != nil {
		return nil, err
	}
	return &PreparedTaskMessageBudget{service: s, admissionGateError: messageGatewayAdmissionError(object), query: store.GatewayMessageBudgetQuery{Namespace: event.Namespace, NamespaceUID: event.NamespaceUID, EventID: event.ID, TaskName: event.TaskName, TaskUID: event.TaskUID, RequestID: requestID}}, nil
}

func (s *Service) ReadPreparedTaskMessageBudget(ctx context.Context, prepared *PreparedTaskMessageBudget) (*TaskMessageBudget, error) {
	if s == nil || prepared == nil || prepared.service != s || !s.Config.Enabled || s.DeliveryStore == nil || s.Config.InterimMessagesPerTask <= 0 {
		return nil, &HTTPError{Code: http.StatusServiceUnavailable, Message: "gateway message budget is unavailable"}
	}
	budget, err := s.DeliveryStore.GetGatewayMessageBudget(ctx, prepared.query)
	if err != nil {
		return nil, taskMessageStoreError(err)
	}
	if !budget.RequestExists && prepared.admissionGateError != nil {
		return nil, prepared.admissionGateError
	}
	return &TaskMessageBudget{GatewayMessageBudget: *budget, Limit: s.Config.InterimMessagesPerTask}, nil
}
