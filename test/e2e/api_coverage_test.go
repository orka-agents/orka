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

var _ = Describe("API Coverage", Ordered, func() {
	var (
		apiBaseURL     string
		token          string
		cancelPF       context.CancelFunc
		portForwardCmd *exec.Cmd
	)

	const (
		providerName = "e2e-api-cov-provider"
		sessionName  = "e2e-api-cov-session"
		agentName    = "e2e-api-cov-agent"
	)

	BeforeAll(func() {
		By("setting up port-forward to controller API")
		var err error
		apiBaseURL, cancelPF, portForwardCmd, err = startControllerAPIPortForward(18085)
		Expect(err).NotTo(HaveOccurred())

		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		stopPortForward(cancelPF, portForwardCmd)

		// Clean up all resources with e2e-api-cov- prefix
		for _, res := range []struct{ kind, name string }{
			{"task", "e2e-api-cov-session-task"},
			{"task", "e2e-api-cov-container-task"},
			{"task", "e2e-api-cov-chat-task"},
			{"agent", agentName},
			{"provider", providerName},
		} {
			cmd := exec.Command("kubectl", "delete", res.kind, res.name,
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
		// Clean up the chat session
		cmd := exec.Command("kubectl", "delete", "session", sessionName,
			"-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "session", "e2e-api-cov-chat-session",
			"-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		dumpDebugInfo("e2e-api-cov-session-task", "e2e-api-cov-container-task")
	})

	// Test 1: GET /api/v1/sessions/:id (session transcript)
	It("should retrieve session transcript via GET /api/v1/sessions/:id", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating a provider for the AI task")
		createProviderCRD(providerName, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, model)

		By("creating an AI task with sessionRef")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "e2e-api-cov-session-task",
				"namespace": "%s"
			},
			"spec": {
				"type": "ai",
				"sessionRef": {
					"name": "%s",
					"create": true
				},
				"ai": {
					"prompt": "What is 3+3? Reply with just the number.",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, namespace, sessionName, model, providerName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the task to complete")
		phase := waitForTaskCompletion("e2e-api-cov-session-task", 3*time.Minute)
		Expect(phase).To(Equal("Succeeded"))

		By("fetching session transcript via API")
		Eventually(func(g Gomega) {
			req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/sessions/"+sessionName, nil)
			g.Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+token)

			client := &http.Client{Timeout: 30 * time.Second}
			resp, err := client.Do(req)
			g.Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			g.Expect(resp.StatusCode).To(Equal(http.StatusOK))

			body, err := io.ReadAll(resp.Body)
			g.Expect(err).NotTo(HaveOccurred())
			var sessionResp map[string]interface{}
			err = json.Unmarshal(body, &sessionResp)
			g.Expect(err).NotTo(HaveOccurred(), "Session response should be valid JSON")
			g.Expect(sessionResp).To(HaveKey("transcript"))
			g.Expect(sessionResp).To(HaveKey("messageCount"))
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	// Test 2: PUT /api/v1/agents/:name (update agent)
	It("should update an agent via PUT /api/v1/agents/:name", func() {
		By("creating an agent via kubectl")
		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"systemPrompt": {
					"inline": "Original prompt"
				},
				"model": {
					"name": "gpt-4o-mini"
				}
			}
		}`, agentName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the agent to be visible via API")
		Eventually(func(g Gomega) {
			req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/agents/"+agentName, nil)
			g.Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+token)

			resp, err := http.DefaultClient.Do(req)
			g.Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(resp.StatusCode).To(Equal(http.StatusOK), "GET /api/v1/agents/:name response body: %s", string(body))
		}, 30*time.Second, time.Second).Should(Succeed())

		By("updating the agent via PUT /api/v1/agents/" + agentName)
		updateBytes, err := json.Marshal(map[string]any{
			"spec": map[string]any{
				"systemPrompt": map[string]any{
					"inline": "Updated prompt",
				},
				"model": map[string]any{
					"name": "gpt-4o-mini",
				},
			},
		})
		Expect(err).NotTo(HaveOccurred())

		body, statusCode, err := doAuthorizedJSONRequest(
			http.MethodPut,
			apiBaseURL+"/api/v1/agents/"+agentName,
			token,
			string(updateBytes),
			"",
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(statusCode).To(BeElementOf(http.StatusOK, http.StatusNoContent),
			"PUT /api/v1/agents/:name response body: %s", body)

		By("verifying the agent was updated")
		Eventually(func(g Gomega) {
			req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/agents/"+agentName, nil)
			g.Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+token)

			resp, err := http.DefaultClient.Do(req)
			g.Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			g.Expect(resp.StatusCode).To(Equal(http.StatusOK))

			body, err := io.ReadAll(resp.Body)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(body)).To(ContainSubstring("Updated prompt"),
				"Agent should have updated systemPrompt")
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	// Test 3: GET /api/v1/tools/:name (single tool)
	It("should retrieve a single tool via GET /api/v1/tools/:name", func() {
		By("fetching the web_search tool")
		req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/tools/web_search", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		bodyStr := string(body)
		Expect(bodyStr).To(ContainSubstring("name"), "Response should have name field")
		Expect(bodyStr).To(ContainSubstring("description"), "Response should have description field")
		Expect(bodyStr).To(ContainSubstring("builtin"), "Builtin tool response should identify builtin tools")
	})

	// Test 4: GET /api/v1/auth/validate
	It("should validate auth tokens via GET /api/v1/auth/validate", func() {
		By("calling with a valid token")
		req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/auth/validate", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK), "Valid token should return 200")

		By("calling with an invalid token")
		req, err = http.NewRequest("GET", apiBaseURL+"/api/v1/auth/validate", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer invalid-token-value")

		resp, err = client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized), "Invalid token should return 401")

		By("calling with no token")
		req, err = http.NewRequest("GET", apiBaseURL+"/api/v1/auth/validate", nil)
		Expect(err).NotTo(HaveOccurred())

		resp, err = client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized), "Empty token should return 401")
	})

	// Test 5: GET /api/v1/secrets
	It("should list secrets without exposing values via GET /api/v1/secrets", func() {
		By("fetching secrets list")
		req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/secrets", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		bodyStr := string(body)
		Expect(bodyStr).NotTo(BeEmpty(), "Secrets response should not be empty")

		// Parse as paginated response map to verify structure
		var response map[string]interface{}
		err = json.Unmarshal(body, &response)
		Expect(err).NotTo(HaveOccurred(), "Response should be valid JSON")

		items, ok := response["items"].([]interface{})
		Expect(ok).To(BeTrue(), "Response should have an items array")

		// Verify no secret values are exposed
		for _, item := range items {
			secret, ok := item.(map[string]interface{})
			Expect(ok).To(BeTrue(), "Each item should be a JSON object")
			Expect(secret).NotTo(HaveKey("data"),
				"Secret entries should not expose data field with actual values")
		}
	})

	// Test 6: DELETE /api/v1/chat/:sessionId
	It("should delete a chat session via DELETE /api/v1/chat/:sessionId", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		// Ensure provider exists (may already exist from test 1)
		createProviderCRD(providerName, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, model)

		chatSessionID := "e2e-api-cov-chat-session"

		By("creating a chat session via POST /api/v1/chat")
		chatBody := fmt.Sprintf(`{"message":"Say hello briefly","provider":"%s","model":"%s","sessionId":"%s"}`,
			providerName, model, chatSessionID)

		req, err := http.NewRequest("POST", apiBaseURL+"/api/v1/chat",
			strings.NewReader(chatBody))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 2 * time.Minute}
		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		// Drain the response body (may be SSE stream)
		_, _ = io.ReadAll(resp.Body)

		By("deleting the chat session")
		Eventually(func(g Gomega) {
			req, err := http.NewRequest("DELETE", apiBaseURL+"/api/v1/chat/"+chatSessionID, nil)
			g.Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+token)

			resp, err := client.Do(req)
			g.Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			g.Expect(resp.StatusCode).To(BeElementOf(http.StatusOK, http.StatusNoContent),
				"DELETE /api/v1/chat/:sessionId should return 200 or 204")
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	// Test 7: GET /api/v1/tasks/:id/plan (404 for non-autonomous)
	It("should return 404 for plan on non-autonomous task", func() {
		const containerTaskName = "e2e-api-cov-container-task"

		By("creating a container task")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "container",
				"image": "busybox:latest",
				"command": ["sleep"],
				"args": ["30"]
			}
		}`, containerTaskName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the task to start running")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", containerTaskName,
				"-o", "jsonpath={.status.phase}", "-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(BeElementOf("Running", "Succeeded"),
				"Task should be running or succeeded")
		}, 2*time.Minute, time.Second).Should(Succeed())

		By("requesting plan for non-autonomous container task")
		req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/tasks/"+containerTaskName+"/plan", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusNotFound),
			"GET /api/v1/tasks/:id/plan should return 404 for non-autonomous tasks")
	})
})
