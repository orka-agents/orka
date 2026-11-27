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
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/orka-agents/orka/internal/workerenv"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/gofiber/fiber/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/controller"
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
	controller.SetRequesterStampKey(testConnectorStampKey)
	task.Annotations[labels.AnnotationRequestedByStamp] = connectors.RequesterStamp(testConnectorStampKey, task.UID, task.Spec.RequestedBy.Issuer, task.Spec.RequestedBy.Subject)
	task.Status.ConnectionBindings = []corev1alpha1.ConnectionBinding{{PolicyName: "github-conn", Provider: "github", ConnectionName: connectors.ConnectionName("github", "https://issuer.example.test", "alice"), UID: "conn-uid", Generation: 2, GrantSequence: 1, Mode: "readOnly"}}
	return task
}

var testConnectorStampKey = []byte("0123456789abcdef0123456789abcdef")

func connectorTestTool(name string, class corev1alpha1.AgentRuntimeBrokeredToolClass, policyName string) *corev1alpha1.Tool {
	return connectorTestToolWithSchema(name, class, policyName, "")
}

func connectorTestToolWithSchema(name string, class corev1alpha1.AgentRuntimeBrokeredToolClass, policyName, parameters string) *corev1alpha1.Tool {
	spec := corev1alpha1.ToolSpec{Description: name, BrokeredToolClass: class, HTTP: &corev1alpha1.HTTPExecution{URL: "https://api.github.example.test/" + name}}
	if policyName != "" {
		spec.HTTP.OutboundAccessPolicyRef = &corev1alpha1.LocalObjectReference{Name: policyName}
	}
	if parameters != "" {
		spec.Parameters = &apiextensionsv1.JSON{Raw: []byte(parameters)}
	}
	return &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: spec}
}

type connectorToolAppOptions struct {
	mode              string
	approvalRequired  []string
	tools             []string
	bindingsEnv       *string
	parameters        string
	transactionSecret string
	noLedger          bool
	noGrant           bool
}

type connectorToolHarness struct {
	app     *fiber.App
	events  *storetest.FakeExecutionEventStore
	client  client.Client
	effects *sqlite.Store
	fence   store.ControllerEpochFence
	tools   map[string]*corev1alpha1.Tool
}

type staticFenceSource struct{ fence store.ControllerEpochFence }

func (s staticFenceSource) CurrentFence(context.Context) (store.ControllerEpochFence, error) {
	return s.fence, nil
}

func newConnectorToolLedger(t *testing.T) (*sqlite.Store, store.ControllerEpochFence) {
	t.Helper()
	db, err := sqlite.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	effects := sqlite.NewStore(db, ":memory:")
	epoch, err := effects.CompareAndSwapControllerEpoch(context.Background(), store.ControllerEpochCAS{
		ExpectedVersion: 0, ExpectedEpoch: 0, NewEpoch: 1, HolderID: "connector-controller",
		RequestDigest: store.CanonicalBytesDigest([]byte("connector-epoch")), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return effects, store.ControllerEpochFence{Name: epoch.Name, Epoch: epoch.Epoch, HolderID: epoch.HolderID}
}

func newConnectorToolApp(t *testing.T, resolver outboundaccess.Resolver, enabled bool) *fiber.App {
	t.Helper()
	app, _, _ := newConnectorToolAppWithOptions(t, resolver, enabled, connectorToolAppOptions{mode: "readOnly", approvalRequired: []string{"gh_write"}})
	return app
}

func newConnectorToolAppWithOptions(t *testing.T, resolver outboundaccess.Resolver, enabled bool, opts connectorToolAppOptions) (*fiber.App, *storetest.FakeExecutionEventStore, client.Client) {
	t.Helper()
	h := newConnectorToolHarness(t, resolver, enabled, opts)
	return h.app, h.events, h.client
}

func newConnectorToolHarness(t *testing.T, resolver outboundaccess.Resolver, enabled bool, opts connectorToolAppOptions) *connectorToolHarness {
	t.Helper()
	task := connectorToolFixtures()
	task.Status.ConnectionBindings[0].Mode = opts.mode
	if opts.noGrant {
		task.Status.ConnectionBindings[0].GrantSequence = 0
	}
	if opts.transactionSecret != "" {
		task.Spec.Transaction = &corev1alpha1.TaskTransaction{Scopes: []string{"orka.credentials.read"}}
		task.Annotations[labels.AnnotationTransactionTokenSecret] = opts.transactionSecret
	}
	job := internalCallerAuthJob(task, "job-a", "job-uid")
	fixtureTools := map[string]*corev1alpha1.Tool{
		"gh_search": connectorTestToolWithSchema("gh_search", corev1alpha1.AgentRuntimeBrokeredToolClassRead, "github-conn", opts.parameters),
		"gh_write":  connectorTestToolWithSchema("gh_write", corev1alpha1.AgentRuntimeBrokeredToolClassWrite, "github-conn", opts.parameters),
		"plain":     connectorTestTool("plain", corev1alpha1.AgentRuntimeBrokeredToolClassRead, ""),
	}
	frozenTools := opts.tools
	if frozenTools == nil {
		frozenTools = []string{"gh_search", "gh_write", "plain"}
	}
	policy := &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "github-conn", Namespace: "default"},
		Spec:       connectorTestPolicySpec(),
	}
	digests := map[string]string{}
	for _, name := range []string{"gh_search", "gh_write"} {
		digest, err := controller.ConnectorToolDispatchDigest(fixtureTools[name].Spec, policy.Spec)
		if err != nil {
			t.Fatal(err)
		}
		digests[name] = controller.NativeConnectorToolDispatchDigest(string(fixtureTools[name].UID), string(policy.UID), digest)
	}
	digestsJSON, _ := json.Marshal(digests)
	bindingsJSON, _ := json.Marshal(task.Status.ConnectionBindings)
	bindingsEnv := string(bindingsJSON)
	if opts.bindingsEnv != nil {
		bindingsEnv = *opts.bindingsEnv
	}
	job.Spec.Template.Spec.Containers = []corev1.Container{{
		Name: "worker",
		Env: []corev1.EnvVar{
			{Name: workerenv.AITools, Value: strings.Join(frozenTools, ",")},
			{Name: workerenv.ApprovalRequiredTools, Value: workerenv.JoinCSV(opts.approvalRequired)},
			{Name: workerenv.ConnectorToolDigests, Value: string(digestsJSON)},
			{Name: workerenv.ConnectionBindings, Value: bindingsEnv},
		},
	}}
	eventStore := storetest.NewFakeExecutionEventStore()
	pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default"},
		Spec:       corev1alpha1.AgentSpec{Tools: []corev1alpha1.ToolReference{{Name: "gh_search"}, {Name: "gh_write"}, {Name: "plain"}}},
	}
	connection := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "github-abc", Namespace: "default", UID: "conn-uid", Generation: 2},
		Spec: corev1alpha1.ConnectionSpec{
			Subject: corev1alpha1.ConnectionSubject{Issuer: "https://issuer.example.test", Subject: "alice"}, ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}, Mode: opts.mode,
		},
		Status: corev1alpha1.ConnectionStatus{GrantSequence: 1, Conditions: []metav1.Condition{
			{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: "Linked", ObservedGeneration: 2},
			{Type: corev1alpha1.ConnectionConditionScopesGranted, Status: metav1.ConditionTrue, Reason: "ScopesGranted", ObservedGeneration: 2},
			{Type: corev1alpha1.ConnectionConditionProviderResolved, Status: metav1.ConditionTrue, Reason: "ProviderResolved", ObservedGeneration: 2},
		}},
	}
	connection.Name = connectors.ConnectionName("github", "https://issuer.example.test", "alice")
	provider := acceptedTestProvider()
	provider.Namespace = "default"
	mode := opts.mode
	if mode == "" {
		mode = corev1alpha1.ConnectionModeReadOnly
	}
	connection.Status.Consent = connectors.ConsentFor(provider)
	connection.Status.GrantedScopes = connectors.ScopesForMode(provider, mode)
	builder := fake.NewClientBuilder().WithScheme(internalCallerAuthScheme(t)).
		WithObjects(task, job, pod, agent, policy, provider, connection,
			fixtureTools["gh_search"].DeepCopy(), fixtureTools["gh_write"].DeepCopy(), fixtureTools["plain"].DeepCopy())
	c := builder.Build()
	cfg := ConnectorToolExecutionConfig{
		Enabled: enabled, OutboundAccess: resolver, KubeClient: k8sfake.NewSimpleClientset(), HTTPClient: &http.Client{Timeout: time.Second},
	}
	harness := &connectorToolHarness{events: eventStore, client: c, tools: fixtureTools}
	if !opts.noLedger {
		harness.effects, harness.fence = newConnectorToolLedger(t)
		cfg.ExternalEffects = harness.effects
		cfg.ControllerEpochs = staticFenceSource{fence: harness.fence}
	}
	handlers := NewInternalHandlers(nil, nil, nil, nil, nil, InternalHandlersConfig{
		Client: c, APIReader: c, ExecutionEventStore: eventStore, ConnectorTools: cfg,
	})
	app := fiber.New()
	app.Use(func(ctx fiber.Ctx) error {
		ctx.Locals(UserInfoContextKey, internalCallerAuthWorkerUser("pod-a", "pod-uid"))
		return ctx.Next()
	})
	app.Post("/internal/v1/tasks/:namespace/:taskName/connector-tools/:tool", handlers.ExecuteConnectorTool)
	harness.app = app
	return harness
}

func seedApproval(t *testing.T, eventStore *storetest.FakeExecutionEventStore, id string, args string, approve bool) {
	t.Helper()
	seedApprovalForTool(t, eventStore, id, args, approve, connectorTestTool("gh_write", corev1alpha1.AgentRuntimeBrokeredToolClassWrite, "github-conn"))
}

func seedApprovalForTool(t *testing.T, eventStore *storetest.FakeExecutionEventStore, id string, args string, approve bool, approvedTool *corev1alpha1.Tool) {
	t.Helper()
	tool := approvedTool.Name
	targetArgs, err := approvals.TargetArguments(json.RawMessage(args), approvedTool)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := approvals.TargetArgsDigest(targetArgs)
	if err != nil {
		t.Fatal(err)
	}
	specDigest, err := approvals.ConnectorTargetSpecDigest(approvedTool.Spec, connectorTestPolicySpec(), "conn-uid", 2, 1)
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
	if status, body := postConnectorTool(t, app, "gh_write", `{}`); status != http.StatusForbidden || !strings.Contains(body, "not enabled") {
		t.Fatalf("hidden write tool = %d %s", status, body)
	}
	if status, body := postConnectorTool(t, app, "gh_search", `{"arguments":{},"subject":"mallory"}`); status != http.StatusBadRequest || !strings.Contains(body, "invalid request body") {
		t.Fatalf("unknown field = %d %s", status, body)
	}
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
	h := newConnectorToolHarness(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
	app, eventStore, c := h.app, h.events, h.client

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
	if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"other"},"approvalId":"ap-1"}`); status != http.StatusForbidden || !strings.Contains(body, "does not bind") {
		t.Fatalf("mismatched arguments = %d %s", status, body)
	}
	if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1","idempotencyKey":"ap-1"}`); status != http.StatusFailedDependency || !strings.Contains(body, "no connection") {
		t.Fatalf("approved write = %d %s", status, body)
	}
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
	seedConnectorEffect(t, h, decisionSeq, 1, store.ExternalEffectInFlight, time.Now().Add(time.Minute))
	if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1","idempotencyKey":"fresh"}`); status != http.StatusConflict || !strings.Contains(body, "still executing") {
		t.Fatalf("replayed approval = %d %s", status, body)
	}
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
	if status, body := postConnectorTool(t, app, "gh_search", `{"arguments":{"q":"x"}}`); status != http.StatusFailedDependency || !strings.Contains(body, "no connection") {
		t.Fatalf("read without approval = %d %s", status, body)
	}
}

func TestExecuteConnectorToolRejectsClassDriftAfterDispatch(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app, eventStore, _ := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: nil})
	seedApproval(t, eventStore, "ap-1", `{"q":"x"}`, true)
	if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`); status != http.StatusConflict || !strings.Contains(body, "does not cover") {
		t.Fatalf("class drift = %d %s", status, body)
	}
}

func TestExecuteConnectorToolRequiresDispatchedToolList(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app, _, _ := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readOnly", tools: []string{"plain"}})
	if status, body := postConnectorTool(t, app, "gh_search", `{"arguments":{"q":"x"}}`); status != http.StatusForbidden || !strings.Contains(body, "dispatched with the task's job") {
		t.Fatalf("undispatched tool = %d %s", status, body)
	}
}

func TestExecuteConnectorToolReleasesClaimWithoutProviderRequest(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app, eventStore, _ := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
	seedApproval(t, eventStore, "ap-1", `{"q":"x"}`, true)
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

func TestExecuteConnectorToolValidatesArgumentsAgainstSchema(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app, _, _ := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{
		mode: "readOnly", parameters: `{"type":"object","required":["q"],"properties":{"q":{"type":"string"},"limit":{"type":"integer","maximum":10}},"additionalProperties":false}`,
	})
	for _, body := range []string{
		`{"arguments":{"limit":1}}`,
		`{"arguments":{"q":7}}`,
		`{"arguments":{"q":"x","limit":11}}`,
		`{"arguments":{"q":"x","extra":true}}`,
		`{"arguments":["q"]}`,
	} {
		if status, reply := postConnectorTool(t, app, "gh_search", body); status != http.StatusBadRequest || !strings.Contains(reply, "invalid tool arguments") {
			t.Fatalf("%s = %d %s, want 400 before any credential is resolved", body, status, reply)
		}
	}
	if resolver.request.PolicyName != "" {
		t.Fatal("schema-rejected arguments must not reach credential resolution")
	}
	if status, reply := postConnectorTool(t, app, "gh_search", `{"arguments":{"q":"x","limit":3}}`); status != http.StatusFailedDependency || !strings.Contains(reply, "no connection") {
		t.Fatalf("valid arguments = %d %s", status, reply)
	}
}

func TestExecuteConnectorToolRefusesToolChangedSinceDispatch(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app, eventStore, c := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
	live := &corev1alpha1.Tool{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gh_search"}, live); err != nil {
		t.Fatal(err)
	}
	live.Spec.HTTP.URL = "https://api.github.example.test/elsewhere"
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	if status, body := postConnectorTool(t, app, "gh_search", `{"arguments":{"q":"x"}}`); status != http.StatusConflict || !strings.Contains(body, "changed since dispatch") {
		t.Fatalf("retargeted read tool = %d %s", status, body)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gh_write"}, live); err != nil {
		t.Fatal(err)
	}
	live.Spec.HTTP.Method = "DELETE"
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	seedApproval(t, eventStore, "ap-1", `{"q":"x"}`, true)
	if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`); status != http.StatusConflict || !strings.Contains(body, "changed since dispatch") {
		t.Fatalf("retargeted write tool = %d %s", status, body)
	}
	claims, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-a",
		EventTypes: []string{events.ExecutionEventTypeApprovalExecutionUpdated}, Limit: 10,
	})
	if err != nil || len(claims) != 0 {
		t.Fatalf("claim events = %d err = %v, want none", len(claims), err)
	}
}

func TestExecuteConnectorToolRequiresJobBindingsToMatchTaskStatus(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	other := `[{"policyName":"github-conn","provider":"github","connectionName":"github-abc","uid":"other-uid","generation":2,"grantSequence":1,"mode":"readOnly"}]`
	app, _, _ := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readOnly", bindingsEnv: &other})
	if status, body := postConnectorTool(t, app, "gh_search", `{"arguments":{"q":"x"}}`); status != http.StatusConflict || !strings.Contains(body, "do not match its dispatched job") {
		t.Fatalf("mismatched bindings = %d %s", status, body)
	}
	none := ""
	app, _, _ = newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readOnly", bindingsEnv: &none})
	if status, body := postConnectorTool(t, app, "gh_search", `{"arguments":{"q":"x"}}`); status != http.StatusConflict {
		t.Fatalf("missing job bindings = %d %s", status, body)
	}
	if resolver.request.PolicyName != "" {
		t.Fatal("a binding mismatch must not reach credential resolution")
	}
}

func TestExecuteConnectorToolBindsApprovalToFrozenConnection(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app, eventStore, _ := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
	approvedTool := connectorTestTool("gh_write", corev1alpha1.AgentRuntimeBrokeredToolClassWrite, "github-conn")
	targetArgs, _ := approvals.TargetArguments(json.RawMessage(`{"q":"x"}`), approvedTool)
	argsDigest, _ := approvals.TargetArgsDigest(targetArgs)
	staleDigest, _ := approvals.ConnectorTargetSpecDigest(approvedTool.Spec, connectorTestPolicySpec(), "previous-account-uid", 1, 1)
	requested, _ := json.Marshal(map[string]any{"approvalID": "ap-stale", "taskUID": "task-uid", "targetTool": "gh_write", "targetArgsDigest": argsDigest, "targetSpecDigest": staleDigest, "action": "Execute gh_write"})
	decided, _ := json.Marshal(map[string]any{"approvalID": "ap-stale", "actor": "reviewer"})
	for _, event := range []*store.ExecutionEvent{
		{Type: events.ExecutionEventTypeApprovalRequested, Content: requested},
		{Type: events.ExecutionEventTypeApprovalApproved, Content: decided},
	} {
		event.Namespace, event.StreamType, event.StreamID, event.TaskName = "default", store.ExecutionEventStreamTypeTask, "task-a", "task-a"
		event.ToolName, event.ToolCallID, event.CreatedAt = "gh_write", "ap-stale", time.Now()
		if _, err := eventStore.AppendExecutionEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-stale"}`); status != http.StatusConflict || !strings.Contains(body, "linked account changed") {
		t.Fatalf("approval for another account = %d %s", status, body)
	}
}

func TestExecuteConnectorToolReleasesClaimWhenAuthorityBindingFails(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app, eventStore, _ := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{
		mode: "readWrite", approvalRequired: []string{"gh_write"}, transactionSecret: "missing-tx-secret",
	})
	seedApproval(t, eventStore, "ap-1", `{"q":"x"}`, true)
	for range 2 {
		if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`); status != http.StatusInternalServerError || !strings.Contains(body, "bind task authority") {
			t.Fatalf("authority binding failure = %d %s", status, body)
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

func TestExecuteConnectorToolRecordsApprovedCallsInEffectLedger(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	h := newConnectorToolHarness(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
	seedApproval(t, h.events, "ap-1", `{"q":"x"}`, true)
	for range 2 {
		if status, body := postConnectorTool(t, h.app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`); status != http.StatusFailedDependency || !strings.Contains(body, "no connection") {
			t.Fatalf("pre-request failure = %d %s", status, body)
		}
	}
	history, err := approvals.ListEvents(context.Background(), h.events, "default", "task-a")
	if err != nil {
		t.Fatal(err)
	}
	var decisionSeq int64
	for _, approval := range approvals.Derive(history, time.Now()) {
		if approval.ID == "ap-1" {
			decisionSeq = approval.DecisionSeq
		}
	}
	task := connectorToolFixtures()
	tool := h.tools["gh_write"]
	targetArgs, _ := approvals.TargetArguments(json.RawMessage(`{"q":"x"}`), tool)
	argsDigest, _ := approvals.TargetArgsDigest(targetArgs)
	specDigest, _ := approvals.ConnectorTargetSpecDigest(tool.Spec, connectorTestPolicySpec(), "conn-uid", 2, 1)
	runFor := func(releases int) connectorToolRun {
		return connectorToolRun{task: task, tool: tool, binding: task.Status.ConnectionBindings[0], claim: &connectorApprovalClaim{
			approvalID: "ap-1", key: fmt.Sprintf("connector-approval-claim:ap-1:%d:%d", decisionSeq, releases), releases: releases,
			argsDigest: argsDigest, specDigest: specDigest,
		}}
	}
	for releases := range 2 {
		run := runFor(releases)
		id, err := connectorToolEffectIdentity(run.task, run.claim).CanonicalID()
		if err != nil {
			t.Fatal(err)
		}
		effect, err := h.effects.GetExternalEffect(context.Background(), id)
		if err != nil || effect.State != store.ExternalEffectFailed {
			t.Fatalf("effect for claim %d = %+v err = %v, want Failed", releases, effect, err)
		}
		if requestDigest, err := controller.ExternalEffectRequestDigest(effect.Identity, connectorToolEffectRequest(run)); err != nil || requestDigest != effect.RequestDigest {
			t.Fatalf("effect request digest = %q, want the tool, approval, argument, spec, and connection digests bound (%q, err %v)", effect.RequestDigest, requestDigest, err)
		}
	}
	committed := runFor(2)
	identity := connectorToolEffectIdentity(committed.task, committed.claim)
	requestDigest, err := controller.ExternalEffectRequestDigest(identity, connectorToolEffectRequest(committed))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	reserved, err := h.effects.ReserveExternalEffect(context.Background(), store.ReserveExternalEffectRequest{Identity: identity, RequestDigest: requestDigest, Fence: h.fence, CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	response, _ := json.Marshal(connectorToolOutcome{Result: `{"ok":true}`})
	lease := now.Add(time.Minute)
	inFlight, err := h.effects.TransitionExternalEffect(context.Background(), store.ExternalEffectTransition{
		ID: reserved.ID, Fence: h.fence, ExpectedVersion: reserved.Version, ExpectedState: reserved.State, NewState: store.ExternalEffectInFlight,
		RequestDigest: requestDigest, LeaseOwner: "controller", LeaseExpiresAt: &lease, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.effects.TransitionExternalEffect(context.Background(), store.ExternalEffectTransition{
		ID: inFlight.ID, Fence: h.fence, ExpectedVersion: inFlight.Version, ExpectedState: store.ExternalEffectInFlight, NewState: store.ExternalEffectSucceeded,
		RequestDigest: requestDigest, ResponseDigest: store.CanonicalBytesDigest(response), Response: response, ExpectedLeaseOwner: "controller", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	spent, _ := json.Marshal(map[string]any{"approvalID": "ap-1", "executionOutcome": "running"})
	if _, appended, err := h.events.AppendExecutionEventIfAbsent(context.Background(), &store.ExecutionEvent{
		Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-a", TaskName: "task-a",
		Type: events.ExecutionEventTypeApprovalExecutionUpdated, ToolCallID: "ap-1", Content: spent, CreatedAt: time.Now(),
	}, committed.claim.key); err != nil || !appended {
		t.Fatalf("spent claim seed appended = %v err = %v", appended, err)
	}
	resolver.request = outboundaccess.ResolveRequest{}
	status, body := postConnectorTool(t, h.app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`)
	if status != http.StatusOK || !strings.Contains(body, `"replayed":true`) || !strings.Contains(body, `ok`) {
		t.Fatalf("replay = %d %s", status, body)
	}
	if resolver.request.PolicyName != "" {
		t.Fatal("a replayed result must not resolve a credential or contact the provider")
	}
	if status, body := postConnectorTool(t, h.app, "gh_write", `{"arguments":{"q":"other"},"approvalId":"ap-1"}`); status != http.StatusForbidden || !strings.Contains(body, "does not bind") {
		t.Fatalf("other arguments = %d %s", status, body)
	}
}

func TestExecuteConnectorToolRefusesApprovedCallsWithoutLedger(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app, eventStore, _ := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}, noLedger: true})
	seedApproval(t, eventStore, "ap-1", `{"q":"x"}`, true)
	if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`); status != http.StatusServiceUnavailable || !strings.Contains(body, "ledger is unavailable") {
		t.Fatalf("no ledger = %d %s", status, body)
	}
	if status, body := postConnectorTool(t, app, "gh_search", `{"arguments":{"q":"x"}}`); status != http.StatusFailedDependency {
		t.Fatalf("read without ledger = %d %s", status, body)
	}
}

func TestExecuteConnectorToolRefusesPolicyChangedSinceDispatch(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app, _, c := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readOnly"})
	live := &corev1alpha1.OutboundAccessPolicy{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "github-conn"}, live); err != nil {
		t.Fatal(err)
	}
	live.Spec.Connection.Output = &corev1alpha1.OutboundCredentialOutput{Header: "X-Linked-Token"}
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	if status, body := postConnectorTool(t, app, "gh_search", `{"arguments":{"q":"x"}}`); status != http.StatusConflict || !strings.Contains(body, "changed since dispatch") {
		t.Fatalf("policy output drift = %d %s", status, body)
	}
	if resolver.request.PolicyName != "" {
		t.Fatal("a changed policy must not reach credential resolution")
	}
}

func seedConnectorEffect(t *testing.T, h *connectorToolHarness, decisionSeq int64, releases int, state store.ExternalEffectState, lease time.Time) {
	t.Helper()
	task := connectorToolFixtures()
	tool := h.tools["gh_write"]
	targetArgs, _ := approvals.TargetArguments(json.RawMessage(`{"q":"x"}`), tool)
	argsDigest, _ := approvals.TargetArgsDigest(targetArgs)
	specDigest, _ := approvals.ConnectorTargetSpecDigest(tool.Spec, connectorTestPolicySpec(), "conn-uid", 2, 1)
	run := connectorToolRun{task: task, tool: tool, binding: task.Status.ConnectionBindings[0], claim: &connectorApprovalClaim{
		approvalID: "ap-1", key: fmt.Sprintf("connector-approval-claim:ap-1:%d:%d", decisionSeq, releases), releases: releases, argsDigest: argsDigest, specDigest: specDigest,
	}}
	identity := connectorToolEffectIdentity(run.task, run.claim)
	requestDigest, err := controller.ExternalEffectRequestDigest(identity, connectorToolEffectRequest(run))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	reserved, err := h.effects.ReserveExternalEffect(context.Background(), store.ReserveExternalEffectRequest{Identity: identity, RequestDigest: requestDigest, Fence: h.fence, CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if state == store.ExternalEffectPending {
		return
	}
	inFlight, err := h.effects.TransitionExternalEffect(context.Background(), store.ExternalEffectTransition{
		ID: reserved.ID, Fence: h.fence, ExpectedVersion: reserved.Version, ExpectedState: reserved.State, NewState: store.ExternalEffectInFlight,
		RequestDigest: requestDigest, LeaseOwner: "controller", LeaseExpiresAt: &lease, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state == store.ExternalEffectInFlight {
		return
	}
	if _, err := h.effects.TransitionExternalEffect(context.Background(), store.ExternalEffectTransition{
		ID: inFlight.ID, Fence: h.fence, ExpectedVersion: inFlight.Version, ExpectedState: store.ExternalEffectInFlight, NewState: state,
		RequestDigest: requestDigest, ExpectedLeaseOwner: "controller", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExecuteConnectorToolReconcilesSpentClaimsFromTheLedger(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	h := newConnectorToolHarness(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
	seedApproval(t, h.events, "ap-1", `{"q":"x"}`, true)
	history, err := approvals.ListEvents(context.Background(), h.events, "default", "task-a")
	if err != nil {
		t.Fatal(err)
	}
	var decisionSeq int64
	for _, approval := range approvals.Derive(history, time.Now()) {
		if approval.ID == "ap-1" {
			decisionSeq = approval.DecisionSeq
		}
	}
	claimSpent := func(releases int) {
		t.Helper()
		spent, _ := json.Marshal(map[string]any{"approvalID": "ap-1", "executionOutcome": "running"})
		if _, appended, err := h.events.AppendExecutionEventIfAbsent(context.Background(), &store.ExecutionEvent{
			Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-a", TaskName: "task-a",
			Type: events.ExecutionEventTypeApprovalExecutionUpdated, ToolCallID: "ap-1", Content: spent, CreatedAt: time.Now(),
		}, fmt.Sprintf("connector-approval-claim:ap-1:%d:%d", decisionSeq, releases)); err != nil || !appended {
			t.Fatalf("seed spent claim %d: appended = %t err = %v", releases, appended, err)
		}
	}
	claimSpent(0)
	if status, body := postConnectorTool(t, h.app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`); status != http.StatusFailedDependency || !strings.Contains(body, "no connection") {
		t.Fatalf("unstarted claim = %d %s", status, body)
	}
	if fenced, err := h.effects.GetExternalEffect(context.Background(), mustEffectID(t, decisionSeq, 0)); err != nil || fenced.State != store.ExternalEffectFailed {
		t.Fatalf("fenced record = %+v err = %v, want Failed so the original request cannot start", fenced, err)
	}
	claimSpent(2)
	seedConnectorEffect(t, h, decisionSeq, 2, store.ExternalEffectPending, time.Time{})
	if status, body := postConnectorTool(t, h.app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`); status != http.StatusFailedDependency || !strings.Contains(body, "no connection") {
		t.Fatalf("pending claim = %d %s", status, body)
	}
	claims, err := h.events.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-a",
		EventTypes: []string{events.ExecutionEventTypeApprovalExecutionUpdated}, Limit: 20,
	})
	if err != nil || len(claims) != 8 {
		t.Fatalf("claim/release events = %d err = %v, want each stopped claim released and each retry claimed then released", len(claims), err)
	}
	claimSpent(4)
	seedConnectorEffect(t, h, decisionSeq, 4, store.ExternalEffectInFlight, time.Now().Add(150*time.Millisecond))
	time.Sleep(200 * time.Millisecond)
	resolver.request = outboundaccess.ResolveRequest{}
	if status, body := postConnectorTool(t, h.app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`); status != http.StatusBadGateway || !strings.Contains(body, "outcome is unknown") {
		t.Fatalf("expired in-flight claim = %d %s", status, body)
	}
	if status, body := postConnectorTool(t, h.app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`); status != http.StatusBadGateway || !strings.Contains(body, "outcome is unknown") {
		t.Fatalf("settled unknown claim = %d %s", status, body)
	}
	if resolver.request.PolicyName != "" {
		t.Fatal("an unknown outcome must not be re-executed")
	}
}

func TestExecuteConnectorToolRetainsBoundedResultsInTheLedger(t *testing.T) {
	small := connectorToolOutcomeFor("ok")
	if small.Receipt != nil || small.Result != "ok" {
		t.Fatalf("small outcome = %+v", small)
	}
	large := connectorToolOutcomeFor(strings.Repeat("x", connectorToolResultRetention+1))
	if large.Result != "" || large.Receipt == nil || large.Receipt.Bytes != connectorToolResultRetention+1 || large.Receipt.Digest == "" {
		t.Fatalf("large outcome = %+v, want a receipt instead of the result", large)
	}
	encoded, _ := json.Marshal(large)
	if len(encoded) > 1024 {
		t.Fatalf("receipt must stay small, got %d bytes", len(encoded))
	}
}

func mustEffectID(t *testing.T, decisionSeq int64, releases int) string {
	t.Helper()
	id, err := store.ExternalEffectIdentity{
		Kind: connectorToolEffectKind, Namespace: "default", AggregateID: "task-uid",
		OperationID: fmt.Sprintf("connector-approval-claim:ap-1:%d:%d", decisionSeq, releases),
	}.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestConnectorClaimReleasesAreScopedToTheDecision(t *testing.T) {
	release := func(seq int64) store.ExecutionEvent {
		content, _ := json.Marshal(map[string]any{"approvalID": "ap-1", "reason": connectorClaimReleaseReason, "decisionSeq": seq})
		return store.ExecutionEvent{Type: events.ExecutionEventTypeApprovalExecutionUpdated, ToolCallID: "ap-1", Content: content}
	}
	other, _ := json.Marshal(map[string]any{"approvalID": "ap-2", "reason": connectorClaimReleaseReason, "decisionSeq": int64(7)})
	history := []store.ExecutionEvent{
		release(3), release(3), release(7),
		{Type: events.ExecutionEventTypeApprovalExecutionUpdated, ToolCallID: "ap-2", Content: other},
	}
	if got := connectorClaimReleases(history, "ap-1", 3); got != 2 {
		t.Fatalf("releases for decision 3 = %d, want 2", got)
	}
	if got := connectorClaimReleases(history, "ap-1", 7); got != 1 {
		t.Fatalf("releases for decision 7 = %d, want 1", got)
	}
	if got := connectorClaimReleases(history, "ap-1", 9); got != 0 {
		t.Fatalf("releases for a fresh decision = %d, want 0", got)
	}
}

func connectorTestPolicySpec() corev1alpha1.OutboundAccessPolicySpec {
	return corev1alpha1.OutboundAccessPolicySpec{Connection: &corev1alpha1.ConnectionOutboundAccess{ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}}}
}

func TestExecuteConnectorToolApprovalBindsPolicyConfiguration(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app, eventStore, _ := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
	approvedTool := connectorTestTool("gh_write", corev1alpha1.AgentRuntimeBrokeredToolClassWrite, "github-conn")
	targetArgs, _ := approvals.TargetArguments(json.RawMessage(`{"q":"x"}`), approvedTool)
	argsDigest, _ := approvals.TargetArgsDigest(targetArgs)
	previous := connectorTestPolicySpec()
	previous.Connection.Output = &corev1alpha1.OutboundCredentialOutput{Header: "X-Previous-Token"}
	staleDigest, _ := approvals.ConnectorTargetSpecDigest(approvedTool.Spec, previous, "conn-uid", 2, 1)
	requested, _ := json.Marshal(map[string]any{"approvalID": "ap-policy", "taskUID": "task-uid", "targetTool": "gh_write", "targetArgsDigest": argsDigest, "targetSpecDigest": staleDigest, "action": "Execute gh_write"})
	decided, _ := json.Marshal(map[string]any{"approvalID": "ap-policy", "actor": "reviewer"})
	for _, event := range []*store.ExecutionEvent{
		{Type: events.ExecutionEventTypeApprovalRequested, Content: requested},
		{Type: events.ExecutionEventTypeApprovalApproved, Content: decided},
	} {
		event.Namespace, event.StreamType, event.StreamID, event.TaskName = "default", store.ExecutionEventStreamTypeTask, "task-a", "task-a"
		event.ToolName, event.ToolCallID, event.CreatedAt = "gh_write", "ap-policy", time.Now()
		if _, err := eventStore.AppendExecutionEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	if status, body := postConnectorTool(t, app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-policy"}`); status != http.StatusConflict || !strings.Contains(body, "changed since approval") {
		t.Fatalf("approval under another policy configuration = %d %s", status, body)
	}
}

func TestExecuteConnectorToolRefusesBindingWithoutGrant(t *testing.T) {
	resolver := &stubOutboundResolver{}
	app, _, _ := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readOnly", noGrant: true})
	if status, body := postConnectorTool(t, app, "gh_search", `{"arguments":{}}`); status != http.StatusFailedDependency || !strings.Contains(body, "carries no grant") {
		t.Fatalf("no grant = %d %s", status, body)
	}
	if resolver.request.PolicyName != "" {
		t.Fatal("resolution must not be attempted for a binding without a grant")
	}
	resolver = &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app = newConnectorToolApp(t, resolver, true)
	if status, _ := postConnectorTool(t, app, "gh_search", `{"arguments":{}}`); status != http.StatusFailedDependency {
		t.Fatalf("resolver refusal = %d", status)
	}
	if resolver.request.CheckedPolicy == nil || resolver.request.PolicyName != "github-conn" {
		t.Fatalf("resolve request = %+v, want the checked policy pinned", resolver.request)
	}
}

func TestConnectorToolTimeoutClamps(t *testing.T) {
	tool := &corev1alpha1.Tool{Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{Timeout: &metav1.Duration{Duration: 3 * time.Hour}}}}
	if got := connectorToolTimeout(tool); got != connectorToolMaxTimeout {
		t.Fatalf("timeout = %v, want the %v maximum", got, connectorToolMaxTimeout)
	}
	tool.Spec.HTTP.Timeout = &metav1.Duration{Duration: 45 * time.Second}
	if got := connectorToolTimeout(tool); got != 45*time.Second {
		t.Fatalf("timeout = %v, want the declared 45s", got)
	}
	if got := connectorToolTimeout(&corev1alpha1.Tool{}); got != connectorToolDefaultTimeout {
		t.Fatalf("timeout = %v, want the default", got)
	}
}

func TestConnectorBindingDigestSeparatesGrants(t *testing.T) {
	first := corev1alpha1.ConnectionBinding{PolicyName: "github-conn", UID: "conn-uid", Generation: 3, GrantSequence: 1}
	relinked := first
	relinked.GrantSequence = 2
	if connectorBindingDigest(first) == connectorBindingDigest(relinked) {
		t.Fatal("calls under different grants must record different connection digests")
	}
	again := first
	if connectorBindingDigest(first) != connectorBindingDigest(again) {
		t.Fatal("the digest must be stable for one grant")
	}
}

type revokingResolver struct {
	client client.Client
}

func (r *revokingResolver) Resolve(ctx context.Context, _ outboundaccess.ResolveRequest) (outboundaccess.Resolution, error) {
	pod := &corev1.Pod{}
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "pod-a"}, pod); err == nil {
		_ = r.client.Delete(ctx, pod)
	}
	return outboundaccess.Resolution{Adapter: outboundaccess.AdapterConnection, CredentialHeader: "Authorization", CredentialValue: "Bearer gho_person"}, nil
}

func TestExecuteConnectorToolRechecksAuthorityAtTheSendBoundary(t *testing.T) {
	resolver := &revokingResolver{}
	h := newConnectorToolHarness(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
	resolver.client = h.client
	seedApproval(t, h.events, "ap-1", `{"q":"x"}`, true)
	status, body := postConnectorTool(t, h.app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`)
	if status == http.StatusBadGateway || status < 400 || status >= 500 {
		t.Fatalf("revoked caller = %d %s, want a refusal before any provider request", status, body)
	}
	claims, err := h.events.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-a",
		EventTypes: []string{events.ExecutionEventTypeApprovalExecutionUpdated}, Limit: 10,
	})
	if err != nil || len(claims) != 2 {
		t.Fatalf("claim/release events = %d err = %v, want the claim handed back", len(claims), err)
	}
}

func TestSettlePendingConnectorEffectLeavesInFlightRecords(t *testing.T) {
	h := newConnectorToolHarness(t, &stubOutboundResolver{}, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
	task := connectorToolFixtures()
	identity := func(releases int) store.ExternalEffectIdentity {
		return connectorToolEffectIdentity(task, &connectorApprovalClaim{key: fmt.Sprintf("connector-approval-claim:ap-1:1:%d", releases)})
	}
	seedConnectorEffect(t, h, 1, 0, store.ExternalEffectInFlight, time.Now().Add(time.Hour))
	if err := controller.SettlePendingExternalEffect(context.Background(), h.effects, h.fence, identity(0), store.ExternalEffectFailed); !errors.Is(err, controller.ErrExternalEffectNotPending) {
		t.Fatalf("settling an in-flight record err = %v, want ErrExternalEffectNotPending", err)
	}
	id, _ := identity(0).CanonicalID()
	if effect, err := h.effects.GetExternalEffect(context.Background(), id); err != nil || effect.State != store.ExternalEffectInFlight {
		t.Fatalf("in-flight record = %+v err = %v, want it untouched", effect, err)
	}
	seedConnectorEffect(t, h, 1, 1, store.ExternalEffectPending, time.Time{})
	if err := controller.SettlePendingExternalEffect(context.Background(), h.effects, h.fence, identity(1), store.ExternalEffectFailed); err != nil {
		t.Fatalf("settling a pending record: %v", err)
	}
	id, _ = identity(1).CanonicalID()
	if effect, err := h.effects.GetExternalEffect(context.Background(), id); err != nil || effect.State != store.ExternalEffectFailed {
		t.Fatalf("pending record = %+v err = %v, want Failed", effect, err)
	}
}

func TestExecuteConnectorToolRefusesARecreatedTool(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	h := newConnectorToolHarness(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
	live := &corev1alpha1.Tool{}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gh_search"}, live); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Delete(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	recreated := h.tools["gh_search"].DeepCopy()
	recreated.UID = "recreated-uid"
	recreated.ResourceVersion = ""
	if err := h.client.Create(context.Background(), recreated); err != nil {
		t.Fatal(err)
	}
	if status, body := postConnectorTool(t, h.app, "gh_search", `{"arguments":{"q":"x"}}`); status != http.StatusConflict || !strings.Contains(body, "changed since dispatch") {
		t.Fatalf("recreated tool = %d %s", status, body)
	}
}

type withdrawingResolver struct {
	client   client.Client
	recreate bool
}

func (r *withdrawingResolver) Resolve(ctx context.Context, _ outboundaccess.ResolveRequest) (outboundaccess.Resolution, error) {
	tool := &corev1alpha1.Tool{}
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "gh_write"}, tool); err == nil {
		_ = r.client.Delete(ctx, tool)
		if r.recreate {
			replacement := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "gh_write", Namespace: "default", UID: "recreated-uid"}, Spec: tool.Spec}
			_ = r.client.Create(ctx, replacement)
		}
	}
	return outboundaccess.Resolution{Adapter: outboundaccess.AdapterConnection, CredentialHeader: "Authorization", CredentialValue: "Bearer gho_person"}, nil
}

func TestExecuteConnectorToolRechecksTheToolAtTheSendBoundary(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		resolver := &withdrawingResolver{recreate: recreate}
		h := newConnectorToolHarness(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
		resolver.client = h.client
		seedApproval(t, h.events, "ap-1", `{"q":"x"}`, true)
		status, body := postConnectorTool(t, h.app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`)
		if status != http.StatusConflict || !strings.Contains(body, "since dispatch") {
			t.Fatalf("recreate=%t: status = %d %s, want a refusal before any provider request", recreate, status, body)
		}
		claims, err := h.events.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
			Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-a",
			EventTypes: []string{events.ExecutionEventTypeApprovalExecutionUpdated}, Limit: 10,
		})
		if err != nil || len(claims) != 2 {
			t.Fatalf("recreate=%t: claim/release events = %d err = %v, want the claim handed back", recreate, len(claims), err)
		}
	}
}

type policyWithdrawingResolver struct{ client client.Client }

func (r *policyWithdrawingResolver) Resolve(ctx context.Context, req outboundaccess.ResolveRequest) (outboundaccess.Resolution, error) {
	policy := &corev1alpha1.OutboundAccessPolicy{}
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: "default", Name: req.PolicyName}, policy); err == nil {
		_ = r.client.Delete(ctx, policy)
	}
	return outboundaccess.Resolution{Adapter: outboundaccess.AdapterConnection, CredentialHeader: "Authorization", CredentialValue: "Bearer gho_person"}, nil
}

func TestExecuteConnectorToolRechecksThePolicyAtTheSendBoundary(t *testing.T) {
	resolver := &policyWithdrawingResolver{}
	h := newConnectorToolHarness(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
	resolver.client = h.client
	seedApproval(t, h.events, "ap-1", `{"q":"x"}`, true)
	status, body := postConnectorTool(t, h.app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`)
	if status != http.StatusConflict || !strings.Contains(body, "outbound access policy was withdrawn since dispatch") {
		t.Fatalf("status = %d %s, want a refusal before any provider request", status, body)
	}
}

type connectionDisconnectingResolver struct{ client client.Client }

func (r *connectionDisconnectingResolver) Resolve(ctx context.Context, req outboundaccess.ResolveRequest) (outboundaccess.Resolution, error) {
	connection := &corev1alpha1.Connection{}
	key := client.ObjectKey{Namespace: "default", Name: connectors.ConnectionName("github", "https://issuer.example.test", "alice")}
	if err := r.client.Get(ctx, key, connection); err == nil {
		_ = r.client.Delete(ctx, connection)
	}
	return outboundaccess.Resolution{Adapter: outboundaccess.AdapterConnection, CredentialHeader: "Authorization", CredentialValue: "Bearer gho_person"}, nil
}

func TestExecuteConnectorToolRechecksTheConnectionAtTheSendBoundary(t *testing.T) {
	resolver := &connectionDisconnectingResolver{}
	h := newConnectorToolHarness(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
	resolver.client = h.client
	seedApproval(t, h.events, "ap-1", `{"q":"x"}`, true)
	status, body := postConnectorTool(t, h.app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`)
	if status != http.StatusConflict || !strings.Contains(body, "connection was disconnected since dispatch") {
		t.Fatalf("status = %d %s, want a refusal before any provider request", status, body)
	}
}

func TestExecuteConnectorToolRefusesTrailingRequestData(t *testing.T) {
	h := newConnectorToolHarness(t, &stubOutboundResolver{}, true, connectorToolAppOptions{mode: "readOnly"})
	status, body := postConnectorTool(t, h.app, "gh_search", `{"arguments":{"q":"x"}} trailing`)
	if status != http.StatusBadRequest || !strings.Contains(body, "single JSON object") {
		t.Fatalf("status = %d %s, want the trailing data refused", status, body)
	}
}

type deadlineResolver struct{ deadline time.Time }

func (r *deadlineResolver) Resolve(ctx context.Context, _ outboundaccess.ResolveRequest) (outboundaccess.Resolution, error) {
	r.deadline, _ = ctx.Deadline()
	return outboundaccess.Resolution{}, errors.New("the requester has no connection to this provider")
}

func TestExecuteConnectorToolBoundsReadCalls(t *testing.T) {
	resolver := &deadlineResolver{}
	h := newConnectorToolHarness(t, resolver, true, connectorToolAppOptions{mode: "readOnly"})
	started := time.Now()
	if status, body := postConnectorTool(t, h.app, "gh_search", `{"arguments":{"q":"x"}}`); status != http.StatusFailedDependency {
		t.Fatalf("read call = %d %s", status, body)
	}
	if resolver.deadline.IsZero() || resolver.deadline.After(started.Add(connectorToolDefaultTimeout+time.Second)) {
		t.Fatalf("deadline = %v, want within the %v connector default", resolver.deadline, connectorToolDefaultTimeout)
	}
}

func TestExecuteConnectorToolReportsAnUnreplayableReceipt(t *testing.T) {
	h := newConnectorToolHarness(t, &stubOutboundResolver{}, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
	seedApproval(t, h.events, "ap-1", `{"q":"x"}`, true)
	history, err := approvals.ListEvents(context.Background(), h.events, "default", "task-a")
	if err != nil {
		t.Fatal(err)
	}
	var decisionSeq int64
	for _, approval := range approvals.Derive(history, time.Now()) {
		if approval.ID == "ap-1" {
			decisionSeq = approval.DecisionSeq
		}
	}
	seedConnectorEffect(t, h, decisionSeq, 0, store.ExternalEffectInFlight, time.Now().Add(time.Minute))
	inFlight, err := h.effects.GetExternalEffect(context.Background(), mustEffectID(t, decisionSeq, 0))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(connectorToolOutcomeFor(strings.Repeat("x", connectorToolResultRetention+1)))
	if _, err := h.effects.TransitionExternalEffect(context.Background(), store.ExternalEffectTransition{
		ID: inFlight.ID, Fence: h.fence, ExpectedVersion: inFlight.Version, ExpectedState: store.ExternalEffectInFlight,
		NewState: store.ExternalEffectSucceeded, RequestDigest: inFlight.RequestDigest,
		ResponseDigest: store.CanonicalBytesDigest(encoded), Response: encoded, ExpectedLeaseOwner: "controller", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	status, body := postConnectorTool(t, h.app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`)
	if status != http.StatusBadGateway || !strings.Contains(body, "cannot be replayed") {
		t.Fatalf("receipt replay = %d %s, want the unreplayable result reported", status, body)
	}
	if effect, err := h.effects.GetExternalEffect(context.Background(), inFlight.ID); err != nil || effect.State != store.ExternalEffectSucceeded {
		t.Fatalf("effect = %+v err = %v, want it left Succeeded", effect, err)
	}
	claims, err := h.events.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-a",
		EventTypes: []string{events.ExecutionEventTypeApprovalExecutionUpdated}, Limit: 10,
	})
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim/release events = %d err = %v, want the claim kept spent", len(claims), err)
	}
}
