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

var _ = Describe("Live Copilot Proxy Provider", Ordered, func() {
	const (
		liveProxyProviderName = "e2e-live-copilot-provider"
		liveProxySecretName   = "e2e-live-copilot-secret"
		liveProxyTaskName     = "e2e-live-copilot-task"
		expectedOutput        = "ORKA_LIVE_COPILOT_OK"
	)

	var (
		apiBaseURL            string
		proxyDiscoveryBaseURL string
		cancelControllerPF    context.CancelFunc
		controllerPFCmd       *exec.Cmd
		cancelProxyPF         context.CancelFunc
		proxyPFCmd            *exec.Cmd
		token                 string
	)

	BeforeAll(func() {
		if e2eLiveCopilotProxyBaseURL == "" {
			Skip("Skipping: E2E_LIVE_COPILOT_PROXY_BASE_URL not set")
		}

		By("setting up port-forward to controller API")
		var err error
		apiBaseURL, cancelControllerPF, controllerPFCmd, err = startControllerAPIPortForward(18086)
		Expect(err).NotTo(HaveOccurred())

		By("setting up port-forward to the live copilot-proxy service")
		proxyDiscoveryBaseURL, cancelProxyPF, proxyPFCmd, err = startServicePortForward(
			liveCopilotProxyServiceNamespace(),
			liveCopilotProxyServiceName(),
			18085,
			liveCopilotProxyServicePort(),
		)
		Expect(err).NotTo(HaveOccurred())

		By("getting a service account token")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())
	})

	AfterAll(func() {
		stopPortForward(cancelProxyPF, proxyPFCmd)
		stopPortForward(cancelControllerPF, controllerPFCmd)
	})

	AfterEach(func() {
		dumpDebugInfo(liveProxyTaskName)
	})

	It("should run a tiny AI task through the live copilot proxy and return the exact output", func() {
		By("discovering a live model from the proxy")
		model := discoverProxyModel(proxyDiscoveryBaseURL)
		Expect(model).NotTo(BeEmpty())

		By("creating a dummy secret for provider validation")
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "secret", liveProxySecretName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
		err := createK8sSecret(liveProxySecretName, namespace, map[string]string{
			"api-key": "dummy-live-copilot-proxy-key",
		})
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("creating a Provider CRD backed by copilot-proxy model %q", model))
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "provider", liveProxyProviderName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
		createProviderCRD(liveProxyProviderName, "openai", liveProxySecretName, "api-key", e2eLiveCopilotProxyBaseURL, model)

		By("creating a tiny AI task that asks for an exact response")
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "task", liveProxyTaskName, "-n", namespace, "--ignore-not-found")
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
				"type": "ai",
				"ai": {
					"providerRef": {
						"name": "%s"
					},
					"prompt": "Reply with exactly %s and nothing else.",
					"temperature": 0,
					"maxTokens": 8
				}
			}
		}`, liveProxyTaskName, namespace, liveProxyProviderName, expectedOutput)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create live copilot proxy AI task")

		By("waiting for the AI task to complete")
		phase := waitForTaskCompletion(liveProxyTaskName, 5*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "Live copilot proxy AI task should succeed")

		By("verifying the result is available")
		verifyResultAvailable(liveProxyTaskName)

		By("fetching the task result through the controller API")
		result := fetchTaskResult(apiBaseURL, token, liveProxyTaskName)
		Expect(strings.TrimSpace(result)).To(Equal(expectedOutput))
	})
})

func fetchTaskResult(apiBaseURL, token, taskName string) string {
	var result string
	Eventually(func(g Gomega) {
		got, err := getTaskResult(apiBaseURL, token, taskName)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(got)).NotTo(BeEmpty())
		result = got
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
	return result
}

func getTaskResult(apiBaseURL, token, taskName string) (string, error) {
	url := fmt.Sprintf("%s/api/v1/tasks/%s/result?namespace=%s", strings.TrimRight(apiBaseURL, "/"), taskName, namespace)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status from task result endpoint: %s (%s)", resp.Status, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}

	return payload.Result, nil
}
