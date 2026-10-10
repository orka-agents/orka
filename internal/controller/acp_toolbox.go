package controller

import (
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// ACPToolboxMountMethod selects how toolbox files reach the runtime Pod.
type ACPToolboxMountMethod string

const (
	// ACPToolboxMountCopy copies each toolbox into a scratch volume with an
	// init-container copier. It works on every supported Kubernetes version.
	ACPToolboxMountCopy ACPToolboxMountMethod = "copy"
	// ACPToolboxMountImageVolume mounts each toolbox image directly as a
	// read-only image volume. It needs Kubernetes 1.36 or newer.
	ACPToolboxMountImageVolume ACPToolboxMountMethod = "imageVolume"

	// ACPToolboxImageVolumeMinimumServerVersion is the first Kubernetes minor
	// where image volumes with subPath are generally available.
	ACPToolboxImageVolumeMinimumServerVersion = "1.36"

	acpToolboxUnavailablePrefix = corev1alpha1.RuntimePoolReasonToolboxUnavailable + ": "
)

// ParseACPToolboxMountMethod accepts the two documented mount methods.
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

// ACPToolboxPolicy is the controller-wide toolbox configuration. It is shared
// by Agent admission, Task planning, and RuntimePool materialization so every
// gate rejects the same inputs with the same message.
type ACPToolboxPolicy struct {
	// Enabled turns the feature on. Off by default.
	Enabled bool
	// AllowedRegistries lists registry hosts or host/path prefixes whose
	// images may be used. Empty means no image is allowed.
	AllowedRegistries []string
	// ImagePullSecrets names Secrets in the runtime namespace added to the
	// Pod's imagePullSecrets. They are never mounted into a container.
	ImagePullSecrets []string
	// NodeSelector is merged into the runtime Pod node selector when the Pod
	// carries toolboxes.
	NodeSelector map[string]string
	// MountMethod selects copy or imageVolume.
	MountMethod ACPToolboxMountMethod
	// UnavailableReason, when set, explains why toolbox mounting cannot be
	// used on this controller even though the feature is enabled (for
	// example, imageVolume on a server older than 1.36). Agents with
	// toolboxes are rejected with it.
	UnavailableReason string
}

// Toolboxes returns the Agent's declared toolboxes, or nil.
func agentToolboxes(agent *corev1alpha1.Agent) []corev1alpha1.AgentToolbox {
	if agent == nil || agent.Spec.Runtime == nil {
		return nil
	}
	return agent.Spec.Runtime.Toolboxes
}

// RuntimeToolboxesFromAgent converts Agent toolboxes into the profile shape,
// preserving declared order.
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

// ValidateAgentToolboxes applies the full toolbox policy to an Agent: feature
// gate, mounting availability, shape rules, and the registry allowlist. An
// Agent without toolboxes always passes.
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
		return fmt.Errorf("runtime.toolboxes require the orka.harness.v2 contract; harness v1 runtimes cannot mount toolboxes")
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

// ToolboxImageAllowed reports whether image matches one allowlist entry. An
// entry is a registry host (with optional port) optionally followed by
// repository path segments. The host must match exactly and path segments
// match whole segments only, so registry.example.com.evil.io never matches
// registry.example.com and registry.example.com/tools-evil never matches
// registry.example.com/tools. No network calls are made.
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

// agentToolboxPlanRejection returns the planning-time rejection for Agent
// toolboxes on execution paths that cannot host them. It repeats the
// admission checks so a Task never reaches a RuntimePool with toolboxes it
// cannot bind, even if the Agent was admitted under a different policy.
func (p ACPToolboxPolicy) agentToolboxPlanRejection(task *corev1alpha1.Task, agent *corev1alpha1.Agent, external bool, harnessV1 bool) string {
	if len(agentToolboxes(agent)) == 0 {
		return ""
	}
	switch {
	case external:
		return "runtime.toolboxes are supported only on built-in runtime types; external runtimeRef runtimes cannot mount toolboxes"
	case harnessV1:
		return "runtime.toolboxes require the orka.harness.v2 contract; harness v1 runtimes cannot mount toolboxes"
	case taskRequestsExecutionWorkspace(task):
		return "runtime.toolboxes are not supported with Task.spec.execution.workspace; execution workspaces (including Agent Sandbox and Substrate) cannot mount toolboxes yet" //nolint:staticcheck // Field path begins the user-facing validation message.
	}
	if err := p.ValidateAgentToolboxes(agent); err != nil {
		return err.Error()
	}
	return ""
}

const (
	// acpToolboxPromptNoteMaxBytes bounds the Orka-generated toolbox note.
	// Four toolboxes with eight 256-byte entries each stay well inside it.
	acpToolboxPromptNoteMaxBytes = 8192
	acpToolboxPromptNoteHeader   = "Extra command-line tools are installed for this task:\n"
	acpToolboxPromptNoteFooter   = "These folders are on PATH. Run `ls <folder>` to see what is available. There is no internet access, so you can't install new packages."
)

// acpToolboxPromptNote renders the short Orka-generated note that tells the
// agent which toolbox folders are on PATH. It lists folders from the frozen
// profile only, never file names from inside the untrusted image, and is
// empty without toolboxes or when newProcess is false.
func acpToolboxPromptNote(toolboxes []harnessv2.RuntimeToolbox, newProcess bool) string {
	if !newProcess || len(toolboxes) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString(acpToolboxPromptNoteHeader)
	listed := 0
	for i := range toolboxes {
		lines := toolboxes[i].PathDirs()
		if len(lines) == 0 {
			// A toolbox without pathEntries is mounted but contributes nothing
			// to PATH; say so instead of implying its tools are on PATH.
			lines = []string{toolboxes[i].MountPath + " (mounted, not on PATH)"}
		}
		for _, dir := range lines {
			line := "- " + dir + "\n"
			const omitted = "- (more folders omitted)\n"
			if builder.Len()+len(line)+len(omitted)+len(acpToolboxPromptNoteFooter) > acpToolboxPromptNoteMaxBytes {
				builder.WriteString(omitted)
				builder.WriteString(acpToolboxPromptNoteFooter)
				return builder.String()
			}
			builder.WriteString(line)
			listed++
		}
	}
	if listed == 0 {
		return ""
	}
	builder.WriteString(acpToolboxPromptNoteFooter)
	return builder.String()
}
