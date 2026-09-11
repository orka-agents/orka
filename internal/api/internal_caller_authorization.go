/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/gofiber/fiber/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
)

const (
	kubernetesJobKind  = "Job"
	kubernetesTaskKind = "Task"
)

type internalCallerAuthorizer struct {
	k8sReader               client.Reader
	taskProvenanceProtected bool
}

func (h *InternalHandlers) internalCallerAuthorizer() internalCallerAuthorizer {
	if h == nil {
		return internalCallerAuthorizer{}
	}
	reader := h.apiReader
	if reader == nil {
		reader = h.k8sClient
	}
	return internalCallerAuthorizer{k8sReader: reader, taskProvenanceProtected: h.taskProvenanceProtected}
}

// verifyNamespace checks that the authenticated caller's ServiceAccount namespace
// matches the target namespace in the URL path. Task-, session-, plan-, result-,
// message-, artifact-, status-, and event-scoped handlers must additionally use
// the worker identity checks below.
func (a internalCallerAuthorizer) verifyNamespace(c fiber.Ctx, namespace string) error {
	userInfo := GetUserInfo(c)
	if userInfo == nil {
		return fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}

	if userInfo.Namespace != "" && userInfo.Namespace != namespace {
		log.Info("cross-namespace access denied",
			"callerNamespace", userInfo.Namespace,
			"targetNamespace", namespace,
			"username", userInfo.Username,
			"ip", c.IP(),
		)
		return fiber.NewError(fiber.StatusForbidden, "cross-namespace access denied")
	}

	// ServiceAccount usernames follow the format:
	// system:serviceaccount:<namespace>:<name>.
	parts := strings.Split(userInfo.Username, ":")
	isServiceAccount := len(parts) == 4 && parts[0] == "system" && parts[1] == "serviceaccount" //nolint:goconst // "system" here is K8s SA prefix, not chat role
	if isServiceAccount && parts[2] != namespace {
		log.Info("cross-namespace access denied",
			"callerNamespace", parts[2],
			"targetNamespace", namespace,
			"username", userInfo.Username,
			"ip", c.IP(),
		)
		return fiber.NewError(fiber.StatusForbidden, "cross-namespace access denied")
	}

	// Fail closed: namespace-scoped internal endpoints require a verifiable
	// caller namespace. Principals without one (for example non-ServiceAccount
	// TokenReview identities) must not pass for arbitrary namespaces.
	if userInfo.Namespace == "" && !isServiceAccount {
		log.Info("internal access denied for caller without namespace identity",
			"targetNamespace", namespace,
			"username", userInfo.Username,
			"ip", c.IP(),
		)
		return fiber.NewError(fiber.StatusForbidden, "caller namespace identity required")
	}

	return nil
}

// verifyTaskCaller resolves the authenticated Pod through its owning Job to an
// immutable Task UID, then verifies that UID is still the active Task addressed
// by the request. It intentionally does not grant controller or harness service
// accounts a name/annotation-based exception. Runtime callers must use their
// capability-bound protocol.
func (a internalCallerAuthorizer) verifyTaskCaller(
	c fiber.Ctx,
	namespace string,
	taskName string,
) (*corev1alpha1.Task, error) {
	callerTask, err := a.resolveTaskWorker(c.Context(), GetUserInfo(c), namespace)
	if err != nil {
		return nil, err
	}

	task := &corev1alpha1.Task{}
	if err := a.k8sReader.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: taskName}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusForbidden, "caller is not authorized for this task")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to load target task: %v", err))
	}
	if task.UID == "" || callerTask.UID != task.UID ||
		callerTask.Status.JobName != task.Status.JobName || callerTask.Status.JobUID != task.Status.JobUID {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller is not the current worker for this task")
	}
	if !activeInternalWorkerTask(task) {
		return nil, fiber.NewError(fiber.StatusForbidden, "target task is not active")
	}
	return task, nil
}

func (a internalCallerAuthorizer) verifyExecutionEventStreamWriter(
	c fiber.Ctx,
	namespace string,
	streamType string,
	streamID string,
) (*corev1alpha1.Task, error) {
	if streamType != events.ExecutionEventStreamTypeTask {
		return nil, fiber.NewError(fiber.StatusBadRequest, "unsupported execution event stream type")
	}
	if a.k8sReader == nil {
		return nil, fiber.NewError(fiber.StatusUnauthorized, "task caller authorization unavailable")
	}
	callerTask, err := a.resolveTaskWorker(c.Context(), GetUserInfo(c), namespace)
	if err != nil {
		return nil, err
	}
	task := &corev1alpha1.Task{}
	if err := a.k8sReader.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: streamID}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusForbidden, "caller is not the current worker for this task")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	if task.UID == "" || callerTask.UID != task.UID {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller is not the current worker for this task")
	}
	if !task.DeletionTimestamp.IsZero() {
		return nil, fiber.NewError(fiber.StatusGone, "task is deleting")
	}
	if !activeInternalWorkerTask(task) {
		return nil, fiber.NewError(fiber.StatusConflict, "task is complete")
	}
	return task, nil
}

func (a internalCallerAuthorizer) verifyTaskWorker(ctx context.Context, userInfo *UserInfo, task *corev1alpha1.Task) error {
	if task == nil {
		return fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	callerTask, err := a.resolveTaskWorker(ctx, userInfo, task.Namespace)
	if err != nil {
		return err
	}
	if task.UID == "" || callerTask.UID != task.UID {
		return fiber.NewError(fiber.StatusForbidden, "caller is not the current worker for this task")
	}
	return nil
}

func (a internalCallerAuthorizer) resolveCallerPod(
	ctx context.Context,
	userInfo *UserInfo,
	namespace string,
) (*corev1.Pod, error) {
	if userInfo == nil {
		return nil, fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	if err := verifyTokenReviewServiceAccount(userInfo, namespace); err != nil {
		return nil, err
	}
	if a.k8sReader == nil {
		return nil, fiber.NewError(fiber.StatusUnauthorized, "task caller authorization unavailable")
	}
	podName := firstUserExtra(userInfo, "authentication.kubernetes.io/pod-name")
	podUID := firstUserExtra(userInfo, "authentication.kubernetes.io/pod-uid")
	if podName == "" || podUID == "" {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller pod identity required")
	}

	pod := &corev1.Pod{}
	if err := a.k8sReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusForbidden, "caller pod not found")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to load caller pod: %v", err))
	}
	if pod.UID == "" || string(pod.UID) != podUID {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller pod identity mismatch")
	}
	if !activeInternalWorkerPod(pod) {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller pod is not active")
	}
	if strings.TrimSpace(pod.Spec.ServiceAccountName) != serviceAccountNameFromUsername(userInfo.Username) {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller pod ServiceAccount mismatch")
	}
	return pod, nil
}

func (a internalCallerAuthorizer) resolveTaskWorker(
	ctx context.Context,
	userInfo *UserInfo,
	namespace string,
) (*corev1alpha1.Task, error) {
	pod, err := a.resolveCallerPod(ctx, userInfo, namespace)
	if err != nil {
		return nil, err
	}
	for _, owner := range pod.OwnerReferences {
		if !validControllerOwnerReference(owner, batchv1.SchemeGroupVersion.String(), kubernetesJobKind) {
			continue
		}
		job := &batchv1.Job{}
		if err := a.k8sReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: owner.Name}, job); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to load caller job: %v", err))
		}
		if job.UID == "" || owner.UID != job.UID || !job.DeletionTimestamp.IsZero() {
			continue
		}
		for _, jobOwner := range job.OwnerReferences {
			if !validControllerOwnerReference(jobOwner, corev1alpha1.GroupVersion.String(), kubernetesTaskKind) {
				continue
			}
			task := &corev1alpha1.Task{}
			if err := a.k8sReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: jobOwner.Name}, task); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to load caller task: %v", err))
			}
			if task.UID == "" || jobOwner.UID != task.UID {
				continue
			}
			if pod.Labels[labels.LabelTask] != labels.SelectorValue(task.Name) {
				continue
			}
			if task.Status.JobName == "" && task.Status.JobUID == "" && activeInternalWorkerTask(task) {
				// Job creation precedes the status write that publishes its identity.
				// Deny access until that write completes, but let a fast worker retry
				// its result instead of treating the publication delay as permanent.
				return nil, fiber.NewError(fiber.StatusServiceUnavailable, "task worker identity is not yet published")
			}
			if strings.TrimSpace(task.Status.JobName) != job.Name || task.Status.JobUID == "" || task.Status.JobUID != string(job.UID) {
				continue
			}
			recordInternalTaskJobAuthority(ctx, task)
			return task, nil
		}
	}
	return nil, fiber.NewError(fiber.StatusForbidden, "caller is not the current worker for this task")
}

func validControllerOwnerReference(owner metav1.OwnerReference, apiVersion, kind string) bool {
	return owner.APIVersion == apiVersion && owner.Kind == kind && owner.Controller != nil && *owner.Controller &&
		strings.TrimSpace(owner.Name) != "" && owner.UID != ""
}

func verifyTokenReviewServiceAccount(userInfo *UserInfo, namespace string) error {
	if userInfo == nil {
		return fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	if userInfo.AuthType != AuthTypeTokenReview {
		return fiber.NewError(fiber.StatusForbidden, "caller pod token required")
	}
	usernameNamespace := parseServiceAccountNamespace(userInfo.Username)
	serviceAccountName := serviceAccountNameFromUsername(userInfo.Username)
	callerNamespace := strings.TrimSpace(userInfo.Namespace)
	if usernameNamespace == "" || serviceAccountName == "" {
		return fiber.NewError(fiber.StatusForbidden, "ServiceAccount identity required")
	}
	if callerNamespace == "" {
		return fiber.NewError(fiber.StatusForbidden, "ServiceAccount namespace required")
	}
	if callerNamespace != usernameNamespace || callerNamespace != namespace {
		return fiber.NewError(fiber.StatusForbidden, "ServiceAccount namespace mismatch")
	}
	return nil
}

func activeInternalWorkerPod(pod *corev1.Pod) bool {
	if pod == nil || !pod.DeletionTimestamp.IsZero() {
		return false
	}
	switch pod.Status.Phase {
	case "", corev1.PodPending, corev1.PodRunning:
		return true
	default:
		return false
	}
}

func activeInternalWorkerTask(task *corev1alpha1.Task) bool {
	if task == nil || task.UID == "" || !task.DeletionTimestamp.IsZero() || task.Status.ExecutionOutcome != nil {
		return false
	}
	switch task.Status.Phase {
	case "", corev1alpha1.TaskPhasePending, corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskPhaseFinalizing:
		return true
	default:
		return false
	}
}

func (a internalCallerAuthorizer) resolveActiveTaskCaller(c fiber.Ctx, namespace string) (*corev1alpha1.Task, error) {
	task, err := a.resolveTaskWorker(c.Context(), GetUserInfo(c), namespace)
	if err != nil {
		return nil, err
	}
	if !activeInternalWorkerTask(task) {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller task is not active")
	}
	return task, nil
}

func (a internalCallerAuthorizer) coordinationTreeSessionReferences(
	ctx context.Context,
	callerTask *corev1alpha1.Task,
) (map[string][]corev1alpha1.SessionReference, error) {
	if a.k8sReader == nil || callerTask == nil || callerTask.UID == "" {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller task identity required")
	}
	allowed := map[string][]corev1alpha1.SessionReference{}
	addTranscriptSessionReference(allowed, callerTask.Spec.SessionRef)
	if !a.taskProvenanceProtected {
		return allowed, nil
	}
	tasks := &corev1alpha1.TaskList{}
	if err := a.k8sReader.List(ctx, tasks, client.InNamespace(callerTask.Namespace)); err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list tasks: %v", err))
	}
	tasksByName := make(map[string]*corev1alpha1.Task, len(tasks.Items))
	for i := range tasks.Items {
		task := &tasks.Items[i]
		tasksByName[task.Name] = task
	}
	listedCaller := tasksByName[callerTask.Name]
	if listedCaller == nil || listedCaller.UID != callerTask.UID {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller task identity changed")
	}
	callerRoot, ok := coordinationRootTask(listedCaller, tasksByName)
	if !ok || callerRoot.UID == "" {
		return allowed, nil
	}
	for i := range tasks.Items {
		task := &tasks.Items[i]
		root, valid := coordinationRootTask(task, tasksByName)
		if !valid || root.UID != callerRoot.UID {
			continue
		}
		addTranscriptSessionReference(allowed, task.Spec.SessionRef)
	}
	return allowed, nil
}

func addTranscriptSessionReference(allowed map[string][]corev1alpha1.SessionReference, ref *corev1alpha1.SessionReference) {
	if ref == nil {
		return
	}
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return
	}
	// Keep every distinct reference so another Task cannot widen the caller's
	// history window for the same session.
	if !slices.Contains(allowed[name], *ref) {
		allowed[name] = append(allowed[name], *ref)
	}
}

func coordinationRootTask(
	task *corev1alpha1.Task,
	tasksByName map[string]*corev1alpha1.Task,
) (*corev1alpha1.Task, bool) {
	if task == nil || task.UID == "" || !task.DeletionTimestamp.IsZero() {
		return nil, false
	}
	current := task
	seen := map[types.UID]struct{}{}
	for {
		if _, exists := seen[current.UID]; exists {
			return nil, false
		}
		seen[current.UID] = struct{}{}
		parentName, parentUID, hasParent, valid := coordinationParentIdentity(current)
		if !valid {
			return nil, false
		}
		if !hasParent {
			return current, true
		}
		parent := tasksByName[parentName]
		if parent == nil || parent.UID != parentUID || !parent.DeletionTimestamp.IsZero() {
			return nil, false
		}
		current = parent
	}
}

func coordinationParentIdentity(task *corev1alpha1.Task) (string, types.UID, bool, bool) {
	if task == nil {
		return "", "", false, false
	}
	parentName := labels.ParentTaskName(task.Labels, task.Annotations)
	if parentName == "" {
		return "", "", false, true
	}
	for _, owner := range task.OwnerReferences {
		if validControllerOwnerReference(owner, corev1alpha1.GroupVersion.String(), kubernetesTaskKind) && owner.Name == parentName {
			return parentName, owner.UID, true, true
		}
	}
	return parentName, "", true, false
}

func (a internalCallerAuthorizer) verifyMessageSender(
	c fiber.Ctx,
	namespace string,
	fromTask string,
	toTask string,
	parentTask string,
) error {
	callerTask, err := a.verifyTaskCaller(c, namespace, fromTask)
	if err != nil {
		return err
	}
	return a.verifyTaskMessageScope(c.Context(), callerTask, toTask, parentTask)
}

func (a internalCallerAuthorizer) verifyTaskMessageScope(ctx context.Context, callerTask *corev1alpha1.Task, toTask, parentTask string) error {
	parent, err := a.verifiedCoordinationParent(ctx, callerTask, parentTask)
	if err != nil {
		return err
	}
	if toTask == "*" {
		return nil
	}
	target := &corev1alpha1.Task{}
	if err := a.k8sReader.Get(ctx, types.NamespacedName{Namespace: callerTask.Namespace, Name: toTask}, target); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusForbidden, "message target is outside caller coordination scope")
		}
		return fiber.NewError(fiber.StatusInternalServerError, "failed to load message target")
	}
	if target.UID == "" || !target.DeletionTimestamp.IsZero() {
		return fiber.NewError(fiber.StatusForbidden, "message target is outside caller coordination scope")
	}
	if target.Name == parent.Name && target.UID == parent.UID {
		return nil
	}
	targetParentName, targetParentUID, hasParent, valid := coordinationParentIdentity(target)
	if !valid || !hasParent || targetParentName != parent.Name || targetParentUID != parent.UID {
		return fiber.NewError(fiber.StatusForbidden, "message target is outside caller coordination scope")
	}
	return nil
}

func (a internalCallerAuthorizer) verifyMessageInbox(
	c fiber.Ctx,
	namespace string,
	taskName string,
	parentTask string,
) error {
	callerTask, err := a.verifyTaskCaller(c, namespace, taskName)
	if err != nil {
		return err
	}
	_, err = a.verifiedCoordinationParent(c.Context(), callerTask, parentTask)
	return err
}

func (a internalCallerAuthorizer) verifiedCoordinationParent(
	ctx context.Context,
	callerTask *corev1alpha1.Task,
	requestedParent string,
) (*corev1alpha1.Task, error) {
	if !a.taskProvenanceProtected {
		return nil, fiber.NewError(fiber.StatusForbidden, "coordination requires Task provenance admission")
	}
	requestedParent = strings.TrimSpace(requestedParent)
	parentName, parentUID, hasParent, valid := coordinationParentIdentity(callerTask)
	if !valid {
		return nil, fiber.NewError(fiber.StatusForbidden, "message parent is outside caller coordination scope")
	}
	if requestedParent == callerTask.Name && callerTask.UID != "" && callerTask.DeletionTimestamp.IsZero() {
		return callerTask, nil
	}
	if !hasParent || requestedParent == "" || requestedParent != parentName {
		return nil, fiber.NewError(fiber.StatusForbidden, "message parent is outside caller coordination scope")
	}
	parent := &corev1alpha1.Task{}
	if err := a.k8sReader.Get(ctx, types.NamespacedName{Namespace: callerTask.Namespace, Name: parentName}, parent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusForbidden, "message parent is outside caller coordination scope")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to load message parent")
	}
	if parent.UID == "" || parent.UID != parentUID || !parent.DeletionTimestamp.IsZero() {
		return nil, fiber.NewError(fiber.StatusForbidden, "message parent is outside caller coordination scope")
	}
	return parent, nil
}

func serviceAccountNameFromUsername(username string) string {
	parts := strings.Split(strings.TrimSpace(username), ":")
	if len(parts) == 4 && parts[0] == "system" && parts[1] == "serviceaccount" {
		return parts[3]
	}
	return ""
}

func firstUserExtra(userInfo *UserInfo, key string) string {
	if userInfo == nil || len(userInfo.Extra) == 0 {
		return ""
	}
	values := userInfo.Extra[key]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func isTerminalInternalTaskPhase(phase corev1alpha1.TaskPhase) bool {
	switch phase {
	case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
		return true
	default:
		return false
	}
}
