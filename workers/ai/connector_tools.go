/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/worker"
	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	connectorToolCallTimeout        = 5 * time.Minute
	connectorToolResponseLimit      = 64 << 20
	connectorToolUnavailableRetries = 5
	connectorToolErrorBodyLimit     = 2048
	saTokenPathDefault              = "/var/run/secrets/kubernetes.io/serviceaccount/token"
)

var connectorBindings = map[string]corev1alpha1.ConnectionBinding{}

func parseConnectionBindings(raw string) map[string]corev1alpha1.ConnectionBinding {
	result := map[string]corev1alpha1.ConnectionBinding{}
	if strings.TrimSpace(raw) == "" {
		return result
	}
	var bindings []corev1alpha1.ConnectionBinding
	if err := json.Unmarshal([]byte(raw), &bindings); err != nil {
		return result
	}
	for _, binding := range bindings {
		if binding.PolicyName != "" {
			result[binding.PolicyName] = binding
		}
	}
	return result
}

func connectorToolProxyTimeout(tool *corev1alpha1.Tool) time.Duration {
	if tool != nil && tool.Spec.HTTP != nil && tool.Spec.HTTP.Timeout != nil && tool.Spec.HTTP.Timeout.Duration > 0 {
		return min(tool.Spec.HTTP.Timeout.Duration, connectors.MaxHTTPToolTimeout) + connectorToolSettlementMargin
	}
	return connectorToolCallTimeout
}

const connectorToolSettlementMargin = 30 * time.Second

var connectorToolRetryBackoff = 500 * time.Millisecond

var connectorBackedToolNames = map[string]bool{}

func frozenConnectorToolDigests(raw string) map[string]string {
	digests := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return digests
	}
	if err := json.Unmarshal([]byte(raw), &digests); err != nil {
		return map[string]string{}
	}
	return digests
}

var connectorToolPolicies = map[string]corev1alpha1.OutboundAccessPolicySpec{}

func connectorBackedTools(
	ctx context.Context,
	k8sClient client.Client,
	namespace string,
	customTools map[string]*corev1alpha1.Tool,
) (map[string]bool, error) {
	result := map[string]bool{}
	for name := range frozenConnectorToolDigests(os.Getenv(workerenv.ConnectorToolDigests)) {
		if _, loaded := customTools[name]; loaded {
			result[name] = true
		}
	}
	for name, tool := range customTools {
		if tool == nil || tool.Spec.HTTP == nil || tool.Spec.HTTP.OutboundAccessPolicyRef == nil {
			continue
		}
		policyKey := client.ObjectKey{Namespace: namespace, Name: tool.Spec.HTTP.OutboundAccessPolicyRef.Name}
		policy, err := readConnectorPolicy(ctx, k8sClient, policyKey)
		if err != nil {
			if apierrors.IsNotFound(err) {
				result[name] = true
				continue
			}
			return nil, fmt.Errorf("read outbound access policy for tool %q: %w", name, err)
		}
		if policy.Spec.Connection != nil {
			result[name] = true
			connectorToolPolicies[name] = policy.Spec
		}
	}
	return result, nil
}

var (
	connectorPolicyReadAttempts = 5
	connectorPolicyReadBackoff  = 200 * time.Millisecond
)

func readConnectorPolicy(
	ctx context.Context, k8sClient client.Client, key client.ObjectKey,
) (*corev1alpha1.OutboundAccessPolicy, error) {
	backoff := connectorPolicyReadBackoff
	var err error
	for range connectorPolicyReadAttempts {
		policy := &corev1alpha1.OutboundAccessPolicy{}
		if err = k8sClient.Get(ctx, key, policy); err == nil {
			return policy, nil
		}
		if apierrors.IsNotFound(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return nil, err
}

const connectorBackedToolAnnotation = "orka.ai/connector-backed"

func isConnectorBackedTool(tool *corev1alpha1.Tool) bool {
	return tool != nil && tool.Annotations[connectorBackedToolAnnotation] == connectorBackedToolValue
}

const connectorBackedToolValue = "true"

func markConnectorBackedTools(customTools map[string]*corev1alpha1.Tool, names map[string]bool) {
	for _, tool := range customTools {
		if tool != nil {
			delete(tool.Annotations, connectorBackedToolAnnotation)
		}
	}
	for name, backed := range names {
		tool := customTools[name]
		if !backed || tool == nil {
			continue
		}
		if tool.Annotations == nil {
			tool.Annotations = map[string]string{}
		}
		tool.Annotations[connectorBackedToolAnnotation] = connectorBackedToolValue
	}
}

type connectorToolRequest struct {
	Arguments      json.RawMessage `json:"arguments"`
	CallID         string          `json:"callId,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	ApprovalID     string          `json:"approvalId,omitempty"`
}

type connectorToolResponse struct {
	Result string          `json:"result"`
	Error  json.RawMessage `json:"error,omitempty"`
}

func (r connectorToolResponse) errorMessage() string {
	var envelope struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(r.Error, &envelope) == nil && envelope.Message != "" {
		return envelope.Message
	}
	var message string
	if json.Unmarshal(r.Error, &message) == nil {
		return message
	}
	return ""
}

func controllerHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func executeConnectorToolViaController(
	ctx context.Context,
	httpClient *http.Client,
	tool *corev1alpha1.Tool,
	args json.RawMessage,
	callID, idempotencyKey string,
) (string, error) {
	toolName := ""
	if tool != nil {
		toolName = tool.Name
	}
	endpoint, err := connectorToolEndpoint(toolName)
	if err != nil {
		return "", err
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	body, err := json.Marshal(connectorToolRequest{
		Arguments: args, CallID: callID, IdempotencyKey: idempotencyKey, ApprovalID: idempotencyKey,
	})
	if err != nil {
		return "", fmt.Errorf("encode connector tool request: %w", err)
	}
	token := workerServiceAccountTokenForConnector()
	if token == "" {
		return "", errors.New("worker service account token is unavailable for connector tool execution")
	}
	callCtx, cancel := context.WithTimeout(ctx, connectorToolProxyTimeout(tool))
	defer cancel()
	if httpClient == nil {
		httpClient = controllerHTTPClient()
	}
	backoff := connectorToolRetryBackoff
	for attempt := 0; ; attempt++ {
		status, raw, err := postConnectorToolRequest(callCtx, httpClient, endpoint, token, body)
		if err != nil {
			if errors.As(err, new(connectorResponseError)) {
				return "", attempted(fmt.Errorf("connector tool %q: %w", toolName, err))
			}
			return "", fmt.Errorf("connector tool %q: %w", toolName, err)
		}
		if status == http.StatusServiceUnavailable && attempt < connectorToolUnavailableRetries {
			select {
			case <-callCtx.Done():
				return "", fmt.Errorf("connector tool %q: controller unavailable: %w", toolName, callCtx.Err())
			case <-time.After(backoff):
			}
			backoff *= 2
			continue
		}
		return decodeConnectorToolResponse(toolName, status, raw)
	}
}

func postConnectorToolRequest(
	ctx context.Context, httpClient *http.Client, endpoint, token string, body []byte,
) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build connector tool request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("controller request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, connectorToolResponseLimit+1))
	if err != nil {
		return 0, nil, connectorResponseError{err: fmt.Errorf("read controller response: %w", err)}
	}
	if len(raw) > connectorToolResponseLimit {
		return 0, nil, connectorResponseError{
			err: fmt.Errorf("controller response exceeds %d bytes", connectorToolResponseLimit),
		}
	}
	return resp.StatusCode, raw, nil
}

type connectorResponseError struct{ err error }

func (e connectorResponseError) Error() string { return e.err.Error() }
func (e connectorResponseError) Unwrap() error { return e.err }

func attempted(err error) error {
	return worker.ToolRequestAttemptedError{Err: err}
}

func decodeConnectorToolResponse(toolName string, status int, raw []byte) (string, error) {
	var parsed connectorToolResponse
	decodeErr := json.Unmarshal(raw, &parsed)
	if status >= 200 && status < 300 {
		if decodeErr != nil {
			return "", attempted(fmt.Errorf("connector tool %q: controller response is not valid JSON", toolName))
		}
		return parsed.Result, nil
	}
	message := strings.TrimSpace(parsed.errorMessage())
	if message == "" {
		preview := raw
		if len(preview) > connectorToolErrorBodyLimit {
			preview = preview[:connectorToolErrorBodyLimit]
		}
		message = strings.TrimSpace(string(preview))
	}
	err := fmt.Errorf("connector tool %q failed (HTTP %d): %s", toolName, status, message)
	if status == http.StatusBadGateway {
		return "", attempted(worker.ToolExecutionError{Err: err})
	}
	return "", err
}

func connectorToolEndpoint(toolName string) (string, error) {
	controllerURL := strings.TrimRight(strings.TrimSpace(os.Getenv(workerenv.ControllerURL)), "/")
	namespace := strings.TrimSpace(os.Getenv(workerenv.TaskNamespace))
	taskName := strings.TrimSpace(os.Getenv(workerenv.TaskName))
	if controllerURL == "" || namespace == "" || taskName == "" {
		return "", errors.New("connector-backed tools require the controller URL and task identity in the worker environment")
	}
	if strings.TrimSpace(toolName) == "" {
		return "", errors.New("connector tool name is required")
	}
	return fmt.Sprintf("%s/internal/v1/tasks/%s/%s/connector-tools/%s",
		controllerURL, url.PathEscape(namespace), url.PathEscape(taskName), url.PathEscape(toolName)), nil
}

func workerServiceAccountTokenForConnector() string {
	if path := strings.TrimSpace(os.Getenv(workerenv.ServiceAccountTokenPath)); path != "" {
		if token, err := os.ReadFile(path); err == nil { // #nosec G304 -- controller-supplied projected token path.
			return strings.TrimSpace(string(token))
		}
	}
	if token, err := os.ReadFile(saTokenPathDefault); err == nil {
		return strings.TrimSpace(string(token))
	}
	return strings.TrimSpace(os.Getenv(workerenv.ServiceAccountToken))
}
