//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/orka-agents/orka/test/utils"
)

var _ = Describe("Live Copilot Proxy Provider", Ordered, func() {
	const (
		liveProxyProviderName = "e2e-live-copilot-provider"
		liveProxySecretName   = "e2e-live-copilot-secret"
		liveProxyTaskName     = "e2e-live-copilot-task"
		expectedOutput        = "ORKA_LIVE_COPILOT_OK"
		liveProxyProbePFPort  = 18091
	)

	var (
		apiBaseURL         string
		cancelControllerPF context.CancelFunc
		controllerPFCmd    *exec.Cmd
		proxyBaseURL       string
		cancelProxyPF      context.CancelFunc
		proxyPFCmd         *exec.Cmd
		discoveredModel    string
		token              string
	)

	BeforeAll(func() {
		if e2eLiveCopilotProxyBaseURL == "" {
			Skip("Skipping: E2E_LIVE_COPILOT_PROXY_BASE_URL not set")
		}

		By("setting up port-forward to controller API")
		var err error
		apiBaseURL, cancelControllerPF, controllerPFCmd, err = startControllerAPIPortForward(18086)
		Expect(err).NotTo(HaveOccurred())

		By("getting a service account token")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())

		By("setting up port-forward to the live copilot proxy")
		proxyBaseURL, cancelProxyPF, proxyPFCmd, err = startServicePortForward(
			liveCopilotProxyServiceNamespace(),
			liveCopilotProxyServiceName(),
			18189,
			liveCopilotProxyServicePort(),
		)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		stopPortForward(cancelControllerPF, controllerPFCmd)
		stopPortForward(cancelProxyPF, proxyPFCmd)
	})

	AfterEach(func() {
		dumpDebugInfo(liveProxyTaskName)
		dumpLiveCopilotProxyDebugInfo(liveProxyProviderName)
	})

	It("should expose a ready proxy service and a non-empty OpenAI model catalog", func() {
		By("verifying the proxy reports ready status")
		ready := waitForProxyReadyViaServiceProxy(
			liveCopilotProxyServiceNamespace(),
			liveCopilotProxyServiceName(),
			liveCopilotProxyServicePort(),
		)
		Expect(ready.Status).To(Equal("ready"))
		Expect(ready.Error).To(BeEmpty())

		By("verifying the proxy exposes OpenAI-compatible models")
		catalog, err := fetchProxyModelCatalogViaServiceProxy(
			liveCopilotProxyServiceNamespace(),
			liveCopilotProxyServiceName(),
			liveCopilotProxyServicePort(),
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(catalog.DataModelIDs).NotTo(BeEmpty(), "proxy should expose models via the OpenAI data field")
		Expect(catalog.AllModelIDs).NotTo(BeEmpty(), "proxy should expose at least one model")

		By("discovering a GPT-family model that works through the OpenAI provider path")
		discoveredModel, err = firstUsableProxyOpenAIModel(
			proxyBaseURL,
			catalog,
			liveProxyOpenAIModelPreferences,
			liveCopilotProxyGPTModelPrefixes...,
		)
		Expect(err).NotTo(HaveOccurred())
		if discoveredModel == "" {
			Skip("Skipping live Copilot proxy OpenAI provider checks: no usable GPT OpenAI model exposed")
		}
		Expect(discoveredModel).To(BeElementOf(catalog.AllModelIDs))
	})

	It("should run a tiny AI task through the live copilot proxy and return the exact output", func() {
		By("discovering a live chat-completions model from the proxy service")
		model := discoveredModel
		if model == "" {
			var err error
			model, err = discoverUsableProxyOpenAIModelViaServiceProxy(
				liveCopilotProxyServiceNamespace(),
				liveCopilotProxyServiceName(),
				liveCopilotProxyServicePort(),
				liveProxyProbePFPort,
				liveProxyOpenAIModelPreferences,
				liveCopilotProxyGPTModelPrefixes...,
			)
			Expect(err).NotTo(HaveOccurred())
		}
		if model == "" {
			Skip("Skipping live Copilot proxy OpenAI provider check: no usable GPT OpenAI model exposed")
		}

		By("creating a dummy secret for provider validation")
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "secret", liveProxySecretName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
		err := createK8sSecret(liveProxySecretName, namespace, map[string]string{
			"api-key": liveProxyProbeAPIKey,
		})
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("creating a Provider CRD backed by copilot-proxy model %q", model))
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "provider", liveProxyProviderName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
		createProviderCRD(liveProxyProviderName, "openai", liveProxySecretName, "api-key", e2eLiveCopilotProxyBaseURL, model)

		By("verifying the Provider status details")
		expectProviderValidated(liveProxyProviderName, "openai", e2eLiveCopilotProxyBaseURL, model)

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
					"maxTokens": 16
				}
			}
		}`, liveProxyTaskName, namespace, liveProxyProviderName, expectedOutput)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create live copilot proxy AI task")

		By("verifying Orka created a worker Job for the task")
		verifyJobCreatedForTask(liveProxyTaskName, 30*time.Second)

		By("waiting for the AI task to complete")
		phase := waitForTaskCompletion(liveProxyTaskName, 5*time.Minute)
		if phase == "Failed" && liveCopilotProxyTaskFailedWithForbidden(liveProxyTaskName) {
			Skip("Skipping: live Copilot proxy chat completions returned 403 for model " + model)
		}
		Expect(phase).To(Equal("Succeeded"), "Live copilot proxy AI task should succeed")

		By("verifying the result is available")
		verifyResultAvailable(liveProxyTaskName)

		By("verifying the final task status details")
		expectNativeAITaskSucceeded(liveProxyTaskName)

		By("fetching the task result through the controller API")
		result := fetchTaskResultViaAPI(apiBaseURL, token, liveProxyTaskName)
		Expect(strings.TrimSpace(result)).To(Equal(expectedOutput))
	})
})
