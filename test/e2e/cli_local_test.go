//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Orka CLI local binary paths", Ordered, func() {
	var (
		apiBaseURL     string
		portForwardCmd *exec.Cmd
		cancelPF       context.CancelFunc
		token          string
		home           string
		suffix         string
	)

	BeforeAll(func() {
		By("building the orka CLI binary")
		buildOrkaCLI()

		By("setting up a controller API port-forward for local CLI commands")
		var err error
		apiBaseURL, cancelPF, portForwardCmd, err = startControllerAPIPortForward(18111)
		Expect(err).NotTo(HaveOccurred(), "Failed to start controller API port-forward")

		By("creating an isolated CLI home with service-account credentials")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())
		home = newIsolatedCLIHome(apiBaseURL, token)
		suffix = fmt.Sprintf("%d", time.Now().UnixNano())
	})

	AfterAll(func() {
		By("stopping local CLI controller API port-forward")
		stopPortForward(cancelPF, portForwardCmd)
	})

	It("manages config in an isolated HOME and masks local fake tokens", func() {
		localHome, err := os.MkdirTemp("", "orka-cli-config-e2e-home-*")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = os.RemoveAll(localHome) })

		writeCLIConfig(localHome, "", namespace, "")
		configPath := filepath.Join(localHome, ".orka", "config.yaml")
		fakeToken := "fake-config-token-sentinel-" + suffix

		By("setting server and a fake token through the compiled binary")
		setServer := runOrka(localHome, "config", "set-server", apiBaseURL)
		expectOrkaSuccess(setServer, fakeToken)
		setToken := runOrka(localHome, "config", "set-token", fakeToken)
		expectOrkaSuccess(setToken, fakeToken)

		By("verifying config view uses the isolated config and masks the fake token")
		view := runOrka(localHome, "config", "view")
		expectOrkaSuccess(view, fakeToken)
		Expect(view.Stdout).To(ContainSubstring(configPath))
		Expect(view.Stdout).To(ContainSubstring(apiBaseURL))
		Expect(view.Stdout).To(ContainSubstring("Namespace:"))
		Expect(view.Stdout).To(ContainSubstring(namespace))
		Expect(view.Stdout).To(ContainSubstring("Token:"))
		Expect(view.Stdout).NotTo(ContainSubstring(fakeToken))

		By("verifying the config file lives under the isolated HOME")
		info, err := os.Stat(configPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.IsDir()).To(BeFalse())
		content, err := os.ReadFile(configPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(content)).To(ContainSubstring(apiBaseURL))
		Expect(string(content)).To(ContainSubstring(namespace))
		Expect(string(content)).To(ContainSubstring(fakeToken))
	})

	It("reports status without leaking the configured token", func() {
		status := runOrka(home, "status")
		expectOrkaSuccess(status, token)
		Expect(status.Stdout).To(ContainSubstring("Health:"))
		Expect(status.Stdout).To(ContainSubstring("Ready:"))
		Expect(status.Stdout).To(ContainSubstring("Tasks:"))
		Expect(status.Stdout).To(ContainSubstring("Agents:"))
	})

	It("creates a container task from flags, reads logs, filters tasks, and traces an empty audit", func() {
		taskName := "e2e-cli-flags-task-" + suffix
		resultSentinel := "cli-e2e-flags-result-" + suffix
		DeferCleanup(deleteK8sResource, "task", taskName)

		By("creating a container task through flags instead of a manifest")
		created := runOrka(home,
			"task", "create",
			"--type", "container",
			"--name", taskName,
			"--image", "busybox:latest",
			"--command", "sh",
			"--command", "-c",
			"--arg", "echo "+resultSentinel,
		)
		expectOrkaSuccess(created, token, resultSentinel)
		Expect(created.Stdout).To(ContainSubstring(taskName))

		By("waiting for the flag-created task to succeed")
		wait := runOrkaWithTimeout(4*time.Minute, home, "task", "wait", taskName, "--timeout", "3m")
		expectOrkaSuccess(wait, token, resultSentinel)
		Expect(wait.Stdout).To(ContainSubstring("succeeded"))

		By("reading the flag-created task result")
		result := runOrka(home, "task", "result", taskName)
		expectOrkaSuccess(result, token)
		Expect(result.Stdout).To(ContainSubstring(resultSentinel))

		By("reading completed task logs from the CLI")
		var logs cliResult
		Eventually(func() error {
			logs = runOrka(home, "task", "logs", taskName)
			expectNoSensitiveOutput(logs, token)
			if logs.Err != nil {
				return fmt.Errorf("task logs failed: %v\n%s", logs.Err, redactedCLIOutput(logs, token))
			}
			if !strings.Contains(logs.Stdout, resultSentinel) {
				return fmt.Errorf("task logs missing sentinel: %s", truncateForLog(logs.Stdout, 512))
			}
			return nil
		}, 45*time.Second, 2*time.Second).Should(Succeed())

		By("filtering succeeded tasks as parseable JSON")
		listSucceeded := runOrka(home, "task", "list", "--status", "Succeeded", "--limit", "5", "-o", "json")
		expectOrkaSuccess(listSucceeded, token)
		expectJSONOutput(listSucceeded.Stdout)

		By("tracing a nonexistent transaction ID")
		auditTrace := runOrka(home, "audit", "trace", "nonexistent-cli-e2e-txn-"+suffix)
		expectOrkaSuccess(auditTrace, token)
		Expect(auditTrace.Stdout).To(ContainSubstring("No tasks found"))
	})
})
