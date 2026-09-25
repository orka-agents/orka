package controller

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	substrateNativeStartupShell    = "/bin/sh"
	substrateNativePodNamespaceEnv = "ORKA_ACP_POD_NAMESPACE"
)

// nativeSubstrateRuntimeTemplate is a pure compiler. Kubernetes-only mounts,
// references, probes and resource fields never reach the provider's API.
func nativeSubstrateRuntimeTemplate(object *unstructured.Unstructured) (*ateapipb.ActorTemplate, error) {
	spec, found, err := unstructured.NestedMap(object.Object, substrateObjectSpecField)
	if err != nil || !found {
		return nil, fmt.Errorf("substrate runtime template has no spec")
	}
	containers, ok := spec["containers"].([]any)
	if !ok || len(containers) != 1 {
		return nil, fmt.Errorf("substrate runtime template requires one supervisor container")
	}
	delete(spec, "containers")
	// This is Orka's admitted placement record, derived from a native
	// selector and the exact matching Kubernetes WorkerPool.
	delete(spec, "workerPoolRef")
	normalizeSubstrateSnapshotEnums(spec)
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	native := &ateapipb.ActorTemplate{}
	if err := protojson.Unmarshal(raw, native); err != nil {
		return nil, fmt.Errorf("substrate infrastructure is incompatible with the pinned native protocol: %w", err)
	}
	if native.GetSandboxConfig().GetSandboxClass() != ateapipb.SandboxClass_SANDBOX_CLASS_GVISOR || native.GetSandboxConfig().GetConfigName() == "" {
		return nil, fmt.Errorf("substrate runtime requires an explicit gVisor sandboxConfig")
	}
	if native.GetSnapshotsConfig().GetStorageLocation() == "" {
		return nil, fmt.Errorf("substrate runtime requires snapshotsConfig.storageLocation")
	}
	containerMap, ok := containers[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("substrate supervisor container is unreadable")
	}
	container := corev1.Container{}
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(containerMap, &container); err != nil {
		return nil, err
	}
	compiled, err := nativeSubstrateSupervisorContainer(container)
	if err != nil {
		return nil, err
	}
	for _, volume := range native.Volumes {
		if volume.GetName() == substrateNativeIdentityVolume {
			return nil, fmt.Errorf("infrastructure template defines the reserved Substrate identity volume")
		}
	}
	native.Volumes = append(native.Volumes, &ateapipb.Volume{Name: substrateNativeIdentityVolume, SystemInfo: &ateapipb.SystemInfoVolumeSource{
		DataSources: []*ateapipb.SystemInfoDataSource{{ActorMetadata: &ateapipb.ActorMetadataDataSource{Items: []*ateapipb.ActorMetadataItem{
			{Field: ateapipb.ActorMetadataField_ACTOR_METADATA_FIELD_ATESPACE, Path: "atespace"},
			{Field: ateapipb.ActorMetadataField_ACTOR_METADATA_FIELD_NAME, Path: substrateNativeObjectName},
			{Field: ateapipb.ActorMetadataField_ACTOR_METADATA_FIELD_UID, Path: substrateNativeObjectUID},
		}}}},
	}})
	compiled.VolumeMounts = append(compiled.VolumeMounts, &ateapipb.VolumeMount{Name: substrateNativeIdentityVolume, MountPath: harnessv2.SubstrateIdentityDirectory})
	native.Containers = []*ateapipb.Container{compiled}
	revision, err := substrateRuntimeTemplateObjectRevision(object)
	if err != nil {
		return nil, err
	}
	native.Metadata = &ateapipb.ResourceMetadata{Atespace: object.GetNamespace(), Name: runtimePoolChildName(object.GetName(), "r-"+strings.TrimPrefix(revision, "sha256:")[:24])}
	return native, nil
}

func nativeSubstrateSupervisorContainer(container corev1.Container) (*ateapipb.Container, error) {
	if len(container.Command) == 0 || container.Command[0] == "" {
		return nil, fmt.Errorf("substrate supervisor requires an explicit command")
	}
	// The pinned provider creates the rootfs overlay's upper directory as
	// 0700. Restore traversal of this Actor's private root before the
	// supervisor starts unprivileged children. Keep command arguments out of
	// the shell program, and fail startup if the permission repair fails.
	args := make([]string, 0, 2+len(container.Command)+len(container.Args))
	args = append(args, `chmod 0755 /; exec "$@"`, "orka-substrate-init")
	args = append(args, container.Command...)
	args = append(args, container.Args...)
	compiled := &ateapipb.Container{
		Name: container.Name, Image: container.Image, Command: []string{substrateNativeStartupShell, "-ec"}, Args: args,
		Readyz: &ateapipb.ContainerReadyz{HttpGet: &ateapipb.HTTPGetAction{Path: harnessv2.HealthPath, Port: substrateActorListenPort}, TimeoutSeconds: 120},
		SecurityContext: &ateapipb.SecurityContext{Capabilities: &ateapipb.Capabilities{
			Drop: []string{substrateNativeAllCapabilities}, Add: []string{"CHOWN", "KILL", "SETGID", "SETUID"},
		}},
	}
	seen := map[string]bool{}
	for _, env := range container.Env {
		if env.ValueFrom != nil || env.Name == "" || seen[env.Name] {
			return nil, fmt.Errorf("substrate environment must contain unique literal names; invalid entry %q", env.Name)
		}
		if len(env.Name) > 256 || len(env.Value) > 32768 {
			return nil, fmt.Errorf("substrate environment entry %q exceeds the upstream size limit", env.Name)
		}
		seen[env.Name] = true
		// Native Actor identity comes from the mounted SystemInfo volume.
		// The supervisor does not consume the Kubernetes Pod namespace.
		if env.Name == substrateNativePodNamespaceEnv {
			continue
		}
		compiled.Env = append(compiled.Env, &ateapipb.EnvVar{Name: env.Name, Value: env.Value})
	}
	if len(compiled.Env) > 32 {
		return nil, fmt.Errorf("substrate supervisor has %d environment entries; upstream admits at most 32", len(compiled.Env))
	}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		if quantity, present := container.Resources.Limits[name]; present {
			if quantity.Sign() <= 0 {
				return nil, fmt.Errorf("substrate %s limit must be positive", name)
			}
			if compiled.Resources == nil {
				compiled.Resources = &ateapipb.Resources{}
			}
			compiled.Resources.Limits = append(compiled.Resources.Limits, &ateapipb.Limits{Name: string(name), Quantity: quantity.String()})
		}
	}
	for _, mount := range container.VolumeMounts {
		if mount.SubPath != "" || mount.SubPathExpr != "" || mount.ReadOnly {
			return nil, fmt.Errorf("substrate does not support subPath or read-only mount overrides")
		}
		compiled.VolumeMounts = append(compiled.VolumeMounts, &ateapipb.VolumeMount{Name: mount.Name, MountPath: mount.MountPath})
		if mount.Name == substrateDurableWorkspaceVolume && mount.MountPath == substrateDurableWorkspaceMountPath {
			// Upstream creates DurableDir roots as 0700. Permit traversal to
			// each child's private 0700 workspace on fresh and restored mounts.
			// Both paths are controller constants, never operator shell input.
			compiled.Args[0] = fmt.Sprintf(`chmod 0755 /; chmod 0711 %s %s; exec "$@"`,
				path.Dir(substrateDurableWorkspaceMountPath), substrateDurableWorkspaceMountPath)
		}
	}
	return compiled, nil
}

func normalizeSubstrateSnapshotEnums(spec map[string]any) {
	config, ok := spec["snapshotsConfig"].(map[string]any)
	if !ok {
		return
	}
	for _, key := range []string{"onPause", "onCommit"} {
		switch config[key] {
		case "Data":
			config[key] = "SNAPSHOT_CONTENT_SCOPE_DATA"
		case "Full":
			config[key] = "SNAPSHOT_CONTENT_SCOPE_FULL"
		}
	}
	if resume, ok := config["onResume"].(map[string]any); ok {
		switch resume["fromData"] {
		case "ColdBoot":
			resume["fromData"] = "RESUME_SOURCE_COLD_BOOT"
		case "Golden":
			resume["fromData"] = "RESUME_SOURCE_GOLDEN"
		}
	}
}

func nativeSubstrateTemplateSpec(template *ateapipb.ActorTemplate) *ateapipb.ActorTemplate {
	if template == nil {
		return nil
	}
	copy := proto.Clone(template).(*ateapipb.ActorTemplate)
	copy.Metadata = &ateapipb.ResourceMetadata{Atespace: template.GetMetadata().GetAtespace(), Name: template.GetMetadata().GetName()}
	copy.Status = nil
	return copy
}

func nativeSubstrateTemplateDigest(template *ateapipb.ActorTemplate) (string, error) {
	raw, err := protojson.Marshal(nativeSubstrateTemplateSpec(template))
	if err != nil {
		return "", err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return runtimePoolJSONRevision(value)
}
