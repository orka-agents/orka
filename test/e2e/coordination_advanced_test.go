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

// Coordination messaging fails closed without Task provenance admission, which
// the default e2e deployment does not enable.
const coordinationMessagingSkipReason = "coordination messaging requires Task provenance admission, " +
	"which this deployment does not enable"

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
		// The scripted cancel child sleeps in code_exec so it is still running
		// when its coordinator cancels it.
		workerTools := ""
		if e2eMockOpenAI {
			workerTools = `, "tools": [{"name": "code_exec"}]`
		}
		workerManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"providerRef": {"name": "%s"},
				"model": {"name": "%s"}%s
			}
		}`, coordAdvWorker, namespace, coordAdvProvider, model, workerTools)

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
		// cancel_task writes the child's status from the AI worker, which both
		// RBAC (no tasks/status) and the admission webhook (controller-only
		// status writes) reject, so the child is never cancelled.
		Skip("cancel_task cannot cancel children: the AI worker may not update tasks/status")

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
		const marker = "[e2e:coord-adv-cancel]"
		prompt := fmt.Sprintf("Delegate a task to the worker agent named '%s' with prompt 'sleep for a very long time by running code that takes minutes'. Then immediately cancel it using the cancel_task tool. Report whether cancellation succeeded. %s", coordAdvWorker, marker)
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

		var sleeper string
		if e2eMockOpenAI {
			By("scripting the coordinator to cancel its generated child Task")
			sleeper = waitForCoordinationChild(taskName, "[e2e:coord-adv-sleeper]", 3*time.Minute)
			injectMockLLMFixtures(coordinationMockToolCall(marker, "e2e-coord-adv-cancel-hold",
				"cancel_task", map[string]any{"task_name": sleeper, "reason": "e2e cancel"}))
		}

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

		if e2eMockOpenAI {
			By("verifying cancel_task cancelled the running child")
			Expect(phase).To(Equal("Succeeded"))
			waitForTaskPhase(sleeper, "Cancelled", time.Minute)
			expectCoordinationToolResult(marker, "cancel_task", `"status":"cancelled"`, time.Minute)
		}
	})

	// ── Test 2: send_message + check_messages ──

	It("should enable inter-agent messaging with send_message and check_messages", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")
		Skip(coordinationMessagingSkipReason)

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
		const marker = "[e2e:coord-adv-msg]"
		prompt := fmt.Sprintf(`Delegate two tasks to the worker agent named '%s'. Task A prompt: 'Send a message to all siblings saying hello from task A using the send_message tool with to_task set to *. Then report message sent.' Task B prompt: 'Check for messages using the check_messages tool. Report any messages you received.' Wait for both tasks to complete and report the results. %s`, coordAdvWorker, marker)
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

		var sender, reader string
		if e2eMockOpenAI {
			By("scripting the coordinator to wait for both generated child Tasks")
			sender = waitForCoordinationChild(taskName, "[e2e:coord-adv-sender]", 3*time.Minute)
			reader = waitForCoordinationChild(taskName, "[e2e:coord-adv-reader]", time.Minute)
			injectMockLLMFixtures(coordinationMockToolCall(marker, "e2e-coord-adv-msg-hold",
				"wait_for_tasks", map[string]any{"tasks": []string{sender, reader}, "timeout": "5m"}))
		}

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

		if e2eMockOpenAI {
			By("verifying both children ran their messaging tools")
			Expect(phase).To(Equal("Succeeded"))
			waitForTaskPhase(sender, "Succeeded", time.Minute)
			waitForTaskPhase(reader, "Succeeded", time.Minute)
			expectCoordinationToolResult("[e2e:coord-adv-sender]", "send_message", "Message sent to all siblings", time.Minute)
			expectMockLLMToolResult("[e2e:coord-adv-reader]", "check_messages", time.Minute)
			expectCoordinationToolResult(marker, "wait_for_tasks", "E2E-READER-DONE", time.Minute)

			// The reader may check before the sender broadcasts, so read its
			// inbox directly. It checked with mark_read=false, which leaves the
			// broadcast unread until the Tasks are deleted.
			By("verifying the broadcast reached the reader's inbox")
			apiBaseURL, cancelPF, portForwardCmd, err := startControllerAPIPortForward(18089)
			Expect(err).NotTo(HaveOccurred(), "Failed to start controller API port-forward")
			defer stopPortForward(cancelPF, portForwardCmd)
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/internal/v1/messages/%s/%s?parentTask=%s&markRead=false",
				apiBaseURL, namespace, reader, taskName), nil)
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			var inbox []struct {
				FromTask string `json:"fromTask"`
				Content  string `json:"content"`
			}
			Expect(json.NewDecoder(resp.Body).Decode(&inbox)).To(Succeed())
			Expect(inbox).To(ContainElement(SatisfyAll(
				HaveField("FromTask", sender),
				HaveField("Content", ContainSubstring("E2E-HELLO-FROM-A")),
			)))
		}
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
		const marker = "[e2e:coord-adv-retry]"
		prompt := fmt.Sprintf(`Delegate a task to the worker agent named '%s' with auto_retry enabled and max_retries=2. Use this prompt for the delegation: 'Exit with an error by calling code_exec with invalid syntax: }}}'. Wait for the result and report the outcome. %s`, coordAdvWorker, marker)
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

		if e2eMockOpenAI {
			// wait_for_tasks reports retry metadata for auto_retry children and
			// leaves the retry decision to the coordinator, so the script
			// re-delegates once the first child fails.
			By("scripting the coordinator to collect the failure and retry")
			failed := waitForCoordinationChild(taskName, "[e2e:coord-adv-flaky]", 3*time.Minute)
			injectMockLLMFixtures(coordinationMockToolCall(marker, "e2e-coord-adv-retry-hold",
				"wait_for_tasks", map[string]any{"tasks": []string{failed}, "timeout": "5m"}))
			retried := waitForCoordinationChild(taskName, "[e2e:coord-adv-recovered]", 5*time.Minute)
			injectMockLLMFixtures(coordinationMockToolCall(marker, "e2e-coord-adv-retry-hold",
				"wait_for_tasks", map[string]any{"tasks": []string{retried}, "timeout": "5m"}))

			By("waiting for coordinator to reach terminal phase")
			Expect(waitForTaskCompletion(taskName, 12*time.Minute)).To(Equal("Succeeded"))

			By("verifying the failure carried retry metadata and the retry recovered")
			waitForTaskPhase(failed, "Failed", time.Minute)
			waitForTaskPhase(retried, "Succeeded", time.Minute)
			output, err := utils.Run(exec.Command("kubectl", "get", "task", failed, "-n", namespace, "-o",
				`jsonpath={.metadata.annotations.orka\.ai/auto-retry}/{.metadata.annotations.orka\.ai/max-retries}/{.metadata.annotations.orka\.ai/retry-count}`))
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("true/2/0"))
			expectCoordinationToolResult(marker, "wait_for_tasks", `"maxRetries": 2`, time.Minute)
			expectCoordinationToolResult(marker, "wait_for_tasks", "E2E-RECOVERED", time.Minute)
			return
		}

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
		allowedAgents := fmt.Sprintf(`, "allowedAgents": [{"name": "%s"}, {"name": "%s"}]`, coordAdvWorker, dynamicAgent)
		if e2eMockOpenAI {
			// create_agent names agents <task>-<role>-<hash>, so only a
			// coordinator without an allowlist can delegate to the agent the
			// script creates.
			allowedAgents = ""
		}
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
					"maxConcurrentChildren": 3%s
				}
			}
		}`, coordName, namespace, coordAdvProvider, model, allowedAgents)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(coordManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("creating a task that creates, delegates to, and deletes a dynamic agent")
		const marker = "[e2e:coord-adv-dynagent]"
		prompt := fmt.Sprintf(`1. Create a new agent called '%s' using the create_agent tool with provider '%s' and model '%s'. 2. Delegate 'What is 10+10? Reply with just the number.' to '%s'. 3. Wait for the result. 4. Delete the agent '%s' using delete_agent. 5. Report the result and confirm cleanup. %s`,
			dynamicAgent, coordAdvProvider, model, dynamicAgent, dynamicAgent, marker)
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

		var created, child string
		if e2eMockOpenAI {
			By("scripting the coordinator to delegate to, wait for, and delete the agent it created")
			created = waitForCreatedAgent(taskName, 3*time.Minute)
			injectMockLLMFixtures(
				coordinationMockToolCall(marker, "e2e-coord-adv-dynagent-hold-1", "delegate_task", map[string]any{
					"agent": created, "prompt": "[e2e:coord-adv-sum] What is 10+10? Reply with just the number.",
				}),
				coordinationMockToolCall(marker, "E2E-SUM-20", "delete_agent", map[string]any{"name": created}),
				coordinationMockText(marker, created, "The dynamic agent answered 20 and was cleaned up."),
			)
			child = waitForCoordinationChild(taskName, "[e2e:coord-adv-sum]", 3*time.Minute)
			injectMockLLMFixtures(coordinationMockToolCall(marker, "e2e-coord-adv-dynagent-hold-2",
				"wait_for_tasks", map[string]any{"tasks": []string{child}, "timeout": "5m"}))
		}

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

		if e2eMockOpenAI {
			By("verifying the created agent served the delegated child")
			Expect(phase).To(Equal("Succeeded"))
			output, err := utils.Run(exec.Command("kubectl", "get", "task", child, "-n", namespace,
				"-o", "jsonpath={.spec.agentRef.name}"))
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal(created))
			waitForTaskPhase(child, "Succeeded", time.Minute)
			expectCoordinationToolResult(marker, "wait_for_tasks", "E2E-SUM-20", time.Minute)
			expectMockLLMToolResult(marker, "delete_agent", time.Minute)
		}
	})

	// ── Test 5: Internal messaging API (structural – no LLM) ──

	It("should send and receive messages via internal messaging API", func() {
		Skip(coordinationMessagingSkipReason)
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

// waitForCreatedAgent returns the Agent that parent's create_agent call made.
func waitForCreatedAgent(parent string, timeout time.Duration) string {
	var name string
	EventuallyWithOffset(1, func(g Gomega) {
		output, err := utils.Run(exec.Command("kubectl", "get", "agents", "-n", namespace,
			"-l", fmt.Sprintf("orka.ai/parent-task=%s,orka.ai/created-by=create_agent", parent),
			"-o", "jsonpath={.items[*].metadata.name}"))
		g.Expect(err).NotTo(HaveOccurred())
		names := strings.Fields(output)
		g.Expect(names).To(HaveLen(1), "expected one agent created by %s", parent)
		name = names[0]
	}, timeout, 500*time.Millisecond).Should(Succeed())
	return name
}
