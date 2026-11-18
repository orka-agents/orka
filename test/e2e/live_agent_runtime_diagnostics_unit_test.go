//go:build e2e

package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLiveACPToolFailureDiagnosticsOmitsPayloads(t *testing.T) {
	t.Parallel()
	var listed liveACPTaskEventsResponse
	require.NoError(t, json.Unmarshal([]byte(`{"events":[
		{"seq":1,"type":"ToolCallCompleted","contentText":"connection refused"},
		{"seq":2,"type":"ToolCallFailed","summary":"private-title",
		 "contentText":"Permission denied: private-output token=private-token",
		 "content":{"journalKind":"tool_terminal","toolKind":"execute","title":"private-command",
		 "arguments":{"secret":"private-argument"}}},
		{"seq":3,"type":"ToolCallFailed","content":{"journalKind":"tool_stream_closure","toolKind":"read",
		 "outcome":"outcome_unknown","controllerSynthesized":true,"contentOmitted":"private-reason"}},
		{"seq":4,"type":"ToolCallFailed","contentText":"private-unrecognized-error",
		 "content":{"journalKind":"private-kind","toolKind":"private-tool","outcome":"private-outcome"}},
		{"seq":5,"type":"ToolCallFailed","content":{"toolKind":{"unexpected":"private-nested"}}}
	]}`), &listed))

	got := liveACPToolFailureDiagnostics(listed)
	require.NotContains(t, got, "private-")
	require.NotContains(t, got, "connection refused")
	var failures []map[string]any
	require.NoError(t, json.Unmarshal([]byte(got), &failures))
	require.Len(t, failures, 4)
	require.Equal(t, "tool_terminal", failures[0]["journalKind"])
	require.Equal(t, "execute", failures[0]["toolKind"])
	require.Equal(t, []any{"permission denied"}, failures[0]["errorTextMarkers"])
	require.Equal(t, "tool_stream_closure", failures[1]["journalKind"])
	require.Equal(t, true, failures[1]["outcomeUnknown"])
	require.Equal(t, true, failures[1]["controllerSynthesized"])
	require.Equal(t, true, failures[1]["contentOmitted"])
	require.NotContains(t, failures[2], "toolKind")
	require.Equal(t, true, failures[3]["metadataUnavailable"])
}

func TestLiveACPToolFailureDiagnosticsBoundsOutput(t *testing.T) {
	t.Parallel()
	const event = `{"seq":1,"type":"ToolCallFailed","contentText":"sandbox: private-output","content":{}}`
	var listed liveACPTaskEventsResponse
	require.NoError(t, json.Unmarshal([]byte(`{"events":[`+strings.Repeat(event+",", 19)+event+`]}`), &listed))
	got := liveACPToolFailureDiagnostics(listed)
	var failures []map[string]any
	require.NoError(t, json.Unmarshal([]byte(got), &failures))
	require.Len(t, failures, 10)
	require.NotContains(t, got, "private-output")
}
