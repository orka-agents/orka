package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
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
	if err := verifyCallerNamespace(c, namespace); err != nil {
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
	if err := h.verifyExecutionEventStreamWriter(c, namespace, streamType, streamID); err != nil {
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
	if event.StreamType == events.ExecutionEventStreamTypeTask && strings.TrimSpace(event.TaskName) == "" {
		event.TaskName = streamID
	}
	if event.StreamType == events.ExecutionEventStreamTypeTask {
		if h.k8sClient == nil {
			return fiber.NewError(fiber.StatusNotImplemented, "task ownership validation not enabled")
		}
		task := &corev1alpha1.Task{}
		if err := h.k8sClient.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: streamID}, task); err != nil {
			if apierrors.IsNotFound(err) {
				return fiber.NewError(fiber.StatusNotFound, "task not found")
			}
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
		}
		if err := verifyCallerOwnsTaskWorker(c.Context(), h.k8sClient, GetUserInfo(c), task); err != nil {
			return err
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

func (h *InternalHandlers) verifyExecutionEventStreamWriter(c fiber.Ctx, namespace, streamType, streamID string) error {
	if h.k8sClient == nil || streamType != events.ExecutionEventStreamTypeTask {
		return nil
	}
	task := &corev1alpha1.Task{}
	if err := h.k8sClient.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: streamID}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusForbidden, "caller is not the current worker for this task")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	if err := verifyCallerOwnsTaskWorker(c.Context(), h.k8sClient, GetUserInfo(c), task); err != nil {
		return err
	}
	if !task.DeletionTimestamp.IsZero() {
		return fiber.NewError(fiber.StatusGone, "task is deleting")
	}
	return nil
}
