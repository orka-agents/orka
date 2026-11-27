// Copyright (c) 2026. MIT License - see LICENSE file for details.

package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

const maxCancelReasonLength = 500

type cancelRequest struct {
	Reason string `json:"reason"`
	UID    string `json:"uid"`
}

type taskCancellation struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Requested bool   `json:"requested"`
	Message   string `json:"message"`
}

type sessionCancellation struct {
	Namespace       string             `json:"namespace"`
	Session         string             `json:"session"`
	Tasks           []taskCancellation `json:"tasks"`
	ChatTurnStopped bool               `json:"chatTurnStopped,omitempty"`
}

type chatTurnStopper interface {
	stopActiveChat(ctx context.Context, namespace, sessionID string) (running bool, release func(), err error)
}

var errTaskNotCancellable = errors.New("task cannot be cancelled")

var errCancelTaskGone = fiber.NewError(fiber.StatusNotFound, "task not found")

func (h *Handlers) CancelTask(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "cancelTask", h.contextTokenAuthorization.TaskUpdateScopes); err != nil {
		return err
	}
	request, err := parseCancelRequest(c)
	if err != nil {
		return err
	}
	task := &corev1alpha1.Task{}
	if err := h.client.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: c.Params("id")}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	result, err := h.requestTaskCancellation(c, task, request.UID, cancelMessage(GetUserInfo(c), request.Reason))
	if err != nil {
		return err
	}
	return c.JSON(result)
}

func (h *Handlers) CancelSession(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "cancelSession", h.contextTokenAuthorization.SessionWriteScopes); err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "cancelSession", h.contextTokenAuthorization.TaskUpdateScopes); err != nil {
		return err
	}
	request, err := parseCancelRequest(c)
	if err != nil {
		return err
	}
	name := c.Params("id")
	ctx := c.Context()
	sessionType, err := transcriptSessionType(ctx, h.sessionStore, namespace, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrGatewayOwnedSession) {
			return fiber.NewError(fiber.StatusNotFound, "session not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get session: %v", err))
	}
	if sessionType == store.SessionTypeGateway {
		return fiber.NewError(fiber.StatusNotFound, "session not found")
	}
	chat := sessionType == store.SessionTypeChat
	epoch, err := h.chatSessionEpoch(ctx, namespace, name, chat)
	if err != nil {
		return err
	}
	if _, err := h.authorizedSessionTaskList(c, namespace, name, chat, epoch); err != nil {
		return err
	}
	result := sessionCancellation{Namespace: namespace, Session: name, Tasks: []taskCancellation{}}
	if chat && h.chatTurns != nil {
		stopped, release, err := h.chatTurns.stopActiveChat(ctx, namespace, name)
		if err != nil {
			return fiber.NewError(fiber.StatusServiceUnavailable, "the chat turn did not stop in time; retry")
		}
		defer release()
		if _, err := transcriptSessionType(ctx, h.sessionStore, namespace, name); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fiber.NewError(fiber.StatusNotFound, "session not found")
			}
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get session: %v", err))
		}
		result.ChatTurnStopped = stopped
	}
	selected, err := h.authorizedSessionTaskList(c, namespace, name, chat, epoch)
	if err != nil {
		return err
	}
	message := cancelMessage(GetUserInfo(c), request.Reason)
	for _, task := range selected {
		cancelled, err := h.recordTaskCancellation(ctx, task, "", message)
		if err != nil {
			var fiberErr *fiber.Error
			if errors.Is(err, errCancelTaskGone) || (errors.As(err, &fiberErr) && fiberErr.Code == fiber.StatusConflict) {
				continue
			}
			return err
		}
		result.Tasks = append(result.Tasks, cancelled)
	}
	if len(result.Tasks) == 0 && !result.ChatTurnStopped {
		return fiber.NewError(fiber.StatusConflict, "session has no running turn")
	}
	return c.JSON(result)
}

func (h *Handlers) chatSessionEpoch(ctx context.Context, namespace, name string, chat bool) (string, error) {
	reader, ok := h.sessionStore.(store.SessionCreationReader)
	if !ok || !chat {
		return "", nil
	}
	createdAt, err := reader.GetSessionCreatedAt(ctx, namespace, name)
	if errors.Is(err, store.ErrNotFound) {
		return "", fiber.NewError(fiber.StatusNotFound, "session not found")
	}
	if err != nil {
		return "", fiber.NewError(fiber.StatusServiceUnavailable, "the chat Session's incarnation is unavailable; retry")
	}
	return labels.SessionEpoch(createdAt), nil
}

func (h *Handlers) authorizedSessionTaskList(c fiber.Ctx, namespace, name string, chat bool, epoch string) ([]*corev1alpha1.Task, error) {
	tasks, err := h.sessionTaskCandidates(c.Context(), namespace, name, chat)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list tasks: %v", err))
	}
	return h.authorizedSessionTasks(c, tasks, namespace, name, chat, epoch)
}

func (h *Handlers) authorizedSessionTasks(
	c fiber.Ctx,
	tasks []corev1alpha1.Task,
	namespace, name string,
	chat bool,
	epoch string,
) ([]*corev1alpha1.Task, error) {
	selected := make([]*corev1alpha1.Task, 0, len(tasks))
	for i := range tasks {
		task := &tasks[i]
		if !cancellableTask(task) || !taskInSession(task, namespace, name, chat, epoch) {
			continue
		}
		if task.Namespace != namespace {
			if err := authorizeKubernetesResourceAction(c.Context(), h.clientset, GetUserInfo(c), task.Namespace,
				apiVerbUpdate, corev1alpha1.GroupVersion.Group, "tasks/cancel", task.Name); err != nil {
				return nil, err
			}
		}
		if err := h.authorizeTaskCancellation(c, task); err != nil {
			return nil, err
		}
		selected = append(selected, task)
	}
	return selected, nil
}

func (h *Handlers) sessionTaskCandidates(ctx context.Context, namespace, name string, chat bool) ([]corev1alpha1.Task, error) {
	reader := h.uncachedReader()
	tasks := &corev1alpha1.TaskList{}
	if err := reader.List(ctx, tasks, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	if !chat || h.watchNamespace != "" || h.enforceNamespaceIsolation {
		return tasks.Items, nil
	}
	created := &corev1alpha1.TaskList{}
	if err := reader.List(ctx, created, client.MatchingLabels{
		labels.LabelChatSession: name, labels.LabelChatSessionNamespace: namespace,
	}); err != nil {
		return nil, err
	}
	candidates := tasks.Items
	for i := range created.Items {
		if created.Items[i].Namespace != namespace {
			candidates = append(candidates, created.Items[i])
		}
	}
	return candidates, nil
}

func taskInSession(task *corev1alpha1.Task, namespace, name string, chat bool, epoch string) bool {
	if pinned := strings.TrimSpace(task.Labels[labels.LabelChatSessionEpoch]); chat && epoch != "" && pinned != "" && pinned != epoch {
		return false
	}
	if task.Namespace == namespace && sessionNameForTask(task) == name {
		return true
	}
	if !chat || strings.TrimSpace(task.Labels[labels.LabelChatSession]) != name {
		return false
	}
	chatNamespace := strings.TrimSpace(task.Labels[labels.LabelChatSessionNamespace])
	if chatNamespace == "" {
		chatNamespace = task.Namespace
	}
	return chatNamespace == namespace
}

func (h *Handlers) requestTaskCancellation(c fiber.Ctx, task *corev1alpha1.Task, expectedUID, message string) (taskCancellation, error) {
	if err := h.authorizeTaskCancellation(c, task); err != nil {
		return taskCancellation{}, err
	}
	return h.recordTaskCancellation(c.Context(), task, expectedUID, message)
}

func (h *Handlers) authorizeTaskCancellation(c fiber.Ctx, task *corev1alpha1.Task) error {
	if err := h.authorizeContextTokenLoadedTask(c, "cancelTask", task); err != nil {
		return err
	}
	return h.taskAccess().authorizeGatewayTaskOperate(c, "cancelTask", task)
}

func (h *Handlers) recordTaskCancellation(ctx context.Context, task *corev1alpha1.Task, expectedUID, message string) (taskCancellation, error) {
	if expectedUID != "" && string(task.UID) != expectedUID {
		return taskCancellation{}, fiber.NewError(fiber.StatusConflict, "the task was replaced since it was confirmed; reload it")
	}
	key := client.ObjectKeyFromObject(task)
	result := taskCancellation{Namespace: task.Namespace, Name: task.Name}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &corev1alpha1.Task{}
		if err := h.uncachedReader().Get(ctx, key, current); err != nil {
			return err
		}
		if current.UID != task.UID {
			return apierrors.NewNotFound(corev1alpha1.GroupVersion.WithResource("tasks").GroupResource(), task.Name)
		}
		result.Phase = string(current.Status.Phase)
		if !current.DeletionTimestamp.IsZero() {
			return fmt.Errorf("%w: the task is being deleted", errTaskNotCancellable)
		}
		if requested, ok := current.Annotations[labels.AnnotationCancelRequested]; ok {
			result.Message = requested
			return nil
		}
		if current.Status.Phase == corev1alpha1.TaskPhaseCancelled {
			result.Message = current.Status.Message
			return nil
		}
		if strings.TrimSpace(current.Spec.Schedule) != "" {
			return fmt.Errorf("%w: the task is a schedule; suspend it to stop future runs", errTaskNotCancellable)
		}
		if !cancellableTask(current) {
			return fmt.Errorf("%w: the task is %s", errTaskNotCancellable, current.Status.Phase)
		}
		base := current.DeepCopy()
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[labels.AnnotationCancelRequested] = message
		if err := h.client.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		result.Requested, result.Message = true, message
		return nil
	})
	switch {
	case err == nil:
		return result, nil
	case errors.Is(err, errTaskNotCancellable):
		return taskCancellation{}, fiber.NewError(fiber.StatusConflict, err.Error())
	case apierrors.IsNotFound(err):
		return taskCancellation{}, errCancelTaskGone
	default:
		return taskCancellation{}, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to cancel task: %v", err))
	}
}

func cancellableTask(task *corev1alpha1.Task) bool {
	if strings.TrimSpace(task.Spec.Schedule) != "" {
		return false
	}
	phase := task.Status.Phase
	return phase == "" || phase == corev1alpha1.TaskPhasePending || phase == corev1alpha1.TaskPhaseRunning
}

func parseCancelRequest(c fiber.Ctx) (cancelRequest, error) {
	request := cancelRequest{}
	if len(c.Body()) > 0 {
		if err := c.Bind().JSON(&request); err != nil {
			return cancelRequest{}, fiber.NewError(fiber.StatusBadRequest, "invalid request body")
		}
	}
	request.Reason = strings.Join(strings.FieldsFunc(request.Reason, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
	if len([]rune(request.Reason)) > maxCancelReasonLength {
		return cancelRequest{}, fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("reason must be at most %d characters", maxCancelReasonLength))
	}
	request.UID = strings.TrimSpace(request.UID)
	return request, nil
}

const cancelMessageLimit = 512

func cancelMessage(userInfo *UserInfo, reason string) string {
	message := "cancelled"
	if actor := approvalDecisionActor(userInfo); actor != "" {
		message += " by " + actor
	}
	if reason != "" {
		message += ": " + reason
	}
	if len(message) <= cancelMessageLimit {
		return message
	}
	limit := cancelMessageLimit
	for limit > 0 && !utf8.RuneStart(message[limit]) {
		limit--
	}
	return message[:limit]
}
