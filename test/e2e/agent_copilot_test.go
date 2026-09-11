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

var _ = Describe("Agent Copilot ACP Runtime", Ordered, func() {
	const (
		taskName  = "e2e-copilot-task"
		agentName = "e2e-copilot-agent"
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

	It("queues a Copilot task on the configured immutable ACP RuntimePool image", func() {
		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {"name": %q, "namespace": %q},
			"spec": {
				"runtime": {
					"contractVersion": "orka.harness.v2",
					"type": "copilot",
					"defaultMaxTurns": 10,
					"defaultAllowBash": true
				},
				"model": {"name": "gpt-5.3-codex"}
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
				"prompt": "List files without modifying the workspace.",
				"agentRef": {"name": %q},
				"agentRuntime": {"maxTurns": 5},
				"workspace": {"intent": "read"}
			}
		}`, taskName, namespace, agentName)
		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Task")

		verifyACPTaskRuntimeForTask(taskName, acpTaskExpectation{
			ProviderKind:    "copilot",
			Model:           "gpt-5.3-codex",
			WorkspaceIntent: "read",
			RuntimeImage:    acpCopilotRuntimeRef,
			MaxTurns:        acpInt32(5),
			AllowBash:       acpBool(true),
			Workspace:       &acpWorkspaceExpectation{Intent: "read"},
		}, 2*time.Minute)
		verifyNoJobForTask(taskName, 5*time.Second)
	})
})
