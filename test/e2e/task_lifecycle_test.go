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
		timeoutTaskName    = "e2e-timeout-task"
		retryTaskName      = "e2e-retry-task"
		cancelTaskName     = "e2e-cancel-task"
		serialSession1Name = "e2e-serial-session-1"
		serialSession2Name = "e2e-serial-session-2"
		lockFailTaskName   = "e2e-lock-fail-task"
		lockRetryTaskName  = "e2e-lock-retry-task"

		serialProviderName = "e2e-serial-provider"
		serialSessionRef   = "e2e-serial-session"
		lockSessionRef     = "e2e-lock-session"
	)

	AfterAll(func() {
		By("cleaning up task lifecycle test resources")
		for _, name := range []string{timeoutTaskName, retryTaskName, cancelTaskName, serialSession1Name, serialSession2Name, lockFailTaskName, lockRetryTaskName} {
			cmd := exec.Command("kubectl", "delete", "task", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
		// Clean up provider
		cmd := exec.Command("kubectl", "delete", "provider", serialProviderName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		// Clean up sessions
		for _, name := range []string{serialSessionRef, lockSessionRef} {
			cmd := exec.Command("kubectl", "delete", "configmap", "session-"+name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
	})

	AfterEach(func() {
		dumpDebugInfo(timeoutTaskName, retryTaskName, cancelTaskName, serialSession1Name, serialSession2Name, lockFailTaskName, lockRetryTaskName)
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

	It("should execute tasks with same sessionRef serially", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating a provider for serial session tests")
		createProviderCRD(serialProviderName, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, model)

		By("creating task 1 with a longer prompt and sessionRef")
		task1Manifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "ai",
				"sessionRef": {"name": "%s", "create": true},
				"ai": {
					"prompt": "Write a short paragraph about the history of computing, covering at least five key milestones. Take your time and be thorough.",
					"model": "%s",
					"providerRef": {"name": "%s"}
				}
			}
		}`, serialSession1Name, namespace, serialSessionRef, model, serialProviderName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(task1Manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create serial session task 1")

		By("creating task 2 with a shorter prompt and same sessionRef")
		task2Manifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "ai",
				"sessionRef": {"name": "%s", "create": true},
				"ai": {
					"prompt": "Say hello.",
					"model": "%s",
					"providerRef": {"name": "%s"}
				}
			}
		}`, serialSession2Name, namespace, serialSessionRef, model, serialProviderName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(task2Manifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create serial session task 2")

		By("waiting for both tasks to complete")
		phase1 := waitForTaskCompletion(serialSession1Name, 5*time.Minute)
		Expect(phase1).To(Equal("Succeeded"), "Serial session task 1 should succeed")
		phase2 := waitForTaskCompletion(serialSession2Name, 5*time.Minute)
		Expect(phase2).To(Equal("Succeeded"), "Serial session task 2 should succeed")

		By("verifying task 2 started after task 1 completed (session lock)")
		var task1CompletionTime, task2StartTime time.Time
		parseTaskTime := func(raw string) (time.Time, error) {
			t, err := time.Parse(time.RFC3339Nano, raw)
			if err == nil {
				return t, nil
			}
			return time.Parse(time.RFC3339, raw)
		}
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", serialSession1Name,
				"-o", "jsonpath={.status.completionTime}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty(), "Task 1 should have a completionTime")
			t, parseErr := parseTaskTime(output)
			g.Expect(parseErr).NotTo(HaveOccurred())
			task1CompletionTime = t
		}, 30*time.Second, time.Second).Should(Succeed())

		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", serialSession2Name,
				"-o", "jsonpath={.status.startTime}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty(), "Task 2 should have a startTime")
			t, parseErr := parseTaskTime(output)
			g.Expect(parseErr).NotTo(HaveOccurred())
			task2StartTime = t
		}, 30*time.Second, time.Second).Should(Succeed())

		Expect(task2StartTime).To(BeTemporally(">=", task1CompletionTime.Add(-1*time.Second)),
			"Task 2 should start after task 1 completes (session serial execution)")
	})

	It("should release session lock when task fails", func() {
		By("creating a container task with sessionRef that will fail")
		failManifest := fmt.Sprintf(`{
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
				"sessionRef": {"name": "%s", "create": true}
			}
		}`, lockFailTaskName, namespace, lockSessionRef)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(failManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create lock-fail task")

		By("waiting for the failing task to complete")
		phase := waitForTaskCompletion(lockFailTaskName, 3*time.Minute)
		Expect(phase).To(Equal("Failed"), "Lock-fail task should fail")

		By("creating a second container task with same sessionRef")
		retryManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "container",
				"image": "busybox:latest",
				"command": ["echo", "lock-released"],
				"sessionRef": {"name": "%s", "create": true}
			}
		}`, lockRetryTaskName, namespace, lockSessionRef)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(retryManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create lock-retry task")

		By("verifying the second task acquires the lock and runs")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", lockRetryTaskName,
				"-o", "jsonpath={.status.phase}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(BeElementOf("Running", "Succeeded"),
				"Second task should acquire session lock and run")
		}, 3*time.Minute, time.Second).Should(Succeed())

		By("waiting for the second task to succeed")
		phase = waitForTaskCompletion(lockRetryTaskName, 3*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "Lock-retry task should succeed after lock is released")
	})
})
