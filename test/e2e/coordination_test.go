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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/orka-agents/orka/test/utils"
)

var _ = Describe("Multi-Agent Coordination", Ordered, func() {
	const (
		coordinatorAgentName = "e2e-coordinator"
		workerAgentName      = "e2e-coord-worker"
		coordTaskName        = "e2e-coord-task"
		coordProviderName    = "e2e-coord-provider"
	)

	AfterAll(func() {
		By("cleaning up coordination test resources")
		cmd := exec.Command("kubectl", "delete", "task", coordTaskName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		// Clean up child tasks
		cmd = exec.Command("kubectl", "delete", "tasks", "-l", fmt.Sprintf("orka.ai/parent-task=%s", coordTaskName),
			"-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		for _, name := range []string{coordinatorAgentName, workerAgentName} {
			cmd = exec.Command("kubectl", "delete", "agent", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
		cmd = exec.Command("kubectl", "delete", "provider", coordProviderName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		dumpDebugInfo(coordTaskName)
	})

	It("should delegate work to another agent and collect results", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating an OpenAI Provider")
		createProviderCRD(coordProviderName, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, model)

		By("creating a worker agent that the coordinator can delegate to")
		workerManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"providerRef": {
					"name": "%s"
				},
				"model": {
					"name": "%s"
				}
			}
		}`, workerAgentName, namespace, coordProviderName, model)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(workerManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create worker agent")

		By("creating a coordinator agent with delegation enabled")
		coordManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"providerRef": {
					"name": "%s"
				},
				"model": {
					"name": "%s"
				},
				"coordination": {
					"enabled": true,
					"maxDepth": 2,
					"maxConcurrentChildren": 3,
					"allowedAgents": [
						{"name": "%s"}
					]
				}
			}
		}`, coordinatorAgentName, namespace, coordProviderName, model, workerAgentName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(coordManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create coordinator agent")

		By("creating a coordination task that should delegate work")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "ai",
				"agentRef": {
					"name": "%s"
				},
				"ai": {
					"prompt": "You are a coordinator. Delegate the following task to the agent named '%s': ask it to compute the factorial of 5 and return just the number. Use the delegate_task tool to delegate, then use wait_for_tasks to get the result. Report the final answer.",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, coordTaskName, namespace, coordinatorAgentName, workerAgentName, model, coordProviderName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create coordination task")

		By("waiting for child tasks to be created")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "tasks",
				"-l", fmt.Sprintf("orka.ai/parent-task=%s", coordTaskName),
				"-o", "jsonpath={.items[*].metadata.name}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).NotTo(BeEmpty(),
				"At least one child task should be created via delegation")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for the coordinator task to complete")
		phase := waitForTaskCompletion(coordTaskName, 10*time.Minute)
		Expect(phase).To(BeElementOf("Succeeded", "Failed"),
			"Coordinator task should reach terminal phase")

		By("verifying child task status is tracked on the parent")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", coordTaskName,
				"-o", "jsonpath={.status.childTasks}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty(),
				"Parent task should have childTasks in status")
		}, 30*time.Second, time.Second).Should(Succeed())
	})
})
