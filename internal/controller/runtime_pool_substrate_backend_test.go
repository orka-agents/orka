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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/workspace"
)

const (
	substrateTestStatusSuspended   = "STATUS_SUSPENDED"
	substrateTestTemplateNamespace = "ate-demo"
	substrateTestBaseTemplateName  = "orka-codex-infra"
	substrateTestActorDNSSuffix    = "actors.test.example"
)

type fakeSubstrateActorControl struct {
	actors  map[string]*workspace.SubstrateRuntimeActor
	created []string
	resumed []string
	boots   []bool
	settled []string
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
		Status: substrateTestStatusSuspended,
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

func (f *fakeSubstrateActorControl) SettleActor(_ context.Context, actorID string) (*workspace.SubstrateRuntimeActor, error) {
	f.settled = append(f.settled, actorID)
	actor, ok := f.actors[actorID]
	if !ok {
		return nil, fmt.Errorf("settle: actor %s not found", actorID)
	}
	actor.Status = substrateTestStatusSuspended
	view := *actor
	return &view, nil
}

func (f *fakeSubstrateActorControl) DeleteActor(_ context.Context, actorID string) error {
	if actor, ok := f.actors[actorID]; ok && actor.Status != substrateTestStatusSuspended {
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
	r.SubstrateCredentialSeeder = func(_ context.Context, routeHost, nonce string, request harnessv2.CredentialBootstrapRequest) error {
		if routeHost == "" || nonce == "" {
			return fmt.Errorf("seeder called without route host or nonce")
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
}

func TestSubstrateRuntimePoolRecyclesActorWithUnexpectedTemplateBeforeBootstrap(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	actorID := substrateTestActorID(pool)
	control.actors[actorID] = &workspace.SubstrateRuntimeActor{
		ActorID:           actorID,
		TemplateNamespace: "attacker-owned",
		TemplateName:      "credential-capture",
		Status:            "STATUS_RUNNING",
		PodNamespace:      "ate-workers",
		PodName:           "worker-0",
	}
	seedAttempts := 0
	r.SubstrateCredentialSeeder = func(context.Context, string, string, harnessv2.CredentialBootstrapRequest) error {
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
	if len(control.settled) != 1 || control.settled[0] != actorID {
		t.Fatalf("settled actors = %v, want the untrusted actor recycled", control.settled)
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
	if got.Annotations[substrateActorRecyclingAnnotation] != actorID {
		t.Fatalf("recycling annotation = %q, want %q", got.Annotations[substrateActorRecyclingAnnotation], actorID)
	}

	runtimePoolReconcile(t, r, pool)
	if len(control.deleted) != 1 || control.deleted[0] != actorID {
		t.Fatalf("deleted actors = %v, want the untrusted actor deleted", control.deleted)
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
	squatted.SetLabels(map[string]string{runtimePoolManagedByLabel: "attacker"})
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
	r.SubstrateCredentialSeeder = func(context.Context, string, string, harnessv2.CredentialBootstrapRequest) error {
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
	container["image"] = "attacker.invalid/runtime:latest"
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
	if len(control.settled) != 1 {
		t.Fatalf("settled actors = %v, want tampered-template actor recycled", control.settled)
	}
	got := runtimePoolTestGetPool(t, r, pool)
	if got.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		!strings.Contains(got.Status.Message, "contents do not match their declared revision") {
		t.Fatalf("status = %s/%q, want template-integrity rejection", got.Status.Lifecycle, got.Status.Message)
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
		"ORKA_ACP_CONTROLLER_TOKEN_FILE", "ORKA_ACP_CAPABILITY_SECRET_FILE", "ORKA_ACP_PROVIDER_TOKEN_FILE",
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
	if len(container.Command) != 1 || container.Command[0] != "/usr/local/bin/orka-acp-runtime" {
		t.Fatalf("derived container command = %v, want the explicit runtime entrypoint (the provider does not read image config)", container.Command)
	}
	var templateSecrets corev1.SecretList
	if err := r.List(context.Background(), &templateSecrets, client.InNamespace(substrateTestTemplateNamespace)); err == nil && len(templateSecrets.Items) != 0 {
		t.Fatalf("template namespace holds %d Secrets; nothing secret may exist there", len(templateSecrets.Items))
	}
	if workerPool, _, _ := unstructured.NestedString(derived.Object, "spec", "workerPoolRef", "name"); workerPool != "orka-workers" {
		t.Fatalf("derived template workerPoolRef = %q, want operator infrastructure copied", workerPool)
	}
	if location, _, _ := unstructured.NestedString(derived.Object, "spec", "snapshotsConfig", "location"); location != "gs://ate-snapshots/orka" {
		t.Fatalf("derived template snapshotsConfig = %q, want operator infrastructure copied (safe: the golden-built instance boots credential-free)", location)
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
	if strings.Contains(active.PodUID, actorID) || strings.Contains(active.RuntimeInstanceID, actorID) {
		t.Fatalf("public active instance leaked raw provider actor ID %q: %#v", actorID, active)
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
	r.SubstrateCredentialSeeder = func(context.Context, string, string, harnessv2.CredentialBootstrapRequest) error {
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
}

func TestSubstrateRuntimePoolHoldsAdmissionUntilCredentialSeeding(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	seedAttempts := 0
	r.SubstrateCredentialSeeder = func(_ context.Context, routeHost, nonce string, request harnessv2.CredentialBootstrapRequest) error {
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
}

func TestSubstrateTeardownDestroysLabeledWorkerPodBeforeSettling(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	runtimePoolReconcile(t, r, pool)
	actorID := substrateTestActorID(pool)

	worker := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ate-workers", Name: "worker-0",
			Labels: map[string]string{"ate.dev/worker-pool": "orka-workers"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "ateom", Image: "example.com/ateom"}}},
	}
	if err := r.Create(context.Background(), worker); err != nil {
		t.Fatalf("create worker pod: %v", err)
	}

	gone, err := r.teardownSubstrateActor(context.Background(), pool, control, actorID)
	if err != nil || gone {
		t.Fatalf("teardown with live worker = (%v, %v), want in-progress", gone, err)
	}
	if len(control.settled) != 0 {
		t.Fatal("actor settled while its workload memory still existed")
	}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "ate-workers", Name: "worker-0"}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("worker pod get after teardown = %v, want deleted", err)
	}

	gone, err = r.teardownSubstrateActor(context.Background(), pool, control, actorID)
	if err != nil || gone {
		t.Fatalf("teardown after workload destruction = (%v, %v), want settling", gone, err)
	}
	if len(control.settled) != 1 {
		t.Fatalf("settled = %v, want the memoryless actor settled", control.settled)
	}
	gone, err = r.teardownSubstrateActor(context.Background(), pool, control, actorID)
	if err != nil || !gone {
		t.Fatalf("final teardown = (%v, %v), want deleted", gone, err)
	}
	if len(control.deleted) != 1 {
		t.Fatalf("deleted = %v, want the settled actor deleted", control.deleted)
	}
}

func TestSubstrateTeardownRefusesUnlabeledPod(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	runtimePoolReconcile(t, r, pool)
	actorID := substrateTestActorID(pool)

	bystander := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ate-workers", Name: "worker-0"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "example.com/app"}}},
	}
	if err := r.Create(context.Background(), bystander); err != nil {
		t.Fatalf("create bystander pod: %v", err)
	}
	if _, err := r.teardownSubstrateActor(context.Background(), pool, control, actorID); err == nil ||
		!strings.Contains(err.Error(), substrateWorkerPoolLabel) {
		t.Fatalf("teardown error = %v, want refusal to delete an unlabeled Pod", err)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "ate-workers", Name: "worker-0"}, &corev1.Pod{}); err != nil {
		t.Fatalf("bystander pod must survive: %v", err)
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
			wantError: "placement is unknown",
		},
		{
			name: "wrong worker namespace",
			mutate: func(actor *workspace.SubstrateRuntimeActor) {
				actor.PodNamespace = "other-workers"
			},
			wantError: "does not match infrastructure WorkerPool namespace",
		},
		{
			name:   "wrong worker pool label",
			mutate: func(*workspace.SubstrateRuntimeActor) {},
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Namespace: "ate-workers", Name: "worker-0",
				Labels: map[string]string{substrateWorkerPoolLabel: "other-workers"},
			}},
			wantError: "does not match infrastructure WorkerPool orka-workers",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			supervisor := &fakeRuntimePoolSupervisorClient{}
			control := newFakeSubstrateActorControl()
			r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
			runtimePoolReconcile(t, r, pool)
			actorID := substrateTestActorID(pool)
			tt.mutate(control.actors[actorID])
			if tt.pod != nil {
				if err := r.Create(context.Background(), tt.pod.DeepCopy()); err != nil {
					t.Fatalf("create worker Pod: %v", err)
				}
			}

			if _, err := r.teardownSubstrateActor(context.Background(), pool, control, actorID); err == nil ||
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
	// Staged teardown: settle the memoryless actor, then delete it.
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
