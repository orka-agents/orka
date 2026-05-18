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
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/sozercan/orka/internal/llm"
	openaiprovider "github.com/sozercan/orka/internal/llm/openai"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/test/utils"
	workercommon "github.com/sozercan/orka/workers/common"
)

const (
	controllerAPIService = "orka-api"
	liveProxyProbeAPIKey = "live-proxy-e2e-probe"
)

// Keep this to models that work through the OpenAI provider path; the live
// catalog can include runtime-only GPT models rejected by the worker route.
var liveProxyOpenAIModelPreferences = []string{
	"gpt-5.5",
	"gpt-5.2",
	"gpt-5-mini",
	"gpt-4.1",
	"gpt-4.1-2025-04-14",
	"gpt-4o",
	"gpt-4o-2024-11-20",
	"gpt-4o-2024-08-06",
	"gpt-4o-2024-05-13",
	"gpt-4o-mini",
	"gpt-4o-mini-2024-07-18",
	"gpt-4",
	"gpt-4-0613",
	"gpt-3.5-turbo",
	"gpt-3.5-turbo-0613",
}

type statusConditionSnapshot struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type providerSnapshot struct {
	Spec struct {
		Type         string `json:"type"`
		BaseURL      string `json:"baseURL"`
		DefaultModel string `json:"defaultModel"`
	} `json:"spec"`
	Status struct {
		Ready         bool                      `json:"ready"`
		Message       string                    `json:"message"`
		LastValidated string                    `json:"lastValidated"`
		Conditions    []statusConditionSnapshot `json:"conditions"`
	} `json:"status"`
}

type taskSnapshot struct {
	Status struct {
		Phase          string `json:"phase"`
		StartTime      string `json:"startTime"`
		CompletionTime string `json:"completionTime"`
		Attempts       int32  `json:"attempts"`
		JobName        string `json:"jobName"`
		Message        string `json:"message"`
		ResultRef      *struct {
			Available bool `json:"available"`
		} `json:"resultRef"`
		Conditions []statusConditionSnapshot `json:"conditions"`
	} `json:"status"`
}

type proxyReadyResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type proxyModelCatalog struct {
	FirstModelID              string
	DataModelIDs              []string
	ExtraModelIDs             []string
	AllModelIDs               []string
	SupportedEndpointsByModel map[string][]string
}

type apiTaskResultResponse struct {
	Result string `json:"result"`
}

type apiMemoryListResponse struct {
	Items []store.Memory `json:"items"`
}

type apiMemoryProposalListResponse struct {
	Items []store.MemoryProposal `json:"items"`
}

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
		body, _ := readResponseBody(resp)
		return "", fmt.Errorf("unexpected status from %s: %s (%s)", modelsURL, resp.Status, strings.TrimSpace(body))
	}

	body, err := readResponseBody(resp)
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

	return "", fmt.Errorf("no models returned from %s", modelsURL)
}

func copilotProxyModelsURL(baseURL string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + "/models"
	}
	return baseURL + "/v1/models"
}

func readResponseBody(resp *http.Response) (string, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
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

func allModelsFromPayload(payload map[string]any) proxyModelCatalog {
	catalog := proxyModelCatalog{SupportedEndpointsByModel: map[string][]string{}}

	if data, ok := payload["data"].([]any); ok {
		for _, item := range data {
			if model := firstModelFromItem(item); model != "" {
				catalog.DataModelIDs = append(catalog.DataModelIDs, model)
				catalog.recordSupportedEndpoints(model, supportedEndpointsFromItem(item))
			}
		}
	}

	if models, ok := payload["models"].([]any); ok {
		for _, item := range models {
			if model := firstModelFromItem(item); model != "" {
				catalog.ExtraModelIDs = append(catalog.ExtraModelIDs, model)
				catalog.recordSupportedEndpoints(model, supportedEndpointsFromItem(item))
			}
		}
	}

	catalog.AllModelIDs = uniqueStrings(append(append([]string{}, catalog.DataModelIDs...), catalog.ExtraModelIDs...))
	if len(catalog.AllModelIDs) > 0 {
		catalog.FirstModelID = catalog.AllModelIDs[0]
	}

	return catalog
}

func (c *proxyModelCatalog) recordSupportedEndpoints(modelID string, endpoints []string) {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if modelID == "" || len(endpoints) == 0 {
		return
	}
	if c.SupportedEndpointsByModel == nil {
		c.SupportedEndpointsByModel = map[string][]string{}
	}
	c.SupportedEndpointsByModel[modelID] = uniqueStrings(append(c.SupportedEndpointsByModel[modelID], endpoints...))
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}

	return unique
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

func supportedEndpointsFromItem(item any) []string {
	v, ok := item.(map[string]any)
	if !ok {
		return nil
	}
	values, ok := v["supported_endpoints"].([]any)
	if !ok {
		return nil
	}

	endpoints := make([]string, 0, len(values))
	for _, value := range values {
		endpoint, ok := value.(string)
		if !ok {
			continue
		}
		if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
			endpoints = append(endpoints, endpoint)
		}
	}
	return uniqueStrings(endpoints)
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

func fetchServiceProxyBody(serviceNamespace, serviceName string, servicePort int, endpointPath string) (string, error) {
	cmd := exec.Command(
		"kubectl",
		"get",
		"--raw",
		serviceProxyPath(serviceNamespace, serviceName, servicePort, endpointPath),
	)
	return utils.Run(cmd)
}

func waitForProxyReadyViaServiceProxy(serviceNamespace, serviceName string, servicePort int) proxyReadyResponse {
	var ready proxyReadyResponse
	Eventually(func(g Gomega) {
		body, err := fetchServiceProxyBody(serviceNamespace, serviceName, servicePort, "/readyz")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(json.Unmarshal([]byte(body), &ready)).To(Succeed())
		g.Expect(ready.Status).To(Equal("ready"))
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
	return ready
}

func discoverProxyModelViaServiceProxy(serviceNamespace, serviceName string, servicePort int) string {
	var modelID string
	Eventually(func(g Gomega) {
		catalog, err := fetchProxyModelCatalogViaServiceProxy(serviceNamespace, serviceName, servicePort)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(catalog.FirstModelID).NotTo(BeEmpty(), "proxy service should return at least one model")
		modelID = catalog.FirstModelID
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
	return modelID
}

func fetchProxyModelCatalogViaServiceProxy(serviceNamespace, serviceName string, servicePort int) (proxyModelCatalog, error) {
	body, err := fetchServiceProxyBody(serviceNamespace, serviceName, servicePort, "/v1/models")
	if err != nil {
		return proxyModelCatalog{}, err
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return proxyModelCatalog{}, err
	}

	catalog := allModelsFromPayload(payload)
	if catalog.FirstModelID != "" {
		return catalog, nil
	}

	return proxyModelCatalog{}, fmt.Errorf(
		"no models returned from service proxy for %s/%s:%d",
		serviceNamespace,
		serviceName,
		servicePort,
	)
}

func discoverProxyModelByFamilyViaServiceProxy(serviceNamespace, serviceName string, servicePort int, prefixes ...string) string {
	var modelID string
	Eventually(func(g Gomega) {
		catalog, err := fetchProxyModelCatalogViaServiceProxy(serviceNamespace, serviceName, servicePort)
		g.Expect(err).NotTo(HaveOccurred())
		modelID = firstProxyModelMatchingPrefixes(catalog, prefixes...)
		g.Expect(modelID).NotTo(BeEmpty(), "proxy service should expose a model matching %v", prefixes)
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
	return modelID
}

func discoverPreferredProxyModelViaServiceProxy(serviceNamespace, serviceName string, servicePort int, preferredIDs []string, prefixes ...string) string {
	var modelID string
	Eventually(func(g Gomega) {
		catalog, err := fetchProxyModelCatalogViaServiceProxy(serviceNamespace, serviceName, servicePort)
		g.Expect(err).NotTo(HaveOccurred())
		modelID = firstPreferredProxyModel(catalog, preferredIDs, prefixes...)
		g.Expect(modelID).NotTo(BeEmpty(), "proxy service should expose a preferred model matching %v", prefixes)
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
	return modelID
}

func discoverUsableProxyOpenAIModelViaServiceProxy(serviceNamespace, serviceName string, servicePort, localPort int, preferredIDs []string, prefixes ...string) (string, error) {
	proxyBaseURL, cancelProxyPF, proxyPFCmd, err := startServicePortForward(serviceNamespace, serviceName, localPort, servicePort)
	if err != nil {
		return "", err
	}
	defer stopPortForward(cancelProxyPF, proxyPFCmd)

	var modelID string
	Eventually(func(g Gomega) {
		catalog, err := fetchProxyModelCatalogViaServiceProxy(serviceNamespace, serviceName, servicePort)
		g.Expect(err).NotTo(HaveOccurred())

		modelID, err = firstUsableProxyOpenAIModel(proxyBaseURL, catalog, preferredIDs, prefixes...)
		if modelID == "" && err == nil {
			return
		}
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(modelID).NotTo(BeEmpty(), "proxy service should expose a usable GPT-family OpenAI model")
	}, 2*time.Minute, 2*time.Second).Should(Succeed())

	return modelID, nil
}

func firstProxyModelMatchingPrefixes(catalog proxyModelCatalog, prefixes ...string) string {
	for _, modelID := range catalog.AllModelIDs {
		if modelMatchesAnyPrefix(modelID, prefixes...) {
			return modelID
		}
	}
	return ""
}

func firstPreferredProxyModel(catalog proxyModelCatalog, preferredIDs []string, prefixes ...string) string {
	candidates := preferredProxyModelCandidates(catalog, preferredIDs, prefixes...)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0]
}

func firstPreferredProxyModelSupportingEndpoint(catalog proxyModelCatalog, endpoint string, preferredIDs []string, prefixes ...string) string {
	for _, modelID := range preferredProxyModelCandidates(catalog, preferredIDs, prefixes...) {
		if catalog.modelSupportsEndpoint(modelID, endpoint) {
			return modelID
		}
	}
	return ""
}

func preferredProxyModelCandidates(catalog proxyModelCatalog, preferredIDs []string, prefixes ...string) []string {
	var candidates []string
	addCandidate := func(modelID string) {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			return
		}
		for _, existing := range candidates {
			if strings.EqualFold(existing, modelID) {
				return
			}
		}
		candidates = append(candidates, modelID)
	}

	for _, preferredID := range preferredIDs {
		preferredID = strings.ToLower(strings.TrimSpace(preferredID))
		if preferredID == "" {
			continue
		}

		for _, modelID := range catalog.AllModelIDs {
			if strings.EqualFold(strings.TrimSpace(modelID), preferredID) {
				addCandidate(modelID)
			}
		}
	}

	for _, modelID := range catalog.AllModelIDs {
		if modelMatchesAnyPrefix(modelID, prefixes...) {
			addCandidate(modelID)
		}
	}

	return candidates
}

func firstUsableProxyOpenAIModel(baseURL string, catalog proxyModelCatalog, preferredIDs []string, prefixes ...string) (string, error) {
	var probeFailures []string

	for _, modelID := range preferredProxyModelCandidates(catalog, preferredIDs, prefixes...) {
		if !isProxyOpenAIProviderCandidate(modelID) {
			continue
		}
		hasEndpointMetadata := catalog.modelHasEndpointMetadata(modelID)
		supportsOpenAIProvider := catalog.modelSupportsEndpoint(modelID, "/responses") ||
			catalog.modelSupportsEndpoint(modelID, "/chat/completions")
		if hasEndpointMetadata && !supportsOpenAIProvider {
			continue
		}
		if err := probeProxyOpenAIProviderModel(baseURL, modelID); err == nil {
			return modelID, nil
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "\nOpenAI provider probe for model %q failed: %v\n", modelID, err)
			probeFailures = append(probeFailures, fmt.Sprintf("%s: %v", modelID, err))
		}
	}

	if len(probeFailures) > 0 {
		return "", fmt.Errorf("GPT OpenAI provider model probes failed: %s", strings.Join(probeFailures, "; "))
	}

	return "", nil
}

func isProxyOpenAIProviderCandidate(modelID string) bool {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if modelID == "" {
		return false
	}
	// These aliases can be visible in the live catalog while still being
	// rejected by the worker's copilot-language-server OpenAI provider route.
	return !strings.Contains(modelID, "codex") &&
		!strings.Contains(modelID, "copilot") &&
		!strings.HasPrefix(modelID, "gpt-5.4")
}

func probeProxyOpenAIProviderModel(baseURL, modelID string) error {
	if _, err := probeProxyOpenAIProviderCompletion(
		baseURL,
		modelID,
		"Reply with exactly OK and nothing else.",
		nil,
	); err != nil {
		return fmt.Errorf("completion probe failed: %w", err)
	}

	tools := []llm.Tool{
		{
			Name:        "noop_tool",
			Description: "No-op probe tool for live proxy model selection.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
	}
	resp, err := probeProxyOpenAIProviderCompletion(
		baseURL,
		modelID,
		"Call noop_tool exactly once before answering.",
		tools,
	)
	if err != nil {
		return fmt.Errorf("tool completion probe failed: %w", err)
	}
	if len(resp.ToolCalls) == 0 {
		return fmt.Errorf("tool completion probe returned no tool calls")
	}
	if resp.ToolCalls[0].Name != "noop_tool" {
		return fmt.Errorf("tool completion probe returned tool %q", resp.ToolCalls[0].Name)
	}

	return nil
}

func probeProxyOpenAIProviderCompletion(baseURL, modelID, prompt string, tools []llm.Tool) (*llm.CompletionResponse, error) {
	provider, err := openaiprovider.NewProvider(llm.ProviderConfig{
		ProviderType: "openai",
		APIKey:       liveProxyProbeAPIKey,
		BaseURL:      strings.TrimRight(baseURL, "/") + "/v1",
	})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return provider.Complete(ctx, &llm.CompletionRequest{
		Model: modelID,
		Messages: []llm.Message{
			{
				Role:    "user",
				Content: prompt,
			},
		},
		MaxTokens: 16,
		Tools:     tools,
	})
}

func (c proxyModelCatalog) modelHasEndpointMetadata(modelID string) bool {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if modelID == "" {
		return false
	}
	return len(c.SupportedEndpointsByModel[modelID]) > 0
}

func (c proxyModelCatalog) modelSupportsEndpoint(modelID, endpoint string) bool {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	endpoint = strings.TrimSpace(endpoint)
	if modelID == "" || endpoint == "" {
		return false
	}

	for _, supportedEndpoint := range c.SupportedEndpointsByModel[modelID] {
		if strings.EqualFold(strings.TrimSpace(supportedEndpoint), endpoint) {
			return true
		}
	}
	return false
}

func modelMatchesAnyPrefix(modelID string, prefixes ...string) bool {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if modelID == "" {
		return false
	}

	for _, prefix := range prefixes {
		prefix = strings.ToLower(strings.TrimSpace(prefix))
		if prefix != "" && strings.HasPrefix(modelID, prefix) {
			return true
		}
	}

	return false
}

func liveCopilotProxyRootURL() string {
	baseURL := strings.TrimRight(e2eLiveCopilotProxyBaseURL, "/")
	return strings.TrimSuffix(baseURL, "/v1")
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

func fetchProviderSnapshot(name string) providerSnapshot {
	var snapshot providerSnapshot
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "provider", name, "-n", namespace, "-o", "json")
		body, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(json.Unmarshal([]byte(body), &snapshot)).To(Succeed())
	}, 30*time.Second, time.Second).Should(Succeed())
	return snapshot
}

func fetchTaskSnapshot(name string) taskSnapshot {
	var snapshot taskSnapshot
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "task", name, "-n", namespace, "-o", "json")
		body, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(json.Unmarshal([]byte(body), &snapshot)).To(Succeed())
	}, 30*time.Second, time.Second).Should(Succeed())
	return snapshot
}

func getJobEnvMap(taskName string) map[string]string {
	var envMap map[string]string
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "jobs",
			"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
			"-o", "jsonpath={.items[0].spec.template.spec.containers[0].env}",
			"-n", namespace,
		)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).NotTo(BeEmpty())

		var envVars []envVar
		g.Expect(json.Unmarshal([]byte(output), &envVars)).To(Succeed())

		envMap = make(map[string]string, len(envVars))
		for _, envVar := range envVars {
			envMap[envVar.Name] = envVar.Value
		}
	}, 30*time.Second, time.Second).Should(Succeed())

	return envMap
}

func getJobInitContainerNames(taskName string) []string {
	var names []string
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "jobs",
			"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
			"-o", "jsonpath={.items[0].spec.template.spec.initContainers[*].name}",
			"-n", namespace,
		)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		names = strings.Fields(output)
	}, 30*time.Second, time.Second).Should(Succeed())

	return names
}

func findStatusCondition(conditions []statusConditionSnapshot, conditionType string) *statusConditionSnapshot {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func stopPortForward(cancel context.CancelFunc, cmd *exec.Cmd) {
	if cancel != nil {
		cancel()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Wait()
	}
}

func fetchTaskResultViaAPI(apiBaseURL, token, taskName string) string {
	var result string
	Eventually(func(g Gomega) {
		got, err := getTaskResultViaAPI(apiBaseURL, token, taskName)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(got)).NotTo(BeEmpty())
		result = got
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
	return result
}

func fetchTaskResultSummaryViaAPI(apiBaseURL, token, taskName string) string {
	return workercommon.ParseStructuredResult(fetchTaskResultViaAPI(apiBaseURL, token, taskName)).Summary
}

func apiValuesWithNamespace(values url.Values) url.Values {
	cloned := url.Values{}
	for key, existing := range values {
		for _, value := range existing {
			cloned.Add(key, value)
		}
	}
	if strings.TrimSpace(cloned.Get("namespace")) == "" {
		cloned.Set("namespace", namespace)
	}
	return cloned
}

func apiEndpoint(apiBaseURL, path string, values url.Values) string {
	endpoint := strings.TrimRight(apiBaseURL, "/") + path
	if len(values) > 0 {
		endpoint += "?" + values.Encode()
	}
	return endpoint
}

func createDurableMemoryViaAPI(apiBaseURL, token string, memory store.Memory) store.Memory {
	if strings.TrimSpace(memory.Namespace) == "" {
		memory.Namespace = namespace
	}
	payload, err := json.Marshal(memory)
	Expect(err).NotTo(HaveOccurred())

	body, statusCode, err := doAuthorizedJSONRequest(
		http.MethodPost,
		apiEndpoint(apiBaseURL, "/api/v1/memories", nil),
		token,
		string(payload),
		"",
	)
	Expect(err).NotTo(HaveOccurred())
	Expect(statusCode).To(Equal(http.StatusCreated), "unexpected durable memory create response: %s", strings.TrimSpace(body))

	var created store.Memory
	Expect(json.Unmarshal([]byte(body), &created)).To(Succeed())
	return created
}

func listDurableMemoriesViaAPI(apiBaseURL, token string, values url.Values) []store.Memory {
	var memories []store.Memory
	Eventually(func(g Gomega) {
		body, statusCode, err := doAuthorizedJSONRequest(
			http.MethodGet,
			apiEndpoint(apiBaseURL, "/api/v1/memories", apiValuesWithNamespace(values)),
			token,
			"",
			"",
		)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(statusCode).To(Equal(http.StatusOK), "unexpected durable memory list response: %s", strings.TrimSpace(body))

		var payload apiMemoryListResponse
		g.Expect(json.Unmarshal([]byte(body), &payload)).To(Succeed())
		memories = payload.Items
	}, time.Minute, 2*time.Second).Should(Succeed())

	return memories
}

func getDurableMemoryViaAPI(apiBaseURL, token, id string) store.Memory {
	body, statusCode, err := doAuthorizedJSONRequest(
		http.MethodGet,
		apiEndpoint(apiBaseURL, "/api/v1/memories/"+url.PathEscape(id), apiValuesWithNamespace(nil)),
		token,
		"",
		"",
	)
	Expect(err).NotTo(HaveOccurred())
	Expect(statusCode).To(Equal(http.StatusOK), "unexpected durable memory get response: %s", strings.TrimSpace(body))

	var memory store.Memory
	Expect(json.Unmarshal([]byte(body), &memory)).To(Succeed())
	return memory
}

func updateDurableMemoryViaAPI(apiBaseURL, token, id string, memory store.Memory) store.Memory {
	if strings.TrimSpace(memory.Namespace) == "" {
		memory.Namespace = namespace
	}
	payload, err := json.Marshal(memory)
	Expect(err).NotTo(HaveOccurred())

	body, statusCode, err := doAuthorizedJSONRequest(
		http.MethodPut,
		apiEndpoint(apiBaseURL, "/api/v1/memories/"+url.PathEscape(id), apiValuesWithNamespace(nil)),
		token,
		string(payload),
		"",
	)
	Expect(err).NotTo(HaveOccurred())
	Expect(statusCode).To(Equal(http.StatusOK), "unexpected durable memory update response: %s", strings.TrimSpace(body))

	var updated store.Memory
	Expect(json.Unmarshal([]byte(body), &updated)).To(Succeed())
	return updated
}

func setDurableMemoryEnabledViaAPI(apiBaseURL, token, id string, enabled bool) store.Memory {
	action := "enable"
	if !enabled {
		action = "disable"
	}
	body, statusCode, err := doAuthorizedJSONRequest(
		http.MethodPost,
		apiEndpoint(apiBaseURL, "/api/v1/memories/"+url.PathEscape(id)+"/"+action, apiValuesWithNamespace(nil)),
		token,
		"",
		"",
	)
	Expect(err).NotTo(HaveOccurred())
	Expect(statusCode).To(Equal(http.StatusNoContent), "unexpected durable memory %s response: %s", action, strings.TrimSpace(body))
	return getDurableMemoryViaAPI(apiBaseURL, token, id)
}

func tryDeleteDurableMemoryViaAPI(apiBaseURL, token, id string) error {
	body, statusCode, err := doAuthorizedJSONRequest(
		http.MethodDelete,
		apiEndpoint(apiBaseURL, "/api/v1/memories/"+url.PathEscape(id), apiValuesWithNamespace(nil)),
		token,
		"",
		"",
	)
	if err != nil {
		return err
	}
	if statusCode != http.StatusNoContent && statusCode != http.StatusNotFound {
		return fmt.Errorf("unexpected durable memory delete response %d: %s", statusCode, strings.TrimSpace(body))
	}
	return nil
}

func deleteDurableMemoryViaAPI(apiBaseURL, token, id string) {
	Expect(tryDeleteDurableMemoryViaAPI(apiBaseURL, token, id)).To(Succeed())
}

func cleanupDurableMemoryViaAPI(apiBaseURL, token, id string) {
	if err := tryDeleteDurableMemoryViaAPI(apiBaseURL, token, id); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "durable memory cleanup skipped: %v\n", err)
	}
}

func createMemoryProposalViaAPI(apiBaseURL, token string, proposal store.MemoryProposal) store.MemoryProposal {
	if strings.TrimSpace(proposal.Namespace) == "" {
		proposal.Namespace = namespace
	}
	payload, err := json.Marshal(proposal)
	Expect(err).NotTo(HaveOccurred())

	body, statusCode, err := doAuthorizedJSONRequest(
		http.MethodPost,
		apiEndpoint(apiBaseURL, "/api/v1/memory-proposals", nil),
		token,
		string(payload),
		"",
	)
	Expect(err).NotTo(HaveOccurred())
	Expect(statusCode).To(Equal(http.StatusCreated), "unexpected memory proposal create response: %s", strings.TrimSpace(body))

	var created store.MemoryProposal
	Expect(json.Unmarshal([]byte(body), &created)).To(Succeed())
	return created
}

func listMemoryProposalsViaAPI(apiBaseURL, token string, values url.Values) []store.MemoryProposal {
	var proposals []store.MemoryProposal
	Eventually(func(g Gomega) {
		body, statusCode, err := doAuthorizedJSONRequest(
			http.MethodGet,
			apiEndpoint(apiBaseURL, "/api/v1/memory-proposals", apiValuesWithNamespace(values)),
			token,
			"",
			"",
		)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(statusCode).To(Equal(http.StatusOK), "unexpected memory proposal list response: %s", strings.TrimSpace(body))

		var payload apiMemoryProposalListResponse
		g.Expect(json.Unmarshal([]byte(body), &payload)).To(Succeed())
		proposals = payload.Items
	}, time.Minute, 2*time.Second).Should(Succeed())

	return proposals
}

func fetchMemoryProposalsViaAPI(apiBaseURL, token, taskName, query string) []store.MemoryProposal {
	values := url.Values{}
	if strings.TrimSpace(taskName) != "" {
		values.Set("taskName", taskName)
	}
	if strings.TrimSpace(query) != "" {
		values.Set("query", query)
	}

	var proposals []store.MemoryProposal
	Eventually(func(g Gomega) {
		proposals = listMemoryProposalsViaAPI(apiBaseURL, token, values)
		g.Expect(proposals).NotTo(BeEmpty(), "expected at least one memory proposal for task %s containing %s", taskName, query)
	}, time.Minute, 2*time.Second).Should(Succeed())

	return proposals
}

func getMemoryProposalViaAPI(apiBaseURL, token, id string) store.MemoryProposal {
	body, statusCode, err := doAuthorizedJSONRequest(
		http.MethodGet,
		apiEndpoint(apiBaseURL, "/api/v1/memory-proposals/"+url.PathEscape(id), apiValuesWithNamespace(nil)),
		token,
		"",
		"",
	)
	Expect(err).NotTo(HaveOccurred())
	Expect(statusCode).To(Equal(http.StatusOK), "unexpected memory proposal get response: %s", strings.TrimSpace(body))

	var proposal store.MemoryProposal
	Expect(json.Unmarshal([]byte(body), &proposal)).To(Succeed())
	return proposal
}

func reviewMemoryProposalViaAPI(apiBaseURL, token, id string, review store.MemoryProposalReview) store.MemoryProposal {
	if strings.TrimSpace(review.Namespace) == "" {
		review.Namespace = namespace
	}
	payload, err := json.Marshal(struct {
		Namespace  string `json:"namespace"`
		Status     string `json:"status"`
		Reviewer   string `json:"reviewer,omitempty"`
		ReviewNote string `json:"reviewNote,omitempty"`
	}{
		Namespace:  review.Namespace,
		Status:     review.Status,
		Reviewer:   review.Reviewer,
		ReviewNote: review.ReviewNote,
	})
	Expect(err).NotTo(HaveOccurred())

	body, statusCode, err := doAuthorizedJSONRequest(
		http.MethodPost,
		apiEndpoint(apiBaseURL, "/api/v1/memory-proposals/"+url.PathEscape(id)+"/review", apiValuesWithNamespace(nil)),
		token,
		string(payload),
		"",
	)
	Expect(err).NotTo(HaveOccurred())
	Expect(statusCode).To(Equal(http.StatusNoContent), "unexpected memory proposal review response: %s", strings.TrimSpace(body))
	return getMemoryProposalViaAPI(apiBaseURL, token, id)
}

func getTaskResultViaAPI(apiBaseURL, token, taskName string) (string, error) {
	url := fmt.Sprintf("%s/api/v1/tasks/%s/result?namespace=%s", strings.TrimRight(apiBaseURL, "/"), taskName, namespace)
	body, statusCode, err := doAuthorizedJSONRequest(http.MethodGet, url, token, "", "")
	if err != nil {
		return "", err
	}
	if statusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status from task result endpoint: %d (%s)", statusCode, strings.TrimSpace(body))
	}

	var payload apiTaskResultResponse
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return "", err
	}

	return payload.Result, nil
}

func fetchSessionViaAPI(apiBaseURL, token, sessionID string) string {
	var body string
	Eventually(func(g Gomega) {
		got, statusCode, err := doAuthorizedJSONRequest(
			http.MethodGet,
			fmt.Sprintf("%s/api/v1/sessions/%s", strings.TrimRight(apiBaseURL, "/"), sessionID),
			token,
			"",
			"",
		)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(statusCode).To(Equal(http.StatusOK))
		body = got
	}, 30*time.Second, time.Second).Should(Succeed())

	return body
}

func doAuthorizedJSONRequest(method, url, token, body, accept string) (string, int, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return "", 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp.StatusCode, err
	}

	return string(responseBody), resp.StatusCode, nil
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

func dumpLiveCopilotProxyDebugInfo(providerNames ...string) {
	if !CurrentSpecReport().Failed() {
		return
	}

	serviceNamespace := liveCopilotProxyServiceNamespace()
	serviceName := liveCopilotProxyServiceName()
	servicePort := liveCopilotProxyServicePort()

	By("collecting live copilot proxy debug information")

	for _, providerName := range providerNames {
		if strings.TrimSpace(providerName) == "" {
			continue
		}
		cmd := exec.Command("kubectl", "get", "provider", providerName, "-n", namespace, "-o", "yaml")
		output, err := utils.Run(cmd)
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Provider %s ===\n%s\n", providerName, output)
		}
	}

	if body, err := fetchServiceProxyBody(serviceNamespace, serviceName, servicePort, "/readyz"); err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "\n=== copilot-proxy /readyz ===\n%s\n", body)
	}

	if body, err := fetchServiceProxyBody(serviceNamespace, serviceName, servicePort, "/v1/models"); err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "\n=== copilot-proxy /v1/models ===\n%s\n", body)
	}
}
