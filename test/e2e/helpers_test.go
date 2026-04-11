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
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/sozercan/orka/test/utils"
)

const controllerAPIService = "orka-api"

// skipIfNoKey skips the current test if the given environment variable is not set or empty.
func skipIfNoKey(envVar string) {
	if os.Getenv(envVar) == "" {
		Skip(fmt.Sprintf("Skipping: %s not set", envVar))
	}
}

// waitForTaskPhase polls until the task reaches the given phase or times out.
func waitForTaskPhase(name, phase string, timeout time.Duration) {
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "task", name,
			"-o", "jsonpath={.status.phase}",
			"-n", namespace,
		)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal(phase), fmt.Sprintf("Task %s should reach phase %s", name, phase))
	}, timeout, time.Second).Should(Succeed())
}

// waitForTaskCompletion waits for a task to reach a terminal phase (Succeeded or Failed).
func waitForTaskCompletion(name string, timeout time.Duration) string {
	var finalPhase string
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "task", name,
			"-o", "jsonpath={.status.phase}",
			"-n", namespace,
		)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(BeElementOf("Succeeded", "Failed"),
			fmt.Sprintf("Task %s should reach terminal phase, got: %s", name, output))
		finalPhase = output
	}, timeout, 2*time.Second).Should(Succeed())
	return finalPhase
}

// verifyResultAvailable checks that a task's resultRef.available is "true".
func verifyResultAvailable(taskName string) {
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "task", taskName,
			"-o", "jsonpath={.status.resultRef.available}",
			"-n", namespace,
		)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal("true"), "Task should have resultRef.available=true")
	}, 30*time.Second, time.Second).Should(Succeed())
}

// verifyJobCreatedForTask waits for a Job labeled with the task name to appear.
func verifyJobCreatedForTask(taskName string, timeout time.Duration) {
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "jobs",
			"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
			"-o", "jsonpath={.items[0].metadata.name}",
			"-n", namespace,
		)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "Failed to get Job for task %s", taskName)
		g.Expect(output).NotTo(BeEmpty(), "Job name should not be empty")
	}, timeout, time.Second).Should(Succeed())
}

// getJobContainerImage returns the container image used by the Job for a task.
func getJobContainerImage(taskName string) string {
	var image string
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "jobs",
			"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
			"-o", "jsonpath={.items[0].spec.template.spec.containers[0].image}",
			"-n", namespace,
		)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).NotTo(BeEmpty())
		image = output
	}, 30*time.Second, time.Second).Should(Succeed())
	return image
}

// createProviderCRD creates a Provider CRD with the given configuration and waits for it to be ready.
func createProviderCRD(name, providerType, secretName, secretKey, baseURL, model string) {
	By(fmt.Sprintf("creating Provider CRD: %s (type: %s)", name, providerType))

	baseURLField := ""
	if baseURL != "" {
		baseURLField = fmt.Sprintf(`,"baseURL":"%s"`, baseURL)
	}
	modelField := ""
	if model != "" {
		modelField = fmt.Sprintf(`,"defaultModel":"%s"`, model)
	}

	manifest := fmt.Sprintf(`{
		"apiVersion": "core.orka.ai/v1alpha1",
		"kind": "Provider",
		"metadata": {
			"name": "%s",
			"namespace": "%s"
		},
		"spec": {
			"type": "%s",
			"secretRef": {
				"name": "%s",
				"key": "%s"
			}%s%s
		}
	}`, name, namespace, providerType, secretName, secretKey, baseURLField, modelField)

	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = stringReader(manifest)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create Provider %s", name)

	// Wait for provider to become ready before returning
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "provider", name,
			"-n", namespace, "-o", "jsonpath={.status.ready}")
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal("true"), "Provider %s should be ready", name)
	}, 30*time.Second, time.Second).Should(Succeed())
}

// discoverProxyModel queries an OpenAI-compatible /v1/models endpoint and returns the first model ID.
func discoverProxyModel(baseURL string) string {
	var modelID string
	Eventually(func(g Gomega) {
		model, err := fetchProxyModel(baseURL)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(model).NotTo(BeEmpty(), "proxy should return at least one model")
		modelID = model
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
	return modelID
}

func fetchProxyModel(baseURL string) (string, error) {
	modelsURL := copilotProxyModelsURL(baseURL)
	req, err := http.NewRequest(http.MethodGet, modelsURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("unexpected status from %s: %s (%s)", modelsURL, resp.Status, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}

	if model := firstModelFromPayload(payload); model != "" {
		return model, nil
	}

	return "", fmt.Errorf("no models returned from %s", modelsURL)
}

func copilotProxyModelsURL(baseURL string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + "/models"
	}
	return baseURL + "/v1/models"
}

func firstModelFromPayload(payload map[string]any) string {
	if data, ok := payload["data"].([]any); ok {
		for _, item := range data {
			if model := firstModelFromItem(item); model != "" {
				return model
			}
		}
	}

	if models, ok := payload["models"].([]any); ok {
		for _, item := range models {
			if model := firstModelFromItem(item); model != "" {
				return model
			}
		}
	}

	if id, ok := payload["id"].(string); ok {
		return strings.TrimSpace(id)
	}

	return ""
}

func firstModelFromItem(item any) string {
	switch v := item.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		if id, ok := v["id"].(string); ok && strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
		if slug, ok := v["slug"].(string); ok && strings.TrimSpace(slug) != "" {
			return strings.TrimSpace(slug)
		}
		if name, ok := v["name"].(string); ok && strings.TrimSpace(name) != "" {
			return strings.TrimSpace(name)
		}
	}

	return ""
}

func startControllerAPIPortForward(localPort int) (string, context.CancelFunc, *exec.Cmd, error) {
	ctx, cancel := context.WithCancel(context.Background())
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", localPort)

	cmd := exec.CommandContext(ctx, "kubectl", "port-forward",
		"-n", namespace,
		"svc/"+controllerAPIService,
		fmt.Sprintf("%d:8080", localPort),
	)
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter

	if err := cmd.Start(); err != nil {
		cancel()
		return "", nil, nil, err
	}

	Eventually(func(g Gomega) {
		resp, err := http.Get(baseURL + "/healthz")
		g.Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
	}, 60*time.Second, time.Second).Should(Succeed())

	return baseURL, cancel, cmd, nil
}

func startServicePortForward(serviceNamespace, serviceName string, localPort, remotePort int) (string, context.CancelFunc, *exec.Cmd, error) {
	ctx, cancel := context.WithCancel(context.Background())
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", localPort)

	cmd := exec.CommandContext(ctx, "kubectl", "port-forward",
		"-n", serviceNamespace,
		"svc/"+serviceName,
		fmt.Sprintf("%d:%d", localPort, remotePort),
	)
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter

	if err := cmd.Start(); err != nil {
		cancel()
		return "", nil, nil, err
	}

	Eventually(func(g Gomega) {
		resp, err := http.Get(baseURL + "/readyz")
		g.Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
	}, 60*time.Second, time.Second).Should(Succeed())

	return baseURL, cancel, cmd, nil
}

func serviceProxyPath(serviceNamespace, serviceName string, servicePort int, endpointPath string) string {
	endpointPath = "/" + strings.TrimLeft(endpointPath, "/")
	return fmt.Sprintf(
		"/api/v1/namespaces/%s/services/http:%s:%d/proxy%s",
		serviceNamespace,
		serviceName,
		servicePort,
		endpointPath,
	)
}

func discoverProxyModelViaServiceProxy(serviceNamespace, serviceName string, servicePort int) string {
	var modelID string
	Eventually(func(g Gomega) {
		model, err := fetchProxyModelViaServiceProxy(serviceNamespace, serviceName, servicePort)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(model).NotTo(BeEmpty(), "proxy service should return at least one model")
		modelID = model
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
	return modelID
}

func fetchProxyModelViaServiceProxy(serviceNamespace, serviceName string, servicePort int) (string, error) {
	cmd := exec.Command(
		"kubectl",
		"get",
		"--raw",
		serviceProxyPath(serviceNamespace, serviceName, servicePort, "/v1/models"),
	)
	body, err := utils.Run(cmd)
	if err != nil {
		return "", err
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return "", err
	}

	if model := firstModelFromPayload(payload); model != "" {
		return model, nil
	}

	return "", fmt.Errorf(
		"no models returned from service proxy for %s/%s:%d",
		serviceNamespace,
		serviceName,
		servicePort,
	)
}

func liveCopilotProxyServiceNamespace() string {
	if ns := strings.TrimSpace(firstSetEnv("E2E_LIVE_COPILOT_PROXY_SERVICE_NAMESPACE")); ns != "" {
		return ns
	}
	return "default"
}

func liveCopilotProxyServiceName() string {
	if name := strings.TrimSpace(firstSetEnv("E2E_LIVE_COPILOT_PROXY_SERVICE_NAME")); name != "" {
		return name
	}
	return "copilot-proxy"
}

func liveCopilotProxyServicePort() int {
	port := strings.TrimSpace(firstSetEnv("E2E_LIVE_COPILOT_PROXY_SERVICE_PORT"))
	if port == "" {
		return 1337
	}

	value, err := strconv.Atoi(port)
	if err != nil || value <= 0 {
		return 1337
	}

	return value
}

func stopPortForward(cancel context.CancelFunc, cmd *exec.Cmd) {
	if cancel != nil {
		cancel()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Wait()
	}
}

// dumpDebugInfo collects and prints debug information on test failure.
func dumpDebugInfo(taskNames ...string) {
	if !CurrentSpecReport().Failed() {
		return
	}

	By("collecting debug information for failed test")

	// Controller logs
	cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
		"-o", "jsonpath={.items[0].metadata.name}", "-n", namespace)
	podName, err := utils.Run(cmd)
	if err == nil && podName != "" {
		cmd = exec.Command("kubectl", "logs", podName, "-n", namespace, "--tail=100")
		logs, err := utils.Run(cmd)
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Controller Logs (last 100 lines) ===\n%s\n", logs)
		}
	}

	// Task details
	for _, name := range taskNames {
		cmd = exec.Command("kubectl", "get", "task", name, "-n", namespace, "-o", "yaml")
		output, err := utils.Run(cmd)
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Task %s ===\n%s\n", name, output)
		}

		// Pod descriptions and logs for task
		cmd = exec.Command("kubectl", "get", "pods", "-l", fmt.Sprintf("orka.ai/task=%s", name),
			"-n", namespace, "-o", "wide")
		output, err = utils.Run(cmd)
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Pods for task %s ===\n%s\n", name, output)
		}

		// Worker pod logs
		cmd = exec.Command("kubectl", "logs", "-l", fmt.Sprintf("orka.ai/task=%s", name),
			"-n", namespace, "--tail=50")
		output, err = utils.Run(cmd)
		if err == nil && output != "" {
			_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Worker Logs for task %s ===\n%s\n", name, output)
		}
	}

	// Namespace events
	cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
	events, err := utils.Run(cmd)
	if err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Namespace Events ===\n%s\n", events)
	}
}
