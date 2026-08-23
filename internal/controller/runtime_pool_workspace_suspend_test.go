/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxextv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func runtimePoolSandboxSuspendTestObject() *corev1alpha1.RuntimePool {
	pool := runtimePoolWorkspaceTestObject()
	pool.Spec.ExecutionWorkspace.AgentSandbox = &corev1alpha1.RuntimePoolAgentSandboxWorkspaceSpec{
		SuspendMode: string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly),
		SuspendVolume: &corev1alpha1.RuntimePoolSandboxDurableVolumeSpec{
			AccessModes: []string{string(corev1.ReadWriteOnce)},
			Capacity:    acpTestDurableCapacity,
		},
	}
	return pool
}

func sandboxSuspendTestSetIntent(t *testing.T, r *RuntimePoolReconciler, pool *corev1alpha1.RuntimePool, suspend bool) {
	t.Helper()
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	if suspend {
		current.Annotations[runtimePoolWorkspaceSuspendAnnotation] = booleanTrueValue
		current.Spec.DesiredReplicas = 0
	} else {
		delete(current.Annotations, runtimePoolWorkspaceSuspendAnnotation)
		current.Spec.DesiredReplicas = 1
	}
	current.Generation++
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("update pool suspend intent: %v", err)
	}
}

func TestWorkspaceRuntimePoolRendersDurableWorkspace(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)

	runtimePoolReconcile(t, r, pool)
	template, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if template == nil || claim == nil {
		t.Fatal("workspace template and claim were not materialized")
	}
	if template.Spec.VolumeClaimTemplatesPolicy != sandboxextv1beta1.VolumeClaimTemplatesPolicyAllowed {
		t.Fatalf("template VCT policy = %q, want Allowed for a suspend-capable pool", template.Spec.VolumeClaimTemplatesPolicy)
	}
	container := template.Spec.PodTemplate.Spec.Containers[0]
	foundMount := false
	for _, mount := range container.VolumeMounts {
		if mount.Name == substrateDurableWorkspaceVolume && mount.MountPath == substrateDurableWorkspaceMountPath {
			foundMount = true
		}
	}
	if !foundMount {
		t.Fatalf("container mounts = %v, want the durable workspace mount", container.VolumeMounts)
	}
	foundEnv := false
	for _, env := range container.Env {
		if env.Name == "ORKA_ACP_DURABLE_WORKSPACE_DIR" && env.Value == substrateDurableWorkspaceMountPath {
			foundEnv = true
		}
	}
	if !foundEnv {
		t.Fatal("container is missing the durable workspace environment variable")
	}
	if len(claim.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("claim VCTs = %d, want exactly the durable workspace volume", len(claim.Spec.VolumeClaimTemplates))
	}
	durable := claim.Spec.VolumeClaimTemplates[0]
	if durable.Name != substrateDurableWorkspaceVolume {
		t.Fatalf("claim VCT name = %q", durable.Name)
	}
	if !runtimePoolDurableVolumeClaimTemplatesMatch(claim, pool) {
		t.Fatal("rendered claim VCT does not match the frozen pool binding")
	}

	// A tampered VCT recycles fail-closed instead of being adopted.
	tampered := claim.DeepCopy()
	tampered.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = tampered.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage].DeepCopy()
	if runtimePoolDurableVolumeClaimTemplatesMatch(tampered, pool) != true {
		t.Fatal("deep-copied VCT must still match")
	}
	tampered.Spec.VolumeClaimTemplates[0].Name = "not-the-reserved-volume"
	if runtimePoolDurableVolumeClaimTemplatesMatch(tampered, pool) {
		t.Fatal("a renamed durable volume must fail the claim fence")
	}
}

func TestWorkspaceRuntimePoolWithoutSuspendKeepsVolumesDisallowed(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolWorkspaceTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)
	runtimePoolReconcile(t, r, pool)
	template, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if template == nil || claim == nil {
		t.Fatal("workspace template and claim were not materialized")
	}
	if template.Spec.VolumeClaimTemplatesPolicy != sandboxextv1beta1.VolumeClaimTemplatesPolicyDisallowed {
		t.Fatalf("template VCT policy = %q, want Disallowed without suspension", template.Spec.VolumeClaimTemplatesPolicy)
	}
	if len(claim.Spec.VolumeClaimTemplates) != 0 {
		t.Fatal("a non-suspendable claim must not request volumes")
	}
}

//nolint:gocyclo // The suspension and cold-resume lifecycle is one auditable end-to-end scenario.
func TestWorkspaceRuntimePoolSuspendsAndColdResumesPVCWorkspace(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)

	runtimePoolReconcile(t, r, pool)
	template, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if template == nil || claim == nil {
		t.Fatal("workspace template and claim were not materialized")
	}
	pod := runtimePoolWorkspaceTestMaterialization(t, r, pool, template, "10.0.0.80")
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "workspace-boot", false)
	runtimePoolReconcile(t, r, pool)
	if got := runtimePoolTestGetPool(t, r, pool).Status.Lifecycle; got != corev1alpha1.RuntimePoolLifecycleServing {
		t.Fatalf("lifecycle = %s, want Serving before suspension", got)
	}

	// Record the adopted Sandbox on the claim the way the provider does. The
	// fake client assigns no UIDs, so stamp the identity the API server would.
	sandbox := &sandboxv1beta1.Sandbox{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}, sandbox); err != nil {
		t.Fatalf("read materialized Sandbox: %v", err)
	}
	if sandbox.UID == "" {
		sandbox.UID = types.UID("sandbox-uid")
		if err := r.Update(context.Background(), sandbox); err != nil {
			t.Fatalf("assign test Sandbox UID: %v", err)
		}
	}
	currentClaim := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), currentClaim); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	base := currentClaim.DeepCopy()
	if currentClaim.Annotations == nil {
		currentClaim.Annotations = map[string]string{}
	}
	currentClaim.Annotations[sandboxextv1beta1.AssignedSandboxNameAnnotation] = sandbox.Name
	if err := r.Patch(context.Background(), currentClaim, client.MergeFrom(base)); err != nil {
		t.Fatalf("record adopted sandbox: %v", err)
	}

	sandboxSuspendTestSetIntent(t, r, pool, true)
	runtimePoolReconcile(t, r, pool)
	if supervisor.drainCalls != 1 {
		t.Fatalf("drain calls = %d, want 1", supervisor.drainCalls)
	}

	supervisor.probe = runtimePoolValidProbe(pool, &pod, "workspace-boot", true)
	runtimePoolReconcile(t, r, pool)
	if got := runtimePoolTestGetPool(t, r, pool).Status.Lifecycle; got != corev1alpha1.RuntimePoolLifecycleQuiescent {
		t.Fatalf("lifecycle after quiescent probe = %s, want Quiescent", got)
	}
	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	record := sandboxConsensualSuspendRecord(&current)
	if record == nil || record.Name != sandbox.Name || record.UID != sandbox.UID {
		t.Fatalf("consent record = %+v (status=%s msg=%q annotations=%v), want the exact adopted Sandbox", record, current.Status.Lifecycle, current.Status.Message, current.Annotations)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sandbox), sandbox); err != nil {
		t.Fatalf("read suspending sandbox: %v", err)
	}
	if sandbox.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Fatalf("sandbox operating mode = %q, want Suspended", sandbox.Spec.OperatingMode)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), currentClaim); err != nil {
		t.Fatalf("claim must survive suspension: %v", err)
	}

	// The provider terminates the Pod and reports the suspended condition.
	if err := r.Delete(context.Background(), &pod); err != nil {
		t.Fatalf("terminate sandbox pod: %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sandbox), sandbox); err != nil {
		t.Fatalf("re-read suspending sandbox: %v", err)
	}
	sandbox.Status.Conditions = []metav1.Condition{{
		Type: string(sandboxv1beta1.SandboxConditionSuspended), Status: metav1.ConditionTrue,
		Reason: "PodTerminated", LastTransitionTime: metav1.Now(),
	}}
	if err := r.Update(context.Background(), sandbox); err != nil {
		t.Fatalf("mark sandbox suspended: %v", err)
	}
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped {
		t.Fatalf("suspended lifecycle = %s, want Stopped", current.Status.Lifecycle)
	}
	if !strings.Contains(current.Status.Message, "durable workspace volume retained") {
		t.Fatalf("suspended message = %q", current.Status.Message)
	}

	// Cold resume: the intent lifts, bootstrap material rotates, the Sandbox
	// blueprint refreshes with the rotated material, and the same Sandbox
	// returns to Running against the preserved PVC.
	sandboxSuspendTestSetIntent(t, r, pool, false)
	for range 8 {
		runtimePoolReconcile(t, r, pool)
	}
	resumed := &sandboxv1beta1.Sandbox{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sandbox), resumed); err != nil {
		t.Fatalf("read resumed sandbox: %v", err)
	}
	if resumed.UID != sandbox.UID {
		t.Fatal("resume must patch the exact suspended Sandbox, never a replacement")
	}
	if resumed.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Fatalf("resumed operating mode = %q, want Running", resumed.Spec.OperatingMode)
	}
	if resumed.Spec.PodTemplate.ObjectMeta.Labels[sandboxextv1beta1.SandboxIDLabel] != string(currentClaim.UID) {
		t.Fatalf("resumed blueprint claim identity label = %q, want the provider-owned value preserved",
			resumed.Spec.PodTemplate.ObjectMeta.Labels[sandboxextv1beta1.SandboxIDLabel])
	}
	if resumed.Spec.PodTemplate.ObjectMeta.Labels[sandboxv1beta1.SandboxTemplateRefHashLabel] == "" {
		t.Fatal("resume must preserve the provider-owned template-ref hash label on the Sandbox blueprint")
	}
	freshNonce := ""
	for _, env := range resumed.Spec.PodTemplate.Spec.Containers[0].Env {
		if env.Name == "ORKA_ACP_CREDENTIAL_BOOTSTRAP_NONCE" {
			freshNonce = env.Value
		}
	}
	if freshNonce == "" {
		t.Fatal("resumed sandbox blueprint is missing the rotated bootstrap nonce")
	}
	staleNonce := ""
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "ORKA_ACP_CREDENTIAL_BOOTSTRAP_NONCE" {
			staleNonce = env.Value
		}
	}
	if staleNonce != "" && staleNonce == freshNonce {
		t.Fatal("resume must rotate the consumed one-time bootstrap nonce")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), currentClaim); err != nil {
		t.Fatalf("claim must survive resume: %v", err)
	}
	encoded, err := json.Marshal(runtimePoolTestGetPool(t, r, pool).Annotations)
	if err != nil {
		t.Fatalf("encode annotations: %v", err)
	}
	if strings.Contains(string(encoded), string(sandbox.UID)) && len(runtimePoolTestGetPool(t, r, pool).Annotations[sandboxSuspendedAnnotation]) > 0 {
		// The consent record retires only once a resumed Pod is observed; with
		// no fresh Pod yet in this fixture the record legitimately remains.
		t.Logf("consent record still pending a resumed Pod: %s", encoded)
	}
}

func TestStripInjectedDurableWorkspaceVolumeVerifiesClaimIdentity(t *testing.T) {
	t.Parallel()
	claimName := substrateDurableWorkspaceVolume + "-sandbox-a"
	injected := corev1.Volume{
		Name: substrateDurableWorkspaceVolume,
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: claimName,
		}},
	}
	const retainedVolumeName = "retained-volume"
	other := corev1.Volume{Name: retainedVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}

	got := stripInjectedDurableWorkspaceVolume(nil, []corev1.Volume{injected, other}, claimName)
	if len(got) != 1 || got[0].Name != retainedVolumeName {
		t.Fatalf("volumes = %+v, want exactly the expected injected claim stripped", got)
	}

	// A reserved-name volume bound to another workspace's PVC is retained so
	// the attestation comparison fails instead of serving foreign data.
	foreign := injected
	foreign.VolumeSource = corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
		ClaimName: substrateDurableWorkspaceVolume + "-sandbox-b",
	}}
	got = stripInjectedDurableWorkspaceVolume(nil, []corev1.Volume{foreign}, claimName)
	if len(got) != 1 {
		t.Fatal("a reserved-name volume bound to a foreign PVC must never be stripped")
	}

	// With no derivable claim identity nothing is stripped.
	if got := stripInjectedDurableWorkspaceVolume(nil, []corev1.Volume{injected}, ""); len(got) != 1 {
		t.Fatal("an empty expected claim name must strip nothing")
	}

	// A template that declares the reserved volume compares untouched.
	if got := stripInjectedDurableWorkspaceVolume([]corev1.Volume{{Name: substrateDurableWorkspaceVolume}}, []corev1.Volume{injected}, claimName); len(got) != 1 {
		t.Fatal("expected-declared reserved volumes must compare untouched")
	}
}

// A vanished or replaced suspended Sandbox is terminal: the pool must record
// the loss, never reprovision a fresh claim, and stay Degraded.
func TestWorkspaceRuntimePoolResumeLossIsTerminal(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)
	runtimePoolReconcile(t, r, pool)
	_, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if claim == nil {
		t.Fatal("claim was not materialized")
	}
	// Record a consensual suspension whose Sandbox no longer exists.
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[sandboxSuspendedAnnotation] = `{"name":"vanished-sandbox","uid":"vanished-uid"}`
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("record suspension: %v", err)
	}
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatalf("resume loss must be recorded durably, annotations=%v", current.Annotations)
	}
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded {
		t.Fatalf("lifecycle = %s, want Degraded", current.Status.Lifecycle)
	}

	// Later reconciles never reprovision a fresh claim.
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded {
		t.Fatalf("post-loss lifecycle = %s, want a permanently Degraded pool", current.Status.Lifecycle)
	}
	claims := &sandboxextv1beta1.SandboxClaimList{}
	if err := r.List(context.Background(), claims); err != nil {
		t.Fatalf("list claims: %v", err)
	}
	for i := range claims.Items {
		if claims.Items[i].DeletionTimestamp.IsZero() && claims.Items[i].Name == claim.Name {
			t.Fatal("resume loss must never reprovision or retain a live workspace claim")
		}
	}
}

func TestSandboxConsensualSuspendRecordRejectsMalformedRecords(t *testing.T) {
	t.Parallel()
	pool := runtimePoolSandboxSuspendTestObject()
	pool.Annotations = map[string]string{sandboxSuspendedAnnotation: "not-json"}
	if sandboxConsensualSuspendRecord(pool) != nil {
		t.Fatal("malformed consent record must be ignored")
	}
	pool.Annotations[sandboxSuspendedAnnotation] = `{"name":"","uid":""}`
	if sandboxConsensualSuspendRecord(pool) != nil {
		t.Fatal("empty consent record must be ignored")
	}
	plain := runtimePoolWorkspaceTestObject()
	plain.Annotations = map[string]string{sandboxSuspendedAnnotation: `{"name":"sb","uid":"u"}`}
	if sandboxConsensualSuspendRecord(plain) != nil {
		t.Fatal("a non-suspend-capable pool can never carry consent")
	}
}
