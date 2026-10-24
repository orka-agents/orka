package supervisor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// mapRuntimeEvent maps a runtime event that produces at most one harness event.
func (s *Server) mapRuntimeEvent(state *sessionState, prompt *promptState, event acp.PromptEvent) (*harnessv2.Event, error) {
	events, err := s.mapRuntimeEvents(state, prompt, event)
	if err != nil || len(events) == 0 {
		return nil, err
	}
	if len(events) > 1 {
		return nil, fmt.Errorf("runtime event produced %d harness events", len(events))
	}
	return &events[0], nil
}

const testLiveCredential = "orka-session-credential-0123456789abcdef"

func streamThrough(stream *redactionStream, chunks ...string) (string, []string) {
	emitted := make([]string, 0, len(chunks)+1)
	for _, chunk := range chunks {
		emitted = append(emitted, stream.write(chunk))
	}
	emitted = append(emitted, stream.flush())
	return strings.Join(emitted, ""), emitted
}

func TestRedactionStreamReplacesACredentialSplitAcrossChunks(t *testing.T) {
	redactor := newCredentialRedactor(testLiveCredential, "short")
	cases := map[string][]string{
		"two chunks":   {"token " + testLiveCredential[:10], testLiveCredential[10:] + " done"},
		"three chunks": {"token " + testLiveCredential[:5], testLiveCredential[5:20], testLiveCredential[20:] + " done"},
		"one byte":     {"token " + testLiveCredential[:len(testLiveCredential)-1], testLiveCredential[len(testLiveCredential)-1:] + " done"},
	}
	for name, chunks := range cases {
		t.Run(name, func(t *testing.T) {
			joined, emitted := streamThrough(redactor.stream(), chunks...)
			if joined != "token [REDACTED] done" {
				t.Fatalf("joined = %q", joined)
			}
			for _, piece := range emitted {
				if strings.Contains(piece, testLiveCredential[:8]) {
					t.Fatalf("emitted piece %q carries a credential prefix", piece)
				}
			}
		})
	}
	if redactor.replace("short") != "short" {
		t.Fatal("values shorter than the credential minimum must not be replaced")
	}
}

func TestRedactionStreamHoldsTheLastWordAndACredentialPrefix(t *testing.T) {
	stream := newCredentialRedactor(testLiveCredential).stream()
	// The last word may still grow into a credential shape.
	if got := stream.write("plain text with no prefix."); got != "plain text with no " {
		t.Fatalf("write = %q, want the last word held", got)
	}
	if got := stream.write(" ends with orka-ses"); got != "prefix. ends with " {
		t.Fatalf("write = %q, want the credential prefix held", got)
	}
	// The stream ends at turn end: the held text was not a credential.
	if got := stream.flush(); got != "orka-ses" {
		t.Fatalf("flush = %q", got)
	}
	if stream.pending() {
		t.Fatal("flush left text pending")
	}
}

// streamPublished streams chunks as the supervisor publishes them: every
// released piece is pattern-redacted on its own.
func streamPublished(stream *redactionStream, chunks ...string) []string {
	_, emitted := streamThrough(stream, chunks...)
	for i := range emitted {
		emitted[i] = executionevents.RedactExecutionEventText(emitted[i])
	}
	return emitted
}

func TestRedactionStreamKeepsCredentialPatternsWholeAcrossChunks(t *testing.T) {
	const token = "ghp_" + "abcdefghijklmnopqrstuvwxyz0123"
	cases := map[string]struct {
		chunks []string
		secret string
	}{
		"well-known token":     {[]string{"use ghp_", token[4:] + " now"}, token[4:]},
		"token is":             {[]string{"the token is ", "s3cr3tvalue123 ok"}, "s3cr3tvalue123"},
		"token is on its line": {[]string{"the token is\n", "s3cr3tvalue123 ok"}, "s3cr3tvalue123"},
		"quoted assignment":    {[]string{`password: "my `, `hidden value" done`}, "hidden value"},
		"bearer header":        {[]string{"Authorization: Bearer ", "abcdef0123456789xyz\nnext"}, "abcdef0123456789xyz"},
		"cookie line":          {[]string{"Cookie: a=1; ", "session=s3cr3tcookie\nnext line"}, "s3cr3tcookie"},
		"assignment":           {[]string{"export GITHUB_TOKEN=", "s3cr3tvalue123 && run"}, "s3cr3tvalue123"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Patterns apply without any exact credential configured.
			pieces := streamPublished(newCredentialRedactor().stream(), tc.chunks...)
			whole := executionevents.RedactExecutionEventText(strings.Join(tc.chunks, ""))
			if joined := strings.Join(pieces, ""); joined != whole {
				t.Fatalf("published %q, want %q", joined, whole)
			}
			for _, piece := range pieces {
				if strings.Contains(piece, tc.secret) {
					t.Fatalf("published piece %q carries the credential", piece)
				}
			}
		})
	}
}

func TestRedactionStreamReleasesProseWordByWord(t *testing.T) {
	stream := newCredentialRedactor().stream()
	if got := stream.write("hello wor"); got != "hello " {
		t.Fatalf("write = %q", got)
	}
	if got := stream.write("ld and more\nnext"); got != "world and more\n" {
		t.Fatalf("write = %q", got)
	}
	// Text with no safe cut past the hold bound waits for the end of the
	// stream, and then comes out whole.
	long := strings.Repeat("x", maxPatternHeldBytes+1)
	for _, chunk := range []string{long, " done"} {
		if got := stream.write(chunk); got != "" {
			t.Fatalf("held text came out early: %d bytes", len(got))
		}
	}
	if got := stream.flush(); got != "next"+long+" done" {
		t.Fatalf("flush = %d bytes", len(got))
	}
}

// An unfinished construct past the hold bound is never released before its
// value arrives, however much text it holds.
func TestRedactionStreamKeepsAnUnfinishedConstructPastTheHoldBound(t *testing.T) {
	long := strings.Repeat("x", maxPatternHeldBytes+1)
	for name, chunks := range map[string][]string{
		"after a long word": {long, ` password: "my `, `hidden value" done`},
		"long quoted value": {`password: "my `, strings.Repeat("hidden ", maxPatternHeldBytes/7+1), `value" done`},
	} {
		t.Run(name, func(t *testing.T) {
			pieces := streamPublished(newCredentialRedactor().stream(), chunks...)
			whole := executionevents.RedactExecutionEventText(strings.Join(chunks, ""))
			if joined := strings.Join(pieces, ""); joined != whole {
				t.Fatalf("published %d bytes, want %d", len(joined), len(whole))
			}
			for _, piece := range pieces {
				if strings.Contains(piece, "hidden") {
					t.Fatalf("published piece of %d bytes carries the credential", len(piece))
				}
			}
		})
	}
}

func TestRedactionStreamCutsOnUTF8Boundaries(t *testing.T) {
	// The credential starts with a multi-byte rune, so a held prefix could
	// otherwise split it.
	credential := "é-credential-value-0123456789"
	stream := newCredentialRedactor(credential).stream()
	first := stream.write("café é")
	if !strings.HasPrefix("café é", first) || !strings.HasSuffix("café é", stream.held) {
		t.Fatalf("write split the text as %q + %q", first, stream.held)
	}
	for _, piece := range []string{first, stream.held} {
		if !strings.ContainsRune(piece, '�') && !json.Valid([]byte(`"`+piece+`"`)) {
			t.Fatalf("piece %q is not valid UTF-8", piece)
		}
	}
}

func TestCredentialRedactorRedactsToolSnapshots(t *testing.T) {
	redactor := newCredentialRedactor(testLiveCredential)
	old := "key=" + testLiveCredential
	tool := &harnessv2.ToolCallUpdate{
		ToolCallID: "call", Status: harnessv2.ToolCallStatusCompleted, Title: "echo " + testLiveCredential,
		Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "out " + testLiveCredential}},
		Diffs:   []harnessv2.ToolCallDiff{{Path: "a.txt", OldText: &old, NewText: testLiveCredential + "\n"}},
	}
	redactor.redactToolCall(tool)
	tool.RawInput = redactor.redactRawInput(json.RawMessage(`{"command":"curl -H 'X: ` + testLiveCredential + `'","n":1}`))
	encoded, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), testLiveCredential) {
		t.Fatalf("tool update still carries the credential: %s", encoded)
	}
	if !json.Valid(tool.RawInput) || !strings.Contains(string(tool.RawInput), `"n":1`) {
		t.Fatalf("raw input = %s", tool.RawInput)
	}
	if redactor.redactRawInput(json.RawMessage(`{"broken"`)) != nil {
		t.Fatal("undecodable raw input must be dropped")
	}
}

func TestScrubEventReplacesACredentialInAnyField(t *testing.T) {
	redactor := newCredentialRedactor(testLiveCredential)
	event := harnessv2.Event{
		Protocol: harnessv2.ProtocolVersion, Type: harnessv2.EventFailed,
		Failed: &harnessv2.FailedEvent{Code: "acp_prompt_failed", Message: "failed: " + testLiveCredential},
	}
	if err := redactor.scrubEvent(&event); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(event.Failed.Message, testLiveCredential) || !strings.Contains(event.Failed.Message, "[REDACTED]") {
		t.Fatalf("message = %q", event.Failed.Message)
	}
}

func TestSplitUTF8KeepsRunesWhole(t *testing.T) {
	text := strings.Repeat("é", 5)
	pieces := splitUTF8(text, 3)
	if strings.Join(pieces, "") != text {
		t.Fatalf("pieces %q do not rejoin", pieces)
	}
	for _, piece := range pieces {
		if len(piece) > 3 || !json.Valid([]byte(`"`+piece+`"`)) {
			t.Fatalf("piece %q exceeds the bound or splits a rune", piece)
		}
	}
}

// writeCredentialEchoUpdates is the fake agent's credential-echo turn: the
// credential split across assistant chunks that arrive too far apart to be
// coalesced, then in a thought, a tool title, raw input, output and a diff.
func writeCredentialEchoUpdates(writer *bufio.Writer, sessionID, workspace, credential string) {
	update := func(value map[string]any) {
		writeHelperMessage(writer, map[string]any{testJSONRPCKey: testJSONRPCVersion, "method": acp.MethodSessionUpdate, "params": map[string]any{
			"sessionId": sessionID, "update": value,
		}})
	}
	text := func(kind, value string) map[string]any {
		return map[string]any{"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": value}}
	}
	for _, piece := range []string{"key: " + credential[:10], credential[10:25], credential[25:] + " end"} {
		update(text("agent_message_chunk", piece))
		time.Sleep(3 * assistantMessageCoalesceWindow)
	}
	update(text("agent_thought_chunk", "thinking about "+credential))
	// Edits are reported under the ACP session cwd, the session workspace.
	update(map[string]any{
		"sessionUpdate": "tool_call", "toolCallId": "echo-call", "title": "echo " + credential, "kind": "edit",
		"rawInput": map[string]any{"command": "echo " + credential},
		"content": []any{
			map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "out " + credential}},
			map[string]any{"type": "diff", "path": filepath.Join(workspace, "notes.txt"), "oldText": credential, "newText": "rotated\n"},
			map[string]any{"type": "diff", "path": "/etc/hosts", "newText": "outside\n"},
		},
	})
	update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "echo-call", "status": "completed"})
}

func runCredentialEchoPrompt(t *testing.T, live bool) ([]byte, []harnessv2.Event, string) {
	t.Helper()
	server, cfg, profile := newTestServer(t, credentialEchoMode)
	create := testCreateSessionRequest(t, cfg, profile)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", response.Code, response.Body.String())
	}
	server.mu.Lock()
	credential := string(server.sessions["session-1"].providerProxy.credential)
	server.mu.Unlock()
	if len(credential) < 40 {
		t.Fatal("session has no provider credential")
	}
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	req := mutationHTTPRequest(t, http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg)
	if live {
		req.Header.Set(harnessv2.LiveUpdatesHeader, "1")
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("prompt status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.Bytes()
	decoder, err := harnessv2.NewEventDecoder(bytes.NewReader(body), eventLimits(cfg.Capabilities.Limits), harnessv2.EventExpectationFromMetadata(prompt.Metadata))
	if err != nil {
		t.Fatal(err)
	}
	events, err := decoder.DecodeAll()
	if err != nil {
		t.Fatalf("decode prompt events: %v", err)
	}
	return body, events, credential
}

func TestSupervisorStreamNeverCarriesASessionCredential(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("live=%v", live), func(t *testing.T) {
			body, events, credential := runCredentialEchoPrompt(t, live)
			if bytes.Contains(body, []byte(credential)) || bytes.Contains(body, []byte(credential[:25])) {
				t.Fatalf("NDJSON stream carries the session credential or a prefix of it")
			}
			var assistant, thought strings.Builder
			var diffs, rawInputs int
			for _, event := range events {
				if event.Update == nil {
					continue
				}
				if event.Update.AssistantMessage != nil {
					assistant.WriteString(event.Update.AssistantMessage.Text)
				}
				if event.Update.AssistantThought != nil {
					thought.WriteString(event.Update.AssistantThought.Text)
				}
				if tool := event.Update.ToolCall; tool != nil {
					diffs += len(tool.Diffs)
					for _, diff := range tool.Diffs {
						if diff.Path != "notes.txt" || !tool.DiffsOmitted {
							t.Fatalf("diff = %#v omitted=%v, want notes.txt kept and the outside edit omitted", diff, tool.DiffsOmitted)
						}
					}
					if len(tool.RawInput) > 0 {
						rawInputs++
					}
				}
			}
			if got := assistant.String(); got != "key: [REDACTED] end" {
				t.Fatalf("streamed assistant text = %q", got)
			}
			terminal := events[len(events)-1]
			if terminal.Completed == nil || terminal.Completed.Result.Content[0].Text != "key: [REDACTED] end" {
				t.Fatalf("terminal result = %#v", terminal)
			}
			// A held word can make the thought arrive in more than one event.
			thoughts := thought.String()
			if live != (thoughts == "thinking about [REDACTED]" && diffs == 1 && rawInputs == 1) {
				t.Fatalf("live=%v carried thought=%q diffs=%d rawInputs=%d", live, thoughts, diffs, rawInputs)
			}
		})
	}
}

func TestMapACPUpdateSendsLiveFieldsOnlyWhenAsked(t *testing.T) {
	thought := &acp.SessionNotification{Update: json.RawMessage(`{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"considering"}}`)}
	if update, _, ok, err := mapACPUpdate(thought); err != nil || ok || update != nil {
		t.Fatalf("non-live thought = %#v ok=%v err=%v, want dropped", update, ok, err)
	}
	update, text, ok, err := mapACPUpdateWithOptions(thought, acpUpdateOptions{live: true})
	if err != nil || !ok || text != "" || update.AssistantThought == nil || update.AssistantThought.Text != "considering" {
		t.Fatalf("live thought = %#v text=%q ok=%v err=%v", update, text, ok, err)
	}
}

func TestMapACPToolUpdateSendsLiveFieldsOnlyWhenAsked(t *testing.T) {
	tool := &acp.SessionNotification{Update: json.RawMessage(`{"sessionUpdate":"tool_call","toolCallId":"t1","name":"edit_file","title":"Edit","kind":"edit",
		"rawInput":{"path":"/w/a.go"},
		"content":[{"type":"content","content":{"type":"text","text":"ok"}},{"type":"diff","path":"/w/a.go","oldText":"a","newText":"b"}]}`)}
	plain, _, _, err := mapACPUpdate(tool)
	if err != nil || !plain.ToolCall.ContentOmitted || plain.ToolCall.Name != "" || plain.ToolCall.RawInput != nil || plain.ToolCall.Diffs != nil {
		t.Fatalf("non-live tool update = %#v err=%v, want today's mapping", plain.ToolCall, err)
	}
	options := acpUpdateOptions{live: true, rawInputs: map[string]string{}, workspaceRoot: "/w"}
	live, _, _, err := mapACPUpdateWithOptions(tool, options)
	if err != nil {
		t.Fatal(err)
	}
	call := live.ToolCall
	if call.Name != "edit_file" || string(call.RawInput) != `{"path":"/w/a.go"}` || len(call.Diffs) != 1 || call.Diffs[0].Path != "a.go" ||
		call.Diffs[0].OldText == nil || *call.Diffs[0].OldText != "a" || call.Diffs[0].NewText != "b" ||
		len(call.Content) != 1 || call.ContentOmitted || !call.ContentReplace {
		t.Fatalf("live tool update = %#v", call)
	}
	if err := call.Validate(); err != nil {
		t.Fatalf("live tool update is invalid: %v", err)
	}

	repeat := &acp.SessionNotification{Update: json.RawMessage(`{"sessionUpdate":"tool_call_update","toolCallId":"t1","rawInput":{"path":"/w/a.go"}}`)}
	if update, _, ok, err := mapACPUpdateWithOptions(repeat, options); err != nil || ok {
		t.Fatalf("a repeated raw input was resent: %#v ok=%v err=%v", update, ok, err)
	}
	changed := &acp.SessionNotification{Update: json.RawMessage(`{"sessionUpdate":"tool_call_update","toolCallId":"t1","rawInput":{"path":"/w/b.go"}}`)}
	if update, _, ok, err := mapACPUpdateWithOptions(changed, options); err != nil || !ok || string(update.ToolCall.RawInput) != `{"path":"/w/b.go"}` {
		t.Fatalf("a changed raw input was not sent: %#v ok=%v err=%v", update, ok, err)
	}
}

func TestMapACPLiveContentOmitsTerminalSnapshotsAndEditsItCannotSend(t *testing.T) {
	content, err := mapACPToolCallLiveContent(json.RawMessage(`[{"type":"terminal","terminalId":"term-1"}]`), "/w")
	if err != nil || !content.contentOmitted || content.blocks != nil || content.diffs != nil {
		t.Fatalf("terminal content = %#v err = %v, want the snapshot omitted", content, err)
	}
	text := `{"type":"content","content":{"type":"text","text":"kept"}}`
	big := strings.Repeat("x", harnessv2.MaxToolCallDiffBytes)
	for name, diff := range map[string]string{
		"outside":   `{"type":"diff","path":"/etc/passwd","newText":"y"}`,
		"dot-dot":   `{"type":"diff","path":"/w/../etc/passwd","newText":"y"}`,
		"empty":     `{"type":"diff","path":"","newText":"y"}`,
		"root":      `{"type":"diff","path":"/w","newText":"y"}`,
		"oversized": `{"type":"diff","path":"/w/a","oldText":"` + big + `","newText":"y"}`,
	} {
		t.Run(name, func(t *testing.T) {
			content, err := mapACPToolCallLiveContent(json.RawMessage(`[`+text+`,`+diff+`]`), "/w")
			if err != nil || content.contentOmitted || !content.diffsOmitted || content.diffs != nil ||
				len(content.blocks) != 1 || content.blocks[0].Text != "kept" {
				t.Fatalf("content = %#v err = %v, want the edit omitted and the text kept", content, err)
			}
		})
	}
	content, err = mapACPToolCallLiveContent(json.RawMessage(`[{"type":"diff","path":"/w/pkg/./new.go","newText":"package w\n"},
		{"type":"diff","path":"docs/a.md","newText":"a"}]`), "/w/")
	if err != nil || content.diffsOmitted || len(content.diffs) != 2 || content.diffs[0].Path != "pkg/new.go" ||
		content.diffs[0].OldText != nil || content.diffs[1].Path != "docs/a.md" {
		t.Fatalf("workspace edits = %#v err = %v", content, err)
	}
}

func newLiveTestPrompt(t *testing.T, credentials ...string) (*Server, *sessionState, *promptState) {
	t.Helper()
	server, cfg, _ := newTestServer(t, "immediate")
	fence := cfg.Fence
	fence.RuntimeSessionUID = "live-session-uid"
	fence.RuntimeSessionGeneration = 1
	redactor := newCredentialRedactor(credentials...)
	prompt := &promptState{
		request: testStartPromptRequest(t, cfg, fence), live: true, redactor: redactor,
		assistantRedaction: redactor.stream(), thoughtRedaction: redactor.stream(), rawInputs: map[string]string{},
	}
	state := &sessionState{descriptor: harnessv2.RuntimeSessionDescriptor{
		RuntimeSessionUID: fence.RuntimeSessionUID, Generation: fence.RuntimeSessionGeneration,
	}}
	return server, state, prompt
}

func liveTestUpdate(t *testing.T, at time.Time, update map[string]any) acp.PromptEvent {
	t.Helper()
	raw, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	return acp.PromptEvent{Type: acp.PromptEventUpdate, Timestamp: at, Update: &acp.SessionNotification{SessionID: "provider-session", Update: raw}}
}

func TestInterleavedUpdatesDoNotReleaseHeldText(t *testing.T) {
	server, state, prompt := newLiveTestPrompt(t, testLiveCredential)
	now := time.Now().UTC()
	chunk := func(text string) map[string]any {
		return map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}}
	}
	var emitted []string
	for i, update := range []map[string]any{
		chunk("key " + testLiveCredential[:12]),
		{"sessionUpdate": "session_info_update", "title": "ignored"},
		{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "Run", "kind": "execute", "status": "in_progress"},
		{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "aside"}},
		chunk(testLiveCredential[12:] + " end"),
	} {
		events, err := server.mapRuntimeEvents(state, prompt, liveTestUpdate(t, now.Add(time.Duration(i)*time.Millisecond), update))
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Update.AssistantMessage != nil {
				emitted = append(emitted, event.Update.AssistantMessage.Text)
			}
		}
	}
	for _, event := range server.flushTextRedaction(state, prompt) {
		if event.Update.AssistantMessage != nil {
			emitted = append(emitted, event.Update.AssistantMessage.Text)
		}
	}
	if joined := strings.Join(emitted, ""); joined != "key [REDACTED] end" {
		t.Fatalf("emitted %q", emitted)
	}
}

func TestHeldTextAtStreamEndKeepsTheLastEventTime(t *testing.T) {
	server, state, prompt := newLiveTestPrompt(t, testLiveCredential)
	at := time.Now().UTC().Add(-time.Minute)
	events, err := server.mapRuntimeEvents(state, prompt, liveTestUpdate(t, at, map[string]any{
		"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "ends with " + testLiveCredential[:3]},
	}))
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %#v err = %v", events, err)
	}
	held := server.flushTextRedaction(state, prompt)
	if len(held) != 1 || !held[0].Identity.Timestamp.Equal(at) || held[0].Identity.Sequence != events[0].Identity.Sequence+1 {
		t.Fatalf("held flush = %#v, want the last event's time and the next sequence", held)
	}
}

func TestToolTitlesArePatternRedactedBeforeBounding(t *testing.T) {
	server, state, prompt := newLiveTestPrompt(t, testLiveCredential)
	events, err := server.mapRuntimeEvents(state, prompt, liveTestUpdate(t, time.Now().UTC(), map[string]any{
		"sessionUpdate": "tool_call", "toolCallId": "t1", "kind": "execute",
		"title": "curl -H 'Authorization: Bearer ghp_" + strings.Repeat("a", 36) + "' x?token=abc " + testLiveCredential,
	}))
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %#v err = %v", events, err)
	}
	title := events[0].Update.ToolCall.Title
	if strings.Contains(title, "ghp_") || strings.Contains(title, "token=abc") || strings.Contains(title, testLiveCredential) {
		t.Fatalf("title = %q", title)
	}
}

func TestRedactionCoversRawInputKeysDiffPathsAndToolNames(t *testing.T) {
	token := "ghp_" + strings.Repeat("b", 36)
	redactor := newCredentialRedactor(testLiveCredential)
	tool := &harnessv2.ToolCallUpdate{
		ToolCallID: "call", Status: harnessv2.ToolCallStatusPending, Name: "tool " + token,
		Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockResourceLink, URI: "https://example.com/a", MimeType: "text/plain; key=" + token}},
		Diffs:   []harnessv2.ToolCallDiff{{Path: "notes/" + token + ".txt", NewText: "x"}},
	}
	redactor.redactToolCall(tool)
	permission := &harnessv2.PermissionRequestedEvent{Title: "Allow " + token, Options: []harnessv2.PermissionOption{{OptionID: "o", Name: "Allow " + token}}}
	redactor.redactPermission(permission)
	if strings.Contains(permission.Title, token) || strings.Contains(permission.Options[0].Name, token) {
		t.Fatalf("permission = %#v", permission)
	}
	raw := redactor.redactRawInput(json.RawMessage(`{"` + token + `":"value"}`))
	encoded, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), token) || strings.Contains(string(raw), token) {
		t.Fatalf("pattern-recognizable credential survived: tool=%s raw=%s", encoded, raw)
	}
}

func TestDigitOnlyCredentialsAreRedactedInNumericJSON(t *testing.T) {
	digits := "12345678901234567890"
	redactor := newCredentialRedactor(digits)
	raw := redactor.redactRawInput(json.RawMessage(`{"n":` + digits + `,"m":7}`))
	if strings.Contains(string(raw), digits) || !strings.Contains(string(raw), `"m":7`) {
		t.Fatalf("raw input = %s", raw)
	}
	event := harnessv2.Event{Type: harnessv2.EventUpdate, Update: &harnessv2.UpdateEvent{Kind: harnessv2.UpdateToolCall,
		ToolCall: &harnessv2.ToolCallUpdate{ToolCallID: "t", Status: harnessv2.ToolCallStatusPending, RawInput: json.RawMessage(`{"n":` + digits + `}`)}}}
	if err := redactor.scrubEvent(&event); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(event.Update.ToolCall.RawInput), digits) {
		t.Fatalf("scrubbed raw input = %s", event.Update.ToolCall.RawInput)
	}
}
