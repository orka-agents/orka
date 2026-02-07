//go:build e2e
// +build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/sozercan/mercan/test/utils"
)

var _ = Describe("Agent Copilot Runtime", Ordered, func() {
	const (
		taskName   = "e2e-copilot-task"
		agentName  = "e2e-copilot-agent"
		secretName = "e2e-copilot-secret"
	)

	AfterAll(func() {
		By("cleaning up copilot agent test resources")
		cmd := exec.Command("kubectl", "delete", "task", taskName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		cmd = exec.Command("kubectl", "delete", "agent", agentName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		cmd = exec.Command("kubectl", "delete", "secret", secretName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		// Results are stored in SQLite — no ConfigMap cleanup needed
	})

	It("should create a Job with Copilot runtime configuration", func() {
		By("creating a Secret with GITHUB_TOKEN")
		secretManifest := fmt.Sprintf(`{
			"apiVersion": "v1",
			"kind": "Secret",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"type": "Opaque",
			"stringData": {
				"GITHUB_TOKEN": "placeholder-token-for-e2e"
			}
		}`, secretName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(secretManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Secret")

		By("creating an Agent with runtime.type: copilot")
		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.mercan.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"runtime": {
					"type": "copilot",
					"defaultMaxTurns": 10,
					"defaultAllowBash": true
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

		By("creating a Task with type agent referencing the Copilot Agent")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.mercan.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "agent",
				"prompt": "list files in current directory",
				"agentRef": {
					"name": "%s"
				},
				"agentRuntime": {
					"maxTurns": 5
				}
			}
		}`, taskName, namespace, agentName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Task")

		By("verifying that a Job is created for the copilot agent task")
		verifyJobCreated := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("mercan.ai/task=%s,mercan.ai/task-type=agent", taskName),
				"-o", "jsonpath={.items[0].metadata.name}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "Failed to get Job for copilot agent task")
			g.Expect(output).NotTo(BeEmpty(), "Job name should not be empty")
		}
		Eventually(verifyJobCreated, 2*time.Minute, time.Second).Should(Succeed())

		By("verifying the Job has the correct Copilot worker image")
		verifyContainerImage := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("mercan.ai/task=%s", taskName),
				"-o", "jsonpath={.items[0].spec.template.spec.containers[0].image}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("copilot"), "Image should be the Copilot agent worker")
		}
		Eventually(verifyContainerImage, 30*time.Second, time.Second).Should(Succeed())

		By("verifying the Job has required environment variables")
		verifyEnvVars := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("mercan.ai/task=%s", taskName),
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

			g.Expect(envMap).To(HaveKey("MERCAN_TASK_NAME"))
			g.Expect(envMap["MERCAN_TASK_NAME"]).To(Equal(taskName))
			g.Expect(envMap).To(HaveKey("MERCAN_TASK_NAMESPACE"))
			g.Expect(envMap["MERCAN_TASK_NAMESPACE"]).To(Equal(namespace))
			g.Expect(envMap).To(HaveKey("MERCAN_PROMPT"))
			g.Expect(envMap["MERCAN_PROMPT"]).To(Equal("list files in current directory"))
			g.Expect(envMap).To(HaveKey("MERCAN_MAX_TURNS"))
			g.Expect(envMap["MERCAN_MAX_TURNS"]).To(Equal("5"))
		}
		Eventually(verifyEnvVars, 30*time.Second, time.Second).Should(Succeed())

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
