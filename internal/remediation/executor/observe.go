package executor

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	pvkube "github.com/orka-agents/orka/internal/patchverification/kube"
	"github.com/orka-agents/orka/internal/remediation/contract"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (b *Backend) observePreparation(ctx context.Context, input pv.PreparationInput, task *corev1alpha1.Task) (*pv.PreparationReport, bool, error) {
	job, err := b.boundPreparationJob(ctx, input, task)
	if err != nil {
		return nil, false, err
	}
	pods := &corev1.PodList{}
	if err := b.reader.List(ctx, pods, client.InNamespace(task.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return nil, false, safeError(contract.ErrUnavailable, err)
	}
	if len(pods.Items) > 1 {
		return nil, false, contract.ErrConflict
	}
	if len(pods.Items) == 0 {
		if job.Status.Failed > 0 || time.Since(job.CreationTimestamp.Time) > 150*time.Second {
			return nil, false, contract.ErrUnavailable
		}
		return nil, false, nil
	}
	pod := &pods.Items[0]
	if pod.UID == "" || !metav1.IsControlledBy(pod, job) ||
		!pvkube.PodExecutionMatches(job.Spec.Template.Spec, pod.Spec) || !pod.DeletionTimestamp.IsZero() {
		return nil, false, contract.ErrConflict
	}
	if len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].State.Terminated == nil {
		if pod.Status.Phase == corev1.PodFailed || time.Since(job.CreationTimestamp.Time) > 150*time.Second {
			return nil, false, contract.ErrUnavailable
		}
		return nil, false, nil
	}
	status := pod.Status.ContainerStatuses[0]
	if status.Name != preparationContainerName || status.RestartCount != 0 || status.ContainerID == "" ||
		status.State.Terminated.ExitCode != 0 || imageDigest(status.ImageID) != b.config.GeneralWorkerImageID {
		return nil, false, contract.ErrUnavailable
	}
	report, err := b.readReport(ctx, pod.Namespace, pod.Name)
	if err != nil {
		return nil, false, safeError(contract.ErrUnavailable, err)
	}
	if report.TaskUID != string(task.UID) || report.PodUID != string(pod.UID) || pv.ValidatePreparationReport(input, report) != nil {
		return nil, false, contract.ErrConflict
	}
	// Re-read the Pod after log transport so a name replacement cannot attach
	// another container's evidence to the previously observed UID.
	current := &corev1.Pod{}
	if err := b.reader.Get(ctx, client.ObjectKeyFromObject(pod), current); err != nil {
		return nil, false, safeError(contract.ErrUnavailable, err)
	}
	if current.UID != pod.UID || !reflect.DeepEqual(current.Status.ContainerStatuses, pod.Status.ContainerStatuses) ||
		!metav1.IsControlledBy(current, job) || !current.DeletionTimestamp.IsZero() ||
		!pvkube.PodExecutionMatches(job.Spec.Template.Spec, current.Spec) {
		return nil, false, contract.ErrConflict
	}
	return &report, true, nil
}

func (b *Backend) boundPreparationJob(ctx context.Context, input pv.PreparationInput, task *corev1alpha1.Task) (*batchv1.Job, error) {
	if task.Annotations[preparationJobUID] == "" {
		return nil, contract.ErrUnavailable
	}
	cm := &corev1.ConfigMap{}
	if err := b.reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: task.Name + "-input"}, cm); err != nil {
		return nil, safeError(contract.ErrUnavailable, err)
	}
	content, err := json.Marshal(input)
	if err != nil || !ptr.Deref(cm.Immutable, false) || !metav1.IsControlledBy(cm, task) || len(cm.Data) != 0 ||
		!reflect.DeepEqual(cm.BinaryData, map[string][]byte{"request.json": content}) {
		return nil, contract.ErrConflict
	}
	expected, err := b.buildPreparationJob(task)
	if err != nil {
		return nil, err
	}
	job := &batchv1.Job{}
	if err := b.reader.Get(ctx, client.ObjectKeyFromObject(expected), job); err != nil {
		return nil, safeError(contract.ErrUnavailable, err)
	}
	if string(job.UID) != task.Annotations[preparationJobUID] || !metav1.IsControlledBy(job, task) ||
		!pvkube.JobExecutionMatches(expected, job) || !job.DeletionTimestamp.IsZero() {
		return nil, contract.ErrConflict
	}
	return job, nil
}

func (b *Backend) readPodReport(ctx context.Context, namespace, name string) (pv.PreparationReport, error) {
	stream, err := b.kubeClient.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{
		Container: preparationContainerName, LimitBytes: ptr.To[int64](pv.MaxPreparationReportBytes + 1)}).Stream(ctx)
	if err != nil {
		return pv.PreparationReport{}, err
	}
	report, readErr := pv.DecodePreparationReport(stream)
	closeErr := stream.Close()
	if readErr != nil {
		return pv.PreparationReport{}, readErr
	}
	if closeErr != nil {
		return pv.PreparationReport{}, closeErr
	}
	return report, nil
}
