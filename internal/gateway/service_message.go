package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/gateway/protocol"
	"github.com/orka-agents/orka/internal/store"
)

// ErrInterimDeliveryUnsupported identifies an absent adapter capability without
// conflating it with Task lifecycle or idempotency conflicts.
var ErrInterimDeliveryUnsupported = &HTTPError{
	Code: http.StatusConflict, Message: "gateway adapter does not advertise interimDelivery capability",
}

// TaskMessageReceipt acknowledges durable admission, not provider delivery. A
// replay returns the retained row's current status without requeueing it.
type TaskMessageReceipt struct {
	DeliveryID string                     `json:"deliveryID"`
	Status     store.GatewayDeliveryState `json:"status"`
	Created    bool                       `json:"created"`
}

// PreparedTaskMessage carries only controller-derived admission data. Prepare it
// during live authorization, then enqueue in the authorized task-data writer.
// It is not a worker credential and must not be retained across requests.
type PreparedTaskMessage struct {
	service            *Service
	request            store.GatewayMessageEnqueue
	admissionGateError error
}

// EnqueueTaskMessage admits a message for an exact, already-authorized Task
// identity. Worker APIs must also fence Job revocation using the split methods.
func (s *Service) EnqueueTaskMessage(ctx context.Context, namespace, taskName, taskUID, requestID, content string) (*TaskMessageReceipt, error) {
	prepared, err := s.PrepareTaskMessage(ctx, namespace, taskName, taskUID, requestID, content)
	if err != nil {
		return nil, err
	}
	return s.EnqueuePreparedTaskMessage(ctx, prepared)
}

// PrepareTaskMessage performs live Kubernetes checks outside the SQLite writer.
// No routing or controller policy is accepted from the caller.
func (s *Service) PrepareTaskMessage(ctx context.Context, namespace, taskName, taskUID, requestID, content string) (*PreparedTaskMessage, error) {
	if s == nil || !s.Config.Enabled || s.EventStore == nil || s.DeliveryStore == nil || s.freshReader() == nil {
		return nil, &HTTPError{Code: http.StatusServiceUnavailable, Message: "gateway message processing is unavailable"}
	}
	// Check the raw text before sanitization can shrink it; never silently truncate.
	if len(content) > protocol.MaxInterimTextBytes {
		return nil, &HTTPError{Code: http.StatusRequestEntityTooLarge, Message: "gateway message content exceeds 16 KiB"}
	}
	if !utf8.ValidString(content) {
		return nil, &HTTPError{Code: http.StatusBadRequest, Message: "gateway message content must be valid UTF-8"}
	}
	text := protocol.SanitizeMessage(content, 0)
	if strings.TrimSpace(text) == "" || len(text) > protocol.MaxInterimTextBytes || !utf8.ValidString(text) {
		return nil, &HTTPError{Code: http.StatusBadRequest, Message: "gateway message content is invalid after sanitization"}
	}
	event, err := s.EventStore.GetGatewayEventForTask(ctx, namespace, taskName, taskUID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, &HTTPError{Code: http.StatusForbidden, Message: "task does not own a gateway event"}
	}
	if err != nil {
		return nil, err
	}
	task, err := s.liveMessageTask(ctx, event)
	if err != nil {
		return nil, err
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseRunning || task.Status.ExecutionOutcome != nil || !task.DeletionTimestamp.IsZero() {
		return nil, &HTTPError{Code: http.StatusConflict, Message: "task is not running"}
	}
	object, err := s.liveMessageGatewayIdentity(ctx, event)
	if err != nil {
		return nil, err
	}
	// Readiness/capability can deny new admission without hiding an authorized
	// receipt. Only the atomic store dedupe may distinguish a replay from a miss.
	gateErr := messageGatewayAdmissionError(object)
	now := time.Now().UTC()
	return &PreparedTaskMessage{service: s, admissionGateError: gateErr, request: store.GatewayMessageEnqueue{
		Namespace: event.Namespace, NamespaceUID: event.NamespaceUID, EventID: event.ID,
		TaskName: event.TaskName, TaskUID: event.TaskUID, RequestID: requestID, Text: text,
		MaxMessages: s.Config.InterimMessagesPerTask, MaxAttempts: s.Config.DeliveryMaxAttempts,
		Now: now, ExpiresAt: now.Add(s.Config.EventExpiry), ReplayOnly: gateErr != nil,
	}}, nil
}

// EnqueuePreparedTaskMessage performs only store work and reuses an existing
// authorized writer. Durable event state is the terminal-race cutoff; there is
// deliberately no claim of a transaction spanning Kubernetes and SQLite.
func (s *Service) EnqueuePreparedTaskMessage(ctx context.Context, prepared *PreparedTaskMessage) (*TaskMessageReceipt, error) {
	if prepared == nil || prepared.service != s || s == nil || !s.Config.Enabled {
		return nil, &HTTPError{Code: http.StatusServiceUnavailable, Message: "gateway message preparation is unavailable"}
	}
	request := prepared.request
	// Refresh admission time rather than reusing the earlier live-read time.
	request.Now = time.Now().UTC()
	row, created, err := s.DeliveryStore.EnqueueGatewayMessage(ctx, request)
	if errors.Is(err, store.ErrGatewayMessageReplayOnly) {
		return nil, prepared.admissionGateError
	}
	if err != nil {
		return nil, taskMessageStoreError(err)
	}
	return &TaskMessageReceipt{DeliveryID: row.ID, Status: row.State, Created: created}, nil
}

func taskMessageStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrValidation):
		return &HTTPError{Code: http.StatusBadRequest, Message: "gateway message request is invalid"}
	case errors.Is(err, store.ErrNotFound):
		return &HTTPError{Code: http.StatusNotFound, Message: "gateway event no longer exists"}
	case errors.Is(err, store.ErrDuplicateMismatch):
		return &HTTPError{Code: http.StatusConflict, Message: "requestID was already used for different content"}
	case errors.Is(err, store.ErrConflict):
		return &HTTPError{Code: http.StatusConflict, Message: "gateway event is no longer eligible for messages"}
	case errors.Is(err, store.ErrCapacity):
		return &HTTPError{Code: http.StatusTooManyRequests, Message: "gateway message limit reached for this task"}
	default:
		return err
	}
}

func (s *Service) liveMessageTask(ctx context.Context, event *store.GatewayEvent) (*corev1alpha1.Task, error) {
	task := &corev1alpha1.Task{}
	if err := s.freshReader().Get(ctx, client.ObjectKey{Namespace: event.Namespace, Name: event.TaskName}, task); err != nil {
		return nil, messageIdentityReadError(err)
	}
	if event.TaskUID == "" || string(task.UID) != event.TaskUID || !gatewayTaskCorrelatesWithEvent(task, event) {
		return nil, &HTTPError{Code: http.StatusForbidden, Message: "task does not own this gateway event"}
	}
	return task, nil
}

func (s *Service) liveMessageGatewayIdentity(ctx context.Context, event *store.GatewayEvent) (*gatewayv1alpha1.Gateway, error) {
	namespace := &corev1.Namespace{}
	if err := s.freshReader().Get(ctx, client.ObjectKey{Name: event.Namespace}, namespace); err != nil {
		return nil, messageIdentityReadError(err)
	}
	if event.NamespaceUID == "" || string(namespace.UID) != event.NamespaceUID || !namespace.DeletionTimestamp.IsZero() {
		return nil, &HTTPError{Code: http.StatusConflict, Message: "gateway namespace identity is no longer active"}
	}
	object := &gatewayv1alpha1.Gateway{}
	if err := s.freshReader().Get(ctx, client.ObjectKey{Namespace: event.Namespace, Name: event.GatewayName}, object); err != nil {
		return nil, messageIdentityReadError(err)
	}
	if event.GatewayUID == "" || string(object.UID) != event.GatewayUID || event.GatewayGeneration <= 0 || object.Generation != event.GatewayGeneration || !object.DeletionTimestamp.IsZero() {
		return nil, &HTTPError{Code: http.StatusConflict, Message: "gateway identity is no longer active"}
	}
	return object, nil
}

func messageGatewayAdmissionError(object *gatewayv1alpha1.Gateway) error {
	if !object.Status.Ready || object.Status.ObservedGeneration != object.Generation {
		return &HTTPError{Code: http.StatusServiceUnavailable, Message: "gateway is not ready for interim delivery"}
	}
	caps := object.Status.ObservedCapabilities
	if caps != nil && caps.ContractVersion != protocol.Version {
		return &HTTPError{Code: http.StatusConflict, Message: "gateway does not currently support interim delivery"}
	}
	if caps == nil || !caps.Capabilities.InterimDelivery {
		return ErrInterimDeliveryUnsupported
	}
	return nil
}

func messageIdentityReadError(err error) error {
	if apierrors.IsNotFound(err) {
		return &HTTPError{Code: http.StatusConflict, Message: "gateway message identity no longer exists"}
	}
	return &HTTPError{Code: http.StatusServiceUnavailable, Message: "gateway message identity is unavailable"}
}

// Revalidate immediately before network dispatch. Terminal Tasks remain valid:
// accepted messages must drain before final/error. Missing/replaced identities,
// deleting Tasks or withdrawn capability abandon the message, never downgrade it.
func (s *Service) validateMessageDelivery(ctx context.Context, delivery *store.GatewayDelivery) error {
	event, err := s.EventStore.GetGatewayEvent(ctx, delivery.Namespace, delivery.EventID)
	if err != nil {
		return err
	}
	if event.NamespaceUID != delivery.NamespaceUID || event.TaskName != delivery.TaskName || event.GatewayUID != delivery.GatewayUID || event.GatewayGeneration != delivery.GatewayGeneration {
		return &HTTPError{Code: http.StatusConflict, Message: "gateway message identity changed"}
	}
	task, err := s.liveMessageTask(ctx, event)
	if err != nil {
		return err
	}
	if !task.DeletionTimestamp.IsZero() {
		return &HTTPError{Code: http.StatusConflict, Message: "gateway message task is deleting"}
	}
	object, err := s.liveMessageGatewayIdentity(ctx, event)
	if err != nil {
		return err
	}
	return messageGatewayAdmissionError(object)
}
