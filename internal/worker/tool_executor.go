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

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

// ToolExecutor handles execution of custom Tool CRDs via HTTP
type ToolExecutor struct {
	client     *http.Client
	secretPath string
}

// NewToolExecutor creates a new tool executor
func NewToolExecutor() *ToolExecutor {
	return &ToolExecutor{
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		secretPath: "/secrets/tools",
	}
}

// Execute executes a Tool CRD by making an HTTP request
func (e *ToolExecutor) Execute(ctx context.Context, tool *corev1alpha1.Tool, args json.RawMessage) (string, error) {
	// Parse the arguments into a map for potential body injection
	var params map[string]interface{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &params); err != nil {
			return "", fmt.Errorf("failed to parse tool arguments: %w", err)
		}
	} else {
		params = make(map[string]interface{})
	}

	// Get auth token if configured
	var authToken string
	if tool.Spec.HTTP.AuthSecretRef != nil {
		token, err := e.getSecretKey(tool.Spec.HTTP.AuthSecretRef.Name, tool.Spec.HTTP.AuthSecretRef.Key)
		if err != nil {
			return "", fmt.Errorf("failed to get auth secret: %w", err)
		}
		authToken = token
	}

	// Handle body-based auth injection
	authInject := tool.Spec.HTTP.AuthInject
	if authInject == "" {
		authInject = "header" // default
	}

	if authInject == "body" && authToken != "" {
		bodyKey := tool.Spec.HTTP.AuthBodyKey
		if bodyKey == "" {
			return "", fmt.Errorf("authBodyKey is required when authInject=body")
		}
		params[bodyKey] = authToken
	}

	// Build request body
	body, err := json.Marshal(params)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request body: %w", err)
	}

	// Determine HTTP method
	method := tool.Spec.HTTP.Method
	if method == "" {
		method = http.MethodPost
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, method, tool.Spec.HTTP.URL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// Set default content type for JSON
	req.Header.Set("Content-Type", "application/json")

	// Add configured headers
	for k, v := range tool.Spec.HTTP.Headers {
		req.Header.Set(k, v)
	}

	// Handle header-based auth injection
	if authInject == "header" && authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	// Configure timeout
	if tool.Spec.HTTP.Timeout != nil {
		e.client.Timeout = tool.Spec.HTTP.Timeout.Duration
	}

	// Execute request
	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("tool request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	// Check for HTTP errors
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("tool returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return string(respBody), nil
}

// getSecretKey reads a key from a mounted secret
func (e *ToolExecutor) getSecretKey(secretName, key string) (string, error) {
	// Try multiple secret paths
	paths := []string{
		fmt.Sprintf("%s/%s/%s", e.secretPath, secretName, key),
		fmt.Sprintf("/secrets/task/%s", key),
		fmt.Sprintf("/secrets/agent/%s", key),
	}

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(data)), nil
		}
	}

	return "", fmt.Errorf("secret %s/%s not found", secretName, key)
}
