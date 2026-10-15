//go:build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/test/utils"
)

// Keep the model deterministic but the complete Kubernetes and Hyperlight
// execution paths real. Only the KVM-capable lane enables these specs.
var _ = Describe("Hyperlight deployment with aimock", Label("hyperlight"), Ordered, func() {
	const (
		providerName = "e2e-hyperlight-provider"
		agentName    = "e2e-hyperlight-opencode"
		aiTask       = "e2e-hyperlight-code-exec"
		acpTask      = "e2e-hyperlight-sandbox-exec"
		missingTask  = "e2e-hyperlight-unavailable"
	)
	var createdTasks []string
	var createdAgents []string
	var gid int64

	BeforeAll(func() {
		if !hyperlightE2EEnabled() {
			Skip("Run scripts/hyperlight-kind-e2e.sh to enable the real KVM deployment lane")
		}
		Expect(e2eMockOpenAI).To(BeTrue(), "Hyperlight deployment tests must use aimock, not live model credentials")
		Expect(e2eMockAnthropic).To(BeTrue(), "Claude Hyperlight tests must use aimock, not live model credentials")
		Expect(e2eLiveCopilotProxyBaseURL).To(BeEmpty(), "the ACP provider proxy must use aimock")
		Expect(hyperlightBundleRef).To(MatchRegexp(`@sha256:[a-f0-9]{64}$`))
		var err error
		gid, err = hyperlightE2EDeviceGID()
		Expect(err).NotTo(HaveOccurred())

		By("deploying aimock behind the production ACP provider proxy")
		projectDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())
		providerProxyMockLLM.deploy(projectDir)
		DeferCleanup(providerProxyMockLLM.delete)
		DeferCleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			// The unavailable-bundle case replaces the controller Pod. Open
			// the API connection now, not before those rollouts.
			apiBaseURL, stop, portForward, err := startControllerAPIPortForward(18089)
			Expect(err).NotTo(HaveOccurred())
			defer stopPortForward(stop, portForward)
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			cleanup, err := newE2ECleanup(apiBaseURL, token)
			Expect(err).NotTo(HaveOccurred())
			Expect(cleanup.tasks(ctx, createdTasks, false)).To(Succeed())
			for _, name := range createdAgents {
				_, err = utils.Run(exec.CommandContext(ctx, "kubectl", "delete", "agent", name,
					"-n", namespace, "--ignore-not-found", "--wait=true", "--timeout=30s"))
				Expect(err).NotTo(HaveOccurred())
			}
			_, err = utils.Run(exec.CommandContext(ctx, "kubectl", "delete", "provider", providerName,
				"-n", namespace, "--ignore-not-found", "--wait=true", "--timeout=30s"))
			Expect(err).NotTo(HaveOccurred())
		})
		createProviderCRD(providerName, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, "gpt-4.1")
	})

	AfterEach(func() { dumpDebugInfo(createdTasks...) })

	It("should deliver code_exec to a real guest and return its evidence to the model", func() {
		Expect(applyManifestJSON(hyperlightAITaskManifest(aiTask, providerName, "[e2e:hyperlight-code-exec]"))).To(Succeed())
		createdTasks = append(createdTasks, aiTask)
		Expect(waitForTaskCompletion(aiTask, 3*time.Minute)).To(Equal("Succeeded"))
		// Re-read after completion to prove the init container actually ran.
		pod := waitHyperlightTaskPod(aiTask, false)
		Expect(validateHyperlightPod(pod, aiWorkerRef, hyperlightBundleRef, gid, false)).To(Succeed())

		By("checking the real code_exec result sent back to aimock, not its canned final answer")
		output := expectMockLLMToolResult("[e2e:hyperlight-code-exec]", "code_exec", 30*time.Second)
		var result tools.CodeExecResult
		Expect(json.Unmarshal([]byte(output), &result)).To(Succeed())
		Expect(result.ExitCode).To(BeZero())
		Expect(result.TimedOut).To(BeFalse())
		Expect(result.OutputTruncated).To(BeFalse())
		Expect(result.ErrorTruncated).To(BeFalse())
		Expect(result.Error).To(ContainSubstring("hyperlight-ai-stderr"))
		Expect(validateHyperlightGuest(result.Output, "hyperlight-ai-guest", hyperlightNodeKernel(pod), false)).To(Succeed())
		verifyResultAvailable(aiTask)
	})

	It("should execute only sandbox_exec in OpenCode and keep a read-intent workspace read-only across guests", func() {
		agent := runtimeAgentManifest(agentName, "opencode", runtimeMatrixMockOpenCodeModel, 4, new(false))
		agent["spec"].(map[string]any)["runtime"].(map[string]any)["defaultAllowedTools"] = []string{acp.SandboxExecToolName}
		Expect(applyManifestJSON(agent)).To(Succeed())
		createdAgents = append(createdAgents, agentName)
		Expect(applyManifestJSON(runtimeAgentTaskManifest(acpTask, agentName,
			"Execute the scripted workspace checks with sandbox_exec. [e2e:hyperlight-sandbox-exec]",
			4, new(false), hyperlightReadWorkspace(), "", nil, nil))).To(Succeed())
		createdTasks = append(createdTasks, acpTask)
		verifyACPTaskRuntimeForTask(acpTask, acpTaskExpectation{
			ProviderKind: "opencode", Model: runtimeMatrixMockOpenCodeModel, MaxTurns: acpInt32(4), AllowBash: new(false),
			AllowedTools:    []string{acp.SandboxExecToolName},
			WorkspaceIntent: "read", Workspace: hyperlightReadWorkspaceExpectation(),
		}, 2*time.Minute)
		Expect(waitForTaskCompletion(acpTask, 3*time.Minute)).To(Equal("Succeeded"))
		pod := waitHyperlightTaskPod(acpTask, true)
		Expect(validateHyperlightPod(pod, acpOpencodeRuntimeRef, hyperlightBundleRef, gid, true)).To(Succeed())

		By("verifying real MCP tool results: the first guest's writes were refused, " +
			"and a second guest found the repository unchanged")
		var results []string
		Eventually(func(g Gomega) {
			requests, err := providerProxyMockLLM.requestsFor("[e2e:hyperlight-sandbox-exec]")
			g.Expect(err).NotTo(HaveOccurred())
			var offered, rejected bool
			for _, request := range requests {
				for _, tool := range request.Body.Tools {
					g.Expect(tool.Function.Name).NotTo(Equal("bash"), "native Bash must stay disabled")
					if tool.Function.Name == "orka_sandbox_exec" {
						offered = true
					}
				}
				rejected = rejected || hyperlightNativeCommandRejected(request, "bash")
				if found := request.ToolResults("orka_sandbox_exec"); len(found) >= 2 {
					results = found
				}
			}
			g.Expect(offered).To(BeTrue(), "OpenCode did not offer the runtime-local MCP tool")
			g.Expect(rejected).To(BeTrue(), "the forced native Bash call must return a real rejection")
			g.Expect(results).To(HaveLen(2), "both guest results must reach the provider proxy's aimock")
		}, 30*time.Second, time.Second).Should(Succeed())
		for i, marker := range []string{"hyperlight-acp-write-refused", "hyperlight-acp-read"} {
			var result acp.SandboxExecResult
			Expect(json.Unmarshal([]byte(results[i]), &result)).To(Succeed())
			Expect(result.ExitCode).To(BeZero())
			Expect(result.TimedOut).To(BeFalse())
			Expect(result.StdoutTruncated).To(BeFalse())
			Expect(result.StderrTruncated).To(BeFalse())
			Expect(validateHyperlightGuest(result.Stdout, marker, hyperlightNodeKernel(pod), true)).To(Succeed())
			if i == 0 {
				Expect(result.Stderr).To(ContainSubstring("hyperlight-acp-stderr"))
			}
		}
		verifyResultAvailable(acpTask)
	})

	for _, provider := range []string{"claude"} {
		It("should exclude native commands and execute real Hyperlight guests in "+provider, func() {
			harness := hyperlightHarness(provider)
			name := "e2e-hyperlight-" + provider
			marker := "[e2e:hyperlight-" + provider + "]"
			allowed := hyperlightHarnessAllowedTools()
			agent := hyperlightHarnessAgentManifest(name, provider)
			Expect(applyManifestJSON(agent)).To(Succeed())
			createdAgents = append(createdAgents, name)
			Expect(applyManifestJSON(runtimeAgentTaskManifest(name, name,
				"Run the scripted no-native-command and sandbox workspace checks. "+marker,
				8, new(false), hyperlightReadWorkspace(), "", nil, nil))).To(Succeed())
			createdTasks = append(createdTasks, name)
			verifyACPTaskRuntimeForTask(name, acpTaskExpectation{
				ProviderKind: provider, Model: harness.model, MaxTurns: acpInt32(8), AllowBash: new(false),
				AllowedTools: allowed, WorkspaceIntent: "read", Workspace: hyperlightReadWorkspaceExpectation(),
			}, 2*time.Minute)
			Expect(waitForTaskCompletion(name, 4*time.Minute)).To(Equal("Succeeded"))
			pod := waitHyperlightTaskPod(name, true)
			Expect(validateHyperlightPod(pod, harness.image(), hyperlightBundleRef, gid, true)).To(Succeed())

			By("checking the real harness catalog and the rejected command result, not just policy projection")
			var results []string
			Eventually(func(g Gomega) {
				requests, err := providerProxyMockLLM.requestsFor(marker)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(validateHyperlightHarnessJournal(requests, harness)).To(Succeed())
				for _, request := range requests {
					if found := request.ToolResults(harness.sandboxTool); len(found) == 2 {
						results = found
					}
				}
				g.Expect(results).To(HaveLen(2), "both guest results must reach aimock through the production provider proxy")
			}, 30*time.Second, time.Second).Should(Succeed())
			hostKernel := hyperlightNodeKernel(pod)
			for i, guestMarker := range []string{"hyperlight-acp-write-refused", "hyperlight-acp-read"} {
				result, err := decodeSandboxExecResult(results[i])
				Expect(err).NotTo(HaveOccurred(), "sandbox_exec result %d as the model saw it: %q", i, results[i])
				Expect(result.ExitCode).To(BeZero())
				Expect(result.TimedOut).To(BeFalse())
				Expect(result.StdoutTruncated).To(BeFalse())
				Expect(result.StderrTruncated).To(BeFalse())
				Expect(validateHyperlightGuest(result.Stdout, guestMarker, hostKernel, true)).To(Succeed())
				if i == 0 {
					Expect(result.Stderr).To(ContainSubstring("hyperlight-acp-stderr"))
				}
			}
			// Both guest scripts also assert that the forced native command left
			// no host-side marker. The first tries to plant hook/MCP configs
			// during the prompt and must see each refused; the second finds none.
			verifyResultAvailable(name)
		})
	}

	It("should reject Hyperlight-only Copilot before any model or native execution", func() {
		name := "e2e-hyperlight-copilot"
		agent := hyperlightHarnessAgentManifest(name, "copilot")
		Expect(applyManifestJSON(agent)).To(Succeed())
		createdAgents = append(createdAgents, name)
		Expect(applyManifestJSON(runtimeAgentTaskManifest(name, name,
			"[e2e:hyperlight-copilot-rejected] This command must never execute.",
			4, new(false), nil, "", nil, nil))).To(Succeed())
		createdTasks = append(createdTasks, name)
		Expect(waitForTaskCompletion(name, 2*time.Minute)).To(Equal("Failed"))
		cmd := exec.Command("kubectl", "get", "task", name, "-n", namespace, "-o", "jsonpath={.status.message}")
		message, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(message).To(ContainSubstring("cannot enforce Hyperlight-only commands"))
		requests, err := providerProxyMockLLM.requestsFor("[e2e:hyperlight-copilot-rejected]")
		Expect(err).NotTo(HaveOccurred())
		Expect(requests).To(BeEmpty(), "unsupported strict policy must fail before provider dispatch")
	})

	It("should report unavailable Hyperlight to the model without executing a host fallback", func() {
		By("withholding the bundle while keeping the production Hyperlight backend and device allocation")
		deployment, err := controllerManagerDeploymentName()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			configureHyperlightE2EController(deployment, hyperlightBundleRef)
			waitHyperlightControllerReady(deployment)
		})
		configureHyperlightE2EController(deployment, "")
		waitHyperlightControllerReady(deployment)
		task := hyperlightAITaskManifest(missingTask, providerName, "[e2e:hyperlight-unavailable]")
		Expect(applyManifestJSON(task)).To(Succeed())
		createdTasks = append(createdTasks, missingTask)
		// The scripted model acknowledges the tool error; Task success itself
		// is deliberately not evidence that the submitted code executed.
		Expect(waitForTaskCompletion(missingTask, 3*time.Minute)).To(Equal("Succeeded"))
		pod := waitHyperlightTaskPod(missingTask, false)
		Expect(validateHyperlightPod(pod, aiWorkerRef, "", gid, false)).To(Succeed())
		output := expectMockLLMToolResult("[e2e:hyperlight-unavailable]", "code_exec", 30*time.Second)
		var result tools.CodeExecResult
		Expect(json.Unmarshal([]byte(output), &result)).To(Succeed())
		Expect(result.ExitCode).To(Equal(-1))
		Expect(result.Output).To(BeEmpty(), "the fallback sentinel must never execute")
		Expect(result.Error).To(ContainSubstring("hyperlight backend unavailable"))
		Expect(result.Error).To(ContainSubstring("hluk binary"))
		Expect(result.TimedOut).To(BeFalse())
	})
})

func hyperlightAITaskManifest(name, providerName, marker string) map[string]any {
	return map[string]any{
		"apiVersion": "core.orka.ai/v1alpha1", "kind": "Task",
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"spec": map[string]any{"type": "ai", "ai": map[string]any{
			"prompt": "Execute the scripted Hyperlight check using code_exec. " + marker,
			"model":  "gpt-4.1", "providerRef": map[string]any{"name": providerName}, "tools": []string{"code_exec"},
		}},
	}
}
