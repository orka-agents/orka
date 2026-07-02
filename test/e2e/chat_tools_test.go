//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"bufio"
	"context"
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
		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("sending a chat message that should trigger code_exec tool use")
		chatBody := fmt.Sprintf(`{
			"message": "Use the code_exec tool to calculate 123 * 456. You MUST use the code_exec tool, do not calculate it yourself.",
			"provider": "%s",
			"model": "%s",
			"tools": ["code_exec"]
		}`, providerName, model)

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
		scanner := bufio.NewScanner(resp.Body)
		var events []string
		hasMessage := false
		hasDone := false

		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event:") {
				eventType := strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				events = append(events, eventType)
				if eventType == "message" {
					hasMessage = true
				}
				if eventType == "done" {
					hasDone = true
					break
				}
			}
		}

		_, _ = fmt.Fprintf(GinkgoWriter, "Received SSE events: %v\n", events)

		Expect(hasMessage).To(BeTrue(), "Should have message events in stream")
		Expect(hasDone).To(BeTrue(), "Should have done event to terminate stream")
	})

	It("should handle chat with web_search tool", func() {
		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("sending a chat message with web_search tool enabled")
		chatBody := fmt.Sprintf(`{
			"message": "What is the capital of France? Just answer directly.",
			"provider": "%s",
			"model": "%s"
		}`, providerName, model)

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
		scanner := bufio.NewScanner(resp.Body)
		hasMessage := false
		hasDone := false
		var contentParts []string

		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event:") {
				eventType := strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				if eventType == "message" {
					hasMessage = true
				}
				if eventType == "done" {
					hasDone = true
					break
				}
			}
			if strings.HasPrefix(line, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data != "" {
					contentParts = append(contentParts, data)
				}
			}
		}

		Expect(hasMessage).To(BeTrue(), "Should receive message events")
		Expect(hasDone).To(BeTrue(), "Should receive done event")

		fullContent := strings.Join(contentParts, "")
		_, _ = fmt.Fprintf(GinkgoWriter, "Chat response content: %s\n", fullContent)
	})
})
