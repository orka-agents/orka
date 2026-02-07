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

	It("should configure workspace volumes and env vars for git clone", func() {
		By("creating an Agent with claude runtime for workspace test")
		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.mercan.ai/v1alpha1",
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
			"apiVersion": "core.mercan.ai/v1alpha1",
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

		By("verifying that a Job is created for the workspace task")
		verifyJobCreated := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("mercan.ai/task=%s,mercan.ai/task-type=agent", taskName),
				"-o", "jsonpath={.items[0].metadata.name}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "Failed to get Job for workspace task")
			g.Expect(output).NotTo(BeEmpty(), "Job name should not be empty")
		}
		Eventually(verifyJobCreated, 2*time.Minute, time.Second).Should(Succeed())

		By("verifying the Job pod has workspace-related env vars")
		verifyWorkspaceEnvVars := func(g Gomega) {
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

			g.Expect(envMap).To(HaveKey("MERCAN_GIT_REPO"))
			g.Expect(envMap["MERCAN_GIT_REPO"]).To(Equal("https://github.com/example/repo"))
			g.Expect(envMap).To(HaveKey("MERCAN_GIT_BRANCH"))
			g.Expect(envMap["MERCAN_GIT_BRANCH"]).To(Equal("main"))
		}
		Eventually(verifyWorkspaceEnvVars, 30*time.Second, time.Second).Should(Succeed())

		By("verifying emptyDir volumes are mounted at /workspace")
		verifyWorkspaceVolume := func(g Gomega) {
			// Check that the workspace volume exists
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("mercan.ai/task=%s", taskName),
				"-o", "jsonpath={.items[0].spec.template.spec.volumes[*].name}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("workspace"), "Should have a workspace volume")

			// Check that the volume mount exists at /workspace
			cmd = exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("mercan.ai/task=%s", taskName),
				"-o", "jsonpath={.items[0].spec.template.spec.containers[0].volumeMounts}",
				"-n", namespace,
			)
			output, err = utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())

			type volumeMount struct {
				Name      string `json:"name"`
				MountPath string `json:"mountPath"`
			}
			var mounts []volumeMount
			err = json.Unmarshal([]byte(output), &mounts)
			g.Expect(err).NotTo(HaveOccurred(), "Failed to parse volume mounts JSON")

			found := false
			for _, m := range mounts {
				if m.Name == "workspace" && m.MountPath == "/workspace" {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "Should have workspace volume mounted at /workspace")
		}
		Eventually(verifyWorkspaceVolume, 30*time.Second, time.Second).Should(Succeed())
	})
})
