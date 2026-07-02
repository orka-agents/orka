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

	"github.com/orka-agents/orka/test/utils"
)

var _ = Describe("Scheduled Tasks", Ordered, func() {
	const (
		cronTaskName    = "e2e-cron-task"
		suspendTaskName = "e2e-cron-suspend"
		forbidTaskName  = "e2e-cron-forbid"
		historyTaskName = "e2e-cron-history"
	)

	AfterAll(func() {
		By("cleaning up scheduled task test resources")
		for _, name := range []string{cronTaskName, suspendTaskName, forbidTaskName, historyTaskName} {
			cmd := exec.Command("kubectl", "delete", "task", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
		// Clean up child tasks
		cmd := exec.Command("kubectl", "delete", "tasks", "-l", "orka.ai/scheduled-run=true",
			"-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		dumpDebugInfo(cronTaskName, suspendTaskName, forbidTaskName, historyTaskName)
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

	It("should enforce concurrencyPolicy Forbid", func() {
		By("creating a scheduled task with concurrencyPolicy Forbid and a slow command")
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
				"args": ["90"],
				"schedule": "* * * * *",
				"concurrencyPolicy": "Forbid"
			}
		}`, forbidTaskName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Forbid scheduled task")

		By("waiting for the first child task to be created and running")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "tasks",
				"-l", fmt.Sprintf("orka.ai/parent-task=%s,orka.ai/scheduled-run=true", forbidTaskName),
				"-o", "jsonpath={.items[*].status.phase}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(ContainSubstring("Running"),
				"First child task should be Running")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting past another minute trigger and verifying no second child is created")
		Consistently(func() int {
			cmd := exec.Command("kubectl", "get", "tasks",
				"-l", fmt.Sprintf("orka.ai/parent-task=%s,orka.ai/scheduled-run=true", forbidTaskName),
				"-o", "jsonpath={.items[*].metadata.name}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			return len(strings.Fields(strings.TrimSpace(output)))
		}, 70*time.Second, 5*time.Second).Should(Equal(1),
			"Only one child task should exist when concurrencyPolicy is Forbid and the first is still running")
	})

	It("should respect successfulRunsHistoryLimit", func() {
		By("creating a scheduled task with successfulRunsHistoryLimit 1")
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
				"args": ["history-test"],
				"schedule": "* * * * *",
				"successfulRunsHistoryLimit": 1
			}
		}`, historyTaskName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create history-limit scheduled task")

		var firstChildName string

		By("waiting for the first child task to be created")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "tasks",
				"-l", fmt.Sprintf("orka.ai/parent-task=%s,orka.ai/scheduled-run=true", historyTaskName),
				"-o", "jsonpath={.items[*].metadata.name}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			names := strings.Fields(strings.TrimSpace(output))
			g.Expect(names).NotTo(BeEmpty(), "Should create an initial child task")
			firstChildName = names[0]
		}, 4*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for the retained child task to roll over to a newer scheduled run")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "tasks",
				"-l", fmt.Sprintf("orka.ai/parent-task=%s,orka.ai/scheduled-run=true", historyTaskName),
				"-o", "jsonpath={.items[*].metadata.name}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			names := strings.Fields(strings.TrimSpace(output))
			g.Expect(names).To(HaveLen(1), "History limit should keep only one child task")
			g.Expect(names[0]).NotTo(Equal(firstChildName),
				"The retained child task should rotate after a later scheduled run")
		}, 4*time.Minute, 5*time.Second).Should(Succeed())
	})
})
