/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/labels"
)

const (
	kubernetesJobKind  = "Job"
	kubernetesTaskKind = "Task"

	harnessWrapperStartedAnnotation     = "orka.ai/harness-wrapper-started"
	harnessWrapperTurnIDAnnotation      = "orka.ai/harness-wrapper-turn-id"
	harnessWrapperRuntimeAnnotation     = "orka.ai/harness-wrapper-runtime-session-id"
	harnessWrapperCorrelationAnnotation = "orka.ai/harness-wrapper-correlation-id"
	harnessWrapperPlannedAtAnnotation   = "orka.ai/harness-wrapper-planned-at"
	harnessWrapperMetadataAnnotation    = "orka.ai/harness-wrapper-metadata"
	harnessWrapperRuntimeRefAnnotation  = "orka.ai/harness-wrapper-runtime-ref"
	harnessWrapperContractAnnotation    = "orka.ai/harness-wrapper-contract-version"
	harnessWrapperServiceAccountEnv     = "ORKA_HARNESS_WRAPPER_SERVICE_ACCOUNT_NAME"
	harnessWrapperPlannedTurnTTL        = 5 * time.Minute
	harnessWrapperComponentLabel        = "agent-harness-wrapper"
)

type internalCallerAuthorizer struct {
	k8sReader client.Reader
}

func (h *InternalHandlers) internalCallerAuthorizer() internalCallerAuthorizer {
	if h == nil {
		return internalCallerAuthorizer{}
	}
	reader := h.apiReader
	if reader == nil {
		reader = h.k8sClient
	}
	return internalCallerAuthorizer{k8sReader: reader}
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

// verifyTaskCaller resolves the authenticated Pod through its owning Job to an
// immutable Task UID, then verifies that UID is still the active Task addressed
// by the request. It intentionally does not grant controller or harness service
// accounts a name/annotation-based exception; future remote runtimes must use
// their capability-bound protocol rather than impersonating an in-cluster worker.
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
	if task.UID == "" || callerTask.UID != task.UID {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller is not the current worker for this task")
	}
	if !activeInternalWorkerTask(task) {
		return nil, fiber.NewError(fiber.StatusForbidden, "target task is not active")
	}
	return task, nil
}

func (a internalCallerAuthorizer) verifyArtifactUploadCaller(c fiber.Ctx, namespace, taskName string) error {
	userInfo := GetUserInfo(c)
	if isHarnessWrapperServiceAccount(userInfo) {
		return a.verifyHarnessWrapperArtifactUpload(c.Context(), userInfo, namespace, taskName)
	}
	_, err := a.verifyTaskCaller(c, namespace, taskName)
	return err
}

func (a internalCallerAuthorizer) verifyHarnessWrapperArtifactUpload(
	ctx context.Context,
	userInfo *UserInfo,
	namespace string,
	taskName string,
) error {
	controlNamespace, err := verifyHarnessWrapperIdentity(userInfo)
	if err != nil {
		return err
	}
	if a.k8sReader == nil {
		return fiber.NewError(fiber.StatusUnauthorized, "task caller authorization unavailable")
	}
	if err := a.verifyHarnessWrapperPod(ctx, userInfo, controlNamespace); err != nil {
		return err
	}

	task := &corev1alpha1.Task{}
	if err := a.k8sReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: taskName}, task); err != nil {
		return fiber.NewError(fiber.StatusForbidden, "target task not found")
	}
	if !harnessWrapperArtifactTaskAuthorized(task) {
		return fiber.NewError(fiber.StatusForbidden, "target task is not an active built-in harness turn")
	}
	return nil
}

func verifyHarnessWrapperIdentity(userInfo *UserInfo) (string, error) {
	if userInfo == nil {
		return "", fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	if userInfo.AuthType != AuthTypeTokenReview {
		return "", fiber.NewError(fiber.StatusForbidden, "caller pod token required")
	}
	callerNamespace := strings.TrimSpace(userInfo.Namespace)
	usernameNamespace := parseServiceAccountNamespace(userInfo.Username)
	if callerNamespace == "" || usernameNamespace == "" || callerNamespace != usernameNamespace {
		return "", fiber.NewError(fiber.StatusForbidden, "ServiceAccount namespace mismatch")
	}
	controlNamespace := currentPodNamespace()
	if controlNamespace == "" {
		return "", fiber.NewError(fiber.StatusForbidden, "controller namespace unavailable")
	}
	if callerNamespace != controlNamespace || serviceAccountNameFromUsername(userInfo.Username) != expectedHarnessWrapperServiceAccountName() {
		return "", fiber.NewError(fiber.StatusForbidden, "caller is not the harness wrapper service account")
	}
	return controlNamespace, nil
}

func (a internalCallerAuthorizer) verifyHarnessWrapperPod(
	ctx context.Context,
	userInfo *UserInfo,
	controlNamespace string,
) error {
	podName := firstUserExtra(userInfo, "authentication.kubernetes.io/pod-name")
	podUID := firstUserExtra(userInfo, "authentication.kubernetes.io/pod-uid")
	if podName == "" || podUID == "" {
		return fiber.NewError(fiber.StatusForbidden, "caller pod identity required")
	}
	pod := &corev1.Pod{}
	if err := a.k8sReader.Get(ctx, types.NamespacedName{Namespace: controlNamespace, Name: podName}, pod); err != nil {
		return fiber.NewError(fiber.StatusForbidden, "caller pod not found")
	}
	if pod.UID == "" || string(pod.UID) != podUID || !activeInternalWorkerPod(pod) {
		return fiber.NewError(fiber.StatusForbidden, "caller pod identity mismatch")
	}
	if pod.Spec.ServiceAccountName != expectedHarnessWrapperServiceAccountName() ||
		pod.Labels["app.kubernetes.io/component"] != harnessWrapperComponentLabel {
		return fiber.NewError(fiber.StatusForbidden, "caller pod is not the harness wrapper")
	}
	return nil
}

func harnessWrapperArtifactTaskAuthorized(task *corev1alpha1.Task) bool {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent || strings.TrimSpace(task.Status.JobName) != "" ||
		!activeInternalWorkerTask(task) || task.Status.HarnessRuntime != nil || task.Annotations == nil {
		return false
	}
	attempt := harnessWrapperArtifactAttempt(task)
	correlationID := strings.TrimSpace(task.Annotations[harnessWrapperCorrelationAnnotation])
	if correlationID == "" || correlationID != string(task.UID) {
		return false
	}
	if strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation]) != harnessWrapperArtifactTurnID(task, attempt) {
		return false
	}
	if strings.TrimSpace(task.Annotations[harnessWrapperRuntimeRefAnnotation]) != "" ||
		strings.TrimSpace(task.Annotations[harnessWrapperContractAnnotation]) != harness.ProtocolVersion {
		return false
	}
	metadata := map[string]string{}
	if err := json.Unmarshal([]byte(task.Annotations[harnessWrapperMetadataAnnotation]), &metadata); err != nil {
		return false
	}
	runtimeName := strings.TrimSpace(metadata["runtime"])
	if runtimeName == "" || strings.TrimSpace(metadata["wrapper"]) != "cli" ||
		strings.TrimSpace(metadata["runtimeRef"]) != "" ||
		strings.TrimSpace(metadata["contractVersion"]) != harness.ProtocolVersion {
		return false
	}
	expectedRuntimeSessionID := harnessWrapperArtifactRuntimeSessionID(task, runtimeName)
	if strings.TrimSpace(task.Annotations[harnessWrapperRuntimeAnnotation]) != expectedRuntimeSessionID {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(task.Annotations[harnessWrapperStartedAnnotation]), "true") {
		return true
	}
	plannedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(task.Annotations[harnessWrapperPlannedAtAnnotation]))
	if err != nil {
		return false
	}
	now := time.Now()
	return !plannedAt.After(now.Add(time.Minute)) && now.Sub(plannedAt) <= harnessWrapperPlannedTurnTTL
}

func harnessWrapperArtifactAttempt(task *corev1alpha1.Task) int32 {
	if task == nil {
		return 1
	}
	attempt := task.Status.Attempts
	if task.Status.Phase == corev1alpha1.TaskPhasePending {
		attempt++
	}
	if attempt <= 0 {
		return 1
	}
	return attempt
}

func harnessWrapperArtifactTurnID(task *corev1alpha1.Task, attempt int32) string {
	identity := fmt.Sprintf("%s/%s/%s/%d", task.Namespace, task.Name, task.UID, attempt)
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s-%s-%d", harnessWrapperArtifactTurnIDPrefix(task.Name), hex.EncodeToString(sum[:])[:12], attempt)
}

func harnessWrapperArtifactTurnIDPrefix(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
		case r == '-', r == '_', r == '.':
			out.WriteRune(r)
		default:
			out.WriteByte('-')
		}
		if out.Len() >= 40 {
			break
		}
	}
	prefix := strings.Trim(out.String(), "-_.")
	if prefix == "" {
		return "turn"
	}
	return prefix
}

// harnessWrapperArtifactRuntimeSessionID intentionally mirrors the controller's
// stable runtime-session identity. Retries are fenced by the attempt-specific
// turn ID; the runtime-session ID remains stable so a session can continue
// across turns and retries.
func harnessWrapperArtifactRuntimeSessionID(task *corev1alpha1.Task, runtimeName string) string {
	sessionName := ""
	if task.Spec.SessionRef != nil && !task.Spec.SessionRef.PromptIncluded {
		sessionName = task.Spec.SessionRef.Name
	}
	identity := harness.ResolveRuntimeSessionIdentity(harness.RuntimeSessionIdentityInput{
		Namespace: task.Namespace, TaskName: task.Name, TaskUID: string(task.UID),
		SessionName: sessionName, RuntimeName: runtimeName, ActiveTask: task.Name,
		Provider: harness.ProviderKindKubernetesService,
	})
	return string(identity.ID)
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

func (a internalCallerAuthorizer) resolveTaskWorker(
	ctx context.Context,
	userInfo *UserInfo,
	namespace string,
) (*corev1alpha1.Task, error) {
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

func (a internalCallerAuthorizer) coordinationTreeSessionNames(
	ctx context.Context,
	callerTask *corev1alpha1.Task,
) (map[string]struct{}, error) {
	if a.k8sReader == nil || callerTask == nil || callerTask.UID == "" {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller task identity required")
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
	parent, err := a.verifiedCoordinationParent(c.Context(), callerTask, parentTask)
	if err != nil {
		return err
	}
	if toTask == "*" {
		return nil
	}
	target := &corev1alpha1.Task{}
	if err := a.k8sReader.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: toTask}, target); err != nil {
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
	if !hasParent || requestedParent == "" || requestedParent != parentName {
		return nil, fiber.NewError(fiber.StatusForbidden, "message parent is outside caller coordination scope")
	}
	parent := &corev1alpha1.Task{}
	if err := a.k8sReader.Get(ctx, types.NamespacedName{Namespace: callerTask.Namespace, Name: parentName}, parent); err != nil {
		return nil, fiber.NewError(fiber.StatusForbidden, "message parent is outside caller coordination scope")
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
