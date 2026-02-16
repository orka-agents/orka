//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
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

var _ = Describe("REST API Endpoints", Ordered, func() {
	var (
		apiBaseURL     string
		portForwardCmd *exec.Cmd
		cancelPF       context.CancelFunc
	)

	BeforeAll(func() {
		By("setting up port-forward to controller API")
		ctx, cancel := context.WithCancel(context.Background())
		cancelPF = cancel

		// Find the controller pod
		cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
			"-o", "jsonpath={.items[0].metadata.name}", "-n", namespace)
		podName, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to find controller pod")
		Expect(strings.TrimSpace(podName)).NotTo(BeEmpty())

		// Start port-forward
		portForwardCmd = exec.CommandContext(ctx, "kubectl", "port-forward",
			strings.TrimSpace(podName), "18080:8080", "-n", namespace)
		err = portForwardCmd.Start()
		Expect(err).NotTo(HaveOccurred(), "Failed to start port-forward")

		apiBaseURL = "http://localhost:18080"

		// Wait for port-forward to be ready
		Eventually(func(g Gomega) {
			resp, err := http.Get(apiBaseURL + "/healthz")
			g.Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	AfterAll(func() {
		By("stopping port-forward")
		if cancelPF != nil {
			cancelPF()
		}
		if portForwardCmd != nil && portForwardCmd.Process != nil {
			_ = portForwardCmd.Wait()
		}
	})

	It("should return healthy status from /healthz", func() {
		resp, err := http.Get(apiBaseURL + "/healthz")
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(ContainSubstring("ok"))
	})

	It("should return ready status from /readyz", func() {
		resp, err := http.Get(apiBaseURL + "/readyz")
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})

	It("should list tasks via GET /api/v1/tasks", func() {
		By("getting a service account token for auth")
		token, err := serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())

		req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/tasks", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).NotTo(BeEmpty())
	})

	It("should list agents via GET /api/v1/agents", func() {
		By("getting a service account token for auth")
		token, err := serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())

		req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/agents", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})

	It("should reject unauthenticated requests", func() {
		req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/tasks", nil)
		Expect(err).NotTo(HaveOccurred())
		// No Authorization header

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("should create a task via POST /api/v1/tasks and retrieve it", func() {
		By("getting a service account token for auth")
		token, err := serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())

		By("creating a task via the API")
		taskBody := `{"name":"e2e-api-crud-task","type":"container","image":"busybox:latest","command":["echo"],"args":["api-created"]}`
		req, err := http.NewRequest("POST", apiBaseURL+"/api/v1/tasks",
			strings.NewReader(taskBody))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusCreated), "POST /api/v1/tasks should return 201")

		By("retrieving the task via GET /api/v1/tasks/e2e-api-crud-task")
		Eventually(func(g Gomega) {
			req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/tasks/e2e-api-crud-task", nil)
			g.Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+token)

			resp, err := http.DefaultClient.Do(req)
			g.Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			g.Expect(resp.StatusCode).To(Equal(http.StatusOK))

			body, err := io.ReadAll(resp.Body)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(body)).To(ContainSubstring("e2e-api-crud-task"))
		}, 30*time.Second, time.Second).Should(Succeed())

		By("waiting for the API-created task to complete")
		waitForTaskCompletion("e2e-api-crud-task", 3*time.Minute)

		By("retrieving the task result via GET /api/v1/tasks/e2e-api-crud-task/result")
		Eventually(func(g Gomega) {
			req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/tasks/e2e-api-crud-task/result", nil)
			g.Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+token)

			resp, err := http.DefaultClient.Do(req)
			g.Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			g.Expect(resp.StatusCode).To(Equal(http.StatusOK))

			body, err := io.ReadAll(resp.Body)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(body)).NotTo(BeEmpty(), "Result should not be empty")
		}, 30*time.Second, time.Second).Should(Succeed())

		By("deleting the task via DELETE /api/v1/tasks/e2e-api-crud-task")
		req, err = http.NewRequest("DELETE", apiBaseURL+"/api/v1/tasks/e2e-api-crud-task", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err = http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(BeElementOf(http.StatusOK, http.StatusNoContent),
			"DELETE should return 200 or 204")

		By("verifying the task is deleted")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", "e2e-api-crud-task",
				"-n", namespace, "--ignore-not-found")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(BeEmpty(), "Task should be deleted")
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	It("should stream task logs via GET /api/v1/tasks/{id}/logs", func() {
		By("creating a container task that produces output")
		const logTaskName = "e2e-api-log-task"

		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "task", logTaskName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

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
				"command": ["echo"],
				"args": ["log-output-for-api-test"]
			}
		}`, logTaskName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the task to complete")
		waitForTaskCompletion(logTaskName, 3*time.Minute)

		By("fetching task logs via API")
		token, err := serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/tasks/"+logTaskName+"/logs", nil)
			g.Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+token)

			resp, err := http.DefaultClient.Do(req)
			g.Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			g.Expect(resp.StatusCode).To(Equal(http.StatusOK))

			body, err := io.ReadAll(resp.Body)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(body)).To(ContainSubstring("log-output-for-api-test"),
				"Logs should contain the task output")
		}, 30*time.Second, time.Second).Should(Succeed())
	})
})
