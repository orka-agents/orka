//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/orka-agents/orka/test/utils"
)

var _ = Describe("Workspace Advanced Features", func() {
	const prefix = "e2e-ws-adv-"

	// --- Test 1: Private repo with gitSecretRef ---

	Describe("Private repo with gitSecretRef", func() {
		const (
			agentName = prefix + "gitsecret-agent"
			taskName  = prefix + "gitsecret-task"
		)

		AfterEach(func() {
			dumpDebugInfo(taskName)
			for _, r := range []struct{ kind, name string }{
				{"task", taskName},
				{"agent", agentName},
			} {
				cmd := exec.Command("kubectl", "delete", r.kind, r.name,
					"-n", namespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			}
		})

		It("should mount git credentials when gitSecretRef is configured", func() {
			skipIfNoKey("E2E_GITHUB_TOKEN")

			By("creating an Agent with claude runtime")
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

			By("creating an agent task with workspace gitRepo and gitSecretRef")
			taskManifest := fmt.Sprintf(`{
				"apiVersion": "core.orka.ai/v1alpha1",
				"kind": "Task",
				"metadata": {
					"name": "%s",
					"namespace": "%s"
				},
				"spec": {
					"type": "agent",
					"agentRef": {"name": "%s"},
					"prompt": "list files",
					"agentRuntime": {
						"maxTurns": 3,
						"workspace": {
							"gitRepo": "https://github.com/sozercan/ayna",
							"gitSecretRef": {"name": "e2e-github-secret"}
						}
					}
				}
			}`, taskName, namespace, agentName)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = stringReader(taskManifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create Task")

			By("verifying harness-wrapper workspace metadata")
			verifyHarnessWrapperMetadataForTask(taskName, map[string]string{
				"runtime":   "claude",
				"wrapper":   "cli",
				"gitRepo":   "https://github.com/sozercan/ayna",
				"maxTurns":  "3",
				"allowBash": "false",
			}, 2*time.Minute)

			By("verifying the Task does not use a worker Job")
			verifyNoJobForTask(taskName, 5*time.Second)
		})
	})

	// --- Test 2: subPath workspace ---

	Describe("subPath workspace", func() {
		const (
			agentName = prefix + "subpath-agent"
			taskName  = prefix + "subpath-task"
		)

		AfterEach(func() {
			dumpDebugInfo(taskName)
			for _, r := range []struct{ kind, name string }{
				{"task", taskName},
				{"agent", agentName},
			} {
				cmd := exec.Command("kubectl", "delete", r.kind, r.name,
					"-n", namespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			}
		})

		It("should set ORKA_WORKSPACE_SUBPATH when subPath is configured", func() {
			By("creating an Agent with claude runtime")
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

			By("creating an agent task with workspace subPath")
			taskManifest := fmt.Sprintf(`{
				"apiVersion": "core.orka.ai/v1alpha1",
				"kind": "Task",
				"metadata": {
					"name": "%s",
					"namespace": "%s"
				},
				"spec": {
					"type": "agent",
					"agentRef": {"name": "%s"},
					"prompt": "list files",
					"agentRuntime": {
						"maxTurns": 3,
						"workspace": {
							"gitRepo": "https://github.com/sozercan/ayna",
							"subPath": "docs"
						}
					}
				}
			}`, taskName, namespace, agentName)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = stringReader(taskManifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create Task")

			By("verifying harness-wrapper workspace metadata")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "task", taskName, "-n", namespace, "-o", "json")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				var task struct {
					Metadata struct {
						Annotations map[string]string `json:"annotations"`
					} `json:"metadata"`
				}
				err = json.Unmarshal([]byte(output), &task)
				g.Expect(err).NotTo(HaveOccurred())

				rawMetadata := task.Metadata.Annotations["orka.ai/harness-wrapper-metadata"]
				g.Expect(rawMetadata).NotTo(BeEmpty())
				metadata := map[string]string{}
				err = json.Unmarshal([]byte(rawMetadata), &metadata)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(metadata).To(HaveKeyWithValue("gitRepo", "https://github.com/sozercan/ayna"))
				g.Expect(metadata).To(HaveKeyWithValue("workspaceSubPath", "docs"))
			}, 30*time.Second, time.Second).Should(Succeed())
		})
	})

	// --- Test 3: Specific ref checkout ---

	Describe("Specific ref checkout", func() {
		const (
			agentName = prefix + "ref-agent"
			taskName  = prefix + "ref-task"
		)

		AfterEach(func() {
			dumpDebugInfo(taskName)
			for _, r := range []struct{ kind, name string }{
				{"task", taskName},
				{"agent", agentName},
			} {
				cmd := exec.Command("kubectl", "delete", r.kind, r.name,
					"-n", namespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			}
		})

		It("should set ORKA_GIT_REF when workspace ref is configured", func() {
			By("creating an Agent with claude runtime")
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

			By("creating an agent task with workspace ref set to main")
			taskManifest := fmt.Sprintf(`{
				"apiVersion": "core.orka.ai/v1alpha1",
				"kind": "Task",
				"metadata": {
					"name": "%s",
					"namespace": "%s"
				},
				"spec": {
					"type": "agent",
					"agentRef": {"name": "%s"},
					"prompt": "list files",
					"agentRuntime": {
						"maxTurns": 3,
						"workspace": {
							"gitRepo": "https://github.com/sozercan/ayna",
							"ref": "main"
						}
					}
				}
			}`, taskName, namespace, agentName)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = stringReader(taskManifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create Task")

			By("verifying harness-wrapper workspace ref metadata")
			verifyHarnessWrapperMetadataForTask(taskName, map[string]string{
				"runtime":   "claude",
				"wrapper":   "cli",
				"gitRepo":   "https://github.com/sozercan/ayna",
				"gitRef":    "main",
				"maxTurns":  "3",
				"allowBash": "false",
			}, 2*time.Minute)

			By("verifying the Task does not use a worker Job")
			verifyNoJobForTask(taskName, 5*time.Second)
		})
	})

	// --- Test 4: Structural env vars (forkRepo, prBaseBranch) ---

	Describe("Structural env vars for forkRepo and prBaseBranch", func() {
		const (
			agentName = prefix + "fork-agent"
			taskName  = prefix + "fork-task"
		)

		AfterEach(func() {
			dumpDebugInfo(taskName)
			cmd := exec.Command("kubectl", "delete", "task", taskName,
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "agent", agentName,
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should set ORKA_FORK_REPO and ORKA_PR_BASE_BRANCH env vars on the Job", func() {
			By("creating an Agent with claude runtime")
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

			By("creating a Task with forkRepo and prBaseBranch in workspace")
			taskManifest := fmt.Sprintf(`{
				"apiVersion": "core.orka.ai/v1alpha1",
				"kind": "Task",
				"metadata": {
					"name": "%s",
					"namespace": "%s"
				},
				"spec": {
					"type": "agent",
					"prompt": "list files",
					"agentRef": {"name": "%s"},
					"agentRuntime": {
						"maxTurns": 3,
						"workspace": {
							"gitRepo": "https://github.com/upstream/repo",
							"forkRepo": "https://github.com/fork/repo",
							"prBaseBranch": "develop"
						}
					}
				}
			}`, taskName, namespace, agentName)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = stringReader(taskManifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create Task")

			By("verifying harness-wrapper fork workspace metadata")
			verifyHarnessWrapperMetadataForTask(taskName, map[string]string{
				"runtime":      "claude",
				"wrapper":      "cli",
				"gitRepo":      "https://github.com/upstream/repo",
				"forkRepo":     "https://github.com/fork/repo",
				"prBaseBranch": "develop",
				"maxTurns":     "3",
				"allowBash":    "false",
			}, 2*time.Minute)

			By("verifying the Task does not use a worker Job")
			verifyNoJobForTask(taskName, 5*time.Second)
		})
	})

	// --- Test 5: Init container for session transcript ---

	Describe("Init container for session transcript", func() {
		const (
			agentName = prefix + "session-agent"
			taskName  = prefix + "session-task"
			sessionID = prefix + "test-session"
		)

		AfterEach(func() {
			dumpDebugInfo(taskName)
			cmd := exec.Command("kubectl", "delete", "task", taskName,
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "agent", agentName,
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should start a harness-wrapper turn when sessionRef is set", func() {
			By("creating an Agent with claude runtime")
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

			By("creating a Task with sessionRef")
			taskManifest := fmt.Sprintf(`{
				"apiVersion": "core.orka.ai/v1alpha1",
				"kind": "Task",
				"metadata": {
					"name": "%s",
					"namespace": "%s"
				},
				"spec": {
					"type": "agent",
					"prompt": "say hello",
					"agentRef": {"name": "%s"},
					"sessionRef": {
						"name": "%s",
						"create": true,
						"append": true
					},
					"agentRuntime": {
						"maxTurns": 3
					}
				}
			}`, taskName, namespace, agentName, sessionID)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = stringReader(taskManifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create Task")

			By("verifying harness-wrapper metadata is planned for the session task")
			verifyHarnessWrapperMetadataForTask(taskName, map[string]string{
				"runtime":   "claude",
				"wrapper":   "cli",
				"maxTurns":  "3",
				"allowBash": "false",
			}, 2*time.Minute)

			By("verifying the Task does not use a worker Job")
			verifyNoJobForTask(taskName, 5*time.Second)
		})
	})
})
