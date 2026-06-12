package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	forkcontext "github.com/sozercan/orka/internal/fork"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
)

type ForkTaskRequest struct {
	AfterSeq    *int64                        `json:"afterSeq,omitempty"`
	NewTaskName string                        `json:"newTaskName,omitempty"`
	AgentRef    *corev1alpha1.AgentReference  `json:"agentRef,omitempty"`
	Prompt      string                        `json:"prompt,omitempty"`
	Workspace   *corev1alpha1.WorkspaceConfig `json:"workspace,omitempty"`
}

type ForkTaskResponse struct {
	Namespace      string              `json:"namespace"`
	SourceTaskName string              `json:"sourceTaskName"`
	NewTaskName    string              `json:"newTaskName"`
	AfterSeq       int64               `json:"afterSeq"`
	ForkContext    forkcontext.Context `json:"forkContext"`
}

// ForkTask handles POST /api/v1/tasks/{id}/fork.
func (h *Handlers) ForkTask(c fiber.Ctx) error {
	sourceName := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenTaskRead(c, "forkTask", namespace, sourceName); err != nil {
		return err
	}
	if h.executionEventStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "execution event storage not enabled")
	}

	var req ForkTaskRequest
	if len(c.Body()) > 0 {
		if err := c.Bind().JSON(&req); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
		}
	}

	source := &corev1alpha1.Task{}
	if err := h.client.Get(c.Context(), types.NamespacedName{Name: sourceName, Namespace: namespace}, source); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	if err := h.authorizeContextTokenLoadedTask(c, "forkTask", source); err != nil {
		return err
	}

	latest, err := h.executionEventStore.GetLatestExecutionEventSeq(
		c.Context(),
		namespace,
		events.ExecutionEventStreamTypeTask,
		sourceName,
	)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get latest execution event sequence: %v", err))
	}
	afterSeq := latest
	if req.AfterSeq != nil {
		afterSeq = *req.AfterSeq
	}
	if afterSeq < 0 {
		return fiber.NewError(fiber.StatusBadRequest, "afterSeq must be non-negative")
	}
	if afterSeq > latest {
		return fiber.NewError(fiber.StatusBadRequest, "afterSeq must be 0, latest, or an existing event sequence")
	}
	eventsBefore, err := listTaskEventsThrough(
		c.Context(),
		h.executionEventStore,
		namespace,
		sourceName,
		afterSeq,
		forkcontext.DefaultMaxEvents,
	)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list execution events: %v", err))
	}
	if !forkcontext.ValidateAfterSeq(afterSeq, eventsBefore) {
		return fiber.NewError(fiber.StatusBadRequest, "afterSeq must be 0, latest, or an existing event sequence")
	}

	forkCtx := forkcontext.BuildContext(namespace, sourceName, afterSeq, eventsBefore, forkcontext.DefaultMaxEvents)
	newName := strings.TrimSpace(req.NewTaskName)
	if newName == "" {
		newName = generatedForkTaskName(sourceName)
	}

	spec := *source.Spec.DeepCopy()
	spec.RequestedBy = nil
	spec.Transaction = nil
	if req.AgentRef != nil {
		spec.AgentRef = req.AgentRef
	}
	if strings.TrimSpace(req.Prompt) != "" {
		spec.Prompt = strings.TrimSpace(req.Prompt)
		if spec.AI != nil {
			spec.AI.Prompt = strings.TrimSpace(req.Prompt)
		}
	}
	if req.Workspace != nil {
		spec.Workspace = req.Workspace
	}

	forked := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newName,
			Namespace: namespace,
			Labels: map[string]string{
				labels.LabelParentTask: labels.SelectorValue(sourceName),
			},
			Annotations: map[string]string{
				labels.AnnotationParentTaskName:       sourceName,
				labels.AnnotationForkSourceTask:       sourceName,
				labels.AnnotationForkSourceSeq:        strconv.FormatInt(afterSeq, 10),
				labels.AnnotationForkContextTruncated: strconv.FormatBool(forkCtx.Truncated),
			},
		},
		Spec: spec,
	}
	stampTaskRequesterFromUserInfo(forked, GetUserInfo(c))
	if err := h.authorizeContextTokenTaskCreate(c, createTaskRequestFromTask(forked), namespace); err != nil {
		return err
	}

	sourceSessionName := sessionNameFromTask(source)
	forkedSessionName := sessionNameFromTask(forked)
	requestContent, _ := json.Marshal(map[string]any{"sourceTaskName": sourceName, "newTaskName": newName, "afterSeq": afterSeq})
	if _, err := h.executionEventStore.AppendExecutionEvent(c.Context(), &store.ExecutionEvent{
		Namespace:   namespace,
		StreamType:  events.ExecutionEventStreamTypeTask,
		StreamID:    sourceName,
		TaskName:    sourceName,
		SessionName: sourceSessionName,
		Type:        events.ExecutionEventTypeTaskForkRequested,
		Severity:    events.ExecutionEventSeverityInfo,
		Summary:     "task fork requested",
		Content:     requestContent,
	}); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to append fork request event: %v", err))
	}

	if err := h.client.Create(c.Context(), forked); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fiber.NewError(fiber.StatusConflict, "forked task already exists")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create forked task: %v", err))
	}

	createdContent, _ := json.Marshal(map[string]any{"sourceTaskName": sourceName, "newTaskName": newName, "afterSeq": afterSeq})
	for _, item := range []struct {
		streamID    string
		sessionName string
	}{
		{streamID: sourceName, sessionName: sourceSessionName},
		{streamID: newName, sessionName: forkedSessionName},
	} {
		if _, err := h.executionEventStore.AppendExecutionEvent(c.Context(), &store.ExecutionEvent{
			Namespace:   namespace,
			StreamType:  events.ExecutionEventStreamTypeTask,
			StreamID:    item.streamID,
			TaskName:    item.streamID,
			SessionName: item.sessionName,
			Type:        events.ExecutionEventTypeTaskForkCreated,
			Severity:    events.ExecutionEventSeverityInfo,
			Summary:     "task fork created",
			Content:     createdContent,
		}); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to append fork created event: %v", err))
		}
	}

	return c.Status(fiber.StatusCreated).JSON(ForkTaskResponse{Namespace: namespace, SourceTaskName: sourceName, NewTaskName: newName, AfterSeq: afterSeq, ForkContext: forkCtx})
}

func listTaskEventsThrough(
	ctx context.Context,
	eventStore store.ExecutionEventStore,
	namespace,
	taskName string,
	throughSeq int64,
	maxEvents ...int,
) ([]store.ExecutionEvent, error) {
	if throughSeq == 0 {
		return nil, nil
	}
	tailLimit := 0
	if len(maxEvents) > 0 {
		limit := maxEvents[0]
		if limit <= 0 {
			limit = forkcontext.DefaultMaxEvents
		}
		tailLimit = limit + 1
	}
	out := []store.ExecutionEvent{}
	if tailLimit > 0 {
		out = make([]store.ExecutionEvent, 0, tailLimit)
	}
	after := int64(0)
	if tailLimit > 0 && throughSeq > int64(tailLimit) {
		after = throughSeq - int64(tailLimit)
	}
	for {
		batch, err := eventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
			Namespace:  namespace,
			StreamType: events.ExecutionEventStreamTypeTask,
			StreamID:   taskName,
			AfterSeq:   after,
			Limit:      store.MaxExecutionEventLimit,
		})
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for _, event := range batch {
			if event.Seq > throughSeq {
				return out, nil
			}
			out = append(out, event)
			if tailLimit > 0 && len(out) > tailLimit {
				copy(out, out[1:])
				out = out[:tailLimit]
			}
			after = event.Seq
		}
		if after >= throughSeq || len(batch) < store.MaxExecutionEventLimit {
			break
		}
	}
	return out, nil
}

func generatedForkTaskName(sourceName string) string {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Sprintf("%s-fork-%d", forkcontext.SanitizeTaskNamePrefix(sourceName), time.Now().Unix())
	}
	return fmt.Sprintf("%s-fork-%s", forkcontext.SanitizeTaskNamePrefix(sourceName), hex.EncodeToString(suffix[:]))
}
