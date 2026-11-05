/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gofiber/fiber/v3"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/capabilitygap"
	"github.com/orka-agents/orka/internal/events"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/usage"
)

const maxSubmitExecutionEventRequestBytes = 256 << 10 // 256 KiB

// SubmitExecutionEvent handles POST /internal/v1/events/{namespace}/{streamType}/{streamID}.
// Workers call this to append sanitized execution timeline events.
//
//nolint:gocyclo // Event authentication, replay checks, and durable acceptance form one boundary.
func (h *InternalHandlers) SubmitExecutionEvent(c fiber.Ctx) error {
	namespace := strings.TrimSpace(c.Params("namespace"))
	streamType := strings.TrimSpace(c.Params("streamType"))
	streamID := strings.TrimSpace(c.Params("streamID"))

	if namespace == "" || streamType == "" || streamID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace, streamType, and streamID are required")
	}
	if h.executionEventStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "execution event storage not enabled")
	}
	if !events.IsValidExecutionEventStreamType(streamType) {
		return fiber.NewError(fiber.StatusBadRequest, "unsupported execution event stream type")
	}
	if strings.Contains(streamID, "/") {
		return fiber.NewError(fiber.StatusBadRequest, "streamID must not contain slash")
	}
	authorizer := h.internalCallerAuthorizer()
	writerTask, err := authorizer.verifyExecutionEventStreamWriter(c, namespace, streamType, streamID)
	if err != nil {
		return err
	}

	body, err := readExecutionEventRequestBody(c)
	if err != nil {
		return err
	}

	var req SubmitExecutionEventRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body: trailing data")
	}

	if strings.TrimSpace(req.Type) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "event type is required")
	}
	event, err := req.ToStoreEvent(namespace, streamType, streamID)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if events.IsTerminalTaskEventType(event.Type) || events.IsTerminalApprovalEventType(event.Type) ||
		event.Type == events.ExecutionEventTypeApprovalExecutionUpdated {
		return fiber.NewError(fiber.StatusForbidden, "this event must use a controller-owned path")
	}
	// The harness identity is controller-owned; workers may submit only their
	// own call records, bound below to the authenticated Task UID.
	var eventContent map[string]json.RawMessage
	if json.Unmarshal(event.Content, &eventContent) == nil {
		if _, ok := eventContent["harnessV2"]; ok {
			return fiber.NewError(fiber.StatusForbidden, "harness events must use the controller journal")
		}
		if _, ok := eventContent["usage"]; ok {
			if err := authorizer.verifyUsageWriter(c.Context(), GetUserInfo(c), writerTask); err != nil {
				return err
			}
			event.Internal = map[string]any{"usageTaskUID": string(writerTask.UID)}
		}
	}
	if event.StreamType == events.ExecutionEventStreamTypeTask {
		event.TaskName = streamID
		if writerTask != nil {
			expectedSessionName, err := executionEventSessionName(c.Context(), h.sessionStore, namespace, writerTask)
			if err != nil {
				return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get session: %v", err))
			}
			if event.SessionName != "" && event.SessionName != expectedSessionName {
				return fiber.NewError(fiber.StatusBadRequest, "sessionName does not match task session")
			}
			event.SessionName = expectedSessionName
		}
	}
	var appended *store.ExecutionEvent
	if err := withInternalTaskDataTransaction(c, h.executionEventStore, "", func(context.Context) error {
		current, err := authorizer.verifyExecutionEventStreamWriter(c, namespace, streamType, streamID)
		if err != nil {
			return err
		}
		if current.UID != writerTask.UID {
			return fiber.NewError(fiber.StatusForbidden, "task identity changed")
		}
		if event.Internal["usageTaskUID"] != nil {
			return authorizer.verifyUsageWriter(c.Context(), GetUserInfo(c), current)
		}
		return nil
	}, func(ctx context.Context) error {
		var err error
		appended, err = h.executionEventStore.AppendExecutionEvent(ctx, event)
		if err != nil {
			if errors.Is(err, store.ErrValidation) {
				return fiber.NewError(fiber.StatusBadRequest, err.Error())
			}
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to append execution event: %v", err))
		}
		return nil
	}); err != nil {
		return err
	}
	// Only a managed AI worker reports its own tool rejections; a container
	// Task's command could submit the same event shape.
	if reason, ok := toolCallCapabilityGapReason(event, eventContent); ok && capabilitygap.Default() != nil &&
		authorizer.verifyUsageWriter(c.Context(), GetUserInfo(c), writerTask) == nil {
		signal := capabilitygap.Signal{
			Category:  capabilitygap.CategoryMissingTool,
			Reason:    reason,
			Runtime:   capabilitygap.RuntimeNative,
			Tool:      event.ToolName,
			Namespace: event.Namespace,
			Task:      event.StreamID,
		}
		if writerTask != nil && writerTask.Name == event.StreamID {
			signal.TaskUID = string(writerTask.UID)
		}
		// Without a verified namespace UID or Gateway owner the signal still
		// counts in Prometheus but stays out of the summary.
		owner, ownerErr := capabilityGapGatewayOwner(c.Context(), h.gatewayEventStore, writerTask)
		signal.Gateway = owner
		if uid, err := usage.NamespaceUIDForObject(c.Context(), uncachedReaderOr(h.apiReader, h.k8sClient), writerTask); err == nil && ownerErr == nil {
			signal.NamespaceUID = uid
		}
		capabilitygap.Record(signal)
	}

	return c.Status(fiber.StatusCreated).JSON(SubmitExecutionEventResponse{
		ID:        appended.ID,
		Seq:       appended.Seq,
		CreatedAt: appended.CreatedAt,
	})
}

// toolCallCapabilityGapReason classifies a built-in AI worker tool call that
// was rejected before it ran. It reads only the closed errorType value; the
// summary, content text and arguments are never read.
func toolCallCapabilityGapReason(event *store.ExecutionEvent, content map[string]json.RawMessage) (capabilitygap.Reason, bool) {
	if event == nil || event.Type != events.ExecutionEventTypeToolCallFailed ||
		event.StreamType != events.ExecutionEventStreamTypeTask {
		return "", false
	}
	var errorType string
	if json.Unmarshal(content[events.ToolCallErrorTypeContentKey], &errorType) != nil {
		return "", false
	}
	switch errorType {
	case events.ToolCallErrorTypeNotEnabled:
		return capabilitygap.ReasonToolNotEnabled, true
	case events.ToolCallErrorTypeNotFound:
		return capabilitygap.ReasonToolNotRegistered, true
	default:
		return "", false
	}
}

// capabilityGapGatewayOwner names the Gateway that owns task, if any,
// including ownership recorded only in its retained GatewayEvent. An error
// means ownership could not be verified.
func capabilityGapGatewayOwner(ctx context.Context, gatewayEvents store.GatewayEventStore, task *corev1alpha1.Task) (capabilitygap.GatewayOwner, error) {
	owner, ok, err := gatewayruntime.ResolveTaskOwner(ctx, gatewayEvents, task)
	if err != nil || !ok {
		return capabilitygap.GatewayOwner{}, err
	}
	return capabilitygap.GatewayOwner{
		Namespace: owner.GatewayNamespace, NamespaceUID: owner.NamespaceUID, Name: owner.GatewayName, UID: owner.GatewayUID,
	}, nil
}

func readExecutionEventRequestBody(c fiber.Ctx) ([]byte, error) {
	body := c.Request().BodyStream()
	if body == nil {
		data := c.Body()
		if len(data) == 0 {
			return nil, fiber.NewError(fiber.StatusBadRequest, "empty request body")
		}
		if len(data) > maxSubmitExecutionEventRequestBytes {
			return nil, fiber.NewError(fiber.StatusRequestEntityTooLarge, "execution event payload exceeds size limit")
		}
		return append([]byte(nil), data...), nil
	}

	lr := io.LimitReader(body, int64(maxSubmitExecutionEventRequestBytes)+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to read request body: %v", err))
	}
	if len(data) == 0 {
		return nil, fiber.NewError(fiber.StatusBadRequest, "empty request body")
	}
	if len(data) > maxSubmitExecutionEventRequestBytes {
		return nil, fiber.NewError(fiber.StatusRequestEntityTooLarge, "execution event payload exceeds size limit")
	}
	return data, nil
}
