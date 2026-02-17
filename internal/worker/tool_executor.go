/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

// ToolExecutor handles execution of custom Tool CRDs via HTTP
type ToolExecutor struct {
	client     *http.Client
	secretPath string
	namespace  string
	k8sClient  kubernetes.Interface
}

// NewToolExecutor creates a new tool executor
func NewToolExecutor() *ToolExecutor {
	namespace := os.Getenv("ORKA_TASK_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}

	// Create Kubernetes client for secret access
	var k8sClient kubernetes.Interface
	if config, err := rest.InClusterConfig(); err == nil {
		k8sClient, _ = kubernetes.NewForConfig(config)
	}

	return &ToolExecutor{
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		secretPath: "/secrets/tools",
		namespace:  namespace,
		k8sClient:  k8sClient,
	}
}

// Execute executes a Tool CRD by making an HTTP request
func (e *ToolExecutor) Execute(ctx context.Context, tool *corev1alpha1.Tool, args json.RawMessage) (string, error) {
	// Parse the arguments into a map for potential body injection
	var params map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &params); err != nil {
			return "", fmt.Errorf("failed to parse tool arguments: %w", err)
		}
	} else {
		params = make(map[string]any)
	}

	// Get auth token if configured
	var authToken string
	if tool.Spec.HTTP.AuthSecretRef != nil {
		token, err := e.getSecretKey(ctx, tool.Spec.HTTP.AuthSecretRef.Name, tool.Spec.HTTP.AuthSecretRef.Key)
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

	// Interpolate URL path parameters
	url := tool.Spec.HTTP.URL
	interpolatedKeys := map[string]bool{}
	for key, val := range params {
		placeholder := "{{" + key + "}}"
		if strings.Contains(url, placeholder) {
			url = strings.ReplaceAll(url, placeholder, fmt.Sprintf("%v", val))
			interpolatedKeys[key] = true
		}
	}

	// Remove interpolated keys from body params
	for key := range interpolatedKeys {
		delete(params, key)
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
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
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
	httpClient := e.client
	if tool.Spec.HTTP.Timeout != nil {
		httpClient = &http.Client{Timeout: tool.Spec.HTTP.Timeout.Duration}
	}

	// Execute request
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("tool request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

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

// getSecretKey reads a key from a secret (mounted path or Kubernetes API)
func (e *ToolExecutor) getSecretKey(ctx context.Context, secretName, key string) (string, error) {
	// Try mounted secret paths first
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

	// Fall back to Kubernetes API
	if e.k8sClient != nil {
		secret, err := e.k8sClient.CoreV1().Secrets(e.namespace).Get(ctx, secretName, metav1.GetOptions{})
		if err == nil {
			if data, ok := secret.Data[key]; ok {
				return strings.TrimSpace(string(data)), nil
			}
		}
	}

	return "", fmt.Errorf("secret %s/%s not found", secretName, key)
}
