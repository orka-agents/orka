/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

//nolint:goconst // Keep MCP wire field names and JSON Schema keywords literal to match the protocol.
package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
)

// The control-plane MCP server lets IDE agents delegate Tasks to Orka Agents.
// It is a stateless Streamable HTTP endpoint that answers every request with
// a single JSON body: it keeps no MCP session, sends no server-initiated
// requests, and opens no SSE stream. Every tool call is executed by replaying
// the equivalent public REST request in process with the caller's own
// credentials, so authentication, route policies, SubjectAccessReviews,
// context-token scopes, namespace isolation, and handler checks are exactly
// those of the REST route. See website/docs/guides/mcp-server.md.

const (
	mcpEndpointPath   = "/mcp"
	mcpServerName     = "orka"
	mcpJSONRPCVersion = "2.0"

	// mcpProtocolVersionSessionless is the first protocol revision whose
	// requests carry their own version and client capabilities in _meta.
	mcpProtocolVersionSessionless = "2026-07-28"
	mcpLatestLegacyVersion        = "2025-11-25"

	mcpMetaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	mcpMetaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	mcpMetaServerInfo         = "io.modelcontextprotocol/serverInfo"
	mcpMetaRelatedTask        = "io.modelcontextprotocol/related-task"
	mcpTasksExtension         = "io.modelcontextprotocol/tasks"
	mcpMethodHeader           = "Mcp-Method"
	mcpProtocolVersionHeader  = "Mcp-Protocol-Version"
	mcpNameHeader             = "Mcp-Name"

	// DefaultMCPMaxConcurrent bounds concurrent MCP requests per API replica,
	// like --chat-max-concurrent bounds chat requests.
	DefaultMCPMaxConcurrent = 16
	// mcpMaxRequestBytes bounds one JSON-RPC message. Prompts are bounded
	// separately; nothing an MCP client sends legitimately approaches this.
	mcpMaxRequestBytes = 1 << 20
	// mcpRequestReadTimeout bounds reading one MCP request, body included.
	mcpRequestReadTimeout = 30 * time.Second
)

// mcpCORSHeaders are the Streamable HTTP request headers a browser client
// sends; the CORS allow-list carries them when the endpoint is enabled.
var mcpCORSHeaders = []string{mcpProtocolVersionHeader, mcpMethodHeader, mcpNameHeader}

// isMCPEndpointPath matches the MCP route as Fiber routes it: case-insensitive
// and with an optional trailing slash.
func isMCPEndpointPath(path string) bool {
	return strings.EqualFold(strings.TrimRight(path, "/"), mcpEndpointPath)
}

// JSON-RPC and MCP error codes.
const (
	mcpCodeParseError          = -32700
	mcpCodeInvalidRequest      = -32600
	mcpCodeMethodNotFound      = -32601
	mcpCodeInvalidParams       = -32602
	mcpCodeInternalError       = -32603
	mcpCodeHeaderMismatch      = -32020
	mcpCodeMissingCapabilities = -32021
	mcpCodeUnsupportedVersion  = -32022
	// mcpCodeServerBusy is in the implementation-defined server error range.
	mcpCodeServerBusy = -32000
	// mcpCodeForbidden reports an authorization or authentication refusal of
	// the replayed REST request.
	mcpCodeForbidden = -32001
)

// mcpSupportedProtocolVersions lists only revisions this server fully
// implements. 2025-03-26 is absent: it requires receiving JSON-RPC batches,
// which every later revision removed and this server refuses.
var mcpSupportedProtocolVersions = []string{
	mcpProtocolVersionSessionless,
	"2025-11-25",
	"2025-06-18",
}

const mcpServerInstructions = "Orka runs Tasks on Kubernetes-hosted Agents. Use list_agents to find an Agent, " +
	"then create_task with its name and a prompt. Track progress with get_task or wait_for_task. " +
	"When a Task is waiting for a human decision, list_pending_approvals shows the request; call " +
	"decide_approval only after the human user has explicitly approved or declined it. " +
	"Fetch output with get_task_result and list_task_artifacts. Every tool runs with the caller's own " +
	"Orka permissions, exactly like the REST API."

// MCPServerConfig configures the control-plane MCP endpoint.
type MCPServerConfig struct {
	// Enabled serves POST /mcp on the API port.
	Enabled bool
	// MaxConcurrent bounds concurrent MCP requests. Zero uses
	// DefaultMCPMaxConcurrent.
	MaxConcurrent int
}

type mcpServer struct {
	handler        fasthttp.RequestHandler
	slots          chan struct{}
	waitSlots      chan struct{}
	allowedOrigins []string
	// replayHeaders is the caller-header allow-list of REST replays.
	replayHeaders []string
	// resultWait bounds how long tasks/result waits for a Task to finish.
	resultWait time.Duration
	// createdTaskWait bounds how long a task handle is withheld while the
	// Task it names is not yet readable.
	createdTaskWait time.Duration
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *mcpError) Error() string { return e.Message }

func newMCPError(code int, message string) *mcpError {
	return &mcpError{Code: code, Message: message}
}

// mcpCall is one JSON-RPC request with the request-scoped protocol facts.
type mcpCall struct {
	c      fiber.Ctx
	method string
	params json.RawMessage
	// sessionless is true when the request carries the 2026-07-28 _meta.
	sessionless bool
	// metaVersion is the _meta protocol version, when the request has one.
	metaVersion string
	// tasksExtension is true when the request's client capabilities declare
	// the MCP Tasks extension.
	tasksExtension bool
	// permit is the request's admission.
	permit *mcpPermit
}

// mcpPermit is the admission one request holds until it ends: a request
// permit, or, once the request starts a long poll, a wait permit in its
// place. Waits therefore never occupy request permits, so short requests,
// including the decisions and cancellations that end a wait, stay admitted
// while every wait permit is in use. Waits beyond the wait permits are
// refused, never queued.
type mcpPermit struct {
	server  *mcpServer
	waiting bool
}

// enterWait trades the request permit for a wait permit. It reports false,
// keeping the request permit, when every wait permit is in use.
func (p *mcpPermit) enterWait() bool {
	if p.waiting {
		return true
	}
	if !tryAcquire(p.server.waitSlots) {
		return false
	}
	p.waiting = true
	<-p.server.slots
	return true
}

func (p *mcpPermit) release() {
	if p.waiting {
		<-p.server.waitSlots
		return
	}
	<-p.server.slots
}

func newMCPServer(handler fasthttp.RequestHandler, cfg MCPServerConfig) *mcpServer {
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultMCPMaxConcurrent
	}
	return &mcpServer{
		handler:         handler,
		slots:           make(chan struct{}, maxConcurrent),
		waitSlots:       make(chan struct{}, max(1, maxConcurrent/2)),
		allowedOrigins:  mcpAllowedOrigins(os.Getenv("ORKA_CORS_ALLOWED_ORIGINS")),
		replayHeaders:   mcpReplayHeaders(ContextTokenConfig{}),
		resultWait:      mcpMaxWaitSeconds * time.Second,
		createdTaskWait: mcpCreatedTaskWait,
	}
}

func mcpAllowedOrigins(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{"*"}
	}
	origins := strings.Split(raw, ",")
	for i := range origins {
		origins[i] = strings.TrimSpace(origins[i])
	}
	return origins
}

// originAllowed implements the Streamable HTTP DNS-rebinding defense with the
// same allow-list as the API's CORS policy. Requests without Origin are not
// browser requests.
func (m *mcpServer) originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	return slices.ContainsFunc(m.allowedOrigins, func(allowed string) bool {
		return allowed == "*" || strings.EqualFold(allowed, origin)
	})
}

func tryAcquire(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// Handle serves POST /mcp. The external auth middleware and the identity-only
// route policy have already authenticated the caller.
func (m *mcpServer) Handle(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	if !m.originAllowed(c.Get(fiber.HeaderOrigin)) {
		return m.writeError(c, fiber.StatusForbidden, nil, newMCPError(mcpCodeInvalidRequest, "origin not allowed"))
	}
	// Read the whole raw body before taking a permit, so a stalled upload
	// cannot hold one. The MCP request config bounds the read in size and
	// time. Decoding a Content-Encoding waits for the permit.
	if c.Request().Header.ContentLength() > mcpMaxRequestBytes {
		c.RequestCtx().SetConnectionClose()
		return m.writeError(c, fiber.StatusRequestEntityTooLarge, nil, newMCPError(mcpCodeInvalidRequest, "MCP request is too large"))
	}
	if !readMCPRawBody(c) {
		c.RequestCtx().SetConnectionClose()
		return m.writeError(c, fiber.StatusRequestEntityTooLarge, nil, newMCPError(mcpCodeInvalidRequest, "MCP request is too large"))
	}
	if !tryAcquire(m.slots) {
		c.Set(fiber.HeaderRetryAfter, "1")
		return m.writeError(c, fiber.StatusTooManyRequests, nil, newMCPError(mcpCodeServerBusy, "too many concurrent MCP requests"))
	}
	permit := &mcpPermit{server: m}
	defer permit.release()

	body, refused := decodeMCPBody(c)
	switch refused {
	case 0:
	case fiber.StatusRequestEntityTooLarge:
		return m.writeError(c, refused, nil, newMCPError(mcpCodeInvalidRequest, "MCP request is too large"))
	case fiber.StatusUnsupportedMediaType:
		return m.writeError(c, refused, nil, newMCPError(mcpCodeInvalidRequest, "unsupported Content-Encoding"))
	default:
		return m.writeError(c, refused, nil, newMCPError(mcpCodeParseError, "request body does not match its Content-Encoding"))
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return m.writeError(c, fiber.StatusBadRequest, nil, newMCPError(mcpCodeInvalidRequest, "JSON-RPC batches are not supported"))
	}
	request, rpcErr := decodeMCPRequest(trimmed)
	if rpcErr != nil {
		return m.writeError(c, fiber.StatusBadRequest, request.ID, rpcErr)
	}
	if header := strings.TrimSpace(c.Get(mcpMethodHeader)); header != "" && header != request.Method {
		return m.writeError(c, fiber.StatusBadRequest, request.ID, newMCPError(mcpCodeHeaderMismatch, "Mcp-Method header does not match the request method"))
	}
	// Notifications and client responses need no answer. This server sends
	// no requests, so a response can only be stray.
	if len(request.ID) == 0 || string(request.ID) == responsesJSONNull || request.Method == "" {
		return c.Status(fiber.StatusAccepted).Send(nil)
	}

	call := &mcpCall{c: c, method: request.Method, params: request.Params, permit: permit}
	if err := call.readMeta(); err != nil {
		return m.writeError(c, call.errorStatus(err), request.ID, err)
	}
	if err := call.validateHeaders(); err != nil {
		return m.writeError(c, call.errorStatus(err), request.ID, err)
	}
	result, err := m.dispatch(call)
	if err != nil {
		var rpcErr *mcpError
		if !errors.As(err, &rpcErr) {
			log.Error(err, "MCP request failed", "method", request.Method)
			rpcErr = newMCPError(mcpCodeInternalError, "internal error")
		}
		return m.writeError(c, call.errorStatus(rpcErr), request.ID, rpcErr)
	}
	if call.sessionless {
		result = withResultType(result, "complete")
	}
	return m.writeResult(c, request.ID, result)
}

// readMCPRawBody buffers the raw request body, reading at most one byte past
// mcpMaxRequestBytes: fasthttp does not bound a streamed chunked body. It
// reports false when the body is too large or cannot be read.
func readMCPRawBody(c fiber.Ctx) bool {
	request := c.Request()
	stream := request.BodyStream()
	if stream == nil {
		return len(request.Body()) <= mcpMaxRequestBytes
	}
	data, err := io.ReadAll(io.LimitReader(stream, mcpMaxRequestBytes+1))
	if err != nil || len(data) > mcpMaxRequestBytes {
		return false
	}
	// Replace the consumed stream, so decoding reads the buffered bytes.
	request.SetBodyRaw(data)
	return true
}

// mcpMaxContentCodings bounds how many codings one request body may stack.
const mcpMaxContentCodings = 3

// decodeMCPBody returns the buffered request body with its Content-Encoding
// removed, or the HTTP status to refuse the request with. Each coding is
// decoded through a limit of mcpMaxRequestBytes, so a small compressed body
// cannot expand past the size a plain one is held to. Fiber's Body() bounds
// the expansion only by the API-wide body limit, which is far larger.
func decodeMCPBody(c fiber.Ctx) ([]byte, int) {
	request := c.Request()
	body := request.Body()
	var codings []string
	for _, line := range request.Header.PeekAll(fiber.HeaderContentEncoding) {
		for coding := range strings.SplitSeq(string(line), ",") {
			if coding = strings.ToLower(strings.TrimSpace(coding)); coding != "" && coding != fiber.StrIdentity {
				codings = append(codings, coding)
			}
		}
	}
	if len(codings) > mcpMaxContentCodings {
		return nil, fiber.StatusUnsupportedMediaType
	}
	// Codings are listed in the order they were applied.
	for _, coding := range slices.Backward(codings) {
		var encoded fasthttp.Request
		encoded.SetBodyRaw(body)
		var err error
		switch coding {
		case fiber.StrGzip, "x-gzip":
			body, err = encoded.BodyGunzipWithLimit(mcpMaxRequestBytes)
		case fiber.StrBr, fiber.StrBrotli:
			body, err = encoded.BodyUnbrotliWithLimit(mcpMaxRequestBytes)
		case fiber.StrDeflate:
			body, err = encoded.BodyInflateWithLimit(mcpMaxRequestBytes)
		case fiber.StrZstd:
			body, err = encoded.BodyUnzstdWithLimit(mcpMaxRequestBytes)
		default:
			return nil, fiber.StatusUnsupportedMediaType
		}
		switch {
		case errors.Is(err, fasthttp.ErrBodyTooLarge):
			return nil, fiber.StatusRequestEntityTooLarge
		case err != nil:
			return nil, fiber.StatusBadRequest
		}
	}
	return body, 0
}

func decodeMCPRequest(body []byte) (mcpRequest, *mcpError) {
	var request mcpRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&request); err != nil {
		return mcpRequest{}, newMCPError(mcpCodeParseError, "invalid JSON-RPC message")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return mcpRequest{}, newMCPError(mcpCodeParseError, "invalid JSON-RPC message")
	}
	if request.JSONRPC != mcpJSONRPCVersion {
		return request, newMCPError(mcpCodeInvalidRequest, "jsonrpc must be 2.0")
	}
	if len(request.ID) > 0 {
		switch request.ID[0] {
		case '"', 'n', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		default:
			return mcpRequest{}, newMCPError(mcpCodeInvalidRequest, "id must be a string or number")
		}
		if len(request.ID) > 256 {
			return mcpRequest{}, newMCPError(mcpCodeInvalidRequest, "id is too long")
		}
	}
	return request, nil
}

// readMeta reads the request-scoped protocol version and client capabilities
// that sessionless (2026-07-28) clients send with every request. Older
// clients negotiate in initialize, which this stateless server does not
// remember, so their requests use the legacy shapes.
func (call *mcpCall) readMeta() *mcpError {
	if len(call.params) == 0 {
		return nil
	}
	var params struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(call.params, &params); err != nil {
		return newMCPError(mcpCodeInvalidParams, "params must be an object")
	}
	if raw, ok := params.Meta[mcpMetaProtocolVersion]; ok {
		var version string
		if err := json.Unmarshal(raw, &version); err != nil {
			return newMCPError(mcpCodeInvalidParams, "invalid _meta protocol version")
		}
		if !slices.Contains(mcpSupportedProtocolVersions, version) {
			return mcpUnsupportedVersionError(version)
		}
		call.metaVersion = version
		call.sessionless = version >= mcpProtocolVersionSessionless
	}
	if raw, ok := params.Meta[mcpMetaClientCapabilities]; ok {
		var capabilities struct {
			Extensions map[string]json.RawMessage `json:"extensions"`
		}
		if err := json.Unmarshal(raw, &capabilities); err != nil {
			return newMCPError(mcpCodeInvalidParams, "invalid _meta client capabilities")
		}
		_, call.tasksExtension = capabilities.Extensions[mcpTasksExtension]
	} else if call.sessionless {
		return newMCPError(mcpCodeInvalidParams, "missing _meta client capabilities")
	}
	return nil
}

func mcpUnsupportedVersionError(version string) *mcpError {
	return &mcpError{Code: mcpCodeUnsupportedVersion, Message: "unsupported protocol version", Data: map[string]any{
		"supported": mcpSupportedProtocolVersions, "requested": version,
	}}
}

func mcpHeaderMismatch(message string) *mcpError {
	return newMCPError(mcpCodeHeaderMismatch, "header mismatch: "+message)
}

// errorStatus is the HTTP status of a JSON-RPC error. Header, version, and
// capability failures are 400, and a sessionless request for a method this
// server does not implement is 404, as the 2026-07-28 transport requires.
// Other errors keep 200, as the earlier revisions answer them.
func (call *mcpCall) errorStatus(rpcErr *mcpError) int {
	switch rpcErr.Code {
	case mcpCodeHeaderMismatch, mcpCodeUnsupportedVersion:
		return fiber.StatusBadRequest
	case mcpCodeMissingCapabilities:
		if call.sessionless {
			return fiber.StatusBadRequest
		}
	case mcpCodeMethodNotFound:
		if call.sessionless {
			return fiber.StatusNotFound
		}
	}
	return fiber.StatusOK
}

// mcpNamedMethods carry the Mcp-Name header, mirroring params.name or
// params.uri.
var mcpNamedMethods = []string{"tools/call", "resources/read", "prompts/get"}

// mcpTaskNamedMethods are the io.modelcontextprotocol/tasks methods whose
// Mcp-Name header mirrors params.taskId, so an intermediary can route every
// request for one task to the same place.
var mcpTaskNamedMethods = []string{"tasks/get", "tasks/update", "tasks/cancel"}

// validateHeaders enforces the Streamable HTTP request metadata rules: every
// mirrored header must match the JSON-RPC body, and a sessionless request
// must carry MCP-Protocol-Version, Mcp-Method, and, where it applies,
// Mcp-Name. The server acts only on the body; the headers are validated so an
// intermediary routing on them cannot be shown different values.
func (call *mcpCall) validateHeaders() *mcpError {
	request := &call.c.Request().Header
	value := func(name string) (string, bool, *mcpError) {
		raw := request.Peek(name)
		if len(raw) == 0 {
			return "", false, nil
		}
		for _, b := range raw {
			if (b < 0x20 || b > 0x7e) && b != '\t' {
				return "", true, mcpHeaderMismatch(name + " header contains invalid characters")
			}
		}
		return string(raw), true, nil
	}

	version, hasVersion, err := value(mcpProtocolVersionHeader)
	if err != nil {
		return err
	}
	switch {
	case call.sessionless && !hasVersion:
		return mcpHeaderMismatch(mcpProtocolVersionHeader + " header is required")
	case hasVersion && call.metaVersion != "" && version != call.metaVersion:
		return mcpHeaderMismatch(mcpProtocolVersionHeader + " header does not match the request _meta protocol version")
	case hasVersion && !slices.Contains(mcpSupportedProtocolVersions, version):
		return mcpUnsupportedVersionError(version)
	case hasVersion && !call.sessionless && version >= mcpProtocolVersionSessionless:
		return mcpHeaderMismatch(mcpProtocolVersionHeader + " header names a protocol version the request _meta does not carry")
	}

	if _, hasMethod, err := value(mcpMethodHeader); err != nil {
		return err
	} else if call.sessionless && !hasMethod {
		return mcpHeaderMismatch(mcpMethodHeader + " header is required")
	}

	name, hasName, err := value(mcpNameHeader)
	if err != nil {
		return err
	}
	bodyName, required := call.mirroredName()
	if !hasName {
		if call.sessionless && bodyName != nil && required {
			return mcpHeaderMismatch(mcpNameHeader + " header is required")
		}
		return nil
	}
	decoded, ok := decodeMCPHeaderValue(name)
	if !ok {
		return mcpHeaderMismatch(mcpNameHeader + " header has an invalid Base64 value")
	}
	if bodyName == nil || decoded != *bodyName {
		return mcpHeaderMismatch(mcpNameHeader + " header does not match the request body")
	}
	return nil
}

// mirroredName is the body value the Mcp-Name header mirrors, nil when the
// body carries none, and whether the method requires the header.
func (call *mcpCall) mirroredName() (*string, bool) {
	var params struct {
		Name   *string `json:"name"`
		URI    *string `json:"uri"`
		TaskID *string `json:"taskId"`
	}
	if len(call.params) > 0 {
		_ = json.Unmarshal(call.params, &params)
	}
	if slices.Contains(mcpTaskNamedMethods, call.method) {
		// The Tasks extension is what requires the header on these methods;
		// a request that does not declare it is answered as before.
		return params.TaskID, call.tasksExtension
	}
	bodyName := params.Name
	if bodyName == nil {
		bodyName = params.URI
	}
	return bodyName, slices.Contains(mcpNamedMethods, call.method)
}

// decodeMCPHeaderValue decodes the "=?base64?...?=" sentinel encoding of a
// mirrored header value; any other value is literal.
func decodeMCPHeaderValue(value string) (string, bool) {
	encoded, ok := strings.CutPrefix(value, "=?base64?")
	if !ok {
		return value, true
	}
	encoded, ok = strings.CutSuffix(encoded, "?=")
	if !ok {
		return value, true
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !utf8.Valid(decoded) {
		return "", false
	}
	return string(decoded), true
}

func (m *mcpServer) dispatch(call *mcpCall) (any, error) {
	switch call.method {
	case "initialize":
		if call.sessionless {
			return nil, newMCPError(mcpCodeMethodNotFound, "initialize is not used by sessionless protocol versions")
		}
		return m.initialize(call.params)
	case "server/discover":
		if !call.sessionless {
			return nil, newMCPError(mcpCodeMethodNotFound, "server/discover requires protocol version "+mcpProtocolVersionSessionless)
		}
		return map[string]any{
			"supportedVersions": mcpSupportedProtocolVersions,
			"capabilities":      mcpServerCapabilities(),
			"instructions":      mcpServerInstructions,
			"_meta":             map[string]any{mcpMetaServerInfo: mcpServerInfo()},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{mcpFieldTools: mcpToolDefinitions()}, nil
	case "tools/call":
		return m.callTool(call)
	case "tasks/get":
		return m.getTask(call)
	case "tasks/result":
		return m.taskResult(call)
	case "tasks/update":
		return m.updateTask(call)
	case "tasks/cancel":
		return m.cancelTask(call)
	default:
		return nil, newMCPError(mcpCodeMethodNotFound, "method not supported")
	}
}

func mcpServerInfo() map[string]any {
	return map[string]any{"name": mcpServerName, "title": "Orka control plane", "version": "v1"}
}

func mcpServerCapabilities() map[string]any {
	return map[string]any{
		mcpFieldTools: map[string]any{"listChanged": false},
		// 2025-11-25 task-augmented tools/call.
		"tasks": map[string]any{
			"cancel":   map[string]any{},
			"requests": map[string]any{mcpFieldTools: map[string]any{"call": map[string]any{}}},
		},
		"extensions": map[string]any{mcpTasksExtension: map[string]any{}},
	}
}

func (m *mcpServer) initialize(raw json.RawMessage) (any, error) {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, newMCPError(mcpCodeInvalidParams, "invalid initialize params")
		}
	}
	version := mcpLatestLegacyVersion
	if params.ProtocolVersion != "" && params.ProtocolVersion < mcpProtocolVersionSessionless &&
		slices.Contains(mcpSupportedProtocolVersions, params.ProtocolVersion) {
		version = params.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    mcpServerCapabilities(),
		"serverInfo":      mcpServerInfo(),
		"instructions":    mcpServerInstructions,
	}, nil
}

// withResultType adds the sessionless resultType discriminator to a result
// object that does not already carry one.
func withResultType(result any, resultType string) any {
	data, err := json.Marshal(result)
	if err != nil {
		return result
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return result
	}
	if _, ok := object["resultType"]; !ok {
		object["resultType"] = json.RawMessage(`"` + resultType + `"`)
	}
	return object
}

func (m *mcpServer) writeResult(c fiber.Ctx, id json.RawMessage, result any) error {
	return c.Status(fiber.StatusOK).JSON(mcpResponse{JSONRPC: mcpJSONRPCVersion, ID: id, Result: result})
}

func (m *mcpServer) writeError(c fiber.Ctx, status int, id json.RawMessage, rpcErr *mcpError) error {
	if len(id) == 0 {
		id = json.RawMessage(responsesJSONNull)
	}
	return c.Status(status).JSON(mcpResponse{JSONRPC: mcpJSONRPCVersion, ID: id, Error: rpcErr})
}
