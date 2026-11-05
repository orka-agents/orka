package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// forwardResultEnvelopeBytes leaves room in the broker's result bound for the
// status, headers, and JSON escaping around a forwarded body.
const forwardResultEnvelopeBytes = 64 << 10

// isForwardingTool reports whether a Tool forwards a client's requests.
func isForwardingTool(tool *corev1alpha1.Tool) bool {
	return tool != nil && tool.Spec.HTTP != nil && tool.Spec.HTTP.Forward != nil
}

// AllowForwarding lets this executor run forwarding Tools. Only the ACP broker
// sets it: there the runtime built the call from a client request and the
// broker checked it against the prompt's frozen tool policy. A native worker,
// or the internal connector endpoint, refuses forwarding Tools.
func (e *ToolExecutor) AllowForwarding() { e.forwarding = true }

func toolForwardProtocol(tool *corev1alpha1.Tool) corev1alpha1.HTTPForwardProtocol {
	if !isForwardingTool(tool) {
		return ""
	}
	return tool.Spec.HTTP.Forward.Protocol
}

// prepareForwardRequest builds the request a forwarding Tool call names: a
// read on the Tool's own origin, with no body and no headers from the call.
// The outbound access policy then adds the credential as for any Tool.
func (e *ToolExecutor) prepareForwardRequest(ctx context.Context, tool *corev1alpha1.Tool, args json.RawMessage) (preparedToolRequest, error) {
	if !e.forwarding {
		return preparedToolRequest{}, fmt.Errorf("tool %q forwards a command-line client's requests and runs only through an ACP runtime", tool.Name)
	}
	httpConfig, routeHost, err := toolHTTPConfig(tool)
	if err != nil {
		return preparedToolRequest{}, err
	}
	if routeHost != "" || tool.Spec.MCP != nil || httpConfig.AuthSecretRef != nil || len(httpConfig.Headers) > 0 ||
		httpConfig.OutboundAccessPolicyRef == nil {
		return preparedToolRequest{}, errors.New("a forwarding tool takes its credential only from its outbound access policy")
	}
	request, err := harnessv2.DecodeMCPForwardRequest(args)
	if err != nil {
		return preparedToolRequest{}, err
	}
	target, err := request.Target(httpConfig.URL)
	if err != nil {
		return preparedToolRequest{}, err
	}
	req, err := http.NewRequestWithContext(ctx, request.Method, target.String(), nil)
	if err != nil {
		return preparedToolRequest{}, fmt.Errorf("failed to create forwarded request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	prepared := preparedToolRequest{httpConfig: httpConfig, request: req, forward: true}
	if err := e.applyOutboundAccessPolicy(ctx, tool, args, &prepared); err != nil {
		return preparedToolRequest{}, err
	}
	if !prepared.direct {
		return preparedToolRequest{}, errors.New("a forwarding tool needs a credential-injecting outbound access policy")
	}
	return prepared, nil
}

// executeForwardRequest returns the upstream answer as it is, whatever its
// status, so the client sees the service's own errors. Redirects are never
// followed: the credential stays on the Tool's origin, and the client gets the
// redirect to handle.
func executeForwardRequest(httpClient *http.Client, prepared preparedToolRequest) (string, error) {
	client := *httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(prepared.request)
	if err != nil {
		return "", ToolRequestAttemptedError{Err: safeToolTransportError(prepared.request.Context(), err, prepared.redactionSecrets...)}
	}
	defer resp.Body.Close() //nolint:errcheck
	limit := int64(harnessv2.MaxMCPResultBytes - forwardResultEnvelopeBytes)
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", ToolRequestAttemptedError{Err: fmt.Errorf("failed to read forwarded response: %w", err)}
	}
	if int64(len(body)) > limit {
		return "", ToolExecutionError{Err: fmt.Errorf("forwarded response exceeds %d bytes", limit)}
	}
	if !utf8.Valid(body) {
		return "", ToolExecutionError{Err: errors.New("forwarded response is not UTF-8 text")}
	}
	response := harnessv2.MCPForwardResponse{
		Status: resp.StatusCode,
		Body:   redactToolSensitiveText(string(body), prepared.redactionSecrets...),
	}
	for _, name := range harnessv2.ForwardResponseHeaders {
		value := strings.TrimSpace(resp.Header.Get(name))
		if value == "" || strings.ContainsAny(value, "\r\n\x00") {
			continue
		}
		if response.Headers == nil {
			response.Headers = map[string]string{}
		}
		response.Headers[name] = redactToolSensitiveText(value, prepared.redactionSecrets...)
	}
	if err := response.Validate(); err != nil {
		return "", ToolExecutionError{Err: err}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
