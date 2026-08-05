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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/test/utils"
)

var _ = Describe("Negative/Error Cases", Ordered, func() {
	const (
		badAgentRefTask = "e2e-neg-bad-agentref"
		badSecretTask   = "e2e-neg-bad-secret"
		badSecretAgent  = "e2e-neg-bad-secret-agent"
	)

	AfterAll(func() {
		By("cleaning up negative test resources")
		for _, name := range []string{badAgentRefTask, badSecretTask} {
			cmd := exec.Command("kubectl", "delete", "task", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
		cmd := exec.Command("kubectl", "delete", "agent", badSecretAgent, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		dumpDebugInfo(badAgentRefTask, badSecretTask)
	})

	It("should fail a task referencing a non-existent agent", func() {
		By("creating a Task referencing a non-existent agent")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "agent",
				"prompt": "this should fail",
				"agentRef": {
					"name": "non-existent-agent"
				},
				"agentRuntime": {
					"maxTurns": 1
				}
			}
		}`, badAgentRefTask, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Task creation should be accepted by API server")

		By("verifying the task fails gracefully")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", badAgentRefTask,
				"-o", "jsonpath={.status.phase}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Failed"),
				"Task referencing non-existent agent should fail")
		}, 2*time.Minute, time.Second).Should(Succeed())
	})

	It("should fail an ACP task with a non-existent workspace readCredentialRef", func() {
		By("creating an ACP Agent without embedded provider credentials")
		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"runtime": {
					"type": "claude",
					"defaultMaxTurns": 3,
					"defaultAllowBash": false
				},
				"model": {"name": "claude-sonnet-4-20250514"}
			}
		}`, badSecretAgent, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Agent creation should be accepted")

		By("creating a Task with a missing workspace read credential")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "agent",
				"prompt": "this should fail due to missing secret",
				"agentRef": {
					"name": "%s"
				},
				"agentRuntime": {
					"maxTurns": 1
				},
				"workspace": {
					"intent": "read",
					"gitRepo": "https://github.com/orka-agents/orka.git",
					"ref": "d03acb995b6014a6e855181c50b922b65ea8e7ff",
					"readCredentialRef": {"name": "non-existent-secret"}
				}
			}
		}`, badSecretTask, namespace, badSecretAgent)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Task creation should be accepted")

		By("verifying credential resolution fails before an ACP prompt attempt")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", badSecretTask, "-o", "json", "-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			var task corev1alpha1.Task
			g.Expect(json.Unmarshal([]byte(output), &task)).To(Succeed())
			g.Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseFailed))
			g.Expect(task.Status.Execution).NotTo(BeNil())
			g.Expect(task.Status.Execution.State).To(Equal(corev1alpha1.TaskExecutionStateFailed))
			g.Expect(task.Status.Execution.Outcome).To(Equal(corev1alpha1.TaskExecutionOutcomeFailed))
			g.Expect(task.Status.Execution.Reason).To(Equal(corev1alpha1.TaskExecutionReason("InvalidWorkspace")))
			g.Expect(task.Status.Execution.Attempt).To(BeZero())
			g.Expect(task.Status.Execution.PromptID).To(BeEmpty())
			g.Expect(task.Status.Execution.RuntimePoolName).To(BeEmpty())
			g.Expect(task.Status.Delivery).NotTo(BeNil())
			g.Expect(task.Status.Delivery.State).To(Equal(corev1alpha1.TaskDeliveryStateNotRequested))
			g.Expect(task.Status.Delivery.Outcome).To(Equal(corev1alpha1.TaskDeliveryOutcomeNotRequested))
			g.Expect(task.Status.Message).To(ContainSubstring("non-existent-secret"))
		}, 2*time.Minute, time.Second).Should(Succeed())
		verifyNoJobForTask(badSecretTask, 5*time.Second)
	})
})
