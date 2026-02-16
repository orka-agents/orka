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

	"github.com/sozercan/orka/test/utils"
)

var _ = Describe("Agent TTL Auto-Deletion", func() {
	AfterEach(func() {
		dumpDebugInfo("e2e-ttl-task")
	})

	It("should auto-delete the agent after TTL expires with no active tasks", func() {
		const (
			agentName = "e2e-ttl-agent"
			taskName  = "e2e-ttl-task"
		)

		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "task", taskName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "agent", agentName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		By("creating an Agent with a short TTL (30s)")
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
					"defaultMaxTurns": 1
				},
				"ttlAfterLastTask": "30s"
			}
		}`, agentName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create TTL agent")

		By("verifying the agent exists")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "agent", agentName,
				"-n", namespace, "-o", "jsonpath={.metadata.name}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal(agentName))
		}, 15*time.Second, time.Second).Should(Succeed())

		By("creating a quick container task referencing the agent")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "container",
				"image": "busybox:1.36",
				"command": ["echo", "hello"],
				"agentRef": {
					"name": "%s"
				}
			}
		}`, taskName, namespace, agentName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create TTL test task")

		By("waiting for the task to complete")
		phase := waitForTaskCompletion(taskName, 2*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "Container task should succeed")

		By("waiting for the agent to be auto-deleted after TTL (up to 90s)")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "agent", agentName,
				"-n", namespace, "--ignore-not-found")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(BeEmpty(), "Agent should be deleted after TTL")
		}, 90*time.Second, 5*time.Second).Should(Succeed())
	})
})
