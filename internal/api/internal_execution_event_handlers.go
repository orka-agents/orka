/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

const maxSubmitExecutionEventRequestBytes = 256 << 10 // 256 KiB

// SubmitExecutionEvent handles POST /internal/v1/events/{namespace}/{streamType}/{streamID}.
// Workers call this to append sanitized execution timeline events.
func (h *InternalHandlers) SubmitExecutionEvent(c fiber.Ctx) error {
	namespace := strings.TrimSpace(c.Params("namespace"))
	streamType := strings.TrimSpace(c.Params("streamType"))
	streamID := strings.TrimSpace(c.Params("streamID"))

	if namespace == "" || streamType == "" || streamID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace, streamType, and streamID are required")
	}
	if err := h.internalCallerAuthorizer().verifyNamespace(c, namespace); err != nil {
		return err
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
	writerTask, err := h.internalCallerAuthorizer().verifyExecutionEventStreamWriter(c, namespace, streamType, streamID)
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
	if events.IsTerminalTaskEventType(event.Type) || events.IsTerminalApprovalEventType(event.Type) {
		return fiber.NewError(fiber.StatusForbidden, "terminal task and approval events must use controller-owned paths")
	}
	if event.StreamType == events.ExecutionEventStreamTypeTask {
		event.TaskName = streamID
		if writerTask != nil {
			expectedSessionName := sessionNameForTask(writerTask)
			if expectedSessionName != "" && h.sessionStore != nil {
				if _, err := h.sessionStore.GetSession(c.Context(), namespace, expectedSessionName); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						expectedSessionName = ""
					} else {
						return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get session: %v", err))
					}
				}
			}
			if event.SessionName != "" && event.SessionName != expectedSessionName {
				return fiber.NewError(fiber.StatusBadRequest, "sessionName does not match task session")
			}
			event.SessionName = expectedSessionName
		}
	}
	appended, err := h.executionEventStore.AppendExecutionEvent(c.Context(), event)
	if err != nil {
		if errors.Is(err, store.ErrValidation) {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to append execution event: %v", err))
	}

	return c.Status(fiber.StatusCreated).JSON(SubmitExecutionEventResponse{
		ID:        appended.ID,
		Seq:       appended.Seq,
		CreatedAt: appended.CreatedAt,
	})
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
