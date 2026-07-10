/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	k8sClient client.Client
}

func (h *InternalHandlers) internalCallerAuthorizer() internalCallerAuthorizer {
	if h == nil {
		return internalCallerAuthorizer{}
	}
	return internalCallerAuthorizer{k8sClient: h.k8sClient}
}

// verifyNamespace checks that the authenticated caller's ServiceAccount namespace
// matches the target namespace in the URL path. Callers of task/session/message
// data must additionally use the task-bound checks below.
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
	if len(parts) == 4 && parts[0] == "system" && parts[1] == "serviceaccount" { //nolint:goconst // "system" here is K8s SA prefix, not chat role
		if parts[2] != namespace {
			log.Info("cross-namespace access denied",
				"callerNamespace", parts[2],
				"targetNamespace", namespace,
				"username", userInfo.Username,
				"ip", c.IP(),
			)
			return fiber.NewError(fiber.StatusForbidden, "cross-namespace access denied")
		}
	}

	return nil
}

func (a internalCallerAuthorizer) verifyTaskCaller(
	c fiber.Ctx,
	namespace string,
	taskName string,
) (*corev1alpha1.Task, error) {
	if a.k8sClient == nil {
		return nil, fiber.NewError(fiber.StatusUnauthorized, "task caller authorization unavailable")
	}
	task := &corev1alpha1.Task{}
	if err := a.k8sClient.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: taskName}, task); err != nil {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller is not authorized for this task")
	}
	userInfo := GetUserInfo(c)
	if isHarnessWrapperServiceAccount(userInfo) {
		if err := a.verifyHarnessWrapperTask(c.Context(), userInfo, task); err != nil {
			return nil, err
		}
		return task, nil
	}
	if err := a.verifyTaskWorker(c.Context(), userInfo, task); err != nil {
		return nil, err
	}
	if !activeInternalWorkerTask(task) {
		return nil, fiber.NewError(fiber.StatusForbidden, "target task is not active")
	}
	return task, nil
}

func (a internalCallerAuthorizer) verifyArtifactUploadCaller(c fiber.Ctx, namespace, taskName string) error {
	_, err := a.verifyTaskCaller(c, namespace, taskName)
	return err
}

// verifyHarnessWrapperArtifactUpload is retained for the artifact-specific tests
// and delegates to the generic harness-backed task authorization path.
func (a internalCallerAuthorizer) verifyHarnessWrapperArtifactUpload(
	ctx context.Context,
	userInfo *UserInfo,
	namespace string,
	taskName string,
) error {
	if a.k8sClient == nil {
		return fiber.NewError(fiber.StatusForbidden, "task caller authorization unavailable")
	}
	task := &corev1alpha1.Task{}
	if err := a.k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: taskName}, task); err != nil {
		return fiber.NewError(fiber.StatusForbidden, "target task not found")
	}
	return a.verifyHarnessWrapperTask(ctx, userInfo, task)
}

func (a internalCallerAuthorizer) verifyHarnessWrapperTask(
	_ context.Context,
	userInfo *UserInfo,
	task *corev1alpha1.Task,
) error {
	if task == nil {
		return fiber.NewError(fiber.StatusForbidden, "target task not found")
	}
	if err := verifyHarnessWrapperIdentity(userInfo); err != nil {
		return err
	}
	if !task.DeletionTimestamp.IsZero() {
		return fiber.NewError(fiber.StatusForbidden, "target task is deleting")
	}
	if task.Spec.Type != corev1alpha1.TaskTypeAgent {
		return fiber.NewError(fiber.StatusForbidden, "target task is not an agent task")
	}
	if strings.TrimSpace(task.Status.JobName) != "" {
		return fiber.NewError(fiber.StatusForbidden, "target task has a worker job")
	}
	if task.Status.Phase != "" && task.Status.Phase != corev1alpha1.TaskPhasePending && task.Status.Phase != corev1alpha1.TaskPhaseRunning {
		return fiber.NewError(fiber.StatusForbidden, "target task is not active")
	}
	if !harnessWrapperTaskAuthorized(task) {
		return fiber.NewError(fiber.StatusForbidden, "target task is not running through harness wrapper")
	}
	return nil
}

func verifyHarnessWrapperIdentity(userInfo *UserInfo) error {
	if userInfo == nil {
		return fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	if userInfo.AuthType != AuthTypeTokenReview {
		return fiber.NewError(fiber.StatusForbidden, "caller pod token required")
	}
	callerNamespace := strings.TrimSpace(userInfo.Namespace)
	usernameNamespace := parseServiceAccountNamespace(userInfo.Username)
	if callerNamespace == "" || usernameNamespace == "" || serviceAccountNameFromUsername(userInfo.Username) == "" {
		return fiber.NewError(fiber.StatusForbidden, "ServiceAccount identity with namespace required")
	}
	if callerNamespace != usernameNamespace {
		return fiber.NewError(fiber.StatusForbidden, "ServiceAccount namespace mismatch")
	}
	controlNamespace := currentPodNamespace()
	if controlNamespace == "" {
		return fiber.NewError(fiber.StatusForbidden, "controller namespace unavailable")
	}
	if callerNamespace != controlNamespace {
		return fiber.NewError(fiber.StatusForbidden, "caller is not a control-plane service account")
	}
	if serviceAccountNameFromUsername(userInfo.Username) != expectedHarnessWrapperServiceAccountName() {
		return fiber.NewError(fiber.StatusForbidden, "caller is not the harness wrapper service account")
	}
	return nil
}

func (a internalCallerAuthorizer) verifyExecutionEventStreamWriter(
	c fiber.Ctx,
	namespace string,
	streamType string,
	streamID string,
) (*corev1alpha1.Task, error) {
	if a.k8sClient == nil || streamType != events.ExecutionEventStreamTypeTask {
		return nil, nil
	}
	task := &corev1alpha1.Task{}
	if err := a.k8sClient.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: streamID}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusForbidden, "caller is not the current worker for this task")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	if err := a.verifyTaskWorker(c.Context(), GetUserInfo(c), task); err != nil {
		return nil, err
	}
	if !task.DeletionTimestamp.IsZero() {
		return nil, fiber.NewError(fiber.StatusGone, "task is deleting")
	}
	if isTerminalInternalTaskPhase(task.Status.Phase) {
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

func (a internalCallerAuthorizer) resolveTaskWorker(
	ctx context.Context,
	userInfo *UserInfo,
	namespace string,
) (*corev1alpha1.Task, error) {
	if a.k8sClient == nil || userInfo == nil {
		return nil, fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	if err := verifyTokenReviewServiceAccount(userInfo, namespace); err != nil {
		return nil, err
	}
	podName := firstUserExtra(userInfo, "authentication.kubernetes.io/pod-name")
	podUID := firstUserExtra(userInfo, "authentication.kubernetes.io/pod-uid")
	if podName == "" || podUID == "" {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller pod identity required")
	}

	pod := &corev1.Pod{}
	if err := a.k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err != nil {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller pod not found")
	}
	if pod.UID == "" || string(pod.UID) != podUID {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller pod identity mismatch")
	}
	if !activeInternalWorkerPod(pod) {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller pod is not active")
	}

	for _, owner := range pod.OwnerReferences {
		if owner.Kind != kubernetesJobKind || strings.TrimSpace(owner.Name) == "" || owner.UID == "" {
			continue
		}
		job := &batchv1.Job{}
		if err := a.k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: owner.Name}, job); err != nil {
			continue
		}
		if job.UID == "" || owner.UID != job.UID {
			continue
		}
		if !job.DeletionTimestamp.IsZero() {
			continue
		}
		for _, jobOwner := range job.OwnerReferences {
			if jobOwner.Kind != kubernetesTaskKind || strings.TrimSpace(jobOwner.Name) == "" || jobOwner.UID == "" {
				continue
			}
			task := &corev1alpha1.Task{}
			if err := a.k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: jobOwner.Name}, task); err != nil {
				continue
			}
			if task.UID == "" || jobOwner.UID != task.UID {
				continue
			}
			if strings.TrimSpace(task.Status.JobName) != job.Name {
				continue
			}
			if pod.Labels[labels.LabelTask] != labels.SelectorValue(task.Name) {
				continue
			}
			return task, nil
		}
	}
	return nil, fiber.NewError(fiber.StatusForbidden, "caller is not the current worker for this task")
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
	if task == nil || !task.DeletionTimestamp.IsZero() {
		return false
	}
	switch task.Status.Phase {
	case "", corev1alpha1.TaskPhasePending, corev1alpha1.TaskPhaseRunning:
		return true
	default:
		return false
	}
}

func (a internalCallerAuthorizer) authorizedSessionNames(
	c fiber.Ctx,
	namespace string,
	requestedSession string,
	taskHint string,
) (map[string]struct{}, error) {
	callerTask, err := a.resolveSessionCallerTask(c, namespace, requestedSession, taskHint)
	if err != nil {
		return nil, err
	}
	allowed, err := a.coordinationTreeSessionNames(c.Context(), callerTask)
	if err != nil {
		return nil, err
	}
	if requestedSession != "" {
		if _, ok := allowed[requestedSession]; !ok {
			return nil, fiber.NewError(fiber.StatusForbidden, "caller is not authorized for this session")
		}
	}
	return allowed, nil
}

func (a internalCallerAuthorizer) resolveSessionCallerTask(
	c fiber.Ctx,
	namespace string,
	sessionName string,
	taskHint string,
) (*corev1alpha1.Task, error) {
	userInfo := GetUserInfo(c)
	if !isHarnessWrapperServiceAccount(userInfo) {
		task, err := a.resolveTaskWorker(c.Context(), userInfo, namespace)
		if err != nil {
			return nil, err
		}
		if !activeInternalWorkerTask(task) {
			return nil, fiber.NewError(fiber.StatusForbidden, "caller task is not active")
		}
		return task, nil
	}
	if a.k8sClient == nil {
		return nil, fiber.NewError(fiber.StatusUnauthorized, "task caller authorization unavailable")
	}
	if err := verifyHarnessWrapperIdentity(userInfo); err != nil {
		return nil, err
	}

	sessionName = strings.TrimSpace(sessionName)
	taskHint = strings.TrimSpace(taskHint)
	if sessionName == "" && taskHint == "" {
		return nil, fiber.NewError(fiber.StatusForbidden, "harness wrapper task identity required")
	}

	tasks := &corev1alpha1.TaskList{}
	if err := a.k8sClient.List(c.Context(), tasks, client.InNamespace(namespace)); err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list tasks: %v", err))
	}
	candidates := make([]*corev1alpha1.Task, 0, 1)
	for i := range tasks.Items {
		task := &tasks.Items[i]
		taskSession := sessionNameForTask(task)
		matches := taskHint != "" && (task.Name == taskHint || taskSession == taskHint)
		if taskHint == "" {
			matches = sessionName != "" && taskSession == sessionName
		}
		if !matches || task.UID == "" {
			continue
		}
		if err := a.verifyHarnessWrapperTask(c.Context(), userInfo, task); err == nil {
			candidates = append(candidates, task)
		}
	}
	if len(candidates) == 0 {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller is not authorized for this session")
	}
	if len(candidates) != 1 {
		return nil, fiber.NewError(fiber.StatusForbidden, "harness wrapper task identity is ambiguous")
	}
	return candidates[0], nil
}

func (a internalCallerAuthorizer) coordinationTreeSessionNames(
	ctx context.Context,
	callerTask *corev1alpha1.Task,
) (map[string]struct{}, error) {
	if a.k8sClient == nil || callerTask == nil || callerTask.UID == "" {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller task identity required")
	}
	tasks := &corev1alpha1.TaskList{}
	if err := a.k8sClient.List(ctx, tasks, client.InNamespace(callerTask.Namespace)); err != nil {
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
	allowed := map[string]struct{}{}
	if callerTask.Spec.SessionRef != nil {
		if sessionName := strings.TrimSpace(callerTask.Spec.SessionRef.Name); sessionName != "" {
			allowed[sessionName] = struct{}{}
		}
	}
	callerRoot, ok := coordinationRootTask(listedCaller, tasksByName)
	if !ok || callerRoot.UID == "" {
		return allowed, nil
	}
	for i := range tasks.Items {
		task := &tasks.Items[i]
		root, valid := coordinationRootTask(task, tasksByName)
		if !valid || root.UID != callerRoot.UID || task.Spec.SessionRef == nil {
			continue
		}
		sessionName := strings.TrimSpace(task.Spec.SessionRef.Name)
		if sessionName != "" {
			allowed[sessionName] = struct{}{}
		}
	}
	return allowed, nil
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
		if owner.Kind == kubernetesTaskKind && owner.Name == parentName && owner.UID != "" {
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
	parent, err := a.verifiedCoordinationParent(c.Context(), callerTask, parentTask)
	if err != nil {
		return err
	}
	if toTask == "*" {
		return nil
	}
	target := &corev1alpha1.Task{}
	if err := a.k8sClient.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: toTask}, target); err != nil {
		return fiber.NewError(fiber.StatusForbidden, "message target is outside caller coordination scope")
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
	requestedParent = strings.TrimSpace(requestedParent)
	parentName, parentUID, hasParent, valid := coordinationParentIdentity(callerTask)
	if !valid {
		return nil, fiber.NewError(fiber.StatusForbidden, "message parent is outside caller coordination scope")
	}
	if requestedParent == callerTask.Name && callerTask.UID != "" && callerTask.DeletionTimestamp.IsZero() {
		return callerTask, nil
	}
	if !hasParent {
		return nil, fiber.NewError(fiber.StatusForbidden, "message parent is outside caller coordination scope")
	}
	if requestedParent == "" || requestedParent != parentName {
		return nil, fiber.NewError(fiber.StatusForbidden, "message parent is outside caller coordination scope")
	}
	parent := &corev1alpha1.Task{}
	if err := a.k8sClient.Get(ctx, types.NamespacedName{Namespace: callerTask.Namespace, Name: parentName}, parent); err != nil {
		return nil, fiber.NewError(fiber.StatusForbidden, "message parent is outside caller coordination scope")
	}
	if parent.UID == "" || parent.UID != parentUID || !parent.DeletionTimestamp.IsZero() {
		return nil, fiber.NewError(fiber.StatusForbidden, "message parent is outside caller coordination scope")
	}
	return parent, nil
}

func harnessWrapperTaskAuthorized(task *corev1alpha1.Task) bool {
	if task == nil || task.Annotations == nil ||
		strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation]) == "" ||
		strings.TrimSpace(task.Annotations[harnessWrapperRuntimeAnnotation]) == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(task.Annotations[harnessWrapperStartedAnnotation]), "true") {
		return true
	}
	plannedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(task.Annotations[harnessWrapperPlannedAtAnno]))
	if err != nil {
		return false
	}
	now := time.Now()
	if plannedAt.After(now.Add(time.Minute)) {
		return false
	}
	return now.Sub(plannedAt) <= harnessWrapperPlannedTurnTTL
}

func currentPodNamespace() string {
	if namespace := strings.TrimSpace(os.Getenv("POD_NAMESPACE")); namespace != "" {
		return namespace
	}
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func expectedHarnessWrapperServiceAccountName() string {
	if name := strings.TrimSpace(os.Getenv(harnessWrapperServiceAccountEnv)); name != "" {
		return name
	}
	return "agent-harness-wrapper"
}

func isHarnessWrapperServiceAccount(userInfo *UserInfo) bool {
	return userInfo != nil && serviceAccountNameFromUsername(userInfo.Username) == expectedHarnessWrapperServiceAccountName()
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
