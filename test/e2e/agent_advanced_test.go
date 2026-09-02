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

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/test/utils"
)

var _ = Describe("Agent Advanced Features", func() {
	const prefix = "e2e-agent-adv-"

	AfterEach(func() {
		// Clean up all resources with the prefix
		for _, kind := range []string{"task", "agent", "provider", "skill", "configmap"} {
			cmd := exec.Command("kubectl", "delete", kind, "-l", "e2e-group=agent-adv",
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
	})

	It("should reject skill-backed built-in ACP tasks before RuntimePool demand", func() {
		skillName := prefix + "skill"
		agentName := prefix + "skill-agent"
		taskName := prefix + "skill-task"

		defer dumpDebugInfo(taskName)

		By("creating a Skill CRD with inline content")
		skillManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Skill",
			"metadata": {
				"name": "%s",
				"namespace": "%s",
				"labels": {"e2e-group": "agent-adv"}
			},
			"spec": {
				"displayName": "E2E Agent Advanced Skill",
				"description": "A test skill for agent advanced features",
				"content": {
					"inline": "You are an expert Go developer specializing in Kubernetes controllers."
				}
			}
		}`, skillName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(skillManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Skill")

		By("creating an Agent with skills referencing the Skill CRD")
		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s",
				"labels": {"e2e-group": "agent-adv"}
			},
			"spec": {
				"runtime": {
					"contractVersion": "orka.harness.v2",
					"type": "claude",
					"defaultMaxTurns": 3,
					"defaultAllowBash": false
				},
				"model": {"name": "claude-sonnet-4-20250514"},
				"skills": [{
					"name": "%s"
				}]
			}
		}`, agentName, namespace, skillName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Agent with skills")

		By("verifying the Agent Ready condition becomes true with the referenced Skill")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "agent", agentName,
				"-n", namespace, "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("True"))
		}, 30*time.Second, time.Second).Should(Succeed())

		By("creating an agent task referencing the agent")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s",
				"labels": {"e2e-group": "agent-adv"}
			},
			"spec": {
				"type": "agent",
				"prompt": "Say hello.",
				"agentRef": {"name": "%s"},
				"agentRuntime": {"maxTurns": 1}
			}
		}`, taskName, namespace, agentName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create task with skill agent")

		By("verifying unsupported skills fail before ACP queue admission")
		Eventually(func(g Gomega) {
			task := fetchTaskForACPAssertion(g, taskName)
			g.Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseFailed))
			g.Expect(task.Status.Message).To(Equal(
				"built-in ACP runtimes do not support Agent.spec.skills; refusing to omit declared skills",
			))
			g.Expect(task.Status.Attempts).To(BeZero())
			g.Expect(task.Status.Execution).NotTo(BeNil())
			g.Expect(task.Status.Execution.State).To(Equal(corev1alpha1.TaskExecutionStateFailed))
			g.Expect(task.Status.Execution.Reason).To(Equal(corev1alpha1.TaskExecutionReason("InvalidRuntimeProfile")))
			g.Expect(task.Status.Execution.Attempt).To(BeZero())
			g.Expect(task.Status.Execution.PromptID).To(BeEmpty())
			g.Expect(task.Status.Execution.RuntimePoolName).To(BeEmpty())
			g.Expect(task.Status.Delivery).NotTo(BeNil())
			g.Expect(task.Status.Delivery.State).To(Equal(corev1alpha1.TaskDeliveryStateNotRequested))
			g.Expect(task.Status.Delivery.Outcome).To(Equal(corev1alpha1.TaskDeliveryOutcomeNotRequested))
		}, 30*time.Second, time.Second).Should(Succeed())

		By("verifying the Task does not use a worker Job")
		verifyNoJobForTask(taskName, 5*time.Second)
	})

	It("should reject resource-backed CLI runtime tasks before creating a Job", func() {
		agentName := prefix + "res-agent"
		taskName := prefix + "res-task"

		defer dumpDebugInfo(taskName)

		By("creating an Agent with resource constraints")
		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s",
				"labels": {"e2e-group": "agent-adv"}
			},
			"spec": {
				"runtime": {
					"contractVersion": "orka.harness.v2",
					"type": "claude",
					"defaultMaxTurns": 3,
					"defaultAllowBash": false
				},
				"model": {"name": "claude-sonnet-4-20250514"},
				"resources": {
					"requests": {"memory": "128Mi", "cpu": "100m"},
					"limits": {"memory": "256Mi", "cpu": "200m"}
				}
			}
		}`, agentName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Agent with resources")

		By("creating an agent-type task referencing the Agent")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s",
				"labels": {"e2e-group": "agent-adv"}
			},
			"spec": {
				"type": "agent",
				"prompt": "echo hello",
				"agentRef": {"name": "%s"},
				"agentRuntime": {"maxTurns": 1}
			}
		}`, taskName, namespace, agentName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create agent task with resources")

		By("verifying unsupported resource-backed CLI runtime task fails without a Job")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", taskName,
				"-o", "jsonpath={.status.phase}{\"|\"}{.status.message}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("Failed|"))
			g.Expect(output).To(ContainSubstring("custom Kubernetes resources"))
		}, 30*time.Second, time.Second).Should(Succeed())

		By("verifying the Task does not use a worker Job")
		verifyNoJobForTask(taskName, 5*time.Second)
	})

	It("should support sessionRef with maxMessages", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		providerName := prefix + "provider"
		taskName1 := prefix + "session-t1"
		taskName2 := prefix + "session-t2"
		sessionName := prefix + "session"

		defer dumpDebugInfo(taskName1, taskName2)

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating a Provider for the session test")
		createProviderCRD(providerName, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, model)

		// Label the provider for cleanup
		cmd := exec.Command("kubectl", "label", "provider", providerName,
			"e2e-group=agent-adv", "-n", namespace, "--overwrite")
		_, _ = utils.Run(cmd)

		By("creating the first AI task with sessionRef (create: true)")
		task1Manifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s",
				"labels": {"e2e-group": "agent-adv"}
			},
			"spec": {
				"type": "ai",
				"sessionRef": {
					"name": "%s",
					"create": true
				},
				"ai": {
					"prompt": "What is 2+2? Reply with just the number.",
					"model": "%s",
					"providerRef": {"name": "%s"}
				}
			}
		}`, taskName1, namespace, sessionName, model, providerName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(task1Manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create first session task")

		By("waiting for the first task to complete")
		phase := waitForTaskCompletion(taskName1, 5*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "First session task should succeed")

		By("creating the second task with same sessionRef and maxMessages: 2")
		task2Manifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s",
				"labels": {"e2e-group": "agent-adv"}
			},
			"spec": {
				"type": "ai",
				"sessionRef": {
					"name": "%s",
					"create": false,
					"maxMessages": 2
				},
				"ai": {
					"prompt": "What was my previous question? Reply briefly.",
					"model": "%s",
					"providerRef": {"name": "%s"}
				}
			}
		}`, taskName2, namespace, sessionName, model, providerName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(task2Manifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create second session task with maxMessages")

		By("waiting for the second task to complete")
		phase = waitForTaskCompletion(taskName2, 5*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "Second session task with maxMessages should succeed")

		By("verifying the second task has a result")
		verifyResultAvailable(taskName2)
	})
})
