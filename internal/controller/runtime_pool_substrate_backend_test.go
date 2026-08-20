/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workspace"
)

const (
	substrateTestTemplateNamespace = "ate-demo"
	substrateTestBaseTemplateName  = "orka-codex-infra"
	substrateTestActorDNSSuffix    = "actors.test.example"
)

type fakeSubstrateActorControl struct {
	actors  map[string]*workspace.SubstrateRuntimeActor
	created []string
	resumed []string
	boots   []bool
	deleted []string
	closed  int
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
		Status: "STATUS_SUSPENDED",
	}
	f.actors[actorID] = actor
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
	actor.PodNamespace = "ate-workers"
	actor.PodName = "worker-0"
	actor.PodIP = "10.99.0.5"
	view := *actor
	return &view, nil
}

func (f *fakeSubstrateActorControl) DeleteActor(_ context.Context, actorID string) error {
	f.deleted = append(f.deleted, actorID)
	delete(f.actors, actorID)
	return nil
}

func (f *fakeSubstrateActorControl) Close() error {
	f.closed++
	return nil
}

func runtimePoolSubstrateTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtimePoolTestScheme(t)
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
		"spec": map[string]any{
			"workerPoolRef":   map[string]any{"namespace": "ate-workers", "name": "orka-workers"},
			"snapshotsConfig": map[string]any{"location": "gs://ate-snapshots/orka"},
			"runsc":           map[string]any{"amd64": map[string]any{"url": "https://example.invalid/runsc"}},
			"containers": []any{map[string]any{
				"name": "operator-base", "image": "example.com/operator@sha256:" + strings.Repeat("1", 64),
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
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool, substrateTestBaseTemplate())
	r.SubstrateEnabled = true
	r.SubstrateConfig = SubstrateConfig{
		APIEndpoint:           "api.ate-system.svc:443",
		APIInsecureSkipVerify: true,
		RouterURL:             "http://atenet-router.ate-system.svc",
		ActorDNSSuffix:        substrateTestActorDNSSuffix,
	}
	r.SubstrateActorControlFactory = func(SubstrateConfig) (workspace.SubstrateRuntimeActorControl, error) {
		return control, nil
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
// instance UID actor:<id> with the route host as its address.
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

func TestSubstrateRuntimePoolMaterializesDerivedTemplateAndActor(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)

	actorID := substrateTestActorID(pool)
	if len(control.created) != 1 || control.created[0] != actorID {
		t.Fatalf("created actors = %v, want exactly %q", control.created, actorID)
	}
	if len(control.resumed) != 1 || len(control.boots) != 1 || !control.boots[0] {
		t.Fatalf("resume calls = %v boots = %v, want one fresh boot", control.resumed, control.boots)
	}

	derived := substrateTestDerivedTemplate(t, r, pool)
	if derived == nil {
		t.Fatal("derived ActorTemplate was not created")
	}
	deployed, err := substrateTemplatePodTemplateSpec(derived)
	if err != nil {
		t.Fatalf("reconstruct deployed template: %v", err)
	}
	container := deployed.Spec.Containers[0]
	if container.Image != pool.Spec.Runtime.Image {
		t.Fatalf("derived container image = %q, want immutable runtime image", container.Image)
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
	for _, forbidden := range []string{"ORKA_ACP_CONTROLLER_TOKEN_FILE", "ORKA_ACP_CAPABILITY_SECRET_FILE", "ORKA_ACP_PROVIDER_TOKEN_FILE"} {
		if _, present := env[forbidden]; present {
			t.Fatalf("derived template carries file-mounted secret env %q", forbidden)
		}
	}
	for _, bootstrap := range []string{"ORKA_ACP_CONTROLLER_TOKEN_BOOTSTRAP", "ORKA_ACP_CAPABILITY_SECRET_BOOTSTRAP", "ORKA_ACP_PROVIDER_TOKEN_BOOTSTRAP"} {
		item, present := env[bootstrap]
		if !present || item.ValueFrom == nil || item.ValueFrom.SecretKeyRef == nil || item.Value != "" {
			t.Fatalf("derived template bootstrap env %q = %#v, want secretKeyRef-only", bootstrap, item)
		}
		secret := &corev1.Secret{}
		if err := r.Get(context.Background(), types.NamespacedName{
			Namespace: substrateTestTemplateNamespace, Name: item.ValueFrom.SecretKeyRef.Name,
		}, secret); err != nil {
			t.Fatalf("bootstrap Secret %q missing in template namespace: %v", item.ValueFrom.SecretKeyRef.Name, err)
		}
	}
	if workerPool, _, _ := unstructured.NestedString(derived.Object, "spec", "workerPoolRef", "name"); workerPool != "orka-workers" {
		t.Fatalf("derived template workerPoolRef = %q, want operator infrastructure copied", workerPool)
	}
	if location, _, _ := unstructured.NestedString(derived.Object, "spec", "snapshotsConfig", "location"); location != "gs://ate-snapshots/orka" {
		t.Fatalf("derived template snapshotsConfig = %q, want operator infrastructure copied", location)
	}

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
	if active.PodNamespace != "ate-workers" || active.PodName != "worker-0" {
		t.Fatalf("ActiveInstance placement = %s/%s, want provider worker placement", active.PodNamespace, active.PodName)
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
	control.actors[actorID].Status = "STATUS_SUSPENDED"
	control.actors[actorID].SnapshotObserved = true

	runtimePoolReconcile(t, r, pool)
	if len(control.deleted) != 1 || control.deleted[0] != actorID {
		t.Fatalf("deleted actors = %v, want the suspended actor recycled", control.deleted)
	}
	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded || got.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
		t.Fatalf("status = %s/%s, want Degraded/Closed", got.Status.Lifecycle, got.Status.AdmissionState)
	}
	if !strings.Contains(got.Status.Message, "suspension is prohibited") {
		t.Fatalf("message = %q, want suspension prohibition", got.Status.Message)
	}
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

	current := runtimePoolTestGetPool(t, r, pool)
	current.Spec.DesiredReplicas = 0
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
	runtimePoolReconcile(t, r, pool)
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

func TestSubstrateRuntimePoolFinalizerDeletesActorTemplateAndSecrets(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)

	runtimePoolReconcile(t, r, pool)
	if substrateTestDerivedTemplate(t, r, pool) == nil {
		t.Fatal("derived template was not materialized")
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if err := r.Delete(context.Background(), &current); err != nil {
		t.Fatalf("delete pool: %v", err)
	}
	for range 5 {
		result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}})
		if err != nil {
			t.Fatalf("finalize reconcile: %v", err)
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
	var secrets corev1.SecretList
	if err := r.List(context.Background(), &secrets, nil...); err == nil {
		for i := range secrets.Items {
			if secrets.Items[i].Namespace == substrateTestTemplateNamespace &&
				secrets.Items[i].Labels[runtimePoolUIDLabel] == string(pool.UID) {
				t.Fatalf("template-namespace Secret %q survived finalization", secrets.Items[i].Name)
			}
		}
	}
	var got corev1alpha1.RuntimePool
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, &got); !apierrors.IsNotFound(err) {
		t.Fatalf("pool still present after finalization: %v", err)
	}
}

func TestSubstrateRouteHTTPTransportDialsRouterPreservingHost(t *testing.T) {
	seenHost := ""
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost = r.Host
		w.WriteHeader(http.StatusNoContent)
	}))
	defer router.Close()

	transport, err := substrateRouteHTTPTransport(router.URL, substrateTestActorDNSSuffix)
	if err != nil {
		t.Fatalf("substrateRouteHTTPTransport: %v", err)
	}
	client := &http.Client{Transport: transport}
	routeHost := "orka-acp-actor." + substrateTestActorDNSSuffix
	resp, err := client.Get("http://" + routeHost + "/v2/health")
	if err != nil {
		t.Fatalf("router-dialed request failed: %v", err)
	}
	_ = resp.Body.Close()
	if seenHost != routeHost {
		t.Fatalf("router saw Host %q, want logical route host %q", seenHost, routeHost)
	}

	if _, err := client.Get("http://not-an-actor.example.com/"); err == nil || !strings.Contains(err.Error(), "refuses non-actor host") {
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
