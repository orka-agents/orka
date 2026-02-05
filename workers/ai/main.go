/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/sozercan/mercan/internal/llm"
	_ "github.com/sozercan/mercan/internal/llm/anthropic"
	_ "github.com/sozercan/mercan/internal/llm/openai"
	"github.com/sozercan/mercan/internal/tools"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	// Get configuration from environment
	taskName := os.Getenv("MERCAN_TASK_NAME")
	taskNamespace := os.Getenv("MERCAN_TASK_NAMESPACE")
	resultConfigMap := os.Getenv("MERCAN_RESULT_CONFIGMAP")

	provider := os.Getenv("MERCAN_AI_PROVIDER")
	model := os.Getenv("MERCAN_AI_MODEL")
	prompt := os.Getenv("MERCAN_AI_PROMPT")
	systemPrompt := os.Getenv("MERCAN_AI_SYSTEM_PROMPT")
	toolsStr := os.Getenv("MERCAN_AI_TOOLS")
	baseURL := os.Getenv("MERCAN_AI_BASE_URL")

	if provider == "" {
		return fmt.Errorf("MERCAN_AI_PROVIDER is required")
	}
	if model == "" {
		return fmt.Errorf("MERCAN_AI_MODEL is required")
	}
	if prompt == "" {
		return fmt.Errorf("MERCAN_AI_PROMPT is required")
	}

	// Get API key
	apiKey := getAPIKey(provider)
	if apiKey == "" {
		return fmt.Errorf("API key for %s not found", provider)
	}

	// Create LLM provider
	llmProvider, err := llm.NewProvider(provider, llm.ProviderConfig{
		APIKey:  apiKey,
		BaseURL: baseURL,
	})
	if err != nil {
		return fmt.Errorf("failed to create LLM provider: %w", err)
	}

	// Parse enabled tools
	var enabledTools []string
	if toolsStr != "" {
		enabledTools = strings.Split(toolsStr, ",")
	}

	// Load session context if available
	sessionContext := loadSessionContext()

	// Build messages
	messages := []llm.Message{}
	messages = append(messages, sessionContext...)
	messages = append(messages, llm.Message{
		Role:    "user",
		Content: prompt,
	})

	// Build tools for LLM
	var llmTools []llm.Tool
	if len(enabledTools) > 0 {
		llmTools = tools.DefaultRegistry.ToLLMTools(enabledTools)
	}

	// Execute the agent loop
	result, err := executeAgentLoop(ctx, llmProvider, messages, systemPrompt, model, llmTools, enabledTools)
	if err != nil {
		return fmt.Errorf("agent execution failed: %w", err)
	}

	// Write result to ConfigMap
	if err := writeResult(ctx, taskNamespace, resultConfigMap, result); err != nil {
		return fmt.Errorf("failed to write result: %w", err)
	}

	fmt.Printf("Task %s/%s completed successfully\n", taskNamespace, taskName)
	return nil
}

// getAPIKey retrieves the API key for the given provider
func getAPIKey(provider string) string {
	// Check environment variables
	switch provider {
	case "anthropic":
		if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
			return key
		}
	case "openai":
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
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
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

// executeAgentLoop runs the agent loop with tool execution
func executeAgentLoop(
	ctx context.Context,
	provider llm.Provider,
	messages []llm.Message,
	systemPrompt string,
	model string,
	llmTools []llm.Tool,
	enabledTools []string,
) (string, error) {
	maxIterations := 10

	for i := 0; i < maxIterations; i++ {
		req := &llm.CompletionRequest{
			Model:        model,
			Messages:     messages,
			SystemPrompt: systemPrompt,
			MaxTokens:    4096,
			Tools:        llmTools,
		}

		resp, err := provider.Complete(ctx, req)
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

			result, err := tools.DefaultRegistry.Execute(ctx, tc.Name, tc.Arguments)
			if err != nil {
				result = fmt.Sprintf("Error executing tool: %v", err)
			}

			// Add tool result
			messages = append(messages, llm.Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
				Name:       tc.Name,
			})
		}

		// Check stop reason
		if resp.StopReason == "end_turn" || resp.StopReason == "stop" {
			return resp.Content, nil
		}
	}

	return "", fmt.Errorf("max iterations reached without completion")
}

// writeResult writes the result to a ConfigMap
func writeResult(ctx context.Context, namespace, name, result string) error {
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create clientset: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"mercan.ai/result": "true",
			},
		},
		Data: map[string]string{
			"result": result,
		},
	}

	_, err = clientset.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		// Try update if create fails
		_, err = clientset.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{})
	}

	return err
}
