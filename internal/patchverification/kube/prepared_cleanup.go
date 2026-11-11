package kube

import (
	"context"
	"errors"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// VerifyPreparedCleanup proves that deleting the preparation Task did not hide
// active validation descendants. Retained terminal Tasks are metadata, not work.
func (s *Service) VerifyPreparedCleanup(ctx context.Context, namespace, taskName, taskUID, inputDigest string) (bool, error) {
	if s == nil || !s.config.Enabled || namespace != s.config.Namespace || taskUID == "" || inputDigest == "" {
		return false, pv.ErrBinding
	}
	if err := s.lockReconciliation(ctx); err != nil {
		return false, err
	}
	defer func() { <-s.reconcileGate }()
	requestID := PreparedSubmissionID(taskUID, inputDigest)
	submission, err := s.store.GetKubernetesValidationSubmission(ctx, namespace, requestID)
	if err != nil && !errors.Is(err, pv.ErrRunNotFound) {
		return false, err
	}
	owners := make(map[string]bool)
	if submission != nil {
		deps := submission.Manifest.Environment.Dependencies
		if submission.SubmittedBy != taskUID || deps[pv.PreparationTaskKey] != taskName ||
			deps[pv.PreparationUIDKey] != taskUID || deps[pv.PreparationDigestKey] != inputDigest {
			return false, pv.ErrBinding
		}
		if submission.State != pv.SubmissionTerminal {
			return false, nil
		}
		done, err := s.cleanupSubmission(ctx, submission, "validation was cancelled")
		if err != nil || !done {
			return done, err
		}
		done, err = s.verifyPreparedInputsGone(ctx, submission)
		if err != nil || !done {
			return done, err
		}
		for _, uid := range []string{submission.OriginalTaskUID, submission.PatchedTaskUID} {
			if uid != "" {
				owners[uid] = true
			}
		}
	}
	done, err := s.verifyPreparedTasks(ctx, namespace, taskName, taskUID, inputDigest, owners)
	if err != nil || !done {
		return done, err
	}
	if err := s.verifyNoPreparedJobs(ctx, namespace, requestID, owners); err != nil {
		return false, err
	}
	pods := &corev1.PodList{}
	if err := s.reader.List(ctx, pods, client.InNamespace(namespace),
		client.MatchingLabels{pv.ValidationTaskPrefix + "request": requestID}, client.Limit(1)); err != nil {
		return false, err
	}
	return len(pods.Items) == 0, nil
}

func (s *Service) verifyPreparedTasks(ctx context.Context, namespace, taskName, taskUID, inputDigest string, owners map[string]bool) (bool, error) {
	original, patched := PreparedTaskNames(taskUID, inputDigest)
	for _, name := range []string{original, patched} {
		task := &corev1alpha1.Task{}
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, task); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		owner := metav1.GetControllerOf(task)
		if !owners[string(task.UID)] || owner == nil || string(owner.UID) != taskUID ||
			owner.Kind != validationTaskKind || owner.Name != taskName || owner.APIVersion != corev1alpha1.GroupVersion.String() {
			return false, pv.ErrBinding
		}
		if (task.Status.Phase != corev1alpha1.TaskPhaseSucceeded && task.Status.Phase != corev1alpha1.TaskPhaseFailed &&
			task.Status.Phase != corev1alpha1.TaskPhaseCancelled) || controllerutil.ContainsFinalizer(task, validationFinalizer) {
			return false, nil
		}
	}
	return true, nil
}

func (s *Service) verifyNoPreparedJobs(ctx context.Context, namespace, requestID string, owners map[string]bool) error {
	token := ""
	for {
		jobs := &batchv1.JobList{}
		if err := s.reader.List(ctx, jobs, client.InNamespace(namespace), client.Limit(64), client.Continue(token)); err != nil {
			return err
		}
		for _, job := range jobs.Items {
			owner := metav1.GetControllerOf(&job)
			if job.Labels[pv.ValidationTaskPrefix+"request"] == requestID || (owner != nil && owners[string(owner.UID)]) {
				// Known dispatches were cleaned above. Anything remaining is
				// foreign, replaced, or a late admission, not absence proof.
				return pv.ErrBinding
			}
		}
		if jobs.Continue == "" {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		token = jobs.Continue
	}
	return nil
}

func (s *Service) verifyPreparedInputsGone(ctx context.Context, submission *pv.KubernetesSubmission) (bool, error) {
	if submission.RunID == "" {
		return true, nil
	}
	gone := true
	for _, side := range pv.ActionSides(submission.Manifest.Action) {
		uid := submission.OriginalTaskUID
		if side == pv.Patched {
			uid = submission.PatchedTaskUID
		}
		for _, check := range submission.Manifest.Checks {
			name := validationJobName(submission.RunID, side, check.ID)
			for _, object := range []client.Object{
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: submission.Namespace, Name: name + "-input"}},
				&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: submission.Namespace, Name: name}},
			} {
				err := s.reader.Get(ctx, client.ObjectKeyFromObject(object), object)
				if apierrors.IsNotFound(err) {
					continue
				}
				if err != nil {
					return false, err
				}
				owner := metav1.GetControllerOf(object)
				if uid == "" || owner == nil || string(owner.UID) != uid {
					return false, pv.ErrBinding
				}
				gone = false
			}
		}
	}
	return gone, nil
}
