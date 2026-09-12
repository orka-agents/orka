package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	agentRuntimeFoundryProvider     = "foundry"
	agentRuntimeFoundryBroker       = "broker"
	agentRuntimeRecoveryPodKind     = "Pod"
	agentRuntimeDurableWorkspaceEnv = "ORKA_ACP_DURABLE_WORKSPACE_DIR"
	agentRuntimeAllCapabilities     = corev1.Capability("ALL")
)

func recoveryProviderKind(spec corev1alpha1.AgentRuntimeRegistrySpec) string {
	if spec.Capabilities == nil || spec.Capabilities.Profile == nil {
		return ""
	}
	return spec.Capabilities.Profile.ProviderKind
}

func recoverySupervisorContainer(spec *corev1.PodSpec, name string) (*corev1.Container, error) {
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i], nil
		}
	}
	return nil, errors.New("runtime recovery supervisor container is missing")
}

func recoverySupervisorStatus(pod *corev1.Pod, name string) (corev1.ContainerStatus, error) {
	if len(pod.Status.ContainerStatuses) == len(pod.Spec.Containers) {
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == name {
				return status, nil
			}
		}
	}
	return corev1.ContainerStatus{}, errors.New("runtime recovery supervisor container status is unavailable")
}

func validateAgentRuntimeRecoveryPodSpec(spec corev1.PodSpec, containerName, provider string) error {
	if len(spec.InitContainers) != 0 || len(spec.EphemeralContainers) != 0 || spec.HostPID || spec.HostIPC || spec.HostNetwork ||
		(spec.ShareProcessNamespace != nil && *spec.ShareProcessNamespace) || (spec.OS != nil && spec.OS.Name != corev1.Linux) {
		return errors.New("runtime recovery requires private Linux process namespaces without init or ephemeral containers")
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		return errors.New("runtime recovery requires explicit automountServiceAccountToken=false")
	}
	foundry := provider == agentRuntimeFoundryProvider
	if foundry {
		if containerName != "supervisor" || len(spec.Containers) != 2 {
			return errors.New("runtime recovery for Foundry requires exactly the supervisor and broker containers")
		}
		if _, err := recoverySupervisorContainer(&spec, agentRuntimeFoundryBroker); err != nil {
			return err
		}
	} else if len(spec.Containers) != 1 {
		return errors.New("local runtime recovery requires exactly one supervisor container")
	}
	supervisor, err := recoverySupervisorContainer(&spec, containerName)
	if err != nil {
		return err
	}
	for _, container := range spec.Containers {
		if err := validateRecoveryContainer(container); err != nil {
			return err
		}
	}
	return validateRecoveryVolumes(spec.Volumes, *supervisor, foundry)
}

// validateAgentRuntimeRecoveryPodSpec also resolves supervisor Secret references:
// disabling automount does not prevent a legacy service account token Secret
// from being mounted or supplied through an explicit environment variable.
func (r *AgentRuntimeReconciler) validateAgentRuntimeRecoveryPodSpec(ctx context.Context, namespace string, spec corev1.PodSpec, containerName, provider string) error {
	if err := validateAgentRuntimeRecoveryPodSpec(spec, containerName, provider); err != nil {
		return err
	}
	supervisor, err := recoverySupervisorContainer(&spec, containerName)
	if err != nil {
		return err
	}
	for _, name := range recoverySupervisorSecretNames(spec.Volumes, *supervisor) {
		if namespace == "" || name == "" {
			return errors.New("runtime recovery supervisor Secret reference is incomplete")
		}
		secret := &corev1.Secret{}
		if err := r.endpointReader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
			return fmt.Errorf("resolve runtime recovery supervisor Secret: %w", err)
		}
		// Type alone determines admission; never inspect or expose Secret data.
		if secret.Type == corev1.SecretTypeServiceAccountToken {
			return errors.New("runtime recovery supervisor cannot reference a service account identity token Secret")
		}
	}
	return nil
}

func recoverySupervisorSecretNames(volumes []corev1.Volume, supervisor corev1.Container) []string {
	var secretNames []string
	for _, volume := range volumes {
		if !recoveryContainerMounts(supervisor, volume.Name) {
			continue
		}
		if volume.Secret != nil {
			secretNames = append(secretNames, volume.Secret.SecretName)
		}
		if volume.Projected != nil {
			for _, source := range volume.Projected.Sources {
				if source.Secret != nil {
					secretNames = append(secretNames, source.Secret.Name)
				}
			}
		}
	}
	for _, env := range supervisor.Env {
		if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
			secretNames = append(secretNames, env.ValueFrom.SecretKeyRef.Name)
		}
	}
	slices.Sort(secretNames)
	return slices.Compact(secretNames)
}

func validateRecoveryContainer(container corev1.Container) error {
	_, digest, pinned := strings.Cut(container.Image, "@")
	if !pinned || store.ValidateCanonicalDigest("runtime image", digest) != nil || len(container.EnvFrom) != 0 {
		return errors.New("runtime recovery requires digest-pinned images and explicit environment")
	}
	for _, env := range container.Env {
		if env.Name == agentRuntimeBootEnvironment {
			return errors.New("runtime recovery forbids a fixed supervisor boot ID")
		}
		if env.Name == agentRuntimeDurableWorkspaceEnv && (env.Value != "" || env.ValueFrom != nil) {
			return errors.New("runtime recovery does not retire durable workspace storage")
		}
	}
	if security := container.SecurityContext; security != nil {
		if security.Privileged != nil && *security.Privileged {
			return errors.New("runtime recovery forbids privileged containers")
		}
		if security.Capabilities != nil && slices.ContainsFunc(security.Capabilities.Add, func(capability corev1.Capability) bool {
			return capability == "SYS_ADMIN" || capability == "SYS_PTRACE" || capability == agentRuntimeAllCapabilities
		}) {
			return errors.New("runtime recovery forbids process namespace escape capabilities")
		}
	}
	return nil
}

func validateRecoveryVolumes(volumes []corev1.Volume, supervisor corev1.Container, foundry bool) error {
	for _, volume := range volumes {
		if volume.PersistentVolumeClaim != nil {
			if !foundry || recoveryContainerMounts(supervisor, volume.Name) {
				return errors.New("runtime recovery permits a ledger PVC only in the Foundry broker")
			}
			continue
		}
		if volume.EmptyDir == nil && volume.Secret == nil && volume.ConfigMap == nil && volume.Projected == nil && volume.DownwardAPI == nil {
			return errors.New("runtime recovery supports only Pod-local ephemeral volumes and read-only configuration")
		}
		// Explicit token projections are independent of the Pod's automount setting.
		if volume.Projected != nil && recoveryContainerMounts(supervisor, volume.Name) {
			for _, source := range volume.Projected.Sources {
				if source.ServiceAccountToken != nil {
					return errors.New("runtime recovery supervisor cannot mount a service account identity token")
				}
			}
		}
	}
	return nil
}

func recoveryContainerMounts(container corev1.Container, volumeName string) bool {
	return slices.ContainsFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool { return mount.Name == volumeName }) ||
		slices.ContainsFunc(container.VolumeDevices, func(device corev1.VolumeDevice) bool { return device.Name == volumeName })
}
