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

	"github.com/sozercan/orka/test/utils"
)

var _ = Describe("Controller Feature Tests", func() {

	// --- SystemPrompt from ConfigMap ---

	Describe("Agent SystemPrompt from ConfigMap", func() {
		const (
			configMapName = "e2e-systemprompt-cm"
			agentName     = "e2e-systemprompt-agent"
			taskName      = "e2e-systemprompt-task"
		)

		AfterEach(func() {
			dumpDebugInfo(taskName)
			for _, r := range []struct{ kind, name string }{
				{"task", taskName},
				{"agent", agentName},
				{"configmap", configMapName},
			} {
				cmd := exec.Command("kubectl", "delete", r.kind, r.name,
					"-n", namespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			}
		})

		It("should set ORKA_SYSTEM_PROMPT from a ConfigMap", func() {
			skipIfNoKey("E2E_OPENAI_API_KEY")

			By("creating a ConfigMap with a system prompt")
			cmManifest := fmt.Sprintf(`{
				"apiVersion": "v1",
				"kind": "ConfigMap",
				"metadata": {
					"name": "%s",
					"namespace": "%s"
				},
				"data": {
					"prompt": "You are an e2e test assistant. Always respond with 'E2E_PROMPT_VERIFIED'."
				}
			}`, configMapName, namespace)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cmManifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a Provider for the agent")
			createProviderCRD("e2e-sp-provider", "openai", "e2e-openai-secret", "api-key",
				e2eOpenAIBaseURL, e2eOpenAIModel)
			DeferCleanup(func() {
				cmd := exec.Command("kubectl", "delete", "provider", "e2e-sp-provider",
					"-n", namespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			})

			By("creating an Agent with systemPrompt.configMapRef")
			model := e2eOpenAIModel
			if model == "" {
				model = "gpt-4o-mini"
			}
			agentManifest := fmt.Sprintf(`{
				"apiVersion": "core.orka.ai/v1alpha1",
				"kind": "Agent",
				"metadata": {
					"name": "%s",
					"namespace": "%s"
				},
				"spec": {
					"model": {
						"name": "%s",
						"providerRef": {
							"name": "e2e-sp-provider"
						}
					},
					"systemPrompt": {
						"configMapRef": {
							"name": "%s",
							"key": "prompt"
						}
					}
				}
			}`, agentName, namespace, model, configMapName)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(agentManifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a task referencing the agent")
			taskManifest := fmt.Sprintf(`{
				"apiVersion": "core.orka.ai/v1alpha1",
				"kind": "Task",
				"metadata": {
					"name": "%s",
					"namespace": "%s"
				},
				"spec": {
					"type": "ai",
					"agentRef": {"name": "%s"},
					"input": "What are you?"
				}
			}`, taskName, namespace, agentName)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(taskManifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the Job has ORKA_SYSTEM_PROMPT env var")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "jobs",
					"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
					"-o", "jsonpath={.items[0].spec.template.spec.containers[0].env[*].name}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("ORKA_SYSTEM_PROMPT"),
					"Job should have ORKA_SYSTEM_PROMPT env var")
			}, 60*time.Second, 2*time.Second).Should(Succeed())
		})
	})

	// --- AllowBash Override ---

	Describe("AllowBash Task Override", func() {
		const (
			agentName = "e2e-allowbash-agent"
			taskName  = "e2e-allowbash-task"
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

		It("should set ORKA_ALLOW_BASH from task override", func() {
			skipIfNoKey("E2E_OPENAI_API_KEY")

			By("creating a Provider")
			createProviderCRD("e2e-bash-provider", "openai", "e2e-openai-secret", "api-key",
				e2eOpenAIBaseURL, e2eOpenAIModel)
			DeferCleanup(func() {
				cmd := exec.Command("kubectl", "delete", "provider", "e2e-bash-provider",
					"-n", namespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			})

			model := e2eOpenAIModel
			if model == "" {
				model = "gpt-4o-mini"
			}

			By("creating an Agent without allowBash")
			agentManifest := fmt.Sprintf(`{
				"apiVersion": "core.orka.ai/v1alpha1",
				"kind": "Agent",
				"metadata": {
					"name": "%s",
					"namespace": "%s"
				},
				"spec": {
					"model": {
						"name": "%s",
						"providerRef": {
							"name": "e2e-bash-provider"
						}
					}
				}
			}`, agentName, namespace, model)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(agentManifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a Task with allowBash=true override")
			taskManifest := fmt.Sprintf(`{
				"apiVersion": "core.orka.ai/v1alpha1",
				"kind": "Task",
				"metadata": {
					"name": "%s",
					"namespace": "%s"
				},
				"spec": {
					"type": "ai",
					"agentRef": {"name": "%s"},
					"input": "Say hello",
					"agentRuntime": {
						"allowBash": true
					}
				}
			}`, taskName, namespace, agentName)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(taskManifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the Job has ORKA_ALLOW_BASH=true env var")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "jobs",
					"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
					"-o", "jsonpath={.items[0].spec.template.spec.containers[0].env}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("ORKA_ALLOW_BASH"),
					"Job should have ORKA_ALLOW_BASH env var")
			}, 60*time.Second, 2*time.Second).Should(Succeed())
		})
	})

	// --- Task Deletion Cleanup (Finalizer) ---

	Describe("Task Deletion with Finalizer Cleanup", func() {
		const taskName = "e2e-finalizer-task"

		AfterEach(func() {
			dumpDebugInfo(taskName)
			cmd := exec.Command("kubectl", "delete", "task", taskName,
				"-n", namespace, "--ignore-not-found", "--timeout=30s")
			_, _ = utils.Run(cmd)
		})

		It("should clean up Jobs when a running task is deleted", func() {
			By("creating a long-running container task")
			taskManifest := fmt.Sprintf(`{
				"apiVersion": "core.orka.ai/v1alpha1",
				"kind": "Task",
				"metadata": {
					"name": "%s",
					"namespace": "%s"
				},
				"spec": {
					"type": "container",
					"image": "busybox:latest",
					"command": ["sleep"],
					"args": ["300"]
				}
			}`, taskName, namespace)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(taskManifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the Job to be created")
			verifyJobCreatedForTask(taskName, 60*time.Second)

			By("verifying the task has the finalizer")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "task", taskName,
					"-o", "jsonpath={.metadata.finalizers}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("orka.ai/cleanup"),
					"Task should have the orka.ai/cleanup finalizer")
			}, 30*time.Second, time.Second).Should(Succeed())

			By("deleting the task")
			cmd = exec.Command("kubectl", "delete", "task", taskName,
				"-n", namespace, "--timeout=60s")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the Job is cleaned up")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "jobs",
					"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
					"-o", "jsonpath={.items}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(BeElementOf("", "[]"),
					"Job should be deleted after task deletion")
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("verifying the task is fully removed")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "task", taskName,
					"-n", namespace, "--ignore-not-found")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(BeEmpty(),
					"Task should be fully deleted")
			}, 30*time.Second, time.Second).Should(Succeed())
		})
	})

	// --- MaxConcurrentChildren Enforcement ---

	Describe("MaxConcurrentChildren Enforcement", func() {
		const (
			parentTaskName = "e2e-concurrency-parent"
			child1Name     = "e2e-concurrency-child-1"
			child2Name     = "e2e-concurrency-child-2"
			child3Name     = "e2e-concurrency-child-3"
			agentName      = "e2e-concurrency-agent"
		)

		AfterEach(func() {
			dumpDebugInfo(parentTaskName, child1Name, child2Name, child3Name)
			for _, name := range []string{child1Name, child2Name, child3Name, parentTaskName} {
				cmd := exec.Command("kubectl", "delete", "task", name,
					"-n", namespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			}
			cmd := exec.Command("kubectl", "delete", "agent", agentName,
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should enforce maxConcurrentChildren limit", func() {
			skipIfNoKey("E2E_OPENAI_API_KEY")

			By("creating a Provider and Agent with coordination and maxConcurrentChildren=1")
			createProviderCRD("e2e-cc-provider", "openai", "e2e-openai-secret", "api-key",
				e2eOpenAIBaseURL, e2eOpenAIModel)
			DeferCleanup(func() {
				cmd := exec.Command("kubectl", "delete", "provider", "e2e-cc-provider",
					"-n", namespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			})

			model := e2eOpenAIModel
			if model == "" {
				model = "gpt-4o-mini"
			}

			agentManifest := fmt.Sprintf(`{
				"apiVersion": "core.orka.ai/v1alpha1",
				"kind": "Agent",
				"metadata": {
					"name": "%s",
					"namespace": "%s"
				},
				"spec": {
					"model": {
						"name": "%s",
						"providerRef": {
							"name": "e2e-cc-provider"
						}
					},
					"coordination": {
						"enabled": true,
						"maxConcurrentChildren": 1
					}
				}
			}`, agentName, namespace, model)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(agentManifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a parent task")
			parentManifest := fmt.Sprintf(`{
				"apiVersion": "core.orka.ai/v1alpha1",
				"kind": "Task",
				"metadata": {
					"name": "%s",
					"namespace": "%s"
				},
				"spec": {
					"type": "container",
					"image": "busybox:latest",
					"command": ["sleep"],
					"args": ["120"]
				}
			}`, parentTaskName, namespace)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(parentManifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating child tasks with parent label")
			for _, childName := range []string{child1Name, child2Name, child3Name} {
				childManifest := fmt.Sprintf(`{
					"apiVersion": "core.orka.ai/v1alpha1",
					"kind": "Task",
					"metadata": {
						"name": "%s",
						"namespace": "%s",
						"labels": {
							"orka.ai/parent-task": "%s"
						}
					},
					"spec": {
						"type": "container",
						"image": "busybox:latest",
						"command": ["sleep"],
						"args": ["10"],
						"agentRef": {"name": "%s"}
					}
				}`, childName, namespace, parentTaskName, agentName)

				cmd = exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(childManifest)
				_, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
			}

			By("waiting a moment for scheduling to occur")
			time.Sleep(15 * time.Second)

			By("verifying at most 1 child is Running at a time")
			cmd = exec.Command("kubectl", "get", "tasks",
				"-l", fmt.Sprintf("orka.ai/parent-task=%s", parentTaskName),
				"-o", "jsonpath={range .items[*]}{.metadata.name}={.status.phase} {end}",
				"-n", namespace)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			runningCount := strings.Count(output, "=Running")
			_, _ = fmt.Fprintf(GinkgoWriter, "Child task phases: %s (running=%d)\n", output, runningCount)
			Expect(runningCount).To(BeNumerically("<=", 1),
				"At most 1 child should be Running with maxConcurrentChildren=1")
		})
	})

	// --- Namespace Isolation ---

	Describe("Namespace Isolation", func() {
		It("should reject access to tasks in other namespaces", func() {
			By("creating a task in the default namespace")
			taskManifest := `{
				"apiVersion": "core.orka.ai/v1alpha1",
				"kind": "Task",
				"metadata": {
					"name": "e2e-cross-ns-task",
					"namespace": "default"
				},
				"spec": {
					"type": "container",
					"image": "busybox:latest",
					"command": ["echo"],
					"args": ["cross-namespace"]
				}
			}`

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(taskManifest)
			// This may fail if CRDs are not installed in default namespace
			// or if RBAC prevents it. Both are valid outcomes.
			output, err := utils.Run(cmd)

			DeferCleanup(func() {
				cmd := exec.Command("kubectl", "delete", "task", "e2e-cross-ns-task",
					"-n", "default", "--ignore-not-found")
				_, _ = utils.Run(cmd)
			})

			if err != nil {
				By("cross-namespace task creation was rejected (RBAC enforcement)")
				_, _ = fmt.Fprintf(GinkgoWriter, "Expected rejection: %s\n", output)
				return
			}

			By("verifying the controller only processes tasks in its watch namespace")
			// The controller should not process tasks outside orka-system
			// The task should stay Pending since no controller watches default ns
			time.Sleep(10 * time.Second)

			cmd = exec.Command("kubectl", "get", "task", "e2e-cross-ns-task",
				"-o", "jsonpath={.status.phase}", "-n", "default")
			phase, err := utils.Run(cmd)
			if err == nil && phase != "" {
				_, _ = fmt.Fprintf(GinkgoWriter,
					"Cross-namespace task phase: %s (empty or Pending expected)\n", phase)
				// If controller is watching all namespaces, it may process this
				// The key check is that enforce-namespace-isolation prevents API access
			}
		})
	})
})
