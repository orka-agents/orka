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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
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
	// substrateMCPProxyWriteTimeout bounds writing one proxied response to the
	// caller. The upstream timeout cannot interrupt a write blocked by a
	// caller that stopped reading. It exceeds the longest upstream timeout, so
	// it never cuts short a response the Actor is still allowed to send.
	substrateMCPProxyWriteTimeout  = substrateMCPProxyMaxTimeout + time.Second
	substrateMCPProxyMaxTokenBytes = 16 << 10
	substrateMCPProxyAction        = "callSubstrateMCPTool"
	// substrateMCPProxyCallNameMember is the tools/call member naming the callable.
	substrateMCPProxyCallNameMember = apiFieldName
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
		// Authorization named this Tool. Forward only the message the proxy
		// checked, never the caller's bytes.
		if body, err = substrateMCPProxyRequestBody(body, tool.Name); err != nil {
			return err
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
	// The caller no longer holds the values the proxy injected, so it cannot
	// redact them from what the Actor returns.
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

// isSubstrateMCPProxyRoute reports whether a request targets the MCP proxy
// routes, POST and DELETE /api/v1/tools/:name/mcp, matching the path the way
// Fiber routes it: on the original path, without regard to case or a trailing
// slash.
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

// substrateMCPProxyCredential resolves the Tool's configured auth Secret
// after checking the caller may use it: Kubernetes identities need
// `get secrets/<name>`, transaction tokens need the credential-read scope.
func (h *Handlers) substrateMCPProxyCredential(c fiber.Ctx, ui *UserInfo, tool *corev1alpha1.Tool) (string, error) {
	if tool.Spec.HTTP == nil || tool.Spec.HTTP.AuthSecretRef == nil {
		return "", nil
	}
	ref := tool.Spec.HTTP.AuthSecretRef
	switch ui.AuthType {
	case AuthTypeTokenReview:
		if err := authorizeKubernetesResourceAction(c.Context(), h.clientset, ui, tool.Namespace, "get", "", "secrets", ref.Name); err != nil {
			return "", fiber.NewError(fiber.StatusForbidden, "not authorized to use the tool credential")
		}
	case AuthTypeContextToken:
		token := ui.ContextToken
		failures := []string{}
		if token == nil || !hasAnyScope(token.Scopes, h.contextTokenAuthorization.SecretCredentialReadScopes()) {
			failures = append(failures, "tool credential requires a credential-read scope")
		}
		if token != nil {
			if want, ok := contextString(token.TransactionContext, "secret"); ok && want != ref.Name {
				failures = append(failures, "tool credential secret does not match token context")
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

// substrateMCPProxyMessage is the JSON-RPC envelope the proxy forwards.
type substrateMCPProxyMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// substrateMCPProxyRequestBody admits one MCP message for the Tool the caller
// was authorized to use and returns the body to forward.
//
// A Tool stands for the single callable that carries its name, but its Actor's
// MCP server may expose others. The proxy therefore admits only the session
// handshake and a tools/call for that name, and rejects batches, other methods
// and client responses. It forwards a message rebuilt from the members it
// checked, so a duplicate or differently cased member the Actor's parser might
// prefer never reaches it.
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
		// Rebuilding drops duplicate members, which a map keeps only once.
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

// substrateMCPProxyInjectedSecrets lists the values the proxy added to the
// Actor request. Longer values come first so one that contains another is
// replaced whole.
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
	// substrateMCPProxyPartialMatch means data ended while it still spelled
	// the beginning of the secret.
	substrateMCPProxyPartialMatch
	substrateMCPProxyFullMatch
)

// decodeSubstrateMCPProxyJSONEscape decodes the JSON string escape that data
// begins with. A surrogate pair is one escape.
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
		// data begins with a backslash; expect `\uXXXX`.
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

// substrateMCPProxyMatcher matches one secret at the start of data.
type substrateMCPProxyMatcher struct {
	data, secret []byte
	// tried records positions where both readings of a backslash failed, so a
	// run of backslashes cannot be explored more than once per position.
	tried map[[2]int]substrateMCPProxyMatch
}

// matchSubstrateMCPProxySecret reports whether data begins with secret, either
// byte for byte or with any of its characters written as a JSON string escape
// (`\/`, `\u002B`, ...). A JSON serializer may choose those freely and the
// caller decodes them back to the secret. It returns the matched length.
func matchSubstrateMCPProxySecret(data, secret []byte) (int, substrateMCPProxyMatch) {
	matcher := substrateMCPProxyMatcher{data: data, secret: secret}
	return matcher.match(0, 0)
}

// match continues at data[k:] and secret[j:] and returns where the match ends.
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
		// A backslash is either the start of an escape or, when the secret has
		// one here, itself. Try both.
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

// redactSubstrateMCPProxy appends data to out with every secret replaced and
// returns how much of data it consumed. Unless final, it stops before a tail
// that may still become a secret once more data arrives; because that is
// decided by matching rather than by length, a complete SSE event is never
// held back.
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
				// A longer secret may still complete here; do not settle for
				// a shorter one or release these bytes yet.
				return append(out, data[flushed:i]...), i
			}
		}
		i++
	}
	return append(out, data[flushed:]...), len(data)
}

// substrateMCPProxyStream streams a bounded actor response and releases the
// upstream request when fasthttp closes it. Exceeding the bound fails the
// response rather than truncating a JSON-RPC message silently.
//
// Values the proxy injected are replaced before they reach the caller, also
// when one spans two reads.
type substrateMCPProxyStream struct {
	body      io.ReadCloser
	remaining int64
	cancel    context.CancelFunc
	closed    bool

	secrets [][]byte
	// pending is response data that may still become a secret.
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
		// Only a clean end of the upstream body completes a response that
		// reached the bound; a failed read must not pass for one.
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
