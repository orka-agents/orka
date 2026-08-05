//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/orka-agents/orka/test/utils"
)

var _ = Describe("Comprehensive Functionality", Ordered, func() {
	const (
		containerCompTaskName = "e2e-comp-container"
		aiCompTaskName        = "e2e-comp-ai"
		claudeCompTaskName    = "e2e-comp-claude"
		claudeCompAgentName   = "e2e-comp-claude-agent"
		copilotCompTaskName   = "e2e-comp-copilot"
		copilotCompAgentName  = "e2e-comp-copilot-agent"
		aiProviderName        = "e2e-comp-provider"
	)

	AfterAll(func() {
		By("cleaning up comprehensive test resources")
		for _, name := range []string{containerCompTaskName, aiCompTaskName, claudeCompTaskName} {
			cmd := exec.Command("kubectl", "delete", "task", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
		for _, name := range []string{claudeCompAgentName} {
			cmd := exec.Command("kubectl", "delete", "agent", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
		cmd := exec.Command("kubectl", "delete", "provider", aiProviderName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		dumpDebugInfo(containerCompTaskName, aiCompTaskName, claudeCompTaskName)
	})

	// Container task: validates execution environment, file I/O, process management, and output
	It("should run a comprehensive container task that validates the full execution environment", func() {
		By("creating a container task with an environment validation script")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "container",
				"image": "busybox:latest",
				"command": ["sh", "-c"],
				"args": ["set -e; echo '=== Environment ===' && env | grep ORKA_ | sort && echo '=== File I/O ===' && echo 'test-data-12345' > /tmp/testfile.txt && cat /tmp/testfile.txt && [ \"$(cat /tmp/testfile.txt)\" = 'test-data-12345' ] && echo 'file_io: PASS' && echo '=== Process ===' && echo \"pid: $$\" && echo \"uid: $(id -u)\" && echo '=== Network ===' && nslookup kubernetes.default.svc.cluster.local > /dev/null 2>&1 && echo 'dns: PASS' || echo 'dns: SKIP' && echo '=== Filesystem ===' && df -h /tmp | tail -1 && echo '=== Multi-step ===' && for i in 1 2 3; do echo \"step $i completed\"; done && echo '=== ALL CHECKS PASSED ==='"],
				"env": [
					{"name": "CUSTOM_VAR", "value": "e2e-test-value"}
				],
				"resources": {
					"requests": {"cpu": "50m", "memory": "32Mi"},
					"limits": {"cpu": "200m", "memory": "64Mi"}
				}
			}
		}`, containerCompTaskName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create comprehensive container task")

		By("waiting for task to succeed")
		phase := waitForTaskCompletion(containerCompTaskName, 3*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "Comprehensive container task should succeed")

		By("verifying the result is stored and contains expected output")
		verifyResultAvailable(containerCompTaskName)
	})

	// AI task: exercises the AI worker with code_exec tool
	It("should run a comprehensive AI task that uses built-in tools", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating an OpenAI Provider for the AI task")
		createProviderCRD(aiProviderName, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, model)

		By("creating an AI task with a prompt that requires tool use")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "ai",
				"ai": {
					"prompt": "Use the code_exec tool to run this Python code and tell me the result:\n\nimport json\ndata = {'numbers': [i**2 for i in range(1, 6)], 'sum': sum(i**2 for i in range(1, 6))}\nprint(json.dumps(data))\n\nAfter running it, confirm the sum is correct by explaining the calculation.",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, aiCompTaskName, namespace, model, aiProviderName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create comprehensive AI task")

		By("waiting for task to complete (up to 5 minutes)")
		phase := waitForTaskCompletion(aiCompTaskName, 5*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "Comprehensive AI task should succeed")

		By("verifying the result is stored")
		verifyResultAvailable(aiCompTaskName)
	})

	// ACP agent task: validates shared RuntimePool planning instead of per-Task Jobs.
	It("should plan a comprehensive Claude task on an ACP v2 RuntimePool", func() {
		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {"name": %q, "namespace": %q},
			"spec": {
				"runtime": {
					"type": "claude",
					"defaultMaxTurns": 10,
					"defaultAllowBash": false
				},
				"model": {"name": "claude-sonnet-4-20250514"}
			}
		}`, claudeCompAgentName, namespace)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create comprehensive Claude agent")

		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": %q, "namespace": %q},
			"spec": {
				"type": "agent",
				"prompt": "Read the workspace and summarize the most important files without modifying them.",
				"agentRef": {"name": %q},
				"agentRuntime": {"maxTurns": 10},
				"workspace": {"intent": "read"}
			}
		}`, claudeCompTaskName, namespace, claudeCompAgentName)
		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create comprehensive Claude task")

		verifyACPTaskRuntimeForTask(claudeCompTaskName, acpTaskExpectation{
			ProviderKind:    "claude",
			Model:           "claude-sonnet-4-20250514",
			WorkspaceIntent: "read",
			MaxTurns:        acpInt32(10),
			AllowBash:       acpBool(false),
			Workspace:       &acpWorkspaceExpectation{Intent: "read"},
		}, 2*time.Minute)
		verifyNoJobForTask(claudeCompTaskName, 5*time.Second)
	})

})
