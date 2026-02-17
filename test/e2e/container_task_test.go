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

var _ = Describe("Container Task Lifecycle", Ordered, func() {
	const (
		successTaskName  = "e2e-container-success"
		failureTaskName  = "e2e-container-failure"
		multiStepTask    = "e2e-container-multistep"
		resourceTaskName = "e2e-container-resources"
	)

	AfterAll(func() {
		By("cleaning up container task test resources")
		for _, name := range []string{successTaskName, failureTaskName, multiStepTask, resourceTaskName} {
			cmd := exec.Command("kubectl", "delete", "task", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
	})

	AfterEach(func() {
		dumpDebugInfo(successTaskName, failureTaskName, multiStepTask, resourceTaskName)
	})

	It("should complete a container task successfully and store the result", func() {
		By("creating a container task that echoes output")
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
				"command": ["echo"],
				"args": ["hello-e2e-container-test"]
			}
		}`, successTaskName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create container task")

		By("waiting for task to succeed")
		phase := waitForTaskCompletion(successTaskName, 3*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "Container task should succeed")

		By("verifying the result is stored")
		verifyResultAvailable(successTaskName)
	})

	It("should mark a container task as Failed when the command fails", func() {
		By("creating a container task with a failing command")
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
				"command": ["false"]
			}
		}`, failureTaskName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create failing container task")

		By("waiting for task to reach terminal phase")
		phase := waitForTaskCompletion(failureTaskName, 3*time.Minute)
		Expect(phase).To(Equal("Failed"), "Container task with 'false' command should fail")
	})

	It("should handle multi-step container commands", func() {
		By("creating a container task with multi-step command")
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
				"command": ["sh", "-c"],
				"args": ["echo step1 && echo step2 && echo done"]
			}
		}`, multiStepTask, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create multi-step container task")

		By("waiting for task to succeed")
		phase := waitForTaskCompletion(multiStepTask, 3*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "Multi-step container task should succeed")

		By("verifying the result is stored")
		verifyResultAvailable(multiStepTask)
	})

	It("should apply resource limits to the container task pod", func() {
		By("creating a container task with resource requests")
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
				"command": ["echo"],
				"args": ["resource-test"],
				"resources": {
					"requests": {
						"cpu": "50m",
						"memory": "32Mi"
					},
					"limits": {
						"cpu": "100m",
						"memory": "64Mi"
					}
				}
			}
		}`, resourceTaskName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create resource-limited container task")

		By("verifying the Job is created")
		verifyJobCreatedForTask(resourceTaskName, 2*time.Minute)

		By("verifying the Pod has the correct resource limits")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("orka.ai/task=%s", resourceTaskName),
				"-o", "jsonpath={.items[0].spec.template.spec.containers[0].resources}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty())

			var resources struct {
				Limits   map[string]string `json:"limits"`
				Requests map[string]string `json:"requests"`
			}
			err = json.Unmarshal([]byte(output), &resources)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(resources.Requests).To(HaveKeyWithValue("cpu", "50m"))
			g.Expect(resources.Requests).To(HaveKeyWithValue("memory", "32Mi"))
			g.Expect(resources.Limits).To(HaveKeyWithValue("cpu", "100m"))
			g.Expect(resources.Limits).To(HaveKeyWithValue("memory", "64Mi"))
		}, 30*time.Second, time.Second).Should(Succeed())

		By("waiting for task to succeed")
		phase := waitForTaskCompletion(resourceTaskName, 3*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "Resource-limited task should succeed")
	})
})
