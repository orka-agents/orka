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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
)

// acceptedBuiltinProvider is a ConnectorProvider the controller accepted
// that declares the named catalog built-ins.
func acceptedBuiltinProvider(name string, builtins ...string) *corev1alpha1.ConnectorProvider {
	provider := testConnectorProvider("tenant", name)
	provider.Spec.Tools = nil
	for _, builtin := range builtins {
		class, _ := connectors.BuiltinConnectorToolClass(builtin)
		provider.Spec.Tools = append(provider.Spec.Tools, corev1alpha1.ConnectorTool{Name: builtin, Class: class, Source: corev1alpha1.ConnectorToolSourceBuiltin})
	}
	provider.Status.Conditions = []metav1.Condition{
		{Type: corev1alpha1.ConnectorProviderConditionAccepted, Status: metav1.ConditionTrue, Reason: "Accepted", ObservedGeneration: provider.Generation},
		{Type: corev1alpha1.ConnectorProviderConditionResolvedRefs, Status: metav1.ConditionTrue, Reason: "Resolved", ObservedGeneration: provider.Generation},
	}
	return provider
}

// brokeredGitHubRegistry is the broker registry with the catalog
// built-ins registered, as the controller builds it with connectors enabled.
func brokeredGitHubRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	registry := tools.NewRegistry()
	if err := tools.RegisterBrokeredGitHubTools(registry, ctrlfake.NewClientBuilder().Build()); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestBuiltinConnectorToolCatalogMatchesBrokeredEffects(t *testing.T) {
	// A write-class built-in that the broker judged read-only would run
	// without the approval its class promises.
	registry := brokeredGitHubRegistry(t)
	for _, name := range connectors.BuiltinConnectorToolNames() {
		class, _ := connectors.BuiltinConnectorToolClass(name)
		if _, registered := registry.Get(name); !registered {
			t.Fatalf("catalog built-in %q is not a registered tool", name)
		}
		effect := brokeredToolEffect(name)
		if class == corev1alpha1.ConnectorToolClassWrite && effect != harnessv2.MCPToolEffectConsequential {
			t.Fatalf("write built-in %q must be consequential", name)
		}
		// A read tool judged consequential would be ledgered and replay-
		// blocked like a mutation for what is only a fetch.
		if class == corev1alpha1.ConnectorToolClassRead && effect != harnessv2.MCPToolEffectReadOnly {
			t.Fatalf("read built-in %q must be read-only", name)
		}
	}
}

func TestClassifyConnectorToolsBuiltins(t *testing.T) {
	f := newConnectorToolFixture(t)
	ctx := context.Background()
	names := []string{"list_pull_requests", "create_pull_request", "web_search", "gh_read"}
	github := acceptedBuiltinProvider("github", "list_pull_requests", "create_pull_request")
	registry := brokeredGitHubRegistry(t)

	infos, err := classifyConnectorTools(ctx, f.reader(github), registry, "tenant", names, connectorScope{builtins: true})
	if err != nil {
		t.Fatal(err)
	}
	read, write := infos["list_pull_requests"], infos["create_pull_request"]
	if !read.Builtin || read.Provider != "github" || read.PolicyName != outboundaccess.BuiltinConnectionKey("list_pull_requests") || read.Class != corev1alpha1.AgentRuntimeBrokeredToolClassRead {
		t.Fatalf("read = %+v", read)
	}
	if !write.Builtin || write.Class != corev1alpha1.AgentRuntimeBrokeredToolClassWrite {
		t.Fatalf("write = %+v", write)
	}
	if _, ok := infos["web_search"]; ok {
		t.Fatal("a built-in outside the catalog is never connector-backed")
	}
	if info, ok := infos["gh_read"]; !ok || info.Builtin {
		t.Fatalf("custom Tool classification must be unchanged: %+v %t", info, ok)
	}
	// Outside the built-in scope (native workers) built-ins are not classified.
	if infos, err := classifyConnectorTools(ctx, f.reader(github), registry, "tenant", names, connectorScope{}); err != nil || infos["list_pull_requests"].Provider != "" {
		t.Fatalf("native scope: %+v err = %v", infos, err)
	}
	// A catalog name the broker registry does not carry is not a built-in
	// here and falls through to the custom Tool path.
	if infos, err := classifyConnectorTools(ctx, f.reader(github), tools.NewRegistry(), "tenant", names, connectorScope{builtins: true}); err != nil || infos["list_pull_requests"].Provider != "" {
		t.Fatalf("unregistered: %+v err = %v", infos, err)
	}
	// An unaccepted provider does not declare anything usable.
	pending := acceptedBuiltinProvider("github", "list_pull_requests")
	pending.Status.Conditions = nil
	if infos, err := classifyConnectorTools(ctx, f.reader(pending), registry, "tenant", names, connectorScope{builtins: true}); err != nil || infos["list_pull_requests"].Provider != "" {
		t.Fatalf("pending provider: %+v err = %v", infos, err)
	}
	// Two accepted providers declaring the same built-in is refused.
	other := acceptedBuiltinProvider("github-enterprise", "list_pull_requests")
	if _, err := classifyConnectorTools(ctx, f.reader(github, other), registry, "tenant", names, connectorScope{builtins: true}); !errors.Is(err, ErrBuiltinToolProviderAmbiguous) {
		t.Fatalf("ambiguous err = %v", err)
	}
	failing := ctrlfake.NewClientBuilder().WithScheme(f.scheme).WithObjects(github).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectList, ...ctrlclient.ListOption) error {
			return errors.New("apiserver unavailable")
		},
	}).Build()
	if _, err := classifyConnectorTools(ctx, failing, registry, "tenant", names, connectorScope{builtins: true}); err == nil {
		t.Fatal("a provider list failure must propagate")
	}
}

func TestFreezeAndFilterBuiltinConnectorTools(t *testing.T) {
	f := newConnectorToolFixture(t)
	ctx := context.Background()
	github := acceptedBuiltinProvider("github", "list_pull_requests", "create_pull_request")
	names := []string{"list_pull_requests", "create_pull_request", "get_issue", "web_search"}
	registry := brokeredGitHubRegistry(t)
	configuration := harnessv2.MCPPolicyConfiguration{}
	for _, name := range names {
		configuration.ToolPolicy.Tools = append(configuration.ToolPolicy.Tools, harnessv2.MCPToolDescriptor{Name: name, Source: harnessv2.MCPToolSourceBrokeredBuiltin})
	}

	// No link: every brokered GitHub built-in is hidden, declared by the
	// provider or not; only the web tool stays, and freezing that planned
	// policy binds nothing.
	visible, write, err := FilterBrokeredConnectorToolsForRequester(ctx, f.reader(github), registry, f.task, names)
	if err != nil || strings.Join(visible, ",") != "web_search" || len(write) != 0 {
		t.Fatalf("no link: visible = %v write = %v err = %v", visible, write, err)
	}
	planned := harnessv2.MCPPolicyConfiguration{}
	planned.ToolPolicy.Tools = []harnessv2.MCPToolDescriptor{{Name: "web_search", Source: harnessv2.MCPToolSourceBrokeredBuiltin}}
	frozen, err := freezeRequesterConnections(ctx, f.reader(github), registry, f.task, planned)
	if err != nil || len(frozen) != 0 {
		t.Fatalf("no link: frozen = %+v err = %v", frozen, err)
	}
	// A policy planned while the link existed but frozen after it went away
	// is a binding race: freezing it would offer a tool no call can use, so
	// binding retries and plans again.
	if _, err := freezeRequesterConnections(ctx, f.reader(github), registry, f.task, configuration); !errors.Is(err, ErrLinkedBuiltinChanged) {
		t.Fatalf("link removed after planning: err = %v, want ErrLinkedBuiltinChanged", err)
	}
	// The same for a write built-in planned under a readWrite link that was
	// narrowed to readOnly before the freeze.
	narrowed := f.reader(github, f.connection(corev1alpha1.ConnectionModeReadOnly, true))
	if _, err := freezeRequesterConnections(ctx, narrowed, registry, f.task, configuration); !errors.Is(err, ErrLinkedBuiltinChanged) {
		t.Fatalf("link narrowed after planning: err = %v, want ErrLinkedBuiltinChanged", err)
	}
	anonymous := f.task.DeepCopy()
	anonymous.Spec.RequestedBy = nil
	if visible, _, err := FilterBrokeredConnectorToolsForRequester(ctx, f.reader(github, f.connection(corev1alpha1.ConnectionModeReadWrite, true)), registry, anonymous, names); err != nil || strings.Join(visible, ",") != "web_search" {
		t.Fatalf("anonymous: visible = %v err = %v", visible, err)
	}

	// A readWrite link freezes one binding per declared tool, shows them,
	// and puts the write tool behind approval; the undeclared get_issue
	// stays hidden.
	linked := f.reader(github, f.connection(corev1alpha1.ConnectionModeReadWrite, true))
	frozen, err = freezeRequesterConnections(ctx, linked, registry, f.task, configuration)
	if err != nil || len(frozen) != 2 {
		t.Fatalf("readWrite: frozen = %+v err = %v", frozen, err)
	}
	for _, entry := range frozen {
		if entry.PolicyName != outboundaccess.BuiltinConnectionKey(entry.Tool) || entry.Provider != "github" || entry.UID != "conn-uid" ||
			entry.GrantSequence != 1 || entry.Mode != corev1alpha1.ConnectionModeReadWrite || entry.PolicyUID != "" {
			t.Fatalf("entry = %+v", entry)
		}
	}
	visible, write, err = FilterBrokeredConnectorToolsForRequester(ctx, linked, registry, f.task, names)
	if err != nil || strings.Join(visible, ",") != "list_pull_requests,create_pull_request,web_search" || strings.Join(write, ",") != "create_pull_request" {
		t.Fatalf("readWrite: visible = %v write = %v err = %v", visible, write, err)
	}
	// The frozen entries round-trip into the executor map with the provider.
	body, err := json.Marshal(agentExecutionSnapshotBody{Connections: frozen})
	if err != nil {
		t.Fatal(err)
	}
	var decoded agentExecutionSnapshotBody
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	bindings := frozenConnectionsFromSnapshot(decoded)
	binding := bindings[outboundaccess.BuiltinConnectionKey("create_pull_request")]
	if binding.UID != "conn-uid" || binding.Provider != "github" || binding.GrantSequence != 1 {
		t.Fatalf("bindings = %+v", bindings)
	}
}

func TestNativePathNeverBindsBuiltinConnectorTools(t *testing.T) {
	// The native filter and freeze never touch built-ins: a worker Pod
	// runs them itself on the Task's own credentials.
	f := newConnectorToolFixture(t)
	ctx := context.Background()
	github := acceptedBuiltinProvider("github", "list_pull_requests", "create_pull_request")
	names := []string{"list_pull_requests", "create_pull_request", "get_issue", "web_search"}
	registry := brokeredGitHubRegistry(t)
	linked := f.reader(github, f.connection(corev1alpha1.ConnectionModeReadWrite, true))
	if visible, write, err := FilterConnectorToolsForRequester(ctx, linked, registry, f.task, names); err != nil || len(visible) != len(names) || len(write) != 0 {
		t.Fatalf("native: visible = %v write = %v err = %v", visible, write, err)
	}
	if frozen, err := freezeRequesterConnectionsForTools(ctx, linked, registry, f.task, names, connectorScope{}); err != nil || len(frozen) != 0 {
		t.Fatalf("native freeze: frozen = %+v err = %v", frozen, err)
	}
}

func TestBuiltinConnectorToolsReadOnlyAndAmbiguousProviders(t *testing.T) {
	f := newConnectorToolFixture(t)
	ctx := context.Background()
	github := acceptedBuiltinProvider("github", "list_pull_requests", "create_pull_request")
	names := []string{"list_pull_requests", "create_pull_request", "get_issue", "web_search"}
	registry := brokeredGitHubRegistry(t)
	configuration := harnessv2.MCPPolicyConfiguration{}
	for _, name := range names {
		configuration.ToolPolicy.Tools = append(configuration.ToolPolicy.Tools, harnessv2.MCPToolDescriptor{Name: name, Source: harnessv2.MCPToolSourceBrokeredBuiltin})
	}

	// A readOnly link hides the write built-in.
	readOnly := f.reader(github, f.connection(corev1alpha1.ConnectionModeReadOnly, true))
	visible, write, err := FilterBrokeredConnectorToolsForRequester(ctx, readOnly, registry, f.task, names)
	if err != nil || strings.Join(visible, ",") != "list_pull_requests,web_search" || len(write) != 0 {
		t.Fatalf("readOnly: visible = %v write = %v err = %v", visible, write, err)
	}

	// Two providers declaring the same built-in is a permanent
	// configuration error at freeze and at visibility time.
	ambiguous := f.reader(github, acceptedBuiltinProvider("ghe", "list_pull_requests"), f.connection(corev1alpha1.ConnectionModeReadWrite, true))
	if _, _, err := FilterBrokeredConnectorToolsForRequester(ctx, ambiguous, registry, f.task, names); !errors.Is(err, ErrBuiltinToolProviderAmbiguous) {
		t.Fatalf("ambiguous filter err = %v", err)
	}
	_, err = freezeRequesterConnections(ctx, ambiguous, registry, f.task, configuration)
	var permanent *permanentACPAgentConfigurationError
	if !errors.As(err, &permanent) || !errors.Is(err, ErrBuiltinToolProviderAmbiguous) {
		t.Fatalf("ambiguous freeze err = %v", err)
	}
}

type fakeLinkedSource struct {
	request    outboundaccess.ConnectionCredentialRequest
	credential outboundaccess.ConnectionCredential
	err        error
	calls      int
}

func (f *fakeLinkedSource) ResolveConnectionCredential(_ context.Context, req outboundaccess.ConnectionCredentialRequest) (outboundaccess.ConnectionCredential, error) {
	f.calls++
	f.request = req
	return f.credential, f.err
}

func TestLinkedBuiltinAccounts(t *testing.T) {
	requester := &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}
	frozen := map[string]outboundaccess.FrozenConnection{
		outboundaccess.BuiltinConnectionKey("list_pull_requests"):  {UID: "conn-uid", Generation: 3, GrantSequence: 1, Provider: "github"},
		outboundaccess.BuiltinConnectionKey("create_pull_request"): {UID: "conn-uid", Generation: 3, GrantSequence: 1, Provider: "github"},
		outboundaccess.BuiltinConnectionKey("get_issue"):           {UID: "conn-uid", Generation: 3, GrantSequence: 1},
	}
	source := &fakeLinkedSource{credential: outboundaccess.ConnectionCredential{AccessToken: "gho_linked", ConnectionUID: "conn-uid", Mode: corev1alpha1.ConnectionModeReadWrite}}
	accounts := linkedBuiltinAccounts{source: source, namespace: "tenant", requester: requester, frozen: frozen}
	ctx := context.Background()

	credential, bound, err := accounts.BuiltinToolCredential(ctx, "list_pull_requests")
	if err != nil || !bound || credential.AccessToken != "gho_linked" || credential.Provider != "github" || credential.ConnectionUID != "conn-uid" {
		t.Fatalf("credential = %+v bound = %t err = %v", credential, bound, err)
	}
	req := source.request
	if req.Namespace != "tenant" || req.Provider != "github" || req.Issuer != requester.Issuer || req.Subject != requester.Subject ||
		req.Frozen.UID != "conn-uid" || req.Frozen.GrantSequence != 1 || !req.Tool.Builtin || req.Tool.Name != "list_pull_requests" ||
		req.Tool.Class != corev1alpha1.AgentRuntimeBrokeredToolClassRead || !req.Tool.TimeoutSet || req.Tool.Timeout != connectors.BuiltinToolTimeout {
		t.Fatalf("request = %+v", req)
	}
	// Not in the catalog, or no binding frozen: the tool keeps its own path.
	for _, name := range []string{"web_search", "comment_on_issue"} {
		if _, bound, err := accounts.BuiltinToolCredential(ctx, name); bound || err != nil {
			t.Fatalf("%s: bound = %t err = %v", name, bound, err)
		}
	}
	// The broker requires the binding: an unbound catalog tool is refused
	// there, while a non-catalog tool is still simply not its concern.
	required := accounts
	required.required = true
	if _, _, err := required.BuiltinToolCredential(ctx, "comment_on_issue"); err == nil || !strings.Contains(err.Error(), "requires the requester's linked account") {
		t.Fatalf("required unbound err = %v", err)
	}
	if _, bound, err := required.BuiltinToolCredential(ctx, "web_search"); bound || err != nil {
		t.Fatalf("required non-catalog: bound = %t err = %v", bound, err)
	}
}

func TestLinkedBuiltinAccountsFailClosed(t *testing.T) {
	requester := &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}
	frozen := map[string]outboundaccess.FrozenConnection{
		outboundaccess.BuiltinConnectionKey("list_pull_requests"):  {UID: "conn-uid", Generation: 3, GrantSequence: 1, Provider: "github"},
		outboundaccess.BuiltinConnectionKey("create_pull_request"): {UID: "conn-uid", Generation: 3, GrantSequence: 1, Provider: "github"},
		outboundaccess.BuiltinConnectionKey("get_issue"):           {UID: "conn-uid", Generation: 3, GrantSequence: 1},
	}
	source := &fakeLinkedSource{credential: outboundaccess.ConnectionCredential{AccessToken: "gho_linked", ConnectionUID: "conn-uid", Mode: corev1alpha1.ConnectionModeReadWrite}}
	accounts := linkedBuiltinAccounts{source: source, namespace: "tenant", requester: requester, frozen: frozen}
	ctx := context.Background()

	// A frozen binding that cannot be used fails closed, never falls back.
	if _, _, err := accounts.BuiltinToolCredential(ctx, "get_issue"); err == nil || !strings.Contains(err.Error(), "names no provider") {
		t.Fatalf("no provider err = %v", err)
	}
	source.err = errors.New("connection revoked")
	if _, _, err := accounts.BuiltinToolCredential(ctx, "list_pull_requests"); err == nil || !strings.Contains(err.Error(), "connection revoked") {
		t.Fatalf("source err = %v", err)
	}
	source.err = nil
	source.credential.Mode = corev1alpha1.ConnectionModeReadOnly
	if _, _, err := accounts.BuiltinToolCredential(ctx, "create_pull_request"); err == nil || !strings.Contains(err.Error(), "readOnly") {
		t.Fatalf("readOnly write err = %v", err)
	}
	if _, bound, err := accounts.BuiltinToolCredential(ctx, "list_pull_requests"); err != nil || !bound {
		t.Fatalf("readOnly read: bound = %t err = %v", bound, err)
	}
	source.credential.AccessToken = ""
	if _, _, err := accounts.BuiltinToolCredential(ctx, "list_pull_requests"); err == nil || !strings.Contains(err.Error(), "empty credential") {
		t.Fatalf("empty err = %v", err)
	}
	if _, _, err := (linkedBuiltinAccounts{namespace: "tenant", requester: requester, frozen: frozen}).BuiltinToolCredential(ctx, "list_pull_requests"); err == nil || !strings.Contains(err.Error(), "only in the controller") {
		t.Fatalf("no source err = %v", err)
	}
	if _, _, err := (linkedBuiltinAccounts{source: source, namespace: "tenant", frozen: frozen}).BuiltinToolCredential(ctx, "list_pull_requests"); err == nil || !strings.Contains(err.Error(), "verified requester") {
		t.Fatalf("no requester err = %v", err)
	}
}

// contextCapturingTool records the ToolContext it executes under.
type contextCapturingTool struct {
	name     string
	captured *tools.ToolContext
	deadline time.Time
}

func (t *contextCapturingTool) Name() string        { return t.name }
func (t *contextCapturingTool) Description() string { return t.name }
func (t *contextCapturingTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (t *contextCapturingTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	t.captured = tools.GetToolContext(ctx)
	t.deadline, _ = ctx.Deadline()
	return `{"ok":true}`, nil
}

func TestRegistryACPMCPToolExecutorBindsLinkedAccountsForBuiltins(t *testing.T) {
	f := newConnectorToolFixture(t)
	task := f.task.DeepCopy()
	task.Status.AgentExecutionBinding = &corev1alpha1.AgentExecutionBinding{Snapshot: corev1alpha1.AgentExecutionSnapshotRef{Digest: "abc"}}
	body, err := json.Marshal(agentExecutionSnapshotBody{Connections: []agentExecutionSnapshotConnection{
		{PolicyName: outboundaccess.BuiltinConnectionKey("list_pull_requests"), Tool: "list_pull_requests", Provider: "github", ConnectionName: "github-x", UID: "conn-uid", Generation: 3, GrantSequence: 1, Mode: "readWrite"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	listTool := &contextCapturingTool{name: "list_pull_requests"}
	searchTool := &contextCapturingTool{name: "web_search"}
	registry.Register(listTool)
	registry.Register(searchTool)
	source := &fakeLinkedSource{credential: outboundaccess.ConnectionCredential{AccessToken: "gho_linked", ConnectionUID: "conn-uid", Mode: "readWrite"}}
	executor := RegistryACPMCPToolExecutor{
		Registry: registry, Reader: f.reader(task), Connections: source,
		AgentExecutionSnapshots: fakeSnapshotStore{snapshot: &store.AgentExecutionSnapshot{Body: body}},
		ContextFactory: func(context.Context, harnessv2.MCPBrokerCallRequest) (*tools.ToolContext, error) {
			return &tools.ToolContext{Brokered: true}, nil
		},
	}
	request := harnessv2.MCPBrokerCallRequest{Namespace: "tenant"}
	request.Metadata.TaskUID = "task-uid"
	request.Call.CallID = "call-1"
	ctx := withACPMCPAuthenticatedTask(context.Background(), ACPMCPAuthenticatedTask{Name: "task", Namespace: "tenant", UID: "task-uid"})

	builtin := harnessv2.MCPToolDescriptor{Name: "list_pull_requests", Source: harnessv2.MCPToolSourceBrokeredBuiltin}
	if _, err := executor.ExecuteACPMCPTool(ctx, request, builtin); err != nil {
		t.Fatal(err)
	}
	// The call is held to the catalog bound the credential was refreshed for.
	if listTool.deadline.IsZero() || time.Until(listTool.deadline) > connectors.BuiltinToolTimeout {
		t.Fatalf("linked built-in deadline = %v, want within the catalog bound", listTool.deadline)
	}
	if listTool.captured == nil || listTool.captured.LinkedAccounts == nil {
		t.Fatal("a catalog built-in must execute with the linked-account binding")
	}
	credential, bound, err := listTool.captured.LinkedAccounts.BuiltinToolCredential(ctx, "list_pull_requests")
	if err != nil || !bound || credential.AccessToken != "gho_linked" || source.request.Subject != "alice" || source.request.Provider != "github" {
		t.Fatalf("credential = %+v bound = %t err = %v request = %+v", credential, bound, err, source.request)
	}
	// The effect record names the frozen binding.
	digest, err := executor.ConnectionDigest(ctx, request, builtin)
	if err != nil || digest != frozenConnectionDigest(map[string]outboundaccess.FrozenConnection{outboundaccess.BuiltinConnectionKey("list_pull_requests"): {UID: "conn-uid", Generation: 3, GrantSequence: 1, Provider: "github"}}, outboundaccess.BuiltinConnectionKey("list_pull_requests")) {
		t.Fatalf("digest = %q err = %v", digest, err)
	}
	// A built-in outside the catalog gets no binding and no digest.
	if _, err := executor.ExecuteACPMCPTool(ctx, request, harnessv2.MCPToolDescriptor{Name: "web_search", Source: harnessv2.MCPToolSourceBrokeredBuiltin}); err != nil {
		t.Fatal(err)
	}
	if searchTool.captured == nil || searchTool.captured.LinkedAccounts != nil {
		t.Fatal("a non-catalog built-in must not carry a linked-account binding")
	}
	if digest, err := executor.ConnectionDigest(ctx, request, harnessv2.MCPToolDescriptor{Name: "web_search", Source: harnessv2.MCPToolSourceBrokeredBuiltin}); err != nil || digest != "" {
		t.Fatalf("web_search digest = %q err = %v", digest, err)
	}
	// A catalog built-in with nothing frozen can never run, so naming its
	// binding fails before any approval or effect record is made, and its
	// binding refuses the call rather than letting it run on anything else.
	if digest, err := executor.ConnectionDigest(ctx, request, harnessv2.MCPToolDescriptor{Name: "get_issue", Source: harnessv2.MCPToolSourceBrokeredBuiltin}); err == nil || digest != "" {
		t.Fatalf("unfrozen digest = %q err = %v, want refusal", digest, err)
	}
	issueTool := &contextCapturingTool{name: "get_issue"}
	registry.Register(issueTool)
	if _, err := executor.ExecuteACPMCPTool(ctx, request, harnessv2.MCPToolDescriptor{Name: "get_issue", Source: harnessv2.MCPToolSourceBrokeredBuiltin}); err != nil {
		t.Fatal(err)
	}
	if issueTool.captured == nil || issueTool.captured.LinkedAccounts == nil {
		t.Fatal("an unfrozen catalog built-in still carries the binding")
	}
	if _, _, err := issueTool.captured.LinkedAccounts.BuiltinToolCredential(ctx, "get_issue"); err == nil {
		t.Fatal("an unfrozen catalog built-in must be refused by its binding")
	}
	// A snapshot that cannot be loaded is a preparation failure, never a
	// silent run without the binding.
	broken := executor
	broken.AgentExecutionSnapshots = fakeSnapshotStore{err: errors.New("db down")}
	if _, err := broken.ExecuteACPMCPTool(ctx, request, builtin); err == nil {
		t.Fatal("an unloadable snapshot must fail the built-in call")
	}
	if _, err := executor.ExecuteACPMCPTool(context.Background(), request, builtin); err == nil {
		t.Fatal("an unauthenticated context must fail the built-in call")
	}
	requireCatalogBuiltinsNeedToolContext(ctx, t, executor, request, builtin)
}

// requireCatalogBuiltinsNeedToolContext checks a broker with no context
// factory, or one that yields nothing: the catalog built-in is refused
// rather than run on the tool's own credential path, while a non-catalog
// built-in still runs.
func requireCatalogBuiltinsNeedToolContext(ctx context.Context, t *testing.T, executor RegistryACPMCPToolExecutor, request harnessv2.MCPBrokerCallRequest, builtin harnessv2.MCPToolDescriptor) {
	t.Helper()
	for name, factory := range map[string]func(context.Context, harnessv2.MCPBrokerCallRequest) (*tools.ToolContext, error){
		"absent": nil,
		"empty":  func(context.Context, harnessv2.MCPBrokerCallRequest) (*tools.ToolContext, error) { return nil, nil },
	} {
		bare := executor
		bare.ContextFactory = factory
		if _, err := bare.ExecuteACPMCPTool(ctx, request, builtin); err == nil || !strings.Contains(err.Error(), "no authenticated task context") {
			t.Fatalf("%s factory: err = %v, want the built-in refused", name, err)
		}
		if _, err := bare.ExecuteACPMCPTool(ctx, request, harnessv2.MCPToolDescriptor{Name: "web_search", Source: harnessv2.MCPToolSourceBrokeredBuiltin}); err != nil {
			t.Fatalf("%s factory: non-catalog built-in must still run: %v", name, err)
		}
	}
}

func TestBrokeredLinkedBuiltins(t *testing.T) {
	registry := brokeredGitHubRegistry(t)
	names := []string{"web_search", "list_pull_requests", "gh_read", "create_pull_request"}
	if got := strings.Join(brokeredLinkedBuiltins(registry, names), ","); got != "list_pull_requests,create_pull_request" {
		t.Fatalf("linked = %q", got)
	}
	if got := brokeredLinkedBuiltins(tools.NewRegistry(), names); len(got) != 0 {
		t.Fatalf("an unregistered catalog name is not brokered: %v", got)
	}
	// Without a broker registry (the ACP broker is not configured, as for a
	// harness-v1-only controller) nothing runs under a linked account, so
	// GitHub tools the native registry carries keep their Task credentials
	// and v1 Tasks that allow them are not refused.
	if got := brokeredLinkedBuiltins(nil, names); len(got) != 0 {
		t.Fatalf("a nil broker registry brokers nothing: %v", got)
	}
}

// TestClassifyConnectorToolsWithoutBrokerRegistry covers a controller with
// no ACP broker: catalog names are never classified as linked built-ins,
// however the native registry is populated.
func TestClassifyConnectorToolsWithoutBrokerRegistry(t *testing.T) {
	f := newConnectorToolFixture(t)
	github := acceptedBuiltinProvider("github", "list_pull_requests", "create_pull_request")
	infos, err := classifyConnectorTools(context.Background(), f.reader(github), nil, "tenant",
		[]string{"list_pull_requests", "create_pull_request"}, connectorScope{builtins: true})
	if err != nil {
		t.Fatal(err)
	}
	for name, info := range infos {
		if info.Builtin {
			t.Fatalf("%s classified as a linked built-in without a broker registry: %+v", name, info)
		}
	}
	visible, write, err := FilterBrokeredConnectorToolsForRequester(context.Background(), f.reader(github), nil, f.task,
		[]string{"list_pull_requests", "create_pull_request"})
	if err != nil || len(visible) != 2 || len(write) != 0 {
		t.Fatalf("visible = %v write = %v err = %v, want both kept on their own credentials", visible, write, err)
	}
}

func TestLinkedRepositoryScopeInherited(t *testing.T) {
	f := newConnectorToolFixture(t)
	ctx := context.Background()
	workspace := func(repos ...string) *corev1alpha1.WorkspaceConfig {
		ws := &corev1alpha1.WorkspaceConfig{}
		if len(repos) > 0 {
			ws.GitRepo = repos[0]
		}
		if len(repos) > 1 {
			ws.PublicationGitRepo = repos[1]
		}
		return ws
	}
	owned := func(name, uid string, parent *corev1alpha1.Task, ws *corev1alpha1.WorkspaceConfig) *corev1alpha1.Task {
		return &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant", UID: types.UID(uid), OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: taskResourceKind, Name: parent.Name, UID: parent.UID, Controller: new(true),
			}}},
			Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Workspace: ws},
		}
	}
	root := f.task.DeepCopy()
	root.Spec.Workspace = workspace("https://github.com/acme/app", "git@github.com:acme/app-fork.git")
	within := owned("within", "within-uid", root, workspace("https://github.com/Acme/App.git"))
	publication := owned("publication", "publication-uid", root, workspace("https://github.com/acme/app", "https://github.com/acme/app-fork"))
	beyond := owned("beyond", "beyond-uid", root, workspace("https://github.com/acme/other"))
	grandchild := owned("grandchild", "grandchild-uid", within, workspace("https://github.com/acme/app"))
	deepBeyond := owned("deep-beyond", "deep-beyond-uid", within, workspace("https://github.com/acme/app", "https://github.com/acme/elsewhere"))
	reader := f.reader(root, within, publication, beyond, grandchild, deepBeyond)

	if err := linkedRepositoryScopeInherited(ctx, reader, root); err != nil {
		t.Fatalf("a root task is its own scope: %v", err)
	}
	for _, task := range []*corev1alpha1.Task{within, publication, grandchild} {
		if err := linkedRepositoryScopeInherited(ctx, reader, task); err != nil {
			t.Fatalf("%s: %v", task.Name, err)
		}
	}
	for _, task := range []*corev1alpha1.Task{beyond, deepBeyond} {
		if err := linkedRepositoryScopeInherited(ctx, reader, task); !errors.Is(err, ErrLinkedRepositoryScope) {
			t.Fatalf("%s: err = %v, want the scope refused", task.Name, err)
		}
	}
	// A replaced parent (same name, other UID) is never trusted as the scope.
	replaced := root.DeepCopy()
	replaced.UID = "other-root-uid"
	if err := linkedRepositoryScopeInherited(ctx, f.reader(replaced, within), within); !errors.Is(err, ErrLinkedRepositoryScope) {
		t.Fatalf("replaced parent err = %v", err)
	}
	// A parent that cannot be read is a retryable error, not a verdict.
	if err := linkedRepositoryScopeInherited(ctx, f.reader(within), within); err == nil || errors.Is(err, ErrLinkedRepositoryScope) {
		t.Fatalf("missing parent err = %v", err)
	}

	// The brokered filter and freeze refuse such a child permanently, and
	// leave a child within scope alone.
	github := acceptedBuiltinProvider("github", "list_pull_requests")
	registry := brokeredGitHubRegistry(t)
	names := []string{"list_pull_requests", "web_search"}
	linkedReader := f.reader(root, within, beyond, github, f.connection(corev1alpha1.ConnectionModeReadWrite, true))
	stamp := func(task *corev1alpha1.Task) *corev1alpha1.Task {
		task = task.DeepCopy()
		task.Spec.RequestedBy = f.requester
		task.Annotations = map[string]string{
			labels.AnnotationRequestedBySource: labels.RequestedBySourceAPI,
			labels.AnnotationRequestedByStamp:  connectors.RequesterStamp(testRequesterStampKey, task.UID, f.requester.Issuer, f.requester.Subject),
		}
		return task
	}
	if _, _, err := FilterBrokeredConnectorToolsForRequester(ctx, linkedReader, registry, stamp(beyond), names); !errors.Is(err, ErrLinkedRepositoryScope) {
		t.Fatalf("beyond filter err = %v", err)
	}
	configuration := brokeredConfiguration()
	configuration.ToolPolicy.Tools = []harnessv2.MCPToolDescriptor{{Name: "list_pull_requests", Source: harnessv2.MCPToolSourceBrokeredBuiltin}}
	var permanent *permanentACPAgentConfigurationError
	if _, err := freezeRequesterConnections(ctx, linkedReader, registry, stamp(beyond), configuration); !errors.As(err, &permanent) || !errors.Is(err, ErrLinkedRepositoryScope) {
		t.Fatalf("beyond freeze err = %v", err)
	}
	if visible, _, err := FilterBrokeredConnectorToolsForRequester(ctx, linkedReader, registry, stamp(within), names); err != nil || strings.Join(visible, ",") != "list_pull_requests,web_search" {
		t.Fatalf("within filter: visible = %v err = %v", visible, err)
	}
	if frozen, err := freezeRequesterConnections(ctx, linkedReader, registry, stamp(within), configuration); err != nil || len(frozen) != 1 {
		t.Fatalf("within freeze: frozen = %+v err = %v", frozen, err)
	}
	// Without a linked built-in in play the chain is never walked.
	if _, _, err := FilterBrokeredConnectorToolsForRequester(ctx, f.reader(beyond), registry, stamp(beyond), []string{"web_search"}); err != nil {
		t.Fatalf("no built-in: %v", err)
	}
}
