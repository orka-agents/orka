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

var _ = Describe("Chat and OpenAI-Compatible API", Ordered, func() {
	var (
		apiBaseURL     string
		portForwardCmd *exec.Cmd
		cancelPF       context.CancelFunc
		token          string
	)

	BeforeAll(func() {
		By("setting up port-forward to controller API")
		var err error
		apiBaseURL, cancelPF, portForwardCmd, err = startControllerAPIPortForward(18081)
		Expect(err).NotTo(HaveOccurred())

		By("getting a service account token")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())
	})

	AfterAll(func() {
		stopPortForward(cancelPF, portForwardCmd)
	})

	// --- Chat Config Endpoint ---

	It("should return chat config from GET /api/v1/chat/config", func() {
		req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/chat/config", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		bodyStr := string(body)
		// Chat config should contain known fields
		Expect(bodyStr).To(ContainSubstring("enabled"))
		Expect(bodyStr).To(ContainSubstring("maxIterations"))
	})

	// --- Chat SSE Streaming ---

	It("should stream SSE events from POST /api/v1/chat", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")
		const marker = "[e2e:chat-api-sse]"

		By("ensuring an OpenAI provider exists for chat")
		createProviderCRD("e2e-chat-provider", "openai", "e2e-openai-secret", "api-key",
			e2eOpenAIBaseURL, e2eOpenAIModel)
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "provider", "e2e-chat-provider",
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		By("sending a chat message with SSE accept header")
		chatBody := fmt.Sprintf(`{"message":"What is 2+2? Reply with just the number. %s","provider":"e2e-chat-provider","model":"%s"}`,
			marker, e2eOpenAIModel)

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

		By("reading SSE events")
		sseEvents := readChatSSEEvents(resp.Body)
		events := chatSSEEventNames(sseEvents)

		By("verifying expected event types were received")
		// Should have at least a message and done event
		Expect(events).NotTo(BeEmpty(), "Should receive SSE events")
		Expect(events).To(ContainElement("done"), "Should receive a 'done' event")

		if e2eMockOpenAI {
			By("verifying the scripted answer streamed back from a single model call")
			Expect(events).To(Equal([]string{"status", "message", "done"}))
			Expect(chatSSEMessageText(sseEvents)).To(Equal("4"))
			usage := chatSSEDoneUsage(sseEvents)
			Expect(usage.LLMCalls).To(Equal(1))
			Expect(usage.ToolCalls).To(BeZero())
			expectMockLLMServed(marker, 30*time.Second)
		}
	})

	// --- OpenAI-Compatible: /openai/v1/models ---

	It("should list models via GET /openai/v1/models", func() {
		req, err := http.NewRequest("GET", apiBaseURL+"/openai/v1/models", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		bodyStr := string(body)
		Expect(bodyStr).To(ContainSubstring("object"))
	})

	// --- OpenAI-Compatible: /openai/v1/chat/completions ---

	It("should proxy chat completions via POST /openai/v1/chat/completions", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")
		const marker = "[e2e:chat-api-openai-compat]"

		By("ensuring an OpenAI provider exists")
		createProviderCRD("e2e-oai-compat-provider", "openai", "e2e-openai-secret", "api-key",
			e2eOpenAIBaseURL, e2eOpenAIModel)
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "provider", "e2e-oai-compat-provider",
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		By("sending an OpenAI-format chat completion request")
		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}
		oaiBody := fmt.Sprintf(`{
			"model": "e2e-oai-compat-provider/%s",
			"messages": [{"role": "user", "content": "What is 1+1? Reply with just the number. %s"}],
			"max_tokens": 50
		}`, model, marker)

		req, err := http.NewRequest("POST", apiBaseURL+"/openai/v1/chat/completions",
			strings.NewReader(oaiBody))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 2 * time.Minute}
		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		bodyStr := string(body)
		Expect(bodyStr).To(ContainSubstring("choices"), "Response should contain choices")
		Expect(bodyStr).To(ContainSubstring("chat.completion"), "Response object should be chat.completion")

		if e2eMockOpenAI {
			By("verifying the coordinator stripped the goal-state sentinel from the scripted answer")
			var completion struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			Expect(json.Unmarshal(body, &completion)).To(Succeed())
			Expect(completion.Choices).To(HaveLen(1))
			Expect(completion.Choices[0].Message.Content).To(Equal("2"))

			By("verifying one model call carried the injected Orka tools")
			expectMockLLMServed(marker, 30*time.Second)
			requests, err := mockLLMRequestsFor(marker)
			Expect(err).NotTo(HaveOccurred())
			Expect(requests).To(HaveLen(1), "a sentinel-prefixed answer must end the loop without a premature-end re-prompt")
			var toolNames []string
			for _, tool := range requests[0].Body.Tools {
				toolNames = append(toolNames, tool.Function.Name)
			}
			Expect(toolNames).To(ContainElements("code_exec", "create_ai_task"))
		}
	})

	// --- Session Listing and Deletion ---

	It("should list sessions via GET /api/v1/sessions", func() {
		req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/sessions", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		// Should return 200 even if empty
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})

	It("should create a session via chat and then delete it", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")
		const (
			marker         = "[e2e:chat-api-session]"
			followUpMarker = "[e2e:chat-api-session-followup]"
		)

		By("ensuring an OpenAI provider exists")
		createProviderCRD("e2e-session-provider", "openai", "e2e-openai-secret", "api-key",
			e2eOpenAIBaseURL, e2eOpenAIModel)
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "provider", "e2e-session-provider",
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		By("sending a chat message to create a session")
		chatBody := fmt.Sprintf(`{"message":"Say hello %s","provider":"e2e-session-provider","model":"%s","sessionId":"e2e-test-session"}`,
			marker, e2eOpenAIModel)

		req, err := http.NewRequest("POST", apiBaseURL+"/api/v1/chat",
			strings.NewReader(chatBody))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 2 * time.Minute}
		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		// Drain the response body
		_, _ = io.ReadAll(resp.Body)

		By("verifying the session can be retrieved")
		var sessionBody []byte
		Eventually(func(g Gomega) {
			req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/sessions/e2e-test-session", nil)
			g.Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+token)

			resp, err := http.DefaultClient.Do(req)
			g.Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
			sessionBody, err = io.ReadAll(resp.Body)
			g.Expect(err).NotTo(HaveOccurred())
		}, 30*time.Second, time.Second).Should(Succeed())

		if e2eMockOpenAI {
			By("verifying the session transcript holds the scripted turn")
			var session struct {
				Transcript string `json:"transcript"`
			}
			Expect(json.Unmarshal(sessionBody, &session)).To(Succeed())
			Expect(session.Transcript).To(ContainSubstring(marker))
			Expect(session.Transcript).To(ContainSubstring("Hello from the aimock chat session."))

			By("continuing the session and verifying the prior turn reaches the model")
			followUpBody := fmt.Sprintf(`{"message":"What did my first message ask? %s","provider":"e2e-session-provider","model":"%s","sessionId":"e2e-test-session"}`,
				followUpMarker, e2eOpenAIModel)
			req, err := http.NewRequest("POST", apiBaseURL+"/api/v1/chat", strings.NewReader(followUpBody))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json")

			resp, err := client.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			var reply struct {
				SessionID string `json:"sessionId"`
				Message   string `json:"message"`
			}
			Expect(json.NewDecoder(resp.Body).Decode(&reply)).To(Succeed())
			Expect(reply.SessionID).To(Equal("e2e-test-session"))
			Expect(reply.Message).To(Equal("Your first message asked me to say hello."))

			requests, err := mockLLMRequestsFor(followUpMarker)
			Expect(err).NotTo(HaveOccurred())
			Expect(requests).To(HaveLen(1))
			var history []string
			for _, message := range requests[0].Body.Messages {
				history = append(history, message.Role+": "+message.Text())
			}
			Expect(history).To(ContainElements(
				ContainSubstring("user: Say hello "+marker),
				Equal("assistant: Hello from the aimock chat session."),
			), "the follow-up request should replay the first turn")
		}

		By("deleting the session")
		req, err = http.NewRequest("DELETE", apiBaseURL+"/api/v1/sessions/e2e-test-session", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err = http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusNoContent))
	})
})

// chatSSEEvent is one server-sent event from POST /api/v1/chat.
type chatSSEEvent struct {
	Name string
	Data string
}

// readChatSSEEvents reads chat SSE events up to and including "done".
func readChatSSEEvents(body io.Reader) []chatSSEEvent {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	var events []chatSSEEvent
	var current chatSSEEvent
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			current.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			current.Data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "" && current.Name != "":
			events = append(events, current)
			if current.Name == "done" {
				return events
			}
			current = chatSSEEvent{}
		}
	}
	if current.Name != "" {
		events = append(events, current)
	}
	return events
}

func chatSSEEventNames(events []chatSSEEvent) []string {
	names := make([]string, 0, len(events))
	for _, event := range events {
		names = append(names, event.Name)
	}
	return names
}

// chatSSEMessageText joins the content of every "message" event.
func chatSSEMessageText(events []chatSSEEvent) string {
	var b strings.Builder
	for _, event := range events {
		if event.Name != "message" {
			continue
		}
		var payload struct {
			Content string `json:"content"`
		}
		if json.Unmarshal([]byte(event.Data), &payload) == nil {
			b.WriteString(payload.Content)
		}
	}
	return b.String()
}

type chatSSEUsage struct {
	LLMCalls  int `json:"llmCalls"`
	ToolCalls int `json:"toolCalls"`
}

// chatSSEDoneUsage decodes the turn usage carried by the "done" event.
func chatSSEDoneUsage(events []chatSSEEvent) chatSSEUsage {
	for _, event := range events {
		if event.Name != "done" {
			continue
		}
		var payload struct {
			Usage chatSSEUsage `json:"usage"`
		}
		ExpectWithOffset(1, json.Unmarshal([]byte(event.Data), &payload)).To(Succeed())
		return payload.Usage
	}
	Fail("chat stream ended without a done event")
	return chatSSEUsage{}
}
