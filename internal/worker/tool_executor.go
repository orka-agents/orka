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
	neturl "net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/contexttoken"
	"github.com/sozercan/orka/internal/workerenv"
)

// ToolExecutor handles execution of custom Tool CRDs via HTTP
type ToolExecutor struct {
	client     *http.Client
	secretPath string
	namespace  string
	k8sClient  kubernetes.Interface

	ttsMu        sync.Mutex
	ttsClient    *contexttoken.KontxtTTSClient
	ttsClientKey string
}

// NewToolExecutor creates a new tool executor
func NewToolExecutor() *ToolExecutor {
	namespace := os.Getenv(workerenv.TaskNamespace)
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
			url = strings.ReplaceAll(url, placeholder, neturl.PathEscape(fmt.Sprintf("%v", val)))
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
	transactionToken, err := e.outboundTransactionToken(ctx, tool)
	if err != nil {
		return "", err
	}
	if transactionToken != "" {
		if values, ok := req.Header[http.CanonicalHeaderKey(contexttoken.HeaderName)]; ok && len(values) > 0 {
			return "", fmt.Errorf("tool configured reserved header %q while transaction token propagation is enabled", contexttoken.HeaderName)
		}
		req.Header.Set(contexttoken.HeaderName, transactionToken)
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
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB limit
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

func (e *ToolExecutor) outboundTransactionToken(ctx context.Context, tool *corev1alpha1.Tool) (string, error) {
	ttsURL := strings.TrimSpace(os.Getenv(workerenv.ContextTokenTTSURL))
	if ttsURL == "" {
		return existingTransactionToken()
	}
	ttsConfig, err := contexttoken.NewTTSConfig(
		ttsURL,
		os.Getenv(workerenv.ContextTokenTTSAudience),
		os.Getenv(workerenv.ContextTokenTTSTimeout),
		os.Getenv(workerenv.ContextTokenTTSTokenSource),
		"",
		os.Getenv(workerenv.ContextTokenToolTokenTTL),
	)
	if err != nil {
		return "", err
	}
	if !ttsConfig.Enabled() {
		return existingTransactionToken()
	}
	subjectToken, err := outboundTTSSubjectToken(ttsConfig.TokenSource)
	if err != nil {
		return "", err
	}
	scope := strings.TrimSpace(os.Getenv(workerenv.ContextTokenOutboundScope))
	if scope == "" {
		scope = strings.TrimSpace(os.Getenv(workerenv.TransactionScope))
	}
	if scope == "" {
		return "", fmt.Errorf("%s or %s is required when %s is set", workerenv.ContextTokenOutboundScope, workerenv.TransactionScope, workerenv.ContextTokenTTSURL)
	}
	if err := validateOutboundTransactionScope(scope); err != nil {
		return "", err
	}
	subjectTokenType := strings.TrimSpace(os.Getenv(workerenv.ContextTokenSubjectTokenType))
	if subjectTokenType == "" {
		subjectTokenType = contexttoken.SubjectTokenTypeForSource(ttsConfig.TokenSource)
	}
	requestDetails := map[string]any{
		"operation": "httpTool",
		"tool":      tool.Name,
		"namespace": e.namespace,
	}
	if taskName := strings.TrimSpace(os.Getenv(workerenv.TaskName)); taskName != "" {
		requestDetails["task"] = taskName
	}
	ttsClient, err := e.outboundTTSClient(ttsConfig)
	if err != nil {
		return "", err
	}
	token, err := ttsClient.Exchange(ctx, contexttoken.ExchangeRequest{
		SubjectToken:     subjectToken,
		SubjectTokenType: subjectTokenType,
		Scope:            scope,
		RequestedTTL:     ttsConfig.ToolTokenTTL,
		RequestDetails:   requestDetails,
	})
	if err != nil {
		return "", fmt.Errorf("token exchange failed: %w", err)
	}
	return token, nil
}

func existingTransactionToken() (string, error) {
	if token, ok, err := workerenv.ReadTokenFileEnv(workerenv.TransactionTokenFile, "transaction token"); ok || err != nil {
		return token, err
	}
	return "", nil
}

func validateOutboundTransactionScope(scope string) error {
	requested := strings.Fields(scope)
	if len(requested) == 0 {
		return fmt.Errorf("outbound transaction scope is required")
	}
	parentScope := strings.TrimSpace(os.Getenv(workerenv.TransactionScopes))
	if parentScope == "" {
		parentScope = strings.TrimSpace(os.Getenv(workerenv.TransactionScope))
	}
	parent := strings.Fields(parentScope)
	if len(parent) == 0 {
		return fmt.Errorf("parent transaction scopes are required for outbound token exchange")
	}
	for _, child := range requested {
		if !slices.Contains(parent, child) {
			return fmt.Errorf("outbound transaction scope %q is not present in parent transaction scopes", child)
		}
	}
	return nil
}

func outboundTTSSubjectToken(tokenSource string) (string, error) {
	switch tokenSource {
	case contexttoken.TTSTokenSourceIncoming:
		if token, ok, err := workerenv.ReadTokenFileEnv(workerenv.ContextTokenSubjectTokenFile, "context token subject token"); ok || err != nil {
			return token, err
		}
		if token, ok, err := workerenv.ReadTokenFileEnv(workerenv.TransactionTokenFile, "transaction token"); ok || err != nil {
			return token, err
		}
		return "", fmt.Errorf("%s or %s is required when %s uses %q", workerenv.ContextTokenSubjectTokenFile, workerenv.TransactionTokenFile, workerenv.ContextTokenTTSTokenSource, tokenSource)
	case contexttoken.TTSTokenSourceServiceAccount:
		return serviceAccountSubjectToken()
	case contexttoken.TTSTokenSourceNone:
		return "", fmt.Errorf("context token TTS token source %q does not provide a subject token", tokenSource)
	default:
		return "", fmt.Errorf("unsupported context token TTS token source %q", tokenSource)
	}
}

func serviceAccountSubjectToken() (string, error) {
	if token := strings.TrimSpace(os.Getenv(workerenv.ServiceAccountToken)); token != "" {
		return token, nil
	}
	return workerenv.ReadTokenFile(workerenv.ServiceAccountTokenFile, "service account token")
}

func (e *ToolExecutor) outboundTTSClient(cfg contexttoken.TTSConfig) (*contexttoken.KontxtTTSClient, error) {
	key := outboundTTSClientKey(cfg)

	e.ttsMu.Lock()
	defer e.ttsMu.Unlock()

	if e.ttsClient != nil && e.ttsClientKey == key {
		return e.ttsClient, nil
	}

	ttsClient, err := contexttoken.NewKontxtTTSClient(cfg)
	if err != nil {
		return nil, err
	}
	e.ttsClient = ttsClient
	e.ttsClientKey = key
	return ttsClient, nil
}

func outboundTTSClientKey(cfg contexttoken.TTSConfig) string {
	return strings.Join([]string{
		strings.TrimRight(strings.TrimSpace(cfg.URL), "/"),
		strings.TrimSpace(cfg.Audience),
		cfg.Timeout.String(),
		strings.TrimSpace(cfg.TokenSource),
		cfg.ChildTokenTTL.String(),
		cfg.ToolTokenTTL.String(),
	}, "\x00")
}
