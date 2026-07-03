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

var _ = Describe("Agent Workspace", Ordered, func() {
	const (
		taskName  = "e2e-workspace-task"
		agentName = "e2e-workspace-agent"
	)

	AfterAll(func() {
		By("cleaning up workspace agent test resources")
		cmd := exec.Command("kubectl", "delete", "task", taskName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		cmd = exec.Command("kubectl", "delete", "agent", agentName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		// Results are stored in SQLite — no ConfigMap cleanup needed
	})

	AfterEach(func() {
		dumpDebugInfo(taskName)
	})

	It("should configure workspace volumes and env vars for git clone", func() {
		By("creating an Agent with claude runtime for workspace test")
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

		By("creating a Task with workspace config (gitRepo and branch)")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "agent",
				"prompt": "review the code",
				"agentRef": {
					"name": "%s"
				},
				"agentRuntime": {
					"maxTurns": 3,
					"workspace": {
						"gitRepo": "https://github.com/example/repo",
						"branch": "main"
					}
				}
			}
		}`, taskName, namespace, agentName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Task with workspace config")

		By("verifying harness-wrapper workspace metadata")
		verifyHarnessWrapperMetadataForTask(taskName, map[string]string{
			"runtime":   "claude",
			"wrapper":   "cli",
			"gitRepo":   "https://github.com/example/repo",
			"gitBranch": "main",
			"maxTurns":  "3",
			"allowBash": "false",
		}, 2*time.Minute)

		By("verifying the Task does not use a worker Job")
		verifyNoJobForTask(taskName, 5*time.Second)
	})
})
