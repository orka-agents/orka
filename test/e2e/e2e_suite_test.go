//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/sozercan/orka/test/utils"
)

var (
	// managerImage is the manager image to be built and loaded for testing.
	managerImage = "ghcr.io/sozercan/orka:latest"

	// Worker and harness images to build and load for e2e testing.
	aiWorkerImage                    = "ghcr.io/sozercan/orka/ai-worker:latest"
	generalWorkerImage               = "ghcr.io/sozercan/orka/general-worker:latest"
	harnessWrapperImage              = "ghcr.io/sozercan/orka/agent-harness-wrapper:latest"
	agentRuntimeExternalHarnessImage = "ghcr.io/sozercan/orka/agent-runtime-external-harness:e2e"
	agentRuntimeExternalE2EEnvVar    = "E2E_AGENTRUNTIME_EXTERNAL"

	// E2E environment configuration (loaded from .env or environment)
	e2eOpenAIAPIKey            string
	e2eOpenAIBaseURL           string
	e2eOpenAIModel             string
	e2eAnthropicAPIKey         string
	e2eAnthropicBaseURL        string
	e2eAnthropicModel          string
	e2eGitHubToken             string
	e2eLiveCopilotProxyBaseURL string
)

// TestE2E runs the e2e test suite to validate the solution in an isolated environment.
// The default setup requires Kind.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting orka e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	By("loading e2e environment file")
	earlyEnvProjectDir, _ := utils.GetProjectDir()
	loadEnvFile(filepath.Join(earlyEnvProjectDir, "test", "e2e", ".env"))

	By("building all Docker images")
	cmd := exec.Command("make", "docker-build-all",
		fmt.Sprintf("IMG=%s", managerImage),
		fmt.Sprintf("AI_WORKER_IMG=%s", aiWorkerImage),
		fmt.Sprintf("GENERAL_WORKER_IMG=%s", generalWorkerImage),
		fmt.Sprintf("HARNESS_WRAPPER_IMG=%s", harnessWrapperImage),
	)
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build Docker images")

	if agentRuntimeExternalE2EEnabled() {
		By("building the AgentRuntime external harness Docker image")
		cmd = exec.Command("docker", "build", "-t", agentRuntimeExternalHarnessImage,
			"-f", "examples/harness/echo/Dockerfile", ".")
		_, err = utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build AgentRuntime external harness image")
	}

	By("loading all images into Kind cluster")
	images := []string{
		managerImage,
		aiWorkerImage,
		generalWorkerImage,
		harnessWrapperImage,
	}
	if agentRuntimeExternalE2EEnabled() {
		images = append(images, agentRuntimeExternalHarnessImage)
	}
	for _, img := range images {
		err = utils.LoadImageToKindClusterWithName(img)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to load image %s into Kind", img))
	}

	By("loading e2e environment configuration")
	projectDir, _ := utils.GetProjectDir()
	loadEnvFile(filepath.Join(projectDir, "test", "e2e", ".env"))
	e2eOpenAIAPIKey = os.Getenv("E2E_OPENAI_API_KEY")
	e2eOpenAIBaseURL = os.Getenv("E2E_OPENAI_BASE_URL")
	e2eOpenAIModel = os.Getenv("E2E_OPENAI_MODEL")
	e2eAnthropicAPIKey = os.Getenv("E2E_ANTHROPIC_API_KEY")
	e2eAnthropicBaseURL = os.Getenv("E2E_ANTHROPIC_BASE_URL")
	e2eAnthropicModel = os.Getenv("E2E_ANTHROPIC_MODEL")
	e2eGitHubToken = os.Getenv("E2E_GITHUB_TOKEN")
	e2eLiveCopilotProxyBaseURL = firstSetEnv(
		"E2E_LIVE_COPILOT_PROXY_BASE_URL",
		"E2E_COPILOT_PROXY_BASE_URL",
		"COPILOT_PROXY_BASE_URL",
	)

	By("creating manager namespace")
	cmd = exec.Command("kubectl", "create", "ns", namespace)
	_, _ = utils.Run(cmd) // ignore if already exists

	By("labeling the namespace to enforce the restricted security policy")
	cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
		"pod-security.kubernetes.io/enforce=restricted")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

	By("creating e2e K8s secrets from environment variables")
	if e2eOpenAIAPIKey != "" {
		err = createK8sSecret("e2e-openai-secret", namespace, map[string]string{"api-key": e2eOpenAIAPIKey})
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to create OpenAI secret")
		_, _ = fmt.Fprintf(GinkgoWriter, "Created e2e-openai-secret\n")
	}
	if e2eAnthropicAPIKey != "" {
		anthropicSecretData := map[string]string{"ANTHROPIC_API_KEY": e2eAnthropicAPIKey}
		if e2eAnthropicBaseURL != "" {
			anthropicSecretData["ANTHROPIC_BASE_URL"] = e2eAnthropicBaseURL
		}
		err = createK8sSecret("e2e-anthropic-secret", namespace, anthropicSecretData)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to create Anthropic secret")
		_, _ = fmt.Fprintf(GinkgoWriter, "Created e2e-anthropic-secret\n")
	}
	if e2eGitHubToken != "" {
		err = createK8sSecret("e2e-github-secret", namespace, map[string]string{"GITHUB_TOKEN": e2eGitHubToken})
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to create GitHub secret")
		_, _ = fmt.Fprintf(GinkgoWriter, "Created e2e-github-secret\n")
	}

	By("installing CRDs")
	cmd = exec.Command("make", "install")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to install CRDs")

	By("waiting for CRDs to be established")
	Eventually(func(g Gomega) {
		for _, crd := range []string{
			"tasks.core.orka.ai",
			"agents.core.orka.ai",
			"agentruntimes.core.orka.ai",
			"tools.core.orka.ai",
			"providers.core.orka.ai",
			"skills.core.orka.ai",
			"repositoryscans.core.orka.ai",
			"repositorymonitors.core.orka.ai",
			"substrateactorpools.core.orka.ai",
		} {
			cmd := exec.Command("kubectl", "wait", "--for=condition=Established",
				"crd/"+crd, "--timeout=30s")
			_, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "CRD %s not established", crd)
		}
	}, 60*time.Second, time.Second).Should(Succeed())

	By("deploying the controller-manager")
	cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

	By("resolving the controller-manager deployment name")
	var controllerManagerDeployment string
	Eventually(func(g Gomega) {
		name, err := controllerManagerDeploymentName()
		g.Expect(err).NotTo(HaveOccurred(), "Failed to resolve the controller-manager deployment")
		g.Expect(name).NotTo(BeEmpty(), "controller-manager deployment name was empty")
		controllerManagerDeployment = name
	}, 30*time.Second, time.Second).Should(Succeed())
	_, _ = fmt.Fprintf(GinkgoWriter, "Resolved controller-manager deployment: %s\n", controllerManagerDeployment)

	By("patching the controller-manager deployment to use kind-loaded images")
	cmd = exec.Command(
		"kubectl", "patch", "deployment", controllerManagerDeployment, "-n", namespace, "--type=strategic",
		"-p", `{"spec":{"template":{"spec":{"containers":[{"name":"manager","imagePullPolicy":"IfNotPresent"}]}}}}`,
	)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to patch controller-manager imagePullPolicy")

	By("waiting for controller-manager to be ready")
	cmd = exec.Command("kubectl", "rollout", "status", "deployment/"+controllerManagerDeployment,
		"-n", namespace, "--timeout=5m")
	_, err = utils.Run(cmd)
	if err != nil {
		dumpControllerManagerDiagnostics()
	}
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed waiting for the controller-manager rollout")
})

var _ = AfterSuite(func() {
	By("cleaning up the curl pod for metrics")
	cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace, "--ignore-not-found")
	_, _ = utils.Run(cmd)

	By("cleaning up e2e secrets")
	for _, s := range []string{"e2e-openai-secret", "e2e-anthropic-secret", "e2e-github-secret"} {
		cmd = exec.Command("kubectl", "delete", "secret", s, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	}

	By("undeploying the controller-manager")
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	cmd = exec.CommandContext(cleanupCtx, "make", "undeploy")
	_, _ = utils.Run(cmd)
	cleanupCancel()

	By("uninstalling CRDs")
	cmd = exec.Command("make", "uninstall")
	_, _ = utils.Run(cmd)

	By("removing manager namespace")
	cmd = exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found")
	_, _ = utils.Run(cmd)

})

// loadEnvFile reads a .env file and sets environment variables that are not already set.
func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "No .env file at %s (using environment directly)\n", path)
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

func dumpControllerManagerDiagnostics() {
	By("dumping controller-manager diagnostics")

	for _, args := range [][]string{
		{"get", "pods", "-l", "control-plane=controller-manager", "-n", namespace, "-o", "wide"},
		{"describe", "pods", "-l", "control-plane=controller-manager", "-n", namespace},
		{"get", "events", "-n", namespace, "--sort-by=.lastTimestamp"},
		{"get", "deployments", "-l", "control-plane=controller-manager", "-n", namespace, "-o", "yaml"},
	} {
		cmd := exec.Command("kubectl", args...)
		output, err := utils.Run(cmd)
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "diagnostic command failed: kubectl %s\n%v\n", strings.Join(args, " "), err)
			continue
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "diagnostic output: kubectl %s\n%s\n", strings.Join(args, " "), output)
	}
}

func controllerManagerDeploymentName() (string, error) {
	cmd := exec.Command("kubectl", "get", "deployments", "-l", "control-plane=controller-manager",
		"-n", namespace, "-o", "jsonpath={.items[0].metadata.name}")
	output, err := utils.Run(cmd)
	if err != nil {
		return "", err
	}

	name := strings.TrimSpace(output)
	if name == "" {
		return "", fmt.Errorf("no controller-manager deployment found")
	}

	return name, nil
}

// createK8sSecret creates a Kubernetes Secret with the given key-value data.
func createK8sSecret(name, ns string, data map[string]string) error {
	secret := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]string{
			"name":      name,
			"namespace": ns,
		},
		"type":       "Opaque",
		"stringData": data,
	}
	manifest, err := json.Marshal(secret)
	if err != nil {
		return fmt.Errorf("failed to marshal secret: %w", err)
	}

	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(string(manifest))
	_, err = utils.Run(cmd)
	return err
}

func agentRuntimeExternalE2EEnabled() bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(agentRuntimeExternalE2EEnvVar)))
	return value == "1" || value == "true" || value == "yes"
}

func firstSetEnv(keys ...string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}
