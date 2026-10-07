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
	// The API sealed this stamp for the Task's UID; the controller trusts
	// the requester only through it.
	controller.SetRequesterStampKey(testConnectorStampKey)
	task.Annotations[labels.AnnotationRequestedByStamp] = connectors.RequesterStamp(testConnectorStampKey, task.UID, task.Spec.RequestedBy.Issuer, task.Spec.RequestedBy.Subject)
	task.Status.ConnectionBindings = []corev1alpha1.ConnectionBinding{{PolicyName: "github-conn", Provider: "github", ConnectionName: "github-abc", UID: "conn-uid", Generation: 2, GrantSequence: 1, Mode: "readOnly"}}
	return task
}

// testConnectorStampKey is the requester stamp key the endpoint tests verify under.
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
	mode             string
	approvalRequired []string
	// tools is the tool list frozen into the Job; nil means every fixture tool.
	tools []string
	// bindingsEnv overrides the bindings frozen into the Job; nil means the
	// Task's status bindings.
	bindingsEnv *string
	// parameters is the JSON Schema of every fixture connector tool.
	parameters string
	// transactionSecret names a Task-owned transaction-token Secret that
	// does not exist, so authority binding fails after the approval claim.
	transactionSecret string
	// noLedger leaves the effect ledger unconfigured.
	noLedger bool
	// noGrant freezes a binding that carries no grant sequence.
	noGrant bool
}

// connectorToolHarness is the endpoint under test with its fakes.
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

// newConnectorToolLedger opens an in-memory effect ledger with one live
// controller epoch, the way the production controller runs.
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
	// The tool list, approval policy, connector tool digests, and Connection
	// bindings the Job builder froze at dispatch.
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
	// The requester's Connection name must match the deterministic form the
	// controller derives; override the fixture name accordingly.
	connection.Name = connectors.ConnectionName("github", "https://issuer.example.test", "alice")
	// The link was consented against the provider as it stands, which
	// dispatch revalidates before treating the Connection as usable.
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

// seedApproval records a requested-then-approved approval for tool bound to
// args, the way the worker's gate and a human decision would.
func seedApproval(t *testing.T, eventStore *storetest.FakeExecutionEventStore, id string, args string, approve bool) {
	t.Helper()
	seedApprovalForTool(t, eventStore, id, args, approve, connectorTestTool("gh_write", corev1alpha1.AgentRuntimeBrokeredToolClassWrite, "github-conn"))
}

func seedApprovalForTool(t *testing.T, eventStore *storetest.FakeExecutionEventStore, id string, args string, approve bool, approvedTool *corev1alpha1.Tool) {
	t.Helper()
	tool := approvedTool.Name
	// The worker digests the plain spec of a connector-backed tool together
	// with the Connection frozen into its Job.
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
	h := newConnectorToolHarness(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
	app, eventStore, c := h.app, h.events, h.client

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
	// A claim whose call reached the provider never hands itself back:
	// while its effect record is in flight under a live lease, presenting
	// the same approval again, with any idempotency key, is refused.
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
	// Arguments the schema admits reach resolution.
	if status, reply := postConnectorTool(t, app, "gh_search", `{"arguments":{"q":"x","limit":3}}`); status != http.StatusFailedDependency || !strings.Contains(reply, "no connection") {
		t.Fatalf("valid arguments = %d %s", status, reply)
	}
}

func TestExecuteConnectorToolRefusesToolChangedSinceDispatch(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app, eventStore, c := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
	// A read tool retargeted after dispatch: same name, class, and policy,
	// different URL. Read tools pass no approval, so the dispatch digest is
	// the only thing standing between the worker and the new destination.
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
	// The same drift on a write tool is refused before the approval is
	// claimed, so the approval stays usable once the Task is re-dispatched.
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
	// Task status names a different Connection than the one frozen into the
	// Job: neither a rewritten status nor a recovered Job may redirect the call.
	other := `[{"policyName":"github-conn","provider":"github","connectionName":"github-abc","uid":"other-uid","generation":2,"grantSequence":1,"mode":"readOnly"}]`
	app, _, _ := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readOnly", bindingsEnv: &other})
	if status, body := postConnectorTool(t, app, "gh_search", `{"arguments":{"q":"x"}}`); status != http.StatusConflict || !strings.Contains(body, "do not match its dispatched job") {
		t.Fatalf("mismatched bindings = %d %s", status, body)
	}
	// A Job dispatched without any binding for the policy can never bind a
	// credential; the call fails closed before approval or resolution.
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
	// An approval requested while a different account was linked (the
	// worker digested another Connection identity) does not authorize a
	// call under the Connection frozen with this Job.
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
	// Loading the Task's transaction-token Secret fails after the claim and
	// before any provider request: the claim is handed back each time.
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
	// Every approval-claimed call is reserved in the durable effect ledger
	// under its claim before the provider is contacted; one that fails
	// before any request settles as Failed and hands the claim back.
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
	// A claim that reached the provider and committed its result stays
	// spent; a worker that lost the response (a controller restart between
	// commit and reply) receives the committed result again from the ledger
	// with no second request, while other arguments are still refused.
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
	// Read calls have no effect record and still run.
	if status, body := postConnectorTool(t, app, "gh_search", `{"arguments":{"q":"x"}}`); status != http.StatusFailedDependency {
		t.Fatalf("read without ledger = %d %s", status, body)
	}
}

func TestExecuteConnectorToolRefusesPolicyChangedSinceDispatch(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app, _, c := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readOnly"})
	// The policy keeps its name and provider but changes how the credential
	// is injected after dispatch; the frozen dispatch digest covers it.
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

// seedConnectorEffect records an effect for the claim (approval ap-1,
// decisionSeq, releases) in the given state, the way a run that reached the
// ledger would have.
func seedConnectorEffect(t *testing.T, h *connectorToolHarness, decisionSeq int64, releases int, state store.ExternalEffectState, lease time.Time) {
	t.Helper()
	task := connectorToolFixtures()
	// Every seeded effect belongs to the one approval-required write tool.
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
	// The controller stopped after the durable claim: the retry reserves
	// the record (idempotently), moves it to Failed, which fences out any
	// request still between its claim and its call, hands the claim back
	// and proceeds under the next claim (which the stub then refuses).
	claimSpent(0)
	if status, body := postConnectorTool(t, h.app, "gh_write", `{"arguments":{"q":"x"},"approvalId":"ap-1"}`); status != http.StatusFailedDependency || !strings.Contains(body, "no connection") {
		t.Fatalf("unstarted claim = %d %s", status, body)
	}
	if fenced, err := h.effects.GetExternalEffect(context.Background(), mustEffectID(t, decisionSeq, 0)); err != nil || fenced.State != store.ExternalEffectFailed {
		t.Fatalf("fenced record = %+v err = %v, want Failed so the original request cannot start", fenced, err)
	}
	// The same holds when the stopped request had already reserved its
	// record (still Pending).
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
	// The controller stopped during the call: the lease expired with the
	// outcome unknown. The approval is spent and the worker is told so.
	claimSpent(4)
	// The lease must be in the future when taken; it lapses before the retry.
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
	// A late release from an older decision must never advance a newer
	// decision's claim counter, or the newer decision could execute twice.
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

// connectorTestPolicySpec is the connection-mode policy the fixtures use.
func connectorTestPolicySpec() corev1alpha1.OutboundAccessPolicySpec {
	return corev1alpha1.OutboundAccessPolicySpec{Connection: &corev1alpha1.ConnectionOutboundAccess{ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}}}
}

func TestExecuteConnectorToolApprovalBindsPolicyConfiguration(t *testing.T) {
	resolver := &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app, eventStore, _ := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readWrite", approvalRequired: []string{"gh_write"}})
	// The approval was requested while the policy injected the credential
	// differently (another output header); it does not authorize a call
	// under the policy as it is now, even though the Tool is unchanged.
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

// A binding without a grant sequence never came from the controller's freeze
// (a linked Connection always carries one), so nothing executes under it.
func TestExecuteConnectorToolRefusesBindingWithoutGrant(t *testing.T) {
	resolver := &stubOutboundResolver{}
	app, _, _ := newConnectorToolAppWithOptions(t, resolver, true, connectorToolAppOptions{mode: "readOnly", noGrant: true})
	if status, body := postConnectorTool(t, app, "gh_search", `{"arguments":{}}`); status != http.StatusFailedDependency || !strings.Contains(body, "carries no grant") {
		t.Fatalf("no grant = %d %s", status, body)
	}
	if resolver.request.PolicyName != "" {
		t.Fatal("resolution must not be attempted for a binding without a grant")
	}
	// A dispatched binding resolves with the checked policy pinned, so the
	// resolver refuses a policy object replaced between check and execute.
	resolver = &stubOutboundResolver{err: errors.New("the requester has no connection to this provider")}
	app = newConnectorToolApp(t, resolver, true)
	if status, _ := postConnectorTool(t, app, "gh_search", `{"arguments":{}}`); status != http.StatusFailedDependency {
		t.Fatalf("resolver refusal = %d", status)
	}
	if resolver.request.CheckedPolicy == nil || resolver.request.PolicyName != "github-conn" {
		t.Fatalf("resolve request = %+v, want the checked policy pinned", resolver.request)
	}
}

// A Tool CR carries no upper bound on its timeout; the connector call and
// its effect lease are clamped to the provider maximum.
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

// TestConnectorBindingDigestSeparatesGrants covers a re-link of the same
// Connection object: each consent is a distinct grant, so the audit digest
// recorded with an approved call differs, as the ACP broker's does.
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

// revokingResolver resolves a usable linked-account credential but, while it
// does (as a slow refresh would), the worker Pod loses its authority.
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

// TestExecuteConnectorToolRechecksAuthorityAtTheSendBoundary covers a Task
// whose worker loses its authority while the call resolves its credential:
// the request never leaves, the approved write is reported refused rather
// than attempted, its effect settles Failed, and the approval is handed back.
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

// TestSettlePendingConnectorEffectLeavesInFlightRecords covers settling a
// claim's reserved record when its call never started: only a record still
// Pending moves; one another execution holds in flight is left alone and
// reported, so its approval is never reopened while it may reach the provider.
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

// TestExecuteConnectorToolRefusesARecreatedTool covers a Tool deleted and
// recreated with the same spec after dispatch: the running Job's dispatch
// identity names the old object, so the new one is refused.
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
