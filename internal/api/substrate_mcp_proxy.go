/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/transactiontoken"
	workerexecutor "github.com/orka-agents/orka/internal/worker"
)

const (
	// substrateMCPProxyMaxRequestBytes bounds one MCP JSON-RPC request body.
	substrateMCPProxyMaxRequestBytes = 4 << 20
	// substrateMCPProxyMaxResponseBytes bounds one streamed actor response,
	// above the 10 MiB tool result limit plus SSE framing.
	substrateMCPProxyMaxResponseBytes = 16 << 20
	substrateMCPProxyDefaultTimeout   = 30 * time.Second
	substrateMCPProxyMaxTimeout       = 5 * time.Minute
	substrateMCPProxyMaxTokenBytes    = 16 << 10
	substrateMCPProxyAction           = "callSubstrateMCPTool"
)

// SubstrateMCPProxyConfig enables the Orka API MCP proxy for Substrate MCP
// actor Tools. The router uses the manager's pod-certificate identity, the
// only client on the Substrate router's SPIFFE allowlist. A nil Router leaves
// the proxy unavailable (503).
type SubstrateMCPProxyConfig struct {
	Router *workerexecutor.SubstrateMCPRouter
}

// substrateMCPProxyRequestHeaders are the only caller headers forwarded to an
// actor. Authorization, cookies, the caller's own Txn-Token (Orka API
// authentication) and every other header are dropped.
var substrateMCPProxyRequestHeaders = []string{
	fiber.HeaderAccept, "MCP-Protocol-Version", "Mcp-Session-Id", "Idempotency-Key", "traceparent", "tracestate",
}

// substrateMCPProxyResponseHeaders are the only actor response headers returned.
var substrateMCPProxyResponseHeaders = []string{fiber.HeaderContentType, "Mcp-Session-Id", "MCP-Protocol-Version"}

// ProxySubstrateMCPTool forwards one MCP Streamable HTTP request to the
// Substrate actor that backs a Tool. The route policy has already required
// `get tools/<name>` in the resolved namespace for Kubernetes identities.
//
//nolint:gocyclo // Authentication, Tool admission, credential and transport checks fail closed in order.
func (h *Handlers) ProxySubstrateMCPTool(c fiber.Ctx) error {
	name := c.Params(apiFieldName)
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return err
	}
	ui := GetUserInfo(c)
	if ui == nil {
		return fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	switch ui.AuthType {
	case AuthTypeTokenReview:
		// Authorized by the route policy's SubjectAccessReview.
	case AuthTypeContextToken:
		// Transaction tokens carry their own scopes. Audit-only or disabled
		// authorization would let any valid token invoke an actor.
		if !h.contextTokenAuthorization.enforcing() {
			return fiber.NewError(fiber.StatusForbidden, "Substrate MCP proxy requires enforced transaction-token authorization")
		}
		if err := h.authorizeContextTokenAction(c, substrateMCPProxyAction, h.contextTokenAuthorization.ToolUseScopes); err != nil {
			return err
		}
		if err := authorizeContextTokenToolUse(c, h.contextTokenAuthorization, substrateMCPProxyAction, []string{name}); err != nil {
			return err
		}
	default:
		return fiber.NewError(fiber.StatusForbidden, "Substrate MCP proxy requires a Kubernetes ServiceAccount or transaction-token identity")
	}
	if h.substrateMCPRouter == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "Substrate MCP proxy is not configured")
	}

	ctx := c.Context()
	tool, err := h.substrateMCPProxyTool(ctx, namespace, name)
	if err != nil {
		return err
	}
	// The caller validated credential authority and approvals against its own
	// Tool snapshot. Refuse before resolving credentials or contacting an Actor
	// if that snapshot is not this exact Tool lifetime, spec and Actor.
	if !workerexecutor.SubstrateMCPToolBindingMatches(tool,
		c.Get(workerexecutor.SubstrateMCPToolUIDHeader),
		c.Get(workerexecutor.SubstrateMCPToolGenerationHeader),
		c.Get(workerexecutor.SubstrateMCPToolActorHeader)) {
		return fiber.NewError(fiber.StatusPreconditionFailed, "tool changed since the caller validated it; retry with the current Tool")
	}
	credential, err := h.substrateMCPProxyCredential(c, ui, tool)
	if err != nil {
		return err
	}

	method := c.Method()
	var body []byte
	if method == fiber.MethodPost {
		body = c.Body()
		if len(body) > substrateMCPProxyMaxRequestBytes {
			return fiber.NewError(fiber.StatusRequestEntityTooLarge, "MCP request body is too large")
		}
		mediaType, _, parseErr := mime.ParseMediaType(c.Get(fiber.HeaderContentType))
		if parseErr != nil || mediaType != fiber.MIMEApplicationJSON {
			return fiber.NewError(fiber.StatusUnsupportedMediaType, "MCP requests must be application/json")
		}
	}
	toolTxnToken := strings.TrimSpace(c.Get(workerexecutor.SubstrateMCPToolTxnTokenHeader))
	if len(toolTxnToken) > substrateMCPProxyMaxTokenBytes || strings.ContainsAny(toolTxnToken, "\r\n") {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tool transaction token header")
	}

	timeout := substrateMCPProxyTimeout(tool)
	// fasthttp streams the body after this handler returns, so the upstream
	// request is bounded by its own timeout rather than the handler context.
	upstreamCtx, cancel := context.WithTimeout(context.Background(), timeout)
	request, err := h.substrateMCPRouter.NewRequest(upstreamCtx, tool, method, bytes.NewReader(body))
	if err != nil {
		cancel()
		return fiber.NewError(fiber.StatusServiceUnavailable, "MCP tool actor endpoint is not ready")
	}
	if method == fiber.MethodPost {
		request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	}
	for _, header := range substrateMCPProxyRequestHeaders {
		if value := strings.TrimSpace(c.Get(header)); value != "" {
			request.Header.Set(header, value)
		}
	}
	for header, value := range toolHTTPHeaders(tool) {
		request.Header.Set(header, value)
	}
	if credential != "" {
		request.Header.Set(fiber.HeaderAuthorization, "Bearer "+credential)
	}
	if toolTxnToken != "" {
		request.Header.Set(transactiontoken.HeaderName, toolTxnToken)
	}

	httpClient := *h.substrateMCPRouter.HTTPClient()
	httpClient.Timeout = 0
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := httpClient.Do(request) //nolint:bodyclose // Closed by substrateMCPProxyStream.
	if err != nil {
		cancel()
		log.Info("Substrate MCP proxy request failed", "namespace", namespace, "tool", name, "timeout", errors.Is(err, context.DeadlineExceeded))
		if errors.Is(err, context.DeadlineExceeded) {
			return fiber.NewError(fiber.StatusGatewayTimeout, "Substrate MCP actor request timed out")
		}
		return fiber.NewError(fiber.StatusBadGateway, "Substrate MCP actor request failed")
	}
	stream := &substrateMCPProxyStream{body: response.Body, remaining: substrateMCPProxyMaxResponseBytes, cancel: cancel}
	for _, header := range substrateMCPProxyResponseHeaders {
		if value := response.Header.Get(header); value != "" {
			c.Set(header, value)
		}
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Status(response.StatusCode)
	if err := c.SendStream(stream); err != nil {
		_ = stream.Close()
		return err
	}
	return nil
}

func (h *Handlers) substrateMCPProxyTool(ctx context.Context, namespace, name string) (*corev1alpha1.Tool, error) {
	reader := client.Reader(h.client)
	if h.apiReader != nil {
		reader = h.apiReader
	}
	tool := &corev1alpha1.Tool{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, tool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusNotFound, "tool not found")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to get tool")
	}
	if !workerexecutor.IsSubstrateMCPActorTool(tool) {
		return nil, fiber.NewError(fiber.StatusBadRequest, "tool is not a Substrate MCP actor tool")
	}
	if !tool.DeletionTimestamp.IsZero() || !tool.Status.Available {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "MCP tool actor endpoint is not ready")
	}
	if tool.Spec.HTTP != nil && tool.Spec.HTTP.OutboundAccessPolicyRef != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "Substrate MCP actor tools do not support outboundAccessPolicyRef")
	}
	if _, err := workerexecutor.SubstrateMCPActorTarget(tool); err != nil {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "MCP tool actor endpoint is not ready")
	}
	// Status names the previous Actor until the controller reconciles a spec
	// change; never pair the current credential with that Actor.
	if !workerexecutor.SubstrateMCPToolReadyForCurrentGeneration(tool) {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "MCP tool actor is not ready for the current Tool specification")
	}
	if _, err := workerexecutor.SubstrateMCPActorPath(tool); err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "invalid Substrate MCP tool path")
	}
	for header := range toolHTTPHeaders(tool) {
		if workerexecutor.SubstrateMCPReservedHeader(header) ||
			(strings.EqualFold(strings.TrimSpace(header), fiber.HeaderAuthorization) && tool.Spec.HTTP.AuthSecretRef != nil) {
			return nil, fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("tool configures reserved header %q", header))
		}
	}
	if tool.Spec.HTTP != nil && tool.Spec.HTTP.AuthInject == "body" && tool.Spec.HTTP.AuthSecretRef != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "MCP tools do not support authInject=body")
	}
	return tool, nil
}

// substrateMCPProxyCredential checks the caller may use every credential the
// Tool's invocation uses, then resolves the Tool's configured auth Secret.
// That includes egress credentials the Substrate gateway injects into the
// actor's outbound requests, which the proxy never reads. Kubernetes
// identities need `get secrets/<name>`, transaction tokens need the
// credential-read scope and a matching secret constraint.
func (h *Handlers) substrateMCPProxyCredential(c fiber.Ctx, ui *UserInfo, tool *corev1alpha1.Tool) (string, error) {
	secretNames := aitools.SubstrateMCPEgressCredentialSecretNames(tool)
	var ref *corev1alpha1.SecretKeySelector
	if tool.Spec.HTTP != nil && tool.Spec.HTTP.AuthSecretRef != nil {
		ref = tool.Spec.HTTP.AuthSecretRef
		secretNames = append(secretNames, ref.Name)
	}
	if len(secretNames) == 0 {
		return "", nil
	}
	switch ui.AuthType {
	case AuthTypeTokenReview:
		for _, name := range secretNames {
			if strings.TrimSpace(name) == "" {
				return "", fiber.NewError(fiber.StatusForbidden, "not authorized to use the tool credential")
			}
			if err := authorizeKubernetesResourceAction(c.Context(), h.clientset, ui, tool.Namespace, "get", "", "secrets", name); err != nil {
				return "", fiber.NewError(fiber.StatusForbidden, "not authorized to use the tool credential")
			}
		}
	case AuthTypeContextToken:
		token := ui.ContextToken
		failures := []string{}
		if token == nil || !hasAnyScope(token.Scopes, h.contextTokenAuthorization.SecretCredentialReadScopes()) {
			failures = append(failures, "tool credential requires a credential-read scope")
		}
		if token != nil {
			if want, ok := contextString(token.TransactionContext, "secret"); ok {
				for _, name := range secretNames {
					if want != name {
						failures = append(failures, "tool credential secret does not match token context")
						break
					}
				}
			}
		}
		if len(failures) > 0 {
			if err := h.handleContextTokenAuthorizationFailures(token, substrateMCPProxyAction, failures); err != nil {
				return "", err
			}
			return "", fiber.NewError(fiber.StatusForbidden, "not authorized to use the tool credential")
		}
	default:
		return "", fiber.NewError(fiber.StatusForbidden, "not authorized to use the tool credential")
	}
	if ref == nil {
		return "", nil
	}
	secret := &corev1.Secret{}
	if err := h.client.Get(c.Context(), types.NamespacedName{Namespace: tool.Namespace, Name: ref.Name}, secret); err != nil {
		return "", fiber.NewError(fiber.StatusServiceUnavailable, "tool credential is unavailable")
	}
	value := strings.TrimSpace(string(secret.Data[ref.Key]))
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", fiber.NewError(fiber.StatusServiceUnavailable, "tool credential is unavailable")
	}
	return value, nil
}

func toolHTTPHeaders(tool *corev1alpha1.Tool) map[string]string {
	if tool == nil || tool.Spec.HTTP == nil {
		return nil
	}
	return tool.Spec.HTTP.Headers
}

func substrateMCPProxyTimeout(tool *corev1alpha1.Tool) time.Duration {
	timeout := substrateMCPProxyDefaultTimeout
	if tool != nil && tool.Spec.HTTP != nil && tool.Spec.HTTP.Timeout != nil && tool.Spec.HTTP.Timeout.Duration > 0 {
		timeout = tool.Spec.HTTP.Timeout.Duration
	}
	return min(timeout, substrateMCPProxyMaxTimeout)
}

// substrateMCPProxyStream streams a bounded actor response and releases the
// upstream request when fasthttp closes it. Exceeding the bound fails the
// response rather than truncating a JSON-RPC message silently.
type substrateMCPProxyStream struct {
	body      io.ReadCloser
	remaining int64
	cancel    context.CancelFunc
	closed    bool
}

func (s *substrateMCPProxyStream) Read(p []byte) (int, error) {
	if s.remaining <= 0 {
		var probe [1]byte
		if n, _ := s.body.Read(probe[:]); n > 0 {
			return 0, errors.New("MCP actor response exceeds the proxy limit")
		}
		return 0, io.EOF
	}
	if int64(len(p)) > s.remaining {
		p = p[:s.remaining]
	}
	n, err := s.body.Read(p)
	s.remaining -= int64(n)
	return n, err
}

func (s *substrateMCPProxyStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	err := s.body.Close()
	s.cancel()
	return err
}
