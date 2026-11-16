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
	"net/http"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/orka-agents/orka/test/utils"
)

var _ = Describe("Chat with Tool Execution", Ordered, func() {
	var (
		apiBaseURL     string
		token          string
		cancelPF       context.CancelFunc
		portForwardCmd *exec.Cmd
	)

	const providerName = "e2e-chat-tool-provider"

	BeforeAll(func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		By("setting up port-forward to controller API")
		var err error
		apiBaseURL, cancelPF, portForwardCmd, err = startControllerAPIPortForward(18083)
		Expect(err).NotTo(HaveOccurred())

		By("getting a service account token")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())

		By("creating a provider for chat tool tests")
		createProviderCRD(providerName, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, e2eOpenAIModel)
	})

	AfterAll(func() {
		stopPortForward(cancelPF, portForwardCmd)
		cmd := exec.Command("kubectl", "delete", "provider", providerName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	It("should execute tools during chat and return tool results in SSE stream", func() {
		const marker = "[e2e:chat-tools-list-tools]"
		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		// The chat coordinator exposes management tools such as list_tools,
		// not worker built-ins such as code_exec.
		By("sending a chat message that should trigger list_tools tool use")
		chatBody := fmt.Sprintf(`{
			"message": "Use the list_tools tool to list the tools in this namespace, then summarize them in one sentence. You MUST use the list_tools tool. %s",
			"provider": "%s",
			"model": "%s",
			"tools": ["list_tools"]
		}`, marker, providerName, model)

		req, err := http.NewRequest("POST", apiBaseURL+"/api/v1/chat",
			strings.NewReader(chatBody))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")

		client := &http.Client{Timeout: 2 * time.Minute}
		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK), "Chat endpoint should return 200")

		By("reading SSE events and checking for tool-related events")
		sseEvents := readChatSSEEvents(resp.Body)
		events := chatSSEEventNames(sseEvents)

		_, _ = fmt.Fprintf(GinkgoWriter, "Received SSE events: %v\n", events)

		Expect(events).To(ContainElement("message"), "Should have message events in stream")
		Expect(events).To(ContainElement("done"), "Should have done event to terminate stream")

		if e2eMockOpenAI {
			By("verifying the coordinator executed list_tools and streamed its result")
			Expect(events).To(Equal([]string{"status", "tool_call", "tool_result", "message", "done"}))
			var toolResult struct {
				Name   string          `json:"name"`
				Result json.RawMessage `json:"result"`
			}
			Expect(json.Unmarshal([]byte(sseEvents[2].Data), &toolResult)).To(Succeed())
			Expect(toolResult.Name).To(Equal("list_tools"))
			Expect(string(toolResult.Result)).To(ContainSubstring(`"success":true`))
			Expect(expectMockLLMToolResult(marker, "list_tools", 30*time.Second)).To(ContainSubstring(`"success":true`))
			Expect(chatSSEMessageText(sseEvents)).To(Equal("list_tools returned the tools in this namespace."))
			usage := chatSSEDoneUsage(sseEvents)
			Expect(usage.LLMCalls).To(Equal(2))
			Expect(usage.ToolCalls).To(Equal(1))
		}
	})

	It("should handle chat with web_search tool", func() {
		const marker = "[e2e:chat-tools-direct-answer]"
		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("sending a chat message with web_search tool enabled")
		chatBody := fmt.Sprintf(`{
			"message": "What is the capital of France? Just answer directly. %s",
			"provider": "%s",
			"model": "%s"
		}`, marker, providerName, model)

		req, err := http.NewRequest("POST", apiBaseURL+"/api/v1/chat",
			strings.NewReader(chatBody))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")

		client := &http.Client{Timeout: 2 * time.Minute}
		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		By("verifying we get a complete response")
		sseEvents := readChatSSEEvents(resp.Body)
		events := chatSSEEventNames(sseEvents)

		Expect(events).To(ContainElement("message"), "Should receive message events")
		Expect(events).To(ContainElement("done"), "Should receive done event")

		fullContent := chatSSEMessageText(sseEvents)
		_, _ = fmt.Fprintf(GinkgoWriter, "Chat response content: %s\n", fullContent)

		if e2eMockOpenAI {
			By("verifying the scripted direct answer needed no tools")
			Expect(events).To(Equal([]string{"status", "message", "done"}))
			Expect(fullContent).To(Equal("Paris"))
			expectMockLLMServed(marker, 30*time.Second)
		}
	})
})
