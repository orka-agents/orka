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

var _ = Describe("Scheduled Tasks", Ordered, func() {
	const (
		cronTaskName    = "e2e-cron-task"
		suspendTaskName = "e2e-cron-suspend"
	)

	AfterAll(func() {
		By("cleaning up scheduled task test resources")
		for _, name := range []string{cronTaskName, suspendTaskName} {
			cmd := exec.Command("kubectl", "delete", "task", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
		// Clean up child tasks
		cmd := exec.Command("kubectl", "delete", "tasks", "-l", "orka.ai/scheduled-run=true",
			"-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		dumpDebugInfo(cronTaskName, suspendTaskName)
	})

	It("should create child tasks on a cron schedule", func() {
		By("creating a scheduled task that runs every minute")
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
				"args": ["scheduled-run"],
				"schedule": "* * * * *",
				"concurrencyPolicy": "Allow",
				"successfulRunsHistoryLimit": 2,
				"failedRunsHistoryLimit": 1
			}
		}`, cronTaskName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create scheduled task")

		By("verifying the task enters Scheduled phase")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", cronTaskName,
				"-o", "jsonpath={.status.phase}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Scheduled"), "Task should be in Scheduled phase")
		}, 30*time.Second, time.Second).Should(Succeed())

		By("waiting for at least one child task to be created")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "tasks",
				"-l", fmt.Sprintf("orka.ai/parent-task=%s,orka.ai/scheduled-run=true", cronTaskName),
				"-o", "jsonpath={.items[*].metadata.name}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).NotTo(BeEmpty(),
				"At least one child task should be created by the schedule")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("verifying lastScheduleTime is set on the parent")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", cronTaskName,
				"-o", "jsonpath={.status.lastScheduleTime}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).NotTo(BeEmpty(),
				"lastScheduleTime should be populated")
		}, 30*time.Second, time.Second).Should(Succeed())

		By("verifying child task has correct labels and no schedule field")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "tasks",
				"-l", fmt.Sprintf("orka.ai/parent-task=%s", cronTaskName),
				"-o", "jsonpath={.items[0].spec.schedule}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(BeEmpty(),
				"Child task should not have a schedule field")
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	It("should respect suspend flag on scheduled tasks", func() {
		By("creating a suspended scheduled task")
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
				"args": ["should-not-run"],
				"schedule": "* * * * *",
				"suspend": true
			}
		}`, suspendTaskName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create suspended scheduled task")

		By("verifying the task enters Scheduled phase")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", suspendTaskName,
				"-o", "jsonpath={.status.phase}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Scheduled"))
		}, 30*time.Second, time.Second).Should(Succeed())

		By("waiting 70 seconds and verifying no child tasks are created")
		time.Sleep(70 * time.Second)

		cmd = exec.Command("kubectl", "get", "tasks",
			"-l", fmt.Sprintf("orka.ai/parent-task=%s,orka.ai/scheduled-run=true", suspendTaskName),
			"-o", "jsonpath={.items[*].metadata.name}",
			"-n", namespace,
		)
		output, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(output)).To(BeEmpty(),
			"No child tasks should be created while suspended")
	})
})
