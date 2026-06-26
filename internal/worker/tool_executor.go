/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package worker

import (
	"bufio"
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
	mcpJSONRPCVersion                = "2.0"
	mcpProtocolVersion               = "2025-06-18"
	mcpProtocolVersionHeader         = "MCP-Protocol-Version"
	mcpSessionIDHeader               = "Mcp-Session-Id"
	mcpToolCallRequestID             = "1"
	mcpInitializeRequestID           = "initialize"
	mcpSessionTerminateTimeout       = 10 * time.Second
	mcpToolsCallMethod               = "tools/call"
	mcpInitializeMethod              = "initialize"
	mcpInitializedNotificationMethod = "notifications/initialized"
	toolIdempotencyKeyHeader         = "Idempotency-Key"
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

	if prepared.mcp {
		return e.executeMCPToolCall(ctx, httpClient, prepared)
	}

	respBody, err := executeToolHTTPRequest(httpClient, prepared.request, prepared.authToken, prepared.transactionToken)
	if err != nil {
		return "", err
	}

	return string(respBody), nil
}

func executeToolHTTPRequest(httpClient *http.Client, req *http.Request, secrets ...string) ([]byte, error) {
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tool request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB limit
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tool returned HTTP %d: %s", resp.StatusCode, redactToolHTTPErrorBody(string(respBody), secrets...))
	}
	return respBody, nil
}

type toolIdempotencyKeyContextKey struct{}

// WithToolIdempotencyKey attaches a trusted idempotency key for HTTP tool execution.
func WithToolIdempotencyKey(ctx context.Context, key string) context.Context {
	key = strings.TrimSpace(key)
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, toolIdempotencyKeyContextKey{}, key)
}

func toolIdempotencyKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	key, _ := ctx.Value(toolIdempotencyKeyContextKey{}).(string)
	return strings.TrimSpace(key)
}

type preparedToolRequest struct {
	httpConfig       corev1alpha1.HTTPExecution
	request          *http.Request
	authToken        string
	transactionToken string
	mcp              bool
}

func decodeToolArguments(args json.RawMessage) (map[string]any, error) {
	if len(bytes.TrimSpace(args)) == 0 {
		return make(map[string]any), nil
	}
	var params map[string]any
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	if err := dec.Decode(&params); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("trailing data")
	}
	return params, nil
}

func (e *ToolExecutor) prepareRequest(ctx context.Context, tool *corev1alpha1.Tool, args json.RawMessage) (preparedToolRequest, error) {
	params, err := decodeToolArguments(args)
	if err != nil {
		return preparedToolRequest{}, fmt.Errorf("failed to parse tool arguments: %w", err)
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

	authInject := toolAuthInject(httpConfig)
	if isMCP && authInject == "body" && authToken != "" {
		return preparedToolRequest{}, fmt.Errorf("MCP tools do not support authInject=body")
	}
	authInject, err = applyToolBodyAuth(params, httpConfig, authToken)
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
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set(mcpProtocolVersionHeader, mcpProtocolVersion)
	}
	for k, v := range httpConfig.Headers {
		req.Header.Set(k, v)
	}
	approvalIdempotencyKey := toolIdempotencyKeyFromContext(ctx)
	if approvalIdempotencyKey != "" {
		if existing := strings.TrimSpace(req.Header.Get(toolIdempotencyKeyHeader)); existing != "" && existing != approvalIdempotencyKey {
			return preparedToolRequest{}, fmt.Errorf("tool configured reserved header %q while approval idempotency is enabled", toolIdempotencyKeyHeader)
		}
		if isMCP {
			req.Header.Del(toolIdempotencyKeyHeader)
		} else {
			req.Header.Set(toolIdempotencyKeyHeader, approvalIdempotencyKey)
		}
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

type mcpInitializeRequest struct {
	JSONRPC string                  `json:"jsonrpc"`
	ID      string                  `json:"id"`
	Method  string                  `json:"method"`
	Params  mcpInitializeParameters `json:"params"`
}

type mcpInitializeParameters struct {
	ProtocolVersion string                  `json:"protocolVersion"`
	Capabilities    map[string]any          `json:"capabilities"`
	ClientInfo      mcpInitializeClientInfo `json:"clientInfo"`
}

type mcpInitializeClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type mcpNotificationRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
}

func marshalMCPToolCallRequest(tool *corev1alpha1.Tool, arguments map[string]any) ([]byte, error) {
	toolName := strings.TrimSpace(tool.Name)
	if toolName == "" {
		return nil, fmt.Errorf("MCP tool name is required")
	}
	body, err := json.Marshal(mcpToolCallRequest{
		JSONRPC: mcpJSONRPCVersion,
		ID:      mcpToolCallRequestID,
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

func marshalMCPInitializeRequest() ([]byte, error) {
	body, err := json.Marshal(mcpInitializeRequest{
		JSONRPC: mcpJSONRPCVersion,
		ID:      mcpInitializeRequestID,
		Method:  mcpInitializeMethod,
		Params: mcpInitializeParameters{
			ProtocolVersion: mcpProtocolVersion,
			Capabilities:    map[string]any{},
			ClientInfo: mcpInitializeClientInfo{
				Name:    "orka",
				Version: "dev",
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal MCP initialize request: %w", err)
	}
	return body, nil
}

func marshalMCPInitializedNotification() ([]byte, error) {
	body, err := json.Marshal(mcpNotificationRequest{
		JSONRPC: mcpJSONRPCVersion,
		Method:  mcpInitializedNotificationMethod,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal MCP initialized notification: %w", err)
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

type mcpInitializeResult struct {
	ProtocolVersion string `json:"protocolVersion,omitempty"`
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

func (e *ToolExecutor) executeMCPToolCall(ctx context.Context, httpClient *http.Client, prepared preparedToolRequest) (result string, err error) {
	sessionID, protocolVersion, err := e.initializeMCP(ctx, httpClient, prepared)
	if err != nil {
		return "", err
	}
	if sessionID != "" {
		defer func() {
			_ = e.terminateMCPSession(ctx, httpClient, prepared, sessionID, protocolVersion)
		}()
		prepared.request.Header.Set(mcpSessionIDHeader, sessionID)
	}
	prepared.request.Header.Set(mcpProtocolVersionHeader, protocolVersion)
	if err := e.sendMCPInitializedNotification(ctx, httpClient, prepared, sessionID, protocolVersion); err != nil {
		return "", err
	}
	if key := toolIdempotencyKeyFromContext(prepared.request.Context()); key != "" {
		prepared.request.Header.Set(toolIdempotencyKeyHeader, key)
	}
	respBody, err := executeMCPHTTPRequest(httpClient, prepared.request, mcpToolCallRequestID, prepared.authToken, prepared.transactionToken, sessionID)
	if err != nil {
		return "", err
	}
	return decodeMCPToolCallResponse(respBody, prepared.authToken, prepared.transactionToken, sessionID)
}

func (e *ToolExecutor) initializeMCP(ctx context.Context, httpClient *http.Client, prepared preparedToolRequest) (string, string, error) {
	body, err := marshalMCPInitializeRequest()
	if err != nil {
		return "", "", err
	}
	req, err := newMCPRequest(ctx, prepared, body, mcpProtocolVersion)
	if err != nil {
		return "", "", fmt.Errorf("failed to create MCP initialize request: %w", err)
	}
	resp, err := executeMCPHTTPRequestWithResponse(httpClient, req, mcpInitializeRequestID, prepared.authToken, prepared.transactionToken)
	sessionID := strings.TrimSpace(resp.header.Get(mcpSessionIDHeader))
	if err != nil {
		if sessionID != "" {
			_ = e.terminateMCPSession(ctx, httpClient, prepared, sessionID, mcpProtocolVersion)
		}
		return "", "", fmt.Errorf("MCP initialize failed: %w", err)
	}
	protocolVersion, err := mcpInitializedProtocolVersion(resp.body, prepared.authToken, prepared.transactionToken, sessionID)
	if err != nil {
		if sessionID != "" {
			_ = e.terminateMCPSession(ctx, httpClient, prepared, sessionID, mcpProtocolVersion)
		}
		return "", "", fmt.Errorf("MCP initialize failed: %w", err)
	}
	return sessionID, protocolVersion, nil
}

func (e *ToolExecutor) sendMCPInitializedNotification(ctx context.Context, httpClient *http.Client, prepared preparedToolRequest, sessionID string, protocolVersion string) error {
	body, err := marshalMCPInitializedNotification()
	if err != nil {
		return err
	}
	req, err := newMCPRequest(ctx, prepared, body, protocolVersion)
	if err != nil {
		return fmt.Errorf("failed to create MCP initialized notification: %w", err)
	}
	if sessionID != "" {
		req.Header.Set(mcpSessionIDHeader, sessionID)
	}
	if _, err := executeMCPHTTPRequestWithResponse(httpClient, req, "", prepared.authToken, prepared.transactionToken, sessionID); err != nil {
		return fmt.Errorf("MCP initialized notification failed: %w", err)
	}
	return nil
}

func (e *ToolExecutor) terminateMCPSession(ctx context.Context, httpClient *http.Client, prepared preparedToolRequest, sessionID, protocolVersion string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpSessionTerminateTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cleanupCtx, http.MethodDelete, prepared.request.URL.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to create MCP session termination request: %w", err)
	}
	req.Host = prepared.request.Host
	req.Header = prepared.request.Header.Clone()
	if toolIdempotencyKeyFromContext(prepared.request.Context()) != "" {
		req.Header.Del(toolIdempotencyKeyHeader)
	}
	req.Header.Del("Content-Type")
	req.Header.Set(mcpSessionIDHeader, sessionID)
	req.Header.Set(mcpProtocolVersionHeader, mcpProtocolHeaderValue(protocolVersion))

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("MCP session termination request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB limit
	if err != nil {
		return fmt.Errorf("failed to read MCP session termination response: %w", err)
	}
	if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil
	}
	return fmt.Errorf(
		"MCP session termination returned HTTP %d: %s",
		resp.StatusCode,
		redactToolHTTPErrorBody(string(respBody), prepared.authToken, prepared.transactionToken, sessionID),
	)
}

func newMCPRequest(ctx context.Context, prepared preparedToolRequest, body []byte, protocolVersion string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, prepared.request.URL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Host = prepared.request.Host
	req.Header = prepared.request.Header.Clone()
	if toolIdempotencyKeyFromContext(prepared.request.Context()) != "" {
		req.Header.Del(toolIdempotencyKeyHeader)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(mcpProtocolVersionHeader, mcpProtocolHeaderValue(protocolVersion))
	return req, nil
}

type mcpHTTPResponse struct {
	body   []byte
	header http.Header
}

func executeMCPHTTPRequest(httpClient *http.Client, req *http.Request, expectedResponseID string, secrets ...string) ([]byte, error) {
	resp, err := executeMCPHTTPRequestWithResponse(httpClient, req, expectedResponseID, secrets...)
	if err != nil {
		return nil, err
	}
	return resp.body, nil
}

func executeMCPHTTPRequestWithResponse(httpClient *http.Client, req *http.Request, expectedResponseID string, secrets ...string) (mcpHTTPResponse, error) {
	resp, err := httpClient.Do(req)
	if err != nil {
		return mcpHTTPResponse{}, fmt.Errorf("tool request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	mcpResp := mcpHTTPResponse{header: resp.Header.Clone()}

	respBody, err := readMCPHTTPResponseBody(resp, expectedResponseID)
	if err != nil {
		return mcpResp, fmt.Errorf("failed to read response: %w", err)
	}
	mcpResp.body = respBody
	redactionSecrets := append([]string{}, secrets...)
	redactionSecrets = append(redactionSecrets, resp.Header.Get(mcpSessionIDHeader))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mcpResp, fmt.Errorf("tool returned HTTP %d: %s", resp.StatusCode, redactToolHTTPErrorBody(string(respBody), redactionSecrets...))
	}
	if err := validateMCPExpectedResponseID(respBody, expectedResponseID); err != nil {
		return mcpResp, err
	}
	return mcpResp, nil
}

func readMCPHTTPResponseBody(resp *http.Response, expectedResponseID string) ([]byte, error) {
	if isMCPEventStream(resp.Header.Get("Content-Type")) && strings.TrimSpace(expectedResponseID) != "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return readMCPEventStreamResponse(resp.Body, expectedResponseID)
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB limit
	if err != nil {
		return nil, err
	}
	return normalizeMCPResponseBody(resp.Header.Get("Content-Type"), respBody), nil
}

func mcpInitializedProtocolVersion(body []byte, secrets ...string) (string, error) {
	response, err := parseMCPResponse(body, mcpInitializeRequestID, secrets...)
	if err != nil {
		return "", err
	}
	var result mcpInitializeResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return "", fmt.Errorf("failed to parse MCP initialize result: %w", err)
	}
	protocolVersion := strings.TrimSpace(result.ProtocolVersion)
	if protocolVersion == "" {
		return "", fmt.Errorf("MCP initialize result missing protocolVersion")
	}
	return protocolVersion, nil
}

func parseMCPResponse(body []byte, expectedID string, secrets ...string) (mcpToolCallResponse, error) {
	var response mcpToolCallResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return mcpToolCallResponse{}, fmt.Errorf("failed to parse MCP response: %w", err)
	}
	if err := validateMCPResponseID(response, expectedID); err != nil {
		return mcpToolCallResponse{}, err
	}
	if response.Error != nil {
		message := response.Error.Message
		if len(response.Error.Data) > 0 {
			message = strings.TrimSpace(message + ": " + string(response.Error.Data))
		}
		return mcpToolCallResponse{}, fmt.Errorf("MCP returned error %d: %s", response.Error.Code, redactToolHTTPErrorBody(message, secrets...))
	}
	if len(response.Result) == 0 {
		return mcpToolCallResponse{}, fmt.Errorf("MCP response missing result")
	}
	return response, nil
}

func validateMCPExpectedResponseID(body []byte, expectedID string) error {
	expectedID = strings.TrimSpace(expectedID)
	if expectedID == "" {
		return nil
	}
	var response mcpToolCallResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return fmt.Errorf("failed to parse MCP response: %w", err)
	}
	return validateMCPResponseID(response, expectedID)
}

func validateMCPResponseID(response mcpToolCallResponse, expectedID string) error {
	expectedID = strings.TrimSpace(expectedID)
	if expectedID == "" || mcpJSONRPCResponseMatchesID(response, expectedID) {
		return nil
	}
	return fmt.Errorf("MCP response id did not match expected %q", expectedID)
}

func mcpProtocolHeaderValue(protocolVersion string) string {
	protocolVersion = strings.TrimSpace(protocolVersion)
	if protocolVersion == "" {
		return mcpProtocolVersion
	}
	return protocolVersion
}

func normalizeMCPResponseBody(contentType string, body []byte) []byte {
	if !isMCPEventStream(contentType) {
		return body
	}
	if data := firstSSEData(body); len(data) > 0 {
		return data
	}
	return body
}

func isMCPEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

func readMCPEventStreamResponse(body io.Reader, expectedResponseID string) ([]byte, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10<<20)
	totalDataBytes := 0
	var data []string
	flush := func() []byte {
		if len(data) == 0 {
			return nil
		}
		joined := strings.TrimSpace(strings.Join(data, "\n"))
		data = nil
		if joined == "" || joined == "[DONE]" {
			return nil
		}
		eventBody := []byte(joined)
		if !isMCPJSONRPCResponse(eventBody, expectedResponseID) {
			return nil
		}
		return eventBody
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if event := flush(); len(event) > 0 {
				return event, nil
			}
			continue
		}
		if after, ok := strings.CutPrefix(line, "data:"); ok {
			after = strings.TrimPrefix(after, " ")
			totalDataBytes += len(after)
			if totalDataBytes > 10<<20 {
				return nil, fmt.Errorf("MCP event stream response exceeded 10MB limit")
			}
			data = append(data, after)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if event := flush(); len(event) > 0 {
		return event, nil
	}
	return nil, fmt.Errorf("MCP event stream ended before response %q", expectedResponseID)
}

func firstSSEData(body []byte) []byte {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 10<<20)
	var data []string
	flush := func() []byte {
		if len(data) == 0 {
			return nil
		}
		joined := strings.TrimSpace(strings.Join(data, "\n"))
		data = nil
		if joined == "" || joined == "[DONE]" {
			return nil
		}
		eventBody := []byte(joined)
		if !isMCPJSONRPCResponse(eventBody, "") {
			return nil
		}
		return eventBody
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if event := flush(); len(event) > 0 {
				return event
			}
			continue
		}
		if after, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimSpace(after))
		}
	}
	return flush()
}

func isMCPJSONRPCResponse(body []byte, expectedID string) bool {
	var response mcpToolCallResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return false
	}
	if response.Error == nil && len(response.Result) == 0 {
		return false
	}
	return mcpJSONRPCResponseMatchesID(response, expectedID)
}

func mcpJSONRPCResponseMatchesID(response mcpToolCallResponse, expectedID string) bool {
	if strings.TrimSpace(expectedID) == "" {
		return true
	}
	if response.ID == nil {
		return response.Error != nil
	}
	return strings.TrimSpace(fmt.Sprint(response.ID)) == strings.TrimSpace(expectedID)
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
	authInject := toolAuthInject(httpConfig)
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

func toolAuthInject(httpConfig corev1alpha1.HTTPExecution) string {
	if httpConfig.AuthInject == "" {
		return "header"
	}
	return httpConfig.AuthInject
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
