package service

import (
	"context"
	"strings"

	"github.com/distribution/reference"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/store"
)

type nativeJobIdentity struct {
	Name  string `json:"name"`
	UID   string `json:"uid"`
	Image string `json:"image"`
}

func pinnedAIWorkerImage(image string) bool {
	name, digest, found := strings.Cut(image, "@")
	if !found || name == "" || store.ValidateCanonicalDigest("image", digest) != nil {
		return false
	}
	_, err := reference.Parse(image)
	return err == nil
}

func (s *Coordinator) observeNativeJob(ctx context.Context, r *run, role string, task *corev1alpha1.Task) (bool, error) {
	op := r.state.Agents[role]
	if op == nil || op.UID != string(task.UID) {
		return false, uncertain()
	}
	if task.Status.JobName == "" || task.Status.JobUID == "" {
		if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded || op.Job != nil {
			return false, uncertain()
		}
		return false, nil
	}
	expected := nativeJobIdentity{
		Name: task.Status.JobName, UID: task.Status.JobUID, Image: r.state.Authority.AIWorkerImage,
	}
	if op.Job != nil && *op.Job != expected {
		return false, uncertain()
	}
	job := new(batchv1.Job)
	if err := s.config.Reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: expected.Name}, job); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, contract.ErrUnavailable
		}
		// TTL cleanup may remove a completed Job, but cannot substitute a new
		// image for the exact Job identity observed and durably retained earlier.
		if op.Job != nil {
			return true, nil
		}
		if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
			return false, uncertain()
		}
		return false, nil
	}
	owner := metav1.GetControllerOf(job)
	if string(job.UID) != expected.UID || owner == nil || owner.UID != task.UID ||
		owner.Name != task.Name || owner.Kind != taskKind || owner.APIVersion != corev1alpha1.GroupVersion.String() {
		return false, uncertain()
	}
	image, workers := "", 0
	for _, container := range job.Spec.Template.Spec.Containers {
		if container.Name == "worker" {
			image = container.Image
			workers++
		}
	}
	if workers != 1 || image != expected.Image || !pinnedAIWorkerImage(image) {
		return false, fail("agent-configuration-changed")
	}
	if op.Job == nil {
		op.Job = &expected
		return false, s.save(ctx, r)
	}
	return true, nil
}
