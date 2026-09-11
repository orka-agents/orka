package admission

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

// authorizedTaskCoordinationParent binds a worker-created coordination edge to
// the authenticated Pod's live Job and Task. ServiceAccount allowlisting alone
// does not establish which Task the worker is authorized to act for.
func (v *TaskProvenanceValidator) authorizedTaskCoordinationParent(
	ctx context.Context,
	req ctrladmission.Request,
	child *corev1alpha1.Task,
) (bool, error) {
	parentOwner := taskCoordinationOwner(child)
	if parentOwner == nil {
		// Label-only lineage (including cross-namespace delegation) grants no
		// coordination access: that requires a matching Task controller owner.
		return true, nil
	}
	parentName := labels.ParentTaskName(child.Labels, child.Annotations)
	if parentName == "" || parentOwner.Name != parentName || parentOwner.UID == "" || req.Namespace == "" || v.reader == nil {
		return false, nil
	}
	pod, err := v.coordinationCallerPod(ctx, req, parentName)
	if err != nil || pod == nil {
		return false, err
	}
	jobOwner := metav1.GetControllerOf(pod)
	if jobOwner == nil || jobOwner.APIVersion != batchv1.SchemeGroupVersion.String() || jobOwner.Kind != "Job" ||
		jobOwner.Name == "" || jobOwner.UID == "" {
		return false, nil
	}
	job := &batchv1.Job{}
	if err := v.reader.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: jobOwner.Name}, job); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if job.UID != jobOwner.UID || !job.DeletionTimestamp.IsZero() {
		return false, nil
	}
	taskOwner := metav1.GetControllerOf(job)
	if taskOwner == nil || taskOwner.APIVersion != corev1alpha1.GroupVersion.String() || taskOwner.Kind != "Task" ||
		taskOwner.Name != parentName || taskOwner.UID != parentOwner.UID {
		return false, nil
	}
	parent := &corev1alpha1.Task{}
	if err := v.reader.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: parentName}, parent); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if parent.UID != parentOwner.UID || parent.Status.JobName != job.Name || parent.Status.JobUID == "" || parent.Status.JobUID != string(job.UID) ||
		!parent.DeletionTimestamp.IsZero() || parent.Status.ExecutionOutcome != nil {
		return false, nil
	}
	switch parent.Status.Phase {
	case "", corev1alpha1.TaskPhasePending, corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskPhaseFinalizing:
		return taskCoordinationSessionAllowed(child, parent), nil
	default:
		return false, nil
	}
}

// A worker can pass on its own session authority, including the history
// cutoff, but cannot introduce an arbitrary session into its Task tree.
func taskCoordinationSessionAllowed(child, parent *corev1alpha1.Task) bool {
	return child.Spec.SessionRef == nil ||
		(parent.Spec.SessionRef != nil && *child.Spec.SessionRef == *parent.Spec.SessionRef)
}

func (v *TaskProvenanceValidator) coordinationCallerPod(
	ctx context.Context,
	req ctrladmission.Request,
	parentName string,
) (*corev1.Pod, error) {
	podNames := req.UserInfo.Extra["authentication.kubernetes.io/pod-name"]
	podUIDs := req.UserInfo.Extra["authentication.kubernetes.io/pod-uid"]
	if len(podNames) != 1 || podNames[0] == "" || len(podUIDs) != 1 || podUIDs[0] == "" {
		return nil, nil
	}
	pod := &corev1.Pod{}
	if err := v.reader.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: podNames[0]}, pod); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if string(pod.UID) != podUIDs[0] || !pod.DeletionTimestamp.IsZero() ||
		pod.Spec.ServiceAccountName == "" || req.UserInfo.Username != serviceAccountUsername(req.Namespace, pod.Spec.ServiceAccountName) ||
		pod.Labels[labels.LabelTask] != labels.SelectorValue(parentName) {
		return nil, nil
	}
	switch pod.Status.Phase {
	case "", corev1.PodPending, corev1.PodRunning:
		return pod, nil
	default:
		return nil, nil
	}
}
