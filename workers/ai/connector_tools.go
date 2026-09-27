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

	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/worker"
	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	connectorToolCallTimeout    = 5 * time.Minute
	connectorToolResponseLimit  = 4 << 20
	connectorToolErrorBodyLimit = 2048
	saTokenPathDefault          = "/var/run/secrets/kubernetes.io/serviceaccount/token"
)

// connectorBackedToolNames is set once at startup from the loaded custom
// Tools; the agent loop consults it to route calls to the controller.
var connectorBackedToolNames = map[string]bool{}

// connectorBackedTools reports which loaded custom Tools sit behind a
// connection-mode OutboundAccessPolicy. Those tools never execute in this
// Pod: the person's token lives only in the controller, so the worker asks
// the controller to run the call on the Task's behalf.
func connectorBackedTools(
	ctx context.Context,
	k8sClient client.Client,
	namespace string,
	customTools map[string]*corev1alpha1.Tool,
) map[string]bool {
	result := map[string]bool{}
	for name, tool := range customTools {
		if tool == nil || tool.Spec.HTTP == nil || tool.Spec.HTTP.OutboundAccessPolicyRef == nil {
			continue
		}
		policy := &corev1alpha1.OutboundAccessPolicy{}
		key := client.ObjectKey{Namespace: namespace, Name: tool.Spec.HTTP.OutboundAccessPolicyRef.Name}
		if err := k8sClient.Get(ctx, key, policy); err != nil {
			// Unreadable policies are treated as connector-backed so the Pod
			// never tries to resolve a credential it may not hold.
			result[name] = true
			continue
		}
		if policy.Spec.Connection != nil {
			result[name] = true
		}
	}
	return result
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
	Error  string `json:"error,omitempty"`
}

// executeConnectorToolViaController runs a connector-backed tool in the
// controller. Errors after the request was sent are marked as attempted so
// the approval gate does not re-fire a consequential action.
func executeConnectorToolViaController(
	ctx context.Context,
	httpClient *http.Client,
	toolName string,
	args json.RawMessage,
	callID, idempotencyKey string,
) (string, error) {
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
	callCtx, cancel := context.WithTimeout(ctx, connectorToolCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build connector tool request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", attempted(fmt.Errorf("connector tool %q: controller request failed: %w", toolName, err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, connectorToolResponseLimit))
	if err != nil {
		return "", attempted(fmt.Errorf("connector tool %q: read controller response: %w", toolName, err))
	}
	return decodeConnectorToolResponse(toolName, resp.StatusCode, raw)
}

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
	message := strings.TrimSpace(parsed.Error)
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
