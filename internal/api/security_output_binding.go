package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

const (
	securityOutputCreatedBy            = "repository-security"
	harnessWrapperAttemptAnnotationAPI = "orka.ai/harness-wrapper-attempt"
)

func taskBoundOutputAttempt(task *corev1alpha1.Task) int64 {
	if task == nil {
		return 0
	}
	attempt := int64(task.Status.Attempts)
	if task.Status.Phase == corev1alpha1.TaskPhasePending || task.Status.Phase == corev1alpha1.TaskPhaseScheduled {
		attempt++
	}
	if task.Annotations != nil {
		if planned, err := strconv.ParseInt(strings.TrimSpace(task.Annotations[harnessWrapperAttemptAnnotationAPI]), 10, 64); err == nil &&
			planned > 0 && planned >= attempt {
			return planned
		}
	}
	return attempt
}

func (h *InternalHandlers) authorizeOutputWrite(c fiber.Ctx, kind, namespace, taskName string) (*store.OutputProvenance, error) {
	mode := h.integrityConfig.WorkerOutputBindingMode
	if mode == "" || mode == security.WorkerOutputBindingOff {
		return nil, h.legacyOutputWriteAuthorization(c, kind, namespace, taskName)
	}

	authorizer := h.internalCallerAuthorizer()
	if authorizer.k8sReader == nil {
		return nil, h.handleOutputBindingFailure(c, kind, mode, "reader_unavailable",
			fiber.NewError(fiber.StatusServiceUnavailable, "task output binding is unavailable"), namespace, taskName)
	}
	task := &corev1alpha1.Task{}
	if err := authorizer.k8sReader.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: taskName}, task); err != nil {
		reason := "task_lookup_failed"
		status := fiber.StatusServiceUnavailable
		if apierrors.IsNotFound(err) {
			reason = "task_not_found"
			status = fiber.StatusForbidden
		}
		return nil, h.handleOutputBindingFailure(c, kind, mode, reason,
			fiber.NewError(status, "caller is not the current worker for this task"), namespace, taskName)
	}
	if strings.TrimSpace(task.Labels[labels.LabelCreatedBy]) != securityOutputCreatedBy {
		return nil, h.legacyOutputWriteAuthorization(c, kind, namespace, taskName)
	}
	if kind == "artifact" {
		filename := strings.TrimSpace(c.Params("filename"))
		if !securityArtifactNameAllowedForStage(filename, securityTaskStage(task)) {
			bindingErr := fiber.NewError(fiber.StatusForbidden, "reserved security artifact is not writable by this stage")
			return nil, h.handleOutputBindingFailure(c, kind, mode, "reserved_artifact", bindingErr, namespace, taskName)
		}
	}

	provenance, err := h.verifySecurityTaskOutputWriter(c, authorizer, task)
	if err != nil {
		return nil, h.handleOutputBindingFailure(c, kind, mode, classifyOutputBindingError(err), err, namespace, taskName)
	}
	return provenance, nil
}

func securityTaskStage(task *corev1alpha1.Task) string {
	if task == nil {
		return ""
	}
	if stage := strings.TrimSpace(task.Labels[labels.LabelSecurityStage]); stage != "" {
		return stage
	}
	return strings.TrimSpace(task.Labels[labels.LabelSecurityMode])
}

func securityArtifactNameAllowedForStage(filename, stage string) bool {
	filename = strings.TrimSpace(filename)
	if filename == "" {
		return false
	}
	switch {
	case filename == security.ArtifactSlices:
		return stage == security.StageMapper
	case filename == security.ArtifactThreatModel:
		return stage == security.StageThreatModel
	case filename == security.ArtifactFindingsV2:
		return stage == security.StageReview
	case strings.HasPrefix(filename, "security-review-context-") && strings.HasSuffix(filename, ".json"):
		return stage == security.StageReview
	case filename == security.ArtifactValidation || filename == security.ArtifactValidationText:
		return stage == security.StageValidation
	case filename == security.ArtifactDroppedFindings:
		return false
	default:
		return true
	}
}

func (h *InternalHandlers) verifySecurityTaskOutputWriter(
	c fiber.Ctx,
	authorizer internalCallerAuthorizer,
	task *corev1alpha1.Task,
) (*store.OutputProvenance, error) {
	if task == nil {
		return nil, fiber.NewError(fiber.StatusForbidden, "target task not found")
	}
	userInfo := GetUserInfo(c)
	if userInfo != nil && serviceAccountNameFromUsername(userInfo.Username) == expectedHarnessWrapperServiceAccountName() {
		if err := authorizer.verifyHarnessWrapperArtifactUpload(c.Context(), userInfo, task.Namespace, task.Name); err != nil {
			return nil, err
		}
		if err := verifyHarnessWrapperPodIdentity(c.Context(), authorizer, userInfo); err != nil {
			return nil, err
		}
		if err := verifyHarnessWrapperOutputHeaders(c, task); err != nil {
			return nil, err
		}
	} else {
		if err := authorizer.verifyNamespace(c, task.Namespace); err != nil {
			return nil, err
		}
		if err := authorizer.verifyTaskWorker(c.Context(), userInfo, task); err != nil {
			return nil, err
		}
	}
	if !task.DeletionTimestamp.IsZero() {
		return nil, fiber.NewError(fiber.StatusGone, "task is deleting")
	}
	switch task.Status.Phase {
	case "", corev1alpha1.TaskPhasePending, corev1alpha1.TaskPhaseScheduled, corev1alpha1.TaskPhaseRunning:
	default:
		return nil, fiber.NewError(fiber.StatusConflict, "task is not accepting worker output")
	}
	provenance, err := outputProvenanceForWriter(c.Context(), authorizer, GetUserInfo(c), task)
	if err != nil {
		return nil, err
	}
	return provenance, nil
}

func outputProvenanceForWriter(
	ctx context.Context,
	authorizer internalCallerAuthorizer,
	userInfo *UserInfo,
	task *corev1alpha1.Task,
) (*store.OutputProvenance, error) {
	if task == nil || userInfo == nil {
		return nil, fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	podUID := firstUserExtra(userInfo, "authentication.kubernetes.io/pod-uid")
	producerKind := store.OutputProducerKubernetesWorker
	jobUID := ""
	runtimeSessionID, turnID, correlationID := "", "", ""
	if serviceAccountNameFromUsername(userInfo.Username) == expectedHarnessWrapperServiceAccountName() {
		producerKind = store.OutputProducerHarnessWrapper
		if task.Annotations != nil {
			runtimeSessionID = strings.TrimSpace(task.Annotations[harnessWrapperRuntimeAnnotation])
			turnID = strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation])
			correlationID = strings.TrimSpace(task.Annotations["orka.ai/harness-wrapper-correlation-id"])
		}
	} else {
		job := &batchv1.Job{}
		if err := authorizer.k8sReader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Status.JobName}, job); err != nil {
			return nil, fiber.NewError(fiber.StatusForbidden, "caller job not found")
		}
		jobUID = string(job.UID)
	}
	attempt := taskBoundOutputAttempt(task)
	bindingInput := strings.Join([]string{
		"output-writer-v1", string(task.UID), fmt.Sprint(attempt), jobUID, podUID,
		producerKind, runtimeSessionID, turnID, correlationID,
	}, "\x00")
	bindingDigest := sha256.Sum256([]byte(bindingInput))
	return &store.OutputProvenance{
		TaskUID:               string(task.UID),
		JobUID:                jobUID,
		PodUID:                podUID,
		TaskAttempt:           attempt,
		ProducerKind:          producerKind,
		RuntimeSessionID:      runtimeSessionID,
		TurnID:                turnID,
		CorrelationID:         correlationID,
		SubmissionNonceDigest: "sha256:" + hex.EncodeToString(bindingDigest[:]),
	}, nil
}

func verifyHarnessWrapperPodIdentity(
	ctx context.Context,
	authorizer internalCallerAuthorizer,
	userInfo *UserInfo,
) error {
	podName := firstUserExtra(userInfo, "authentication.kubernetes.io/pod-name")
	podUID := firstUserExtra(userInfo, "authentication.kubernetes.io/pod-uid")
	if podName == "" || podUID == "" {
		return fiber.NewError(fiber.StatusForbidden, "caller pod identity required")
	}
	namespace := strings.TrimSpace(userInfo.Namespace)
	if namespace == "" {
		namespace = parseServiceAccountNamespace(userInfo.Username)
	}
	pod := &corev1.Pod{}
	if err := authorizer.k8sReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err != nil {
		return fiber.NewError(fiber.StatusForbidden, "caller pod not found")
	}
	if string(pod.UID) != podUID || pod.Spec.ServiceAccountName != expectedHarnessWrapperServiceAccountName() {
		return fiber.NewError(fiber.StatusForbidden, "caller pod identity mismatch")
	}
	return nil
}

func verifyHarnessWrapperOutputHeaders(c fiber.Ctx, task *corev1alpha1.Task) error {
	for header, annotation := range map[string]string{
		"X-Orka-Runtime-Session-ID": harnessWrapperRuntimeAnnotation,
		"X-Orka-Turn-ID":            harnessWrapperTurnIDAnnotation,
		"X-Orka-Correlation-ID":     "orka.ai/harness-wrapper-correlation-id",
	} {
		expected := ""
		if task.Annotations != nil {
			expected = strings.TrimSpace(task.Annotations[annotation])
		}
		if expected == "" || strings.TrimSpace(c.Get(header)) != expected {
			return fiber.NewError(fiber.StatusForbidden, "harness wrapper output identity mismatch")
		}
	}
	return nil
}

func (h *InternalHandlers) legacyOutputWriteAuthorization(c fiber.Ctx, kind, namespace, taskName string) error {
	if kind == "artifact" {
		return h.internalCallerAuthorizer().verifyArtifactUploadCaller(c, namespace, taskName)
	}
	return h.internalCallerAuthorizer().verifyNamespace(c, namespace)
}

func (h *InternalHandlers) handleOutputBindingFailure(
	c fiber.Ctx,
	kind string,
	mode security.WorkerOutputBindingMode,
	reason string,
	bindingErr error,
	namespace string,
	taskName string,
) error {
	metrics.RecordSecurityOutputWrite(kind, string(mode), "denied", reason)
	if mode == security.WorkerOutputBindingAudit {
		if legacyErr := h.legacyOutputWriteAuthorization(c, kind, namespace, taskName); legacyErr != nil {
			return legacyErr
		}
		log.Info("repository security output write would be denied",
			"kind", kind,
			"reason", reason,
			"namespace", namespace,
			"task", taskName,
		)
		return nil
	}
	if mode != security.WorkerOutputBindingEnforce {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("unsupported output binding mode %q", mode))
	}
	return bindingErr
}

func classifyOutputBindingError(err error) string {
	if err == nil {
		return "ok"
	}
	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		switch fiberErr.Code {
		case fiber.StatusUnauthorized:
			return "authentication_required"
		case fiber.StatusGone:
			return "task_deleting"
		case fiber.StatusConflict:
			return "task_not_writable"
		case fiber.StatusServiceUnavailable, fiber.StatusInternalServerError:
			return "dependency_unavailable"
		}
	}
	message := strings.ToLower(err.Error())
	for _, item := range []struct {
		needle string
		reason string
	}{
		{"pod identity required", "pod_identity_missing"},
		{"pod identity mismatch", "pod_uid_mismatch"},
		{"pod not found", "pod_not_found"},
		{"caller job not found", "job_not_found"},
		{"does not belong to task", "wrong_task"},
		{"no active worker job", "no_active_attempt"},
		{"current worker", "wrong_attempt"},
		{"harness wrapper", "harness_binding_failed"},
		{"cross-namespace", "namespace_mismatch"},
	} {
		if strings.Contains(message, item.needle) {
			return item.reason
		}
	}
	return "binding_mismatch"
}

func (h *InternalHandlers) saveAuthorizedResult(
	ctx context.Context,
	namespace string,
	taskName string,
	data []byte,
	provenance *store.OutputProvenance,
) error {
	if provenance != nil {
		if bound, ok := h.resultStore.(store.BoundOutputStore); ok {
			if err := bound.SaveBoundResult(ctx, &store.BoundResult{
				Namespace: namespace, TaskName: taskName, Data: data, Provenance: *provenance,
			}); err != nil {
				if !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrDuplicateMismatch) {
					return securityOutputStoreError(err, "save bound result")
				}
				metrics.RecordSecurityOutputWrite("result", string(h.integrityConfig.WorkerOutputBindingMode),
					"accepted_legacy_unverified", "bound_store_conflict")
				if h.integrityConfig.WorkerOutputBindingMode == security.WorkerOutputBindingEnforce {
					return securityOutputStoreError(err, "save bound result")
				}
			} else {
				metrics.RecordSecurityOutputWrite("result", string(h.integrityConfig.WorkerOutputBindingMode), "allowed", "ok")
				return nil
			}
		}
		if h.integrityConfig.WorkerOutputBindingMode == security.WorkerOutputBindingEnforce {
			return fiber.NewError(fiber.StatusServiceUnavailable, "bound result storage is unavailable")
		}
	}
	if err := h.resultStore.SaveResult(ctx, namespace, taskName, data); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to save result: %v", err))
	}
	return nil
}

func (h *InternalHandlers) saveAuthorizedArtifact(
	ctx context.Context,
	namespace string,
	taskName string,
	filename string,
	contentType string,
	data []byte,
	provenance *store.OutputProvenance,
) error {
	if provenance != nil {
		if bound, ok := h.artifactStore.(store.BoundOutputStore); ok {
			if err := bound.SaveBoundArtifact(ctx, &store.BoundArtifact{
				Namespace: namespace, TaskName: taskName, Filename: filename, ContentType: contentType,
				Data: data, Provenance: *provenance,
			}); err != nil {
				if !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrDuplicateMismatch) {
					return securityOutputStoreError(err, "save bound artifact")
				}
				metrics.RecordSecurityOutputWrite("artifact", string(h.integrityConfig.WorkerOutputBindingMode),
					"accepted_legacy_unverified", "bound_store_conflict")
				if h.integrityConfig.WorkerOutputBindingMode == security.WorkerOutputBindingEnforce {
					return securityOutputStoreError(err, "save bound artifact")
				}
			} else {
				metrics.RecordSecurityOutputWrite("artifact", string(h.integrityConfig.WorkerOutputBindingMode), "allowed", "ok")
				return nil
			}
		}
		if h.integrityConfig.WorkerOutputBindingMode == security.WorkerOutputBindingEnforce {
			return fiber.NewError(fiber.StatusServiceUnavailable, "bound artifact storage is unavailable")
		}
	}
	if err := h.artifactStore.SaveArtifact(ctx, namespace, taskName, filename, contentType, data); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to save artifact: %v", err))
	}
	return nil
}

func securityOutputStoreError(err error, action string) error {
	switch {
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrDuplicateMismatch):
		return fiber.NewError(fiber.StatusConflict, err.Error())
	case errors.Is(err, store.ErrValidation):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	default:
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to %s: %v", action, err))
	}
}
