/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

// Model-free evals for the chat orchestrator. A scripted provider stands in for
// the LLM, so these run in milliseconds and check what the real HandleChat path
// sends to the model and how Orka handles the decisions a model makes. They do
// not measure model judgment.
//
// A case with knownDefect documents a check that fails on current code. It
// passes while the defect reproduces and fails once the defect is fixed, so the
// fix also removes the knownDefect marker.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
	chattools "github.com/orka-agents/orka/internal/tools"
)

const (
	chatEvalProviderType = "chat-eval-scripted"
	chatEvalNamespace    = "team-eval"
	// chatEvalMaxModelContextBytes bounds the system prompt plus tool schemas
	// sent on every chat turn. Raise it deliberately when the prompt must grow.
	chatEvalMaxModelContextBytes = 40000
)

// evalRecordingProvider records every completion request the chat loop sends.
type evalRecordingProvider struct {
	chatMockProvider
	requests []*llm.CompletionRequest
}

func (p *evalRecordingProvider) Complete(ctx context.Context, req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	recorded := *req
	recorded.Messages = slices.Clone(req.Messages)
	recorded.Tools = slices.Clone(req.Tools)
	p.requests = append(p.requests, &recorded)
	return p.chatMockProvider.Complete(ctx, req)
}

// chatEvalCluster seeds each namespace with the same Providers and Agents:
// a runtime coding agent, a plain AI reviewer, and a non-runtime coordinator.
func chatEvalCluster(t *testing.T, namespaces ...string) client.Client {
	t.Helper()
	objs := make([]runtime.Object, 0, 8*len(namespaces))
	for _, ns := range namespaces {
		objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: types.UID("uid-" + ns)}})
		objs = append(objs, providerCRD("openai", ns, chatEvalProviderType, "eval-model")...)
		objs = append(objs, providerCRD("secondary", ns, chatEvalProviderType, "eval-model")...)
		objs = append(objs,
			&corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: ns},
				Spec:       corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex}},
			},
			&corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: ns},
				Spec:       corev1alpha1.AgentSpec{ProviderRef: &corev1alpha1.ProviderReference{Name: "openai"}},
			},
			&corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "dev-coordinator", Namespace: ns},
				Spec: corev1alpha1.AgentSpec{
					ProviderRef:  &corev1alpha1.ProviderReference{Name: "openai"},
					Coordination: &corev1alpha1.CoordinationConfig{Enabled: true},
				},
			},
		)
	}
	return fake.NewClientBuilder().WithScheme(newTestScheme()).WithRuntimeObjects(objs...).Build()
}

// runChatEvalTurn sends one chat request through HandleChat with a scripted
// model and returns the recorded model requests and the chat response.
func runChatEvalTurn(t *testing.T, c client.Client, req ChatRequest, responses ...*llm.CompletionResponse) (*evalRecordingProvider, ChatResponse) {
	t.Helper()
	provider := &evalRecordingProvider{chatMockProvider: chatMockProvider{name: chatEvalProviderType, responses: responses}}
	llm.RegisterProvider(chatEvalProviderType, func(llm.ProviderConfig) (llm.Provider, error) { return provider, nil })

	cfg := DefaultChatConfig()
	cfg.MaxIterations = 3
	cfg.RuntimeAvailability = ACPRuntimeAvailability{Codex: true, Copilot: true}
	ch := newTestChatHandler(t, c, newTestSessionStore(t), newTestResultStore(t), cfg)
	app := fiber.New()
	app.Post("/api/v1/chat", ch.HandleChat)

	if req.SessionID == "" {
		req.SessionID = "chat-eval"
	}
	if req.Provider == "" {
		req.Provider = "openai"
	}
	body, err := json.Marshal(req)
	require.NoError(t, err)
	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	resp, err := app.Test(httpReq)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", data)

	var chatResp ChatResponse
	require.NoError(t, json.Unmarshal(data, &chatResp))
	require.NotEmpty(t, provider.requests, "chat turn made no model call")
	return provider, chatResp
}

// chatEvalModelInput is everything the model reads on a turn except the static
// tool schemas.
func chatEvalModelInput(req *llm.CompletionRequest) string {
	var sb strings.Builder
	sb.WriteString(req.SystemPrompt)
	for _, m := range req.Messages {
		sb.WriteString("\n")
		sb.WriteString(m.Role)
		sb.WriteString(": ")
		sb.WriteString(m.Content)
	}
	return sb.String()
}

// expectChatEvalCheck applies the knownDefect contract described at the top
// of this file.
func expectChatEvalCheck(t *testing.T, ok bool, detail, knownDefect string) {
	t.Helper()
	switch {
	case ok && knownDefect != "":
		t.Errorf("known defect no longer reproduces; remove knownDefect: %s", knownDefect)
	case !ok && knownDefect == "":
		t.Errorf("check failed: %s", detail)
	case !ok:
		t.Logf("known defect still present: %s (%s)", knownDefect, detail)
	}
}

// TestChatEvalRequestContextReachesModel checks that request fields the prompt
// tells the model to act on change what the model sees. Identical model input
// for different requests means the model cannot follow that instruction.
func TestChatEvalRequestContextReachesModel(t *testing.T) {
	const message = "review the design doc at https://example.com/design.md and list the top 3 risks"
	tests := []struct {
		name        string
		base        ChatRequest
		variant     ChatRequest
		knownDefect string
	}{
		{
			name:    "runtime agent selection",
			base:    ChatRequest{Message: message},
			variant: ChatRequest{Message: message, AgentRef: "coder"},
		},
		{
			name:        "non-runtime agent selection",
			base:        ChatRequest{Message: message},
			variant:     ChatRequest{Message: message, AgentRef: "reviewer"},
			knownDefect: "HandleChat only tells the model about a selected agent when it has a runtime, but the prompt says to use create_ai_task with agentRef for non-runtime agents",
		},
		{
			name:        "request namespace",
			base:        ChatRequest{Message: message, Namespace: defaultNamespace},
			variant:     ChatRequest{Message: message, Namespace: chatEvalNamespace},
			knownDefect: `the model is never told the request namespace, yet the prompt says "use the namespace from the request or ask the user"; tools silently default to it`,
		},
		{
			name:        "session provider",
			base:        ChatRequest{Message: message, Provider: "openai"},
			variant:     ChatRequest{Message: message, Provider: "secondary"},
			knownDefect: `the prompt says "Use the same provider that this chat session is using" but lists providers without marking the session's`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := chatEvalCluster(t, defaultNamespace, chatEvalNamespace)
			base, _ := runChatEvalTurn(t, c, tt.base, &llm.CompletionResponse{Content: "ok"})
			again, _ := runChatEvalTurn(t, c, tt.base, &llm.CompletionResponse{Content: "ok"})
			require.Equal(t, chatEvalModelInput(base.requests[0]), chatEvalModelInput(again.requests[0]),
				"identical requests must produce identical model input for this comparison to mean anything")
			variant, _ := runChatEvalTurn(t, c, tt.variant, &llm.CompletionResponse{Content: "ok"})
			differs := chatEvalModelInput(base.requests[0]) != chatEvalModelInput(variant.requests[0])
			expectChatEvalCheck(t, differs, "model input is identical for both requests", tt.knownDefect)
		})
	}
}

var chatEvalProviderRefPattern = regexp.MustCompile(`providerRef\\?"?\s*[=:]\s*\\?"?([A-Za-z0-9][A-Za-z0-9._-]*)`)

func chatEvalProviderRefs(text string) []string {
	matches := chatEvalProviderRefPattern.FindAllStringSubmatch(text, -1)
	refs := make([]string, 0, len(matches))
	for _, m := range matches {
		refs = append(refs, m[1])
	}
	return refs
}

func TestChatEvalProviderRefExtraction(t *testing.T) {
	tests := []struct {
		text string
		want []string
	}{
		{text: `Set coordination.enabled=true, providerRef="copilot", model.name="gpt-5.4"`, want: []string{"copilot"}},
		{text: `create_ai_task (prompt: "...", providerRef: "openai")`, want: []string{"openai"}},
		{text: `{"providerRef":"team-llm"}`, want: []string{"team-llm"}},
		{text: `providerRef=local.v1 and providerRef = "b"`, want: []string{"local.v1", "b"}},
		{text: `set providerRef to an available provider name`, want: []string{}},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, chatEvalProviderRefs(tt.text), tt.text)
	}
}

// TestChatEvalInjectedDirectivesReferenceExistingProviders checks text that
// HandleChat adds to the user's message. Those directives are mandatory for the
// model, so every Provider they name must exist in the namespace.
func TestChatEvalInjectedDirectivesReferenceExistingProviders(t *testing.T) {
	tests := []struct {
		name        string
		req         ChatRequest
		knownDefect string
	}{
		{
			name: "runtime agent hint",
			req:  ChatRequest{Message: "refactor the auth middleware in https://github.com/acme/api", AgentRef: "coder"},
		},
		{
			name:        "issue workflow",
			req:         ChatRequest{Message: "pick up https://github.com/acme/api/issues/42 and open a PR for it"},
			knownDefect: `the issue-workflow directive hard-codes providerRef="copilot" instead of an available Provider`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := chatEvalCluster(t, defaultNamespace)
			provider, _ := runChatEvalTurn(t, c, tt.req, &llm.CompletionResponse{Content: "ok"})
			messages := provider.requests[0].Messages
			require.NotEmpty(t, messages)
			content := messages[len(messages)-1].Content
			require.True(t, strings.HasSuffix(content, tt.req.Message), "user message was not preserved: %q", content)
			injected := strings.TrimSuffix(content, tt.req.Message)
			require.NotEmpty(t, strings.TrimSpace(injected), "expected HandleChat to inject a directive")

			var providers corev1alpha1.ProviderList
			require.NoError(t, c.List(t.Context(), &providers, client.InNamespace(defaultNamespace)))
			existing := map[string]bool{}
			for i := range providers.Items {
				existing[providers.Items[i].Name] = true
			}
			var missing []string
			for _, ref := range chatEvalProviderRefs(injected) {
				if !existing[ref] {
					missing = append(missing, ref)
				}
			}
			expectChatEvalCheck(t, len(missing) == 0, "injected directive names missing Providers "+strings.Join(missing, ","), tt.knownDefect)
		})
	}
}

// Names followed by = or : are tool arguments such as wait_timeout="30m".
var chatEvalToolMentionPattern = regexp.MustCompile(`\b((?:create|wait|fetch|check|delegate|send|cancel|list|update|delete|get)_[a-z]+(?:_[a-z]+)*)\b(\s*[=:])?`)

// TestChatEvalPromptToolReferencesExist catches prompt or tool-description text
// that still names a renamed or removed tool. Coordinator-only tools may be mentioned because the
// prompt describes what coordinator Agents will do.
func TestChatEvalPromptToolReferencesExist(t *testing.T) {
	c := chatEvalCluster(t, defaultNamespace)
	provider, _ := runChatEvalTurn(t, c, ChatRequest{Message: "hello"}, &llm.CompletionResponse{Content: "ok"})
	req := provider.requests[0]

	known := map[string]bool{}
	for _, name := range chattools.KnownBuiltInToolNames() {
		known[name] = true
	}
	offered := map[string]bool{}
	for _, tool := range req.Tools {
		offered[tool.Name] = true
	}
	for _, name := range chattools.ChatToolNames() {
		require.True(t, offered[name], "chat tool %s is not offered to the model", name)
	}

	text := make([]string, 0, 1+len(req.Tools))
	text = append(text, req.SystemPrompt)
	for _, tool := range req.Tools {
		text = append(text, tool.Description)
	}
	var stale []string
	for _, m := range chatEvalToolMentionPattern.FindAllStringSubmatch(strings.Join(text, "\n"), -1) {
		name, isArgument := m[1], m[2] != ""
		if !isArgument && !known[name] && !slices.Contains(stale, name) {
			stale = append(stale, name)
		}
	}
	require.Empty(t, stale, "prompt or tool descriptions name tools that are not registered anywhere")
}

func TestChatEvalModelContextBudget(t *testing.T) {
	c := chatEvalCluster(t, defaultNamespace)
	provider, _ := runChatEvalTurn(t, c, ChatRequest{Message: "hello"}, &llm.CompletionResponse{Content: "ok"})
	req := provider.requests[0]
	tools, err := json.Marshal(req.Tools)
	require.NoError(t, err)
	size := len(req.SystemPrompt) + len(tools)
	t.Logf("system prompt %d bytes, %d tools as %d bytes of JSON, total %d bytes", len(req.SystemPrompt), len(req.Tools), len(tools), size)
	require.LessOrEqual(t, size, chatEvalMaxModelContextBytes, "chat model context grew past the budget")
}

// TestChatEvalModelDecisionsFailFast replays tool calls a model can make and
// checks that Orka rejects bad ones immediately, in a result the model can act
// on, instead of accepting them and failing minutes later. Bad decisions are
// paired with a valid control so a check cannot pass by rejecting everything.
func TestChatEvalModelDecisionsFailFast(t *testing.T) {
	tests := []struct {
		name        string
		tool        string
		args        string
		wantReject  bool
		knownDefect string
	}{
		{
			name: "create agent with an existing provider",
			tool: "create_agent",
			args: `{"name":"planner","providerRef":"openai","coordination":{"enabled":true}}`,
		},
		{
			name:        "create agent with a missing provider",
			tool:        "create_agent",
			args:        `{"name":"planner","providerRef":"copilot","coordination":{"enabled":true}}`,
			wantReject:  true,
			knownDefect: "create_agent accepts a providerRef that names no Provider; tasks using the Agent fail later",
		},
		{
			name:       "create an agent that already exists",
			tool:       "create_agent",
			args:       `{"name":"dev-coordinator","providerRef":"openai","coordination":{"enabled":true}}`,
			wantReject: true,
		},
		{
			name: "agent task for a runtime agent",
			tool: "create_agent_task",
			args: `{"name":"refactor-auth","agentRef":"coder","prompt":"refactor the auth middleware","timeout":"20m"}`,
		},
		{
			name:        "agent task for a non-runtime agent",
			tool:        "create_agent_task",
			args:        `{"name":"review-doc","agentRef":"reviewer","prompt":"review the design doc","timeout":"20m"}`,
			wantReject:  true,
			knownDefect: "create_agent_task accepts an Agent without a runtime; the controller fails the Task later",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := chatEvalCluster(t, defaultNamespace)
			_, resp := runChatEvalTurn(t, c, ChatRequest{Message: "eval"},
				&llm.CompletionResponse{ToolCalls: []llm.ToolCall{{ID: "eval-call", Name: tt.tool, Arguments: json.RawMessage(tt.args)}}},
				&llm.CompletionResponse{Content: "done"},
			)
			require.Len(t, resp.ToolCalls, 1)
			var result ToolResult
			require.NoError(t, json.Unmarshal(resp.ToolCalls[0].Result, &result))
			if !tt.wantReject {
				require.True(t, result.Success, "valid decision was rejected: %s %s", result.ErrorType, result.Error)
				return
			}
			rejected := !result.Success && strings.TrimSpace(result.Error) != ""
			expectChatEvalCheck(t, rejected, "tool accepted the decision", tt.knownDefect)
		})
	}
}
