//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/sozercan/orka/test/utils"
)

// skipIfNoKey skips the current test if the given environment variable is not set or empty.
func skipIfNoKey(envVar string) {
	if os.Getenv(envVar) == "" {
		Skip(fmt.Sprintf("Skipping: %s not set", envVar))
	}
}

// waitForTaskPhase polls until the task reaches the given phase or times out.
func waitForTaskPhase(name, phase string, timeout time.Duration) {
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "task", name,
			"-o", "jsonpath={.status.phase}",
			"-n", namespace,
		)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal(phase), fmt.Sprintf("Task %s should reach phase %s", name, phase))
	}, timeout, time.Second).Should(Succeed())
}

// waitForTaskCompletion waits for a task to reach a terminal phase (Succeeded or Failed).
func waitForTaskCompletion(name string, timeout time.Duration) string {
	var finalPhase string
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "task", name,
			"-o", "jsonpath={.status.phase}",
			"-n", namespace,
		)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(BeElementOf("Succeeded", "Failed"),
			fmt.Sprintf("Task %s should reach terminal phase, got: %s", name, output))
		finalPhase = output
	}, timeout, 2*time.Second).Should(Succeed())
	return finalPhase
}

// verifyResultAvailable checks that a task's resultRef.available is "true".
func verifyResultAvailable(taskName string) {
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "task", taskName,
			"-o", "jsonpath={.status.resultRef.available}",
			"-n", namespace,
		)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal("true"), "Task should have resultRef.available=true")
	}, 30*time.Second, time.Second).Should(Succeed())
}

// verifyJobCreatedForTask waits for a Job labeled with the task name to appear.
func verifyJobCreatedForTask(taskName string, timeout time.Duration) {
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "jobs",
			"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
			"-o", "jsonpath={.items[0].metadata.name}",
			"-n", namespace,
		)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "Failed to get Job for task %s", taskName)
		g.Expect(output).NotTo(BeEmpty(), "Job name should not be empty")
	}, timeout, time.Second).Should(Succeed())
}

// getJobContainerImage returns the container image used by the Job for a task.
func getJobContainerImage(taskName string) string {
	var image string
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "jobs",
			"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
			"-o", "jsonpath={.items[0].spec.template.spec.containers[0].image}",
			"-n", namespace,
		)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).NotTo(BeEmpty())
		image = output
	}, 30*time.Second, time.Second).Should(Succeed())
	return image
}

// createProviderCRD creates a Provider CRD with the given configuration.
func createProviderCRD(name, providerType, secretName, secretKey, baseURL, model string) {
	By(fmt.Sprintf("creating Provider CRD: %s (type: %s)", name, providerType))

	baseURLField := ""
	if baseURL != "" {
		baseURLField = fmt.Sprintf(`,"baseURL":"%s"`, baseURL)
	}
	modelField := ""
	if model != "" {
		modelField = fmt.Sprintf(`,"defaultModel":"%s"`, model)
	}

	manifest := fmt.Sprintf(`{
		"apiVersion": "core.orka.ai/v1alpha1",
		"kind": "Provider",
		"metadata": {
			"name": "%s",
			"namespace": "%s"
		},
		"spec": {
			"type": "%s",
			"secretRef": {
				"name": "%s",
				"key": "%s"
			}%s%s
		}
	}`, name, namespace, providerType, secretName, secretKey, baseURLField, modelField)

	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = stringReader(manifest)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create Provider %s", name)
}

// dumpDebugInfo collects and prints debug information on test failure.
func dumpDebugInfo(taskNames ...string) {
	if !CurrentSpecReport().Failed() {
		return
	}

	By("collecting debug information for failed test")

	// Controller logs
	cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
		"-o", "jsonpath={.items[0].metadata.name}", "-n", namespace)
	podName, err := utils.Run(cmd)
	if err == nil && podName != "" {
		cmd = exec.Command("kubectl", "logs", podName, "-n", namespace, "--tail=100")
		logs, err := utils.Run(cmd)
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Controller Logs (last 100 lines) ===\n%s\n", logs)
		}
	}

	// Task details
	for _, name := range taskNames {
		cmd = exec.Command("kubectl", "get", "task", name, "-n", namespace, "-o", "yaml")
		output, err := utils.Run(cmd)
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Task %s ===\n%s\n", name, output)
		}

		// Pod descriptions and logs for task
		cmd = exec.Command("kubectl", "get", "pods", "-l", fmt.Sprintf("orka.ai/task=%s", name),
			"-n", namespace, "-o", "wide")
		output, err = utils.Run(cmd)
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Pods for task %s ===\n%s\n", name, output)
		}

		// Worker pod logs
		cmd = exec.Command("kubectl", "logs", "-l", fmt.Sprintf("orka.ai/task=%s", name),
			"-n", namespace, "--tail=50")
		output, err = utils.Run(cmd)
		if err == nil && output != "" {
			_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Worker Logs for task %s ===\n%s\n", name, output)
		}
	}

	// Namespace events
	cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
	events, err := utils.Run(cmd)
	if err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Namespace Events ===\n%s\n", events)
	}
}
