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
	if a.k8sClient == nil || userInfo == nil {
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
	if err := a.k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: taskName}, task); err != nil {
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
	if a.k8sClient == nil || userInfo == nil || task == nil {
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
	if err := a.k8sClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: podName}, pod); err != nil {
		return fiber.NewError(fiber.StatusForbidden, "caller pod not found")
	}
	if string(pod.UID) != podUID {
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
		if owner.Kind != "Job" || owner.Name == "" {
			continue
		}
		if owner.Name != currentJobName {
			continue
		}
		job := &batchv1.Job{}
		if err := a.k8sClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: owner.Name}, job); err != nil {
			return fiber.NewError(fiber.StatusForbidden, "caller job not found")
		}
		if owner.UID != "" && owner.UID != job.UID {
			continue
		}
		for _, jobOwner := range job.OwnerReferences {
			if jobOwner.Kind == "Task" && jobOwner.UID == task.UID {
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
