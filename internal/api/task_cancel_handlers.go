// Copyright (c) 2026. MIT License - see LICENSE file for details.

package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

// maxCancelReasonLength bounds the reason a person gives for cancelling.
const maxCancelReasonLength = 500

type cancelRequest struct {
	Reason string `json:"reason"`
	// UID, when set, is the Task the caller confirmed. A Task recreated under
	// the same name since then is refused rather than cancelled.
	UID string `json:"uid"`
}

// taskCancellation is the response for a cancel request.
type taskCancellation struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	// Requested is true when this call recorded the request, false when the
	// Task already had one or was already Cancelled.
	Requested bool   `json:"requested"`
	Message   string `json:"message"`
}

type sessionCancellation struct {
	Namespace string             `json:"namespace"`
	Session   string             `json:"session"`
	Tasks     []taskCancellation `json:"tasks"`
	// ChatTurnStopped is true when an in-flight chat request was stopped.
	ChatTurnStopped bool `json:"chatTurnStopped,omitempty"`
}

// chatTurnStopper stops a chat Session's in-flight request without deleting
// the Session, and reports whether one was running. The Session accepts no
// next message until release is called.
type chatTurnStopper interface {
	stopActiveChat(ctx context.Context, namespace, sessionID string) (running bool, release func(), err error)
}

var errTaskNotCancellable = errors.New("task cannot be cancelled")

// CancelTask asks the controller to stop a Pending or Running Task. Unlike
// delete, it keeps everything the Task produced. The controller ends the
// Task as Cancelled; an Agent Task's run is stopped by the ACP dispatcher.
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
	if request.UID != "" && string(task.UID) != request.UID {
		return fiber.NewError(fiber.StatusConflict, "the task was replaced since it was confirmed; reload it")
	}
	result, err := h.requestTaskCancellation(c, task, cancelMessage(GetUserInfo(c), request.Reason))
	if err != nil {
		return err
	}
	return c.JSON(result)
}

// CancelSession stops a Session's current turn: an in-flight chat request,
// then its Pending and Running Tasks. The Session stays usable for the next
// message.
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
	// Session metadata only: the transcript is not needed to stop a turn.
	sessionType, err := transcriptSessionType(ctx, h.sessionStore, namespace, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrGatewayOwnedSession) {
			return fiber.NewError(fiber.StatusNotFound, "session not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get session: %v", err))
	}
	result := sessionCancellation{Namespace: namespace, Session: name, Tasks: []taskCancellation{}}
	if sessionType == store.SessionTypeChat && h.chatTurns != nil {
		// Stop the chat request first so it starts no Task after the list
		// below. It ends as a disconnected client's request does, and the
		// Session takes no next message until its Tasks are cancelled.
		stopped, release, err := h.chatTurns.stopActiveChat(ctx, namespace, name)
		if err != nil {
			return fiber.NewError(fiber.StatusServiceUnavailable, "the chat turn did not stop in time; retry")
		}
		defer release()
		result.ChatTurnStopped = stopped
	}
	// Read live: the cache can miss a Task the stopped request just created.
	tasks := &corev1alpha1.TaskList{}
	if err := h.uncachedReader().List(ctx, tasks, client.InNamespace(namespace)); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list tasks: %v", err))
	}
	message := cancelMessage(GetUserInfo(c), request.Reason)
	for i := range tasks.Items {
		task := &tasks.Items[i]
		// A chat-created Task may name its continuity Session in
		// spec.sessionRef and the conversation in the chat label; either
		// ties it to this Session, the label only for a chat Session.
		inChat := sessionType == store.SessionTypeChat && strings.TrimSpace(task.Labels[labels.LabelChatSession]) == name
		if !cancellableTaskPhase(task.Status.Phase) || (sessionNameForTask(task) != name && !inChat) {
			continue
		}
		cancelled, err := h.requestTaskCancellation(c, task, message)
		if err != nil {
			var fiberErr *fiber.Error
			if errors.As(err, &fiberErr) && fiberErr.Code == fiber.StatusConflict {
				continue // finished meanwhile
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

// requestTaskCancellation records a cancel request on task. A second request
// changes nothing; a finished Task other than a Cancelled one is a conflict.
func (h *Handlers) requestTaskCancellation(c fiber.Ctx, task *corev1alpha1.Task, message string) (taskCancellation, error) {
	if err := h.authorizeContextTokenLoadedTask(c, "cancelTask", task); err != nil {
		return taskCancellation{}, err
	}
	if err := h.taskAccess().authorizeGatewayTaskOperate(c, "cancelTask", task); err != nil {
		return taskCancellation{}, err
	}
	ctx := c.Context()
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
		if !cancellableTaskPhase(current.Status.Phase) {
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
		return taskCancellation{}, fiber.NewError(fiber.StatusNotFound, "task not found")
	default:
		return taskCancellation{}, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to cancel task: %v", err))
	}
}

// cancellableTaskPhase reports whether a Task is still Pending or Running. A
// Scheduled Task is a cron template; suspend it instead.
func cancellableTaskPhase(phase corev1alpha1.TaskPhase) bool {
	return phase == "" || phase == corev1alpha1.TaskPhasePending || phase == corev1alpha1.TaskPhaseRunning
}

// parseCancelRequest reads the optional body, collapsing the reason to one
// line.
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

// cancelMessage names the authenticated caller, never a value from the
// request body, and the reason they gave.
func cancelMessage(userInfo *UserInfo, reason string) string {
	message := "cancelled"
	if actor := approvalDecisionActor(userInfo); actor != "" {
		message += " by " + actor
	}
	if reason != "" {
		message += ": " + reason
	}
	return message
}
