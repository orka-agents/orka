package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"

	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/gateway/protocol"
)

// Allow fully JSON-escaped content and request identity, but not unbounded
// whitespace or unknown fields. The decoded content still has its own 16 KiB cap.
const maxGatewayMessageRequestBytes = 6*(protocol.MaxInterimTextBytes+protocol.MaxIdentityBytes) + 1024

// The transaction helper preserves this exact *fiber.Error. Its identity—not
// arbitrary error text—carries the controller-owned capability failure code.
var errGatewayInterimDeliveryUnsupported = fiber.NewError(fiber.StatusConflict, gatewayruntime.ErrInterimDeliveryUnsupported.Error())

type gatewayMessageRequest struct {
	Content   string `json:"content"`
	RequestID string `json:"requestID"`
}

// SubmitGatewayMessage handles POST /internal/v1/tasks/:namespace/:taskName/gateway-messages.
// Only the exact active native Task Job's Pod may call it. Runtime/broker callers
// must use capability-bound authorization, never a ServiceAccount exception here.
func (h *InternalHandlers) SubmitGatewayMessage(c fiber.Ctx) error {
	receipt, err := h.submitGatewayMessage(c)
	if err != nil {
		return sendGatewayMessageError(c, err)
	}
	status := fiber.StatusOK
	if receipt.Created {
		status = fiber.StatusAccepted
	}
	return c.Status(status).JSON(receipt)
}

func (h *InternalHandlers) submitGatewayMessage(c fiber.Ctx) (*gatewayruntime.TaskMessageReceipt, error) {
	namespace, taskName := c.Params("namespace"), c.Params("taskName")
	authorizer := h.internalCallerAuthorizer()
	authorizedTask, err := authorizer.verifyTaskCaller(c, namespace, taskName)
	if err != nil {
		return nil, err
	}
	if h.gatewayService == nil {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "gateway message processing is unavailable")
	}
	req, err := readGatewayMessageRequest(c)
	if err != nil {
		return nil, err
	}
	var prepared *gatewayruntime.PreparedTaskMessage
	var receipt *gatewayruntime.TaskMessageReceipt
	err = withInternalTaskDataTransaction(c, h.gatewayService.DeliveryStore, taskName, func(ctx context.Context) error {
		if err := authorizer.revalidateTaskCaller(c, authorizedTask); err != nil {
			return err
		}
		var err error
		prepared, err = h.gatewayService.PrepareTaskMessage(ctx, namespace, taskName, string(authorizedTask.UID), req.RequestID, req.Content)
		return gatewayMessageTransactionError(err)
	}, func(ctx context.Context) error {
		var err error
		receipt, err = h.gatewayService.EnqueuePreparedTaskMessage(ctx, prepared)
		// Preserve domain errors before the transaction helper masks unknown failures.
		return gatewayMessageTransactionError(err)
	})
	return receipt, err
}

func readGatewayMessageRequest(c fiber.Ctx) (*gatewayMessageRequest, error) {
	var data []byte
	if body := c.Request().BodyStream(); body != nil {
		var err error
		data, err = io.ReadAll(io.LimitReader(body, maxGatewayMessageRequestBytes+1))
		if err != nil {
			return nil, fiber.NewError(fiber.StatusBadRequest, "invalid gateway message body")
		}
	} else {
		data = c.Body()
	}
	if len(data) > maxGatewayMessageRequestBytes {
		return nil, fiber.NewError(fiber.StatusRequestEntityTooLarge, "gateway message body is too large")
	}
	if !utf8.Valid(data) {
		return nil, fiber.NewError(fiber.StatusBadRequest, "gateway message body must be valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var req gatewayMessageRequest
	if err := decoder.Decode(&req); err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "invalid gateway message JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fiber.NewError(fiber.StatusBadRequest, "expected one gateway message JSON object")
	}
	return &req, nil
}

func gatewayMessageTransactionError(err error) error {
	if errors.Is(err, gatewayruntime.ErrInterimDeliveryUnsupported) {
		return errGatewayInterimDeliveryUnsupported
	}
	if domain, ok := errors.AsType[*gatewayruntime.HTTPError](err); ok {
		return fiber.NewError(domain.Code, domain.Message)
	}
	return err
}

//nolint:goconst // Keep this worker error contract independent of unrelated E2E/tool error constants.
func sendGatewayMessageError(c fiber.Ctx, err error) error {
	err = gatewayMessageTransactionError(err)
	status := fiber.StatusInternalServerError
	if apiErr, ok := errors.AsType[*fiber.Error](err); ok {
		status = apiErr.Code
	}
	// Do not return authorization/library/store diagnostics, content, routing or
	// provider metadata. The status and stable code suffice for worker decisions.
	code, message := "internal_error", "gateway message admission failed"
	switch status {
	case fiber.StatusBadRequest:
		code, message = "invalid_request", "gateway message request is invalid"
	case fiber.StatusUnauthorized:
		code, message = "unauthorized", "authentication required"
	case fiber.StatusForbidden:
		code, message = "forbidden", "caller is not authorized for gateway messages on this task"
	case fiber.StatusNotFound:
		code, message = "not_found", "gateway event no longer exists"
	case fiber.StatusConflict:
		code, message = "conflict", "gateway message is not eligible or requestID conflicts"
	case fiber.StatusRequestEntityTooLarge:
		code, message = "too_large", "gateway message exceeds the request or content limit"
	case fiber.StatusTooManyRequests:
		code, message = "limit_reached", "gateway message limit reached for this task"
	case fiber.StatusServiceUnavailable:
		code, message = "unavailable", "gateway message admission is temporarily unavailable"
	}
	if errors.Is(err, errGatewayInterimDeliveryUnsupported) {
		code, message = "interim_delivery_unsupported", errGatewayInterimDeliveryUnsupported.Message
	}
	return c.Status(status).JSON(fiber.Map{"error": fiber.Map{"code": code, "message": message}})
}
