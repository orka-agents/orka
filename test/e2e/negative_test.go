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

	It("should fail an agent task with a non-existent secretRef", func() {
		By("creating an Agent with a non-existent secretRef")
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
				"secretRef": {
					"name": "non-existent-secret"
				}
			}
		}`, badSecretAgent, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Agent creation should be accepted")

		By("creating a Task referencing the agent with bad secret")
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
				}
			}
		}`, badSecretTask, namespace, badSecretAgent)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Task creation should be accepted")

		By("verifying the task fails")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", badSecretTask,
				"-o", "jsonpath={.status.phase}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Failed"),
				"Task with non-existent secret should fail")
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
	})
})
