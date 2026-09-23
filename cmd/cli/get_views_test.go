/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// runCLI executes the root command against a test server and returns
// combined stdout/stderr.
func runCLI(t *testing.T, serverURL string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"--server", serverURL, "--token", "test-token", "--namespace", "default"}, args...))
	err := root.Execute()
	return out.String(), err
}

// jsonServer answers every GET/POST with the given routes' JSON bodies.
func jsonServer(t *testing.T, routes map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			body, ok = routes[r.URL.Path]
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(body) //nolint:errcheck
	}))
}

// assertReadableAndJSON runs a command twice: the default must print every
// wanted fragment as a readable view without JSON braces, and -o json must
// be exactly the indented JSON encoding of the API response.
func assertReadableAndJSON(t *testing.T, srv *httptest.Server, response any, wanted []string, unwanted []string, args ...string) {
	t.Helper()
	out, err := runCLI(t, srv.URL, args...)
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("%v default output is JSON:\n%s", args, out)
	}
	for _, want := range wanted {
		if !strings.Contains(out, want) {
			t.Errorf("%v default output missing %q:\n%s", args, want, out)
		}
	}
	for _, bad := range unwanted {
		if strings.Contains(out, bad) {
			t.Errorf("%v default output contains %q:\n%s", args, bad, out)
		}
	}
	jsonOut, err := runCLI(t, srv.URL, append(args, "-o", "json")...)
	if err != nil {
		t.Fatalf("%v -o json: %v", args, err)
	}
	expected, _ := json.MarshalIndent(response, "", "  ")
	if jsonOut != string(expected)+"\n" {
		t.Fatalf("%v -o json changed:\n got: %s\nwant: %s", args, jsonOut, expected)
	}
}

func TestTaskGetPrintsReadableViewByDefault(t *testing.T) {
	task := map[string]any{
		"metadata": map[string]any{"name": "proxy-78055b2e", "namespace": "default", "creationTimestamp": "2026-09-23T08:00:00Z"},
		"spec": map[string]any{
			"type": "agent", "prompt": "Add a /healthz endpoint",
			"agentRef":   map[string]any{"name": "codex-builder"},
			"sessionRef": map[string]any{"name": "chat-1"},
			"workspace":  map[string]any{"intent": "write"},
		},
		"status": map[string]any{
			"phase": "Succeeded", "resultRef": map[string]any{"available": true},
			"delivery": map[string]any{"state": "VerifiedExact", "branch": "orka/add-healthz-a7f2b1e9"},
		},
	}
	srv := jsonServer(t, map[string]any{
		"/api/v1/tasks/proxy-78055b2e":        task,
		"/api/v1/tasks/proxy-78055b2e/result": map[string]any{"result": "Added the endpoint.\nTests pass.\n"},
	})
	defer srv.Close()
	assertReadableAndJSON(t, srv, task,
		[]string{"Name:", "proxy-78055b2e", "Phase:", "Succeeded", "Agent:", "codex-builder", "Session:", "chat-1",
			"Delivery:", "VerifiedExact", "Publication branch:", "orka/add-healthz-a7f2b1e9", "Result:", "Added the endpoint."},
		[]string{"resultRef", "creationTimestamp"},
		"task", "get", "proxy-78055b2e")
}

func TestTaskGetReportsUnavailableResult(t *testing.T) {
	task := map[string]any{
		"metadata": map[string]any{"name": "t1", "namespace": "default"},
		"spec":     map[string]any{"type": "ai", "ai": map[string]any{"provider": "openai", "model": "gpt-5.6"}},
		"status":   map[string]any{"phase": "Succeeded", "resultRef": map[string]any{"available": true}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/result") {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":"result read denied"}`) //nolint:errcheck
			return
		}
		json.NewEncoder(w).Encode(task) //nolint:errcheck
	}))
	defer srv.Close()
	out, err := runCLI(t, srv.URL, "task", "get", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Result:") || !strings.Contains(out, "unavailable") || !strings.Contains(out, "403") {
		t.Fatalf("result failure not surfaced:\n%s", out)
	}
	if !strings.Contains(out, "Provider:") || !strings.Contains(out, "openai") {
		t.Fatalf("spec.ai.provider not shown:\n%s", out)
	}
}

func TestReadinessAbsentRendersFalse(t *testing.T) {
	agent := map[string]any{
		"metadata": map[string]any{"name": "new-agent", "namespace": "default"},
		"spec":     map[string]any{"model": map[string]any{"name": "gpt-5.6"}},
	}
	provider := map[string]any{
		"metadata": map[string]any{"name": "cold", "namespace": "default"},
		"spec":     map[string]any{"type": "openai"},
		"status":   map[string]any{"message": "secret missing"},
	}
	srv := jsonServer(t, map[string]any{"/api/v1/agents/new-agent": agent, "/api/v1/providers/cold": provider})
	defer srv.Close()
	for _, args := range [][]string{{"agent", "get", "new-agent"}, {"provider", "get", "cold"}} {
		out, err := runCLI(t, srv.URL, args...)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for line := range strings.SplitSeq(out, "\n") {
			if strings.HasPrefix(line, "Ready:") && strings.HasSuffix(line, "false") {
				found = true
			}
		}
		if !found {
			t.Fatalf("%v should show Ready: false:\n%s", args, out)
		}
	}
}

func TestAgentGetPrintsReadableViewByDefault(t *testing.T) {
	agent := map[string]any{
		"metadata": map[string]any{"name": "customer-support", "namespace": "default"},
		"spec": map[string]any{
			"model":        map[string]any{"name": "gpt-5.6", "provider": "openai"},
			"systemPrompt": map[string]any{"inline": strings.Repeat("Answer politely and briefly. ", 8)},
			"tools":        []any{map[string]any{"name": "web_search"}, map[string]any{"name": "crm-lookup"}},
		},
		"status": map[string]any{"ready": true, "activeTasks": 2},
	}
	srv := jsonServer(t, map[string]any{"/api/v1/agents/customer-support": agent})
	defer srv.Close()
	t.Setenv("COLUMNS", "80")
	out, _ := runCLI(t, srv.URL, "agent", "get", "customer-support")
	for line := range strings.SplitSeq(out, "\n") {
		if len([]rune(line)) > 80 {
			t.Fatalf("instructions did not wrap: %q", line)
		}
	}
	assertReadableAndJSON(t, srv, agent,
		[]string{"Model:", "gpt-5.6", "Provider:", "openai", "Tools:", "web_search, crm-lookup", "Instructions:", "Answer politely", "Active tasks:", "2"},
		[]string{"systemPrompt", "inline"},
		"agent", "get", "customer-support")
}

func TestProviderGetPrintsReadableViewByDefault(t *testing.T) {
	provider := map[string]any{
		"metadata": map[string]any{"name": "vekil", "namespace": "default"},
		"spec":     map[string]any{"type": "openai", "baseURL": "http://vekil:8080/v1", "defaultModel": "gpt-5.6"},
		"status":   map[string]any{"ready": true, "message": "validated"},
	}
	srv := jsonServer(t, map[string]any{"/api/v1/providers/vekil": provider})
	defer srv.Close()
	assertReadableAndJSON(t, srv, provider,
		[]string{"Name:", "vekil", "Type:", "openai", "Base URL:", "http://vekil:8080/v1", "Default model:", "gpt-5.6", "Ready:", "true"},
		nil, "provider", "get", "vekil")
}

func TestSecurityFindingGetAndPatchPrintReadableViews(t *testing.T) {
	finding := map[string]any{
		"id": "fnd_a9d4f27383dc", "sliceId": "slc_1", "title": "Zip-slip via AdmZip.extractAllTo on POST /import",
		"severity": "critical", "category": "path-traversal", "validationStatus": "validated", "state": "open",
		"filePath": "routes/import.js", "line": 42, "summary": "Archive entries are extracted without path checks.",
		"createdAt": "2026-09-23T08:00:00Z", "updatedAt": "2026-09-23T08:05:00Z",
	}
	proposal := map[string]any{
		"id": "pp_1", "findingID": "fnd_a9d4f27383dc", "status": "pr_opened", "branch": "orka/security-fix-1",
		"prURL": "https://github.com/example/app/pull/7", "taskName": "app-patch-1", "createdAt": "2026-09-23T09:00:00Z",
	}
	srv := jsonServer(t, map[string]any{
		"GET /api/v1/security/findings/fnd_a9d4f27383dc":         finding,
		"POST /api/v1/security/findings/fnd_a9d4f27383dc/patch":  proposal,
		"GET /api/v1/security/findings/fnd_a9d4f27383dc/patches": map[string]any{"items": []any{proposal}},
		"POST /api/v1/security/findings/fnd_a9d4f27383dc/pull-request": map[string]any{
			"prNumber": 7, "prURL": "https://github.com/example/app/pull/7", "status": "Open",
		},
	})
	defer srv.Close()
	assertReadableAndJSON(t, srv, finding,
		[]string{"Title:", "Zip-slip", "Severity:", "critical", "Validation:", "validated", "Location:", "routes/import.js:42", "Summary:", "Archive entries", "Category:", "path-traversal", "ID:", "fnd_a9d4f27383dc"},
		[]string{"sliceId", "updatedAt"},
		"security", "finding", "get", "fnd_a9d4f27383dc")
	assertReadableAndJSON(t, srv, proposal,
		[]string{"Status:", "pr_opened", "Branch:", "orka/security-fix-1", "Pull request:", "https://github.com/example/app/pull/7"},
		nil, "security", "finding", "patch", "fnd_a9d4f27383dc")

	out, err := runCLI(t, srv.URL, "security", "finding", "patches", "fnd_a9d4f27383dc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "STATUS") || !strings.Contains(out, "BRANCH") || !strings.Contains(out, "PULL REQUEST") || !strings.Contains(out, "CREATED") || !strings.Contains(out, "pr_opened") {
		t.Fatalf("patches table:\n%s", out)
	}
	out, err = runCLI(t, srv.URL, "security", "finding", "pr", "fnd_a9d4f27383dc")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "https://github.com/example/app/pull/7" {
		t.Fatalf("pr output = %q", out)
	}
}

func TestSecurityThreatModelGetPrintsMarkdownAsText(t *testing.T) {
	model := map[string]any{
		"namespace": "default", "repositoryScan": "nodejs-goof", "version": 3, "source": "generated",
		"content": "# Threat model\n\n## Assets\n\n- Session cookies\n", "updatedAt": "2026-09-23T08:00:00Z",
	}
	srv := jsonServer(t, map[string]any{"/api/v1/security/repositories/nodejs-goof/threat-model": model})
	defer srv.Close()
	assertReadableAndJSON(t, srv, model,
		[]string{"Repository:", "nodejs-goof", "Version:", "3", "Source:", "generated", "# Threat model\n\n## Assets\n\n- Session cookies\n"},
		[]string{`\n`},
		"security", "threat-model", "get", "nodejs-goof")
}

func TestMemoryProposalAndMemoryGetPrintReadableViews(t *testing.T) {
	proposal := map[string]any{
		"id": "mprop-e0330bbb", "namespace": "default", "taskName": "proxy-1", "agentName": "coder", "type": "memory",
		"title": "Prefer bun", "content": "The UI is built with bun, not npm.", "status": "accepted",
		"reviewer": "alice", "reviewNote": "Confirmed in Makefile.", "appliedMemoryId": "mem-1",
		"createdAt": "2026-09-23T08:00:00Z", "updatedAt": "2026-09-23T08:00:00Z",
	}
	memory := map[string]any{
		"id": "mem-1", "namespace": "default", "content": "The UI is built with bun, not npm.", "source": "proposal",
		"tags": []any{"build", "ui"}, "disabled": false, "deleted": false, "recalledCount": 4,
		"createdAt": "2026-09-23T08:00:00Z", "updatedAt": "2026-09-23T08:00:00Z",
	}
	srv := jsonServer(t, map[string]any{
		"/api/v1/memory-proposals/mprop-e0330bbb": proposal,
		"/api/v1/memories/mem-1":                  memory,
	})
	defer srv.Close()
	assertReadableAndJSON(t, srv, proposal,
		[]string{"Text:", "built with bun", "Status:", "accepted", "From task:", "proxy-1", "Reviewer:", "alice", "Review note:", "Confirmed", "Applied memory:", "mem-1"},
		[]string{"updatedAt"},
		"memory", "proposal", "get", "mprop-e0330bbb")
	assertReadableAndJSON(t, srv, memory,
		[]string{"Content:", "built with bun", "Source:", "proposal", "Tags:", "build, ui", "Recalled:", "4"},
		[]string{"disabled"},
		"memory", "get", "mem-1")
}

func TestToolAndAgentRuntimeGetPrintReadableViews(t *testing.T) {
	tool := map[string]any{
		"metadata": map[string]any{"name": "create-work-order", "namespace": "default"},
		"spec": map[string]any{
			"description": "Open a maintenance work order.",
			"http": map[string]any{"method": "POST", "url": "https://cmms.example.com/work-orders",
				"outboundAccessPolicyRef": map[string]any{"name": "cmms-egress"}},
		},
	}
	runtime := map[string]any{
		"metadata": map[string]any{"name": "demo-fibey", "namespace": "default"},
		"spec": map[string]any{
			"contractVersion": "orka.harness.v2",
			"capabilities": map[string]any{
				"profile":   map[string]any{"providerKind": "codex", "model": "gpt-5.6", "workspaceIntent": "read"},
				"mcpPolicy": map[string]any{"allowedTools": []any{"lookup-asset", "read-manual"}, "approvalRequiredTools": []any{"create-work-order"}},
			},
		},
		"status": map[string]any{"ready": true},
	}
	srv := jsonServer(t, map[string]any{
		"/api/v1/tools/create-work-order":   tool,
		"/api/v1/agent-runtimes/demo-fibey": runtime,
	})
	defer srv.Close()
	assertReadableAndJSON(t, srv, tool,
		[]string{"Type:", "http", "Method:", "POST", "URL:", "https://cmms.example.com/work-orders", "Outbound policy:", "cmms-egress", "Description:", "Open a maintenance"},
		nil, "tool", "get", "create-work-order")
	assertReadableAndJSON(t, srv, runtime,
		[]string{"Ready:", "true", "May call:", "lookup-asset, read-manual", "Needs approval:", "create-work-order", "Provider:", "codex/gpt-5.6"},
		nil, "agent-runtime", "get", "demo-fibey")
}

func TestToolGetPrefersMCPBackendAndRuntimePoolGetDescribes(t *testing.T) {
	mcpTool := map[string]any{
		"metadata": map[string]any{"name": "docs-search", "namespace": "default"},
		"spec": map[string]any{
			"description": "Search the docs.",
			"http":        map[string]any{"method": "POST"},
			"mcp":         map[string]any{"path": "/mcp", "workspace": map[string]any{"classRef": map[string]any{"name": "mcp-service"}, "port": 8080}},
		},
	}
	pool := map[string]any{
		"metadata": map[string]any{"name": "acp-codex-b8fda737", "namespace": "default", "creationTimestamp": "2026-09-23T08:00:00Z"},
		"status": map[string]any{
			"lifecycle": "Serving", "admissionState": "Accepting", "currentReplicas": 2, "desiredReplicas": 2,
			"capacity": map[string]any{"residentSessions": 3, "maxResidentSessions": 8, "runningPrompts": 1, "maxRunningPrompts": 4, "queuedTasks": 0},
		},
	}
	srv := jsonServer(t, map[string]any{
		"/api/v1/tools/docs-search":                mcpTool,
		"/api/v1/runtime-pools/acp-codex-b8fda737": pool,
	})
	defer srv.Close()
	assertReadableAndJSON(t, srv, mcpTool,
		[]string{"Type:", "mcp", "MCP server:", "workspace class mcp-service (/mcp)"},
		[]string{"Type:        http"},
		"tool", "get", "docs-search")
	assertReadableAndJSON(t, srv, pool,
		[]string{"Name:", "acp-codex-b8fda737", "Lifecycle:", "Serving", "Admission:", "Accepting", "Pods:", "2/2", "Sessions:", "3/8", "Prompts:", "1/4"},
		[]string{"LIFECYCLE"},
		"runtime-pool", "get", "acp-codex-b8fda737")
}

func TestWorkspaceStatusAndWhoamiPrintOneFieldPerLine(t *testing.T) {
	task := map[string]any{
		"metadata": map[string]any{"name": "write-1", "namespace": "default"},
		"spec":     map[string]any{"workspace": map[string]any{"intent": "write", "gitRepo": "https://github.com/example/app", "readCredentialRef": map[string]any{"name": "repo-read"}}},
		"status":   map[string]any{"phase": "Running", "delivery": map[string]any{"state": "Publishing"}},
	}
	identity := map[string]any{
		"authenticated": true, "authType": "tokenreview", "username": "system:serviceaccount:default:orka-client",
		"groups": []any{"system:serviceaccounts"}, "namespace": "default",
	}
	srv := jsonServer(t, map[string]any{
		"/api/v1/tasks/write-1": task,
		"/api/v1/auth/whoami":   identity,
		"/api/v1/auth/validate": map[string]any{"authenticated": true},
	})
	defer srv.Close()
	out, err := runCLI(t, srv.URL, "workspace", "status", "write-1")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasPrefix(lines[0], "task:") || !strings.HasSuffix(lines[0], "write-1") {
		t.Fatalf("task should come first: %q", lines[0])
	}
	for _, want := range [][2]string{{"phase:", "Running"}, {"  gitRepo:", "https://github.com/example/app"}, {"  readCredentialConfigured:", "true"}, {"  state:", "Publishing"}} {
		found := false
		for _, line := range lines {
			if strings.HasPrefix(line, want[0]) && strings.HasSuffix(line, want[1]) {
				found = true
			}
		}
		if !found {
			t.Errorf("workspace status missing %q %q:\n%s", want[0], want[1], out)
		}
	}
	if !strings.Contains(out, "workspace:\n") || !strings.Contains(out, "delivery:\n") {
		t.Errorf("workspace status sections missing:\n%s", out)
	}
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("workspace status printed JSON:\n%s", out)
	}
	assertReadableAndJSON(t, srv, identity,
		[]string{"User:", "system:serviceaccount:default:orka-client", "Auth type:", "tokenreview", "Groups:", "system:serviceaccounts"},
		nil, "auth", "whoami")
	out, err = runCLI(t, srv.URL, "auth", "validate")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "authenticated: true" {
		t.Fatalf("auth validate = %q", out)
	}
}

func TestCRUDGetFallsBackToGenericFields(t *testing.T) {
	pool := map[string]any{
		"metadata": map[string]any{"name": "actors", "namespace": "default", "creationTimestamp": "2026-09-23T08:00:00Z"},
		"status":   map[string]any{"phase": "Ready"},
	}
	srv := jsonServer(t, map[string]any{"/api/v1/substrate-actor-pools/actors": pool})
	defer srv.Close()
	assertReadableAndJSON(t, srv, pool,
		[]string{"Name:", "actors", "Namespace:", "default", "Status:", "Ready", "Age:"},
		nil, "substrate", "pool", "get", "actors")
}

func TestGatewayGetViewsShowReadinessEvenWhenOmitted(t *testing.T) {
	gateway := map[string]any{
		"metadata": map[string]any{"name": "slack-main", "namespace": "default"},
		"spec":     map[string]any{"gatewayClassName": "slack"},
		"status":   map[string]any{"accepted": true, "resolvedEndpoint": "http://slack-adapter:8080", "observedCapabilities": map[string]any{"adapterName": "slack", "adapterVersion": "1.2.0"}},
	}
	class := map[string]any{
		"metadata": map[string]any{"name": "slack"},
		"spec":     map[string]any{"contractVersion": "gateway.orka.ai/v1", "category": "chat"},
		"status":   map[string]any{"message": "adapter image not pinned"},
	}
	binding := map[string]any{
		"metadata": map[string]any{"name": "support", "namespace": "default"},
		"spec":     map[string]any{"gatewayRef": map[string]any{"name": "slack-main"}, "agentRef": map[string]any{"name": "support-agent"}, "priority": 10},
		"status":   map[string]any{"accepted": true, "resolvedRefs": true, "programmed": true, "ready": true},
	}
	srv := jsonServer(t, map[string]any{
		"/api/v1/gateways/slack-main":     gateway,
		"/api/v1/gatewayclasses/slack":    class,
		"/api/v1/gatewaybindings/support": binding,
	})
	defer srv.Close()
	assertReadableAndJSON(t, srv, gateway,
		[]string{"Class:", "slack", "Adapter:", "slack 1.2.0", "Endpoint:", "http://slack-adapter:8080", "Accepted:", "true", "Connected:", "false", "Ready:", "false"},
		nil, "gateway", "get", "slack-main")
	assertReadableAndJSON(t, srv, class,
		[]string{"Contract:", "gateway.orka.ai/v1", "Category:", "chat", "Accepted:", "false", "Message:", "adapter image not pinned"},
		nil, "gateway", "class", "get", "slack")
	assertReadableAndJSON(t, srv, binding,
		[]string{"Gateway:", "slack-main", "Agent:", "support-agent", "Priority:", "10", "Programmed:", "true", "Ready:", "true"},
		nil, "gateway", "binding", "get", "support")
}

func TestGatewayDeliveryGetReadsAttemptCounter(t *testing.T) {
	delivery := map[string]any{
		"id": "dlv-1", "state": "retrying", "kind": "reply", "gatewayName": "slack-main", "eventId": "evt-1",
		"attemptCount": 2, "maxAttempts": 5, "nextAttemptAt": "2026-09-23T08:10:00Z", "lastError": "upstream 503",
	}
	srv := jsonServer(t, map[string]any{"/api/v1/gateway-deliveries/dlv-1": delivery})
	defer srv.Close()
	assertReadableAndJSON(t, srv, delivery,
		[]string{"Attempts:", "2 of 5", "Next attempt:", "State:", "retrying: upstream 503"},
		nil, "gateway", "deliveries", "get", "dlv-1")
}

func TestProviderGetReadsFlatProjection(t *testing.T) {
	flat := map[string]any{"name": "vekil", "namespace": "default", "type": "anthropic", "defaultModel": "claude-sonnet-5", "ready": true}
	srv := jsonServer(t, map[string]any{"/api/v1/providers/vekil": flat})
	defer srv.Close()
	assertReadableAndJSON(t, srv, flat,
		[]string{"Name:", "vekil", "Type:", "anthropic", "Default model:", "claude-sonnet-5", "Ready:", "true"},
		nil, "provider", "get", "vekil")
}
