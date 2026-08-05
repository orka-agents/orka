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

var _ = Describe("Agent Claude ACP Runtime", Ordered, func() {
	const (
		taskName  = "e2e-claude-task"
		agentName = "e2e-claude-agent"
	)

	AfterAll(func() {
		cmd := exec.Command("kubectl", "delete", "task", taskName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "agent", agentName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		dumpDebugInfo(taskName)
	})

	It("queues a Claude task on an ACP v2 RuntimePool without provider credentials in the Agent", func() {
		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {"name": %q, "namespace": %q},
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

		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": %q, "namespace": %q},
			"spec": {
				"type": "agent",
				"prompt": "Read the workspace and report a concise result.",
				"agentRef": {"name": %q},
				"agentRuntime": {"maxTurns": 3},
				"workspace": {"intent": "read"}
			}
		}`, taskName, namespace, agentName)
		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Task")

		verifyACPTaskRuntimeForTask(taskName, acpTaskExpectation{
			ProviderKind:    "claude",
			Model:           "claude-sonnet-4-20250514",
			WorkspaceIntent: "read",
			MaxTurns:        acpInt32(3),
			AllowBash:       acpBool(false),
			Workspace:       &acpWorkspaceExpectation{Intent: "read"},
		}, 2*time.Minute)
		verifyNoJobForTask(taskName, 5*time.Second)
	})
})
