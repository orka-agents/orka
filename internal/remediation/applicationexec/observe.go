package applicationexec

import (
	"context"
	"errors"
	"io"
	"reflect"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (b *Backend) observeJob(ctx context.Context, r *run, side, role string, job *batchv1.Job, imageID string, terminal bool, readFailure ...bool) (*corev1.Pod, pv.ApplicationContainerIdentity, bool, error) {
	pod, err := b.findJobPod(ctx, job)
	if err != nil {
		return nil, pv.ApplicationContainerIdentity{}, false, err
	}
	if pod == nil {
		_, previouslyObserved := r.state.Pods[resourceKey(side, role)]
		if job.Status.Failed > 0 || job.Status.Succeeded > 0 || previouslyObserved {
			return nil, pv.ApplicationContainerIdentity{}, false, contract.ErrConflict
		}
		return nil, pv.ApplicationContainerIdentity{}, false, nil
	}
	identity, err := b.bindObservedPod(ctx, r, side, role, job, pod, imageID)
	if err != nil {
		return nil, pv.ApplicationContainerIdentity{}, false, err
	}
	return b.observeContainer(ctx, r, job, pod, identity, terminal, len(readFailure) != 0 && readFailure[0])
}

func (b *Backend) findJobPod(ctx context.Context, job *batchv1.Job) (*corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := b.targetReader.List(ctx, pods, client.InNamespace(job.Namespace), client.Limit(64)); err != nil {
		return nil, safeError(contract.ErrUnavailable, err)
	}
	if pods.Continue != "" {
		return nil, ErrIsolation
	}
	var selected *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		owned, suspicious := false, pod.Labels["job-name"] == job.Name
		for _, owner := range pod.OwnerReferences {
			owned = owned || owner.UID == job.UID
			suspicious = suspicious || owner.Name == job.Name
		}
		if !owned {
			if suspicious {
				return nil, contract.ErrConflict
			}
			continue
		}
		if selected != nil {
			return nil, contract.ErrConflict
		}
		selected = pod
	}
	return selected, nil
}

func (b *Backend) bindObservedPod(ctx context.Context, r *run, side, role string, job *batchv1.Job,
	pod *corev1.Pod, imageID string) (pv.ApplicationContainerIdentity, error) {
	if pod.UID == "" || !ownedByJob(pod, job) || !pod.DeletionTimestamp.IsZero() ||
		(pod.Spec.NodeName != "" && pod.Spec.NodeName != r.state.Target.NodeName) ||
		pod.Labels[runLabel] != string(r.object.UID) ||
		!b.podExecutionMatches(job.Spec.Template.Spec, pod.Spec) ||
		!podPolicyMatches(job.Spec.Template.Spec, pod.Spec) || b.validatePod(pod.Spec) != nil {
		return pv.ApplicationContainerIdentity{}, contract.ErrConflict
	}
	sa := &corev1.ServiceAccount{}
	if err := b.targetReader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: pod.Spec.ServiceAccountName}, sa); err != nil {
		return pv.ApplicationContainerIdentity{}, safeError(contract.ErrUnavailable, err)
	}
	knownSA := false
	for _, entry := range r.state.Effects {
		if entry.Kind == serviceAccountKind && entry.Namespace == pod.Namespace && entry.Name == sa.Name && entry.UID == sa.UID && entry.UID != "" && !entry.Deleted {
			knownSA = true
		}
	}
	if !knownSA || !sa.DeletionTimestamp.IsZero() || ptr.Deref(sa.AutomountServiceAccountToken, true) ||
		len(sa.ImagePullSecrets) != 0 || len(sa.Secrets) != 0 {
		return pv.ApplicationContainerIdentity{}, ErrIsolation
	}
	identity := pv.ApplicationContainerIdentity{PodName: pod.Name, PodUID: string(pod.UID),
		ContainerName: job.Spec.Template.Spec.Containers[0].Name, ImageID: imageID, JobUID: string(job.UID),
		ServiceAccountName: sa.Name, ServiceAccountUID: string(sa.UID)}
	namespace := r.effect(resourceKey(side, "namespace"))
	if namespace == nil || namespace.Name != pod.Namespace || namespace.UID == "" {
		return identity, contract.ErrConflict
	}
	identity.Namespace, identity.NamespaceUID = namespace.Name, string(namespace.UID)
	if len(pod.Status.ContainerStatuses) == 1 {
		identity.ContainerID = pod.Status.ContainerStatuses[0].ContainerID
	}
	if err := b.rememberPod(ctx, r, resourceKey(side, role), podReceipt{Namespace: pod.Namespace, JobUID: job.UID, Identity: identity}); err != nil {
		return pv.ApplicationContainerIdentity{}, err
	}
	return identity, nil
}

func (b *Backend) observeContainer(ctx context.Context, r *run, job *batchv1.Job, pod *corev1.Pod,
	identity pv.ApplicationContainerIdentity, terminal, readFailure bool) (*corev1.Pod, pv.ApplicationContainerIdentity, bool, error) {
	if len(pod.Status.ContainerStatuses) != 1 {
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return nil, identity, false, contract.ErrUnavailable
		}
		return pod, identity, false, nil
	}
	status := pod.Status.ContainerStatuses[0]
	if status.Name != identity.ContainerName || status.RestartCount != 0 ||
		(status.ImageID != "" && imageDigest(status.ImageID) != imageDigest(identity.ImageID)) {
		return nil, identity, false, contract.ErrConflict
	}
	if status.State.Waiting != nil || status.ContainerID == "" || status.ImageID == "" {
		return pod, identity, false, nil
	}
	if pod.Spec.NodeName != r.state.Target.NodeName {
		return nil, identity, false, ErrIsolation
	}
	if err := b.recordVolumes(ctx, r, pod); err != nil {
		return nil, identity, false, err
	}
	if !b.validInstallerStatuses(job, pod) {
		return nil, identity, false, contract.ErrConflict
	}
	if status.State.Running != nil && job.Spec.Template.Spec.Containers[0].ReadinessProbe != nil && !status.Ready {
		return pod, identity, false, nil
	}
	ready, err := containerCompletion(pod, status, terminal, readFailure)
	if err != nil {
		return nil, identity, false, err
	}
	return pod, identity, ready, nil
}

func containerCompletion(pod *corev1.Pod, status corev1.ContainerStatus, terminal, readFailure bool) (bool, error) {
	if terminal {
		if status.State.Terminated == nil {
			return false, nil
		}
		if status.State.Terminated.ExitCode != 0 || pod.Status.Phase != corev1.PodSucceeded {
			return false, contract.ErrUnavailable
		}
	} else if readFailure && status.State.Terminated != nil {
		return true, nil
	} else if status.State.Running == nil || status.State.Terminated != nil || pod.Status.Phase != corev1.PodRunning {
		return false, contract.ErrUnavailable
	}
	return true, nil
}

func (b *Backend) validInstallerStatuses(job *batchv1.Job, pod *corev1.Pod) bool {
	if len(pod.Status.InitContainerStatuses) != len(job.Spec.Template.Spec.InitContainers) {
		return false
	}
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name != workerModeInstall || status.RestartCount != 0 || status.ContainerID == "" ||
			imageDigest(status.ImageID) != b.config.WorkerImageID || status.State.Terminated == nil || status.State.Terminated.ExitCode != 0 {
			return false
		}
	}
	return true
}

func ownedByJob(pod *corev1.Pod, job *batchv1.Job) bool {
	if len(pod.OwnerReferences) != 1 {
		return false
	}
	owner := pod.OwnerReferences[0]
	return owner.APIVersion == batchv1.SchemeGroupVersion.String() && owner.Kind == jobKind &&
		owner.Name == job.Name && owner.UID == job.UID && owner.Controller != nil && *owner.Controller
}

func readWorkerReport[T any](ctx context.Context, b *Backend, pod *corev1.Pod, limit int64, decode func(io.Reader) (T, error)) (T, error) {
	var zero T
	if pod == nil || pod.UID == "" || len(pod.Status.ContainerStatuses) != 1 ||
		pod.Status.ContainerStatuses[0].State.Terminated == nil ||
		pod.Status.ContainerStatuses[0].State.Terminated.ExitCode != 0 {
		return zero, contract.ErrConflict
	}
	stream, err := b.targetKube.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: pod.Status.ContainerStatuses[0].Name, LimitBytes: new(limit + 1)}).Stream(ctx)
	if err != nil {
		return zero, safeError(contract.ErrUnavailable, err)
	}
	report, decodeErr := decode(stream)
	closeErr := stream.Close()
	if decodeErr != nil || closeErr != nil {
		return zero, safeError(contract.ErrUnavailable, errors.Join(decodeErr, closeErr))
	}
	// Log URLs are name-addressed. A second exact API observation prevents a
	// replaced Pod/container from lending its bytes to the first UID receipt.
	current := &corev1.Pod{}
	if err := b.targetReader.Get(ctx, client.ObjectKeyFromObject(pod), current); err != nil {
		return zero, safeError(contract.ErrUnavailable, err)
	}
	if current.UID != pod.UID || !current.DeletionTimestamp.IsZero() ||
		!reflect.DeepEqual(current.OwnerReferences, pod.OwnerReferences) ||
		!reflect.DeepEqual(current.Spec, pod.Spec) ||
		!reflect.DeepEqual(current.Status.ContainerStatuses, pod.Status.ContainerStatuses) ||
		!reflect.DeepEqual(current.Status.InitContainerStatuses, pod.Status.InitContainerStatuses) {
		return zero, contract.ErrConflict
	}
	return report, nil
}
