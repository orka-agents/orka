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
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/connectors"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
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
		connection.Status.GrantSequence = 1
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
	frozen, err := freezeRequesterConnections(context.Background(), reader, tools.NewRegistry(), task, brokeredConfiguration("gh_search", "gh_search"))
	if err != nil {
		t.Fatal(err)
	}
	if len(frozen) != 1 || frozen[0].PolicyName != "github-conn" || frozen[0].UID != "conn-uid" || frozen[0].Generation != 4 || frozen[0].Mode != "readWrite" || frozen[0].Provider != "github" {
		t.Fatalf("frozen = %+v", frozen)
	}

	// Not Ready or no requester: the connection-mode policy is still frozen
	// (so a later retargeting to a service credential is refused) but no
	// Connection fills the entry, and the call fails closed.
	_, _, unready, _ := freezeFixtures(false)
	reader = ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tool, policy, unready).Build()
	if frozen, err := freezeRequesterConnections(context.Background(), reader, nil, task, brokeredConfiguration("gh_search")); err != nil ||
		len(frozen) != 1 || frozen[0].PolicyName != "github-conn" || frozen[0].UID != "" || frozen[0].GrantSequence != 0 {
		t.Fatalf("unready: frozen = %+v err = %v, want the policy frozen without a Connection", frozen, err)
	}
	reader = ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tool, policy, connection).Build()
	anonymous := task.DeepCopy()
	anonymous.Spec.RequestedBy = nil
	if frozen, err := freezeRequesterConnections(context.Background(), reader, nil, anonymous, brokeredConfiguration("gh_search")); err != nil ||
		len(frozen) != 1 || frozen[0].UID != "" {
		t.Fatalf("anonymous: frozen = %+v err = %v, want the policy frozen without a Connection", frozen, err)
	}
	// A non-connection policy freezes nothing. A brokered Tool that vanished
	// between descriptor building and the freeze makes binding retry: a Tool
	// recreated under a policy in another mode must never run unfrozen.
	direct := policy.(*corev1alpha1.OutboundAccessPolicy).DeepCopy()
	direct.Spec = corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{}}
	reader = ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tool, direct, connection).Build()
	if frozen, err := freezeRequesterConnections(context.Background(), reader, nil, task, brokeredConfiguration("gh_search")); err != nil || len(frozen) != 0 {
		t.Fatalf("direct: frozen = %+v err = %v", frozen, err)
	}
	if _, err := freezeRequesterConnections(context.Background(), reader, nil, task, brokeredConfiguration("missing_tool")); err == nil || !strings.Contains(err.Error(), "binding retries") {
		t.Fatalf("missing tool: err = %v, want a retry", err)
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
	if _, err := freezeRequesterConnections(context.Background(), failing, nil, task, brokeredConfiguration("gh_search")); err == nil {
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
	if frozen, err := freezeRequesterConnections(context.Background(), reader, nil, forged, brokeredConfiguration("gh_search")); err != nil || !frozenWithoutConnection(frozen) {
		t.Fatalf("forged parentless task: frozen = %+v err = %v, want the policy frozen without a Connection", frozen, err)
	}
	if frozen, err := freezeRequesterConnections(context.Background(), reader, nil, stamped, brokeredConfiguration("gh_search")); err != nil || len(frozen) != 1 {
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
		if frozen, err := freezeRequesterConnections(context.Background(), reader, nil, task, brokeredConfiguration("gh_search")); err != nil || !frozenWithoutConnection(frozen) {
			t.Fatalf("%s task: frozen = %+v err = %v, want no Connection without a stamp sealed for this UID", task.Name, frozen, err)
		}
	}
	// A Task the API just created is waiting for its seal (a second write
	// after the create): dispatch retries instead of committing a write-once
	// binding with no Connections. Past the grace window it is unverified.
	fresh := planted.DeepCopy()
	fresh.Name, fresh.UID, fresh.CreationTimestamp = "fresh", "fresh-uid", metav1.Now()
	if _, err := freezeRequesterConnections(context.Background(), reader, nil, fresh, brokeredConfiguration("gh_search")); !errors.Is(err, ErrRequesterStampPending) {
		t.Fatalf("fresh unsealed task: err = %v, want ErrRequesterStampPending", err)
	}
	// A Task that reaches no connection-mode policy is not held for a seal
	// it has no use for.
	plain := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "tenant"},
		Spec:       corev1alpha1.ToolSpec{Description: "plain", HTTP: &corev1alpha1.HTTPExecution{URL: "https://api.example.test/x"}},
	}
	plainReader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(plain).Build()
	if frozen, err := freezeRequesterConnections(context.Background(), plainReader, nil, fresh, brokeredConfiguration("plain")); err != nil || len(frozen) != 0 {
		t.Fatalf("fresh unsealed task without connection-mode policies: frozen = %+v err = %v, want no wait", frozen, err)
	}
	fresh.CreationTimestamp = metav1.NewTime(time.Now().Add(-requesterStampGrace - time.Minute))
	if frozen, err := freezeRequesterConnections(context.Background(), reader, nil, fresh, brokeredConfiguration("gh_search")); err != nil || !frozenWithoutConnection(frozen) {
		t.Fatalf("stale unsealed task: frozen = %+v err = %v, want unverified", frozen, err)
	}
	// Without a configured key nothing is ever verified, and nothing waits
	// for a seal that cannot arrive: dispatch proceeds without Connections.
	SetRequesterStampKey(nil)
	t.Cleanup(func() { SetRequesterStampKey(testRequesterStampKey) })
	if frozen, err := freezeRequesterConnections(context.Background(), reader, nil, stamped, brokeredConfiguration("gh_search")); err != nil || !frozenWithoutConnection(frozen) {
		t.Fatalf("no stamp key: frozen = %+v err = %v, want fail closed", frozen, err)
	}
	fresh.CreationTimestamp = metav1.Now()
	if frozen, err := freezeRequesterConnections(context.Background(), reader, nil, fresh, brokeredConfiguration("gh_search")); err != nil || !frozenWithoutConnection(frozen) {
		t.Fatalf("no stamp key, fresh task: frozen = %+v err = %v, want no wait", frozen, err)
	}
	SetRequesterStampKey(testRequesterStampKey)

	// A coordination child is trusted only once the controller sealed a stamp
	// for the child's own UID (on its parent's worker's authenticated
	// request); an owner reference to a stamped parent proves nothing by
	// itself, since a Task writer can plant one while admission is off.
	controller := true
	child := forged.DeepCopy()
	child.Name, child.UID = "child", "child-uid"
	child.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: stamped.Name, UID: stamped.UID, Controller: &controller,
	}}
	sealed := child.DeepCopy()
	sealed.Name, sealed.UID = "sealed", "sealed-uid"
	sealed.Annotations = map[string]string{
		labels.AnnotationRequestedBySource: labels.RequestedBySourceAPI,
		labels.AnnotationRequestedByStamp:  connectors.RequesterStamp(testRequesterStampKey, "sealed-uid", stamped.Spec.RequestedBy.Issuer, stamped.Spec.RequestedBy.Subject),
	}
	impostor := sealed.DeepCopy()
	impostor.Name, impostor.UID = "impostor", "impostor-uid"
	impostor.Spec.RequestedBy = &corev1alpha1.RequestedBy{Issuer: stamped.Spec.RequestedBy.Issuer, Subject: "victim"}
	reader = ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tool, policy, connection, stamped, child, sealed, impostor).Build()
	if frozen, err := freezeRequesterConnections(context.Background(), reader, nil, child, brokeredConfiguration("gh_search")); err != nil || !frozenWithoutConnection(frozen) {
		t.Fatalf("owner reference alone: frozen = %+v err = %v, want nothing without the child's own seal", frozen, err)
	}
	// A child a worker just created is waiting for its parent's worker to
	// have it sealed: dispatch retries instead of committing a write-once
	// binding with no Connections. Past the grace window it is unverified.
	freshChild := child.DeepCopy()
	freshChild.Name, freshChild.UID, freshChild.CreationTimestamp = "fresh-child", "fresh-child-uid", metav1.Now()
	if _, err := freezeRequesterConnections(context.Background(), reader, nil, freshChild, brokeredConfiguration("gh_search")); !errors.Is(err, ErrRequesterStampPending) {
		t.Fatalf("fresh unsealed child: err = %v, want ErrRequesterStampPending", err)
	}
	freshChild.CreationTimestamp = metav1.NewTime(time.Now().Add(-requesterStampGrace - time.Minute))
	if frozen, err := freezeRequesterConnections(context.Background(), reader, nil, freshChild, brokeredConfiguration("gh_search")); err != nil || !frozenWithoutConnection(frozen) {
		t.Fatalf("stale unsealed child: frozen = %+v err = %v, want unverified", frozen, err)
	}
	// A fresh Task that is neither API-stamped nor a coordination child
	// waits for nothing.
	loose := freshChild.DeepCopy()
	loose.Name, loose.UID, loose.CreationTimestamp, loose.OwnerReferences = "loose", "loose-uid", metav1.Now(), nil
	if frozen, err := freezeRequesterConnections(context.Background(), reader, nil, loose, brokeredConfiguration("gh_search")); err != nil || !frozenWithoutConnection(frozen) {
		t.Fatalf("loose fresh task: frozen = %+v err = %v, want no wait", frozen, err)
	}
	if frozen, err := freezeRequesterConnections(context.Background(), reader, nil, sealed, brokeredConfiguration("gh_search")); err != nil || len(frozen) != 1 {
		t.Fatalf("sealed child: frozen = %+v err = %v", frozen, err)
	}
	if frozen, err := freezeRequesterConnections(context.Background(), reader, nil, impostor, brokeredConfiguration("gh_search")); err != nil || !frozenWithoutConnection(frozen) {
		t.Fatalf("child with a copied stamp and another requester: frozen = %+v err = %v", frozen, err)
	}
}

// The broker seals the children it creates for an authenticated ACP Task
// directly: the parent must carry a valid stamp, the child must be
// controller-owned by it with the same requester, and a concurrent write
// that fences the patch is retried against the re-read child.
func TestACPChildTaskSealerSealsOwnedChildren(t *testing.T) {
	SetRequesterStampKey(testRequesterStampKey)
	t.Cleanup(func() { SetRequesterStampKey(nil) })
	requester := &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "tenant", UID: "parent-uid", Annotations: map[string]string{
			labels.AnnotationRequestedBySource: labels.RequestedBySourceAPI,
			labels.AnnotationRequestedByStamp:  connectors.RequesterStamp(testRequesterStampKey, "parent-uid", requester.Issuer, requester.Subject),
		}},
		Spec: corev1alpha1.TaskSpec{RequestedBy: requester},
	}
	isController := true
	owned := func(name, uid string, by *corev1alpha1.RequestedBy) *corev1alpha1.Task {
		return &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant", UID: types.UID(uid), OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: parent.Name, UID: parent.UID, Controller: &isController,
			}}},
			Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, RequestedBy: by},
		}
	}
	child := owned("child", "child-uid", requester)
	impostor := owned("impostor", "impostor-uid", &corev1alpha1.RequestedBy{Issuer: requester.Issuer, Subject: "victim"})
	stranger := owned("stranger", "stranger-uid", requester)
	stranger.OwnerReferences[0].UID = "other-parent-uid"
	fenced := owned("fenced", "fenced-uid", requester)
	var fenceOnce atomic.Bool
	c := ctrlfake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(parent, child, impostor, stranger, fenced).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl ctrlclient.WithWatch, obj ctrlclient.Object, patch ctrlclient.Patch, opts ...ctrlclient.PatchOption) error {
				if task, ok := obj.(*corev1alpha1.Task); ok && task.Name == "fenced" && fenceOnce.CompareAndSwap(false, true) {
					return apierrors.NewConflict(corev1alpha1.GroupVersion.WithResource("tasks").GroupResource(), task.Name, errors.New("fenced"))
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	seal := ACPChildTaskSealer(c, parent.Namespace, parent.Name, string(parent.UID))
	ctx := context.Background()
	for _, task := range []*corev1alpha1.Task{child, fenced} {
		if err := seal(ctx, c, task.DeepCopy()); err != nil {
			t.Fatalf("%s: %v", task.Name, err)
		}
		sealed := &corev1alpha1.Task{}
		if err := c.Get(ctx, ctrlclient.ObjectKeyFromObject(task), sealed); err != nil {
			t.Fatal(err)
		}
		want := connectors.RequesterStamp(testRequesterStampKey, task.UID, requester.Issuer, requester.Subject)
		if sealed.Annotations[labels.AnnotationRequestedByStamp] != want || sealed.Annotations[labels.AnnotationRequestedBySource] != labels.RequestedBySourceAPI {
			t.Fatalf("%s annotations = %v, want a UID-bound stamp", task.Name, sealed.Annotations)
		}
		if !connectors.RequesterStampValid(testRequesterStampKey, sealed) {
			t.Fatalf("%s must verify after sealing", task.Name)
		}
	}
	if !fenceOnce.Load() {
		t.Fatal("the fenced child must have hit the conflict once")
	}
	for _, task := range []*corev1alpha1.Task{impostor, stranger} {
		if err := seal(ctx, c, task.DeepCopy()); !errors.Is(err, ErrChildSealRefused) {
			t.Fatalf("%s: err = %v, want refusal", task.Name, err)
		}
		unsealed := &corev1alpha1.Task{}
		if err := c.Get(ctx, ctrlclient.ObjectKeyFromObject(task), unsealed); err != nil || unsealed.Annotations[labels.AnnotationRequestedByStamp] != "" {
			t.Fatalf("%s must stay unsealed: %v %v", task.Name, unsealed.Annotations, err)
		}
	}
	// A parent whose identity changed, or one without a verified stamp,
	// seals nothing.
	if err := ACPChildTaskSealer(c, parent.Namespace, parent.Name, "other-uid")(ctx, c, child.DeepCopy()); !errors.Is(err, ErrChildSealRefused) {
		t.Fatalf("changed parent identity err = %v", err)
	}
	unverified := parent.DeepCopy()
	unverified.Annotations[labels.AnnotationRequestedByStamp] = "forged"
	if err := SealChildRequesterStamp(ctx, c, testRequesterStampKey, unverified, child.DeepCopy()); !errors.Is(err, ErrChildSealRefused) {
		t.Fatalf("unverified parent err = %v", err)
	}
	if err := SealChildRequesterStamp(ctx, c, nil, parent, child.DeepCopy()); err == nil || errors.Is(err, ErrChildSealRefused) {
		t.Fatalf("missing key err = %v, want a configuration error", err)
	}
}

// TestACPChildTaskSealerSkipsWhatItCannotSeal covers a seal requested
// while stamps are disabled, or for a child without a requester: there is
// nothing to seal, so the hook returns at once without reading the parent
// or retrying.
func TestACPChildTaskSealerSkipsWhatItCannotSeal(t *testing.T) {
	var reads atomic.Int32
	c := ctrlfake.NewClientBuilder().WithScheme(newTestScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
				reads.Add(1)
				return errors.New("apiserver unavailable")
			},
		}).Build()
	seal := ACPChildTaskSealer(c, "tenant", "parent", "parent-uid")
	requested := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "tenant", UID: "child-uid"},
		Spec:       corev1alpha1.TaskSpec{RequestedBy: &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}},
	}
	SetRequesterStampKey(nil)
	if err := seal(context.Background(), c, requested); err != nil {
		t.Fatalf("stamps disabled: err = %v", err)
	}
	SetRequesterStampKey(testRequesterStampKey)
	t.Cleanup(func() { SetRequesterStampKey(nil) })
	anonymous := requested.DeepCopy()
	anonymous.Spec.RequestedBy = nil
	if err := seal(context.Background(), c, anonymous); err != nil {
		t.Fatalf("no requester: err = %v", err)
	}
	if reads.Load() != 0 {
		t.Fatalf("reads = %d, want none", reads.Load())
	}
}

// TestHandleScheduledSealsRunsOfAVerifiedRequester covers a scheduled Task
// the API sealed for a person: each run acts for the same person, so the
// controller seals it against the scheduled Task right after creating it,
// and the run's binding neither waits out the grace window nor loses the
// person's Connections.
func TestHandleScheduledSealsRunsOfAVerifiedRequester(t *testing.T) {
	SetRequesterStampKey(testRequesterStampKey)
	t.Cleanup(func() { SetRequesterStampKey(nil) })
	requester := &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}
	lastSchedule := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	scheduled := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sched-person", Namespace: "default", UID: "sched-person-uid",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
			Annotations: map[string]string{
				labels.AnnotationRequestedBySource: labels.RequestedBySourceAPI,
				labels.AnnotationRequestedByStamp:  connectors.RequesterStamp(testRequesterStampKey, "sched-person-uid", requester.Issuer, requester.Subject),
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI, Prompt: "triage", Schedule: "* * * * *",
			StartingDeadlineSeconds: new(int64(300)), RequestedBy: requester,
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseScheduled, LastScheduleTime: &lastSchedule},
	}
	ctx := context.Background()
	r := newUnitReconciler(newTestScheme(), scheduled)
	// The API server assigns UIDs; the fake client does not.
	r.Client = interceptor.NewClient(r.Client.(ctrlclient.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, cl ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
			if obj.GetUID() == "" {
				obj.SetUID(types.UID(obj.GetName() + "-uid"))
			}
			return cl.Create(ctx, obj, opts...)
		},
	})
	if _, err := r.handleScheduled(ctx, scheduled); err != nil {
		t.Fatal(err)
	}
	var runs corev1alpha1.TaskList
	if err := r.List(ctx, &runs, ctrlclient.InNamespace("default"), ctrlclient.MatchingLabels{labels.LabelParentTask: labels.SelectorValue(scheduled.Name)}); err != nil {
		t.Fatal(err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs.Items))
	}
	if run := runs.Items[0]; !connectors.RequesterStampValid(testRequesterStampKey, &run) {
		t.Fatalf("run annotations = %v, want a stamp sealed for the run's own UID", run.Annotations)
	}
}

// frozenWithoutConnection reports a snapshot that froze exactly the
// connection-mode policy but bound no Connection to it.
func frozenWithoutConnection(frozen []agentExecutionSnapshotConnection) bool {
	return len(frozen) == 1 && frozen[0].PolicyName == "github-conn" && frozen[0].UID == "" && frozen[0].GrantSequence == 0
}

// TestACPChildTaskSealerRetriesTransientFailures covers a brokered child
// whose seal first fails on a transient parent read: nothing repairs a seal
// later, so the sealer retries and the child ends up sealed.
func TestACPChildTaskSealerRetriesTransientFailures(t *testing.T) {
	SetRequesterStampKey(testRequesterStampKey)
	t.Cleanup(func() { SetRequesterStampKey(nil) })
	previous := acpChildSealRetryBackoff
	acpChildSealRetryBackoff = time.Millisecond
	t.Cleanup(func() { acpChildSealRetryBackoff = previous })
	requester := &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "tenant", UID: "parent-uid", Annotations: map[string]string{
			labels.AnnotationRequestedBySource: labels.RequestedBySourceAPI,
			labels.AnnotationRequestedByStamp:  connectors.RequesterStamp(testRequesterStampKey, "parent-uid", requester.Issuer, requester.Subject),
		}},
		Spec: corev1alpha1.TaskSpec{RequestedBy: requester},
	}
	isController := true
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "tenant", UID: "child-uid", OwnerReferences: []metav1.OwnerReference{{
			APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: parent.Name, UID: parent.UID, Controller: &isController,
		}}},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, RequestedBy: requester},
	}
	var failed atomic.Bool
	c := ctrlfake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(parent, child).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
				if key.Name == "parent" && failed.CompareAndSwap(false, true) {
					return errors.New("apiserver unavailable")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	if err := ACPChildTaskSealer(c, parent.Namespace, parent.Name, string(parent.UID))(context.Background(), c, child.DeepCopy()); err != nil {
		t.Fatalf("seal after a transient failure: %v", err)
	}
	sealed := &corev1alpha1.Task{}
	if err := c.Get(context.Background(), ctrlclient.ObjectKeyFromObject(child), sealed); err != nil || !connectors.RequesterStampValid(testRequesterStampKey, sealed) {
		t.Fatalf("child = %v err = %v, want it sealed", sealed.Annotations, err)
	}
}

// TestConnectorToolsForWaitsForAMissingPolicy covers a Tool whose policy is
// absent while a binding is made: it cannot be classified, so the binding
// retries instead of treating the Tool as plain and later running it under a
// policy recreated in another mode.
func TestConnectorToolsForWaitsForAMissingPolicy(t *testing.T) {
	scheme := connectorTestScheme(t)
	tool, _, _, _ := freezeFixtures(true)
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tool).Build()
	if _, err := classifyConnectorTools(context.Background(), reader, nil, "tenant", []string{"gh_search"}, true); err == nil {
		t.Fatal("a Tool whose policy is missing must not be classified as a plain Tool")
	}
	// A Tool without any policy is plain, and an unknown Tool is skipped.
	plain := tool.(*corev1alpha1.Tool).DeepCopy()
	plain.Spec.HTTP.OutboundAccessPolicyRef = nil
	reader = ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(plain).Build()
	if infos, err := classifyConnectorTools(context.Background(), reader, nil, "tenant", []string{"gh_search", "unknown"}, true); err != nil || len(infos) != 0 {
		t.Fatalf("plain and unknown tools: infos = %v err = %v", infos, err)
	}
}

// TestBrokeredCustomCandidatesFollowDescriptorPrecedence covers a Tool CR
// that shares a name with a registered built-in or a provider-native tool:
// those take precedence and the CR is never exposed, so it cannot make the
// Task connector-backed.
func TestBrokeredCustomCandidatesFollowDescriptorPrecedence(t *testing.T) {
	native := acp.BuiltInRuntimeNativeToolNames("codex")
	if len(native) == 0 {
		t.Skip("no provider-native tools for codex")
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewWebSearchTool())
	got := brokeredCustomCandidates([]string{native[0], "web_search", "gh_search"}, "codex", registry)
	if len(got) != 1 || got[0] != "gh_search" {
		t.Fatalf("candidates = %v, want only the custom Tool", got)
	}
}
