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

	"github.com/sozercan/orka/test/utils"
)

var _ = Describe("Live Anthropic Compat API", Ordered, func() {
	const (
		liveAnthropicProviderName = "e2e-live-anthropic-compat-provider"
		liveAnthropicSecretName   = "e2e-live-anthropic-compat-secret"
		liveAnthropicExpectedText = "ORKA_LIVE_ANTHROPIC_OK"
		controllerPFPort          = 18090
	)

	var (
		apiBaseURL      string
		cancelPF        context.CancelFunc
		portForwardCmd  *exec.Cmd
		token           string
		liveClaudeModel string
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

		By("discovering a live Claude-family model from the proxy catalog")
		liveClaudeModel = discoverPreferredProxyModelViaServiceProxy(
			liveCopilotProxyServiceNamespace(),
			liveCopilotProxyServiceName(),
			liveCopilotProxyServicePort(),
			liveCopilotProxyClaudeModelPreferences,
			liveCopilotProxyClaudeModelPrefixes...,
		)
		Expect(liveClaudeModel).NotTo(BeEmpty(), "proxy should expose an allowed Claude-family model")

		By("creating a dummy secret for the live provider")
		err = createK8sSecret(liveAnthropicSecretName, namespace, map[string]string{
			"api-key": "dummy-live-anthropic-key",
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "secret", liveAnthropicSecretName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		By("creating a Provider CRD backed by the live copilot proxy")
		createProviderCRD(liveAnthropicProviderName, "openai", liveAnthropicSecretName, "api-key", e2eLiveCopilotProxyBaseURL, liveClaudeModel)
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "provider", liveAnthropicProviderName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
	})

	AfterEach(func() {
		dumpDebugInfo()
		dumpLiveCopilotProxyDebugInfo(liveAnthropicProviderName)
	})

	It("should list models via GET /anthropic/v1/models", func() {
		models := fetchLiveAnthropicModels(apiBaseURL, token)
		Expect(models.Object).To(Equal("list"))
		Expect(models.Data).NotTo(BeEmpty())
		Expect(models.IDs()).To(ContainElements(
			liveAnthropicProviderName+"/"+liveClaudeModel,
			liveClaudeModel,
		))
	})

	It("should return a non-streaming anthropic message response", func() {
		resp := postLiveAnthropicJSON(apiBaseURL, token, liveAnthropicProviderName, liveClaudeModel, liveAnthropicExpectedText)

		Expect(resp.Type).To(Equal("message"))
		Expect(resp.Role).To(Equal("assistant"))
		Expect(resp.Model).To(Equal(liveClaudeModel))
		Expect(resp.StopReason).NotTo(BeNil())
		Expect(*resp.StopReason).To(Equal("end_turn"))
		Expect(resp.Usage.InputTokens).To(BeNumerically(">", 0))
		Expect(resp.Usage.OutputTokens).To(BeNumerically(">", 0))
		Expect(flattenAnthropicText(resp.Content)).To(ContainSubstring(liveAnthropicExpectedText))
	})

	It("should stream anthropic messages via SSE", func() {
		stream := postLiveAnthropicSSE(apiBaseURL, token, liveAnthropicProviderName, liveClaudeModel, liveAnthropicExpectedText)

		Expect(stream.Events).To(ContainElements(
			"message_start",
			"content_block_start",
			"content_block_delta",
			"content_block_stop",
			"message_delta",
			"message_stop",
		))
		Expect(stream.Model).To(Equal(liveClaudeModel))
		Expect(stream.StopReason).To(Equal("end_turn"))
		Expect(stream.OutputTokens).To(BeNumerically(">", 0))
		Expect(strings.TrimSpace(stream.Text)).To(ContainSubstring(liveAnthropicExpectedText))
	})
})

type liveAnthropicModelList struct {
	Object string                  `json:"object"`
	Data   []liveAnthropicListItem `json:"data"`
}

type liveAnthropicListItem struct {
	ID string `json:"id"`
}

func (m liveAnthropicModelList) IDs() []string {
	ids := make([]string, 0, len(m.Data))
	for _, item := range m.Data {
		ids = append(ids, item.ID)
	}
	return ids
}

type liveAnthropicResponse struct {
	ID         string                      `json:"id"`
	Type       string                      `json:"type"`
	Role       string                      `json:"role"`
	Content    []liveAnthropicContentBlock `json:"content"`
	Model      string                      `json:"model"`
	StopReason *string                     `json:"stop_reason"`
	Usage      liveAnthropicUsage          `json:"usage"`
}

type liveAnthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type liveAnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type liveAnthropicStream struct {
	Text         string
	Model        string
	StopReason   string
	OutputTokens int
	Events       []string
}

type liveAnthropicStreamEvent struct {
	Type         string                     `json:"type"`
	Message      *liveAnthropicResponse     `json:"message,omitempty"`
	ContentBlock *liveAnthropicContentBlock `json:"content_block,omitempty"`
	Delta        *liveAnthropicStreamDelta  `json:"delta,omitempty"`
	Usage        *liveAnthropicUsage        `json:"usage,omitempty"`
}

type liveAnthropicStreamDelta struct {
	Type       string  `json:"type,omitempty"`
	Text       string  `json:"text,omitempty"`
	StopReason *string `json:"stop_reason,omitempty"`
}

func fetchLiveAnthropicModels(apiBaseURL, token string) liveAnthropicModelList {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(apiBaseURL, "/")+"/anthropic/v1/models", nil)
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("x-api-key", token)

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()

	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("application/json"))

	payload, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())

	var models liveAnthropicModelList
	Expect(json.Unmarshal(payload, &models)).To(Succeed())
	return models
}

func postLiveAnthropicJSON(apiBaseURL, token, providerName, model, expectedText string) liveAnthropicResponse {
	body := fmt.Sprintf(`{
		"model": "%s/%s",
		"max_tokens": 32,
		"messages": [{
			"role": "user",
			"content": "Reply with exactly %s and nothing else. Do not use any tools."
		}]
	}`, providerName, model, expectedText)

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(apiBaseURL, "/")+"/anthropic/v1/messages", strings.NewReader(body))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("x-api-key", token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 3 * time.Minute}
	resp, err := client.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()

	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("application/json"))

	payload, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())

	var messageResp liveAnthropicResponse
	Expect(json.Unmarshal(payload, &messageResp)).To(Succeed())
	return messageResp
}

func postLiveAnthropicSSE(apiBaseURL, token, providerName, model, expectedText string) liveAnthropicStream {
	body := fmt.Sprintf(`{
		"model": "%s/%s",
		"max_tokens": 32,
		"stream": true,
		"messages": [{
			"role": "user",
			"content": "Reply with exactly %s and nothing else. Do not use any tools."
		}]
	}`, providerName, model, expectedText)

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(apiBaseURL, "/")+"/anthropic/v1/messages", strings.NewReader(body))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("x-api-key", token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 3 * time.Minute}
	resp, err := client.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()

	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("text/event-stream"))

	stream, err := parseLiveAnthropicSSE(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	return stream
}

func parseLiveAnthropicSSE(body io.Reader) (liveAnthropicStream, error) {
	scanner := bufio.NewScanner(body)
	var (
		currentEvent string
		currentData  string
		stream       liveAnthropicStream
		textBuilder  strings.Builder
	)

	flush := func() error {
		if currentEvent == "" {
			return nil
		}

		stream.Events = append(stream.Events, currentEvent)

		var payload liveAnthropicStreamEvent
		if err := json.Unmarshal([]byte(currentData), &payload); err != nil {
			return fmt.Errorf("failed to parse anthropic SSE %s event: %w", currentEvent, err)
		}

		switch currentEvent {
		case "message_start":
			if payload.Message != nil {
				stream.Model = payload.Message.Model
			}
		case "content_block_delta":
			if payload.Delta != nil {
				textBuilder.WriteString(payload.Delta.Text)
			}
		case "message_delta":
			if payload.Delta != nil && payload.Delta.StopReason != nil {
				stream.StopReason = *payload.Delta.StopReason
			}
			if payload.Usage != nil {
				stream.OutputTokens = payload.Usage.OutputTokens
			}
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
				return liveAnthropicStream{}, err
			}
		case strings.HasPrefix(line, "event:"):
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			currentData += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if err := scanner.Err(); err != nil {
		return liveAnthropicStream{}, err
	}
	if err := flush(); err != nil {
		return liveAnthropicStream{}, err
	}

	stream.Text = strings.TrimSpace(textBuilder.String())
	return stream, nil
}

func flattenAnthropicText(blocks []liveAnthropicContentBlock) string {
	var textParts []string
	for _, block := range blocks {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			textParts = append(textParts, block.Text)
		}
	}
	return strings.TrimSpace(strings.Join(textParts, ""))
}
