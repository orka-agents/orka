/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func callTool(t *testing.T, message string, header http.Header) string {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
		`"params":{"name":"probe","arguments":{"message":"` + message + `"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	for name, values := range header {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	recorder := httptest.NewRecorder()
	handleMCP(recorder, req)
	var response struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil || len(response.Result.Content) != 1 {
		t.Fatalf("decode tools/call response: %v", err)
	}
	return response.Result.Content[0].Text
}

func TestEgressProbeSendsOnlyAPlaceholderAndReturnsTheUpstreamAnswer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(handleCredentialDigest))
	defer upstream.Close()
	t.Setenv(upstreamURLEnv, upstream.URL)

	sum := sha256.Sum256([]byte(egressPlaceholder))
	if got, want := callTool(t, "egress-probe", nil), "mcp-e2e-egress:sha256:"+hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("egress-probe = %q, want %q", got, want)
	}
	if got, want := callTool(t, "egress-probe-unauthenticated", nil), "mcp-e2e-egress:"+upstreamMissing; got != want {
		t.Fatalf("egress-probe-unauthenticated = %q, want %q", got, want)
	}
}

func TestEgressProbeReportsRefusalsAsBlocked(t *testing.T) {
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "egress denied", http.StatusForbidden)
	}))
	defer refusing.Close()
	t.Setenv(upstreamURLEnv, refusing.URL)
	if got := callTool(t, "egress-probe", nil); got != "mcp-e2e-egress:"+egressBlocked {
		t.Fatalf("refused probe = %q", got)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	t.Setenv(upstreamURLEnv, closed.URL)
	if got := callTool(t, "egress-probe", nil); got != "mcp-e2e-egress:"+egressBlocked {
		t.Fatalf("unreachable probe = %q", got)
	}

	t.Setenv(upstreamURLEnv, "")
	if got := callTool(t, "egress-probe", nil); got != "mcp-e2e-egress:unconfigured" {
		t.Fatalf("unconfigured probe = %q", got)
	}
}

func TestTraceContextReturnsOnlyTheReceivedTraceIDAndFlags(t *testing.T) {
	header := http.Header{}
	header.Set("traceparent", "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01")
	if got := callTool(t, "trace-context", header); got != "mcp-e2e-trace:4bf92f3577b34da6a3ce929d0e0e4736:01" {
		t.Fatalf("trace-context = %q", got)
	}
	if got := callTool(t, "trace-context", nil); got != "mcp-e2e-trace:none" {
		t.Fatalf("trace-context without traceparent = %q", got)
	}
	header.Set("traceparent", "garbage")
	if got := callTool(t, "trace-context", header); got != "mcp-e2e-trace:none" {
		t.Fatalf("trace-context with invalid traceparent = %q", got)
	}
}
