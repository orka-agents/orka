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

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/test/utils"
)

// Coordination memory tools run against the live Copilot proxy when
// E2E_LIVE_COPILOT_PROXY_BASE_URL is set, and otherwise against the suite's
// aimock, which scripts the four tool calls with this run's markers.
var _ = Describe("Coordination Memory Tools", Ordered, func() {
	const (
		providerName         = "e2e-memory-tools-provider"
		secretName           = "e2e-memory-tools-secret"
		agentName            = "e2e-memory-tools-agent"
		taskName             = "e2e-memory-tools-task"
		expectedOutput       = "ORKA_COORDINATION_MEMORY_OK"
		memoryProposalMarker = "orka-coordination-memory-e2e"
		marker               = "[e2e:memory-tools]"
		mockModel            = "gpt-4o-mini"
	)

	var (
		apiBaseURL string
		token      string
		model      string
		live       bool
	)

	BeforeAll(func() {
		live = strings.TrimSpace(e2eLiveCopilotProxyBaseURL) != ""
		if !live && !e2eMockOpenAI {
			Skip("Skipping: needs E2E_LIVE_COPILOT_PROXY_BASE_URL or aimock")
		}

		By("setting up port-forward to controller API")
		var (
			cancelPF context.CancelFunc
			pfCmd    *exec.Cmd
			err      error
		)
		apiBaseURL, cancelPF, pfCmd, err = startControllerAPIPortForward(18092)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { stopPortForward(cancelPF, pfCmd) })

		By("getting a service account token")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())

		providerBaseURL := mockLLMServiceURL() + "/v1"
		model = mockModel
		if live {
			By("discovering a live chat-completions model from the proxy service")
			proxyBaseURL, cancelProxyPF, proxyPFCmd, err := startServicePortForward(
				liveCopilotProxyServiceNamespace(),
				liveCopilotProxyServiceName(),
				18192,
				liveCopilotProxyServicePort(),
			)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { stopPortForward(cancelProxyPF, proxyPFCmd) })
			catalog, err := fetchProxyModelCatalogViaServiceProxy(
				liveCopilotProxyServiceNamespace(),
				liveCopilotProxyServiceName(),
				liveCopilotProxyServicePort(),
			)
			Expect(err).NotTo(HaveOccurred())
			var skipReason string
			model, skipReason, err = firstLiveCopilotProxyChatCompletionModel(
				proxyBaseURL,
				e2eGitHubToken,
				catalog,
				liveCopilotProxyChatModelPreferences(),
				"gpt-",
				"claude-",
			)
			Expect(err).NotTo(HaveOccurred())
			if model == "" {
				Skip("Skipping: " + skipReason)
			}
			providerBaseURL = e2eLiveCopilotProxyBaseURL
		}

		By("creating a dummy secret for provider validation")
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "secret", secretName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
		Expect(createK8sSecret(secretName, namespace, map[string]string{"api-key": liveProxyProbeAPIKey})).To(Succeed())

		By(fmt.Sprintf("creating a Provider CRD for model %q", model))
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "provider", providerName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
		createProviderCRD(providerName, "openai", secretName, "api-key", providerBaseURL, model)
		Expect(fetchProviderSnapshot(providerName).Status.Ready).To(BeTrue())
	})

	AfterEach(func() {
		dumpDebugInfo(taskName)
		if live {
			dumpLiveCopilotProxyDebugInfo(providerName)
		}
	})

	It("should auto-inject and execute coordination memory tools for an Agent", func() {
		By("creating an AI Agent with coordination enabled and no explicit tools")
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "agent", agentName, "-n", namespace, "--ignore-not-found")
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
		}`, agentName, namespace, providerName, model)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create coordination Agent")

		uniqueMarkerSuffix := time.Now().UnixNano()
		durableMemoryMarker := fmt.Sprintf("orka-coordination-durable-e2e-%d", uniqueMarkerSuffix)
		proposalMarker := fmt.Sprintf("%s-%d", memoryProposalMarker, uniqueMarkerSuffix)

		By("pre-seeding a durable memory for recall")
		preseededMemory := createDurableMemoryViaAPI(apiBaseURL, token, store.Memory{
			TaskName:  taskName,
			AgentName: agentName,
			Source:    "e2e",
			Content:   fmt.Sprintf("Durable coordination memory marker %s for recall only.", durableMemoryMarker),
			Tags:      []string{"e2e", "coordination"},
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

		if !live {
			By("scripting the four memory tool calls with this run's markers")
			injectMockLLMFixtures(
				map[string]any{
					"match": map[string]any{"userMessage": marker, "hasToolResult": false},
					"response": map[string]any{"toolCalls": []map[string]any{
						{"name": "search_transcript", "arguments": map[string]any{"query": proposalMarker}},
						{"name": "recall_memory", "arguments": map[string]any{"query": durableMemoryMarker}},
						{"name": "propose_memory", "arguments": map[string]any{
							"title":   "Coordination memory tools executed " + proposalMarker,
							"content": "Coordination memory tools executed in the E2E suite: " + proposalMarker,
						}},
						{"name": "remember", "arguments": map[string]any{
							"content": "Coordination memory tools ran in the E2E suite: " + proposalMarker,
						}},
					}},
				},
				map[string]any{
					"match":    map[string]any{"userMessage": marker, "hasToolResult": true},
					"response": map[string]any{"content": expectedOutput},
				},
			)
		}

		By("creating an AI Task referencing the coordination Agent")
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "task", taskName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
		prompt := fmt.Sprintf(`This is an Orka coordination memory tool execution check. %[4]s

Use these tools exactly once before your final answer:
1. search_transcript with query "%[1]s"
2. recall_memory with query "%[2]s"
3. propose_memory with a title containing "%[1]s" and content containing "%[1]s" describing that coordination memory tools executed
4. remember with content containing "%[1]s"

The recall_memory query marker and proposal marker are intentionally different. Do not put "%[2]s" in the proposed memory content.
After the tools have been called, reply with exactly %[3]s and nothing else.`, proposalMarker, durableMemoryMarker, expectedOutput, marker)
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
		}`, taskName, namespace, agentName, providerName, model, prompt)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create coordination AI task")

		By("verifying Orka created a worker Job for the coordination task")
		verifyJobCreatedForTask(taskName, 30*time.Second)

		By("verifying coordination env vars and memory tools were auto-injected")
		envMap := getJobEnvMap(taskName)
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
		phase := waitForTaskCompletion(taskName, 8*time.Minute)
		if live && phase == "Failed" && liveCopilotProxyTaskFailedWithForbidden(taskName) {
			Skip("Skipping: live Copilot proxy chat completions returned 403 for model " + model)
		}
		Expect(phase).To(Equal("Succeeded"), "coordination AI task should succeed")

		By("verifying the worker logs show each memory tool was executed")
		Eventually(func(g Gomega) {
			task := fetchTaskSnapshot(taskName)
			g.Expect(task.Status.JobName).NotTo(BeEmpty())

			cmd := exec.Command("kubectl", "logs", "job/"+task.Status.JobName, "-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			for _, tool := range []string{"recall_memory", "remember", "propose_memory", "search_transcript"} {
				g.Expect(output).To(ContainSubstring("Executing tool: " + tool))
			}
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		if !live {
			By("verifying recall_memory returned the pre-seeded memory to the model")
			Expect(expectMockLLMToolResult(marker, "recall_memory", time.Minute)).To(ContainSubstring(durableMemoryMarker))
		}

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
				"taskName": []string{taskName},
				"query":    []string{proposalMarker},
			})
			g.Expect(len(proposals)).To(BeNumerically(">=", 2))
		}, time.Minute, 2*time.Second).Should(Succeed())

		By("reviewing memory proposals without applying them to durable memory")
		acceptedProposal := reviewMemoryProposalViaAPI(apiBaseURL, token, proposals[0].ID, store.MemoryProposalReview{
			Status:     "accepted",
			Reviewer:   "e2e-coordination",
			ReviewNote: "Accepted during coordination memory E2E coverage.",
		})
		rejectedProposal := reviewMemoryProposalViaAPI(apiBaseURL, token, proposals[1].ID, store.MemoryProposalReview{
			Status:     "rejected",
			Reviewer:   "e2e-coordination",
			ReviewNote: "Rejected during coordination memory E2E coverage.",
		})
		Expect(acceptedProposal.Status).To(Equal("accepted"))
		Expect(acceptedProposal.Reviewer).To(Equal("e2e-coordination"))
		Expect(acceptedProposal.ReviewedAt).NotTo(BeNil())
		Expect(rejectedProposal.Status).To(Equal("rejected"))
		Expect(rejectedProposal.Reviewer).To(Equal("e2e-coordination"))
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
