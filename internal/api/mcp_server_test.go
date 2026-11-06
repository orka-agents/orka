package api

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

type mcpTestResponse struct {
	status int
	header http.Header
	body   string
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *mcpError       `json:"error"`
}

type mcpTestToolResult struct {
	Content           []mcpContent   `json:"content"`
	StructuredContent map[string]any `json:"structuredContent"`
	IsError           bool           `json:"isError"`
}

// newMCPFixture is the external-authorization fixture with a Kubernetes
// client that assigns UIDs on create, as the API server does.
func newMCPFixture(t *testing.T) *externalAuthorizationFixture {
	t.Helper()
	f := newExternalAuthorizationFixture(t)
	base, ok := f.kube.(client.WithWatch)
	require.True(t, ok)
	f.kube = interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, task := obj.(*corev1alpha1.Task); task && obj.GetUID() == "" {
				obj.SetUID(types.UID("uid-" + obj.GetName()))
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	f.config.APIReader = f.kube
	f.server = NewServer(f.kube, nil, f.config)
	return f
}

func (f *externalAuthorizationFixture) allowAll() {
	f.reviews = nil
	f.review = func(review *authorizationv1.SubjectAccessReview) error {
		review.Status.Allowed = true
		return nil
	}
}

func (f *externalAuthorizationFixture) mcpRaw(t *testing.T, method, body string, headers map[string]string) mcpTestResponse {
	t.Helper()
	f.sequence++
	request := httptest.NewRequest(method, mcpEndpointPath, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", fmt.Sprintf("Bearer fixture-%s-%d", t.Name(), f.sequence))
	for name, value := range headers {
		if value == "" {
			request.Header.Del(name)
			continue
		}
		request.Header.Set(name, value)
	}
	response, err := f.server.app.Test(request, fiber.TestConfig{Timeout: 2 * time.Minute, FailOnTimeout: true})
	require.NoError(t, err)
	defer response.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	result := mcpTestResponse{status: response.StatusCode, header: response.Header, body: string(data)}
	if len(data) > 0 && data[0] == '{' {
		require.NoError(t, json.Unmarshal(data, &result), string(data))
	}
	return result
}

func mcpBody(t *testing.T, method string, params any) string {
	t.Helper()
	message := map[string]any{"jsonrpc": "2.0", "id": 7, "method": method}
	if params != nil {
		message["params"] = params
	}
	data, err := json.Marshal(message)
	require.NoError(t, err)
	return string(data)
}

// mcpRPC sends a request with the Streamable HTTP headers a conforming
// client mirrors from the body: Mcp-Method always, MCP-Protocol-Version for a
// _meta protocol version, and Mcp-Name for a tools/call name.
func (f *externalAuthorizationFixture) mcpRPC(t *testing.T, method string, params any) mcpTestResponse {
	t.Helper()
	return f.mcpRaw(t, http.MethodPost, mcpBody(t, method, params), mcpMirroredHeaders(method, params))
}

func mcpMirroredHeaders(method string, params any) map[string]string {
	headers := map[string]string{mcpMethodHeader: method}
	values, _ := params.(map[string]any)
	if meta, ok := values["_meta"].(map[string]any); ok {
		if version, ok := meta[mcpMetaProtocolVersion].(string); ok {
			headers[mcpProtocolVersionHeader] = version
		}
	}
	if name, ok := values["name"].(string); ok && method == "tools/call" {
		headers[mcpNameHeader] = name
	}
	return headers
}

func (f *externalAuthorizationFixture) mcpTool(t *testing.T, name string, args any) mcpTestToolResult {
	t.Helper()
	response := f.mcpRPC(t, "tools/call", map[string]any{"name": name, "arguments": args})
	require.Equal(t, http.StatusOK, response.status, response.body)
	require.Nil(t, response.Error, response.body)
	var result mcpTestToolResult
	require.NoError(t, json.Unmarshal(response.Result, &result))
	require.Len(t, result.Content, 1)
	return result
}

func mcpErrorStatus(t *testing.T, result mcpTestToolResult) float64 {
	t.Helper()
	require.True(t, result.IsError, result.Content)
	errorValue, ok := result.StructuredContent["error"].(map[string]any)
	require.True(t, ok, result.StructuredContent)
	status, _ := errorValue["status"].(float64)
	return status
}

func extensionMeta() map[string]any {
	return map[string]any{mcpMetaClientCapabilities: map[string]any{"extensions": map[string]any{mcpTasksExtension: map[string]any{}}}}
}

func TestMCPProtocolConformance(t *testing.T) {
	f := newMCPFixture(t)

	t.Run("initialize negotiates and stays stateless", func(t *testing.T) {
		response := f.mcpRPC(t, "initialize", map[string]any{
			"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"},
		})
		require.Equal(t, http.StatusOK, response.status, response.body)
		require.Empty(t, response.header.Get("Mcp-Session-Id"))
		require.Equal(t, "no-store", response.header.Get("Cache-Control"))
		require.JSONEq(t, `7`, string(response.ID))
		var result map[string]any
		require.NoError(t, json.Unmarshal(response.Result, &result))
		require.Equal(t, "2025-06-18", result["protocolVersion"])
		capabilities := result["capabilities"].(map[string]any)
		require.Contains(t, capabilities, "tools")
		require.Contains(t, capabilities["extensions"].(map[string]any), mcpTasksExtension)
		require.Contains(t, capabilities["tasks"].(map[string]any)["requests"].(map[string]any), "tools")
		require.Equal(t, mcpServerName, result["serverInfo"].(map[string]any)["name"])
		require.NotEmpty(t, result["instructions"])

		response = f.mcpRPC(t, "initialize", map[string]any{"protocolVersion": "1999-01-01"})
		require.NoError(t, json.Unmarshal(response.Result, &result))
		require.Equal(t, mcpLatestLegacyVersion, result["protocolVersion"])
	})

	t.Run("notifications and responses are accepted without a body", func(t *testing.T) {
		response := f.mcpRaw(t, http.MethodPost, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
		require.Equal(t, http.StatusAccepted, response.status)
		require.Empty(t, response.body)
		response = f.mcpRaw(t, http.MethodPost, `{"jsonrpc":"2.0","id":3,"result":{}}`, nil)
		require.Equal(t, http.StatusAccepted, response.status)
	})

	t.Run("ping", func(t *testing.T) {
		response := f.mcpRPC(t, "ping", nil)
		require.JSONEq(t, `{}`, string(response.Result))
	})

	t.Run("tools/list", func(t *testing.T) {
		response := f.mcpRPC(t, "tools/list", map[string]any{})
		var result struct {
			Tools []map[string]any `json:"tools"`
		}
		require.NoError(t, json.Unmarshal(response.Result, &result))
		names := make([]string, 0, len(result.Tools))
		for _, tool := range result.Tools {
			names = append(names, tool["name"].(string))
			schema := tool["inputSchema"].(map[string]any)
			require.Equal(t, "object", schema["type"])
			require.Equal(t, false, schema["additionalProperties"])
			require.NotContains(t, tool, "REST")
			require.Contains(t, tool, "annotations")
		}
		require.Equal(t, []string{
			mcpToolListAgents, mcpToolGetAgent, mcpToolCreateTask, mcpToolGetTask, mcpToolListTasks, mcpToolWaitForTask,
			mcpToolCancelTask, mcpToolListPendingApprovals, mcpToolDecideApproval, mcpToolGetTaskResult,
			mcpToolListTaskArtifacts, mcpToolGetTaskArtifact,
		}, names)
		create := result.Tools[slices.IndexFunc(result.Tools, func(tool map[string]any) bool { return tool["name"] == mcpToolCreateTask })]
		require.Equal(t, map[string]any{"taskSupport": "optional"}, create["execution"])
	})

	t.Run("JSON-RPC errors", func(t *testing.T) {
		for name, tc := range map[string]struct {
			body   string
			status int
			code   int
		}{
			"unknown method":                 {mcpBody(t, "resources/list", nil), http.StatusOK, mcpCodeMethodNotFound},
			"batch":                          {`[` + mcpBody(t, "ping", nil) + `]`, http.StatusBadRequest, mcpCodeInvalidRequest},
			"parse":                          {`{"jsonrpc":`, http.StatusBadRequest, mcpCodeParseError},
			"trailing data":                  {mcpBody(t, "ping", nil) + `{}`, http.StatusBadRequest, mcpCodeParseError},
			"version":                        {`{"jsonrpc":"1.0","id":1,"method":"ping"}`, http.StatusBadRequest, mcpCodeInvalidRequest},
			"object id":                      {`{"jsonrpc":"2.0","id":{},"method":"ping"}`, http.StatusBadRequest, mcpCodeInvalidRequest},
			"unknown tool":                   {mcpBody(t, "tools/call", map[string]any{"name": "delete_cluster"}), http.StatusOK, mcpCodeInvalidParams},
			"tasks/update without extension": {mcpBody(t, "tasks/update", map[string]any{"taskId": "x"}), http.StatusOK, mcpCodeMissingCapabilities},
			"bad task id":                    {mcpBody(t, "tasks/get", map[string]any{"taskId": "orka.v1.%%%"}), http.StatusOK, mcpCodeInvalidParams},
		} {
			t.Run(name, func(t *testing.T) {
				response := f.mcpRaw(t, http.MethodPost, tc.body, nil)
				require.Equal(t, tc.status, response.status, response.body)
				require.NotNil(t, response.Error, response.body)
				require.Equal(t, tc.code, response.Error.Code)
			})
		}
	})

	t.Run("Mcp-Method header must match", func(t *testing.T) {
		response := f.mcpRaw(t, http.MethodPost, mcpBody(t, "ping", nil), map[string]string{mcpMethodHeader: "tools/list"})
		require.Equal(t, mcpCodeHeaderMismatch, response.Error.Code)
	})

	t.Run("authentication is required", func(t *testing.T) {
		response := f.mcpRaw(t, http.MethodPost, mcpBody(t, "ping", nil), map[string]string{"Authorization": ""})
		require.Equal(t, http.StatusUnauthorized, response.status)
	})

	t.Run("GET opens no stream", func(t *testing.T) {
		response := f.mcpRaw(t, http.MethodGet, "", nil)
		require.Equal(t, http.StatusMethodNotAllowed, response.status, response.body)
	})

	t.Run("sessionless protocol", func(t *testing.T) {
		meta := map[string]any{mcpMetaProtocolVersion: mcpProtocolVersionSessionless, mcpMetaClientCapabilities: map[string]any{}}
		response := f.mcpRPC(t, "server/discover", map[string]any{"_meta": meta})
		var discovered map[string]any
		require.NoError(t, json.Unmarshal(response.Result, &discovered))
		require.Equal(t, "complete", discovered["resultType"])
		require.Contains(t, discovered["supportedVersions"], mcpProtocolVersionSessionless)

		response = f.mcpRPC(t, "tools/list", map[string]any{"_meta": meta})
		require.Contains(t, string(response.Result), `"resultType":"complete"`)

		response = f.mcpRPC(t, "initialize", map[string]any{"_meta": meta})
		require.Equal(t, mcpCodeMethodNotFound, response.Error.Code)
		response = f.mcpRPC(t, "server/discover", nil)
		require.Equal(t, mcpCodeMethodNotFound, response.Error.Code)
		response = f.mcpRPC(t, "ping", map[string]any{"_meta": map[string]any{mcpMetaProtocolVersion: "2099-01-01", mcpMetaClientCapabilities: map[string]any{}}})
		require.Equal(t, mcpCodeUnsupportedVersion, response.Error.Code)
		response = f.mcpRPC(t, "ping", map[string]any{"_meta": map[string]any{mcpMetaProtocolVersion: mcpProtocolVersionSessionless}})
		require.Equal(t, mcpCodeInvalidParams, response.Error.Code)
	})

	t.Run("concurrency is bounded", func(t *testing.T) {
		mcp := f.server.mcpServer
		for range cap(mcp.slots) {
			mcp.slots <- struct{}{}
		}
		defer func() {
			for range cap(mcp.slots) {
				<-mcp.slots
			}
		}()
		response := f.mcpRPC(t, "ping", nil)
		require.Equal(t, http.StatusTooManyRequests, response.status)
		require.Equal(t, mcpCodeServerBusy, response.Error.Code)
		require.Equal(t, "1", response.header.Get("Retry-After"))
	})

	t.Run("origin must be allowed", func(t *testing.T) {
		mcp := f.server.mcpServer
		previous := mcp.allowedOrigins
		mcp.allowedOrigins = []string{"https://orka.example.com"}
		defer func() { mcp.allowedOrigins = previous }()
		response := f.mcpRaw(t, http.MethodPost, mcpBody(t, "ping", nil), map[string]string{"Origin": "https://evil.example.com"})
		require.Equal(t, http.StatusForbidden, response.status)
		response = f.mcpRaw(t, http.MethodPost, mcpBody(t, "ping", nil), map[string]string{"Origin": "https://orka.example.com"})
		require.Equal(t, http.StatusOK, response.status)
	})
}

func TestMCPEndpointDisabledByDefault(t *testing.T) {
	f := newExternalAuthorizationFixture(t)
	f.config.MCP = MCPServerConfig{}
	f.server = NewServer(f.kube, nil, f.config)
	response := f.mcpRPC(t, "ping", nil)
	require.Equal(t, http.StatusNotFound, response.status)
	for _, route := range f.server.app.GetRoutes(true) {
		require.NotEqual(t, mcpEndpointPath, route.Path)
	}
}

type mcpToolRouteCase struct {
	tool  string
	args  map[string]any
	route string
}

// mcpToolRouteCases maps every tool to the REST route whose authorization it
// must reproduce exactly.
var mcpToolRouteCases = []mcpToolRouteCase{
	{mcpToolListAgents, map[string]any{}, "GET /api/v1/agents"},
	{mcpToolGetAgent, map[string]any{"name": "protected"}, "GET /api/v1/agents/:name"},
	{mcpToolCreateTask, map[string]any{"agent": "protected", "prompt": "hello", "taskType": "agent", "name": "created"}, "POST /api/v1/tasks"},
	{mcpToolGetTask, map[string]any{"name": "protected"}, "GET /api/v1/tasks/:id"},
	{mcpToolListTasks, map[string]any{}, "GET /api/v1/tasks"},
	{mcpToolWaitForTask, map[string]any{"name": "protected", "timeoutSeconds": 1}, "GET /api/v1/tasks/:id"},
	{mcpToolCancelTask, map[string]any{"name": "protected"}, "DELETE /api/v1/tasks/:id"},
	{mcpToolListPendingApprovals, map[string]any{"task": "protected"}, "GET /api/v1/tasks/:id/approvals"},
	{mcpToolDecideApproval, map[string]any{"task": "protected", "approvalId": "protected", "decision": "approve"}, "POST /api/v1/tasks/:id/approvals/:approvalID/decision"},
	{mcpToolGetTaskResult, map[string]any{"name": "protected"}, "GET /api/v1/tasks/:id/result"},
	{mcpToolListTaskArtifacts, map[string]any{"name": "protected"}, "GET /api/v1/tasks/:id/artifacts"},
	{mcpToolGetTaskArtifact, map[string]any{"name": "protected", "filename": "protected"}, "GET /api/v1/tasks/:id/artifacts/:filename"},
}

func mcpRoutePermissions(t *testing.T, route string) []authorizationv1.ResourceAttributes {
	t.Helper()
	for _, tc := range externalAuthorizationCases(t) {
		if tc.route == route {
			return tc.permissions
		}
	}
	t.Fatalf("no authorization case for %s", route)
	return nil
}

func TestMCPToolRouteCasesCoverEveryTool(t *testing.T) {
	definitions := mcpToolDefinitions()
	tools := make([]string, 0, len(definitions))
	for _, tool := range definitions {
		tools = append(tools, tool.Name)
		index := slices.IndexFunc(mcpToolRouteCases, func(tc mcpToolRouteCase) bool { return tc.tool == tool.Name })
		require.NotEqual(t, -1, index, "tool %s lacks an authorization case", tool.Name)
		require.Equal(t, mcpToolRouteCases[index].route, tool.REST[0], "documented REST route for %s", tool.Name)
		for _, route := range tool.REST {
			route, _, _ = strings.Cut(route, " (")
			require.Contains(t, externalAPIPolicies, route, "tool %s replays an unknown route", tool.Name)
		}
	}
	require.Len(t, mcpToolRouteCases, len(tools))
}

// Every tool asks exactly the REST route's SubjectAccessReviews in order and
// stops at the first denial, before any Kubernetes read, store write, or
// provider call, and with only the caller's identity.
func TestMCPToolsDenyEachRESTPermissionBeforeEffects(t *testing.T) {
	f := newMCPFixture(t)
	for _, tc := range mcpToolRouteCases {
		permissions := mcpRoutePermissions(t, tc.route)
		require.NotEmpty(t, permissions)
		for deniedIndex := range permissions {
			t.Run(fmt.Sprintf("%s/permission-%d", tc.tool, deniedIndex), func(t *testing.T) {
				f.reviews = nil
				f.kubeCalls = 0
				before := f.changes(t)
				beforeTokenReviews := f.tokenReviews
				f.review = func(review *authorizationv1.SubjectAccessReview) error {
					index := len(f.reviews) - 1
					require.LessOrEqual(t, index, deniedIndex)
					f.requireIdentity(t, review.Spec)
					require.Equal(t, permissions[index], *review.Spec.ResourceAttributes)
					review.Status.Allowed = index < deniedIndex
					return nil
				}
				result := f.mcpTool(t, tc.tool, tc.args)
				require.Equal(t, float64(http.StatusForbidden), mcpErrorStatus(t, result))
				require.NotContains(t, result.Content[0].Text, "protected-content")
				require.Len(t, f.reviews, deniedIndex+1)
				require.Equal(t, beforeTokenReviews+1, f.tokenReviews, "replays must reuse the caller's authenticated token")
				require.Zero(t, f.kubeCalls, "denied tool touched a Kubernetes resource")
				require.Equal(t, before, f.changes(t), "denied tool changed a record")
				require.Zero(t, f.externalCalls.Load())
			})
		}
	}
}

// mcpToolRESTRequests is the REST request each tool case replays, sent
// directly to compare the reviews both paths ask.
var mcpToolRESTRequests = map[string]struct{ method, path, body string }{
	mcpToolListAgents:           {http.MethodGet, "/api/v1/agents?limit=20", ""},
	mcpToolGetAgent:             {http.MethodGet, "/api/v1/agents/protected", ""},
	mcpToolCreateTask:           {http.MethodPost, "/api/v1/tasks", `{"name":"created","type":"agent","prompt":"hello","agentRef":{"name":"protected"}}`},
	mcpToolGetTask:              {http.MethodGet, "/api/v1/tasks/protected", ""},
	mcpToolListTasks:            {http.MethodGet, "/api/v1/tasks?limit=20", ""},
	mcpToolWaitForTask:          {http.MethodGet, "/api/v1/tasks/protected", ""},
	mcpToolCancelTask:           {http.MethodDelete, "/api/v1/tasks/protected", ""},
	mcpToolListPendingApprovals: {http.MethodGet, "/api/v1/tasks/protected/approvals", ""},
	mcpToolDecideApproval:       {http.MethodPost, "/api/v1/tasks/protected/approvals/protected/decision", `{"decision":"approve"}`},
	mcpToolGetTaskResult:        {http.MethodGet, "/api/v1/tasks/protected/result", ""},
	mcpToolListTaskArtifacts:    {http.MethodGet, "/api/v1/tasks/protected/artifacts", ""},
	mcpToolGetTaskArtifact:      {http.MethodGet, "/api/v1/tasks/protected/artifacts/protected", ""},
}

// With every permission granted, a tool asks exactly the reviews of the REST
// request it replays: the route policy first, then the handler's own checks.
func TestMCPToolsAskTheSameReviewsAsREST(t *testing.T) {
	asked := func(f *externalAuthorizationFixture) []authorizationv1.ResourceAttributes {
		attributes := make([]authorizationv1.ResourceAttributes, 0, len(f.reviews))
		for _, review := range f.reviews {
			f.requireIdentity(t, review)
			attributes = append(attributes, *review.ResourceAttributes)
		}
		return attributes
	}
	for _, tc := range mcpToolRouteCases {
		t.Run(tc.tool, func(t *testing.T) {
			f := newMCPFixture(t)
			f.allowAll()
			result := f.mcpTool(t, tc.tool, tc.args)
			if result.IsError {
				require.NotEqual(t, float64(http.StatusForbidden), mcpErrorStatus(t, result))
			}
			viaMCP := asked(f)
			routePermissions := mcpRoutePermissions(t, tc.route)
			require.GreaterOrEqual(t, len(viaMCP), len(routePermissions))
			require.Equal(t, routePermissions, viaMCP[:len(routePermissions)])

			direct := newMCPFixture(t)
			direct.allowAll()
			request, ok := mcpToolRESTRequests[tc.tool]
			require.True(t, ok)
			status, body := direct.request(t, request.method, request.path, request.body)
			require.NotEqual(t, http.StatusForbidden, status, body)
			require.Equal(t, asked(direct), viaMCP)
		})
	}
}

func TestMCPToolsEnforceNamespaceIsolation(t *testing.T) {
	f := newMCPFixture(t)
	f.allowAll()
	for _, tc := range mcpToolRouteCases {
		t.Run(tc.tool, func(t *testing.T) {
			args := map[string]any{"namespace": "other"}
			maps.Copy(args, tc.args)
			f.reviews = nil
			f.kubeCalls = 0
			result := f.mcpTool(t, tc.tool, args)
			require.Equal(t, float64(http.StatusForbidden), mcpErrorStatus(t, result))
			require.Empty(t, f.reviews, "a namespace outside the caller's isolation boundary is refused before RBAC")
			require.Zero(t, f.kubeCalls)
		})
	}

	t.Run("task IDs cannot name another namespace", func(t *testing.T) {
		f.reviews = nil
		id := encodeMCPTaskID("other", "protected", "uid-protected")
		for _, method := range []string{"tasks/get", "tasks/cancel", "tasks/result"} {
			response := f.mcpRPC(t, method, map[string]any{"taskId": id})
			require.NotNil(t, response.Error, response.body)
			require.Equal(t, mcpCodeForbidden, response.Error.Code)
		}
		require.Empty(t, f.reviews)
	})
}

func TestMCPToolsEnforceContextTokenScopes(t *testing.T) {
	provider := newTestOIDCProvider(t)
	f := newMCPFixture(t)
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	require.NoError(t, err)
	cfg := f.config
	cfg.ContextTokens = testContextTokenConfig(t, provider, "")
	cfg.ContextTokenAuthorization = authz
	f.server = NewServer(f.kube, nil, cfg)

	call := func(t *testing.T, scope, tool string, args map[string]any) mcpTestToolResult {
		t.Helper()
		token := issueTestContextToken(t, provider, nil, map[string]any{"scope": scope, "tctx": map[string]any{"namespace": "default"}})
		response := f.mcpRaw(t, http.MethodPost, mcpBody(t, "tools/call", map[string]any{"name": tool, "arguments": args}),
			map[string]string{"Authorization": "", TransactionTokenHeaderName: token})
		require.Equal(t, http.StatusOK, response.status, response.body)
		require.Nil(t, response.Error, response.body)
		var result mcpTestToolResult
		require.NoError(t, json.Unmarshal(response.Result, &result))
		return result
	}

	for _, tc := range []struct {
		tool    string
		args    map[string]any
		granted string
		other   string
	}{
		{mcpToolListTasks, map[string]any{}, ContextTokenScopeTaskList, ContextTokenScopeTaskGet},
		{mcpToolGetTask, map[string]any{"name": "protected"}, ContextTokenScopeTaskGet, ContextTokenScopeTaskList},
		{mcpToolListAgents, map[string]any{}, ContextTokenScopeAgentsRead, ContextTokenScopeTaskList},
		{mcpToolCancelTask, map[string]any{"name": "protected"}, ContextTokenScopeTaskDelete, ContextTokenScopeTaskGet},
		{mcpToolCreateTask, map[string]any{"agent": "protected", "prompt": "hi", "taskType": "agent"}, "", ContextTokenScopeTaskGet},
		{mcpToolDecideApproval, map[string]any{"task": "protected", "approvalId": "a", "decision": "approve"}, "", ContextTokenScopeTaskGet},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			before := f.changes(t)
			denied := call(t, tc.other, tc.tool, tc.args)
			require.Equal(t, float64(http.StatusForbidden), mcpErrorStatus(t, denied))
			require.Equal(t, before, f.changes(t))
			if tc.granted != "" {
				allowed := call(t, tc.granted, tc.tool, tc.args)
				require.False(t, allowed.IsError, allowed.Content)
			}
			require.Empty(t, f.reviews, "transaction tokens never acquire Kubernetes RBAC semantics")
			require.Zero(t, f.tokenReviews)
		})
	}
}

func createMCPTestTask(t *testing.T, f *externalAuthorizationFixture, name string, phase corev1alpha1.TaskPhase, mutate ...func(*corev1alpha1.Task)) {
	t.Helper()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID("uid-" + name)},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "fix the bug token=supersecretvalue123",
			AgentRef: &corev1alpha1.AgentReference{Name: "protected"},
		},
		Status: corev1alpha1.TaskStatus{Phase: phase},
	}
	for _, fn := range mutate {
		fn(task)
	}
	require.NoError(t, f.kube.Create(t.Context(), task))
}

func appendMCPTestApproval(t *testing.T, f *externalAuthorizationFixture, task, id string) {
	t.Helper()
	_, err := f.store.AppendExecutionEvent(t.Context(), &store.ExecutionEvent{
		Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: task, TaskName: task,
		Type:    events.ExecutionEventTypeApprovalRequested,
		Content: json.RawMessage(fmt.Sprintf(`{"approvalID":%q,"action":"create_pr","riskSummary":"pushes with password=hunter2hunter2"}`, id)),
	})
	require.NoError(t, err)
}

func TestMCPApprovalDecisionsFollowRESTRules(t *testing.T) {
	f := newMCPFixture(t)
	f.allowAll()
	createMCPTestTask(t, f, "running", corev1alpha1.TaskPhaseRunning)
	appendMCPTestApproval(t, f, "running", "first")

	task := f.mcpTool(t, mcpToolGetTask, map[string]any{"name": "running"})
	require.False(t, task.IsError, task.Content)
	require.Equal(t, mcpStatusInputRequired, task.StructuredContent["status"])
	require.Equal(t, float64(1), task.StructuredContent["pendingApprovals"])
	require.NotContains(t, task.Content[0].Text, "supersecretvalue123", "prompts are redacted")

	listed := f.mcpTool(t, mcpToolListPendingApprovals, map[string]any{"task": "running"})
	approvalsList := listed.StructuredContent["approvals"].([]any)
	require.Len(t, approvalsList, 1)
	require.Equal(t, "first", approvalsList[0].(map[string]any)["id"])
	require.NotContains(t, listed.Content[0].Text, "hunter2hunter2")

	waited := f.mcpTool(t, mcpToolWaitForTask, map[string]any{"name": "running", "timeoutSeconds": 5})
	require.Equal(t, false, waited.StructuredContent["timedOut"], "a pending approval ends the wait")

	decided := f.mcpTool(t, mcpToolDecideApproval, map[string]any{"task": "running", "approvalId": "first", "decision": "approve", "reason": "reviewed by alice"})
	require.False(t, decided.IsError, decided.Content)
	require.Equal(t, "approved", decided.StructuredContent["status"])
	require.Equal(t, "system:serviceaccount:default:viewer", decided.StructuredContent["decisionActor"], "decisions are recorded under the caller's identity")

	repeated := f.mcpTool(t, mcpToolDecideApproval, map[string]any{"task": "running", "approvalId": "first", "decision": "approve"})
	require.False(t, repeated.IsError, "repeating the winning decision returns its saved state")
	competing := f.mcpTool(t, mcpToolDecideApproval, map[string]any{"task": "running", "approvalId": "first", "decision": "decline"})
	require.Equal(t, float64(http.StatusConflict), mcpErrorStatus(t, competing))

	missing := f.mcpTool(t, mcpToolDecideApproval, map[string]any{"task": "running", "approvalId": "missing", "decision": "approve"})
	require.Equal(t, float64(http.StatusNotFound), mcpErrorStatus(t, missing))

	createMCPTestTask(t, f, "finished", corev1alpha1.TaskPhaseSucceeded)
	appendMCPTestApproval(t, f, "finished", "late")
	late := f.mcpTool(t, mcpToolDecideApproval, map[string]any{"task": "finished", "approvalId": "late", "decision": "approve"})
	require.Equal(t, float64(http.StatusConflict), mcpErrorStatus(t, late), "finished Tasks refuse decisions")

	for _, args := range []map[string]any{
		{"task": "running", "approvalId": "first", "decision": "maybe"},
		{"task": "running", "approvalId": "", "decision": "approve"},
		{"task": "running", "approvalId": "..", "decision": "approve"},
		{"task": "running", "approvalId": "first", "decision": "approve", "reason": strings.Repeat("x", mcpMaxReasonLength+1)},
		{"task": "running", "approvalId": "first", "decision": "approve", "unexpected": true},
	} {
		invalid := f.mcpTool(t, mcpToolDecideApproval, args)
		require.True(t, invalid.IsError)
		require.Zero(t, mcpErrorStatus(t, invalid), "argument errors never reach the REST route")
	}
}

func TestMCPTasksExtensionLifecycle(t *testing.T) {
	f := newMCPFixture(t)
	f.allowAll()

	created := f.mcpRPC(t, "tools/call", map[string]any{
		"name": mcpToolCreateTask, "_meta": extensionMeta(),
		"arguments": map[string]any{"agent": "protected", "prompt": "do the thing", "taskType": "agent", "name": "delegated"},
	})
	require.Nil(t, created.Error, created.body)
	var task map[string]any
	require.NoError(t, json.Unmarshal(created.Result, &task))
	require.Equal(t, "task", task["resultType"])
	require.Equal(t, mcpStatusWorking, task["status"])
	require.Contains(t, task, "ttlMs")
	require.Equal(t, float64(mcpTaskPollIntervalMS), task["pollIntervalMs"])
	taskID := task["taskId"].(string)
	ref, err := decodeMCPTaskID(taskID)
	require.NoError(t, err)
	require.Equal(t, mcpTaskRef{Namespace: "default", Name: "delegated", UID: "uid-delegated"}, ref)

	stored := &corev1alpha1.Task{}
	require.NoError(t, f.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "delegated"}, stored))
	require.Equal(t, corev1alpha1.TaskTypeAgent, stored.Spec.Type)
	require.Equal(t, "protected", stored.Spec.AgentRef.Name)
	require.Equal(t, "do the thing", stored.Spec.Prompt)

	get := func() map[string]any {
		response := f.mcpRPC(t, "tasks/get", map[string]any{"taskId": taskID, "_meta": extensionMeta()})
		require.Nil(t, response.Error, response.body)
		var result map[string]any
		require.NoError(t, json.Unmarshal(response.Result, &result))
		return result
	}
	require.Equal(t, mcpStatusWorking, get()["status"])

	appendMCPTestApproval(t, f, "delegated", "approval-1")
	appendMCPTestApproval(t, f, "delegated", "approval-2")
	result := get()
	require.Equal(t, "complete", result["resultType"])
	require.Equal(t, mcpStatusInputRequired, result["status"])
	requests := result["inputRequests"].(map[string]any)
	require.Len(t, requests, 2)
	request := requests["approval-1"].(map[string]any)
	require.Equal(t, "elicitation/create", request["method"])
	params := request["params"].(map[string]any)
	require.Equal(t, "form", params["mode"])
	require.NotContains(t, params["message"], "hunter2hunter2")

	update := func(responses map[string]any) mcpTestResponse {
		return f.mcpRPC(t, "tasks/update", map[string]any{"taskId": taskID, "inputResponses": responses, "_meta": extensionMeta()})
	}
	response := update(map[string]any{"unknown": map[string]any{"action": "accept", "content": map[string]any{"decision": "approve"}}})
	require.Equal(t, mcpCodeInvalidParams, response.Error.Code)
	response = update(map[string]any{"approval-1": map[string]any{"action": "accept", "content": map[string]any{}}})
	require.Equal(t, mcpCodeInvalidParams, response.Error.Code, "accepting requires a decision")
	response = update(map[string]any{"approval-1": map[string]any{"action": "cancel"}})
	require.Nil(t, response.Error, response.body)
	require.Len(t, get()["inputRequests"].(map[string]any), 2, "a dismissed request stays pending")

	response = update(map[string]any{
		"approval-1": map[string]any{"action": "accept", "content": map[string]any{"decision": "approve", "reason": "ok"}},
		"approval-2": map[string]any{"action": "decline"},
	})
	require.Nil(t, response.Error, response.body)
	require.JSONEq(t, `{"resultType":"complete"}`, string(response.Result))
	listed := f.mcpTool(t, mcpToolListPendingApprovals, map[string]any{"task": "delegated", "includeDecided": true})
	statuses := map[string]any{}
	for _, item := range listed.StructuredContent["approvals"].([]any) {
		statuses[item.(map[string]any)["id"].(string)] = item.(map[string]any)["status"]
	}
	require.Equal(t, map[string]any{"approval-1": "approved", "approval-2": "declined"}, statuses)
	require.Equal(t, mcpStatusWorking, get()["status"])

	// Finished: tasks/get carries the create_task result.
	require.NoError(t, f.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "delegated"}, stored))
	stored.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	stored.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	require.NoError(t, f.kube.Update(t.Context(), stored))
	require.NoError(t, f.store.SaveResult(t.Context(), "default", "delegated", []byte("done; api_key=abcdef0123456789")))
	result = get()
	require.Equal(t, mcpStatusCompleted, result["status"])
	toolResult := result["result"].(map[string]any)
	require.NotContains(t, toolResult, "isError")
	require.Contains(t, toolResult["content"].([]any)[0].(map[string]any)["text"], "done;")
	require.NotContains(t, fmt.Sprint(toolResult), "abcdef0123456789")

	response = f.mcpRPC(t, "tasks/cancel", map[string]any{"taskId": taskID, "_meta": extensionMeta()})
	require.Equal(t, mcpCodeInvalidParams, response.Error.Code, "finished tasks cannot be cancelled")

	// A Task replaced under the same name is a different MCP task.
	require.NoError(t, f.kube.Delete(t.Context(), stored))
	createMCPTestTask(t, f, "delegated", corev1alpha1.TaskPhaseRunning, func(task *corev1alpha1.Task) { task.UID = "uid-replacement" })
	response = f.mcpRPC(t, "tasks/get", map[string]any{"taskId": taskID, "_meta": extensionMeta()})
	require.Equal(t, mcpCodeInvalidParams, response.Error.Code)
}

func TestMCPCoreTaskAugmentedCreateTask(t *testing.T) {
	f := newMCPFixture(t)
	f.allowAll()
	created := f.mcpRPC(t, "tools/call", map[string]any{
		"name": mcpToolCreateTask, "task": map[string]any{"ttl": 60000},
		"arguments": map[string]any{"agent": "protected", "prompt": "do the thing", "taskType": "agent", "name": "core"},
	})
	require.Nil(t, created.Error, created.body)
	var result struct {
		Task map[string]any `json:"task"`
	}
	require.NoError(t, json.Unmarshal(created.Result, &result))
	require.Equal(t, mcpStatusWorking, result.Task["status"])
	require.Contains(t, result.Task, "ttl")
	require.Equal(t, float64(mcpTaskPollIntervalMS), result.Task["pollInterval"])
	taskID := result.Task["taskId"].(string)

	response := f.mcpRPC(t, "tasks/get", map[string]any{"taskId": taskID})
	var fields map[string]any
	require.NoError(t, json.Unmarshal(response.Result, &fields))
	require.Equal(t, mcpStatusWorking, fields["status"])
	require.NotContains(t, fields, "resultType")

	appendMCPTestApproval(t, f, "core", "pending")
	response = f.mcpRPC(t, "tasks/get", map[string]any{"taskId": taskID})
	require.NoError(t, json.Unmarshal(response.Result, &fields))
	require.Equal(t, mcpStatusInputRequired, fields["status"])
	require.Contains(t, fields["statusMessage"], "decide_approval")

	response = f.mcpRPC(t, "tasks/cancel", map[string]any{"taskId": taskID})
	require.Nil(t, response.Error, response.body)
	require.NoError(t, json.Unmarshal(response.Result, &fields))
	require.Equal(t, mcpStatusCancelled, fields["status"])
	err := f.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "core"}, &corev1alpha1.Task{})
	require.Error(t, err, "cancellation deletes the Task through DELETE /api/v1/tasks/:id")

	// tasks/result returns the tool result of a finished Task.
	createMCPTestTask(t, f, "failed", corev1alpha1.TaskPhaseFailed, func(task *corev1alpha1.Task) { task.Status.Message = "boom" })
	response = f.mcpRPC(t, "tasks/result", map[string]any{"taskId": encodeMCPTaskID("default", "failed", "uid-failed")})
	require.Nil(t, response.Error, response.body)
	var toolResult map[string]any
	require.NoError(t, json.Unmarshal(response.Result, &toolResult))
	require.Equal(t, true, toolResult["isError"])
	require.Equal(t, encodeMCPTaskID("default", "failed", "uid-failed"),
		toolResult["_meta"].(map[string]any)[mcpMetaRelatedTask].(map[string]any)["taskId"])
}

func TestMCPCreateTaskInfersTypeAndMapsArguments(t *testing.T) {
	f := newMCPFixture(t)
	f.allowAll()
	native := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "native", Namespace: "default"}}
	runtime := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "default"}, Spec: corev1alpha1.AgentSpec{
		Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude},
	}}
	require.NoError(t, f.kube.Create(t.Context(), native))
	require.NoError(t, f.kube.Create(t.Context(), runtime))

	result := f.mcpTool(t, mcpToolCreateTask, map[string]any{"agent": "native", "prompt": "summarize", "name": "native-task", "timeout": "10m"})
	require.False(t, result.IsError, result.Content)
	task := &corev1alpha1.Task{}
	require.NoError(t, f.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "native-task"}, task))
	require.Equal(t, corev1alpha1.TaskTypeAI, task.Spec.Type)
	require.Equal(t, "summarize", task.Spec.AI.Prompt)
	require.Equal(t, 10*time.Minute, task.Spec.Timeout.Duration)

	result = f.mcpTool(t, mcpToolCreateTask, map[string]any{
		"agent": "coder", "prompt": "fix", "name": "coder-task",
		"workspace": map[string]any{"gitRepo": "https://github.com/orka-agents/orka", "branch": "main"},
		"session":   map[string]any{"name": "continued", "create": true},
	})
	require.False(t, result.IsError, result.Content)
	require.NoError(t, f.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "coder-task"}, task))
	require.Equal(t, corev1alpha1.TaskTypeAgent, task.Spec.Type)
	require.Equal(t, "https://github.com/orka-agents/orka", task.Spec.Workspace.GitRepo)
	require.Equal(t, "continued", task.Spec.SessionRef.Name)
	require.True(t, task.Spec.SessionRef.Create)

	generated := f.mcpTool(t, mcpToolCreateTask, map[string]any{"agent": "coder", "prompt": "fix"})
	require.False(t, generated.IsError, generated.Content)
	require.True(t, strings.HasPrefix(generated.StructuredContent["task"].(map[string]any)["name"].(string), "mcp-"))

	for _, args := range []map[string]any{
		{"agent": "coder"},
		{"agent": "coder", "prompt": strings.Repeat("x", mcpMaxPromptBytes+1)},
		{"agent": "Bad Name", "prompt": "x"},
		{"agent": "coder", "prompt": "x", "taskType": "container"},
		{"agent": "coder", "prompt": "x", "workspace": map[string]any{"password": "x"}},
		{"agent": "coder", "prompt": "x", "image": "busybox"},
	} {
		invalid := f.mcpTool(t, mcpToolCreateTask, args)
		require.True(t, invalid.IsError, args)
		require.Zero(t, mcpErrorStatus(t, invalid))
	}

	conflict := f.mcpTool(t, mcpToolCreateTask, map[string]any{"agent": "coder", "prompt": "fix", "name": "coder-task"})
	require.Equal(t, float64(http.StatusConflict), mcpErrorStatus(t, conflict))
}

func TestMCPResultsAreBoundedAndRedacted(t *testing.T) {
	f := newMCPFixture(t)
	f.allowAll()
	createMCPTestTask(t, f, "output", corev1alpha1.TaskPhaseSucceeded, func(task *corev1alpha1.Task) {
		task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	})
	secret := "ghp_" + strings.Repeat("A", 36)
	// Put the token across the first chunk boundary.
	text := strings.Repeat("a", mcpDefaultOutputBytes-10) + " " + secret + " " + strings.Repeat("é", 40000)
	require.NoError(t, f.store.SaveResult(t.Context(), "default", "output", []byte(text)))

	var collected strings.Builder
	offset := 0.0
	for range 10 {
		chunk := f.mcpTool(t, mcpToolGetTaskResult, map[string]any{"name": "output", "offset": offset})
		require.False(t, chunk.IsError, chunk.Content)
		content := chunk.StructuredContent["content"].(string)
		require.LessOrEqual(t, len(content), mcpDefaultOutputBytes)
		require.True(t, utf8.ValidString(content))
		require.NotContains(t, content, "AAAAAAAAAAAAAAAA")
		collected.WriteString(content)
		next, more := chunk.StructuredContent["nextOffset"].(float64)
		if !more {
			require.Equal(t, false, chunk.StructuredContent["truncated"])
			break
		}
		require.Equal(t, true, chunk.StructuredContent["truncated"])
		offset = next
	}
	require.NotContains(t, collected.String(), secret)
	require.Contains(t, collected.String(), "[REDACTED]")
	require.Equal(t, float64(len(collected.String())), float64(len(strings.Replace(text, secret, "[REDACTED]", 1))))

	tooLarge := f.mcpTool(t, mcpToolGetTaskResult, map[string]any{"name": "output", "maxBytes": mcpMaxOutputBytes + 1})
	require.True(t, tooLarge.IsError)
	quoted := f.mcpTool(t, mcpToolGetTaskResult, map[string]any{"name": "output", "maxBytes": "100"})
	require.False(t, quoted.IsError, "numeric strings are accepted")
	require.Equal(t, float64(100), quoted.StructuredContent["returnedBytes"])

	owner := store.ArtifactOwner{Namespace: "default", TaskName: "output", TaskUID: "uid-output"}
	require.NoError(t, f.store.SaveArtifact(t.Context(), store.SaveArtifactRequest{Owner: owner, Filename: "notes.md", ContentType: "text/markdown", Data: []byte("token: " + secret)}))
	require.NoError(t, f.store.SaveArtifact(t.Context(), store.SaveArtifactRequest{Owner: owner, Filename: "image.png", ContentType: "image/png", Data: []byte{0x89, 'P', 'N', 'G', 0, 1, 2}}))
	artifacts := f.mcpTool(t, mcpToolListTaskArtifacts, map[string]any{"name": "output"})
	require.Len(t, artifacts.StructuredContent["artifacts"], 2)
	notes := f.mcpTool(t, mcpToolGetTaskArtifact, map[string]any{"name": "output", "filename": "notes.md"})
	require.False(t, notes.IsError, notes.Content)
	require.NotContains(t, notes.Content[0].Text, secret)
	binary := f.mcpTool(t, mcpToolGetTaskArtifact, map[string]any{"name": "output", "filename": "image.png"})
	require.True(t, binary.IsError)
	require.Contains(t, binary.Content[0].Text, "binary")
	unsafe := f.mcpTool(t, mcpToolGetTaskArtifact, map[string]any{"name": "output", "filename": "../secret"})
	require.True(t, unsafe.IsError)

	// Task summaries never carry environment values or credential references.
	createMCPTestTask(t, f, "with-env", corev1alpha1.TaskPhaseRunning, func(task *corev1alpha1.Task) {
		task.Spec.Env = append(task.Spec.Env, corev1.EnvVar{Name: "API_TOKEN", Value: "plain-env-secret-value"})
		task.Spec.SecretRef = &corev1alpha1.SecretReference{Name: "credentials-secret"}
		task.Status.Message = "failed with Authorization: Bearer abcdefghijklmnop"
	})
	summary := f.mcpTool(t, mcpToolGetTask, map[string]any{"name": "with-env"})
	require.NotContains(t, summary.Content[0].Text, "plain-env-secret-value")
	require.NotContains(t, summary.Content[0].Text, "credentials-secret")
	require.NotContains(t, summary.Content[0].Text, "abcdefghijklmnop")
	listed := f.mcpTool(t, mcpToolListTasks, map[string]any{"limit": 50})
	require.NotContains(t, listed.Content[0].Text, "plain-env-secret-value")
	require.True(t, f.mcpTool(t, mcpToolListTasks, map[string]any{"limit": 51}).IsError)
}

func TestMCPWaitForTaskIsBounded(t *testing.T) {
	f := newMCPFixture(t)
	f.allowAll()
	createMCPTestTask(t, f, "slow", corev1alpha1.TaskPhaseRunning)
	started := time.Now()
	result := f.mcpTool(t, mcpToolWaitForTask, map[string]any{"name": "slow", "timeoutSeconds": 1})
	require.False(t, result.IsError, result.Content)
	require.Equal(t, true, result.StructuredContent["timedOut"])
	require.Less(t, time.Since(started), 10*time.Second)
	require.True(t, f.mcpTool(t, mcpToolWaitForTask, map[string]any{"name": "slow", "timeoutSeconds": mcpMaxWaitSeconds + 1}).IsError)

	mcp := f.server.mcpServer
	for range cap(mcp.waitSlots) {
		mcp.waitSlots <- struct{}{}
	}
	defer func() {
		for range cap(mcp.waitSlots) {
			<-mcp.waitSlots
		}
	}()
	busy := f.mcpTool(t, mcpToolWaitForTask, map[string]any{"name": "slow", "timeoutSeconds": 1})
	require.Equal(t, float64(http.StatusTooManyRequests), mcpErrorStatus(t, busy))
}

// tasks/result keeps waiting through input_required under one wait permit,
// so result waits can never hold more than the reserved share of requests.
func TestMCPTaskResultHoldsItsWaitPermitWhileInputIsRequired(t *testing.T) {
	f := newMCPFixture(t)
	f.allowAll()
	createMCPTestTask(t, f, "parked", corev1alpha1.TaskPhaseRunning)
	appendMCPTestApproval(t, f, "parked", "pending")
	mcp := f.server.mcpServer
	mcp.resultWait = 3 * time.Second

	type outcome struct{ response mcpTestResponse }
	done := make(chan outcome, 1)
	go func() {
		done <- outcome{f.mcpRPC(t, "tasks/result", map[string]any{"taskId": encodeMCPTaskID("default", "parked", "uid-parked")})}
	}()
	require.Eventually(t, func() bool { return len(mcp.waitSlots) == 1 }, 2*time.Second, 10*time.Millisecond)
	for range 10 {
		require.Len(t, mcp.waitSlots, 1, "the permit is held while the approval is pending")
		time.Sleep(150 * time.Millisecond)
	}
	result := <-done
	require.NotNil(t, result.response.Error, result.response.body)
	require.Equal(t, mcpCodeServerBusy, result.response.Error.Code)
	require.Empty(t, mcp.waitSlots)
}

// A stalled upload holds no concurrency permit: the body is read, bounded in
// size and time by the MCP request config, before a permit is taken.
func TestMCPStalledUploadHoldsNoPermit(t *testing.T) {
	for _, path := range []string{mcpEndpointPath, "/MCP/", mcpEndpointPath + "?x=1"} {
		var header fasthttp.RequestHeader
		header.SetMethod(fiber.MethodPost)
		header.SetRequestURI(path)
		header.SetHost("orka.example.com")
		config := requestBodyConfig(&header)
		require.Equal(t, mcpMaxRequestBytes, config.MaxRequestBodySize, path)
		require.Equal(t, mcpRequestReadTimeout, config.ReadTimeout, path)
	}

	app := fiber.New(fiber.Config{StreamRequestBody: true})
	app.Server().HeaderReceived = requestBodyConfig
	mcp := newMCPServer(nil, MCPServerConfig{MaxConcurrent: 1})
	app.Post(mcpEndpointPath, mcp.Handle)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() { served <- app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() {
		require.NoError(t, app.ShutdownWithTimeout(5*time.Second))
		require.NoError(t, <-served)
	})

	// Send more than fasthttp's 8 KiB body prefetch, so the handler runs
	// while the rest of the body is still outstanding.
	stalled, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	_, err = fmt.Fprintf(stalled, "POST %s HTTP/1.1\r\nHost: orka\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n{%s",
		mcpEndpointPath, 256<<10, strings.Repeat(" ", 16<<10))
	require.NoError(t, err)
	require.Never(t, func() bool { return len(mcp.slots) > 0 }, time.Second, 10*time.Millisecond)

	response, err := http.Post("http://"+listener.Addr().String()+mcpEndpointPath, "application/json", strings.NewReader(mcpBody(t, "ping", nil)))
	require.NoError(t, err)
	defer response.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusOK, response.StatusCode, "the stalled upload holds no permit")
	require.NoError(t, stalled.Close())

	// A chunked body carries no Content-Length and fasthttp does not bound
	// the stream: the read stops one byte past the limit and is refused
	// without waiting for the rest of the upload.
	chunked, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer chunked.Close() //nolint:errcheck
	size := mcpMaxRequestBytes + 1024
	_, err = fmt.Fprintf(chunked, "POST %s HTTP/1.1\r\nHost: orka\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n",
		mcpEndpointPath, size, strings.Repeat(" ", size))
	require.NoError(t, err)
	require.NoError(t, chunked.SetReadDeadline(time.Now().Add(10*time.Second)))
	tooLarge, err := http.ReadResponse(bufio.NewReader(chunked), nil)
	require.NoError(t, err, "the oversized chunked body is refused before the upload ends")
	defer tooLarge.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusRequestEntityTooLarge, tooLarge.StatusCode)

	// Decoding a Content-Encoding is work admitted by a permit: with every
	// permit taken, a compressed body is refused before it is expanded.
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, err = writer.Write([]byte(strings.Repeat(" ", 2*mcpMaxRequestBytes)))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	mcp.slots <- struct{}{}
	defer func() { <-mcp.slots }()
	request := httptest.NewRequest(http.MethodPost, mcpEndpointPath, &compressed)
	request.Header.Set("Content-Encoding", "gzip")
	busy, err := app.Test(request)
	require.NoError(t, err)
	defer busy.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusTooManyRequests, busy.StatusCode)
}

// The replay decodes nothing: Content-Encoding describes the MCP body, which
// was decoded before parsing, not the JSON the replay sends.
func TestMCPReplayDropsContentEncoding(t *testing.T) {
	f := newMCPFixture(t)
	f.allowAll()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, err := writer.Write([]byte(mcpBody(t, "tools/call", map[string]any{
		"name": mcpToolCreateTask, "arguments": map[string]any{"agent": "protected", "prompt": "do it", "taskType": "agent", "name": "gzipped"},
	})))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	response := f.mcpRaw(t, http.MethodPost, compressed.String(), map[string]string{"Content-Encoding": "gzip"})
	require.Equal(t, http.StatusOK, response.status, response.body)
	require.Nil(t, response.Error, response.body)
	var result mcpTestToolResult
	require.NoError(t, json.Unmarshal(response.Result, &result))
	require.False(t, result.IsError, result.Content)
	require.NoError(t, f.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "gzipped"}, &corev1alpha1.Task{}))
}

// Every maxBytes the schema allows advances through any content.
func TestMCPOutputChunksAlwaysAdvance(t *testing.T) {
	for _, text := range []string{"é" + strings.Repeat("x", 10), "𝄞𝄞𝄞", "a\x80\x80\x80\x80\x80\x80b", "\x80\x80\x80\x80\x80\x80\x80"} {
		limit := int64(utf8.UTFMax)
		offset := int64(0)
		var collected strings.Builder
		finished := false
		for range len(text) + 1 {
			chunk := mcpOutputChunk(text, offset, limit)
			content := chunk["content"].(string)
			collected.WriteString(content)
			next, more := chunk["nextOffset"].(int64)
			if !more {
				finished = true
				break
			}
			require.NotEmpty(t, content, "%q at %d", text, offset)
			require.Greater(t, next, offset, "%q at %d", text, offset)
			offset = next
		}
		require.True(t, finished, "%q", text)
		if utf8.ValidString(text) {
			require.Equal(t, text, collected.String())
		}
	}

	f := newMCPFixture(t)
	f.allowAll()
	createMCPTestTask(t, f, "accented", corev1alpha1.TaskPhaseSucceeded, func(task *corev1alpha1.Task) {
		task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	})
	require.NoError(t, f.store.SaveResult(t.Context(), "default", "accented", []byte("é and more")))
	tooSmall := f.mcpTool(t, mcpToolGetTaskResult, map[string]any{"name": "accented", "maxBytes": utf8.UTFMax - 1})
	require.True(t, tooSmall.IsError)
	require.Zero(t, mcpErrorStatus(t, tooSmall), "argument errors never reach the REST route")
	smallest := f.mcpTool(t, mcpToolGetTaskResult, map[string]any{"name": "accented", "maxBytes": utf8.UTFMax})
	require.False(t, smallest.IsError, smallest.Content)
	require.NotEmpty(t, smallest.StructuredContent["content"])
	require.Greater(t, smallest.StructuredContent["nextOffset"], float64(0))
}

// tasks/result and tasks/cancel fence their REST replays to the task ID's
// UID, and the REST routes enforce it, so a Task that reuses the name between
// the MCP layer's UID check and the replay is neither read nor deleted.
func TestMCPTaskResultAndCancelAreUIDFenced(t *testing.T) {
	f := newMCPFixture(t)
	f.allowAll()
	// Record Delete preconditions; the fake client does not enforce UIDs.
	var preconditions []*metav1.Preconditions
	f.kube = interceptor.NewClient(f.kube.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			options := &client.DeleteOptions{}
			options.ApplyOptions(opts)
			preconditions = append(preconditions, options.Preconditions)
			return c.Delete(ctx, obj, opts...)
		},
	})
	f.config.APIReader = f.kube
	f.server = NewServer(f.kube, nil, f.config)
	mcp := f.server.mcpServer
	next := mcp.handler
	var replays []string
	mcp.handler = func(ctx *fasthttp.RequestCtx) {
		replays = append(replays, string(ctx.Method())+" "+string(ctx.RequestURI()))
		next(ctx)
	}

	createMCPTestTask(t, f, "reused", corev1alpha1.TaskPhaseSucceeded, func(task *corev1alpha1.Task) {
		task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	})
	require.NoError(t, f.store.SaveResult(t.Context(), "default", "reused", []byte("reused output")))
	response := f.mcpRPC(t, "tasks/result", map[string]any{"taskId": encodeMCPTaskID("default", "reused", "uid-reused")})
	require.Nil(t, response.Error, response.body)
	require.Contains(t, response.body, "reused output")
	require.Contains(t, replays, "GET /api/v1/tasks/reused/result?namespace=default&uid=uid-reused")

	createMCPTestTask(t, f, "running", corev1alpha1.TaskPhaseRunning)
	response = f.mcpRPC(t, "tasks/cancel", map[string]any{"taskId": encodeMCPTaskID("default", "running", "uid-running")})
	require.Nil(t, response.Error, response.body)
	require.Contains(t, replays, "DELETE /api/v1/tasks/running?namespace=default&uid=uid-running")
	require.Len(t, preconditions, 1)
	require.NotNil(t, preconditions[0])
	require.Equal(t, types.UID("uid-running"), *preconditions[0].UID, "the Delete itself is UID-preconditioned")

	// REST enforcement: a mismatched uid is "task not found" and changes nothing.
	status, body := f.request(t, http.MethodGet, "/api/v1/tasks/reused/result?uid=uid-original", "")
	require.Equal(t, http.StatusNotFound, status, body)
	require.NotContains(t, body, "reused output")
	status, body = f.request(t, http.MethodGet, "/api/v1/tasks/reused/result", "")
	require.Equal(t, http.StatusOK, status, "the uid fence is optional for REST clients: %s", body)
	createMCPTestTask(t, f, "kept", corev1alpha1.TaskPhaseRunning)
	status, body = f.request(t, http.MethodDelete, "/api/v1/tasks/kept?uid=uid-original", "")
	require.Equal(t, http.StatusNotFound, status, body)
	require.NoError(t, f.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "kept"}, &corev1alpha1.Task{}))
	require.Len(t, preconditions, 1, "a mismatched uid never reaches Delete")
}

// A same-name replacement can publish its result before the cache sees it, so
// the post-read UID check must read live: a stale cached Task that still
// matches the requested UID must not let the replacement's output through.
func TestTaskResultUIDFenceReadsLiveTask(t *testing.T) {
	f := newMCPFixture(t)
	f.allowAll()
	createMCPTestTask(t, f, "replaced", corev1alpha1.TaskPhaseSucceeded, func(task *corev1alpha1.Task) {
		task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	})
	require.NoError(t, f.store.SaveResult(t.Context(), "default", "replaced", []byte("replacement output")))
	// The cached client still holds uid-replaced; the live reader already
	// sees the replacement Task.
	f.config.APIReader = interceptor.NewClient(f.kube.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if _, task := obj.(*corev1alpha1.Task); task && key.Name == "replaced" {
				obj.SetUID("uid-replacement")
			}
			return nil
		},
	})
	f.server = NewServer(f.kube, nil, f.config)

	status, body := f.request(t, http.MethodGet, "/api/v1/tasks/replaced/result?uid=uid-replaced", "")
	require.Equal(t, http.StatusNotFound, status, body)
	require.NotContains(t, body, "replacement output")
}

// A Streamable HTTP client may POST a chunked body without Content-Length.
// The full middleware chain must let it reach the MCP handler's bounded body
// reader, while other API routes keep requiring a Content-Length.
func TestMCPAcceptsChunkedRequestThroughServer(t *testing.T) {
	f := newMCPFixture(t)
	f.allowAll()
	chunked := func(path, body string) (int, string) {
		t.Helper()
		f.sequence++
		request := httptest.NewRequest(http.MethodPost, path, io.MultiReader(strings.NewReader(body)))
		request.ContentLength = 0
		request.TransferEncoding = []string{"chunked"}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Authorization", fmt.Sprintf("Bearer fixture-%s-%d", t.Name(), f.sequence))
		response, err := f.server.app.Test(request, fiber.TestConfig{Timeout: time.Minute, FailOnTimeout: true})
		require.NoError(t, err)
		defer response.Body.Close() //nolint:errcheck
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		return response.StatusCode, string(data)
	}

	for _, path := range []string{mcpEndpointPath, "/MCP/"} {
		status, body := chunked(path, mcpBody(t, "ping", nil))
		require.Equal(t, http.StatusOK, status, "%s: %s", path, body)
		require.Contains(t, body, `"result"`, path)
	}
	status, body := chunked("/api/v1/tasks", `{}`)
	require.Equal(t, http.StatusLengthRequired, status, body)
}

// Browser Streamable HTTP clients send MCP headers after initialization; the
// CORS preflight allows them only while the endpoint is enabled.
func TestMCPCORSPreflightAllowsMCPHeaders(t *testing.T) {
	preflight := func(server *Server) http.Header {
		request := httptest.NewRequest(http.MethodOptions, mcpEndpointPath, nil)
		request.Header.Set("Origin", "https://ide.example.com")
		request.Header.Set("Access-Control-Request-Method", http.MethodPost)
		request.Header.Set("Access-Control-Request-Headers", "authorization, content-type, mcp-protocol-version, mcp-method, mcp-name")
		response, err := server.app.Test(request)
		require.NoError(t, err)
		defer response.Body.Close() //nolint:errcheck
		require.Equal(t, http.StatusNoContent, response.StatusCode)
		return response.Header
	}
	f := newMCPFixture(t)
	allowed := strings.ToLower(preflight(f.server).Get("Access-Control-Allow-Headers"))
	for _, name := range mcpCORSHeaders {
		require.Contains(t, allowed, strings.ToLower(name))
	}

	f.config.MCP.Enabled = false
	allowed = strings.ToLower(preflight(NewServer(f.kube, nil, f.config)).Get("Access-Control-Allow-Headers"))
	require.NotContains(t, allowed, "mcp-")
}

// requireExtensionDetailedTask checks a tasks/get result against the
// io.modelcontextprotocol/tasks 2026-07-28 GetTaskResult (Result &
// DetailedTask): the common Task fields, then exactly the payload field of the
// status variant.
func requireExtensionDetailedTask(t *testing.T, task map[string]any) {
	t.Helper()
	require.Equal(t, "complete", task["resultType"])
	for _, field := range []string{"taskId", "createdAt", "lastUpdatedAt"} {
		require.IsType(t, "", task[field], field)
	}
	require.Contains(t, task, "ttlMs")
	payload := map[string]string{mcpStatusInputRequired: "inputRequests", mcpStatusCompleted: "result", mcpStatusFailed: "error"}
	status, _ := task["status"].(string)
	require.Contains(t, []string{mcpStatusWorking, mcpStatusInputRequired, mcpStatusCompleted, mcpStatusFailed, mcpStatusCancelled}, status)
	for variant, field := range payload {
		if variant == status {
			require.IsType(t, map[string]any{}, task[field], "%s requires %s", status, field)
			continue
		}
		require.NotContains(t, task, field, "%s must not carry %s", status, field)
	}
	if status == mcpStatusFailed {
		errorObject := task["error"].(map[string]any)
		require.IsType(t, float64(0), errorObject["code"])
		require.IsType(t, "", errorObject["message"])
	}
}

// The extension reserves failed for JSON-RPC errors: a failed tool call is
// completed with an isError result, and a cancelled task carries no result.
// The 2025-11-25 core status keeps failed, which covers isError results.
func TestMCPExtensionTaskShapesFollowTheSchema(t *testing.T) {
	f := newMCPFixture(t)
	f.allowAll()
	createMCPTestTask(t, f, "broken", corev1alpha1.TaskPhaseFailed, func(task *corev1alpha1.Task) {
		task.Status.Message = "agent exited with status 1"
		task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	})
	require.NoError(t, f.store.SaveResult(t.Context(), "default", "broken", []byte("stack trace")))
	createMCPTestTask(t, f, "stopped", corev1alpha1.TaskPhaseCancelled)
	createMCPTestTask(t, f, "busy", corev1alpha1.TaskPhaseRunning)
	createMCPTestTask(t, f, "asking", corev1alpha1.TaskPhaseRunning)
	appendMCPTestApproval(t, f, "asking", "pending")
	createMCPTestTask(t, f, "done", corev1alpha1.TaskPhaseSucceeded)

	get := func(name string, meta map[string]any) map[string]any {
		params := map[string]any{"taskId": encodeMCPTaskID("default", name, "uid-"+name)}
		if meta != nil {
			params["_meta"] = meta
		}
		response := f.mcpRPC(t, "tasks/get", params)
		require.Nil(t, response.Error, response.body)
		var task map[string]any
		require.NoError(t, json.Unmarshal(response.Result, &task))
		return task
	}
	for name, status := range map[string]string{
		"broken": mcpStatusCompleted, "stopped": mcpStatusCancelled, "busy": mcpStatusWorking,
		"asking": mcpStatusInputRequired, "done": mcpStatusCompleted,
	} {
		task := get(name, extensionMeta())
		requireExtensionDetailedTask(t, task)
		require.Equal(t, status, task["status"], name)
	}

	failed := get("broken", extensionMeta())
	result := failed["result"].(map[string]any)
	require.Equal(t, true, result["isError"], "a failed Task is a tool result with isError")
	require.Contains(t, fmt.Sprint(result), "stack trace")
	require.Equal(t, "agent exited with status 1", failed["statusMessage"])
	require.NotContains(t, get("done", extensionMeta())["result"], "isError")

	core := get("broken", nil)
	require.Equal(t, mcpStatusFailed, core["status"], "the core status keeps failed")
	require.NotContains(t, core, "result")
	response := f.mcpRPC(t, "tasks/result", map[string]any{"taskId": encodeMCPTaskID("default", "broken", "uid-broken")})
	require.Nil(t, response.Error, response.body)
	require.Contains(t, string(response.Result), `"isError":true`)
}

// Waits trade their request permit for a wait permit, so with a single
// request permit a pending tasks/result wait never blocks the approval
// decision or cancellation that ends it, and waits beyond the wait permits are
// refused rather than queued.
func TestMCPWaitsNeverStarveShortRequests(t *testing.T) {
	f := newMCPFixture(t)
	f.allowAll()
	f.config.MCP.MaxConcurrent = 1
	f.server = NewServer(f.kube, nil, f.config)
	mcp := f.server.mcpServer
	require.Equal(t, 1, cap(mcp.slots))
	require.Equal(t, 1, cap(mcp.waitSlots))
	mcp.resultWait = 30 * time.Second
	createMCPTestTask(t, f, "parked", corev1alpha1.TaskPhaseRunning)
	appendMCPTestApproval(t, f, "parked", "pending")
	taskID := encodeMCPTaskID("default", "parked", "uid-parked")

	done := make(chan mcpTestResponse, 1)
	go func() { done <- f.mcpRPC(t, "tasks/result", map[string]any{"taskId": taskID}) }()
	require.Eventually(t, func() bool { return len(mcp.waitSlots) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Empty(t, mcp.slots, "the wait gave back its request permit")

	second := f.mcpRPC(t, "tasks/result", map[string]any{"taskId": taskID})
	require.Equal(t, http.StatusOK, second.status, second.body)
	require.NotNil(t, second.Error, second.body)
	require.Equal(t, mcpCodeServerBusy, second.Error.Code, "a wait beyond the wait permits is refused")
	waited := f.mcpTool(t, mcpToolWaitForTask, map[string]any{"name": "parked", "timeoutSeconds": 1})
	require.Equal(t, float64(http.StatusTooManyRequests), mcpErrorStatus(t, waited))

	decided := f.mcpTool(t, mcpToolDecideApproval, map[string]any{"task": "parked", "approvalId": "pending", "decision": "approve"})
	require.False(t, decided.IsError, decided.Content)
	cancelled := f.mcpRPC(t, "tasks/cancel", map[string]any{"taskId": taskID})
	require.Nil(t, cancelled.Error, cancelled.body)

	select {
	case response := <-done:
		require.NotNil(t, response.Error, "the cancelled Task ends the wait: %s", response.body)
	case <-time.After(10 * time.Second):
		t.Fatal("the wait did not end after its Task was cancelled")
	}
	require.Empty(t, mcp.slots)
	require.Empty(t, mcp.waitSlots)
}

// Streamable HTTP 2026-07-28: mirrored headers must be present on sessionless
// requests and match the body; header, version, and capability failures are
// 400, an unimplemented sessionless method is 404, and sessions are never
// minted or echoed.
func TestMCPSessionlessHeadersAreValidated(t *testing.T) {
	f := newMCPFixture(t)
	f.allowAll()
	meta := map[string]any{mcpMetaProtocolVersion: mcpProtocolVersionSessionless, mcpMetaClientCapabilities: map[string]any{}}
	listAgents := map[string]any{"name": mcpToolListAgents, "arguments": map[string]any{}, "_meta": meta}
	send := func(method string, params any, edit func(map[string]string)) mcpTestResponse {
		headers := mcpMirroredHeaders(method, params)
		if edit != nil {
			edit(headers)
		}
		return f.mcpRaw(t, http.MethodPost, mcpBody(t, method, params), headers)
	}

	ok := send("tools/call", listAgents, func(h map[string]string) { h["Mcp-Session-Id"] = "stale-session" })
	require.Equal(t, http.StatusOK, ok.status, ok.body)
	require.Nil(t, ok.Error, ok.body)
	require.Empty(t, ok.header.Get("Mcp-Session-Id"), "sessions are ignored, never echoed")
	encoded := send("tools/call", listAgents, func(h map[string]string) {
		h[mcpNameHeader] = "=?base64?" + base64.StdEncoding.EncodeToString([]byte(mcpToolListAgents)) + "?="
	})
	require.Nil(t, encoded.Error, "a Base64 sentinel Mcp-Name is decoded before comparison: %s", encoded.body)

	for name, tc := range map[string]struct {
		method string
		params any
		edit   func(map[string]string)
		status int
		code   int
	}{
		"missing protocol version header":  {"tools/call", listAgents, func(h map[string]string) { delete(h, mcpProtocolVersionHeader) }, http.StatusBadRequest, mcpCodeHeaderMismatch},
		"protocol version header differs":  {"tools/call", listAgents, func(h map[string]string) { h[mcpProtocolVersionHeader] = "2025-11-25" }, http.StatusBadRequest, mcpCodeHeaderMismatch},
		"missing Mcp-Method":               {"tools/call", listAgents, func(h map[string]string) { delete(h, mcpMethodHeader) }, http.StatusBadRequest, mcpCodeHeaderMismatch},
		"missing Mcp-Name":                 {"tools/call", listAgents, func(h map[string]string) { delete(h, mcpNameHeader) }, http.StatusBadRequest, mcpCodeHeaderMismatch},
		"Mcp-Name differs":                 {"tools/call", listAgents, func(h map[string]string) { h[mcpNameHeader] = mcpToolCreateTask }, http.StatusBadRequest, mcpCodeHeaderMismatch},
		"Mcp-Name bad Base64":              {"tools/call", listAgents, func(h map[string]string) { h[mcpNameHeader] = "=?base64?!!!?=" }, http.StatusBadRequest, mcpCodeHeaderMismatch},
		"Mcp-Name without a body name":     {"ping", map[string]any{"_meta": meta}, func(h map[string]string) { h[mcpNameHeader] = "anything" }, http.StatusBadRequest, mcpCodeHeaderMismatch},
		"legacy Mcp-Name differs":          {"tools/call", map[string]any{"name": mcpToolListAgents, "arguments": map[string]any{}}, func(h map[string]string) { h[mcpNameHeader] = mcpToolCreateTask }, http.StatusBadRequest, mcpCodeHeaderMismatch},
		"header claims a modern version":   {"ping", nil, func(h map[string]string) { h[mcpProtocolVersionHeader] = mcpProtocolVersionSessionless }, http.StatusBadRequest, mcpCodeHeaderMismatch},
		"unsupported legacy header":        {"ping", nil, func(h map[string]string) { h[mcpProtocolVersionHeader] = "2024-11-05" }, http.StatusBadRequest, mcpCodeUnsupportedVersion},
		"unsupported _meta version":        {"ping", map[string]any{"_meta": map[string]any{mcpMetaProtocolVersion: "1900-01-01", mcpMetaClientCapabilities: map[string]any{}}}, nil, http.StatusBadRequest, mcpCodeUnsupportedVersion},
		"unimplemented sessionless method": {"resources/list", map[string]any{"_meta": meta}, nil, http.StatusNotFound, mcpCodeMethodNotFound},
		"sessionless initialize":           {"initialize", map[string]any{"_meta": meta}, nil, http.StatusNotFound, mcpCodeMethodNotFound},
		"sessionless missing capability":   {"tasks/update", map[string]any{"taskId": "x", "_meta": meta}, nil, http.StatusBadRequest, mcpCodeMissingCapabilities},
	} {
		t.Run(name, func(t *testing.T) {
			response := send(tc.method, tc.params, tc.edit)
			require.Equal(t, tc.status, response.status, response.body)
			require.NotNil(t, response.Error, response.body)
			require.Equal(t, tc.code, response.Error.Code, response.body)
		})
	}
	unsupported := send("ping", nil, func(h map[string]string) { h[mcpProtocolVersionHeader] = "2024-11-05" })
	require.Contains(t, unsupported.body, `"supported"`)

	// Legacy clients keep their shapes: no required headers, 200 errors.
	legacy := f.mcpRaw(t, http.MethodPost, mcpBody(t, "ping", nil), map[string]string{mcpProtocolVersionHeader: "2025-11-25"})
	require.Equal(t, http.StatusOK, legacy.status, legacy.body)
	require.Nil(t, legacy.Error, legacy.body)
	missing := f.mcpRaw(t, http.MethodPost, mcpBody(t, "resources/list", nil), nil)
	require.Equal(t, http.StatusOK, missing.status, missing.body)
	require.Equal(t, mcpCodeMethodNotFound, missing.Error.Code)
}

// The REST replay carries only the allow-listed caller headers: credentials
// (including configured context-token headers) and request labels. Nothing
// else an MCP client sends reaches the REST route.
func TestMCPReplayForwardsOnlyAllowListedHeaders(t *testing.T) {
	f := newMCPFixture(t)
	f.allowAll()
	mcp := f.server.mcpServer
	mcp.replayHeaders = mcpReplayHeaders(ContextTokenConfig{Profiles: []ContextTokenProfileConfig{{
		Headers: []TokenHeaderConfig{{Name: "X-Context-Token"}},
	}}})
	next := mcp.handler
	var replayed []http.Header
	mcp.handler = func(ctx *fasthttp.RequestCtx) {
		header := http.Header{}
		for key, value := range ctx.Request.Header.All() {
			header.Add(string(key), string(value))
		}
		replayed = append(replayed, header)
		next(ctx)
	}
	headers := mcpMirroredHeaders("tools/call", map[string]any{"name": mcpToolListAgents})
	maps.Copy(headers, map[string]string{
		"X-Context-Token": "context-token-value", "Traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		"User-Agent": "mcp-test", "Idempotency-Key": "replayed-key", "X-Orka-Tools": "disabled",
		"X-Forwarded-For": "203.0.113.9", "X-Forwarded-User": "admin", "Impersonate-User": "admin",
		"Mcp-Param-Region": "us", "Cookie": "session=abc", "Content-Encoding": "identity",
	})
	response := f.mcpRaw(t, http.MethodPost, mcpBody(t, "tools/call", map[string]any{"name": mcpToolListAgents, "arguments": map[string]any{}}), headers)
	require.Nil(t, response.Error, response.body)
	require.NotEmpty(t, replayed)
	for _, header := range replayed {
		require.True(t, strings.HasPrefix(header.Get("Authorization"), "Bearer fixture-"), header)
		require.Equal(t, "context-token-value", header.Get("X-Context-Token"))
		require.Equal(t, "mcp-test", header.Get("User-Agent"))
		require.NotEmpty(t, header.Get("Traceparent"))
		for _, name := range []string{
			"Idempotency-Key", "X-Orka-Tools", "X-Forwarded-For", "X-Forwarded-User", "Impersonate-User", "Mcp-Param-Region",
			"Cookie", "Content-Encoding", "Origin", mcpMethodHeader, mcpNameHeader, mcpProtocolVersionHeader,
		} {
			require.Empty(t, header.Values(name), "%s must not reach the REST route", name)
		}
	}
}

// A finished state is final for an MCP client, so tasks/get and tasks/result
// never report one without the Task's output when the result read fails
// transiently; they fail and the client retries. A result the REST route
// reports as absent is reported without output.
func TestMCPTerminalResultsAreNeverSilentlyDropped(t *testing.T) {
	f := newMCPFixture(t)
	f.allowAll()
	createMCPTestTask(t, f, "finished", corev1alpha1.TaskPhaseSucceeded, func(task *corev1alpha1.Task) {
		task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	})
	require.NoError(t, f.store.SaveResult(t.Context(), "default", "finished", []byte("the output")))
	mcp := f.server.mcpServer
	next := mcp.handler
	failResults := true
	mcp.handler = func(ctx *fasthttp.RequestCtx) {
		if failResults && strings.HasSuffix(string(ctx.Path()), "/result") {
			ctx.SetStatusCode(fiber.StatusServiceUnavailable)
			ctx.SetBodyString(`{"error":{"message":"result store unavailable"}}`)
			return
		}
		next(ctx)
	}
	taskID := encodeMCPTaskID("default", "finished", "uid-finished")

	get := f.mcpRPC(t, "tasks/get", map[string]any{"taskId": taskID, "_meta": extensionMeta()})
	require.NotNil(t, get.Error, "a completed task without its output is never reported: %s", get.body)
	require.Equal(t, mcpCodeInternalError, get.Error.Code)
	result := f.mcpRPC(t, "tasks/result", map[string]any{"taskId": taskID})
	require.NotNil(t, result.Error, result.body)

	failResults = false
	get = f.mcpRPC(t, "tasks/get", map[string]any{"taskId": taskID, "_meta": extensionMeta()})
	require.Nil(t, get.Error, get.body)
	require.Contains(t, get.body, "the output")

	// The store lost the result: the Task is still reported, without output.
	createMCPTestTask(t, f, "lost", corev1alpha1.TaskPhaseSucceeded, func(task *corev1alpha1.Task) {
		task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	})
	lost := f.mcpRPC(t, "tasks/get", map[string]any{"taskId": encodeMCPTaskID("default", "lost", "uid-lost"), "_meta": extensionMeta()})
	require.Nil(t, lost.Error, lost.body)
	var task map[string]any
	require.NoError(t, json.Unmarshal(lost.Result, &task))
	requireExtensionDetailedTask(t, task)
	require.Equal(t, mcpStatusCompleted, task["status"])
}
