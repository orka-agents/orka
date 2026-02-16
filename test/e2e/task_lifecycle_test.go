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

var _ = Describe("Task Lifecycle Advanced", Ordered, func() {
	const (
		timeoutTaskName = "e2e-timeout-task"
		retryTaskName   = "e2e-retry-task"
		cancelTaskName  = "e2e-cancel-task"
	)

	AfterAll(func() {
		By("cleaning up task lifecycle test resources")
		for _, name := range []string{timeoutTaskName, retryTaskName, cancelTaskName} {
			cmd := exec.Command("kubectl", "delete", "task", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
	})

	AfterEach(func() {
		dumpDebugInfo(timeoutTaskName, retryTaskName, cancelTaskName)
	})

	It("should fail a task that exceeds its timeout", func() {
		By("creating a container task with a short timeout that sleeps too long")
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
				"args": ["300"],
				"timeout": "15s"
			}
		}`, timeoutTaskName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create timeout task")

		By("waiting for the task to be marked as Failed due to timeout")
		phase := waitForTaskCompletion(timeoutTaskName, 3*time.Minute)
		Expect(phase).To(Equal("Failed"), "Task should fail due to timeout")

		By("verifying the failure message mentions timeout")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", timeoutTaskName,
				"-o", "jsonpath={.status.message}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("timed out"),
				"Failure message should indicate timeout")
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	It("should retry a failed task according to retryPolicy", func() {
		By("creating a container task that fails but has retries configured")
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
				"command": ["false"],
				"retryPolicy": {
					"maxRetries": 2,
					"initialDelay": "5s",
					"backoffMultiplier": 2
				}
			}
		}`, retryTaskName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create retry task")

		By("waiting for the task to eventually fail after exhausting retries")
		phase := waitForTaskCompletion(retryTaskName, 5*time.Minute)
		Expect(phase).To(Equal("Failed"), "Task should ultimately fail after retries")

		By("verifying the task was retried (attempts > 0)")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", retryTaskName,
				"-o", "jsonpath={.status.attempts}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())

			var attempts int
			err = json.Unmarshal([]byte(output), &attempts)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(attempts).To(BeNumerically(">=", 2),
				"Task should have been retried at least twice")
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	It("should cancel a running task", func() {
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
				"args": ["600"]
			}
		}`, cancelTaskName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create cancel task")

		By("waiting for the task to reach Running phase")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", cancelTaskName,
				"-o", "jsonpath={.status.phase}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Running"), "Task should reach Running phase")
		}, 3*time.Minute, time.Second).Should(Succeed())

		By("cancelling the task via status subresource patch")
		cmd = exec.Command("kubectl", "patch", "task", cancelTaskName,
			"--type=merge", "--subresource=status",
			"-p", `{"status":{"phase":"Cancelled","message":"cancelled by e2e test"}}`,
			"-n", namespace,
		)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to cancel task")

		By("verifying the task reaches Cancelled phase")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", cancelTaskName,
				"-o", "jsonpath={.status.phase}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Cancelled"), "Task should be cancelled")
		}, 30*time.Second, time.Second).Should(Succeed())
	})
})
