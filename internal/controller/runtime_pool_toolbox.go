package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp/toolbox"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	runtimePoolToolboxesEnv       = "ORKA_ACP_TOOLBOXES"
	runtimePoolToolboxMountMethod = "ORKA_ACP_TOOLBOX_MOUNT_METHOD"

	runtimePoolToolboxHandoffVolume    = "toolbox-handoff"
	runtimePoolToolboxHandoffMountPath = "/handoff"
	runtimePoolToolboxOutputMountPath  = "/out"
	runtimePoolToolboxVolumePrefix     = "toolbox-"
	runtimePoolToolboxCopyPrefix       = "toolbox-copy-"
	runtimePoolToolboxHandoffContainer = "toolbox-handoff"
	runtimePoolToolboxDstFlag          = "--dst"

	runtimePoolToolboxHandoffUID int64 = 64020
	runtimePoolToolboxCopyUID    int64 = 64021

	runtimePoolDropAllCapabilities corev1.Capability = "ALL"

	podWaitingReasonErrImagePull          = "ErrImagePull"
	podWaitingReasonImagePullBackOff      = "ImagePullBackOff"
	podWaitingReasonRegistryUnavailable   = "RegistryUnavailable"
	podWaitingReasonInvalidImageName      = "InvalidImageName"
	podWaitingReasonCreateContainer       = "CreateContainerError"
	podWaitingReasonCreateContainerConfig = "CreateContainerConfigError"
	podWaitingReasonRunContainer          = "RunContainerError"
)

const (
	runtimePoolToolboxVolumeHeadroomBytes int64 = 64 << 20
	runtimePoolToolboxBlockBytes          int64 = 4096
	runtimePoolToolboxVolumeBytes               = toolbox.DefaultMaxTotalBytes + toolbox.DefaultMaxEntries*runtimePoolToolboxBlockBytes + runtimePoolToolboxVolumeHeadroomBytes
	runtimePoolToolboxHandoffBytes        int64 = 64 << 20
)

var (
	runtimePoolToolboxHandoffSizeLimit = resource.MustParse("64Mi")
	runtimePoolToolboxVolumeSizeLimit  = resource.MustParse(strconv.FormatInt(runtimePoolToolboxVolumeBytes, 10))
)

func runtimePoolToolboxVolumeName(index int) string {
	return runtimePoolToolboxVolumePrefix + strconv.Itoa(index)
}

func runtimePoolToolboxCopyContainerName(index int) string {
	return runtimePoolToolboxCopyPrefix + strconv.Itoa(index)
}

func runtimePoolToolboxesJSON(toolboxes []harnessv2.RuntimeToolbox) (string, error) {
	data, err := json.Marshal(toolboxes)
	if err != nil {
		return "", fmt.Errorf("marshal runtime toolboxes: %w", err)
	}
	return string(data), nil
}

func runtimePoolToolboxesFromEnvironment(environment map[string]string) ([]harnessv2.RuntimeToolbox, error) {
	raw := strings.TrimSpace(environment[runtimePoolToolboxesEnv])
	if raw == "" {
		return nil, nil
	}
	var toolboxes []harnessv2.RuntimeToolbox
	if err := json.Unmarshal([]byte(raw), &toolboxes); err != nil {
		return nil, err
	}
	if len(toolboxes) == 0 {
		return nil, nil
	}
	if err := harnessv2.ValidateRuntimeToolboxes(toolboxes); err != nil {
		return nil, err
	}
	return toolboxes, nil
}

func runtimePoolToolboxInitSecurityContext(uid int64) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser:                &uid,
		RunAsGroup:               &uid,
		RunAsNonRoot:             new(true),
		AllowPrivilegeEscalation: new(false),
		ReadOnlyRootFilesystem:   new(true),
		Privileged:               new(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{runtimePoolDropAllCapabilities}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func runtimePoolToolboxInitResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
}

func applyRuntimePoolToolboxTemplate(template *corev1.PodTemplateSpec, toolboxes []harnessv2.RuntimeToolbox, policy ACPToolboxPolicy) error {
	if template == nil || len(toolboxes) == 0 {
		return nil
	}
	if len(template.Spec.Containers) != 1 {
		return fmt.Errorf("runtime Pod template must have exactly one container")
	}
	method, err := ParseACPToolboxMountMethod(string(policy.MountMethod))
	if err != nil {
		return err
	}
	toolboxesJSON, err := runtimePoolToolboxesJSON(toolboxes)
	if err != nil {
		return err
	}
	runtime := &template.Spec.Containers[0]
	runtime.Env = append(runtime.Env,
		corev1.EnvVar{Name: runtimePoolToolboxesEnv, Value: toolboxesJSON},
		corev1.EnvVar{Name: runtimePoolToolboxMountMethod, Value: string(method)},
	)
	runtime.TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError
	switch method {
	case ACPToolboxMountCopy:
		applyRuntimePoolToolboxCopyTemplate(template, runtime, toolboxes)
	case ACPToolboxMountImageVolume:
		applyRuntimePoolToolboxImageVolumeTemplate(template, runtime, toolboxes)
	}
	applyRuntimePoolImagePullPolicy(template, policy)
	return nil
}

func applyRuntimePoolImagePullPolicy(template *corev1.PodTemplateSpec, policy ACPToolboxPolicy) {
	for _, name := range policy.ImagePullSecrets {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		template.Spec.ImagePullSecrets = append(template.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: name})
	}
	if len(policy.NodeSelector) > 0 {
		if template.Spec.NodeSelector == nil {
			template.Spec.NodeSelector = map[string]string{}
		}
		for key, value := range policy.NodeSelector {
			if key == runtimePoolNodeOSLabel {
				continue
			}
			template.Spec.NodeSelector[key] = value
		}
	}
}

const runtimePoolNodeOSLabel = "kubernetes.io/os"

func ValidateACPToolboxNodeSelector(selector map[string]string) error {
	for key, value := range selector {
		if errors := validation.IsQualifiedName(key); len(errors) != 0 {
			return fmt.Errorf("toolbox node selector key %q is invalid: %s", key, strings.Join(errors, "; "))
		}
		if errors := validation.IsValidLabelValue(value); len(errors) != 0 {
			return fmt.Errorf("toolbox node selector value %s=%q is invalid: %s", key, value, strings.Join(errors, "; "))
		}
	}
	if value, ok := selector[runtimePoolNodeOSLabel]; ok && value != "linux" {
		return fmt.Errorf("toolbox node selector %s=%q conflicts with the Linux-only runtime Pods", runtimePoolNodeOSLabel, value)
	}
	return nil
}

func applyRuntimePoolToolboxCopyTemplate(template *corev1.PodTemplateSpec, runtime *corev1.Container, toolboxes []harnessv2.RuntimeToolbox) {
	addRuntimePoolToolboxHandoff(template, runtime)
	for i := range toolboxes {
		volumeName := runtimePoolToolboxVolumeName(i)
		volumeSize := runtimePoolToolboxVolumeSizeLimit
		template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
			Name:         volumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &volumeSize}},
		})
		args := []string{
			toolbox.CopySubcommand,
			"--src", toolboxes[i].MountPath,
			runtimePoolToolboxDstFlag, runtimePoolToolboxOutputMountPath,
		}
		if len(toolboxes[i].PathEntries) > 0 {
			args = append(args, "--path-entries", strings.Join(toolboxes[i].PathEntries, ","))
		}
		template.Spec.InitContainers = append(template.Spec.InitContainers, corev1.Container{
			Name:                     runtimePoolToolboxCopyContainerName(i),
			Image:                    toolboxes[i].Image,
			ImagePullPolicy:          corev1.PullIfNotPresent,
			Command:                  []string{runtimePoolToolboxHandoffMountPath + "/" + toolbox.HandoffBinaryName},
			Args:                     args,
			WorkingDir:               "/",
			SecurityContext:          runtimePoolToolboxInitSecurityContext(runtimePoolToolboxCopyUID),
			Resources:                runtimePoolToolboxInitResources(),
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			VolumeMounts: []corev1.VolumeMount{
				{Name: runtimePoolToolboxHandoffVolume, MountPath: runtimePoolToolboxHandoffMountPath, ReadOnly: true},
				{Name: volumeName, MountPath: runtimePoolToolboxOutputMountPath},
			},
		})
		runtime.VolumeMounts = append(runtime.VolumeMounts, corev1.VolumeMount{
			Name: volumeName, MountPath: toolboxes[i].MountPath, SubPath: toolbox.OutputRootName, ReadOnly: true,
		})
	}
	extra := runtimePoolToolboxHandoffBytes + int64(len(toolboxes))*runtimePoolToolboxVolumeBytes
	addEphemeralStorage(&runtime.Resources, extra)
}

func addRuntimePoolToolboxHandoff(template *corev1.PodTemplateSpec, runtime *corev1.Container) {
	handoffSize := runtimePoolToolboxHandoffSizeLimit
	template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
		Name:         runtimePoolToolboxHandoffVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &handoffSize}},
	})
	template.Spec.InitContainers = append(template.Spec.InitContainers, corev1.Container{
		Name:                     runtimePoolToolboxHandoffContainer,
		Image:                    runtime.Image,
		ImagePullPolicy:          corev1.PullIfNotPresent,
		Command:                  []string{toolbox.SupervisorBinaryPath},
		Args:                     []string{toolbox.HandoffSubcommand, runtimePoolToolboxDstFlag, runtimePoolToolboxHandoffMountPath},
		WorkingDir:               "/",
		SecurityContext:          runtimePoolToolboxInitSecurityContext(runtimePoolToolboxHandoffUID),
		Resources:                runtimePoolToolboxInitResources(),
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		VolumeMounts: []corev1.VolumeMount{
			{Name: runtimePoolToolboxHandoffVolume, MountPath: runtimePoolToolboxHandoffMountPath},
		},
	})
}

func addEphemeralStorage(resources *corev1.ResourceRequirements, bytes int64) {
	for _, list := range []corev1.ResourceList{resources.Requests, resources.Limits} {
		quantity, ok := list[corev1.ResourceEphemeralStorage]
		if !ok {
			continue
		}
		quantity.Add(*resource.NewQuantity(bytes, resource.BinarySI))
		list[corev1.ResourceEphemeralStorage] = quantity
	}
}

func applyRuntimePoolToolboxImageVolumeTemplate(template *corev1.PodTemplateSpec, runtime *corev1.Container, toolboxes []harnessv2.RuntimeToolbox) {
	for i := range toolboxes {
		volumeName := runtimePoolToolboxVolumeName(i)
		template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
			Name: volumeName,
			VolumeSource: corev1.VolumeSource{Image: &corev1.ImageVolumeSource{
				Reference: toolboxes[i].Image, PullPolicy: corev1.PullIfNotPresent,
			}},
		})
		runtime.VolumeMounts = append(runtime.VolumeMounts, corev1.VolumeMount{
			Name: volumeName, MountPath: toolboxes[i].MountPath,
			SubPath: strings.TrimPrefix(toolboxes[i].MountPath, "/"), ReadOnly: true,
		})
	}
	extendRuntimePoolStartupProbe(runtime, toolbox.ImageVolumeCheckTimeout)
}

func extendRuntimePoolStartupProbe(runtime *corev1.Container, budget time.Duration) {
	if probe := runtime.StartupProbe; probe != nil && probe.PeriodSeconds > 0 {
		period := time.Duration(probe.PeriodSeconds) * time.Second
		probe.FailureThreshold += int32((budget + period - 1) / period)
	}
}

func runtimePoolToolboxFailure(pods []corev1.Pod, images []runtimePoolBoundImage, now time.Time) (string, string, bool) {
	if len(images) == 0 {
		return "", "", false
	}
	for i := range pods {
		for _, status := range pods[i].Status.InitContainerStatuses {
			if !runtimePoolBoundImageInitContainer(status.Name) {
				continue
			}
			if message, ok := runtimePoolToolboxTerminationFailure(status); ok {
				return corev1alpha1.RuntimePoolReasonToolboxUnavailable, message, true
			}
			if message, ok := runtimePoolToolboxUnexplainedTermination(status); ok {
				return corev1alpha1.RuntimePoolReasonToolboxUnavailable, message, true
			}
			waiting := status.State.Waiting
			if waiting == nil || !runtimePoolBoundImageCopyContainer(status.Name) {
				continue
			}
			{
				switch waiting.Reason {
				case podWaitingReasonErrImagePull, podWaitingReasonImagePullBackOff, podWaitingReasonRegistryUnavailable, podWaitingReasonInvalidImageName:
					if !runtimePoolToolboxPullRetryExpired(&pods[i], status, now) {
						continue
					}
					return corev1alpha1.RuntimePoolReasonToolboxUnavailable,
						acpToolboxUnavailableMessage(toolbox.ReasonImagePull, status.Name+": "+waiting.Reason+" "+waiting.Message), true
				case podWaitingReasonCreateContainer, podWaitingReasonCreateContainerConfig, podWaitingReasonRunContainer:
					return corev1alpha1.RuntimePoolReasonToolboxUnavailable,
						acpToolboxUnavailableMessage(toolbox.ReasonMountFailed, status.Name+": "+waiting.Reason+" "+waiting.Message), true
				}
			}
		}
		for _, status := range pods[i].Status.ContainerStatuses {
			if status.Name != runtimeField {
				continue
			}
			if message, ok := runtimePoolToolboxTerminationFailure(status); ok {
				return corev1alpha1.RuntimePoolReasonToolboxUnavailable, message, true
			}
			waiting := status.State.Waiting
			if waiting == nil {
				continue
			}
			switch waiting.Reason {
			case podWaitingReasonCreateContainer, podWaitingReasonCreateContainerConfig, podWaitingReasonRunContainer:
				if strings.Contains(waiting.Message, "ImageVolumeMountFailed") || runtimePoolMessageMentionsBoundImage(waiting.Message, images) {
					return corev1alpha1.RuntimePoolReasonToolboxUnavailable,
						acpToolboxUnavailableMessage(toolbox.ReasonMountFailed, waiting.Message), true
				}
			case podWaitingReasonErrImagePull, podWaitingReasonImagePullBackOff, podWaitingReasonRegistryUnavailable, podWaitingReasonInvalidImageName:
				if runtimePoolMessageMentionsBoundImage(waiting.Message, images) &&
					runtimePoolToolboxPullRetryExpired(&pods[i], status, now) {
					return corev1alpha1.RuntimePoolReasonToolboxUnavailable,
						acpToolboxUnavailableMessage(toolbox.ReasonImagePull, waiting.Reason+" "+waiting.Message), true
				}
			}
		}
	}
	return "", "", false
}

const runtimePoolToolboxPullRetryWindow = 5 * time.Minute

const runtimePoolToolboxPullFailuresAnnotation = "orka.ai/toolbox-pull-failures"

type runtimePoolToolboxPullFailure struct {
	FirstObserved time.Time `json:"firstObserved"`
	RestartCount  int32     `json:"restartCount"`
}

func (r *RuntimePoolReconciler) observeRuntimePoolToolboxPullFailures(ctx context.Context, pods []corev1.Pod, images []runtimePoolBoundImage) error {
	if len(images) == 0 {
		return nil
	}
	for i := range pods {
		pod := &pods[i]
		previous := map[string]runtimePoolToolboxPullFailure{}
		if raw := pod.Annotations[runtimePoolToolboxPullFailuresAnnotation]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &previous); err != nil {
				return fmt.Errorf("decode toolbox pull observations for Pod %s/%s: %w", pod.Namespace, pod.Name, err)
			}
		}
		current := previous
		statuses := append(append([]corev1.ContainerStatus(nil), pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
		for _, status := range statuses {
			failure, exists := current[status.Name]
			if exists && (failure.RestartCount != status.RestartCount || status.State.Running != nil || status.State.Terminated != nil) {
				delete(current, status.Name)
				exists = false
			}
			waiting := status.State.Waiting
			if waiting == nil || (waiting.Reason != podWaitingReasonErrImagePull && waiting.Reason != podWaitingReasonImagePullBackOff && waiting.Reason != podWaitingReasonRegistryUnavailable) {
				continue
			}
			if !runtimePoolBoundImageCopyContainer(status.Name) &&
				(status.Name != runtimeField || !runtimePoolMessageMentionsBoundImage(waiting.Message, images)) {
				continue
			}
			if !exists || failure.FirstObserved.IsZero() {
				failure = runtimePoolToolboxPullFailure{FirstObserved: r.now(), RestartCount: status.RestartCount}
			}
			current[status.Name] = failure
		}
		encoded := ""
		if len(current) != 0 {
			data, err := json.Marshal(current)
			if err != nil {
				return err
			}
			encoded = string(data)
		}
		if encoded == pod.Annotations[runtimePoolToolboxPullFailuresAnnotation] {
			continue
		}
		base := pod.DeepCopy()
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		if encoded == "" {
			delete(pod.Annotations, runtimePoolToolboxPullFailuresAnnotation)
		} else {
			pod.Annotations[runtimePoolToolboxPullFailuresAnnotation] = encoded
		}
		if err := r.Patch(ctx, pod, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("persist toolbox pull observation for Pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}
	return nil
}

func runtimePoolToolboxPullRetryExpired(pod *corev1.Pod, status corev1.ContainerStatus, now time.Time) bool {
	if status.State.Waiting != nil && status.State.Waiting.Reason == podWaitingReasonInvalidImageName {
		return true
	}
	var observations map[string]runtimePoolToolboxPullFailure
	if json.Unmarshal([]byte(pod.Annotations[runtimePoolToolboxPullFailuresAnnotation]), &observations) != nil {
		return false
	}
	observed, exists := observations[status.Name]
	return exists && observed.RestartCount == status.RestartCount && !observed.FirstObserved.IsZero() &&
		!now.Before(observed.FirstObserved.Add(runtimePoolToolboxPullRetryWindow))
}

func runtimePoolToolboxFailedTermination(status corev1.ContainerStatus) *corev1.ContainerStateTerminated {
	if terminated := status.State.Terminated; terminated != nil {
		if terminated.ExitCode != 0 {
			return terminated
		}
		return nil
	}
	if status.State.Waiting == nil {
		return nil
	}
	if terminated := status.LastTerminationState.Terminated; terminated != nil && terminated.ExitCode != 0 {
		return terminated
	}
	return nil
}

func runtimePoolToolboxTerminationFailure(status corev1.ContainerStatus) (string, bool) {
	terminated := runtimePoolToolboxFailedTermination(status)
	if terminated == nil {
		return "", false
	}
	failure, ok := toolbox.ParseFailureLine(terminated.Message)
	if !ok {
		return "", false
	}
	return acpToolboxUnavailableMessage(failure.Reason, status.Name+": "+failure.Message), true
}

const runtimePoolToolboxUnexplainedRestartLimit int32 = 3

func runtimePoolToolboxUnexplainedTermination(status corev1.ContainerStatus) (string, bool) {
	if status.RestartCount < runtimePoolToolboxUnexplainedRestartLimit {
		return "", false
	}
	terminated := runtimePoolToolboxFailedTermination(status)
	if terminated == nil {
		return "", false
	}
	detail := fmt.Sprintf("%s: exited %d", status.Name, terminated.ExitCode)
	if reason := strings.TrimSpace(terminated.Reason); reason != "" && reason != "Error" {
		detail += " (" + reason + ")"
	}
	if message := strings.TrimSpace(terminated.Message); message != "" {
		detail += ": " + message
	}
	return acpToolboxUnavailableMessage(toolbox.ReasonCopyFailed, detail), true
}

type runtimePoolBoundImage struct {
	image  string
	volume string
}

func runtimePoolBoundImages(toolboxes []harnessv2.RuntimeToolbox, skills []harnessv2.RuntimeSkill) []runtimePoolBoundImage {
	images := make([]runtimePoolBoundImage, 0, len(toolboxes)+len(skills))
	for i := range toolboxes {
		images = append(images, runtimePoolBoundImage{image: toolboxes[i].Image, volume: runtimePoolToolboxVolumeName(i)})
	}
	for i := range skills {
		images = append(images, runtimePoolBoundImage{image: skills[i].Image, volume: runtimePoolSkillVolumeName(i)})
	}
	return images
}

func runtimePoolBoundImageInitContainer(name string) bool {
	return strings.HasPrefix(name, runtimePoolToolboxVolumePrefix) || strings.HasPrefix(name, runtimePoolSkillCopyPrefix)
}

func runtimePoolBoundImageCopyContainer(name string) bool {
	return strings.HasPrefix(name, runtimePoolToolboxCopyPrefix) || strings.HasPrefix(name, runtimePoolSkillCopyPrefix)
}

func runtimePoolMessageMentionsBoundImage(message string, images []runtimePoolBoundImage) bool {
	for i := range images {
		if strings.Contains(message, images[i].image) || strings.Contains(message, images[i].volume) {
			return true
		}
	}
	return false
}

func acpToolboxUnavailableMessage(reason, detail string) string {
	return sanitizeStatusMessage(acpToolboxUnavailablePrefix + reason + ": " + strings.TrimSpace(detail))
}
