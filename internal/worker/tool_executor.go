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

const (
	mcpJSONRPCVersion  = "2.0"
	mcpToolsCallMethod = "tools/call"
)

// ToolExecutor handles execution of custom Tool CRDs via HTTP or MCP-over-HTTP.
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

// Execute executes a Tool CRD by making an HTTP request.
func (e *ToolExecutor) Execute(ctx context.Context, tool *corev1alpha1.Tool, args json.RawMessage) (string, error) {
	prepared, err := e.prepareRequest(ctx, tool, args)
	if err != nil {
		return "", err
	}

	// Configure timeout
	httpClient := e.client
	if prepared.httpConfig.Timeout != nil {
		httpClient = &http.Client{Timeout: prepared.httpConfig.Timeout.Duration}
	}

	// Execute request
	resp, err := httpClient.Do(prepared.request)
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
		return "", fmt.Errorf("tool returned HTTP %d: %s", resp.StatusCode, redactToolHTTPErrorBody(string(respBody), prepared.authToken, prepared.transactionToken))
	}

	if prepared.mcp {
		return decodeMCPToolCallResponse(respBody, prepared.authToken, prepared.transactionToken)
	}

	return string(respBody), nil
}

type preparedToolRequest struct {
	httpConfig       corev1alpha1.HTTPExecution
	request          *http.Request
	authToken        string
	transactionToken string
	mcp              bool
}

func (e *ToolExecutor) prepareRequest(ctx context.Context, tool *corev1alpha1.Tool, args json.RawMessage) (preparedToolRequest, error) {
	var params map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &params); err != nil {
			return preparedToolRequest{}, fmt.Errorf("failed to parse tool arguments: %w", err)
		}
	} else {
		params = make(map[string]any)
	}

	httpConfig, routeHost, err := toolHTTPConfig(tool)
	if err != nil {
		return preparedToolRequest{}, err
	}
	isMCP := isMCPSubstrateActorTool(tool)

	var authToken string
	if httpConfig.AuthSecretRef != nil {
		token, err := e.getSecretKey(ctx, httpConfig.AuthSecretRef.Name, httpConfig.AuthSecretRef.Key)
		if err != nil {
			return preparedToolRequest{}, fmt.Errorf("failed to get auth secret: %w", err)
		}
		authToken = token
	}

	authInject, err := applyToolBodyAuth(params, httpConfig, authToken)
	if err != nil {
		return preparedToolRequest{}, err
	}

	url := httpConfig.URL
	method := httpConfig.Method
	if method == "" {
		method = http.MethodPost
	}
	var body []byte
	if isMCP {
		method = http.MethodPost
		body, err = marshalMCPToolCallRequest(tool, params)
		if err != nil {
			return preparedToolRequest{}, err
		}
	} else {
		url = interpolateToolURL(url, params)
		body, err = json.Marshal(params)
		if err != nil {
			return preparedToolRequest{}, fmt.Errorf("failed to marshal request body: %w", err)
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return preparedToolRequest{}, fmt.Errorf("failed to create request: %w", err)
	}
	if routeHost != "" {
		req.Host = routeHost
	}

	req.Header.Set("Content-Type", "application/json")
	if isMCP {
		req.Header.Set("Accept", "application/json")
	}
	for k, v := range httpConfig.Headers {
		req.Header.Set(k, v)
	}
	if authInject == "header" && authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	transactionToken, err := e.outboundTransactionToken(ctx, tool)
	if err != nil {
		return preparedToolRequest{}, err
	}
	if transactionToken != "" {
		if values, ok := req.Header[http.CanonicalHeaderKey(contexttoken.HeaderName)]; ok && len(values) > 0 {
			return preparedToolRequest{}, fmt.Errorf("tool configured reserved header %q while transaction token propagation is enabled", contexttoken.HeaderName)
		}
		req.Header.Set(contexttoken.HeaderName, transactionToken)
	}

	return preparedToolRequest{
		httpConfig:       httpConfig,
		request:          req,
		authToken:        authToken,
		transactionToken: transactionToken,
		mcp:              isMCP,
	}, nil
}

func toolHTTPConfig(tool *corev1alpha1.Tool) (corev1alpha1.HTTPExecution, string, error) {
	var httpConfig corev1alpha1.HTTPExecution
	if tool != nil && tool.Spec.HTTP != nil {
		httpConfig = *tool.Spec.HTTP
	}
	if !isMCPSubstrateActorTool(tool) {
		if tool == nil || tool.Spec.HTTP == nil {
			return corev1alpha1.HTTPExecution{}, "", fmt.Errorf("http tool endpoint is not configured")
		}
		return httpConfig, "", nil
	}

	httpConfig.URL = strings.TrimSpace(tool.Status.Endpoint)
	if httpConfig.Method == "" {
		httpConfig.Method = http.MethodPost
	}
	routeHost := ""
	if tool.Status.Actor != nil {
		routeHost = strings.TrimSpace(tool.Status.Actor.RouteHost)
	}
	if httpConfig.URL == "" || routeHost == "" {
		return corev1alpha1.HTTPExecution{}, "", fmt.Errorf("MCP tool actor endpoint is not ready")
	}
	return httpConfig, routeHost, nil
}

func isMCPSubstrateActorTool(tool *corev1alpha1.Tool) bool {
	return tool != nil && tool.Spec.MCP != nil && tool.Spec.MCP.SubstrateActor != nil
}

type mcpToolCallRequest struct {
	JSONRPC string                `json:"jsonrpc"`
	ID      string                `json:"id"`
	Method  string                `json:"method"`
	Params  mcpToolCallParameters `json:"params"`
}

type mcpToolCallParameters struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

func marshalMCPToolCallRequest(tool *corev1alpha1.Tool, arguments map[string]any) ([]byte, error) {
	toolName := strings.TrimSpace(tool.Name)
	if toolName == "" {
		return nil, fmt.Errorf("MCP tool name is required")
	}
	body, err := json.Marshal(mcpToolCallRequest{
		JSONRPC: mcpJSONRPCVersion,
		ID:      "1",
		Method:  mcpToolsCallMethod,
		Params: mcpToolCallParameters{
			Name:      toolName,
			Arguments: arguments,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal MCP tool call request: %w", err)
	}
	return body, nil
}

type mcpToolCallResponse struct {
	JSONRPC string           `json:"jsonrpc,omitempty"`
	ID      any              `json:"id,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *mcpJSONRPCError `json:"error,omitempty"`
}

type mcpJSONRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type mcpToolCallResult struct {
	Content []mcpToolContent `json:"content,omitempty"`
	IsError bool             `json:"isError,omitempty"`
}

type mcpToolContent struct {
	Type string          `json:"type,omitempty"`
	Text string          `json:"text,omitempty"`
	Raw  json.RawMessage `json:"-"`
}

func (c *mcpToolContent) UnmarshalJSON(data []byte) error {
	type mcpToolContentAlias struct {
		Type string `json:"type,omitempty"`
		Text string `json:"text,omitempty"`
	}
	var parsed mcpToolContentAlias
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	c.Type = parsed.Type
	c.Text = parsed.Text
	c.Raw = append(json.RawMessage(nil), data...)
	return nil
}

func decodeMCPToolCallResponse(body []byte, secrets ...string) (string, error) {
	var response mcpToolCallResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("failed to parse MCP response: %w", err)
	}
	if response.Error != nil {
		message := response.Error.Message
		if len(response.Error.Data) > 0 {
			message = strings.TrimSpace(message + ": " + string(response.Error.Data))
		}
		return "", fmt.Errorf("MCP tool returned error %d: %s", response.Error.Code, redactToolHTTPErrorBody(message, secrets...))
	}
	if len(response.Result) == 0 {
		return "", fmt.Errorf("MCP response missing result")
	}

	var result mcpToolCallResult
	if err := json.Unmarshal(response.Result, &result); err == nil && (len(result.Content) > 0 || result.IsError) {
		text := mcpToolContentText(result.Content)
		if result.IsError {
			if text == "" {
				text = string(response.Result)
			}
			return "", fmt.Errorf("MCP tool reported error: %s", redactToolHTTPErrorBody(text, secrets...))
		}
		if text != "" {
			return text, nil
		}
	}

	return string(response.Result), nil
}

func mcpToolContentText(content []mcpToolContent) string {
	parts := make([]string, 0, len(content))
	for _, item := range content {
		if item.Type == "text" && item.Text != "" {
			parts = append(parts, item.Text)
			continue
		}
		if len(item.Raw) > 0 {
			parts = append(parts, string(item.Raw))
			continue
		}
		raw, err := json.Marshal(item)
		if err == nil && string(raw) != "{}" {
			parts = append(parts, string(raw))
		}
	}
	return strings.Join(parts, "\n")
}

func applyToolBodyAuth(params map[string]any, httpConfig corev1alpha1.HTTPExecution, authToken string) (string, error) {
	authInject := httpConfig.AuthInject
	if authInject == "" {
		authInject = "header"
	}
	if authInject != "body" || authToken == "" {
		return authInject, nil
	}
	bodyKey := httpConfig.AuthBodyKey
	if bodyKey == "" {
		return "", fmt.Errorf("authBodyKey is required when authInject=body")
	}
	params[bodyKey] = authToken
	return authInject, nil
}

func interpolateToolURL(url string, params map[string]any) string {
	interpolatedKeys := map[string]bool{}
	for key, val := range params {
		placeholder := "{{" + key + "}}"
		if strings.Contains(url, placeholder) {
			url = strings.ReplaceAll(url, placeholder, neturl.PathEscape(fmt.Sprintf("%v", val)))
			interpolatedKeys[key] = true
		}
	}
	for key := range interpolatedKeys {
		delete(params, key)
	}
	return url
}

func redactToolHTTPErrorBody(body string, secrets ...string) string {
	redacted := body
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret == "" {
			continue
		}
		redacted = strings.ReplaceAll(redacted, secret, "[REDACTED]")
	}
	return redacted
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
