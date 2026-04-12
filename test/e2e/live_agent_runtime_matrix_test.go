//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/sozercan/orka/test/utils"
	workercommon "github.com/sozercan/orka/workers/common"
)

const (
	liveRuntimeRepoURL      = "https://github.com/octocat/Hello-World.git"
	liveRuntimeRepoRef      = "7fd1a60b01f91b314f59955a4e4d4e80d8edf11d"
	liveRuntimeRepoSentinel = "Hello World!"
	liveRuntimeTimeout      = 8 * time.Minute
)

var _ = Describe("Live Agent Runtime Matrix", Ordered, func() {
	const (
		codexPriorSecretName   = "e2e-live-runtime-prior-secret"
		codexPriorAgentName    = "e2e-live-runtime-prior-agent"
		codexSecretName        = "e2e-live-runtime-codex-secret"
		codexAgentName         = "e2e-live-runtime-codex-agent"
		codexTaskWriteName     = "e2e-live-runtime-codex-write"
		codexTaskReadName      = "e2e-live-runtime-codex-read"
		codexNonce             = "CODEX_LIVE_RUNTIME_NONCE_1"
		codexNonceFile         = "codex-live-nonce.txt"
		claudeSecretName       = "e2e-live-runtime-claude-secret"
		claudeAgentName        = "e2e-live-runtime-claude-agent"
		claudeTaskName         = "e2e-live-runtime-claude-task"
		claudeExpectedResponse = "ORKA_LIVE_CLAUDE_OK"
		copilotAgentName       = "e2e-live-runtime-copilot-agent"
		copilotTaskName        = "e2e-live-runtime-copilot-task"
	)

	var (
		apiBaseURL         string
		cancelControllerPF context.CancelFunc
		controllerPFCmd    *exec.Cmd
		token              string
		gptModel           string
		claudeModel        string
		geminiModel        string
		claudeSessionName  string
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
			liveCopilotProxyServiceNamespace(),
			liveCopilotProxyServiceName(),
			liveCopilotProxyServicePort(),
		)
		Expect(ready.Status).To(Equal("ready"))
		Expect(ready.Error).To(BeEmpty())

		By("discovering live runtime models by family")
		gptModel = discoverPreferredProxyModelViaServiceProxy(
			liveCopilotProxyServiceNamespace(),
			liveCopilotProxyServiceName(),
			liveCopilotProxyServicePort(),
			[]string{"gpt-5.3-codex", "gpt-5.2-codex", "gpt-5.4", "gpt-5.2"},
			"gpt-",
		)
		claudeModel = discoverPreferredProxyModelViaServiceProxy(
			liveCopilotProxyServiceNamespace(),
			liveCopilotProxyServiceName(),
			liveCopilotProxyServicePort(),
			[]string{"claude-sonnet-4.6", "claude-sonnet-4.5", "claude-sonnet-4"},
			"claude-",
		)
		geminiModel = discoverPreferredProxyModelViaServiceProxy(
			liveCopilotProxyServiceNamespace(),
			liveCopilotProxyServiceName(),
			liveCopilotProxyServicePort(),
			[]string{"gemini-2.5-pro", "gemini-3.1-pro-preview", "gemini-3-flash-preview"},
			"gemini-",
		)

		claudeSessionName = fmt.Sprintf("e2e-live-runtime-claude-%d", time.Now().UnixNano())
	})

	AfterAll(func() {
		stopPortForward(cancelControllerPF, controllerPFCmd)
	})

	AfterEach(func() {
		dumpDebugInfo(
			codexTaskWriteName,
			codexTaskReadName,
			claudeTaskName,
			copilotTaskName,
		)
		dumpLiveCopilotProxyDebugInfo()
	})

	It("should let codex consume priorTaskRef state on a git workspace", func() {
		DeferCleanup(func() {
			for _, resource := range []struct {
				kind string
				name string
			}{
				{"task", codexTaskReadName},
				{"task", codexTaskWriteName},
				{"agent", codexPriorAgentName},
				{"secret", codexPriorSecretName},
				{"agent", codexAgentName},
				{"secret", codexSecretName},
			} {
				cmd := exec.Command("kubectl", "delete", resource.kind, resource.name, "-n", namespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			}
		})

		By("creating a Claude runtime secret that routes Anthropic traffic through the live proxy")
		err := createK8sSecret(codexPriorSecretName, namespace, map[string]string{
			"ANTHROPIC_API_KEY":  "dummy-live-prior-key",
			"ANTHROPIC_BASE_URL": liveCopilotProxyRootURL(),
		})
		Expect(err).NotTo(HaveOccurred())

		By("creating a Claude agent to generate the priorTaskRef workspace diff")
		err = applyManifestJSON(runtimeAgentManifest(codexPriorAgentName, "claude", codexPriorSecretName, claudeModel, 5, true))
		Expect(err).NotTo(HaveOccurred())

		By("creating the prior task that writes the nonce file into the pinned repository")
		err = applyManifestJSON(runtimeAgentTaskManifest(
			codexTaskWriteName,
			codexPriorAgentName,
			fmt.Sprintf("Use the simplest possible edit to create %s in the repository root containing exactly %s followed by a newline. Reply with exactly CREATED and nothing else.", codexNonceFile, codexNonce),
			6,
			boolPtr(true),
			&runtimeWorkspaceConfig{GitRepo: liveRuntimeRepoURL, Ref: liveRuntimeRepoRef},
			"",
			"",
			nil,
			nil,
		))
		Expect(err).NotTo(HaveOccurred())

		By("verifying the prior task has the expected workspace and runtime wiring")
		verifyJobCreatedForTask(codexTaskWriteName, 2*time.Minute)
		runtimeAssertJobBasics(
			codexTaskWriteName,
			claudeWorkerImage,
			map[string]string{
				"ORKA_MODEL":      claudeModel,
				"ORKA_MAX_TURNS":  "6",
				"ORKA_ALLOW_BASH": "true",
				"ORKA_GIT_REPO":   liveRuntimeRepoURL,
				"ORKA_GIT_REF":    liveRuntimeRepoRef,
			},
			codexPriorSecretName,
			nil,
			nil,
		)

		By("waiting for the prior task to succeed and emit a structured diff result")
		Expect(waitForTaskCompletion(codexTaskWriteName, liveRuntimeTimeout)).To(Equal("Succeeded"))
		verifyResultAvailable(codexTaskWriteName)
		firstResult := workercommon.ParseStructuredResult(fetchTaskResultViaAPI(apiBaseURL, token, codexTaskWriteName))
		Expect(strings.TrimSpace(firstResult.Summary)).To(ContainSubstring("CREATED"))
		Expect(strings.TrimSpace(firstResult.Diff)).NotTo(BeEmpty(), "prior task should produce a diff for priorTaskRef")
		Expect(firstResult.Files).To(ContainElement(codexNonceFile))

		By("creating a Codex runtime secret that routes OpenAI traffic through the live proxy")
		err = createK8sSecret(codexSecretName, namespace, map[string]string{
			"OPENAI_API_KEY":  "dummy-live-codex-key",
			"OPENAI_BASE_URL": e2eLiveCopilotProxyBaseURL,
		})
		Expect(err).NotTo(HaveOccurred())

		By("creating a Codex agent backed by the discovered GPT-family model")
		err = applyManifestJSON(runtimeAgentManifest(codexAgentName, "codex", codexSecretName, gptModel, 5, true))
		Expect(err).NotTo(HaveOccurred())

		By("creating the Codex task that reads the file through priorTaskRef")
		err = applyManifestJSON(runtimeAgentTaskManifest(
			codexTaskReadName,
			codexAgentName,
			fmt.Sprintf("Read %s from the repository root and reply with exactly %s and nothing else.", codexNonceFile, codexNonce),
			3,
			boolPtr(true),
			&runtimeWorkspaceConfig{GitRepo: liveRuntimeRepoURL, Ref: liveRuntimeRepoRef},
			codexTaskWriteName,
			"",
			nil,
			nil,
		))
		Expect(err).NotTo(HaveOccurred())

		By("verifying the Codex job carries priorTaskRef and workspace wiring")
		verifyJobCreatedForTask(codexTaskReadName, 2*time.Minute)
		runtimeAssertJobBasics(
			codexTaskReadName,
			codexWorkerImage,
			map[string]string{
				"ORKA_MODEL":                gptModel,
				"ORKA_MAX_TURNS":            "3",
				"ORKA_ALLOW_BASH":           "true",
				"ORKA_GIT_REPO":             liveRuntimeRepoURL,
				"ORKA_GIT_REF":              liveRuntimeRepoRef,
				"ORKA_PRIOR_TASK":           codexTaskWriteName,
				"ORKA_PRIOR_TASK_NAMESPACE": namespace,
			},
			codexSecretName,
			nil,
			nil,
		)

		By("waiting for the Codex task to return the exact nonce from the prior diff")
		Expect(waitForTaskCompletion(codexTaskReadName, liveRuntimeTimeout)).To(Equal("Succeeded"))
		verifyResultAvailable(codexTaskReadName)
		Expect(strings.TrimSpace(fetchTaskResultSummaryViaAPI(apiBaseURL, token, codexTaskReadName))).To(Equal(codexNonce))
	})

	It("should run claude code through the live proxy with session wiring and exact output", func() {
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "task", claudeTaskName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "agent", claudeAgentName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "secret", claudeSecretName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			_, _, _ = doAuthorizedJSONRequest(
				http.MethodDelete,
				fmt.Sprintf("%s/api/v1/sessions/%s", strings.TrimRight(apiBaseURL, "/"), claudeSessionName),
				token,
				"",
				"",
			)
		})

		By("creating a Claude runtime secret that routes Anthropic traffic through the live proxy")
		err := createK8sSecret(claudeSecretName, namespace, map[string]string{
			"ANTHROPIC_API_KEY":  "dummy-live-claude-key",
			"ANTHROPIC_BASE_URL": liveCopilotProxyRootURL(),
		})
		Expect(err).NotTo(HaveOccurred())

		By("creating a Claude agent backed by the discovered Claude-family model")
		err = applyManifestJSON(runtimeAgentManifest(claudeAgentName, "claude", claudeSecretName, claudeModel, 5, false))
		Expect(err).NotTo(HaveOccurred())

		By("creating a Claude task with sessionRef wiring")
		err = applyManifestJSON(runtimeAgentTaskManifest(
			claudeTaskName,
			claudeAgentName,
			fmt.Sprintf("Reply with exactly %s and nothing else.", claudeExpectedResponse),
			3,
			boolPtr(false),
			nil,
			"",
			claudeSessionName,
			boolPtr(true),
			boolPtr(true),
		))
		Expect(err).NotTo(HaveOccurred())

		By("verifying the Claude job wiring")
		verifyJobCreatedForTask(claudeTaskName, 2*time.Minute)
		runtimeAssertJobBasics(
			claudeTaskName,
			claudeWorkerImage,
			map[string]string{
				"ORKA_MODEL":        claudeModel,
				"ORKA_MAX_TURNS":    "3",
				"ORKA_SESSION_NAME": claudeSessionName,
			},
			claudeSecretName,
			[]string{"ORKA_ALLOW_BASH"},
			[]string{"fetch-session"},
		)

		By("waiting for the Claude task to return the exact sentinel")
		Expect(waitForTaskCompletion(claudeTaskName, liveRuntimeTimeout)).To(Equal("Succeeded"))
		verifyResultAvailable(claudeTaskName)
		Expect(strings.TrimSpace(fetchTaskResultSummaryViaAPI(apiBaseURL, token, claudeTaskName))).To(Equal(claudeExpectedResponse))
		Expect(fetchSessionViaAPI(apiBaseURL, token, claudeSessionName)).To(ContainSubstring(claudeExpectedResponse))
	})

	It("should run copilot against a pinned public checkout with a Gemini-family model", func() {
		skipIfNoKey("E2E_GITHUB_TOKEN")

		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "task", copilotTaskName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "agent", copilotAgentName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		By("creating a Copilot agent backed by the discovered Gemini-family model")
		err := applyManifestJSON(runtimeAgentManifest(copilotAgentName, "copilot", "e2e-github-secret", geminiModel, 5, true))
		Expect(err).NotTo(HaveOccurred())

		By("creating a Copilot task against a pinned public repo/ref")
		err = applyManifestJSON(runtimeAgentTaskManifest(
			copilotTaskName,
			copilotAgentName,
			fmt.Sprintf("Read the README file in the repository root and reply with exactly %s and nothing else.", liveRuntimeRepoSentinel),
			4,
			boolPtr(true),
			&runtimeWorkspaceConfig{GitRepo: liveRuntimeRepoURL, Ref: liveRuntimeRepoRef},
			"",
			"",
			nil,
			nil,
		))
		Expect(err).NotTo(HaveOccurred())

		By("verifying the Copilot job wiring")
		verifyJobCreatedForTask(copilotTaskName, 2*time.Minute)
		runtimeAssertJobBasics(
			copilotTaskName,
			copilotWorkerImage,
			map[string]string{
				"ORKA_MODEL":      geminiModel,
				"ORKA_MAX_TURNS":  "4",
				"ORKA_ALLOW_BASH": "true",
				"ORKA_GIT_REPO":   liveRuntimeRepoURL,
				"ORKA_GIT_REF":    liveRuntimeRepoRef,
			},
			"e2e-github-secret",
			nil,
			nil,
		)

		By("waiting for the Copilot task to return the exact README sentinel")
		Expect(waitForTaskCompletion(copilotTaskName, liveRuntimeTimeout)).To(Equal("Succeeded"))
		verifyResultAvailable(copilotTaskName)
		Expect(strings.TrimSpace(fetchTaskResultSummaryViaAPI(apiBaseURL, token, copilotTaskName))).To(Equal(liveRuntimeRepoSentinel))
	})
})

type runtimeWorkspaceConfig struct {
	GitRepo string
	Ref     string
}

func runtimeAgentManifest(name, runtimeType, secretName, modelName string, defaultMaxTurns int, defaultAllowBash bool) map[string]any {
	spec := map[string]any{
		"runtime": map[string]any{
			"type":             runtimeType,
			"defaultMaxTurns":  defaultMaxTurns,
			"defaultAllowBash": defaultAllowBash,
		},
	}
	if secretName != "" {
		spec["secretRef"] = map[string]any{"name": secretName}
	}
	if modelName != "" {
		spec["model"] = map[string]any{"name": modelName}
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
	priorTaskName string,
	sessionName string,
	sessionCreate, sessionAppend *bool,
) map[string]any {
	agentRuntime := map[string]any{
		"maxTurns": maxTurns,
	}
	if allowBash != nil {
		agentRuntime["allowBash"] = *allowBash
	}
	if workspace != nil {
		agentRuntime["workspace"] = map[string]any{
			"gitRepo": workspace.GitRepo,
			"ref":     workspace.Ref,
		}
	}

	spec := map[string]any{
		"type":         "agent",
		"prompt":       prompt,
		"agentRef":     map[string]any{"name": agentName},
		"agentRuntime": agentRuntime,
	}
	if priorTaskName != "" {
		spec["priorTaskRef"] = map[string]any{"name": priorTaskName}
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

func runtimeAssertJobBasics(
	taskName, expectedImage string,
	requiredEnv map[string]string,
	expectedSecret string,
	forbiddenEnvKeys []string,
	expectedInitContainers []string,
) {
	snapshot := runtimeFetchTaskJobSnapshot(taskName)
	Expect(snapshot.Containers).NotTo(BeEmpty())

	container := snapshot.Containers[0]
	Expect(container.Image).To(Equal(expectedImage))

	envMap := runtimeEnvMapFromContainer(container)
	for key, value := range requiredEnv {
		Expect(envMap).To(HaveKeyWithValue(key, value))
	}
	for _, key := range forbiddenEnvKeys {
		Expect(envMap).NotTo(HaveKey(key))
	}

	if expectedSecret != "" {
		Expect(runtimeSecretNamesFromEnvFrom(container.EnvFrom)).To(ContainElement(expectedSecret))
	}

	if len(expectedInitContainers) > 0 {
		initNames := make([]string, 0, len(snapshot.InitContainers))
		for _, initContainer := range snapshot.InitContainers {
			initNames = append(initNames, initContainer.Name)
		}
		for _, initName := range expectedInitContainers {
			Expect(initNames).To(ContainElement(initName))
		}
	}
}

func runtimeFetchTaskJobSnapshot(taskName string) runtimeJobPodSpecSnapshot {
	var snapshot runtimeJobPodSpecSnapshot
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "jobs",
			"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
			"-o", "json",
			"-n", namespace,
		)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "Failed to get Job for task %s", taskName)

		var payload runtimeJobListSnapshot
		g.Expect(json.Unmarshal([]byte(output), &payload)).To(Succeed())
		g.Expect(payload.Items).NotTo(BeEmpty(), "Job list should not be empty")

		snapshot = payload.Items[0].Spec.Template.Spec
		g.Expect(snapshot.Containers).NotTo(BeEmpty(), "Job should have at least one container")
	}, 2*time.Minute, time.Second).Should(Succeed())

	return snapshot
}

func runtimeEnvMapFromContainer(container runtimeJobContainerSnapshot) map[string]string {
	envMap := make(map[string]string, len(container.Env))
	for _, envVar := range container.Env {
		envMap[envVar.Name] = envVar.Value
	}
	return envMap
}

func runtimeSecretNamesFromEnvFrom(envFrom []runtimeJobEnvFromSnapshot) []string {
	names := make([]string, 0, len(envFrom))
	for _, source := range envFrom {
		if source.SecretRef != nil && strings.TrimSpace(source.SecretRef.Name) != "" {
			names = append(names, strings.TrimSpace(source.SecretRef.Name))
		}
	}
	return names
}

type runtimeJobSecretRefSnapshot struct {
	Name string `json:"name"`
}

type runtimeJobEnvFromSnapshot struct {
	SecretRef *runtimeJobSecretRefSnapshot `json:"secretRef,omitempty"`
}

type runtimeJobContainerSnapshot struct {
	Name    string                      `json:"name"`
	Image   string                      `json:"image"`
	Env     []envVar                    `json:"env"`
	EnvFrom []runtimeJobEnvFromSnapshot `json:"envFrom"`
}

type runtimeJobPodSpecSnapshot struct {
	Containers     []runtimeJobContainerSnapshot `json:"containers"`
	InitContainers []runtimeJobContainerSnapshot `json:"initContainers"`
}

type runtimeJobTemplateSnapshot struct {
	Spec runtimeJobPodSpecSnapshot `json:"spec"`
}

type runtimeJobSpecSnapshot struct {
	Template runtimeJobTemplateSnapshot `json:"template"`
}

type runtimeJobListSnapshot struct {
	Items []struct {
		Spec runtimeJobSpecSnapshot `json:"spec"`
	} `json:"items"`
}

func boolPtr(v bool) *bool {
	return &v
}
