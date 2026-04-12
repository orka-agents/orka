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
		apiBaseURL         string
		cancelControllerPF context.CancelFunc
		controllerPFCmd    *exec.Cmd
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
	})

	AfterAll(func() {
		stopPortForward(cancelControllerPF, controllerPFCmd)
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
		discoveredModel = firstPreferredProxyModel(catalog, []string{
			"gpt-5.4",
			"gpt-5.2",
			"gpt-5.4-mini",
		}, "gpt-")
		Expect(discoveredModel).To(BeElementOf(catalog.DataModelIDs))
		Expect(discoveredModel).NotTo(BeEmpty(), "proxy should expose a GPT-family model")
	})

	It("should run a tiny AI task through the live copilot proxy and return the exact output", func() {
		By("discovering a live GPT-family model from the proxy service")
		model := discoveredModel
		if model == "" {
			model = discoverPreferredProxyModelViaServiceProxy(
				liveCopilotProxyServiceNamespace(),
				liveCopilotProxyServiceName(),
				liveCopilotProxyServicePort(),
				[]string{"gpt-5.4", "gpt-5.2", "gpt-5.4-mini"},
				"gpt-",
			)
		}
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

		By("verifying the Provider status details")
		provider := fetchProviderSnapshot(liveProxyProviderName)
		Expect(provider.Spec.Type).To(Equal("openai"))
		Expect(provider.Spec.BaseURL).To(Equal(e2eLiveCopilotProxyBaseURL))
		Expect(provider.Spec.DefaultModel).To(Equal(model))
		Expect(provider.Status.Ready).To(BeTrue())
		Expect(provider.Status.Message).To(Equal("Provider configuration is valid"))
		Expect(provider.Status.LastValidated).NotTo(BeEmpty())
		readyCondition := findStatusCondition(provider.Status.Conditions, "Ready")
		Expect(readyCondition).NotTo(BeNil())
		Expect(readyCondition.Status).To(Equal("True"))
		Expect(readyCondition.Reason).To(Equal("ValidationSucceeded"))

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

		By("verifying Orka created a worker Job for the task")
		verifyJobCreatedForTask(liveProxyTaskName, 30*time.Second)

		By("waiting for the AI task to complete")
		phase := waitForTaskCompletion(liveProxyTaskName, 5*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "Live copilot proxy AI task should succeed")

		By("verifying the result is available")
		verifyResultAvailable(liveProxyTaskName)

		By("verifying the final task status details")
		task := fetchTaskSnapshot(liveProxyTaskName)
		Expect(task.Status.Phase).To(Equal("Succeeded"))
		Expect(task.Status.JobName).NotTo(BeEmpty())
		Expect(task.Status.Attempts).To(Equal(int32(1)))
		Expect(task.Status.Message).To(Equal("task completed successfully"))
		Expect(task.Status.StartTime).NotTo(BeEmpty())
		Expect(task.Status.CompletionTime).NotTo(BeEmpty())
		Expect(task.Status.ResultRef).NotTo(BeNil())
		Expect(task.Status.ResultRef.Available).To(BeTrue())
		completeCondition := findStatusCondition(task.Status.Conditions, "Complete")
		Expect(completeCondition).NotTo(BeNil())
		Expect(completeCondition.Status).To(Equal("True"))
		Expect(completeCondition.Reason).To(Equal("TaskSucceeded"))

		By("fetching the task result through the controller API")
		result := fetchTaskResultViaAPI(apiBaseURL, token, liveProxyTaskName)
		Expect(strings.TrimSpace(result)).To(Equal(expectedOutput))
	})
})
