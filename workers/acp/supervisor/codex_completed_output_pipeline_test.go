package supervisor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/api"
	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/harness/v2/eventjournal"
	"github.com/orka-agents/orka/internal/store/storetest"
)

// This seam uses the real supervisor event mapper, controller event journal and
// public HTTP handler. Only the adapter notifications and persistence backend
// are fixtures; it does not invoke a native provider or reconstruct recordings.
type codexOutputTestPipeline struct {
	server  *Server
	session *sessionState
	prompt  *promptState
	journal *eventjournal.State
	app     *fiber.App
}

func newCodexOutputTestPipeline(t *testing.T) *codexOutputTestPipeline {
	t.Helper()
	cfg := Config{
		Provider: ProviderProfile{
			Kind: providerKindCodex, AdapterName: "codex-acp-orka-dist",
			AdapterDigest: "sha256:" + acp.CodexACPOrkaDistSHA256,
		},
		Fence: harnessv2.Fence{RuntimeInstanceID: "runtime-1", SupervisorBootID: "boot-1"},
		Capabilities: harnessv2.CapabilitiesResponse{Limits: harnessv2.ProtocolLimits{
			MaxEventLineBytes: harnessv2.DefaultMaxEventLineBytes,
		}},
	}
	pipeline := &codexOutputTestPipeline{
		server: &Server{cfg: cfg},
		session: &sessionState{
			profile: harnessv2.RuntimeProfile{ProviderKind: providerKindCodex},
			descriptor: harnessv2.RuntimeSessionDescriptor{
				RuntimeSessionUID: "session-1", Generation: 1,
			},
		},
		prompt: &promptState{request: harnessv2.StartPromptRequest{Metadata: harnessv2.MutationMetadata{
			TaskUID: "task-uid-1", TaskAttempt: 1, PromptID: "prompt-1",
			RequestDigest: harnessv2.RequestDigest("sha256:" + strings.Repeat("a", 64)),
		}}},
	}
	eventStore := storetest.NewFakeExecutionEventStore()
	journal := eventjournal.Journal{EventStore: eventStore, MapContext: eventjournal.MapContext{
		Namespace: "default", TaskName: "task-1", Provider: providerKindCodex, Model: "fixture-model",
	}}
	var err error
	pipeline.journal, err = journal.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "task-1"},
	}).Build()
	handlers := api.NewHandlers(api.HandlersConfig{Client: client, ExecutionEventStore: eventStore})
	pipeline.app = fiber.New()
	pipeline.app.Get("/api/v1/tasks/:id/events", handlers.ListTaskEvents)
	return pipeline
}

func (p *codexOutputTestPipeline) update(t *testing.T, wire map[string]any) *harnessv2.Event {
	t.Helper()
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	return p.rawUpdate(t, raw)
}

func (p *codexOutputTestPipeline) rawUpdate(t *testing.T, raw json.RawMessage) *harnessv2.Event {
	t.Helper()
	mapped, err := p.server.mapRuntimeEvent(p.session, p.prompt, acp.PromptEvent{
		Type: acp.PromptEventUpdate, Timestamp: time.Now().UTC(),
		Update: &acp.SessionNotification{SessionID: "codex-session", Update: raw},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mapped == nil {
		return nil
	}
	if _, _, err := p.journal.AppendUpdateIfNew(context.Background(), *mapped); err != nil {
		t.Fatal(err)
	}
	return mapped
}

func (p *codexOutputTestPipeline) events(t *testing.T) (api.ListExecutionEventsResponse, string) {
	t.Helper()
	response, err := p.app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/tasks/task-1/events?namespace=default&after=0&limit=100", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("events endpoint returned status %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var events api.ListExecutionEventsResponse
	if err := json.Unmarshal(body, &events); err != nil {
		t.Fatal(err)
	}
	return events, string(body)
}

func TestCodexCompletedOutputPublicEvents(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
		status string
		exit   any
	}{
		{name: "completed stdout and stderr", output: "stdout: hello\nstderr: warning\n", status: "completed", exit: 0},
		{name: "failed command", output: "command failed\n", status: "failed", exit: 7},
		{name: "unknown exit", output: "interrupted\n", status: "failed"},
		{name: "empty authoritative snapshot", status: "completed", exit: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			pipeline := newCodexOutputTestPipeline(t)
			pipeline.update(t, codexOutputTestStart(codexOutputTestCallID, "printf hello"))
			started, _ := pipeline.events(t)
			if len(started.Events) != 1 || started.Events[0].ContentText != "" {
				t.Fatal("incomplete tool content became public")
			}
			complete := codexOutputTestComplete(codexOutputTestCallID, test.output, test.exit)
			complete["status"] = test.status
			pipeline.update(t, complete)
			listed, body := pipeline.events(t)
			if len(listed.Events) != 2 {
				t.Fatalf("public tool lifecycle has %d events, want 2", len(listed.Events))
			}
			last := listed.Events[1]
			wantType := executionevents.ExecutionEventTypeToolCallCompleted
			if test.status == "failed" {
				wantType = executionevents.ExecutionEventTypeToolCallFailed
			}
			if last.Type != wantType || last.ContentText != test.output || last.ToolCallID != listed.Events[0].ToolCallID {
				t.Fatal("public command output, outcome or call correlation differs from the completed adapter event")
			}
			if last.Truncation != nil || strings.Contains(string(last.Content), "streamed_text_truncated_or_omitted") || strings.Contains(body, "Exit code:") {
				t.Fatal("complete output retained omission or gained synthetic exit metadata")
			}
		})
	}
}

func TestCodexCompletedOutputPublicEventsRedactCompleteLogicalText(t *testing.T) {
	const suffix = "syntheticfixtureabcdefghijklmnop"
	for _, test := range []struct {
		name       string
		title      string
		output     string
		prior      string
		wantHidden string
	}{
		{name: "secret split across streamed deltas", title: "printf hello", output: "sk-" + suffix, wantHidden: suffix},
		{name: "title to output boundary", title: "sk-", output: suffix, wantHidden: suffix},
		{name: "output to title boundary", title: suffix, output: "sk-", wantHidden: suffix},
		{name: "prior output to next output", title: "printf hello", output: suffix, prior: "sk-", wantHidden: suffix},
	} {
		t.Run(test.name, func(t *testing.T) {
			pipeline := newCodexOutputTestPipeline(t)
			if test.prior != "" {
				pipeline.update(t, codexOutputTestStart("prior-command", "printf prior"))
				pipeline.update(t, codexOutputTestComplete("prior-command", test.prior, 0))
			}
			pipeline.update(t, codexOutputTestStart(codexOutputTestCallID, test.title))
			for _, delta := range []string{"sk-", suffix} {
				mapped := pipeline.update(t, map[string]any{
					"sessionUpdate": "tool_call_update", "toolCallId": codexOutputTestCallID,
					"_meta": map[string]any{"terminal_output_delta": map[string]any{"terminal_id": codexOutputTestCallID, "data": delta}},
				})
				if mapped != nil {
					t.Fatal("partial output entered the journal")
				}
			}
			pipeline.update(t, codexOutputTestComplete(codexOutputTestCallID, test.output, 0))
			listed, body := pipeline.events(t)
			if strings.Contains(body, test.wantHidden) || listed.Events[len(listed.Events)-1].ContentText != executionevents.ExecutionEventRedactedValue {
				t.Fatal("public events exposed a secret-shaped logical field or a reconstructable split")
			}
		})
	}
}

func TestCodexCompletedOutputPublicEventsPreserveOmission(t *testing.T) {
	for _, test := range []struct {
		name    string
		output  string
		maxLine int
		missing bool
	}{
		{name: "journal character bound", output: strings.Repeat("x", executionevents.MaxExecutionEventContentTextChars+1)},
		{name: "harness text bound", output: strings.Repeat("x", harnessv2.MaxPromptContentBytes+1)},
		{name: "event line bound", output: strings.Repeat("x", 5000), maxLine: 2048},
		{name: "missing completed output", missing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pipeline := newCodexOutputTestPipeline(t)
			if test.maxLine != 0 {
				pipeline.server.cfg.Capabilities.Limits.MaxEventLineBytes = test.maxLine
			}
			pipeline.update(t, codexOutputTestStart(codexOutputTestCallID, "printf hello"))
			complete := codexOutputTestComplete(codexOutputTestCallID, test.output, 0)
			if test.missing {
				delete(complete, "rawOutput")
			}
			pipeline.update(t, complete)
			listed, body := pipeline.events(t)
			if len(listed.Events) != 2 || listed.Events[1].ContentText != "" || !strings.Contains(body, "streamed_text_truncated_or_omitted") {
				t.Fatal("missing or oversized command output lost its omission marker")
			}
		})
	}
}

func TestCodexCompletedOutputRuntimeProviderGate(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*codexOutputTestPipeline)
	}{
		{name: "different provider", mutate: func(p *codexOutputTestPipeline) { p.server.cfg.Provider.Kind = "claude" }},
		{name: "different session profile", mutate: func(p *codexOutputTestPipeline) { p.session.profile.ProviderKind = "claude" }},
		{name: "different adapter", mutate: func(p *codexOutputTestPipeline) { p.server.cfg.Provider.AdapterName = "external-adapter" }},
		{name: "different adapter digest", mutate: func(p *codexOutputTestPipeline) {
			p.server.cfg.Provider.AdapterDigest = "sha256:" + strings.Repeat("b", 64)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			pipeline := newCodexOutputTestPipeline(t)
			test.mutate(pipeline)
			pipeline.update(t, codexOutputTestStart(codexOutputTestCallID, "printf hello"))
			pipeline.update(t, codexOutputTestComplete(codexOutputTestCallID, "provider-private-output", 0))
			listed, body := pipeline.events(t)
			if len(listed.Events) != 2 || strings.Contains(body, "provider-private-output") {
				t.Fatal("a different provider or adapter gained raw output projection")
			}
		})
	}
}
