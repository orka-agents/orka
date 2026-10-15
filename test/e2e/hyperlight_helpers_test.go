//go:build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/test/utils"
)

const hyperlightE2EDefaultBundleImage = "ghcr.io/orka-agents/orka/hyperlight-bundle:e2e"

var hyperlightBundleRef string

func hyperlightE2EEnabled() bool { return e2eFlagEnabled("E2E_HYPERLIGHT") }

func hyperlightE2EBundleImage() string {
	if image := strings.TrimSpace(os.Getenv("E2E_HYPERLIGHT_BUNDLE_IMAGE")); image != "" {
		return image
	}
	return hyperlightE2EDefaultBundleImage
}

func hyperlightE2EDeviceGID() (int64, error) {
	gid, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("E2E_HYPERLIGHT_DEVICE_GID")), 10, 32)
	if err != nil || gid <= 0 {
		return 0, fmt.Errorf("E2E_HYPERLIGHT_DEVICE_GID must be a positive 32-bit device group")
	}
	return gid, nil
}

// Preserve the normal controller arguments and replace, rather than stack, the
// device group flag when the unavailable-bundle test restores the configuration.
func hyperlightControllerArgs(args []string, gid int64) []string {
	result := make([]string, 0, len(args)+1)
	for i := 0; i < len(args); i++ {
		if args[i] == "--hyperlight-device-gid" {
			i++
			continue
		}
		if !strings.HasPrefix(args[i], "--hyperlight-device-gid=") {
			result = append(result, args[i])
		}
	}
	return append(result, "--hyperlight-device-gid="+strconv.FormatInt(gid, 10))
}

// Maps deliberately retain an explicit empty value. EnvVar's omitempty would
// leave the previous bundle image in place in a strategic merge patch.
func hyperlightControllerPatch(args []string, gid int64, bundleRef string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []map[string]any{{
			"name": "manager",
			"args": hyperlightControllerArgs(args, gid),
			"env": []map[string]string{
				{"name": "ORKA_AI_WORKER_CODE_EXEC_BACKEND", "value": "hyperlight"},
				{"name": "ORKA_ACP_SANDBOX_EXEC_ENABLED", "value": "true"},
				{"name": "ORKA_HYPERLIGHT_BUNDLE_IMAGE", "value": bundleRef},
			},
		}}}}},
	})
}

func configureHyperlightE2EController(deploymentName, bundleRef string) {
	gid, err := hyperlightE2EDeviceGID()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	var deployment appsv1.Deployment
	kubectlJSON(Default, &deployment, "get", "deployment", deploymentName, "-n", namespace)
	index := slices.IndexFunc(deployment.Spec.Template.Spec.Containers, func(c corev1.Container) bool {
		return c.Name == "manager"
	})
	ExpectWithOffset(1, index).To(BeNumerically(">=", 0), "controller manager container is missing")
	patch, err := hyperlightControllerPatch(deployment.Spec.Template.Spec.Containers[index].Args, gid, bundleRef)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	_, err = utils.Run(exec.Command("kubectl", "patch", "deployment", deploymentName, "-n", namespace,
		"--type=strategic", "-p", string(patch)))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

func waitHyperlightControllerReady(deploymentName string) {
	_, err := utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+deploymentName,
		"-n", namespace, "--timeout=5m"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Hyperlight controller configuration did not roll out")
	// A worker started now could still be refused by the API Service; probe it
	// from the suite's aimock Pod, which the Hyperlight lane always deploys.
	waitForControllerAPIServiceReady("deployment/"+mockLLMName, "-n", namespace, "-c", "aimock", "--",
		"node", "-e", "fetch(process.argv[1], {signal: AbortSignal.timeout(2000)})"+
			".then(response => response.text()).then(body => console.log(body))")
}

func waitHyperlightTaskPod(taskName string, agent bool) corev1.Pod {
	var pod corev1.Pod
	EventuallyWithOffset(1, func(g Gomega) {
		if agent {
			task := getACPTask(g, taskName)
			g.Expect(task.Status.Execution).NotTo(BeNil())
			g.Expect(task.Status.Execution.RuntimePoolName).NotTo(BeEmpty())
			_, pod = runtimePoolAndActivePod(g, task.Status.Execution.RuntimePoolName)
		} else {
			var pods corev1.PodList
			kubectlJSON(g, &pods, "get", "pods", "-n", namespace, "-l", "orka.ai/task="+taskName)
			g.Expect(pods.Items).To(HaveLen(1))
			pod = pods.Items[0]
		}
		g.Expect(pod.Spec.NodeName).NotTo(BeEmpty())
		g.Expect(pod.Status.ContainerStatuses).NotTo(BeEmpty())
	}, 2*time.Minute, time.Second).Should(Succeed())
	return pod
}

func hyperlightNodeKernel(pod corev1.Pod) string {
	output, err := utils.Run(exec.Command("kubectl", "get", "node", pod.Spec.NodeName,
		"-o", "jsonpath={.status.nodeInfo.kernelVersion}"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, strings.TrimSpace(output)).NotTo(BeEmpty())
	return strings.TrimSpace(output)
}

// Check the actual Pod and successful init container, not only a Job or pool
// template. An empty bundle is intentional only in the fail-closed case.
func validateHyperlightPod(pod corev1.Pod, image, bundleRef string, gid int64, agent bool) error {
	index := slices.IndexFunc(pod.Spec.Containers, func(c corev1.Container) bool { return c.Image == image })
	if index < 0 || !imageDigestPattern.MatchString(image) {
		return fmt.Errorf("Pod did not use the pinned production worker image")
	}
	worker := pod.Spec.Containers[index]
	for _, resources := range []corev1.ResourceList{worker.Resources.Requests, worker.Resources.Limits} {
		device := resources[controller.DefaultHyperlightDeviceResource]
		if device.CmpInt64(1) != 0 {
			return fmt.Errorf("worker did not request and limit one Hyperlight device")
		}
	}
	if pod.Spec.SecurityContext == nil || !slices.Contains(pod.Spec.SecurityContext.SupplementalGroups, gid) {
		return fmt.Errorf("Pod lacks the hypervisor supplemental group")
	}
	env := make(map[string]string)
	for _, value := range worker.Env {
		env[value.Name] = value.Value
	}
	if agent {
		if env["ORKA_ACP_SANDBOX_EXEC_ENABLED"] != "true" || env["ORKA_HYPERLIGHT_DEVICE_GID"] != strconv.FormatInt(gid, 10) {
			return fmt.Errorf("runtime did not enable sandbox_exec with the hypervisor group")
		}
	} else if env[workerenv.CodeExecBackend] != "hyperlight" || env[workerenv.CodeExecBackendEnforced] != "true" {
		return fmt.Errorf("AI worker backend was not pinned to Hyperlight")
	}
	if env["ORKA_HYPERLIGHT_BINARY"] != "/opt/orka/hyperlight/bin/hluk" ||
		env["ORKA_HYPERLIGHT_ROOTFS_DIR"] != "/opt/orka/hyperlight/rootfs" {
		return fmt.Errorf("worker did not use the controller-owned bundle paths")
	}
	cache := slices.IndexFunc(worker.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.MountPath == "/var/cache/orka-hyperlight" && !m.ReadOnly
	})
	if cache < 0 {
		return fmt.Errorf("worker lacks the writable snapshot cache")
	}
	init := slices.IndexFunc(pod.Spec.InitContainers, func(c corev1.Container) bool {
		return c.Name == "hyperlight-bundle"
	})
	mount := slices.IndexFunc(worker.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.MountPath == "/opt/orka/hyperlight"
	})
	if bundleRef == "" {
		if init >= 0 || mount >= 0 {
			return fmt.Errorf("unavailable-bundle control unexpectedly received a bundle")
		}
		return nil
	}
	if init < 0 || pod.Spec.InitContainers[init].Image != bundleRef || !imageDigestPattern.MatchString(bundleRef) {
		return fmt.Errorf("Pod lacks the pinned Hyperlight bundle init container")
	}
	if mount < 0 || !worker.VolumeMounts[mount].ReadOnly ||
		worker.VolumeMounts[mount].Name == worker.VolumeMounts[cache].Name {
		return fmt.Errorf("bundle is not mounted read-only separately from the cache")
	}
	status := slices.IndexFunc(pod.Status.InitContainerStatuses, func(c corev1.ContainerStatus) bool {
		return c.Name == "hyperlight-bundle"
	})
	if status < 0 {
		return fmt.Errorf("bundle init container status is missing")
	}
	terminated := pod.Status.InitContainerStatuses[status].State.Terminated
	if terminated == nil || terminated.ExitCode != 0 {
		return fmt.Errorf("bundle init container did not complete successfully")
	}
	return nil
}

type hyperlightGuestEvidence struct {
	Kernel        string `json:"kernel"`
	Marker        string `json:"marker"`
	Workspace     string `json:"workspace"`
	NetworkDenied bool   `json:"network_denied"`
}

func validateHyperlightGuest(output, marker, hostKernel string, workspace bool) error {
	var evidence hyperlightGuestEvidence
	if err := json.Unmarshal([]byte(output), &evidence); err != nil {
		return fmt.Errorf("guest did not return execution evidence: %w", err)
	}
	// The pinned Unikraft kernel advertises the Ijiraq release, not the node's
	// Linux kernel. A model's canned final answer is not guest evidence.
	if hostKernel == "" || evidence.Kernel == hostKernel || !strings.Contains(evidence.Kernel, "Ijiraq") {
		return fmt.Errorf("result did not identify the pinned guest kernel separately from the host")
	}
	if evidence.Marker != marker || !evidence.NetworkDenied {
		return fmt.Errorf("guest marker or socket-policy evidence is missing")
	}
	if workspace && strings.TrimRight(evidence.Workspace, "/") != "/workspace" {
		return fmt.Errorf("sandbox did not execute in the mounted workspace")
	}
	return nil
}

// claudeToolResultReminder is the context-budget reminder the pinned Claude
// Code appends to MCP tool results. Nothing else may follow the tool's JSON:
// hook or workspace output there would mean the exclusion failed.
var claudeToolResultReminder = regexp.MustCompile(
	`^<system-reminder>\n<total_tokens>\d+ tokens left</total_tokens>\n</system-reminder>$`)

// decodeSandboxExecResult decodes a sandbox_exec result as the model saw it.
func decodeSandboxExecResult(text string) (acp.SandboxExecResult, error) {
	var result acp.SandboxExecResult
	decoder := json.NewDecoder(strings.NewReader(text))
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	if rest := strings.TrimSpace(text[decoder.InputOffset():]); rest != "" && !claudeToolResultReminder.MatchString(rest) {
		return result, fmt.Errorf("unexpected text after the sandbox_exec result: %q", rest)
	}
	return result, nil
}

// These are the model-facing identities used by the pinned harnesses, not the
// canonical Orka policy names. aimock normalizes Responses and Anthropic
// requests to the same chat-shaped journal, so results stay attributable to the
// actual tool calls across protocols.
type hyperlightHarnessCase struct {
	provider       string
	model          string
	sandboxTool    string
	readTool       string
	commandTools   []string
	endpointSuffix string
}

func hyperlightHarness(provider string) hyperlightHarnessCase {
	switch provider {
	case "claude":
		return hyperlightHarnessCase{
			provider: provider, model: runtimeMatrixMockClaudeModel,
			sandboxTool: "mcp__orka__sandbox_exec", readTool: "Read",
			commandTools: []string{"Bash"}, endpointSuffix: "/messages",
		}
	case "copilot":
		return hyperlightHarnessCase{
			provider: provider, model: "gpt-4.1",
			sandboxTool: "orka-sandbox_exec", readTool: "view",
			commandTools: []string{"bash", "list_bash", "read_bash", "stop_bash", "write_bash"}, endpointSuffix: "/responses",
		}
	default:
		panic("unsupported Hyperlight harness case: " + provider)
	}
}

func (h hyperlightHarnessCase) image() string {
	switch h.provider {
	case "claude":
		return acpClaudeRuntimeRef
	case "copilot":
		return acpCopilotRuntimeRef
	default:
		panic("unsupported Hyperlight harness image: " + h.provider)
	}
}

func hyperlightHarnessAllowedTools() []string {
	return []string{"Read", acp.SandboxExecToolName}
}

// hyperlightReadWorkspace gives a sandbox_exec session the pinned repository
// with read intent, which mounts it read-only in every guest. Its README holds
// liveRuntimeRepoSentinel for the guests and the native Read to find.
func hyperlightReadWorkspace() *runtimeWorkspaceConfig {
	return &runtimeWorkspaceConfig{GitRepo: liveRuntimeRepoURL, Ref: liveRuntimeRepoRef}
}

func hyperlightReadWorkspaceExpectation() *acpWorkspaceExpectation {
	return &acpWorkspaceExpectation{Intent: "read", GitRepo: liveRuntimeRepoURL, Ref: liveRuntimeRepoRef}
}

func hyperlightHarnessAgentManifest(name, provider string) map[string]any {
	agent := runtimeAgentManifest(name, provider, hyperlightHarness(provider).model, 8, new(false))
	agent["spec"].(map[string]any)["runtime"].(map[string]any)["defaultAllowedTools"] = hyperlightHarnessAllowedTools()
	return agent
}

func validateHyperlightHarnessJournal(requests []mockLLMRequest, h hyperlightHarnessCase) error {
	var offeredSandbox, offeredRead, rejectedCommand, nativeRead bool
	for _, request := range requests {
		if request.Body.Truncated {
			return fmt.Errorf("%s tool catalog/result evidence was truncated", h.provider)
		}
		requestURL, parseErr := url.ParseRequestURI(request.Path)
		if parseErr != nil || !strings.HasSuffix(requestURL.Path, h.endpointSuffix) {
			return fmt.Errorf("%s did not use its pinned %s protocol", h.provider, h.endpointSuffix)
		}
		for _, tool := range request.Body.Tools {
			name := tool.Function.Name
			if slices.ContainsFunc(h.commandTools, func(command string) bool { return strings.EqualFold(command, name) }) {
				return fmt.Errorf("%s offered excluded native command tool %q", h.provider, name)
			}
			if strings.Contains(name, "hyperlight_unmanaged") {
				return fmt.Errorf("%s offered an unmanaged workspace MCP tool", h.provider)
			}
			offeredSandbox = offeredSandbox || name == h.sandboxTool
			offeredRead = offeredRead || name == h.readTool
		}
		rejectedCommand = rejectedCommand || hyperlightNativeCommandRejected(request, h.commandTools[0])
		for _, output := range request.ToolResults(h.readTool) {
			nativeRead = nativeRead || strings.Contains(output, liveRuntimeRepoSentinel)
		}
	}
	if !offeredSandbox || !offeredRead || !rejectedCommand || !nativeRead {
		return fmt.Errorf("%s lacks real catalog/rejection/read evidence: sandbox=%t read=%t rejected=%t readResult=%t",
			h.provider, offeredSandbox, offeredRead, rejectedCommand, nativeRead)
	}
	return nil
}

func hyperlightCommandRejected(output string) bool {
	output = strings.ToLower(output)
	for _, reason := range []string{
		"not allowed", "not permitted", "not available", "unknown tool", "tool not found",
		"unavailable tool", "arguments provided to the tool are invalid",
		"no such tool", "unsupported call", "denied", "disabled", "unrecognized tool", "not enabled", "invalid tool",
	} {
		if strings.Contains(output, reason) {
			return true
		}
	}
	return false
}

// hyperlightNativeCommandRejected correlates a real tool result with a native
// call. OpenCode rewrites unavailable tools to InvalidTool and transports may
// regenerate IDs; the original tool name remains in its structured arguments.
func hyperlightNativeCommandRejected(request mockLLMRequest, name string) bool {
	calls := map[string]bool{}
	for _, message := range request.Body.Messages {
		for _, call := range message.ToolCalls {
			original := call.Function.Name
			if original == "invalid" {
				var arguments struct {
					Tool string `json:"tool"`
				}
				if json.Unmarshal([]byte(call.Function.Arguments), &arguments) == nil {
					original = arguments.Tool
				}
			}
			calls[call.ID] = strings.EqualFold(original, name)
		}
		if message.Role == "tool" && calls[message.ToolCallID] && hyperlightCommandRejected(message.Text()) {
			return true
		}
	}
	return false
}
