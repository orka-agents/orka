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

var _ = Describe("Agent Task", Ordered, func() {
	const (
		taskName  = "e2e-agent-task"
		agentName = "e2e-test-agent"
	)

	// Clean up resources after the test
	AfterAll(func() {
		By("deleting the agent task")
		cmd := exec.Command("kubectl", "delete", "task", taskName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("deleting the agent")
		cmd = exec.Command("kubectl", "delete", "agent", agentName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		// Results are stored in SQLite — no ConfigMap cleanup needed
	})

	AfterEach(func() {
		dumpDebugInfo(taskName)
	})

	It("should create a Job and complete for an agent-type task", func() {
		By("creating an Agent CRD with claude runtime")
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

		By("creating a Task with type agent referencing the Agent")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "agent",
				"prompt": "echo hello world",
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
		Expect(err).NotTo(HaveOccurred(), "Failed to create agent Task")

		By("verifying that a Job is created for the agent task")
		verifyJobCreated := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("orka.ai/task=%s,orka.ai/task-type=agent", taskName),
				"-o", "jsonpath={.items[0].metadata.name}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "Failed to get Job for agent task")
			g.Expect(output).NotTo(BeEmpty(), "Job name should not be empty")
		}
		Eventually(verifyJobCreated, 2*time.Minute, time.Second).Should(Succeed())

		By("verifying the Job has the correct container image")
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

		By("verifying the Job has required environment variables")
		verifyEnvVars := func(g Gomega) {
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
			g.Expect(envMap).To(HaveKey("ORKA_RESULT_ENDPOINT"))
			g.Expect(envMap).To(HaveKey("ORKA_CONTROLLER_URL"))
			g.Expect(envMap).To(HaveKey("ORKA_PROMPT"))
			g.Expect(envMap["ORKA_PROMPT"]).To(Equal("echo hello world"))
			g.Expect(envMap).To(HaveKey("ORKA_MAX_TURNS"))
			g.Expect(envMap["ORKA_MAX_TURNS"]).To(Equal("3"))
		}
		Eventually(verifyEnvVars, 30*time.Second, time.Second).Should(Succeed())

		By("verifying the Task status transitions to Running")
		verifyTaskRunning := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", taskName,
				"-o", "jsonpath={.status.phase}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(BeElementOf("Running", "Succeeded", "Failed"),
				"Task should transition from Pending")
		}
		Eventually(verifyTaskRunning, 2*time.Minute, time.Second).Should(Succeed())

		By("verifying the Task has a jobName in status")
		verifyJobName := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", taskName,
				"-o", "jsonpath={.status.jobName}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty(), "Task should have a jobName in status")
		}
		Eventually(verifyJobName, 30*time.Second, time.Second).Should(Succeed())

		By("verifying the Job pod template has the correct security context")
		verifyPodSecurity := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
				"-o", "jsonpath={.items[0].spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("false"), "Pod should not allow privilege escalation")
		}
		Eventually(verifyPodSecurity, 30*time.Second, time.Second).Should(Succeed())
	})
})

// envVar is a simplified representation of a Kubernetes environment variable
// for parsing JSON output from kubectl.
type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// stringReader returns a strings.Reader for use as command stdin.
func stringReader(s string) *strings.Reader {
	return strings.NewReader(s)
}
