package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

const mcpNameField = "name"

func TestServeAdvertisesAndAcceptsRequiredCodexAuthentication(t *testing.T) {
	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"authenticate","params":{"methodId":"api-key"}}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"method":"session/new","params":{}}` + "\n",
	)
	var output bytes.Buffer
	if err := serve(input, &output); err != nil {
		t.Fatalf("serve() error = %v", err)
	}

	decoder := json.NewDecoder(&output)
	var initialize struct {
		Result struct {
			ProtocolVersion int `json:"protocolVersion"`
			AuthMethods     []struct {
				ID string `json:"id"`
			} `json:"authMethods"`
		} `json:"result"`
	}
	if err := decoder.Decode(&initialize); err != nil {
		t.Fatal(err)
	}
	if initialize.Result.ProtocolVersion != protocolVersion || len(initialize.Result.AuthMethods) != 1 ||
		initialize.Result.AuthMethods[0].ID != apiKeyAuthMethod {
		t.Fatalf("initialize result = %#v", initialize.Result)
	}

	var authenticated struct {
		ID     int            `json:"id"`
		Result map[string]any `json:"result"`
	}
	if err := decoder.Decode(&authenticated); err != nil {
		t.Fatal(err)
	}
	if authenticated.ID != 2 || authenticated.Result == nil {
		t.Fatalf("authenticate result = %#v", authenticated)
	}

	var newSession struct {
		Result struct {
			SessionID string `json:"sessionId"`
		} `json:"result"`
	}
	if err := decoder.Decode(&newSession); err != nil {
		t.Fatal(err)
	}
	if newSession.Result.SessionID != providerSessionID {
		t.Fatalf("session ID = %q, want %q", newSession.Result.SessionID, providerSessionID)
	}
}

func TestTerminalResultThreatModelPreservesBinding(t *testing.T) {
	prompt := `Use this exact envelope and identity values; replace only threatModel with the complete markdown document:
	{
	  "schemaVersion": 1,
	  "kind": "orka.security.threat-model.v1",
	  "repositoryScan": "security-goof",
	  "scanId": "scan_123",
	  "policyDigest": "sha256:policy",
	  "threatModel": "# Threat Model\n\n..."
	}
	`
	result, err := terminalResult(prompt)
	if err != nil {
		t.Fatalf("terminalResult() error = %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(result, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["scanId"] != "scan_123" || envelope["policyDigest"] != "sha256:policy" {
		t.Fatalf("binding changed: %#v", envelope)
	}
	if threatModel, _ := envelope["threatModel"].(string); len(threatModel) == 0 || threatModel[0] != '#' {
		t.Fatalf("threatModel = %q, want markdown heading", threatModel)
	}
}

func TestTerminalResultReviewUsesMapperContextAndAddsRejectedEvidence(t *testing.T) {
	prompt := "Use this exact envelope, repository identity, and binding values. " +
		"Populate findings.findings; keep it an empty array when no supported finding exists:\n" +
		`{
	  "schemaVersion": 1,
	  "kind": "orka.security.findings.v1",
	  "repositoryScan": "security-goof",
	  "scanId": "scan_123",
	  "sliceId": "slice_api",
	  "policyDigest": "sha256:policy",
	  "contextDigest": "sha256:context",
	  "findings": {
	    "schemaVersion": 2,
	    "repository": {
	      "repoURL": "https://github.com/example/repo",
	      "branch": "main",
	      "headSHA": "abc"
	    },
	    "scan": {
	      "mode": "initial",
	      "sliceId": "slice_api",
	      "summary": "..."
	    },
	    "findings": []
	  }
	}

Valid evidence paths for this review:
- README.md (context)
- server.js (owned)

Cite findings only from included file ranges below.

--- README.md (context) ---
     1  docs

--- server.js (owned) ---
    17  app.post('/admin', handler)
`
	result, err := terminalResult(prompt)
	if err != nil {
		t.Fatalf("terminalResult() error = %v", err)
	}
	var envelope struct {
		Findings struct {
			Findings []struct {
				Evidence []struct {
					Path      string `json:"path"`
					StartLine int    `json:"startLine"`
				} `json:"evidence"`
			} `json:"findings"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(result, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Findings.Findings) != 2 {
		t.Fatalf("findings = %#v, want accepted and rejected fixtures", envelope.Findings.Findings)
	}
	if got := envelope.Findings.Findings[0].Evidence[0]; got.Path != "server.js" || got.StartLine != 17 {
		t.Fatalf("accepted evidence = %#v, want mapper-owned server.js:17", got)
	}
	if got := envelope.Findings.Findings[1].Evidence[0].Path; got != "outside-mapper-context.txt" {
		t.Fatalf("rejected evidence path = %q", got)
	}
}

func TestTerminalResultMalformedScanBreaksIdentityBinding(t *testing.T) {
	prompt := `Use this exact envelope and identity values; replace only threatModel with the complete markdown document:
	{
	  "schemaVersion": 1,
	  "kind": "orka.security.threat-model.v1",
	  "repositoryScan": "security-goof-malformed-result",
	  "scanId": "scan_123",
	  "policyDigest": "sha256:policy",
	  "threatModel": "# Threat Model\n\n..."
	}
	`
	result, err := terminalResult(prompt)
	if err != nil {
		t.Fatalf("terminalResult() error = %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(result, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["scanId"] == "scan_123" {
		t.Fatalf("malformed fixture preserved scan binding: %#v", envelope)
	}
}

func TestServeAuthorityProbeCallsConfiguredMCPTool(t *testing.T) {
	var methods []string
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-mcp-credential" {
			t.Fatalf("MCP authorization header = %q", r.Header.Get("Authorization"))
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		methods = append(methods, request.Method)
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18"}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{mcpNameField: authorityProbeTool}}}
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(request.Params, &params); err != nil {
				t.Fatal(err)
			}
			if params.Name != authorityProbeTool || params.Arguments["probe"] != "authority" {
				t.Fatalf("MCP tool call = %#v", params)
			}
			result = map[string]any{
				"content": []map[string]any{{fieldType: contentTypeText, contentTypeText: `{"ok":true}`}},
			}
		default:
			t.Fatalf("unexpected MCP method %q", request.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			fieldJSONRPC: jsonRPCVersion,
			"id":         request.ID,
			"result":     result,
		})
	}))
	defer mcp.Close()

	input := new(bytes.Buffer)
	for _, message := range []map[string]any{
		{
			fieldJSONRPC: jsonRPCVersion, "id": 1, "method": methodSessionNew,
			"params": map[string]any{"mcpServers": []map[string]any{{
				fieldType: "http", mcpNameField: "orka", "url": mcp.URL,
				"headers": []map[string]any{{mcpNameField: "Authorization", "value": "Bearer fixture-mcp-credential"}},
			}}},
		},
		{
			fieldJSONRPC: jsonRPCVersion, "id": 2, "method": methodSessionPrompt,
			"params": map[string]any{
				"prompt": []map[string]any{{fieldType: contentTypeText, contentTypeText: authorityProbeMarker}},
			},
		},
	} {
		encoded, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		input.Write(encoded)
		input.WriteByte('\n')
	}

	var output bytes.Buffer
	if err := serve(input, &output); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(methods, []string{"initialize", "tools/list", "tools/call"}) {
		t.Fatalf("MCP methods = %#v", methods)
	}
	foundResult := false
	scanner := bufio.NewScanner(&output)
	for scanner.Scan() {
		var message map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			t.Fatal(err)
		}
		params, _ := message["params"].(map[string]any)
		update, _ := params["update"].(map[string]any)
		content, _ := update["content"].(map[string]any)
		text, _ := content[contentTypeText].(string)
		if strings.Contains(text, `"authorityProbe":true`) {
			foundResult = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !foundResult {
		t.Fatalf("ACP output did not contain the authority probe result: %s", output.String())
	}
}

func TestAuthorityObserverRecordsOnlyCountsAndDigests(t *testing.T) {
	observer := &authorityObserver{}
	subjectToken := "fixture-subject-token"
	transactionToken := "fixture-transaction-token"

	form := url.Values{"subject_token": []string{subjectToken}}
	tokenRequest := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	tokenRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenResponse := httptest.NewRecorder()
	observer.ServeHTTP(tokenResponse, tokenRequest)
	if tokenResponse.Code != http.StatusOK {
		t.Fatalf("token response = %d body=%s", tokenResponse.Code, tokenResponse.Body.String())
	}

	toolRequest := httptest.NewRequest(http.MethodPost, "/tool", strings.NewReader(`{"probe":"authority"}`))
	toolRequest.Header.Set("Txn-Token", transactionToken)
	toolResponse := httptest.NewRecorder()
	observer.ServeHTTP(toolResponse, toolRequest)
	if toolResponse.Code != http.StatusOK {
		t.Fatalf("tool response = %d body=%s", toolResponse.Code, toolResponse.Body.String())
	}

	statsResponse := httptest.NewRecorder()
	observer.ServeHTTP(statsResponse, httptest.NewRequest(http.MethodGet, "/stats", nil))
	var stats struct {
		TTSCalls               int    `json:"ttsCalls"`
		ToolCalls              int    `json:"toolCalls"`
		SubjectTokenDigest     string `json:"subjectTokenDigest"`
		TransactionTokenDigest string `json:"transactionTokenDigest"`
	}
	if err := json.Unmarshal(statsResponse.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.TTSCalls != 1 || stats.ToolCalls != 1 ||
		stats.SubjectTokenDigest != digestValue(subjectToken) ||
		stats.TransactionTokenDigest != digestValue(transactionToken) {
		t.Fatalf("observer stats = %#v", stats)
	}
	statsBody := statsResponse.Body.String()
	if strings.Contains(statsBody, subjectToken) || strings.Contains(statsBody, transactionToken) {
		t.Fatalf("observer stats exposed a raw token: %s", statsBody)
	}

	resetResponse := httptest.NewRecorder()
	observer.ServeHTTP(resetResponse, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if resetResponse.Code != http.StatusNoContent {
		t.Fatalf("reset response = %d", resetResponse.Code)
	}
	statsResponse = httptest.NewRecorder()
	observer.ServeHTTP(statsResponse, httptest.NewRequest(http.MethodGet, "/stats", nil))
	if err := json.Unmarshal(statsResponse.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.TTSCalls != 0 || stats.ToolCalls != 0 ||
		stats.SubjectTokenDigest != "" || stats.TransactionTokenDigest != "" {
		t.Fatalf("observer stats after reset = %#v", stats)
	}
}

func TestAuthorityObserverRecordsTransactionlessToolCall(t *testing.T) {
	observer := &authorityObserver{}
	toolResponse := httptest.NewRecorder()
	observer.ServeHTTP(
		toolResponse,
		httptest.NewRequest(http.MethodPost, "/tool", strings.NewReader(`{"probe":"authority"}`)),
	)
	if toolResponse.Code != http.StatusOK {
		t.Fatalf("tool response = %d body=%s", toolResponse.Code, toolResponse.Body.String())
	}

	statsResponse := httptest.NewRecorder()
	observer.ServeHTTP(statsResponse, httptest.NewRequest(http.MethodGet, "/stats", nil))
	var stats struct {
		TTSCalls               int    `json:"ttsCalls"`
		ToolCalls              int    `json:"toolCalls"`
		SubjectTokenDigest     string `json:"subjectTokenDigest"`
		TransactionTokenDigest string `json:"transactionTokenDigest"`
	}
	if err := json.Unmarshal(statsResponse.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.TTSCalls != 0 || stats.ToolCalls != 1 ||
		stats.SubjectTokenDigest != "" || stats.TransactionTokenDigest != "" {
		t.Fatalf("transactionless observer stats = %#v", stats)
	}
}
