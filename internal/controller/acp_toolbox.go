package controller

import (
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

type ACPToolboxMountMethod string

const (
	ACPToolboxMountCopy        ACPToolboxMountMethod = "copy"
	ACPToolboxMountImageVolume ACPToolboxMountMethod = "imageVolume"

	ACPToolboxImageVolumeMinimumServerVersion = "1.36"

	acpToolboxUnavailablePrefix = corev1alpha1.RuntimePoolReasonToolboxUnavailable + ": "
)

func ParseACPToolboxMountMethod(raw string) (ACPToolboxMountMethod, error) {
	switch method := ACPToolboxMountMethod(strings.TrimSpace(raw)); method {
	case "", ACPToolboxMountCopy:
		return ACPToolboxMountCopy, nil
	case ACPToolboxMountImageVolume:
		return method, nil
	default:
		return "", fmt.Errorf("toolbox mount method must be %q or %q, got %q", ACPToolboxMountCopy, ACPToolboxMountImageVolume, raw)
	}
}

type ACPToolboxPolicy struct {
	Enabled           bool
	SkillsEnabled     bool
	AllowedRegistries []string
	ImagePullSecrets  []string
	NodeSelector      map[string]string
	MountMethod       ACPToolboxMountMethod
	UnavailableReason string
}

func agentToolboxes(agent *corev1alpha1.Agent) []corev1alpha1.AgentToolbox {
	if agent == nil || agent.Spec.Runtime == nil {
		return nil
	}
	return agent.Spec.Runtime.Toolboxes
}

func RuntimeToolboxesFromAgent(toolboxes []corev1alpha1.AgentToolbox) []harnessv2.RuntimeToolbox {
	if len(toolboxes) == 0 {
		return nil
	}
	result := make([]harnessv2.RuntimeToolbox, len(toolboxes))
	for i := range toolboxes {
		result[i] = harnessv2.RuntimeToolbox{
			Image:     strings.TrimSpace(toolboxes[i].Image),
			MountPath: strings.TrimSpace(toolboxes[i].MountPath),
		}
		if toolboxes[i].PathEntries != nil {
			result[i].PathEntries = append([]string(nil), toolboxes[i].PathEntries...)
		}
	}
	return result
}

func runtimePoolToolboxesFromProfile(toolboxes []harnessv2.RuntimeToolbox) []corev1alpha1.RuntimePoolToolbox {
	if len(toolboxes) == 0 {
		return nil
	}
	result := make([]corev1alpha1.RuntimePoolToolbox, len(toolboxes))
	for i := range toolboxes {
		result[i] = corev1alpha1.RuntimePoolToolbox{Image: toolboxes[i].Image, MountPath: toolboxes[i].MountPath}
		if toolboxes[i].PathEntries != nil {
			result[i].PathEntries = append([]string(nil), toolboxes[i].PathEntries...)
		}
	}
	return result
}

func runtimeToolboxesFromPool(toolboxes []corev1alpha1.RuntimePoolToolbox) []harnessv2.RuntimeToolbox {
	if len(toolboxes) == 0 {
		return nil
	}
	result := make([]harnessv2.RuntimeToolbox, len(toolboxes))
	for i := range toolboxes {
		result[i] = harnessv2.RuntimeToolbox{
			Image:     strings.TrimSpace(toolboxes[i].Image),
			MountPath: strings.TrimSpace(toolboxes[i].MountPath),
		}
		if toolboxes[i].PathEntries != nil {
			result[i].PathEntries = append([]string(nil), toolboxes[i].PathEntries...)
		}
	}
	return result
}

func (p ACPToolboxPolicy) ValidateAgentToolboxes(agent *corev1alpha1.Agent) error {
	toolboxes := agentToolboxes(agent)
	if len(toolboxes) == 0 {
		return nil
	}
	if !p.Enabled {
		return fmt.Errorf("runtime.toolboxes are disabled on this controller; enable --acp-toolboxes-enabled and allow the toolbox registries")
	}
	if reason := strings.TrimSpace(p.UnavailableReason); reason != "" {
		return fmt.Errorf("%s%s", acpToolboxUnavailablePrefix, reason)
	}
	if agent.Spec.Runtime.RuntimeRef != nil && strings.TrimSpace(agent.Spec.Runtime.RuntimeRef.Name) != "" {
		return fmt.Errorf("runtime.toolboxes are supported only on built-in runtime types; external runtimeRef runtimes cannot mount toolboxes")
	}
	if agent.Spec.Runtime.Type == "" {
		return fmt.Errorf("runtime.toolboxes require a built-in runtime.type")
	}
	if contract := agent.BuiltInContractVersion(); contract != "" && contract != corev1alpha1.AgentRuntimeContractHarnessV2 {
		return fmt.Errorf("runtime.toolboxes require the orka.harness.v2 contract")
	}
	converted := RuntimeToolboxesFromAgent(toolboxes)
	if err := harnessv2.ValidateRuntimeToolboxes(converted); err != nil {
		return fmt.Errorf("runtime.%w", err)
	}
	for i := range converted {
		if !ToolboxImageAllowed(converted[i].Image, p.AllowedRegistries) {
			return fmt.Errorf("runtime.toolboxes[%d] image %q is not from an allowed toolbox registry", i, converted[i].Image)
		}
	}
	return nil
}

func ToolboxImageAllowed(image string, allowed []string) bool {
	image = strings.TrimSpace(image)
	repository, _, found := strings.Cut(image, "@")
	if !found || repository == "" {
		return false
	}
	repositorySegments := strings.Split(repository, "/")
	for _, entry := range allowed {
		entry = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(entry), "/"))
		if entry == "" {
			continue
		}
		entrySegments := strings.Split(entry, "/")
		if len(entrySegments) > len(repositorySegments) {
			continue
		}
		matched := true
		for i := range entrySegments {
			if entrySegments[i] == "" || entrySegments[i] != repositorySegments[i] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func (p ACPToolboxPolicy) agentToolboxPlanRejection(task *corev1alpha1.Task, agent *corev1alpha1.Agent, external bool) string {
	if len(agentToolboxes(agent)) == 0 {
		return ""
	}
	switch {
	case external:
		return "runtime.toolboxes are supported only on built-in runtime types; external runtimeRef runtimes cannot mount toolboxes"
	case taskRequestsExecutionWorkspace(task):
		return "runtime.toolboxes are not supported with Task.spec.execution.workspace; execution workspaces (including Agent Sandbox and Substrate) cannot mount toolboxes yet" //nolint:staticcheck // Field path begins the user-facing validation message.
	}
	if err := p.ValidateAgentToolboxes(agent); err != nil {
		return err.Error()
	}
	return ""
}

const (
	acpToolboxPromptNoteMaxBytes   = 8192
	acpToolboxPromptNoteHeader     = "Extra command-line tools are installed for this task:\n"
	acpToolboxPromptNoteAllOnPath  = "These folders are on PATH. "
	acpToolboxPromptNoteSomeOnPath = "Folders not marked otherwise are on PATH. "
	acpToolboxPromptNoteFooter     = "Run `ls <folder>` to see what is available. There is no internet access, so you can't install new packages."
)

func acpToolboxNoteDue(session *acpTaskSession) bool {
	return session == nil || !session.Binding.PromptDelivered
}

func acpToolboxPromptNote(toolboxes []harnessv2.RuntimeToolbox, newProcess bool) string {
	if !newProcess || len(toolboxes) == 0 {
		return ""
	}
	const omitted = "- (more folders omitted)\n"
	footerBytes := max(len(acpToolboxPromptNoteAllOnPath), len(acpToolboxPromptNoteSomeOnPath)) + len(acpToolboxPromptNoteFooter)
	var builder strings.Builder
	builder.WriteString(acpToolboxPromptNoteHeader)
	onPath, mountedOnly := 0, 0
	truncated := false
list:
	for i := range toolboxes {
		lines := toolboxes[i].PathDirs()
		mounted := len(lines) == 0
		if mounted {
			lines = []string{toolboxes[i].MountPath + " (mounted, not on PATH)"}
		}
		for _, dir := range lines {
			line := "- " + dir + "\n"
			if builder.Len()+len(line)+len(omitted)+footerBytes > acpToolboxPromptNoteMaxBytes {
				builder.WriteString(omitted)
				truncated = true
				break list
			}
			builder.WriteString(line)
			if mounted {
				mountedOnly++
			} else {
				onPath++
			}
		}
	}
	if onPath+mountedOnly == 0 && !truncated {
		return ""
	}
	switch {
	case onPath > 0 && mountedOnly > 0:
		builder.WriteString(acpToolboxPromptNoteSomeOnPath)
	case onPath > 0:
		builder.WriteString(acpToolboxPromptNoteAllOnPath)
	}
	builder.WriteString(acpToolboxPromptNoteFooter)
	return builder.String()
}
