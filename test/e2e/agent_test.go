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

var _ = Describe("Agent Task", Ordered, func() {
	const (
		taskName  = "e2e-agent-task"
		agentName = "e2e-test-agent"
	)

	// Clean up resources after the test
	AfterAll(func() {
		By("deleting the agent task")
		cmd := exec.Command("kubectl", "delete", "task", taskName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("deleting the agent")
		cmd = exec.Command("kubectl", "delete", "agent", agentName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		// Results are stored in SQLite — no ConfigMap cleanup needed
	})

	AfterEach(func() {
		dumpDebugInfo(taskName)
	})

	It("should start a harness-wrapper turn for an agent-type task", func() {
		By("creating an Agent CRD with claude runtime")
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
					"defaultMaxTurns": 5,
					"defaultAllowBash": false
				}
			}
		}`, agentName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Agent")

		By("creating a Task with type agent referencing the Agent")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "agent",
				"prompt": "echo hello world",
				"agentRef": {
					"name": "%s"
				},
				"agentRuntime": {
					"maxTurns": 3
				}
			}
		}`, taskName, namespace, agentName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create agent Task")

		By("verifying harness-wrapper metadata is planned for the agent task")
		verifyHarnessWrapperMetadataForTask(taskName, map[string]string{
			"runtime":   "claude",
			"wrapper":   "cli",
			"maxTurns":  "3",
			"allowBash": "false",
		}, 2*time.Minute)

		By("verifying the Task status transitions from Pending")
		verifyTaskRunning := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", taskName,
				"-o", "jsonpath={.status.phase}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(BeElementOf("Running", "Succeeded", "Failed"),
				"Task should transition from Pending")
		}
		Eventually(verifyTaskRunning, 2*time.Minute, time.Second).Should(Succeed())

		By("verifying the Task does not use a worker Job")
		verifyNoJobForTask(taskName, 5*time.Second)
	})
})

// envVar is a simplified representation of a Kubernetes environment variable
// for parsing JSON output from kubectl.
type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// stringReader returns a strings.Reader for use as command stdin.
func stringReader(s string) *strings.Reader {
	return strings.NewReader(s)
}
