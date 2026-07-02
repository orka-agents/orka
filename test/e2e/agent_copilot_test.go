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

var _ = Describe("Agent Copilot Runtime", Ordered, func() {
	const (
		taskName   = "e2e-copilot-task"
		agentName  = "e2e-copilot-agent"
		secretName = "e2e-copilot-secret"
	)

	AfterAll(func() {
		By("cleaning up copilot agent test resources")
		cmd := exec.Command("kubectl", "delete", "task", taskName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		cmd = exec.Command("kubectl", "delete", "agent", agentName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		cmd = exec.Command("kubectl", "delete", "secret", secretName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		// Results are stored in SQLite — no ConfigMap cleanup needed
	})

	AfterEach(func() {
		dumpDebugInfo(taskName, "e2e-copilot-real-task")
	})

	It("should start a harness-wrapper turn with Copilot runtime configuration", func() {
		By("creating a Secret with GITHUB_TOKEN")
		secretManifest := fmt.Sprintf(`{
			"apiVersion": "v1",
			"kind": "Secret",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"type": "Opaque",
			"stringData": {
				"GITHUB_TOKEN": "placeholder-token-for-e2e"
			}
		}`, secretName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(secretManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Secret")

		By("creating an Agent with runtime.type: copilot")
		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"runtime": {
					"type": "copilot",
					"defaultMaxTurns": 10,
					"defaultAllowBash": true
				},
				"secretRef": {
					"name": "%s"
				}
			}
		}`, agentName, namespace, secretName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Agent")

		By("creating a Task with type agent referencing the Copilot Agent")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "agent",
				"prompt": "list files in current directory",
				"agentRef": {
					"name": "%s"
				},
				"agentRuntime": {
					"maxTurns": 5
				}
			}
		}`, taskName, namespace, agentName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Task")

		By("verifying harness-wrapper metadata is planned for the copilot agent task")
		verifyHarnessWrapperMetadataForTask(taskName, map[string]string{
			"runtime":   "copilot",
			"wrapper":   "cli",
			"maxTurns":  "5",
			"allowBash": "true",
		}, 2*time.Minute)

		By("verifying the Task does not use a worker Job")
		verifyNoJobForTask(taskName, 5*time.Second)

		By("verifying the Task reaches a terminal status")
		verifyTaskTerminal := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", taskName,
				"-o", "jsonpath={.status.phase}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(BeElementOf("Running", "Succeeded", "Failed"),
				"Task should transition from Pending")
		}
		Eventually(verifyTaskTerminal, 2*time.Minute, time.Second).Should(Succeed())
	})

	It("should complete a Copilot agent task with real GitHub token", func() {
		skipIfNoKey("E2E_GITHUB_TOKEN")

		const (
			realTaskName  = "e2e-copilot-real-task"
			realAgentName = "e2e-copilot-real-agent"
		)

		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "task", realTaskName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "agent", realAgentName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		By("creating an Agent with real GitHub secret")
		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"runtime": {
					"type": "copilot",
					"defaultMaxTurns": 5,
					"defaultAllowBash": true
				},
				"secretRef": {
					"name": "e2e-github-secret"
				}
			}
		}`, realAgentName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Agent with real GitHub token")

		By("creating a Task with simple prompt")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "agent",
				"prompt": "What is 2+2? Reply with just the number.",
				"agentRef": {
					"name": "%s"
				},
				"agentRuntime": {
					"maxTurns": 3
				}
			}
		}`, realTaskName, namespace, realAgentName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Task with real GitHub token")

		By("waiting for the task to reach terminal phase (up to 5 minutes)")
		phase := waitForTaskCompletion(realTaskName, 5*time.Minute)
		Expect(phase).To(BeElementOf("Succeeded", "Failed"),
			"Copilot agent task should reach terminal phase")

		By("verifying the real copilot task used the harness wrapper")
		verifyHarnessWrapperMetadataForTask(realTaskName, map[string]string{
			"runtime": "copilot",
			"wrapper": "cli",
		}, 30*time.Second)
	})
})
