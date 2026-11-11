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

// errCancelTaskGone is the 404 for a Task deleted or replaced while it was
// being cancelled. It is distinct from an authorization refusal answered as
// 404, which a Session cancel must not skip.
var errCancelTaskGone = fiber.NewError(fiber.StatusNotFound, "task not found")

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
	result, err := h.requestTaskCancellation(c, task, request.UID, cancelMessage(GetUserInfo(c), request.Reason))
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
	// Gateway-owned Sessions are outside the general Session API: answer as
	// for a missing one, before any conflict check or Task is touched.
	if sessionType == store.SessionTypeGateway {
		return fiber.NewError(fiber.StatusNotFound, "session not found")
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
		// A first turn that was already ending, after an error or a client
		// disconnect, may have discarded its empty Session before this
		// cancel could keep it. Its release finishes before the turn reports
		// done, so the Session is read again only now, and a discarded one
		// is reported as missing rather than as a successful cancel.
		if _, err := transcriptSessionType(ctx, h.sessionStore, namespace, name); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fiber.NewError(fiber.StatusNotFound, "session not found")
			}
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get session: %v", err))
		}
		result.ChatTurnStopped = stopped
	}
	epoch := ""
	if reader, ok := h.sessionStore.(store.SessionCreationReader); ok && sessionType == store.SessionTypeChat {
		// Tasks of an earlier chat under this name are told apart by the
		// current one's creation time, so a cancel that cannot read it stops
		// here rather than cancel them too. Stores without creation times
		// pin no incarnation and keep the name-only match.
		createdAt, err := reader.GetSessionCreatedAt(ctx, namespace, name)
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "session not found")
		}
		if err != nil {
			return fiber.NewError(fiber.StatusServiceUnavailable, "the chat Session's incarnation is unavailable; retry")
		}
		epoch = labels.SessionEpoch(createdAt)
	}
	tasks, err := h.sessionTaskCandidates(ctx, namespace, name, sessionType == store.SessionTypeChat)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list tasks: %v", err))
	}
	message := cancelMessage(GetUserInfo(c), request.Reason)
	selected, err := h.authorizedSessionTasks(c, tasks, namespace, name, sessionType == store.SessionTypeChat, epoch)
	if err != nil {
		return err
	}
	for _, task := range selected {
		cancelled, err := h.requestTaskCancellation(c, task, "", message)
		if err != nil {
			var fiberErr *fiber.Error
			if errors.Is(err, errCancelTaskGone) || (errors.As(err, &fiberErr) && fiberErr.Code == fiber.StatusConflict) {
				continue // finished, deleted or replaced since the list
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

// authorizedSessionTasks returns the cancellable Tasks of the Session. It
// authorizes every one before the caller cancels any, so a denial leaves none
// of them cancelled.
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
			// The route authorized the Session's namespace only.
			if err := authorizeKubernetesResourceAction(c.Context(), h.clientset, GetUserInfo(c), task.Namespace,
				apiVerbUpdate, corev1alpha1.GroupVersion.Group, "tasks/cancel", task.Name); err != nil {
				return nil, err
			}
		}
		selected = append(selected, task)
	}
	return selected, nil
}

// sessionTaskCandidates lists, live, the Tasks that may belong to a Session:
// those in its namespace and, for a chat Session, the Tasks its chat created
// in other namespaces, which only happens when the API may act outside the
// Session's namespace. The cache can miss a Task a stopped request just
// created, so the API reader is used.
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

// taskInSession reports whether task belongs to the Session namespace/name:
// through spec.sessionRef, which names a Session in the Task's namespace, or,
// for a chat Session, through the chat labels. A chat-created Task may carry
// both, naming its continuity Session in spec.sessionRef. A chat Task without
// the chat namespace label belongs to a chat in its own namespace.
func taskInSession(task *corev1alpha1.Task, namespace, name string, chat bool, epoch string) bool {
	// A chat Task pinned to another incarnation of the chat, one deleted and
	// recreated under this name, is not this Session's turn.
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

// requestTaskCancellation records a cancel request on task. A second request
// changes nothing; a finished Task other than a Cancelled one is a conflict.
func (h *Handlers) requestTaskCancellation(c fiber.Ctx, task *corev1alpha1.Task, expectedUID, message string) (taskCancellation, error) {
	if err := h.authorizeContextTokenLoadedTask(c, "cancelTask", task); err != nil {
		return taskCancellation{}, err
	}
	if err := h.taskAccess().authorizeGatewayTaskOperate(c, "cancelTask", task); err != nil {
		return taskCancellation{}, err
	}
	// The confirmed UID is compared only once the caller may act on this
	// Task, so a refusal says nothing about the Task's identity.
	if expectedUID != "" && string(task.UID) != expectedUID {
		return taskCancellation{}, fiber.NewError(fiber.StatusConflict, "the task was replaced since it was confirmed; reload it")
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

// cancellableTask reports whether a Task is still Pending or Running and is
// not a schedule. A Task with spec.schedule is a cron template, in every
// phase including the empty or Pending one it starts in; cancelling it would
// end all future runs, so it is suspended instead.
func cancellableTask(task *corev1alpha1.Task) bool {
	if strings.TrimSpace(task.Spec.Schedule) != "" {
		return false
	}
	phase := task.Status.Phase
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
// cancelMessageLimit bounds a cancel request's message in bytes, so the
// Task status it becomes, with any explanation recovery appends, stays
// within the execution status limit whatever the caller's name.
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
