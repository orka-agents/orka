package controller

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/orka-agents/orka/internal/acp/skill"
	"github.com/orka-agents/orka/internal/acp/toolbox"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// Skill Pod wiring reuses the toolbox image delivery (copy init containers or
// read-only image volumes) under its own names. The skill source is mounted
// read-only at /opt/orka-skills/<name>; the supervisor validates it and copies
// it into each session's native skills folder.
const (
	runtimePoolSkillsEnv         = "ORKA_ACP_SKILLS"
	runtimePoolSkillVolumePrefix = "skill-"
	runtimePoolSkillCopyPrefix   = "skill-copy-"

	// runtimePoolSkillVolumeBytes bounds one copied skill: the content limit,
	// one filesystem block per permitted entry, and headroom for directory
	// metadata and the copier's completion marker.
	runtimePoolSkillVolumeBytes int64 = harnessv2.MaxRuntimeSkillTotalBytes +
		harnessv2.MaxRuntimeSkillEntries*runtimePoolToolboxBlockBytes + 4<<20

	// runtimePoolSkillSessionCopyBytes bounds the private copy of one skill
	// that every resident session receives in the sessions volume: the content
	// limit and one filesystem block per permitted entry.
	runtimePoolSkillSessionCopyBytes int64 = harnessv2.MaxRuntimeSkillTotalBytes +
		harnessv2.MaxRuntimeSkillEntries*runtimePoolToolboxBlockBytes
)

var runtimePoolSkillVolumeSizeLimit = resource.MustParse(strconv.FormatInt(runtimePoolSkillVolumeBytes, 10))

func runtimePoolSkillVolumeName(index int) string {
	return runtimePoolSkillVolumePrefix + strconv.Itoa(index)
}

func runtimePoolSkillCopyContainerName(index int) string {
	return runtimePoolSkillCopyPrefix + strconv.Itoa(index)
}

// runtimePoolSkillsJSON is the exact environment projection of the frozen
// skill list. Order is preserved; nothing is sorted.
func runtimePoolSkillsJSON(skills []harnessv2.RuntimeSkill) (string, error) {
	data, err := json.Marshal(skills)
	if err != nil {
		return "", fmt.Errorf("marshal runtime skills: %w", err)
	}
	return string(data), nil
}

// runtimePoolSkillsFromEnvironment rebuilds the profile skill list from a
// deployed template. An absent variable means no skills.
func runtimePoolSkillsFromEnvironment(environment map[string]string) ([]harnessv2.RuntimeSkill, error) {
	raw := strings.TrimSpace(environment[runtimePoolSkillsEnv])
	if raw == "" {
		return nil, nil
	}
	var skills []harnessv2.RuntimeSkill
	if err := json.Unmarshal([]byte(raw), &skills); err != nil {
		return nil, err
	}
	if len(skills) == 0 {
		return nil, nil
	}
	if err := harnessv2.ValidateRuntimeSkills(skills); err != nil {
		return nil, err
	}
	return skills, nil
}

// applyRuntimePoolSkillTemplate adds the skill volumes, init containers,
// mounts, and environment to a rendered runtime Pod template. It runs after
// applyRuntimePoolToolboxTemplate; toolboxesApplied reports whether that call
// already added the shared mount method, handoff, pull secrets, and node
// selector. maxResidentSessions sizes the storage for the per-session skill
// copies. It is a no-op without skills, so pools that predate the feature
// render byte-for-byte the same template.
func applyRuntimePoolSkillTemplate(template *corev1.PodTemplateSpec, skills []harnessv2.RuntimeSkill, maxResidentSessions int32, toolboxesApplied bool, policy ACPToolboxPolicy) error {
	if template == nil || len(skills) == 0 {
		return nil
	}
	if len(template.Spec.Containers) != 1 {
		return fmt.Errorf("runtime Pod template must have exactly one container")
	}
	method, err := ParseACPToolboxMountMethod(string(policy.MountMethod))
	if err != nil {
		return err
	}
	skillsJSON, err := runtimePoolSkillsJSON(skills)
	if err != nil {
		return err
	}
	runtime := &template.Spec.Containers[0]
	runtime.Env = append(runtime.Env, corev1.EnvVar{Name: runtimePoolSkillsEnv, Value: skillsJSON})
	if !slices.ContainsFunc(runtime.Env, func(env corev1.EnvVar) bool { return env.Name == runtimePoolToolboxMountMethod }) {
		runtime.Env = append(runtime.Env, corev1.EnvVar{Name: runtimePoolToolboxMountMethod, Value: string(method)})
	}
	// The supervisor's skill check and the copy init containers write one
	// stable FAIL line; surface it through the termination message.
	runtime.TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError
	switch method {
	case ACPToolboxMountCopy:
		applyRuntimePoolSkillCopyTemplate(template, runtime, skills)
	case ACPToolboxMountImageVolume:
		for i := range skills {
			volumeName := runtimePoolSkillVolumeName(i)
			template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
				Name: volumeName,
				VolumeSource: corev1.VolumeSource{Image: &corev1.ImageVolumeSource{
					Reference: skills[i].Image, PullPolicy: corev1.PullIfNotPresent,
				}},
			})
			runtime.VolumeMounts = append(runtime.VolumeMounts, corev1.VolumeMount{
				Name: volumeName, MountPath: skills[i].MountPath(),
				SubPath: strings.TrimPrefix(skills[i].Path, "/"), ReadOnly: true,
			})
		}
	}
	// The supervisor reads every skill before it serves, after any toolbox
	// check: from the image volume, or from the copy the init containers
	// left. That read is bounded, and the startup probe must outlast the
	// bound with either mount method, or kubelet restarts the container
	// before a stalled read reports its classified failure.
	extendRuntimePoolStartupProbe(runtime, skill.StartupCheckTimeout)
	reserveRuntimePoolSkillSessionCopies(template, runtime, len(skills), maxResidentSessions)
	if !toolboxesApplied {
		applyRuntimePoolImagePullPolicy(template, policy)
	}
	return nil
}

// reserveRuntimePoolSkillSessionCopies raises the sessions volume limit and
// the runtime container's ephemeral storage by the most the per-session skill
// copies can occupy at full resident capacity, with either mount method.
// Without it a pool with many residents and large skills would exhaust the
// fixed sessions volume on skill copies alone, and exceeding an emptyDir limit
// evicts the Pod with every session in it.
func reserveRuntimePoolSkillSessionCopies(template *corev1.PodTemplateSpec, runtime *corev1.Container, skills int, maxResidentSessions int32) {
	bytes := int64(max(maxResidentSessions, 1)) * int64(skills) * runtimePoolSkillSessionCopyBytes
	for i := range template.Spec.Volumes {
		volume := &template.Spec.Volumes[i]
		if volume.Name != runtimePoolSessionsVolume || volume.EmptyDir == nil || volume.EmptyDir.SizeLimit == nil {
			continue
		}
		limit := volume.EmptyDir.SizeLimit.DeepCopy()
		limit.Add(*resource.NewQuantity(bytes, resource.BinarySI))
		volume.EmptyDir.SizeLimit = &limit
	}
	addEphemeralStorage(&runtime.Resources, bytes)
}

func applyRuntimePoolSkillCopyTemplate(template *corev1.PodTemplateSpec, runtime *corev1.Container, skills []harnessv2.RuntimeSkill) {
	extra := int64(0)
	if !slices.ContainsFunc(template.Spec.InitContainers, func(container corev1.Container) bool {
		return container.Name == runtimePoolToolboxHandoffContainer
	}) {
		addRuntimePoolToolboxHandoff(template, runtime)
		extra += runtimePoolToolboxHandoffBytes
	}
	limits := []string{
		"--max-entries", strconv.Itoa(harnessv2.MaxRuntimeSkillEntries),
		"--max-total-bytes", strconv.Itoa(harnessv2.MaxRuntimeSkillTotalBytes),
		"--max-file-bytes", strconv.Itoa(harnessv2.MaxRuntimeSkillFileBytes),
		"--max-depth", strconv.Itoa(harnessv2.MaxRuntimeSkillDepth),
	}
	for i := range skills {
		volumeName := runtimePoolSkillVolumeName(i)
		volumeSize := runtimePoolSkillVolumeSizeLimit
		template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
			Name:         volumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &volumeSize}},
		})
		args := append([]string{
			toolbox.CopySubcommand,
			"--src", skills[i].Path,
			runtimePoolToolboxDstFlag, runtimePoolToolboxOutputMountPath,
		}, limits...)
		template.Spec.InitContainers = append(template.Spec.InitContainers, corev1.Container{
			Name:            runtimePoolSkillCopyContainerName(i),
			Image:           skills[i].Image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			// The skill image's ENTRYPOINT and CMD are both replaced, exactly as
			// for toolboxes: nothing from the image ever runs.
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
			Name: volumeName, MountPath: skills[i].MountPath(), SubPath: toolbox.OutputRootName, ReadOnly: true,
		})
	}
	extra += int64(len(skills)) * runtimePoolSkillVolumeBytes
	addEphemeralStorage(&runtime.Resources, extra)
}
