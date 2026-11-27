/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gofiber/fiber/v3"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/microsoft/agent-host-protocol/clients/go/ahpws"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/ahphost"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/executionmode"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/liveevents"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/store/storetest"
)

const (
	ahpTestNamespace = "team"
	ahpTestRepo      = "https://github.com/octo/widgets"
	ahpTestDir       = "file:///workspace/src/octo/widgets"
	ahpTestSession   = "ahp-session:/one"
)

type ahpTestEnv struct {
	t       *testing.T
	kube    client.Client
	live    *liveevents.Broker
	backend *ahpBackend
	host    *ahphost.Host
	server  *httptest.Server
	tokens  *ahphost.TokenService

	mu           sync.Mutex
	decisions    []string
	gitRequests  []*http.Request
	onTaskCreate func(*corev1alpha1.Task) error
}

type ahpRoundTrip func(*http.Request) (*http.Response, error)

func (f ahpRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func ahpTestAdvertisement(branch string) string {
	pkt := func(line string) string { return fmt.Sprintf("%04x%s", len(line)+4, line) }
	return pkt("# service=git-upload-pack\n") + "0000" +
		pkt("1111111111111111111111111111111111111111 HEAD\x00multi_ack symref=HEAD:refs/heads/"+branch+" agent=git/test\n") +
		pkt("1111111111111111111111111111111111111111 refs/heads/"+branch+"\n") + "0000"
}

func ahpTestAgent(name string, runtime corev1alpha1.AgentRuntimeType, model string) *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ahpTestNamespace},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: runtime},
			Model:   &corev1alpha1.ModelConfig{Name: model},
		},
	}
}

func newAHPTestEnv(t *testing.T, objects ...client.Object) *ahpTestEnv {
	t.Helper()
	return newAHPTestEnvWith(t, ContextTokenAuthorizationConfig{}, objects...)
}

func newAHPTestEnvWith(t *testing.T, authz ContextTokenAuthorizationConfig, objects ...client.Object) *ahpTestEnv {
	t.Helper()
	objects = append(objects,
		ahpTestAgent("copilot-haiku", corev1alpha1.AgentRuntimeCopilot, "claude-haiku-5.5"),
		ahpTestAgent("copilot-gpt", corev1alpha1.AgentRuntimeCopilot, "gpt-5.4-nano"),
		ahpTestAgent("claude-sonnet", corev1alpha1.AgentRuntimeClaude, "claude-sonnet-5"),
		&corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "no-model", Namespace: ahpTestNamespace},
			Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex}}},
	)
	var env *ahpTestEnv
	kube := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(objects...).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if task, ok := obj.(*corev1alpha1.Task); ok && env != nil && env.onTaskCreate != nil {
				if err := env.onTaskCreate(task); err != nil {
					return err
				}
			}
			if obj.GetUID() == "" {
				obj.SetUID(types.UID("uid-" + obj.GetName()))
			}
			return c.Create(ctx, obj, opts...)
		}}).Build()
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	tokens := &ahphost.TokenService{Store: s}
	registry, err := ahphost.NewRegistry()
	require.NoError(t, err)
	host, err := ahphost.New(ahphost.Config{Store: s, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	live := liveevents.NewBroker(0)
	handlers := NewHandlers(HandlersConfig{
		Client: kube, APIReader: kube, ExecutionMode: executionmode.HarnessV2, ResultStore: s, ContextTokenAuthorization: authz,
	})
	env = &ahpTestEnv{t: t, kube: kube, live: live, host: host, tokens: tokens}
	backend := newAHPBackend(handlers, s, live, AHPBackendConfig{GitCredentialSecret: "git-token"})
	backend.emitterFor = host.Emitter
	backend.decideApproval = func(_ context.Context, ui *UserInfo, namespace, task, approvalID string, approve bool) error {
		env.mu.Lock()
		defer env.mu.Unlock()
		env.decisions = append(env.decisions, strings.Join([]string{ui.Subject, namespace, task, approvalID, map[bool]string{true: "approve", false: "decline"}[approve]}, " "))
		return nil
	}
	backend.gitHTTP = &http.Client{Transport: ahpRoundTrip(func(req *http.Request) (*http.Response, error) {
		env.mu.Lock()
		env.gitRequests = append(env.gitRequests, req)
		env.mu.Unlock()
		body := ahpTestAdvertisement("main")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	require.NoError(t, host.RegisterBackend(backend))
	env.backend = backend
	env.server = httptest.NewServer(host)
	t.Cleanup(env.server.Close)
	return env
}

func (e *ahpTestEnv) connect(subject string) *ahp.Client {
	e.t.Helper()
	return e.connectWith(subject, "client-"+subject)
}

func (e *ahpTestEnv) connectWith(subject, clientID string) *ahp.Client {
	e.t.Helper()
	minted, err := e.tokens.Mint(context.Background(), ahphost.MintRequest{
		Issuer: "https://issuer.example", Subject: subject, Username: subject, Namespace: ahpTestNamespace,
	})
	require.NoError(e.t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transport, err := ahpws.Connect(ctx, "ws"+strings.TrimPrefix(e.server.URL, "http")+"/?tkn="+minted.Token)
	require.NoError(e.t, err)
	c, err := ahp.Connect(ctx, transport, ahp.DefaultConfig())
	require.NoError(e.t, err)
	e.t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	_, err = c.Initialize(ctx, clientID, []string{"0.9.0"}, nil)
	require.NoError(e.t, err)
	return c
}

func ahpCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func (e *ahpTestEnv) setUpSession(c *ahp.Client, mode string) string {
	e.t.Helper()
	ctx := ahpCtx(e.t)
	var cloned struct {
		Project map[string]any `json:"project"`
	}
	require.NoError(e.t, c.Request(ctx, ahpExtensionCloneProject, map[string]string{"url": ahpTestRepo + ".git"}, &cloned))
	require.Equal(e.t, "/workspace/src/octo/widgets", cloned.Project["path"])
	require.Equal(e.t, "ready", cloned.Project["status"])
	provider := "copilot"
	modeJSON, _ := json.Marshal(mode)
	require.NoError(e.t, c.Request(ctx, "createSession", ahptypes.CreateSessionParams{
		Channel: ahpTestSession, Provider: &provider, WorkingDirectories: []string{ahpTestDir},
		Config: map[string]json.RawMessage{"approvalMode": modeJSON, "baseBranch": json.RawMessage(`"main"`)},
	}, nil))
	result, _, err := c.Subscribe(ctx, ahpTestSession)
	require.NoError(e.t, err)
	state := result.Snapshot.State.Session
	require.Len(e.t, state.Chats, 1)
	return state.Chats[0].Resource
}

func (e *ahpTestEnv) waitForTask(name string) *corev1alpha1.Task {
	e.t.Helper()
	task := &corev1alpha1.Task{}
	require.Eventually(e.t, func() bool {
		return e.kube.Get(context.Background(), types.NamespacedName{Namespace: ahpTestNamespace, Name: name}, task) == nil
	}, 5*time.Second, 20*time.Millisecond, "the turn's Task was never created")
	return task
}

func (e *ahpTestEnv) setPhase(name string, phase corev1alpha1.TaskPhase) {
	e.t.Helper()
	task := &corev1alpha1.Task{}
	require.NoError(e.t, e.kube.Get(context.Background(), types.NamespacedName{Namespace: ahpTestNamespace, Name: name}, task))
	task.Status.Phase = phase
	require.NoError(e.t, e.kube.Status().Update(context.Background(), task))
}

func (e *ahpTestEnv) publish(task string, update *harnessv2.UpdateEvent) {
	e.live.Publish(liveevents.Event{Namespace: ahpTestNamespace, Task: task, Runtime: &harnessv2.Event{Type: harnessv2.EventUpdate, Update: update}})
}

func ahpReadyInput(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var ready ahptypes.ChatToolCallReadyAction
	require.NoError(t, json.Unmarshal(raw, &ready))
	require.NotNil(t, ready.ToolInput, "the confirmation shows the call's input")
	require.NotNil(t, ready.ToolInput.Inline)
	return *ready.ToolInput.Inline
}

func waitForActionType(t *testing.T, sub *ahp.Subscription, want string) json.RawMessage {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case event := <-sub.Events():
			if action, ok := event.(ahp.SubscriptionEventAction); ok {
				raw, _ := json.Marshal(action.Envelope.Action)
				var head struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal(raw, &head)
				if head.Type == want {
					return raw
				}
			}
		case <-timeout:
			t.Fatalf("no %s action arrived", want)
		}
	}
}

func TestAHPBackendOffersBuiltInRuntimesAndProjects(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	result, _, err := c.Subscribe(ctx, ahptypes.RootResourceURI)
	require.NoError(t, err)
	root := result.Snapshot.State.Root
	require.JSONEq(t, `{"available":true}`, string(root.Meta[ahpCopilotProjectManagementMeta]))
	providers := map[string][]string{}
	var names []string
	for _, agent := range root.Agents {
		for _, model := range agent.Models {
			providers[agent.Provider] = append(providers[agent.Provider], model.Id)
			names = append(names, model.Name)
			require.NotNil(t, model.PolicyState)
			require.Equal(t, ahptypes.PolicyStateEnabled, *model.PolicyState, "listed models are enabled for the user")
		}
		require.Empty(t, agent.ProtectedResources, "Orka never asks for the person's own credentials")
	}
	require.Equal(t, map[string][]string{"copilot": {"gpt-5.4-nano", "claude-haiku-5.5", "claude-sonnet-5"}}, providers,
		"one agent offers every model, in runtime order and then Agent name order")
	require.Equal(t, []string{"gpt-5.4-nano (Copilot CLI)", "claude-haiku-5.5 (Copilot CLI)", "claude-sonnet-5 (Claude Code)"}, names,
		"each model names the runtime that runs it")

	for _, bad := range []string{"http://github.com/a/b", "https://user:pw@github.com/a/b", "https://github.com/a", "https://github.com/a/b?x=1"} {
		err := c.Request(ctx, ahpExtensionCloneProject, map[string]string{"url": bad}, nil)
		require.Error(t, err, bad)
	}
	provider := "copilot"
	err = c.Request(ctx, "createSession", ahptypes.CreateSessionParams{
		Channel: "ahp-session:/elsewhere", Provider: &provider, WorkingDirectories: []string{"file:///etc"},
	}, nil)
	require.Error(t, err, "a session outside a project is refused")
}

func TestAHPBackendOffersNoModelOfAnUnusableAgent(t *testing.T) {
	agent := func(name, model string, conditions ...metav1.Condition) *corev1alpha1.Agent {
		agent := ahpTestAgent(name, corev1alpha1.AgentRuntimeOpencode, model)
		agent.Status.Conditions = conditions
		return agent
	}
	env := newAHPTestEnv(t,
		agent("opencode-broken", "openai/broken", metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "ValidationFailed",
			Message: "opencode runtime requires a positive spec.model.contextWindow", LastTransitionTime: metav1.Now()}),
		agent("opencode-unchecked", "openai/unchecked"))
	c := env.connect("alice")
	result, _, err := c.Subscribe(ahpCtx(t), ahptypes.RootResourceURI)
	require.NoError(t, err)
	var models []string
	for _, offered := range result.Snapshot.State.Root.Agents {
		for _, model := range offered.Models {
			models = append(models, model.Id)
		}
	}
	require.Contains(t, models, "openai/unchecked")
	require.NotContains(t, models, "openai/broken")
}

func TestAHPBackendMovesASessionOffAnAgentThatIsNoLongerOffered(t *testing.T) {
	env := newAHPTestEnv(t, ahpTestAgent("copilot-gpt-spare", corev1alpha1.AgentRuntimeCopilot, "gpt-5.4-nano"))
	ctx := ahpCtx(t)
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Username: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	record := ahpSession{Agent: "copilot-gpt", Model: "gpt-5.4-nano", Runtime: string(corev1alpha1.AgentRuntimeCopilot)}
	choice, switching, err := env.backend.resolveModel(ctx, alice, record, &ahptypes.ModelSelection{Id: "gpt-5.4-nano"})
	require.NoError(t, err)
	require.Equal(t, "copilot-gpt", choice.Agent)
	require.False(t, switching, "a session keeps its Agent while it is offered")

	agent := &corev1alpha1.Agent{}
	require.NoError(t, env.kube.Get(ctx, types.NamespacedName{Namespace: ahpTestNamespace, Name: "copilot-gpt"}, agent))
	agent.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "ValidationFailed", LastTransitionTime: metav1.Now()}}
	require.NoError(t, env.kube.Update(ctx, agent))
	for _, selected := range []*ahptypes.ModelSelection{nil, {Id: "gpt-5.4-nano"}} {
		choice, switching, err = env.backend.resolveModel(ctx, alice, record, selected)
		require.NoError(t, err)
		require.Equal(t, "copilot-gpt-spare", choice.Agent, "the Agent that still offers the model runs it")
		require.True(t, switching, "the session moves to a new Orka Session for its new Agent")
	}

	require.NoError(t, env.kube.Get(ctx, types.NamespacedName{Namespace: ahpTestNamespace, Name: "copilot-gpt-spare"}, agent))
	require.NoError(t, env.kube.Delete(ctx, agent))
	_, _, err = env.backend.resolveModel(ctx, alice, record, nil)
	require.ErrorContains(t, err, "no longer available", "with no Agent left for the model, the session's turn is refused")
}

//nolint:gocyclo // One end-to-end turn checks every mapped action against the reduced state.
func TestAHPBackendRunsATurnAsATask(t *testing.T) {
	gitSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "git-token", Namespace: ahpTestNamespace}, Data: map[string][]byte{"token": []byte("not-a-real-token")}}
	env := newAHPTestEnv(t, gitSecret)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "manual")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	_, sessionSub, err := c.Subscribe(ctx, ahpTestSession)
	require.NoError(t, err)

	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "Add a comment and run the tests", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser},
			Model: &ahptypes.ModelSelection{Id: "claude-haiku-5.5"}},
	}})
	require.NoError(t, err)

	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, ahphost.UserKey("https://issuer.example", "alice"), ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	task := env.waitForTask(taskName)
	require.Equal(t, "copilot-haiku", task.Spec.AgentRef.Name)
	require.Equal(t, "Add a comment and run the tests", task.Spec.Prompt)
	require.True(t, task.Spec.SessionRef.Create)
	require.True(t, task.Spec.SessionRef.Append)
	require.Equal(t, ahpTestRepo, task.Spec.Workspace.GitRepo)
	require.Equal(t, "main", task.Spec.Workspace.Branch)
	require.True(t, strings.HasPrefix(task.Spec.Workspace.PushBranch, "orka/session-"), "turns publish to the session's branch")
	require.Equal(t, corev1alpha1.WorkspaceIntentWrite, task.Spec.Workspace.Intent, "a configured git credential makes turns writable")
	require.Equal(t, "git-token", task.Spec.Workspace.PublicationCredentialRef.Name)
	require.Equal(t, "alice", task.Spec.RequestedBy.Subject, "the Task is the person's")
	require.Equal(t, labels.RequestedBySourceAPI, task.Annotations[labels.AnnotationRequestedBySource])
	require.Equal(t, labels.LabelAHPTurnValue, task.Labels[labels.LabelAHPTurn], "the API shows a turn's Task only to its requester")
	require.Equal(t, corev1alpha1.PermissionDecisionsRequester, task.Spec.AgentRuntime.PermissionDecisions,
		"manual approvals hold permission requests for the person")

	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-x", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "again", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)

	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantThoughtChunk, AssistantThought: &harnessv2.AssistantMessageChunk{Text: "Thinking."}})
	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantMessageChunk, AssistantMessage: &harnessv2.AssistantMessageChunk{Text: "Running "}})
	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantMessageChunk, AssistantMessage: &harnessv2.AssistantMessageChunk{Text: "them."}})
	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateToolCall, ToolCall: &harnessv2.ToolCallUpdate{
		ToolCallID: "tool-1", Name: "bash", Title: "Run tests", Status: harnessv2.ToolCallStatusPending,
		RawInput: json.RawMessage(`{"command":"go test ./..."}`),
	}})
	env.live.Publish(liveevents.Event{Namespace: ahpTestNamespace, Task: taskName, Approval: &liveevents.Approval{
		ID: "approval-1", State: liveevents.ApprovalPending, ToolCallID: "tool-1", ToolName: "bash", Title: "Run tests",
	}})
	require.JSONEq(t, `{"command":"go test ./..."}`, ahpReadyInput(t, waitForActionType(t, chatSub, "chat/toolCallReady")))
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatToolCallConfirmedAction{
		Type: ahptypes.ActionTypeChatToolCallConfirmed, TurnId: "turn-1", ToolCallId: "tool-1", Approved: true,
	}})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		env.mu.Lock()
		defer env.mu.Unlock()
		return len(env.decisions) == 1
	}, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, "alice team "+taskName+" approval-1 approve", env.decisions[0])

	old := "package widgets\n"
	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateToolCallUpdate, ToolCall: &harnessv2.ToolCallUpdate{
		ToolCallID: "tool-1", Status: harnessv2.ToolCallStatusCompleted,
		Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "ok"}},
		Diffs:   []harnessv2.ToolCallDiff{{Path: "widgets.go", OldText: &old, NewText: "// Widgets.\npackage widgets\n"}},
	}})
	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantMessageChunk, AssistantMessage: &harnessv2.AssistantMessageChunk{Text: "Done."}})
	waitForActionType(t, chatSub, "chat/delta")
	env.setPhase(taskName, corev1alpha1.TaskPhaseSucceeded)
	waitForActionType(t, chatSub, "chat/turnComplete")
	waitForActionType(t, sessionSub, "session/activityChanged")

	snapshot, _, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	turns := snapshot.Snapshot.State.Chat.Turns
	require.Len(t, turns, 1, "the refused second message never became a turn")
	require.Equal(t, ahptypes.TurnStateComplete, turns[0].State)
	var kinds []string
	var text strings.Builder
	for _, part := range turns[0].ResponseParts {
		switch p := part.Value.(type) {
		case *ahptypes.MarkdownResponsePart:
			kinds = append(kinds, "markdown")
			text.WriteString(p.Content)
		case *ahptypes.ReasoningResponsePart:
			kinds = append(kinds, "reasoning")
		case *ahptypes.ToolCallResponsePart:
			kinds = append(kinds, "toolCall")
			raw, _ := json.Marshal(p.ToolCall)
			require.Contains(t, string(raw), `"status":"completed"`)
			require.Contains(t, string(raw), `"toolName":"bash"`)
			require.Contains(t, string(raw), `"pastTenseMessage":"Run tests"`, "an untitled update keeps the call's title")
		}
	}
	require.Equal(t, []string{"reasoning", "markdown", "toolCall", "markdown"}, kinds)
	require.Equal(t, "Running them.Done.", text.String())

	changes, _, err := c.Subscribe(ctx, record.Changeset)
	require.NoError(t, err)
	files := changes.Snapshot.State.Changeset.Files
	require.Len(t, files, 1)
	require.Equal(t, ahpTestDir+"/widgets.go", files[0].Edit.After.Uri)
	require.Equal(t, int64(1), *files[0].Edit.Diff.Added)
	require.Equal(t, int64(0), *files[0].Edit.Diff.Removed)
	var after, before ahptypes.ResourceReadResult
	require.NoError(t, c.Request(ctx, "resourceRead", ahptypes.ResourceReadParams{Channel: record.Changeset, Uri: ahpTestDir + "/widgets.go"}, &after))
	require.Equal(t, "// Widgets.\npackage widgets\n", after.Data)
	require.NoError(t, c.Request(ctx, "resourceRead", ahptypes.ResourceReadParams{Channel: record.Changeset, Uri: files[0].Edit.Before.Content.Uri}, &before))
	require.Equal(t, old, before.Data)
	rootAfter, err := c.ResourceRead(ctx, ahptypes.ResourceReadParams{Uri: ahpTestDir + "/widgets.go"})
	require.NoError(t, err)
	require.Equal(t, after.Data, rootAfter.Data)
	rootBefore, err := c.ResourceRead(ctx, ahptypes.ResourceReadParams{Uri: files[0].Edit.Before.Content.Uri})
	require.NoError(t, err)
	require.Equal(t, old, rootBefore.Data)
	_, err = env.connect("bob").ResourceRead(ctx, ahptypes.ResourceReadParams{Uri: ahpTestDir + "/widgets.go"})
	require.Error(t, err, "another person cannot read the session's files")

	var listed ahptypes.ListSessionsResult
	require.NoError(t, c.Request(ctx, "listSessions", ahptypes.ListSessionsParams{}, &listed))
	require.Equal(t, "Add a comment and run the tests", listed.Items[0].Title)
	require.Equal(t, ahptypes.SessionStatusIdle, listed.Items[0].Status)
	require.Equal(t, int64(1), *listed.Items[0].Changes.Files)

	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-2", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "and again", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}, Model: &ahptypes.ModelSelection{Id: "claude-haiku-5.5"}},
	}})
	require.NoError(t, err)
	second := env.waitForTask(record.OrkaSession + "-t2")
	require.False(t, second.Spec.SessionRef.Create)
	require.Equal(t, "and again", second.Spec.Prompt)
	env.setPhase(second.Name, corev1alpha1.TaskPhaseSucceeded)
	require.Eventually(t, func() bool {
		return env.backend.activeTurn(ahphost.UserKey("https://issuer.example", "alice"), ahpTestSession) == nil
	}, 5*time.Second, 20*time.Millisecond)

	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-3", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "switch", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}, Model: &ahptypes.ModelSelection{Id: "gpt-5.4-nano"}},
	}})
	require.NoError(t, err)
	third := env.waitForTask(record.OrkaSession + "-g2-t3")
	require.True(t, third.Spec.SessionRef.Create)
	require.Equal(t, record.OrkaSession+"-g2", third.Spec.SessionRef.Name)
	require.Equal(t, "copilot-gpt", third.Spec.AgentRef.Name)
	require.Contains(t, third.Spec.Prompt, "User: Add a comment and run the tests\nAssistant: Running them.Done.")
	require.True(t, strings.HasSuffix(third.Spec.Prompt, "---\n\nswitch"))
	require.Equal(t, record.Branch, third.Spec.Workspace.PushBranch)
	waitForActionType(t, chatSub, "chat/delta")
}

func TestAHPBackendCancelsATurn(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	_, sessionSub, err := c.Subscribe(ctx, ahpTestSession)
	require.NoError(t, err)
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "sleep", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, ahphost.UserKey("https://issuer.example", "alice"), ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	task := env.waitForTask(taskName)
	require.Equal(t, corev1alpha1.WorkspaceIntentRead, task.Spec.Workspace.Intent, "without a git credential turns are read-only")
	require.True(t, task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.PermissionDecisions == "", "allow all leaves decisions to policy")
	require.Eventually(t, func() bool {
		return env.backend.activeTurn(ahphost.UserKey("https://issuer.example", "alice"), ahpTestSession) != nil
	}, 5*time.Second, 20*time.Millisecond)

	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnCancelledAction{
		Type: ahptypes.ActionTypeChatTurnCancelled, TurnId: "turn-1", Duration: 10,
	}})
	require.NoError(t, err)
	waitForActionType(t, chatSub, "chat/turnCancelled")
	require.Eventually(t, func() bool {
		current := &corev1alpha1.Task{}
		_ = env.kube.Get(context.Background(), types.NamespacedName{Namespace: ahpTestNamespace, Name: taskName}, current)
		_, ok := current.Annotations[labels.AnnotationCancelRequested]
		return ok
	}, 5*time.Second, 20*time.Millisecond, "cancelling the turn cancels its Task")
	env.setPhase(taskName, corev1alpha1.TaskPhaseCancelled)
	waitForActionType(t, sessionSub, "session/activityChanged")
	require.Eventually(t, func() bool {
		return env.backend.activeTurn(ahphost.UserKey("https://issuer.example", "alice"), ahpTestSession) == nil
	}, 5*time.Second, 20*time.Millisecond)
	snapshot, _, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	turns := snapshot.Snapshot.State.Chat.Turns
	require.Len(t, turns, 1)
	require.Equal(t, ahptypes.TurnStateCancelled, turns[0].State)
}

func TestAHPBackendShowsATurnItsDeadlineStoppedAsAnError(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	alice := ahphost.UserKey("https://issuer.example", "alice")
	_, err = c.Dispatch(ctx, chat, ahpUserTurn("turn-1", "run long"))
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	task := env.waitForTask(taskName)
	require.NotNil(t, task.Spec.Timeout, "each turn's Task is bounded")
	require.Eventually(t, func() bool { return env.backend.activeTurn(alice, ahpTestSession) != nil }, 5*time.Second, 20*time.Millisecond)

	require.NoError(t, env.kube.Get(ctx, types.NamespacedName{Namespace: ahpTestNamespace, Name: taskName}, task))
	task.Status.Phase = corev1alpha1.TaskPhaseCancelled
	task.Status.Execution = &corev1alpha1.TaskExecutionStatus{
		State: corev1alpha1.TaskExecutionStateCancelled, Outcome: corev1alpha1.TaskExecutionOutcomeCancelled,
		Reason: controller.ACPTaskTimeoutReason, Message: "task deadline cancellation settled",
	}
	require.NoError(t, env.kube.Status().Update(ctx, task))

	raw := waitForActionType(t, chatSub, "chat/error")
	var failed ahptypes.ChatErrorAction
	require.NoError(t, json.Unmarshal(raw, &failed))
	require.Equal(t, controller.ACPTaskTimeoutReason, failed.Part.Error.ErrorType)
	require.Contains(t, failed.Part.Error.Message, task.Spec.Timeout.Duration.String()+" time limit")
}

func TestAHPBackendFollowsTheTaskItCreatedNotItsName(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	alice := ahphost.UserKey("https://issuer.example", "alice")
	_, err = c.Dispatch(ctx, chat, ahpUserTurn("turn-1", "hello"))
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	task := env.waitForTask(taskName)
	require.Eventually(t, func() bool {
		var current ahpSession
		return env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &current) == nil && current.ActiveTaskUID == string(task.UID)
	}, 5*time.Second, 20*time.Millisecond, "the session records the UID of the Task it created")

	say := func(uid, text string) {
		env.live.Publish(liveevents.Event{Namespace: ahpTestNamespace, Task: taskName, TaskUID: uid, Runtime: &harnessv2.Event{
			Type: harnessv2.EventUpdate, Update: &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantMessageChunk,
				AssistantMessage: &harnessv2.AssistantMessageChunk{Text: text}}}})
	}
	say("uid-of-another-task", "intruder")
	say(string(task.UID), "mine")
	raw := waitForActionType(t, chatSub, "chat/delta")
	require.Contains(t, string(raw), "mine")
	require.NotContains(t, string(raw), "intruder")

	require.NoError(t, env.kube.Delete(ctx, task))
	replacement := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: taskName, Namespace: ahpTestNamespace, UID: "uid-of-another-task"},
		Spec: task.Spec}
	require.NoError(t, env.kube.Create(ctx, replacement))
	replacement.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	require.NoError(t, env.kube.Status().Update(ctx, replacement))
	failed := waitForActionType(t, chatSub, "chat/error")
	require.Contains(t, string(failed), "TaskDeleted", "the turn's own Task is gone; another's result does not settle it")
	state, err := ahphost.ChannelState(ctx, env.backend.store, alice, chat)
	require.NoError(t, err)
	require.NotContains(t, ahpTurnText(state.Chat.Turns[len(state.Chat.Turns)-1].ResponseParts), "intruder")
}

func TestAHPBackendRecoveryPinsALegacyTurnsTask(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	_, err = c.Dispatch(ctx, chat, ahpUserTurn("turn-1", "hello"))
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	task := env.waitForTask(taskName)
	var turn *ahpTurn
	require.Eventually(t, func() bool { turn = env.backend.activeTurn(alice.UserKey, ahpTestSession); return turn != nil },
		5*time.Second, 20*time.Millisecond)

	env.backend.forgetTurn(turn)
	require.NoError(t, env.backend.updateSession(ctx, alice.UserKey, ahpTestSession, func(s *ahpSession) { s.ActiveTaskUID = "" }))
	env.backend.mu.Lock()
	delete(env.backend.recovered, alice.UserKey)
	env.backend.mu.Unlock()
	env.backend.recoverTurns(ctx, alice)
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	require.Equal(t, string(task.UID), record.ActiveTaskUID, "recovery keeps the UID of the Task it follows")

	say := func(uid, text string) {
		env.live.Publish(liveevents.Event{Namespace: ahpTestNamespace, Task: taskName, TaskUID: uid, Runtime: &harnessv2.Event{
			Type: harnessv2.EventUpdate, Update: &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantMessageChunk,
				AssistantMessage: &harnessv2.AssistantMessageChunk{Text: text}}}})
	}
	say("uid-of-another-task", "intruder")
	say(string(task.UID), "mine")
	raw := waitForActionType(t, chatSub, "chat/delta")
	require.Contains(t, string(raw), "mine")
	require.NotContains(t, string(raw), "intruder", "an event of another Task under the name is not the recovered turn's")
}

func TestAHPBackendRecoveryAdoptsNoTaskForALegacyTurnWhoseTaskIsGone(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	_, err = c.Dispatch(ctx, chat, ahpUserTurn("turn-1", "hello"))
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	task := env.waitForTask(taskName)
	var turn *ahpTurn
	require.Eventually(t, func() bool { turn = env.backend.activeTurn(alice.UserKey, ahpTestSession); return turn != nil },
		5*time.Second, 20*time.Millisecond)
	env.backend.forgetTurn(turn)
	require.NoError(t, env.backend.updateSession(ctx, alice.UserKey, ahpTestSession, func(s *ahpSession) { s.ActiveTaskUID = "" }))
	require.NoError(t, env.kube.Delete(ctx, task))
	env.backend.mu.Lock()
	delete(env.backend.recovered, alice.UserKey)
	env.backend.mu.Unlock()
	env.backend.recoverTurns(ctx, alice)
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	require.Equal(t, ahpTaskGoneUID, record.ActiveTaskUID, "a later recovery knows the Task was gone too")

	replacement := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: taskName, Namespace: ahpTestNamespace, UID: "uid-of-another-task"},
		Spec: task.Spec}
	require.NoError(t, env.kube.Create(ctx, replacement))
	env.live.Publish(liveevents.Event{Namespace: ahpTestNamespace, Task: taskName, TaskUID: "uid-of-another-task", Runtime: &harnessv2.Event{
		Type: harnessv2.EventUpdate, Update: &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantMessageChunk,
			AssistantMessage: &harnessv2.AssistantMessageChunk{Text: "intruder"}}}})
	replacement.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	require.NoError(t, env.kube.Status().Update(ctx, replacement))
	failed := waitForActionType(t, chatSub, "chat/error")
	require.Contains(t, string(failed), "TaskDeleted")
	state, err := ahphost.ChannelState(ctx, env.backend.store, alice.UserKey, chat)
	require.NoError(t, err)
	require.NotContains(t, ahpTurnText(state.Chat.Turns[len(state.Chat.Turns)-1].ResponseParts), "intruder")
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	require.NotEqual(t, "uid-of-another-task", record.ActiveTaskUID)
}

func TestAHPBackendReadsNoResultOfATaskThatTookTheName(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	alice := ahphost.UserKey("https://issuer.example", "alice")
	_, err := c.Dispatch(ctx, chat, ahpUserTurn("turn-1", "hello"))
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	task := env.waitForTask(taskName)
	var turn *ahpTurn
	require.Eventually(t, func() bool { turn = env.backend.activeTurn(alice, ahpTestSession); return turn != nil },
		5*time.Second, 20*time.Millisecond)
	require.NoError(t, env.backend.h.resultStore.SaveResult(ctx, ahpTestNamespace, taskName, []byte("the turn's answer")))
	read := func() ([]byte, error) {
		turn.mu.Lock()
		defer turn.mu.Unlock()
		return turn.readResult(task)
	}
	data, err := read()
	require.NoError(t, err)
	require.Equal(t, "the turn's answer", string(data))

	env.backend.h.apiReader = interceptor.NewClient(env.kube.(client.WithWatch), interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("the API server timed out")
		}})
	_, err = read()
	require.ErrorIs(t, err, store.ErrNotFound, "an unconfirmed result is withheld")
	env.backend.h.apiReader = nil

	require.NoError(t, env.kube.Delete(ctx, task))
	replacement := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: taskName, Namespace: ahpTestNamespace, UID: "uid-of-another-task"},
		Spec: task.Spec}
	require.NoError(t, env.kube.Create(ctx, replacement))
	require.NoError(t, env.backend.h.resultStore.SaveResult(ctx, ahpTestNamespace, taskName, []byte("intruder")))
	_, err = read()
	require.ErrorIs(t, err, store.ErrNotFound, "another Task's result is not the turn's answer")
}

func TestAHPBackendRetriesAFailedTurnCancel(t *testing.T) {
	env := newAHPTestEnv(t)
	env.backend.cancelBackoff = wait.Backoff{Duration: 20 * time.Millisecond, Factor: 1, Steps: 500}
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, err := c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "sleep", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, ahphost.UserKey("https://issuer.example", "alice"), ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	task := env.waitForTask(taskName)
	require.Eventually(t, func() bool {
		return env.backend.activeTurn(ahphost.UserKey("https://issuer.example", "alice"), ahpTestSession) != nil
	}, 5*time.Second, 20*time.Millisecond)

	task.Spec.RequestedBy.Subject = "bob"
	require.NoError(t, env.kube.Update(ctx, task))
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnCancelledAction{
		Type: ahptypes.ActionTypeChatTurnCancelled, TurnId: "turn-1", Duration: 10,
	}})
	require.NoError(t, err)
	cancelRequested := func() bool {
		current := &corev1alpha1.Task{}
		_ = env.kube.Get(context.Background(), types.NamespacedName{Namespace: ahpTestNamespace, Name: taskName}, current)
		_, ok := current.Annotations[labels.AnnotationCancelRequested]
		return ok
	}
	require.Never(t, cancelRequested, 200*time.Millisecond, 20*time.Millisecond)

	require.NoError(t, env.kube.Get(ctx, types.NamespacedName{Namespace: ahpTestNamespace, Name: taskName}, task))
	task.Spec.RequestedBy.Subject = "alice"
	require.NoError(t, env.kube.Update(ctx, task))
	require.Eventually(t, cancelRequested, 5*time.Second, 20*time.Millisecond, "the cancellation is retried until it lands")
}

func TestAHPBackendCancelsATaskCreatedAfterTheClientCancelled(t *testing.T) {
	env := newAHPTestEnv(t)
	env.backend.cancelBackoff = wait.Backoff{Duration: 20 * time.Millisecond, Factor: 1, Steps: 50}
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	alice := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice"), Issuer: "https://issuer.example",
		Subject: "alice", Username: "alice", Namespace: ahpTestNamespace}
	missed := make(chan struct{}, 1)
	env.backend.h.apiReader = interceptor.NewClient(env.kube.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			if _, ok := obj.(*corev1alpha1.Task); ok && apierrors.IsNotFound(err) {
				select {
				case missed <- struct{}{}:
				default:
				}
			}
			return err
		},
	})
	var cancelled atomic.Bool
	env.onTaskCreate = func(*corev1alpha1.Task) error {
		record := ahpSession{}
		require.NoError(t, env.backend.getRecord(context.Background(), alice.UserKey, ahpRecordSession, ahpTestSession, &record))
		require.Equal(t, "turn-1", record.ActiveTurn, "the turn is recorded before its Task is created")
		env.backend.cancelTurn(context.Background(), alice, record, "turn-1")
		select {
		case <-missed:
		case <-time.After(5 * time.Second):
			t.Error("the cancel never looked for the Task")
		}
		cancelled.Store(true)
		return nil
	}
	_, err := c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "sleep", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)
	record := ahpSession{}
	require.Eventually(t, func() bool {
		return env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record) == nil && record.ActiveTask != ""
	}, 5*time.Second, 20*time.Millisecond)
	env.waitForTask(record.ActiveTask)
	require.True(t, cancelled.Load())
	require.Eventually(t, func() bool {
		current := &corev1alpha1.Task{}
		_ = env.kube.Get(context.Background(), types.NamespacedName{Namespace: ahpTestNamespace, Name: record.ActiveTask}, current)
		_, ok := current.Annotations[labels.AnnotationCancelRequested]
		return ok
	}, 5*time.Second, 20*time.Millisecond, "the Task created after the cancel is cancelled")
}

func TestAHPBackendKeepsUsersApart(t *testing.T) {
	env := newAHPTestEnv(t)
	alice := env.connect("alice")
	env.setUpSession(alice, "manual")
	bob := env.connect("bob")
	ctx := ahpCtx(t)
	var projects struct {
		Projects []map[string]any `json:"projects"`
	}
	require.NoError(t, bob.Request(ctx, ahpExtensionListProjects, map[string]any{}, &projects))
	require.Empty(t, projects.Projects, "projects are per person")
	var listed ahptypes.ListSessionsResult
	require.NoError(t, bob.Request(ctx, "listSessions", ahptypes.ListSessionsParams{}, &listed))
	require.Empty(t, listed.Items)
}

func TestAHPBackendAdmitsOneTurnAtATime(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	for _, id := range []string{"turn-a", "turn-b"} {
		_, err := c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
			Type: ahptypes.ActionTypeChatTurnStarted, TurnId: id, StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Message: ahptypes.Message{Text: "hello " + id, Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
		}})
		require.NoError(t, err)
	}
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, ahphost.UserKey("https://issuer.example", "alice"), ahpRecordSession, ahpTestSession, &record))
	first := env.waitForTask(record.OrkaSession + "-t1")
	require.Equal(t, "hello turn-a", first.Spec.Prompt)
	time.Sleep(200 * time.Millisecond)
	var tasks corev1alpha1.TaskList
	require.NoError(t, env.kube.List(ctx, &tasks, client.InNamespace(ahpTestNamespace)))
	require.Len(t, tasks.Items, 1, "a message sent while a turn is starting is refused")
}

func TestAHPBackendDecidesAsTheConfirmingConnection(t *testing.T) {
	env := newAHPTestEnvWith(t, ContextTokenAuthorizationConfig{
		Mode: ContextTokenAuthorizationModeEnforce, TaskUpdateScopes: []string{"orka:tasks:update"},
	})
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "manual")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "run it", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, ahphost.UserKey("https://issuer.example", "alice"), ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	env.waitForTask(taskName)

	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateToolCall, ToolCall: &harnessv2.ToolCallUpdate{
		ToolCallID: "tool-1", Name: "bash", Title: "Run", Status: harnessv2.ToolCallStatusInProgress,
	}})
	waitForActionType(t, chatSub, "chat/toolCallReady")
	env.live.Publish(liveevents.Event{Namespace: ahpTestNamespace, Task: taskName, Approval: &liveevents.Approval{
		ID: "approval-1", State: liveevents.ApprovalPending, ToolCallID: "tool-1", ToolName: "bash", Title: "Run",
		Input: json.RawMessage(`{"command":"make release"}`),
	}})
	require.JSONEq(t, `{"command":"make release"}`, ahpReadyInput(t, waitForActionType(t, chatSub, "chat/toolCallReady")))

	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatToolCallConfirmedAction{
		Type: ahptypes.ActionTypeChatToolCallConfirmed, TurnId: "turn-1", ToolCallId: "tool-1", Approved: false,
	}})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		env.mu.Lock()
		defer env.mu.Unlock()
		return len(env.decisions) == 1
	}, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, "alice team "+taskName+" approval-1 decline", env.decisions[0])
}

func TestAHPBackendDisposeKeepsOtherSessions(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "manual")
	provider := "copilot"
	require.NoError(t, c.Request(ctx, "createSession", ahptypes.CreateSessionParams{
		Channel: ahpTestSession + "-more", Provider: &provider, WorkingDirectories: []string{ahpTestDir},
	}, nil))
	require.NoError(t, c.Request(ctx, "disposeSession", ahptypes.DisposeSessionParams{Channel: ahpTestSession}, nil))
	userKey := ahphost.UserKey("https://issuer.example", "alice")
	var kept ahpSession
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, ahpTestSession+"-more", &kept))
	var gone ahpSession
	require.Error(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, ahpTestSession, &gone))
}

func TestAHPBackendKeepsASessionWhoseTaskItCannotCancel(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, err := c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "run", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser},
			Model: &ahptypes.ModelSelection{Id: "claude-haiku-5.5"}},
	}})
	require.NoError(t, err)
	userKey := ahphost.UserKey("https://issuer.example", "alice")
	var record ahpSession
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	task := env.waitForTask(taskName)

	task.Spec.RequestedBy.Subject = "bob"
	require.NoError(t, env.kube.Update(ctx, task))
	require.Error(t, c.Request(ctx, "disposeSession", ahptypes.DisposeSessionParams{Channel: ahpTestSession}, nil))
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, ahpTestSession, &record), "the session is kept")
	require.NotNil(t, env.backend.activeTurn(userKey, ahpTestSession), "the turn still follows its Task")

	require.NoError(t, env.kube.Get(ctx, types.NamespacedName{Namespace: ahpTestNamespace, Name: taskName}, task))
	task.Spec.RequestedBy.Subject = "alice"
	require.NoError(t, env.kube.Update(ctx, task))
	require.NoError(t, c.Request(ctx, "disposeSession", ahptypes.DisposeSessionParams{Channel: ahpTestSession}, nil))
	require.Error(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, ahpTestSession, &record), "the retried disposal ends the session")
	require.NoError(t, env.kube.Get(ctx, types.NamespacedName{Namespace: ahpTestNamespace, Name: taskName}, task))
	require.Contains(t, task.Annotations, labels.AnnotationCancelRequested, "the Task was asked to stop")
}

func TestAHPBackendCancelsOnlyTheTaskItsTurnCreated(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, err := c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "run", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser},
			Model: &ahptypes.ModelSelection{Id: "claude-haiku-5.5"}},
	}})
	require.NoError(t, err)
	alice := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice"), Issuer: "https://issuer.example",
		Subject: "alice", Username: "alice", Namespace: ahpTestNamespace}
	var record ahpSession
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	task := env.waitForTask(record.OrkaSession + "-t1")
	require.NotEmpty(t, task.UID)

	require.NoError(t, env.backend.cancelTask(ctx, alice, task.Namespace, task.Name, "another-task-uid"))
	require.NoError(t, env.kube.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, task))
	require.NotContains(t, task.Annotations, labels.AnnotationCancelRequested, "a Task under another UID is not stopped")

	require.NoError(t, env.backend.cancelTask(ctx, alice, task.Namespace, task.Name, string(task.UID)))
	require.NoError(t, env.kube.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, task))
	require.Contains(t, task.Annotations, labels.AnnotationCancelRequested, "the turn's own Task is stopped")
}

func TestAHPBackendReadsRootFilesFromTheRunningWorkspace(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "allow-all")
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	var record ahpSession
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	before, after := "package widgets\n", "// Reported by the agent.\npackage widgets\n"
	changes := &ahpChanges{b: env.backend, ctx: ctx, principal: alice, session: record, emit: env.backend.emitterFor(alice)}
	changes.setFile("/workspace/src/octo/widgets/widgets.go", &before, &after, false)
	uri := ahpTestDir + "/widgets.go"
	read := func() (ahptypes.ResourceReadResult, error) {
		var result ahptypes.ResourceReadResult
		err := c.Request(ctx, "resourceRead", ahptypes.ResourceReadParams{Channel: ahptypes.RootResourceURI, Uri: uri}, &result)
		return result, err
	}

	result, err := read()
	require.NoError(t, err)
	require.Equal(t, after, result.Data, "without a workspace, the reported contents serve")

	env.backend.readWorkspace = func(context.Context, ahphost.Principal, string) (ahptypes.ResourceReadResult, bool, error) {
		return ahptypes.ResourceReadResult{Data: "// Edited in a terminal.\n", Encoding: ahptypes.ContentEncodingUtf8}, true, nil
	}
	result, err = read()
	require.NoError(t, err)
	require.Equal(t, "// Edited in a terminal.\n", result.Data, "the running workspace's file serves")

	env.backend.readWorkspace = func(context.Context, ahphost.Principal, string) (ahptypes.ResourceReadResult, bool, error) {
		return ahptypes.ResourceReadResult{}, true, ahphost.ErrResourceNotFound
	}
	_, err = read()
	require.Error(t, err, "a file deleted in the running workspace is gone")
}

func TestAHPBackendRefusesASessionOnAReservedScheme(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	require.NoError(t, c.Request(ctx, ahpExtensionCloneProject, map[string]string{"url": ahpTestRepo}, nil))
	provider := "copilot"
	for _, uri := range []string{"ahp-chat:/one", "ahp-changeset:/one/session", ahpContentScheme + "one"} {
		require.Error(t, c.Request(ctx, "createSession", ahptypes.CreateSessionParams{
			Channel: uri, Provider: &provider, WorkingDirectories: []string{ahpTestDir},
		}, nil), uri)
	}
}

func TestAHPBackendRemovesAProjectNoSessionUses(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "manual")
	var listed struct {
		Projects []map[string]any `json:"projects"`
	}
	require.NoError(t, c.Request(ctx, ahpExtensionListProjects, map[string]any{}, &listed))
	require.Len(t, listed.Projects, 1)
	id := listed.Projects[0]["id"]

	require.Error(t, c.Request(ctx, ahpExtensionRemoveProject, map[string]any{"id": id, "deleteClone": true}, nil),
		"the host never deletes a project's files")
	require.Error(t, c.Request(ctx, ahpExtensionRemoveProject, map[string]any{"id": id}, nil),
		"a project a session uses stays")
	var removed struct {
		Removed bool `json:"removed"`
	}
	require.NoError(t, c.Request(ctx, ahpExtensionRemoveProject, map[string]any{"id": "project-unknown"}, &removed))
	require.False(t, removed.Removed)

	require.NoError(t, c.Request(ctx, "disposeSession", ahptypes.DisposeSessionParams{Channel: ahpTestSession}, nil))
	_, rootSub, err := c.Subscribe(ctx, ahptypes.RootResourceURI)
	require.NoError(t, err)
	require.NoError(t, c.Request(ctx, ahpExtensionRemoveProject, map[string]any{"id": id}, &removed))
	require.True(t, removed.Removed)
	waitForActionType(t, rootSub, "root/configChanged")
	require.NoError(t, c.Request(ctx, ahpExtensionListProjects, map[string]any{}, &listed))
	require.Empty(t, listed.Projects)
	root, _, err := c.Subscribe(ctx, ahptypes.RootResourceURI)
	require.NoError(t, err)
	require.JSONEq(t, `{"hostName":"orka","projects":[]}`, string(root.Snapshot.State.Root.Config.Values[ahpCopilotConfigKey]))
	provider := "copilot"
	require.Error(t, c.Request(ctx, "createSession", ahptypes.CreateSessionParams{
		Channel: ahpTestSession + "-again", Provider: &provider, WorkingDirectories: []string{ahpTestDir},
	}, nil), "a removed project takes no new sessions")
}

func TestAHPBackendRootReadHonorsANewerDeletion(t *testing.T) {
	env := newAHPTestEnv(t)
	ctx := ahpCtx(t)
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	put := func(kind, key string, value []byte, at time.Time) {
		t.Helper()
		require.NoError(t, env.backend.store.PutAHPRecord(ctx, store.AHPRecord{UserKey: alice.UserKey, Kind: kind, Key: key, Value: value, UpdatedAt: at}))
	}
	record := func(kind, key string, value any, at time.Time) {
		t.Helper()
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		put(kind, key, raw, at)
	}
	at := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	file := "/workspace/src/octo/widgets/a.go"
	older, newer := ahpHash(alice.UserKey, "ahp-older"), ahpHash(alice.UserKey, "ahp-newer")
	record(ahpRecordSession, "ahp-older", ahpSession{Session: "ahp-older"}, at)
	record(ahpRecordSession, "ahp-newer", ahpSession{Session: "ahp-newer"}, at)
	record(ahpRecordFile, older+"/"+file, ahpFileContent{Path: file, After: "n1"}, at)
	put(ahpRecordContent, older+"/n1", []byte("old contents"), at)
	read := func() (string, error) {
		result, err := env.backend.ReadResource(ctx, alice, "", "", "file://"+file)
		return result.Data, err
	}
	data, err := read()
	require.NoError(t, err)
	require.Equal(t, "old contents", data)

	record(ahpRecordFile, newer+"/"+file, ahpFileContent{Path: file, Before: "n1", Deleted: true}, at.Add(time.Hour))
	_, err = read()
	require.ErrorIs(t, err, ahphost.ErrResourceNotFound, "a newer session deleted the file")

	record(ahpRecordFile, older+"/"+file, ahpFileContent{Path: file, After: "n2"}, at.Add(2*time.Hour))
	put(ahpRecordContent, older+"/n2", []byte("rewritten"), at.Add(2*time.Hour))
	data, err = read()
	require.NoError(t, err)
	require.Equal(t, "rewritten", data, "a write after the deletion is the file's")
}

func TestAHPBackendDefaultBranchBackfillKeepsARemovedProjectRemoved(t *testing.T) {
	env := newAHPTestEnv(t)
	ctx := ahpCtx(t)
	alice := ahphost.UserKey("https://issuer.example", "alice")
	env.backend.backfillDefaultBranch(ctx, alice, "project-removed", "main")
	var project ahpProject
	require.ErrorIs(t, env.backend.getRecord(ctx, alice, ahpRecordProject, "project-removed", &project), store.ErrNotFound)

	require.NoError(t, env.backend.putRecord(ctx, alice, ahpRecordProject, "project-kept", ahpProject{ID: "project-kept", URL: ahpTestRepo}))
	env.backend.backfillDefaultBranch(ctx, alice, "project-kept", "main")
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordProject, "project-kept", &project))
	require.Equal(t, "main", project.DefaultBranch)
	require.Equal(t, ahpTestRepo, project.URL, "the rest of the stored project stays")
	env.backend.backfillDefaultBranch(ctx, alice, "project-kept", "trunk")
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordProject, "project-kept", &project))
	require.Equal(t, "main", project.DefaultBranch, "a branch already found stays")
}

func TestAHPBackendRepeatCloneKeepsARemovedProjectRemoved(t *testing.T) {
	gitSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "git-token", Namespace: ahpTestNamespace}, Data: map[string][]byte{"token": []byte("not-a-real-token")}}
	env := newAHPTestEnv(t, gitSecret)
	ctx := ahpCtx(t)
	alice := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice"), Issuer: "https://issuer.example",
		Subject: "alice", Username: "alice", Namespace: ahpTestNamespace}
	repo := "https://github.com/octo/widgets"
	id := "project-" + ahpHash(repo)[:16]
	require.NoError(t, env.backend.putRecord(ctx, alice.UserKey, ahpRecordProject, id, ahpProject{ID: id, URL: repo, Name: "widgets", Owner: "octo"}))
	env.backend.gitHTTP = &http.Client{Transport: ahpRoundTrip(func(*http.Request) (*http.Response, error) {
		removed, err := env.backend.removeProject(context.Background(), alice, id)
		require.NoError(t, err)
		require.True(t, removed)
		body := ahpTestAdvertisement("main")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	project, err := env.backend.addProject(ctx, alice, repo)
	require.NoError(t, err)
	require.Equal(t, "main", project.DefaultBranch)
	var stored ahpProject
	require.ErrorIs(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordProject, id, &stored), store.ErrNotFound,
		"the late backfill does not recreate the project")
}

func TestAHPBackendRefusesAProjectOnAnotherProjectsPath(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	require.NoError(t, c.Request(ctx, ahpExtensionCloneProject, map[string]string{"url": ahpTestRepo}, nil))
	require.NoError(t, c.Request(ctx, ahpExtensionCloneProject, map[string]string{"url": ahpTestRepo + ".git"}, nil),
		"adding the same repository again returns it")
	err := c.Request(ctx, ahpExtensionCloneProject, map[string]string{"url": "https://git.example.com/octo/widgets"}, nil)
	require.ErrorContains(t, err, "already uses /workspace/src/octo/widgets")
	var listed struct {
		Projects []map[string]any `json:"projects"`
	}
	require.NoError(t, c.Request(ctx, ahpExtensionListProjects, map[string]any{}, &listed))
	require.Len(t, listed.Projects, 1)
	require.Equal(t, ahpTestRepo, listed.Projects[0]["remoteUrl"])
}

func TestAHPBackendSetsASessionsApproveAll(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "manual")
	alice := ahphost.UserKey("https://issuer.example", "alice")
	for _, tc := range []struct {
		enabled bool
		mode    string
	}{{true, "allow-all"}, {false, "manual"}} {
		require.NoError(t, c.Request(ctx, ahpExtensionSetSessionApproveAll, map[string]any{"channel": ahpTestSession, "enabled": tc.enabled}, nil))
		var record ahpSession
		require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &record))
		require.Equal(t, tc.mode, record.ApprovalMode, "the next turn uses the mode")
		result, _, err := c.Subscribe(ctx, ahpTestSession)
		require.NoError(t, err)
		values := result.Snapshot.State.Session.Config.Values
		require.JSONEq(t, `"`+tc.mode+`"`, string(values[ahpConfigApprovalMode]), "clients show the selection")
		require.JSONEq(t, `"`+tc.mode+`"`, string(values[ahpConfigEffectiveMode]))
	}
	require.Error(t, c.Request(ctx, ahpExtensionSetSessionApproveAll, map[string]any{"channel": "ahp-session:/unknown", "enabled": true}, nil))
	require.Error(t, c.Request(ctx, ahpExtensionSetSessionApproveAll, map[string]any{"channel": ahpTestSession}, nil), "enabled is required")
	bob := env.connect("bob")
	require.Error(t, bob.Request(ctx, ahpExtensionSetSessionApproveAll, map[string]any{"channel": ahpTestSession, "enabled": true}, nil),
		"another user's session is unknown")
}

func TestAHPBackendReopensARefusedApproval(t *testing.T) {
	env := newAHPTestEnv(t)
	refusals := 1
	env.backend.decideApproval = func(_ context.Context, _ *UserInfo, _, _, approvalID string, approve bool) error {
		env.mu.Lock()
		defer env.mu.Unlock()
		if refusals > 0 {
			refusals--
			return fiber.NewError(fiber.StatusConflict, "try again")
		}
		env.decisions = append(env.decisions, approvalID+" "+map[bool]string{true: "approve", false: "decline"}[approve])
		return nil
	}
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "manual")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "run it", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, ahphost.UserKey("https://issuer.example", "alice"), ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	env.waitForTask(taskName)
	env.live.Publish(liveevents.Event{Namespace: ahpTestNamespace, Task: taskName, Approval: &liveevents.Approval{
		ID: "approval-1", State: liveevents.ApprovalPending, ToolCallID: "tool-1", ToolName: "bash", Title: "Run",
	}})
	waitForActionType(t, chatSub, "chat/toolCallReady")
	confirm := func() {
		_, err := c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatToolCallConfirmedAction{
			Type: ahptypes.ActionTypeChatToolCallConfirmed, TurnId: "turn-1", ToolCallId: "tool-1", Approved: true,
		}})
		require.NoError(t, err)
	}
	confirm()
	waitForActionType(t, chatSub, "chat/toolCallReady")
	confirm()
	require.Eventually(t, func() bool {
		env.mu.Lock()
		defer env.mu.Unlock()
		return len(env.decisions) == 1 && env.decisions[0] == "approval-1 approve"
	}, 5*time.Second, 20*time.Millisecond)
}

func TestAHPBackendDoesNotReopenAnExpiredApproval(t *testing.T) {
	env := newAHPTestEnv(t)
	userKey := ahphost.UserKey("https://issuer.example", "alice")
	var taskName string
	decided := make(chan struct{})
	env.backend.decideApproval = func(context.Context, *UserInfo, string, string, string, bool) error {
		defer close(decided)
		env.live.Publish(liveevents.Event{Namespace: ahpTestNamespace, Task: taskName, Approval: &liveevents.Approval{
			ID: "approval-1", State: liveevents.ApprovalExpired, ToolCallID: "tool-1",
		}})
		time.Sleep(200 * time.Millisecond)
		return fiber.NewError(fiber.StatusConflict, "the request expired")
	}
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "manual")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "run it", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, ahpTestSession, &record))
	taskName = record.OrkaSession + "-t1"
	env.waitForTask(taskName)
	env.live.Publish(liveevents.Event{Namespace: ahpTestNamespace, Task: taskName, Approval: &liveevents.Approval{
		ID: "approval-1", State: liveevents.ApprovalPending, ToolCallID: "tool-1", ToolName: "bash", Title: "Run",
	}})
	waitForActionType(t, chatSub, "chat/toolCallReady")
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatToolCallConfirmedAction{
		Type: ahptypes.ActionTypeChatToolCallConfirmed, TurnId: "turn-1", ToolCallId: "tool-1", Approved: true,
	}})
	require.NoError(t, err)
	select {
	case <-decided:
	case <-time.After(5 * time.Second):
		t.Fatal("the decision was never made")
	}

	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case event := <-chatSub.Events():
			if action, ok := event.(ahp.SubscriptionEventAction); ok {
				if _, reopened := action.Envelope.Action.Value.(*ahptypes.ChatToolCallReadyAction); reopened {
					t.Fatal("an expired request was asked again")
				}
			}
		case <-deadline:
			return
		}
	}
}

func TestAHPBackendResolvesTheDefaultBranchOnGitHubOnly(t *testing.T) {
	gitSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "git-token", Namespace: ahpTestNamespace}, Data: map[string][]byte{"token": []byte("not-a-real-token")}}
	env := newAHPTestEnv(t, gitSecret)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	var cloned struct {
		Project map[string]any `json:"project"`
	}
	require.NoError(t, c.Request(ctx, ahpExtensionCloneProject, map[string]string{"url": "https://github.com/octo/widgets"}, &cloned))
	require.Equal(t, "main", cloned.Project["defaultBranch"])
	require.Equal(t, true, cloned.Project["git"])
	env.mu.Lock()
	require.Len(t, env.gitRequests, 1)
	request := env.gitRequests[0]
	env.mu.Unlock()
	require.Equal(t, "https://github.com/octo/widgets.git/info/refs?service=git-upload-pack", request.URL.String())
	user, password, ok := request.BasicAuth()
	require.True(t, ok)
	require.Equal(t, "x-access-token", user)
	require.Equal(t, "not-a-real-token", password)

	cloned.Project = nil
	require.NoError(t, c.Request(ctx, ahpExtensionCloneProject, map[string]string{"url": "https://git.example.com/octo/gadgets"}, &cloned))
	require.Nil(t, cloned.Project["defaultBranch"])
	env.mu.Lock()
	require.Len(t, env.gitRequests, 1)
	env.mu.Unlock()
}

func TestAHPAdvertisedHead(t *testing.T) {
	branch, err := ahpAdvertisedHead(strings.NewReader(ahpTestAdvertisement("master")))
	require.NoError(t, err)
	require.Equal(t, "master", branch)
	for _, bad := range []string{"", "zzzz", "0000", "0008abc\n"} {
		_, err := ahpAdvertisedHead(strings.NewReader(bad))
		require.Error(t, err, "%q", bad)
	}
}

func TestAHPBackendSessionsPublishToTheClientsBranch(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	var cloned map[string]any
	require.NoError(t, c.Request(ctx, ahpExtensionCloneProject, map[string]string{"url": ahpTestRepo}, &cloned))
	provider := "copilot"
	for session, branch := range map[string]string{
		"ahp-session:/named": `"feature-x"`, "ahp-session:/bad": `"../escape"`, "ahp-session:/base": `"main"`, "ahp-session:/under": `"main/fix"`,
	} {
		require.NoError(t, c.Request(ctx, "createSession", ahptypes.CreateSessionParams{
			Channel: session, Provider: &provider, WorkingDirectories: []string{ahpTestDir},
			Config: map[string]json.RawMessage{"branch": json.RawMessage(branch), "baseBranch": json.RawMessage(`"main"`)},
		}, nil))
	}
	userKey := ahphost.UserKey("https://issuer.example", "alice")
	var named, bad, base, under ahpSession
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, "ahp-session:/named", &named))
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, "ahp-session:/bad", &bad))
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, "ahp-session:/base", &base))
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, "ahp-session:/under", &under))
	require.Equal(t, "feature-x", named.Branch)
	require.True(t, strings.HasPrefix(bad.Branch, "orka/session-"), "an unsafe branch name is replaced")
	require.True(t, strings.HasPrefix(base.Branch, "orka/session-"), "a session never publishes to its base branch")
	require.True(t, strings.HasPrefix(under.Branch, "orka/session-"), "Git cannot hold a branch under the base branch")
	require.Equal(t, "/workspace/src/octo/widgets", named.WorkDir, "without workspaces the session works in the project path")
}

func TestAHPBackendKeepsWorkspaceBranchesClearOfTheDefault(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	var cloned map[string]any
	require.NoError(t, c.Request(ctx, ahpExtensionCloneProject, map[string]string{"url": ahpTestRepo}, &cloned))
	unreachable := &rest.Config{Host: "http://127.0.0.1:1"}
	clientset, err := kubernetes.NewForConfig(unreachable)
	require.NoError(t, err)
	env.backend.workspaces = &ahpWorkspaces{b: env.backend, clientset: clientset, rest: unreachable, namespace: "orka-ahp-workspaces",
		image: "example.com/git@sha256:" + ahpHash("x"), startWait: 50 * time.Millisecond}
	provider := "copilot"
	for session, branch := range map[string]string{"ahp-session:/under": `"main/fix"`, "ahp-session:/beside": `"develop/fix"`} {
		require.NoError(t, c.Request(ctx, "createSession", ahptypes.CreateSessionParams{
			Channel: session, Provider: &provider, WorkingDirectories: []string{ahpTestDir},
			Config: map[string]json.RawMessage{"branch": json.RawMessage(branch), "baseBranch": json.RawMessage(`"release"`)},
		}, nil))
	}
	userKey := ahphost.UserKey("https://issuer.example", "alice")
	var under, beside ahpSession
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, "ahp-session:/under", &under))
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, "ahp-session:/beside", &beside))
	require.True(t, strings.HasPrefix(under.Branch, "orka/session-"), "Git cannot hold a branch under the clone's default branch")
	require.Equal(t, "develop/fix", beside.Branch)
}

func TestAHPBackendGivesEachBranchOneSession(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	var cloned map[string]any
	require.NoError(t, c.Request(ctx, ahpExtensionCloneProject, map[string]string{"url": ahpTestRepo}, &cloned))
	provider := "copilot"
	create := func(session, branch string) error {
		return c.Request(ctx, "createSession", ahptypes.CreateSessionParams{
			Channel: session, Provider: &provider, WorkingDirectories: []string{ahpTestDir},
			Config: map[string]json.RawMessage{"branch": json.RawMessage(`"` + branch + `"`)},
		}, nil)
	}
	require.NoError(t, create("ahp-session:/first", "feature-x"))
	require.Error(t, create("ahp-session:/second", "feature-x"), "a second session cannot share the branch and its worktree")
	require.Error(t, create("ahp-session:/child", "feature-x/child"), "Git cannot hold a branch under another session's")
	require.NoError(t, create("ahp-session:/parent", "nested/child"))
	require.Error(t, create("ahp-session:/nested", "nested"), "Git cannot hold a branch over another session's")
	require.NoError(t, create("ahp-session:/sibling", "feature-xy"), "a longer name is not a clash")
	require.Equal(t, "/workspace/worktrees/octo/widgets/feature-x", ahpWorktreePath(ahpProject{Owner: "octo", Name: "widgets"}, "feature-x"))
}

func TestAHPBackendShowsPlansUsageAndRunningOutput(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "fix it", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)
	alice := ahphost.UserKey("https://issuer.example", "alice")
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	env.waitForTask(taskName)
	require.Eventually(t, func() bool { return env.backend.activeTurn(alice, ahpTestSession) != nil }, 5*time.Second, 20*time.Millisecond)

	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdatePlan, Plan: &harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{
		{Content: "Read the code", Status: harnessv2.PlanEntryCompleted}, {Content: "Run the tests", Status: harnessv2.PlanEntryInProgress},
	}}})
	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateUsage, Usage: &harnessv2.UsageUpdate{InputTokens: 120, OutputTokens: 30}})
	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateToolCall, ToolCall: &harnessv2.ToolCallUpdate{
		ToolCallID: "tool-1", Name: "bash", Title: "Viewing .../a1b2-123/workspace/pkg/widgets.go", Status: harnessv2.ToolCallStatusInProgress,
		Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "ok  pkg/a\n"}},
	}})
	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateToolCallUpdate, ToolCall: &harnessv2.ToolCallUpdate{
		ToolCallID: "tool-1", Status: harnessv2.ToolCallStatusInProgress,
		Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "ok  pkg/b\n"}},
	}})
	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateToolCallUpdate, ToolCall: &harnessv2.ToolCallUpdate{
		ToolCallID: "tool-1", Status: harnessv2.ToolCallStatusCompleted,
	}})
	waitForActionType(t, chatSub, "chat/toolCallComplete")

	var plan struct {
		Plan struct {
			Exists  bool   `json:"exists"`
			Content string `json:"content"`
		} `json:"plan"`
	}
	require.NoError(t, c.Request(ctx, ahpExtensionGetPlan, map[string]string{"channel": ahpTestSession}, &plan))
	require.True(t, plan.Plan.Exists)
	require.Equal(t, "- [x] Read the code\n- [~] Run the tests\n", plan.Plan.Content)

	snapshot, _, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	active := snapshot.Snapshot.State.Chat.ActiveTurn
	require.NotNil(t, active)
	require.NotNil(t, active.Usage)
	require.Equal(t, int64(120), *active.Usage.InputTokens)
	var calls []string
	for _, part := range active.ResponseParts {
		if p, ok := part.Value.(*ahptypes.ToolCallResponsePart); ok {
			raw, _ := json.Marshal(p.ToolCall)
			calls = append(calls, string(raw))
		}
	}
	require.Len(t, calls, 2, "the plan and the tool call")
	require.Contains(t, calls[0], "Run the tests")
	require.Contains(t, calls[1], `"displayName":"Viewing pkg/widgets.go"`, "runtime-private paths become relative")
	require.Contains(t, calls[1], `ok  pkg/a\nok  pkg/b\n`, "streamed output stays as the result")

	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantMessageChunk, AssistantMessage: &harnessv2.AssistantMessageChunk{Text: "Error: 400 model unavailable"}})
	waitForActionType(t, chatSub, "chat/delta")
	task := &corev1alpha1.Task{}
	require.NoError(t, env.kube.Get(ctx, types.NamespacedName{Namespace: ahpTestNamespace, Name: taskName}, task))
	task.Status.Phase, task.Status.Message = corev1alpha1.TaskPhaseFailed, "prompt failed"
	require.NoError(t, env.kube.Status().Update(ctx, task))
	waitForActionType(t, chatSub, "chat/error")
	final, _, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	turns := final.Snapshot.State.Chat.Turns
	require.Len(t, turns, 1)
	raw, _ := json.Marshal(turns[0])
	require.Contains(t, string(raw), "prompt failed: Error: 400 model unavailable")
	require.Contains(t, string(raw), `"displayName":"Plan"`)
}

func TestAHPRelativeTitle(t *testing.T) {
	for title, want := range map[string]string{
		"Viewing .../abc-123/workspace/README.md":           "Viewing README.md",
		"Viewing …/abc-123/workspace/README.md":             "Viewing README.md",
		"Edit /var/lib/orka/sessions/s1/workspace/pkg/a.go": "Edit pkg/a.go",
		"Run tests in pkg/workspace/":                       "Run tests in pkg/workspace/",
		"apply_patch":                                       "apply_patch",
	} {
		require.Equal(t, want, ahpRelativeTitle(title), title)
	}
}

func TestAHPChooseModel(t *testing.T) {
	choices := []ahpAgentChoice{
		{Agent: "a-opencode", Runtime: corev1alpha1.AgentRuntimeOpencode, Model: "shared"},
		{Agent: "b-claude", Runtime: corev1alpha1.AgentRuntimeClaude, Model: "shared"},
		{Agent: "c-codex", Runtime: corev1alpha1.AgentRuntimeCodex, Model: "codex-only"},
	}
	choice, ok := ahpChooseModel(choices, string(ahpAgentProvider), "shared")
	require.True(t, ok)
	require.Equal(t, "b-claude", choice.Agent, "the offered agent runs a model on the first runtime in listing order, not the first Agent by name")
	choice, ok = ahpChooseModel(choices, string(ahpAgentProvider), "")
	require.True(t, ok)
	require.Equal(t, "b-claude", choice.Agent)
	choice, ok = ahpChooseModel(choices, string(corev1alpha1.AgentRuntimeOpencode), "shared")
	require.True(t, ok)
	require.Equal(t, "a-opencode", choice.Agent, "a session on a runtime's own provider stays on that runtime")
	_, ok = ahpChooseModel(choices, string(corev1alpha1.AgentRuntimeOpencode), "codex-only")
	require.False(t, ok)
	_, ok = ahpChooseModel(choices, string(ahpAgentProvider), "missing")
	require.False(t, ok)
}

func TestAHPBackendSwitchesModelsAcrossRuntimesAndBranches(t *testing.T) {
	gitSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "git-token", Namespace: ahpTestNamespace}, Data: map[string][]byte{"token": []byte("not-a-real-token")}}
	env := newAHPTestEnv(t, gitSecret)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	alice := ahphost.UserKey("https://issuer.example", "alice")

	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "start", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}, Model: &ahptypes.ModelSelection{Id: "claude-sonnet-5"}},
	}})
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &record))
	first := env.waitForTask(record.OrkaSession + "-t1")
	require.Equal(t, "claude-sonnet", first.Spec.AgentRef.Name)

	first.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	first.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{Branch: record.Branch, State: corev1alpha1.TaskDeliveryStateVerifiedExact}
	require.NoError(t, env.kube.Status().Update(ctx, first))
	waitForActionType(t, chatSub, "chat/turnComplete")
	require.Eventually(t, func() bool { return env.backend.activeTurn(alice, ahpTestSession) == nil }, 5*time.Second, 20*time.Millisecond)

	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-2", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "continue", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}, Model: &ahptypes.ModelSelection{Id: "gpt-5.4-nano"}},
	}})
	require.NoError(t, err)
	second := env.waitForTask(record.OrkaSession + "-g2-t2")
	require.Equal(t, "copilot-gpt", second.Spec.AgentRef.Name)
	require.True(t, second.Spec.SessionRef.Create)
	require.Equal(t, record.Branch+"-2", second.Spec.Workspace.PushBranch, "the new Orka Session publishes to a new branch")
	require.Equal(t, record.Branch, second.Spec.Workspace.Branch, "based on the one the old Orka Session published")
	updated := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &updated))
	require.Equal(t, record.Branch+"-2", updated.Branch)
	require.False(t, updated.Published)
	require.Equal(t, record.WorkDir, updated.WorkDir, "without workspaces the session keeps the project directory")
	require.Eventually(t, func() bool {
		snapshot, _, err := c.Subscribe(ctx, chat)
		if err != nil {
			return false
		}
		raw, _ := json.Marshal(snapshot.Snapshot.State.Chat.ActiveTurn)
		return strings.Contains(string(raw), "Switched to gpt-5.4-nano")
	}, 5*time.Second, 20*time.Millisecond)
}

func TestAHPBackendRecordsAPublicationItsReplyWriteLost(t *testing.T) {
	gitSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "git-token", Namespace: ahpTestNamespace}, Data: map[string][]byte{"token": []byte("not-a-real-token")}}
	env := newAHPTestEnv(t, gitSecret)
	env.backend.sessionRetry = time.Millisecond
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	alice := ahphost.UserKey("https://issuer.example", "alice")
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "start", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &record))
	first := env.waitForTask(record.OrkaSession + "-t1")

	var refused atomic.Int32
	failing := &ahpFailingStore{AHPStore: env.backend.store, fail: func(r store.AHPRecord) bool {
		var session ahpSession
		if json.Unmarshal(r.Value, &session) != nil || !session.Published || session.ActiveTurn == "" {
			return false
		}
		refused.Add(1)
		return true
	}}
	env.backend.store = failing
	failing.armed.Store(true)
	first.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	first.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{Branch: record.Branch, State: corev1alpha1.TaskDeliveryStateVerifiedExact}
	require.NoError(t, env.kube.Status().Update(ctx, first))
	waitForActionType(t, chatSub, "chat/turnComplete")
	require.Eventually(t, func() bool {
		var current ahpSession
		return env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &current) == nil && current.ActiveTurn == ""
	}, 5*time.Second, 10*time.Millisecond)
	require.Positive(t, refused.Load(), "the reply's write was refused")
	updated := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &updated))
	require.True(t, updated.Published, "the turn's end records that it published")
}

func TestAHPBackendRunsMessagesSentDuringATurn(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	alice := ahphost.UserKey("https://issuer.example", "alice")
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "first", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &record))
	env.waitForTask(record.OrkaSession + "-t1")
	require.Eventually(t, func() bool { return env.backend.activeTurn(alice, ahpTestSession) != nil }, 5*time.Second, 20*time.Millisecond)

	user := ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}
	for _, pending := range []*ahptypes.ChatPendingMessageSetAction{
		{Type: ahptypes.ActionTypeChatPendingMessageSet, Kind: ahptypes.PendingMessageKindQueued, Id: "q1", Message: ahptypes.Message{Text: "queued", Origin: user}},
		{Type: ahptypes.ActionTypeChatPendingMessageSet, Kind: ahptypes.PendingMessageKindSteering, Id: "s1", Message: ahptypes.Message{Text: "steer", Origin: user}},
	} {
		_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: pending})
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool {
		var current ahpSession
		return env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &current) == nil && len(current.Pending) == 2
	}, 5*time.Second, 20*time.Millisecond)

	var ids []string
	require.Eventually(t, func() bool {
		queued, _, err := c.Subscribe(ctx, chat)
		if err != nil || queued.Snapshot.State.Chat.SteeringMessage != nil {
			return false
		}
		ids = ids[:0]
		for _, p := range queued.Snapshot.State.Chat.QueuedMessages {
			ids = append(ids, p.Id)
		}
		return len(ids) == 2
	}, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, []string{"s1", "q1"}, ids)

	env.setPhase(record.OrkaSession+"-t1", corev1alpha1.TaskPhaseSucceeded)
	second := env.waitForTask(record.OrkaSession + "-t2")
	require.Equal(t, "steer", second.Spec.Prompt)
	waitForActionType(t, chatSub, "chat/pendingMessageRemoved")
	env.setPhase(second.Name, corev1alpha1.TaskPhaseSucceeded)
	third := env.waitForTask(record.OrkaSession + "-t3")
	require.Equal(t, "queued", third.Spec.Prompt)
	env.setPhase(third.Name, corev1alpha1.TaskPhaseSucceeded)
	require.Eventually(t, func() bool { return env.backend.activeTurn(alice, ahpTestSession) == nil }, 5*time.Second, 20*time.Millisecond)

	snapshot, _, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	state := snapshot.Snapshot.State.Chat
	require.Len(t, state.Turns, 3)
	require.Nil(t, state.SteeringMessage)
	require.Empty(t, state.QueuedMessages)
}

type ahpFailingStore struct {
	store.AHPStore
	armed atomic.Bool
	kind  string
	fail  func(store.AHPRecord) bool
}

func (s *ahpFailingStore) PutAHPRecord(ctx context.Context, record store.AHPRecord) error {
	kind := cmp.Or(s.kind, ahpRecordSession)
	if s.armed.Load() && record.Kind == kind && s.fail(record) {
		return errors.New("the store refused the write")
	}
	return s.AHPStore.PutAHPRecord(ctx, record)
}

func TestAHPBackendSessionThatCannotBeRecordedLeavesNothing(t *testing.T) {
	for _, kind := range []string{ahpRecordChat, ahpRecordSession} {
		t.Run(kind, func(t *testing.T) {
			env := newAHPTestEnv(t)
			failing := &ahpFailingStore{AHPStore: env.backend.store, kind: kind, fail: func(store.AHPRecord) bool { return true }}
			env.backend.store = failing
			c := env.connect("alice")
			ctx := ahpCtx(t)
			var cloned struct {
				Project map[string]any `json:"project"`
			}
			require.NoError(t, c.Request(ctx, ahpExtensionCloneProject, map[string]string{"url": ahpTestRepo + ".git"}, &cloned))
			alice := ahphost.UserKey("https://issuer.example", "alice")
			provider := "copilot"
			create := func(session string) error {
				return c.Request(ctx, "createSession", ahptypes.CreateSessionParams{
					Channel: session, Provider: &provider, WorkingDirectories: []string{ahpTestDir},
					Config: map[string]json.RawMessage{"branch": json.RawMessage(`"feature"`)},
				}, nil)
			}

			failing.armed.Store(true)
			require.Error(t, create(ahpTestSession))
			failing.armed.Store(false)
			for _, kind := range []string{ahpRecordChat, ahpRecordSession} {
				records, err := env.backend.store.ListAHPRecords(ctx, alice, kind)
				require.NoError(t, err)
				require.Empty(t, records, "no %s record is left behind", kind)
			}
			require.NoError(t, create("ahp-session:/two"), "the branch is free for another session")
		})
	}
}

type ahpFailingScanStore struct {
	store.AHPStore
	armed atomic.Bool
	scans atomic.Int32
}

func (s *ahpFailingScanStore) ListAHPRecords(ctx context.Context, userKey, kind string) ([]store.AHPRecord, error) {
	if kind == ahpRecordSession {
		s.scans.Add(1)
		if s.armed.Load() {
			return nil, errors.New("the store refused the read")
		}
	}
	return s.AHPStore.ListAHPRecords(ctx, userKey, kind)
}

func TestAHPBackendRecoveryEndsATurnThatNeverStarted(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	require.NoError(t, env.backend.emitterFor(alice).Emit(ctx, chat, ahpUserTurn("turn-lost", "run me")))
	state, err := ahphost.ChannelState(ctx, env.backend.store, alice.UserKey, chat)
	require.NoError(t, err)
	require.NotNil(t, state.Chat.ActiveTurn, "the chat shows the turn started")
	env.backend.mu.Lock()
	delete(env.backend.recovered, alice.UserKey)
	env.backend.mu.Unlock()

	subscribed := make(chan error, 1)
	go func() {
		_, _, err := c.Subscribe(ctx, ahptypes.RootResourceURI)
		subscribed <- err
	}()
	select {
	case err := <-subscribed:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("subscribing to the root after a restart waited on recovery")
	}
	require.Eventually(t, func() bool {
		state, err = ahphost.ChannelState(ctx, env.backend.store, alice.UserKey, chat)
		return err == nil && state.Chat.ActiveTurn == nil
	}, 5*time.Second, 10*time.Millisecond, "the turn that never started is ended")
	var tasks corev1alpha1.TaskList
	require.NoError(t, env.kube.List(ctx, &tasks))
	require.Empty(t, tasks.Items)
}

func TestAHPBackendRecoversMessagesAnIdleSessionLeftWaiting(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "allow-all")
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	require.NoError(t, env.backend.updateSession(ctx, alice.UserKey, ahpTestSession, func(s *ahpSession) {
		s.Pending = []ahpPending{{ID: "left-behind", Kind: string(ahptypes.PendingMessageKindQueued), Version: "v1",
			Message: ahptypes.Message{Text: "run me", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}}}}
	}))
	env.backend.mu.Lock()
	delete(env.backend.recovered, alice.UserKey)
	env.backend.mu.Unlock()

	env.backend.recoverTurns(ctx, alice)
	require.Eventually(t, func() bool {
		var record ahpSession
		return env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record) == nil && len(record.Pending) == 0
	}, 5*time.Second, 20*time.Millisecond, "the waiting message left the queue")
	var tasks corev1alpha1.TaskList
	require.NoError(t, env.kube.List(ctx, &tasks))
	require.Empty(t, tasks.Items, "a message whose sender is gone does not run")
}

func TestAHPBackendRetriesAFailedRecoveryScan(t *testing.T) {
	env := newAHPTestEnv(t)
	failing := &ahpFailingScanStore{AHPStore: env.backend.store}
	failing.armed.Store(true)
	env.backend.store = failing
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}

	env.backend.recoverTurns(ahpCtx(t), alice)
	require.Equal(t, int32(1), failing.scans.Load())
	failing.armed.Store(false)
	env.backend.recoverTurns(ahpCtx(t), alice)
	require.Equal(t, int32(2), failing.scans.Load(), "the failed scan is retried")
	env.backend.recoverTurns(ahpCtx(t), alice)
	require.Equal(t, int32(2), failing.scans.Load(), "a scan that worked is not repeated")
}

func ahpUserTurn(id, text string) ahptypes.StateAction {
	return ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: id, StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: text, Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}}
}

func TestAHPBackendKeepsAMessageItCouldNotTakeOffTheQueue(t *testing.T) {
	env := newAHPTestEnv(t)
	failing := &ahpFailingStore{AHPStore: env.backend.store, fail: func(record store.AHPRecord) bool {
		return !bytes.Contains(record.Value, []byte(`"q1"`))
	}}
	env.backend.store = failing
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	alice := ahphost.UserKey("https://issuer.example", "alice")
	_, err := c.Dispatch(ctx, chat, ahpUserTurn("turn-1", "first"))
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &record))
	env.waitForTask(record.OrkaSession + "-t1")
	require.Eventually(t, func() bool { return env.backend.activeTurn(alice, ahpTestSession) != nil }, 5*time.Second, 20*time.Millisecond)
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatPendingMessageSetAction{
		Type: ahptypes.ActionTypeChatPendingMessageSet, Kind: ahptypes.PendingMessageKindQueued, Id: "q1",
		Message: ahptypes.Message{Text: "queued", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		var current ahpSession
		return env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &current) == nil && len(current.Pending) == 1
	}, 5*time.Second, 20*time.Millisecond)

	failing.armed.Store(true)
	env.setPhase(record.OrkaSession+"-t1", corev1alpha1.TaskPhaseSucceeded)
	require.Eventually(t, func() bool { return env.backend.activeTurn(alice, ahpTestSession) == nil }, 5*time.Second, 20*time.Millisecond)
	require.Never(t, func() bool {
		return env.kube.Get(ctx, types.NamespacedName{Namespace: ahpTestNamespace, Name: record.OrkaSession + "-t2"}, &corev1alpha1.Task{}) == nil
	}, time.Second, 20*time.Millisecond, "a message still in the stored queue started")
	var current ahpSession
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &current))
	require.Len(t, current.Pending, 1)
}

func TestAHPBackendFailsATurnItCannotRecord(t *testing.T) {
	env := newAHPTestEnv(t)
	failing := &ahpFailingStore{AHPStore: env.backend.store, fail: func(record store.AHPRecord) bool {
		return bytes.Contains(record.Value, []byte(`"activeTask":`))
	}}
	env.backend.store = failing
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	alice := ahphost.UserKey("https://issuer.example", "alice")
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &record))

	failing.armed.Store(true)
	_, err = c.Dispatch(ctx, chat, ahpUserTurn("turn-1", "first"))
	require.NoError(t, err)
	waitForActionType(t, chatSub, "chat/error")
	require.Never(t, func() bool {
		return env.kube.Get(ctx, types.NamespacedName{Namespace: ahpTestNamespace, Name: record.OrkaSession + "-t1"}, &corev1alpha1.Task{}) == nil
	}, time.Second, 20*time.Millisecond, "an unrecorded turn created a Task")
	require.Nil(t, env.backend.activeTurn(alice, ahpTestSession))
}

func TestAHPBackendRecordsATurnBeforeItsTask(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	alice := ahphost.UserKey("https://issuer.example", "alice")
	recorded := make(chan string, 1)
	env.onTaskCreate = func(task *corev1alpha1.Task) error {
		var current ahpSession
		if err := env.backend.getRecord(context.Background(), alice, ahpRecordSession, ahpTestSession, &current); err == nil {
			recorded <- current.ActiveTask
		}
		return nil
	}
	_, err := c.Dispatch(ctx, chat, ahpUserTurn("turn-1", "first"))
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &record))
	env.waitForTask(record.OrkaSession + "-t1")
	select {
	case active := <-recorded:
		require.Equal(t, record.OrkaSession+"-t1", active, "the session must name the Task before it is created")
	case <-time.After(5 * time.Second):
		t.Fatal("the turn's Task was created without the session recording it")
	}
}

func TestAHPBackendTakesBackATurnWhoseTaskWasNotCreated(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	alice := ahphost.UserKey("https://issuer.example", "alice")
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &record))
	kept := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: record.OrkaSession + "-t1", Namespace: ahpTestNamespace},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "first"}}
	var refuse atomic.Bool
	refuse.Store(true)
	env.onTaskCreate = func(*corev1alpha1.Task) error {
		if refuse.Load() {
			return apierrors.NewServiceUnavailable("try later")
		}
		return nil
	}
	_, err = c.Dispatch(ctx, chat, ahpUserTurn("turn-1", "first"))
	require.NoError(t, err)
	waitForActionType(t, chatSub, "chat/error")
	require.Eventually(t, func() bool { return env.backend.activeTurn(alice, ahpTestSession) == nil }, 5*time.Second, 20*time.Millisecond)
	refuse.Store(false)
	require.NoError(t, env.kube.Create(ctx, kept))
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &record))
	require.Empty(t, record.ActiveTurn)
	require.Zero(t, record.SessionTurns)
	require.Empty(t, record.History)

	_, err = c.Dispatch(ctx, chat, ahpUserTurn("turn-2", "again"))
	require.NoError(t, err)
	retried := env.waitForTask(record.OrkaSession + "-t2")
	require.Equal(t, "again", retried.Spec.Prompt, "the next turn takes a name of its own")
	require.True(t, retried.Spec.SessionRef.Create, "it is still the Orka Session's first turn")
}

func TestAHPReorderPending(t *testing.T) {
	pending := []ahpPending{{ID: "s", Kind: "steering"}, {ID: "a", Kind: "queued"}, {ID: "b", Kind: "queued"}, {ID: "c", Kind: "queued"}}
	ids := make([]string, 0, len(pending))
	for _, p := range ahpReorderPending(pending, []string{"c", "a"}) {
		ids = append(ids, p.ID)
	}
	require.Equal(t, []string{"s", "c", "a", "b"}, ids)
}

func TestAHPChangesDropAFileThatNoLongerDiffers(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "allow-all")
	principal := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice"), Namespace: ahpTestNamespace}
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, principal.UserKey, ahpRecordSession, ahpTestSession, &record))
	changes := &ahpChanges{b: env.backend, ctx: ctx, principal: principal, session: record, emit: env.backend.emitterFor(principal)}
	dir := ahpTestDir[len("file://"):]

	original, edited := "package a\n", "package a\n// edited\n"
	changes.setFile(dir+"/a.go", &original, &edited, true)
	changes.setFile(dir+"/a.go", &edited, &original, true)
	created := "new\n"
	changes.setFile(dir+"/new.go", nil, &created, true)
	changes.setFile(dir+"/new.go", &created, nil, true)
	kept := "package c\n// kept\n"
	changes.setFile(dir+"/c.go", &original, &kept, true)
	changes.emitCounts()

	require.Len(t, changes.files(), 1)
	snapshot, _, err := c.Subscribe(ctx, record.Changeset)
	require.NoError(t, err)
	files := snapshot.Snapshot.State.Changeset.Files
	require.Len(t, files, 1, "only the file that still differs is listed")
	require.Equal(t, ahpTestDir+"/c.go", files[0].Edit.After.Uri)
}

func TestAHPChangesKeepOnlyTheContentsTheirFilesName(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "allow-all")
	principal := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice"), Namespace: ahpTestNamespace}
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, principal.UserKey, ahpRecordSession, ahpTestSession, &record))
	changes := &ahpChanges{b: env.backend, ctx: ctx, principal: principal, session: record, emit: env.backend.emitterFor(principal)}
	dir := ahpTestDir[len("file://"):]
	stored := func() []string {
		records, err := env.backend.store.ListAHPRecords(ctx, principal.UserKey, ahpRecordContent)
		require.NoError(t, err)
		contents := make([]string, 0, len(records))
		for _, record := range records {
			contents = append(contents, string(record.Value))
		}
		slices.Sort(contents)
		return contents
	}

	v0, v1, v2, v3 := "v0\n", "v1\n", "v2\n", "v3\n"
	changes.change(func() {
		changes.setFile(dir+"/a.go", &v0, &v1, true)
		changes.setFile(dir+"/b.go", nil, &v1, true)
	})
	changes.change(func() { changes.setFile(dir+"/a.go", &v1, &v2, true) })
	require.Equal(t, []string{v0, v1, v2}, stored())
	changes.change(func() {
		for range 5 {
			changes.setFile(dir+"/a.go", &v2, &v3, true)
			changes.setFile(dir+"/a.go", &v3, &v2, true)
		}
	})
	require.Equal(t, []string{v0, v1, v2}, stored(), "versions no file names any more go")
	changes.sync(map[string]ahpGitChange{dir + "/b.go": {After: &v3}})
	require.Equal(t, []string{v3}, stored(), "the branch's files replace the reported ones")
}

type ahpFlakyChangeStore struct {
	store.AHPStore
	failList, failDelete atomic.Bool
}

func (s *ahpFlakyChangeStore) ListAHPRecords(ctx context.Context, userKey, kind string) ([]store.AHPRecord, error) {
	if kind == ahpRecordFile && s.failList.Load() {
		return nil, errors.New("list failed")
	}
	return s.AHPStore.ListAHPRecords(ctx, userKey, kind)
}

func (s *ahpFlakyChangeStore) DeleteAHPRecord(ctx context.Context, userKey, kind, key string) error {
	if kind == ahpRecordContent && s.failDelete.Load() {
		return errors.New("delete failed")
	}
	return s.AHPStore.DeleteAHPRecord(ctx, userKey, kind, key)
}

func TestAHPChangesRetryContentsTheyCouldNotDrop(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "allow-all")
	principal := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice"), Namespace: ahpTestNamespace}
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, principal.UserKey, ahpRecordSession, ahpTestSession, &record))
	flaky := &ahpFlakyChangeStore{AHPStore: env.backend.store}
	env.backend.store = flaky
	newChanges := func() *ahpChanges {
		return &ahpChanges{b: env.backend, ctx: ctx, principal: principal, session: record, emit: env.backend.emitterFor(principal)}
	}
	dir := ahpTestDir[len("file://"):]
	stored := func() []string {
		records, err := flaky.AHPStore.ListAHPRecords(ctx, principal.UserKey, ahpRecordContent)
		require.NoError(t, err)
		contents := make([]string, 0, len(records))
		for _, record := range records {
			contents = append(contents, string(record.Value))
		}
		slices.Sort(contents)
		return contents
	}
	v0, v1, v2, v3 := "v0\n", "v1\n", "v2\n", "v3\n"
	newChanges().change(func() { newChanges().setFile(dir+"/a.go", &v0, &v1, false) })
	require.Equal(t, []string{v0, v1}, stored())

	flaky.failList.Store(true)
	changes := newChanges()
	changes.change(func() { changes.setFile(dir+"/a.go", &v1, &v2, false) })
	require.Equal(t, []string{v0, v1, v2}, stored(), "nothing is dropped while the files cannot be listed")
	flaky.failList.Store(false)
	flaky.failDelete.Store(true)
	changes = newChanges()
	changes.change(func() { changes.setFile(dir+"/a.go", &v2, &v3, false) })
	require.Equal(t, []string{v0, v1, v2, v3}, stored(), "nothing is dropped while deletes fail")
	flaky.failDelete.Store(false)
	changes = newChanges()
	changes.change(func() {})
	require.Equal(t, []string{v2, v3}, stored(), "the next change drops what earlier ones could not")
}

type ahpFlakyEmitter struct {
	ahpActionsEmitter
	fail atomic.Bool
}

func (e *ahpFlakyEmitter) Emit(ctx context.Context, channel string, actions ...ahptypes.StateAction) error {
	if e.fail.Load() {
		return errors.New("journal unavailable")
	}
	return e.ahpActionsEmitter.Emit(ctx, channel, actions...)
}

func TestAHPChangesShowAgainWhatClientsMissed(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "allow-all")
	principal := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice"), Namespace: ahpTestNamespace}
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, principal.UserKey, ahpRecordSession, ahpTestSession, &record))
	emitter := &ahpFlakyEmitter{}
	newChanges := func() *ahpChanges {
		return &ahpChanges{b: env.backend, ctx: ctx, principal: principal, session: record, emit: emitter}
	}
	shown := func() map[string]string {
		emitter.mu.Lock()
		defer emitter.mu.Unlock()
		state := map[string]string{}
		for _, action := range emitter.actions {
			switch a := action.Value.(type) {
			case *ahptypes.ChangesetFileSetAction:
				state[a.File.Id] = *a.File.Edit.After.Content.Nonce
			case *ahptypes.ChangesetFileRemovedAction:
				delete(state, a.FileId)
			}
		}
		return state
	}
	dir := ahpTestDir[len("file://"):]
	v0, v1, v2 := "v0\n", "v1\n", "v2\n"
	a, b := ahpHash(dir + "/a.go")[:16], ahpHash(dir + "/b.go")[:16]

	emitter.fail.Store(true)
	changes := newChanges()
	changes.change(func() {
		changes.setFile(dir+"/a.go", &v0, &v1, false)
		changes.setFile(dir+"/b.go", &v0, &v1, false)
	})
	require.Empty(t, shown())
	emitter.fail.Store(false)
	changes = newChanges()
	changes.change(func() { changes.setFile(dir+"/b.go", &v1, &v0, true) })
	require.Equal(t, map[string]string{a: "sha256:" + ahpHash(v1)}, shown(),
		"the missed a.go is shown as stored, and b.go, edited back, is not")

	emitter.fail.Store(true)
	changes = newChanges()
	changes.change(func() { changes.setFile(dir+"/a.go", &v1, &v2, false) })
	emitter.fail.Store(false)
	changes = newChanges()
	changes.change(func() {})
	require.Equal(t, map[string]string{a: "sha256:" + ahpHash(v2)}, shown())
	require.NotContains(t, shown(), b, "b.go no longer differs")
}

func TestAHPChangesFollowTheBranch(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "allow-all")
	principal := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice"), Namespace: ahpTestNamespace}
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, principal.UserKey, ahpRecordSession, ahpTestSession, &record))
	changes := &ahpChanges{b: env.backend, ctx: ctx, principal: principal, session: record, emit: env.backend.emitterFor(principal)}

	before, reported := "package a\n", "package a\n// reported\n"
	changes.setFile(ahpTestDir[len("file://"):]+"/a.go", &before, &reported, true)
	gone, edited := "x\n", "package b\n// shell edit\n"
	changes.sync(map[string]ahpGitChange{
		ahpTestDir[len("file://"):] + "/b.go":   {After: &edited},
		ahpTestDir[len("file://"):] + "/old.md": {Before: &gone},
	})

	snapshot, _, err := c.Subscribe(ctx, record.Changeset)
	require.NoError(t, err)
	files := snapshot.Snapshot.State.Changeset.Files
	require.Len(t, files, 2, "the reported edit the branch does not hold is gone")
	byURI := map[string]ahptypes.FileEdit{}
	for _, file := range files {
		uri := ""
		if file.Edit.After != nil {
			uri = file.Edit.After.Uri
		} else {
			uri = file.Edit.Before.Uri
		}
		byURI[uri] = file.Edit
	}
	added := byURI[ahpTestDir+"/b.go"]
	require.Nil(t, added.Before, "a new file has no before side")
	require.Equal(t, int64(2), *added.Diff.Added)
	deleted := byURI[ahpTestDir+"/old.md"]
	require.Nil(t, deleted.After, "a deleted file has no after side")
	require.Equal(t, int64(1), *deleted.Diff.Removed)
	var read ahptypes.ResourceReadResult
	require.NoError(t, c.Request(ctx, "resourceRead", ahptypes.ResourceReadParams{Channel: record.Changeset, Uri: deleted.Before.Content.Uri}, &read))
	require.Equal(t, gone, read.Data)
}

func TestAHPBackendDropsWaitingMessagesWithoutTheirSender(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	alice := ahphost.UserKey("https://issuer.example", "alice")
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "first", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &record))
	env.waitForTask(record.OrkaSession + "-t1")
	require.Eventually(t, func() bool { return env.backend.activeTurn(alice, ahpTestSession) != nil }, 5*time.Second, 20*time.Millisecond)
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatPendingMessageSetAction{
		Type: ahptypes.ActionTypeChatPendingMessageSet, Kind: ahptypes.PendingMessageKindQueued, Id: "q1",
		Message: ahptypes.Message{Text: "later", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		env.backend.mu.Lock()
		defer env.backend.mu.Unlock()
		return len(env.backend.pendingAuth) == 1
	}, 5*time.Second, 20*time.Millisecond)
	env.backend.mu.Lock()
	for key, sender := range env.backend.pendingAuth {
		sender.version = "another version"
		env.backend.pendingAuth[key] = sender
	}
	env.backend.mu.Unlock()

	env.setPhase(record.OrkaSession+"-t1", corev1alpha1.TaskPhaseSucceeded)
	waitForActionType(t, chatSub, "chat/error")
	snapshot, _, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	turns := snapshot.Snapshot.State.Chat.Turns
	require.Len(t, turns, 2)
	raw, _ := json.Marshal(turns[1])
	require.Contains(t, string(raw), "the connection that sent this message is gone")
	err = env.kube.Get(ctx, types.NamespacedName{Namespace: ahpTestNamespace, Name: record.OrkaSession + "-t2"}, &corev1alpha1.Task{})
	require.True(t, apierrors.IsNotFound(err), "the message never ran")
}

func TestAHPBackendSwitchKeepsTheApprovalMode(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	alice := ahphost.UserKey("https://issuer.example", "alice")
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "first", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}, Model: &ahptypes.ModelSelection{Id: "claude-sonnet-5"}},
	}})
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &record))
	first := env.waitForTask(record.OrkaSession + "-t1")
	require.True(t, first.Spec.AgentRuntime == nil || first.Spec.AgentRuntime.PermissionDecisions == "")
	env.setPhase(first.Name, corev1alpha1.TaskPhaseSucceeded)
	waitForActionType(t, chatSub, "chat/turnComplete")
	require.Eventually(t, func() bool { return env.backend.activeTurn(alice, ahpTestSession) == nil }, 5*time.Second, 20*time.Millisecond)

	_, err = c.Dispatch(ctx, ahpTestSession, ahptypes.StateAction{Value: &ahptypes.SessionConfigChangedAction{
		Type: ahptypes.ActionTypeSessionConfigChanged, Config: map[string]json.RawMessage{"approvalMode": json.RawMessage(`"manual"`)},
	}})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		var current ahpSession
		return env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &current) == nil && current.ApprovalMode == "manual"
	}, 5*time.Second, 20*time.Millisecond)
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-2", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "second", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}, Model: &ahptypes.ModelSelection{Id: "gpt-5.4-nano"}},
	}})
	require.NoError(t, err)
	second := env.waitForTask(record.OrkaSession + "-g2-t2")
	require.NotNil(t, second.Spec.AgentRuntime)
	require.Equal(t, corev1alpha1.PermissionDecisionsRequester, second.Spec.AgentRuntime.PermissionDecisions, "the switched turn asks the person")
	var current ahpSession
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &current))
	require.Equal(t, "manual", current.ApprovalMode)
	require.Equal(t, 2, current.Generation)
}

func TestAHPFileURIsEscapeAndRoundTrip(t *testing.T) {
	for _, name := range []string{"a#b.go", "a?b.go", "a%20b.go", "a b.go", "plain.go"} {
		file := "/workspace/src/octo/widgets/" + name
		uri := ahpDirectoryURI(file)
		parsed, err := url.Parse(uri)
		require.NoError(t, err, uri)
		require.Equal(t, file, parsed.Path, "a client reads the name %q back from %s", name, uri)
		require.Empty(t, parsed.RawQuery+parsed.Fragment, uri)
		got, ok := ahpWorkspacePath(uri)
		require.True(t, ok, uri)
		require.Equal(t, file, got)
	}
	require.Equal(t, "file:///workspace/src/octo/widgets", ahpDirectoryURI("/workspace/src/octo/widgets"))
	for _, bad := range []string{"file:///workspace/a?b", "file:///workspace/a#b", "file://host/workspace/a", "https://example.com/workspace", "file:///workspace/%00"} {
		_, ok := ahpWorkspacePath(bad)
		require.False(t, ok, bad)
	}
}

func TestAHPBackendRunsNoTurnForADisposedSession(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "allow-all")
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	var record ahpSession
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	require.NoError(t, c.Request(ctx, "disposeSession", ahptypes.DisposeSessionParams{Channel: ahpTestSession}, nil))

	require.True(t, env.backend.reserveTurn(alice.UserKey, ahpTestSession))
	env.backend.startTurn(ctx, alice, record, &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-late", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "late", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}, env.backend.emitterFor(alice))
	var tasks corev1alpha1.TaskList
	require.NoError(t, env.kube.List(ctx, &tasks))
	require.Empty(t, tasks.Items, "no Task runs for a disposed session")
	require.Nil(t, env.backend.activeTurn(alice.UserKey, ahpTestSession))
}

type ahpSessionReadStore struct {
	store.AHPStore
	mu   sync.Mutex
	next func(store.AHPRecord, error) (store.AHPRecord, error)
}

func (s *ahpSessionReadStore) GetAHPRecord(ctx context.Context, userKey, kind, key string) (store.AHPRecord, error) {
	record, err := s.AHPStore.GetAHPRecord(ctx, userKey, kind, key)
	if kind != ahpRecordSession {
		return record, err
	}
	s.mu.Lock()
	next := s.next
	s.next = nil
	s.mu.Unlock()
	if next != nil {
		return next(record, err)
	}
	return record, err
}

func TestAHPBackendStartsNoTurnWithAStaleApprovalMode(t *testing.T) {
	for name, next := range map[string]func(store.AHPRecord, error) (store.AHPRecord, error){
		"unreadable session": func(store.AHPRecord, error) (store.AHPRecord, error) {
			return store.AHPRecord{}, errors.New("the store refused the read")
		},
		"mode changed after the read": func(record store.AHPRecord, err error) (store.AHPRecord, error) {
			var stale ahpSession
			if err != nil || json.Unmarshal(record.Value, &stale) != nil {
				return record, err
			}
			stale.ApprovalMode = ahpApprovalAllowAll
			record.Value, err = json.Marshal(stale)
			return record, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := newAHPTestEnv(t)
			c := env.connect("alice")
			ctx := ahpCtx(t)
			env.setUpSession(c, "manual")
			reads := &ahpSessionReadStore{AHPStore: env.backend.store}
			env.backend.store = reads
			alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
				UserKey: ahphost.UserKey("https://issuer.example", "alice")}
			var record ahpSession
			require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
			require.Equal(t, ahpApprovalManual, record.ApprovalMode)

			reads.mu.Lock()
			reads.next = next
			reads.mu.Unlock()
			require.True(t, env.backend.reserveTurn(alice.UserKey, ahpTestSession))
			env.backend.startTurn(ctx, alice, record, &ahptypes.ChatTurnStartedAction{
				Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
				Message: ahptypes.Message{Text: "run", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser},
					Model: &ahptypes.ModelSelection{Id: "claude-haiku-5.5"}},
			}, env.backend.emitterFor(alice))
			var tasks corev1alpha1.TaskList
			require.NoError(t, env.kube.List(ctx, &tasks))
			require.Empty(t, tasks.Items, "no Task runs under a stale approval mode")
			require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
			require.Empty(t, record.ActiveTurn)
		})
	}
}

func TestAHPBackendUsesTheStoredResultAfterLiveUpdatesLag(t *testing.T) {
	env := newAHPTestEnv(t)
	env.live = liveevents.NewBroker(1)
	env.backend.live = env.live
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	userKey := ahphost.UserKey("https://issuer.example", "alice")
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "explain", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser},
			Model: &ahptypes.ModelSelection{Id: "claude-haiku-5.5"}},
	}})
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	task := env.waitForTask(taskName)
	var turn *ahpTurn
	require.Eventually(t, func() bool { turn = env.backend.activeTurn(userKey, ahpTestSession); return turn != nil },
		5*time.Second, 20*time.Millisecond)
	chunk := func(text string) *harnessv2.UpdateEvent {
		return &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantMessageChunk, AssistantMessage: &harnessv2.AssistantMessageChunk{Text: text}}
	}
	env.publish(taskName, chunk("The first part"))
	waitForActionType(t, chatSub, "chat/delta")

	turn.mu.Lock()
	for range 3 {
		env.publish(taskName, chunk(" lost"))
	}
	turn.mu.Unlock()
	require.Eventually(t, func() bool { turn.mu.Lock(); defer turn.mu.Unlock(); return turn.lagged }, 5*time.Second, 20*time.Millisecond)

	answer := "The first part, and the rest of the answer."
	require.NoError(t, env.backend.h.resultStore.SaveResult(ctx, ahpTestNamespace, taskName, []byte(answer)))
	require.NoError(t, env.kube.Get(ctx, types.NamespacedName{Namespace: ahpTestNamespace, Name: taskName}, task))
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	require.NoError(t, env.kube.Status().Update(ctx, task))
	waitForActionType(t, chatSub, "chat/turnComplete")
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, ahpTestSession, &record))
	require.NotEmpty(t, record.History)
	require.Equal(t, answer, record.History[len(record.History)-1].Reply)
}

func ahpTurnText(parts []ahptypes.ResponsePart) string {
	var text strings.Builder
	for _, part := range parts {
		if markdown, ok := part.Value.(*ahptypes.MarkdownResponsePart); ok {
			text.WriteString(markdown.Content)
		}
	}
	return text.String()
}

func TestAHPBackendRecoveryDoesNotRepeatAStreamedAnswer(t *testing.T) {
	for _, test := range []struct{ streamed, answer, want string }{
		{streamed: "woke-up", answer: "woke-up", want: "woke-up"},
		{streamed: "42 is not the result", answer: "42", want: "42 is not the result42"},
	} {
		t.Run(test.streamed, func(t *testing.T) { testAHPRecoveredAnswer(t, test.streamed, test.answer, test.want) })
	}
}

func testAHPRecoveredAnswer(t *testing.T, streamed, answer, want string) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	_, err = c.Dispatch(ctx, chat, ahpUserTurn("turn-1", "wake me"))
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	task := env.waitForTask(taskName)
	var turn *ahpTurn
	require.Eventually(t, func() bool { turn = env.backend.activeTurn(alice.UserKey, ahpTestSession); return turn != nil },
		5*time.Second, 20*time.Millisecond)
	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantMessageChunk, AssistantMessage: &harnessv2.AssistantMessageChunk{Text: streamed}})
	waitForActionType(t, chatSub, "chat/delta")

	env.backend.forgetTurn(turn)
	env.backend.mu.Lock()
	delete(env.backend.recovered, alice.UserKey)
	env.backend.mu.Unlock()
	require.NoError(t, env.backend.h.resultStore.SaveResult(ctx, ahpTestNamespace, taskName, []byte(answer)))
	require.NoError(t, env.kube.Get(ctx, types.NamespacedName{Namespace: ahpTestNamespace, Name: taskName}, task))
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	require.NoError(t, env.kube.Status().Update(ctx, task))

	env.backend.recoverTurns(ctx, alice)
	waitForActionType(t, chatSub, "chat/turnComplete")
	state, err := ahphost.ChannelState(ctx, env.backend.store, alice.UserKey, chat)
	require.NoError(t, err)
	turns := state.Chat.Turns
	require.Len(t, turns, 1)
	require.Equal(t, want, ahpTurnText(turns[0].ResponseParts))
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	require.Equal(t, answer, record.History[len(record.History)-1].Reply)
}

func TestAHPBackendRecoveryWaitsForSessionChanges(t *testing.T) {
	env := newAHPTestEnv(t)
	scans := &ahpFailingScanStore{AHPStore: env.backend.store}
	env.backend.store = scans
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}

	unlock := env.backend.lockUser(alice.UserKey)
	done := make(chan struct{}, 2)
	for range 2 {
		go func() { env.backend.PrepareConnection(ahpCtx(t), alice); done <- struct{}{} }()
	}
	select {
	case <-done:
		t.Fatal("recovery ran while the user's sessions were changing")
	case <-time.After(100 * time.Millisecond):
	}
	require.Zero(t, scans.scans.Load())
	unlock()
	for range 2 {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("recovery did not finish once the change did")
		}
	}
	require.Equal(t, int32(1), scans.scans.Load(), "the second connection waits for the first recovery instead of scanning")
}

func TestAHPBackendRecoversTurnsForAClientThatSkipsTheRoot(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	_, err := c.Dispatch(ctx, chat, ahpUserTurn("turn-1", "wake me"))
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	task := env.waitForTask(taskName)
	var turn *ahpTurn
	require.Eventually(t, func() bool { turn = env.backend.activeTurn(alice.UserKey, ahpTestSession); return turn != nil },
		5*time.Second, 20*time.Millisecond)

	env.backend.forgetTurn(turn)
	env.backend.mu.Lock()
	delete(env.backend.recovered, alice.UserKey)
	env.backend.mu.Unlock()
	require.NoError(t, env.backend.h.resultStore.SaveResult(ctx, ahpTestNamespace, taskName, []byte("done")))
	require.NoError(t, env.kube.Get(ctx, types.NamespacedName{Namespace: ahpTestNamespace, Name: taskName}, task))
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	require.NoError(t, env.kube.Status().Update(ctx, task))

	again := env.connectWith("alice", "client-alice-again")
	_, chatSub, err := again.Subscribe(ctx, chat)
	require.NoError(t, err)
	waitForActionType(t, chatSub, "chat/turnComplete")
	require.Eventually(t, func() bool {
		var settled ahpSession
		return env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &settled) == nil && settled.ActiveTurn == ""
	}, 5*time.Second, 20*time.Millisecond, "the recovered turn settled")
}

func TestAHPBackendRetriesDrainingAfterAStartItCouldNotShow(t *testing.T) {
	env := newAHPTestEnv(t)
	env.backend.sessionRetry = time.Millisecond
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "allow-all")
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	key := ahpPendingKey(alice.UserKey, ahpTestSession, "waiting")
	env.backend.mu.Lock()
	env.backend.pendingAuth = map[string]ahpPendingSender{key: {principal: alice, version: "v1"}}
	env.backend.mu.Unlock()
	require.NoError(t, env.backend.updateSession(ctx, alice.UserKey, ahpTestSession, func(s *ahpSession) {
		s.Pending = []ahpPending{{ID: "waiting", Kind: string(ahptypes.PendingMessageKindQueued), Version: "v1",
			Message: ahptypes.Message{Text: "run me", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}}}}
	}))
	refusals := &atomic.Int32{}
	refusals.Store(3)
	emit := ahpRefusingStartEmitter{Emitter: env.backend.emitterFor(alice), refusals: refusals}
	var restoreRefusals atomic.Int32
	failing := &ahpFailingStore{AHPStore: env.backend.store, fail: func(record store.AHPRecord) bool {
		var session ahpSession
		if json.Unmarshal(record.Value, &session) != nil || len(session.Pending) == 0 {
			return false
		}
		return restoreRefusals.Add(1) <= 2
	}}
	env.backend.store = failing
	failing.armed.Store(true)

	env.backend.runPending(ctx, alice.UserKey, ahpTestSession, emit)
	require.Eventually(t, func() bool {
		var tasks corev1alpha1.TaskList
		return env.kube.List(ctx, &tasks) == nil && len(tasks.Items) == 1
	}, 5*time.Second, 10*time.Millisecond, "the waiting message runs once its start can be shown")
	require.LessOrEqual(t, refusals.Load(), int32(0))
	require.Greater(t, restoreRefusals.Load(), int32(2), "putting the message back was refused, then tried again")
}

type ahpRefusingStartEmitter struct {
	ahphost.Emitter
	refusals *atomic.Int32
}

func (e ahpRefusingStartEmitter) Emit(ctx context.Context, channel string, actions ...ahptypes.StateAction) error {
	for _, action := range actions {
		if _, ok := action.Value.(*ahptypes.ChatTurnStartedAction); ok && e.refusals.Add(-1) >= 0 {
			return errors.New("the journal refused the write")
		}
	}
	return e.Emitter.Emit(ctx, channel, actions...)
}

type ahpRefusingDeltaEmitter struct {
	ahphost.Emitter
	armed *atomic.Bool
}

func (e ahpRefusingDeltaEmitter) Emit(ctx context.Context, channel string, actions ...ahptypes.StateAction) error {
	for _, action := range actions {
		if _, ok := action.Value.(*ahptypes.ChatDeltaAction); ok && e.armed.CompareAndSwap(true, false) {
			return errors.New("the journal refused the write")
		}
	}
	return e.Emitter.Emit(ctx, channel, actions...)
}

func TestAHPBackendShowsTheStoredAnswerAfterARefusedDelta(t *testing.T) {
	env := newAHPTestEnv(t)
	var refuse atomic.Bool
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	userKey := ahphost.UserKey("https://issuer.example", "alice")
	_, err = c.Dispatch(ctx, chat, ahpUserTurn("turn-1", "explain"))
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	task := env.waitForTask(taskName)
	var turn *ahpTurn
	require.Eventually(t, func() bool { turn = env.backend.activeTurn(userKey, ahpTestSession); return turn != nil }, 5*time.Second, 20*time.Millisecond)
	turn.mu.Lock()
	turn.emit = ahpRefusingDeltaEmitter{Emitter: turn.emit, armed: &refuse}
	turn.mu.Unlock()
	chunk := func(text string) *harnessv2.UpdateEvent {
		return &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantMessageChunk, AssistantMessage: &harnessv2.AssistantMessageChunk{Text: text}}
	}
	env.publish(taskName, chunk("The first part"))
	waitForActionType(t, chatSub, "chat/delta")
	refuse.Store(true)
	env.publish(taskName, chunk(", the middle"))
	require.Eventually(t, func() bool { return !refuse.Load() }, 5*time.Second, 10*time.Millisecond)
	env.publish(taskName, chunk(" and the end."))
	waitForActionType(t, chatSub, "chat/delta")

	answer := "The first part, the middle and the end."
	require.NoError(t, env.backend.h.resultStore.SaveResult(ctx, ahpTestNamespace, taskName, []byte(answer)))
	require.NoError(t, env.kube.Get(ctx, types.NamespacedName{Namespace: ahpTestNamespace, Name: taskName}, task))
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	require.NoError(t, env.kube.Status().Update(ctx, task))
	waitForActionType(t, chatSub, "chat/turnComplete")
	state, err := ahphost.ChannelState(ctx, env.backend.store, userKey, chat)
	require.NoError(t, err)
	require.Contains(t, ahpTurnText(state.Chat.Turns[0].ResponseParts), answer, "the whole answer is shown")
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, ahpTestSession, &record))
	require.Equal(t, answer, record.History[len(record.History)-1].Reply)
}

func TestAHPBackendShowsAPermissionRequestHeldWhileLiveUpdatesLagged(t *testing.T) {
	env := newAHPTestEnv(t)
	env.live = liveevents.NewBroker(1)
	env.backend.live = env.live
	approvalEvents := storetest.NewFakeExecutionEventStore()
	env.backend.h.executionEventStore = approvalEvents
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "manual")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	userKey := ahphost.UserKey("https://issuer.example", "alice")
	_, err = c.Dispatch(ctx, chat, ahpUserTurn("turn-1", "run the tests"))
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	env.waitForTask(taskName)
	var turn *ahpTurn
	require.Eventually(t, func() bool { turn = env.backend.activeTurn(userKey, ahpTestSession); return turn != nil },
		5*time.Second, 20*time.Millisecond)

	content, err := json.Marshal(map[string]any{
		"approvalID": "approval-held", "kind": approvals.KindPermission, "action": "Allow the agent to run bash",
		"riskSummary": "Run the tests", "targetTool": "bash", "toolCallID": "tool-held",
		"expiresAt": time.Now().Add(5 * time.Minute),
		"options":   []approvals.Option{{OptionID: "allow", Kind: "allow_once"}, {OptionID: "reject", Kind: "reject_once"}},
	})
	require.NoError(t, err)
	_, err = approvalEvents.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace: ahpTestNamespace, StreamType: events.ExecutionEventStreamTypeTask, StreamID: taskName, TaskName: taskName,
		Type: events.ExecutionEventTypeApprovalRequested, ToolCallID: "approval-held", Content: content,
	})
	require.NoError(t, err)
	turn.mu.Lock()
	for range 3 {
		env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantMessageChunk, AssistantMessage: &harnessv2.AssistantMessageChunk{Text: " lost"}})
	}
	turn.mu.Unlock()

	ready := waitForActionType(t, chatSub, "chat/toolCallReady")
	var action ahptypes.ChatToolCallReadyAction
	require.NoError(t, json.Unmarshal(ready, &action))
	require.Equal(t, "tool-held", action.ToolCallId)
	require.NotNil(t, action.ConfirmationTitle, "the held request is shown for the person's decision")
	require.Nil(t, action.ToolInput)
	require.Len(t, action.Options, 1)
	require.Equal(t, ahptypes.ConfirmationOptionKindDeny, action.Options[0].Kind)
	require.NotNil(t, action.ConfirmationTitle.Plain)
	require.Contains(t, *action.ConfirmationTitle.Plain, ahpDenyOnlyNote)

	confirm := func(approved bool) {
		_, err := c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatToolCallConfirmedAction{
			Type: ahptypes.ActionTypeChatToolCallConfirmed, TurnId: "turn-1", ToolCallId: "tool-held", Approved: approved,
		}})
		require.NoError(t, err)
	}
	confirm(true)
	reopened := waitForActionType(t, chatSub, "chat/toolCallReady")
	require.NoError(t, json.Unmarshal(reopened, &action))
	require.Len(t, action.Options, 1, "an approval it did not offer is refused and the request asked again")
	env.mu.Lock()
	require.Empty(t, env.decisions, "nothing was approved")
	env.mu.Unlock()
	confirm(false)
	require.Eventually(t, func() bool {
		env.mu.Lock()
		defer env.mu.Unlock()
		return len(env.decisions) == 1 && strings.HasSuffix(env.decisions[0], "approval-held decline")
	}, 5*time.Second, 20*time.Millisecond, "the denial reaches the Task")
}

func TestAHPBackendOffersApprovalForARecoveredRequestWithKnownInput(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "manual")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	userKey := ahphost.UserKey("https://issuer.example", "alice")
	_, err = c.Dispatch(ctx, chat, ahpUserTurn("turn-1", "run the tests"))
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, ahpTestSession, &record))
	env.waitForTask(record.OrkaSession + "-t1")
	var turn *ahpTurn
	require.Eventually(t, func() bool { turn = env.backend.activeTurn(userKey, ahpTestSession); return turn != nil },
		5*time.Second, 20*time.Millisecond)
	turn.mu.Lock()
	call := turn.startTool("tool-known", "bash", "Run the tests")
	call.input = json.RawMessage(`{"command":"go test ./..."}`)
	turn.onApprovalFrom(&liveevents.Approval{ID: "approval-known", State: liveevents.ApprovalPending, ToolCallID: "tool-known",
		ToolName: "bash", Title: "Run the tests"}, true)
	turn.mu.Unlock()
	ready := waitForActionType(t, chatSub, "chat/toolCallReady")
	var action ahptypes.ChatToolCallReadyAction
	require.NoError(t, json.Unmarshal(ready, &action))
	require.Len(t, action.Options, 2)
	require.NotNil(t, action.ToolInput)
	require.Contains(t, *action.ToolInput.Inline, "go test")
}

type ahpConfigEmitter struct {
	mu      sync.Mutex
	configs []map[string]json.RawMessage
}

func (e *ahpConfigEmitter) Emit(_ context.Context, _ string, actions ...ahptypes.StateAction) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, action := range actions {
		if changed, ok := action.Value.(*ahptypes.SessionConfigChangedAction); ok {
			e.configs = append(e.configs, changed.Config)
		}
	}
	return nil
}

func TestAHPBackendShowsTheApprovalModeTurnsRunUnder(t *testing.T) {
	env := newAHPTestEnv(t)
	env.backend.modeBackoff = wait.Backoff{Duration: time.Millisecond, Factor: 1, Steps: 3}
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "allow-all")
	failing := &ahpFailingStore{AHPStore: env.backend.store, fail: func(record store.AHPRecord) bool {
		return bytes.Contains(record.Value, []byte(`"approvalMode":"manual"`))
	}}
	failing.armed.Store(true)
	env.backend.store = failing
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	shown := &ahpConfigEmitter{}
	env.backend.HandleClientAction(ctx, alice, ahpTestSession, ahptypes.StateAction{Value: &ahptypes.SessionConfigChangedAction{
		Type: ahptypes.ActionTypeSessionConfigChanged, Config: map[string]json.RawMessage{ahpConfigApprovalMode: json.RawMessage(`"manual"`)},
	}}, shown)

	var record ahpSession
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	require.Equal(t, ahpApprovalAllowAll, record.ApprovalMode)
	shown.mu.Lock()
	defer shown.mu.Unlock()
	require.NotEmpty(t, shown.configs, "the client is shown the mode turns run under")
	last := shown.configs[len(shown.configs)-1]
	require.JSONEq(t, `"allow-all"`, string(last[ahpConfigApprovalMode]))
	require.JSONEq(t, `"allow-all"`, string(last[ahpConfigEffectiveMode]))
}

type ahpFailingEmitter struct{}

func (ahpFailingEmitter) Emit(context.Context, string, ...ahptypes.StateAction) error {
	return errors.New("the journal refused the write")
}

func TestAHPBackendKeepsAMessageWhoseStartCannotBeShown(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "allow-all")
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	key := ahpPendingKey(alice.UserKey, ahpTestSession, "waiting")
	env.backend.mu.Lock()
	env.backend.pendingAuth = map[string]ahpPendingSender{key: {principal: alice, version: "v1"}}
	env.backend.mu.Unlock()
	require.NoError(t, env.backend.updateSession(ctx, alice.UserKey, ahpTestSession, func(s *ahpSession) {
		s.Pending = []ahpPending{{ID: "waiting", Kind: string(ahptypes.PendingMessageKindQueued), Version: "v1",
			Message: ahptypes.Message{Text: "run me", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}}}}
	}))

	env.backend.runPending(ctx, alice.UserKey, ahpTestSession, ahpFailingEmitter{})
	var record ahpSession
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	require.Len(t, record.Pending, 1)
	require.Equal(t, "v1", record.Pending[0].Version, "the same message waits")
	env.backend.mu.Lock()
	_, kept := env.backend.pendingAuth[key]
	env.backend.mu.Unlock()
	require.True(t, kept, "its sender is kept")
	var tasks corev1alpha1.TaskList
	require.NoError(t, env.kube.List(ctx, &tasks))
	require.Empty(t, tasks.Items)
}

func TestAHPBackendKeepsTheWaitingMessageBound(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	require.NoError(t, env.backend.updateSession(ctx, alice.UserKey, ahpTestSession, func(s *ahpSession) {
		for i := range ahpPendingMax {
			s.Pending = append(s.Pending, ahpPending{ID: fmt.Sprintf("waiting-%d", i), Kind: string(ahptypes.PendingMessageKindQueued),
				Message: ahptypes.Message{Text: "wait", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}}})
		}
	}))
	env.backend.HandleClientAction(ctx, alice, chat, ahptypes.StateAction{Value: &ahptypes.ChatPendingMessageSetAction{
		Type: ahptypes.ActionTypeChatPendingMessageSet, Kind: ahptypes.PendingMessageKindQueued, Id: "one-too-many",
		Message: ahptypes.Message{Text: "over", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}}, env.backend.emitterFor(alice))
	var record ahpSession
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	require.Len(t, record.Pending, ahpPendingMax)
	require.False(t, slices.ContainsFunc(record.Pending, func(p ahpPending) bool { return p.ID == "one-too-many" }))
}

func TestAHPBackendStartsNoTurnBesideAnActiveOne(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "allow-all")
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	var record ahpSession
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	require.NoError(t, env.backend.updateSession(ctx, alice.UserKey, ahpTestSession, func(s *ahpSession) {
		s.ActiveTurn, s.ActiveTask, s.Turns = "turn-first", record.OrkaSession+"-t1", 1
	}))

	require.True(t, env.backend.reserveTurn(alice.UserKey, ahpTestSession))
	env.backend.startTurn(ctx, alice, record, &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-late", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "late", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser},
			Model: &ahptypes.ModelSelection{Id: "claude-haiku-5.5"}},
	}, env.backend.emitterFor(alice))
	var tasks corev1alpha1.TaskList
	require.NoError(t, env.kube.List(ctx, &tasks))
	require.Empty(t, tasks.Items, "no second Task runs beside the active turn")
	require.Nil(t, env.backend.activeTurn(alice.UserKey, ahpTestSession))
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	require.Equal(t, "turn-first", record.ActiveTurn, "the active turn stays the session's")
	require.Equal(t, 1, record.Turns)
}

func TestAHPBackendRedactsAStoredResult(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	_, chatSub, err := c.Subscribe(ctx, chat)
	require.NoError(t, err)
	_, err = c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "Find the token", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser},
			Model: &ahptypes.ModelSelection{Id: "claude-haiku-5.5"}},
	}})
	require.NoError(t, err)
	userKey := ahphost.UserKey("https://issuer.example", "alice")
	var record ahpSession
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	task := env.waitForTask(taskName)

	secret := "github" + "_pat_" + "1234567890abcdefghijklmnopqrstuvwxyz"
	require.NoError(t, env.backend.h.resultStore.SaveResult(ctx, ahpTestNamespace, taskName, []byte("found "+secret)))
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	require.NoError(t, env.kube.Status().Update(ctx, task))

	var shown strings.Builder
	timeout := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case event := <-chatSub.Events():
			if action, ok := event.(ahp.SubscriptionEventAction); ok {
				switch value := action.Envelope.Action.Value.(type) {
				case *ahptypes.ChatDeltaAction:
					shown.WriteString(value.Content)
				case *ahptypes.ChatTurnCompleteAction:
					done = true
				}
			}
		case <-timeout:
			t.Fatal("the turn never completed")
		}
	}
	require.Contains(t, shown.String(), "found ")
	require.NotContains(t, shown.String(), secret, "the stored result is shown redacted")
	require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, ahpTestSession, &record))
	require.NotEmpty(t, record.History)
	require.NotContains(t, record.History[len(record.History)-1].Reply, secret, "the reply a model switch carries over is redacted")
}

func TestAHPBackendGivesARecreatedSessionItsOwnOrkaSession(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	userKey := ahphost.UserKey("https://issuer.example", "alice")
	send := func(text string) string {
		var record ahpSession
		require.NoError(t, env.backend.getRecord(ctx, userKey, ahpRecordSession, ahpTestSession, &record))
		_, err := c.Dispatch(ctx, record.Chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
			Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Message: ahptypes.Message{Text: text, Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser},
				Model: &ahptypes.ModelSelection{Id: "claude-haiku-5.5"}},
		}})
		require.NoError(t, err)
		require.Equal(t, text, env.waitForTask(record.OrkaSession+"-t1").Spec.Prompt)
		return record.OrkaSession
	}
	env.setUpSession(c, "allow-all")
	first := send("first")
	require.NoError(t, c.Request(ctx, "disposeSession", ahptypes.DisposeSessionParams{Channel: ahpTestSession}, nil))

	provider := "copilot"
	require.NoError(t, c.Request(ctx, "createSession", ahptypes.CreateSessionParams{
		Channel: ahpTestSession, Provider: &provider, WorkingDirectories: []string{ahpTestDir},
		Config: map[string]json.RawMessage{"approvalMode": json.RawMessage(`"allow-all"`), "baseBranch": json.RawMessage(`"main"`)},
	}, nil))
	require.NotEqual(t, first, send("second"))
}

func TestAHPCanonicalRepository(t *testing.T) {
	for raw, want := range map[string]string{
		"https://github.com/octo/widgets":      "https://github.com/octo/widgets",
		"https://GitHub.com/octo/widgets.git/": "https://github.com/octo/widgets",
	} {
		repo, _, _, err := ahpCanonicalRepository(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, repo, raw)
	}
	for _, raw := range []string{
		"http://github.com/octo/widgets", "https://github.com/octo", "https://user@github.com/octo/widgets",
		"https://github.com/octo/widgets?x=1", "https://github.com/octo/wid%3Fgets", "https://github.com/octo/wid%23gets",
		"https://github.com/oc%20to/widgets", "https://github.com/octo/..",
	} {
		_, _, _, err := ahpCanonicalRepository(raw)
		require.Error(t, err, raw)
	}
}

func TestAHPBackendSendsTheGitCredentialToGitHubOnly(t *testing.T) {
	env := newAHPTestEnv(t)
	ctx := ahpCtx(t)
	require.NoError(t, env.kube.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git-token", Namespace: ahpTestNamespace},
		Data:       map[string][]byte{ahpGitCredentialSecretKey: []byte("not-a-real-token")},
	}))
	principal := ahphost.Principal{Namespace: ahpTestNamespace, UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	choice := ahpAgentChoice{Agent: "copilot-gpt", Runtime: corev1alpha1.AgentRuntimeCopilot, Model: "gpt-5.4-nano"}
	for repo, credentialed := range map[string]bool{
		"https://github.com/octo/widgets":         true,
		"https://git.example.com/octo/widgets":    false,
		"https://github.com.example/octo/widgets": false,
	} {
		record := ahpSession{RepoURL: repo, BaseBranch: "main", Branch: "orka/session-1", OrkaSession: "ahp-1"}
		workspace := env.backend.turnTask(ctx, principal, record, choice, "ahp-1-t1", "hi", true).Spec.Workspace
		if credentialed {
			require.NotNil(t, workspace.ReadCredentialRef, repo)
			require.NotNil(t, workspace.PublicationCredentialRef, repo)
			require.Equal(t, corev1alpha1.WorkspaceIntentWrite, workspace.Intent, repo)
		} else {
			require.Nil(t, workspace.ReadCredentialRef, repo)
			require.Nil(t, workspace.PublicationCredentialRef, repo)
			require.Equal(t, corev1alpha1.WorkspaceIntentRead, workspace.Intent, repo)
		}
	}
}

func TestAHPBackendRetriesATurnEndTheStoreRefused(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "allow-all")
	env.backend.sessionRetry = time.Millisecond
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	require.NoError(t, env.backend.updateSession(ctx, alice.UserKey, ahpTestSession, func(s *ahpSession) {
		s.ActiveTurn, s.ActiveTask = "turn-1", "task-1"
	}))
	var record ahpSession
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	failing := &ahpFailingStore{AHPStore: env.backend.store, fail: func(store.AHPRecord) bool { return true }}
	env.backend.store = failing
	failing.armed.Store(true)
	turnCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	turn := &ahpTurn{b: env.backend, principal: alice, session: record, id: "turn-1", ctx: turnCtx, cancel: cancel,
		emit: env.backend.emitterFor(alice), tools: map[string]*ahpToolCall{}}

	turn.finishSession()
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	require.Equal(t, "turn-1", record.ActiveTurn, "the store refused the turn's end")
	time.Sleep(300 * time.Millisecond)
	failing.armed.Store(false)
	cancel()
	require.Eventually(t, func() bool {
		var current ahpSession
		return env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &current) == nil && current.ActiveTurn == ""
	}, 5*time.Second, 10*time.Millisecond, "the turn's end is tried again")
}

type ahpActionsEmitter struct {
	mu      sync.Mutex
	actions []ahptypes.StateAction
}

func (e *ahpActionsEmitter) Emit(_ context.Context, _ string, actions ...ahptypes.StateAction) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.actions = append(e.actions, actions...)
	return nil
}

func TestAHPBackendKeepsAWaitingMessageWhoseReplacementFails(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "allow-all")
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	key := ahpPendingKey(alice.UserKey, ahpTestSession, "waiting")
	env.backend.mu.Lock()
	env.backend.pendingAuth = map[string]ahpPendingSender{key: {principal: alice, version: "v1"}}
	env.backend.mu.Unlock()
	waiting := ahptypes.Message{Text: "run me", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}}
	require.NoError(t, env.backend.updateSession(ctx, alice.UserKey, ahpTestSession, func(s *ahpSession) {
		s.ActiveTurn = "running"
		s.Pending = []ahpPending{{ID: "waiting", Kind: string(ahptypes.PendingMessageKindQueued), Version: "v1", Message: waiting}}
	}))
	var record ahpSession
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	failing := &ahpFailingStore{AHPStore: env.backend.store, fail: func(store.AHPRecord) bool { return true }}
	env.backend.store = failing
	failing.armed.Store(true)

	emitter := &ahpActionsEmitter{}
	env.backend.HandleClientAction(ctx, alice, record.Chat, ahptypes.StateAction{Value: &ahptypes.ChatPendingMessageSetAction{
		Type: ahptypes.ActionTypeChatPendingMessageSet, Kind: ahptypes.PendingMessageKindQueued, Id: "waiting",
		Message: ahptypes.Message{Text: "run me instead", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}}, emitter)

	env.backend.mu.Lock()
	sender := env.backend.pendingAuth[key]
	env.backend.mu.Unlock()
	require.Equal(t, "v1", sender.version, "the waiting message keeps its sender")
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	require.Len(t, record.Pending, 1)
	require.Equal(t, "v1", record.Pending[0].Version)
	require.Len(t, emitter.actions, 1)
	shown, ok := emitter.actions[0].Value.(*ahptypes.ChatPendingMessageSetAction)
	require.True(t, ok, "the client shows the waiting message again, not %#v", emitter.actions[0].Value)
	require.Equal(t, "run me", shown.Message.Text)
}

func TestAHPBackendDeferredActionsKeepToTheirTurn(t *testing.T) {
	env := newAHPTestEnv(t)
	env.backend.cancelBackoff = wait.Backoff{Duration: 20 * time.Millisecond, Factor: 1, Steps: 5}
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	_, err := c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "sleep", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	env.waitForTask(taskName)
	var turn *ahpTurn
	require.Eventually(t, func() bool {
		turn = env.backend.activeTurn(alice.UserKey, ahpTestSession)
		return turn != nil
	}, 5*time.Second, 20*time.Millisecond)

	env.backend.HandleClientAction(ctx, alice, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnCancelledAction{
		Type: ahptypes.ActionTypeChatTurnCancelled, TurnId: "turn-0", Duration: 10,
	}}, &ahpActionsEmitter{})
	require.Never(t, func() bool {
		current := &corev1alpha1.Task{}
		_ = env.kube.Get(context.Background(), types.NamespacedName{Namespace: ahpTestNamespace, Name: taskName}, current)
		_, ok := current.Annotations[labels.AnnotationCancelRequested]
		return ok
	}, 300*time.Millisecond, 20*time.Millisecond, "the running turn's Task is not cancelled")

	var decided atomic.Int32
	env.backend.decideApproval = func(context.Context, *UserInfo, string, string, string, bool) error {
		decided.Add(1)
		return nil
	}
	pending := func() {
		turn.mu.Lock()
		turn.tools["call-1"] = &ahpToolCall{approval: "approval-1"}
		turn.mu.Unlock()
	}
	confirm := func(turnID string) {
		env.backend.HandleClientAction(ctx, alice, chat, ahptypes.StateAction{Value: &ahptypes.ChatToolCallConfirmedAction{
			Type: ahptypes.ActionTypeChatToolCallConfirmed, TurnId: turnID, ToolCallId: "call-1", Approved: true,
		}}, &ahpActionsEmitter{})
	}
	pending()
	confirm("turn-0")
	require.Zero(t, decided.Load(), "an earlier turn's decision")
	confirm("turn-1")
	require.EqualValues(t, 1, decided.Load(), "the running turn's own decision")
}

func TestAHPBackendCreatesTheOrkaSessionNoTurnCreated(t *testing.T) {
	env := newAHPTestEnv(t)
	sessions, ok := env.backend.store.(store.SessionStore)
	require.True(t, ok)
	env.backend.h.sessionStore = sessions
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	alice := ahphost.UserKey("https://issuer.example", "alice")
	require.NoError(t, env.backend.updateSession(ctx, alice, ahpTestSession, func(s *ahpSession) {
		s.Turns, s.SessionTurns = 1, 1
	}))
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &record))

	_, err := c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-2", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "hello", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)
	task := env.waitForTask(record.OrkaSession + "-t2")
	require.True(t, task.Spec.SessionRef.Create, "the turn creates the Orka Session")
}

func TestAHPBackendTakesBackAFailedTurnTheStoreRefusedAtFirst(t *testing.T) {
	env := newAHPTestEnv(t)
	env.backend.sessionRetry = time.Millisecond
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	alice := ahphost.UserKey("https://issuer.example", "alice")
	var refused atomic.Int32
	failing := &ahpFailingStore{AHPStore: env.backend.store, fail: func(record store.AHPRecord) bool {
		var session ahpSession
		if json.Unmarshal(record.Value, &session) != nil || session.ActiveTurn != "" {
			return false
		}
		refused.Add(1)
		return true
	}}
	env.backend.store = failing
	env.onTaskCreate = func(*corev1alpha1.Task) error {
		failing.armed.Store(true)
		return errors.New("the API server refused the Task")
	}
	_, err := c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "hello", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return refused.Load() >= 3 }, 5*time.Second, 5*time.Millisecond, "the take-back is tried again")
	failing.armed.Store(false)
	require.Eventually(t, func() bool {
		var current ahpSession
		return env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &current) == nil && current.ActiveTurn == ""
	}, 5*time.Second, 10*time.Millisecond, "the session is free again")
}

func TestAHPBackendKeepsAFailedTurnUntilItsTaskIsStopped(t *testing.T) {
	env := newAHPTestEnv(t)
	env.backend.cancelBackoff = wait.Backoff{Duration: 20 * time.Millisecond, Factor: 1, Steps: 50}
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	alice := ahphost.UserKey("https://issuer.example", "alice")
	var reachable atomic.Bool
	var looks atomic.Int32
	env.backend.h.apiReader = interceptor.NewClient(env.kube.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1alpha1.Task); ok && !reachable.Load() {
				looks.Add(1)
				return errors.New("the API server is unreachable")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	env.onTaskCreate = func(*corev1alpha1.Task) error { return errors.New("the create timed out") }
	_, err := c.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-1", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: "hello", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return looks.Load() >= 3 }, 5*time.Second, 5*time.Millisecond, "the Task is looked for again")
	var current ahpSession
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &current))
	require.Equal(t, "turn-1", current.ActiveTurn, "the session stays busy while the Task may run")
	reachable.Store(true)
	require.Eventually(t, func() bool {
		var now ahpSession
		return env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &now) == nil && now.ActiveTurn == ""
	}, 5*time.Second, 10*time.Millisecond, "the turn is taken back once the Task is known absent")
}

func TestAHPBackendRunsAWaitingMessageAfterALateTakeBack(t *testing.T) {
	env := newAHPTestEnv(t)
	env.backend.sessionRetry = time.Millisecond
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	key := ahpPendingKey(alice.UserKey, ahpTestSession, "waiting")
	env.backend.mu.Lock()
	env.backend.pendingAuth = map[string]ahpPendingSender{key: {principal: alice, version: "v1"}}
	env.backend.mu.Unlock()
	require.NoError(t, env.backend.updateSession(ctx, alice.UserKey, ahpTestSession, func(s *ahpSession) {
		s.Pending = []ahpPending{{ID: "waiting", Kind: string(ahptypes.PendingMessageKindQueued), Version: "v1",
			Message: ahptypes.Message{Text: "run me", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}}}}
	}))
	var refused atomic.Int32
	failing := &ahpFailingStore{AHPStore: env.backend.store, fail: func(record store.AHPRecord) bool {
		var session ahpSession
		if json.Unmarshal(record.Value, &session) != nil || session.ActiveTurn != "" {
			return false
		}
		refused.Add(1)
		return true
	}}
	env.backend.store = failing
	var creates atomic.Int32
	env.onTaskCreate = func(*corev1alpha1.Task) error {
		if creates.Add(1) == 1 {
			failing.armed.Store(true)
			return errors.New("the API server refused the Task")
		}
		return nil
	}
	_, err := c.Dispatch(ctx, chat, ahpUserTurn("turn-1", "hello"))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return refused.Load() >= 3 }, 5*time.Second, 5*time.Millisecond, "the take-back is tried again")
	failing.armed.Store(false)
	require.Eventually(t, func() bool { return creates.Load() >= 2 }, 5*time.Second, 10*time.Millisecond,
		"the waiting message starts its turn once the take-back lands")
}

func TestAHPBackendKeepsTheReplyTheStoreRefusedAtFirst(t *testing.T) {
	env := newAHPTestEnv(t)
	env.backend.sessionRetry = time.Millisecond
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	alice := ahphost.UserKey("https://issuer.example", "alice")
	_, err := c.Dispatch(ctx, chat, ahpUserTurn("turn-1", "what is the answer"))
	require.NoError(t, err)
	record := ahpSession{}
	require.NoError(t, env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &record))
	taskName := record.OrkaSession + "-t1"
	task := env.waitForTask(taskName)
	require.Eventually(t, func() bool { return env.backend.activeTurn(alice, ahpTestSession) != nil }, 5*time.Second, 20*time.Millisecond)
	env.publish(taskName, &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantMessageChunk, AssistantMessage: &harnessv2.AssistantMessageChunk{Text: "42"}})

	var refused atomic.Int32
	failing := &ahpFailingStore{AHPStore: env.backend.store, fail: func(record store.AHPRecord) bool {
		var session ahpSession
		if json.Unmarshal(record.Value, &session) != nil || session.Session != ahpTestSession {
			return false
		}
		refused.Add(1)
		return true
	}}
	failing.armed.Store(true)
	env.backend.store = failing
	require.NoError(t, env.backend.h.resultStore.SaveResult(ctx, ahpTestNamespace, taskName, []byte("42")))
	require.NoError(t, env.kube.Get(ctx, types.NamespacedName{Namespace: ahpTestNamespace, Name: taskName}, task))
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	require.NoError(t, env.kube.Status().Update(ctx, task))
	require.Eventually(t, func() bool { return refused.Load() >= 3 }, 10*time.Second, 5*time.Millisecond, "the turn's end is tried again")
	failing.armed.Store(false)
	require.Eventually(t, func() bool {
		var current ahpSession
		if env.backend.getRecord(ctx, alice, ahpRecordSession, ahpTestSession, &current) != nil || current.ActiveTurn != "" || len(current.History) == 0 {
			return false
		}
		return current.History[len(current.History)-1].Reply == "42"
	}, 5*time.Second, 10*time.Millisecond, "the reply is in the session's history")
}

func TestAHPBackendRetriesDrainingAWaitingMessage(t *testing.T) {
	env := newAHPTestEnv(t)
	env.backend.sessionRetry = time.Millisecond
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "allow-all")
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	key := ahpPendingKey(alice.UserKey, ahpTestSession, "waiting")
	env.backend.mu.Lock()
	env.backend.pendingAuth = map[string]ahpPendingSender{key: {principal: alice, version: "v1"}}
	env.backend.mu.Unlock()
	require.NoError(t, env.backend.updateSession(ctx, alice.UserKey, ahpTestSession, func(s *ahpSession) {
		s.Pending = []ahpPending{{ID: "waiting", Kind: string(ahptypes.PendingMessageKindQueued), Version: "v1",
			Message: ahptypes.Message{Text: "run me", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}}}}
	}))
	var refused atomic.Int32
	failing := &ahpFailingStore{AHPStore: env.backend.store, fail: func(record store.AHPRecord) bool {
		var session ahpSession
		if json.Unmarshal(record.Value, &session) != nil || len(session.Pending) != 0 || session.ActiveTurn != "" {
			return false
		}
		refused.Add(1)
		return true
	}}
	env.backend.store = failing
	failing.armed.Store(true)

	env.backend.runPending(ctx, alice.UserKey, ahpTestSession, env.backend.emitterFor(alice))
	require.Eventually(t, func() bool { return refused.Load() >= 3 }, 5*time.Second, 5*time.Millisecond, "taking the message off is tried again")
	failing.armed.Store(false)
	require.Eventually(t, func() bool {
		var tasks corev1alpha1.TaskList
		return env.kube.List(ctx, &tasks) == nil && len(tasks.Items) == 1
	}, 5*time.Second, 10*time.Millisecond, "the waiting message runs once the store takes it off")
}

type ahpFailingChannelStore struct {
	store.AHPStore
	armed atomic.Bool
}

func (s *ahpFailingChannelStore) GetAHPChannelState(ctx context.Context, userKey, channel string) (store.AHPChannelState, error) {
	if s.armed.Load() {
		return store.AHPChannelState{}, errors.New("the store refused the read")
	}
	return s.AHPStore.GetAHPChannelState(ctx, userKey, channel)
}

func TestAHPBackendRetriesEndingAnUnstartedTurnItCouldNotRead(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	chat := env.setUpSession(c, "allow-all")
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	require.NoError(t, env.backend.emitterFor(alice).Emit(ctx, chat, ahpUserTurn("turn-lost", "run me")))
	failing := &ahpFailingChannelStore{AHPStore: env.backend.store}
	failing.armed.Store(true)
	env.backend.store = failing
	env.backend.mu.Lock()
	delete(env.backend.recovered, alice.UserKey)
	env.backend.mu.Unlock()

	env.backend.recoverTurns(ctx, alice)
	require.Eventually(t, func() bool {
		env.backend.mu.Lock()
		defer env.backend.mu.Unlock()
		return !env.backend.recovered[alice.UserKey]
	}, 5*time.Second, 10*time.Millisecond, "a recovery that could not read the chat is eligible again")

	failing.armed.Store(false)
	env.backend.recoverTurns(ctx, alice)
	require.Eventually(t, func() bool {
		state, err := ahphost.ChannelState(ctx, failing.AHPStore, alice.UserKey, chat)
		return err == nil && state.Chat.ActiveTurn == nil
	}, 5*time.Second, 10*time.Millisecond, "the retry ends the turn that never started")
}

type ahpFlakyResultStore struct {
	store.ResultStore
	failures atomic.Int32
}

func (s *ahpFlakyResultStore) GetResult(ctx context.Context, namespace, taskName string) ([]byte, error) {
	if s.failures.Add(-1) >= 0 {
		return nil, errors.New("the store refused the read")
	}
	return s.ResultStore.GetResult(ctx, namespace, taskName)
}

func TestAHPTurnRetriesReadingItsResult(t *testing.T) {
	previous := ahpResultReadDelays
	ahpResultReadDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { ahpResultReadDelays = previous })
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	results := sqlite.NewStore(db, ":memory:")
	require.NoError(t, results.SaveResult(t.Context(), ahpTestNamespace, "turn-task", []byte("the answer")))
	flaky := &ahpFlakyResultStore{ResultStore: results}
	turn := &ahpTurn{b: &ahpBackend{h: &Handlers{resultStore: flaky}}, ctx: t.Context()}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: ahpTestNamespace, Name: "turn-task"}}

	flaky.failures.Store(2)
	data, err := turn.readResult(task)
	require.NoError(t, err)
	require.Equal(t, "the answer", string(data), "two failed reads are retried")

	flaky.failures.Store(3)
	_, err = turn.readResult(task)
	require.Error(t, err, "a read that keeps failing is reported")
}

func TestAHPAgentChoicesFollowAgentUseAuthorization(t *testing.T) {
	enableAgentUseAuthorization(t)
	kube := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(
		ahpTestAgent("a-denied-claude", corev1alpha1.AgentRuntimeClaude, "shared"),
		ahpTestAgent("b-allowed-claude", corev1alpha1.AgentRuntimeClaude, "shared"),
		ahpTestAgent("c-denied-codex", corev1alpha1.AgentRuntimeCodex, "codex-only"),
	).Build()
	clientset, _ := agentUseClientset(ahpTestNamespace + "/b-allowed-claude")
	backend := &ahpBackend{h: NewHandlers(HandlersConfig{Client: kube, APIReader: kube, KubeClient: clientset})}
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Username: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	choices, err := backend.agentChoices(t.Context(), alice)
	require.NoError(t, err)
	require.Equal(t, []ahpAgentChoice{{Agent: "b-allowed-claude", Runtime: corev1alpha1.AgentRuntimeClaude, Model: "shared"}}, choices,
		"a model is offered through an Agent the person may use, and not through one they may not")
}

type ahpPausingStore struct {
	store.AHPStore
	pauseDeletes chan chan struct{}
	pauseRead    chan chan struct{}
}

func (s *ahpPausingStore) DeleteAHPRecords(ctx context.Context, userKey, kind, prefix string) error {
	if kind == ahpRecordContent {
		select {
		case release := <-s.pauseDeletes:
			<-release
		default:
		}
	}
	return s.AHPStore.DeleteAHPRecords(ctx, userKey, kind, prefix)
}

func (s *ahpPausingStore) GetAHPRecord(ctx context.Context, userKey, kind, key string) (store.AHPRecord, error) {
	record, err := s.AHPStore.GetAHPRecord(ctx, userKey, kind, key)
	if kind == ahpRecordSession {
		select {
		case release := <-s.pauseRead:
			<-release
		default:
		}
	}
	return record, err
}

func TestAHPBackendDisposalLeavesNoSessionAnUpdateWritesBack(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	ctx := ahpCtx(t)
	env.setUpSession(c, "allow-all")
	alice := ahphost.Principal{Issuer: "https://issuer.example", Subject: "alice", Namespace: ahpTestNamespace,
		UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	pausing := &ahpPausingStore{AHPStore: env.backend.store, pauseDeletes: make(chan chan struct{}, 1), pauseRead: make(chan chan struct{}, 1)}
	env.backend.store = pausing

	releaseDispose := make(chan struct{})
	pausing.pauseDeletes <- releaseDispose
	disposed := make(chan error, 1)
	go func() { disposed <- env.backend.DisposeSession(ctx, alice, ahpTestSession) }()
	require.Eventually(t, func() bool { return len(pausing.pauseDeletes) == 0 }, 5*time.Second, 5*time.Millisecond)

	releaseUpdate := make(chan struct{})
	pausing.pauseRead <- releaseUpdate
	updated := make(chan error, 1)
	go func() {
		updated <- env.backend.updateSession(ctx, alice.UserKey, ahpTestSession, func(s *ahpSession) { s.Plan = "late" })
	}()
	require.Eventually(t, func() bool { return len(pausing.pauseRead) == 0 }, 5*time.Second, 5*time.Millisecond)

	close(releaseDispose)
	time.Sleep(100 * time.Millisecond)
	close(releaseUpdate)
	require.NoError(t, <-updated)
	require.NoError(t, <-disposed)
	var record ahpSession
	require.ErrorIs(t, env.backend.getRecord(ctx, alice.UserKey, ahpRecordSession, ahpTestSession, &record), store.ErrNotFound,
		"the disposed session is not written back")
}

func TestAHPToolCallStartsDescribedAndAvoidsTypedNamesWithoutInput(t *testing.T) {
	emitter := &ahpActionsEmitter{}
	turn := &ahpTurn{id: "turn-1", ctx: t.Context(), emit: emitter, tools: map[string]*ahpToolCall{}}
	starts := func() []*ahptypes.ChatToolCallStartAction {
		emitter.mu.Lock()
		defer emitter.mu.Unlock()
		var out []*ahptypes.ChatToolCallStartAction
		for _, action := range emitter.actions {
			if start, ok := action.Value.(*ahptypes.ChatToolCallStartAction); ok {
				out = append(out, start)
			}
		}
		return out
	}
	startOf := func(id string) *ahptypes.ChatToolCallStartAction {
		for _, start := range starts() {
			if start.ToolCallId == id {
				return start
			}
		}
		return nil
	}

	turn.tool(&harnessv2.ToolCallUpdate{ToolCallID: "read-1", Kind: "read", Status: harnessv2.ToolCallStatusPending})
	require.Empty(t, starts())
	turn.tool(&harnessv2.ToolCallUpdate{ToolCallID: "read-1", Kind: "read", Title: "Viewing README.md", Status: harnessv2.ToolCallStatusPending})
	turn.tool(&harnessv2.ToolCallUpdate{ToolCallID: "read-1", Status: harnessv2.ToolCallStatusCompleted})
	require.Len(t, starts(), 1)
	require.Equal(t, "read_call", startOf("read-1").ToolName)
	require.Equal(t, "Viewing README.md", startOf("read-1").DisplayName)

	turn.tool(&harnessv2.ToolCallUpdate{ToolCallID: "search-1", Kind: "search", Status: harnessv2.ToolCallStatusPending})
	turn.tool(&harnessv2.ToolCallUpdate{ToolCallID: "search-1", Status: harnessv2.ToolCallStatusInProgress})
	require.Equal(t, "search_call", startOf("search-1").ToolName)

	turn.tool(&harnessv2.ToolCallUpdate{ToolCallID: "read-2", Name: "read", Kind: "read", Title: "Viewing go.sum", Status: harnessv2.ToolCallStatusPending})
	require.Equal(t, "read_call", startOf("read-2").ToolName)
	turn.tool(&harnessv2.ToolCallUpdate{ToolCallID: "grep-1", Name: "grep", Title: "Searching", RawInput: json.RawMessage(`{"path":"."}`), Status: harnessv2.ToolCallStatusInProgress})
	require.Equal(t, "grep_call", startOf("grep-1").ToolName)

	input := json.RawMessage(`{"file_path":"README.md"}`)
	turn.tool(&harnessv2.ToolCallUpdate{ToolCallID: "read-3", Name: "Read", Kind: "read", RawInput: input, Status: harnessv2.ToolCallStatusPending})
	turn.tool(&harnessv2.ToolCallUpdate{ToolCallID: "read-3", Title: "Read File", Status: harnessv2.ToolCallStatusInProgress})
	require.Equal(t, "Read", startOf("read-3").ToolName)
	require.JSONEq(t, string(input), string(turn.tools["read-3"].input))
	require.Empty(t, turn.heldTools)

	turn.tool(&harnessv2.ToolCallUpdate{
		ToolCallID: "read-4", Name: "read", Kind: "read", Title: "Viewing go.mod", Status: harnessv2.ToolCallStatusPending,
		RawInput: json.RawMessage(`{"path":"/sessions/session-1/workspace/go.mod","view_range":[1,5]}`),
	})
	path := json.RawMessage(`{"path":"go.mod","view_range":[1,5]}`)
	turn.tool(&harnessv2.ToolCallUpdate{ToolCallID: "read-4", Status: harnessv2.ToolCallStatusCompleted})
	require.Equal(t, "read", startOf("read-4").ToolName)
	var ready *ahptypes.ChatToolCallReadyAction
	emitter.mu.Lock()
	for _, action := range emitter.actions {
		if r, ok := action.Value.(*ahptypes.ChatToolCallReadyAction); ok && r.ToolCallId == "read-4" {
			ready = r
		}
	}
	emitter.mu.Unlock()
	require.NotNil(t, ready)
	require.NotNil(t, ready.ToolInput)
	require.NotNil(t, ready.ToolInput.Inline)
	require.JSONEq(t, string(path), *ready.ToolInput.Inline)

	turn.tool(&harnessv2.ToolCallUpdate{ToolCallID: "bash-1", Name: "bash", Kind: "execute", Title: "Run tests", Status: harnessv2.ToolCallStatusInProgress})
	turn.tool(&harnessv2.ToolCallUpdate{ToolCallID: "exec-1", Name: "execute", Kind: "execute", Title: "List files", Status: harnessv2.ToolCallStatusInProgress})
	turn.tool(&harnessv2.ToolCallUpdate{ToolCallID: "other-1", Kind: "other", Title: "Thinking", Status: harnessv2.ToolCallStatusInProgress})
	require.Equal(t, "bash", startOf("bash-1").ToolName)
	require.Equal(t, "execute", startOf("exec-1").ToolName)
	require.Equal(t, "other", startOf("other-1").ToolName)

	command := json.RawMessage(`{"command":"make test"}`)
	turn.tool(&harnessv2.ToolCallUpdate{ToolCallID: "bash-2", Kind: "execute", RawInput: command, Status: harnessv2.ToolCallStatusPending})
	turn.onApproval(&liveevents.Approval{ID: "approval-1", ToolCallID: "bash-2", State: liveevents.ApprovalPending})
	require.Equal(t, "execute", startOf("bash-2").ToolName)
	require.JSONEq(t, string(command), string(turn.tools["bash-2"].input))
	require.Empty(t, turn.heldTools)
}
