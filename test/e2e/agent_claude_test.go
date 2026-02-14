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

	"github.com/sozercan/orka/test/utils"
)

var _ = Describe("Agent Claude Runtime", Ordered, func() {
	const (
		taskName   = "e2e-claude-task"
		agentName  = "e2e-claude-agent"
		secretName = "e2e-claude-secret"
	)

	AfterAll(func() {
		By("cleaning up claude agent test resources")
		cmd := exec.Command("kubectl", "delete", "task", taskName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		cmd = exec.Command("kubectl", "delete", "agent", agentName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		cmd = exec.Command("kubectl", "delete", "secret", secretName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		// Results are stored in SQLite — no ConfigMap cleanup needed
	})

	It("should create a Job with Claude runtime configuration", func() {
		By("creating a Secret with ANTHROPIC_API_KEY")
		secretManifest := fmt.Sprintf(`{
			"apiVersion": "v1",
			"kind": "Secret",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"type": "Opaque",
			"stringData": {
				"ANTHROPIC_API_KEY": "placeholder-key-for-e2e"
			}
		}`, secretName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(secretManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Secret")

		By("creating an Agent with runtime.type: claude")
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
				"secretRef": {
					"name": "%s"
				}
			}
		}`, agentName, namespace, secretName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Agent")

		By("creating a Task with type agent referencing the Claude Agent")
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
				"agentRef": {
					"name": "%s"
				},
				"agentRuntime": {
					"maxTurns": 3
				}
			}
		}`, taskName, namespace, agentName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Task")

		By("verifying that a Job is created for the claude agent task")
		verifyJobCreated := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("orka.ai/task=%s,orka.ai/task-type=agent", taskName),
				"-o", "jsonpath={.items[0].metadata.name}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "Failed to get Job for claude agent task")
			g.Expect(output).NotTo(BeEmpty(), "Job name should not be empty")
		}
		Eventually(verifyJobCreated, 2*time.Minute, time.Second).Should(Succeed())

		By("verifying the Job has the correct Claude worker image")
		verifyContainerImage := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
				"-o", "jsonpath={.items[0].spec.template.spec.containers[0].image}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("claude"), "Image should be the Claude agent worker")
		}
		Eventually(verifyContainerImage, 30*time.Second, time.Second).Should(Succeed())

		By("verifying the Job has ANTHROPIC_API_KEY env var from secret")
		verifySecretEnv := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
				"-o", "jsonpath={.items[0].spec.template.spec.containers[0].env}",
				"-n", namespace,
			)
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

			g.Expect(envMap).To(HaveKey("ORKA_TASK_NAME"))
			g.Expect(envMap["ORKA_TASK_NAME"]).To(Equal(taskName))
			g.Expect(envMap).To(HaveKey("ORKA_TASK_NAMESPACE"))
			g.Expect(envMap["ORKA_TASK_NAMESPACE"]).To(Equal(namespace))
			g.Expect(envMap).To(HaveKey("ORKA_PROMPT"))
			g.Expect(envMap["ORKA_PROMPT"]).To(Equal("say hello"))
			g.Expect(envMap).To(HaveKey("ORKA_MAX_TURNS"))
			g.Expect(envMap["ORKA_MAX_TURNS"]).To(Equal("3"))
		}
		Eventually(verifySecretEnv, 30*time.Second, time.Second).Should(Succeed())

		By("verifying the Task reaches a terminal status")
		verifyTaskTerminal := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", taskName,
				"-o", "jsonpath={.status.phase}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(BeElementOf("Running", "Succeeded", "Failed"),
				"Task should transition from Pending")
		}
		Eventually(verifyTaskTerminal, 2*time.Minute, time.Second).Should(Succeed())
	})
})
