/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
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

type internalCallerAuthorizer struct {
	k8sReader client.Reader
}

const (
	internalMemoryTaskLocalKey       = "internalMemoryTask"
	taskProvenancePolicyLabel        = "orka.ai/task-provenance-policy"
	taskProvenancePolicyJobComponent = "job"
	taskProvenancePolicyPodComponent = "pod"
)

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
// matches the target namespace in the URL path.
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

func (a internalCallerAuthorizer) verifyMemoryNamespace(c fiber.Ctx, namespace string) error {
	userInfo := GetUserInfo(c)
	if centralHarnessWrapperCaller(userInfo) {
		task, err := a.currentHarnessWrapperMemoryTask(c.Context(), userInfo, namespace)
		if err != nil {
			return err
		}
		c.Locals(internalMemoryTaskLocalKey, task)
		return nil
	}
	if err := a.verifyNamespace(c, namespace); err != nil {
		return err
	}
	if userInfo == nil || userInfo.AuthType != AuthTypeTokenReview ||
		serviceAccountNameFromUsername(userInfo.Username) == "" {
		return fiber.NewError(fiber.StatusForbidden, "internal memory caller must be a Kubernetes workload identity")
	}
	task, err := a.currentTaskWorker(c.Context(), userInfo, namespace)
	if err != nil {
		return err
	}
	c.Locals(internalMemoryTaskLocalKey, task)
	return nil
}

func centralHarnessWrapperCaller(userInfo *UserInfo) bool {
	if userInfo == nil || userInfo.AuthType != AuthTypeTokenReview ||
		serviceAccountNameFromUsername(userInfo.Username) != expectedHarnessWrapperServiceAccountName() {
		return false
	}
	controlNamespace := currentPodNamespace()
	usernameNamespace := parseServiceAccountNamespace(userInfo.Username)
	callerNamespace := strings.TrimSpace(userInfo.Namespace)
	return controlNamespace != "" && usernameNamespace == controlNamespace &&
		(callerNamespace == "" || callerNamespace == controlNamespace)
}

func (a internalCallerAuthorizer) currentHarnessWrapperMemoryTask(
	ctx context.Context,
	userInfo *UserInfo,
	namespace string,
) (*corev1alpha1.Task, error) {
	if a.k8sReader == nil || userInfo == nil || userInfo.ContextToken == nil {
		return nil, fiber.NewError(fiber.StatusForbidden, "task-scoped Txn-Token is required")
	}
	token := userInfo.ContextToken
	tokenNamespace, ok := contextString(token.TransactionContext, "namespace")
	if !ok || tokenNamespace != namespace {
		return nil, fiber.NewError(fiber.StatusForbidden, "namespace does not match the task-scoped Txn-Token")
	}
	taskName := internalMemoryTokenTaskName(token, namespace)
	if taskName == "" {
		return nil, fiber.NewError(fiber.StatusForbidden, "task name is missing from the Txn-Token context")
	}
	taskUID, ok := contextString(token.TransactionContext, "taskUID")
	if !ok || strings.TrimSpace(taskUID) == "" {
		return nil, fiber.NewError(fiber.StatusForbidden, "task UID is missing from the Txn-Token context")
	}
	task := &corev1alpha1.Task{}
	if err := a.k8sReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: taskName}, task); err != nil {
		return nil, fiber.NewError(fiber.StatusForbidden, "task identity could not be verified")
	}
	if task.Namespace != namespace || task.Name != taskName || string(task.UID) != taskUID ||
		task.Spec.Type != corev1alpha1.TaskTypeAgent || strings.TrimSpace(task.Status.JobName) != "" ||
		task.Status.HarnessRuntime == nil || strings.TrimSpace(task.Status.HarnessRuntime.RuntimeRefName) == "" ||
		!task.DeletionTimestamp.IsZero() || isTerminalInternalTaskPhase(task.Status.Phase) ||
		!harnessWrapperArtifactUploadAuthorized(task) {
		return nil, fiber.NewError(fiber.StatusForbidden, "task identity is not an active runtimeRef harness task")
	}
	return task, nil
}

func internalMemoryTokenTaskName(token *ContextToken, namespace string) string {
	if token == nil {
		return ""
	}
	if taskName, ok := contextString(token.TransactionContext, "taskName"); ok {
		return strings.TrimSpace(taskName)
	}
	taskRef, ok := contextString(token.TransactionContext, "task")
	if !ok {
		return ""
	}
	if after, found := strings.CutPrefix(taskRef, namespace+"/"); found {
		return strings.TrimSpace(after)
	}
	if !strings.Contains(taskRef, "/") {
		return strings.TrimSpace(taskRef)
	}
	return ""
}

func (a internalCallerAuthorizer) currentTaskWorker(
	ctx context.Context,
	userInfo *UserInfo,
	namespace string,
) (*corev1alpha1.Task, error) {
	if a.k8sReader == nil || userInfo == nil {
		return nil, fiber.NewError(fiber.StatusForbidden, "current worker identity could not be verified")
	}
	podName := firstUserExtra(userInfo, "authentication.kubernetes.io/pod-name")
	podUID := firstUserExtra(userInfo, "authentication.kubernetes.io/pod-uid")
	if podName == "" || podUID == "" {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller pod identity required")
	}
	pod := &corev1.Pod{}
	if err := a.k8sReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err != nil {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller pod not found")
	}
	if string(pod.UID) != podUID || pod.Spec.ServiceAccountName != serviceAccountNameFromUsername(userInfo.Username) {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller pod identity mismatch")
	}
	for _, owner := range pod.OwnerReferences {
		if !trustedControllerOwner(owner, batchv1.SchemeGroupVersion.String(), "Job") {
			continue
		}
		job := &batchv1.Job{}
		if err := a.k8sReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: owner.Name}, job); err != nil ||
			owner.UID != job.UID {
			continue
		}
		for _, taskOwner := range job.OwnerReferences {
			if !trustedControllerOwner(taskOwner, corev1alpha1.GroupVersion.String(), "Task") {
				continue
			}
			task := &corev1alpha1.Task{}
			if err := a.k8sReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: taskOwner.Name}, task); err != nil ||
				taskOwner.UID != task.UID {
				continue
			}
			if !activeInternalMemoryTask(task, job, pod) {
				continue
			}
			if !taskUIDProvenanceMatches(task, job, pod, false) || !a.legacyTaskProvenanceAdmitted(ctx, job, pod) {
				continue
			}
			return task, nil
		}
	}
	return nil, fiber.NewError(fiber.StatusForbidden, "caller is not the current worker for an active task")
}

func trustedControllerOwner(owner metav1.OwnerReference, apiVersion, kind string) bool {
	return owner.APIVersion == apiVersion && owner.Kind == kind && owner.Name != "" && owner.UID != "" &&
		owner.Controller != nil && *owner.Controller
}

func activeInternalMemoryTask(task *corev1alpha1.Task, job *batchv1.Job, pod *corev1.Pod) bool {
	if task == nil || job == nil || pod == nil {
		return false
	}
	taskLabel := labels.SelectorValue(task.Name)
	return (task.Spec.Type == corev1alpha1.TaskTypeAI || task.Spec.Type == corev1alpha1.TaskTypeAgent) &&
		task.Status.JobName == job.Name && task.DeletionTimestamp.IsZero() &&
		!isTerminalInternalTaskPhase(task.Status.Phase) &&
		job.Labels[labels.LabelTask] == taskLabel && pod.Labels[labels.LabelTask] == taskLabel
}

func taskUIDProvenanceMatches(task *corev1alpha1.Task, job *batchv1.Job, pod *corev1.Pod, required bool) bool {
	jobTaskUID := strings.TrimSpace(job.Labels[labels.LabelTaskUID])
	podTaskUID := strings.TrimSpace(pod.Labels[labels.LabelTaskUID])
	if jobTaskUID == "" && podTaskUID == "" {
		return !required
	}
	return jobTaskUID == string(task.UID) && podTaskUID == string(task.UID)
}

func (a internalCallerAuthorizer) legacyTaskProvenanceAdmitted(ctx context.Context, job *batchv1.Job, pod *corev1.Pod) bool {
	if a.k8sReader == nil || job == nil || pod == nil {
		return false
	}
	// The policies allow the legacy provenance shape only when it is established
	// by the authorized Orka Task and Kubernetes Job controllers. Requiring the
	// policies to be currently active preserves that attestation while allowing
	// Jobs and Pods created before the policies were installed to keep running
	// through a mixed-version rollout.
	return a.taskProvenancePolicyActive(ctx, taskProvenancePolicyJobComponent) &&
		a.taskProvenancePolicyActive(ctx, taskProvenancePolicyPodComponent)
}

func (a internalCallerAuthorizer) taskProvenancePolicyActive(ctx context.Context, component string) bool {
	policies := &admissionregistrationv1.ValidatingAdmissionPolicyList{}
	if err := a.k8sReader.List(ctx, policies, client.MatchingLabels{taskProvenancePolicyLabel: component}); err != nil {
		return false
	}
	bindings := &admissionregistrationv1.ValidatingAdmissionPolicyBindingList{}
	if err := a.k8sReader.List(ctx, bindings, client.MatchingLabels{taskProvenancePolicyLabel: component}); err != nil {
		return false
	}
	for i := range policies.Items {
		policy := &policies.Items[i]
		if !policy.DeletionTimestamp.IsZero() {
			continue
		}
		for j := range bindings.Items {
			binding := &bindings.Items[j]
			if !binding.DeletionTimestamp.IsZero() || binding.Spec.PolicyName != policy.Name ||
				!slices.Contains(binding.Spec.ValidationActions, admissionregistrationv1.Deny) {
				continue
			}
			return true
		}
	}
	return false
}

func (a internalCallerAuthorizer) verifyArtifactUploadCaller(c fiber.Ctx, namespace, taskName string) error {
	userInfo := GetUserInfo(c)
	if err := a.verifyNamespace(c, namespace); err != nil {
		var fiberErr *fiber.Error
		if !errors.As(err, &fiberErr) || fiberErr.Code != fiber.StatusForbidden {
			return err
		}
		if allowErr := a.verifyHarnessWrapperArtifactUpload(c.Context(), userInfo, namespace, taskName); allowErr == nil {
			return nil
		}
		return err
	}
	if userInfo != nil && serviceAccountNameFromUsername(userInfo.Username) == expectedHarnessWrapperServiceAccountName() {
		return a.verifyHarnessWrapperArtifactUpload(c.Context(), userInfo, namespace, taskName)
	}
	return nil
}

func (a internalCallerAuthorizer) verifyHarnessWrapperArtifactUpload(
	ctx context.Context,
	userInfo *UserInfo,
	namespace string,
	taskName string,
) error {
	if a.k8sReader == nil || userInfo == nil {
		return fiber.NewError(fiber.StatusForbidden, "cross-namespace access denied")
	}
	if userInfo.AuthType != AuthTypeTokenReview {
		return fiber.NewError(fiber.StatusForbidden, "caller pod token required")
	}
	controlNamespace := currentPodNamespace()
	if controlNamespace == "" {
		return fiber.NewError(fiber.StatusForbidden, "controller namespace unavailable")
	}
	callerNamespace := strings.TrimSpace(userInfo.Namespace)
	if callerNamespace == "" {
		callerNamespace = parseServiceAccountNamespace(userInfo.Username)
	}
	if callerNamespace != controlNamespace {
		return fiber.NewError(fiber.StatusForbidden, "caller is not a control-plane service account")
	}
	if serviceAccountNameFromUsername(userInfo.Username) != expectedHarnessWrapperServiceAccountName() {
		return fiber.NewError(fiber.StatusForbidden, "caller is not the harness wrapper service account")
	}

	task := &corev1alpha1.Task{}
	if err := a.k8sReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: taskName}, task); err != nil {
		return fiber.NewError(fiber.StatusForbidden, "target task not found")
	}
	if task.Spec.Type != corev1alpha1.TaskTypeAgent {
		return fiber.NewError(fiber.StatusForbidden, "target task is not an agent task")
	}
	if strings.TrimSpace(task.Status.JobName) != "" {
		return fiber.NewError(fiber.StatusForbidden, "target task has a worker job")
	}
	if !harnessWrapperArtifactUploadAuthorized(task) {
		return fiber.NewError(fiber.StatusForbidden, "target task is not running through harness wrapper")
	}
	return nil
}

func (a internalCallerAuthorizer) verifyExecutionEventStreamWriter(
	c fiber.Ctx,
	namespace string,
	streamType string,
	streamID string,
) (*corev1alpha1.Task, error) {
	if a.k8sReader == nil || streamType != events.ExecutionEventStreamTypeTask {
		return nil, nil
	}
	task := &corev1alpha1.Task{}
	if err := a.k8sReader.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: streamID}, task); err != nil {
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
	if a.k8sReader == nil || userInfo == nil || task == nil {
		return fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	if userInfo.AuthType != AuthTypeTokenReview {
		return fiber.NewError(fiber.StatusForbidden, "caller pod token required")
	}
	podName := firstUserExtra(userInfo, "authentication.kubernetes.io/pod-name")
	podUID := firstUserExtra(userInfo, "authentication.kubernetes.io/pod-uid")
	if podName == "" || podUID == "" {
		return fiber.NewError(fiber.StatusForbidden, "caller pod identity required")
	}

	pod := &corev1.Pod{}
	if err := a.k8sReader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: podName}, pod); err != nil {
		return fiber.NewError(fiber.StatusForbidden, "caller pod not found")
	}
	if string(pod.UID) != podUID || pod.Spec.ServiceAccountName != serviceAccountNameFromUsername(userInfo.Username) {
		return fiber.NewError(fiber.StatusForbidden, "caller pod identity mismatch")
	}
	if pod.Labels[labels.LabelTask] != labels.SelectorValue(task.Name) {
		return fiber.NewError(fiber.StatusForbidden, "caller pod does not belong to task")
	}
	currentJobName := strings.TrimSpace(task.Status.JobName)
	if currentJobName == "" {
		return fiber.NewError(fiber.StatusForbidden, "task has no active worker job")
	}

	for _, owner := range pod.OwnerReferences {
		if !trustedControllerOwner(owner, batchv1.SchemeGroupVersion.String(), "Job") || owner.Name != currentJobName {
			continue
		}
		job := &batchv1.Job{}
		if err := a.k8sReader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: owner.Name}, job); err != nil {
			return fiber.NewError(fiber.StatusForbidden, "caller job not found")
		}
		if owner.UID != job.UID || job.Labels[labels.LabelTask] != labels.SelectorValue(task.Name) ||
			!taskUIDProvenanceMatches(task, job, pod, false) {
			continue
		}
		for _, jobOwner := range job.OwnerReferences {
			if trustedControllerOwner(jobOwner, corev1alpha1.GroupVersion.String(), "Task") &&
				jobOwner.Name == task.Name && jobOwner.UID == task.UID {
				return nil
			}
		}
	}
	return fiber.NewError(fiber.StatusForbidden, "caller is not the current worker for this task")
}

func harnessWrapperArtifactUploadAuthorized(task *corev1alpha1.Task) bool {
	if task == nil || task.Annotations == nil ||
		strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation]) == "" ||
		strings.TrimSpace(task.Annotations[harnessWrapperRuntimeAnnotation]) == "" {
		return false
	}
	if task.Status.Phase != "" && task.Status.Phase != corev1alpha1.TaskPhasePending && task.Status.Phase != corev1alpha1.TaskPhaseRunning {
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
