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

	"github.com/sozercan/orka/test/utils"
)

var _ = Describe("API Extended Coverage", Ordered, func() {
	var (
		apiBaseURL     string
		token          string
		cancelPF       context.CancelFunc
		portForwardCmd *exec.Cmd
	)

	const (
		providerName = "e2e-api-ext-provider"
		taskName     = "e2e-api-ext-task"
	)

	BeforeAll(func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		By("setting up port-forward to controller API")
		var err error
		apiBaseURL, cancelPF, portForwardCmd, err = startControllerAPIPortForward(18082)
		Expect(err).NotTo(HaveOccurred())

		By("getting a service account token")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		stopPortForward(cancelPF, portForwardCmd)
		cmd := exec.Command("kubectl", "delete", "task", taskName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "provider", providerName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		dumpDebugInfo(taskName)
	})

	It("should retrieve task result via GET /api/v1/tasks/{id}/result", func() {
		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating a provider and running a task to completion")
		createProviderCRD(providerName, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, model)

		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "ai",
				"ai": {
					"prompt": "What is 7+7? Reply with just the number.",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, taskName, namespace, model, providerName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for task completion")
		phase := waitForTaskCompletion(taskName, 3*time.Minute)
		Expect(phase).To(Equal("Succeeded"))

		By("fetching the result via API")
		req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/tasks/"+taskName+"/result", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK), "GET /api/v1/tasks/{id}/result should return 200")

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		bodyStr := string(body)
		Expect(bodyStr).NotTo(BeEmpty(), "Result should not be empty")
		Expect(bodyStr).To(ContainSubstring("result"), "Response should contain result field")
	})

	It("should list tools via GET /api/v1/tools", func() {
		By("fetching built-in tools")
		req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/tools", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		bodyStr := string(body)

		// Should include built-in tools
		Expect(bodyStr).To(ContainSubstring("web_search"), "Should list web_search tool")
		Expect(bodyStr).To(ContainSubstring("code_exec"), "Should list code_exec tool")
		Expect(bodyStr).To(ContainSubstring("file_read"), "Should list file_read tool")
	})

	It("should list tasks with pagination via GET /api/v1/tasks", func() {
		By("listing tasks with limit parameter")
		req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/tasks?limit=5", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())

		var result map[string]interface{}
		err = json.Unmarshal(body, &result)
		Expect(err).NotTo(HaveOccurred())

		// Should have items array
		items, ok := result["items"]
		Expect(ok).To(BeTrue(), "Response should have items field")
		Expect(items).NotTo(BeNil())
	})

	It("should get task children via GET /api/v1/tasks/{id}/children for scheduled tasks", func() {
		By("creating a scheduled task with a short interval")
		scheduledTaskName := "e2e-api-sched-parent"
		scheduledManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "container",
				"schedule": "*/1 * * * *",
				"image": "busybox:1.36",
				"command": ["echo", "scheduled"]
			}
		}`, scheduledTaskName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(scheduledManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "task", scheduledTaskName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		By("confirming task is in Scheduled phase")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", scheduledTaskName,
				"-o", "jsonpath={.status.phase}",
				"-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("Scheduled"))
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("waiting for at least one child task")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "tasks",
				"-l", fmt.Sprintf("orka.ai/parent-task=%s", scheduledTaskName),
				"-o", "jsonpath={.items[*].metadata.name}",
				"-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty(), "Should have at least one child task")
		}, 4*time.Minute, 5*time.Second).Should(Succeed())

		By("querying children via the API")
		req, err := http.NewRequest("GET",
			apiBaseURL+"/api/v1/tasks/"+scheduledTaskName+"/children", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())

		var result map[string]interface{}
		err = json.Unmarshal(body, &result)
		Expect(err).NotTo(HaveOccurred())

		items, ok := result["items"].([]interface{})
		Expect(ok).To(BeTrue(), "Response should have items array")
		Expect(len(items)).To(BeNumerically(">=", 1), "Should have at least 1 child task")
	})

	It("should manage agents via API CRUD", func() {
		apiAgentName := "e2e-api-crud-agent"

		By("creating an agent via API")
		agentBody := fmt.Sprintf(`{
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
		}`, apiAgentName, namespace, providerName, e2eOpenAIModel)

		req, err := http.NewRequest("POST", apiBaseURL+"/api/v1/agents",
			strings.NewReader(agentBody))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(BeElementOf(http.StatusOK, http.StatusCreated),
			"POST /api/v1/agents should return 200 or 201")

		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "agent", apiAgentName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		By("listing agents via API")
		req, err = http.NewRequest("GET", apiBaseURL+"/api/v1/agents", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err = http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(ContainSubstring(apiAgentName))

		By("getting a single agent via API")
		req, err = http.NewRequest("GET", apiBaseURL+"/api/v1/agents/"+apiAgentName, nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err = http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		By("deleting the agent via API")
		req, err = http.NewRequest("DELETE", apiBaseURL+"/api/v1/agents/"+apiAgentName, nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err = http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(BeElementOf(http.StatusOK, http.StatusNoContent),
			"DELETE /api/v1/agents/{id} should return 200 or 204")

		By("verifying the agent is gone")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "agent", apiAgentName, "-n", namespace, "--ignore-not-found")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(BeEmpty(), "Agent should be deleted")
		}, 30*time.Second, time.Second).Should(Succeed())
	})
})
