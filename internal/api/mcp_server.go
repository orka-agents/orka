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

const (
	mcpEndpointPath   = "/mcp"
	mcpServerName     = "orka"
	mcpJSONRPCVersion = "2.0"

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
	mcpMethodToolsCall        = "tools/call"

	DefaultMCPMaxConcurrent = 16
	mcpMaxRequestBytes      = 1 << 20
	mcpRequestReadTimeout   = 30 * time.Second
)

var mcpCORSHeaders = []string{mcpProtocolVersionHeader, mcpMethodHeader, mcpNameHeader}

func isMCPEndpointPath(path string) bool {
	return strings.EqualFold(strings.TrimRight(path, "/"), mcpEndpointPath)
}

const (
	mcpCodeParseError          = -32700
	mcpCodeInvalidRequest      = -32600
	mcpCodeMethodNotFound      = -32601
	mcpCodeInvalidParams       = -32602
	mcpCodeInternalError       = -32603
	mcpCodeHeaderMismatch      = -32020
	mcpCodeMissingCapabilities = -32021
	mcpCodeUnsupportedVersion  = -32022
	mcpCodeServerBusy          = -32000
	mcpCodeForbidden           = -32001
)

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

type MCPServerConfig struct {
	Enabled       bool
	MaxConcurrent int
	ResourceURL   string
}

type mcpServer struct {
	handler         fasthttp.RequestHandler
	slots           chan struct{}
	waitSlots       chan struct{}
	allowedOrigins  []string
	replayHeaders   []string
	resultWait      time.Duration
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

type mcpCall struct {
	c              fiber.Ctx
	method         string
	params         json.RawMessage
	sessionless    bool
	metaVersion    string
	tasksExtension bool
	permit         *mcpPermit
}

type mcpPermit struct {
	server  *mcpServer
	waiting bool
}

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

func (m *mcpServer) Handle(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	if !m.originAllowed(c.Get(fiber.HeaderOrigin)) {
		return m.writeError(c, fiber.StatusForbidden, nil, newMCPError(mcpCodeInvalidRequest, "origin not allowed"))
	}
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

func readMCPRawBody(c fiber.Ctx) bool {
	return readBoundedRawBody(c, mcpMaxRequestBytes)
}

func readBoundedRawBody(c fiber.Ctx, limit int) bool {
	request := c.Request()
	stream := request.BodyStream()
	if stream == nil {
		return len(request.Body()) <= limit
	}
	data, err := io.ReadAll(io.LimitReader(stream, int64(limit)+1))
	if err != nil || len(data) > limit {
		return false
	}
	request.SetBodyRaw(data)
	return true
}

const mcpMaxContentCodings = 3

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

var mcpNamedMethods = []string{mcpMethodToolsCall, "resources/read", "prompts/get"}

var mcpTaskNamedMethods = []string{"tasks/get", "tasks/update", "tasks/cancel"}

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
		return params.TaskID, call.tasksExtension
	}
	bodyName := params.Name
	if bodyName == nil {
		bodyName = params.URI
	}
	return bodyName, slices.Contains(mcpNamedMethods, call.method)
}

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
	case mcpMethodToolsCall:
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
