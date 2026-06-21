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
				}
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

		By("verifying harness-wrapper metadata is planned for the first session task")
		verifyHarnessWrapperMetadataForTask(taskName1, map[string]string{
			"runtime":   "claude",
			"wrapper":   "cli",
			"maxTurns":  "3",
			"allowBash": "false",
		}, 2*time.Minute)

		By("verifying the runtime session identity includes the shared session")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", taskName1,
				"-o", "jsonpath={.metadata.annotations.orka\\.ai/harness-wrapper-runtime-session-id}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring(sessionID))
		}, 30*time.Second, time.Second).Should(Succeed())

		By("creating the second Task with the same sessionID")
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

		By("verifying the second Task was accepted and references the same session")
		verifyTask2Accepted := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", taskName2,
				"-o", "jsonpath={.spec.sessionRef.name}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal(sessionID), "Second task should reference the same session")
		}
		Eventually(verifyTask2Accepted, 30*time.Second, time.Second).Should(Succeed())
	})
})
