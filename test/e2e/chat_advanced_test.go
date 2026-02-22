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

var _ = Describe("Chat Advanced Features", Ordered, func() {
	var (
		apiBaseURL     string
		portForwardCmd *exec.Cmd
		cancelPF       context.CancelFunc
		token          string
	)

	const providerName = "e2e-chat-adv-provider"
	const agentName = "e2e-chat-adv-agent"

	BeforeAll(func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		By("setting up port-forward to controller API")
		ctx, cancel := context.WithCancel(context.Background())
		cancelPF = cancel

		cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
			"-o", "jsonpath={.items[0].metadata.name}", "-n", namespace)
		podName, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(podName)).NotTo(BeEmpty())

		portForwardCmd = exec.CommandContext(ctx, "kubectl", "port-forward",
			strings.TrimSpace(podName), "18086:8080", "-n", namespace)
		err = portForwardCmd.Start()
		Expect(err).NotTo(HaveOccurred())

		apiBaseURL = "http://localhost:18086"

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

		By("creating a provider for chat advanced tests")
		createProviderCRD(providerName, "openai", "e2e-openai-secret", "api-key",
			e2eOpenAIBaseURL, e2eOpenAIModel)
	})

	AfterAll(func() {
		if cancelPF != nil {
			cancelPF()
		}
		if portForwardCmd != nil && portForwardCmd.Process != nil {
			_ = portForwardCmd.Wait()
		}
		cmd := exec.Command("kubectl", "delete", "provider", providerName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "agent", agentName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	// --- Test 1: JSON response mode ---

	It("should return a JSON response when Accept header is application/json", func() {
		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("sending a chat message with Accept: application/json")
		chatBody := fmt.Sprintf(`{"message":"What is 2+2? Reply with just the number.","provider":"%s","model":"%s"}`,
			providerName, model)

		req, err := http.NewRequest("POST", apiBaseURL+"/api/v1/chat",
			strings.NewReader(chatBody))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		client := &http.Client{Timeout: 2 * time.Minute}
		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		By("verifying Content-Type is application/json")
		contentType := resp.Header.Get("Content-Type")
		Expect(contentType).To(ContainSubstring("application/json"),
			"Response Content-Type should be application/json")

		By("verifying JSON response structure")
		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(body).NotTo(BeEmpty())

		var result map[string]interface{}
		err = json.Unmarshal(body, &result)
		Expect(err).NotTo(HaveOccurred(), "Response should be valid JSON")

		Expect(result).To(HaveKey("message"), "Response should have a 'message' field")
		Expect(result).To(HaveKey("sessionId"), "Response should have a 'sessionId' field")
	})

	// --- Test 2: Chat with agentRef ---

	It("should handle chat with agentRef", func() {
		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating an Agent CRD for agentRef test")
		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"providerRef": {
					"name": "%s"
				},
				"model": {
					"name": "%s"
				}
			}
		}`, agentName, namespace, providerName, model)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("sending a chat message with agentRef")
		chatBody := fmt.Sprintf(`{"message":"Say hello in one word.","agentRef":"%s"}`, agentName)

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

		By("reading SSE events from agentRef chat")
		scanner := bufio.NewScanner(resp.Body)
		var events []string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event:") {
				eventType := strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				events = append(events, eventType)
				if eventType == "done" {
					break
				}
			}
		}

		_, _ = fmt.Fprintf(GinkgoWriter, "AgentRef chat SSE events: %v\n", events)

		Expect(events).To(ContainElement("message"), "Should receive 'message' events in stream")
		Expect(events).To(ContainElement("done"), "Should receive 'done' event in stream")
	})

	// --- Test 3: Management tools - create_agent via chat ---

	It("should create an agent via create_agent management tool in chat", func() {
		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		createdAgentName := "e2e-chat-created-agent"

		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "agent", createdAgentName,
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		By("sending a chat message with create_agent tool")
		chatBody := fmt.Sprintf(`{
			"message": "Create an agent called '%s' with provider '%s'. Use the create_agent tool.",
			"provider": "%s",
			"model": "%s",
			"tools": ["create_agent"]
		}`, createdAgentName, providerName, providerName, model)

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

		By("reading SSE stream until done")
		scanner := bufio.NewScanner(resp.Body)
		var events []string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event:") {
				eventType := strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				events = append(events, eventType)
				if eventType == "done" {
					break
				}
			}
		}

		_, _ = fmt.Fprintf(GinkgoWriter, "create_agent chat SSE events: %v\n", events)
		Expect(events).To(ContainElement("done"), "Should receive 'done' event")

		By("verifying the agent CRD was created")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "agent", createdAgentName,
				"-n", namespace, "-o", "jsonpath={.metadata.name}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal(createdAgentName))
		}, 30*time.Second, time.Second).Should(Succeed())
	})
})
