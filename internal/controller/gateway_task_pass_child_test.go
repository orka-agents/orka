package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type childPassRequest struct {
	ProtocolVersion, GrantRef, NamespaceUID, GatewayUID, TaskUID, ParentTaskUID string
	Tools                                                                       []string
	Deadline                                                                    time.Time
}

// This fake implements the proposed gateway contract, not provider OAuth.
// It refuses conflicting reacquisition, wider children and account relinking.
type childPassGateway struct {
	sync.Mutex
	bindings   map[string]childPassRequest
	issued     []string
	called     []string
	relinked   bool
	afterIssue func()
}

func (g *childPassGateway) serve(w http.ResponseWriter, r *http.Request) {
	g.Lock()
	defer g.Unlock()
	if r.URL.Path == "/v1/task-passes" {
		var req childPassRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil || r.Header.Get("Authorization") != "Bearer fixture-controller" || req.GrantRef != "sender-generation-1" || g.relinked {
			w.WriteHeader(403)
			return
		}
		if req.ParentTaskUID != "" {
			p, ok := g.bindings[req.ParentTaskUID]
			if !ok || p.NamespaceUID != req.NamespaceUID || p.GatewayUID != req.GatewayUID || req.Deadline.After(p.Deadline) || !allGatewayToolsIn(req.Tools, p.Tools) {
				w.WriteHeader(409)
				return
			}
		}
		if old, ok := g.bindings[req.TaskUID]; ok && (old.ParentTaskUID != req.ParentTaskUID || !slices.Equal(old.Tools, req.Tools) || !old.Deadline.Equal(req.Deadline)) {
			w.WriteHeader(409)
			return
		}
		g.bindings[req.TaskUID] = req
		g.issued = append(g.issued, req.TaskUID)
		if g.afterIssue != nil {
			f := g.afterIssue
			g.afterIssue = nil
			f()
		}
		expires := time.Now().Add(time.Minute)
		if req.Deadline.Before(expires) {
			expires = req.Deadline
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"pass": "fixture-pass-" + req.TaskUID, "expiresAt": expires})
		return
	}
	if r.URL.Path == "/v1/task-tools/lookup" {
		uid := r.Header.Get("Authorization")[len("Bearer fixture-pass-"):]
		if _, ok := g.bindings[uid]; !ok {
			w.WriteHeader(403)
			return
		}
		g.called = append(g.called, uid)
		_, _ = w.Write([]byte(`{"answer":"child"}`))
		return
	}
	w.WriteHeader(404)
}

func allGatewayToolsIn(tools, parent []string) bool {
	for _, name := range tools {
		if !slices.Contains(parent, name) {
			return false
		}
	}
	return true
}

func gatewayBoundBody(t *testing.T, r *TaskReconciler, task *corev1alpha1.Task) agentExecutionSnapshotBody {
	t.Helper()
	snapshot, err := r.AgentExecutionSnapshots.GetAgentExecutionSnapshot(t.Context(), store.AgentExecutionSnapshotKey{TaskUID: string(task.UID), Digest: task.Status.AgentExecutionBinding.Snapshot.Digest})
	require.NoError(t, err)
	require.NotContains(t, string(snapshot.Body), "fixture-controller")
	require.NotContains(t, string(snapshot.Body), "fixture-pass-")
	body, err := decodeAgentExecutionSnapshot(snapshot.Body)
	require.NoError(t, err)
	return body
}

func bindPassTask(t *testing.T, r *TaskReconciler, task *corev1alpha1.Task, agent *corev1alpha1.Agent) {
	t.Helper()
	_, planErr := r.resolveAgentExecutionCandidate(t.Context(), task, agent)
	require.NoError(t, planErr)
	_, err, handled := r.ensureAgentExecutionBinding(t.Context(), task, agent)
	require.NoError(t, err)
	require.False(t, handled, "status: %+v", task.Status)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	task.Status.Phase = corev1alpha1.TaskPhaseRunning
	require.NoError(t, r.Status().Update(t.Context(), task))
}

func newGatewayPassChild(t *testing.T, r *TaskReconciler, parent *corev1alpha1.Task, name string, namespaces ...string) *corev1alpha1.Task {
	t.Helper()
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: parent.Namespace, Finalizers: []string{labels.TaskFinalizer}, Annotations: map[string]string{
		labels.AnnotationParentTaskName: parent.Name, labels.AnnotationParentTaskNamespace: parent.Namespace, labels.AnnotationParentTaskUID: string(parent.UID),
	}}, Spec: *parent.Spec.DeepCopy()}
	if len(namespaces) > 0 {
		child.Namespace = namespaces[0]
	}
	child.Spec.Timeout = &metav1.Duration{Duration: 30 * time.Minute}
	child.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"lookup", "forbidden"}}
	require.NoError(t, r.Create(t.Context(), child))
	return child
}

func TestGatewayTaskPassChildInheritance(t *testing.T) {
	g := &childPassGateway{bindings: map[string]childPassRequest{}}
	server := httptest.NewTLSServer(http.HandlerFunc(g.serve))
	defer server.Close()
	r, parent, agent := gatewayPassAPIFixture(t, server.URL)
	r.TaskProvenanceProtected = true
	r.GatewayService.HTTPClient = server.Client()
	r.GatewayService.Config.AllowInsecureLoopback = true
	rootGrant, err := r.GatewayService.ResolveTaskGrant(t.Context(), parent)
	require.NoError(t, err)
	r.GatewayService.TaskPassCredentials = map[string]gatewayruntime.TaskPassCredential{rootGrant.GatewayUID: {Endpoint: server.URL, Token: "fixture-controller"}}
	for _, name := range []string{"lookup", "other", "forbidden"} {
		require.NoError(t, r.Create(t.Context(), &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: parent.Namespace}, Spec: corev1alpha1.ToolSpec{Description: name, BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead, HTTP: &corev1alpha1.HTTPExecution{URL: server.URL + "/v1/task-tools/" + name, GatewayTaskPass: true}}}))
	}
	agent.Spec.Runtime.DefaultAllowedTools = []string{"lookup", "other"}
	bindPassTask(t, r, parent, agent)
	child := newGatewayPassChild(t, r, parent, "child-pass")
	bindPassTask(t, r, child, agent)
	parentBody, childBody := gatewayBoundBody(t, r, parent), gatewayBoundBody(t, r, child)
	require.Equal(t, []string{"lookup"}, childBody.GatewayGrant.Tools)
	require.Equal(t, parentBody.GatewayGrant.Deadline, childBody.GatewayGrant.Deadline)
	require.Equal(t, parentBody.GatewayGrant.GrantRef, childBody.GatewayGrant.GrantRef)
	request, _ := testMCPBrokerRequest(t, harnessv2.MCPToolEffectReadOnly)
	request.Namespace = child.Namespace
	request.Metadata.TaskUID = harnessv2.TaskUID(child.UID)
	request.Metadata.OperationID = "child-operation"
	request.Call.ToolName = "lookup"
	request.Call.Arguments = json.RawMessage(`{}`)
	tool := &corev1alpha1.Tool{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: child.Namespace, Name: "lookup"}, tool))
	descriptor, err := customACPMCPToolDescriptor(tool)
	require.NoError(t, err)
	ctx := withACPMCPAuthenticatedTask(t.Context(), ACPMCPAuthenticatedTask{Name: child.Name, Namespace: child.Namespace, UID: string(child.UID)})
	protected := true
	execute := func() error {
		executor := RegistryACPMCPToolExecutor{TaskProvenanceProtected: protected, Reader: r.Client, KubeClient: k8sfake.NewSimpleClientset(), HTTPClient: server.Client(), AgentExecutionSnapshots: r.AgentExecutionSnapshots, GatewayService: r.GatewayService}
		_, err := executor.ExecuteACPMCPTool(ctx, request, descriptor)
		return err
	}
	require.NoError(t, execute())
	g.Lock()
	require.Equal(t, []string{string(parent.UID), string(child.UID)}, g.issued)
	require.Equal(t, []string{string(child.UID)}, g.called)
	g.Unlock()
	// A reconstructed executor reacquires the exact binding, with no parent tool effect.
	require.NoError(t, execute())
	g.Lock()
	require.Len(t, g.called, 2)
	g.Unlock()
	protected = false
	require.Error(t, execute(), "restart without provenance protection must refuse existing children")
	protected = true
	r.TaskProvenanceProtected = false
	_, err = r.freezeGatewayTaskGrant(t.Context(), child, *childBody.MCPConfiguration)
	require.True(t, isPermanentACPAgentConfigurationError(err))
	r.TaskProvenanceProtected = true
	for _, change := range []func(*corev1alpha1.Task){
		func(c *corev1alpha1.Task) { c.Annotations[labels.AnnotationParentTaskUID] = "replacement" },
		func(c *corev1alpha1.Task) { c.Spec.RequestedBy.Subject = "other-sender" },
	} {
		forged := child.DeepCopy()
		change(forged)
		_, err = r.freezeGatewayTaskGrant(t.Context(), forged, *childBody.MCPConfiguration)
		require.True(t, isPermanentACPAgentConfigurationError(err))
	}
	// The advertised but non-inherited tool never gets a pass.
	forbidden := &corev1alpha1.Tool{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: child.Namespace, Name: "forbidden"}, forbidden))
	forbiddenDescriptor, err := customACPMCPToolDescriptor(forbidden)
	require.NoError(t, err)
	deniedRequest := request
	deniedRequest.Call.ToolName = "forbidden"
	deniedExecutor := RegistryACPMCPToolExecutor{TaskProvenanceProtected: true, Reader: r.Client, KubeClient: k8sfake.NewSimpleClientset(), HTTPClient: server.Client(), AgentExecutionSnapshots: r.AgentExecutionSnapshots, GatewayService: r.GatewayService}
	_, err = deniedExecutor.ExecuteACPMCPTool(ctx, deniedRequest, forbiddenDescriptor)
	require.Error(t, err)
	grandchild := newGatewayPassChild(t, r, child, "grandchild-pass")
	bindPassTask(t, r, grandchild, agent)
	grandBody := gatewayBoundBody(t, r, grandchild)
	require.Equal(t, []string{"lookup"}, grandBody.GatewayGrant.Tools)
	grandCtx := withACPMCPAuthenticatedTask(t.Context(), ACPMCPAuthenticatedTask{Name: grandchild.Name, Namespace: grandchild.Namespace, UID: string(grandchild.UID)})
	grandRequest := request
	grandRequest.Metadata.TaskUID = harnessv2.TaskUID(grandchild.UID)
	_, err = deniedExecutor.ExecuteACPMCPTool(grandCtx, grandRequest, descriptor)
	require.NoError(t, err)
	g.Lock()
	require.Equal(t, []string{string(parent.UID), string(child.UID), string(grandchild.UID)}, g.issued[len(g.issued)-3:])
	g.Unlock()
	// Cross-namespace child execution retains the original gateway namespace UID.
	require.NoError(t, r.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pass-child-ns"}}))
	for _, name := range []string{"lookup", "forbidden"} {
		copy := tool.DeepCopy()
		copy.ObjectMeta = metav1.ObjectMeta{Name: name, Namespace: "pass-child-ns"}
		copy.Spec.HTTP.URL = server.URL + "/v1/task-tools/" + name
		require.NoError(t, r.Create(t.Context(), copy))
	}
	cross := newGatewayPassChild(t, r, parent, "cross-pass", "pass-child-ns")
	bindPassTask(t, r, cross, agent)
	crossCtx := withACPMCPAuthenticatedTask(t.Context(), ACPMCPAuthenticatedTask{Name: cross.Name, Namespace: cross.Namespace, UID: string(cross.UID)})
	crossRequest := request
	crossRequest.Namespace = cross.Namespace
	crossRequest.Metadata.TaskUID = harnessv2.TaskUID(cross.UID)
	crossTool := &corev1alpha1.Tool{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: cross.Namespace, Name: "lookup"}, crossTool))
	crossDescriptor, err := customACPMCPToolDescriptor(crossTool)
	require.NoError(t, err)
	_, err = deniedExecutor.ExecuteACPMCPTool(crossCtx, crossRequest, crossDescriptor)
	require.NoError(t, err)
	g.Lock()
	require.Equal(t, rootGrant.NamespaceUID, g.bindings[string(cross.UID)].NamespaceUID)
	g.Unlock()
	for _, key := range []string{labels.AnnotationCancelRequested, labels.AnnotationParentTaskUID, labels.AnnotationContinuationOfUID} {
		saved := child.Annotations[key]
		child.Annotations[key] = "changed"
		require.NoError(t, r.Update(t.Context(), child))
		g.Lock()
		before := len(g.issued)
		g.Unlock()
		require.Error(t, execute())
		g.Lock()
		require.Len(t, g.issued, before)
		g.Unlock()
		if saved == "" {
			delete(child.Annotations, key)
		} else {
			child.Annotations[key] = saved
		}
		require.NoError(t, r.Update(t.Context(), child))
	}
	// Parent deliberately parks after its successful turn; root reply authority stays closed.
	parent.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	parent.Status.Execution = &corev1alpha1.TaskExecutionStatus{Attempt: 1, PromptID: "parent-turn", State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded}
	parent.Status.Wait = &corev1alpha1.TaskWaitStatus{RequestedAt: metav1.Now(), State: corev1alpha1.TaskWaitStateWaiting, Kind: corev1alpha1.TaskWaitKindTasks, Attempt: 1, PromptID: "parent-turn", References: []string{child.Name}, ReferenceUIDs: []string{string(child.UID)}, Deadline: metav1.NewTime(time.Now().Add(2 * time.Minute))}
	require.NoError(t, r.Status().Update(t.Context(), parent))
	eligible, err := r.GatewayService.ResolveReplyEligibility(t.Context(), parent)
	require.NoError(t, err)
	require.False(t, eligible)
	require.NoError(t, execute())
	// Final reply projection must not consume the separate child delegation.
	require.NoError(t, r.GatewayService.ProjectTerminals(t.Context()))
	event, err := r.GatewayService.EventStore.GetGatewayEventForTask(t.Context(), parent.Namespace, parent.Name, string(parent.UID))
	require.NoError(t, err)
	require.Equal(t, store.GatewayEventCompleted, event.State)
	require.NoError(t, execute())
	lateChild := newGatewayPassChild(t, r, parent, "late-child-pass")
	parent.Status.Wait.References = append(parent.Status.Wait.References, lateChild.Name)
	parent.Status.Wait.ReferenceUIDs = append(parent.Status.Wait.ReferenceUIDs, string(lateChild.UID))
	require.NoError(t, r.Status().Update(t.Context(), parent))
	bindPassTask(t, r, lateChild, agent)
	require.True(t, parent.Status.Wait.Deadline.Time.Equal(gatewayBoundBody(t, r, lateChild).GatewayGrant.Deadline))
	good := parent.DeepCopy()
	for _, tc := range []struct {
		name   string
		change func(*corev1alpha1.Task)
	}{
		{"cancel-request", func(p *corev1alpha1.Task) { p.Annotations[labels.AnnotationCancelRequested] = "stop" }},
		{"cancel-wait", func(p *corev1alpha1.Task) { p.Annotations[labels.AnnotationCancelWait] = "stop" }},
		{"failed", func(p *corev1alpha1.Task) { p.Status.Phase = corev1alpha1.TaskPhaseFailed }},
		{"root-link", func(p *corev1alpha1.Task) {
			p.Annotations[gatewayruntime.TaskGatewayEventAnnotation] = "different-event"
		}},
		{"chain-deadline", func(p *corev1alpha1.Task) {
			p.Annotations[labels.AnnotationContinuationChainDeadline] = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
		}},
		{"abandoned", func(p *corev1alpha1.Task) { p.Status.Wait.State = corev1alpha1.TaskWaitStateAbandoned }},
		{"expired", func(p *corev1alpha1.Task) { p.Status.Wait.Deadline = metav1.NewTime(time.Now().Add(-time.Second)) }},
		{"wrong-child", func(p *corev1alpha1.Task) { p.Status.Wait.ReferenceUIDs = []string{"replacement"} }},
		{"wrong-turn", func(p *corev1alpha1.Task) { p.Status.Wait.PromptID = "other-turn" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(parent), parent))
			tc.change(parent)
			status := *parent.Status.DeepCopy()
			require.NoError(t, r.Update(t.Context(), parent))
			parent.Status = status
			require.NoError(t, r.Status().Update(t.Context(), parent))
			g.Lock()
			before := len(g.issued)
			g.Unlock()
			require.Error(t, execute())
			g.Lock()
			require.Len(t, g.issued, before)
			g.Unlock()
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(parent), parent))
			parent.Annotations = good.DeepCopy().Annotations
			require.NoError(t, r.Update(t.Context(), parent))
			parent.Status = *good.Status.DeepCopy()
			require.NoError(t, r.Status().Update(t.Context(), parent))
		})
	}
	g.Lock()
	g.relinked = true
	before := len(g.called)
	g.Unlock()
	require.Error(t, execute())
	g.Lock()
	require.Len(t, g.called, before)
	g.relinked = false
	g.Unlock()
	// Cancellation racing issuance must prevent the following tool send.
	g.Lock()
	g.afterIssue = func() {
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(parent), parent))
		require.NoError(t, r.requestDelegatedTaskCancellation(t.Context(), parent, "stop"))
		require.Empty(t, parent.Annotations[labels.AnnotationCancelRequested], "a completed parked turn gets only cancel-wait")
		require.Equal(t, "stop", parent.Annotations[labels.AnnotationCancelWait])
	}
	g.Unlock()
	require.Error(t, execute())
	g.Lock()
	require.Len(t, g.called, before)
	g.Unlock()
	// Deletion independently stops the child, even with a live waiting parent.
	delete(parent.Annotations, labels.AnnotationCancelRequested)
	delete(parent.Annotations, labels.AnnotationCancelWait)
	require.NoError(t, r.Update(t.Context(), parent))
	require.NoError(t, r.Delete(t.Context(), child))
	g.Lock()
	issuedBefore := len(g.issued)
	g.Unlock()
	require.Error(t, execute())
	g.Lock()
	require.Len(t, g.issued, issuedBefore)
	g.Unlock()
}
