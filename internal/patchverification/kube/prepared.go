package kube

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PreparedSubmissionID is deterministic for the exact accepted preparation Task.
// It does not authorize creation of another workload after that identity is lost.
func PreparedSubmissionID(taskUID, inputDigest string) string {
	return "vr-" + strings.TrimPrefix(pv.Digest([]byte(taskUID+"\x00"+inputDigest)), "sha256:")[:32]
}

func PreparedTaskNames(taskUID, inputDigest string) (original, patched string) {
	suffix := strings.TrimPrefix(PreparedSubmissionID(taskUID, inputDigest), "vr-")[:16]
	return "pv-" + suffix + "-original", "pv-" + suffix + "-patched"
}

// SubmitPrepared accepts only a supervisor-produced public-source manifest.
// Repeated handoff recovers the same durable submission, never another attempt.
func (s *Service) SubmitPrepared(ctx context.Context, task *corev1alpha1.Task, input pv.PreparationInput, report pv.PreparationReport) (*pv.KubernetesSubmission, error) {
	if s == nil || !s.config.Enabled || task == nil || task.UID == "" || task.Namespace != s.config.Namespace ||
		task.Namespace != input.Namespace || string(task.UID) != input.TaskUID || pv.ValidatePreparationReport(input, report) != nil ||
		s.config.PreparationConfiguration == "" || report.Manifest.Environment.Dependencies[pv.PreparationConfigurationKey] != s.config.PreparationConfiguration {
		return nil, pv.ErrEvidence
	}
	digest, err := pv.PreparationInputDigest(input)
	if err != nil || task.Annotations[pv.PreparationInputAnnotation] != digest ||
		report.Manifest.Environment.Image != s.config.ToolImage || report.Manifest.Environment.ImageID != s.config.ToolImageID ||
		report.Manifest.Environment.Profile != s.config.Profile || report.Manifest.Environment.Platform != s.config.Platform ||
		report.Manifest.Environment.Dependencies[helperImageKey] != s.config.HelperImage ||
		report.Manifest.Environment.Dependencies[helperImageIDKey] != s.config.HelperImageID {
		return nil, pv.ErrBinding
	}
	manifest := report.Manifest
	manifest.Environment.Dependencies = maps.Clone(manifest.Environment.Dependencies)
	maps.Copy(manifest.Environment.Dependencies, map[string]string{
		pv.PreparationTaskKey: task.Name, pv.PreparationUIDKey: string(task.UID), pv.PreparationDigestKey: digest,
		pv.PreparationParentTaskKey: input.ParentTaskName, pv.PreparationParentUIDKey: input.ParentTaskUID,
	})
	requestID := PreparedSubmissionID(string(task.UID), digest)
	suffix := strings.TrimPrefix(requestID, "vr-")[:16]
	originalName, patchedName := PreparedTaskNames(string(task.UID), digest)
	expected := pv.KubernetesSubmission{Namespace: task.Namespace, RequestID: requestID,
		SubmittedBy: string(task.UID), AttemptID: "prep-" + suffix, OriginalTaskName: originalName,
		Manifest: manifest, Provenance: report.Provenance, State: pv.SubmissionPreparing}
	if manifest.Action == pv.VerifyPatch {
		expected.PatchedTaskName = patchedName
	}
	if err := s.validatePreparationOwner(ctx, &expected); err != nil {
		return nil, err
	}
	existing, err := s.store.GetKubernetesValidationSubmission(ctx, task.Namespace, requestID)
	if errors.Is(err, pv.ErrRunNotFound) {
		createErr := s.store.CreateKubernetesValidationSubmission(ctx, expected)
		existing, err = s.store.GetKubernetesValidationSubmission(ctx, task.Namespace, requestID)
		if err != nil {
			return nil, errors.Join(createErr, err)
		}
	} else if err != nil {
		return nil, err
	}
	if existing.SubmittedBy != expected.SubmittedBy || existing.AttemptID != expected.AttemptID ||
		existing.OriginalTaskName != expected.OriginalTaskName || existing.PatchedTaskName != expected.PatchedTaskName ||
		!reflect.DeepEqual(existing.Manifest, expected.Manifest) || !reflect.DeepEqual(existing.Provenance, expected.Provenance) {
		return nil, pv.ErrBinding
	}
	return existing, nil
}

func preparedSubmission(submission *pv.KubernetesSubmission) bool {
	return submission.Manifest.Environment.Dependencies[pv.PreparationPolicyKey] == pv.PreparationPolicy
}

func preparationOwner(submission *pv.KubernetesSubmission) metav1.OwnerReference {
	deps := submission.Manifest.Environment.Dependencies
	return metav1.OwnerReference{APIVersion: corev1alpha1.GroupVersion.String(), Kind: validationTaskKind,
		Name: deps[pv.PreparationTaskKey], UID: types.UID(deps[pv.PreparationUIDKey]),
		Controller: new(true), BlockOwnerDeletion: new(true)}
}

func (s *Service) validatePreparationOwner(ctx context.Context, submission *pv.KubernetesSubmission) error {
	if s.config.PreparationConfiguration != "" && submission.Manifest.Environment.Dependencies[pv.PreparationConfigurationKey] != s.config.PreparationConfiguration {
		return pv.ErrBinding
	}
	if !preparedSubmission(submission) {
		return nil
	}
	deps := submission.Manifest.Environment.Dependencies
	task, parent := &corev1alpha1.Task{}, &corev1alpha1.Task{}
	if deps[pv.PreparationTaskKey] == "" || deps[pv.PreparationUIDKey] == "" || deps[pv.PreparationDigestKey] == "" ||
		deps[pv.PreparationParentTaskKey] == "" || deps[pv.PreparationParentUIDKey] == "" {
		return pv.ErrBinding
	}
	for _, lookup := range []struct {
		name string
		task *corev1alpha1.Task
	}{{deps[pv.PreparationTaskKey], task}, {deps[pv.PreparationParentTaskKey], parent}} {
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: submission.Namespace, Name: lookup.name}, lookup.task); err != nil {
			if apierrors.IsNotFound(err) {
				return pv.ErrBinding
			}
			return err
		}
	}
	fence := &corev1.ConfigMap{}
	err := s.reader.Get(ctx, client.ObjectKey{Namespace: submission.Namespace,
		Name: pv.PreparationCancellationFenceName(task.Name)}, fence)
	if err == nil {
		return pv.ErrBinding
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	if string(task.UID) != deps[pv.PreparationUIDKey] || string(parent.UID) != deps[pv.PreparationParentUIDKey] ||
		!task.DeletionTimestamp.IsZero() || !parent.DeletionTimestamp.IsZero() ||
		task.Annotations[pv.PreparationInputAnnotation] != deps[pv.PreparationDigestKey] ||
		task.Annotations[pv.PreparationCancelAnnotation] != "" || !metav1.IsControlledBy(task, parent) {
		return pv.ErrBinding
	}
	if task.CreationTimestamp.IsZero() || time.Since(task.CreationTimestamp.Time) > s.config.RunTimeout {
		return pv.ErrBinding
	}
	owner := metav1.GetControllerOf(task)
	if owner == nil || owner.Kind != validationTaskKind || owner.APIVersion != corev1alpha1.GroupVersion.String() || owner.Name != parent.Name {
		return pv.ErrBinding
	}
	return nil
}

func (s *Service) reservePreparedTask(ctx context.Context, submission *pv.KubernetesSubmission, name string) error {
	if !preparedSubmission(submission) {
		return nil
	}
	// The marker is written before admission. After an uncertain outcome or
	// deletion, absence is not permission to launch the same work again.
	marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name + "-intent", Namespace: submission.Namespace,
		OwnerReferences: []metav1.OwnerReference{preparationOwner(submission)}},
		Immutable: new(true), Data: map[string]string{"request": submission.RequestID}}
	return s.client.Create(ctx, marker)
}
