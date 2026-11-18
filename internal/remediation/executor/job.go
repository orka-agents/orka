package executor

import (
	"context"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	preparationBackend       = "public-source-preparation"
	preparationJobUID        = pv.ValidationTaskPrefix + "preparation-job-uid"
	preparationFinalizer     = pv.ValidationTaskPrefix + "preparation-cleanup"
	preparationCleaned       = pv.ValidationTaskPrefix + "preparation-cleaned"
	preparationMarkerSet     = "true"
	preparationContainerName = "prepare"
	preparationWorkspace     = "/workspace"
)

func (b *Backend) preparationTask(input pv.PreparationInput, digest string) *corev1alpha1.Task {
	nameDigest := pv.Digest([]byte(input.Namespace + "\x00" + input.ID + "\x00" + input.ParentTaskUID + "\x00" + string(input.Manifest.Action)))
	return &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "rv-prep-" + strings.TrimPrefix(nameDigest, "sha256:")[:24], Namespace: input.Namespace,
		Finalizers:  []string{preparationFinalizer},
		Annotations: map[string]string{pv.ValidationBackendKey: preparationBackend, pv.PreparationInputAnnotation: digest},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: input.ParentTaskName,
			UID: types.UID(input.ParentTaskUID), Controller: new(true), BlockOwnerDeletion: new(true),
		}},
	}, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer, Image: b.config.GeneralWorkerImage,
		Command: []string{"/bin/sh", "-c"}, Args: []string{"exit 125"},
		Priority: ptr.To[int32](500), ConcurrencyPolicy: corev1alpha1.ForbidConcurrent,
		StartingDeadlineSeconds: ptr.To[int64](100), SuccessfulRunsHistoryLimit: ptr.To[int32](3), FailedRunsHistoryLimit: ptr.To[int32](1),
		RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 0, BackoffMultiplier: 2}}}
}

func (b *Backend) buildPreparationJob(task *corev1alpha1.Task) (*batchv1.Job, error) {
	nodeSelector := map[string]string{"kubernetes.io/os": "linux",
		"kubernetes.io/arch":     strings.TrimPrefix(b.config.Platform, "linux/"),
		b.config.NodeSelectorKey: b.config.NodeSelectorValue}
	mode := int32(0440)
	workLimit := resource.MustParse("256Mi")
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: task.Name, Namespace: task.Namespace,
		Labels: map[string]string{pv.ValidationTaskPrefix + "preparation": string(task.UID)}},
		Spec: batchv1.JobSpec{BackoffLimit: ptr.To[int32](0), ActiveDeadlineSeconds: new(min(int64(120), int64(b.config.RunTimeout/time.Second))),
			PodReplacementPolicy: new(batchv1.Failed),
			Completions:          ptr.To[int32](1), Parallelism: ptr.To[int32](1),
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				pv.ValidationTaskPrefix + "preparation": string(task.UID)}},
				Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: new(false),
					EnableServiceLinks: new(false), TerminationGracePeriodSeconds: ptr.To[int64](5),
					NodeSelector: nodeSelector,
					SecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1000), RunAsGroup: ptr.To[int64](1000),
						FSGroup: ptr.To[int64](1000), RunAsNonRoot: new(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Volumes: []corev1.Volume{
						{Name: "input", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: task.Name + "-input"}, DefaultMode: &mode}}},
						{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium: corev1.StorageMediumMemory, SizeLimit: &workLimit}}},
					},
					Containers: []corev1.Container{{Name: preparationContainerName, Image: b.config.GeneralWorkerImage, ImagePullPolicy: corev1.PullIfNotPresent,
						Command: []string{"/worker", "--prepare-validation-source", "/input/request.json"},
						Env: []corev1.EnvVar{
							{Name: "ORKA_VALIDATION_TASK_UID", Value: string(task.UID)},
							{Name: "ORKA_VALIDATION_POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.uid"}}},
							{Name: "HOME", Value: preparationWorkspace}, {Name: "TMPDIR", Value: preparationWorkspace},
							{Name: "GOMAXPROCS", Value: "1"}, {Name: "GOMEMLIMIT", Value: "160MiB"},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("256Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("256Mi")}},
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: new(false), ReadOnlyRootFilesystem: new(true),
							RunAsUser: ptr.To[int64](1000), RunAsGroup: ptr.To[int64](1000), RunAsNonRoot: new(true),
							Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
						VolumeMounts: []corev1.VolumeMount{{Name: "input", MountPath: "/input", ReadOnly: true}, {Name: "workspace", MountPath: preparationWorkspace}},
					}},
				}},
		}}
	if err := controllerutil.SetControllerReference(task, job, b.scheme); err != nil {
		return nil, safeError(contract.ErrInvalid, err)
	}
	return job, nil
}

func (b *Backend) BuildValidationJob(ctx context.Context, task *corev1alpha1.Task) (*batchv1.Job, error) {
	if b == nil || !b.config.Enabled {
		return nil, contract.ErrDisabled
	}
	if task.Annotations[pv.ValidationBackendKey] == preparationBackend {
		// Preparation admission belongs to Start, after its persistence callback.
		return nil, contract.ErrForbidden
	}
	return b.validation.BuildValidationJob(ctx, task)
}

func (b *Backend) ReconcileValidationTask(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, bool, error) {
	if task.Annotations[pv.ValidationBackendKey] != preparationBackend {
		if b != nil && b.validation != nil {
			return b.validation.ReconcileValidationTask(ctx, task)
		}
		for key := range task.Annotations {
			if strings.HasPrefix(key, pv.ValidationTaskPrefix) {
				return ctrl.Result{}, true, contract.ErrDisabled
			}
		}
		return ctrl.Result{}, false, nil
	}
	if b == nil || !b.config.Enabled {
		return ctrl.Result{}, true, contract.ErrDisabled
	}
	if !task.DeletionTimestamp.IsZero() || task.Status.Phase == corev1alpha1.TaskPhaseCancelled {
		err := b.Cancel(ctx, contract.ExecutionReceipt{Namespace: task.Namespace, TaskName: task.Name,
			TaskUID: string(task.UID), InputDigest: task.Annotations[pv.PreparationInputAnnotation]})
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{}, true, nil
}
