package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/aitools"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/gateway/protocol"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/tools"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type brokeredGatewayReplySender struct {
	access  brokeredTaskDataAccess
	service *gatewayruntime.Service
}

type brokeredGatewayMeetingDraftSender struct {
	*brokeredGatewayReplySender
}

var _ tools.GatewayMeetingDraftSender = (*brokeredGatewayMeetingDraftSender)(nil)

// NewBrokeredGatewayReplySender binds an ACP prompt guard and exact Task identity
// for content-only replies. Each method independently reauthorizes, prepares live
// data outside the SQLite writer, and consumes the preparation under the existing
// authorized data transaction.
func NewBrokeredGatewayReplySender(reader client.Reader, service *gatewayruntime.Service, key client.ObjectKey, uid string, guard func(context.Context, func(context.Context) error) error) tools.GatewayReplySender {
	return NewBrokeredGatewayReplySenderForDescriptor(reader, service, key, uid, guard, harnessv2.MCPToolDescriptor{})
}

// NewBrokeredGatewayReplySenderForDescriptor additionally binds draft capability
// to the supervisor's frozen descriptor carried by the authenticated broker call,
// never to tool arguments. External runtimes also pin it to a controller snapshot.
// Availability alone is insufficient: old profiles must remain content-only even
// when the current registry implementation understands meetingDraft.
func NewBrokeredGatewayReplySenderForDescriptor(reader client.Reader, service *gatewayruntime.Service, key client.ObjectKey, uid string, guard func(context.Context, func(context.Context) error) error, descriptor harnessv2.MCPToolDescriptor) tools.GatewayReplySender {
	sender := &brokeredGatewayReplySender{access: brokeredTaskDataAccess{authorizer: internalCallerAuthorizer{k8sReader: reader}, taskKey: key, taskUID: uid, guard: guard}, service: service}
	tool := tools.NewReplyInConversationTool()
	if descriptor.Name != tool.Name() || descriptor.Source != harnessv2.MCPToolSourceBrokeredBuiltin || descriptor.Effect != harnessv2.MCPToolEffectConsequential {
		return sender
	}
	frozen, err := harnessv2.CanonicalJSON(descriptor.InputSchema)
	if err != nil {
		return sender
	}
	current, err := harnessv2.CanonicalJSON(tool.Parameters())
	if err != nil || !bytes.Equal(frozen, current) {
		return sender
	}
	return &brokeredGatewayMeetingDraftSender{sender}
}

func (s *brokeredGatewayReplySender) authorize(_ context.Context, task *corev1alpha1.Task) error {
	if !aitools.ACPGatewayReplyScopeAllows(task) {
		return tools.NewGatewayReplyRejection("forbidden", nil)
	}
	return nil
}

func (s *brokeredGatewayReplySender) Budget(ctx context.Context, id string) (tools.GatewayReplyBudget, error) {
	var result tools.GatewayReplyBudget
	if s.service == nil {
		return result, errors.New("gateway reply service unavailable")
	}
	var prepared *gatewayruntime.PreparedTaskMessageBudget
	err := s.access.withData(ctx, s.service.DeliveryStore, func(ctx context.Context, task *corev1alpha1.Task) error {
		if err := s.authorize(ctx, task); err != nil {
			return err
		}
		var err error
		prepared, err = s.service.PrepareTaskMessageBudget(ctx, task.Namespace, task.Name, string(task.UID), id)
		return err
	}, func(ctx context.Context) error {
		budget, err := s.service.ReadPreparedTaskMessageBudget(ctx, prepared)
		if err == nil {
			result = tools.GatewayReplyBudget{Accepted: budget.Accepted, Limit: budget.Limit, RequestExists: budget.RequestExists}
		}
		return err
	})
	return result, brokeredGatewayReplyError(err)
}

func (s *brokeredGatewayReplySender) Enqueue(ctx context.Context, id, content string) (tools.GatewayReplyReceipt, error) {
	return s.enqueue(ctx, id, content, nil)
}

func (s *brokeredGatewayMeetingDraftSender) EnqueueMeetingDraft(ctx context.Context, id, content string, draft protocol.MeetingDraft) (tools.GatewayReplyReceipt, error) {
	return s.enqueue(ctx, id, content, &draft)
}

func (s *brokeredGatewayReplySender) enqueue(ctx context.Context, id, content string, draft *protocol.MeetingDraft) (tools.GatewayReplyReceipt, error) {
	var result tools.GatewayReplyReceipt
	if s.service == nil {
		return result, errors.New("gateway reply service unavailable")
	}
	var prepared *gatewayruntime.PreparedTaskMessage
	err := s.access.withData(ctx, s.service.DeliveryStore, func(ctx context.Context, task *corev1alpha1.Task) error {
		if err := s.authorize(ctx, task); err != nil {
			return err
		}
		var err error
		if draft != nil {
			prepared, err = s.service.PrepareTaskMeetingDraft(ctx, task.Namespace, task.Name, string(task.UID), id, content, *draft)
		} else {
			prepared, err = s.service.PrepareTaskMessage(ctx, task.Namespace, task.Name, string(task.UID), id, content)
		}
		return err
	}, func(ctx context.Context) error {
		receipt, err := s.service.EnqueuePreparedTaskMessage(ctx, prepared)
		if err == nil {
			result = tools.GatewayReplyReceipt{DeliveryID: receipt.DeliveryID, Status: string(receipt.Status), Created: receipt.Created}
		}
		return err
	})
	return result, brokeredGatewayReplyError(err)
}

//nolint:goconst // Mirror only the narrow gateway wire error contract, not unrelated API codes.
func brokeredGatewayReplyError(err error) error {
	if err == nil {
		return nil
	}
	code := ""
	if errors.Is(err, gatewayruntime.ErrInterimDeliveryUnsupported) {
		code = "interim_delivery_unsupported"
	} else if errors.Is(err, gatewayruntime.ErrMeetingDraftsUnsupported) {
		code = "meeting_drafts_unsupported"
	} else if domain, ok := errors.AsType[*gatewayruntime.HTTPError](err); ok {
		switch domain.Code {
		case http.StatusBadRequest:
			code = "invalid_request"
		case http.StatusRequestEntityTooLarge:
			code = "too_large"
		case http.StatusForbidden:
			code = "forbidden"
		case http.StatusNotFound:
			code = "not_found"
		case http.StatusConflict:
			code = "conflict"
		case http.StatusTooManyRequests:
			code = "limit_reached"
		case http.StatusServiceUnavailable:
			code = "unavailable"
		}
	}
	if rejection := tools.NewGatewayReplyRejection(code, err); rejection != nil {
		return rejection
	}
	return err // Guard/store/transport failures retain consequential uncertainty.
}
