package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net/http"
	"reflect"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/transactiontoken"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	remoteMCPResponseLimit  = 1 << 20
	remoteMCPDiscoveryLimit = 2 << 20
	remoteMCPMaxPages       = 8
	remoteMCPMaxTools       = 1024
	remoteMCPTextContent    = "text"
)

// SetRemoteMCPPreparationFence installs the native Task's frozen-dependency
// validation before use. It runs after credentials and transport are resolved,
// but before the first MCP request, for both discovery and invocation.
func (e *ToolExecutor) SetRemoteMCPPreparationFence(fence func(context.Context, *corev1alpha1.Tool) error) {
	e.remotePreparationFence = fence
}

// VerifyRemoteMCPTool verifies the operator-reviewed descriptor under the same
// Task authority and gateway transport used for calls. It exposes no server
// instructions or metadata and retains no protocol session between operations.
func (e *ToolExecutor) VerifyRemoteMCPTool(ctx context.Context, tool *corev1alpha1.Tool) error {
	if err := aitools.ValidateRemoteMCPConfiguration(tool); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, remoteMCPTimeout(tool))
	defer cancel()
	prepared, err := e.prepareRemoteMCPRequest(ctx, tool, json.RawMessage(`{}`), true)
	if err != nil {
		return err
	}
	httpClient, err := toolHTTPClient(e.client, prepared.httpConfig.Timeout, prepared.gatewayTLS, true)
	if err != nil {
		return err
	}
	if _, err = e.executeRemoteMCPSession(ctx, httpClient, prepared, true); err != nil {
		return err
	}
	return e.bindRemoteMCPTransport(prepared, true)
}

func (e *ToolExecutor) prepareRemoteMCPRequest(ctx context.Context, tool *corev1alpha1.Tool, args json.RawMessage, verifyOnly bool) (preparedToolRequest, error) {
	if err := aitools.ValidateRemoteMCPConfiguration(tool); err != nil {
		return preparedToolRequest{}, err
	}
	if e == nil || tool.Namespace != e.namespace {
		return preparedToolRequest{}, errors.New("remote MCP requires the Task namespace")
	}
	if !e.transactionAuthoritySet || e.transactionToken == "" || len(e.currentParentTransactionScopes()) == 0 {
		return preparedToolRequest{}, errors.New("remote MCP requires explicit Task transaction authority and scopes")
	}
	if !e.credentialAuthorityEnforced {
		return preparedToolRequest{}, errors.New("remote MCP requires Task credential authorization")
	}
	params, err := decodeToolArguments(args)
	if err != nil || params == nil {
		return preparedToolRequest{}, errors.New("remote MCP arguments must be a JSON object")
	}
	// Discovery has no call arguments. Validate real calls before any credential
	// reads or exchanges, and never expose validator errors containing input data.
	if !verifyOnly {
		if err := aitools.ValidateRemoteMCPNumericBudget(params); err != nil {
			return preparedToolRequest{}, err
		}
		schema, err := aitools.ResolveRemoteMCPParameters(tool.Spec.Parameters)
		if err != nil {
			return preparedToolRequest{}, err
		}
		if err := schema.Validate(params); err != nil {
			return preparedToolRequest{}, errors.New("remote MCP arguments do not match reviewed parameters")
		}
	}
	h := *tool.Spec.HTTP
	token, err := e.remoteMCPSecret(ctx, *h.AuthSecretRef)
	if err != nil {
		return preparedToolRequest{}, err
	}
	body, err := json.Marshal(mcpToolCallRequest{JSONRPC: mcpJSONRPCVersion, ID: mcpToolCallRequestID, Method: mcpToolsCallMethod, Params: mcpToolCallParameters{Name: tool.Spec.MCP.Remote.ToolName, Arguments: params}})
	if err != nil {
		return preparedToolRequest{}, errors.New("invalid remote MCP arguments")
	}
	h.URL = tool.Spec.MCP.Remote.URL
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return preparedToolRequest{}, errors.New("invalid remote MCP endpoint")
	}
	req.GetBody = nil // Never make an automatically replayable call.
	for k, v := range h.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(mcpProtocolVersionHeader, mcpProtocolVersion)
	prepared := preparedToolRequest{httpConfig: h, request: req, authToken: token, redactionSecrets: []string{token, e.transactionToken}, mcp: true, remote: tool}
	if err := e.applyOutboundAccessPolicy(ctx, tool, &prepared); err != nil {
		return preparedToolRequest{}, gatewayMCPError{Err: err}
	}
	if !prepared.gateway || prepared.direct || strings.TrimSpace(prepared.transactionToken) == "" || strings.TrimSpace(req.Header.Get(transactiontoken.HeaderName)) == "" {
		return preparedToolRequest{}, errors.New("remote MCP requires a gateway with Task transaction authority")
	}
	// Validate only after the final resolution. The request and TLS material below
	// stay frozen: resolving again after this fence would reopen the startup race.
	if e.remotePreparationFence != nil {
		if err := e.remotePreparationFence(ctx, tool); err != nil {
			return preparedToolRequest{}, err
		}
	}
	if err := e.bindRemoteMCPTransport(prepared, false); err != nil {
		return preparedToolRequest{}, err
	}
	return prepared, nil
}

func (e *ToolExecutor) remoteMCPSecret(ctx context.Context, ref corev1alpha1.SecretKeySelector) (string, error) {
	if err := outboundaccess.ValidateCredentialAuthority(true, e.credentialScopeAllowed, e.credentialSecret, []string{ref.Name}, false); err != nil {
		return "", err
	}
	// Legacy mounted task/agent files are keyed only by key, not by Secret name.
	// Remote credentials must come from the exact namespace/name/key API selector.
	if e.k8sClient == nil {
		return "", errors.New("remote MCP credential client is unavailable")
	}
	secret, err := e.k8sClient.CoreV1().Secrets(e.namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return "", errors.New("remote MCP auth Secret is unavailable")
	}
	if !secret.DeletionTimestamp.IsZero() {
		return "", errors.New("remote MCP auth Secret is terminating")
	}
	value := strings.TrimSpace(string(secret.Data[ref.Key]))
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("remote MCP auth Secret key is empty or invalid")
	}
	return value, nil
}

func (e *ToolExecutor) bindRemoteMCPTransport(p preparedToolRequest, record bool) error {
	// Freeze the effective dial endpoint, original authority, TLS and named
	// resource credential. Token exchanges may mint a fresh Task token per session.
	headers := p.request.Header.Clone()
	headers.Del(transactiontoken.HeaderName)
	data, err := json.Marshal(struct {
		URL, Host string
		Headers   http.Header
		TLS       any
	}{p.request.URL.String(), p.request.Host, headers, p.gatewayTLS})
	if err != nil {
		return errors.New("cannot bind remote MCP transport")
	}
	digest := sha256.Sum256(data)
	key := p.remote.Namespace + "/" + p.remote.Name
	e.remoteMu.Lock()
	defer e.remoteMu.Unlock()
	if previous, ok := e.remoteBindings[key]; ok && previous != digest {
		return errors.New("remote MCP transport or credential changed after exposure")
	}
	if record {
		if e.remoteBindings == nil {
			e.remoteBindings = make(map[string][32]byte)
		}
		e.remoteBindings[key] = digest
	}
	return nil
}

func remoteMCPTimeout(tool *corev1alpha1.Tool) time.Duration {
	timeout := 30 * time.Second
	if tool.Spec.HTTP != nil && tool.Spec.HTTP.Timeout != nil {
		timeout = tool.Spec.HTTP.Timeout.Duration
	}
	return min(timeout, 2*time.Minute)
}

func (e *ToolExecutor) executeRemoteMCPSession(ctx context.Context, httpClient *http.Client, p preparedToolRequest, verifyOnly bool) (result string, err error) {
	ctx, cancel := context.WithTimeout(ctx, remoteMCPTimeout(p.remote))
	defer cancel()
	// Exchange/parser errors below contain only local diagnostics, never server
	// error bodies or credential-bearing transport errors. Preserve that context.
	defer func() {
		if err != nil {
			err = fmt.Errorf("remote MCP session: %w", err)
		}
	}()
	// The admitted transport supplies the only credentials. Never inherit a jar.
	clientCopy := *httpClient
	clientCopy.Jar = nil
	httpClient = &clientCopy
	p.request = p.request.Clone(ctx)
	body, _ := marshalMCPInitializeRequest()
	req, err := newMCPRequest(ctx, p, body, mcpProtocolVersion)
	if err != nil {
		return "", err
	}
	response, header, err := remoteMCPExchange(httpClient, req, mcpInitializeRequestID)
	session := header.Get(mcpSessionIDHeader)
	if session != "" {
		if len(session) > 1024 || strings.IndexFunc(session, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
			return "", errors.New("invalid remote MCP session identifier")
		}
		defer func() { _ = e.terminateMCPSession(ctx, httpClient, p, session, mcpProtocolVersion) }()
	}
	if err != nil {
		return "", err
	}
	var initialized struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
		ServerInfo      *struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			Title   string `json:"title,omitempty"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions,omitempty"`
	}
	if err := decodeRemoteMCPObject(response, &initialized); err != nil {
		return "", err
	}
	if initialized.ProtocolVersion != mcpProtocolVersion {
		return "", errors.New("unsupported remote MCP protocol version")
	}
	if initialized.ServerInfo == nil || initialized.ServerInfo.Name == "" || initialized.ServerInfo.Version == "" {
		return "", errors.New("remote MCP server identity is missing")
	}
	var toolsCapability struct {
		ListChanged bool `json:"listChanged,omitempty"`
	}
	if err := decodeRemoteMCPObject(initialized.Capabilities["tools"], &toolsCapability); err != nil {
		return "", errors.New("remote MCP server does not support tools")
	}
	p.request.Header.Set(mcpProtocolVersionHeader, mcpProtocolVersion)
	if session != "" {
		p.request.Header.Set(mcpSessionIDHeader, session)
	}
	body, _ = marshalMCPInitializedNotification()
	req, err = newMCPRequest(ctx, p, body, mcpProtocolVersion)
	if err != nil {
		return "", err
	}
	if _, _, err = remoteMCPExchange(httpClient, req, ""); err != nil {
		return "", err
	}
	if err := verifyRemoteMCPDescriptor(ctx, httpClient, p); err != nil {
		return "", err
	}
	if verifyOnly {
		return "", nil
	}
	if key := toolIdempotencyKeyFromContext(ctx); key != "" {
		p.request.Header.Set(toolIdempotencyKeyHeader, key)
	}
	response, _, err = remoteMCPExchange(httpClient, p.request, mcpToolCallRequestID)
	if err != nil {
		return "", ToolRequestAttemptedError{Err: err}
	}
	result, err = projectRemoteMCPResult(response, p.secrets(session)...)
	if err != nil {
		return "", ToolRequestAttemptedError{Err: err}
	}
	return result, nil
}

func verifyRemoteMCPDescriptor(ctx context.Context, httpClient *http.Client, p preparedToolRequest) error {
	var reviewed any
	if err := decodeRemoteMCPJSON(p.remote.Spec.Parameters.Raw, &reviewed); err != nil {
		return err
	}
	cursor := ""
	seenCursors := map[string]bool{}
	seenNames := map[string]bool{}
	selected := false
	total := 0
	for page := range remoteMCPMaxPages {
		id := fmt.Sprintf("list-%d", page)
		params := map[string]string{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": mcpJSONRPCVersion, "id": id, "method": "tools/list", "params": params})
		req, err := newMCPRequest(ctx, p, body, mcpProtocolVersion)
		if err != nil {
			return err
		}
		raw, _, err := remoteMCPExchange(httpClient, req, id)
		if err != nil {
			return err
		}
		total += len(raw)
		if total > remoteMCPDiscoveryLimit {
			return errors.New("remote MCP discovery exceeds size limit")
		}
		var list struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor,omitempty"`
		}
		if err := decodeRemoteMCPObject(raw, &list); err != nil {
			return err
		}
		if list.Tools == nil {
			return errors.New("remote MCP tools/list missing tools")
		}
		for _, rawTool := range list.Tools {
			var descriptor struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema"`
			}
			if err := json.Unmarshal(rawTool, &descriptor); err != nil || descriptor.Name == "" {
				return errors.New("invalid remote MCP descriptor")
			}
			if seenNames[descriptor.Name] {
				return errors.New("duplicate remote MCP tool name")
			}
			seenNames[descriptor.Name] = true
			if len(seenNames) > remoteMCPMaxTools {
				return errors.New("remote MCP discovery exceeds tool limit")
			}
			if descriptor.Name != p.remote.Spec.MCP.Remote.ToolName {
				continue
			}
			var observed any
			if err := decodeRemoteMCPJSON(descriptor.InputSchema, &observed); err != nil {
				return err
			}
			if err := aitools.ValidateRemoteMCPNumericBudget(observed); err != nil {
				return err
			}
			var selectedDescriptor struct {
				Name         string          `json:"name"`
				Description  string          `json:"description,omitempty"`
				Title        string          `json:"title,omitempty"`
				InputSchema  json.RawMessage `json:"inputSchema"`
				OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
				Annotations  json.RawMessage `json:"annotations,omitempty"`
			}
			// Description, title, annotations and outputSchema never enter model definitions.
			if err := decodeRemoteMCPObject(rawTool, &selectedDescriptor); err != nil {
				return err
			}
			if !reflect.DeepEqual(canonicalRemoteMCPNumbers(reviewed), canonicalRemoteMCPNumbers(observed)) {
				return errors.New("remote MCP input schema differs from reviewed parameters")
			}
			selected = true
		}
		if list.NextCursor == "" {
			if !selected {
				return errors.New("selected remote MCP tool is missing")
			}
			return nil
		}
		if len(list.NextCursor) > 4096 || seenCursors[list.NextCursor] {
			return errors.New("invalid remote MCP pagination cursor")
		}
		seenCursors[list.NextCursor] = true
		cursor = list.NextCursor
	}
	return errors.New("remote MCP discovery exceeds page limit")
}

// remoteMCPExchange keeps the existing prepared raw-JSON/gateway request path,
// but rejects interactions, metadata and ambiguous envelopes before accepting
// the terminal response. It does not drain an SSE stream after that response:
// Streamable HTTP servers SHOULD close then, but are not required to do so.
func remoteMCPExchange(httpClient *http.Client, req *http.Request, id string) (json.RawMessage, http.Header, error) {
	req.GetBody = nil
	resp, err := httpClient.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, nil, req.Context().Err()
		}
		return nil, nil, errors.New("remote MCP transport failed")
	}
	defer resp.Body.Close() //nolint:errcheck
	header := resp.Header.Clone()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, header, fmt.Errorf("remote MCP returned HTTP %d", resp.StatusCode)
	}
	if id == "" {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 2))
		if err != nil || len(body) != 0 || resp.StatusCode != http.StatusAccepted {
			return nil, header, errors.New("invalid remote MCP notification response")
		}
		return nil, header, nil
	}
	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, header, errors.New("invalid remote MCP content type")
	}
	var body []byte
	switch contentType {
	case "application/json":
		body, err = io.ReadAll(io.LimitReader(resp.Body, remoteMCPResponseLimit+1))
	case "text/event-stream":
		body, err = readRemoteMCPEvent(resp.Body)
	default:
		return nil, header, errors.New("unsupported remote MCP content type")
	}
	if err != nil {
		if req.Context().Err() != nil {
			return nil, header, req.Context().Err()
		}
		return nil, header, err
	}
	if len(body) > remoteMCPResponseLimit {
		return nil, header, errors.New("remote MCP response exceeds size limit")
	}
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := decodeRemoteMCPObject(body, &envelope); err != nil {
		return nil, header, err
	}
	var responseID string
	if envelope.JSONRPC != mcpJSONRPCVersion || json.Unmarshal(envelope.ID, &responseID) != nil || responseID != id {
		return nil, header, errors.New("invalid remote MCP response identity")
	}
	if len(envelope.Error) > 0 {
		return nil, header, errors.New("remote MCP returned a protocol error")
	}
	if len(envelope.Result) == 0 || bytes.Equal(envelope.Result, []byte("null")) {
		return nil, header, errors.New("remote MCP result is missing")
	}
	return envelope.Result, header, nil
}

func readRemoteMCPEvent(body io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(io.LimitReader(body, remoteMCPResponseLimit+1))
	scanner.Buffer(make([]byte, 4096), remoteMCPResponseLimit)
	var data []string
	total := 0
	for scanner.Scan() {
		line := scanner.Text()
		total += len(line) + 1
		if total > remoteMCPResponseLimit {
			return nil, errors.New("remote MCP stream exceeds size limit")
		}
		if line == "" && len(data) > 0 {
			return []byte(strings.Join(data, "\n")), nil
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(value, " "))
		} else if line != "" && !strings.HasPrefix(line, ":") && line != "event: message" && line != "event:message" {
			return nil, errors.New("unsupported remote MCP stream event")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.New("invalid remote MCP stream")
	}
	if len(data) > 0 {
		return []byte(strings.Join(data, "\n")), nil
	}
	return nil, errors.New("remote MCP stream ended without a response")
}

// validateRemoteMCPJSON rejects duplicate members and excessive nesting before
// any projection or schema comparison can erase that ambiguity.
func validateRemoteMCPJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errors.New("remote MCP JSON nesting exceeds limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return errors.New("invalid remote MCP JSON")
		}
		delim, container := token.(json.Delim)
		if !container {
			return nil
		}
		seen := map[string]bool{}
		for decoder.More() {
			if delim == '{' {
				key, err := decoder.Token()
				if err != nil {
					return errors.New("invalid remote MCP JSON member")
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate remote MCP JSON member")
				}
				seen[name] = true
			}
			if err := value(depth + 1); err != nil {
				return err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return errors.New("invalid remote MCP JSON container")
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("invalid trailing remote MCP JSON")
	}
	return nil
}

type canonicalRemoteNumber string

// Normalize decimal notation without binary64 or allocating an exponent-sized
// integer. This compares 1, 1.0 and 1e0 equally while distinguishing large values.
func canonicalRemoteMCPNumbers(value any) any {
	switch v := value.(type) {
	case json.Number:
		number := strings.ToLower(string(v))
		mantissa, exponent, _ := strings.Cut(number, "e")
		power := new(big.Int)
		if exponent != "" {
			power.SetString(exponent, 10)
		}
		sign := ""
		if strings.HasPrefix(mantissa, "-") {
			sign = "-"
			mantissa = mantissa[1:]
		}
		integer, fraction, _ := strings.Cut(mantissa, ".")
		digits := strings.TrimLeft(integer+fraction, "0")
		if digits == "" {
			return canonicalRemoteNumber("0")
		}
		trimmed := strings.TrimRight(digits, "0")
		adjustment := big.NewInt(int64(len(digits) - len(trimmed) - len(fraction)))
		power.Add(power, adjustment)
		return canonicalRemoteNumber(sign + trimmed + "e" + power.String())
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = canonicalRemoteMCPNumbers(v[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = canonicalRemoteMCPNumbers(item)
		}
		return out
	}
	return value
}

func decodeRemoteMCPJSON(raw []byte, target any) error {
	if err := validateRemoteMCPJSON(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid remote MCP JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("invalid trailing remote MCP JSON")
	}
	return nil
}

func decodeRemoteMCPObject(raw []byte, target any) error {
	var fields map[string]json.RawMessage
	if err := decodeRemoteMCPJSON(raw, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("remote MCP object is required")
	}
	// encoding/json matches struct fields case-insensitively; MCP names are exact.
	typ := reflect.TypeOf(target).Elem()
	allowed := make(map[string]bool, typ.NumField())
	for field := range typ.Fields() {
		allowed[strings.Split(field.Tag.Get("json"), ",")[0]] = true
	}
	for name := range fields {
		if !allowed[name] {
			return errors.New("unsupported remote MCP field")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("unsupported or invalid remote MCP fields")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("invalid trailing remote MCP JSON")
	}
	return nil
}

func projectRemoteMCPResult(raw json.RawMessage, secrets ...string) (string, error) {
	var result struct {
		Content           []json.RawMessage `json:"content"`
		StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
		IsError           bool              `json:"isError,omitempty"`
	}
	if err := decodeRemoteMCPObject(raw, &result); err != nil {
		return "", err
	}
	if result.Content == nil {
		return "", errors.New("remote MCP result requires content")
	}
	content := make([]map[string]string, 0, len(result.Content))
	for _, item := range result.Content {
		var text struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		}
		if err := decodeRemoteMCPObject(item, &text); err != nil {
			return "", err
		}
		if text.Type != remoteMCPTextContent || text.Text == nil {
			return "", errors.New("unsupported remote MCP content")
		}
		content = append(content, map[string]string{"type": remoteMCPTextContent, remoteMCPTextContent: redactToolSensitiveText(*text.Text, secrets...)})
	}
	if result.IsError {
		return "", ToolExecutionError{Err: errors.New("remote MCP tool reported an error")}
	}
	var structured any
	if len(result.StructuredContent) > 0 {
		if err := decodeRemoteMCPJSON(result.StructuredContent, &structured); err != nil {
			return "", err
		}
		if _, ok := structured.(map[string]any); !ok {
			return "", errors.New("remote MCP structuredContent must be an object")
		}
		structured = redactRemoteMCPStrings(structured, secrets)
	}
	output := map[string]any{"content": content}
	if structured != nil {
		output["structuredContent"] = structured
	}
	projected, err := json.Marshal(output)
	if err != nil || len(projected) > remoteMCPResponseLimit {
		return "", errors.New("remote MCP projected result exceeds limit")
	}
	return string(projected), nil
}

func redactRemoteMCPStrings(value any, secrets []string) any {
	switch v := value.(type) {
	case string:
		return redactToolSensitiveText(v, secrets...)
	case []any:
		for i := range v {
			v[i] = redactRemoteMCPStrings(v[i], secrets)
		}
	case map[string]any:
		clean := make(map[string]any, len(v))
		for key, item := range v {
			clean[redactToolSensitiveText(key, secrets...)] = redactRemoteMCPStrings(item, secrets)
		}
		return clean
	}
	return value
}
