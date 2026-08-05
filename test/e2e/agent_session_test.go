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

var _ = Describe("Agent Session Continuity", Ordered, func() {
	const (
		taskName1 = "e2e-session-task-1"
		taskName2 = "e2e-session-task-2"
		agentName = "e2e-session-agent"
		sessionID = "e2e-test-session"
	)

	AfterAll(func() {
		By("cleaning up session agent test resources")
		cmd := exec.Command("kubectl", "delete", "task", taskName1, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		cmd = exec.Command("kubectl", "delete", "task", taskName2, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		cmd = exec.Command("kubectl", "delete", "agent", agentName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		// Results and sessions are stored in SQLite — no ConfigMap cleanup needed
	})

	AfterEach(func() {
		dumpDebugInfo(taskName1, taskName2)
	})

	It("should reference the same session across multiple tasks", func() {
		By("creating an Agent for session test")
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
				},
				"model": {"name": "claude-sonnet-4-20250514"}
			}
		}`, agentName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Agent")

		By("creating the first Task with a sessionID")
		task1Manifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "agent",
				"prompt": "first message in session",
				"agentRef": {
					"name": "%s"
				},
				"sessionRef": {
					"name": "%s",
					"create": true,
					"append": true
				},
				"agentRuntime": {
					"maxTurns": 3
				}
			}
		}`, taskName1, namespace, agentName, sessionID)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(task1Manifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create first Task")

		By("verifying the first task is queued on an ACP v2 RuntimePool")
		verifyACPTaskRuntimeForTask(taskName1, acpTaskExpectation{
			ProviderKind:    "claude",
			WorkspaceIntent: "read",
			MaxTurns:        acpInt32(3),
			AllowBash:       acpBool(false),
			SessionName:     sessionID,
		}, 2*time.Minute)

		By("waiting for the first task to settle so the shared Session lease is available")
		_ = waitForTaskCompletion(taskName1, 5*time.Minute)

		var firstRuntimeSessionUID string
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", taskName1,
				"-o", "jsonpath={.status.execution.runtimeSessionUID}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty())
			firstRuntimeSessionUID = output
		}, 2*time.Minute, time.Second).Should(Succeed())

		By("creating the second Task with the same sessionID")
		task2Manifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "agent",
				"prompt": "second message in session",
				"agentRef": {
					"name": "%s"
				},
				"sessionRef": {
					"name": "%s",
					"create": false,
					"append": true
				},
				"agentRuntime": {
					"maxTurns": 3
				}
			}
		}`, taskName2, namespace, agentName, sessionID)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(task2Manifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create second Task")

		By("verifying the second Task reuses the ACP RuntimeSession identity")
		verifyACPTaskRuntimeForTask(taskName2, acpTaskExpectation{
			ProviderKind:    "claude",
			WorkspaceIntent: "read",
			MaxTurns:        acpInt32(3),
			AllowBash:       acpBool(false),
			SessionName:     sessionID,
		}, 2*time.Minute)
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", taskName2,
				"-o", "jsonpath={.status.execution.runtimeSessionUID}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal(firstRuntimeSessionUID))
		}, 5*time.Minute, time.Second).Should(Succeed())
	})
})
