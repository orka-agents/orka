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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxextv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
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

// sandboxSuspendTestDurablePVC materializes the provider-created durable
// workspace PVC, controller-owned by the adopted Sandbox, the way
// agent-sandbox realizes the claim's volumeClaimTemplate.
func sandboxSuspendTestDurablePVC(
	t *testing.T,
	r *RuntimePoolReconciler,
	sandbox *sandboxv1beta1.Sandbox,
	uid string,
) *corev1.PersistentVolumeClaim {
	t.Helper()
	controller := true
	expected, err := runtimePoolDurableVolumeClaimTemplate(runtimePoolSandboxSuspendTestObject())
	if err != nil {
		t.Fatalf("render durable claim template: %v", err)
	}
	provisionedBy := "test.orka.ai/provisioner"
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pv-" + uid, UID: types.UID("pv-" + uid),
			Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": provisionedBy},
		},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			ClaimRef: &corev1.ObjectReference{
				Namespace: sandbox.Namespace, Name: durableWorkspacePVCName(sandbox.Name), UID: types.UID(uid),
			},
		},
	}
	if err := r.Create(context.Background(), pv); err != nil {
		t.Fatalf("materialize durable workspace PV: %v", err)
	}
	spec := expected.Spec.DeepCopy()
	spec.VolumeName = pv.Name
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sandbox.Namespace, Name: durableWorkspacePVCName(sandbox.Name), UID: types.UID(uid),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: sandboxv1beta1.GroupVersion.String(), Kind: "Sandbox",
				Name: sandbox.Name, UID: sandbox.UID, Controller: &controller,
			}},
		},
		Spec: *spec,
	}
	if err := r.Create(context.Background(), pvc); err != nil {
		t.Fatalf("materialize durable workspace PVC: %v", err)
	}
	return pvc
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
		// The materialized Pod's controller reference predates the stamped
		// UID; refresh it so ownership attestation still holds.
		refreshed := &corev1.Pod{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(&pod), refreshed); err != nil {
			t.Fatalf("read materialized pod: %v", err)
		}
		for i := range refreshed.OwnerReferences {
			if refreshed.OwnerReferences[i].Name == sandbox.Name {
				refreshed.OwnerReferences[i].UID = sandbox.UID
			}
		}
		if err := r.Update(context.Background(), refreshed); err != nil {
			t.Fatalf("refresh pod ownership: %v", err)
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

	// The provider would create the durable workspace PVC from the claim's
	// volumeClaimTemplate, controller-owned by the Sandbox; materialize it
	// before admission because attestation now verifies the realized PVC.
	durablePVC := sandboxSuspendTestDurablePVC(t, r, sandbox, "durable-pvc-uid")

	supervisor.probe = runtimePoolValidProbe(pool, &pod, "workspace-boot", false)
	runtimePoolReconcile(t, r, pool)
	if got := runtimePoolTestGetPool(t, r, pool).Status.Lifecycle; got != corev1alpha1.RuntimePoolLifecycleServing {
		t.Fatalf("lifecycle = %s (msg=%q), want Serving before suspension", got, runtimePoolTestGetPool(t, r, pool).Status.Message)
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
	if record == nil || record.Name != sandbox.Name || record.UID != sandbox.UID || record.PVCUID != durablePVC.UID ||
		record.PVName != durablePVC.Spec.VolumeName || record.PVUID == "" {
		t.Fatalf("consent record = %+v (status=%s msg=%q annotations=%v), want the exact adopted Sandbox, durable PVC, and bound PV", record, current.Status.Lifecycle, current.Status.Message, current.Annotations)
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

	// The provider reports the suspended Sandbox's claim as not ready - the
	// live claim controller does exactly this while the Sandbox is suspended,
	// and the resume must not be preempted by that expected state.
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), currentClaim); err != nil {
		t.Fatalf("re-read claim before resume: %v", err)
	}
	currentClaim.Status.Conditions = []metav1.Condition{{
		Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse,
		Reason: "SandboxNotReady", Message: "sandbox is suspended", LastTransitionTime: metav1.Now(),
	}}
	if err := r.Update(context.Background(), currentClaim); err != nil {
		t.Fatalf("record suspended claim readiness: %v", err)
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

// The upstream claim controller injects the claim's volumeClaimTemplates
// into the materialized Sandbox while the controller-rendered blueprint
// template carries none; attestation must accept exactly the claim's volume
// claims or every suspend-capable pool loops through rollouts at bring-up.
func TestWorkspaceMaterializationAttestationAcceptsClaimInjectedVolumes(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)

	runtimePoolReconcile(t, r, pool)
	template, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if template == nil || claim == nil {
		t.Fatal("suspend-capable pool must render a template and claim")
	}
	pod := runtimePoolWorkspaceTestMaterialization(t, r, pool, template, "10.0.0.9")
	// The realized PVC is part of attestation for suspend-capable pools.
	attSandbox := &sandboxv1beta1.Sandbox{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}, attSandbox); err != nil {
		t.Fatalf("read materialized Sandbox: %v", err)
	}
	sandboxSuspendTestDurablePVC(t, r, attSandbox, "claim-injected-pvc-uid")
	current := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}, current); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	materialized, err := r.attestWorkspaceRuntimePoolMaterialization(context.Background(), pool, current, template, &pod)
	if err != nil {
		t.Fatalf("attestation rejected the claim-injected durable volume: %v", err)
	}
	if !materialized {
		t.Fatal("attestation did not accept the materialized suspend-capable Sandbox")
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

// A real claim failure during a resume must not be hidden behind the suspend
// record: only the expected suspended-claim readiness states are bypassed.
func TestWorkspaceRuntimePoolResumeSurfacesUnrelatedClaimFailures(t *testing.T) {
	pool := runtimePoolSandboxSuspendTestObject()
	r := &RuntimePoolReconciler{}
	status := corev1alpha1.RuntimePoolStatus{}
	claim := &sandboxextv1beta1.SandboxClaim{}
	claim.Status.Conditions = []metav1.Condition{{
		Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse,
		Reason: "TemplateNotFound", Message: "SandboxTemplate was deleted", LastTransitionTime: metav1.Now(),
	}}
	if !r.applySandboxClaimFailureConditions(pool, claim, &status, true) {
		t.Fatal("an unrelated claim failure must degrade the pool even while a suspend record exists")
	}
	for _, reason := range []string{sandboxv1beta1.SandboxReasonSuspended, "SandboxNotReady"} {
		expected := &sandboxextv1beta1.SandboxClaim{}
		expected.Status.Conditions = []metav1.Condition{{
			Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse,
			Reason: reason, Message: "sandbox is suspended", LastTransitionTime: metav1.Now(),
		}}
		fresh := corev1alpha1.RuntimePoolStatus{}
		if r.applySandboxClaimFailureConditions(pool, expected, &fresh, true) {
			t.Fatalf("the expected suspended-claim reason %q must not preempt the resume", reason)
		}
		if r.applySandboxClaimFailureConditions(pool, expected, &fresh, false) {
			// Without a suspend record SandboxNotReady/SandboxSuspended still
			// degrade: only a recorded consensual suspension expects them.
			continue
		}
		t.Fatalf("without a suspend record the reason %q must degrade the pool", reason)
	}
}

// A mutated SandboxClaim adding PVC fields the frozen binding never declared
// must fail the template match, or bootstrap would trust storage outside the
// binding (for example an attacker-bound volumeName or dataSource).
func TestDurableVolumeClaimTemplatesMatchRejectsTamperedFields(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)
	runtimePoolReconcile(t, r, pool)
	_, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if claim == nil {
		t.Fatal("suspend-capable pool must render a claim")
	}
	if !runtimePoolDurableVolumeClaimTemplatesMatch(claim, pool) {
		t.Fatal("the controller-rendered claim must match its own frozen template")
	}
	tamper := func(mutate func(*sandboxv1beta1.PersistentVolumeClaimTemplate)) *sandboxextv1beta1.SandboxClaim {
		mutated := claim.DeepCopy()
		mutate(&mutated.Spec.VolumeClaimTemplates[0])
		return mutated
	}
	volumeName := "attacker-pv"
	if runtimePoolDurableVolumeClaimTemplatesMatch(tamper(func(template *sandboxv1beta1.PersistentVolumeClaimTemplate) {
		template.Spec.VolumeName = volumeName
	}), pool) {
		t.Fatal("a bound volumeName outside the frozen binding must fail the match")
	}
	if runtimePoolDurableVolumeClaimTemplatesMatch(tamper(func(template *sandboxv1beta1.PersistentVolumeClaimTemplate) {
		apiGroup := "snapshot.storage.k8s.io"
		template.Spec.DataSource = &corev1.TypedLocalObjectReference{
			APIGroup: &apiGroup, Kind: "VolumeSnapshot", Name: "foreign-snapshot",
		}
	}), pool) {
		t.Fatal("a dataSource outside the frozen binding must fail the match")
	}
	if runtimePoolDurableVolumeClaimTemplatesMatch(tamper(func(template *sandboxv1beta1.PersistentVolumeClaimTemplate) {
		mode := corev1.PersistentVolumeBlock
		template.Spec.VolumeMode = &mode
	}), pool) {
		t.Fatal("a volumeMode outside the frozen binding must fail the match")
	}
	if runtimePoolDurableVolumeClaimTemplatesMatch(tamper(func(template *sandboxv1beta1.PersistentVolumeClaimTemplate) {
		template.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"steal": "other-workspace-data"}}
	}), pool) {
		t.Fatal("a selector outside the frozen binding must fail the match")
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
	current.Annotations[sandboxSuspendedAnnotation] = `{"name":"vanished-sandbox","uid":"vanished-uid","pvcUID":"vanished-pvc-uid"}`
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
	pool.Annotations[sandboxSuspendedAnnotation] = `{"name":"sb","uid":"u"}`
	if sandboxConsensualSuspendRecord(pool) != nil {
		t.Fatal("a consent record without its durable PVC pin must be ignored")
	}
	plain := runtimePoolWorkspaceTestObject()
	plain.Annotations = map[string]string{sandboxSuspendedAnnotation: `{"name":"sb","uid":"u","pvcUID":"p"}`}
	if sandboxConsensualSuspendRecord(plain) != nil {
		t.Fatal("a non-suspend-capable pool can never carry consent")
	}
}

// A durable workspace PVC that vanishes or is replaced under its deterministic
// name while the checkpoint is settling is terminal exactly like a vanished
// Sandbox: the loss is recorded and the pool degrades instead of settling a
// checkpoint that no longer holds the preserved data.
func TestWorkspaceRuntimePoolSuspendFailsClosedOnReplacedPVC(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)
	runtimePoolReconcile(t, r, pool)

	sandbox := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Namespace: pool.Namespace, Name: "checkpointed-sandbox", UID: types.UID("checkpointed-sandbox-uid"),
	}}
	if err := r.Create(context.Background(), sandbox); err != nil {
		t.Fatalf("create checkpointed sandbox: %v", err)
	}
	replacement := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Namespace: pool.Namespace, Name: durableWorkspacePVCName(sandbox.Name), UID: types.UID("replacement-pvc-uid"),
	}}
	if err := r.Create(context.Background(), replacement); err != nil {
		t.Fatalf("create replacement PVC: %v", err)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[runtimePoolWorkspaceSuspendAnnotation] = booleanTrueValue
	current.Annotations[sandboxSuspendedAnnotation] = `{"name":"checkpointed-sandbox","uid":"checkpointed-sandbox-uid","pvcUID":"recorded-pvc-uid"}`
	current.Spec.DesiredReplicas = 0
	current.Generation++
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("record replaced-PVC suspension: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatalf("a replaced durable PVC must record terminal loss, annotations=%v", current.Annotations)
	}
	if current.Annotations[sandboxSuspendedAnnotation] != "" {
		t.Fatal("the invalidated consent record must be retired")
	}
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded {
		t.Fatalf("lifecycle = %s, want Degraded", current.Status.Lifecycle)
	}
}

// Recycling the physical instance of a workspace pool must never delete the
// claim while it holds the only preserved copy of a consensually suspended
// workspace whose resume has not passed the Serving fence yet.
func TestRecycleRuntimePoolInstancePreservesSuspendedClaim(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)
	runtimePoolReconcile(t, r, pool)
	_, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if claim == nil {
		t.Fatal("claim was not materialized")
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[sandboxSuspendedAnnotation] = `{"name":"sb","uid":"sb-uid","pvcUID":"pvc-uid"}`
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("record consent: %v", err)
	}
	if err := r.recycleRuntimePoolInstance(context.Background(), &current, nil); err == nil ||
		!strings.Contains(err.Error(), "consensually suspended checkpoint") {
		t.Fatalf("recycle error = %v, want a fail-closed claim-preserving refusal", err)
	}
	preserved := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
		t.Fatalf("claim must survive the refused recycle: %v", err)
	}
	if !preserved.DeletionTimestamp.IsZero() {
		t.Fatal("claim must not be deleted while the checkpoint record stands")
	}
}

// A probe-validated instance that is not the admitted ActiveInstance must fail
// the suspension closed instead of being silently adopted and checkpointed.
func TestWorkspaceRuntimePoolSuspendRejectsReplacedInstance(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)
	runtimePoolReconcile(t, r, pool)
	template, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if template == nil || claim == nil {
		t.Fatal("workspace template and claim were not materialized")
	}
	pod := runtimePoolWorkspaceTestMaterialization(t, r, pool, template, "10.0.0.81")
	sandbox := &sandboxv1beta1.Sandbox{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}, sandbox); err != nil {
		t.Fatalf("read materialized Sandbox: %v", err)
	}
	if sandbox.UID == "" {
		sandbox.UID = types.UID("replaced-instance-sandbox-uid")
		if err := r.Update(context.Background(), sandbox); err != nil {
			t.Fatalf("assign test Sandbox UID: %v", err)
		}
		// The materialized Pod's controller reference predates the stamped
		// UID; refresh it so ownership attestation still holds.
		refreshed := &corev1.Pod{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(&pod), refreshed); err != nil {
			t.Fatalf("read materialized pod: %v", err)
		}
		for i := range refreshed.OwnerReferences {
			if refreshed.OwnerReferences[i].Name == sandbox.Name {
				refreshed.OwnerReferences[i].UID = sandbox.UID
			}
		}
		if err := r.Update(context.Background(), refreshed); err != nil {
			t.Fatalf("refresh pod ownership: %v", err)
		}
	}
	sandboxSuspendTestDurablePVC(t, r, sandbox, "replaced-instance-pvc-uid")
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "workspace-boot", false)
	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || current.Status.ActiveInstance == nil {
		t.Fatalf("lifecycle = %s, want a Serving pool with an admitted instance", current.Status.Lifecycle)
	}

	// The persisted admitted identity diverges from the live Pod the probe
	// validates: the suspension must reject the replacement.
	base := current.DeepCopy()
	current.Status.ActiveInstance.PodUID = "replaced-pod-uid"
	if err := r.Status().Patch(context.Background(), &current, client.MergeFrom(base)); err != nil {
		t.Fatalf("record divergent admitted instance: %v", err)
	}
	sandboxSuspendTestSetIntent(t, r, pool, true)
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		!strings.Contains(current.Status.Message, "authenticated runtime identity changed before workspace suspension") {
		t.Fatalf("lifecycle = %s message = %q, want a fail-closed replaced-instance rejection", current.Status.Lifecycle, current.Status.Message)
	}
	if supervisor.drainCalls != 0 {
		t.Fatalf("drain calls = %d, want none against a replaced instance", supervisor.drainCalls)
	}
}

// The idle reaper's scale-to-zero can land before the adapter records the
// pool suspension intent; ordinary scale-down in that window would delete the
// SandboxClaim and cascade the durable PVC away.
const pendingSuspendWorkspaceName = "acp-ws-pending-suspend"

func TestWorkspaceRuntimePoolHoldsScaleDownWhileSuspendIntentPending(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add workspace scheme: %v", err)
	}
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)
	runtimePoolReconcile(t, r, pool)
	_, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if claim == nil {
		t.Fatal("claim was not materialized")
	}

	current := runtimePoolTestGetPool(t, r, pool)
	if current.Labels == nil {
		current.Labels = map[string]string{}
	}
	current.Labels[acpExecutionWorkspaceLinkLabel] = pendingSuspendWorkspaceName
	current.Spec.DesiredReplicas = 0
	current.Generation++
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("scale pool to zero: %v", err)
	}
	linked := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: pool.Namespace, Name: pendingSuspendWorkspaceName},
		Spec:       workspacev1alpha1.ExecutionWorkspaceSpec{DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredSuspended},
	}
	if err := r.Create(context.Background(), linked); err != nil {
		t.Fatalf("create linked workspace: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDraining ||
		!strings.Contains(current.Status.Message, "waiting for the durable pool suspension intent") {
		t.Fatalf("lifecycle = %s message = %q, want a held scale-down", current.Status.Lifecycle, current.Status.Message)
	}
	preserved := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
		t.Fatalf("claim must be preserved while suspension intent is pending: %v", err)
	}
	if !preserved.DeletionTimestamp.IsZero() {
		t.Fatal("claim must not be deleting while suspension intent is pending")
	}
}

// sandboxSuspendTestReachServing materializes a suspend-capable sandbox pool
// through adoption, durable-PVC attestation, and Serving admission, returning
// the live children for suspension scenarios.
func sandboxSuspendTestReachServing(
	t *testing.T,
	r *RuntimePoolReconciler,
	pool *corev1alpha1.RuntimePool,
	supervisor *fakeRuntimePoolSupervisorClient,
) (*sandboxv1beta1.Sandbox, *sandboxextv1beta1.SandboxClaim, corev1.Pod, *corev1.PersistentVolumeClaim) {
	t.Helper()
	runtimePoolReconcile(t, r, pool)
	template, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if template == nil || claim == nil {
		t.Fatal("workspace template and claim were not materialized")
	}
	pod := runtimePoolWorkspaceTestMaterialization(t, r, pool, template, "10.0.0.81")
	sandbox := &sandboxv1beta1.Sandbox{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}, sandbox); err != nil {
		t.Fatalf("read materialized Sandbox: %v", err)
	}
	if sandbox.UID == "" {
		sandbox.UID = types.UID("sandbox-uid")
		if err := r.Update(context.Background(), sandbox); err != nil {
			t.Fatalf("assign test Sandbox UID: %v", err)
		}
		refreshed := &corev1.Pod{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(&pod), refreshed); err != nil {
			t.Fatalf("read materialized pod: %v", err)
		}
		for i := range refreshed.OwnerReferences {
			if refreshed.OwnerReferences[i].Name == sandbox.Name {
				refreshed.OwnerReferences[i].UID = sandbox.UID
			}
		}
		if err := r.Update(context.Background(), refreshed); err != nil {
			t.Fatalf("refresh pod ownership: %v", err)
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
	durablePVC := sandboxSuspendTestDurablePVC(t, r, sandbox, "durable-pvc-uid")
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "workspace-boot", false)
	runtimePoolReconcile(t, r, pool)
	if got := runtimePoolTestGetPool(t, r, pool).Status.Lifecycle; got != corev1alpha1.RuntimePoolLifecycleServing {
		t.Fatalf("lifecycle = %s (msg=%q), want Serving", got, runtimePoolTestGetPool(t, r, pool).Status.Message)
	}
	return sandbox, currentClaim, pod, durablePVC
}

// A transient Sandbox read failure after the persisted Quiescent barrier must
// preserve the admitted identity and the SandboxClaim: clearing the instance
// would send the next reconcile into ordinary scale-down, deleting the claim
// and its durable PVC and losing unpublished workspace data to a read outage.
func TestWorkspaceRuntimePoolPreservesClaimAcrossPreCheckpointReadFailure(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)
	sandbox, claim, pod, _ := sandboxSuspendTestReachServing(t, r, pool, supervisor)

	sandboxSuspendTestSetIntent(t, r, pool, true)
	runtimePoolReconcile(t, r, pool)
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "workspace-boot", true)
	runtimePoolReconcile(t, r, pool)
	if got := runtimePoolTestGetPool(t, r, pool).Status.Lifecycle; got != corev1alpha1.RuntimePoolLifecycleQuiescent {
		t.Fatalf("lifecycle after quiescent probe = %s, want Quiescent", got)
	}

	// The adopted Sandbox becomes unreadable before the checkpoint record is
	// persisted.
	if err := r.Delete(context.Background(), sandbox); err != nil {
		t.Fatalf("delete adopted sandbox: %v", err)
	}
	for range 3 {
		runtimePoolReconcile(t, r, pool)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.ActiveInstance == nil {
		t.Fatalf("ActiveInstance cleared on a pre-checkpoint read failure (lifecycle=%s msg=%q)",
			current.Status.Lifecycle, current.Status.Message)
	}
	currentClaim := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), currentClaim); err != nil {
		t.Fatalf("SandboxClaim must survive a pre-checkpoint read failure: %v", err)
	}
	if currentClaim.DeletionTimestamp != nil {
		t.Fatal("SandboxClaim must not be deleting after a pre-checkpoint read failure")
	}
}

// A durable workspace PVC that is already terminating (held only by
// pvc-protection while its Pod runs) must be rejected by attestation before
// admission: once the Pod stops, protection releases and the session
// workspace vanishes.
func TestWorkspaceRuntimePoolRejectsTerminatingDurablePVC(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)
	_, _, pod, durablePVC := sandboxSuspendTestReachServing(t, r, pool, supervisor)

	// The PVC is deleted but held by pvc-protection while the Pod runs.
	currentPVC := &corev1.PersistentVolumeClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(durablePVC), currentPVC); err != nil {
		t.Fatalf("read durable PVC: %v", err)
	}
	currentPVC.Finalizers = append(currentPVC.Finalizers, "kubernetes.io/pvc-protection")
	if err := r.Update(context.Background(), currentPVC); err != nil {
		t.Fatalf("add pvc-protection finalizer: %v", err)
	}
	if err := r.Delete(context.Background(), currentPVC); err != nil {
		t.Fatalf("delete durable PVC: %v", err)
	}

	supervisor.probe = runtimePoolValidProbe(pool, &pod, "workspace-boot", false)
	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing {
		t.Fatalf("a terminating durable PVC must never be admitted (msg=%q)", current.Status.Message)
	}
	if !strings.Contains(current.Status.Message, "terminating") {
		t.Fatalf("status message = %q, want the terminating-PVC rejection", current.Status.Message)
	}
}

// A provider that accepts operatingMode Suspended but never settles the
// suspension must hit the bounded window: the suspension fails closed with
// resume-lost recorded while the consent record, claim, and durable volume
// stay preserved.
func TestWorkspaceRuntimePoolBoundsUnsettledSuspension(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool)
	_, claim, pod, _ := sandboxSuspendTestReachServing(t, r, pool, supervisor)

	sandboxSuspendTestSetIntent(t, r, pool, true)
	runtimePoolReconcile(t, r, pool)
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "workspace-boot", true)
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	record := sandboxConsensualSuspendRecord(&current)
	if record == nil {
		t.Fatalf("consent record missing (msg=%q)", current.Status.Message)
	}
	if record.RequestedAt.IsZero() {
		t.Fatal("consent record must stamp RequestedAt to bound the settlement window")
	}

	// The provider never terminates the Pod nor publishes Suspended. Within
	// the window the pool waits.
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] != "" {
		t.Fatal("suspension must not fail before the bounded window elapses")
	}

	// Backdate the record past the bounded window.
	stale := *record
	stale.RequestedAt = metav1.NewTime(time.Now().Add(-sandboxSuspendSettleTimeout - time.Minute))
	encoded, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("encode backdated record: %v", err)
	}
	base := current.DeepCopy()
	current.Annotations[sandboxSuspendedAnnotation] = string(encoded)
	if err := r.Patch(context.Background(), &current, client.MergeFrom(base)); err != nil {
		t.Fatalf("backdate consent record: %v", err)
	}
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatalf("an unsettled suspension past the bounded window must fail closed (msg=%q)", current.Status.Message)
	}
	if sandboxConsensualSuspendRecord(&current) == nil {
		t.Fatal("the consent record must stand so the durable claim stays preserved")
	}
	currentClaim := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), currentClaim); err != nil {
		t.Fatalf("SandboxClaim must survive the failed suspension: %v", err)
	}
}
