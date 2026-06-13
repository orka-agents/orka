/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

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
	validAfterSeq, err := taskEventSeqExists(c.Context(), h.executionEventStore, namespace, sourceName, afterSeq, latest)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to validate execution event sequence: %v", err))
	}
	if !validAfterSeq {
		return fiber.NewError(fiber.StatusBadRequest, "afterSeq must be 0, latest, or an existing event sequence")
	}
	eventsBefore, err := listTaskEventsThrough(c.Context(), h.executionEventStore, namespace, sourceName, afterSeq)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list execution events: %v", err))
	}

	forkCtx := forkcontext.BuildContext(namespace, sourceName, afterSeq, eventsBefore, forkcontext.DefaultMaxEvents)
	newName := strings.TrimSpace(req.NewTaskName)
	if newName == "" {
		newName = generatedForkTaskName(sourceName)
	}

	spec := *source.Spec.DeepCopy()
	applyForkRequestOverrides(&spec, req)
	if err := applyForkContextToSpec(&spec, forkCtx); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to encode fork context: %v", err))
	}

	forked := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newName,
			Namespace: namespace,
			Labels: map[string]string{
				labels.LabelParentTask: labels.SelectorValue(sourceName),
			},
			Annotations: map[string]string{
				labels.AnnotationParentTaskName:                sourceName,
				labels.AnnotationForkSourceTask:                sourceName,
				labels.AnnotationForkSourceSeq:                 strconv.FormatInt(afterSeq, 10),
				labels.AnnotationForkContextTruncated:          strconv.FormatBool(forkCtx.Truncated),
				labels.AnnotationDisableCoordinationToolInject: strconv.FormatBool(true),
			},
		},
		Spec: spec,
	}
	stampTaskRequesterFromUserInfo(forked, GetUserInfo(c))
	if err := h.authorizeContextTokenTaskCreate(c, createTaskRequestFromTask(forked), namespace); err != nil {
		return err
	}

	sourceSessionName, forkedSessionName, err := h.resolveForkSessionNames(c.Context(), namespace, source, forked)
	if err != nil {
		return err
	}
	requestContent, err := marshalForkEventContent(sourceName, newName, afterSeq, "request")
	if err != nil {
		return err
	}
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

	createdContent, err := marshalForkEventContent(sourceName, newName, afterSeq, "created")
	if err != nil {
		return err
	}
	for _, event := range []struct {
		streamID    string
		sessionName string
	}{
		{streamID: sourceName, sessionName: sourceSessionName},
		{streamID: newName, sessionName: forkedSessionName},
	} {
		if _, err := h.executionEventStore.AppendExecutionEvent(c.Context(), &store.ExecutionEvent{
			Namespace:   namespace,
			StreamType:  events.ExecutionEventStreamTypeTask,
			StreamID:    event.streamID,
			TaskName:    event.streamID,
			SessionName: event.sessionName,
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

func applyForkRequestOverrides(spec *corev1alpha1.TaskSpec, req ForkTaskRequest) {
	if spec == nil {
		return
	}
	spec.RequestedBy = nil
	spec.Transaction = nil
	if req.AgentRef != nil {
		spec.AgentRef = req.AgentRef
	}
	if prompt := strings.TrimSpace(req.Prompt); prompt != "" {
		spec.Prompt = prompt
		if spec.AI != nil {
			spec.AI.Prompt = prompt
		}
	}
	if req.Workspace != nil {
		spec.Workspace = req.Workspace
	}
	clearForkSchedule(spec)
}

func clearForkSchedule(spec *corev1alpha1.TaskSpec) {
	if spec == nil {
		return
	}
	spec.Schedule = ""
	spec.TimeZone = nil
	spec.ConcurrencyPolicy = ""
	spec.StartingDeadlineSeconds = nil
	spec.SuccessfulRunsHistoryLimit = nil
	spec.FailedRunsHistoryLimit = nil
	spec.Suspend = nil
}

func applyForkContextToSpec(spec *corev1alpha1.TaskSpec, forkCtx forkcontext.Context) error {
	if spec == nil || len(forkCtx.Events) == 0 {
		return nil
	}
	encoded, err := json.Marshal(forkCtx)
	if err != nil {
		return err
	}
	contextBlock := "Fork context through execution event checkpoint:\n" + string(encoded)
	if spec.AI != nil {
		spec.AI.Prompt = joinForkContextPrompt(contextBlock, spec.AI.Prompt)
	}
	if spec.Type == corev1alpha1.TaskTypeAgent || strings.TrimSpace(spec.Prompt) != "" {
		spec.Prompt = joinForkContextPrompt(contextBlock, spec.Prompt)
	}
	return nil
}

func joinForkContextPrompt(contextBlock, prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return contextBlock
	}
	return contextBlock + "\n\nUser continuation prompt:\n" + prompt
}

func (h *Handlers) resolveForkSessionNames(
	ctx context.Context,
	namespace string,
	source *corev1alpha1.Task,
	forked *corev1alpha1.Task,
) (string, string, error) {
	sourceSessionName, err := h.existingSessionNameForTask(ctx, namespace, source)
	if err != nil {
		return "", "", fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get source session: %v", err))
	}
	if sessionNameForTask(source) != "" && sourceSessionName == "" {
		forked.Spec.SessionRef = nil
	}
	forkedSessionName, err := h.existingSessionNameForTask(ctx, namespace, forked)
	if err != nil {
		return "", "", fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get forked session: %v", err))
	}
	return sourceSessionName, forkedSessionName, nil
}

func marshalForkEventContent(sourceName, newName string, afterSeq int64, eventKind string) (json.RawMessage, error) {
	content, err := json.Marshal(map[string]any{"sourceTaskName": sourceName, "newTaskName": newName, "afterSeq": afterSeq})
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to encode fork %s event: %v", eventKind, err))
	}
	return content, nil
}

func taskEventSeqExists(
	ctx context.Context,
	eventStore store.ExecutionEventStore,
	namespace,
	taskName string,
	seq,
	latestSeq int64,
) (bool, error) {
	if seq == 0 || seq == latestSeq {
		return true, nil
	}
	if seq < 0 || seq > latestSeq {
		return false, nil
	}
	listed, err := eventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace:  namespace,
		StreamType: events.ExecutionEventStreamTypeTask,
		StreamID:   taskName,
		AfterSeq:   seq - 1,
		Limit:      1,
	})
	if err != nil {
		return false, err
	}
	return len(listed) == 1 && listed[0].Seq == seq, nil
}

func listTaskEventsThrough(
	ctx context.Context,
	eventStore store.ExecutionEventStore,
	namespace,
	taskName string,
	throughSeq int64,
) ([]store.ExecutionEvent, error) {
	if throughSeq == 0 {
		return nil, nil
	}
	readLimit := forkcontext.DefaultMaxEvents + 1
	after := max(throughSeq-int64(readLimit), 0)
	out := make([]store.ExecutionEvent, 0, readLimit)
	for {
		batch, err := eventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
			Namespace:  namespace,
			StreamType: events.ExecutionEventStreamTypeTask,
			StreamID:   taskName,
			AfterSeq:   after,
			Limit:      min(store.MaxExecutionEventLimit, readLimit-len(out)),
		})
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for _, event := range batch {
			if event.Seq > throughSeq || len(out) >= readLimit {
				return out, nil
			}
			out = append(out, event)
			after = event.Seq
		}
		if after >= throughSeq || len(out) >= readLimit || len(batch) < store.MaxExecutionEventLimit {
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
