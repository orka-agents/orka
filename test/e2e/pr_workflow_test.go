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

var _ = Describe("ACP PR Delivery Profile", func() {
	const (
		agentName                 = "e2e-pr-delivery-agent"
		taskName                  = "e2e-pr-delivery-task"
		publicationCredentialName = "e2e-pr-delivery-publication"
		forgeCredentialName       = "e2e-pr-delivery-forge"
	)

	AfterEach(func() {
		dumpDebugInfo(taskName)
		for _, resource := range []struct{ kind, name string }{
			{kind: "task", name: taskName},
			{kind: "agent", name: agentName},
			{kind: "secret", name: publicationCredentialName},
			{kind: "secret", name: forgeCredentialName},
		} {
			cmd := exec.Command("kubectl", "delete", resource.kind, resource.name,
				"-n", namespace, "--ignore-not-found", "--timeout=30s")
			_, _ = utils.Run(cmd)
		}
	})

	It("queues publication and PR fields as a governed ACP write delivery", func() {
		Expect(createK8sSecret(publicationCredentialName, namespace, map[string]string{
			"token": "placeholder",
		})).To(Succeed(), "Failed to create dummy publication credential Secret")
		Expect(createK8sSecret(forgeCredentialName, namespace, map[string]string{
			"token": "placeholder",
		})).To(Succeed(), "Failed to create dummy forge credential Secret")

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
				"prompt": "Prepare a reviewed change.",
				"agentRef": {"name": %q},
				"agentRuntime": {"maxTurns": 3},
				"workspace": {
					"intent": "write",
					"gitRepo": "https://github.com/sozercan/ayna",
					"publicationGitRepo": "https://github.com/fork/ayna",
					"publicationCredentialRef": {"name": %q, "key": "token"},
					"forgeCredentialRef": {"name": %q, "key": "token"},
					"pushBranch": "e2e/test-branch",
					"prBaseBranch": "develop",
					"createPR": true
				}
			}
		}`, taskName, namespace, agentName, publicationCredentialName, forgeCredentialName)
		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Task")

		verifyACPTaskRuntimeForTask(taskName, acpTaskExpectation{
			ProviderKind:    "claude",
			WorkspaceIntent: "write",
			MaxTurns:        acpInt32(3),
			AllowBash:       acpBool(false),
			Workspace: &acpWorkspaceExpectation{
				Intent:                    "write",
				GitRepo:                   "https://github.com/sozercan/ayna",
				PublicationGitRepo:        "https://github.com/fork/ayna",
				PublicationCredentialName: publicationCredentialName,
				PushBranch:                "e2e/test-branch",
				PRBaseBranch:              "develop",
				CreatePR:                  acpBool(true),
			},
			DeliveryState:   acpDeliveryState("NotRequested"),
			DeliveryOutcome: acpDeliveryOutcome("NotRequested"),
		}, 2*time.Minute)
		verifyNoJobForTask(taskName, 5*time.Second)
	})

	It("rejects createPR on a read-only workspace", func() {
		manifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": "e2e-pr-invalid", "namespace": %q},
			"spec": {
				"type": "agent",
				"prompt": "invalid",
				"agentRef": {"name": "unused"},
				"workspace": {"intent": "read", "createPR": true}
			}
		}`, namespace)
		cmd := exec.Command("kubectl", "apply", "--dry-run=server", "-f", "-")
		cmd.Stdin = stringReader(manifest)
		output, err := utils.Run(cmd)
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(output)).To(ContainSubstring("createpr requires write"))
	})
})
