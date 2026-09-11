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

// Called inside the artifact store transaction, after the request body is
// complete. The capability is checked against the latest durable v1 attempt,
// not Task annotations, a ServiceAccount name, or caller-supplied turn metadata.
func (h *InternalHandlers) verifyHarnessV1ArtifactUpload(ctx context.Context, c fiber.Ctx, upload harness.ArtifactUpload) error {
	denied := fiber.NewError(fiber.StatusForbidden, "harness artifact authorization failed")
	authorizer := h.internalCallerAuthorizer()
	if authorizer.k8sReader == nil {
		return denied
	}
	task := &corev1alpha1.Task{}
	if err := authorizer.k8sReader.Get(ctx, types.NamespacedName{Namespace: upload.Namespace, Name: upload.TaskName}, task); err != nil {
		return denied
	}
	binding := task.Status.AgentExecutionBinding
	if task.UID == "" || task.Spec.Type != corev1alpha1.TaskTypeAgent || !activeInternalWorkerTask(task) ||
		binding == nil || binding.SchemaVersion != 1 || binding.Task.UID != task.UID ||
		binding.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV1 ||
		binding.Backend != corev1alpha1.AgentExecutionBackendHarnessWrapper || task.Status.JobName != "" {
		return denied
	}
	attempts, ok := h.artifactStore.(interface {
		ListHarnessV1AttemptsByTask(context.Context, string, string) ([]store.HarnessV1Attempt, error)
	})
	if !ok {
		return denied
	}
	listed, err := attempts.ListHarnessV1AttemptsByTask(ctx, task.Namespace, string(task.UID))
	if err != nil || len(listed) == 0 {
		return denied
	}
	latest := &listed[0]
	for i := range listed {
		if listed[i].Attempt > latest.Attempt {
			latest = &listed[i]
		}
	}
	if !activeHarnessV1ArtifactAttempt(task, latest) {
		return denied
	}
	if _, err := authorizer.resolveCallerPod(ctx, GetUserInfo(c), latest.AuthSecretNamespace); err != nil {
		return denied
	}
	secret := &corev1.Secret{}
	if err := authorizer.k8sReader.Get(ctx, types.NamespacedName{Namespace: latest.AuthSecretNamespace, Name: latest.AuthSecretName}, secret); err != nil {
		return denied
	}
	if string(secret.UID) != latest.AuthSecretUID || secret.ResourceVersion != latest.AuthSecretResourceVersion || !secret.DeletionTimestamp.IsZero() {
		return denied
	}
	upload.TaskUID, upload.TurnID, upload.BindingDigest = string(task.UID), latest.TurnID, latest.BindingDigest
	if err := harness.VerifyArtifactUpload(strings.TrimSpace(string(secret.Data[latest.AuthSecretKey])), artifactcap.Authorization{
		Capability: c.Get(artifactcap.CapabilityHeader), RequestDigest: c.Get(artifactcap.RequestDigestHeader),
	}, upload, time.Now().UTC()); err != nil {
		return denied
	}
	return nil
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
