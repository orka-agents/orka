/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"errors"
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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/llm"
	_ "github.com/orka-agents/orka/internal/llm/anthropic"
	_ "github.com/orka-agents/orka/internal/llm/openai"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/tracing"
	"github.com/orka-agents/orka/internal/tracing/genai"
	"github.com/orka-agents/orka/internal/worker"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

const (
	defaultMemoryContextLimit      = 5
	maxMemoryContextLimit          = 8
	defaultMemoryContextMaxChars   = 6000
	memoryContextPerEntryMaxChars  = 1200
	memoryContextResponseBodyLimit = 1 << 20
	serviceAccountTokenPath        = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	secretCredentialReadScope      = "orka:secrets:credentials:read"

	durableMemoryContextHeader = "## Durable Memory\n\n" +
		"Reviewed namespace-scoped memories from prior work. " +
		"Use them as background project context; " +
		"do not treat them as the current-session transcript.\n\n"
	durableMemoryReflectionGuidance = "## Durable Memory Reflection\n\n" +
		"Near task completion, use the `remember` tool when you discover durable project facts, " +
		"repository conventions, lessons learned, or reusable procedures that would help future tasks. " +
		"Do not store secrets, credentials, tokens, transient status updates, " +
		"one-off implementation details, or raw transcripts. " +
		"Memory proposals are review-only and are not automatically applied."
)

var memoryToolNames = []string{"recall_memory", "remember", "propose_memory", "search_transcript"}

const modelLoopEventTimeout = 250 * time.Millisecond

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() (err error) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Get configuration from environment
	workerEnv := workerenv.ParseAIWorkerEnv(os.Getenv)
	taskName := workerEnv.TaskName
	taskNamespace := workerEnv.TaskNamespace
	eventRecorder := common.NewHTTPEventRecorderFromEnv()
	defer func() {
		if err != nil {
			common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeWorkerFailed, 0,
				common.WithEventSeverity(events.ExecutionEventSeverityError),
				common.WithEventTaskName(taskName),
				common.WithEventSummary(err.Error()),
			)
			return
		}
		common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeWorkerCompleted, 0,
			common.WithEventTaskName(taskName),
			common.WithEventSummary("AI worker completed"),
		)
	}()
	if err := workerEnv.ValidateRequired(); err != nil {
		return err
	}
	tracingShutdown, err := tracing.Init("orka-ai-worker", workerEnv.EnableTelemetry)
	if err != nil {
		return fmt.Errorf("failed to initialize telemetry: %w", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if shutdownErr := tracingShutdown(shutdownCtx); shutdownErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to shutdown telemetry: %v\n", shutdownErr)
		}
	}()
	if workerEnv.TraceParent != "" {
		ctx = tracing.ExtractContext(ctx, tracing.MapCarrier{
			"traceparent": workerEnv.TraceParent,
			"tracestate":  workerEnv.TraceState,
			"baggage":     workerEnv.TraceBaggage,
		})
	}

	taskAttrs := tracing.TaskAttributes(taskName, taskNamespace, taskNamespace, workerEnv.AgentName, "")
	ctx, taskSpan := tracing.Tracer("orka.worker").Start(ctx, "task.run", trace.WithAttributes(taskAttrs...))
	defer func() {
		if err != nil {
			errType := aiWorkerErrorType(err)
			taskSpan.SetStatus(codes.Error, errType)
			taskSpan.SetAttributes(attribute.String(genai.AttrErrorType, errType))
		}
		taskSpan.End()
	}()

	transactionLogFields := workerenv.TransactionLogFields(
		workerEnv.TransactionID, workerEnv.TransactionProfile,
	)
	fmt.Printf("Worker ai started task=%s/%s%s\n", taskNamespace, taskName, transactionLogFields)
	common.RecordEvent(ctx, eventRecorder, events.ExecutionEventTypeWorkerStarted,
		common.WithEventTaskName(taskName),
		common.WithEventSummary("AI worker started"),
		common.WithEventContent(eventContent(map[string]any{
			"provider": workerEnv.Provider,
			"model":    workerEnv.Model,
		})),
	)
	provider := workerEnv.Provider
	model := workerEnv.Model
	prompt := workerEnv.Prompt
	systemPrompt := workerEnv.SystemPrompt
	baseURL := workerEnv.BaseURL
	azureAPIVersion := workerEnv.AzureAPIVersion

	// Get API key
	apiKey := getAPIKey(provider)
	if apiKey == "" {
		return fmt.Errorf("API key for %s not found", provider)
	}

	// Create LLM provider
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
	if len(workerEnv.Fallbacks) > 0 {
		var fallbacks []llm.FallbackEntry
		for i, fallbackEnv := range workerEnv.Fallbacks {
			if fallbackEnv.Provider == "" || fallbackEnv.APIKey == "" {
				fmt.Printf("Warning: skipping fallback %d: missing provider or API key\n", i)
				continue
			}

			fbProvider, err := llm.NewProvider(fallbackEnv.Provider, llm.ProviderConfig{
				APIKey:          fallbackEnv.APIKey,
				BaseURL:         fallbackEnv.BaseURL,
				ProviderType:    fallbackEnv.Provider,
				AzureAPIVersion: fallbackEnv.AzureAPIVersion,
			})
			if err != nil {
				fmt.Printf("Warning: skipping fallback %d: %v\n", i, err)
				continue
			}

			fallbacks = append(fallbacks, llm.FallbackEntry{
				Provider: llm.NewRetryProvider(fbProvider, 0),
				Model:    fallbackEnv.Model,
			})
		}
		if len(fallbacks) > 0 {
			fp := llm.NewFallbackProvider(llmProvider, fallbacks)
			fp.SetCooldownTracker(llm.NewCooldownTracker())
			llmProvider = fp
		}
	}

	// Wrap with GenAI tracing after retry/fallback composition so spans report the
	// concrete serving provider while still covering the whole logical model call.
	llmProvider = llm.NewTracingProvider(llmProvider)

	// Parse enabled tools
	enabledTools := normalizeEnabledTools(workerEnv.Tools)

	// Create Kubernetes client for Tool CRDs
	k8sClient, err := createK8sClient()
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %w", err)
	}

	coordinationEnv := workerenv.ParseCoordinationEnv(os.Getenv)

	// Register coordination tools if enabled
	if coordinationEnv.Enabled {
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
	if coordinationEnv.AutonomousMode {
		iteration := coordinationEnv.AutonomousIteration
		maxIter := coordinationEnv.AutonomousMaxIterations

		// Augment system prompt with autonomous instructions
		systemPrompt += autonomousSystemPromptSuffix(iteration, maxIter)

		// Fetch existing plan state from controller
		planContext := loadPlanContext()
		resolvedApprovals, err := parseResolvedApprovals(os.Getenv(workerenv.ResolvedApprovals))
		if err != nil {
			return err
		}
		resolvedContext := strings.TrimSpace(formatResolvedApprovalsContext(resolvedApprovals))
		if planContext != "" && resolvedContext != "" {
			prompt = fmt.Sprintf("## Previous Plan State\n\n%s\n\n%s\n\n## Task\n\n%s", planContext, resolvedContext, prompt)
		} else if planContext != "" {
			prompt = fmt.Sprintf("## Previous Plan State\n\n%s\n\n## Task\n\n%s", planContext, prompt)
		} else if resolvedContext != "" {
			prompt = prependResolvedApprovalsContext(prompt, resolvedApprovals)
		}

		fmt.Printf("Autonomous mode: iteration %d\n", iteration)
	}

	// Inject reviewed durable memory and reflection guidance into the system
	// prompt. Both are best-effort: memory infrastructure must never prevent the
	// worker from running the task.
	if memoryContext := loadDurableMemoryContext(ctx); memoryContext != "" {
		systemPrompt = appendSystemPromptSection(systemPrompt, memoryContext)
	}
	if shouldAppendMemoryReflectionGuidance(enabledTools) {
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

	baseToolCtx := &tools.ToolContext{
		Client:    k8sClient,
		Namespace: taskNamespace,
		Tenant:    taskNamespace,
		TaskID:    taskName,
		AuthorizeSecretRead: workerSecretReadAuthorizer(
			k8sClient,
			taskNamespace,
			taskName,
			workerEnv.TransactionID,
		),
		RequireSecretReadAuthorization: workerEnv.TransactionID != "",
	}

	// Execute the agent loop
	result, err := executeAgentLoopWithEvents(
		ctx, llmProvider, messages, systemPrompt, model,
		llmTools, customTools, toolExecutor, eventRecorder, baseToolCtx,
	)
	if err != nil {
		return fmt.Errorf("agent execution failed: %w", err)
	}

	// Write result to controller via HTTP
	if err := writeResult(result); err != nil {
		return fmt.Errorf("failed to write result: %w", err)
	}
	common.RecordEvent(ctx, eventRecorder, events.ExecutionEventTypeResultSubmitted,
		common.WithEventTaskName(taskName),
		common.WithEventSummary("AI worker submitted result"),
		common.WithEventContent(eventContent(map[string]any{"resultLength": len(result)})),
	)

	// Upload any artifacts the agent wrote
	if err := common.UploadArtifacts(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: artifact upload failed: %v\n", err)
		common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeArtifactUploadFailed, 0,
			common.WithEventSeverity(events.ExecutionEventSeverityWarning),
			common.WithEventTaskName(taskName),
			common.WithEventSummary("AI worker artifact upload failed"),
			common.WithEventContent(eventContent(map[string]any{"artifact": "all", "error": err.Error()})),
		)
		// Don't fail the task if artifact upload fails
	} else {
		common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeArtifactUploadCompleted, 0,
			common.WithEventTaskName(taskName),
			common.WithEventSummary("AI worker artifact upload completed"),
			common.WithEventContent(eventContent(map[string]any{"artifact": "all"})),
		)
	}

	fmt.Printf("Task %s/%s completed successfully%s\n", taskNamespace, taskName, transactionLogFields)
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
	if err := batchv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add batch scheme: %w", err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add networking scheme: %w", err)
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
	loaded := make([]*corev1alpha1.Tool, 0, len(toolNames))

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
		bindApprovalAuthRefVersion(ctx, k8sClient, namespace, tool)

		customTools[name] = tool
		loaded = append(loaded, tool)
	}
	registerToolAliases(customTools, loaded)

	return customTools
}

func clearApprovalAuthRefVersion(tool *corev1alpha1.Tool) {
	if tool == nil || tool.Annotations == nil {
		return
	}
	delete(tool.Annotations, approvalAuthRefUIDAnnotation)
	delete(tool.Annotations, approvalAuthRefResourceVersionAnnotation)
	delete(tool.Annotations, legacyApprovalAuthRefUIDAnnotation)
	delete(tool.Annotations, legacyApprovalAuthRefResourceVersionAnnotation)
}

func bindApprovalAuthRefVersion(
	ctx context.Context,
	k8sClient client.Client,
	namespace string,
	tool *corev1alpha1.Tool,
) {
	if tool == nil || tool.Spec.HTTP == nil || tool.Spec.HTTP.AuthSecretRef == nil {
		return
	}
	clearApprovalAuthRefVersion(tool)
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: tool.Spec.HTTP.AuthSecretRef.Name}
	if err := k8sClient.Get(ctx, key, secret); err != nil {
		fmt.Printf(
			"Warning: auth secret %q for tool %q was not available for approval binding: %v\n",
			key.Name,
			tool.Name,
			err,
		)
		return
	}
	if strings.TrimSpace(string(secret.Data[tool.Spec.HTTP.AuthSecretRef.Key])) == "" {
		clearApprovalAuthRefVersion(tool)
		fmt.Printf(
			"Warning: auth secret %q key %q for tool %q is empty; approval binding skipped\n",
			key.Name,
			tool.Spec.HTTP.AuthSecretRef.Key,
			tool.Name,
		)
		return
	}
	if tool.Annotations == nil {
		tool.Annotations = map[string]string{}
	}
	tool.Annotations[approvalAuthRefUIDAnnotation] = string(secret.UID)
	tool.Annotations[approvalAuthRefResourceVersionAnnotation] = secret.ResourceVersion
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
				Name:        advertisedCustomToolName(tool, customTools),
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
	return strings.TrimSpace(os.Getenv(workerenv.ControllerURL)) != "" &&
		strings.TrimSpace(os.Getenv(workerenv.TaskNamespace)) != "" &&
		strings.TrimSpace(os.Getenv(workerenv.TaskName)) != ""
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
	if strings.EqualFold(strings.TrimSpace(os.Getenv(workerenv.MemoryContextEnabled)), "false") {
		return ""
	}
	if !memoryControllerConfigPresent() {
		return ""
	}

	controllerURL := strings.TrimRight(strings.TrimSpace(os.Getenv(workerenv.ControllerURL)), "/")
	namespace := strings.TrimSpace(os.Getenv(workerenv.TaskNamespace))
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
	if token := strings.TrimSpace(os.Getenv(workerenv.ServiceAccountToken)); token != "" {
		return token
	}
	if data, err := os.ReadFile(serviceAccountTokenPath); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}

func memoryContextLimit() int {
	limit := defaultMemoryContextLimit
	if raw := strings.TrimSpace(os.Getenv(workerenv.MemoryContextLimit)); raw != "" {
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
	if raw := strings.TrimSpace(os.Getenv(workerenv.MemoryContextMaxChars)); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			maxChars = parsed
		}
	}
	if maxChars > defaultMemoryContextMaxChars {
		return defaultMemoryContextMaxChars
	}
	return maxChars
}

func shouldAppendMemoryReflectionGuidance(enabledTools []string) bool {
	return memoryControllerConfigPresent() &&
		(containsTool(enabledTools, "remember") || containsTool(enabledTools, "propose_memory"))
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
	appendBounded(&sb, durableMemoryContextHeader, maxChars)

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
	return appendSystemPromptSection(systemPrompt, durableMemoryReflectionGuidance)
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
		if key := os.Getenv(workerenv.AnthropicAPIKey); key != "" {
			return key
		}
	case "openai", "azure-openai":
		if key := os.Getenv(workerenv.OpenAIAPIKey); key != "" {
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
	controllerURL := os.Getenv(workerenv.ControllerURL)
	taskName := os.Getenv(workerenv.TaskName)
	taskNamespace := os.Getenv(workerenv.TaskNamespace)

	if controllerURL == "" || taskName == "" || taskNamespace == "" {
		return ""
	}

	planURL := fmt.Sprintf("%s/internal/v1/plans/%s/%s", controllerURL, taskNamespace, taskName)

	saToken := workerServiceAccountToken()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, planURL, nil)
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
	return executeAgentLoopWithEvents(
		ctx,
		provider,
		messages,
		systemPrompt,
		model,
		llmTools,
		customTools,
		toolExecutor,
		common.NoopEventRecorder{},
	)
}

func executeAgentLoopWithEvents(
	ctx context.Context,
	provider llm.Provider,
	messages []llm.Message,
	systemPrompt string,
	model string,
	llmTools []llm.Tool,
	customTools map[string]*corev1alpha1.Tool,
	toolExecutor *worker.ToolExecutor,
	eventRecorder common.EventRecorder,
	baseToolCtxOpt ...*tools.ToolContext,
) (string, error) {
	var baseToolCtx *tools.ToolContext
	if len(baseToolCtxOpt) > 0 {
		baseToolCtx = baseToolCtxOpt[0]
	}

	coordinationEnv := workerenv.ParseCoordinationEnv(os.Getenv)
	guard := newAnalysisLoopGuard(llmTools, customTools)
	maxIterations := 10
	if coordinationEnv.Enabled {
		maxIterations = 50
	}
	if guard.validationRequired {
		maxIterations = analysisLoopMaxIterations
	}
	if coordinationEnv.AutonomousMode {
		maxIterations = 100
	}
	allowedToolCalls := advertisedToolNames(llmTools)
	if !coordinationEnv.AutonomousMode && approvalToolingRequested(coordinationEnv, allowedToolCalls) {
		return "", fmt.Errorf("human approval tools require autonomous coordination mode")
	}
	baseToolCtx = prepareApprovalToolContext(baseToolCtx, eventRecorder)
	approvalGate, err := newApprovalGateFromEnv(eventRecorder, baseToolCtx)
	if err != nil {
		return "", err
	}

	for iteration := range maxIterations {
		stepCtx, stepSpan := startAgentStepSpan(ctx, iteration, provider, model, llmTools, baseToolCtx)
		req := &llm.CompletionRequest{
			Model:        model,
			Messages:     messages,
			SystemPrompt: systemPrompt,
			MaxTokens:    4096,
			Tools:        llmTools,
		}
		guard.prepareRequest(req, messages, iteration, maxIterations)
		requestToolCalls := advertisedToolNames(req.Tools)
		common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeModelRequestStarted, modelLoopEventTimeout,
			common.WithEventSummary("model request started"),
			common.WithEventContent(eventContent(map[string]any{
				"iteration":    iteration + 1,
				"model":        model,
				"provider":     llm.ProviderTelemetryName(provider),
				"messageCount": len(messages),
				"toolCount":    len(req.Tools),
			})),
		)

		resp, err := provider.Complete(stepCtx, req)
		if err != nil && llm.IsContextTooLongErr(err) {
			tokenEstimate := 0
			for _, m := range messages {
				tokenEstimate += len(m.Content) / 4
			}
			beforeCount := len(messages)
			messages = llm.TruncateMessages(messages, tokenEstimate/2)
			req.Messages = messages
			common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeContextTruncated, modelLoopEventTimeout,
				common.WithEventSeverity(events.ExecutionEventSeverityWarning),
				common.WithEventSummary("model context truncated after provider context limit error"),
				common.WithEventContent(eventContent(map[string]any{
					"iteration":          iteration + 1,
					"messageCountBefore": beforeCount,
					"messageCountAfter":  len(messages),
				})),
			)
			resp, err = provider.Complete(stepCtx, req)
		}
		if err != nil {
			errType := aiWorkerErrorType(err)
			stepSpan.SetStatus(codes.Error, errType)
			stepSpan.SetAttributes(attribute.String(genai.AttrErrorType, errType))
			common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeModelRequestFailed, modelLoopEventTimeout,
				common.WithEventSeverity(events.ExecutionEventSeverityError),
				common.WithEventSummary(err.Error()),
				common.WithEventContent(eventContent(map[string]any{
					"iteration": iteration + 1,
					"model":     model,
					"provider":  llm.ProviderTelemetryName(provider),
				})),
			)
			stepSpan.End()
			return "", fmt.Errorf("completion failed: %w", err)
		}
		originalToolCalls := resp.ToolCalls
		selectedToolCalls := guard.selectToolCalls(originalToolCalls)
		stepSpan.SetAttributes(attribute.Int("agent.step.tool_call_count", len(originalToolCalls)))
		common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeModelRequestCompleted, modelLoopEventTimeout,
			common.WithEventSummary("model request completed"),
			common.WithEventContent(eventContent(map[string]any{
				"iteration":         iteration + 1,
				"model":             firstNonBlankOriginal(resp.Model, model),
				"provider":          firstNonBlankOriginal(resp.Provider, llm.ProviderTelemetryName(provider)),
				"inputTokens":       resp.InputTokens,
				"outputTokens":      resp.OutputTokens,
				"stopReason":        resp.StopReason,
				"toolCalls":         len(originalToolCalls),
				"selectedToolCalls": len(selectedToolCalls),
			})),
		)
		common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeModelMessage, modelLoopEventTimeout,
			common.WithEventSummary("model returned message"),
			common.WithEventContent(eventContent(map[string]any{
				"iteration":         iteration + 1,
				"contentChars":      len([]rune(resp.Content)),
				"toolCalls":         len(originalToolCalls),
				"selectedToolCalls": len(selectedToolCalls),
				"stopReason":        resp.StopReason,
			})),
			common.WithEventContentText(resp.Content),
		)
		recordSkippedAnalysisToolCalls(eventRecorder, originalToolCalls, selectedToolCalls)
		resp.ToolCalls = selectedToolCalls

		if len(resp.ToolCalls) == 0 {
			decision := guard.handleFinalResponse(resp.Content, iteration, maxIterations, messages)
			stepSpan.End()
			if decision.err != nil {
				return "", decision.err
			}
			if decision.retry {
				messages = decision.messages
				continue
			}
			return decision.result, nil
		}

		// Add assistant message with tool calls
		messages = append(messages, llm.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		var approvalResult string
		var done bool
		var continueLoop bool
		var approvalErr error
		messages, approvalResult, done, continueLoop, approvalErr = processApprovalBatch(
			stepCtx,
			messages,
			resp.ToolCalls,
			approvalGate,
			requestToolCalls,
			customTools,
			eventRecorder,
			baseToolCtx,
		)
		if approvalErr != nil {
			stepSpan.RecordError(approvalErr)
			stepSpan.SetStatus(codes.Error, aiWorkerErrorType(approvalErr))
			stepSpan.SetAttributes(attribute.String(genai.AttrErrorType, aiWorkerErrorType(approvalErr)))
			stepSpan.End()
			return "", approvalErr
		}
		if done {
			stepSpan.End()
			return approvalResult, nil
		}
		if continueLoop {
			stepSpan.End()
			continue
		}

		// Execute tool calls.
		validatedFinal := ""
		validationRepair := ""
		var repeatedValidationErr error
		for _, tc := range resp.ToolCalls {
			fmt.Printf("Executing tool: %s\n", tc.Name)

			var result string
			var execErr error
			toolName := strings.TrimSpace(tc.Name)
			common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeToolCallStarted, modelLoopEventTimeout,
				common.WithEventToolName(toolName),
				common.WithEventToolCallID(tc.ID),
				common.WithEventSummary("tool call started"),
				common.WithEventContent(eventContent(map[string]any{
					"toolName":      toolName,
					"toolCallID":    tc.ID,
					"argumentBytes": len(tc.Arguments),
				})),
			)

			result, execErr, cached := executeGuardedLoopTool(
				stepCtx,
				tc,
				toolName,
				requestToolCalls,
				customTools,
				toolExecutor,
				approvalGate,
				baseToolCtx,
				guard,
			)

			inspection := guard.inspectTool(toolName, tc.Arguments, result, execErr)
			execErr = inspection.execErr
			if inspection.final != "" {
				validatedFinal = inspection.final
			}
			if inspection.repair != "" {
				validationRepair = inspection.repair
			}
			if inspection.fatal != nil {
				repeatedValidationErr = inspection.fatal
			}

			if execErr == nil && !cached {
				guard.rememberToolResult(toolName, tc.Arguments, result, customTools[toolName])
			}

			if execErr != nil {
				common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeToolCallFailed, modelLoopEventTimeout,
					common.WithEventSeverity(events.ExecutionEventSeverityError),
					common.WithEventToolName(toolName),
					common.WithEventToolCallID(tc.ID),
					common.WithEventSummary(execErr.Error()),
				)
				result = fmt.Sprintf("Error executing tool: %v", execErr)
			} else {
				summary := "tool call completed"
				if cached {
					summary = "duplicate tool call reused"
				}
				common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeToolCallCompleted, modelLoopEventTimeout,
					common.WithEventToolName(toolName),
					common.WithEventToolCallID(tc.ID),
					common.WithEventSummary(summary),
					common.WithEventContent(eventContent(map[string]any{
						"toolName":     toolName,
						"toolCallID":   tc.ID,
						"resultLength": len(result),
						"cached":       cached,
					})),
				)
			}

			// Add tool result
			messages = append(messages, llm.Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
				Name:       tc.Name,
			})
		}
		validatedFinal, validationRepair = guard.finishValidatedResult(validatedFinal, validationRepair)
		if validatedFinal != "" {
			stepSpan.End()
			return validatedFinal, nil
		}
		if repeatedValidationErr != nil {
			stepSpan.End()
			return "", repeatedValidationErr
		}
		if validationRepair != "" {
			messages = append(messages, llm.Message{Role: "user", Content: validationRepair})
		}
		stepSpan.End()
	}

	return "", fmt.Errorf("max iterations reached without completion")
}

// transientWithoutTimeline reports whether a parsed final answer claims a
// transient verdict without a successful timeline verification.
func transientWithoutTimeline(content string, timelineVerified bool) bool {
	transient, _, err := analysisTransientState(content)
	return err == nil && transient && !timelineVerified
}

func approvalToolingRequested(coordinationEnv workerenv.CoordinationEnv, allowedToolCalls map[string]struct{}) bool {
	if len(coordinationEnv.ApprovalRequiredTools) > 0 {
		return true
	}
	_, requestApprovalAdvertised := allowedToolCalls["request_approval"]
	return requestApprovalAdvertised
}

func aiWorkerErrorType(err error) string {
	if err == nil {
		return ""
	}
	var providerErr *llm.ProviderError
	if errors.As(err, &providerErr) && providerErr.StatusCode > 0 {
		return fmt.Sprintf("llm_status_%d", providerErr.StatusCode)
	}
	return fmt.Sprintf("%T", err)
}

func startAgentStepSpan(
	ctx context.Context,
	iteration int,
	provider llm.Provider,
	model string,
	llmTools []llm.Tool,
	baseToolCtx *tools.ToolContext,
) (context.Context, trace.Span) {
	attrs := []attribute.KeyValue{
		attribute.Int("agent.step.iteration", iteration+1),
		attribute.Int("agent.step.available_tools", len(llmTools)),
	}
	if model != "" {
		attrs = append(attrs, attribute.String(genai.AttrRequestModel, model))
	}
	if providerName := llm.ProviderTelemetryName(provider); providerName != "" {
		attrs = append(attrs, attribute.String(genai.AttrProviderName, providerName))
	}
	if baseToolCtx != nil {
		tenant := baseToolCtx.Tenant
		if tenant == "" {
			tenant = baseToolCtx.Namespace
		}
		attrs = append(attrs, tracing.TaskAttributes(baseToolCtx.TaskID, baseToolCtx.Namespace, tenant, "", "")...)
	}
	return tracing.Tracer("orka.agent").Start(ctx, "agent.step", trace.WithAttributes(attrs...))
}

func advertisedToolNames(llmTools []llm.Tool) map[string]struct{} {
	names := make(map[string]struct{}, len(llmTools))
	for _, tool := range llmTools {
		if name := strings.TrimSpace(tool.Name); name != "" {
			names[name] = struct{}{}
		}
	}
	return names
}

func eventContent(values map[string]any) json.RawMessage {
	data, err := json.Marshal(values)
	if err != nil {
		return nil
	}
	return json.RawMessage(data)
}

// firstNonBlankOriginal returns the original value for the first non-blank string.
// Event metadata should preserve provider-supplied model IDs exactly while
// still treating whitespace-only values as empty.
func firstNonBlankOriginal(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// writeResult submits the result to the controller via HTTP POST.
func writeResult(result string) error {
	return common.SubmitResult([]byte(result))
}

func workerSecretReadAuthorizer(
	k8sClient client.Client,
	taskNamespace string,
	taskName string,
	transactionID string,
) func(context.Context, string, string) *tools.ChatToolError {
	return func(ctx context.Context, namespace, secretName string) *tools.ChatToolError {
		if strings.TrimSpace(transactionID) == "" {
			return nil
		}
		if k8sClient == nil {
			return workerSecretReadError(
				"missing Kubernetes client for secret credential authorization",
				"Run without git credentials or provide a transaction authorized for credential use",
			)
		}
		var task corev1alpha1.Task
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: taskName, Namespace: taskNamespace}, &task); err != nil {
			return workerSecretReadError(
				fmt.Sprintf("load task transaction for secret credential authorization: %v", err),
				"Retry after the task transaction metadata is available",
			)
		}
		tx := task.Spec.Transaction
		if tx == nil {
			return workerSecretReadError(
				"task transaction metadata is required for secret credential authorization",
				"Use a transaction token that grants credential access",
			)
		}
		if !tools.TransactionHasScope(tx, secretCredentialReadScope) {
			return workerSecretReadError(
				fmt.Sprintf(
					"missing required scope %q for git secret %s/%s",
					secretCredentialReadScope,
					namespace,
					secretName,
				),
				"Use a transaction token that includes the secret credential read scope",
			)
		}
		if want := strings.TrimSpace(tx.Context["namespace"]); want != "" && namespace != want {
			return workerSecretReadError(
				fmt.Sprintf("secret namespace %q does not match transaction context %q", namespace, want),
				"Use a git secret in the transaction namespace",
			)
		}
		if want := strings.TrimSpace(tx.Context["secret"]); want != "" && secretName != want {
			return workerSecretReadError(
				fmt.Sprintf("git secret %q does not match transaction context %q", secretName, want),
				"Use the git secret authorized by the transaction context",
			)
		}
		return nil
	}
}

func workerSecretReadError(message, suggestion string) *tools.ChatToolError {
	return &tools.ChatToolError{
		Type:       "authorization_failed",
		Message:    message,
		Suggestion: suggestion,
	}
}

// loadSkillsFromVolume reads skill content from the mounted volume at /workspace/.skills/.
func loadSkillsFromVolume() string {
	skillsDir := os.Getenv(workerenv.SkillsDir)
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
