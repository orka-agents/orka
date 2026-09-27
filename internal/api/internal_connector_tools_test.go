/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/orka-agents/orka/internal/workerenv"
	corev1 "k8s.io/api/core/v1"

	"github.com/gofiber/fiber/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/outboundaccess"
)

type stubOutboundResolver struct {
	resolution outboundaccess.Resolution
	err        error
	request    outboundaccess.ResolveRequest
}

func (s *stubOutboundResolver) Resolve(_ context.Context, req outboundaccess.ResolveRequest) (outboundaccess.Resolution, error) {
	s.request = req
	return s.resolution, s.err
}

func connectorToolFixtures() *corev1alpha1.Task {
	task := internalCallerAuthTask()
	task.Spec = corev1alpha1.TaskSpec{
		Type:        corev1alpha1.TaskTypeAI,
		AgentRef:    &corev1alpha1.AgentReference{Name: "agent"},
		RequestedBy: &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"},
	}
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[labels.AnnotationRequestedBySource] = labels.RequestedBySourceAPI
	task.Status.ConnectionBindings = []corev1alpha1.ConnectionBinding{{PolicyName: "github-conn", Provider: "github", ConnectionName: "github-abc", UID: "conn-uid", Generation: 2, Mode: "readOnly"}}
	return task
}

func connectorTestTool(name string, class corev1alpha1.AgentRuntimeBrokeredToolClass, policyName string) *corev1alpha1.Tool {
	spec := corev1alpha1.ToolSpec{Description: name, BrokeredToolClass: class, HTTP: &corev1alpha1.HTTPExecution{URL: "https://api.github.example.test/" + name}}
	if policyName != "" {
		spec.HTTP.OutboundAccessPolicyRef = &corev1alpha1.LocalObjectReference{Name: policyName}
	}
	return &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: spec}
}

type connectorToolAppOptions struct {
	mode             string
	approvalRequired []string
	// tools is the tool list frozen into the Job; nil means every fixture tool.
	tools []string
}

func newConnectorToolApp(t *testing.T, resolver outboundaccess.Resolver, enabled bool) *fiber.App {
	t.Helper()
	app, _, _ := newConnectorToolAppWithOptions(t, resolver, enabled, connectorToolAppOptions{mode: "readOnly", approvalRequired: []string{"gh_write"}})
	return app
}

func newConnectorToolAppWithOptions(t *testing.T, resolver outboundaccess.Resolver, enabled bool, opts connectorToolAppOptions) (*fiber.App, *storetest.FakeExecutionEventStore, client.Client) {
	t.Helper()
	task := connectorToolFixtures()
	task.Status.ConnectionBindings[0].Mode = opts.mode
	job := internalCallerAuthJob(task, "job-a", "job-uid")
	// The tool list and approval policy the Job builder froze at dispatch.
	frozenTools := opts.tools
	if frozenTools == nil {
		frozenTools = []string{"gh_search", "gh_write", "plain"}
	}
	job.Spec.Template.Spec.Containers = []corev1.Container{{
		Name: "worker",
		Env: []corev1.EnvVar{
			{Name: workerenv.AITools, Value: strings.Join(frozenTools, ",")},
			{Name: workerenv.ApprovalRequiredTools, Value: workerenv.JoinCSV(opts.approvalRequired)},
		},
	}}
	eventStore := storetest.NewFakeExecutionEventStore()
	pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default"},
		Spec:       corev1alpha1.AgentSpec{Tools: []corev1alpha1.ToolReference{{Name: "gh_search"}, {Name: "gh_write"}, {Name: "plain"}}},
	}
	policy := &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "github-conn", Namespace: "default"},
		Spec:       corev1alpha1.OutboundAccessPolicySpec{Connection: &corev1alpha1.ConnectionOutboundAccess{ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}}},
	}
	tool := connectorTestTool
	connection := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "github-abc", Namespace: "default", UID: "conn-uid", Generation: 2},
		Spec: corev1alpha1.ConnectionSpec{
			Subject: corev1alpha1.ConnectionSubject{Issuer: "https://issuer.example.test", Subject: "alice"}, ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}, Mode: opts.mode,
		},
		Status: corev1alpha1.ConnectionStatus{Conditions: []metav1.Condition{
			{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: "Linked", ObservedGeneration: 2},
			{Type: corev1alpha1.ConnectionConditionScopesGranted, Status: metav1.ConditionTrue, Reason: "ScopesGranted", ObservedGeneration: 2},
			{Type: corev1alpha1.ConnectionConditionProviderResolved, Status: metav1.ConditionTrue, Reason: "ProviderResolved", ObservedGeneration: 2},
		}},
	}
	// The requester's Connection name must match the deterministic form the
	// controller derives; override the fixture name accordingly.
	connection.Name = connectors.ConnectionName("github", "https://issuer.example.test", "alice")
	builder := fake.NewClientBuilder().WithScheme(internalCallerAuthScheme(t)).
		WithObjects(task, job, pod, agent, policy, connection,
			tool("gh_search", corev1alpha1.AgentRuntimeBrokeredToolClassRead, "github-conn"),
			tool("gh_write", corev1alpha1.AgentRuntimeBrokeredToolClassWrite, "github-conn"),
			tool("plain", corev1alpha1.AgentRuntimeBrokeredToolClassRead, ""))
	c := builder.Build()
	handlers := NewInternalHandlers(nil, nil, nil, nil, nil, InternalHandlersConfig{
		Client: c, APIReader: c, ExecutionEventStore: eventStore,
		ConnectorTools: ConnectorToolExecutionConfig{
			Enabled: enabled, OutboundAccess: resolver, KubeClient: k8sfake.NewSimpleClientset(),
			HTTPClient: &http.Client{Timeout: time.Second},
		},
	})
	app := fiber.New()
	app.Use(func(ctx fiber.Ctx) error {
		ctx.Locals(UserInfoContextKey, internalCallerAuthWorkerUser("pod-a", "pod-uid"))
		return ctx.Next()
	})
	app.Post("/internal/v1/tasks/:namespace/:taskName/connector-tools/:tool", handlers.ExecuteConnectorTool)
	return app, eventStore, c
}

// seedApproval records a requested-then-approved approval for tool bound to
// args, the way the worker's gate and a human decision would.
func seedApproval(t *testing.T, eventStore *storetest.FakeExecutionEventStore, id string, args string, approve bool) {
	const tool = "gh_write"
	t.Helper()
	// The worker digests the plain spec of a connector-backed tool.
	approvedTool := connectorTestTool(tool, corev1alpha1.AgentRuntimeBrokeredToolClassWrite, "github-conn")
	targetArgs, err := approvals.TargetArguments(json.RawMessage(args), approvedTool)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := approvals.TargetArgsDigest(targetArgs)
	if err != nil {
		t.Fatal(err)
	}
	specDigest, err := approvals.TargetSpecDigest(approvedTool.Spec)
	if err != nil {
		t.Fatal(err)
	}
	requested, _ := json.Marshal(map[string]any{
		"approvalID": id, "taskUID": "task-uid", "targetTool": tool, "targetArgsDigest": digest, "targetSpecDigest": specDigest, "action": "Execute " + tool,
	})
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-a", TaskName: "task-a",
		Type: events.ExecutionEventTypeApprovalRequested, ToolName: tool, ToolCallID: id, Content: requested, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if !approve {
		return
	}
	decided, _ := json.Marshal(map[string]any{"approvalID": id, "actor": "reviewer"})
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-a", TaskName: "task-a",
		Type: events.ExecutionEventTypeApprovalApproved, ToolName: tool, ToolCallID: id, Content: decided, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func postConnectorTool(t *testing.T, app *fiber.App, tool, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/tasks/default/task-a/connector-tools/"+tool, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(raw)
}

func TestExecuteConnectorToolGates(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app := newConnectorToolApp(t, resolver, true)

	if status, body := postConnectorTool(t, app, "plain", `{}`); status != http.StatusForbidden || !strings.Contains(body, "connector-backed") {
		t.Fatalf("non-connector tool = %d %s", status, body)
	}
	if status, _ := postConnectorTool(t, app, "missing", `{}`); status != http.StatusNotFound {
		t.Fatalf("missing tool = %d", status)
	}
	// gh_write is hidden from a readOnly link, so the worker cannot reach it.
	if status, body := postConnectorTool(t, app, "gh_write", `{}`); status != http.StatusForbidden || !strings.Contains(body, "not enabled") {
		t.Fatalf("hidden write tool = %d %s", status, body)
	}
	if status, body := postConnectorTool(t, app, "gh_search", `{"arguments":{},"subject":"mallory"}`); status != http.StatusBadRequest || !strings.Contains(body, "invalid request body") {
		t.Fatalf("unknown field = %d %s", status, body)
	}
	// A visible connector tool reaches resolution with the requester and the
	// frozen binding from Task status; resolver refusal maps to 424.
	status, body := postConnectorTool(t, app, "gh_search", `{"arguments":{"q":"orka"},"callId":"c1","idempotencyKey":"k1"}`)
	if status != http.StatusFailedDependency || !strings.Contains(body, "no connection") {
		t.Fatalf("resolver refusal = %d %s", status, body)
	}
	if resolver.request.Requester == nil || resolver.request.Requester.Subject != "alice" {
		t.Fatalf("resolver requester = %+v", resolver.request.Requester)
	}
	if frozen := resolver.request.FrozenConnections["github-conn"]; frozen.UID != "conn-uid" || frozen.Generation != 2 {
		t.Fatalf("resolver frozen = %+v", resolver.request.FrozenConnections)
	}
	if resolver.request.PolicyName != "github-conn" || resolver.request.TargetScheme != "https" {
		t.Fatalf("resolver request = %+v", resolver.request)
	}
}

func TestExecuteConnectorToolDisabledAndWrongCaller(t *testing.T) {
	app := newConnectorToolApp(t, &stubOutboundResolver{}, false)
	if status, _ := postConnectorTool(t, app, "gh_search", `{}`); status != http.StatusNotImplemented {
		t.Fatalf("disabled = %d", status)
	}
	// A caller that is not the Task's worker Pod is refused before any lookup.
	app = newConnectorToolApp(t, &stubOutboundResolver{}, true)
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/tasks/default/other-task/connector-tools/gh_search", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong task = %d", resp.StatusCode)
	}
}

func TestExecuteConnectorToolEnforcesApprovalAtTheController(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app, eventStore, c := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})

	// A write tool in the frozen approval set executes only with an approval.
	if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"x"}}`); status != http.StatusForbidden || !strings.Contains(body, "approval is required") {
		t.Fatalf("write without approval = %d %s", status, body)
	}
	if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-missing"}`); status != http.StatusForbidden || !strings.Contains(body, "not found") {
		t.Fatalf("unknown approval = %d %s", status, body)
	}
	seedApproval(t, eventStore, "ap-pending", `{"q":"x"}`, false)
	if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-pending"}`); status != http.StatusForbidden || !strings.Contains(body, "is pending") {
		t.Fatalf("pending approval = %d %s", status, body)
	}
	seedApproval(t, eventStore, "ap-1", `{"q":"x"}`, true)
	// The approval binds the exact arguments and the tool.
	if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"other"},"approvalId":"ap-1"}`); status != http.StatusForbidden || !strings.Contains(body, "does not bind") {
		t.Fatalf("mismatched arguments = %d %s", status, body)
	}
	// A matching approval passes the gate and is claimed; the stub resolver
	// then refuses before any provider request (424), which hands the claim back.
	if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1","idempotencyKey":"ap-1"}`); status != http.StatusFailedDependency || !strings.Contains(body, "no connection") {
		t.Fatalf("approved write = %d %s", status, body)
	}
	// An execution that reached the provider never hands its claim back:
	// presenting the same approval again, with any idempotency key, is a replay.
	history, err := approvals.ListEvents(context.Background(), eventStore, "default", "task-a")
	if err != nil {
		t.Fatal(err)
	}
	var decisionSeq int64
	for _, approval := range approvals.Derive(history, time.Now()) {
		if approval.ID == "ap-1" {
			decisionSeq = approval.DecisionSeq
		}
	}
	spent, _ := json.Marshal(map[string]any{"approvalID": "ap-1", "executionOutcome": "running"})
	if _, appended, err := eventStore.AppendExecutionEventIfAbsent(context.Background(), &store.ExecutionEvent{
		Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-a", TaskName: "task-a",
		Type: events.ExecutionEventTypeApprovalExecutionUpdated, ToolCallID: "ap-1", Content: spent, CreatedAt: time.Now(),
	}, fmt.Sprintf("connector-approval-claim:ap-1:%d:1", decisionSeq)); err != nil || !appended {
		t.Fatalf("seed spent claim: appended = %t err = %v", appended, err)
	}
	if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1","idempotencyKey":"fresh"}`); status != http.StatusConflict || !strings.Contains(body, "already used") {
		t.Fatalf("replayed approval = %d %s", status, body)
	}
	// A Tool retargeted after approval (same name, class, policy, and
	// arguments) cannot ride the old approval.
	seedApproval(t, eventStore, "ap-2", `{"q":"y"}`, true)
	live := &corev1alpha1.Tool{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gh_write"}, live); err != nil {
		t.Fatal(err)
	}
	live.Spec.HTTP.URL = "https://api.github.example.test/other-resource"
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"y"},"approvalId":"ap-2"}`); status != http.StatusConflict || !strings.Contains(body, "configuration changed") {
		t.Fatalf("retargeted tool = %d %s", status, body)
	}
	// Read tools outside the frozen set need no approval.
	if status, body := postConnectorTool(t, app, "gh_search", `{"arguments":{"q":"x"}}`); status != http.StatusFailedDependency || !strings.Contains(body, "no connection") {
		t.Fatalf("read without approval = %d %s", status, body)
	}
}

func TestExecuteConnectorToolRejectsClassDriftAfterDispatch(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	// The Job was dispatched when gh_write was not approval-required (it was
	// read-class then); the live Tool is write-class now.
	app, eventStore, _ := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: nil})
	seedApproval(t, eventStore, "ap-1", `{"q":"x"}`, true)
	if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`); status != http.StatusConflict || !strings.Contains(body, "does not cover") {
		t.Fatalf("class drift = %d %s", status, body)
	}
}

func TestExecuteConnectorToolRequiresDispatchedToolList(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	// gh_search is enabled on the live Agent but was not in the list the
	// worker was started with: an Agent edited after dispatch cannot add it.
	app, _, _ := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readOnly", tools: []string{"plain"}})
	if status, body := postConnectorTool(t, app, "gh_search", `{"arguments":{"q":"x"}}`); status != http.StatusForbidden || !strings.Contains(body, "dispatched with the task's job") {
		t.Fatalf("undispatched tool = %d %s", status, body)
	}
}

func TestExecuteConnectorToolReleasesClaimWithoutProviderRequest(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app, eventStore, _ := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
	seedApproval(t, eventStore, "ap-1", `{"q":"x"}`, true)
	// Resolution fails before any provider request: the claim is handed back
	// and the same approval can be claimed again on an exact retry.
	for range 2 {
		if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`); status != http.StatusFailedDependency || !strings.Contains(body, "no connection") {
			t.Fatalf("pre-request failure = %d %s", status, body)
		}
	}
	claims, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-a",
		EventTypes: []string{events.ExecutionEventTypeApprovalExecutionUpdated}, Limit: 10,
	})
	if err != nil || len(claims) != 4 {
		t.Fatalf("claim/release events = %d err = %v, want two claims and two releases", len(claims), err)
	}
}
