/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/transactiontoken"
)

const (
	contextTokenCredentialLocalKey = "contextTokenCredential"
	directTaskTokenOwnerKind       = "Task"
)

func contextTokenCredential(c fiber.Ctx) string {
	credential, _ := c.Locals(contextTokenCredentialLocalKey).(string)
	return strings.TrimSpace(credential)
}

func (h *Handlers) prepareDirectTaskTransactionToken(c fiber.Ctx, task *corev1alpha1.Task) (string, error) {
	if h == nil || task == nil || h.contextTokenAuthorization.Mode != ContextTokenAuthorizationModeEnforce ||
		task.Spec.Transaction == nil ||
		(task.Spec.Type != corev1alpha1.TaskTypeAI && task.Spec.Type != corev1alpha1.TaskTypeAgent) {
		return "", nil
	}
	if strings.TrimSpace(task.Spec.Schedule) != "" {
		return "", fiber.NewError(fiber.StatusUnprocessableEntity,
			"scheduled transactional AI and agent tasks require per-run task token provisioning")
	}
	if !h.contextTokenTTS.Enabled() {
		return "", fiber.NewError(fiber.StatusServiceUnavailable,
			"direct transactional task creation requires context-token TTS")
	}
	if h.contextTokenTTS.TokenSource != ContextTokenTTSTokenSourceIncoming {
		return "", fiber.NewError(fiber.StatusServiceUnavailable,
			"direct transactional task creation requires context-token TTS tokenSource=incoming")
	}
	if task.Spec.Type == corev1alpha1.TaskTypeAI &&
		h.contextTokenTTS.ChildTokenTTL < transactiontoken.MinimumProjectedTokenRequestedTTL {
		return "", fiber.NewError(fiber.StatusServiceUnavailable,
			"direct transactional AI task creation requires a propagation-safe context-token TTS child TTL")
	}
	credential := contextTokenCredential(c)
	if credential == "" {
		return "", fiber.NewError(fiber.StatusServiceUnavailable,
			"task-scoped transaction token provisioning is unavailable")
	}
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[labels.AnnotationTransactionTokenPending] = queryTrue
	task.Annotations[labels.AnnotationTransactionTokenPendingSince] = time.Now().UTC().Format(time.RFC3339Nano)
	task.Annotations[labels.AnnotationTransactionTokenSecret] = "orka-task-txn-" + uuid.NewString()
	return credential, nil
}

func (h *Handlers) persistDirectTaskTransactionTokenSubject(
	ctx context.Context,
	task *corev1alpha1.Task,
	subjectToken string,
) error {
	if h == nil || h.client == nil || task == nil || strings.TrimSpace(subjectToken) == "" {
		return errors.New("direct task transaction token setup is incomplete")
	}
	if task.UID == "" {
		return errors.New("created task UID is required for transaction token setup")
	}
	workloadSecretName := strings.TrimSpace(task.Annotations[labels.AnnotationTransactionTokenSecret])
	if workloadSecretName == "" {
		return errors.New("created task is missing its transaction token Secret reference")
	}
	ownerReferences := []metav1.OwnerReference{{
		APIVersion: corev1alpha1.GroupVersion.String(), Kind: directTaskTokenOwnerKind,
		Name: task.Name, UID: task.UID,
	}}
	taskUIDLabel := labels.SelectorValue(string(task.UID))
	requestDetails := map[string]any{
		"operation": "createTask",
		"namespace": task.Namespace,
		"taskName":  task.Name,
		"taskUID":   string(task.UID),
	}
	if transactionID := strings.TrimSpace(task.Spec.Transaction.ID); transactionID != "" {
		requestDetails["txn"] = transactionID
	}
	encodedRequestDetails, err := json.Marshal(requestDetails)
	if err != nil {
		return fmt.Errorf("encoding task transaction token request details: %w", err)
	}
	authoritySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "orka-task-authority-" + uuid.NewString(),
			Namespace: task.Namespace,
			Labels: map[string]string{
				labels.LabelPurpose: transactiontoken.AuthoritySecretPurpose,
				labels.LabelTaskUID: taskUIDLabel,
			},
			OwnerReferences: ownerReferences,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			transactiontoken.SubjectSecretKey:          []byte(subjectToken),
			transactiontoken.SubjectTokenTypeSecretKey: []byte(transactiontoken.SubjectTokenTypeTransactionToken),
			transactiontoken.RequestDetailsSecretKey:   encodedRequestDetails,
		},
	}
	if err := h.client.Create(ctx, authoritySecret); err != nil {
		return fmt.Errorf("creating task transaction renewal authority: %w", err)
	}
	workloadSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workloadSecretName,
			Namespace: task.Namespace,
			Labels: map[string]string{
				labels.LabelTask:    labels.SelectorValue(task.Name),
				labels.LabelTaskUID: taskUIDLabel,
				labels.LabelPurpose: transactiontoken.WorkloadSecretPurpose,
			},
			OwnerReferences: ownerReferences,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{},
	}
	if err := h.client.Create(ctx, workloadSecret); err != nil {
		if cleanupErr := h.client.Delete(ctx, authoritySecret); cleanupErr != nil && !apierrors.IsNotFound(cleanupErr) {
			return errors.Join(
				fmt.Errorf("creating task transaction token workload Secret: %w", err),
				fmt.Errorf("cleaning task transaction renewal authority: %w", cleanupErr),
			)
		}
		return fmt.Errorf("creating task transaction token workload Secret: %w", err)
	}
	return nil
}

func (h *Handlers) cleanupTaskAfterTransactionTokenSetupFailure(ctx context.Context, task *corev1alpha1.Task) error {
	if h == nil || h.client == nil || task == nil || task.Name == "" {
		return nil
	}
	if task.UID == "" {
		return errors.New("created task UID is required for transaction token cleanup")
	}
	uid := task.UID
	if err := h.client.Delete(ctx, &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: task.Name, Namespace: task.Namespace,
	}}, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting task after transaction token setup failure: %w", err)
	}
	return nil
}
