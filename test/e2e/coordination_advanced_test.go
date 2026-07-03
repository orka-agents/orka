//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/orka-agents/orka/test/utils"
)

var _ = Describe("Advanced Coordination Tools", Ordered, func() {
	const (
		coordAdvProvider = "e2e-coord-adv-provider"
		coordAdvWorker   = "e2e-coord-adv-worker"
	)

	var model string

	BeforeAll(func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model = e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating a shared provider for advanced coordination tests")
		createProviderCRD(coordAdvProvider, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, model)

		By("creating a shared worker agent")
		workerManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"providerRef": {"name": "%s"},
				"model": {"name": "%s"}
			}
		}`, coordAdvWorker, namespace, coordAdvProvider, model)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(workerManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create shared worker agent")
	})

	AfterAll(func() {
		By("cleaning up shared advanced coordination resources")
		cmd := exec.Command("kubectl", "delete", "agent", coordAdvWorker, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "provider", coordAdvProvider, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	// ── Test 1: cancel_task tool ──

	It("should cancel a delegated task using cancel_task", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		const (
			coordName = "e2e-coord-adv-cancel-coord"
			taskName  = "e2e-coord-adv-cancel-task"
		)

		defer func() {
			cmd := exec.Command("kubectl", "delete", "task", taskName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "tasks", "-l",
				fmt.Sprintf("orka.ai/parent-task=%s", taskName), "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "agent", coordName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}()

		DeferCleanup(func() { dumpDebugInfo(taskName) })

		By("creating a coordinator agent")
		coordManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {"name": "%s", "namespace": "%s"},
			"spec": {
				"providerRef": {"name": "%s"},
				"model": {"name": "%s"},
				"coordination": {
					"enabled": true,
					"maxDepth": 2,
					"maxConcurrentChildren": 3,
					"allowedAgents": [{"name": "%s"}]
				}
			}
		}`, coordName, namespace, coordAdvProvider, model, coordAdvWorker)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(coordManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("creating a task that delegates and immediately cancels")
		prompt := fmt.Sprintf("Delegate a task to the worker agent named '%s' with prompt 'sleep for a very long time by running code that takes minutes'. Then immediately cancel it using the cancel_task tool. Report whether cancellation succeeded.", coordAdvWorker)
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": "%s", "namespace": "%s"},
			"spec": {
				"type": "ai",
				"agentRef": {"name": "%s"},
				"ai": {
					"prompt": "%s",
					"model": "%s",
					"providerRef": {"name": "%s"}
				}
			}
		}`, taskName, namespace, coordName, prompt, model, coordAdvProvider)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for child tasks to appear")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "tasks", "-l",
				fmt.Sprintf("orka.ai/parent-task=%s", taskName),
				"-o", "jsonpath={.items[*].metadata.name}", "-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).NotTo(BeEmpty())
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		By("verifying at least one child task reaches Cancelled phase")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "tasks", "-l",
				fmt.Sprintf("orka.ai/parent-task=%s", taskName),
				"-o", "jsonpath={.items[*].status.phase}", "-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("Cancelled"))
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for coordinator task to complete")
		phase := waitForTaskCompletion(taskName, 10*time.Minute)
		Expect(phase).To(BeElementOf("Succeeded", "Failed"))
	})

	// ── Test 2: send_message + check_messages ──

	It("should enable inter-agent messaging with send_message and check_messages", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		const (
			coordName = "e2e-coord-adv-msg-coord"
			taskName  = "e2e-coord-adv-msg-task"
		)

		defer func() {
			cmd := exec.Command("kubectl", "delete", "task", taskName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "tasks", "-l",
				fmt.Sprintf("orka.ai/parent-task=%s", taskName), "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "agent", coordName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}()

		DeferCleanup(func() { dumpDebugInfo(taskName) })

		By("creating a coordinator agent")
		coordManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {"name": "%s", "namespace": "%s"},
			"spec": {
				"providerRef": {"name": "%s"},
				"model": {"name": "%s"},
				"coordination": {
					"enabled": true,
					"maxDepth": 2,
					"maxConcurrentChildren": 3,
					"allowedAgents": [{"name": "%s"}]
				}
			}
		}`, coordName, namespace, coordAdvProvider, model, coordAdvWorker)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(coordManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("creating a task that delegates two subtasks for messaging")
		prompt := fmt.Sprintf(`Delegate two tasks to the worker agent named '%s'. Task A prompt: 'Send a message to all siblings saying hello from task A using the send_message tool with to_task set to *. Then report message sent.' Task B prompt: 'Check for messages using the check_messages tool. Report any messages you received.' Wait for both tasks to complete and report the results.`, coordAdvWorker)
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": "%s", "namespace": "%s"},
			"spec": {
				"type": "ai",
				"agentRef": {"name": "%s"},
				"ai": {
					"prompt": "%s",
					"model": "%s",
					"providerRef": {"name": "%s"}
				}
			}
		}`, taskName, namespace, coordName, prompt, model, coordAdvProvider)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for at least two child tasks to be created")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "tasks", "-l",
				fmt.Sprintf("orka.ai/parent-task=%s", taskName),
				"-o", "jsonpath={.items[*].metadata.name}", "-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			names := strings.Fields(strings.TrimSpace(output))
			g.Expect(len(names)).To(BeNumerically(">=", 2),
				"At least two child tasks should be created for messaging test")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for coordinator to reach terminal phase")
		phase := waitForTaskCompletion(taskName, 10*time.Minute)
		Expect(phase).To(BeElementOf("Succeeded", "Failed"))

		By("verifying result is available")
		verifyResultAvailable(taskName)
	})

	// ── Test 3: auto-retry (self-healing delegation) ──

	It("should auto-retry a failed delegated task", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		const (
			coordName = "e2e-coord-adv-retry-coord"
			taskName  = "e2e-coord-adv-retry-task"
		)

		defer func() {
			cmd := exec.Command("kubectl", "delete", "task", taskName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "tasks", "-l",
				fmt.Sprintf("orka.ai/parent-task=%s", taskName), "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "agent", coordName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}()

		DeferCleanup(func() { dumpDebugInfo(taskName) })

		By("creating a coordinator agent")
		coordManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {"name": "%s", "namespace": "%s"},
			"spec": {
				"providerRef": {"name": "%s"},
				"model": {"name": "%s"},
				"coordination": {
					"enabled": true,
					"maxDepth": 2,
					"maxConcurrentChildren": 3,
					"allowedAgents": [{"name": "%s"}]
				}
			}
		}`, coordName, namespace, coordAdvProvider, model, coordAdvWorker)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(coordManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("creating a task that delegates with auto_retry enabled")
		prompt := fmt.Sprintf(`Delegate a task to the worker agent named '%s' with auto_retry enabled and max_retries=2. Use this prompt for the delegation: 'Exit with an error by calling code_exec with invalid syntax: }}}'. Wait for the result and report the outcome.`, coordAdvWorker)
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": "%s", "namespace": "%s"},
			"spec": {
				"type": "ai",
				"agentRef": {"name": "%s"},
				"ai": {
					"prompt": "%s",
					"model": "%s",
					"providerRef": {"name": "%s"}
				}
			}
		}`, taskName, namespace, coordName, prompt, model, coordAdvProvider)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for at least one retry task with retried-from annotation")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "tasks", "-l",
				fmt.Sprintf("orka.ai/parent-task=%s", taskName),
				"-o", "jsonpath={range .items[*]}{.metadata.annotations.orka\\.ai/retried-from}{' '}{end}",
				"-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			// At least one task should have the retried-from annotation set
			fields := strings.Fields(strings.TrimSpace(output))
			g.Expect(len(fields)).To(BeNumerically(">=", 1),
				"At least one retry task should have orka.ai/retried-from annotation")
		}, 10*time.Minute, 10*time.Second).Should(Succeed())

		By("waiting for coordinator to reach terminal phase")
		phase := waitForTaskCompletion(taskName, 12*time.Minute)
		Expect(phase).To(BeElementOf("Succeeded", "Failed"))
	})

	// ── Test 4: create_agent + delete_agent tools ──

	It("should dynamically create and delete an agent via coordination tools", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		const (
			coordName    = "e2e-coord-adv-dynagent-coord"
			taskName     = "e2e-coord-adv-dynagent-task"
			dynamicAgent = "e2e-dynamic-agent"
		)

		defer func() {
			cmd := exec.Command("kubectl", "delete", "task", taskName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "tasks", "-l",
				fmt.Sprintf("orka.ai/parent-task=%s", taskName), "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "agent", coordName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "agent", dynamicAgent, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}()

		DeferCleanup(func() { dumpDebugInfo(taskName) })

		By("creating a coordinator agent with dynamic agent in allowedAgents")
		coordManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {"name": "%s", "namespace": "%s"},
			"spec": {
				"providerRef": {"name": "%s"},
				"model": {"name": "%s"},
				"coordination": {
					"enabled": true,
					"maxDepth": 2,
					"maxConcurrentChildren": 3,
					"allowedAgents": [{"name": "%s"}, {"name": "%s"}]
				}
			}
		}`, coordName, namespace, coordAdvProvider, model, coordAdvWorker, dynamicAgent)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(coordManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("creating a task that creates, delegates to, and deletes a dynamic agent")
		prompt := fmt.Sprintf(`1. Create a new agent called '%s' using the create_agent tool with provider '%s' and model '%s'. 2. Delegate 'What is 10+10? Reply with just the number.' to '%s'. 3. Wait for the result. 4. Delete the agent '%s' using delete_agent. 5. Report the result and confirm cleanup.`,
			dynamicAgent, coordAdvProvider, model, dynamicAgent, dynamicAgent)
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": "%s", "namespace": "%s"},
			"spec": {
				"type": "ai",
				"agentRef": {"name": "%s"},
				"ai": {
					"prompt": "%s",
					"model": "%s",
					"providerRef": {"name": "%s"}
				}
			}
		}`, taskName, namespace, coordName, prompt, model, coordAdvProvider)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for child task to be created for dynamic agent")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "tasks", "-l",
				fmt.Sprintf("orka.ai/parent-task=%s", taskName),
				"-o", "jsonpath={.items[*].metadata.name}", "-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).NotTo(BeEmpty())
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for coordinator to reach terminal phase")
		phase := waitForTaskCompletion(taskName, 10*time.Minute)
		Expect(phase).To(BeElementOf("Succeeded", "Failed"))

		By("verifying the dynamic agent was cleaned up")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "agent", dynamicAgent,
				"-n", namespace, "--ignore-not-found", "-o", "jsonpath={.metadata.name}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(BeEmpty(),
				"Dynamic agent should have been deleted")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})

	// ── Test 5: Internal messaging API (structural – no LLM) ──

	It("should send and receive messages via internal messaging API", func() {
		var (
			apiBaseURL     string
			portForwardCmd *exec.Cmd
			cancelPF       context.CancelFunc
		)

		By("setting up port-forward to controller API")
		var err error
		apiBaseURL, cancelPF, portForwardCmd, err = startControllerAPIPortForward(18089)
		Expect(err).NotTo(HaveOccurred(), "Failed to start controller API port-forward")

		defer func() {
			stopPortForward(cancelPF, portForwardCmd)
		}()

		By("getting a service account token for auth")
		token, err := serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())

		const (
			fromTask   = "e2e-coord-adv-msg-sender"
			toTask     = "e2e-coord-adv-msg-receiver"
			parentTask = "e2e-coord-adv-msg-parent"
			msgContent = "hello from e2e messaging test"
		)

		By("sending a message via POST /internal/v1/messages")
		msgBody := fmt.Sprintf(`{"fromTask":"%s","toTask":"%s","parentTask":"%s","content":"%s"}`,
			fromTask, toTask, parentTask, msgContent)
		req, err := http.NewRequest("POST",
			fmt.Sprintf("%s/internal/v1/messages/%s", apiBaseURL, namespace),
			strings.NewReader(msgBody))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusNoContent),
			"POST message should return 204 No Content")

		By("retrieving messages via GET /internal/v1/messages")
		getURL := fmt.Sprintf("%s/internal/v1/messages/%s/%s?parentTask=%s&markRead=false",
			apiBaseURL, namespace, toTask, parentTask)
		req, err = http.NewRequest("GET", getURL, nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err = http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())

		var messages []map[string]interface{}
		err = json.Unmarshal(body, &messages)
		Expect(err).NotTo(HaveOccurred(), "Response should be valid JSON array")
		Expect(messages).NotTo(BeEmpty(), "Should have received at least one message")

		msg := messages[0]
		Expect(msg["fromTask"]).To(Equal(fromTask), "Message fromTask should match sender")
		Expect(msg["content"]).To(Equal(msgContent), "Message content should match")
	})
})
