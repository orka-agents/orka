package controller

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp/toolbox"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// Toolbox Pod wiring. Names, paths, and identities are constants so the
// rendered template is stable across reconciles.
const (
	runtimePoolToolboxesEnv       = "ORKA_ACP_TOOLBOXES"
	runtimePoolToolboxMountMethod = "ORKA_ACP_TOOLBOX_MOUNT_METHOD"

	runtimePoolToolboxHandoffVolume    = "toolbox-handoff"
	runtimePoolToolboxHandoffMountPath = "/handoff"
	runtimePoolToolboxOutputMountPath  = "/out"
	runtimePoolToolboxVolumePrefix     = "toolbox-"
	runtimePoolToolboxCopyPrefix       = "toolbox-copy-"
	runtimePoolToolboxHandoffContainer = "toolbox-handoff"

	// Fixed non-root identities for the toolbox init containers. They differ
	// from each other and from the 20000-29999 session identity range.
	runtimePoolToolboxHandoffUID int64 = 64020
	runtimePoolToolboxCopyUID    int64 = 64021

	runtimePoolDropAllCapabilities corev1.Capability = "ALL"

	podWaitingReasonErrImagePull          = "ErrImagePull"
	podWaitingReasonImagePullBackOff      = "ImagePullBackOff"
	podWaitingReasonInvalidImageName      = "InvalidImageName"
	podWaitingReasonCreateContainer       = "CreateContainerError"
	podWaitingReasonCreateContainerConfig = "CreateContainerConfigError"
	podWaitingReasonRunContainer          = "RunContainerError"
)

// runtimePoolToolboxVolumeHeadroomBytes covers directory metadata and the
// copier's completion marker on top of the accepted content bytes, and
// runtimePoolToolboxBlockBytes reserves one filesystem block per permitted
// entry, so a toolbox at the documented limits (including many tiny files)
// still fits a size-enforced emptyDir.
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

// runtimePoolToolboxesJSON is the exact environment projection of the frozen
// toolbox list. Order is preserved; nothing is sorted.
func runtimePoolToolboxesJSON(toolboxes []harnessv2.RuntimeToolbox) (string, error) {
	data, err := json.Marshal(toolboxes)
	if err != nil {
		return "", fmt.Errorf("marshal runtime toolboxes: %w", err)
	}
	return string(data), nil
}

// runtimePoolToolboxesFromEnvironment rebuilds the profile toolbox list from a
// deployed template. An absent variable means no toolboxes.
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

// applyRuntimePoolToolboxTemplate adds the toolbox volumes, init containers,
// mounts, environment, pull secrets, and node selector to a rendered runtime
// Pod template. It is a no-op without toolboxes, so pools that predate the
// feature render byte-for-byte the same template.
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
	// The supervisor's own startup check and the toolbox init containers write
	// one stable FAIL line; surface it through the termination message so the
	// controller can classify the failure without reading Pod logs.
	runtime.TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError
	switch method {
	case ACPToolboxMountCopy:
		applyRuntimePoolToolboxCopyTemplate(template, runtime, toolboxes)
	case ACPToolboxMountImageVolume:
		applyRuntimePoolToolboxImageVolumeTemplate(template, runtime, toolboxes)
	}
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
				// Runtime images and toolbox helpers are Linux-only; the fixed
				// selector is never overridden.
				continue
			}
			template.Spec.NodeSelector[key] = value
		}
	}
	return nil
}

// runtimePoolNodeOSLabel is the fixed Linux node selector every runtime Pod
// template carries.
const runtimePoolNodeOSLabel = "kubernetes.io/os"

// ValidateACPToolboxNodeSelector rejects a toolbox node selector that would
// move Linux-only runtime Pods to another OS.
func ValidateACPToolboxNodeSelector(selector map[string]string) error {
	if value, ok := selector[runtimePoolNodeOSLabel]; ok && value != "linux" {
		return fmt.Errorf("toolbox node selector %s=%q conflicts with the Linux-only runtime Pods", runtimePoolNodeOSLabel, value)
	}
	return nil
}

func applyRuntimePoolToolboxCopyTemplate(template *corev1.PodTemplateSpec, runtime *corev1.Container, toolboxes []harnessv2.RuntimeToolbox) {
	handoffSize := runtimePoolToolboxHandoffSizeLimit
	template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
		Name:         runtimePoolToolboxHandoffVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &handoffSize}},
	})
	template.Spec.InitContainers = append(template.Spec.InitContainers, corev1.Container{
		Name:            runtimePoolToolboxHandoffContainer,
		Image:           runtime.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		// Command and a non-empty Args together replace both the image's
		// entrypoint and its CMD, so no image default can reach the binary.
		Command:                  []string{toolbox.SupervisorBinaryPath},
		Args:                     []string{toolbox.HandoffSubcommand, "--dst", runtimePoolToolboxHandoffMountPath},
		WorkingDir:               "/",
		SecurityContext:          runtimePoolToolboxInitSecurityContext(runtimePoolToolboxHandoffUID),
		Resources:                runtimePoolToolboxInitResources(),
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		VolumeMounts: []corev1.VolumeMount{
			{Name: runtimePoolToolboxHandoffVolume, MountPath: runtimePoolToolboxHandoffMountPath},
		},
	})
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
			"--dst", runtimePoolToolboxOutputMountPath,
		}
		if len(toolboxes[i].PathEntries) > 0 {
			args = append(args, "--path-entries", strings.Join(toolboxes[i].PathEntries, ","))
		}
		template.Spec.InitContainers = append(template.Spec.InitContainers, corev1.Container{
			Name:            runtimePoolToolboxCopyContainerName(i),
			Image:           toolboxes[i].Image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			// The toolbox image's ENTRYPOINT and CMD are both replaced: Command
			// names Orka's copier and the non-empty Args carry every option, so
			// an image CMD such as ["--help"] can never reach the invocation.
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
	// Kubelet charges emptyDir usage against the Pod's summed container
	// ephemeral-storage limit, so budget the copied toolboxes on top of the
	// resource class defaults or the Pod is evicted while it fills them.
	extra := runtimePoolToolboxHandoffBytes + int64(len(toolboxes))*runtimePoolToolboxVolumeBytes
	addEphemeralStorage(&runtime.Resources, extra)
}

// addEphemeralStorage raises the container's ephemeral-storage request and
// limit by bytes, leaving absent values absent.
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
		// Without a subPath the whole image root lands under mountPath.
		runtime.VolumeMounts = append(runtime.VolumeMounts, corev1.VolumeMount{
			Name: volumeName, MountPath: toolboxes[i].MountPath,
			SubPath: strings.TrimPrefix(toolboxes[i].MountPath, "/"), ReadOnly: true,
		})
	}
}

// runtimePoolToolboxFailure classifies toolbox-related Pod failures into the
// stable ToolboxUnavailable reason. It inspects toolbox init containers (copy
// mode), the runtime container's own toolbox startup check, and image-volume
// mount or pull failures. Messages are sanitized before they reach status.
func runtimePoolToolboxFailure(pods []corev1.Pod, toolboxes []harnessv2.RuntimeToolbox) (string, string, bool) {
	if len(toolboxes) == 0 {
		return "", "", false
	}
	for i := range pods {
		for _, status := range pods[i].Status.InitContainerStatuses {
			if !strings.HasPrefix(status.Name, runtimePoolToolboxVolumePrefix) {
				continue
			}
			if message, ok := runtimePoolToolboxTerminationFailure(status); ok {
				return corev1alpha1.RuntimePoolReasonToolboxUnavailable, message, true
			}
			// A toolbox init container exists only to bind a toolbox, so an
			// unsuccessful end without the stable line (OOM kill, a crash
			// before the copier starts) is still a toolbox failure once it has
			// exhausted a bounded number of retries.
			if message, ok := runtimePoolToolboxUnexplainedTermination(status); ok {
				return corev1alpha1.RuntimePoolReasonToolboxUnavailable, message, true
			}
			// The handoff container runs the trusted runtime image; a pull or
			// container-setup failure there is an ordinary rollout failure that
			// keeps the usual retry behavior, not a toolbox failure. Only the
			// toolbox-copy-* containers run the untrusted image.
			waiting := status.State.Waiting
			if waiting == nil || !strings.HasPrefix(status.Name, runtimePoolToolboxCopyPrefix) {
				continue
			}
			{
				switch waiting.Reason {
				case podWaitingReasonErrImagePull, podWaitingReasonImagePullBackOff, podWaitingReasonInvalidImageName:
					return corev1alpha1.RuntimePoolReasonToolboxUnavailable,
						acpToolboxUnavailableMessage(toolbox.ReasonImagePull, status.Name+": "+waiting.Reason+" "+waiting.Message), true
				case podWaitingReasonCreateContainer, podWaitingReasonCreateContainerConfig, podWaitingReasonRunContainer:
					// The container never ran: typically a toolbox image that
					// blocks the /handoff or /out mount points with a file.
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
				// Kubelet reports a missing or invalid image-volume subPath as
				// a container config error on some runtimes and as a create
				// error on others; both are permanent toolbox mount failures.
				if strings.Contains(waiting.Message, "ImageVolumeMountFailed") || runtimePoolToolboxMessageMentionsToolbox(waiting.Message, toolboxes) {
					return corev1alpha1.RuntimePoolReasonToolboxUnavailable,
						acpToolboxUnavailableMessage(toolbox.ReasonMountFailed, waiting.Message), true
				}
			case podWaitingReasonErrImagePull, podWaitingReasonImagePullBackOff, podWaitingReasonInvalidImageName:
				if runtimePoolToolboxMessageMentionsToolbox(waiting.Message, toolboxes) {
					return corev1alpha1.RuntimePoolReasonToolboxUnavailable,
						acpToolboxUnavailableMessage(toolbox.ReasonImagePull, waiting.Reason+" "+waiting.Message), true
				}
			}
		}
	}
	return "", "", false
}

// runtimePoolToolboxFailedTermination returns the termination that currently
// describes a failed container: its current terminated state with a non-zero
// exit, or, while it waits to restart after a failure, its last termination.
// A container that has since restarted successfully (running, or terminated
// with exit 0) is never classified by an older failure, so the copier's
// retry and recovery behavior is preserved.
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

// runtimePoolToolboxUnexplainedTermination classifies a toolbox init container
// that ended unsuccessfully without a stable FAIL line.
// runtimePoolToolboxUnexplainedRestartLimit is how many unexplained failed
// attempts (no stable FAIL line: a SIGKILL or OOM mid-copy, for example) a
// toolbox init container may make before the failure is treated as
// permanent. The copier's retry is idempotent, so a transient interruption
// gets a bounded chance to recover before waiting Tasks are failed.
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

func runtimePoolToolboxMessageMentionsToolbox(message string, toolboxes []harnessv2.RuntimeToolbox) bool {
	for i := range toolboxes {
		if strings.Contains(message, toolboxes[i].Image) || strings.Contains(message, runtimePoolToolboxVolumeName(i)) {
			return true
		}
	}
	return false
}

func acpToolboxUnavailableMessage(reason, detail string) string {
	return sanitizeStatusMessage(acpToolboxUnavailablePrefix + reason + ": " + strings.TrimSpace(detail))
}
