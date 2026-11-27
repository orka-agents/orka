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

const (
	runtimePoolSkillsEnv         = "ORKA_ACP_SKILLS"
	runtimePoolSkillVolumePrefix = "skill-"
	runtimePoolSkillCopyPrefix   = "skill-copy-"

	runtimePoolSkillVolumeBytes int64 = harnessv2.MaxRuntimeSkillTotalBytes +
		harnessv2.MaxRuntimeSkillEntries*runtimePoolToolboxBlockBytes + 4<<20

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

func runtimePoolSkillsJSON(skills []harnessv2.RuntimeSkill) (string, error) {
	data, err := json.Marshal(skills)
	if err != nil {
		return "", fmt.Errorf("marshal runtime skills: %w", err)
	}
	return string(data), nil
}

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
	extendRuntimePoolStartupProbe(runtime, skill.StartupCheckTimeout)
	reserveRuntimePoolSkillSessionCopies(template, runtime, len(skills), maxResidentSessions)
	if !toolboxesApplied {
		applyRuntimePoolImagePullPolicy(template, policy)
	}
	return nil
}

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
			Name:                     runtimePoolSkillCopyContainerName(i),
			Image:                    skills[i].Image,
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
			Name: volumeName, MountPath: skills[i].MountPath(), SubPath: toolbox.OutputRootName, ReadOnly: true,
		})
	}
	extra += int64(len(skills)) * runtimePoolSkillVolumeBytes
	addEphemeralStorage(&runtime.Resources, extra)
}
