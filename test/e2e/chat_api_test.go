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
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/sozercan/orka/test/utils"
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
		ctx, cancel := context.WithCancel(context.Background())
		cancelPF = cancel

		cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
			"-o", "jsonpath={.items[0].metadata.name}", "-n", namespace)
		podName, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(podName)).NotTo(BeEmpty())

		portForwardCmd = exec.CommandContext(ctx, "kubectl", "port-forward",
			strings.TrimSpace(podName), "18081:8080", "-n", namespace)
		err = portForwardCmd.Start()
		Expect(err).NotTo(HaveOccurred())

		apiBaseURL = "http://localhost:18081"

		Eventually(func(g Gomega) {
			resp, err := http.Get(apiBaseURL + "/healthz")
			g.Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
		}, 30*time.Second, time.Second).Should(Succeed())

		By("getting a service account token")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())
	})

	AfterAll(func() {
		if cancelPF != nil {
			cancelPF()
		}
		if portForwardCmd != nil && portForwardCmd.Process != nil {
			_ = portForwardCmd.Wait()
		}
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

		By("ensuring an OpenAI provider exists for chat")
		createProviderCRD("e2e-chat-provider", "openai", "e2e-openai-secret", "api-key",
			e2eOpenAIBaseURL, e2eOpenAIModel)
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "provider", "e2e-chat-provider",
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		By("sending a chat message with SSE accept header")
		chatBody := fmt.Sprintf(`{"message":"What is 2+2? Reply with just the number.","provider":"e2e-chat-provider","model":"%s"}`,
			e2eOpenAIModel)

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
		scanner := bufio.NewScanner(resp.Body)
		var events []string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event:") {
				events = append(events, strings.TrimSpace(strings.TrimPrefix(line, "event:")))
			}
		}

		By("verifying expected event types were received")
		// Should have at least a message and done event
		Expect(events).NotTo(BeEmpty(), "Should receive SSE events")
		Expect(events).To(ContainElement("done"), "Should receive a 'done' event")
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
			"messages": [{"role": "user", "content": "What is 1+1? Reply with just the number."}],
			"max_tokens": 50
		}`, model)

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

		By("ensuring an OpenAI provider exists")
		createProviderCRD("e2e-session-provider", "openai", "e2e-openai-secret", "api-key",
			e2eOpenAIBaseURL, e2eOpenAIModel)
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "provider", "e2e-session-provider",
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		By("sending a chat message to create a session")
		chatBody := fmt.Sprintf(`{"message":"Say hello","provider":"e2e-session-provider","model":"%s","sessionId":"e2e-test-session"}`,
			e2eOpenAIModel)

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
		Eventually(func(g Gomega) {
			req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/sessions/e2e-test-session", nil)
			g.Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+token)

			resp, err := http.DefaultClient.Do(req)
			g.Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
		}, 30*time.Second, time.Second).Should(Succeed())

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
