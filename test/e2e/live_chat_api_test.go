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

	"github.com/sozercan/orka/test/utils"
)

var _ = Describe("Live Chat API", Ordered, func() {
	const (
		liveChatProviderName = "e2e-live-chat-provider"
		liveChatSecretName   = "e2e-live-chat-secret"
		liveChatExpectedText = "ORKA_LIVE_CHAT_OK"
		controllerPFPort     = 18087
	)

	var (
		apiBaseURL      string
		cancelPF        context.CancelFunc
		portForwardCmd  *exec.Cmd
		token           string
		liveGPTModel    string
		liveProxyModels proxyModelCatalog
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

		By("discovering a live GPT-family model from the proxy catalog")
		liveProxyModels, err = fetchProxyModelCatalogViaServiceProxy(
			liveCopilotProxyServiceNamespace(),
			liveCopilotProxyServiceName(),
			liveCopilotProxyServicePort(),
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(liveProxyModels.AllModelIDs).NotTo(BeEmpty(), "proxy should expose at least one model")
		liveGPTModel = firstPreferredProxyModel(liveProxyModels, []string{
			"gpt-5.4",
			"gpt-5.2",
			"gpt-5.4-mini",
		}, "gpt-")
		Expect(liveGPTModel).NotTo(BeEmpty(), "proxy should expose a GPT-family model")

		By("creating a dummy secret for the live provider")
		err = createK8sSecret(liveChatSecretName, namespace, map[string]string{
			"api-key": "dummy-live-chat-key",
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "secret", liveChatSecretName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		By("creating a Provider CRD backed by the live copilot proxy")
		createProviderCRD(liveChatProviderName, "openai", liveChatSecretName, "api-key", e2eLiveCopilotProxyBaseURL, liveGPTModel)
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "provider", liveChatProviderName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
	})

	AfterEach(func() {
		dumpDebugInfo()
		dumpLiveCopilotProxyDebugInfo(liveChatProviderName)
	})

	It("should return JSON chat output and expose the created session", func() {
		resp := postLiveChatJSON(apiBaseURL, token, liveChatProviderName, liveGPTModel, liveChatExpectedText)

		Expect(resp.SessionID).NotTo(BeEmpty(), "JSON mode should return a sessionId")
		Expect(resp.Message).To(Equal(liveChatExpectedText))

		session := fetchLiveChatSession(apiBaseURL, token, resp.SessionID)
		Expect(session.Transcript).To(ContainSubstring(liveChatExpectedText))
		Expect(session.MessageCount).To(BeNumerically(">=", 2))
	})

	It("should stream chat SSE, create a session, and persist the exact sentinel", func() {
		sessionID, content, events := postLiveChatSSE(apiBaseURL, token, liveChatProviderName, liveGPTModel, liveChatExpectedText)

		Expect(sessionID).NotTo(BeEmpty(), "SSE stream should include a sessionId")
		Expect(events).To(ContainElement("status"))
		Expect(events).To(ContainElement("done"))
		if trimmedContent := strings.TrimSpace(content); trimmedContent != "" {
			Expect(trimmedContent).To(Equal(liveChatExpectedText))
		}

		session := fetchLiveChatSession(apiBaseURL, token, sessionID)
		Expect(session.Transcript).To(ContainSubstring(liveChatExpectedText))
		Expect(session.MessageCount).To(BeNumerically(">=", 2))
	})
})

type liveChatJSONResponse struct {
	SessionID string `json:"sessionId"`
	Message   string `json:"message"`
}

type liveChatSessionResponse struct {
	Transcript   string `json:"transcript"`
	MessageCount int    `json:"messageCount"`
}

func postLiveChatSSE(apiBaseURL, token, providerName, model, expectedText string) (string, string, []string) {
	body := fmt.Sprintf(`{
		"message": "Reply with exactly %s and nothing else.",
		"provider": "%s",
		"model": "%s",
		"temperature": 0,
		"maxTokens": 16
	}`, expectedText, providerName, model)

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(apiBaseURL, "/")+"/api/v1/chat", strings.NewReader(body))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 3 * time.Minute}
	resp, err := client.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()

	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("text/event-stream"))

	sessionID, content, events, err := parseLiveChatSSE(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	return sessionID, strings.TrimSpace(content), events
}

func postLiveChatJSON(apiBaseURL, token, providerName, model, expectedText string) liveChatJSONResponse {
	body := fmt.Sprintf(`{
		"message": "Reply with exactly %s and nothing else.",
		"provider": "%s",
		"model": "%s",
		"temperature": 0,
		"maxTokens": 16
	}`, expectedText, providerName, model)

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(apiBaseURL, "/")+"/api/v1/chat", strings.NewReader(body))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 3 * time.Minute}
	resp, err := client.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()

	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("application/json"))

	payload, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())

	var chatResp liveChatJSONResponse
	Expect(json.Unmarshal(payload, &chatResp)).To(Succeed())
	return chatResp
}

func parseLiveChatSSE(body io.Reader) (string, string, []string, error) {
	scanner := bufio.NewScanner(body)
	var (
		currentEvent string
		currentData  string
		sessionID    string
		messageText  strings.Builder
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
				return "", "", events, err
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
		return "", "", events, err
	}
	if err := flush(); err != nil {
		return "", "", events, err
	}
	if !hasDone {
		return "", "", events, fmt.Errorf("SSE stream did not emit a done event")
	}
	if sessionID == "" {
		return "", "", events, fmt.Errorf("SSE stream did not include a sessionId")
	}

	return sessionID, messageText.String(), events, nil
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
