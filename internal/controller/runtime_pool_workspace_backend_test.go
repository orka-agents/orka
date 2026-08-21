/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	sandboxextv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

func runtimePoolWorkspaceTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtimePoolTestScheme(t)
	if err := sandboxextv1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme sandbox extensions: %v", err)
	}
	return scheme
}

func runtimePoolWorkspaceTestObject() *corev1alpha1.RuntimePool {
	pool := runtimePoolTestObject(1)
	pool.Name = "acp-ws-codex-0123456789abcdef"
	pool.Spec.ExecutionWorkspace = &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{
		Provider:      corev1alpha1.WorkspaceProviderAgentSandbox,
		BindingDigest: "sha256:" + strings.Repeat("9", 64),
	}
	pool.Spec.Capacity = &corev1alpha1.RuntimePoolCapacitySpec{MaxResidentSessions: 1, MaxRunningPrompts: 1}
	return pool
}

func runtimePoolWorkspaceTestChildren(
	t *testing.T,
	r *RuntimePoolReconciler,
	pool *corev1alpha1.RuntimePool,
) (*sandboxextv1beta1.SandboxTemplate, *sandboxextv1beta1.SandboxWarmPool, *sandboxextv1beta1.SandboxClaim) {
	t.Helper()
	base := runtimePoolResourceName(pool.Namespace, pool.Name)
	template := &sandboxextv1beta1.SandboxTemplate{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: pool.Namespace, Name: runtimePoolSandboxTemplateName(base)}, template); err != nil {
		if apierrors.IsNotFound(err) {
			template = nil
		} else {
			t.Fatalf("Get SandboxTemplate: %v", err)
		}
	}
	warmPool := &sandboxextv1beta1.SandboxWarmPool{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: pool.Namespace, Name: runtimePoolSandboxWarmPoolName(base)}, warmPool); err != nil {
		if apierrors.IsNotFound(err) {
			warmPool = nil
		} else {
			t.Fatalf("Get SandboxWarmPool: %v", err)
		}
	}
	claim := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: pool.Namespace, Name: runtimePoolSandboxClaimName(base)}, claim); err != nil {
		if apierrors.IsNotFound(err) {
			claim = nil
		} else {
			t.Fatalf("Get SandboxClaim: %v", err)
		}
	}
	return template, warmPool, claim
}

func runtimePoolWorkspaceReadyPod(
	pool *corev1alpha1.RuntimePool,
	template *sandboxextv1beta1.SandboxTemplate,
	name, uid, ip string,
) corev1.Pod {
	pod := runtimePoolReadyPod(pool, pool.Namespace, name, uid, ip)
	pod.Labels = cloneStringMap(template.Spec.PodTemplate.ObjectMeta.Labels)
	pod.Annotations = cloneStringMap(template.Spec.PodTemplate.ObjectMeta.Annotations)
	return pod
}

func TestWorkspaceRuntimePoolMaterializesProviderWorkload(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolWorkspaceTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)

	runtimePoolReconcile(t, r, pool)

	base := runtimePoolResourceName(pool.Namespace, pool.Name)
	template, warmPool, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if template == nil || warmPool == nil || claim == nil {
		t.Fatalf("workspace children template/warmPool/claim = %v/%v/%v, want all present", template != nil, warmPool != nil, claim != nil)
	}
	if template.Spec.NetworkPolicyManagement != sandboxextv1beta1.NetworkPolicyManagementUnmanaged {
		t.Fatalf("sandbox template network policy management = %q, want Unmanaged (the pool NetworkPolicies own the boundary)", template.Spec.NetworkPolicyManagement)
	}
	if template.Spec.EnvVarsInjectionPolicy != sandboxextv1beta1.EnvVarsInjectionPolicyDisallowed ||
		template.Spec.VolumeClaimTemplatesPolicy != sandboxextv1beta1.VolumeClaimTemplatesPolicyDisallowed {
		t.Fatalf("sandbox template injection policies = %q/%q, want Disallowed/Disallowed", template.Spec.EnvVarsInjectionPolicy, template.Spec.VolumeClaimTemplatesPolicy)
	}
	podSpec := template.Spec.PodTemplate.Spec
	assertRuntimePoolPodHardening(t, podSpec)
	assertRuntimePoolContainerHardening(t, podSpec.Containers[0])
	assertRuntimePoolBoundedVolumes(t, podSpec.Volumes)
	assertRuntimePoolEnvironment(t, r, pool, podSpec.Containers[0].Env)
	if template.Spec.PodTemplate.ObjectMeta.Labels[runtimePoolKeyLabel] != runtimePoolKey(pool.Namespace, pool.Name) {
		t.Fatal("sandbox template Pod labels do not carry the pool selector label")
	}
	if strings.TrimSpace(template.Spec.PodTemplate.ObjectMeta.Annotations[runtimePoolTemplateRevisionAnnotation]) == "" {
		t.Fatal("sandbox template Pod annotations do not carry the template revision")
	}
	if template.Spec.PodTemplate.ObjectMeta.Annotations[runtimePoolProfileAnnotation] != pool.Spec.Runtime.Profile.Digest {
		t.Fatal("sandbox template Pod annotations do not carry the immutable profile digest")
	}

	if got := ptr.Deref(warmPool.Spec.Replicas, -1); got != 0 {
		t.Fatalf("sandbox warm pool replicas = %d, want 0 (claims always cold-start from the exact template)", got)
	}
	if warmPool.Spec.TemplateRef.Name != runtimePoolSandboxTemplateName(base) {
		t.Fatalf("sandbox warm pool templateRef = %q, want %q", warmPool.Spec.TemplateRef.Name, runtimePoolSandboxTemplateName(base))
	}
	if claim.Spec.WarmPoolRef.Name != runtimePoolSandboxWarmPoolName(base) {
		t.Fatalf("sandbox claim warmPoolRef = %q, want %q", claim.Spec.WarmPoolRef.Name, runtimePoolSandboxWarmPoolName(base))
	}
	if len(claim.Spec.Env) != 0 || len(claim.Spec.VolumeClaimTemplates) != 0 {
		t.Fatal("sandbox claim must not inject env or volumes; credentials never cross the provider API")
	}

	var deployment appsv1.Deployment
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: pool.Namespace, Name: base}, &deployment); !apierrors.IsNotFound(err) {
		t.Fatalf("workspace-backed pool created a Deployment (err=%v); the provider owns the workload", err)
	}

	status := runtimePoolTestGetPool(t, r, pool).Status
	if status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStarting || status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
		t.Fatalf("status = %s/%s, want Starting/Closed", status.Lifecycle, status.AdmissionState)
	}
}

func TestWorkspaceRuntimePoolRejectsUnownedProviderChildren(t *testing.T) {
	tests := []struct {
		name   string
		object func() client.Object
	}{
		{name: "template", object: func() client.Object { return &sandboxextv1beta1.SandboxTemplate{} }},
		{name: "warm pool", object: func() client.Object { return &sandboxextv1beta1.SandboxWarmPool{} }},
		{name: "claim", object: func() client.Object { return &sandboxextv1beta1.SandboxClaim{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtimePoolWorkspaceTestScheme(t)
			pool := runtimePoolWorkspaceTestObject()
			base := runtimePoolResourceName(pool.Namespace, pool.Name)
			object := tt.object()
			object.SetNamespace(pool.Namespace)
			switch object.(type) {
			case *sandboxextv1beta1.SandboxTemplate:
				object.SetName(runtimePoolSandboxTemplateName(base))
			case *sandboxextv1beta1.SandboxWarmPool:
				object.SetName(runtimePoolSandboxWarmPoolName(base))
			case *sandboxextv1beta1.SandboxClaim:
				object.SetName(runtimePoolSandboxClaimName(base))
			}
			r := runtimePoolTestReconciler(t, scheme, &fakeRuntimePoolSupervisorClient{}, pool, object)

			runtimePoolReconcile(t, r, pool)

			status := runtimePoolTestGetPool(t, r, pool).Status
			if status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
				status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed ||
				!strings.Contains(status.Message, "ownership identity") {
				t.Fatalf("unowned %s status = %s/%s %q, want Degraded/Closed ownership failure", tt.name, status.Lifecycle, status.AdmissionState, status.Message)
			}
		})
	}
}

func TestWorkspaceRuntimePoolRecyclesClaimBeforeRepairingTamperedTemplate(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolWorkspaceTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)

	runtimePoolReconcile(t, r, pool)
	template, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if template == nil || claim == nil {
		t.Fatal("workspace children were not materialized")
	}
	template.Spec.PodTemplate.Spec.Containers[0].Image = runtimePoolTestTamperedImage
	if err := r.Update(context.Background(), template); err != nil {
		t.Fatalf("tamper SandboxTemplate: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	if supervisor.probeCalls != 0 {
		t.Fatalf("tampered template triggered %d authenticated probes before claim recycling", supervisor.probeCalls)
	}
	if _, _, claim = runtimePoolWorkspaceTestChildren(t, r, pool); claim != nil {
		t.Fatal("tampered SandboxTemplate claim was not recycled")
	}
	status := runtimePoolTestGetPool(t, r, pool).Status
	if status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopping || status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
		t.Fatalf("tampered template status = %s/%s, want Stopping/Closed", status.Lifecycle, status.AdmissionState)
	}

	runtimePoolReconcile(t, r, pool)
	template, _, claim = runtimePoolWorkspaceTestChildren(t, r, pool)
	if template == nil || claim == nil {
		t.Fatal("controller did not repair the template and reacquire a claim")
	}
	if got := template.Spec.PodTemplate.Spec.Containers[0].Image; got == runtimePoolTestTamperedImage {
		t.Fatal("controller retained the tampered runtime image")
	}
}

func TestWorkspaceRuntimePoolColdStartTimeout(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolWorkspaceTestObject()
	pool.Spec.ColdStartTimeoutSeconds = 5
	now := runtimePoolTestNow
	r := runtimePoolTestReconciler(t, scheme, &fakeRuntimePoolSupervisorClient{}, pool)
	r.Now = func() time.Time { return now }

	runtimePoolReconcile(t, r, pool)
	now = now.Add(6 * time.Second)
	runtimePoolReconcile(t, r, pool)

	status := runtimePoolTestGetPool(t, r, pool).Status
	condition := meta.FindStatusCondition(status.Conditions, corev1alpha1.RuntimePoolConditionRolloutReady)
	if status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		condition == nil || condition.Reason != runtimePoolRolloutReasonTimedOut {
		t.Fatalf("cold-start status/condition = %s/%#v, want Degraded/RolloutTimedOut", status.Lifecycle, condition)
	}
}

func TestWorkspaceRuntimePoolPreservesExplicitClaimFailure(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolWorkspaceTestObject()
	r := runtimePoolTestReconciler(t, scheme, &fakeRuntimePoolSupervisorClient{}, pool)

	runtimePoolReconcile(t, r, pool)
	_, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if claim == nil {
		t.Fatal("workspace claim was not materialized")
	}
	claim.Status.Conditions = []metav1.Condition{{
		Type:    string(sandboxv1beta1.SandboxConditionReady),
		Status:  metav1.ConditionFalse,
		Reason:  "ProvisioningFailed",
		Message: "provider capacity exhausted",
	}}
	if err := r.Update(context.Background(), claim); err != nil {
		t.Fatalf("update failed SandboxClaim: %v", err)
	}

	runtimePoolReconcile(t, r, pool)

	status := runtimePoolTestGetPool(t, r, pool).Status
	condition := meta.FindStatusCondition(status.Conditions, corev1alpha1.RuntimePoolConditionRolloutReady)
	if status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed ||
		condition == nil || condition.Status != metav1.ConditionFalse ||
		condition.Reason != corev1alpha1.RuntimePoolReasonRolloutFailed ||
		!strings.Contains(status.Message, "provider capacity exhausted") {
		t.Fatalf("claim failure status/condition = %s/%s %q/%#v, want Degraded/Closed explicit RolloutFailed", status.Lifecycle, status.AdmissionState, status.Message, condition)
	}
}

func TestWorkspaceRuntimePoolFinalizationPreservesIsolationUntilClaimGone(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolWorkspaceTestObject()
	pool.Finalizers = []string{runtimePoolFinalizer}
	deletedAt := metav1.NewTime(runtimePoolTestNow)
	pool.DeletionTimestamp = &deletedAt
	base := runtimePoolResourceName(pool.Namespace, pool.Name)
	labels := map[string]string{runtimePoolKeyLabel: runtimePoolKey(pool.Namespace, pool.Name)}
	claim := &sandboxextv1beta1.SandboxClaim{ObjectMeta: metav1.ObjectMeta{
		Name:       runtimePoolSandboxClaimName(base),
		Namespace:  pool.Namespace,
		Finalizers: []string{"test.orka.ai/hold-provider-cleanup"},
	}}
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: runtimePoolChildName(base, "deny-all"), Namespace: pool.Namespace, Labels: labels,
	}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "pool-auth", Namespace: pool.Namespace, Labels: labels,
	}}
	r := runtimePoolTestReconciler(t, scheme, nil, pool, claim, policy, secret)

	result := runtimePoolReconcile(t, r, pool)
	if result.RequeueAfter == 0 {
		t.Fatalf("finalization result = %#v, want requeue while provider claim remains", result)
	}
	currentClaim := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), currentClaim); err != nil {
		t.Fatalf("get terminating SandboxClaim: %v", err)
	}
	if currentClaim.DeletionTimestamp.IsZero() {
		t.Fatal("provider SandboxClaim deletion was not initiated")
	}
	for _, object := range []client.Object{policy, secret} {
		check := object.DeepCopyObject().(client.Object)
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(object), check); err != nil {
			t.Fatalf("isolation child %T was removed before provider teardown completed: %v", object, err)
		}
	}
	currentPool := runtimePoolTestGetPool(t, r, pool)
	if !controllerutil.ContainsFinalizer(&currentPool, runtimePoolFinalizer) {
		t.Fatal("RuntimePool finalizer was removed before provider teardown completed")
	}
}

func TestWorkspaceRuntimePoolServesThroughSandboxPod(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolWorkspaceTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)

	runtimePoolReconcile(t, r, pool)
	template, _, _ := runtimePoolWorkspaceTestChildren(t, r, pool)
	pod := runtimePoolWorkspaceReadyPod(pool, template, "sandbox-pod", "sandbox-pod-uid", "10.0.0.71")
	runtimePoolTestCreatePod(t, r, &pod)
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "sandbox-boot", false)

	runtimePoolReconcile(t, r, pool)

	status := runtimePoolTestGetPool(t, r, pool).Status
	if status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting {
		t.Fatalf("status = %s/%s, want Serving/Accepting", status.Lifecycle, status.AdmissionState)
	}
	active := status.ActiveInstance
	if active == nil || active.PodName != "sandbox-pod" || active.PodUID != "sandbox-pod-uid" ||
		active.PodAddress != "10.0.0.71" || active.BootID != "sandbox-boot" ||
		active.RuntimeInstanceID != runtimePoolRuntimeInstanceID(pod.UID, "sandbox-boot") {
		t.Fatalf("ActiveInstance = %#v, want the exact sandbox Pod identity", active)
	}
	if status.Capacity.MaxResidentSessions != 1 || status.Capacity.MaxRunningPrompts != 1 {
		t.Fatalf("capacity = %d/%d, want 1/1 single-session workspace pool", status.Capacity.MaxResidentSessions, status.Capacity.MaxRunningPrompts)
	}
}

func TestWorkspaceRuntimePoolSupervisorRestartRecyclesClaim(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolWorkspaceTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)

	runtimePoolReconcile(t, r, pool)
	template, _, _ := runtimePoolWorkspaceTestChildren(t, r, pool)
	pod := runtimePoolWorkspaceReadyPod(pool, template, "sandbox-pod", "sandbox-pod-uid", "10.0.0.72")
	runtimePoolTestCreatePod(t, r, &pod)
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "boot-1", false)
	runtimePoolReconcile(t, r, pool)

	// The supervisor restarted in place: same Pod, different boot.
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "boot-2", false)
	runtimePoolReconcile(t, r, pool)
	status := runtimePoolTestGetPool(t, r, pool).Status
	if status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
		t.Fatalf("admission after in-place restart = %s, want Closed", status.AdmissionState)
	}
	if _, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool); claim == nil {
		t.Fatal("claim deleted before the admission closure barrier persisted")
	}

	runtimePoolReconcile(t, r, pool)
	if _, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool); claim != nil {
		t.Fatal("claim was not deleted to recycle the restarted supervisor instance")
	}
	status = runtimePoolTestGetPool(t, r, pool).Status
	if status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopping || status.ActiveInstance != nil {
		t.Fatalf("status = %s (active=%v), want Stopping with no active instance", status.Lifecycle, status.ActiveInstance)
	}
}

func TestWorkspaceRuntimePoolScaleToZeroDrainsThenDeletesClaim(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolWorkspaceTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)

	runtimePoolReconcile(t, r, pool)
	template, _, _ := runtimePoolWorkspaceTestChildren(t, r, pool)
	pod := runtimePoolWorkspaceReadyPod(pool, template, "sandbox-pod", "sandbox-pod-uid", "10.0.0.73")
	runtimePoolTestCreatePod(t, r, &pod)
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "boot-1", false)
	runtimePoolReconcile(t, r, pool)

	// Scale to zero. The spec change bumps the pool generation, so the flow
	// mirrors the Deployment path: authenticated rollout drain of the exact
	// old instance validated against the deployed template identity.
	current := runtimePoolTestGetPool(t, r, pool)
	current.Spec.DesiredReplicas = 0
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("scale pool to zero: %v", err)
	}
	runtimePoolReconcile(t, r, pool)
	if supervisor.drainCalls != 1 {
		t.Fatalf("drain calls = %d, want 1", supervisor.drainCalls)
	}
	if _, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool); claim == nil {
		t.Fatal("claim deleted before authenticated drain quiescence")
	}

	supervisor.probe = runtimePoolValidProbe(pool, &pod, "boot-1", true)
	runtimePoolReconcile(t, r, pool)
	if got := runtimePoolTestGetPool(t, r, pool).Status.Lifecycle; got != corev1alpha1.RuntimePoolLifecycleQuiescent {
		t.Fatalf("lifecycle after quiescent probe = %s, want Quiescent", got)
	}
	runtimePoolReconcile(t, r, pool)
	if _, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool); claim != nil {
		t.Fatal("claim was not deleted after the persisted quiescence barrier")
	}

	// The provider cascades the sandbox Pod; simulate its termination.
	if err := r.Delete(context.Background(), &pod); err != nil {
		t.Fatalf("delete sandbox Pod: %v", err)
	}
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	status := runtimePoolTestGetPool(t, r, pool).Status
	if status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped {
		t.Fatalf("lifecycle = %s, want Stopped", status.Lifecycle)
	}
	if status.ActiveInstance != nil {
		t.Fatal("stopped workspace pool retained an active instance")
	}
}

func TestWorkspaceRuntimePoolFinalizerDeletesProviderChildren(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolWorkspaceTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)

	runtimePoolReconcile(t, r, pool)
	if template, warmPool, claim := runtimePoolWorkspaceTestChildren(t, r, pool); template == nil || warmPool == nil || claim == nil {
		t.Fatal("workspace children were not materialized")
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if err := r.Delete(context.Background(), &current); err != nil {
		t.Fatalf("delete pool: %v", err)
	}
	// Finalization is idempotent and requeues until every child is gone.
	for range 5 {
		result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}})
		if err != nil {
			t.Fatalf("finalize reconcile: %v", err)
		}
		if result.RequeueAfter == 0 {
			break
		}
	}
	if template, warmPool, claim := runtimePoolWorkspaceTestChildren(t, r, pool); template != nil || warmPool != nil || claim != nil {
		t.Fatalf("workspace children survived finalization: template=%v warmPool=%v claim=%v", template != nil, warmPool != nil, claim != nil)
	}
	var got corev1alpha1.RuntimePool
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, &got); !apierrors.IsNotFound(err) {
		t.Fatalf("pool still present after finalization: %v", err)
	}
}

func TestValidateRuntimePoolExecutionWorkspace(t *testing.T) {
	pool := runtimePoolWorkspaceTestObject()
	if err := validateRuntimePoolExecutionWorkspace(pool); err != nil {
		t.Fatalf("valid workspace pool rejected: %v", err)
	}

	plain := runtimePoolTestObject(1)
	if err := validateRuntimePoolExecutionWorkspace(plain); err != nil {
		t.Fatalf("plain pool rejected: %v", err)
	}

	substrateMissingTemplate := runtimePoolWorkspaceTestObject()
	substrateMissingTemplate.Spec.ExecutionWorkspace.Provider = corev1alpha1.WorkspaceProviderSubstrate
	if err := validateRuntimePoolExecutionWorkspace(substrateMissingTemplate); err == nil || !strings.Contains(err.Error(), "infrastructure ActorTemplate") {
		t.Fatalf("substrate provider error = %v, want missing infrastructure template", err)
	}
	substratePool := runtimePoolWorkspaceTestObject()
	substratePool.Spec.ExecutionWorkspace.Provider = corev1alpha1.WorkspaceProviderSubstrate
	substratePool.Spec.ExecutionWorkspace.Substrate = &corev1alpha1.RuntimePoolSubstrateWorkspaceSpec{
		BaseTemplateNamespace: "ate-demo", BaseTemplateName: "orka-codex-infra",
	}
	if err := validateRuntimePoolExecutionWorkspace(substratePool); err != nil {
		t.Fatalf("valid substrate pool rejected: %v", err)
	}
	sandboxWithSubstrate := runtimePoolWorkspaceTestObject()
	sandboxWithSubstrate.Spec.ExecutionWorkspace.Substrate = &corev1alpha1.RuntimePoolSubstrateWorkspaceSpec{
		BaseTemplateNamespace: "ate-demo", BaseTemplateName: "orka-codex-infra",
	}
	if err := validateRuntimePoolExecutionWorkspace(sandboxWithSubstrate); err == nil || !strings.Contains(err.Error(), "only valid for provider substrate") {
		t.Fatalf("sandbox-with-substrate error = %v, want provider mismatch", err)
	}

	badDigest := runtimePoolWorkspaceTestObject()
	badDigest.Spec.ExecutionWorkspace.BindingDigest = "not-a-digest"
	if err := validateRuntimePoolExecutionWorkspace(badDigest); err == nil || !strings.Contains(err.Error(), "bindingDigest") {
		t.Fatalf("binding digest error = %v, want digest rejection", err)
	}

	badCapacity := runtimePoolWorkspaceTestObject()
	badCapacity.Spec.Capacity = &corev1alpha1.RuntimePoolCapacitySpec{MaxResidentSessions: 10, MaxRunningPrompts: 4}
	if err := validateRuntimePoolExecutionWorkspace(badCapacity); err == nil || !strings.Contains(err.Error(), "exactly one resident RuntimeSession") {
		t.Fatalf("capacity error = %v, want single-session requirement", err)
	}
}

func TestWorkspaceRuntimePoolMissingProviderCRDsFailsClosed(t *testing.T) {
	// The scheme intentionally omits the sandbox extension types: without the
	// externally operated provider installation, the pool degrades and closes
	// admission instead of falling back to a Deployment.
	scheme := runtimePoolTestScheme(t)
	pool := runtimePoolWorkspaceTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)

	runtimePoolReconcile(t, r, pool)

	status := runtimePoolTestGetPool(t, r, pool).Status
	if status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded || status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
		t.Fatalf("status = %s/%s, want Degraded/Closed", status.Lifecycle, status.AdmissionState)
	}
	if !strings.Contains(status.Message, "agent-sandbox provider CRDs are not installed") {
		t.Fatalf("message = %q, want missing provider CRD failure", status.Message)
	}
	var deployment appsv1.Deployment
	base := runtimePoolResourceName(pool.Namespace, pool.Name)
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: pool.Namespace, Name: base}, &deployment); !apierrors.IsNotFound(err) {
		t.Fatalf("degraded workspace pool created a Deployment (err=%v); there is no cross-backend fallback", err)
	}
}
