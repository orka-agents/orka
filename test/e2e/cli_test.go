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
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/sozercan/orka/test/utils"
)

var _ = Describe("Orka CLI binary", Ordered, func() {
	var (
		apiBaseURL     string
		portForwardCmd *exec.Cmd
		cancelPF       context.CancelFunc
		token          string
		home           string
		suffix         string
	)

	BeforeAll(func() {
		By("building the orka CLI binary")
		buildOrkaCLI()

		By("setting up a controller API port-forward for CLI commands")
		var err error
		apiBaseURL, cancelPF, portForwardCmd, err = startControllerAPIPortForward(18110)
		Expect(err).NotTo(HaveOccurred(), "Failed to start controller API port-forward")

		By("creating an isolated CLI home with service-account credentials")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())
		home = newIsolatedCLIHome(apiBaseURL, token)
		suffix = fmt.Sprintf("%d", time.Now().UnixNano())
	})

	AfterAll(func() {
		By("stopping CLI controller API port-forward")
		stopPortForward(cancelPF, portForwardCmd)
	})

	It("authenticates from isolated config and lists model catalogs", func() {
		By("validating the configured service-account token")
		validate := runOrka(home, "auth", "validate")
		expectOrkaSuccess(validate, token)
		validateJSON := expectJSONObject(validate.Stdout)
		Expect(validateJSON["authenticated"]).To(Equal(true))

		By("showing a sanitized identity")
		whoami := runOrka(home, "auth", "whoami", "-o", "json")
		expectOrkaSuccess(whoami, token)
		identity := expectJSONObject(whoami.Stdout)
		Expect(identity["authenticated"]).To(Equal(true))
		Expect(identity).NotTo(HaveKey("token"))
		Expect(identity).NotTo(HaveKey("bearerToken"))

		By("listing OpenAI-compatible models as JSON")
		openAIModels := runOrka(home, "models", "list", "--compat", "openai", "-o", "json")
		expectOrkaSuccess(openAIModels, token)
		expectJSONObject(openAIModels.Stdout)

		By("listing Anthropic-compatible models as JSON")
		anthropicModels := runOrka(home, "models", "list", "--compat", "anthropic", "-o", "json")
		expectOrkaSuccess(anthropicModels, token)
		expectJSONObject(anthropicModels.Stdout)

		By("rejecting an invalid configured token without printing it")
		invalidToken := "invalid-cli-e2e-token-" + suffix
		invalidHome := newIsolatedCLIHome(apiBaseURL, invalidToken)
		invalid := runOrka(invalidHome, "auth", "validate")
		expectOrkaFailure(invalid, invalidToken)
	})

	It("runs a container task workflow through the compiled CLI", func() {
		taskName := "e2e-cli-task-" + suffix
		failedTaskName := "e2e-cli-fail-" + suffix
		mismatchTaskName := "e2e-cli-ns-mismatch-" + suffix
		tmpDir := GinkgoT().TempDir()

		DeferCleanup(deleteK8sResource, "task", taskName)
		DeferCleanup(deleteK8sResource, "task", failedTaskName)
		DeferCleanup(deleteK8sResource, "task", mismatchTaskName)

		By("creating a container task from a manifest without metadata.namespace")
		taskManifest := writeTempManifest(tmpDir, "task.yaml", fmt.Sprintf(`
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: %s
spec:
  type: container
  image: busybox:latest
  command: ["sh", "-c"]
  args: ["echo cli-e2e-result"]
`, taskName))
		created := runOrka(home, "task", "create", "-f", taskManifest)
		expectOrkaSuccess(created, token)
		Expect(created.Stdout).To(ContainSubstring(taskName))

		By("listing tasks as JSON and table output")
		listJSON := runOrka(home, "task", "list", "--limit", "200", "-o", "json")
		expectOrkaSuccess(listJSON, token)
		expectListContainsName(expectJSONOutput(listJSON.Stdout), taskName)

		listTable := runOrka(home, "task", "list", "--limit", "200", "-o", "table")
		expectOrkaSuccess(listTable, token)
		Expect(listTable.Stdout).To(ContainSubstring("NAME"))
		Expect(listTable.Stdout).To(ContainSubstring(taskName))

		By("getting the task as YAML and verifying CLI namespace injection")
		getYAML := runOrka(home, "task", "get", taskName, "-o", "yaml")
		expectOrkaSuccess(getYAML, token)
		taskYAML := expectYAMLOutput(getYAML.Stdout)
		Expect(nestedStringFromMap(taskYAML, "metadata", "namespace")).To(Equal(namespace))

		By("overriding a wrong configured namespace with --namespace")
		wrongNamespaceHome := newIsolatedCLIHomeWithNamespace(apiBaseURL, "default", token)
		withoutOverride := runOrka(wrongNamespaceHome, "task", "get", taskName, "-o", "json")
		expectOrkaFailure(withoutOverride, token)
		withOverride := runOrka(wrongNamespaceHome, "--namespace", namespace, "task", "get", taskName, "-o", "json")
		expectOrkaSuccess(withOverride, token)
		withOverrideJSON := expectJSONObject(withOverride.Stdout)
		Expect(nestedStringFromMap(withOverrideJSON, "metadata", "namespace")).To(Equal(namespace))

		By("rejecting a manifest namespace mismatch before creating a task")
		mismatchManifest := writeTempManifest(tmpDir, "namespace-mismatch.yaml", fmt.Sprintf(`
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: %s
  namespace: default
spec:
  type: container
  image: busybox:latest
  command: ["echo"]
  args: ["must-not-run"]
`, mismatchTaskName))
		mismatch := runOrka(home, "--namespace", namespace, "task", "create", "-f", mismatchManifest)
		expectOrkaFailure(mismatch, token)
		Expect(mismatch.Stderr + mismatch.Stdout).To(ContainSubstring("manifest namespace"))
		Expect(k8sResourceExists("task", mismatchTaskName)).To(BeFalse())

		By("waiting for the task to succeed through the CLI")
		wait := runOrkaWithTimeout(4*time.Minute, home, "task", "wait", taskName, "--timeout", "3m")
		expectOrkaSuccess(wait, token)
		Expect(wait.Stdout).To(ContainSubstring("succeeded"))

		By("reading task result text and JSON through the CLI")
		var result cliResult
		Eventually(func() error {
			result = runOrka(home, "task", "result", taskName)
			if result.Err != nil {
				return fmt.Errorf("task result failed: %v\n%s", result.Err, redactedCLIOutput(result, token))
			}
			if !strings.Contains(result.Stdout, "cli-e2e-result") {
				return fmt.Errorf("task result missing sentinel: %s", truncateForLog(result.Stdout, 512))
			}
			return nil
		}, 45*time.Second, 2*time.Second).Should(Succeed())
		expectNoSensitiveOutput(result, token)

		resultJSON := runOrka(home, "task", "result", taskName, "-o", "json")
		expectOrkaSuccess(resultJSON, token)
		resultPayload := expectJSONObject(resultJSON.Stdout)
		Expect(fmt.Sprint(resultPayload["result"])).To(ContainSubstring("cli-e2e-result"))

		By("checking child-task and plan endpoints through the CLI")
		children := runOrka(home, "task", "children", taskName, "-o", "json")
		expectOrkaSuccess(children, token)
		expectJSONOutput(children.Stdout)

		plan := runOrka(home, "task", "plan", taskName, "-o", "json")
		expectOrkaFailure(plan, token)

		workspace := runOrka(home, "workspace", "status", taskName, "-o", "json")
		expectOrkaSuccess(workspace, token)
		workspacePayload := expectJSONObject(workspace.Stdout)
		Expect(workspacePayload["task"]).To(Equal(taskName))
		Expect(workspacePayload["namespace"]).To(Equal(namespace))

		By("returning nonzero when waiting for a failed task")
		failedManifest := writeTempManifest(tmpDir, "failed-task.yaml", fmt.Sprintf(`
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: %s
spec:
  type: container
  image: busybox:latest
  command: ["false"]
`, failedTaskName))
		failedCreated := runOrka(home, "task", "create", "-f", failedManifest)
		expectOrkaSuccess(failedCreated, token)
		failedWait := runOrkaWithTimeout(3*time.Minute, home, "task", "wait", failedTaskName, "--timeout", "2m")
		expectOrkaFailure(failedWait, token)
		Expect(failedWait.Stderr + failedWait.Stdout).To(ContainSubstring("Failed"))

		By("deleting tasks through the CLI")
		deleted := runOrka(home, "task", "delete", taskName)
		expectOrkaSuccess(deleted, token)
		failedDeleted := runOrka(home, "task", "delete", failedTaskName)
		expectOrkaSuccess(failedDeleted, token)
	})

	It("performs provider, agent, tool, skill, and secret workflows through the CLI", func() {
		tmpDir := GinkgoT().TempDir()
		providerName := "e2e-cli-provider-" + suffix
		providerSecretName := "e2e-cli-provider-secret-" + suffix
		agentName := "e2e-cli-agent-" + suffix
		toolName := "e2e-cli-tool-" + suffix
		skillName := "e2e-cli-skill-" + suffix
		secretSentinel := "cli-e2e-secret-sentinel-" + suffix

		DeferCleanup(deleteK8sResource, "provider", providerName)
		DeferCleanup(deleteK8sResource, "secret", providerSecretName)
		DeferCleanup(deleteK8sResource, "agent", agentName)
		DeferCleanup(deleteK8sResource, "tool", toolName)
		DeferCleanup(deleteK8sResource, "skill", skillName)

		By("creating a test secret without logging its value")
		Expect(createK8sSecret(providerSecretName, namespace, map[string]string{"api-key": secretSentinel})).To(Succeed())

		By("creating, reading, listing, updating, and deleting a provider")
		providerManifest := writeTempManifest(tmpDir, "provider.yaml", fmt.Sprintf(`
apiVersion: core.orka.ai/v1alpha1
kind: Provider
metadata:
  name: %s
spec:
  type: openai
  secretRef:
    name: %s
    key: api-key
  defaultModel: gpt-cli-e2e
`, providerName, providerSecretName))
		providerUpdatedManifest := writeTempManifest(tmpDir, "provider-updated.yaml", fmt.Sprintf(`
apiVersion: core.orka.ai/v1alpha1
kind: Provider
metadata:
  name: %s
spec:
  type: openai
  secretRef:
    name: %s
    key: api-key
  defaultModel: gpt-cli-e2e-updated
`, providerName, providerSecretName))
		expectOrkaSuccess(runOrka(home, "provider", "create", "-f", providerManifest), token, secretSentinel)
		providerGet := runOrka(home, "provider", "get", providerName, "-o", "json")
		expectOrkaSuccess(providerGet, token, secretSentinel)
		Expect(nestedStringFromMap(expectJSONObject(providerGet.Stdout), "metadata", "name")).To(Equal(providerName))
		providerList := runOrka(home, "provider", "list", "-o", "yaml")
		expectOrkaSuccess(providerList, token, secretSentinel)
		expectListContainsName(expectYAMLOutput(providerList.Stdout), providerName)
		expectOrkaSuccess(runOrka(home, "provider", "update", providerName, "-f", providerUpdatedManifest), token, secretSentinel)
		providerUpdated := expectJSONObject(runSuccessfulOrka(home, []string{token, secretSentinel}, "provider", "get", providerName, "-o", "json").Stdout)
		Expect(nestedStringFromMap(providerUpdated, "spec", "defaultModel")).To(Equal("gpt-cli-e2e-updated"))

		By("ensuring secret list output is metadata-only and redacted")
		secretList := runOrka(home, "secret", "list", "-o", "json")
		expectOrkaSuccess(secretList, token, secretSentinel)
		expectListContainsName(expectJSONObject(secretList.Stdout), providerSecretName)
		Expect(secretList.Stdout).NotTo(ContainSubstring("stringData"))
		Expect(secretList.Stdout).NotTo(ContainSubstring("\"data\""))

		By("creating, reading, listing, updating, and deleting an agent")
		agentManifest := writeTempManifest(tmpDir, "agent.yaml", fmt.Sprintf(`
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: %s
spec:
  runtime:
    type: claude
    defaultMaxTurns: 5
    defaultAllowBash: false
`, agentName))
		agentUpdatedManifest := writeTempManifest(tmpDir, "agent-updated.yaml", fmt.Sprintf(`
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: %s
spec:
  runtime:
    type: claude
    defaultMaxTurns: 7
    defaultAllowBash: false
`, agentName))
		expectOrkaSuccess(runOrka(home, "agent", "create", "-f", agentManifest), token)
		agentGet := runOrka(home, "agent", "get", agentName, "-o", "json")
		expectOrkaSuccess(agentGet, token)
		Expect(nestedStringFromMap(expectJSONObject(agentGet.Stdout), "metadata", "name")).To(Equal(agentName))
		agentList := runOrka(home, "agent", "list", "-o", "json")
		expectOrkaSuccess(agentList, token)
		expectListContainsName(expectJSONOutput(agentList.Stdout), agentName)
		expectOrkaSuccess(runOrka(home, "agent", "update", agentName, "-f", agentUpdatedManifest), token)
		agentUpdated := expectJSONObject(runSuccessfulOrka(home, []string{token}, "agent", "get", agentName, "-o", "json").Stdout)
		agentSpec, ok := agentUpdated["spec"].(map[string]any)
		Expect(ok).To(BeTrue(), "agent spec should be a JSON object")
		agentRuntime, ok := agentSpec["runtime"].(map[string]any)
		Expect(ok).To(BeTrue(), "agent runtime should be a JSON object")
		Expect(agentRuntime["defaultMaxTurns"]).To(BeNumerically("==", 7))

		By("creating, reading, listing, updating, and deleting a tool")
		toolManifest := writeTempManifest(tmpDir, "tool.yaml", fmt.Sprintf(`
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata:
  name: %s
spec:
  description: CLI e2e HTTP tool
  http:
    url: https://example.invalid/orka-cli-e2e
    method: POST
`, toolName))
		toolUpdatedManifest := writeTempManifest(tmpDir, "tool-updated.yaml", fmt.Sprintf(`
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata:
  name: %s
spec:
  description: CLI e2e HTTP tool updated
  http:
    url: https://example.invalid/orka-cli-e2e-updated
    method: POST
`, toolName))
		expectOrkaSuccess(runOrka(home, "tool", "create", "-f", toolManifest), token)
		toolGet := runOrka(home, "tool", "get", toolName, "-o", "json")
		expectOrkaSuccess(toolGet, token)
		Expect(nestedStringFromMap(expectJSONObject(toolGet.Stdout), "metadata", "name")).To(Equal(toolName))
		toolList := runOrka(home, "tool", "list", "-o", "json")
		expectOrkaSuccess(toolList, token)
		expectListContainsName(expectJSONObject(toolList.Stdout), toolName)
		expectOrkaSuccess(runOrka(home, "tool", "update", toolName, "-f", toolUpdatedManifest), token)
		toolUpdated := expectJSONObject(runSuccessfulOrka(home, []string{token}, "tool", "get", toolName, "-o", "json").Stdout)
		Expect(nestedStringFromMap(toolUpdated, "spec", "description")).To(Equal("CLI e2e HTTP tool updated"))

		By("initializing, validating, importing, reading, updating, and deleting a skill")
		skillDir := filepath.Join(tmpDir, "skill")
		expectOrkaSuccess(runOrka(home, "skill", "init", skillDir, "--name", skillName, "--description", "CLI e2e skill"), token)
		skillFile := filepath.Join(skillDir, "SKILL.md")
		expectOrkaSuccess(runOrka(home, "skill", "validate", skillFile), token)
		expectOrkaSuccess(runOrka(home, "skill", "import", skillFile, "--name", skillName), token)
		skillList := runOrka(home, "skill", "list", "-o", "json")
		expectOrkaSuccess(skillList, token)
		expectListContainsName(expectJSONOutput(skillList.Stdout), skillName)
		skillGet := runOrka(home, "skill", "get", skillName, "-o", "json")
		expectOrkaSuccess(skillGet, token)
		Expect(nestedStringFromMap(expectJSONObject(skillGet.Stdout), "metadata", "name")).To(Equal(skillName))
		skillContent := runOrka(home, "skill", "content", skillName)
		expectOrkaSuccess(skillContent, token)
		Expect(skillContent.Stdout).To(ContainSubstring("CLI e2e skill"))
		skillUpdateManifest := writeTempManifest(tmpDir, "skill-updated.yaml", fmt.Sprintf(`
apiVersion: core.orka.ai/v1alpha1
kind: Skill
metadata:
  name: %s
spec:
  description: CLI e2e skill updated
  content:
    inline: |
      # %s

      Updated CLI e2e skill content.
`, skillName, skillName))
		expectOrkaSuccess(runOrka(home, "skill", "update", skillName, "-f", skillUpdateManifest), token)
		skillUpdatedContent := runOrka(home, "skill", "content", skillName)
		expectOrkaSuccess(skillUpdatedContent, token)
		Expect(skillUpdatedContent.Stdout).To(ContainSubstring("Updated CLI e2e skill content"))

		By("deleting created resources through the CLI")
		expectOrkaSuccess(runOrka(home, "skill", "delete", skillName), token)
		expectOrkaSuccess(runOrka(home, "tool", "delete", toolName), token)
		expectOrkaSuccess(runOrka(home, "agent", "delete", agentName), token)
		expectOrkaSuccess(runOrka(home, "provider", "delete", providerName), token, secretSentinel)
	})
})

func runSuccessfulOrka(home string, forbidden []string, args ...string) cliResult {
	GinkgoHelper()
	result := runOrka(home, args...)
	expectOrkaSuccess(result, forbidden...)
	return result
}

func deleteK8sResource(kind, name string) {
	if strings.TrimSpace(name) == "" {
		return
	}
	cmd := exec.Command("kubectl", "delete", kind, name, "-n", namespace, "--ignore-not-found")
	_, _ = utils.Run(cmd)
}

func k8sResourceExists(kind, name string) bool {
	cmd := exec.Command("kubectl", "get", kind, name, "-n", namespace, "--ignore-not-found")
	output, err := utils.Run(cmd)
	return err == nil && strings.TrimSpace(output) != ""
}
