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
	"github.com/orka-agents/orka/internal/worker"
	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	connectorToolCallTimeout = 5 * time.Minute
	// connectorToolResponseLimit bounds the controller's JSON envelope. The
	// executor caps a tool body at 10 MiB and JSON escaping can expand a
	// body of control characters sixfold, so the bound covers that worst
	// case with room for the envelope; overflow is reported, not truncated.
	connectorToolResponseLimit = 64 << 20
	// connectorToolUnavailableRetries bounds how often a 503 from the
	// controller is retried: a fresh worker Pod can reach the endpoint
	// before the Task's Job identity is published, and that window closes
	// within seconds.
	connectorToolUnavailableRetries = 5
	connectorToolErrorBodyLimit     = 2048
	saTokenPathDefault              = "/var/run/secrets/kubernetes.io/serviceaccount/token"
)

// connectorBindings holds, by OutboundAccessPolicy name, the Connection
// identity the controller froze for this Task at dispatch. It is set once at
// startup from the Job environment and carries no token material.
var connectorBindings = map[string]corev1alpha1.ConnectionBinding{}

// parseConnectionBindings decodes the frozen bindings the Job builder placed
// in the worker environment. An unreadable value yields no bindings, which
// makes every connector approval digest fail to match at the controller.
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

// connectorToolProxyTimeout is the Tool's declared request timeout plus a
// settlement margin, or the default proxy deadline when it declares none.
func connectorToolProxyTimeout(tool *corev1alpha1.Tool) time.Duration {
	if tool != nil && tool.Spec.HTTP != nil && tool.Spec.HTTP.Timeout != nil && tool.Spec.HTTP.Timeout.Duration > 0 {
		return tool.Spec.HTTP.Timeout.Duration + connectorToolSettlementMargin
	}
	return connectorToolCallTimeout
}

// connectorToolSettlementMargin covers the controller's own work around the
// provider call (claims, ledger writes, response encoding).
const connectorToolSettlementMargin = 30 * time.Second

// connectorToolRetryBackoff is the first 503 retry delay; it doubles per
// attempt. A variable so tests do not wait through it.
var connectorToolRetryBackoff = 500 * time.Millisecond

// connectorBackedToolNames is set once at startup from the loaded custom
// Tools; the agent loop consults it to route calls to the controller.
var connectorBackedToolNames = map[string]bool{}

// frozenConnectorToolDigests decodes the connector tool digests the Job
// builder froze into the worker environment; an unreadable value yields none.
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

// connectorToolPolicies holds, per connector-backed tool, the connection-mode
// policy spec loaded with it, so approval targets bind the injection
// configuration the controller will recompute from the live policy.
var connectorToolPolicies = map[string]corev1alpha1.OutboundAccessPolicySpec{}

// connectorBackedTools reports which loaded custom Tools sit behind a
// connection-mode OutboundAccessPolicy. Those tools never execute in this
// Pod: the person's token lives only in the controller, so the worker asks
// the controller to run the call on the Task's behalf.
//
// A policy that cannot be read is retried briefly and then fails startup:
// a tool marked connector-backed without its policy spec would compute an
// approval target the controller never matches, wasting the approval.
func connectorBackedTools(
	ctx context.Context,
	k8sClient client.Client,
	namespace string,
	customTools map[string]*corev1alpha1.Tool,
) (map[string]bool, error) {
	result := map[string]bool{}
	// The digests the controller froze into this Job are the routing upper
	// bound: a tool dispatched as connector-backed always goes to the
	// controller, which refuses it if its policy or definition drifted. Live
	// state can only add tools, never route a frozen one back into this Pod.
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
				// A missing policy is treated as connector-backed so the Pod
				// never tries to resolve a credential it may not hold.
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

// connectorPolicyReadAttempts and connectorPolicyReadBackoff bound the
// retries of a transient policy read at startup; tests shorten the backoff.
var (
	connectorPolicyReadAttempts = 5
	connectorPolicyReadBackoff  = 200 * time.Millisecond
)

// readConnectorPolicy reads a policy, retrying transient failures; a
// NotFound is returned at once.
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

// connectorToolRequest is the body sent to the controller's internal
// connector-tool endpoint.
// connectorBackedToolAnnotation marks an in-memory Tool the worker routes to
// the controller. Approval targets for such tools digest the plain Tool spec,
// which the controller recomputes from the live Tool before claiming the
// approval; policy and transaction identity are the controller's concern.
const connectorBackedToolAnnotation = "orka.ai/connector-backed"

// markConnectorBackedTools annotates the in-memory Tools the worker routes to
// the controller so the approval gate digests their plain spec. The marker is
// derived from the routing classification only: any value that arrived on
// the Tool object itself is cleared first, so a Tool cannot opt out of the
// worker's full approval digest by carrying the annotation.
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
		tool.Annotations[connectorBackedToolAnnotation] = "true"
	}
}

type connectorToolRequest struct {
	Arguments      json.RawMessage `json:"arguments"`
	CallID         string          `json:"callId,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	// ApprovalID is the approval the worker matched for this call, when the
	// tool required one. The controller verifies it before executing.
	ApprovalID string `json:"approvalId,omitempty"`
}

type connectorToolResponse struct {
	Result string `json:"result"`
	// Error is the API's shared envelope ({"code", "message"}); a plain
	// string is accepted too.
	Error json.RawMessage `json:"error,omitempty"`
}

// errorMessage returns the controller's error message from either shape.
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

// controllerHTTPClient is the client for the worker's authenticated calls to
// the controller's internal API: inherited proxy settings are ignored and
// redirects are not followed, so the projected ServiceAccount token is only
// ever sent to the controller URL itself.
func controllerHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// executeConnectorToolViaController runs a connector-backed tool in the
// controller. Errors after the request was sent are marked as attempted so
// the approval gate does not re-fire a consequential action.
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
	// The approval key doubles as the idempotency key in the worker's gate.
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
	// The controller applies the Tool's own request timeout; the proxy
	// deadline covers that plus the controller's settlement margin, so a
	// legitimately slow tool is never cut off by the proxy first.
	callCtx, cancel := context.WithTimeout(ctx, connectorToolProxyTimeout(tool))
	defer cancel()
	if httpClient == nil {
		httpClient = controllerHTTPClient()
	}
	backoff := connectorToolRetryBackoff
	for attempt := 0; ; attempt++ {
		status, raw, err := postConnectorToolRequest(callCtx, httpClient, endpoint, token, body)
		if err != nil {
			// A transport failure is ambiguous (the request may or may not
			// have reached the controller) but never consumes the local
			// approval: the controller's claim and effect ledger make an
			// exact retry safe, replaying a committed result or refusing a
			// call that is still executing. A response that arrived but
			// could not be read was attempted.
			if errors.As(err, new(connectorResponseError)) {
				return "", attempted(fmt.Errorf("connector tool %q: %w", toolName, err))
			}
			return "", fmt.Errorf("connector tool %q: %w", toolName, err)
		}
		// 503 means the controller cannot yet judge this caller (the Task's
		// Job identity is not published); nothing was executed, so retry.
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

// postConnectorToolRequest sends one request and returns the status and
// bounded body. A body past the bound is an error rather than a truncated
// document that would later fail to decode as an attempted call.
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

// connectorResponseError marks a failure after the controller answered: the
// call was attempted, whatever the body said.
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
		// The controller reached the provider; the action may have happened.
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
