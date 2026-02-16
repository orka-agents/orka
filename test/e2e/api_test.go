//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"context"
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
})
