/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/orka-agents/orka/internal/hyperlight"
)

// DefaultHyperlightDeviceResource is the extended resource the Hyperlight
// device plugin (hyperlight-on-kubernetes) advertises for /dev/kvm or /dev/mshv.
const DefaultHyperlightDeviceResource = "hyperlight.dev/hypervisor"

const (
	hyperlightVolume        = "hyperlight"
	hyperlightCacheVolume   = "hyperlight-cache"
	hyperlightBundleInit    = "hyperlight-bundle"
	hyperlightDir           = "/opt/orka/hyperlight"
	hyperlightCacheDir      = "/var/cache/orka-hyperlight"
	hyperlightBundleStaging = "/mnt/hyperlight"
	hyperlightCacheStaging  = "/mnt/hyperlight-cache"
	// hyperlightDeviceGIDEnv names the device's group for a process that
	// hands it to the users it runs micro-VMs as (the ACP supervisor).
	hyperlightDeviceGIDEnv = "ORKA_HYPERLIGHT_DEVICE_GID"
)

// HyperlightPodConfig is what a Pod needs to run Hyperlight micro-VMs.
type HyperlightPodConfig struct {
	// BundleImage holds hluk and the runtime images under
	// /opt/orka/hyperlight (bin/hluk, rootfs/<runtime>.cpio); an init
	// container copies them into the Pod. Empty: the main image has them.
	BundleImage string
	// DeviceResource is the device plugin's extended resource.
	DeviceResource string
	// DeviceGID owns the device in the Pod (the plugin's DEVICE_GID); 0 adds
	// no group.
	DeviceGID int64
}

// apply gives the Pod's main container the hypervisor device, its group,
// and, from the bundle image, hluk with its runtime images and a writable
// snapshot cache.
func (c HyperlightPodConfig) apply(spec *corev1.PodSpec, container *corev1.Container) {
	resourceName := corev1.ResourceName(c.DeviceResource)
	if resourceName == "" {
		resourceName = DefaultHyperlightDeviceResource
	}
	// The requirements may be shared with a Task or Agent: copy them.
	resources := container.Resources.DeepCopy()
	if resources.Limits == nil {
		resources.Limits = corev1.ResourceList{}
	}
	if resources.Requests == nil {
		resources.Requests = corev1.ResourceList{}
	}
	resources.Limits[resourceName] = resource.MustParse("1")
	resources.Requests[resourceName] = resource.MustParse("1")
	container.Resources = *resources

	if c.DeviceGID > 0 {
		if spec.SecurityContext == nil {
			spec.SecurityContext = &corev1.PodSecurityContext{}
		}
		if !slices.Contains(spec.SecurityContext.SupplementalGroups, c.DeviceGID) {
			spec.SecurityContext.SupplementalGroups = append(spec.SecurityContext.SupplementalGroups, c.DeviceGID)
		}
		container.Env = setControllerEnvValue(container.Env, hyperlightDeviceGIDEnv, strconv.FormatInt(c.DeviceGID, 10))
	}

	// The warm snapshots and run scripts get a volume of their own, apart
	// from the bundle; hluk uses it only while no one else can write it.
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name:         hyperlightCacheVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: new(resource.MustParse("4Gi"))}},
	})
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: hyperlightCacheVolume, MountPath: hyperlightCacheDir})
	container.Env = setControllerEnvValue(container.Env, hyperlight.EnvCacheDir, hyperlightCacheDir+"/c")

	if c.BundleImage == "" {
		return
	}
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name:         hyperlightVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: new(resource.MustParse("1Gi"))}},
	})
	spec.InitContainers = append(spec.InitContainers, corev1.Container{
		Name:            hyperlightBundleInit,
		Image:           c.BundleImage,
		ImagePullPolicy: platformImagePullPolicy(c.BundleImage),
		// Copy the contents, not the attributes: a volume's root belongs to
		// root, which a non-root Pod cannot set times on. Then close the
		// cache volume's root to other users where this Pod owns it (a root
		// Pod, whose sessions run as other users).
		Command: []string{"sh", "-c", "cp -R " + hyperlightDir + "/bin " + hyperlightDir + "/rootfs " + hyperlightBundleStaging + "/ && " +
			"{ chmod 0755 " + hyperlightCacheStaging + " 2>/dev/null || true; }"},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: new(false),
			ReadOnlyRootFilesystem:   new(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{substrateNativeAllCapabilities}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: hyperlightVolume, MountPath: hyperlightBundleStaging},
			{Name: hyperlightCacheVolume, MountPath: hyperlightCacheStaging},
		},
	})
	// Read-only: no process in the Pod can replace hluk or its images.
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: hyperlightVolume, MountPath: hyperlightDir, ReadOnly: true})
	container.Env = setControllerEnvValue(container.Env, hyperlight.EnvBinary, hyperlightDir+"/bin/hluk")
	container.Env = setControllerEnvValue(container.Env, hyperlight.EnvRootfsDir, hyperlightDir+"/rootfs")
}
