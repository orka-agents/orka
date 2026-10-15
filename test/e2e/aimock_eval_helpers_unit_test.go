//go:build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAimockEvalCaptureRejectsUnprovedObservations(t *testing.T) {
	const marker = "[e2e:eval-control]"
	body := `{"messages":[{"role":"user","content":"` + marker + `"}],"tools":[{}],` +
		`"metadata":{"padding":"` + strings.Repeat("x", 70000) + `"}}`
	entry := mockLLMCaptureEntry{RequestID: "new", Body: body, Status: 200, Complete: true}
	request := mockLLMRequest{RequestID: "new"}
	request.Body.Truncated = true
	request.Response.Fixture = json.RawMessage(`{"match":{"userMessage":"` + marker + `"}}`)
	before := mockLLMEvalSnapshot{instanceID: "instance", requestIDs: map[string]bool{}}
	state := mockLLMCaptureState{InstanceID: "instance", Entries: []mockLLMCaptureEntry{entry}}
	observed, err := validateMockLLMEvalCapture(before, state, []mockLLMRequest{request}, []string{marker})
	require.NoError(t, err)
	require.Len(t, observed, 1, "complete capture must cover an oversized aimock body")

	tests := []struct {
		name string
		edit func(*mockLLMCaptureState, *[]mockLLMRequest)
	}{
		{"collector restart", func(s *mockLLMCaptureState, _ *[]mockLLMRequest) { s.InstanceID = "replacement" }},
		{"duplicate identity", func(s *mockLLMCaptureState, _ *[]mockLLMRequest) {
			s.Entries = append(s.Entries, s.Entries[0])
		}},
		{"empty", func(s *mockLLMCaptureState, _ *[]mockLLMRequest) { s.Entries = nil }},
		{"incomplete", func(s *mockLLMCaptureState, _ *[]mockLLMRequest) { s.Entries[0].Complete = false }},
		{"failed stream", func(s *mockLLMCaptureState, _ *[]mockLLMRequest) { s.Entries[0].Error = "relay failed" }},
		{"body missing", func(s *mockLLMCaptureState, _ *[]mockLLMRequest) { s.Entries[0].Body = "" }},
		{"failed response", func(s *mockLLMCaptureState, _ *[]mockLLMRequest) { s.Entries[0].Status = 500 }},
		{"rejected", func(s *mockLLMCaptureState, _ *[]mockLLMRequest) { s.Rejected++ }},
		{"receipt missing", func(_ *mockLLMCaptureState, j *[]mockLLMRequest) { *j = nil }},
		{"wrong fixture", func(_ *mockLLMCaptureState, j *[]mockLLMRequest) {
			(*j)[0].Response.Fixture = json.RawMessage(`{"match":{}}`)
		}},
		{"missing identity", func(s *mockLLMCaptureState, _ *[]mockLLMRequest) { s.Entries[0].RequestID = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mockLLMCaptureState{InstanceID: "instance", Entries: []mockLLMCaptureEntry{entry}}
			j := []mockLLMRequest{request}
			tt.edit(&s, &j)
			_, err := validateMockLLMEvalCapture(before, s, j, []string{marker})
			require.Error(t, err)
			require.NotContains(t, err.Error(), body)
		})
	}
}

func TestAimockEvalCanaryDetector(t *testing.T) {
	const canary = "SYNTHETIC_CANARY_ONLY"
	for _, body := range []string{
		`{"messages":[{"content":"` + canary + `"}]}`,
		`{"metadata":{"unknownField":"` + canary + `"}}`,
		`{"metadata":{"unknownField":"\u0053YNTHETIC_CANARY_ONLY"}}`,
		`{"tools":[{"description":"` + canary + `"}]}`,
	} {
		err := checkModelContentCanaries(
			[]mockLLMEvalRequest{{capture: mockLLMCaptureEntry{Body: body}}}, map[string]string{"read": canary})
		require.Error(t, err)
		require.Contains(t, err.Error(), "role read")
		require.NotContains(t, err.Error(), canary)
		require.NotContains(t, err.Error(), body)
	}
	require.NoError(t, checkModelContentCanaries(
		[]mockLLMEvalRequest{{capture: mockLLMCaptureEntry{Body: `{"messages":[]}`}}}, map[string]string{"read": canary}))
	require.Error(t, checkModelContentCanaries(nil, map[string]string{"read": canary}))
	require.Error(t, checkModelContentCanaries(
		[]mockLLMEvalRequest{{capture: mockLLMCaptureEntry{Body: `{}`}}}, map[string]string{"read": ""}))
}

func TestAimockEvalOversizedToolResults(t *testing.T) {
	for name, body := range map[string]string{
		"chat": `{"messages":[
			{"role":"assistant","tool_calls":[{"id":"call","function":{"name":"denied","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call","content":"not authorized"}]}`,
		"responses": `{"input":[
			{"type":"function_call","call_id":"call","name":"denied","arguments":"{}"},
			{"type":"function_call_output","call_id":"call","output":"not authorized"}]}`,
		"anthropic": `{"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call","name":"denied","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call",
			"content":[{"type":"text","text":"not authorized"}]}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := mockLLMRequest{RawBody: json.RawMessage(body)}
			request.Body.Truncated = true
			require.Equal(t, []string{"not authorized"}, request.ToolResults("denied"))
			require.Empty(t, request.ToolResults("not-called"))
		})
	}
	request := mockLLMRequest{RawBody: json.RawMessage(`{"unknown":true}`)}
	request.Body.Truncated = true
	require.Empty(t, request.ToolResults("denied"))
}

func TestAimockEvalCaptureIncludesAuxiliaryRequests(t *testing.T) {
	const marker = "[e2e:isolated]"
	const canary = "SYNTHETIC_AUXILIARY_CANARY"
	before := mockLLMEvalSnapshot{instanceID: "instance", requestIDs: map[string]bool{"old": true}}
	state := mockLLMCaptureState{InstanceID: "instance", Entries: []mockLLMCaptureEntry{
		{RequestID: "old", Body: `{}`, Status: 200, Complete: true},
		{RequestID: "main", Body: `{"messages":[{"content":"` + marker + `"}]}`, Status: 200, Complete: true},
		{RequestID: "auxiliary", Body: `{"metadata":{"opaque":"` + canary + `"}}`, Status: 200, Complete: true},
	}}
	main := mockLLMRequest{RequestID: "main"}
	main.Response.Fixture = json.RawMessage(`{"match":{"userMessage":"` + marker + `"}}`)
	auxiliary := mockLLMRequest{RequestID: "auxiliary"}
	auxiliary.Response.Fixture = json.RawMessage(`{"match":{}}`)
	requests, err := validateMockLLMEvalCapture(before, state, []mockLLMRequest{main, auxiliary}, []string{marker})
	require.NoError(t, err)
	require.Len(t, requests, 2)
	err = checkModelContentCanaries(requests, map[string]string{"read": canary})
	require.Error(t, err)
	require.NotContains(t, err.Error(), canary)
	state.Entries = state.Entries[1:]
	_, err = validateMockLLMEvalCapture(before, state, []mockLLMRequest{main, auxiliary}, []string{marker})
	require.ErrorContains(t, err, "lost previously observed")
}

func TestAimockEvalJournalHeaderCorrelation(t *testing.T) {
	const marker = "[e2e:journal-shape]"
	const unretained = "SYNTHETIC_AUTH_HEADER_NOT_RETAINED"
	var request mockLLMRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"id":"aimock-internal-id", "headers":{"x-request-id":"capture-id",
		"authorization":"Bearer `+unretained+`"}, "body":{"messages":[]},
		"response":{"status":200,"fixture":{"match":{"userMessage":"`+marker+`"}}}
	}`), &request))
	state := mockLLMCaptureState{InstanceID: "instance", Entries: []mockLLMCaptureEntry{{
		RequestID: "capture-id", Body: `{"messages":[{"content":"` + marker + `"}]}`, Status: 200, Complete: true,
	}}}
	_, err := validateMockLLMEvalCapture(mockLLMEvalSnapshot{instanceID: "instance"},
		state, []mockLLMRequest{request}, []string{marker})
	require.NoError(t, err)
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), unretained)
}

func TestAimockEvalNoRequestsPreservesCaptureIntegrity(t *testing.T) {
	before := mockLLMEvalSnapshot{instanceID: "instance", requestIDs: map[string]bool{}}
	state := mockLLMCaptureState{InstanceID: "instance"}
	require.NoError(t, validateMockLLMEvalNoRequests(before, state))
	before.requestIDs["old"] = true
	state.Entries = []mockLLMCaptureEntry{{RequestID: "old"}}
	require.NoError(t, validateMockLLMEvalNoRequests(before, state))
	for _, test := range []struct {
		name string
		edit func(*mockLLMCaptureState)
	}{
		{"restart", func(s *mockLLMCaptureState) { s.InstanceID = "replacement" }},
		{"missing instance", func(s *mockLLMCaptureState) { s.InstanceID = "" }},
		{"lost identity", func(s *mockLLMCaptureState) { s.Entries = nil }},
		{"duplicate identity", func(s *mockLLMCaptureState) { s.Entries = append(s.Entries, s.Entries[0]) }},
		{"empty identity", func(s *mockLLMCaptureState) { s.Entries[0].RequestID = "" }},
		{"new request", func(s *mockLLMCaptureState) {
			s.Entries = append(s.Entries, mockLLMCaptureEntry{RequestID: "new"})
		}},
		{"rejected request", func(s *mockLLMCaptureState) { s.Rejected++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := mockLLMCaptureState{InstanceID: "instance", Entries: []mockLLMCaptureEntry{{RequestID: "old"}}}
			test.edit(&current)
			require.Error(t, validateMockLLMEvalNoRequests(before, current))
		})
	}
	require.Error(t, validateMockLLMEvalNoRequests(mockLLMEvalSnapshot{}, state))
}

func TestAimockEvalCaptureReadDeadline(t *testing.T) {
	stallKubectl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := suiteMockLLM.captureJournalWithin(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), 2*time.Second)
	require.NotContains(t, err.Error(), "SYNTHETIC_TIMEOUT_CANARY")
}

func TestAimockEvalJournalReadDeadline(t *testing.T) {
	stallKubectl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := suiteMockLLM.journalWithin(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), 2*time.Second)
	require.NotContains(t, err.Error(), "SYNTHETIC_TIMEOUT_CANARY")
}

// stallKubectl puts a kubectl on PATH that writes partial output, then hangs.
func stallKubectl(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	stub := "#!/bin/sh\nexec \"$E2E_CAPTURE_STALL_BINARY\" -test.run=^TestAimockEvalCaptureStallHelper$\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "kubectl"), []byte(stub), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("E2E_CAPTURE_STALL_BINARY", os.Args[0])
	t.Setenv("E2E_CAPTURE_STALL_HELPER", "1")
}

func TestAimockEvalCaptureStallHelper(t *testing.T) {
	if os.Getenv("E2E_CAPTURE_STALL_HELPER") != "1" {
		return
	}
	const partial = `{"instanceId":"stalled","entries":[{"body":"SYNTHETIC_TIMEOUT_CANARY`
	if _, err := os.Stdout.WriteString(partial); err != nil {
		os.Exit(1)
	}
	time.Sleep(time.Hour) // The parent's bounded CommandContext must terminate this process.
	os.Exit(0)
}

func TestAimockEvalCaptureFinalFlushDeadline(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("host Node unavailable; the pinned mock-image integration covers the remote reader")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"instanceId":"healthy","entries":[],"rejected":0}`))
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	// A successful write() return may still leave a small final chunk buffered.
	// Withhold its callback, while an active handle models the blocked pipe.
	blocked := `Object.defineProperty(process, "stdout", {value: {write(_chunk, _callback) {
		setInterval(() => {}, 1000); return true;
	}}});`
	// Node startup on a hosted runner can take seconds, so wall-clock time does
	// not identify the deadline. Exit status 1 with the local context unexpired
	// does: a local kill reports -1.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "-e", blocked+mockLLMReadScript(port, "/", 100*time.Millisecond))
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	require.Equal(t, 1, exit.ExitCode())
	require.NoError(t, ctx.Err(), "the remote deadline, not the local test deadline, must stop the reader")
}
