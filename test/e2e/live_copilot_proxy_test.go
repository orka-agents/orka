//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/test/utils"
)

var _ = Describe("Live Copilot Proxy Provider", Ordered, func() {
	const (
		liveProxyProviderName = "e2e-live-copilot-provider"
		liveProxySecretName   = "e2e-live-copilot-secret"
		liveProxyTaskName     = "e2e-live-copilot-task"
		liveProxyCoordAgent   = "e2e-live-copilot-coord-agent"
		liveProxyCoordTask    = "e2e-live-copilot-coord-task"
		expectedOutput        = "ORKA_LIVE_COPILOT_OK"
		expectedCoordOutput   = "ORKA_LIVE_COPILOT_COORDINATION_OK"
		memoryProposalMarker  = "orka-live-copilot-coordination-memory-e2e"
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
		dumpDebugInfo(liveProxyTaskName, liveProxyCoordTask)
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
		discoveredModel = firstPreferredProxyModel(
			catalog,
			liveCopilotProxyChatGPTModelPreferences,
			liveCopilotProxyGPTModelPrefixes...,
		)
		Expect(discoveredModel).To(BeElementOf(catalog.AllModelIDs))
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
				liveCopilotProxyChatGPTModelPreferences,
				liveCopilotProxyGPTModelPrefixes...,
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
		if phase == "Failed" && liveCopilotProxyTaskFailedWithForbidden(liveProxyTaskName) {
			Skip("Skipping: live Copilot proxy chat completions returned 403 for model " + model)
		}
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

	It("should auto-inject and execute coordination memory tools for a live copilot proxy Agent", func() {
		By("discovering a live chat-completions model from the proxy service")
		catalog, err := fetchProxyModelCatalogViaServiceProxy(
			liveCopilotProxyServiceNamespace(),
			liveCopilotProxyServiceName(),
			liveCopilotProxyServicePort(),
		)
		Expect(err).NotTo(HaveOccurred())
		model, skipReason, err := firstLiveCopilotProxyChatCompletionModel(
			proxyBaseURL,
			catalog,
			liveCopilotProxyChatModelPreferences(),
			"gpt-",
			"claude-",
		)
		Expect(err).NotTo(HaveOccurred())
		if model == "" {
			Skip("Skipping: " + skipReason)
		}

		By("creating a dummy secret for provider validation")
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "secret", liveProxySecretName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
		err = createK8sSecret(liveProxySecretName, namespace, map[string]string{
			"api-key": "dummy-live-copilot-proxy-key",
		})
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("creating a Provider CRD backed by copilot-proxy model %q", model))
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "provider", liveProxyProviderName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
		createProviderCRD(liveProxyProviderName, "openai", liveProxySecretName, "api-key", e2eLiveCopilotProxyBaseURL, model)

		By("verifying the Provider is ready")
		provider := fetchProviderSnapshot(liveProxyProviderName)
		Expect(provider.Status.Ready).To(BeTrue())

		By("creating an AI Agent with coordination enabled and no explicit tools")
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "agent", liveProxyCoordAgent, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
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
					"name": "%s",
					"temperature": 0,
					"maxTokens": 512
				},
				"systemPrompt": {
					"inline": "You are an Orka E2E test coordinator. When the user asks you to verify tools, call the requested tools before returning the final exact marker."
				},
				"coordination": {
					"enabled": true
				}
			}
		}`, liveProxyCoordAgent, namespace, liveProxyProviderName, model)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create live copilot proxy coordination Agent")

		uniqueMarkerSuffix := time.Now().UnixNano()
		durableMemoryMarker := fmt.Sprintf("orka-live-copilot-coordination-durable-e2e-%d", uniqueMarkerSuffix)
		proposalMarker := fmt.Sprintf("%s-%d", memoryProposalMarker, uniqueMarkerSuffix)

		By("pre-seeding a durable memory for recall")
		preseededMemory := createDurableMemoryViaAPI(apiBaseURL, token, store.Memory{
			TaskName:  liveProxyCoordTask,
			AgentName: liveProxyCoordAgent,
			Source:    "e2e",
			Content:   fmt.Sprintf("Durable live Copilot proxy coordination memory marker %s for recall only.", durableMemoryMarker),
			Tags:      []string{"e2e", "live-copilot-proxy", "coordination"},
		})
		DeferCleanup(func() {
			cleanupDurableMemoryViaAPI(apiBaseURL, token, preseededMemory.ID)
		})
		initialDurableMemories := listDurableMemoriesViaAPI(apiBaseURL, token, url.Values{
			"query": []string{durableMemoryMarker},
		})
		Expect(initialDurableMemories).To(ContainElement(WithTransform(func(memory store.Memory) string {
			return memory.ID
		}, Equal(preseededMemory.ID))))
		initialDurableMemoryCount := len(initialDurableMemories)

		By("creating an AI Task referencing the coordination Agent")
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "task", liveProxyCoordTask, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
		prompt := fmt.Sprintf(`This is an Orka live Copilot proxy E2E tool execution check.

Use these tools exactly once before your final answer:
1. search_transcript with query "%[1]s"
2. recall_memory with query "%[2]s"
3. propose_memory with a title containing "%[1]s" and content containing "%[1]s" describing that live Copilot proxy coordination memory tools executed
4. remember with content containing "%[1]s"

The recall_memory query marker and proposal marker are intentionally different. Do not put "%[2]s" in the proposed memory content.
After the tools have been called, reply with exactly %[3]s and nothing else.`, proposalMarker, durableMemoryMarker, expectedCoordOutput)
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "ai",
				"agentRef": {
					"name": "%s"
				},
				"ai": {
					"providerRef": {
						"name": "%s"
					},
					"model": "%s",
					"prompt": %q,
					"temperature": 0,
					"maxTokens": 512
				}
			}
		}`, liveProxyCoordTask, namespace, liveProxyCoordAgent, liveProxyProviderName, model, prompt)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create live copilot proxy coordination AI task")

		By("verifying Orka created a worker Job for the coordination task")
		verifyJobCreatedForTask(liveProxyCoordTask, 30*time.Second)

		By("verifying coordination env vars and memory tools were auto-injected")
		envMap := getJobEnvMap(liveProxyCoordTask)
		Expect(envMap).To(HaveKeyWithValue("ORKA_COORDINATION_ENABLED", "true"))

		toolsEnv := envMap["ORKA_AI_TOOLS"]
		Expect(toolsEnv).NotTo(BeEmpty())
		toolSet := map[string]bool{}
		for _, tool := range strings.Split(toolsEnv, ",") {
			toolSet[strings.TrimSpace(tool)] = true
		}
		for _, tool := range []string{"recall_memory", "remember", "propose_memory", "search_transcript"} {
			Expect(toolSet).To(HaveKey(tool), "ORKA_AI_TOOLS = %s", toolsEnv)
		}

		By("waiting for the coordination AI task to complete")
		phase := waitForTaskCompletion(liveProxyCoordTask, 8*time.Minute)
		if phase == "Failed" && liveCopilotProxyTaskFailedWithForbidden(liveProxyCoordTask) {
			Skip("Skipping: live Copilot proxy chat completions returned 403 for model " + model)
		}
		Expect(phase).To(Equal("Succeeded"), "Live copilot proxy coordination AI task should succeed")

		By("verifying the worker logs show each memory tool was executed")
		Eventually(func(g Gomega) {
			task := fetchTaskSnapshot(liveProxyCoordTask)
			g.Expect(task.Status.JobName).NotTo(BeEmpty())

			cmd := exec.Command("kubectl", "logs", "job/"+task.Status.JobName, "-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			for _, tool := range []string{"recall_memory", "remember", "propose_memory", "search_transcript"} {
				g.Expect(output).To(ContainSubstring("Executing tool: " + tool))
			}
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("verifying recall used durable memory without creating new durable entries")
		durableMemories := listDurableMemoriesViaAPI(apiBaseURL, token, url.Values{
			"query": []string{durableMemoryMarker},
		})
		Expect(durableMemories).To(ContainElement(WithTransform(func(memory store.Memory) string {
			return memory.ID
		}, Equal(preseededMemory.ID))))
		Expect(durableMemories).To(HaveLen(initialDurableMemoryCount))

		By("verifying proposed memory content was not written as durable memory")
		proposalDurableMemories := listDurableMemoriesViaAPI(apiBaseURL, token, url.Values{
			"query": []string{proposalMarker},
		})
		Expect(proposalDurableMemories).To(BeEmpty())

		By("verifying propose_memory and remember persisted governance proposals")
		var proposals []store.MemoryProposal
		Eventually(func(g Gomega) {
			proposals = listMemoryProposalsViaAPI(apiBaseURL, token, url.Values{
				"taskName": []string{liveProxyCoordTask},
				"query":    []string{proposalMarker},
			})
			g.Expect(len(proposals)).To(BeNumerically(">=", 2))
		}, time.Minute, 2*time.Second).Should(Succeed())

		By("reviewing memory proposals without applying them to durable memory")
		acceptedProposal := reviewMemoryProposalViaAPI(apiBaseURL, token, proposals[0].ID, store.MemoryProposalReview{
			Status:     "accepted",
			Reviewer:   "e2e-live-copilot",
			ReviewNote: "Accepted during live Copilot proxy E2E coverage.",
		})
		rejectedProposal := reviewMemoryProposalViaAPI(apiBaseURL, token, proposals[1].ID, store.MemoryProposalReview{
			Status:     "rejected",
			Reviewer:   "e2e-live-copilot",
			ReviewNote: "Rejected during live Copilot proxy E2E coverage.",
		})
		Expect(acceptedProposal.Status).To(Equal("accepted"))
		Expect(acceptedProposal.Reviewer).To(Equal("e2e-live-copilot"))
		Expect(acceptedProposal.ReviewedAt).NotTo(BeNil())
		Expect(rejectedProposal.Status).To(Equal("rejected"))
		Expect(rejectedProposal.Reviewer).To(Equal("e2e-live-copilot"))
		Expect(rejectedProposal.ReviewedAt).NotTo(BeNil())

		By("verifying reviewed proposals still were not applied as durable memory")
		proposalDurableMemories = listDurableMemoriesViaAPI(apiBaseURL, token, url.Values{
			"query": []string{proposalMarker},
		})
		Expect(proposalDurableMemories).To(BeEmpty())
		durableMemories = listDurableMemoriesViaAPI(apiBaseURL, token, url.Values{
			"query": []string{durableMemoryMarker},
		})
		Expect(durableMemories).To(ContainElement(WithTransform(func(memory store.Memory) string {
			return memory.ID
		}, Equal(preseededMemory.ID))))
		Expect(durableMemories).To(HaveLen(initialDurableMemoryCount))
	})
})
