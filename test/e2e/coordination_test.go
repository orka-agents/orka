//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/orka-agents/orka/test/utils"
)

const coordinationDelegateMarker = "[e2e:coord-delegate]"

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
					"prompt": "You are a coordinator. Delegate the following task to the agent named '%s': ask it to compute the factorial of 5 and return just the number. Use the delegate_task tool to delegate, then use wait_for_tasks to get the result. Report the final answer. %s",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, coordTaskName, namespace, coordinatorAgentName, workerAgentName, coordinationDelegateMarker, model, coordProviderName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create coordination task")

		if e2eMockOpenAI {
			By("scripting the coordinator to wait for its generated child Task")
			child := waitForCoordinationChild(coordTaskName, "[e2e:coord-factorial]", 3*time.Minute)
			injectMockLLMFixtures(coordinationMockToolCall(coordinationDelegateMarker, "e2e-coord-task-hold",
				"wait_for_tasks", map[string]any{"tasks": []string{child}, "timeout": "5m"}))
		}

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

		if e2eMockOpenAI {
			By("verifying the coordinator collected the child's result through wait_for_tasks")
			Expect(phase).To(Equal("Succeeded"))
			waitForTaskPhase(waitForCoordinationChild(coordTaskName, "[e2e:coord-factorial]", time.Minute),
				"Succeeded", time.Minute)
			expectCoordinationToolResult(coordinationDelegateMarker, "wait_for_tasks", "E2E-FACTORIAL-120", time.Minute)
		}
	})
})

func coordinationMockToolCall(marker, toolResultContains, tool string, args map[string]any) map[string]any {
	return map[string]any{
		"match":    map[string]any{"userMessage": marker, "toolResultContains": toolResultContains},
		"response": map[string]any{"toolCalls": []map[string]any{{"name": tool, "arguments": args}}},
	}
}

func coordinationMockText(marker, toolResultContains, content string) map[string]any {
	return map[string]any{
		"match":    map[string]any{"userMessage": marker, "toolResultContains": toolResultContains},
		"response": map[string]any{"content": content},
	}
}

// waitForCoordinationChild returns the child Task of parent whose prompt
// carries marker.
func waitForCoordinationChild(parent, marker string, timeout time.Duration) string {
	var name string
	EventuallyWithOffset(1, func(g Gomega) {
		output, err := utils.Run(exec.Command("kubectl", "get", "tasks", "-n", namespace,
			"-l", fmt.Sprintf("orka.ai/parent-task=%s", parent), "-o", "json"))
		g.Expect(err).NotTo(HaveOccurred())
		var tasks struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Spec struct {
					Prompt string `json:"prompt"`
				} `json:"spec"`
			} `json:"items"`
		}
		g.Expect(json.Unmarshal([]byte(output), &tasks)).To(Succeed())
		for _, task := range tasks.Items {
			if strings.Contains(task.Spec.Prompt, marker) {
				name = task.Metadata.Name
				return
			}
		}
		g.Expect(name).NotTo(BeEmpty(), "no child of %s carries %s", parent, marker)
	}, timeout, 500*time.Millisecond).Should(Succeed())
	return name
}

// expectCoordinationToolResult waits until a toolName result containing want
// reached the model in the conversation marked by marker. Unlike
// expectMockLLMToolResult it checks every call, since coordinators call
// wait_for_tasks more than once.
func expectCoordinationToolResult(marker, toolName, want string, timeout time.Duration) {
	EventuallyWithOffset(1, func(g Gomega) {
		requests, err := mockLLMRequestsFor(marker)
		g.Expect(err).NotTo(HaveOccurred())
		var results []string
		for _, request := range requests {
			results = append(results, request.ToolResults(toolName)...)
		}
		g.Expect(results).To(ContainElement(ContainSubstring(want)),
			"no %s result containing %q reached the model for %q", toolName, want, marker)
	}, timeout, 2*time.Second).Should(Succeed())
}
