/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/llm"
	_ "github.com/sozercan/orka/internal/llm/anthropic"
	_ "github.com/sozercan/orka/internal/llm/openai"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/tools"
	"github.com/sozercan/orka/internal/worker"
	"github.com/sozercan/orka/workers/common"
)

const trueStr = "true"

const (
	defaultMemoryContextLimit      = 5
	maxMemoryContextLimit          = 8
	defaultMemoryContextMaxChars   = 6000
	memoryContextPerEntryMaxChars  = 1200
	memoryContextResponseBodyLimit = 1 << 20
	serviceAccountTokenPath        = "/var/run/secrets/kubernetes.io/serviceaccount/token"
)

var memoryToolNames = []string{"recall_memory", "remember", "propose_memory", "search_transcript"}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Get configuration from environment
	taskName := os.Getenv("ORKA_TASK_NAME")
	taskNamespace := os.Getenv("ORKA_TASK_NAMESPACE")

	provider := os.Getenv("ORKA_AI_PROVIDER")
	model := os.Getenv("ORKA_AI_MODEL")
	prompt := os.Getenv("ORKA_AI_PROMPT")
	systemPrompt := os.Getenv("ORKA_AI_SYSTEM_PROMPT")
	toolsStr := os.Getenv("ORKA_AI_TOOLS")
	baseURL := os.Getenv("ORKA_AI_BASE_URL")

	if provider == "" {
		return fmt.Errorf("ORKA_AI_PROVIDER is required")
	}
	if model == "" {
		return fmt.Errorf("ORKA_AI_MODEL is required")
	}
	if prompt == "" {
		return fmt.Errorf("ORKA_AI_PROMPT is required")
	}

	// Get API key
	apiKey := getAPIKey(provider)
	if apiKey == "" {
		return fmt.Errorf("API key for %s not found", provider)
	}

	// Create LLM provider
	azureAPIVersion := os.Getenv("ORKA_AI_AZURE_API_VERSION")
	llmProvider, err := llm.NewProvider(provider, llm.ProviderConfig{
		APIKey:          apiKey,
		BaseURL:         baseURL,
		ProviderType:    provider,
		AzureAPIVersion: azureAPIVersion,
	})
	if err != nil {
		return fmt.Errorf("failed to create LLM provider: %w", err)
	}

	// Wrap with retry logic for transient errors
	llmProvider = llm.NewRetryProvider(llmProvider, 0)

	// Set up fallback providers if configured
	fallbackCountStr := os.Getenv("ORKA_AI_FALLBACK_COUNT")
	if fallbackCountStr != "" {
		fallbackCount, _ := strconv.Atoi(fallbackCountStr)
		if fallbackCount > 0 {
			var fallbacks []llm.FallbackEntry
			for i := range fallbackCount {
				prefix := fmt.Sprintf("ORKA_AI_FALLBACK_%d", i)
				fbProviderType := os.Getenv(prefix + "_PROVIDER")
				fbAPIKey := os.Getenv(prefix + "_API_KEY")
				fbModel := os.Getenv(prefix + "_MODEL")
				fbBaseURL := os.Getenv(prefix + "_BASE_URL")
				fbAzureAPIVersion := os.Getenv(prefix + "_AZURE_API_VERSION")

				if fbProviderType == "" || fbAPIKey == "" {
					fmt.Printf("Warning: skipping fallback %d: missing provider or API key\n", i)
					continue
				}

				fbProvider, err := llm.NewProvider(fbProviderType, llm.ProviderConfig{
					APIKey:          fbAPIKey,
					BaseURL:         fbBaseURL,
					ProviderType:    fbProviderType,
					AzureAPIVersion: fbAzureAPIVersion,
				})
				if err != nil {
					fmt.Printf("Warning: skipping fallback %d: %v\n", i, err)
					continue
				}

				fallbacks = append(fallbacks, llm.FallbackEntry{
					Provider: llm.NewRetryProvider(fbProvider, 0),
					Model:    fbModel,
				})
			}
			if len(fallbacks) > 0 {
				fp := llm.NewFallbackProvider(llmProvider, fallbacks)
				fp.SetCooldownTracker(llm.NewCooldownTracker())
				llmProvider = fp
			}
		}
	}

	// Parse enabled tools
	enabledTools := normalizeEnabledTools(strings.Split(toolsStr, ","))

	// Create Kubernetes client for Tool CRDs
	k8sClient, err := createK8sClient()
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %w", err)
	}

	// Register coordination tools if enabled
	if os.Getenv("ORKA_COORDINATION_ENABLED") == trueStr {
		tools.RegisterCoordinationTools(k8sClient)
	}

	// Memory tools use the controller's internal API and are safe to register
	// independently of coordination mode. Auto-enable them when the controller
	// context needed to execute them is present.
	registerMemoryTools()
	enabledTools = autoEnableMemoryTools(enabledTools)

	// Load custom Tool CRDs
	customTools := loadCustomTools(ctx, k8sClient, taskNamespace, enabledTools)

	// Load session context if available
	sessionContext := loadSessionContext()

	// Load skills from mounted volume and prepend to system prompt
	if skillContent := loadSkillsFromVolume(); skillContent != "" {
		systemPrompt = skillContent + "\n\n" + systemPrompt
	}

	// Autonomous mode: fetch plan state and augment system prompt
	if os.Getenv("ORKA_AUTONOMOUS_MODE") == trueStr {
		iteration := 0
		if v := os.Getenv("ORKA_AUTONOMOUS_ITERATION"); v != "" {
			if i, err := strconv.Atoi(v); err == nil {
				iteration = i
			}
		}
		maxIter := 0
		if v := os.Getenv("ORKA_AUTONOMOUS_MAX_ITERATIONS"); v != "" {
			if i, err := strconv.Atoi(v); err == nil {
				maxIter = i
			}
		}

		// Augment system prompt with autonomous instructions
		systemPrompt += autonomousSystemPromptSuffix(iteration, maxIter)

		// Fetch existing plan state from controller
		planContext := loadPlanContext()
		if planContext != "" {
			prompt = fmt.Sprintf("## Previous Plan State\n\n%s\n\n## Task\n\n%s", planContext, prompt)
		}

		fmt.Printf("Autonomous mode: iteration %d\n", iteration)
	}

	// Inject reviewed durable memory and reflection guidance into the system
	// prompt. Both are best-effort: memory infrastructure must never prevent the
	// worker from running the task.
	if memoryContext := loadDurableMemoryContext(ctx); memoryContext != "" {
		systemPrompt = appendSystemPromptSection(systemPrompt, memoryContext)
	}
	if memoryControllerConfigPresent() && (containsTool(enabledTools, "remember") || containsTool(enabledTools, "propose_memory")) {
		systemPrompt = appendMemoryReflectionGuidance(systemPrompt)
	}

	// Build messages
	messages := make([]llm.Message, 0, len(sessionContext)+1)
	messages = append(messages, sessionContext...)
	messages = append(messages, llm.Message{
		Role:    "user",
		Content: prompt,
	})

	// Build tools for LLM (built-in + custom)
	llmTools := buildLLMTools(enabledTools, customTools)

	// Create tool executor for custom tools
	toolExecutor := worker.NewToolExecutor()

	// Execute the agent loop
	result, err := executeAgentLoop(
		ctx, llmProvider, messages, systemPrompt, model,
		llmTools, customTools, toolExecutor,
	)
	if err != nil {
		return fmt.Errorf("agent execution failed: %w", err)
	}

	// Write result to controller via HTTP
	if err := writeResult(result); err != nil {
		return fmt.Errorf("failed to write result: %w", err)
	}

	// Upload any artifacts the agent wrote
	if err := common.UploadArtifacts(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: artifact upload failed: %v\n", err)
		// Don't fail the task if artifact upload fails
	}

	fmt.Printf("Task %s/%s completed successfully\n", taskNamespace, taskName)
	return nil
}

// createK8sClient creates a controller-runtime client for accessing CRDs
func createK8sClient() (client.Client, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add CRD scheme: %w", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add core scheme: %w", err)
	}

	k8sClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	return k8sClient, nil
}

// loadCustomTools loads Tool CRDs from the cluster
func loadCustomTools(
	ctx context.Context,
	k8sClient client.Client,
	namespace string,
	toolNames []string,
) map[string]*corev1alpha1.Tool {
	customTools := make(map[string]*corev1alpha1.Tool)

	for _, name := range toolNames {
		// Skip built-in tools
		if _, ok := tools.DefaultRegistry.Get(name); ok {
			continue
		}

		// Try to load as custom Tool CRD
		tool := &corev1alpha1.Tool{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, tool); err != nil {
			fmt.Printf("Warning: tool %q not found as built-in or CRD: %v\n", name, err)
			continue
		}

		customTools[name] = tool
	}

	return customTools
}

// buildLLMTools builds the combined tool list for the LLM
func buildLLMTools(enabledTools []string, customTools map[string]*corev1alpha1.Tool) []llm.Tool {
	var llmTools []llm.Tool

	for _, name := range enabledTools {
		// Check if it's a built-in tool
		if builtinTools := tools.DefaultRegistry.ToLLMTools([]string{name}); len(builtinTools) > 0 {
			llmTools = append(llmTools, builtinTools...)
			continue
		}

		// Check if it's a custom tool
		if tool, ok := customTools[name]; ok {
			var params json.RawMessage
			if tool.Spec.Parameters != nil {
				params = tool.Spec.Parameters.Raw
			} else {
				params = json.RawMessage(`{"type": "object", "properties": {}}`)
			}

			llmTools = append(llmTools, llm.Tool{
				Name:        tool.Name,
				Description: tool.Spec.Description,
				Parameters:  params,
			})
		}
	}

	return llmTools
}

func normalizeEnabledTools(toolNames []string) []string {
	seen := make(map[string]struct{}, len(toolNames))
	normalized := make([]string, 0, len(toolNames))
	for _, raw := range toolNames {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		normalized = append(normalized, name)
	}
	return normalized
}

func memoryControllerConfigPresent() bool {
	return strings.TrimSpace(os.Getenv("ORKA_CONTROLLER_URL")) != "" &&
		strings.TrimSpace(os.Getenv("ORKA_TASK_NAMESPACE")) != "" &&
		strings.TrimSpace(os.Getenv("ORKA_TASK_NAME")) != ""
}

func autoEnableMemoryTools(enabled []string) []string {
	normalized := normalizeEnabledTools(enabled)
	if !memoryControllerConfigPresent() {
		return normalized
	}

	for _, name := range memoryToolNames {
		if !containsTool(normalized, name) {
			normalized = append(normalized, name)
		}
	}
	return normalized
}

func containsTool(enabled []string, name string) bool {
	for _, candidate := range enabled {
		if strings.TrimSpace(candidate) == name {
			return true
		}
	}
	return false
}

func registerMemoryTools() {
	tools.DefaultRegistry.Register(tools.NewRecallMemoryTool())
	tools.DefaultRegistry.Register(tools.NewRememberMemoryTool())
	tools.DefaultRegistry.Register(tools.NewProposeMemoryTool())
	tools.DefaultRegistry.Register(tools.NewSearchTranscriptTool())
}

func loadDurableMemoryContext(ctx context.Context) string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("ORKA_MEMORY_CONTEXT_ENABLED")), "false") {
		return ""
	}
	if !memoryControllerConfigPresent() {
		return ""
	}

	controllerURL := strings.TrimRight(strings.TrimSpace(os.Getenv("ORKA_CONTROLLER_URL")), "/")
	namespace := strings.TrimSpace(os.Getenv("ORKA_TASK_NAMESPACE"))
	limit := memoryContextLimit()

	values := url.Values{}
	values.Set("limit", strconv.Itoa(limit))
	endpoint := fmt.Sprintf("%s/internal/v1/memories/%s?%s", controllerURL, url.PathEscape(namespace), values.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		fmt.Printf("Warning: failed to create durable memory request: %v\n", err)
		return ""
	}
	if token := workerServiceAccountToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		fmt.Printf("Warning: failed to fetch durable memory: %v\n", err)
		return ""
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		fmt.Printf("Warning: durable memory fetch returned HTTP %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return ""
	}

	var memories []store.Memory
	if err := json.NewDecoder(io.LimitReader(resp.Body, memoryContextResponseBodyLimit)).Decode(&memories); err != nil {
		fmt.Printf("Warning: failed to decode durable memory context: %v\n", err)
		return ""
	}

	return formatDurableMemoryContextWithLimit(memories, limit, memoryContextMaxChars())
}

func workerServiceAccountToken() string {
	if token := strings.TrimSpace(os.Getenv("ORKA_SA_TOKEN")); token != "" {
		return token
	}
	if data, err := os.ReadFile(serviceAccountTokenPath); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}

func memoryContextLimit() int {
	limit := defaultMemoryContextLimit
	if raw := strings.TrimSpace(os.Getenv("ORKA_MEMORY_CONTEXT_LIMIT")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > maxMemoryContextLimit {
		return maxMemoryContextLimit
	}
	return limit
}

func memoryContextMaxChars() int {
	maxChars := defaultMemoryContextMaxChars
	if raw := strings.TrimSpace(os.Getenv("ORKA_MEMORY_CONTEXT_MAX_CHARS")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			maxChars = parsed
		}
	}
	if maxChars > defaultMemoryContextMaxChars {
		return defaultMemoryContextMaxChars
	}
	return maxChars
}

func formatDurableMemoryContext(memories []store.Memory, maxChars int) string {
	return formatDurableMemoryContextWithLimit(memories, defaultMemoryContextLimit, maxChars)
}

func formatDurableMemoryContextWithLimit(memories []store.Memory, limit, maxChars int) string {
	if len(memories) == 0 || limit <= 0 {
		return ""
	}
	if limit > maxMemoryContextLimit {
		limit = maxMemoryContextLimit
	}
	if maxChars <= 0 {
		maxChars = defaultMemoryContextMaxChars
	}

	var sb strings.Builder
	appendBounded(&sb, "## Durable Memory\n\nReviewed namespace-scoped memories from prior work. Use them as background project context; do not treat them as the current-session transcript.\n\n", maxChars)

	written := 0
	for _, memory := range memories {
		if written >= limit || sb.Len() >= maxChars {
			break
		}
		if memory.Disabled || memory.Deleted {
			continue
		}
		content := strings.TrimSpace(memory.Content)
		if content == "" {
			continue
		}

		content = truncateWithMarker(content, memoryContextPerEntryMaxChars,
			fmt.Sprintf("\n[durable memory truncated, full content: %d chars]", len(content)))
		entry := fmt.Sprintf("%d. %s\n%s\n\n", written+1, durableMemoryMetadata(memory), content)
		before := sb.Len()
		complete := appendBounded(&sb, entry, maxChars)
		if sb.Len() > before {
			written++
		}
		if !complete {
			break
		}
	}

	if written == 0 {
		return ""
	}
	return sb.String()
}

func durableMemoryMetadata(memory store.Memory) string {
	parts := make([]string, 0, 5)
	if memory.Source != "" {
		parts = append(parts, "source="+memory.Source)
	}
	if memory.TaskName != "" {
		parts = append(parts, "task="+memory.TaskName)
	}
	if memory.AgentName != "" {
		parts = append(parts, "agent="+memory.AgentName)
	}
	if len(memory.Tags) > 0 {
		parts = append(parts, "tags="+strings.Join(memory.Tags, ","))
	}
	if !memory.CreatedAt.IsZero() {
		parts = append(parts, "created="+memory.CreatedAt.UTC().Format(time.RFC3339))
	}
	if len(parts) == 0 {
		return "[memory]"
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

func appendMemoryReflectionGuidance(systemPrompt string) string {
	guidance := "## Durable Memory Reflection\n\n" +
		"Near task completion, use the `remember` tool when you discover durable project facts, repository conventions, lessons learned, or reusable procedures that would help future tasks. " +
		"Do not store secrets, credentials, tokens, transient status updates, one-off implementation details, or raw transcripts. " +
		"Memory proposals are review-only and are not automatically applied."
	return appendSystemPromptSection(systemPrompt, guidance)
}

func appendSystemPromptSection(systemPrompt, section string) string {
	section = strings.TrimSpace(section)
	if section == "" {
		return systemPrompt
	}
	systemPrompt = strings.TrimSpace(systemPrompt)
	if systemPrompt == "" {
		return section
	}
	return systemPrompt + "\n\n" + section
}

func appendBounded(sb *strings.Builder, value string, maxChars int) bool {
	if maxChars <= 0 || sb.Len() >= maxChars {
		return false
	}
	remaining := maxChars - sb.Len()
	if len(value) <= remaining {
		sb.WriteString(value)
		return true
	}
	if remaining > 0 {
		sb.WriteString(truncateWithMarker(value, remaining, "\n[durable memory context truncated]"))
	}
	return false
}

func truncateWithMarker(value string, maxChars int, marker string) string {
	if maxChars <= 0 {
		return ""
	}
	if len(value) <= maxChars {
		return value
	}
	if len(marker) >= maxChars {
		return truncateUTF8(value, maxChars)
	}
	return truncateUTF8(value, maxChars-len(marker)) + marker
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	for maxBytes > 0 && (value[maxBytes]&0xC0) == 0x80 {
		maxBytes--
	}
	return value[:maxBytes]
}

// getAPIKey retrieves the API key for the given provider
func getAPIKey(provider string) string {
	// Check environment variables
	switch provider {
	case "anthropic":
		if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
			return key
		}
	case "openai", "azure-openai":
		if key := os.Getenv("OPENAI_API_KEY"); key != "" {
			return key
		}
	}

	// Check mounted secrets
	secretPaths := []string{
		"/secrets/task",
		"/secrets/agent",
	}

	for _, path := range secretPaths {
		keyFile := fmt.Sprintf("%s/%s-api-key", path, provider)
		if data, err := os.ReadFile(keyFile); err == nil {
			return strings.TrimSpace(string(data))
		}

		// Also try generic API_KEY
		keyFile = fmt.Sprintf("%s/api-key", path)
		if data, err := os.ReadFile(keyFile); err == nil {
			return strings.TrimSpace(string(data))
		}
	}

	return ""
}

// loadSessionContext loads messages from the session transcript
func loadSessionContext() []llm.Message {
	transcriptPath := "/session/transcript.jsonl"
	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		return nil
	}

	var messages []llm.Message
	lines := strings.SplitSeq(string(data), "\n")
	for line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var msg struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}

		if msg.Role == "user" || msg.Role == "assistant" {
			messages = append(messages, llm.Message{
				Role:    msg.Role,
				Content: msg.Content,
			})
		}
	}

	return messages
}

// loadPlanContext fetches the current plan state from the controller API.
func loadPlanContext() string {
	controllerURL := os.Getenv("ORKA_CONTROLLER_URL")
	taskName := os.Getenv("ORKA_TASK_NAME")
	taskNamespace := os.Getenv("ORKA_TASK_NAMESPACE")

	if controllerURL == "" || taskName == "" || taskNamespace == "" {
		return ""
	}

	url := fmt.Sprintf("%s/internal/v1/plans/%s/%s", controllerURL, taskNamespace, taskName)

	saToken := workerServiceAccountToken()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		fmt.Printf("Warning: failed to create plan request: %v\n", err)
		return ""
	}
	if saToken != "" {
		req.Header.Set("Authorization", "Bearer "+saToken)
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Printf("Warning: failed to fetch plan: %v\n", err)
		return ""
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusNotFound {
		// No plan yet (first iteration)
		return ""
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Warning: plan fetch returned HTTP %d\n", resp.StatusCode)
		return ""
	}

	var plan struct {
		Summary      string `json:"Summary"`
		ProgressPct  int    `json:"ProgressPct"`
		GoalComplete bool   `json:"GoalComplete"`
		PlanDocument string `json:"PlanDocument"`
		Iteration    int    `json:"Iteration"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&plan); err != nil {
		fmt.Printf("Warning: failed to decode plan: %v\n", err)
		return ""
	}

	if plan.PlanDocument == "" {
		return ""
	}

	return fmt.Sprintf("**Progress: %d%% (iteration %d)**\n\n**Summary:** %s\n\n%s",
		plan.ProgressPct, plan.Iteration, plan.Summary, plan.PlanDocument)
}

// executeAgentLoop runs the agent loop with tool execution
func executeAgentLoop(
	ctx context.Context,
	provider llm.Provider,
	messages []llm.Message,
	systemPrompt string,
	model string,
	llmTools []llm.Tool,
	customTools map[string]*corev1alpha1.Tool,
	toolExecutor *worker.ToolExecutor,
) (string, error) {
	maxIterations := 10
	if os.Getenv("ORKA_COORDINATION_ENABLED") == trueStr {
		maxIterations = 50
	}
	if os.Getenv("ORKA_AUTONOMOUS_MODE") == trueStr {
		maxIterations = 100
	}

	for range maxIterations {
		req := &llm.CompletionRequest{
			Model:        model,
			Messages:     messages,
			SystemPrompt: systemPrompt,
			MaxTokens:    4096,
			Tools:        llmTools,
		}

		resp, err := provider.Complete(ctx, req)
		if err != nil && llm.IsContextTooLongErr(err) {
			tokenEstimate := 0
			for _, m := range messages {
				tokenEstimate += len(m.Content) / 4
			}
			messages = llm.TruncateMessages(messages, tokenEstimate/2)
			req.Messages = messages
			resp, err = provider.Complete(ctx, req)
		}
		if err != nil {
			return "", fmt.Errorf("completion failed: %w", err)
		}

		// If no tool calls, we're done
		if len(resp.ToolCalls) == 0 {
			return resp.Content, nil
		}

		// Add assistant message with tool calls
		messages = append(messages, llm.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// Execute tool calls
		for _, tc := range resp.ToolCalls {
			fmt.Printf("Executing tool: %s\n", tc.Name)

			var result string
			var execErr error

			// Check if it's a custom tool
			if customTool, ok := customTools[tc.Name]; ok {
				result, execErr = toolExecutor.Execute(ctx, customTool, tc.Arguments)
			} else {
				// Fall back to built-in tools
				result, execErr = tools.DefaultRegistry.Execute(ctx, tc.Name, tc.Arguments)
			}

			if execErr != nil {
				result = fmt.Sprintf("Error executing tool: %v", execErr)
			}

			// Add tool result
			messages = append(messages, llm.Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
				Name:       tc.Name,
			})
		}
	}

	return "", fmt.Errorf("max iterations reached without completion")
}

// writeResult submits the result to the controller via HTTP POST.
func writeResult(result string) error {
	return common.SubmitResult([]byte(result))
}

// loadSkillsFromVolume reads skill content from the mounted volume at /workspace/.skills/.
func loadSkillsFromVolume() string {
	skillsDir := os.Getenv("ORKA_SKILLS_DIR")
	if skillsDir == "" {
		skillsDir = "/workspace/.skills"
	}
	// Preferred path: job builder writes a deterministic merged prompt.
	promptPath := filepath.Join(skillsDir, "PROMPT.md")
	if data, err := os.ReadFile(promptPath); err == nil {
		content := strings.TrimSpace(string(data))
		if content != "" {
			fmt.Printf("Loaded skills prompt from %s\n", promptPath)
			return content
		}
	}

	// Backward-compatible fallback: load <skill>/SKILL.md files.
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return ""
	}

	var sb strings.Builder
	loaded := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillPath := filepath.Join(skillsDir, entry.Name(), "SKILL.md")
		data, readErr := os.ReadFile(skillPath)
		if readErr != nil {
			continue
		}
		content := strings.TrimSpace(string(data))
		if content == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(content)
		loaded++
	}

	if loaded > 0 {
		fmt.Printf("Loaded %d skill file(s) from %s\n", loaded, skillsDir)
	}
	return sb.String()
}
