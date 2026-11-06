/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

//nolint:goconst // Keep MCP wire field names and JSON Schema keywords literal to match the protocol.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"time"

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

var mcpSupportedProtocolVersions = []string{
	mcpProtocolVersionSessionless,
	"2025-11-25",
	"2025-06-18",
	"2025-03-26",
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
	// resultWait bounds how long tasks/result waits for a Task to finish.
	resultWait time.Duration
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
	// tasksExtension is true when the request's client capabilities declare
	// the MCP Tasks extension.
	tasksExtension bool
}

func newMCPServer(handler fasthttp.RequestHandler, cfg MCPServerConfig) *mcpServer {
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultMCPMaxConcurrent
	}
	return &mcpServer{
		handler:        handler,
		slots:          make(chan struct{}, maxConcurrent),
		waitSlots:      make(chan struct{}, max(1, maxConcurrent/2)),
		allowedOrigins: mcpAllowedOrigins(os.Getenv("ORKA_CORS_ALLOWED_ORIGINS")),
		resultWait:     mcpMaxWaitSeconds * time.Second,
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
	// Read the whole body before taking a permit, so a stalled upload cannot
	// hold one. The MCP request config bounds the read in size and time.
	if c.Request().Header.ContentLength() > mcpMaxRequestBytes {
		c.RequestCtx().SetConnectionClose()
		return m.writeError(c, fiber.StatusRequestEntityTooLarge, nil, newMCPError(mcpCodeInvalidRequest, "MCP request is too large"))
	}
	body := c.Body()
	if len(body) > mcpMaxRequestBytes {
		return m.writeError(c, fiber.StatusRequestEntityTooLarge, nil, newMCPError(mcpCodeInvalidRequest, "MCP request is too large"))
	}
	if !tryAcquire(m.slots) {
		c.Set(fiber.HeaderRetryAfter, "1")
		return m.writeError(c, fiber.StatusTooManyRequests, nil, newMCPError(mcpCodeServerBusy, "too many concurrent MCP requests"))
	}
	defer func() { <-m.slots }()

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

	call := &mcpCall{c: c, method: request.Method, params: request.Params}
	if err := call.readMeta(); err != nil {
		return m.writeError(c, fiber.StatusOK, request.ID, err)
	}
	result, err := m.dispatch(call)
	if err != nil {
		var rpcErr *mcpError
		if !errors.As(err, &rpcErr) {
			log.Error(err, "MCP request failed", "method", request.Method)
			rpcErr = newMCPError(mcpCodeInternalError, "internal error")
		}
		return m.writeError(c, fiber.StatusOK, request.ID, rpcErr)
	}
	if call.sessionless {
		result = withResultType(result, "complete")
	}
	return m.writeResult(c, request.ID, result)
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
		if version >= mcpProtocolVersionSessionless {
			if !slices.Contains(mcpSupportedProtocolVersions, version) {
				return &mcpError{Code: mcpCodeUnsupportedVersion, Message: "unsupported protocol version", Data: map[string]any{
					"supported": mcpSupportedProtocolVersions, "requested": version,
				}}
			}
			call.sessionless = true
		}
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
