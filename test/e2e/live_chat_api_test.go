//go:build e2e
// +build e2e

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

var _ = Describe("Live Chat API", Ordered, func() {
	const (
		liveChatProviderName = "e2e-live-chat-provider"
		liveChatSecretName   = "e2e-live-chat-secret"
		liveChatExpectedText = "ORKA_LIVE_CHAT_OK"
		controllerPFPort     = 18087
	)

	var (
		apiBaseURL          string
		cancelPF            context.CancelFunc
		portForwardCmd      *exec.Cmd
		proxyBaseURL        string
		cancelProxyPF       context.CancelFunc
		proxyPortForwardCmd *exec.Cmd
		token               string
		liveChatModel       string
		liveProxyModels     proxyModelCatalog
	)

	BeforeAll(func() {
		if strings.TrimSpace(e2eLiveCopilotProxyBaseURL) == "" {
			Skip("Skipping: E2E_LIVE_COPILOT_PROXY_BASE_URL not set")
		}

		var err error

		By("setting up port-forward to controller API")
		apiBaseURL, cancelPF, portForwardCmd, err = startControllerAPIPortForward(controllerPFPort)
		Expect(err).NotTo(HaveOccurred())

		DeferCleanup(func() {
			stopPortForward(cancelPF, portForwardCmd)
		})

		By("getting a service account token")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())

		By("verifying the live proxy is ready")
		ready := waitForProxyReadyViaServiceProxy(
			liveCopilotProxyServiceNamespace(),
			liveCopilotProxyServiceName(),
			liveCopilotProxyServicePort(),
		)
		Expect(ready.Status).To(Equal("ready"))
		Expect(ready.Error).To(BeEmpty())

		By("setting up port-forward to the live copilot proxy")
		proxyBaseURL, cancelProxyPF, proxyPortForwardCmd, err = startServicePortForward(
			liveCopilotProxyServiceNamespace(),
			liveCopilotProxyServiceName(),
			18188,
			liveCopilotProxyServicePort(),
		)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			stopPortForward(cancelProxyPF, proxyPortForwardCmd)
		})

		By("discovering a live chat-completions model from the proxy catalog")
		liveProxyModels, err = fetchProxyModelCatalogViaServiceProxy(
			liveCopilotProxyServiceNamespace(),
			liveCopilotProxyServiceName(),
			liveCopilotProxyServicePort(),
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(liveProxyModels.AllModelIDs).NotTo(BeEmpty(), "proxy should expose at least one model")
		var skipReason string
		liveChatModel, skipReason, err = firstLiveCopilotProxyChatCompletionModel(
			proxyBaseURL,
			e2eGitHubToken,
			liveProxyModels,
			liveCopilotProxyChatModelPreferences(),
			"gpt-",
			"claude-",
		)
		Expect(err).NotTo(HaveOccurred())
		if liveChatModel == "" {
			Skip("Skipping: " + skipReason)
		}

		By("creating a dummy secret for the live provider")
		err = createK8sSecret(liveChatSecretName, namespace, map[string]string{
			"api-key": liveProxyProbeAPIKey,
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "secret", liveChatSecretName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		By("creating a Provider CRD backed by the live copilot proxy")
		createProviderCRD(liveChatProviderName, "openai", liveChatSecretName, "api-key", e2eLiveCopilotProxyBaseURL, liveChatModel)
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "provider", liveChatProviderName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
	})

	AfterEach(func() {
		dumpDebugInfo()
		dumpLiveCopilotProxyDebugInfo(liveChatProviderName)
	})

	It("should stream chat SSE and create a live session", func() {
		sessionID, content, usage, events, err := postLiveChatSSE(apiBaseURL, token, liveChatProviderName, liveChatModel, liveChatExpectedText)
		if isLiveCopilotProxyForbiddenError(err) {
			Skip("Skipping: live Copilot proxy chat completions returned 403 for model " + liveChatModel)
		}
		Expect(err).NotTo(HaveOccurred())

		Expect(sessionID).NotTo(BeEmpty(), "SSE stream should include a sessionId")
		Expect(events).To(ContainElement("status"))
		Expect(events).To(ContainElement("done"))
		Expect(usage.LLMCalls).To(BeNumerically(">=", 1), "SSE mode should record at least one LLM call")
		if trimmedContent := strings.TrimSpace(content); trimmedContent != "" {
			Expect(trimmedContent).To(Equal(liveChatExpectedText))
		}

		session := fetchLiveChatSession(apiBaseURL, token, sessionID)
		Expect(session.MessageCount).To(BeNumerically(">=", 1))
		if assistant := lastAssistantContent(session.Transcript); assistant != "" {
			Expect(assistant).To(Equal(liveChatExpectedText))
		}
	})

	It("should return JSON chat metadata and expose the created session", func() {
		resp, err := postLiveChatJSON(apiBaseURL, token, liveChatProviderName, liveChatModel, liveChatExpectedText)
		if isLiveCopilotProxyForbiddenError(err) {
			Skip("Skipping: live Copilot proxy chat completions returned 403 for model " + liveChatModel)
		}
		Expect(err).NotTo(HaveOccurred())

		Expect(resp.SessionID).NotTo(BeEmpty(), "JSON mode should return a sessionId")
		Expect(resp.Usage.LLMCalls).To(BeNumerically(">=", 1), "JSON mode should record at least one LLM call")

		session := fetchLiveChatSession(apiBaseURL, token, resp.SessionID)
		Expect(session.MessageCount).To(BeNumerically(">=", 1))
		if trimmedMessage := strings.TrimSpace(resp.Message); trimmedMessage != "" {
			Expect(trimmedMessage).To(Equal(liveChatExpectedText))
		}
	})
})

type liveChatJSONResponse struct {
	SessionID string `json:"sessionId"`
	Message   string `json:"message"`
	Usage     struct {
		LLMCalls int `json:"llmCalls"`
	} `json:"usage"`
}

type liveChatSessionResponse struct {
	Transcript   string `json:"transcript"`
	MessageCount int    `json:"messageCount"`
}

type liveChatSessionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type liveChatUsage struct {
	LLMCalls int `json:"llmCalls"`
}

func postLiveChatSSE(apiBaseURL, token, providerName, model, expectedText string) (string, string, liveChatUsage, []string, error) {
	body := fmt.Sprintf(`{
		"message": "Reply with exactly %s and nothing else.",
		"provider": "%s",
		"model": "%s",
		"temperature": 0,
		"maxTokens": 16
	}`, expectedText, providerName, model)

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(apiBaseURL, "/")+"/api/v1/chat", strings.NewReader(body))
	if err != nil {
		return "", "", liveChatUsage{}, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 3 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", liveChatUsage{}, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		payload, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return "", "", liveChatUsage{}, nil, readErr
		}
		return "", "", liveChatUsage{}, nil, fmt.Errorf("live chat SSE returned %s: %s", resp.Status, strings.TrimSpace(string(payload)))
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		return "", "", liveChatUsage{}, nil, fmt.Errorf("live chat SSE returned content-type %q", contentType)
	}

	sessionID, content, usage, events, err := parseLiveChatSSE(resp.Body)
	return sessionID, strings.TrimSpace(content), usage, events, err
}

func postLiveChatJSON(apiBaseURL, token, providerName, model, expectedText string) (liveChatJSONResponse, error) {
	body := fmt.Sprintf(`{
		"message": "Reply with exactly %s and nothing else.",
		"provider": "%s",
		"model": "%s",
		"temperature": 0,
		"maxTokens": 16
	}`, expectedText, providerName, model)

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(apiBaseURL, "/")+"/api/v1/chat", strings.NewReader(body))
	if err != nil {
		return liveChatJSONResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 3 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return liveChatJSONResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		payload, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return liveChatJSONResponse{}, readErr
		}
		return liveChatJSONResponse{}, fmt.Errorf("live chat JSON returned %s: %s", resp.Status, strings.TrimSpace(string(payload)))
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		return liveChatJSONResponse{}, fmt.Errorf("live chat JSON returned content-type %q", contentType)
	}

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return liveChatJSONResponse{}, err
	}

	var chatResp liveChatJSONResponse
	if err := json.Unmarshal(payload, &chatResp); err != nil {
		return liveChatJSONResponse{}, err
	}
	return chatResp, nil
}

func parseLiveChatSSE(body io.Reader) (string, string, liveChatUsage, []string, error) {
	scanner := bufio.NewScanner(body)
	var (
		currentEvent string
		currentData  string
		sessionID    string
		messageText  strings.Builder
		usage        liveChatUsage
		events       []string
		hasDone      bool
	)

	flush := func() error {
		if currentEvent == "" {
			return nil
		}

		events = append(events, currentEvent)

		switch currentEvent {
		case "status":
			var payload struct {
				SessionID string `json:"sessionId"`
			}
			if err := json.Unmarshal([]byte(currentData), &payload); err != nil {
				return fmt.Errorf("failed to parse SSE status event: %w", err)
			}
			if payload.SessionID != "" {
				sessionID = payload.SessionID
			}
		case "message":
			var payload struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal([]byte(currentData), &payload); err != nil {
				return fmt.Errorf("failed to parse SSE message event: %w", err)
			}
			messageText.WriteString(payload.Content)
		case "done":
			var payload struct {
				Usage liveChatUsage `json:"usage"`
			}
			if err := json.Unmarshal([]byte(currentData), &payload); err != nil {
				return fmt.Errorf("failed to parse SSE done event: %w", err)
			}
			usage = payload.Usage
			hasDone = true
		case "error":
			return fmt.Errorf("received SSE error event: %s", strings.TrimSpace(currentData))
		}

		currentEvent = ""
		currentData = ""
		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case line == "":
			if err := flush(); err != nil {
				return "", "", liveChatUsage{}, events, err
			}
		case strings.HasPrefix(line, "event:"):
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if currentData == "" {
				currentData = data
			} else {
				currentData += "\n" + data
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return "", "", liveChatUsage{}, events, err
	}
	if err := flush(); err != nil {
		return "", "", liveChatUsage{}, events, err
	}
	if !hasDone {
		return "", "", liveChatUsage{}, events, fmt.Errorf("SSE stream did not emit a done event")
	}
	if sessionID == "" {
		return "", "", liveChatUsage{}, events, fmt.Errorf("SSE stream did not include a sessionId")
	}

	return sessionID, messageText.String(), usage, events, nil
}

func fetchLiveChatSession(apiBaseURL, token, sessionID string) liveChatSessionResponse {
	var session liveChatSessionResponse

	Eventually(func(g Gomega) {
		req, err := http.NewRequest(http.MethodGet, strings.TrimRight(apiBaseURL, "/")+"/api/v1/sessions/"+sessionID, nil)
		g.Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		g.Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		g.Expect(resp.StatusCode).To(Equal(http.StatusOK))

		payload, err := io.ReadAll(resp.Body)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(json.Unmarshal(payload, &session)).To(Succeed())
		g.Expect(session.Transcript).NotTo(BeEmpty())
	}, 2*time.Minute, 2*time.Second).Should(Succeed())

	return session
}

func lastAssistantContent(transcript string) string {
	lines := strings.Split(strings.TrimSpace(transcript), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}

		var message liveChatSessionMessage
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			continue
		}
		if message.Role == "assistant" {
			return strings.TrimSpace(message.Content)
		}
	}

	return ""
}
