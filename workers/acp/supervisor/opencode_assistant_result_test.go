package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const assistantResultHelperEnv = "GO_WANT_SUPERVISOR_ASSISTANT_RESULT_HELPER"
const assistantResultTestLimit = 4096
const assistantResultThoughtFixture = "synthetic reasoning must never appear in the NDJSON stream"

// These are synthetic ACP wire fixtures, not provider responses or credentials.
// The subprocess receives them as the prompt's text, then emits real ACP updates.
type assistantResultTestChunk struct {
	// Byte encoding preserves malformed Unicode until the helper emits the raw
	// JSON identity on its ACP stream; the enclosing test prompt stays valid.
	RawMessageID []byte `json:"rawMessageId,omitempty"`
	RawUpdate    []byte `json:"rawUpdate,omitempty"`
	MessageID    string `json:"messageId,omitempty"`
	Text         string `json:"text"`
	Thought      bool   `json:"thought,omitempty"`
	Phase        string `json:"phase,omitempty"`
}

func TestSupervisorOpenCodeAssistantResultHTTP(t *testing.T) {
	text := func(id, value string) assistantResultTestChunk {
		return assistantResultTestChunk{MessageID: id, Text: value}
	}
	thought := func(id string) assistantResultTestChunk {
		return assistantResultTestChunk{MessageID: id, Text: assistantResultThoughtFixture, Thought: true}
	}
	messages := func(count int) []assistantResultTestChunk {
		chunks := make([]assistantResultTestChunk, count)
		for i := range chunks {
			chunks[i] = text(fmt.Sprintf("message-%03d", i), fmt.Sprintf("text-%03d", i))
		}
		return chunks
	}
	emptyMessages := func(count int) []assistantResultTestChunk {
		chunks := make([]assistantResultTestChunk, count)
		for i := range chunks {
			chunks[i] = text(fmt.Sprintf("empty-%03d", i), "")
		}
		return chunks
	}
	thoughtsThenFinal := func(count int) []assistantResultTestChunk {
		chunks := make([]assistantResultTestChunk, 0, count+1)
		for i := range count {
			chunks = append(chunks, thought(fmt.Sprintf("thought-%03d", i)))
		}
		return append(chunks, text("answer", "Visible final."))
	}
	for _, test := range []struct {
		name         string
		chunks       []assistantResultTestChunk
		want         string
		fail         bool
		failCode     string
		rejectStream bool
	}{
		{
			name: "compaction_and_progress_do_not_prefix_final",
			chunks: []assistantResultTestChunk{
				text("summary", "Compacted context: prior work.\n"),
				text("progress", "Checking sources now.\n"),
				text("answer", "Only the final answer."),
			},
			want: "Only the final answer.",
		},
		{
			name: "same_message_chunks_concatenate",
			chunks: []assistantResultTestChunk{
				text("answer", "One "), text("answer", "complete "), text("answer", "answer."),
			},
			want: "One complete answer.",
		},
		{
			name: "late_older_chunks_do_not_reselect_previous_message",
			chunks: []assistantResultTestChunk{
				text("summary", "Earlier summary. "), text("answer", "Final "),
				text("summary", "Late older chunk. "), text("answer", "answer."),
				text("summary", "Even later older chunk."),
			},
			want: "Final answer.",
		},
		{
			name:   "empty_first_observation_keeps_late_older_text_old",
			chunks: []assistantResultTestChunk{text("old", ""), text("answer", "Current answer."), text("old", "Late old text.")},
			want:   "Current answer.",
		},
		{
			name:   "newer_empty_message_does_not_resurrect_old_note",
			chunks: []assistantResultTestChunk{text("old", "Old work note."), text("new", "")},
			want:   "Prompt completed without textual output.",
		},
		{
			name:   "empty_same_id_preserves_nonempty_fragments",
			chunks: []assistantResultTestChunk{text("answer", "One "), text("answer", ""), text("answer", "answer.")},
			want:   "One answer.",
		},
		{
			name:         "empty_anonymous_update_after_named_message_is_rejected",
			chunks:       []assistantResultTestChunk{text("answer", "Named."), text("", "")},
			rejectStream: true, fail: true,
		},
		{name: "anonymous_empty_only_keeps_legacy_placeholder", chunks: []assistantResultTestChunk{text("", "")}, want: "Prompt completed without textual output."},
		{name: "256_empty_message_identities_are_observed", chunks: emptyMessages(256), want: "Prompt completed without textual output."},
		{name: "257_empty_message_identities_are_rejected", chunks: emptyMessages(257), rejectStream: true, fail: true},
		{name: "512_byte_empty_identity_is_accepted", chunks: []assistantResultTestChunk{text(strings.Repeat("m", 512), "")}, want: "Prompt completed without textual output."},
		{name: "513_byte_empty_identity_is_rejected", chunks: []assistantResultTestChunk{text(strings.Repeat("m", 513), "")}, rejectStream: true, fail: true},
		{
			name: "selection_uses_identity_not_headings",
			chunks: []assistantResultTestChunk{
				text("old", "Final answer: this is actually an earlier message.\n"),
				text("new", "Work note: this is the actual last message."),
			},
			want: "Work note: this is the actual last message.",
		},
		{
			name:   "anonymous_only_legacy_concatenates",
			chunks: []assistantResultTestChunk{text("", "Legacy "), text("", "answer.")},
			want:   "Legacy answer.",
		},
		{
			name:   "anonymous_only_whitespace_preserves_legacy_fallback",
			chunks: []assistantResultTestChunk{text("", " \t\n")},
			want:   "Prompt completed without textual output.",
		},
		{
			name:   "named_answer_supersedes_earlier_anonymous_progress",
			chunks: []assistantResultTestChunk{text("", "Old anonymous progress. "), text("answer", "Named answer.")},
			want:   "Named answer.",
		},
		{
			name:         "missing_identity_after_named_message_fails_closed",
			chunks:       []assistantResultTestChunk{text("answer", "Named answer. "), text("", "Unattributed tail.")},
			rejectStream: true, fail: true,
		},
		{
			name:   "reasoning_then_visible_text_for_same_new_id",
			chunks: []assistantResultTestChunk{text("summary", "Earlier summary. "), thought("answer"), text("answer", "Visible final.")},
			want:   "Visible final.",
		},
		{
			name:   "late_older_reasoning_does_not_reselect_summary",
			chunks: []assistantResultTestChunk{text("summary", "Earlier summary. "), text("answer", "Visible final."), thought("summary")},
			want:   "Visible final.",
		},
		{
			name:   "late_text_for_id_first_seen_in_reasoning_stays_old",
			chunks: []assistantResultTestChunk{thought("old"), text("answer", "Visible final."), text("old", "Late older text.")},
			want:   "Visible final.",
		},
		{
			name:         "anonymous_reasoning_after_named_message_fails_closed",
			chunks:       []assistantResultTestChunk{text("answer", "Named answer."), thought("")},
			rejectStream: true, fail: true,
		},
		{
			name:   "newer_reasoning_only_message_cannot_complete_with_summary",
			chunks: []assistantResultTestChunk{text("summary", "Must not become the final answer."), thought("answer")},
			want:   "Prompt completed without textual output.",
		},
		{
			name:   "newer_whitespace_only_message_cannot_complete_with_summary",
			chunks: []assistantResultTestChunk{text("summary", "Must not become the final answer."), text("answer", " \n\t ")},
			want:   "Prompt completed without textual output.",
		},
		{
			name:   "oversized_whitespace_retains_too_large_classification",
			chunks: []assistantResultTestChunk{text("summary", "Earlier summary."), text("answer", strings.Repeat(" ", assistantResultTestLimit+1))},
			fail:   true, failCode: "terminal_result_too_large",
		},
		{
			name:   "oversized_earlier_message_does_not_poison_bounded_final",
			chunks: []assistantResultTestChunk{text("summary", strings.Repeat("x", assistantResultTestLimit+1)), text("answer", "Bounded final.")},
			want:   "Bounded final.",
		},
		{
			name:   "oversized_late_older_chunk_does_not_poison_bounded_final",
			chunks: []assistantResultTestChunk{text("summary", "Earlier summary."), text("answer", "Bounded final."), text("summary", strings.Repeat("x", assistantResultTestLimit+1))},
			want:   "Bounded final.",
		},
		{
			name:   "selected_message_aggregate_overflow_fails_closed",
			chunks: []assistantResultTestChunk{text("answer", strings.Repeat("x", assistantResultTestLimit/2)), text("answer", strings.Repeat("y", assistantResultTestLimit/2+1))},
			fail:   true, failCode: "terminal_result_too_large",
		},
		{name: "256_distinct_ids_accepted", chunks: messages(256), want: "text-255"},
		{name: "256_distinct_text_and_thought_ids_accepted", chunks: thoughtsThenFinal(255), want: "Visible final."},
		{name: "257_distinct_text_and_thought_ids_fail_closed", chunks: thoughtsThenFinal(256), rejectStream: true, fail: true},
		{name: "257_distinct_ids_fail_closed", chunks: messages(257), rejectStream: true, fail: true},
		{name: "512_byte_id_accepted", chunks: []assistantResultTestChunk{text(strings.Repeat("m", 512), "Bounded identity.")}, want: "Bounded identity."},
		{name: "513_byte_id_fails_closed", chunks: []assistantResultTestChunk{text(strings.Repeat("m", 513), "Do not accept.")}, rejectStream: true, fail: true},
		{name: "512_byte_utf8_id_accepted", chunks: []assistantResultTestChunk{text(strings.Repeat("é", 256), "UTF-8 identity.")}, want: "UTF-8 identity."},
		{name: "utf8_id_limit_counts_bytes_not_runes", chunks: []assistantResultTestChunk{text(strings.Repeat("é", 257), "Do not accept.")}, rejectStream: true, fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAssistantResultHTTPFixture(t, providerKindOpencode)
			if test.rejectStream {
				request, raw := fixture.startPrompt(t, "prompt-1", test.chunks)
				fixture.assertRejectedStreamAndReplay(t, request, raw, test.chunks)
				return
			}
			_, events, raw := fixture.prompt(t, "prompt-1", test.chunks)
			assertAssistantResultThoughtsAbsent(t, raw, test.chunks)
			terminal := assistantResultTerminal(t, events)
			if test.fail {
				if terminal.Type != harnessv2.EventFailed || terminal.Failed == nil || terminal.Completed != nil || terminal.Failed.Retryable {
					t.Fatalf("terminal type = %q, want non-retryable failed event without a completed result", terminal.Type)
				}
				if test.failCode != "" && terminal.Failed.Code != test.failCode {
					t.Fatalf("failure code = %q, want %q", terminal.Failed.Code, test.failCode)
				}
				return
			}
			assertAssistantResultCompleted(t, terminal, test.want)
			assertAssistantResultVisibleStream(t, events, test.chunks)
		})
	}
}

func TestSupervisorOpenCodeAssistantResultReusedIDsAcrossPromptsHTTP(t *testing.T) {
	if runtime.GOOS != linuxGOOS {
		t.Skip("same-session continuation requires the production Linux workspace-freeze proof")
	}
	fixture := newAssistantResultHTTPFixture(t, providerKindOpencode)
	firstChunks := []assistantResultTestChunk{
		{MessageID: "shared-a", Text: "First summary. "},
		{MessageID: "shared-b", Text: "First final."},
	}
	first, events, _ := fixture.prompt(t, "prompt-1", firstChunks)
	assertAssistantResultCompleted(t, assistantResultTerminal(t, events), "First final.")
	fixture.validateWorkspace(t, first)

	// Reverse the reused identities: a session-global seen set or selected ID
	// would incorrectly keep shared-b or append the previous answer here.
	secondChunks := []assistantResultTestChunk{
		{MessageID: "shared-b", Text: "Second summary. "},
		{MessageID: "shared-a", Text: "Second "},
		{MessageID: "shared-a", Text: "final."},
	}
	_, events, _ = fixture.prompt(t, "prompt-2", secondChunks)
	assertAssistantResultCompleted(t, assistantResultTerminal(t, events), "Second final.")
	assertAssistantResultVisibleStream(t, events, secondChunks)
}

func TestSupervisorOtherProvidersKeepAssistantResultBehaviorHTTP(t *testing.T) {
	for _, test := range []struct {
		name, provider, want string
		chunks               []assistantResultTestChunk
	}{
		{
			name: "codex_named_messages_still_concatenate", provider: providerKindCodex,
			chunks: []assistantResultTestChunk{{MessageID: "old", Text: "Progress. "}, {MessageID: "new", Text: "Answer."}},
			want:   "Progress. Answer.",
		},
		{
			name: "claude_named_messages_still_concatenate", provider: providerKindClaude,
			chunks: []assistantResultTestChunk{{MessageID: "old", Text: "Progress. "}, {MessageID: "new", Text: "Answer."}},
			want:   "Progress. Answer.",
		},
		{
			name: "codex_phase_still_selects_final_answer", provider: providerKindCodex,
			chunks: []assistantResultTestChunk{
				{MessageID: "old", Text: strings.Repeat("x", assistantResultTestLimit+1), Phase: "commentary"},
				{MessageID: "new", Text: "Codex final.", Phase: "final_answer"},
			},
			want: "Codex final.",
		},
		{
			name: "opencode_identity_constraints_do_not_apply_to_codex", provider: providerKindCodex,
			chunks: []assistantResultTestChunk{{MessageID: strings.Repeat("m", 513), Text: "Named. "}, {Text: "Anonymous."}},
			want:   "Named. Anonymous.",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAssistantResultHTTPFixture(t, test.provider)
			_, events, _ := fixture.prompt(t, "prompt-1", test.chunks)
			assertAssistantResultCompleted(t, assistantResultTerminal(t, events), test.want)
			assertAssistantResultVisibleStream(t, events, test.chunks)
		})
	}
}

type assistantResultHTTPFixture struct {
	cfg        Config
	create     harnessv2.CreateRuntimeSessionRequest
	created    harnessv2.CreateRuntimeSessionResponse
	server     *httptest.Server
	lastPrompt *harnessv2.StartPromptRequest
}

func newAssistantResultHTTPFixture(t *testing.T, provider string) *assistantResultHTTPFixture {
	t.Helper()
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("requires an isolated root Linux process-test lane with the production ACP exec helper")
	}
	if _, err := os.Stat(acp.DefaultExecHelperCommand); err != nil {
		t.Fatalf("provision the production ACP exec helper in the isolated test lane: %v", err)
	}
	// Authentication stays in the existing test harness; no credential literals
	// are embedded in the new ACP fixture. The unused upstream gets a fresh nonce.
	nonce, err := harnessv2.NewCapabilityNonce()
	if err != nil {
		t.Fatal(err)
	}
	cfg, profile := newTestConfigWithUpstream(t, "immediate", "http://127.0.0.1:1", nonce)
	profile.ProviderKind = provider
	profile.AgentConfigurationDigest, err = harnessv2.CanonicalAgentConfigurationDigest(*testServerAgentConfiguration(profile))
	if err != nil {
		t.Fatal(err)
	}
	profileDigest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Fence.RuntimeProfileDigest = profileDigest
	cfg.Capabilities.RuntimeProfileDigest = profileDigest
	cfg.Capabilities.Provider.ProviderKinds = []string{provider}
	cfg.Capabilities.Limits.MaxTerminalResultBytes = assistantResultTestLimit
	// The ID-limit tests exercise selection bounds, not unrelated queue/rate limits.
	cfg.Capabilities.Limits.MaxBufferedEvents = 1024
	cfg.Provider.Kind = provider
	cfg.Provider.Command = assistantResultHelperExecutable(t)
	cfg.Provider.Args = []string{"-test.run=^TestSupervisorAssistantResultACPHelper$"}
	cfg.Provider.Environment = map[string]string{assistantResultHelperEnv: "1"}
	cfg.ProviderProxy.ProviderKind = provider
	cfg.ProviderProxy.ModelOutputLimit = assistantResultTestLimit
	supervisor, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := supervisor.Close(ctx); err != nil {
			t.Errorf("close isolated supervisor: %v", err)
		}
	})
	httpServer := httptest.NewServer(supervisor.Handler())
	httpServer.Client().Timeout = 20 * time.Second
	httpServer.Client().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.Cleanup(httpServer.Close)
	fixture := &assistantResultHTTPFixture{cfg: cfg, create: testCreateSessionRequest(t, cfg, profile), server: httpServer}
	raw := fixture.mutate(t, "/v2/runtime-sessions/session-1", fixture.create, http.StatusCreated)
	if err := json.Unmarshal(raw, &fixture.created); err != nil {
		t.Fatal(err)
	}
	if err := fixture.created.ValidateFor(fixture.create); err != nil {
		t.Fatalf("create response: %v", err)
	}
	// Use the public deletion/cleanup receipt before closing the server. Failed
	// prompts may already be undergoing automatic cleanup; do not race that
	// work with a second filesystem teardown through Supervisor.Close.
	t.Cleanup(func() { fixture.deleteSession(t) })
	return fixture
}

// The production launcher drops to the fixture's distinct non-root UID. Go's
// temporary build directory is private to the root test runner, so give the
// child a byte-for-byte copy of this synthetic ACP executable in a test-owned,
// searchable directory. This does not replace or bypass the real exec helper.
func assistantResultHelperExecutable(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "orka-assistant-result-bin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove synthetic ACP executable: %v", err)
		}
	})
	if err := os.Chmod(dir, 0o711); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close() //nolint:errcheck
	path := filepath.Join(dir, "synthetic-acp-provider")
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy synthetic ACP executable: copy=%v close=%v", copyErr, closeErr)
	}
	if err := os.Chmod(path, 0o555); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f *assistantResultHTTPFixture) exchange(t *testing.T, ctx context.Context, method, route string, body any) (int, []byte) {
	t.Helper()
	signed := mutationHTTPRequest(t, method, route, body, f.cfg)
	request, err := http.NewRequestWithContext(ctx, method, f.server.URL+route, signed.Body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header = signed.Header.Clone()
	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatalf("supervisor HTTP request: %v", err)
	}
	defer response.Body.Close() //nolint:errcheck
	const maxResponseBytes = 4 << 20
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		t.Fatalf("bounded response read failed: bytes=%d error=%v", len(raw), err)
	}
	return response.StatusCode, raw
}

func (f *assistantResultHTTPFixture) mutate(t *testing.T, route string, body any, status int) []byte {
	t.Helper()
	return f.mutateContext(t, t.Context(), route, body, status)
}

func (f *assistantResultHTTPFixture) mutateContext(t *testing.T, ctx context.Context, route string, body any, status int) []byte {
	t.Helper()
	actual, raw := f.exchange(t, ctx, http.MethodPut, route, body)
	if actual != status {
		t.Fatalf("HTTP status = %d, want %d for %s", actual, status, route)
	}
	return raw
}

func (f *assistantResultHTTPFixture) deleteSession(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	metadata := f.create.Metadata
	if f.lastPrompt != nil {
		metadata = f.lastPrompt.Metadata
		status, raw := f.exchange(t, ctx, http.MethodPut, "/v2/runtime-sessions/session-1/prompts/"+string(metadata.PromptID), *f.lastPrompt)
		if status == http.StatusNotFound {
			return // a failed prompt's verified automatic cleanup already finished
		}
		if status == http.StatusOK {
			var admission harnessv2.PromptAdmissionResponse
			if err := json.Unmarshal(raw, &admission); err != nil {
				t.Fatal(err)
			}
			if err := admission.Validate(); err != nil || admission.Settlement == nil {
				t.Fatalf("cleanup requires a settled prompt: %v", err)
			}
			if admission.Settlement.Outcome == harnessv2.PromptOutcomeSucceeded {
				// Completed prompts remain in validation until their read-only
				// workspace is proved unchanged. Follow that public lifecycle;
				// deletion must not bypass the validation fence.
				f.validateWorkspaceContext(t, ctx, *f.lastPrompt)
			}
		} else if status != http.StatusConflict {
			t.Fatalf("cleanup settlement status = %d", status)
		}
	}
	metadata.OperationID = "delete-fixture-session"
	metadata.RequestDigest = ""
	request := harnessv2.DeleteRuntimeSessionRequest{Protocol: harnessv2.ProtocolVersion, Metadata: metadata, Reason: "isolated fixture complete"}
	sealRequest(t, &request.Metadata.RequestDigest, request)
	for ctx.Err() == nil {
		status, raw := f.exchange(t, ctx, http.MethodDelete, "/v2/runtime-sessions/session-1", request)
		if status == http.StatusNotFound {
			// This exact session was created and validated by this fixture; its
			// failed-prompt reaper may have completed deletion before this call.
			return
		}
		if status == http.StatusOK {
			var deleted harnessv2.DeleteRuntimeSessionResponse
			if err := json.Unmarshal(raw, &deleted); err != nil {
				t.Fatal(err)
			}
			if deleted.Protocol != harnessv2.ProtocolVersion || deleted.State != harnessv2.RuntimeSessionStateDeleted || deleted.Tombstone.RuntimeSessionUID != request.Metadata.Fence.RuntimeSessionUID {
				t.Fatal("public cleanup receipt did not identify the deleted fixture session")
			}
			return
		}
		if status != http.StatusConflict {
			t.Fatalf("public fixture cleanup status = %d", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("public fixture cleanup did not settle within its bound")
}

func (f *assistantResultHTTPFixture) startPrompt(t *testing.T, id harnessv2.PromptID, chunks []assistantResultTestChunk) (harnessv2.StartPromptRequest, []byte) {
	t.Helper()
	encoded, err := json.Marshal(chunks)
	if err != nil {
		t.Fatal(err)
	}
	request := testStartPromptRequest(t, f.cfg, f.create.Metadata.Fence)
	request.Metadata.OperationID = harnessv2.OperationID("start-" + string(id))
	request.Metadata.PromptID = id
	request.MCPAuthorization.PromptID = id
	request.Input.Content = []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: string(encoded)}}
	request.Metadata.RequestDigest = ""
	sealRequest(t, &request.Metadata.RequestDigest, request)
	f.lastPrompt = &request
	raw := f.mutate(t, "/v2/runtime-sessions/session-1/prompts/"+string(id), request, http.StatusOK)
	return request, raw
}

func (f *assistantResultHTTPFixture) prompt(t *testing.T, id harnessv2.PromptID, chunks []assistantResultTestChunk) (harnessv2.StartPromptRequest, []harnessv2.Event, []byte) {
	t.Helper()
	request, raw := f.startPrompt(t, id, chunks)
	decoder, err := harnessv2.NewEventDecoder(bytes.NewReader(raw), eventLimits(f.cfg.Capabilities.Limits), harnessv2.EventExpectationFromMetadata(request.Metadata))
	if err != nil {
		t.Fatal(err)
	}
	events, err := decoder.DecodeAll()
	if err != nil {
		t.Fatalf("decode HTTP NDJSON: %v", err)
	}
	return request, events, raw
}

func (f *assistantResultHTTPFixture) assertRejectedStreamAndReplay(t *testing.T, request harnessv2.StartPromptRequest, raw []byte, chunks []assistantResultTestChunk) {
	t.Helper()
	assertAssistantResultThoughtsAbsent(t, raw, chunks)
	decoder, err := harnessv2.NewEventDecoder(bytes.NewReader(raw), eventLimits(f.cfg.Capabilities.Limits), harnessv2.EventExpectationFromMetadata(request.Metadata))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decoder.DecodeAll(); !errors.Is(err, harnessv2.ErrMissingTerminalEvent) {
		t.Fatalf("invalid identity stream must reject completion, got %v", err)
	}
	decoder, err = harnessv2.NewEventDecoder(bytes.NewReader(raw), eventLimits(f.cfg.Capabilities.Limits), harnessv2.EventExpectationFromMetadata(request.Metadata))
	if err != nil {
		t.Fatal(err)
	}
	for {
		event, err := decoder.Decode()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("unexpected invalid pre-terminal event: %v", err)
		}
		if event.Type == harnessv2.EventCompleted {
			t.Fatal("rejected identity stream contained a successful result")
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, replay := f.exchange(t, t.Context(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/"+string(request.Metadata.PromptID), request)
		if status == http.StatusNotFound {
			return
		} // verified session already reaped
		if status == http.StatusConflict {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if status != http.StatusOK {
			t.Fatalf("rejected-prompt replay status = %d", status)
		}
		var admission harnessv2.PromptAdmissionResponse
		if err := json.Unmarshal(replay, &admission); err != nil {
			t.Fatal(err)
		}
		if err := admission.Validate(); err != nil || admission.Settlement == nil {
			t.Fatalf("rejected prompt has no valid settlement: %v", err)
		}
		if admission.Settlement.Outcome == harnessv2.PromptOutcomeSucceeded || admission.Settlement.TerminalEvent == harnessv2.EventCompleted {
			t.Fatal("rejected identity prompt replayed a successful settlement")
		}
		return
	}
	t.Fatal("rejected prompt replay did not settle within its bound")
}

func (f *assistantResultHTTPFixture) validateWorkspace(t *testing.T, prompt harnessv2.StartPromptRequest) {
	t.Helper()
	f.validateWorkspaceContext(t, t.Context(), prompt)
}

func (f *assistantResultHTTPFixture) validateWorkspaceContext(t *testing.T, ctx context.Context, prompt harnessv2.StartPromptRequest) {
	t.Helper()
	// Obtain settlement by the public duplicate-admission contract, then perform
	// real no-change workspace validation. Never reset supervisor state in tests.
	raw := f.mutateContext(t, ctx, "/v2/runtime-sessions/session-1/prompts/"+string(prompt.Metadata.PromptID), prompt, http.StatusOK)
	var admission harnessv2.PromptAdmissionResponse
	if err := json.Unmarshal(raw, &admission); err != nil {
		t.Fatal(err)
	}
	if err := admission.Validate(); err != nil || admission.Settlement == nil {
		t.Fatalf("settled admission is unavailable: %v", err)
	}
	digest, err := harnessv2.CanonicalPromptSettlementDigest(*admission.Settlement)
	if err != nil {
		t.Fatal(err)
	}
	request := harnessv2.CreateWorkspaceDeltaRequest{
		Protocol: harnessv2.ProtocolVersion, Metadata: prompt.Metadata, DeltaID: harnessv2.WorkspaceDeltaID("validated-" + string(prompt.Metadata.PromptID)),
		Intent: f.create.Workspace.Intent, VerifiedBaseline: f.create.Workspace.Baseline,
		PromptSettlementDigest: digest, Limits: harnessv2.WorkspaceDeltaLimits{MaxBytes: 1 << 20, MaxEntries: 100},
	}
	request.Metadata.OperationID = harnessv2.OperationID("validate-" + string(prompt.Metadata.PromptID))
	request.Metadata.RequestDigest = ""
	sealRequest(t, &request.Metadata.RequestDigest, request)
	raw = f.mutateContext(t, ctx, "/v2/runtime-sessions/session-1/workspace-deltas/"+string(request.DeltaID), request, http.StatusOK)
	var delta harnessv2.CreateWorkspaceDeltaResponse
	if err := json.Unmarshal(raw, &delta); err != nil {
		t.Fatal(err)
	}
	if err := delta.ValidateFor(request); err != nil || delta.Delta.State != harnessv2.WorkspaceDeltaNoChange {
		t.Fatalf("workspace validation did not permit safe continuation: state=%q error=%v", delta.Delta.State, err)
	}
}

func assistantResultTerminal(t *testing.T, events []harnessv2.Event) harnessv2.Event {
	t.Helper()
	if len(events) < 2 || events[0].Type != harnessv2.EventAccepted {
		t.Fatal("stream does not begin with an accepted prompt")
	}
	terminals := 0
	for i, event := range events {
		if event.Identity.Sequence != uint64(i+1) {
			t.Fatalf("event %d has non-contiguous sequence %d", i, event.Identity.Sequence)
		}
		if event.Type.IsTerminal() {
			terminals++
			if i != len(events)-1 {
				t.Fatal("event follows terminal settlement")
			}
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal event count = %d, want 1", terminals)
	}
	return events[len(events)-1]
}

func assertAssistantResultCompleted(t *testing.T, terminal harnessv2.Event, want string) {
	t.Helper()
	if terminal.Type != harnessv2.EventCompleted || terminal.Completed == nil || len(terminal.Completed.Result.Content) != 1 {
		t.Fatalf("terminal type = %q, want one completed text result", terminal.Type)
	}
	content := terminal.Completed.Result.Content[0]
	if content.Type != harnessv2.ContentBlockText || content.Text != want {
		t.Fatalf("terminal result differs: got %q, want %q", content.Text, want)
	}
}

func assertAssistantResultVisibleStream(t *testing.T, events []harnessv2.Event, chunks []assistantResultTestChunk) {
	t.Helper()
	var got, want strings.Builder
	for _, event := range events {
		if event.Type == harnessv2.EventUpdate && event.Update != nil && event.Update.AssistantMessage != nil {
			got.WriteString(event.Update.AssistantMessage.Text)
		}
	}
	for _, chunk := range chunks {
		if !chunk.Thought {
			want.WriteString(chunk.Text)
		}
	}
	if got.String() != want.String() {
		t.Fatalf("visible delta stream changed: got %d bytes, want %d exact bytes", got.Len(), want.Len())
	}
}

func assertAssistantResultThoughtsAbsent(t *testing.T, raw []byte, chunks []assistantResultTestChunk) {
	t.Helper()
	for _, chunk := range chunks {
		if chunk.Thought && bytes.Contains(raw, []byte(chunk.Text)) {
			t.Fatal("synthetic reasoning leaked into the HTTP NDJSON response")
		}
	}
}

func TestSupervisorAssistantResultACPHelper(t *testing.T) {
	if os.Getenv(assistantResultHelperEnv) != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	writer := bufio.NewWriter(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			os.Exit(2)
		}
		switch request.Method {
		case acp.MethodInitialize:
			writeHelperMessage(writer, map[string]any{
				testJSONRPCKey: testJSONRPCVersion, "id": rawID(request.ID),
				"result": map[string]any{"protocolVersion": acp.ProtocolVersion, "agentCapabilities": map[string]any{"mcpCapabilities": map[string]any{"http": true}}},
			})
		case acp.MethodSessionNew:
			writeHelperMessage(writer, map[string]any{testJSONRPCKey: testJSONRPCVersion, "id": rawID(request.ID), "result": map[string]any{"sessionId": "assistant-result-provider-session"}})
		case acp.MethodSessionPrompt:
			var prompt acp.PromptRequest
			if err := json.Unmarshal(request.Params, &prompt); err != nil || len(prompt.Prompt) != 1 || prompt.Prompt[0].Type != "text" {
				os.Exit(2)
			}
			var chunks []assistantResultTestChunk
			if err := json.Unmarshal([]byte(prompt.Prompt[0].Text), &chunks); err != nil {
				os.Exit(2)
			}
			for _, chunk := range chunks {
				if chunk.RawUpdate != nil {
					writeHelperMessage(writer, map[string]any{testJSONRPCKey: testJSONRPCVersion, "method": acp.MethodSessionUpdate, "params": map[string]any{"sessionId": prompt.SessionID, "update": json.RawMessage(chunk.RawUpdate)}})
					continue
				}
				kind := "agent_message_chunk"
				if chunk.Thought {
					kind = "agent_thought_chunk"
				}
				update := map[string]any{"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": chunk.Text}}
				if chunk.RawMessageID != nil {
					update["messageId"] = json.RawMessage(chunk.RawMessageID)
				} else if chunk.MessageID != "" {
					update["messageId"] = chunk.MessageID
				}
				if chunk.Phase != "" {
					update["_meta"] = map[string]any{"codex": map[string]any{"phase": chunk.Phase}}
				}
				writeHelperMessage(writer, map[string]any{testJSONRPCKey: testJSONRPCVersion, "method": acp.MethodSessionUpdate, "params": map[string]any{"sessionId": prompt.SessionID, "update": update}})
			}
			writeHelperMessage(writer, map[string]any{testJSONRPCKey: testJSONRPCVersion, "id": rawID(request.ID), "result": map[string]any{"stopReason": acp.StopReasonEndTurn}})
		}
	}
	if scanner.Err() != nil {
		os.Exit(2)
	}
	// A helper process must not append the Go test runner's PASS line to ACP.
	os.Exit(0)
}

func TestSupervisorOpenCodeAssistantResultMalformedUnicodeHTTP(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  []byte
	}{
		{"invalid UTF8 ff", []byte{'"', 0xff, '"'}},
		{"invalid UTF8 fe", []byte{'"', 0xfe, '"'}},
		{"unpaired high", []byte(`"\ud800"`)},
		{"different unpaired high", []byte(`"\ud801"`)},
		{"unpaired low", []byte(`"\udfff"`)},
		{"high followed by ordinary scalar", []byte(`"\ud800\u0061"`)},
		{"two high surrogates", []byte(`"\ud800\ud801"`)},
		{"high followed by literal escape text", []byte(`"\ud800\\udfff"`)},
	} {
		for _, thought := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/thought=%t", test.name, thought), func(t *testing.T) {
				fixture := newAssistantResultHTTPFixture(t, providerKindOpencode)
				chunks := []assistantResultTestChunk{{MessageID: "earlier", Text: "Earlier message."}, {RawMessageID: test.raw, Text: "Malformed identity candidate.", Thought: thought}}
				request, raw := fixture.startPrompt(t, "prompt-1", chunks)
				fixture.assertRejectedStreamAndReplay(t, request, raw, chunks)
			})
		}
	}
}

func TestSupervisorOpenCodeAssistantResultValidUnicodeRepresentationsHTTP(t *testing.T) {
	for _, test := range []struct{ name, raw, decoded string }{
		{"surrogate pair", `"\ud83d\ude00"`, "😀"},
		{"uppercase surrogate pair", `"\uD83D\uDE00"`, "😀"},
		{"escaped scalar", `"\u00e9"`, "é"},
		{"replacement character", `"\ufffd"`, "�"},
		{"literal escape text", `"\\ud800"`, `\ud800`},
		{"anonymous empty", `""`, ""},
		{"anonymous null", `null`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAssistantResultHTTPFixture(t, providerKindOpencode)
			chunks := []assistantResultTestChunk{{RawMessageID: []byte(test.raw), Text: "First."}, {MessageID: test.decoded, Text: " Second."}}
			_, events, _ := fixture.prompt(t, "prompt-1", chunks)
			assertAssistantResultCompleted(t, assistantResultTerminal(t, events), "First. Second.")
		})
	}
}

func TestSupervisorOpenCodeAssistantResultMalformedIdentityCannotMergeWithReplacementHTTP(t *testing.T) {
	fixture := newAssistantResultHTTPFixture(t, providerKindOpencode)
	chunks := []assistantResultTestChunk{
		{MessageID: "�", Text: "Valid replacement-character identity."},
		{RawMessageID: []byte(`"\ud800"`), Text: "Malformed identity must not be coalesced into it."},
	}
	request, raw := fixture.startPrompt(t, "prompt-1", chunks)
	fixture.assertRejectedStreamAndReplay(t, request, raw, chunks)
}
