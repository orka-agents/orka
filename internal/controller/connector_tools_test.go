/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/store"
	workerexecutor "github.com/orka-agents/orka/internal/worker"
)

type connectorToolFixture struct {
	scheme     *runtime.Scheme
	policy     *corev1alpha1.OutboundAccessPolicy
	readTool   *corev1alpha1.Tool
	writeTool  *corev1alpha1.Tool
	plainTool  *corev1alpha1.Tool
	directTool *corev1alpha1.Tool
	direct     *corev1alpha1.OutboundAccessPolicy
	requester  *corev1alpha1.RequestedBy
	task       *corev1alpha1.Task
}

func newConnectorToolFixture(t *testing.T) connectorToolFixture {
	t.Helper()
	tool := func(name string, class corev1alpha1.AgentRuntimeBrokeredToolClass, policy string) *corev1alpha1.Tool {
		spec := corev1alpha1.ToolSpec{
			Description: name, BrokeredToolClass: class,
			Parameters: &apiextensionsv1.JSON{Raw: json.RawMessage(`{"type":"object","properties":{}}`)},
			HTTP:       &corev1alpha1.HTTPExecution{URL: "https://api.github.example.test/" + name},
		}
		if policy != "" {
			spec.HTTP.OutboundAccessPolicyRef = &corev1alpha1.LocalObjectReference{Name: policy}
		}
		return &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant"}, Spec: spec}
	}
	requester := &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}
	return connectorToolFixture{
		scheme: connectorTestScheme(t),
		policy: &corev1alpha1.OutboundAccessPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "github-conn", Namespace: "tenant"},
			Spec:       corev1alpha1.OutboundAccessPolicySpec{Connection: &corev1alpha1.ConnectionOutboundAccess{ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}}},
		},
		direct: &corev1alpha1.OutboundAccessPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "direct", Namespace: "tenant"},
			Spec:       corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{}},
		},
		readTool:   tool("gh_read", corev1alpha1.AgentRuntimeBrokeredToolClassRead, "github-conn"),
		writeTool:  tool("gh_write", corev1alpha1.AgentRuntimeBrokeredToolClassWrite, "github-conn"),
		plainTool:  tool("plain", corev1alpha1.AgentRuntimeBrokeredToolClassWrite, ""),
		directTool: tool("direct_write", corev1alpha1.AgentRuntimeBrokeredToolClassWrite, "direct"),
		requester:  requester,
		task: &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name: "task", Namespace: "tenant", UID: "task-uid",
				Annotations: map[string]string{labels.AnnotationRequestedBySource: labels.RequestedBySourceAPI},
			},
			Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, RequestedBy: requester},
		},
	}
}

func (f connectorToolFixture) connection(mode string, ready bool) *corev1alpha1.Connection {
	connection := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: connectors.ConnectionName("github", f.requester.Issuer, f.requester.Subject), Namespace: "tenant", UID: "conn-uid", Generation: 3},
		Spec: corev1alpha1.ConnectionSpec{
			Subject: corev1alpha1.ConnectionSubject{Issuer: f.requester.Issuer, Subject: f.requester.Subject}, ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}, Mode: mode,
		},
	}
	if ready {
		connection.Status.Conditions = []metav1.Condition{
			{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonLinked, ObservedGeneration: 3},
			{Type: corev1alpha1.ConnectionConditionScopesGranted, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonScopesGranted, ObservedGeneration: 3},
			{Type: corev1alpha1.ConnectionConditionProviderResolved, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonProviderResolved, ObservedGeneration: 3},
		}
	}
	return connection
}

func (f connectorToolFixture) reader(extra ...ctrlclient.Object) ctrlclient.Client {
	objects := append([]ctrlclient.Object{f.policy, f.direct, f.readTool, f.writeTool, f.plainTool, f.directTool}, extra...)
	return ctrlfake.NewClientBuilder().WithScheme(f.scheme).WithObjects(objects...).Build()
}

func TestFilterConnectorToolsForRequester(t *testing.T) {
	f := newConnectorToolFixture(t)
	names := []string{"gh_read", "gh_write", "plain", "direct_write", "missing"}
	ctx := context.Background()

	visible, write, err := FilterConnectorToolsForRequester(ctx, f.reader(), f.task, names)
	if err != nil || strings.Join(visible, ",") != strings.Join(names, ",") || strings.Join(write, ",") != "gh_write" {
		t.Fatalf("no link: visible = %v write = %v err = %v", visible, write, err)
	}
	visible, write, err = FilterConnectorToolsForRequester(ctx, f.reader(f.connection(corev1alpha1.ConnectionModeReadWrite, true)), f.task, names)
	if err != nil || strings.Join(visible, ",") != strings.Join(names, ",") || strings.Join(write, ",") != "gh_write" {
		t.Fatalf("readWrite: visible = %v write = %v err = %v", visible, write, err)
	}
	visible, write, err = FilterConnectorToolsForRequester(ctx, f.reader(f.connection(corev1alpha1.ConnectionModeReadOnly, true)), f.task, names)
	if err != nil || strings.Join(visible, ",") != "gh_read,plain,direct_write,missing" || len(write) != 0 {
		t.Fatalf("readOnly: visible = %v write = %v err = %v", visible, write, err)
	}
	visible, _, err = FilterConnectorToolsForRequester(ctx, f.reader(f.connection(corev1alpha1.ConnectionModeReadOnly, false)), f.task, names)
	if err != nil || strings.Join(visible, ",") != strings.Join(names, ",") {
		t.Fatalf("unready readOnly link must not hide (call fails closed instead): %v err = %v", visible, err)
	}
	anonymous := f.task.DeepCopy()
	anonymous.Spec.RequestedBy = nil
	visible, _, err = FilterConnectorToolsForRequester(ctx, f.reader(f.connection(corev1alpha1.ConnectionModeReadOnly, true)), anonymous, names)
	if err != nil || strings.Join(visible, ",") != strings.Join(names, ",") {
		t.Fatalf("anonymous: visible = %v err = %v", visible, err)
	}
	if visible, _, err := FilterConnectorToolsForRequester(ctx, nil, f.task, names); err != nil || len(visible) != len(names) {
		t.Fatalf("nil reader: %v %v", visible, err)
	}
	failing := ctrlfake.NewClientBuilder().WithScheme(f.scheme).WithObjects(f.policy, f.readTool).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
			if _, isPolicy := obj.(*corev1alpha1.OutboundAccessPolicy); isPolicy {
				return errors.New("apiserver unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	if _, _, err := FilterConnectorToolsForRequester(ctx, failing, f.task, []string{"gh_read"}); err == nil {
		t.Fatal("read failure must propagate")
	}
}

func TestFreezeAndBindNativeConnections(t *testing.T) {
	f := newConnectorToolFixture(t)
	ctx := context.Background()
	frozen, err := freezeRequesterConnectionsForTools(ctx, f.reader(f.connection(corev1alpha1.ConnectionModeReadWrite, true)), f.task, []string{"gh_read", "gh_write", "plain", "direct_write"})
	if err != nil || len(frozen) != 1 || frozen[0].PolicyName != "github-conn" || frozen[0].UID != "conn-uid" || frozen[0].Generation != 3 {
		t.Fatalf("frozen = %+v err = %v", frozen, err)
	}
	bindings := taskConnectionBindings(frozen)
	if len(bindings) != 1 || bindings[0].ConnectionName == "" || bindings[0].Mode != corev1alpha1.ConnectionModeReadWrite {
		t.Fatalf("bindings = %+v", bindings)
	}
	if taskConnectionBindings(nil) != nil {
		t.Fatal("empty freeze must yield nil bindings")
	}
	task := f.task.DeepCopy()
	task.Status.ConnectionBindings = bindings
	executor := workerexecutor.NewToolExecutorForNamespace("tenant", nil, nil)
	if err := BindNativeTaskConnectorAuthority(ctx, f.reader(), task, nil, false, executor); err != nil {
		t.Fatal(err)
	}
	if executor.Requester() == nil || executor.Requester().Subject != "alice" {
		t.Fatalf("requester = %+v", executor.Requester())
	}
	if got := executor.FrozenConnections()["github-conn"]; got.UID != "conn-uid" || got.Generation != 3 {
		t.Fatalf("frozen map = %+v", executor.FrozenConnections())
	}
	if FrozenConnectionsFromTaskStatus(nil) != nil || FrozenConnectionsFromTaskStatus(f.task) != nil {
		t.Fatal("tasks without bindings must yield nil")
	}
	if err := BindNativeTaskConnectorAuthority(ctx, f.reader(), nil, nil, false, executor); err == nil {
		t.Fatal("nil task must fail")
	}
	digest := frozenConnectionDigest(executor.FrozenConnections(), "github-conn")
	if digest == "" || digest != frozenConnectionDigest(map[string]outboundaccess.FrozenConnection{"github-conn": {UID: "conn-uid", Generation: 3}}, "github-conn") {
		t.Fatalf("digest = %q", digest)
	}
	if frozenConnectionDigest(nil, "github-conn") != "" || frozenConnectionDigest(executor.FrozenConnections(), "other") != "" {
		t.Fatal("missing bindings must yield an empty digest")
	}
}

func TestRegistryACPMCPToolExecutorConnectionDigest(t *testing.T) {
	f := newConnectorToolFixture(t)
	task := f.task.DeepCopy()
	task.Status.AgentExecutionBinding = &corev1alpha1.AgentExecutionBinding{Snapshot: corev1alpha1.AgentExecutionSnapshotRef{Digest: "abc"}}
	body, err := json.Marshal(agentExecutionSnapshotBody{Connections: []agentExecutionSnapshotConnection{
		{PolicyName: "github-conn", Provider: "github", ConnectionName: "github-x", UID: "conn-uid", Generation: 3, Mode: "readWrite"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	reader := f.reader(task)
	executor := RegistryACPMCPToolExecutor{Reader: reader, AgentExecutionSnapshots: fakeSnapshotStore{snapshot: &store.AgentExecutionSnapshot{Body: body}}}
	request := harnessv2.MCPBrokerCallRequest{Namespace: "tenant"}
	request.Metadata.TaskUID = "task-uid"
	ctx := withACPMCPAuthenticatedTask(context.Background(), ACPMCPAuthenticatedTask{Name: "task", Namespace: "tenant", UID: "task-uid"})
	custom := harnessv2.MCPToolDescriptor{Name: "gh_write", Source: harnessv2.MCPToolSourceBrokeredCustom}
	digest := executor.ConnectionDigest(ctx, request, custom)
	if digest == "" || digest != frozenConnectionDigest(map[string]outboundaccess.FrozenConnection{"github-conn": {UID: "conn-uid", Generation: 3}}, "github-conn") {
		t.Fatalf("digest = %q", digest)
	}
	if executor.ConnectionDigest(ctx, request, harnessv2.MCPToolDescriptor{Name: "plain", Source: harnessv2.MCPToolSourceBrokeredCustom}) != "" {
		t.Fatal("non-connector tools have no digest")
	}
	if executor.ConnectionDigest(ctx, request, harnessv2.MCPToolDescriptor{Name: "web_search", Source: harnessv2.MCPToolSourceBrokeredBuiltin}) != "" {
		t.Fatal("built-in tools have no digest")
	}
	if executor.ConnectionDigest(context.Background(), request, custom) != "" {
		t.Fatal("an unauthenticated context yields no digest")
	}
	if (RegistryACPMCPToolExecutor{Reader: reader}).ConnectionDigest(ctx, request, custom) != "" {
		t.Fatal("no snapshot store yields no digest")
	}
}

func TestBuildRuntimeSessionMCPConfigurationConnectorWriteTools(t *testing.T) {
	f := newConnectorToolFixture(t)
	allowBash := false
	task := f.task.DeepCopy()
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"gh_read", "gh_write"}, AllowBash: &allowBash}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "tenant", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model:   &corev1alpha1.ModelConfig{Name: "model"},
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2)},
		},
	}
	plan, err := PlanACPRuntime(task, agent, ACPRuntimeImages{Claude: "docker.io/example/claude@sha256:" + strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	descriptorNames := func(cfg harnessv2.MCPPolicyConfiguration) string {
		names := make([]string, 0, len(cfg.ToolPolicy.Tools))
		for _, descriptor := range cfg.ToolPolicy.Tools {
			names = append(names, descriptor.Name)
		}
		return strings.Join(names, ",")
	}

	build := func(connection *corev1alpha1.Connection, runtimeType corev1alpha1.AgentRuntimeType) harnessv2.MCPPolicyConfiguration {
		t.Helper()
		var extra []ctrlclient.Object
		if connection != nil {
			extra = append(extra, connection)
		}
		reader := f.reader(extra...)
		planAgent := agent.DeepCopy()
		planAgent.Spec.Runtime.Type = runtimeType
		adjustedTask, adjustedAgent, err := adjustInputsForConnectorTools(context.Background(), reader, task, planAgent)
		if err != nil {
			t.Fatal(err)
		}
		adjustedPlan, err := PlanACPRuntime(adjustedTask, adjustedAgent, ACPRuntimeImages{Claude: "docker.io/example/claude@sha256:" + strings.Repeat("a", 64)})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := buildRuntimeSessionMCPConfiguration(context.Background(), reader, adjustedTask, adjustedAgent, adjustedPlan.Profile)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.ValidateProfile(adjustedPlan.Profile); err != nil {
			t.Fatalf("adjusted policy must match the adjusted plan: %v", err)
		}
		return cfg
	}
	_ = plan

	// A readWrite link on a runtime without approval support hides the write
	// tool rather than exposing it unapproved or failing the session.
	cfg := build(f.connection(corev1alpha1.ConnectionModeReadWrite, true), corev1alpha1.AgentRuntimeClaude)
	if descriptorNames(cfg) != "gh_read" || len(cfg.ApprovalPolicy.RequiredTools) != 0 {
		t.Fatalf("claude runtime: tools = %q approval = %v", descriptorNames(cfg), cfg.ApprovalPolicy.RequiredTools)
	}
	// A readOnly link hides it as well.
	if cfg := build(f.connection(corev1alpha1.ConnectionModeReadOnly, true), corev1alpha1.AgentRuntimeClaude); descriptorNames(cfg) != "gh_read" {
		t.Fatalf("readOnly: tools = %q", descriptorNames(cfg))
	}
	// Without any link the tools stay listed and fail closed at call time.
	if cfg := build(nil, corev1alpha1.AgentRuntimeClaude); descriptorNames(cfg) != "gh_read" {
		t.Fatalf("no link on claude: tools = %q (write hidden for lack of approvals)", descriptorNames(cfg))
	}

	// The adjustment leaves unrelated inputs untouched and never mutates the
	// caller's objects.
	adjustedTask, adjustedAgent, err := adjustInputsForConnectorTools(context.Background(), f.reader(f.connection(corev1alpha1.ConnectionModeReadWrite, true)), task, agent)
	if err != nil {
		t.Fatal(err)
	}
	if adjustedTask == task || strings.Join(adjustedTask.Spec.AgentRuntime.AllowedTools, ",") != "gh_read" {
		t.Fatalf("adjusted task allowed = %v", adjustedTask.Spec.AgentRuntime.AllowedTools)
	}
	if strings.Join(task.Spec.AgentRuntime.AllowedTools, ",") != "gh_read,gh_write" || agent.Spec.Coordination != nil {
		t.Fatal("inputs must not be mutated")
	}
	if adjustedAgent != agent {
		t.Fatal("no approval change means the agent is returned as-is")
	}
}
