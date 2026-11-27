/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
	"go.opentelemetry.io/otel/trace"
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
	substrateMCPProxyMaxRequestBytes    = 4 << 20
	substrateMCPProxyMaxResponseBytes   = 16 << 20
	substrateMCPProxyDefaultTimeout     = 30 * time.Second
	substrateMCPProxyMaxTimeout         = 5 * time.Minute
	substrateMCPProxyWriteTimeout       = substrateMCPProxyMaxTimeout + time.Second
	substrateMCPProxyMaxTokenBytes      = 16 << 10
	substrateMCPProxyRequestReadTimeout = 30 * time.Second
	substrateMCPProxyAction             = "callSubstrateMCPTool"
	substrateMCPProxyCallNameMember     = apiFieldName
)

type SubstrateMCPProxyConfig struct {
	Router *workerexecutor.SubstrateMCPRouter
}

var substrateMCPProxyRequestHeaders = []string{
	fiber.HeaderAccept, "MCP-Protocol-Version", "Mcp-Session-Id", "Idempotency-Key", "traceparent", "tracestate",
}

var substrateMCPProxyResponseHeaders = []string{fiber.HeaderContentType, "Mcp-Session-Id", "MCP-Protocol-Version"}

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
	case AuthTypeContextToken:
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
		if c.Request().Header.ContentLength() > substrateMCPProxyMaxRequestBytes ||
			!readBoundedRawBody(c, substrateMCPProxyMaxRequestBytes) {
			return fiber.NewError(fiber.StatusRequestEntityTooLarge, "MCP request body is too large")
		}
		body = c.Body()
		if len(body) > substrateMCPProxyMaxRequestBytes {
			return fiber.NewError(fiber.StatusRequestEntityTooLarge, "MCP request body is too large")
		}
		mediaType, _, parseErr := mime.ParseMediaType(c.Get(fiber.HeaderContentType))
		if parseErr != nil || mediaType != fiber.MIMEApplicationJSON {
			return fiber.NewError(fiber.StatusUnsupportedMediaType, "MCP requests must be application/json")
		}
		if body, err = substrateMCPProxyRequestBody(body, tool.Name); err != nil {
			return err
		}
	}
	toolTxnToken := strings.TrimSpace(c.Get(workerexecutor.SubstrateMCPToolTxnTokenHeader))
	if len(toolTxnToken) > substrateMCPProxyMaxTokenBytes || strings.ContainsAny(toolTxnToken, "\r\n") {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tool transaction token header")
	}

	timeout := substrateMCPProxyTimeout(tool)
	upstreamCtx, cancel := context.WithTimeout(
		trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(c.Context())), timeout)
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
	injected := substrateMCPProxyInjectedSecrets(tool, credential, toolTxnToken)
	stream := newSubstrateMCPProxyStream(response.Body, cancel, injected)
	for _, header := range substrateMCPProxyResponseHeaders {
		if value := response.Header.Get(header); value != "" {
			redacted, _ := redactSubstrateMCPProxy(nil, []byte(value), injected, true)
			c.Set(header, string(redacted))
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

func isSubstrateMCPProxyRoute(header *fasthttp.RequestHeader) bool {
	if !header.IsPost() && !header.IsDelete() {
		return false
	}
	var uri fasthttp.URI
	if uri.Parse(header.Host(), header.RequestURI()) != nil {
		return false
	}
	parts := bytes.Split(bytes.Trim(uri.PathOriginal(), "/"), []byte("/"))
	return len(parts) == 5 && len(parts[3]) > 0 &&
		bytes.EqualFold(parts[0], []byte("api")) && bytes.EqualFold(parts[1], []byte("v1")) &&
		bytes.EqualFold(parts[2], []byte("tools")) && bytes.EqualFold(parts[4], []byte("mcp"))
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

type substrateMCPProxyMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func substrateMCPProxyRequestBody(body []byte, toolName string) ([]byte, error) {
	malformed := func(message string) error { return fiber.NewError(fiber.StatusBadRequest, message) }
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil || envelope == nil {
		return nil, malformed("MCP request must be a single JSON-RPC message; batches are not supported")
	}
	for member := range envelope {
		switch member {
		case "jsonrpc", "id", "method", "params":
		default:
			return nil, malformed(fmt.Sprintf("MCP request has unsupported member %q", member))
		}
	}
	message := substrateMCPProxyMessage{ID: envelope["id"], Params: envelope["params"]}
	if json.Unmarshal(envelope["jsonrpc"], &message.JSONRPC) != nil || message.JSONRPC != "2.0" {
		return nil, malformed("MCP request must be JSON-RPC 2.0")
	}
	if json.Unmarshal(envelope["method"], &message.Method) != nil || message.Method == "" {
		return nil, malformed("MCP request must name a method")
	}
	if id := bytes.TrimSpace(message.ID); len(id) > 0 && (id[0] == '{' || id[0] == '[') {
		return nil, malformed("MCP request id must be a string or a number")
	}
	var params map[string]json.RawMessage
	if len(message.Params) > 0 {
		if err := json.Unmarshal(message.Params, &params); err != nil || params == nil {
			return nil, malformed("MCP request params must be an object")
		}
	}
	switch message.Method {
	case "initialize", "notifications/initialized":
	case "tools/call":
		var name string
		for member := range params {
			if member != substrateMCPProxyCallNameMember && strings.EqualFold(member, substrateMCPProxyCallNameMember) {
				return nil, malformed(fmt.Sprintf("MCP tools/call has ambiguous member %q", member))
			}
		}
		if json.Unmarshal(params[substrateMCPProxyCallNameMember], &name) != nil || name != toolName {
			return nil, fiber.NewError(fiber.StatusForbidden, "MCP tools/call must name the Tool this endpoint serves")
		}
	default:
		return nil, fiber.NewError(fiber.StatusForbidden, fmt.Sprintf("MCP method %q is not available through the Orka MCP proxy", message.Method))
	}
	if params != nil {
		rebuilt, err := json.Marshal(params)
		if err != nil {
			return nil, malformed("MCP request params must be an object")
		}
		message.Params = rebuilt
	}
	forwarded, err := json.Marshal(message)
	if err != nil {
		return nil, malformed("MCP request must be a single JSON-RPC message")
	}
	return forwarded, nil
}

const substrateMCPProxyRedaction = "[REDACTED]"

func substrateMCPProxyInjectedSecrets(tool *corev1alpha1.Tool, credential, toolTxnToken string) [][]byte {
	values := append(workerexecutor.SubstrateMCPSensitiveHeaderValues(toolHTTPHeaders(tool)), credential, toolTxnToken)
	seen := map[string]struct{}{}
	secrets := [][]byte{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if _, ok := seen[value]; ok || value == "" {
			continue
		}
		seen[value] = struct{}{}
		secrets = append(secrets, []byte(value))
	}
	slices.SortStableFunc(secrets, func(a, b []byte) int { return len(b) - len(a) })
	return secrets
}

type substrateMCPProxyMatch int

const (
	substrateMCPProxyNoMatch substrateMCPProxyMatch = iota
	substrateMCPProxyPartialMatch
	substrateMCPProxyFullMatch
)

func decodeSubstrateMCPProxyJSONEscape(data []byte) (rune, int, substrateMCPProxyMatch) {
	if len(data) < 2 {
		return 0, 0, substrateMCPProxyPartialMatch
	}
	switch data[1] {
	case '"', '\\', '/':
		return rune(data[1]), 2, substrateMCPProxyFullMatch
	case 'b':
		return '\b', 2, substrateMCPProxyFullMatch
	case 'f':
		return '\f', 2, substrateMCPProxyFullMatch
	case 'n':
		return '\n', 2, substrateMCPProxyFullMatch
	case 'r':
		return '\r', 2, substrateMCPProxyFullMatch
	case 't':
		return '\t', 2, substrateMCPProxyFullMatch
	case 'u':
	default:
		return 0, 0, substrateMCPProxyNoMatch
	}
	unit := func(data []byte) (rune, substrateMCPProxyMatch) {
		var value rune
		for i := 1; i < 6; i++ {
			if i >= len(data) {
				return 0, substrateMCPProxyPartialMatch
			}
			c := data[i]
			switch {
			case i == 1:
				if c != 'u' {
					return 0, substrateMCPProxyNoMatch
				}
			case c >= '0' && c <= '9':
				value = value<<4 | rune(c-'0')
			case c >= 'a' && c <= 'f':
				value = value<<4 | rune(c-'a'+10)
			case c >= 'A' && c <= 'F':
				value = value<<4 | rune(c-'A'+10)
			default:
				return 0, substrateMCPProxyNoMatch
			}
		}
		return value, substrateMCPProxyFullMatch
	}
	high, status := unit(data)
	if status != substrateMCPProxyFullMatch || !utf16.IsSurrogate(high) {
		return high, 6, status
	}
	if len(data) == 6 {
		return 0, 0, substrateMCPProxyPartialMatch
	}
	if data[6] != '\\' {
		return 0, 0, substrateMCPProxyNoMatch
	}
	low, status := unit(data[6:])
	if status != substrateMCPProxyFullMatch {
		return 0, 0, status
	}
	if combined := utf16.DecodeRune(high, low); combined != utf8.RuneError {
		return combined, 12, substrateMCPProxyFullMatch
	}
	return 0, 0, substrateMCPProxyNoMatch
}

type substrateMCPProxyMatcher struct {
	data, secret []byte
	tried        map[[2]int]substrateMCPProxyMatch
}

func matchSubstrateMCPProxySecret(data, secret []byte) (int, substrateMCPProxyMatch) {
	matcher := substrateMCPProxyMatcher{data: data, secret: secret}
	return matcher.match(0, 0)
}

func (m *substrateMCPProxyMatcher) match(k, j int) (int, substrateMCPProxyMatch) {
	for j < len(m.secret) {
		if k >= len(m.data) {
			return 0, substrateMCPProxyPartialMatch
		}
		if m.data[k] != '\\' {
			if m.data[k] != m.secret[j] {
				return 0, substrateMCPProxyNoMatch
			}
			k++
			j++
			continue
		}
		literal := m.secret[j] == '\\'
		if outcome, ok := m.tried[[2]int{k, j}]; ok {
			return 0, outcome
		}
		outcome := substrateMCPProxyNoMatch
		decoded, size, status := decodeSubstrateMCPProxyJSONEscape(m.data[k:])
		want, wantSize := utf8.DecodeRune(m.secret[j:])
		switch {
		case status == substrateMCPProxyPartialMatch:
			outcome = substrateMCPProxyPartialMatch
		case status == substrateMCPProxyFullMatch && decoded == want:
			end, rest := m.match(k+size, j+wantSize)
			if rest == substrateMCPProxyFullMatch {
				return end, rest
			}
			outcome = rest
		}
		if literal {
			end, rest := m.match(k+1, j+1)
			if rest == substrateMCPProxyFullMatch {
				return end, rest
			}
			outcome = max(outcome, rest)
			if m.tried == nil {
				m.tried = map[[2]int]substrateMCPProxyMatch{}
			}
			m.tried[[2]int{k, j}] = outcome
		}
		return 0, outcome
	}
	return k, substrateMCPProxyFullMatch
}

func redactSubstrateMCPProxy(out, data []byte, secrets [][]byte, final bool) ([]byte, int) {
	flushed := 0
scan:
	for i := 0; i < len(data); {
		for _, secret := range secrets {
			if data[i] != '\\' && data[i] != secret[0] {
				continue
			}
			length, status := matchSubstrateMCPProxySecret(data[i:], secret)
			if status == substrateMCPProxyFullMatch {
				out = append(append(out, data[flushed:i]...), substrateMCPProxyRedaction...)
				i += length
				flushed = i
				continue scan
			}
			if status == substrateMCPProxyPartialMatch && !final {
				return append(out, data[flushed:i]...), i
			}
		}
		i++
	}
	return append(out, data[flushed:]...), len(data)
}

type substrateMCPProxyStream struct {
	body      io.ReadCloser
	remaining int64
	cancel    context.CancelFunc
	closed    bool

	secrets [][]byte
	pending []byte
	ready   []byte
	err     error
}

func newSubstrateMCPProxyStream(body io.ReadCloser, cancel context.CancelFunc, secrets [][]byte) *substrateMCPProxyStream {
	return &substrateMCPProxyStream{body: body, remaining: substrateMCPProxyMaxResponseBytes, cancel: cancel, secrets: secrets}
}

func (s *substrateMCPProxyStream) Read(p []byte) (int, error) {
	if len(s.secrets) == 0 {
		return s.readBounded(p)
	}
	for len(s.ready) == 0 && s.err == nil {
		chunk := make([]byte, max(len(p), 4096))
		n, err := s.readBounded(chunk)
		s.pending = append(s.pending, chunk[:n]...)
		s.err = err
		var consumed int
		s.ready, consumed = redactSubstrateMCPProxy(s.ready[:0], s.pending, s.secrets, err != nil)
		s.pending = append(s.pending[:0], s.pending[consumed:]...)
	}
	if len(s.ready) == 0 {
		return 0, s.err
	}
	n := copy(p, s.ready)
	s.ready = s.ready[n:]
	return n, nil
}

func (s *substrateMCPProxyStream) readBounded(p []byte) (int, error) {
	if s.remaining <= 0 {
		var probe [1]byte
		for {
			n, err := s.body.Read(probe[:])
			if n > 0 {
				return 0, errors.New("MCP actor response exceeds the proxy limit")
			}
			if err != nil {
				return 0, err
			}
		}
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
