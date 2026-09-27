/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	workerexecutor "github.com/orka-agents/orka/internal/worker"
)

func freezeFixtures(ready bool) (runtime.Object, runtime.Object, runtime.Object, *corev1alpha1.Task) {
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "gh_search", Namespace: "tenant"},
		Spec: corev1alpha1.ToolSpec{Description: "search", BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead, HTTP: &corev1alpha1.HTTPExecution{
			URL: "https://api.github.com/search", OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "github-conn"},
		}},
	}
	policy := &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "github-conn", Namespace: "tenant"},
		Spec:       corev1alpha1.OutboundAccessPolicySpec{Connection: &corev1alpha1.ConnectionOutboundAccess{ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}}},
	}
	requester := &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}
	connection := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: connectors.ConnectionName("github", requester.Issuer, requester.Subject), Namespace: "tenant", UID: "conn-uid", Generation: 4},
		Spec: corev1alpha1.ConnectionSpec{
			Subject: corev1alpha1.ConnectionSubject{Issuer: requester.Issuer, Subject: requester.Subject}, ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}, Mode: "readWrite",
		},
	}
	if ready {
		connection.Status.Conditions = []metav1.Condition{
			{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonLinked, ObservedGeneration: 4},
			{Type: corev1alpha1.ConnectionConditionScopesGranted, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonScopesGranted, ObservedGeneration: 4},
			{Type: corev1alpha1.ConnectionConditionProviderResolved, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonProviderResolved, ObservedGeneration: 4},
		}
	}
	SetRequesterStampKey(testRequesterStampKey)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "task", Namespace: "tenant", UID: "task-uid",
			Annotations: map[string]string{
				labels.AnnotationRequestedBySource: labels.RequestedBySourceAPI,
				labels.AnnotationRequestedByStamp:  connectors.RequesterStamp(testRequesterStampKey, "task-uid", requester.Issuer, requester.Subject),
			},
		},
		Spec: corev1alpha1.TaskSpec{RequestedBy: requester},
	}
	return tool, policy, connection, task
}

// testRequesterStampKey is the stamp key the freeze fixtures verify under.
var testRequesterStampKey = []byte("0123456789abcdef0123456789abcdef")

func brokeredConfiguration(names ...string) harnessv2.MCPPolicyConfiguration {
	cfg := harnessv2.MCPPolicyConfiguration{}
	for _, name := range names {
		cfg.ToolPolicy.Tools = append(cfg.ToolPolicy.Tools, harnessv2.MCPToolDescriptor{Name: name, Source: harnessv2.MCPToolSourceBrokeredCustom})
	}
	return cfg
}

func TestFreezeRequesterConnections(t *testing.T) {
	scheme := connectorTestScheme(t)
	tool, policy, connection, task := freezeFixtures(true)
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tool, policy, connection).Build()
	frozen, err := freezeRequesterConnections(context.Background(), reader, task, brokeredConfiguration("gh_search", "gh_search"))
	if err != nil {
		t.Fatal(err)
	}
	if len(frozen) != 1 || frozen[0].PolicyName != "github-conn" || frozen[0].UID != "conn-uid" || frozen[0].Generation != 4 || frozen[0].Mode != "readWrite" || frozen[0].Provider != "github" {
		t.Fatalf("frozen = %+v", frozen)
	}

	// Not Ready, no requester, non-connection policy, or unknown tool: nothing frozen, no error.
	_, _, unready, _ := freezeFixtures(false)
	reader = ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tool, policy, unready).Build()
	if frozen, err := freezeRequesterConnections(context.Background(), reader, task, brokeredConfiguration("gh_search")); err != nil || len(frozen) != 0 {
		t.Fatalf("unready: frozen = %+v err = %v", frozen, err)
	}
	reader = ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tool, policy, connection).Build()
	anonymous := task.DeepCopy()
	anonymous.Spec.RequestedBy = nil
	if frozen, err := freezeRequesterConnections(context.Background(), reader, anonymous, brokeredConfiguration("gh_search")); err != nil || len(frozen) != 0 {
		t.Fatalf("anonymous: frozen = %+v err = %v", frozen, err)
	}
	direct := policy.(*corev1alpha1.OutboundAccessPolicy).DeepCopy()
	direct.Spec = corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{}}
	reader = ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tool, direct, connection).Build()
	if frozen, err := freezeRequesterConnections(context.Background(), reader, task, brokeredConfiguration("gh_search")); err != nil || len(frozen) != 0 {
		t.Fatalf("direct: frozen = %+v err = %v", frozen, err)
	}
	if frozen, err := freezeRequesterConnections(context.Background(), reader, task, brokeredConfiguration("missing_tool")); err != nil || len(frozen) != 0 {
		t.Fatalf("missing tool: frozen = %+v err = %v", frozen, err)
	}

	// Read failures propagate so binding retries.
	failing := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tool, policy, connection).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
			if _, isConnection := obj.(*corev1alpha1.Connection); isConnection {
				return errors.New("apiserver unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	if _, err := freezeRequesterConnections(context.Background(), failing, task, brokeredConfiguration("gh_search")); err == nil {
		t.Fatal("read failure must propagate")
	}
}

type fakeSnapshotStore struct {
	snapshot *store.AgentExecutionSnapshot
	err      error
}

func (f fakeSnapshotStore) PersistAgentExecutionSnapshot(context.Context, store.AgentExecutionSnapshot) error {
	return nil
}

func (f fakeSnapshotStore) GetAgentExecutionSnapshot(context.Context, store.AgentExecutionSnapshotKey) (*store.AgentExecutionSnapshot, error) {
	return f.snapshot, f.err
}

func (f fakeSnapshotStore) DeleteAgentExecutionSnapshots(context.Context, string) error { return nil }

func TestBindFrozenConnections(t *testing.T) {
	_, _, _, task := freezeFixtures(true)
	task.Status.AgentExecutionBinding = &corev1alpha1.AgentExecutionBinding{Snapshot: corev1alpha1.AgentExecutionSnapshotRef{Digest: "abc"}}
	body, err := json.Marshal(agentExecutionSnapshotBody{Connections: []agentExecutionSnapshotConnection{
		{PolicyName: "github-conn", Provider: "github", ConnectionName: "github-x", UID: "conn-uid", Generation: 4, Mode: "readWrite"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	executor := workerexecutor.NewToolExecutorForNamespace("tenant", nil, nil)
	snapshots := fakeSnapshotStore{snapshot: &store.AgentExecutionSnapshot{Body: body}}
	if err := bindFrozenConnections(context.Background(), snapshots, task, executor); err != nil {
		t.Fatal(err)
	}
	requester, frozen := executor.Requester(), executor.FrozenConnections()
	if requester == nil || requester.Subject != "alice" {
		t.Fatalf("requester = %+v", requester)
	}
	if frozen["github-conn"].UID != "conn-uid" || frozen["github-conn"].Generation != 4 {
		t.Fatalf("frozen = %+v", frozen)
	}

	// No snapshot store or missing snapshot: requester set, nothing frozen.
	executor = workerexecutor.NewToolExecutorForNamespace("tenant", nil, nil)
	if err := bindFrozenConnections(context.Background(), nil, task, executor); err != nil {
		t.Fatal(err)
	}
	if requester, frozen := executor.Requester(), executor.FrozenConnections(); requester == nil || len(frozen) != 0 {
		t.Fatalf("without store: requester = %+v frozen = %+v", requester, frozen)
	}
	if err := bindFrozenConnections(context.Background(), fakeSnapshotStore{err: store.ErrNotFound}, task, executor); err != nil {
		t.Fatalf("missing snapshot must not fail: %v", err)
	}
	if err := bindFrozenConnections(context.Background(), fakeSnapshotStore{err: errors.New("db down")}, task, executor); err == nil {
		t.Fatal("store failure must propagate")
	}
	if err := bindFrozenConnections(context.Background(), fakeSnapshotStore{snapshot: &store.AgentExecutionSnapshot{Body: []byte("{")}}, task, executor); err == nil {
		t.Fatal("corrupt snapshot must fail")
	}
	if err := bindFrozenConnections(context.Background(), snapshots, nil, executor); err == nil {
		t.Fatal("nil task must fail")
	}
	_ = types.UID("")
}

// TestFreezeRequiresVerifiedRequesterProvenance covers a compromised trusted
// worker creating a Task that copies another person's requester identity:
// without the API stamp or a matching verified parent chain, nothing freezes.
func TestFreezeRequiresVerifiedRequesterProvenance(t *testing.T) {
	scheme := connectorTestScheme(t)
	tool, policy, connection, stamped := freezeFixtures(true)
	forged := stamped.DeepCopy()
	forged.Name, forged.UID, forged.Annotations = "forged", "forged-uid", nil
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tool, policy, connection, stamped, forged).Build()
	if frozen, err := freezeRequesterConnections(context.Background(), reader, forged, brokeredConfiguration("gh_search")); err != nil || len(frozen) != 0 {
		t.Fatalf("forged parentless task: frozen = %+v err = %v", frozen, err)
	}
	if frozen, err := freezeRequesterConnections(context.Background(), reader, stamped, brokeredConfiguration("gh_search")); err != nil || len(frozen) != 1 {
		t.Fatalf("api-stamped task: frozen = %+v err = %v", frozen, err)
	}
	// Admission is not retroactive: a Task planted with the source
	// annotation while admission was disabled carries no stamp the API
	// sealed, and a stamp copied from another Task does not match this UID.
	planted := stamped.DeepCopy()
	planted.Name, planted.UID = "planted", "planted-uid"
	planted.Annotations = map[string]string{labels.AnnotationRequestedBySource: labels.RequestedBySourceAPI}
	copied := planted.DeepCopy()
	copied.Name, copied.UID = "copied", "copied-uid"
	copied.Annotations[labels.AnnotationRequestedByStamp] = stamped.Annotations[labels.AnnotationRequestedByStamp]
	reader = ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tool, policy, connection, stamped, planted, copied).Build()
	for _, task := range []*corev1alpha1.Task{planted, copied} {
		if frozen, err := freezeRequesterConnections(context.Background(), reader, task, brokeredConfiguration("gh_search")); err != nil || len(frozen) != 0 {
			t.Fatalf("%s task: frozen = %+v err = %v, want nothing without a stamp sealed for this UID", task.Name, frozen, err)
		}
	}
	// A Task the API just created is waiting for its seal (a second write
	// after the create): dispatch retries instead of committing a write-once
	// binding with no Connections. Past the grace window it is unverified.
	fresh := planted.DeepCopy()
	fresh.Name, fresh.UID, fresh.CreationTimestamp = "fresh", "fresh-uid", metav1.Now()
	if _, err := freezeRequesterConnections(context.Background(), reader, fresh, brokeredConfiguration("gh_search")); !errors.Is(err, ErrRequesterStampPending) {
		t.Fatalf("fresh unsealed task: err = %v, want ErrRequesterStampPending", err)
	}
	fresh.CreationTimestamp = metav1.NewTime(time.Now().Add(-requesterStampGrace - time.Minute))
	if frozen, err := freezeRequesterConnections(context.Background(), reader, fresh, brokeredConfiguration("gh_search")); err != nil || len(frozen) != 0 {
		t.Fatalf("stale unsealed task: frozen = %+v err = %v, want unverified", frozen, err)
	}
	// Without a configured key nothing is ever verified, and nothing waits
	// for a seal that cannot arrive: dispatch proceeds without Connections.
	SetRequesterStampKey(nil)
	t.Cleanup(func() { SetRequesterStampKey(testRequesterStampKey) })
	if frozen, err := freezeRequesterConnections(context.Background(), reader, stamped, brokeredConfiguration("gh_search")); err != nil || len(frozen) != 0 {
		t.Fatalf("no stamp key: frozen = %+v err = %v, want fail closed", frozen, err)
	}
	fresh.CreationTimestamp = metav1.Now()
	if frozen, err := freezeRequesterConnections(context.Background(), reader, fresh, brokeredConfiguration("gh_search")); err != nil || len(frozen) != 0 {
		t.Fatalf("no stamp key, fresh task: frozen = %+v err = %v, want no wait", frozen, err)
	}
	SetRequesterStampKey(testRequesterStampKey)

	// A child that inherits the requester through its verified coordination
	// parent is trusted; one that names a different person is not.
	controller := true
	child := forged.DeepCopy()
	child.Name, child.UID = "child", "child-uid"
	child.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: stamped.Name, UID: stamped.UID, Controller: &controller,
	}}
	impostor := child.DeepCopy()
	impostor.Name, impostor.UID = "impostor", "impostor-uid"
	impostor.Spec.RequestedBy = &corev1alpha1.RequestedBy{Issuer: stamped.Spec.RequestedBy.Issuer, Subject: "victim"}
	orphan := child.DeepCopy()
	orphan.Name, orphan.UID = "orphan", "orphan-uid"
	orphan.OwnerReferences[0].UID = "stale-parent-uid"
	reader = ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tool, policy, connection, stamped, child, impostor, orphan).Build()
	if frozen, err := freezeRequesterConnections(context.Background(), reader, child, brokeredConfiguration("gh_search")); err != nil || len(frozen) != 1 {
		t.Fatalf("inheriting child: frozen = %+v err = %v", frozen, err)
	}
	if frozen, err := freezeRequesterConnections(context.Background(), reader, impostor, brokeredConfiguration("gh_search")); err != nil || len(frozen) != 0 {
		t.Fatalf("impostor child: frozen = %+v err = %v", frozen, err)
	}
	if frozen, err := freezeRequesterConnections(context.Background(), reader, orphan, brokeredConfiguration("gh_search")); err != nil || len(frozen) != 0 {
		t.Fatalf("child with a replaced parent: frozen = %+v err = %v", frozen, err)
	}
}
