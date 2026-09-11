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
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/orka-agents/orka/test/utils"
)

var _ = Describe("Agent Session Continuity", Ordered, func() {
	const (
		taskName1 = "e2e-session-task-1"
		taskName2 = "e2e-session-task-2"
		agentName = "e2e-session-agent"
		sessionID = "e2e-agent-session-continuity"
	)

	AfterAll(func() {
		By("cleaning up session agent test resources")
		apiBaseURL, cancelPF, portForwardCmd, err := startControllerAPIPortForward(18115)
		Expect(err).NotTo(HaveOccurred(), "Failed to start the cleanup API port-forward")
		defer stopPortForward(cancelPF, portForwardCmd)

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		// Keep the token in memory; the command runner logs command arguments.
		cmd := exec.CommandContext(ctx, "kubectl", "create", "token", serviceAccountName,
			"-n", namespace, "--duration=10m", "--request-timeout=10s")
		tokenBytes, err := cmd.Output()
		Expect(err).NotTo(HaveOccurred(), "Failed to authenticate Session cleanup")
		token := strings.TrimSpace(string(tokenBytes))
		Expect(token).NotTo(BeEmpty())

		Expect(cleanupAgentSessionTasks(ctx, apiBaseURL, token, sessionID,
			[]string{taskName1, taskName2}, utils.Run)).To(Succeed())

		cmd = exec.CommandContext(ctx, "kubectl", "delete", "agent", agentName,
			"-n", namespace, "--ignore-not-found", "--timeout=30s", "--request-timeout=10s")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to delete the Session test Agent")
	})

	AfterEach(func() {
		dumpDebugInfo(taskName1, taskName2)
	})

	It("should reference the same session across multiple tasks", func() {
		By("creating an Agent for session test")
		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"runtime": {
					"contractVersion": "orka.harness.v2",
					"type": "claude",
					"defaultMaxTurns": 5,
					"defaultAllowBash": false
				},
				"model": {"name": "claude-sonnet-4-20250514"}
			}
		}`, agentName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Agent")

		By("creating the first Task with a sessionID")
		task1Manifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "agent",
				"prompt": "first message in session",
				"agentRef": {
					"name": "%s"
				},
				"sessionRef": {
					"name": "%s",
					"create": true,
					"append": true
				},
				"agentRuntime": {
					"maxTurns": 3
				}
			}
		}`, taskName1, namespace, agentName, sessionID)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(task1Manifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create first Task")

		By("verifying the first task is queued on an ACP v2 RuntimePool")
		verifyACPTaskRuntimeForTask(taskName1, acpTaskExpectation{
			ProviderKind:    "claude",
			WorkspaceIntent: "read",
			MaxTurns:        acpInt32(3),
			AllowBash:       acpBool(false),
			SessionName:     sessionID,
		}, 2*time.Minute)

		By("waiting for the first task to settle so the shared Session lease is available")
		_ = waitForTaskCompletion(taskName1, 5*time.Minute)

		var firstRuntimeSessionUID string
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", taskName1,
				"-o", "jsonpath={.status.execution.runtimeSessionUID}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty())
			firstRuntimeSessionUID = output
		}, 2*time.Minute, time.Second).Should(Succeed())

		By("creating the second Task with the same sessionID")
		task2Manifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "agent",
				"prompt": "second message in session",
				"agentRef": {
					"name": "%s"
				},
				"sessionRef": {
					"name": "%s",
					"create": false,
					"append": true
				},
				"agentRuntime": {
					"maxTurns": 3
				}
			}
		}`, taskName2, namespace, agentName, sessionID)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(task2Manifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create second Task")

		By("verifying the second Task reuses the ACP RuntimeSession identity")
		verifyACPTaskRuntimeForTask(taskName2, acpTaskExpectation{
			ProviderKind:    "claude",
			WorkspaceIntent: "read",
			MaxTurns:        acpInt32(3),
			AllowBash:       acpBool(false),
			SessionName:     sessionID,
		}, 2*time.Minute)
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", taskName2,
				"-o", "jsonpath={.status.execution.runtimeSessionUID}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal(firstRuntimeSessionUID))
		}, 5*time.Minute, time.Second).Should(Succeed())
	})
})

// cleanupAgentSessionTasks requests cancellation before archival, then waits for
// normal Task finalization. Session-bound Tasks retain their cleanup authority
// until Session DELETE has durably archived the original turn receipts.
func cleanupAgentSessionTasks(
	ctx context.Context, apiBaseURL, token, sessionName string,
	taskNames []string, run func(*exec.Cmd) (string, error),
) error {
	for _, taskName := range taskNames {
		cmd := exec.CommandContext(ctx, "kubectl", "delete", "task", taskName,
			"-n", namespace, "--ignore-not-found", "--wait=false", "--request-timeout=10s")
		if _, err := run(cmd); err != nil {
			return fmt.Errorf("request cancellation of Task %s: %w", taskName, err)
		}
	}

	endpoint := strings.TrimRight(apiBaseURL, "/") + "/api/v1/sessions/" + url.PathEscape(sessionName) +
		"?namespace=" + url.QueryEscape(namespace)
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
		if err != nil {
			return false, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			return false, err
		}
		if err := resp.Body.Close(); err != nil {
			return false, err
		}
		switch resp.StatusCode {
		case http.StatusNoContent, http.StatusNotFound:
			return true, nil
		case http.StatusConflict:
			// Active turns and unsettled original cleanup remain owned. Retry
			// the same canonical Session cleanup request within the deadline.
			return false, nil
		default:
			return false, fmt.Errorf("Session DELETE returned HTTP %d", resp.StatusCode)
		}
	}); err != nil {
		return fmt.Errorf("archive Session %s: %w", sessionName, err)
	}

	return wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		for _, taskName := range taskNames {
			cmd := exec.CommandContext(ctx, "kubectl", "get", "task", taskName,
				"-n", namespace, "--ignore-not-found", "-o", "name", "--request-timeout=10s")
			output, err := run(cmd)
			if err != nil {
				return false, fmt.Errorf("observe Task %s deletion: %w", taskName, err)
			}
			if strings.TrimSpace(output) != "" {
				return false, nil
			}
		}
		return true, nil
	})
}
