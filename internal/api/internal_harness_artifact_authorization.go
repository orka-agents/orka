package api

import (
	"context"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/artifactcap"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/store"
)

// Resolve Kubernetes authority before reserving the store writer. The returned
// database-only check revalidates the immutable attempt identity, active state,
// and capability before committing the artifact.
func (h *InternalHandlers) prepareHarnessV1ArtifactUpload(ctx context.Context, c fiber.Ctx, upload harness.ArtifactUpload) (func(context.Context) error, error) {
	denied := fiber.NewError(fiber.StatusForbidden, "harness artifact authorization failed")
	authorizer := h.internalCallerAuthorizer()
	if authorizer.k8sReader == nil {
		return nil, denied
	}
	task := &corev1alpha1.Task{}
	if err := authorizer.k8sReader.Get(ctx, types.NamespacedName{Namespace: upload.Namespace, Name: upload.TaskName}, task); err != nil {
		return nil, denied
	}
	binding := task.Status.AgentExecutionBinding
	if task.UID == "" || task.Spec.Type != corev1alpha1.TaskTypeAgent || !activeInternalWorkerTask(task) ||
		binding == nil || binding.SchemaVersion != 1 || binding.Task.UID != task.UID ||
		binding.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV1 ||
		binding.Backend != corev1alpha1.AgentExecutionBackendHarnessWrapper || task.Status.JobName != "" {
		return nil, denied
	}
	latest, err := h.latestHarnessV1ArtifactAttempt(ctx, task.Namespace, string(task.UID))
	if err != nil || !activeHarnessV1ArtifactAttempt(task, latest) {
		return nil, denied
	}
	if err := authorizer.verifyHarnessWrapperPod(ctx, GetUserInfo(c), latest); err != nil {
		return nil, denied
	}
	secret := &corev1.Secret{}
	if err := authorizer.k8sReader.Get(ctx, types.NamespacedName{Namespace: latest.AuthSecretNamespace, Name: latest.AuthSecretName}, secret); err != nil {
		return nil, denied
	}
	if string(secret.UID) != latest.AuthSecretUID || secret.ResourceVersion != latest.AuthSecretResourceVersion || !secret.DeletionTimestamp.IsZero() {
		return nil, denied
	}
	// Cancellation and deletion can start while workload and Secret checks run.
	// Recheck the live Task before reserving the writer; durable attempt state
	// and the cleanup generation still fence the database access itself.
	currentTask := &corev1alpha1.Task{}
	if err := authorizer.k8sReader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, currentTask); err != nil {
		return nil, denied
	}
	if currentTask.UID != task.UID || !activeInternalWorkerTask(currentTask) || currentTask.Status.JobName != "" ||
		currentTask.Status.AgentExecutionBinding == nil || !activeHarnessV1ArtifactAttempt(currentTask, latest) {
		return nil, denied
	}
	task = currentTask
	upload.TaskUID, upload.TurnID, upload.BindingDigest = string(task.UID), latest.TurnID, latest.BindingDigest
	capability := artifactcap.Authorization{
		Capability: c.Get(artifactcap.CapabilityHeader), RequestDigest: c.Get(artifactcap.RequestDigestHeader),
	}
	return func(txCtx context.Context) error {
		current, err := h.latestHarnessV1ArtifactAttempt(txCtx, task.Namespace, string(task.UID))
		if err != nil || current.Attempt != latest.Attempt || !activeHarnessV1ArtifactAttempt(task, current) {
			return denied
		}
		if err := harness.VerifyArtifactUpload(strings.TrimSpace(string(secret.Data[latest.AuthSecretKey])), capability, upload, time.Now().UTC()); err != nil {
			return denied
		}
		return nil
	}, nil
}

func (h *InternalHandlers) latestHarnessV1ArtifactAttempt(ctx context.Context, namespace, taskUID string) (*store.HarnessV1Attempt, error) {
	denied := fiber.NewError(fiber.StatusForbidden, "harness artifact authorization failed")
	attempts, ok := h.artifactStore.(interface {
		ListHarnessV1AttemptsByTask(context.Context, string, string) ([]store.HarnessV1Attempt, error)
	})
	if !ok {
		return nil, denied
	}
	listed, err := attempts.ListHarnessV1AttemptsByTask(ctx, namespace, taskUID)
	if err != nil || len(listed) == 0 {
		return nil, denied
	}
	latest := &listed[0]
	for i := range listed {
		if listed[i].Attempt > latest.Attempt {
			latest = &listed[i]
		}
	}
	return latest, nil
}

func activeHarnessV1ArtifactAttempt(task *corev1alpha1.Task, attempt *store.HarnessV1Attempt) bool {
	binding := task.Status.AgentExecutionBinding
	if attempt.Namespace != task.Namespace || attempt.TaskName != task.Name || attempt.TaskUID != string(task.UID) ||
		attempt.BindingDigest != binding.BindingDigest || attempt.SnapshotDigest != binding.Snapshot.Digest ||
		attempt.Backend != string(binding.Backend) || attempt.AuthSecretNamespace == "" || attempt.AuthSecretName == "" ||
		attempt.AuthSecretKey == "" || attempt.AuthSecretUID == "" || attempt.AuthSecretResourceVersion == "" {
		return false
	}
	switch attempt.State {
	case store.HarnessV1AttemptSubmitting, store.HarnessV1AttemptSubmittedUnknown, store.HarnessV1AttemptAccepted, store.HarnessV1AttemptRunning:
		return true
	default:
		return false
	}
}
