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

var _ = Describe("ACP Workspace Profiles", func() {
	const prefix = "e2e-ws-adv-"

	cleanup := func(taskName, agentName string) {
		dumpDebugInfo(taskName)
		for _, resource := range []struct{ kind, name string }{
			{"task", taskName},
			{"agent", agentName},
		} {
			cmd := exec.Command("kubectl", "delete", resource.kind, resource.name,
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
	}

	createClaudeAgent := func(agentName string) {
		manifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {"name": %q, "namespace": %q},
			"spec": {
				"runtime": {
					"contractVersion": "orka.harness.v2",
					"type": "claude",
					"defaultMaxTurns": 5,
					"defaultAllowBash": false
				},
				"model": {"name": "claude-sonnet-4-20250514"}
			}
		}`, agentName, namespace)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Agent")
	}

	It("uses a readCredentialRef only at the governed workspace boundary", func() {
		skipIfNoKey("E2E_GITHUB_TOKEN")
		agentName := prefix + "credential-agent"
		taskName := prefix + "credential-task"
		DeferCleanup(cleanup, taskName, agentName)
		createClaudeAgent(agentName)

		manifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": %q, "namespace": %q},
			"spec": {
				"type": "agent",
				"agentRef": {"name": %q},
				"prompt": "List files without modifying the workspace.",
				"agentRuntime": {"maxTurns": 3},
				"workspace": {
					"intent": "read",
					"gitRepo": "https://github.com/sozercan/ayna",
					"readCredentialRef": {"name": "e2e-github-secret"}
				}
			}
		}`, taskName, namespace, agentName)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Task")

		verifyACPTaskRuntimeForTask(taskName, acpTaskExpectation{
			ProviderKind:    "claude",
			WorkspaceIntent: "read",
			MaxTurns:        acpInt32(3),
			AllowBash:       acpBool(false),
			Workspace: &acpWorkspaceExpectation{
				Intent:             "read",
				GitRepo:            "https://github.com/sozercan/ayna",
				ReadCredentialName: "e2e-github-secret",
			},
		}, 2*time.Minute)
		verifyNoJobForTask(taskName, 5*time.Second)
	})

	It("includes subPath in the immutable read workspace profile", func() {
		agentName := prefix + "subpath-agent"
		taskName := prefix + "subpath-task"
		DeferCleanup(cleanup, taskName, agentName)
		createClaudeAgent(agentName)

		manifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": %q, "namespace": %q},
			"spec": {
				"type": "agent",
				"agentRef": {"name": %q},
				"prompt": "Inspect the configured subdirectory.",
				"agentRuntime": {"maxTurns": 3},
				"workspace": {
					"intent": "read",
					"gitRepo": "https://github.com/example/monorepo",
					"subPath": "packages/backend"
				}
			}
		}`, taskName, namespace, agentName)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Task")

		verifyACPTaskRuntimeForTask(taskName, acpTaskExpectation{
			ProviderKind:    "claude",
			WorkspaceIntent: "read",
			MaxTurns:        acpInt32(3),
			AllowBash:       acpBool(false),
			Workspace: &acpWorkspaceExpectation{
				Intent:  "read",
				GitRepo: "https://github.com/example/monorepo",
				SubPath: "packages/backend",
			},
		}, 2*time.Minute)
	})

	It("keeps an exact source ref on the top-level Task workspace", func() {
		agentName := prefix + "ref-agent"
		taskName := prefix + "ref-task"
		DeferCleanup(cleanup, taskName, agentName)
		createClaudeAgent(agentName)

		manifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": %q, "namespace": %q},
			"spec": {
				"type": "agent",
				"agentRef": {"name": %q},
				"prompt": "Inspect the pinned revision.",
				"agentRuntime": {"maxTurns": 3},
				"workspace": {
					"intent": "read",
					"gitRepo": "https://github.com/example/repo",
					"ref": "abc123def456"
				}
			}
		}`, taskName, namespace, agentName)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Task")

		verifyACPTaskRuntimeForTask(taskName, acpTaskExpectation{
			ProviderKind:    "claude",
			WorkspaceIntent: "read",
			MaxTurns:        acpInt32(3),
			AllowBash:       acpBool(false),
			Workspace: &acpWorkspaceExpectation{
				Intent:  "read",
				GitRepo: "https://github.com/example/repo",
				Ref:     "abc123def456",
			},
		}, 2*time.Minute)
	})

	It("models publication and PR fields as a write delivery profile", func() {
		agentName := prefix + "publication-agent"
		taskName := prefix + "publication-task"
		publicationCredentialName := prefix + "publication-credential"
		forgeCredentialName := prefix + "forge-credential"
		DeferCleanup(func() {
			cleanup(taskName, agentName)
			for _, secretName := range []string{publicationCredentialName, forgeCredentialName} {
				cmd := exec.Command("kubectl", "delete", "secret", secretName,
					"-n", namespace, "--ignore-not-found", "--timeout=30s")
				_, _ = utils.Run(cmd)
			}
		})
		Expect(createK8sSecret(publicationCredentialName, namespace, map[string]string{
			"token": "placeholder",
		})).To(Succeed(), "Failed to create dummy publication credential Secret")
		Expect(createK8sSecret(forgeCredentialName, namespace, map[string]string{
			"token": "placeholder",
		})).To(Succeed(), "Failed to create dummy forge credential Secret")
		createClaudeAgent(agentName)

		manifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": %q, "namespace": %q},
			"spec": {
				"type": "agent",
				"agentRef": {"name": %q},
				"prompt": "Prepare a reviewed change.",
				"agentRuntime": {"maxTurns": 3},
				"workspace": {
					"intent": "write",
					"gitRepo": "https://github.com/example/source",
					"publicationGitRepo": "https://github.com/example/fork",
					"publicationCredentialRef": {"name": %q, "key": "token"},
					"forgeCredentialRef": {"name": %q, "key": "token"},
					"pushBranch": "e2e/test-branch",
					"prBaseBranch": "develop",
					"createPR": true
				}
			}
		}`, taskName, namespace, agentName, publicationCredentialName, forgeCredentialName)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Task")

		verifyACPTaskRuntimeForTask(taskName, acpTaskExpectation{
			ProviderKind:    "claude",
			WorkspaceIntent: "write",
			MaxTurns:        acpInt32(3),
			AllowBash:       acpBool(false),
			Workspace: &acpWorkspaceExpectation{
				Intent:                    "write",
				GitRepo:                   "https://github.com/example/source",
				PublicationGitRepo:        "https://github.com/example/fork",
				PublicationCredentialName: publicationCredentialName,
				PushBranch:                "e2e/test-branch",
				PRBaseBranch:              "develop",
				CreatePR:                  acpBool(true),
			},
		}, 2*time.Minute)
	})

	It("queues sessionRef tasks without legacy turn annotations", func() {
		agentName := prefix + "session-agent"
		taskName := prefix + "session-task"
		DeferCleanup(cleanup, taskName, agentName)
		createClaudeAgent(agentName)

		manifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": %q, "namespace": %q},
			"spec": {
				"type": "agent",
				"agentRef": {"name": %q},
				"sessionRef": {"name": "e2e-workspace-session", "create": true, "append": true},
				"prompt": "Start a governed ACP session.",
				"agentRuntime": {"maxTurns": 3},
				"workspace": {"intent": "read"}
			}
		}`, taskName, namespace, agentName)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Task")

		verifyACPTaskRuntimeForTask(taskName, acpTaskExpectation{
			ProviderKind:    "claude",
			WorkspaceIntent: "read",
			MaxTurns:        acpInt32(3),
			AllowBash:       acpBool(false),
			SessionName:     "e2e-workspace-session",
			Workspace:       &acpWorkspaceExpectation{Intent: "read"},
		}, 2*time.Minute)
		verifyNoJobForTask(taskName, 5*time.Second)
	})
})
