/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/workspace"
)

const (
	substrateTestSnapshotLocation  = "gs://ate-snapshots/orka"
	substrateTestStatusSuspended   = "STATUS_SUSPENDED"
	substrateTestStatusSuspending  = "STATUS_SUSPENDING"
	substrateTestStatusCrashed     = "STATUS_CRASHED"
	substrateTestTemplateNamespace = "ate-demo"
	substrateTestBaseTemplateName  = "orka-codex-infra"
	substrateTestActorDNSSuffix    = "actors.test.example"
	substrateTestWorkerNamespace   = "ate-workers"
	substrateTestWorkerPoolName    = "orka-workers"
	substrateTestWorkerPodName     = "worker-0"
	substrateTestWorkerPodUID      = "worker-0-uid"
	substrateTestObjectNameField   = "name"
	substrateTestObjectImageField  = "image"
	substrateTestAttackerManagedBy = "attacker"
)

type fakeSubstrateActorControl struct {
	actors        map[string]*workspace.SubstrateRuntimeActor
	created       []string
	resumed       []string
	boots         []bool
	settled       []string
	dataSuspended []string
	deleted       []string
	closed        int
	afterCreate   func()
	afterResume   func(*workspace.SubstrateRuntimeActor)
	afterSettle   func(*workspace.SubstrateRuntimeActor)
}

type blockingSubstrateActorControl struct{}

func (blockingSubstrateActorControl) wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (c blockingSubstrateActorControl) GetActor(ctx context.Context, _ string) (*workspace.SubstrateRuntimeActor, error) {
	return nil, c.wait(ctx)
}

func (c blockingSubstrateActorControl) CreateActor(
	ctx context.Context,
	_, _, _ string,
) (*workspace.SubstrateRuntimeActor, error) {
	return nil, c.wait(ctx)
}

func (c blockingSubstrateActorControl) ResumeActor(
	ctx context.Context,
	_ string,
	_ bool,
) (*workspace.SubstrateRuntimeActor, error) {
	return nil, c.wait(ctx)
}

func (c blockingSubstrateActorControl) SettleActor(ctx context.Context, _ string) (*workspace.SubstrateRuntimeActor, error) {
	return nil, c.wait(ctx)
}

func (c blockingSubstrateActorControl) SuspendActorForDataCheckpoint(ctx context.Context, _ string) (*workspace.SubstrateRuntimeActor, error) {
	return nil, c.wait(ctx)
}

func (c blockingSubstrateActorControl) DeleteActor(ctx context.Context, _ string) error {
	return c.wait(ctx)
}

func (blockingSubstrateActorControl) Close() error {
	return nil
}

func TestSubstrateActorControlForCleanupAppliesClaimTimeout(t *testing.T) {
	const claimTimeout = 5 * time.Millisecond
	r := &RuntimePoolReconciler{
		SubstrateConfig: SubstrateConfig{ClaimTimeout: claimTimeout},
		SubstrateActorControlFactory: func(cfg SubstrateConfig) (workspace.SubstrateRuntimeActorControl, error) {
			if cfg.ClaimTimeout != claimTimeout {
				t.Fatalf("factory claim timeout = %s, want %s", cfg.ClaimTimeout, claimTimeout)
			}
			return blockingSubstrateActorControl{}, nil
		},
	}
	control, err := r.substrateActorControlForCleanup()
	if err != nil {
		t.Fatalf("create actor control: %v", err)
	}
	defer control.Close() //nolint:errcheck // fake close cannot fail

	tests := []struct {
		name string
		call func(context.Context) error
	}{
		{name: "GetActor", call: func(ctx context.Context) error {
			_, callErr := control.GetActor(ctx, "actor")
			return callErr
		}},
		{name: "CreateActor", call: func(ctx context.Context) error {
			_, callErr := control.CreateActor(ctx, "actor", "namespace", "template")
			return callErr
		}},
		{name: "ResumeActor", call: func(ctx context.Context) error {
			_, callErr := control.ResumeActor(ctx, "actor", true)
			return callErr
		}},
		{name: "SettleActor", call: func(ctx context.Context) error {
			_, callErr := control.SettleActor(ctx, "actor")
			return callErr
		}},
		{name: "DeleteActor", call: func(ctx context.Context) error {
			return control.DeleteActor(ctx, "actor")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("actor control error = %v, want context deadline exceeded", err)
			}
		})
	}
}

func newFakeSubstrateActorControl() *fakeSubstrateActorControl {
	return &fakeSubstrateActorControl{actors: map[string]*workspace.SubstrateRuntimeActor{}}
}

func (f *fakeSubstrateActorControl) GetActor(_ context.Context, actorID string) (*workspace.SubstrateRuntimeActor, error) {
	actor, ok := f.actors[actorID]
	if !ok {
		return nil, nil
	}
	view := *actor
	return &view, nil
}

func (f *fakeSubstrateActorControl) CreateActor(_ context.Context, actorID, templateNamespace, templateName string) (*workspace.SubstrateRuntimeActor, error) {
	f.created = append(f.created, actorID)
	actor := &workspace.SubstrateRuntimeActor{
		ActorID: actorID, TemplateNamespace: templateNamespace, TemplateName: templateName,
		Status: substrateTestStatusSuspended,
	}
	f.actors[actorID] = actor
	if f.afterCreate != nil {
		f.afterCreate()
	}
	view := *actor
	return &view, nil
}

func (f *fakeSubstrateActorControl) ResumeActor(_ context.Context, actorID string, boot bool) (*workspace.SubstrateRuntimeActor, error) {
	f.resumed = append(f.resumed, actorID)
	f.boots = append(f.boots, boot)
	actor := f.actors[actorID]
	if actor == nil {
		actor = &workspace.SubstrateRuntimeActor{ActorID: actorID}
		f.actors[actorID] = actor
	}
	actor.Status = "STATUS_RUNNING"
	actor.PodNamespace = substrateTestWorkerNamespace
	actor.PodName = substrateTestWorkerPodName
	actor.PodIP = "10.99.0.5"
	if f.afterResume != nil {
		f.afterResume(actor)
	}
	view := *actor
	return &view, nil
}

func (f *fakeSubstrateActorControl) SettleActor(_ context.Context, actorID string) (*workspace.SubstrateRuntimeActor, error) {
	f.settled = append(f.settled, actorID)
	actor, ok := f.actors[actorID]
	if !ok {
		return nil, fmt.Errorf("settle: actor %s not found", actorID)
	}
	actor.Status = substrateTestStatusSuspended
	if f.afterSettle != nil {
		f.afterSettle(actor)
	}
	view := *actor
	return &view, nil
}

func (f *fakeSubstrateActorControl) SuspendActorForDataCheckpoint(_ context.Context, actorID string) (*workspace.SubstrateRuntimeActor, error) {
	f.dataSuspended = append(f.dataSuspended, actorID)
	actor, ok := f.actors[actorID]
	if !ok {
		return nil, fmt.Errorf("suspend: actor %s not found", actorID)
	}
	actor.Status = substrateTestStatusSuspended
	actor.PodNamespace = ""
	actor.PodName = ""
	actor.PodIP = ""
	actor.SnapshotObserved = true
	view := *actor
	return &view, nil
}

func (f *fakeSubstrateActorControl) DeleteActor(_ context.Context, actorID string) error {
	if actor, ok := f.actors[actorID]; ok && actor.Status != substrateTestStatusSuspended && actor.Status != substrateTestStatusCrashed {
		// Mirror the provider: only suspended (settled) actors are deletable.
		return fmt.Errorf("FailedPrecondition: Actor %s is not suspended (status: %s)", actorID, actor.Status)
	}
	f.deleted = append(f.deleted, actorID)
	delete(f.actors, actorID)
	return nil
}

func (f *fakeSubstrateActorControl) Close() error {
	f.closed++
	return nil
}

type substrateTemplateUIDClient struct {
	client.Client
	next int
}

func (c *substrateTemplateUIDClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if template, ok := obj.(*unstructured.Unstructured); ok &&
		template.GroupVersionKind() == substrateActorTemplateGVK && template.GetUID() == "" {
		c.next++
		template.SetUID(types.UID(fmt.Sprintf("test-substrate-template-%d", c.next)))
	}
	return c.Client.Create(ctx, obj, opts...)
}

type substrateTemplateErrorReader struct {
	client.Reader
	err error
}

func (r *substrateTemplateErrorReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if object.GetObjectKind().GroupVersionKind() == substrateActorTemplateGVK {
		return r.err
	}
	return r.Reader.Get(ctx, key, object, options...)
}

type substrateNamespaceScopedNetworkPolicyReader struct {
	client.Reader
}

func (r *substrateNamespaceScopedNetworkPolicyReader) List(
	ctx context.Context,
	list client.ObjectList,
	options ...client.ListOption,
) error {
	if _, ok := list.(*networkingv1.NetworkPolicyList); ok {
		applied := (&client.ListOptions{}).ApplyOptions(options)
		if applied.Namespace == "" {
			return errors.New("cluster-wide NetworkPolicy list is forbidden")
		}
	}
	return r.Reader.List(ctx, list, options...)
}

func runtimePoolSubstrateTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtimePoolTestScheme(t)
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add workspace scheme: %v", err)
	}
	scheme.AddKnownTypeWithName(substrateActorTemplateGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(substrateActorTemplateGVK.GroupVersion().WithKind("ActorTemplateList"), &unstructured.UnstructuredList{})
	metav1.AddToGroupVersion(scheme, substrateActorTemplateGVK.GroupVersion())
	return scheme
}

func runtimePoolSubstrateTestObject() *corev1alpha1.RuntimePool {
	pool := runtimePoolWorkspaceTestObject()
	pool.Name = "acp-ws-codex-fedcba9876543210"
	pool.Spec.ExecutionWorkspace.Provider = corev1alpha1.WorkspaceProviderSubstrate
	pool.Spec.ExecutionWorkspace.Substrate = &corev1alpha1.RuntimePoolSubstrateWorkspaceSpec{
		BaseTemplateNamespace: substrateTestTemplateNamespace,
		BaseTemplateName:      substrateTestBaseTemplateName,
	}
	return pool
}

func substrateTestBaseTemplate() *unstructured.Unstructured {
	template := &unstructured.Unstructured{Object: map[string]any{
		substrateObjectSpecField: map[string]any{
			"workerPoolRef":   map[string]any{"namespace": substrateTestWorkerNamespace, substrateTestObjectNameField: substrateTestWorkerPoolName},
			"snapshotsConfig": map[string]any{"location": substrateTestSnapshotLocation},
			"runsc":           map[string]any{"amd64": map[string]any{"url": "https://example.invalid/runsc"}},
			"containers": []any{map[string]any{
				substrateTestObjectNameField: "operator-base", substrateTestObjectImageField: "example.com/operator@sha256:" + strings.Repeat("1", 64),
			}},
		},
	}}
	template.SetGroupVersionKind(substrateActorTemplateGVK)
	template.SetNamespace(substrateTestTemplateNamespace)
	template.SetName(substrateTestBaseTemplateName)
	return template
}

func runtimePoolSubstrateTestReconciler(
	t *testing.T,
	supervisor RuntimePoolSupervisorClient,
	control workspace.SubstrateRuntimeActorControl,
) (*RuntimePoolReconciler, *corev1alpha1.RuntimePool) {
	t.Helper()
	scheme := runtimePoolSubstrateTestScheme(t)
	pool := runtimePoolSubstrateTestObject()
	worker := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: substrateTestWorkerNamespace,
		Name:      substrateTestWorkerPodName,
		UID:       substrateTestWorkerPodUID,
		Labels:    map[string]string{substrateWorkerPoolLabel: substrateTestWorkerPoolName},
	}}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool, substrateTestBaseTemplate(), worker)
	r.Client = &substrateTemplateUIDClient{Client: r.Client}
	r.SubstrateEnabled = true
	r.SubstrateConfig = SubstrateConfig{
		APIEndpoint:           "api.ate-system.svc:443",
		APIInsecureSkipVerify: true,
		RouterURL:             defaultSubstrateRouterURL,
		ActorDNSSuffix:        substrateTestActorDNSSuffix,
	}
	r.SubstrateActorControlFactory = func(SubstrateConfig) (workspace.SubstrateRuntimeActorControl, error) {
		return control, nil
	}
	r.SubstrateCredentialSeeder = func(_ context.Context, routeHost, nonce string, signingSeed []byte, request harnessv2.CredentialBootstrapRequest) error {
		if routeHost == "" || nonce == "" {
			return fmt.Errorf("seeder called without route host or nonce")
		}
		if len(signingSeed) < harnessv2.MinCapabilitySecretBytes {
			return fmt.Errorf("seeder called without a valid signing seed")
		}
		if err := request.Validate(); err != nil {
			return err
		}
		return nil
	}
	return r, pool
}

func substrateTestActorID(pool *corev1alpha1.RuntimePool) string {
	return runtimePoolSubstrateActorID(runtimePoolResourceName(pool.Namespace, pool.Name))
}

func substrateTestRouteHost(pool *corev1alpha1.RuntimePool) string {
	return substrateActorRouteHost(substrateTestActorID(pool), substrateTestActorDNSSuffix)
}

// substrateTestProbePod is the fixture identity the supervisor would advertise:
// an opaque Orka instance UID with the route host as its address.
func substrateTestProbePod(pool *corev1alpha1.RuntimePool) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID(substrateActorInstanceUID(substrateTestActorID(pool)))},
		Status:     corev1.PodStatus{PodIP: substrateTestRouteHost(pool)},
	}
}

func substrateTestDerivedTemplate(t *testing.T, r *RuntimePoolReconciler, pool *corev1alpha1.RuntimePool) *unstructured.Unstructured {
	t.Helper()
	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(substrateActorTemplateGVK)
	err := r.Get(context.Background(), types.NamespacedName{
		Namespace: substrateTestTemplateNamespace,
		Name:      runtimePoolSubstrateTemplateName(runtimePoolResourceName(pool.Namespace, pool.Name)),
	}, template)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("Get derived ActorTemplate: %v", err)
	}
	return template
}

//nolint:gocyclo // The materialization, fencing, and network-policy invariants form one end-to-end scenario.
func TestSubstrateRuntimePoolMaterializesDerivedTemplateAndActor(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	policiesAtActorCreation := 0
	control.afterCreate = func() {
		var policies networkingv1.NetworkPolicyList
		if err := r.List(context.Background(), &policies, client.MatchingLabels{
			runtimePoolKeyLabel: runtimePoolKey(pool.Namespace, pool.Name),
		}); err != nil {
			t.Fatalf("list Substrate RuntimePool NetworkPolicies during Actor creation: %v", err)
		}
		policiesAtActorCreation = len(policies.Items)
	}

	runtimePoolReconcile(t, r, pool)

	actorID := substrateTestActorID(pool)
	if len(control.created) != 1 || control.created[0] != actorID {
		t.Fatalf("created actors = %v, want exactly %q", control.created, actorID)
	}
	if len(control.resumed) != 1 || len(control.boots) != 1 || !control.boots[0] {
		t.Fatalf("resume calls = %v boots = %v, want one fresh boot", control.resumed, control.boots)
	}
	if policiesAtActorCreation != 5 {
		t.Fatalf("NetworkPolicies present at Actor creation = %d, want 5", policiesAtActorCreation)
	}

	derived := substrateTestDerivedTemplate(t, r, pool)
	if derived == nil {
		t.Fatal("derived ActorTemplate was not created")
	}
	assertSubstrateDerivedTemplate(t, r, pool, derived, actorID)

	var deployment appsv1.Deployment
	base := runtimePoolResourceName(pool.Namespace, pool.Name)
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: pool.Namespace, Name: base}, &deployment); !apierrors.IsNotFound(err) {
		t.Fatalf("substrate-backed pool created a Deployment (err=%v); the provider owns the workload", err)
	}

	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStarting || got.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
		t.Fatalf("status = %s/%s, want Starting/Closed", got.Status.Lifecycle, got.Status.AdmissionState)
	}
	if got.Annotations[substrateActorBootedAnnotation] != actorID {
		t.Fatalf("booted annotation = %q, want %q", got.Annotations[substrateActorBootedAnnotation], actorID)
	}
	if got.Annotations[substrateActorTemplateFenceAnnotation] == "" {
		t.Fatal("validated ActorTemplate UID/resourceVersion fence was not recorded before actor creation")
	}
	workerPodFence, err := substrateRuntimePoolWorkerPodFenceFromAnnotation(&got)
	if err != nil {
		t.Fatalf("read exact worker Pod fence: %v", err)
	}
	if workerPodFence == nil || workerPodFence.ActorID != actorID ||
		workerPodFence.Namespace != substrateTestWorkerNamespace || workerPodFence.Name != substrateTestWorkerPodName ||
		workerPodFence.UID != substrateTestWorkerPodUID {
		t.Fatalf("exact worker Pod fence = %#v, want actor %q Pod %s/worker-0 UID worker-0-uid", workerPodFence, actorID, substrateTestWorkerNamespace)
	}
	var policies networkingv1.NetworkPolicyList
	if err := r.List(context.Background(), &policies, client.InNamespace(substrateTestWorkerNamespace), client.MatchingLabels{
		runtimePoolKeyLabel: runtimePoolKey(pool.Namespace, pool.Name),
	}); err != nil {
		t.Fatalf("list Substrate RuntimePool NetworkPolicies: %v", err)
	}
	if len(policies.Items) != 4 {
		t.Fatalf("Substrate RuntimePool NetworkPolicy count = %d, want 4", len(policies.Items))
	}
	for i := range policies.Items {
		policy := policies.Items[i]
		if policy.Spec.PodSelector.MatchLabels[substrateWorkerPoolLabel] != substrateTestWorkerPoolName {
			t.Fatalf("NetworkPolicy %q selector = %#v, want exact WorkerPool label", policy.Name, policy.Spec.PodSelector)
		}
		if len(policy.Spec.PodSelector.MatchLabels) != 1 {
			t.Fatalf("NetworkPolicy %q selector = %#v, want replacement-safe WorkerPool-only selection", policy.Name, policy.Spec.PodSelector)
		}
		if len(policy.Spec.PolicyTypes) != 1 || policy.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress || len(policy.Spec.Ingress) != 0 {
			t.Fatalf("NetworkPolicy %q types/ingress = %v/%v, want egress-only confinement", policy.Name, policy.Spec.PolicyTypes, policy.Spec.Ingress)
		}
	}
	providerIngress := &networkingv1.NetworkPolicy{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Namespace: runtimePoolDefaultControllerNamespace,
		Name:      runtimePoolChildName(runtimePoolResourceName(pool.Namespace, pool.Name), runtimePoolSubstrateProviderIngressSuffix),
	}, providerIngress); err != nil {
		t.Fatalf("get provider proxy ingress NetworkPolicy: %v", err)
	}
	if !apiequality.Semantic.DeepEqual(providerIngress.Spec.PodSelector.MatchLabels, r.ProviderProxy.PodLabels) {
		t.Fatalf("provider ingress selector = %#v, want %#v", providerIngress.Spec.PodSelector.MatchLabels, r.ProviderProxy.PodLabels)
	}
	if len(providerIngress.Spec.PolicyTypes) != 1 || providerIngress.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress ||
		len(providerIngress.Spec.Ingress) != 1 || len(providerIngress.Spec.Ingress[0].From) != 1 ||
		len(providerIngress.Spec.Ingress[0].Ports) != 1 {
		t.Fatalf("provider ingress policy = %#v, want one ingress rule from the worker pool", providerIngress.Spec)
	}
	from := providerIngress.Spec.Ingress[0].From[0]
	if from.NamespaceSelector == nil || from.NamespaceSelector.MatchLabels[corev1.LabelMetadataName] != substrateTestWorkerNamespace ||
		from.PodSelector == nil || from.PodSelector.MatchLabels[substrateWorkerPoolLabel] != substrateTestWorkerPoolName {
		t.Fatalf("provider ingress source = %#v, want worker pool %s/%s", from, substrateTestWorkerNamespace, substrateTestWorkerPoolName)
	}
	port := providerIngress.Spec.Ingress[0].Ports[0]
	if port.Protocol == nil || *port.Protocol != corev1.ProtocolTCP || port.Port == nil || port.Port.IntVal != 8080 {
		t.Fatalf("provider ingress port = %#v, want TCP/8080", port)
	}
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)
}

func TestSubstrateRuntimePoolRejectsRacedForeignActorBeforeResume(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	actorID := substrateTestActorID(pool)
	control.afterCreate = func() {
		control.actors[actorID].TemplateNamespace = "attacker-owned"
		control.actors[actorID].TemplateName = "credential-capture"
	}

	runtimePoolReconcile(t, r, pool)

	if len(control.created) != 1 || control.created[0] != actorID {
		t.Fatalf("created actors = %v, want exactly %q", control.created, actorID)
	}
	if len(control.resumed) != 0 || len(control.boots) != 0 {
		t.Fatalf("raced foreign actor was resumed: actors=%v boots=%v", control.resumed, control.boots)
	}
	if len(control.settled) != 0 || len(control.deleted) != 0 {
		t.Fatalf("raced foreign actor was modified: settled=%v deleted=%v", control.settled, control.deleted)
	}
	if supervisor.probeCalls != 0 {
		t.Fatalf("raced foreign actor reached authenticated probe: %d calls", supervisor.probeCalls)
	}
	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		got.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed ||
		!strings.Contains(got.Status.Message, "refusing to resume the foreign actor") {
		t.Fatalf("raced foreign actor status = %s/%s %q, want Degraded/Closed identity rejection", got.Status.Lifecycle, got.Status.AdmissionState, got.Status.Message)
	}
}

func TestSubstrateRuntimePoolReplacesPredictableUnboundAuthSecret(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	cfg, err := r.runtimePoolConfig(pool)
	if err != nil {
		t.Fatalf("runtimePoolConfig() error = %v", err)
	}
	epoch := fmt.Sprintf("%d", cfg.controllerEpoch)
	forgedName := runtimePoolChildName(cfg.baseName, "auth-e"+epoch)
	immutable := true
	forged := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      forgedName,
			Namespace: cfg.namespace,
			Labels: mergeStringMap(cloneStringMap(cfg.labels), map[string]string{
				runtimePoolAuthLabel:            scheduledRunLabelValue,
				runtimePoolCredentialEpochLabel: epoch,
			}),
		},
		Immutable: &immutable,
		Data: map[string][]byte{
			runtimePoolControllerTokenKey:      []byte(strings.Repeat("a", 32)),
			runtimePoolCapabilitySecretKey:     []byte(strings.Repeat("b", 32)),
			runtimePoolBootstrapNonceKey:       []byte(strings.Repeat("c", 32)),
			runtimePoolBootstrapSigningSeedKey: []byte(strings.Repeat("d", 32)),
		},
	}
	if err := controllerutil.SetControllerReference(pool, forged, r.Scheme); err != nil {
		t.Fatalf("set forged Secret owner: %v", err)
	}
	if err := r.Create(context.Background(), forged); err != nil {
		t.Fatalf("create forged predictable auth Secret: %v", err)
	}

	runtimePoolReconcile(t, r, pool)

	if err := r.Get(context.Background(), client.ObjectKeyFromObject(forged), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("forged predictable auth Secret survived reconciliation: %v", err)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	binding := current.Annotations[runtimePoolPrivateAuthSecretBindingAnnotation(cfg.controllerEpoch)]
	boundName, boundUID, err := parseRuntimePoolPrivateSecretBinding(binding)
	if err != nil {
		t.Fatalf("parse private auth binding %q: %v", binding, err)
	}
	if boundName == forgedName {
		t.Fatalf("private auth binding retained predictable Secret name %q", boundName)
	}
	bound := &corev1.Secret{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: cfg.namespace, Name: boundName}, bound); err != nil {
		t.Fatalf("get bound private auth Secret: %v", err)
	}
	if bound.UID != boundUID {
		t.Fatalf("bound private auth Secret UID = %q, want %q", bound.UID, boundUID)
	}
	if string(bound.Data[runtimePoolControllerTokenKey]) == strings.Repeat("a", 32) ||
		string(bound.Data[runtimePoolCapabilitySecretKey]) == strings.Repeat("b", 32) ||
		string(bound.Data[runtimePoolBootstrapNonceKey]) == strings.Repeat("c", 32) ||
		string(bound.Data[runtimePoolBootstrapSigningSeedKey]) == strings.Repeat("d", 32) {
		t.Fatal("bound private auth Secret retained attacker-selected credential bytes")
	}
	derived := substrateTestDerivedTemplate(t, r, pool)
	deployed, err := substrateTemplatePodTemplateSpec(derived)
	if err != nil {
		t.Fatalf("read deployed substrate template: %v", err)
	}
	resolved, err := r.substrateTemplateAuthSecret(context.Background(), &current, cfg, deployed)
	if err != nil {
		t.Fatalf("resolve deployed private auth Secret: %v", err)
	}
	if resolved.Name != boundName || resolved.UID != boundUID {
		t.Fatalf("resolved deployed auth Secret = %s/%s, want %s/%s", resolved.Name, resolved.UID, boundName, boundUID)
	}
}

func TestSubstrateRuntimePoolRejectsTemplateMutateAndRestoreDuringActorCreation(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	seedAttempts := 0
	r.SubstrateCredentialSeeder = func(context.Context, string, string, []byte, harnessv2.CredentialBootstrapRequest) error {
		seedAttempts++
		return nil
	}
	control.afterCreate = func() {
		derived := substrateTestDerivedTemplate(t, r, pool)
		containers, found, err := unstructured.NestedSlice(derived.Object, "spec", "containers")
		if err != nil || !found || len(containers) != 1 {
			t.Fatalf("read derived containers before mutate-and-restore: found=%v err=%v", found, err)
		}
		container := containers[0].(map[string]any)
		originalImage := container[substrateTestObjectImageField]
		container[substrateTestObjectImageField] = runtimePoolTestTamperedImage
		containers[0] = container
		if err := unstructured.SetNestedSlice(derived.Object, containers, "spec", "containers"); err != nil {
			t.Fatalf("mutate derived ActorTemplate: %v", err)
		}
		if err := r.Update(context.Background(), derived); err != nil {
			t.Fatalf("persist mutated derived ActorTemplate: %v", err)
		}

		restored := substrateTestDerivedTemplate(t, r, pool)
		containers, found, err = unstructured.NestedSlice(restored.Object, "spec", "containers")
		if err != nil || !found || len(containers) != 1 {
			t.Fatalf("read derived containers before restore: found=%v err=%v", found, err)
		}
		container = containers[0].(map[string]any)
		container[substrateTestObjectImageField] = originalImage
		containers[0] = container
		if err := unstructured.SetNestedSlice(restored.Object, containers, "spec", "containers"); err != nil {
			t.Fatalf("restore derived ActorTemplate: %v", err)
		}
		if err := r.Update(context.Background(), restored); err != nil {
			t.Fatalf("persist restored derived ActorTemplate: %v", err)
		}
	}

	runtimePoolReconcile(t, r, pool)

	if len(control.created) != 1 || len(control.resumed) != 0 {
		t.Fatalf("actor activity after template race = created:%v resumed:%v, want create followed by rejection before boot", control.created, control.resumed)
	}
	if seedAttempts != 0 || supervisor.probeCalls != 0 {
		t.Fatalf("template race reached credentials or probe: seeds=%d probes=%d", seedAttempts, supervisor.probeCalls)
	}
	if len(control.deleted) != 1 {
		t.Fatalf("deleted actors = %v, want raced actor recycled", control.deleted)
	}
	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		!strings.Contains(got.Status.Message, "changed during actor creation") {
		t.Fatalf("status = %s/%q, want template-fence rejection", got.Status.Lifecycle, got.Status.Message)
	}
	if got.Annotations[substrateActorTemplateFenceAnnotation] != "" {
		t.Fatal("template fence survived recycle of the raced actor")
	}
	if _, err := substrateRuntimeTemplateIntegrity(substrateTestDerivedTemplate(t, r, pool)); err != nil {
		t.Fatalf("restored template integrity = %v, want content restored while resourceVersion still exposes the race", err)
	}
}

func TestSubstrateRuntimePoolRejectsForeignWorkerEgressPolicy(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	seedAttempts := 0
	r.SubstrateCredentialSeeder = func(context.Context, string, string, []byte, harnessv2.CredentialBootstrapRequest) error {
		seedAttempts++
		return nil
	}
	base := runtimePoolResourceName(pool.Namespace, pool.Name)
	foreign := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: runtimePoolChildName(base, runtimePoolSubstrateDenyEgressSuffix), Namespace: substrateTestWorkerNamespace,
	}}
	if err := r.Create(context.Background(), foreign); err != nil {
		t.Fatalf("create foreign NetworkPolicy: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)

	if len(control.created) != 0 || len(control.resumed) != 0 {
		t.Fatalf("foreign policy reached Actor materialization: created:%v resumed:%v", control.created, control.resumed)
	}
	if seedAttempts != 0 || supervisor.probeCalls != 0 {
		t.Fatalf("foreign egress policy reached credentials or probe: seeds=%d probes=%d", seedAttempts, supervisor.probeCalls)
	}
	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		!strings.Contains(got.Status.Message, "NetworkPolicy") ||
		!strings.Contains(got.Status.Message, "ownership identity") {
		t.Fatalf("status = %s/%q, want fail-closed foreign NetworkPolicy rejection", got.Status.Lifecycle, got.Status.Message)
	}
}

func TestSubstrateRuntimePoolWaitsAfterNetworkPolicyRepairBeforeCredentialSeed(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	seedAttempts := 0
	r.SubstrateCredentialSeeder = func(context.Context, string, string, []byte, harnessv2.CredentialBootstrapRequest) error {
		seedAttempts++
		return nil
	}

	runtimePoolReconcile(t, r, pool)
	if seedAttempts != 0 {
		t.Fatalf("credential seed attempts during initial Actor boot = %d, want 0", seedAttempts)
	}
	base := runtimePoolResourceName(pool.Namespace, pool.Name)
	policy := &networkingv1.NetworkPolicy{}
	key := types.NamespacedName{
		Namespace: substrateTestWorkerNamespace,
		Name:      runtimePoolChildName(base, runtimePoolSubstrateDenyEgressSuffix),
	}
	if err := r.Get(context.Background(), key, policy); err != nil {
		t.Fatalf("get RuntimePool default-deny policy: %v", err)
	}
	policy.Spec.PodSelector.MatchLabels[substrateWorkerPoolLabel] = "drifted-worker-pool"
	if err := r.Update(context.Background(), policy); err != nil {
		t.Fatalf("drift RuntimePool default-deny policy: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	if seedAttempts != 0 || supervisor.probeCalls != 0 {
		t.Fatalf("policy repair reached credentials or probe: seeds=%d probes=%d", seedAttempts, supervisor.probeCalls)
	}
	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStarting ||
		got.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed ||
		!strings.Contains(got.Status.Message, "egress confinement") {
		t.Fatalf("status = %s/%s %q, want Starting/Closed confinement barrier", got.Status.Lifecycle, got.Status.AdmissionState, got.Status.Message)
	}

	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)
	if seedAttempts != 1 {
		t.Fatalf("credential seed attempts after confinement barrier = %d, want 1", seedAttempts)
	}
}

func TestSubstrateRuntimePoolFinalizerRejectsNetworkPolicyLabelDrift(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)

	current := runtimePoolTestGetPool(t, r, pool)
	cfg, err := r.runtimePoolConfig(&current)
	if err != nil {
		t.Fatalf("runtimePoolConfig: %v", err)
	}
	name := runtimePoolChildName(cfg.baseName, runtimePoolSubstrateDenyEgressSuffix)
	policy := &networkingv1.NetworkPolicy{}
	key := types.NamespacedName{Namespace: substrateTestWorkerNamespace, Name: name}
	if err := r.Get(context.Background(), key, policy); err != nil {
		t.Fatalf("get RuntimePool NetworkPolicy: %v", err)
	}
	base := policy.DeepCopy()
	policy.Labels[runtimePoolUIDLabel] = "drifted-owner"
	if err := r.Patch(context.Background(), policy, client.MergeFrom(base)); err != nil {
		t.Fatalf("drift RuntimePool NetworkPolicy labels: %v", err)
	}

	remaining, err := r.deleteSubstrateRuntimePoolNetworkPolicies(context.Background(), &current, cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "refusing to delete foreign") {
		t.Fatalf("cleanup after ownership-label drift = remaining:%t err:%v, want fail-closed rejection", remaining, err)
	}
	if err := r.Get(context.Background(), key, &networkingv1.NetworkPolicy{}); err != nil {
		t.Fatalf("label-drifted NetworkPolicy was deleted: %v", err)
	}
}

func TestSubstrateRuntimePoolDisabledAdmissionPreservesActiveInstance(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)
	serving := runtimePoolTestGetPool(t, r, pool)
	if serving.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || serving.Status.ActiveInstance == nil {
		t.Fatalf("precondition status = %s active=%v, want Serving active instance", serving.Status.Lifecycle, serving.Status.ActiveInstance)
	}
	wantRuntimeInstanceID := serving.Status.ActiveInstance.RuntimeInstanceID

	r.SubstrateEnabled = false
	runtimePoolReconcile(t, r, pool)
	disabled := runtimePoolTestGetPool(t, r, pool)
	if disabled.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing ||
		disabled.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed ||
		disabled.Status.ActiveInstance == nil ||
		disabled.Status.ActiveInstance.RuntimeInstanceID != wantRuntimeInstanceID {
		t.Fatalf("disabled status = %s/%s active=%#v, want serving instance preserved with admission closed", disabled.Status.Lifecycle, disabled.Status.AdmissionState, disabled.Status.ActiveInstance)
	}
	if len(control.deleted) != 0 {
		t.Fatalf("disabling admission deleted active Actor: %v", control.deleted)
	}
}

func TestSubstrateRuntimePoolRolloutRecyclesUnadmittedRunningActorWithoutProbe(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	r.ControllerEpoch++
	runtimePoolReconcile(t, r, pool)

	if supervisor.probeCalls != 0 {
		t.Fatalf("unadmitted running Actor received %d authenticated rollout probes", supervisor.probeCalls)
	}
	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopping || got.Status.ActiveInstance != nil {
		t.Fatalf("rollout status = %s active=%v, want Stopping with no active instance", got.Status.Lifecycle, got.Status.ActiveInstance)
	}
	if got.Annotations[substrateActorRecyclingAnnotation] != substrateTestActorID(pool) {
		t.Fatalf("recycling annotation = %q, want actor %q", got.Annotations[substrateActorRecyclingAnnotation], substrateTestActorID(pool))
	}
}

func TestSubstrateRuntimePoolScaleToZeroRecyclesUnadmittedRunningActorWithoutProbe(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	current.Spec.DesiredReplicas = 0
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("scale unadmitted Substrate pool to zero: %v", err)
	}
	runtimePoolReconcile(t, r, pool)

	if supervisor.probeCalls != 0 {
		t.Fatalf("unadmitted running Actor received %d authenticated scale-down probes", supervisor.probeCalls)
	}
	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopping || got.Status.ActiveInstance != nil {
		t.Fatalf("scale-down status = %s active=%v, want Stopping with no active instance", got.Status.Lifecycle, got.Status.ActiveInstance)
	}
	if got.Annotations[substrateActorRecyclingAnnotation] != substrateTestActorID(pool) {
		t.Fatalf("recycling annotation = %q, want actor %q", got.Annotations[substrateActorRecyclingAnnotation], substrateTestActorID(pool))
	}
}

func TestSubstrateRuntimePoolScaleToZeroRecyclesCrashedActiveActor(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)
	actorID := substrateTestActorID(pool)
	control.actors[actorID].Status = substrateTestStatusCrashed

	current := runtimePoolTestGetPool(t, r, pool)
	current.Spec.DesiredReplicas = 0
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("scale crashed Substrate pool to zero: %v", err)
	}
	supervisor.probeCalls = 0
	runtimePoolReconcile(t, r, pool)

	if supervisor.probeCalls != 0 {
		t.Fatalf("crashed Actor received %d authenticated scale-down probes", supervisor.probeCalls)
	}
	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopping || got.Status.ActiveInstance != nil {
		t.Fatalf("crashed scale-down status = %s active=%v, want Stopping with no active instance", got.Status.Lifecycle, got.Status.ActiveInstance)
	}
	if got.Annotations[substrateActorRecyclingAnnotation] != actorID {
		t.Fatalf("recycling annotation = %q, want actor %q", got.Annotations[substrateActorRecyclingAnnotation], actorID)
	}

	for range 6 {
		runtimePoolReconcile(t, r, pool)
	}
	if len(control.deleted) != 1 || control.deleted[0] != actorID {
		t.Fatalf("deleted actors = %v, want crashed actor %q", control.deleted, actorID)
	}
	got = runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped || got.Status.ActiveInstance != nil {
		t.Fatalf("final crashed scale-down status = %s active=%v, want Stopped with no active instance", got.Status.Lifecycle, got.Status.ActiveInstance)
	}
}

func TestSubstrateRuntimePoolRecyclesCrashedServingActorBeforeReplacement(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-crashed-serving", false)
	runtimePoolReconcile(t, r, pool)

	actorID := substrateTestActorID(pool)
	oldAuth := runtimePoolTestPrivateAuthSecret(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	bindingKey := runtimePoolPrivateAuthSecretBindingAnnotation(7)
	oldBinding := current.Annotations[bindingKey]
	createdBeforeRecycle := len(control.created)
	control.actors[actorID].Status = substrateTestStatusCrashed
	supervisor.probeCalls = 0

	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if supervisor.probeCalls != 0 {
		t.Fatalf("crashed serving Actor received %d authenticated probes", supervisor.probeCalls)
	}
	if current.Annotations[substrateActorRecyclingAnnotation] != actorID {
		t.Fatalf("recycling annotation = %q, want crashed actor %q", current.Annotations[substrateActorRecyclingAnnotation], actorID)
	}
	if current.Annotations[bindingKey] != oldBinding {
		t.Fatal("crashed Actor credentials rotated before exact workload teardown")
	}
	if !strings.Contains(current.Status.Message, "crashed") {
		t.Fatalf("crashed Actor status message = %q", current.Status.Message)
	}

	for range 8 {
		runtimePoolReconcile(t, r, pool)
		current = runtimePoolTestGetPool(t, r, pool)
		if control.actors[actorID] == nil && !substrateActorWorkloadProofRequired(&current, actorID) {
			break
		}
	}
	if control.actors[actorID] != nil || substrateActorWorkloadProofRequired(&current, actorID) {
		t.Fatalf("crashed Actor teardown did not prove workload absence: actor=%v annotations=%v", control.actors[actorID], current.Annotations)
	}
	if current.Annotations[bindingKey] != oldBinding {
		t.Fatal("crashed Actor credentials rotated before staged teardown completed")
	}
	if len(control.created) != createdBeforeRecycle {
		t.Fatal("replacement Actor was created during crashed Actor teardown")
	}

	r.Rand = &runtimePoolTestEntropyReader{next: 100}
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if strings.TrimSpace(current.Annotations[bindingKey]) != "" {
		t.Fatal("crashed Actor auth Secret remained published after teardown")
	}
	if strings.TrimSpace(current.Annotations[runtimePoolBootstrapInstanceBindingAnnotation]) == "" {
		t.Fatal("old crashed Actor binding cleared before fresh credentials were published")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(&oldAuth), &corev1.Secret{}); err != nil {
		t.Fatalf("crashed Actor auth Secret disappeared before create-before-publish rotation: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	newAuth := runtimePoolTestPrivateAuthSecret(t, r, pool)
	if newAuth.UID == oldAuth.UID || newAuth.Name == oldAuth.Name {
		t.Fatalf("rotated crashed Actor auth Secret retained consumed identity %s/%s", newAuth.Name, newAuth.UID)
	}
	if strings.TrimSpace(current.Annotations[runtimePoolBootstrapInstanceBindingAnnotation]) != "" {
		t.Fatal("old crashed Actor binding survived fresh credential publication")
	}
	if len(control.created) != createdBeforeRecycle {
		t.Fatal("replacement Actor was created in the crashed credential-rotation barrier pass")
	}
}

func TestSubstrateRuntimePoolBackfillsPlacementBeforeRecyclingUnfencedActor(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	runtimePoolReconcile(t, r, pool)

	legacy := runtimePoolTestGetPool(t, r, pool)
	base := legacy.DeepCopy()
	delete(legacy.Annotations, substrateActorTemplateFenceAnnotation)
	delete(legacy.Annotations, substrateActorWorkerPlacementAnnotation)
	if err := r.Patch(context.Background(), &legacy, client.MergeFrom(base)); err != nil {
		t.Fatalf("remove legacy actor fence and placement: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	workerNamespace, workerPool, err := substrateRuntimePoolWorkerPlacementFromAnnotation(&current)
	if err != nil || workerNamespace != substrateTestWorkerNamespace || workerPool != substrateTestWorkerPoolName {
		t.Fatalf("backfilled placement = %q/%q, %v, want %s/%s", workerNamespace, workerPool, err, substrateTestWorkerNamespace, substrateTestWorkerPoolName)
	}
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		current.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
		t.Fatalf("unfenced actor status = %s/%s, want Degraded/Closed during recycle", current.Status.Lifecycle, current.Status.AdmissionState)
	}
	runtimePoolReconcile(t, r, pool)
	if len(control.settled) != 1 || control.settled[0] != substrateTestActorID(pool) {
		t.Fatalf("settled actors = %v, want the unfenced actor recycling after placement backfill", control.settled)
	}
}

func TestSubstrateRuntimePoolRefusesActorWithUnexpectedTemplateBeforeBootstrap(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	actorID := substrateTestActorID(pool)
	control.actors[actorID] = &workspace.SubstrateRuntimeActor{
		ActorID:           actorID,
		TemplateNamespace: "attacker-owned",
		TemplateName:      "credential-capture",
		Status:            "STATUS_RUNNING",
		PodNamespace:      substrateTestWorkerNamespace,
		PodName:           substrateTestWorkerPodName,
	}
	seedAttempts := 0
	r.SubstrateCredentialSeeder = func(context.Context, string, string, []byte, harnessv2.CredentialBootstrapRequest) error {
		seedAttempts++
		return nil
	}

	runtimePoolReconcile(t, r, pool)

	if seedAttempts != 0 {
		t.Fatalf("credential seed attempts = %d, want none for an actor with unexpected template identity", seedAttempts)
	}
	if len(control.resumed) != 0 {
		t.Fatalf("resumed actors = %v, want none for an actor with unexpected template identity", control.resumed)
	}
	if len(control.settled) != 0 || len(control.deleted) != 0 {
		t.Fatalf("foreign actor was modified: settled=%v deleted=%v", control.settled, control.deleted)
	}
	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded || got.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
		t.Fatalf("status = %s/%s, want Degraded/Closed", got.Status.Lifecycle, got.Status.AdmissionState)
	}
	if !strings.Contains(got.Status.Message, "does not use the controller-derived runtime template") {
		t.Fatalf("message = %q, want template-identity rejection", got.Status.Message)
	}
	if got.Annotations[substrateActorBootedAnnotation] != "" {
		t.Fatal("untrusted actor was recorded as booted")
	}
	if got.Annotations[substrateActorRecyclingAnnotation] != "" {
		t.Fatalf("foreign actor was marked for recycling: %q", got.Annotations[substrateActorRecyclingAnnotation])
	}

	runtimePoolReconcile(t, r, pool)
	if len(control.settled) != 0 || len(control.deleted) != 0 {
		t.Fatalf("foreign actor was modified on retry: settled=%v deleted=%v", control.settled, control.deleted)
	}
	if seedAttempts != 0 {
		t.Fatalf("credential seed attempts after deletion = %d, want none", seedAttempts)
	}
}

func TestSubstrateRuntimePoolRejectsSquattedDerivedTemplateOwnership(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	squatted := substrateTestBaseTemplate().DeepCopy()
	squatted.SetName(runtimePoolSubstrateTemplateName(runtimePoolResourceName(pool.Namespace, pool.Name)))
	squatted.SetLabels(map[string]string{runtimePoolManagedByLabel: substrateTestAttackerManagedBy})
	if err := r.Create(context.Background(), squatted); err != nil {
		t.Fatalf("create squatted derived template: %v", err)
	}

	runtimePoolReconcile(t, r, pool)

	if len(control.created) != 0 || len(control.resumed) != 0 || supervisor.probeCalls != 0 {
		t.Fatalf("untrusted template caused actor activity: created=%v resumed=%v probes=%d", control.created, control.resumed, supervisor.probeCalls)
	}
	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		!strings.Contains(got.Status.Message, "exact RuntimePool ownership identity") {
		t.Fatalf("status = %s/%q, want ownership rejection", got.Status.Lifecycle, got.Status.Message)
	}
}

func TestSubstrateRuntimePoolRecyclesActorWhenTemplateContentsDoNotMatchRevision(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	seedAttempts := 0
	r.SubstrateCredentialSeeder = func(context.Context, string, string, []byte, harnessv2.CredentialBootstrapRequest) error {
		seedAttempts++
		return nil
	}

	runtimePoolReconcile(t, r, pool)
	derived := substrateTestDerivedTemplate(t, r, pool)
	containers, found, err := unstructured.NestedSlice(derived.Object, "spec", "containers")
	if err != nil || !found || len(containers) != 1 {
		t.Fatalf("read derived containers: found=%v err=%v", found, err)
	}
	container := containers[0].(map[string]any)
	container[substrateTestObjectImageField] = runtimePoolTestTamperedImage
	containers[0] = container
	if err := unstructured.SetNestedSlice(derived.Object, containers, "spec", "containers"); err != nil {
		t.Fatalf("tamper derived container: %v", err)
	}
	if err := r.Update(context.Background(), derived); err != nil {
		t.Fatalf("update tampered derived template: %v", err)
	}

	runtimePoolReconcile(t, r, pool)

	if seedAttempts != 0 || supervisor.probeCalls != 0 {
		t.Fatalf("tampered template received credentials or probe: seeds=%d probes=%d", seedAttempts, supervisor.probeCalls)
	}
	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		!strings.Contains(got.Status.Message, "contents do not match their declared revision") {
		t.Fatalf("status = %s/%q, want template-integrity rejection", got.Status.Lifecycle, got.Status.Message)
	}
	runtimePoolReconcile(t, r, pool)
	if len(control.settled) != 1 {
		t.Fatalf("settled actors = %v, want tampered-template actor recycled", control.settled)
	}
}

//nolint:gocyclo // Every rendered-template invariant is asserted in one place.
func assertSubstrateDerivedTemplate(
	t *testing.T,
	r *RuntimePoolReconciler,
	pool *corev1alpha1.RuntimePool,
	derived *unstructured.Unstructured,
	actorID string,
) {
	t.Helper()
	if _, err := substrateRuntimeTemplateIntegrity(derived); err != nil {
		t.Fatalf("derived ActorTemplate integrity: %v", err)
	}
	deployed, err := substrateTemplatePodTemplateSpec(derived)
	if err != nil {
		t.Fatalf("reconstruct deployed template: %v", err)
	}
	container := deployed.Spec.Containers[0]
	if container.Image != pool.Spec.Runtime.Image {
		t.Fatalf("derived container image = %q, want immutable runtime image", container.Image)
	}
	if container.ImagePullPolicy != "" {
		t.Fatalf("derived container imagePullPolicy = %q, want the Kubernetes-only field omitted", container.ImagePullPolicy)
	}
	if len(container.VolumeMounts) != 0 || container.SecurityContext != nil || container.LivenessProbe != nil {
		t.Fatal("derived container carries Kubernetes-only surfaces; the provider sandbox owns them")
	}
	env := map[string]corev1.EnvVar{}
	for _, item := range container.Env {
		env[item.Name] = item
	}
	if env["ORKA_ACP_POD_UID"].Value != substrateActorInstanceUID(actorID) {
		t.Fatalf("derived POD_UID = %q, want actor instance UID", env["ORKA_ACP_POD_UID"].Value)
	}
	if env["ORKA_ACP_LISTEN_ADDRESS"].Value != ":80" ||
		len(container.Ports) != 1 || container.Ports[0].ContainerPort != substrateActorListenPort {
		t.Fatalf("derived listen address/port = %q/%v, want the conventional actor port 80", env["ORKA_ACP_LISTEN_ADDRESS"].Value, container.Ports)
	}
	for _, forbidden := range []string{
		runtimePoolControllerTokenFileEnv, runtimePoolCapabilitySecretFileEnv, runtimePoolProviderTokenFileEnv,
		"ORKA_ACP_CONTROLLER_TOKEN_BOOTSTRAP", "ORKA_ACP_CAPABILITY_SECRET_BOOTSTRAP", "ORKA_ACP_PROVIDER_TOKEN_BOOTSTRAP",
	} {
		if _, present := env[forbidden]; present {
			t.Fatalf("derived template carries credential env %q; provider templates must stay credential-free", forbidden)
		}
	}
	for _, item := range container.Env {
		if item.ValueFrom != nil {
			t.Fatalf("derived template env %q uses valueFrom; provider workloads must not resolve Secrets", item.Name)
		}
	}
	if strings.TrimSpace(env["ORKA_ACP_CREDENTIAL_BOOTSTRAP_NONCE"].Value) == "" {
		t.Fatal("derived template is missing the public credential bootstrap nonce")
	}
	var authSecrets corev1.SecretList
	if err := r.List(context.Background(), &authSecrets, client.InNamespace(pool.Namespace), client.MatchingLabels{
		runtimePoolUIDLabel:  string(pool.UID),
		runtimePoolAuthLabel: booleanTrueValue,
	}); err != nil || len(authSecrets.Items) != 1 {
		t.Fatalf("list RuntimePool auth Secret = %d, %v, want one", len(authSecrets.Items), err)
	}
	wantPublicKey, err := harnessv2.CredentialBootstrapPublicKey(authSecrets.Items[0].Data[runtimePoolBootstrapSigningSeedKey])
	if err != nil {
		t.Fatalf("derive expected bootstrap public key: %v", err)
	}
	if env[harnessv2.CredentialBootstrapPublicKeyEnv].Value != wantPublicKey {
		t.Fatal("derived template bootstrap public key is not bound to the controller-only signing seed")
	}
	if len(container.Command) != 1 || container.Command[0] != "/usr/local/bin/orka-acp-runtime" {
		t.Fatalf("derived container command = %v, want the explicit runtime entrypoint (the provider does not read image config)", container.Command)
	}
	var templateSecrets corev1.SecretList
	if err := r.List(context.Background(), &templateSecrets, client.InNamespace(substrateTestTemplateNamespace)); err == nil && len(templateSecrets.Items) != 0 {
		t.Fatalf("template namespace holds %d Secrets; nothing secret may exist there", len(templateSecrets.Items))
	}
	if workerPool, _, _ := unstructured.NestedString(derived.Object, substrateObjectSpecField, "workerPoolRef", substrateTestObjectNameField); workerPool != substrateTestWorkerPoolName {
		t.Fatalf("derived template workerPoolRef = %q, want operator infrastructure copied", workerPool)
	}
	if location, _, _ := unstructured.NestedString(derived.Object, "spec", "snapshotsConfig", "location"); location != substrateTestSnapshotLocation {
		t.Fatalf("derived template snapshotsConfig = %q, want operator infrastructure copied (safe: the golden-built instance boots credential-free)", location)
	}
}

func TestSubstrateRuntimePoolRejectsTemplateNamespaceContainingPoolSecrets(t *testing.T) {
	scheme := runtimePoolSubstrateTestScheme(t)
	pool := runtimePoolSubstrateTestObject()
	pool.Spec.RuntimeNamespace = acpTestRuntimeNamespace
	pool.Spec.ExecutionWorkspace.Substrate.BaseTemplateNamespace = pool.Spec.RuntimeNamespace
	r := runtimePoolTestReconciler(t, scheme, &fakeRuntimePoolSupervisorClient{}, pool)
	r.RuntimeNamespace = pool.Spec.RuntimeNamespace

	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		current.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed ||
		!strings.Contains(current.Status.Message, "must differ") {
		t.Fatalf("shared template/runtime namespace status = %s/%s %q, want Degraded/Closed namespace rejection", current.Status.Lifecycle, current.Status.AdmissionState, current.Status.Message)
	}
	var secrets corev1.SecretList
	if err := r.List(context.Background(), &secrets, client.InNamespace(pool.Spec.RuntimeNamespace)); err != nil {
		t.Fatalf("list runtime namespace Secrets: %v", err)
	}
	if len(secrets.Items) != 0 {
		t.Fatalf("shared template/runtime namespace created %d Secrets before rejection", len(secrets.Items))
	}
}

func TestSubstrateRuntimePoolServesThroughRouterHost(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)

	status := runtimePoolTestGetPool(t, r, pool).Status
	if status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting {
		t.Fatalf("status = %s/%s, want Serving/Accepting", status.Lifecycle, status.AdmissionState)
	}
	active := status.ActiveInstance
	actorID := substrateTestActorID(pool)
	if active == nil || active.PodAddress != substrateTestRouteHost(pool) ||
		active.PodUID != substrateActorInstanceUID(actorID) ||
		active.RuntimeInstanceID != substrateActorInstanceUID(actorID)+".actor-boot" {
		t.Fatalf("ActiveInstance = %#v, want route-host address with actor instance identity", active)
	}
	if active.PodNamespace != pool.Namespace || active.PodName != substrateTestWorkerPodName {
		t.Fatalf("ActiveInstance namespace/name = %s/%s, want RuntimePool credential namespace %s and internal worker name", active.PodNamespace, active.PodName, pool.Namespace)
	}
	if strings.Contains(active.PodUID, actorID) || strings.Contains(active.RuntimeInstanceID, actorID) {
		t.Fatalf("public active instance leaked raw provider actor ID %q: %#v", actorID, active)
	}
}

func TestSubstrateRuntimePoolRequiresVerifiedWorkerPlacementBeforeBootRecord(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*workspace.SubstrateRuntimeActor)
	}{
		{
			name: "missing placement",
			mutate: func(actor *workspace.SubstrateRuntimeActor) {
				actor.PodNamespace = ""
				actor.PodName = ""
			},
		},
		{
			name: "wrong namespace",
			mutate: func(actor *workspace.SubstrateRuntimeActor) {
				actor.PodNamespace = "other-workers"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			supervisor := &fakeRuntimePoolSupervisorClient{}
			control := newFakeSubstrateActorControl()
			control.afterResume = tt.mutate
			r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
			seedAttempts := 0
			r.SubstrateCredentialSeeder = func(context.Context, string, string, []byte, harnessv2.CredentialBootstrapRequest) error {
				seedAttempts++
				return nil
			}

			runtimePoolReconcile(t, r, pool)
			runtimePoolReconcile(t, r, pool)

			got := runtimePoolTestGetPool(t, r, pool)
			if got.Annotations[substrateActorBootedAnnotation] != "" {
				t.Fatalf("unsafe provider placement recorded a booted actor: %#v", got.Annotations)
			}
			if seedAttempts != 0 || supervisor.probeCalls != 0 {
				t.Fatalf("unsafe provider placement reached credentials or probe: seeds=%d probes=%d", seedAttempts, supervisor.probeCalls)
			}
			if got.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed ||
				!strings.Contains(got.Status.Message, "placement") {
				t.Fatalf("unsafe provider placement status = %s/%q, want Closed placement wait", got.Status.AdmissionState, got.Status.Message)
			}
		})
	}
}

func TestSubstrateRuntimePoolColdStartTimeout(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	now := runtimePoolTestNow
	r.Now = func() time.Time { return now }
	pool.Spec.ColdStartTimeoutSeconds = 5
	if err := r.Update(context.Background(), pool); err != nil {
		t.Fatalf("update cold-start timeout: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	actorID := substrateTestActorID(pool)
	control.actors[actorID].Status = "STATUS_RESUMING"
	now = now.Add(6 * time.Second)
	runtimePoolReconcile(t, r, pool)

	got := runtimePoolTestGetPool(t, r, pool)
	condition := meta.FindStatusCondition(got.Status.Conditions, corev1alpha1.RuntimePoolConditionRolloutReady)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		condition == nil || condition.Reason != runtimePoolRolloutReasonTimedOut {
		t.Fatalf("cold-start status/condition = %s/%#v, want Degraded/RolloutTimedOut", got.Status.Lifecycle, condition)
	}
}

func TestSubstrateRuntimePoolRecyclesActorOnCredentialSeedConflict(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	r.SubstrateCredentialSeeder = func(context.Context, string, string, []byte, harnessv2.CredentialBootstrapRequest) error {
		return errSubstrateCredentialConflict
	}

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)

	actorID := substrateTestActorID(pool)
	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded || got.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
		t.Fatalf("status = %s/%s, want Degraded/Closed", got.Status.Lifecycle, got.Status.AdmissionState)
	}
	if !strings.Contains(got.Status.Message, "seeded by another party") {
		t.Fatalf("message = %q, want the seed-conflict reason", got.Status.Message)
	}

	// The staged teardown settles the memoryless actor, then deletes it —
	// never a direct delete of a running workload.
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	if len(control.settled) != 1 || control.settled[0] != actorID {
		t.Fatalf("settled actors = %v, want the conflicted actor settled after workload destruction", control.settled)
	}
	if len(control.deleted) != 1 || control.deleted[0] != actorID {
		t.Fatalf("deleted actors = %v, want the conflicted actor recycled", control.deleted)
	}
	got = runtimePoolTestGetPool(t, r, pool)
	if got.Annotations[substrateActorBootedAnnotation] != "" {
		t.Fatal("booted annotation survived the recycle")
	}
	if got.Annotations[substrateActorRecyclingAnnotation] != "" {
		t.Fatal("recycling annotation survived teardown completion")
	}
	if got.Annotations[substrateActorCredentialSeededAnnotation] != "" {
		t.Fatal("credential-seeded annotation survived teardown completion")
	}
}

func TestSubstrateRuntimePoolRecyclesActorWhenPhysicalWorkerChanges(t *testing.T) {
	for _, replacementName := range []string{substrateTestWorkerPodName, "worker-1"} {
		t.Run(replacementName, func(t *testing.T) {
			supervisor := &fakeRuntimePoolSupervisorClient{}
			control := newFakeSubstrateActorControl()
			r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
			seedCalls := 0
			r.SubstrateCredentialSeeder = func(context.Context, string, string, []byte, harnessv2.CredentialBootstrapRequest) error {
				seedCalls++
				return nil
			}

			runtimePoolReconcile(t, r, pool)
			probePod := substrateTestProbePod(pool)
			supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-original-boot", false)
			runtimePoolReconcile(t, r, pool)
			if seedCalls != 1 {
				t.Fatalf("initial credential seed calls = %d, want 1", seedCalls)
			}

			worker := &corev1.Pod{}
			key := types.NamespacedName{Namespace: substrateTestWorkerNamespace, Name: substrateTestWorkerPodName}
			if err := r.Get(context.Background(), key, worker); err != nil {
				t.Fatalf("get original worker Pod: %v", err)
			}
			if err := r.Delete(context.Background(), worker); err != nil {
				t.Fatalf("delete original worker Pod: %v", err)
			}
			replacement := worker.DeepCopy()
			replacement.Name = replacementName
			replacement.ResourceVersion = ""
			replacement.UID = types.UID(replacementName + "-replacement-uid")
			if err := r.Create(context.Background(), replacement); err != nil {
				t.Fatalf("create replacement worker Pod: %v", err)
			}
			control.actors[substrateTestActorID(pool)].PodName = replacementName

			runtimePoolReconcile(t, r, pool)

			if seedCalls != 1 {
				t.Fatalf("replacement physical worker received %d total credential seeds, want the original seed only", seedCalls)
			}
			got := runtimePoolTestGetPool(t, r, pool)
			if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
				got.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed ||
				got.Status.ActiveInstance != nil ||
				!strings.Contains(got.Status.Message, "physical worker changed") {
				t.Fatalf("replacement physical worker status = %s/%s active=%v message=%q, want Degraded/Closed with no active instance", got.Status.Lifecycle, got.Status.AdmissionState, got.Status.ActiveInstance, got.Status.Message)
			}
			if got.Annotations[substrateActorRecyclingAnnotation] != substrateTestActorID(pool) {
				t.Fatalf("recycling annotation = %q, want actor %q", got.Annotations[substrateActorRecyclingAnnotation], substrateTestActorID(pool))
			}
		})
	}
}

func TestSubstrateRuntimePoolRotatesConsumedBootstrapBeforeReplacementActor(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	r.SubstrateCredentialSeeder = func(context.Context, string, string, []byte, harnessv2.CredentialBootstrapRequest) error {
		return errSubstrateCredentialConflict
	}

	runtimePoolReconcile(t, r, pool)
	actorID := substrateTestActorID(pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-bootstrap-rotation", false)
	runtimePoolReconcile(t, r, pool)

	oldAuth := runtimePoolTestPrivateAuthSecret(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	oldBinding, err := runtimePoolBootstrapInstanceBindingFromAnnotation(&current)
	if err != nil || oldBinding == nil {
		t.Fatalf("bootstrap instance binding = %#v, error=%v, want the served Actor binding", oldBinding, err)
	}
	if oldBinding.AuthSecretUID != oldAuth.UID || oldBinding.WorkloadUID != substrateTestWorkerPodUID {
		t.Fatalf("bootstrap instance binding = %#v, want auth Secret %q and worker Pod %q", oldBinding, oldAuth.UID, substrateTestWorkerPodUID)
	}

	for range 8 {
		runtimePoolReconcile(t, r, pool)
		current = runtimePoolTestGetPool(t, r, pool)
		if control.actors[actorID] == nil && !substrateActorWorkloadProofRequired(&current, actorID) {
			break
		}
	}
	if control.actors[actorID] != nil || substrateActorWorkloadProofRequired(&current, actorID) {
		t.Fatalf("conflicted Actor teardown did not prove workload absence: actor=%v annotations=%v", control.actors[actorID], current.Annotations)
	}
	createdBeforeRotation := len(control.created)
	r.Rand = &runtimePoolTestEntropyReader{next: 100}

	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if strings.TrimSpace(current.Annotations[runtimePoolPrivateAuthSecretBindingAnnotation(7)]) != "" {
		t.Fatal("consumed Substrate auth Secret remained published after Actor teardown")
	}
	if strings.TrimSpace(current.Annotations[runtimePoolBootstrapInstanceBindingAnnotation]) == "" {
		t.Fatal("old Actor binding cleared before fresh credentials were published")
	}
	if len(control.created) != createdBeforeRotation {
		t.Fatal("replacement Actor was created before consumed bootstrap material rotated")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(&oldAuth), &corev1.Secret{}); err != nil {
		t.Fatalf("consumed Substrate auth Secret disappeared before create-before-publish rotation: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	newAuth := runtimePoolTestPrivateAuthSecret(t, r, pool)
	if newAuth.UID == oldAuth.UID || newAuth.Name == oldAuth.Name {
		t.Fatalf("rotated Substrate auth Secret retained consumed identity %s/%s", newAuth.Name, newAuth.UID)
	}
	if strings.TrimSpace(current.Annotations[runtimePoolPrivateAuthSecretBindingAnnotation(7)]) == "" {
		t.Fatal("fresh Substrate auth Secret was not published before clearing the old Actor binding")
	}
	if strings.TrimSpace(current.Annotations[runtimePoolBootstrapInstanceBindingAnnotation]) != "" {
		t.Fatal("old Actor binding survived fresh credential publication")
	}
	if len(control.created) != createdBeforeRotation {
		t.Fatal("replacement Actor was created in the credential-rotation barrier pass")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(&oldAuth), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("consumed Substrate auth Secret survived rotation: %v", err)
	}

	for range 10 {
		runtimePoolReconcile(t, r, pool)
		if len(control.created) > createdBeforeRotation {
			return
		}
	}
	t.Fatal("replacement Actor was not created after fresh bootstrap material was published")
}

func TestSubstrateRuntimePoolRecoversAfterBoundAuthSecretDisappears(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-missing-auth", false)
	runtimePoolReconcile(t, r, pool)

	actorID := substrateTestActorID(pool)
	oldAuth := runtimePoolTestPrivateAuthSecret(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	bindingKey := runtimePoolPrivateAuthSecretBindingAnnotation(7)
	oldBinding := current.Annotations[bindingKey]
	if oldBinding == "" || strings.TrimSpace(current.Annotations[runtimePoolBootstrapInstanceBindingAnnotation]) == "" {
		t.Fatal("serving Actor did not record credential and physical-instance bindings")
	}
	if err := r.Delete(context.Background(), &oldAuth); err != nil {
		t.Fatalf("delete bound Substrate auth Secret: %v", err)
	}
	createdBeforeRecovery := len(control.created)

	for range 8 {
		runtimePoolReconcile(t, r, pool)
		current = runtimePoolTestGetPool(t, r, pool)
		if strings.TrimSpace(current.Annotations[bindingKey]) == "" {
			break
		}
		if current.Annotations[bindingKey] != oldBinding {
			t.Fatal("missing Substrate auth binding changed before actor teardown completed")
		}
		if len(control.created) != createdBeforeRecovery {
			t.Fatal("replacement Actor was created while the missing auth binding remained published")
		}
	}
	if strings.TrimSpace(current.Annotations[bindingKey]) != "" {
		t.Fatalf("missing Substrate auth binding survived exact Actor teardown: annotations=%v", current.Annotations)
	}
	if control.actors[actorID] != nil || substrateActorWorkloadProofRequired(&current, actorID) {
		t.Fatalf("auth binding cleared before Actor workload absence was proven: actor=%v annotations=%v", control.actors[actorID], current.Annotations)
	}
	if strings.TrimSpace(current.Annotations[runtimePoolBootstrapInstanceBindingAnnotation]) == "" {
		t.Fatal("old Actor binding cleared before fresh credentials were published")
	}
	if len(control.created) != createdBeforeRecovery {
		t.Fatal("replacement Actor was created before missing credentials rotated")
	}

	r.Rand = &runtimePoolTestEntropyReader{next: 100}
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	newAuth := runtimePoolTestPrivateAuthSecret(t, r, pool)
	if newAuth.UID == oldAuth.UID || newAuth.Name == oldAuth.Name {
		t.Fatalf("replacement Substrate auth Secret retained missing identity %s/%s", newAuth.Name, newAuth.UID)
	}
	if strings.TrimSpace(current.Annotations[bindingKey]) == "" {
		t.Fatal("fresh Substrate auth Secret was not published")
	}
	if strings.TrimSpace(current.Annotations[runtimePoolBootstrapInstanceBindingAnnotation]) != "" {
		t.Fatal("old Actor binding survived fresh credential publication")
	}
	if len(control.created) != createdBeforeRecovery {
		t.Fatal("replacement Actor was created in the credential-rotation barrier pass")
	}
}

func TestSubstrateRuntimePoolRetriesAmbiguousClosedBootstrapWithoutProof(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{probeErr: errors.New("route not ready")}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	seedAttempts := 0
	r.SubstrateCredentialSeeder = func(context.Context, string, string, []byte, harnessv2.CredentialBootstrapRequest) error {
		seedAttempts++
		if seedAttempts == 1 {
			return errSubstrateCredentialAlreadyComplete
		}
		return nil
	}

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	runtimePoolReconcile(t, r, pool)

	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStarting || got.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
		t.Fatalf("status = %s/%s, want Starting/Closed", got.Status.Lifecycle, got.Status.AdmissionState)
	}
	if !strings.Contains(got.Status.Message, "completion is not yet authenticated") {
		t.Fatalf("message = %q, want ambiguous-bootstrap retry reason", got.Status.Message)
	}
	if got.Annotations[substrateActorRecyclingAnnotation] != "" || len(control.settled) != 0 || len(control.deleted) != 0 {
		t.Fatalf("ambiguous route recycled actor: annotation=%q settled=%v deleted=%v", got.Annotations[substrateActorRecyclingAnnotation], control.settled, control.deleted)
	}

	supervisor.probeErr = nil
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-route-ready", false)
	runtimePoolReconcile(t, r, pool)
	got = runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || got.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting {
		t.Fatalf("status after route propagation = %s/%s, want Serving/Accepting", got.Status.Lifecycle, got.Status.AdmissionState)
	}
	if seedAttempts != 2 {
		t.Fatalf("seed attempts = %d, want retry after ambiguous 404", seedAttempts)
	}
}

func TestSubstrateRuntimePoolHoldsAdmissionUntilCredentialSeeding(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	seedAttempts := 0
	r.SubstrateCredentialSeeder = func(_ context.Context, routeHost, nonce string, _ []byte, request harnessv2.CredentialBootstrapRequest) error {
		seedAttempts++
		if routeHost == "" || nonce == "" {
			t.Fatalf("seeder called without route host or nonce")
		}
		if err := request.Validate(); err != nil {
			t.Fatalf("seeder received invalid pool credentials: %v", err)
		}
		if seedAttempts == 1 {
			return errors.New("supervisor is still booting")
		}
		return nil
	}

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)

	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStarting || got.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
		t.Fatalf("status = %s/%s, want Starting/Closed while seeding is incomplete", got.Status.Lifecycle, got.Status.AdmissionState)
	}
	if !strings.Contains(got.Status.Message, "credential bootstrap is not complete") {
		t.Fatalf("message = %q, want incomplete-bootstrap reason", got.Status.Message)
	}

	runtimePoolReconcile(t, r, pool)
	status := runtimePoolTestGetPool(t, r, pool).Status
	if status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting {
		t.Fatalf("status = %s/%s, want Serving/Accepting after seeding succeeds", status.Lifecycle, status.AdmissionState)
	}
	if seedAttempts < 2 {
		t.Fatalf("seed attempts = %d, want a retry after the transient failure", seedAttempts)
	}
	got = runtimePoolTestGetPool(t, r, pool)
	if got.Annotations[substrateActorCredentialSeededAnnotation] != substrateTestActorID(pool) {
		t.Fatal("successfully seeded actor lifetime was not recorded")
	}
}

func TestSubstrateTeardownDestroysLabeledWorkerPodBeforeSettling(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	runtimePoolReconcile(t, r, pool)
	actorID := substrateTestActorID(pool)
	current := runtimePoolTestGetPool(t, r, pool)

	gone, err := r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || gone {
		t.Fatalf("teardown with live worker = (%v, %v), want in-progress", gone, err)
	}
	if len(control.settled) != 0 {
		t.Fatal("actor settled while its workload memory still existed")
	}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: substrateTestWorkerNamespace, Name: substrateTestWorkerPodName}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("worker pod get after teardown = %v, want deleted", err)
	}

	gone, err = r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || gone {
		t.Fatalf("teardown after workload destruction = (%v, %v), want settling", gone, err)
	}
	if len(control.settled) != 1 {
		t.Fatalf("settled = %v, want the memoryless actor settled", control.settled)
	}
	gone, err = r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || !gone {
		t.Fatalf("final teardown = (%v, %v), want deleted", gone, err)
	}
	if len(control.deleted) != 1 {
		t.Fatalf("deleted = %v, want the settled actor deleted", control.deleted)
	}
}

func TestSubstrateTeardownMissingActorProvesExactWorkerPodAbsent(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	runtimePoolReconcile(t, r, pool)
	actorID := substrateTestActorID(pool)
	current := runtimePoolTestGetPool(t, r, pool)
	delete(control.actors, actorID)

	gone, err := r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || gone {
		t.Fatalf("teardown with missing Actor and live exact worker Pod = (%v, %v), want in-progress", gone, err)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: substrateTestWorkerNamespace, Name: substrateTestWorkerPodName}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("exact worker Pod after missing-Actor teardown = %v, want deleted", err)
	}
	if current.Annotations[substrateActorWorkloadAbsentAnnotation] != "" {
		t.Fatal("workload absence was recorded before the exact worker Pod deletion became observable")
	}

	gone, err = r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || gone {
		t.Fatalf("teardown while persisting missing-Actor workload absence = (%v, %v), want in-progress", gone, err)
	}
	if current.Annotations[substrateActorWorkloadAbsentAnnotation] != actorID {
		t.Fatalf("workload absence barrier = %q, want %q", current.Annotations[substrateActorWorkloadAbsentAnnotation], actorID)
	}

	gone, err = r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || !gone {
		t.Fatalf("teardown after persisted missing-Actor workload absence = (%v, %v), want gone", gone, err)
	}
	if len(control.settled) != 0 || len(control.deleted) != 0 {
		t.Fatalf("missing Actor triggered provider mutations: settled=%v deleted=%v", control.settled, control.deleted)
	}
}

func TestSubstrateRuntimePoolMissingActorDoesNotReportStoppedBeforeWorkerAbsence(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	runtimePoolReconcile(t, r, pool)
	actorID := substrateTestActorID(pool)
	delete(control.actors, actorID)

	current := runtimePoolTestGetPool(t, r, pool)
	current.Spec.DesiredReplicas = 0
	current.Generation++
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("scale pool with missing Actor to zero: %v", err)
	}
	runtimePoolReconcile(t, r, pool)

	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopping ||
		got.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
		t.Fatalf("missing-Actor scale-down status = %s/%s, want Stopping/Closed until exact worker absence", got.Status.Lifecycle, got.Status.AdmissionState)
	}
	if got.Annotations[substrateActorRecyclingAnnotation] != actorID {
		t.Fatalf("missing-Actor recycling annotation = %q, want %q", got.Annotations[substrateActorRecyclingAnnotation], actorID)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: substrateTestWorkerNamespace, Name: substrateTestWorkerPodName}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("exact worker Pod after missing-Actor scale-down = %v, want deleted", err)
	}
}

func TestSubstrateTeardownDestroysWorkerPodWhileActorSuspending(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	runtimePoolReconcile(t, r, pool)
	actorID := substrateTestActorID(pool)
	control.actors[actorID].Status = substrateTestStatusSuspending
	current := runtimePoolTestGetPool(t, r, pool)

	worker := &corev1.Pod{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: substrateTestWorkerNamespace, Name: substrateTestWorkerPodName}, worker); err != nil {
		t.Fatalf("get worker pod: %v", err)
	}

	gone, err := r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || gone {
		t.Fatalf("teardown while suspending = (%v, %v), want in-progress", gone, err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(worker), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("worker pod get after suspending teardown = %v, want deleted", err)
	}
	if len(control.settled) != 0 || len(control.deleted) != 0 {
		t.Fatalf("provider calls while suspending = settle %v delete %v, want neither", control.settled, control.deleted)
	}

	gone, err = r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || gone {
		t.Fatalf("teardown after workload absence while suspending = (%v, %v), want waiting", gone, err)
	}
	control.actors[actorID].Status = substrateTestStatusSuspended
	gone, err = r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || !gone {
		t.Fatalf("teardown after suspension settled = (%v, %v), want deleted", gone, err)
	}
}

func TestSubstrateTeardownUsesFrozenWorkerPlacementAfterTemplateMutation(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	runtimePoolReconcile(t, r, pool)
	actorID := substrateTestActorID(pool)
	current := runtimePoolTestGetPool(t, r, pool)
	workerNamespace, workerPool, err := substrateRuntimePoolWorkerPlacementFromAnnotation(&current)
	if err != nil || workerNamespace != substrateTestWorkerNamespace || workerPool != substrateTestWorkerPoolName {
		t.Fatalf("recorded placement = %q/%q, %v, want ate-workers/orka-workers", workerNamespace, workerPool, err)
	}

	derived := &unstructured.Unstructured{}
	derived.SetGroupVersionKind(substrateActorTemplateGVK)
	key := types.NamespacedName{
		Namespace: substrateTestTemplateNamespace,
		Name:      runtimePoolSubstrateTemplateName(runtimePoolResourceName(pool.Namespace, pool.Name)),
	}
	if err := r.Get(context.Background(), key, derived); err != nil {
		t.Fatalf("get derived template: %v", err)
	}
	if err := unstructured.SetNestedField(derived.Object, "other-workers", "spec", "workerPoolRef", "namespace"); err != nil {
		t.Fatalf("mutate derived template namespace: %v", err)
	}
	if err := unstructured.SetNestedField(derived.Object, "other-pool", substrateObjectSpecField, "workerPoolRef", substrateTestObjectNameField); err != nil {
		t.Fatalf("mutate derived template WorkerPool: %v", err)
	}
	if err := r.Update(context.Background(), derived); err != nil {
		t.Fatalf("update derived template: %v", err)
	}

	worker := &corev1.Pod{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: substrateTestWorkerNamespace, Name: substrateTestWorkerPodName}, worker); err != nil {
		t.Fatalf("get original-placement worker pod: %v", err)
	}
	gone, err := r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || gone {
		t.Fatalf("teardown after template placement mutation = (%v, %v), want worker deletion in progress", gone, err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(worker), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("original-placement worker pod get after teardown = %v, want deleted", err)
	}
}

func TestSubstrateTeardownUsesWorkloadAbsenceBarrierAfterPlacementCleared(t *testing.T) {
	tests := []struct {
		name       string
		finalState string
	}{
		{name: "suspended", finalState: substrateTestStatusSuspended},
		{name: "crashed", finalState: substrateTestStatusCrashed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			supervisor := &fakeRuntimePoolSupervisorClient{}
			control := newFakeSubstrateActorControl()
			control.afterSettle = func(actor *workspace.SubstrateRuntimeActor) {
				actor.Status = tt.finalState
				actor.PodNamespace = ""
				actor.PodName = ""
			}
			r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
			runtimePoolReconcile(t, r, pool)
			actorID := substrateTestActorID(pool)

			current := runtimePoolTestGetPool(t, r, pool)
			gone, err := r.teardownSubstrateActor(context.Background(), &current, control, actorID)
			if err != nil || gone {
				t.Fatalf("teardown with live worker = (%v, %v), want in-progress", gone, err)
			}

			current = runtimePoolTestGetPool(t, r, pool)
			gone, err = r.teardownSubstrateActor(context.Background(), &current, control, actorID)
			if err != nil || gone {
				t.Fatalf("teardown after workload deletion = (%v, %v), want settled in-progress", gone, err)
			}
			if len(control.settled) != 1 {
				t.Fatalf("settled actors = %v, want one", control.settled)
			}
			current = runtimePoolTestGetPool(t, r, pool)
			if current.Annotations[substrateActorWorkloadAbsentAnnotation] != actorID {
				t.Fatalf("workload absence barrier = %q, want %q", current.Annotations[substrateActorWorkloadAbsentAnnotation], actorID)
			}

			gone, err = r.teardownSubstrateActor(context.Background(), &current, control, actorID)
			if err != nil || !gone {
				t.Fatalf("teardown after placement cleared = (%v, %v), want gone", gone, err)
			}
			if len(control.deleted) != 1 || control.deleted[0] != actorID {
				t.Fatalf("deleted actors = %v, want %q", control.deleted, actorID)
			}
		})
	}
}

func TestSubstrateTeardownReprovesReplacementWorkerAfterAbsenceBarrier(t *testing.T) {
	const replacementPodName = "worker-1"

	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	runtimePoolReconcile(t, r, pool)
	actorID := substrateTestActorID(pool)
	current := runtimePoolTestGetPool(t, r, pool)

	gone, err := r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || gone {
		t.Fatalf("initial teardown = (%v, %v), want worker deletion in progress", gone, err)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	workloadGone, err := r.destroySubstrateActorWorkload(context.Background(), &current, actorID, control.actors[actorID])
	if err != nil || !workloadGone {
		t.Fatalf("independent workload absence proof = (%v, %v), want absent", workloadGone, err)
	}
	if err := r.setSubstrateRuntimePoolAnnotation(context.Background(), &current, substrateActorWorkloadAbsentAnnotation, actorID); err != nil {
		t.Fatalf("persist workload absence proof: %v", err)
	}
	if len(control.settled) != 0 {
		t.Fatalf("settled actors = %v, want none before replacement reassignment", control.settled)
	}

	replacement := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: substrateTestWorkerNamespace,
		Name:      replacementPodName,
		UID:       "worker-1-uid",
		Labels:    map[string]string{substrateWorkerPoolLabel: substrateTestWorkerPoolName},
	}}
	if err := r.Create(context.Background(), replacement); err != nil {
		t.Fatalf("create replacement worker Pod: %v", err)
	}
	control.actors[actorID].PodName = replacementPodName

	current = runtimePoolTestGetPool(t, r, pool)
	gone, err = r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || gone {
		t.Fatalf("teardown fencing replacement placement = (%v, %v), want in-progress", gone, err)
	}
	if len(control.settled) != 0 {
		t.Fatalf("settled actors = %v, want none while replacement worker exists", control.settled)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(replacement), &corev1.Pod{}); err != nil {
		t.Fatalf("replacement worker Pod was deleted before exact fencing: %v", err)
	}

	current = runtimePoolTestGetPool(t, r, pool)
	gone, err = r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || gone {
		t.Fatalf("teardown promoting replacement fence = (%v, %v), want in-progress", gone, err)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	gone, err = r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || gone {
		t.Fatalf("teardown deleting replacement worker = (%v, %v), want in-progress", gone, err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(replacement), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("replacement worker Pod survived exact fenced deletion: %v", err)
	}
	if len(control.settled) != 0 {
		t.Fatalf("settled actors = %v, want none until replacement absence is re-proven", control.settled)
	}

	current = runtimePoolTestGetPool(t, r, pool)
	gone, err = r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || gone {
		t.Fatalf("teardown after replacement absence = (%v, %v), want settled in-progress", gone, err)
	}
	if len(control.settled) != 1 || control.settled[0] != actorID {
		t.Fatalf("settled actors = %v, want actor %q only after replacement absence", control.settled, actorID)
	}
}

func TestSubstrateTeardownUsesExactPodFenceAfterLabelDrift(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	runtimePoolReconcile(t, r, pool)
	actorID := substrateTestActorID(pool)
	current := runtimePoolTestGetPool(t, r, pool)

	worker := &corev1.Pod{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: substrateTestWorkerNamespace, Name: substrateTestWorkerPodName}, worker); err != nil {
		t.Fatalf("get exact worker Pod: %v", err)
	}
	worker.Labels = nil
	if err := r.Update(context.Background(), worker); err != nil {
		t.Fatalf("remove exact worker Pod labels: %v", err)
	}
	gone, err := r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || gone {
		t.Fatalf("teardown after exact worker Pod label drift = (%v, %v), want deletion in progress", gone, err)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: substrateTestWorkerNamespace, Name: substrateTestWorkerPodName}, &corev1.Pod{}); err != nil {
		if !apierrors.IsNotFound(err) {
			t.Fatalf("get exact worker Pod after teardown: %v", err)
		}
	} else {
		t.Fatal("exact UID-fenced worker Pod survived teardown after mutable label drift")
	}
}

func TestSubstrateTeardownPreservesSameNamePodReplacement(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	runtimePoolReconcile(t, r, pool)
	actorID := substrateTestActorID(pool)
	current := runtimePoolTestGetPool(t, r, pool)
	delete(control.actors, actorID)

	worker := &corev1.Pod{}
	key := types.NamespacedName{Namespace: substrateTestWorkerNamespace, Name: substrateTestWorkerPodName}
	if err := r.Get(context.Background(), key, worker); err != nil {
		t.Fatalf("get exact worker Pod: %v", err)
	}
	if err := r.Delete(context.Background(), worker); err != nil {
		t.Fatalf("delete exact worker Pod: %v", err)
	}
	replacement := worker.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = "worker-0-replacement-uid"
	if err := r.Create(context.Background(), replacement); err != nil {
		t.Fatalf("create same-name replacement Pod: %v", err)
	}

	gone, err := r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || gone {
		t.Fatalf("teardown with same-name replacement = (%v, %v), want persisted absence in progress", gone, err)
	}
	observed := &corev1.Pod{}
	if err := r.Get(context.Background(), key, observed); err != nil {
		t.Fatalf("same-name replacement Pod was deleted: %v", err)
	}
	if observed.UID != replacement.UID {
		t.Fatalf("same-name Pod UID = %q, want replacement UID %q", observed.UID, replacement.UID)
	}
	if current.Annotations[substrateActorWorkloadAbsentAnnotation] != actorID {
		t.Fatalf("exact workload absence barrier = %q, want %q", current.Annotations[substrateActorWorkloadAbsentAnnotation], actorID)
	}
}

func TestSubstrateTeardownPromotesValidatedReplacementFenceBeforeDeletion(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	runtimePoolReconcile(t, r, pool)
	actorID := substrateTestActorID(pool)
	current := runtimePoolTestGetPool(t, r, pool)

	worker := &corev1.Pod{}
	key := types.NamespacedName{Namespace: substrateTestWorkerNamespace, Name: substrateTestWorkerPodName}
	if err := r.Get(context.Background(), key, worker); err != nil {
		t.Fatalf("get exact worker Pod: %v", err)
	}
	if err := r.Delete(context.Background(), worker); err != nil {
		t.Fatalf("delete exact worker Pod: %v", err)
	}
	replacement := worker.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = "worker-0-replacement-uid"
	if err := r.Create(context.Background(), replacement); err != nil {
		t.Fatalf("create same-name replacement Pod: %v", err)
	}
	if err := r.verifySubstrateActorWorkerPlacement(context.Background(), &current, control.actors[actorID]); !errors.Is(err, errSubstrateWorkerPodFenceConflict) {
		t.Fatalf("replacement placement verification error = %v, want exact-fence conflict", err)
	}

	gone, err := r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || gone {
		t.Fatalf("teardown promoting replacement fence = (%v, %v), want promotion in progress", gone, err)
	}
	if len(control.settled) != 0 {
		t.Fatalf("settled actors = %v, want none before replacement workload deletion", control.settled)
	}
	replacementFence, err := substrateRuntimePoolWorkerPodFenceFromAnnotation(&current)
	if err != nil || replacementFence == nil || replacementFence.UID != replacement.UID {
		t.Fatalf("promoted worker Pod fence = %#v, error=%v, want UID %q", replacementFence, err, replacement.UID)
	}
	if current.Annotations[substrateActorReplacementWorkerPodFenceAnnotation] != "" {
		t.Fatal("replacement worker Pod fence remained staged after promotion")
	}

	gone, err = r.teardownSubstrateActor(context.Background(), &current, control, actorID)
	if err != nil || gone {
		t.Fatalf("teardown deleting promoted replacement = (%v, %v), want deletion in progress", gone, err)
	}
	if err := r.Get(context.Background(), key, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("promoted replacement Pod survived exact fenced deletion: %v", err)
	}
	if len(control.settled) != 0 {
		t.Fatalf("settled actors = %v, want none until replacement workload absence is observed", control.settled)
	}
}

func TestSubstrateTeardownRefusesUnknownOrMismatchedWorkerPlacement(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*workspace.SubstrateRuntimeActor)
		pod       *corev1.Pod
		wantError string
	}{
		{
			name: "missing provider placement",
			mutate: func(actor *workspace.SubstrateRuntimeActor) {
				actor.PodNamespace = ""
				actor.PodName = ""
			},
			wantError: "placement is incomplete",
		},
		{
			name: "wrong worker namespace",
			mutate: func(actor *workspace.SubstrateRuntimeActor) {
				actor.PodNamespace = "other-workers"
			},
			wantError: "does not match infrastructure WorkerPool namespace",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			supervisor := &fakeRuntimePoolSupervisorClient{}
			control := newFakeSubstrateActorControl()
			r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
			runtimePoolReconcile(t, r, pool)
			actorID := substrateTestActorID(pool)
			current := runtimePoolTestGetPool(t, r, pool)
			tt.mutate(control.actors[actorID])
			if tt.pod != nil {
				worker := &corev1.Pod{}
				if err := r.Get(context.Background(), client.ObjectKeyFromObject(tt.pod), worker); err != nil {
					t.Fatalf("get worker Pod: %v", err)
				}
				worker.Labels = cloneStringMap(tt.pod.Labels)
				if err := r.Update(context.Background(), worker); err != nil {
					t.Fatalf("update worker Pod: %v", err)
				}
			}

			if _, err := r.teardownSubstrateActor(context.Background(), &current, control, actorID); err == nil ||
				!strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("teardown error = %v, want %q", err, tt.wantError)
			}
			if len(control.settled) != 0 {
				t.Fatalf("settled actors = %v, want none after placement rejection", control.settled)
			}
		})
	}
}

func TestGetSubstrateActorTemplateUsesUncachedReader(t *testing.T) {
	scheme := runtimePoolSubstrateTestScheme(t)
	template := substrateTestBaseTemplate()
	cachedClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	apiReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(template).Build()
	r := &RuntimePoolReconciler{Client: cachedClient, APIReader: apiReader}

	got, err := r.getSubstrateActorTemplate(context.Background(), template.GetNamespace(), template.GetName())
	if err != nil {
		t.Fatalf("getSubstrateActorTemplate() error = %v", err)
	}
	if got == nil || got.GetUID() != template.GetUID() || got.GetName() != template.GetName() {
		t.Fatalf("uncached ActorTemplate = %#v, want %s/%s", got, template.GetNamespace(), template.GetName())
	}
}

func TestSubstrateRuntimePoolRecyclesProviderSuspendedActor(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	actorID := substrateTestActorID(pool)
	// The provider suspended the booted actor behind the controller's back:
	// supervisor memory (credentials included) has been checkpointed.
	control.actors[actorID].Status = substrateTestStatusSuspended
	control.actors[actorID].SnapshotObserved = true

	runtimePoolReconcile(t, r, pool)
	if len(control.deleted) != 0 {
		t.Fatalf("deleted actors before workload absence proof = %v, want none", control.deleted)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: substrateTestWorkerNamespace, Name: substrateTestWorkerPodName}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("suspended actor worker Pod get = %v, want deleted before Actor", err)
	}
	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded || got.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
		t.Fatalf("status = %s/%s, want Degraded/Closed", got.Status.Lifecycle, got.Status.AdmissionState)
	}
	if !strings.Contains(got.Status.Message, "suspension is prohibited") {
		t.Fatalf("message = %q, want suspension prohibition", got.Status.Message)
	}

	runtimePoolReconcile(t, r, pool)
	if len(control.deleted) != 1 || control.deleted[0] != actorID {
		t.Fatalf("deleted actors = %v, want the workload-free suspended actor recycled", control.deleted)
	}
	got = runtimePoolTestGetPool(t, r, pool)
	if got.Annotations[substrateActorBootedAnnotation] != "" {
		t.Fatal("booted annotation survived the recycle")
	}

	// The replacement boots from scratch.
	runtimePoolReconcile(t, r, pool)
	if len(control.boots) != 2 || !control.boots[1] {
		t.Fatalf("boots = %v, want a second fresh boot", control.boots)
	}
}

func TestSubstrateRuntimePoolScaleToZeroDrainsThenDeletesActor(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)

	// Teardown must authenticate against the frozen derived template even when
	// the mutable infrastructure template is gone. Simulate the API server's
	// generation bump for the scale-to-zero spec update as well: the running
	// Actor remains fenced to the generation it was deployed with.
	if err := r.Delete(context.Background(), substrateTestBaseTemplate()); err != nil {
		t.Fatalf("delete mutable base template before scale-down: %v", err)
	}
	r.SubstrateEnabled = false
	current := runtimePoolTestGetPool(t, r, pool)
	current.Spec.DesiredReplicas = 0
	current.Generation++
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("scale pool to zero: %v", err)
	}
	runtimePoolReconcile(t, r, pool)
	if supervisor.drainCalls != 1 {
		t.Fatalf("drain calls = %d, want 1", supervisor.drainCalls)
	}
	if len(control.deleted) != 0 {
		t.Fatal("actor deleted before authenticated drain quiescence")
	}

	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", true)
	runtimePoolReconcile(t, r, pool)
	if got := runtimePoolTestGetPool(t, r, pool).Status.Lifecycle; got != corev1alpha1.RuntimePoolLifecycleQuiescent {
		t.Fatalf("lifecycle after quiescent probe = %s, want Quiescent", got)
	}
	// Staged teardown: settle the memoryless actor, then delete it.
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	if len(control.settled) != 1 {
		t.Fatalf("settled actors = %v, want the drained actor settled before deletion", control.settled)
	}
	if len(control.deleted) != 1 {
		t.Fatalf("deleted actors = %v, want the drained actor deleted", control.deleted)
	}
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	status := runtimePoolTestGetPool(t, r, pool).Status
	if status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped || status.ActiveInstance != nil {
		t.Fatalf("status = %s (active=%v), want Stopped with no active instance", status.Lifecycle, status.ActiveInstance)
	}
}

func TestSubstrateRuntimePoolScaleToZeroUsesPersistedFencesWhenTemplatesAreGone(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-template-gone", false)
	runtimePoolReconcile(t, r, pool)

	derived := substrateTestDerivedTemplate(t, r, pool)
	if derived == nil {
		t.Fatal("derived ActorTemplate was not materialized")
	}
	if err := r.Delete(context.Background(), derived); err != nil {
		t.Fatalf("delete derived ActorTemplate before scale-down: %v", err)
	}
	if err := r.Delete(context.Background(), substrateTestBaseTemplate()); err != nil {
		t.Fatalf("delete base ActorTemplate before scale-down: %v", err)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	current.Spec.DesiredReplicas = 0
	current.Generation++
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("scale pool to zero: %v", err)
	}

	for range 10 {
		runtimePoolReconcile(t, r, pool)
		current = runtimePoolTestGetPool(t, r, pool)
		if current.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleStopped {
			break
		}
	}
	if supervisor.drainCalls != 0 {
		t.Fatalf("drain calls without a deployed template = %d, want 0", supervisor.drainCalls)
	}
	if len(control.deleted) != 1 || control.deleted[0] != substrateTestActorID(pool) {
		t.Fatalf("deleted actors = %v, want the fenced actor removed", control.deleted)
	}
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped || current.Status.ActiveInstance != nil {
		t.Fatalf("status = %s (active=%v), want Stopped with no active instance", current.Status.Lifecycle, current.Status.ActiveInstance)
	}
	if recreated := substrateTestDerivedTemplate(t, r, pool); recreated != nil {
		t.Fatal("derived ActorTemplate was rebuilt during fenced scale-down")
	}
}

func TestSubstrateRuntimePoolDemandReturnCompletesDrainBeforeReplacement(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-reactivate", false)
	runtimePoolReconcile(t, r, pool)

	current := runtimePoolTestGetPool(t, r, pool)
	current.Spec.DesiredReplicas = 0
	current.Generation++
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("scale pool to zero: %v", err)
	}
	runtimePoolReconcile(t, r, pool)
	if supervisor.drainCalls != 1 {
		t.Fatalf("drain calls = %d, want 1", supervisor.drainCalls)
	}

	current = runtimePoolTestGetPool(t, r, pool)
	current.Spec.DesiredReplicas = 1
	current.Generation++
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("restore substrate demand: %v", err)
	}
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-reactivate", true)
	runtimePoolReconcile(t, r, pool)
	if got := runtimePoolTestGetPool(t, r, pool).Status.Lifecycle; got != corev1alpha1.RuntimePoolLifecycleQuiescent {
		t.Fatalf("lifecycle after demand returned to a drained actor = %s, want Quiescent rollout barrier", got)
	}
	for range 8 {
		runtimePoolReconcile(t, r, pool)
		if len(control.deleted) != 0 {
			break
		}
	}
	if len(control.deleted) != 1 {
		t.Fatalf("deleted actors = %v, want the drained actor deleted despite renewed demand", control.deleted)
	}

	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-replacement", false)
	for range 8 {
		runtimePoolReconcile(t, r, pool)
		if len(control.boots) >= 2 {
			break
		}
	}
	if len(control.boots) != 2 || !control.boots[1] {
		t.Fatalf("boots = %v, want renewed demand to boot a replacement actor", control.boots)
	}
}

func TestSubstrateRuntimePoolFinalizerDeletesActorTemplateAndSecrets(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	if substrateTestDerivedTemplate(t, r, pool) == nil {
		t.Fatal("derived template was not materialized")
	}
	baseTemplate := substrateTestBaseTemplate()
	if err := r.Delete(context.Background(), baseTemplate); err != nil {
		t.Fatalf("delete mutable base template before finalization: %v", err)
	}
	// Disabling new Substrate dispatch must not strand existing provider
	// resources or the RuntimePool cleanup finalizer.
	r.SubstrateEnabled = false
	current := runtimePoolTestGetPool(t, r, pool)
	if err := r.Delete(context.Background(), &current); err != nil {
		t.Fatalf("delete pool: %v", err)
	}
	templateDeletionObservedBeforePoolGone := false
	for range 6 {
		result, gone, err := runtimePoolTestFinalize(r, pool)
		if err != nil {
			t.Fatalf("finalize reconcile: %v", err)
		}
		if gone {
			break
		}
		if substrateTestDerivedTemplate(t, r, pool) == nil {
			var deletingPool corev1alpha1.RuntimePool
			if getErr := r.Get(context.Background(), types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, &deletingPool); getErr == nil {
				templateDeletionObservedBeforePoolGone = true
			} else if !apierrors.IsNotFound(getErr) {
				t.Fatalf("get deleting pool after template deletion: %v", getErr)
			}
		}
		if result.RequeueAfter == 0 {
			break
		}
	}
	if len(control.deleted) == 0 {
		t.Fatal("finalizer did not delete the provider actor")
	}
	if substrateTestDerivedTemplate(t, r, pool) != nil {
		t.Fatal("derived template survived finalization")
	}
	if !templateDeletionObservedBeforePoolGone {
		t.Fatal("RuntimePool finalizer did not wait for ActorTemplate deletion to be observed")
	}
	var secrets corev1.SecretList
	if err := r.List(context.Background(), &secrets, nil...); err == nil {
		for i := range secrets.Items {
			if secrets.Items[i].Labels[runtimePoolUIDLabel] == string(pool.UID) {
				t.Fatalf("pool Secret %q survived finalization", secrets.Items[i].Name)
			}
		}
	}
	var got corev1alpha1.RuntimePool
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, &got); !apierrors.IsNotFound(err) {
		t.Fatalf("pool still present after finalization: %v", err)
	}
}

func TestSubstrateRuntimePoolFinalizerDeletesPoliciesWithoutTemplates(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	derived := substrateTestDerivedTemplate(t, r, pool)
	if derived == nil {
		t.Fatal("derived template was not materialized")
	}
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)
	var policies networkingv1.NetworkPolicyList
	if err := r.List(context.Background(), &policies, client.MatchingLabels{runtimePoolUIDLabel: string(pool.UID)}); err != nil {
		t.Fatalf("list RuntimePool policies: %v", err)
	}
	if len(policies.Items) != 5 {
		t.Fatalf("RuntimePool policy count = %d, want 5", len(policies.Items))
	}
	current := runtimePoolTestGetPool(t, r, pool)
	wantPolicyNamespaces := substrateTestWorkerNamespace + "," + runtimePoolDefaultControllerNamespace
	if got := current.Annotations[substrateNetworkPolicyNamespacesAnnotation]; got != wantPolicyNamespaces {
		t.Fatalf("recorded NetworkPolicy namespaces = %q, want %s", got, wantPolicyNamespaces)
	}
	r.APIReader = &substrateNamespaceScopedNetworkPolicyReader{Reader: r.Client}
	delete(control.actors, substrateTestActorID(pool))
	if err := r.Delete(context.Background(), derived); err != nil {
		t.Fatalf("delete derived template before finalization: %v", err)
	}
	if err := r.Delete(context.Background(), substrateTestBaseTemplate()); err != nil {
		t.Fatalf("delete base template before finalization: %v", err)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if err := r.Delete(context.Background(), &current); err != nil {
		t.Fatalf("delete RuntimePool: %v", err)
	}
	for range 6 {
		result, gone, err := runtimePoolTestFinalize(r, pool)
		if err != nil {
			t.Fatalf("finalize reconcile: %v", err)
		}
		if gone || result.RequeueAfter == 0 {
			break
		}
	}
	policies = networkingv1.NetworkPolicyList{}
	if err := r.List(context.Background(), &policies, client.MatchingLabels{runtimePoolUIDLabel: string(pool.UID)}); err != nil {
		t.Fatalf("list RuntimePool policies after finalization: %v", err)
	}
	if len(policies.Items) != 0 {
		t.Fatalf("RuntimePool policies survived missing-template finalization: %#v", policies.Items)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), &corev1alpha1.RuntimePool{}); !apierrors.IsNotFound(err) {
		t.Fatalf("RuntimePool survived finalization: %v", err)
	}
}

func TestSubstrateRuntimePoolFinalizerToleratesRemovedProviderCRD(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	delete(control.actors, substrateTestActorID(pool))
	r.APIReader = &substrateTemplateErrorReader{
		Reader: r.Client,
		err: &meta.NoKindMatchError{
			GroupKind:        substrateActorTemplateGVK.GroupKind(),
			SearchedVersions: []string{substrateActorTemplateGVK.Version},
		},
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if err := r.Delete(context.Background(), &current); err != nil {
		t.Fatalf("delete RuntimePool: %v", err)
	}
	for range 8 {
		result, gone, err := runtimePoolTestFinalize(r, pool)
		if err != nil {
			t.Fatalf("finalize after provider CRD removal: %v", err)
		}
		if gone || result.RequeueAfter == 0 {
			break
		}
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), &corev1alpha1.RuntimePool{}); !apierrors.IsNotFound(err) {
		t.Fatalf("RuntimePool survived provider CRD removal cleanup: %v", err)
	}
}

func TestSubstrateRuntimePoolFinalizerPreservesForeignActorTemplate(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	derived := substrateTestDerivedTemplate(t, r, pool)
	derived.SetLabels(map[string]string{runtimePoolManagedByLabel: substrateTestAttackerManagedBy})
	if err := r.Update(context.Background(), derived); err != nil {
		t.Fatalf("replace derived template ownership: %v", err)
	}
	delete(control.actors, runtimePoolSubstrateActorID(runtimePoolResourceName(pool.Namespace, pool.Name)))

	current := runtimePoolTestGetPool(t, r, pool)
	if err := r.Delete(context.Background(), &current); err != nil {
		t.Fatalf("delete pool: %v", err)
	}
	var err error
	for range 4 {
		_, _, err = runtimePoolTestFinalize(r, pool)
		if err != nil {
			break
		}
	}
	if err == nil || !strings.Contains(err.Error(), "exact RuntimePool ownership identity") {
		t.Fatalf("finalize error = %v, want ownership rejection", err)
	}
	if substrateTestDerivedTemplate(t, r, pool) == nil {
		t.Fatal("foreign same-name ActorTemplate was deleted")
	}
	got := runtimePoolTestGetPool(t, r, pool)
	if !controllerutil.ContainsFinalizer(&got, runtimePoolFinalizer) {
		t.Fatal("RuntimePool finalizer was removed after ownership rejection")
	}
}

func TestSubstrateRuntimePoolFinalizerDrainsLiveActorBeforeTeardown(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-finalizer", false)
	runtimePoolReconcile(t, r, pool)

	current := runtimePoolTestGetPool(t, r, pool)
	if err := r.Delete(context.Background(), &current); err != nil {
		t.Fatalf("delete live Substrate RuntimePool: %v", err)
	}
	runtimePoolReconcile(t, r, pool)
	if supervisor.drainCalls != 1 {
		t.Fatalf("finalizer drain calls = %d, want 1", supervisor.drainCalls)
	}
	if len(control.settled) != 0 || len(control.deleted) != 0 {
		t.Fatalf("live Actor was torn down before finalizer drain quiescence: settled=%v deleted=%v", control.settled, control.deleted)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: substrateTestWorkerNamespace, Name: substrateTestWorkerPodName}, &corev1.Pod{}); err != nil {
		t.Fatalf("worker Pod was removed before finalizer drain quiescence: %v", err)
	}

	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-finalizer", true)
	runtimePoolReconcile(t, r, pool)
	if got := runtimePoolTestGetPool(t, r, pool).Status.Lifecycle; got != corev1alpha1.RuntimePoolLifecycleQuiescent {
		t.Fatalf("finalizer lifecycle after quiescent probe = %s, want Quiescent", got)
	}
	for range 10 {
		result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
		if err != nil {
			t.Fatalf("finish drained Substrate finalization: %v", err)
		}
		if result.RequeueAfter == 0 {
			break
		}
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), &corev1alpha1.RuntimePool{}); !apierrors.IsNotFound(err) {
		t.Fatalf("drained Substrate RuntimePool survived finalization: %v", err)
	}
	if len(control.deleted) != 1 {
		t.Fatalf("deleted actors = %v, want one after authenticated finalizer drain", control.deleted)
	}
}

func TestSubstrateRouteHTTPTransportDialsRouterPreservingHost(t *testing.T) {
	seenHost := ""
	seenPath := ""
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost = r.Host
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer router.Close()

	transport, err := substrateRouteHTTPTransport(router.URL+"/substrate", substrateTestActorDNSSuffix)
	if err != nil {
		t.Fatalf("substrateRouteHTTPTransport: %v", err)
	}
	httpClient := &http.Client{Transport: transport}
	routeHost := "orka-acp-actor." + substrateTestActorDNSSuffix
	resp, err := httpClient.Get("http://" + routeHost + "/v2/health")
	if err != nil {
		t.Fatalf("router-dialed request failed: %v", err)
	}
	_ = resp.Body.Close()
	if seenHost != routeHost {
		t.Fatalf("router saw Host %q, want logical route host %q", seenHost, routeHost)
	}
	if seenPath != "/substrate/v2/health" {
		t.Fatalf("router saw path %q, want configured base path plus actor request path", seenPath)
	}
	uppercaseSuffixTransport, err := substrateRouteHTTPTransport(router.URL+"/substrate/", strings.ToUpper(substrateTestActorDNSSuffix))
	if err != nil {
		t.Fatalf("uppercase DNS suffix transport: %v", err)
	}
	uppercaseResponse, err := (&http.Client{Transport: uppercaseSuffixTransport}).Get("http://" + routeHost + "/v2/health")
	if err != nil {
		t.Fatalf("case-insensitive actor DNS suffix route failed: %v", err)
	}
	_ = uppercaseResponse.Body.Close()

	if _, err := httpClient.Get("http://not-an-actor.example.com/"); err == nil || !strings.Contains(err.Error(), "refuses non-actor host") {
		t.Fatalf("non-actor host error = %v, want refusal", err)
	}

	if _, err := substrateRouteHTTPTransport("", substrateTestActorDNSSuffix); err == nil {
		t.Fatal("empty router URL accepted")
	}
	if _, err := substrateRouteHTTPTransport(router.URL, " . "); err == nil {
		t.Fatal("empty DNS suffix accepted")
	}
	if parsed, _ := url.Parse(router.URL); parsed != nil && parsed.Scheme != "http" {
		t.Fatal("test router must be plain HTTP")
	}
}

func TestSubstrateRouteHTTPTransportUsesRouterTLSIdentity(t *testing.T) {
	seenHost := ""
	router := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost = r.Host
		w.WriteHeader(http.StatusNoContent)
	}))
	defer router.Close()

	roundTripper, err := substrateRouteHTTPTransport(router.URL, substrateTestActorDNSSuffix)
	if err != nil {
		t.Fatalf("substrateRouteHTTPTransport: %v", err)
	}
	routed, ok := roundTripper.(*substrateRouteRoundTripper)
	if !ok {
		t.Fatalf("transport type = %T, want substrate route transport", roundTripper)
	}
	roots := x509.NewCertPool()
	roots.AddCert(router.Certificate())
	routed.transport.TLSClientConfig.RootCAs = roots

	routeHost := "orka-acp-actor." + substrateTestActorDNSSuffix
	resp, err := (&http.Client{Transport: routed}).Get("http://" + routeHost + "/v2/health")
	if err != nil {
		t.Fatalf("TLS router-dialed request failed: %v", err)
	}
	_ = resp.Body.Close()
	if seenHost != routeHost {
		t.Fatalf("TLS router saw Host %q, want logical route host %q", seenHost, routeHost)
	}
}

func TestRuntimePoolInstanceEndpoint(t *testing.T) {
	plain := runtimePoolTestObject(1)
	pod := runtimePoolReadyPod(plain, plain.Namespace, "pod", "pod-uid", "10.0.0.9")
	if got := runtimePoolInstanceEndpoint(plain, &pod); got != "http://10.0.0.9:8080" {
		t.Fatalf("plain endpoint = %q, want exact Pod dial", got)
	}
	substrate := runtimePoolSubstrateTestObject()
	routed := substrateTestProbePod(substrate)
	if got := runtimePoolInstanceEndpoint(substrate, &routed); got != "http://"+substrateTestRouteHost(substrate) {
		t.Fatalf("substrate endpoint = %q, want route host", got)
	}
}

// Recycling an actor whose cold resume consumed the suspension consent but
// never completed admission is terminal: the DurableDir being destroyed is
// the only copy of the preserved session data.
func TestRecycleSubstrateActorDuringResumeRecordsTerminalLoss(t *testing.T) {
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, &fakeRuntimePoolSupervisorClient{}, control)
	actorID := substrateTestActorID(pool)
	derivedName := runtimePoolSubstrateTemplateName(runtimePoolResourceName(pool.Namespace, pool.Name))
	if _, err := control.CreateActor(context.Background(), actorID, substrateTestTemplateNamespace, derivedName); err != nil {
		t.Fatalf("create actor: %v", err)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[substrateActorResumingAnnotation] = actorID
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("record resume in progress: %v", err)
	}

	// The teardown spans reconciles; the terminal loss must be recorded
	// before any destruction stage runs.
	if err := r.recycleSubstrateActor(context.Background(), &current, control, actorID); err != nil {
		t.Fatalf("recycle: %v", err)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatalf("recycling a resuming actor must record terminal loss, annotations=%v", current.Annotations)
	}
}

// Recycling an actor whose consensual suspension consent still stands (for
// example through the integrity-triggered recycle that runs before the resume
// handler) is equally terminal.
func TestRecycleSubstrateActorWithStandingConsentRecordsTerminalLoss(t *testing.T) {
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, &fakeRuntimePoolSupervisorClient{}, control)
	actorID := substrateTestActorID(pool)
	derivedName := runtimePoolSubstrateTemplateName(runtimePoolResourceName(pool.Namespace, pool.Name))
	if _, err := control.CreateActor(context.Background(), actorID, substrateTestTemplateNamespace, derivedName); err != nil {
		t.Fatalf("create actor: %v", err)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[substrateActorSuspendedAnnotation] = actorID
	// Consent is honored only on a suspend-capable binding.
	current.Spec.ExecutionWorkspace.Substrate.SuspendMode = "DataOnly"
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("record suspension consent: %v", err)
	}
	if err := r.recycleSubstrateActor(context.Background(), &current, control, actorID); err != nil {
		t.Fatalf("recycle: %v", err)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatalf("recycling a consensually suspended actor must record terminal loss, annotations=%v", current.Annotations)
	}
}
