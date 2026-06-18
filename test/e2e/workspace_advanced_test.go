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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/sozercan/orka/test/utils"
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

			By("verifying a Job is created")
			verifyJobCreatedForTask(taskName, 2*time.Minute)

			By("verifying workspace env vars and git credential volume are configured")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "jobs",
					"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
					"-o", "jsonpath={.items[0].spec.template.spec.containers[0].env}",
					"-n", namespace)
				envOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(envOutput).NotTo(BeEmpty())

				var envVars []envVar
				err = json.Unmarshal([]byte(envOutput), &envVars)
				g.Expect(err).NotTo(HaveOccurred())

				envMap := make(map[string]string)
				for _, e := range envVars {
					envMap[e.Name] = e.Value
				}
				g.Expect(envMap).To(HaveKeyWithValue("ORKA_GIT_REPO", "https://github.com/sozercan/ayna"))

				cmd = exec.Command("kubectl", "get", "jobs",
					"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
					"-o", "jsonpath={.items[0].spec.template.spec.volumes}",
					"-n", namespace)
				volOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(volOutput).To(ContainSubstring(`"name":"git-credentials"`))
				g.Expect(volOutput).To(ContainSubstring(`"secretName":"e2e-github-secret"`))
			}, 30*time.Second, time.Second).Should(Succeed())
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

			By("verifying a Job is created")
			verifyJobCreatedForTask(taskName, 2*time.Minute)

			By("verifying ORKA_GIT_REF env var is set")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "jobs",
					"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
					"-o", "jsonpath={.items[0].spec.template.spec.containers[0].env}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())

				var envVars []envVar
				err = json.Unmarshal([]byte(output), &envVars)
				g.Expect(err).NotTo(HaveOccurred())

				envMap := make(map[string]string)
				for _, e := range envVars {
					envMap[e.Name] = e.Value
				}
				g.Expect(envMap).To(HaveKeyWithValue("ORKA_GIT_REF", "main"))
			}, 30*time.Second, time.Second).Should(Succeed())
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

			By("verifying a Job is created")
			verifyJobCreatedForTask(taskName, 2*time.Minute)

			By("verifying the Job has ORKA_FORK_REPO and ORKA_PR_BASE_BRANCH env vars")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "jobs",
					"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
					"-o", "jsonpath={.items[0].spec.template.spec.containers[0].env}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())

				var envVars []envVar
				err = json.Unmarshal([]byte(output), &envVars)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to parse env vars JSON")

				envMap := make(map[string]string)
				for _, e := range envVars {
					envMap[e.Name] = e.Value
				}

				g.Expect(envMap).To(HaveKey("ORKA_FORK_REPO"))
				g.Expect(envMap["ORKA_FORK_REPO"]).To(Equal("https://github.com/fork/repo"))
				g.Expect(envMap).To(HaveKey("ORKA_PR_BASE_BRANCH"))
				g.Expect(envMap["ORKA_PR_BASE_BRANCH"]).To(Equal("develop"))
			}, 30*time.Second, time.Second).Should(Succeed())
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

		It("should add a fetch-session init container when sessionRef is set", func() {
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

			By("verifying a Job is created")
			verifyJobCreatedForTask(taskName, 2*time.Minute)

			By("verifying the Pod has a fetch-session init container")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "jobs",
					"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
					"-o", "jsonpath={.items[0].spec.template.spec.initContainers[*].name}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(ContainSubstring("fetch-session"),
					"Pod should have a fetch-session init container")
			}, 30*time.Second, time.Second).Should(Succeed())
		})
	})
})
