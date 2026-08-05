//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/test/utils"
)

const (
	liveRuntimeRepoURL      = "https://github.com/octocat/Hello-World.git"
	liveRuntimeRepoRef      = "7fd1a60b01f91b314f59955a4e4d4e80d8edf11d"
	liveRuntimeRepoSentinel = "Hello World!"
	liveRuntimeTimeout      = 8 * time.Minute
)

var _ = Describe("Live Agent Runtime Matrix", Ordered, func() {
	const (
		codexAgentName         = "e2e-live-runtime-codex-agent"
		codexTaskReadName      = "e2e-live-runtime-codex-read"
		opencodeAgentName      = "e2e-live-runtime-opencode-agent"
		opencodeTaskReadName   = "e2e-live-runtime-opencode-read"
		claudeAgentName        = "e2e-live-runtime-claude-agent"
		claudeTaskName         = "e2e-live-runtime-claude-task"
		claudeExpectedResponse = "ORKA_LIVE_CLAUDE_OK"
	)

	var (
		apiBaseURL              string
		cancelControllerPF      context.CancelFunc
		controllerPFCmd         *exec.Cmd
		token                   string
		gptModel                string
		gptModelSkipReason      string
		opencodeModel           string
		opencodeModelSkipReason string
		claudeModel             string
		claudeSessionName       string
	)

	BeforeAll(func() {
		if strings.TrimSpace(e2eLiveCopilotProxyBaseURL) == "" {
			Skip("Skipping: E2E_LIVE_COPILOT_PROXY_BASE_URL not set")
		}

		var err error

		By("setting up port-forward to the controller API")
		apiBaseURL, cancelControllerPF, controllerPFCmd, err = startControllerAPIPortForward(18088)
		Expect(err).NotTo(HaveOccurred())

		By("getting a service account token")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())

		By("verifying the live proxy is ready")
		ready := waitForProxyReadyViaServiceProxy(
			liveACPProviderProxyServiceNamespace(),
			liveACPProviderProxyServiceName(),
			liveACPProviderProxyServicePort(),
		)
		Expect(ready.Status).To(Equal("ready"))
		Expect(ready.Error).To(BeEmpty())

		By("discovering live runtime models by family")
		runtimeCatalog, err := fetchProxyModelCatalogViaServiceProxy(
			liveACPProviderProxyServiceNamespace(),
			liveACPProviderProxyServiceName(),
			liveACPProviderProxyServicePort(),
		)
		Expect(err).NotTo(HaveOccurred())
		gptModel = strings.TrimSpace(os.Getenv("E2E_LIVE_CODEX_RUNTIME_MODEL"))
		if gptModel != "" {
			if !runtimeCatalog.modelSupportsEndpoint(gptModel, "/responses") {
				gptModelSkipReason = "configured E2E_LIVE_CODEX_RUNTIME_MODEL does not advertise /responses support"
				gptModel = ""
			}
		} else {
			gptModel = firstPreferredProxyCodexModelSupportingEndpoint(
				runtimeCatalog,
				"/responses",
				liveCopilotProxyCodexModelPreferences,
				liveCopilotProxyCodexModelPrefixes...,
			)
			if gptModel == "" {
				gptModelSkipReason = "no Codex-family GPT model with /responses support exposed"
			}
		}
		opencodeModel = strings.TrimSpace(os.Getenv("E2E_LIVE_OPENCODE_RUNTIME_MODEL"))
		if opencodeModel != "" {
			if !openCodeModelSupportsEndpoint(runtimeCatalog, opencodeModel, "/chat/completions") {
				opencodeModelSkipReason = "configured E2E_LIVE_OPENCODE_RUNTIME_MODEL does not advertise /chat/completions support"
				opencodeModel = ""
			}
		} else {
			opencodeModel = firstPreferredProxyModelSupportingEndpoint(
				runtimeCatalog,
				"/chat/completions",
				liveCopilotProxyChatGPTModelPreferences,
				liveCopilotProxyGPTModelPrefixes...,
			)
			if opencodeModel == "" {
				opencodeModelSkipReason = "no GPT-family model with /chat/completions support exposed"
			}
		}

		if opencodeModel != "" && !strings.Contains(opencodeModel, "/") {
			opencodeModel = "openai/" + opencodeModel
		}

		claudeModel = firstPreferredProxyModel(
			runtimeCatalog,
			liveCopilotProxyClaudeModelPreferences,
			liveCopilotProxyClaudeModelPrefixes...,
		)
		Expect(claudeModel).NotTo(BeEmpty(), "proxy service should expose a Claude-family model")

		claudeSessionName = fmt.Sprintf("e2e-live-runtime-claude-%d", time.Now().UnixNano())
	})

	AfterAll(func() {
		stopPortForward(cancelControllerPF, controllerPFCmd)
	})

	AfterEach(func() {
		dumpDebugInfo(
			codexTaskReadName,
			opencodeTaskReadName,
			claudeTaskName,
		)
		dumpLiveCopilotProxyDebugInfo()
	})

	It("should run Codex through ACP v2 against a pinned read workspace", func() {
		if gptModel == "" {
			Skip("Skipping Codex runtime live proxy check: " + gptModelSkipReason)
		}

		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "task", codexTaskReadName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "agent", codexAgentName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		By("creating a Codex agent backed by the discovered GPT-family model")
		err := applyManifestJSON(runtimeAgentManifest(codexAgentName, "codex", gptModel, 5, nil))
		Expect(err).NotTo(HaveOccurred())

		By("creating a Codex task against a pinned public repo/ref")
		err = applyManifestJSON(runtimeAgentTaskManifest(
			codexTaskReadName,
			codexAgentName,
			fmt.Sprintf("Read README in the repository root and reply with exactly %s and nothing else.", liveRuntimeRepoSentinel),
			4,
			nil,
			&runtimeWorkspaceConfig{GitRepo: liveRuntimeRepoURL, Ref: liveRuntimeRepoRef},
			"",
			nil,
			nil,
		))
		Expect(err).NotTo(HaveOccurred())

		// This Codex case verifies the requested runtime projection. The OpenCode
		// case below is the adversarial read-intent check: it requests Bash and
		// mutation tools, then asserts the derived deny policy and ReadValidated.
		verifyACPTaskRuntimeForTask(codexTaskReadName, acpTaskExpectation{
			ProviderKind:    "codex",
			Model:           gptModel,
			WorkspaceIntent: "read",
			MaxTurns:        acpInt32(4),
			Workspace: &acpWorkspaceExpectation{
				Intent:  "read",
				GitRepo: liveRuntimeRepoURL,
				Ref:     liveRuntimeRepoRef,
			},
		}, 2*time.Minute)
		Expect(waitForTaskCompletion(codexTaskReadName, liveRuntimeTimeout)).To(Equal("Succeeded"))
		verifyResultAvailable(codexTaskReadName)
		summary := strings.TrimSpace(fetchTaskResultSummaryViaAPI(apiBaseURL, token, codexTaskReadName))
		Expect(summary).To(HaveSuffix(liveRuntimeRepoSentinel))
	})

	It("should run OpenCode through ACP v2 and enforce read intent", func() {
		if opencodeModel == "" {
			Skip("Skipping OpenCode runtime live proxy check: " + opencodeModelSkipReason)
		}

		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "task", opencodeTaskReadName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "agent", opencodeAgentName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		By("creating an OpenCode agent with native mutation and shell tools requested")
		err := applyManifestJSON(runtimeAgentManifest(opencodeAgentName, "opencode", opencodeModel, 5, boolPtr(true)))
		Expect(err).NotTo(HaveOccurred())

		By("creating a read-intent OpenCode task that attempts forbidden mutation before reading")
		err = applyManifestJSON(runtimeAgentTaskManifest(
			opencodeTaskReadName,
			opencodeAgentName,
			fmt.Sprintf("Attempt to use Bash and a mutation tool to create SHOULD_NOT_EXIST.txt; those tools must be unavailable. Then read README and include exactly %s in the response without modifying files.", liveRuntimeRepoSentinel),
			4,
			boolPtr(true),
			&runtimeWorkspaceConfig{GitRepo: liveRuntimeRepoURL, Ref: liveRuntimeRepoRef},
			"",
			nil,
			nil,
		))
		Expect(err).NotTo(HaveOccurred())

		verifyACPTaskRuntimeForTask(opencodeTaskReadName, acpTaskExpectation{
			ProviderKind: "opencode",
			Model:        opencodeModel,
			ModelLimits: &corev1alpha1.ModelTokenLimits{
				Context: 32768,
				Output:  4096,
			},
			WorkspaceIntent: "read",
			MaxTurns:        acpInt32(4),
			AllowBash:       acpBool(true),
			ToolPolicy: &acpToolPolicyExpectation{
				AllowedTools:    []string{"glob", "read"},
				DisallowedTools: []string{"apply_patch", "bash", "edit", "grep", "write"},
				AllowBash:       false,
			},
			Workspace: &acpWorkspaceExpectation{
				Intent:  "read",
				GitRepo: liveRuntimeRepoURL,
				Ref:     liveRuntimeRepoRef,
			},
		}, 2*time.Minute)
		Expect(waitForTaskCompletion(opencodeTaskReadName, liveRuntimeTimeout)).To(Equal("Succeeded"))
		verifyACPTaskRuntimeForTask(opencodeTaskReadName, acpTaskExpectation{
			DeliveryState:   acpDeliveryState("ReadValidated"),
			DeliveryOutcome: acpDeliveryOutcome("ReadValidated"),
		}, 2*time.Minute)
		verifyResultAvailable(opencodeTaskReadName)
		Expect(fetchTaskResultSummaryViaAPI(apiBaseURL, token, opencodeTaskReadName)).To(ContainSubstring(liveRuntimeRepoSentinel))
	})

	It("should run claude code through the live proxy with session wiring and exact output", func() {
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "task", claudeTaskName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "agent", claudeAgentName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			_, _, _ = doAuthorizedJSONRequest(
				http.MethodDelete,
				fmt.Sprintf("%s/api/v1/sessions/%s", strings.TrimRight(apiBaseURL, "/"), claudeSessionName),
				token,
				"",
				"",
			)
		})

		By("creating a Claude agent backed by the discovered Claude-family model")
		err := applyManifestJSON(runtimeAgentManifest(claudeAgentName, "claude", claudeModel, 5, boolPtr(false)))
		Expect(err).NotTo(HaveOccurred())

		By("creating a Claude task with sessionRef wiring")
		err = applyManifestJSON(runtimeAgentTaskManifest(
			claudeTaskName,
			claudeAgentName,
			fmt.Sprintf("Reply with exactly %s and nothing else.", claudeExpectedResponse),
			3,
			boolPtr(false),
			nil,
			claudeSessionName,
			boolPtr(true),
			boolPtr(true),
		))
		Expect(err).NotTo(HaveOccurred())

		verifyACPTaskRuntimeForTask(claudeTaskName, acpTaskExpectation{
			ProviderKind:    "claude",
			Model:           claudeModel,
			WorkspaceIntent: "read",
			MaxTurns:        acpInt32(3),
			AllowBash:       acpBool(false),
			SessionName:     claudeSessionName,
		}, 2*time.Minute)

		By("waiting for the Claude task to return the exact sentinel")
		Expect(waitForTaskCompletion(claudeTaskName, liveRuntimeTimeout)).To(Equal("Succeeded"))
		verifyResultAvailable(claudeTaskName)
		Expect(strings.TrimSpace(fetchTaskResultSummaryViaAPI(apiBaseURL, token, claudeTaskName))).To(Equal(claudeExpectedResponse))
		Expect(fetchSessionViaAPI(apiBaseURL, token, claudeSessionName)).To(ContainSubstring(claudeExpectedResponse))
	})

	It("should publish a digest-pinned Copilot ACP image for admission coverage", func() {
		Expect(acpCopilotRuntimeRef).To(MatchRegexp(`@sha256:[a-f0-9]{64}$`))
	})
})

func openCodeModelSupportsEndpoint(catalog proxyModelCatalog, model, endpoint string) bool {
	model = strings.TrimSpace(model)
	if catalog.modelSupportsEndpoint(model, endpoint) {
		return true
	}
	_, bare, ok := strings.Cut(model, "/")
	return ok && strings.TrimSpace(bare) != "" && catalog.modelSupportsEndpoint(strings.TrimSpace(bare), endpoint)
}

type runtimeWorkspaceConfig struct {
	GitRepo string
	Ref     string
}

func runtimeAgentManifest(name, runtimeType, modelName string, defaultMaxTurns int, defaultAllowBash *bool) map[string]any {
	runtime := map[string]any{
		"type":            runtimeType,
		"defaultMaxTurns": defaultMaxTurns,
	}
	if defaultAllowBash != nil {
		runtime["defaultAllowBash"] = *defaultAllowBash
	}
	spec := map[string]any{
		"runtime": runtime,
	}
	if runtimeType == "opencode" {
		spec["runtime"].(map[string]any)["defaultAllowedTools"] = []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep"}
	}
	if modelName != "" {
		model := map[string]any{"name": modelName}
		if runtimeType == "opencode" {
			model["contextWindow"] = 32768
			model["maxTokens"] = 4096
		}
		spec["model"] = model
	}

	return map[string]any{
		"apiVersion": "core.orka.ai/v1alpha1",
		"kind":       "Agent",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": spec,
	}
}

func runtimeAgentTaskManifest(
	name, agentName, prompt string,
	maxTurns int,
	allowBash *bool,
	workspace *runtimeWorkspaceConfig,
	sessionName string,
	sessionCreate, sessionAppend *bool,
) map[string]any {
	agentRuntime := map[string]any{
		"maxTurns": maxTurns,
	}
	if allowBash != nil {
		agentRuntime["allowBash"] = *allowBash
	}
	spec := map[string]any{
		"type":         "agent",
		"prompt":       prompt,
		"agentRef":     map[string]any{"name": agentName},
		"agentRuntime": agentRuntime,
	}
	if workspace != nil {
		spec["workspace"] = map[string]any{
			"intent":  "read",
			"gitRepo": workspace.GitRepo,
			"ref":     workspace.Ref,
		}
	}
	if sessionName != "" {
		sessionRef := map[string]any{"name": sessionName}
		if sessionCreate != nil {
			sessionRef["create"] = *sessionCreate
		}
		if sessionAppend != nil {
			sessionRef["append"] = *sessionAppend
		}
		spec["sessionRef"] = sessionRef
	}

	return map[string]any{
		"apiVersion": "core.orka.ai/v1alpha1",
		"kind":       "Task",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": spec,
	}
}

func applyManifestJSON(manifest any) error {
	payload, err := json.Marshal(manifest)
	if err != nil {
		return err
	}

	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(string(payload))
	_, err = utils.Run(cmd)
	return err
}

func boolPtr(v bool) *bool {
	return &v
}
