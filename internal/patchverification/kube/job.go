package kube

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
)

const (
	validationContainerName = "validation"
	inputVolumeName         = "input"
	sourceVolumeName        = "src"
	runnerVolumeName        = "runner"
	workVolumeName          = "work"
	tmpVolumeName           = "tmp"
)

func (s *Service) BuildValidationJob(ctx context.Context, task *corev1alpha1.Task) (*batchv1.Job, error) {
	submission, err := s.store.GetKubernetesValidationSubmissionByTask(ctx, task.Namespace, task.Name)
	if err != nil || submission.Binding == nil {
		return nil, fmt.Errorf("validation Task is not durably bound")
	}
	if err := s.validateTask(submission, task); err != nil {
		return nil, err
	}
	side := task.Annotations[pv.ValidationSideKey]
	var check pv.Check
	found := false
	for _, candidate := range submission.Manifest.Checks {
		_, lookupErr := s.store.GetKubernetesValidationDispatch(ctx, submission.RunID, side, candidate.ID)
		if lookupErr == nil {
			continue
		}
		if !errors.Is(lookupErr, pv.ErrRunNotFound) {
			return nil, lookupErr
		}
		check, found = candidate, true
		break
	}
	if !found {
		return nil, fmt.Errorf("validation Task has no undispatched check")
	}
	return s.buildCheckJob(submission, task, check)
}

func (s *Service) buildCheckJob(submission *pv.KubernetesSubmission, task *corev1alpha1.Task, check pv.Check) (*batchv1.Job, error) {
	side := task.Annotations[pv.ValidationSideKey]
	jobName := validationJobName(submission.RunID, side, check.ID)
	deadline := int64(check.TimeoutSeconds + 120)
	nodeSelector := map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/arch": strings.TrimPrefix(submission.Manifest.Environment.Platform, "linux/")}
	nodeSelector[s.config.NodeSelectorKey] = s.config.NodeSelectorValue
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: task.Namespace,
		Labels: map[string]string{"patchverification.orka.ai/request": submission.RequestID, "patchverification.orka.ai/side": side}},
		Spec: batchv1.JobSpec{BackoffLimit: ptr.To[int32](0), ActiveDeadlineSeconds: &deadline, Completions: ptr.To[int32](1), Parallelism: ptr.To[int32](1),
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				"patchverification.orka.ai/request": submission.RequestID, "patchverification.orka.ai/job": jobName}}, Spec: corev1.PodSpec{
				AutomountServiceAccountToken: new(false), RestartPolicy: corev1.RestartPolicyNever,
				EnableServiceLinks: new(false), TerminationGracePeriodSeconds: ptr.To[int64](10),
				NodeSelector:    nodeSelector,
				SecurityContext: &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
				Volumes:         validationVolumes(jobName),
				InitContainers: []corev1.Container{{Name: "install-runner", Image: submission.Manifest.Environment.Dependencies[helperImageKey], ImagePullPolicy: corev1.PullIfNotPresent,
					Command: []string{"/bin/sh", "-ceu"}, Args: []string{`cp /artifacts/orka-validation-worker /runner/orka-validation-worker
cp /artifacts/orka-validation-guard /runner/orka-validation-guard
cp /bundle/bundle.gz /input/bundle.gz
chmod 0555 /runner/orka-validation-worker /runner/orka-validation-guard
chmod 0500 /runner
chmod 0700 /input
chmod 0400 /input/bundle.gz`},
					SecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To[int64](0), RunAsGroup: ptr.To[int64](0), AllowPrivilegeEscalation: new(false), ReadOnlyRootFilesystem: new(true),
						Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					VolumeMounts: []corev1.VolumeMount{{Name: runnerVolumeName, MountPath: "/runner"}, {Name: inputVolumeName, MountPath: "/input"}, {Name: "bundle", MountPath: "/bundle", ReadOnly: true}}}},
				Containers: []corev1.Container{{Name: validationContainerName, Image: submission.Manifest.Environment.Image, ImagePullPolicy: corev1.PullIfNotPresent,
					Command: []string{"/runner/orka-validation-worker", "--input", "/input/bundle.gz"},
					Env:     validationIdentityEnv(string(task.UID)), Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")}, Limits: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")}},
					SecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To[int64](0), RunAsGroup: ptr.To[int64](0),
						AllowPrivilegeEscalation: new(false), ReadOnlyRootFilesystem: new(true),
						Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"SETUID", "SETGID", "CHOWN", "KILL", "FOWNER"}}},
					VolumeMounts: validationMounts()}},
			}}}}
	if preparedSubmission(submission) {
		*job.Spec.ActiveDeadlineSeconds = min(*job.Spec.ActiveDeadlineSeconds, max(int64(1), int64(s.config.RunTimeout/time.Second)))
		job.Spec.PodReplacementPolicy = new(batchv1.Failed)
		pod := &job.Spec.Template.Spec
		for index := range pod.Volumes {
			if pod.Volumes[index].EmptyDir != nil {
				pod.Volumes[index].EmptyDir.Medium = corev1.StorageMediumMemory
			}
		}
		pod.InitContainers[0].Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		}
		for _, container := range []*corev1.Container{&pod.InitContainers[0], &pod.Containers[0]} {
			container.Resources.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse("256Mi")
			container.Resources.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse("256Mi")
		}
		pod.Containers[0].Env = append(pod.Containers[0].Env, corev1.EnvVar{Name: "GOMEMLIMIT", Value: "160MiB"})
	}
	if err := s.OwnJob(task, job); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Service) validationInputBundle(submission *pv.KubernetesSubmission, side, checkID string) ([]byte, error) {
	archiveID := submission.Manifest.Sources.Original.ArchiveDigest
	if side == pv.Patched {
		archiveID = submission.Manifest.Sources.Patched.ArchiveDigest
	}
	input := pv.PodInput{Version: pv.PodProtocolVersion, Manifest: submission.Manifest, Binding: *submission.Binding,
		Side: side, CheckID: checkID, Archive: submission.Provenance[archiveID]}
	if s.config.Profile == pv.LocalServices {
		input.CanaryHost, input.CanaryPort = s.config.CanaryHost, s.config.CanaryPort
	}
	bundle, err := gzipJSON(input)
	if err != nil || len(bundle) > pv.MaxPodInputBytes {
		return nil, fmt.Errorf("validation pod input exceeds its compressed limit")
	}
	return bundle, nil
}

func gzipJSON(input pv.PodInput) ([]byte, error) {
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	encoder := json.NewEncoder(writer)
	if err := encoder.Encode(input); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func validationJobName(runID, side, checkID string) string {
	digest := pv.Digest([]byte(runID + "\x00" + side + "\x00" + checkID))
	raw, _ := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	return fmt.Sprintf("pv-%s-%s-%x", side[:1], strings.TrimPrefix(runID, "pv-")[:12], raw[:6])
}

func validationVolumes(jobName string) []corev1.Volume {
	mode := int32(0440)
	volumes := make([]corev1.Volume, 0, 7)
	volumes = append(volumes, corev1.Volume{Name: "bundle", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: jobName + "-input"}, DefaultMode: &mode}}})
	for _, name := range []string{runnerVolumeName, inputVolumeName, sourceVolumeName, "checks", workVolumeName, tmpVolumeName} {
		limit := resource.MustParse("64Mi")
		if name == sourceVolumeName || name == workVolumeName || name == tmpVolumeName {
			limit = resource.MustParse("256Mi")
		}
		volumes = append(volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &limit}}})
	}
	return volumes
}

func validationMounts() []corev1.VolumeMount {
	mounts := make([]corev1.VolumeMount, 0, 6)
	for _, name := range []string{runnerVolumeName, inputVolumeName, sourceVolumeName, "checks", workVolumeName, tmpVolumeName} {
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: "/" + name, ReadOnly: name == runnerVolumeName || name == inputVolumeName})
	}
	return mounts
}

func validationIdentityEnv(taskUID string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "ORKA_VALIDATION_TASK_UID", Value: taskUID},
		{Name: "ORKA_VALIDATION_POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.uid"}}},
	}
}
