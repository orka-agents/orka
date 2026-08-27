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

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxextv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

type runtimePoolSuspendPrerequisiteFailureClient struct {
	client.Client
	stage string
}

type runtimePoolFailSandboxClaimDeleteOnceClient struct {
	client.Client
	failed bool
}

type runtimePoolFailSandboxReadsReader struct {
	client.Reader
}

const (
	runtimePoolPrerequisiteStageNamespace   = "namespace"
	runtimePoolPrerequisiteStageCredentials = "credentials"
	runtimePoolPrerequisiteStageAncillary   = "ancillary"
	runtimePoolSuspendTestStorageClassUID   = "acp-test-default-storage-class-uid"
	malformedSandboxMetadata                = "not-json"
)

func (c *runtimePoolSuspendPrerequisiteFailureClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	switch c.stage {
	case runtimePoolPrerequisiteStageNamespace:
		if _, ok := object.(*corev1.Namespace); ok {
			return errors.New("injected runtime namespace prerequisite failure")
		}
	case runtimePoolPrerequisiteStageAncillary:
		if _, ok := object.(*corev1.Service); ok {
			return errors.New("injected runtime ancillary-resource prerequisite failure")
		}
	}
	return c.Client.Get(ctx, key, object, options...)
}

func (c *runtimePoolSuspendPrerequisiteFailureClient) List(
	ctx context.Context,
	list client.ObjectList,
	options ...client.ListOption,
) error {
	if c.stage == runtimePoolPrerequisiteStageCredentials {
		if _, ok := list.(*corev1.SecretList); ok {
			return errors.New("injected runtime credential prerequisite failure")
		}
	}
	return c.Client.List(ctx, list, options...)
}

func (c *runtimePoolFailSandboxClaimDeleteOnceClient) Delete(
	ctx context.Context,
	object client.Object,
	options ...client.DeleteOption,
) error {
	if _, ok := object.(*sandboxextv1beta1.SandboxClaim); ok && !c.failed {
		c.failed = true
		return errors.New("injected SandboxClaim deletion failure")
	}
	return c.Client.Delete(ctx, object, options...)
}

func (r *runtimePoolFailSandboxReadsReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if _, ok := object.(*sandboxv1beta1.Sandbox); ok {
		return errors.New("injected Sandbox read failure")
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func (r *runtimePoolFailSandboxReadsReader) List(
	ctx context.Context,
	list client.ObjectList,
	options ...client.ListOption,
) error {
	if _, ok := list.(*sandboxv1beta1.SandboxList); ok {
		return errors.New("injected Sandbox list failure")
	}
	return r.Reader.List(ctx, list, options...)
}

func runtimePoolSandboxSuspendTestObject() *corev1alpha1.RuntimePool {
	pool := runtimePoolWorkspaceTestObject()
	pool.Spec.ExecutionWorkspace.AgentSandbox = &corev1alpha1.RuntimePoolAgentSandboxWorkspaceSpec{
		SuspendMode: string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly),
		SuspendVolume: &corev1alpha1.RuntimePoolSandboxDurableVolumeSpec{
			StorageClassName: acpTestDefaultStorageClass().Name,
			StorageClassUID:  runtimePoolSuspendTestStorageClassUID,
			AccessModes:      []string{string(corev1.ReadWriteOnce)},
			Capacity:         acpTestDurableCapacity,
		},
	}
	return pool
}

func runtimePoolSandboxSuspendTestReconciler(
	t *testing.T,
	scheme *runtime.Scheme,
	supervisor RuntimePoolSupervisorClient,
	pool *corev1alpha1.RuntimePool,
) *RuntimePoolReconciler {
	t.Helper()
	if !scheme.Recognizes(storagev1.SchemeGroupVersion.WithKind("StorageClass")) {
		if err := storagev1.AddToScheme(scheme); err != nil {
			t.Fatalf("add storage scheme: %v", err)
		}
	}
	class := acpTestDefaultStorageClass()
	class.UID = types.UID(runtimePoolSuspendTestStorageClassUID)
	return runtimePoolTestReconciler(t, scheme, supervisor, pool, class)
}

// sandboxSuspendTestDurablePVC materializes the provider-created durable
// workspace PVC, controller-owned by the adopted Sandbox, the way
// agent-sandbox realizes the claim's volumeClaimTemplate.
func sandboxSuspendTestDurablePVC(
	t *testing.T,
	r *RuntimePoolReconciler,
	pool *corev1alpha1.RuntimePool,
	sandbox *sandboxv1beta1.Sandbox,
	uid string,
) *corev1.PersistentVolumeClaim {
	t.Helper()
	controller := true
	expected, err := runtimePoolDurableVolumeClaimTemplate(pool)
	if err != nil {
		t.Fatalf("render durable claim template: %v", err)
	}
	provisionedBy := acpTestStorageProvisioner
	storageClassName := ""
	if expected.Spec.StorageClassName != nil {
		storageClassName = *expected.Spec.StorageClassName
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pv-" + uid, UID: types.UID("pv-" + uid),
			Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": provisionedBy},
		},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			StorageClassName:              storageClassName,
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

func sandboxSuspendTestDurableLineageRecord(
	t *testing.T,
	r *RuntimePoolReconciler,
	pvc *corev1.PersistentVolumeClaim,
) sandboxDurableLineageRecord {
	t.Helper()
	pv := &corev1.PersistentVolume{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); err != nil {
		t.Fatalf("read durable workspace PV: %v", err)
	}
	return sandboxDurableLineageRecord{PVCUID: pvc.UID, PVName: pv.Name, PVUID: pv.UID}
}

func sandboxSuspendTestStampDurableLineage(
	t *testing.T,
	r *RuntimePoolReconciler,
	pool *corev1alpha1.RuntimePool,
	pvc *corev1.PersistentVolumeClaim,
) {
	t.Helper()
	record := sandboxSuspendTestDurableLineageRecord(t, r, pvc)
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("encode durable lineage: %v", err)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[runtimePoolDurableLineageAnnotation] = string(encoded)
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("stamp durable lineage: %v", err)
	}
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

func TestSandboxSuspensionSettledRequiresCurrentProviderProof(t *testing.T) {
	base := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Spec:       sandboxv1beta1.SandboxSpec{OperatingMode: sandboxv1beta1.SandboxOperatingModeSuspended},
		Status: sandboxv1beta1.SandboxStatus{Conditions: []metav1.Condition{{
			Type:               string(sandboxv1beta1.SandboxConditionSuspended),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: 2,
			Reason:             sandboxv1beta1.SandboxReasonSuspendedPodTerminated,
		}}},
	}
	tests := []struct {
		name   string
		mutate func(*sandboxv1beta1.Sandbox)
		want   bool
	}{
		{name: "current suspended generation", want: true},
		{
			name: "running Sandbox with historical condition",
			mutate: func(sandbox *sandboxv1beta1.Sandbox) {
				sandbox.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
			},
		},
		{
			name: "stale observed generation",
			mutate: func(sandbox *sandboxv1beta1.Sandbox) {
				sandbox.Status.Conditions[0].ObservedGeneration--
			},
		},
		{
			name: "provider has not confirmed Pod termination",
			mutate: func(sandbox *sandboxv1beta1.Sandbox) {
				sandbox.Status.Conditions[0].Reason = sandboxv1beta1.SandboxReasonSuspendedPodTerminating
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sandbox := base.DeepCopy()
			if test.mutate != nil {
				test.mutate(sandbox)
			}
			if got := sandboxSuspensionSettled(sandbox); got != test.want {
				t.Fatalf("sandboxSuspensionSettled() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestResolveSuspendableWorkspaceSandboxRejectsConflictingClaimNames(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	_, claim, _, _ := sandboxSuspendTestReachServing(t, r, pool, supervisor)

	conflicted := claim.DeepCopy()
	conflicted.Status.SandboxStatus.Name = "different-sandbox"
	if _, err := r.resolveSuspendableWorkspaceSandbox(context.Background(), conflicted); err == nil ||
		!strings.Contains(err.Error(), "disagree") {
		t.Fatalf("resolve error = %v, want conflicting status and annotation names rejected", err)
	}
}

func TestResolveSuspendableWorkspaceSandboxRejectsMultipleControlledSandboxes(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	sandbox, claim, _, _ := sandboxSuspendTestReachServing(t, r, pool, supervisor)

	duplicate := sandbox.DeepCopy()
	duplicate.ResourceVersion = ""
	duplicate.Name = "duplicate-controlled-sandbox"
	duplicate.UID = types.UID("duplicate-controlled-sandbox-uid")
	if err := r.Create(context.Background(), duplicate); err != nil {
		t.Fatalf("create duplicate controlled Sandbox: %v", err)
	}
	if _, err := r.resolveSuspendableWorkspaceSandbox(context.Background(), claim); err == nil ||
		!strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("resolve error = %v, want multiple controlled Sandboxes rejected", err)
	}
}

func TestWorkspaceRuntimePoolRendersDurableWorkspace(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)

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
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)

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
	durablePVC := sandboxSuspendTestDurablePVC(t, r, pool, sandbox, "durable-pvc-uid")

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
		ObservedGeneration: sandbox.Generation,
		Reason:             sandboxv1beta1.SandboxReasonSuspendedPodTerminated,
		LastTransitionTime: metav1.Now(),
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

// Once a resumed lineage reaches Serving, the permanent record must pin the
// exact PVC and PV. A same-name replacement is shaped correctly but contains
// no preserved repository data, so admission must fail closed.
func TestWorkspaceRuntimePoolRejectsReplacedVolumeAfterResume(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	sandbox, claim, _, durablePVC := sandboxSuspendTestReachServing(t, r, pool, supervisor)
	lineage := sandboxSuspendTestDurableLineageRecord(t, r, durablePVC)
	lineage.Name = sandbox.Name
	lineage.UID = sandbox.UID

	checkpoint, err := json.Marshal(sandboxSuspendRecord{
		Name: sandbox.Name, UID: sandbox.UID,
		PVCUID: lineage.PVCUID, PVName: lineage.PVName, PVUID: lineage.PVUID,
		RequestedAt: metav1.Now(),
	})
	if err != nil {
		t.Fatalf("encode checkpoint: %v", err)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	current.Annotations[sandboxSuspendedAnnotation] = string(checkpoint)
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("record resumed checkpoint: %v", err)
	}
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	recorded := sandboxRecordedDurableLineage(&current)
	if recorded == nil || *recorded != lineage {
		t.Fatalf("durable lineage = %+v, want %+v", recorded, lineage)
	}
	if current.Annotations[sandboxSuspendedAnnotation] != "" {
		t.Fatal("checkpoint consent must retire after the resumed pool reaches Serving")
	}

	oldPV := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: durablePVC.Spec.VolumeName}}
	if err := r.Delete(context.Background(), durablePVC); err != nil {
		t.Fatalf("delete original durable PVC: %v", err)
	}
	if err := r.Delete(context.Background(), oldPV); err != nil {
		t.Fatalf("delete original durable PV: %v", err)
	}
	replacement := sandboxSuspendTestDurablePVC(t, r, pool, sandbox, "replacement-pvc-uid")
	runtimePoolReconcile(t, r, pool)

	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		current.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed ||
		!strings.Contains(current.Status.Message, "recorded resumed-lineage volume") {
		t.Fatalf("replacement-volume status = %s/%s %q, want a fail-closed lineage rejection", current.Status.Lifecycle, current.Status.AdmissionState, current.Status.Message)
	}
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatal("a replaced resumed-lineage PVC must record terminal durable-data loss")
	}
	preserved := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
		t.Fatalf("lineage claim must survive replacement rejection: %v", err)
	}
	if !preserved.DeletionTimestamp.IsZero() {
		t.Fatal("lineage claim must not be deleting after replacement rejection")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(replacement), &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("replacement PVC should remain for explicit cleanup: %v", err)
	}
}

// Once a resumed lineage exists, its SandboxClaim is permanent identity. A
// missing claim cannot be recreated under the deterministic name because that
// would provision a blank PVC while the workspace still names the old lineage.
func TestWorkspaceRuntimePoolFailsDurableLineageOnMissingClaim(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	_, claim, _, durablePVC := sandboxSuspendTestReachServing(t, r, pool, supervisor)
	sandboxSuspendTestStampDurableLineage(t, r, pool, durablePVC)

	if err := r.Delete(context.Background(), claim); err != nil {
		t.Fatalf("delete resumed-lineage claim: %v", err)
	}
	runtimePoolReconcile(t, r, pool)

	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatal("a missing resumed-lineage claim must record terminal durable-data loss")
	}
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		current.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
		t.Fatalf("status = %s/%s %q, want Degraded/Closed after resumed-lineage claim loss",
			current.Status.Lifecycle, current.Status.AdmissionState, current.Status.Message)
	}
	replacement := &sandboxextv1beta1.SandboxClaim{}
	err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), replacement)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("missing resumed-lineage claim was recreated: err=%v claim=%+v", err, replacement)
	}
}

// Checkpoint retirement and permanent-lineage creation form one write-ahead
// transition. If the claim disappeared before that transition, the controller
// must re-read the lineage fence before reaching claim materialization.
func TestWorkspaceRuntimePoolRefreshesLineageBeforeReplacingMissingClaim(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	sandbox, claim, pod, durablePVC := sandboxSuspendTestReachServing(t, r, pool, supervisor)
	lineage := sandboxSuspendTestDurableLineageRecord(t, r, durablePVC)
	lineage.Name = sandbox.Name
	lineage.UID = sandbox.UID

	checkpoint, err := json.Marshal(sandboxSuspendRecord{
		Name: sandbox.Name, UID: sandbox.UID,
		PVCUID: lineage.PVCUID, PVName: lineage.PVName, PVUID: lineage.PVUID,
		RequestedAt: metav1.Now(),
	})
	if err != nil {
		t.Fatalf("encode checkpoint: %v", err)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	current.Annotations[sandboxSuspendedAnnotation] = string(checkpoint)
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("record resumed checkpoint: %v", err)
	}
	if err := r.Delete(context.Background(), claim); err != nil {
		t.Fatalf("delete resumed claim before checkpoint retirement: %v", err)
	}
	if err := r.Delete(context.Background(), &pod); err != nil {
		t.Fatalf("delete resumed pod before checkpoint retirement: %v", err)
	}

	result := runtimePoolReconcile(t, r, pool)
	if result.RequeueAfter <= 0 {
		t.Fatalf("checkpoint retirement result = %+v, want a bounded lineage refresh", result)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if recorded := sandboxRecordedDurableLineage(&current); recorded == nil || *recorded != lineage {
		t.Fatalf("durable lineage = %+v, want %+v", recorded, lineage)
	}
	if current.Annotations[sandboxSuspendedAnnotation] != "" {
		t.Fatal("checkpoint consent must retire after the resumed pool reaches Serving")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), &sandboxextv1beta1.SandboxClaim{}); !apierrors.IsNotFound(err) {
		t.Fatalf("missing claim was replaced before the lineage refresh: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatal("the refreshed lineage must record terminal loss for the missing claim")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), &sandboxextv1beta1.SandboxClaim{}); !apierrors.IsNotFound(err) {
		t.Fatalf("missing resumed-lineage claim was recreated: %v", err)
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
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
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
	current.Annotations[sandboxSuspendedAnnotation] = `{"name":"vanished-sandbox","uid":"vanished-uid","pvcUID":"vanished-pvc-uid","pvName":"pv-vanished","pvUID":"pv-vanished-uid"}`
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

func TestWorkspaceRuntimePoolRetriesResumeLossClaimCleanup(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	runtimePoolReconcile(t, r, pool)
	_, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if claim == nil {
		t.Fatal("claim was not materialized")
	}

	current := runtimePoolTestGetPool(t, r, pool)
	current.Annotations[sandboxSuspendedAnnotation] = `{"name":"vanished-sandbox","uid":"vanished-uid","pvcUID":"vanished-pvc-uid","pvName":"pv-vanished","pvUID":"pv-vanished-uid"}`
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("record suspension: %v", err)
	}
	failing := &runtimePoolFailSandboxClaimDeleteOnceClient{Client: r.Client}
	r.Client = failing
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err == nil || !strings.Contains(err.Error(), "injected SandboxClaim deletion failure") {
		t.Fatalf("first reconcile error = %v, want injected claim deletion failure", err)
	}

	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatal("resume loss must be durable before claim deletion")
	}
	if current.Annotations[sandboxSuspendedAnnotation] == "" {
		t.Fatal("checkpoint record must remain as the pending cleanup marker")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), &sandboxextv1beta1.SandboxClaim{}); err != nil {
		t.Fatalf("claim disappeared despite the injected deletion failure: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), &sandboxextv1beta1.SandboxClaim{}); !apierrors.IsNotFound(err) {
		t.Fatalf("claim cleanup was not retried: %v", err)
	}
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[sandboxSuspendedAnnotation] != "" {
		t.Fatal("checkpoint cleanup marker remained after the claim disappeared")
	}
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded {
		t.Fatalf("lifecycle = %s, want Degraded after terminal cleanup", current.Status.Lifecycle)
	}
}

func TestSandboxConsensualSuspendRecordRejectsMalformedRecords(t *testing.T) {
	t.Parallel()
	pool := runtimePoolSandboxSuspendTestObject()
	pool.Annotations = map[string]string{sandboxSuspendedAnnotation: malformedSandboxMetadata}
	if sandboxConsensualSuspendRecord(pool) != nil {
		t.Fatal("malformed consent record must not parse as valid")
	}
	if !sandboxConsensualSuspendRecordMalformed(pool) {
		t.Fatal("a nonempty malformed checkpoint record must remain distinguishable from no record")
	}
	pool.Annotations[sandboxSuspendedAnnotation] = `{"name":"","uid":""}`
	if sandboxConsensualSuspendRecord(pool) != nil {
		t.Fatal("empty consent record must be ignored")
	}
	pool.Annotations[sandboxSuspendedAnnotation] = `{"name":"sb","uid":"u"}`
	if sandboxConsensualSuspendRecord(pool) != nil {
		t.Fatal("a consent record without its durable PVC pin must be ignored")
	}
	pool.Annotations[sandboxSuspendedAnnotation] = `{"name":"sb","uid":"u","pvcUID":"p"}`
	if sandboxConsensualSuspendRecord(pool) != nil {
		t.Fatal("a consent record without its bound-PV identity must be ignored: a same-name replacement PV could otherwise be admitted")
	}
	pool.Annotations[sandboxSuspendedAnnotation] = `{"name":"sb","uid":"u","pvcUID":"p","pvName":"pv-a"}`
	if sandboxConsensualSuspendRecord(pool) != nil {
		t.Fatal("a consent record with a PV name but no PV UID must be ignored")
	}
	plain := runtimePoolWorkspaceTestObject()
	plain.Annotations = map[string]string{sandboxSuspendedAnnotation: `{"name":"sb","uid":"u","pvcUID":"p","pvName":"pv-a","pvUID":"pvu"}`}
	if sandboxConsensualSuspendRecord(plain) != nil {
		t.Fatal("a non-suspend-capable pool can never carry consent")
	}
	if sandboxConsensualSuspendRecordMalformed(plain) {
		t.Fatal("a stale cross-provider annotation must not fence an ordinary pool")
	}
}

// A nonempty malformed checkpoint may mean the provider mutation already
// happened before the record was damaged. It must never be treated as the
// pre-checkpoint state, or ordinary scale-down will delete the claim and PVC.
func TestWorkspaceRuntimePoolMalformedCheckpointRetainsDurableClaim(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	_, claim, _, _ := sandboxSuspendTestReachServing(t, r, pool, supervisor)

	sandboxSuspendTestSetIntent(t, r, pool, true)
	current := runtimePoolTestGetPool(t, r, pool)
	current.Annotations[sandboxSuspendedAnnotation] = malformedSandboxMetadata
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("damage checkpoint record: %v", err)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	statusBase := current.DeepCopy()
	current.Status.ActiveInstance = nil
	current.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
	current.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	if err := r.Status().Patch(context.Background(), &current, client.MergeFrom(statusBase)); err != nil {
		t.Fatalf("record post-dispatch status: %v", err)
	}

	for range 3 {
		runtimePoolReconcile(t, r, pool)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		!strings.Contains(current.Status.Message, "checkpoint metadata is malformed") {
		t.Fatalf("malformed checkpoint status = %s/%q, want a durable fail-closed hold", current.Status.Lifecycle, current.Status.Message)
	}
	preserved := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
		t.Fatalf("SandboxClaim must survive malformed checkpoint metadata: %v", err)
	}
	if !preserved.DeletionTimestamp.IsZero() {
		t.Fatal("SandboxClaim must not be deleting while malformed checkpoint metadata stands")
	}
}

// Namespace, credential, and ancillary-resource failures happen before the
// workspace suspension state machine. They must preserve the admitted
// identity so the next successful pass cannot enter unadmitted scale-down.
func TestWorkspaceRuntimePoolPrerequisiteFailuresPreserveSuspendFence(t *testing.T) {
	for _, stage := range []string{
		runtimePoolPrerequisiteStageNamespace,
		runtimePoolPrerequisiteStageCredentials,
		runtimePoolPrerequisiteStageAncillary,
	} {
		t.Run(stage, func(t *testing.T) {
			scheme := runtimePoolWorkspaceTestScheme(t)
			pool := runtimePoolSandboxSuspendTestObject()
			supervisor := &fakeRuntimePoolSupervisorClient{}
			r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
			_, claim, _, _ := sandboxSuspendTestReachServing(t, r, pool, supervisor)
			before := runtimePoolTestGetPool(t, r, pool)
			if before.Status.ActiveInstance == nil {
				t.Fatal("test requires an admitted runtime instance")
			}
			admittedInstanceID := before.Status.ActiveInstance.RuntimeInstanceID
			sandboxSuspendTestSetIntent(t, r, pool, true)

			delegate := r.Client
			r.Client = &runtimePoolSuspendPrerequisiteFailureClient{Client: delegate, stage: stage}
			runtimePoolReconcile(t, r, pool)
			current := runtimePoolTestGetPool(t, r, pool)
			if current.Status.ActiveInstance == nil || current.Status.ActiveInstance.RuntimeInstanceID != admittedInstanceID {
				t.Fatalf("ActiveInstance = %+v after %s prerequisite failure; the suspend fence must survive", current.Status.ActiveInstance, stage)
			}
			preserved := &sandboxextv1beta1.SandboxClaim{}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
				t.Fatalf("SandboxClaim must survive %s prerequisite failure: %v", stage, err)
			}

			r.Client = delegate
			runtimePoolReconcile(t, r, pool)
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
				t.Fatalf("SandboxClaim must survive the recovered %s prerequisite: %v", stage, err)
			}
			if !preserved.DeletionTimestamp.IsZero() {
				t.Fatalf("SandboxClaim must not be deleting after the %s prerequisite recovers", stage)
			}
		})
	}
}

func TestWorkspaceRuntimePoolConfigurationFailurePreservesDurableState(t *testing.T) {
	for _, tt := range []struct {
		name    string
		prepare func(*testing.T, *RuntimePoolReconciler, *corev1alpha1.RuntimePool, *corev1.PersistentVolumeClaim)
	}{
		{
			name: "pending suspension",
			prepare: func(t *testing.T, r *RuntimePoolReconciler, pool *corev1alpha1.RuntimePool, _ *corev1.PersistentVolumeClaim) {
				t.Helper()
				sandboxSuspendTestSetIntent(t, r, pool, true)
			},
		},
		{
			name: "durable lineage",
			prepare: func(t *testing.T, r *RuntimePoolReconciler, pool *corev1alpha1.RuntimePool, pvc *corev1.PersistentVolumeClaim) {
				t.Helper()
				sandboxSuspendTestStampDurableLineage(t, r, pool, pvc)
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtimePoolWorkspaceTestScheme(t)
			pool := runtimePoolSandboxSuspendTestObject()
			supervisor := &fakeRuntimePoolSupervisorClient{}
			r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
			_, claim, _, durablePVC := sandboxSuspendTestReachServing(t, r, pool, supervisor)
			before := runtimePoolTestGetPool(t, r, pool)
			if before.Status.ActiveInstance == nil {
				t.Fatal("test requires an admitted runtime instance")
			}
			admittedInstanceID := before.Status.ActiveInstance.RuntimeInstanceID
			tt.prepare(t, r, pool, durablePVC)

			approvedImages := r.AllowedImages
			r.AllowedImages.Codex = "docker.io/sozercan/orka-acp@sha256:" + strings.Repeat("9", 64)
			runtimePoolReconcile(t, r, pool)
			current := runtimePoolTestGetPool(t, r, pool)
			if current.Status.ActiveInstance == nil || current.Status.ActiveInstance.RuntimeInstanceID != admittedInstanceID {
				t.Fatalf("ActiveInstance = %+v after configuration failure; the durable-state fence must survive", current.Status.ActiveInstance)
			}
			preserved := &sandboxextv1beta1.SandboxClaim{}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
				t.Fatalf("SandboxClaim must survive configuration failure: %v", err)
			}

			r.AllowedImages = approvedImages
			runtimePoolReconcile(t, r, pool)
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
				t.Fatalf("SandboxClaim must survive after configuration recovers: %v", err)
			}
			if !preserved.DeletionTimestamp.IsZero() {
				t.Fatal("SandboxClaim must not be deleting after configuration recovers")
			}
		})
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
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
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
	current.Annotations[sandboxSuspendedAnnotation] = `{"name":"checkpointed-sandbox","uid":"checkpointed-sandbox-uid","pvcUID":"recorded-pvc-uid","pvName":"pv-recorded","pvUID":"pv-recorded-uid"}`
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

// A settled suspension whose preserved PVC enters deletion before the cold
// resume must fail the resume closed immediately: patching the Sandbox back
// to Running would wait forever on a Pod Kubernetes cannot schedule against
// a terminating claim while the preserved data is being released.
func TestWorkspaceRuntimePoolResumeFailsClosedOnTerminatingPVC(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	sandbox, _, pod, durablePVC := sandboxSuspendTestReachServing(t, r, pool, supervisor)

	sandboxSuspendTestSetIntent(t, r, pool, true)
	runtimePoolReconcile(t, r, pool)
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "workspace-boot", true)
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if sandboxConsensualSuspendRecord(&current) == nil {
		t.Fatalf("consent record missing before resume (lifecycle=%s msg=%q)", current.Status.Lifecycle, current.Status.Message)
	}
	if err := r.Delete(context.Background(), &pod); err != nil {
		t.Fatalf("terminate sandbox pod: %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sandbox), sandbox); err != nil {
		t.Fatalf("re-read sandbox: %v", err)
	}
	sandbox.Status.Conditions = []metav1.Condition{{
		Type: string(sandboxv1beta1.SandboxConditionSuspended), Status: metav1.ConditionTrue,
		ObservedGeneration: sandbox.Generation,
		Reason:             sandboxv1beta1.SandboxReasonSuspendedPodTerminated,
		LastTransitionTime: metav1.Now(),
	}}
	if err := r.Update(context.Background(), sandbox); err != nil {
		t.Fatalf("mark sandbox suspended: %v", err)
	}
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped {
		t.Fatalf("suspended lifecycle = %s, want Stopped", current.Status.Lifecycle)
	}

	// The preserved PVC enters deletion, held only by protection.
	currentPVC := &corev1.PersistentVolumeClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(durablePVC), currentPVC); err != nil {
		t.Fatalf("read durable PVC: %v", err)
	}
	currentPVC.Finalizers = append(currentPVC.Finalizers, "kubernetes.io/pvc-protection")
	if err := r.Update(context.Background(), currentPVC); err != nil {
		t.Fatalf("hold PVC: %v", err)
	}
	if err := r.Delete(context.Background(), currentPVC); err != nil {
		t.Fatalf("start durable PVC deletion: %v", err)
	}

	sandboxSuspendTestSetIntent(t, r, pool, false)
	for range 4 {
		runtimePoolReconcile(t, r, pool)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatalf("a terminating preserved PVC must record terminal resume loss (lifecycle=%s msg=%q annotations=%v)",
			current.Status.Lifecycle, current.Status.Message, current.Annotations)
	}
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded {
		t.Fatalf("lifecycle = %s, want Degraded", current.Status.Lifecycle)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sandbox), sandbox); err == nil {
		if sandbox.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeRunning {
			t.Fatal("resume must never patch the Sandbox to Running against a terminating claim")
		}
	}
}

// A transient provider read failure before the suspend dispatch must
// preserve the admitted-identity fence: clearing it would let the recovered
// reconcile fall into unadmitted scale-down and delete the claim plus its
// durable PVC without a checkpoint.
func TestWorkspaceRuntimePoolPreservesFenceAcrossProviderReadFailure(t *testing.T) {
	const admittedInstanceID = "admitted-instance"

	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[runtimePoolWorkspaceSuspendAnnotation] = booleanTrueValue
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("request suspension: %v", err)
	}
	base := current.DeepCopy()
	current.Status.ActiveInstance = &corev1alpha1.RuntimePoolActiveInstanceStatus{RuntimeInstanceID: admittedInstanceID}
	if err := r.Status().Patch(context.Background(), &current, client.MergeFrom(base)); err != nil {
		t.Fatalf("seed admitted instance: %v", err)
	}
	result, err := r.finishWorkspacePoolProviderReadFailure(
		context.Background(), &current, runtimePoolConfig{}, errors.New("injected provider read failure"),
	)
	if err != nil {
		t.Fatalf("read-failure handling: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("result = %+v, want a bounded retry", result)
	}
	refreshed := runtimePoolTestGetPool(t, r, pool)
	if refreshed.Status.ActiveInstance == nil || refreshed.Status.ActiveInstance.RuntimeInstanceID != admittedInstanceID {
		t.Fatalf("ActiveInstance = %+v; the suspend fence must survive a transient provider read failure", refreshed.Status.ActiveInstance)
	}
}

// The linked workspace can request suspension before the adapter records the
// pool annotation. A provider read failure in that interval must preserve the
// admitted instance just like an already-recorded suspension request.
func TestWorkspaceRuntimePoolPreservesFenceAcrossPendingSuspendProviderReadFailure(t *testing.T) {
	const admittedInstanceID = "admitted-instance"

	scheme := runtimePoolWorkspaceTestScheme(t)
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add workspace scheme: %v", err)
	}
	pool := runtimePoolSandboxSuspendTestObject()
	if pool.Labels == nil {
		pool.Labels = map[string]string{}
	}
	pool.Labels[acpExecutionWorkspaceLinkLabel] = pendingSuspendWorkspaceName
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	pool.Annotations[acpExecutionWorkspaceUIDAnnotation] = pendingSuspendWorkspaceUID
	pool.Status.ActiveInstance = &corev1alpha1.RuntimePoolActiveInstanceStatus{RuntimeInstanceID: admittedInstanceID}
	linked := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: pool.Namespace,
			Name:      pendingSuspendWorkspaceName,
			UID:       types.UID(pendingSuspendWorkspaceUID),
		},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredSuspended},
	}
	r := runtimePoolTestReconciler(t, scheme, &fakeRuntimePoolSupervisorClient{}, pool, linked)
	current := runtimePoolTestGetPool(t, r, pool)
	result, err := r.finishWorkspacePoolProviderReadFailure(
		context.Background(), &current, runtimePoolConfig{}, errors.New("injected provider read failure"),
	)
	if err != nil {
		t.Fatalf("read-failure handling: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("result = %+v, want a bounded retry", result)
	}
	refreshed := runtimePoolTestGetPool(t, r, pool)
	if refreshed.Status.ActiveInstance == nil || refreshed.Status.ActiveInstance.RuntimeInstanceID != admittedInstanceID {
		t.Fatalf("ActiveInstance = %+v; the pending suspend fence must survive a provider read failure", refreshed.Status.ActiveInstance)
	}
}

// A drifted SandboxClaim of a resumed durable lineage is preserved even after
// the consent record retired: the permanent lineage fence keeps the sole
// preserved PVC from being deleted and replaced blank.
func TestWorkspaceRuntimePoolPreservesDriftedClaimForDurableLineage(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	_, claim, _, durablePVC := sandboxSuspendTestReachServing(t, r, pool, supervisor)
	sandboxSuspendTestStampDurableLineage(t, r, pool, durablePVC)
	// The provider mutates the claim out of band (spec drift).
	currentClaim := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), currentClaim); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	currentClaim.Spec.WarmPoolRef.Name = "drifted-warm-pool"
	if err := r.Update(context.Background(), currentClaim); err != nil {
		t.Fatalf("drift claim: %v", err)
	}
	runtimePoolReconcile(t, r, pool)
	preserved := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
		t.Fatalf("the lineage claim must survive drift: %v", err)
	}
	if !preserved.DeletionTimestamp.IsZero() {
		t.Fatal("the lineage claim must not be deleting after drift")
	}
	refreshed := runtimePoolTestGetPool(t, r, pool)
	if refreshed.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded {
		t.Fatalf("lifecycle = %s, want the fail-closed Degraded hold", refreshed.Status.Lifecycle)
	}
}

// A durable workspace PVC that enters deletion while the settled checkpoint
// record stands is terminal exactly like a vanished or replaced PVC: the
// preserved data is being irreversibly released, so the pool records the loss
// and degrades instead of continuing to publish a resumable suspension.
func TestWorkspaceRuntimePoolSettledSuspendFailsClosedOnTerminatingPVC(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	runtimePoolReconcile(t, r, pool)

	sandbox := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Namespace: pool.Namespace, Name: "checkpointed-sandbox", UID: types.UID("checkpointed-sandbox-uid"),
	}}
	if err := r.Create(context.Background(), sandbox); err != nil {
		t.Fatalf("create checkpointed sandbox: %v", err)
	}
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{
		Name: "pv-recorded", UID: types.UID("pv-recorded-uid"),
	}}
	if err := r.Create(context.Background(), pv); err != nil {
		t.Fatalf("create bound PV: %v", err)
	}
	durablePVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: pool.Namespace, Name: durableWorkspacePVCName(sandbox.Name),
			UID:        types.UID("recorded-pvc-uid"),
			Finalizers: []string{"kubernetes.io/pvc-protection"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv-recorded"},
	}
	if err := r.Create(context.Background(), durablePVC); err != nil {
		t.Fatalf("create durable PVC: %v", err)
	}
	// The matching PVC enters deletion, held only by pvc-protection.
	if err := r.Delete(context.Background(), durablePVC); err != nil {
		t.Fatalf("start durable PVC deletion: %v", err)
	}

	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[runtimePoolWorkspaceSuspendAnnotation] = booleanTrueValue
	current.Annotations[sandboxSuspendedAnnotation] = `{"name":"checkpointed-sandbox","uid":"checkpointed-sandbox-uid","pvcUID":"recorded-pvc-uid","pvName":"pv-recorded","pvUID":"pv-recorded-uid"}`
	current.Spec.DesiredReplicas = 0
	current.Generation++
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("record terminating-PVC suspension: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatalf("a terminating durable PVC under a settled checkpoint must record terminal loss, annotations=%v", current.Annotations)
	}
	if current.Annotations[sandboxSuspendedAnnotation] != "" {
		t.Fatal("the consent record over a vanishing volume must be retired")
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
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	runtimePoolReconcile(t, r, pool)
	_, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if claim == nil {
		t.Fatal("claim was not materialized")
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[sandboxSuspendedAnnotation] = `{"name":"sb","uid":"sb-uid","pvcUID":"pvc-uid","pvName":"pv-a","pvUID":"pv-a-uid"}`
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

// After a cold resume passes Serving and the consent record retires, the
// permanent durable-lineage fence still refuses instance recycling: the
// claim's PVC holds the sole copy of the resumed data, and a recycle would
// replace it with a blank volume under the same pool identity.
func TestRecycleRuntimePoolInstanceRefusesResumedDurableLineage(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	_, claim, _, durablePVC := sandboxSuspendTestReachServing(t, r, pool, supervisor)
	sandboxSuspendTestStampDurableLineage(t, r, pool, durablePVC)
	current := runtimePoolTestGetPool(t, r, pool)
	if err := r.recycleRuntimePoolInstance(context.Background(), &current, nil); err == nil ||
		!strings.Contains(err.Error(), "resumed durable lineage") {
		t.Fatalf("recycle error = %v, want the lineage-preserving refusal", err)
	}
	preserved := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
		t.Fatalf("claim must survive the refused recycle: %v", err)
	}
	if !preserved.DeletionTimestamp.IsZero() {
		t.Fatal("claim must not be deleted while the durable lineage stands")
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatal("a refused durable-lineage recycle must record terminal resume loss")
	}
}

// A controller upgrade can set desired replicas to zero after resume lifts
// the suspend intent but before Serving stamps the permanent lineage. The
// standing checkpoint record must protect the sole durable claim in that gap.
func TestWorkspaceRuntimePoolScaleDownPreservesInFlightCheckpoint(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	sandbox, claim, _, durablePVC := sandboxSuspendTestReachServing(t, r, pool, supervisor)
	volume := sandboxSuspendTestDurableLineageRecord(t, r, durablePVC)
	checkpoint, err := json.Marshal(sandboxSuspendRecord{
		Name: sandbox.Name, UID: sandbox.UID,
		PVCUID: volume.PVCUID, PVName: volume.PVName, PVUID: volume.PVUID,
		RequestedAt: metav1.Now(),
	})
	if err != nil {
		t.Fatalf("encode checkpoint record: %v", err)
	}

	current := runtimePoolTestGetPool(t, r, pool)
	current.Spec.DesiredReplicas = 0
	current.Annotations[sandboxSuspendedAnnotation] = string(checkpoint)
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("record in-flight checkpoint scale-down: %v", err)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	statusBase := current.DeepCopy()
	current.Status.ActiveInstance = nil
	current.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
	current.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	if err := r.Status().Patch(context.Background(), &current, client.MergeFrom(statusBase)); err != nil {
		t.Fatalf("mark resume admission pending: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	preserved := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
		t.Fatalf("in-flight checkpoint claim must survive upgrade scale-down: %v", err)
	}
	if !preserved.DeletionTimestamp.IsZero() {
		t.Fatal("in-flight checkpoint claim must not be deleting")
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] != "" {
		t.Fatalf("in-flight checkpoint was incorrectly declared lost: %q", current.Annotations[runtimePoolWorkspaceResumeLostAnnotation])
	}
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		current.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed ||
		!strings.Contains(current.Status.Message, "checkpoint") {
		t.Fatalf("status = %s/%s %q, want checkpoint-preserving Degraded/Closed hold",
			current.Status.Lifecycle, current.Status.AdmissionState, current.Status.Message)
	}
}

func TestWorkspaceRuntimePoolUnhealthyResumedLineagePersistsAdmissionClosure(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	_, claim, _, durablePVC := sandboxSuspendTestReachServing(t, r, pool, supervisor)
	sandboxSuspendTestStampDurableLineage(t, r, pool, durablePVC)
	supervisor.probe.Status.Lifecycle = harnessv2.SupervisorLifecycleUnhealthy

	runtimePoolReconcile(t, r, pool)

	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		current.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed ||
		!strings.Contains(current.Status.Message, "resumed durable lineage") {
		t.Fatalf("status = %s/%s %q, want persisted Degraded/Closed recycle refusal",
			current.Status.Lifecycle, current.Status.AdmissionState, current.Status.Message)
	}
	if current.Status.ActiveInstance == nil {
		t.Fatal("the refused recycle must retain the exact unhealthy instance fence")
	}
	preserved := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
		t.Fatalf("durable lineage claim must survive the refused unhealthy-instance recycle: %v", err)
	}
	if !preserved.DeletionTimestamp.IsZero() {
		t.Fatal("durable lineage claim must not be deleting after the refused unhealthy-instance recycle")
	}
}

func TestWorkspaceRuntimePoolScaleDownFailsLostResumedDurableFence(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, &fakeRuntimePoolSupervisorClient{}, pool)
	runtimePoolReconcile(t, r, pool)
	_, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if claim == nil {
		t.Fatal("claim was not materialized")
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[runtimePoolDurableLineageAnnotation] = `{"pvcUID":"pvc-uid","pvName":"pv-a","pvUID":"pv-a-uid"}`
	current.Annotations[runtimePoolWorkspaceSuspendAnnotation] = booleanTrueValue
	current.Spec.DesiredReplicas = 0
	current.Generation++
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("record durable lineage and scale to zero: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		!strings.Contains(current.Status.Message, "no admitted runtime identity") {
		t.Fatalf("lifecycle = %s message = %q, want terminal lost-fence degradation", current.Status.Lifecycle, current.Status.Message)
	}
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatal("a resumed lineage without an admitted suspension fence must record terminal loss")
	}
	preserved := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
		t.Fatalf("claim must survive ordinary scale-down while durable lineage stands: %v", err)
	}
	if !preserved.DeletionTimestamp.IsZero() {
		t.Fatal("claim must not be deleting while durable lineage stands")
	}
}

// A probe-validated instance that is not the admitted ActiveInstance must fail
// the suspension closed instead of being silently adopted and checkpointed.
func TestWorkspaceRuntimePoolSuspendRejectsReplacedInstance(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
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
	sandboxSuspendTestDurablePVC(t, r, pool, sandbox, "replaced-instance-pvc-uid")
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
const (
	pendingSuspendWorkspaceName     = "acp-ws-pending-suspend"
	pendingSuspendWorkspaceUID      = "pending-suspend-uid"
	pendingSuspendRuntimeInstanceID = "held-instance"
)

func TestWorkspaceRuntimePoolHoldsScaleDownWhileSuspendIntentPending(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add workspace scheme: %v", err)
	}
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
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
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	// The hold is honored only for the pool's exact workspace incarnation.
	current.Annotations[acpExecutionWorkspaceUIDAnnotation] = pendingSuspendWorkspaceUID
	current.Spec.DesiredReplicas = 0
	current.Generation++
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("scale pool to zero: %v", err)
	}
	linked := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: pool.Namespace, Name: pendingSuspendWorkspaceName, UID: pendingSuspendWorkspaceUID},
		Spec:       workspacev1alpha1.ExecutionWorkspaceSpec{DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredSuspended},
	}
	if err := r.Create(context.Background(), linked); err != nil {
		t.Fatalf("create linked workspace: %v", err)
	}
	// The pool had an admitted instance when the race landed; the wait must
	// preserve that fence or the suspend flow that follows the recorded
	// intent would fall into unadmitted scale-down and delete the durable
	// PVC without a checkpoint.
	current = runtimePoolTestGetPool(t, r, pool)
	baseStatus := current.DeepCopy()
	current.Status.ActiveInstance = &corev1alpha1.RuntimePoolActiveInstanceStatus{RuntimeInstanceID: pendingSuspendRuntimeInstanceID}
	if err := r.Status().Patch(context.Background(), &current, client.MergeFrom(baseStatus)); err != nil {
		t.Fatalf("seed admitted instance: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDraining ||
		!strings.Contains(current.Status.Message, "waiting for the durable pool suspension intent") {
		t.Fatalf("lifecycle = %s message = %q, want a held scale-down", current.Status.Lifecycle, current.Status.Message)
	}
	if current.Status.ActiveInstance == nil || current.Status.ActiveInstance.RuntimeInstanceID != pendingSuspendRuntimeInstanceID {
		t.Fatalf("ActiveInstance = %+v; the admitted-identity fence must survive the intent wait", current.Status.ActiveInstance)
	}
	preserved := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
		t.Fatalf("claim must be preserved while suspension intent is pending: %v", err)
	}
	if !preserved.DeletionTimestamp.IsZero() {
		t.Fatal("claim must not be deleting while suspension intent is pending")
	}
}

func TestWorkspaceRuntimePoolClaimFailurePreservesPendingSuspendFence(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add workspace scheme: %v", err)
	}
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	_, claim, _, _ := sandboxSuspendTestReachServing(t, r, pool, supervisor)

	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.ActiveInstance == nil {
		t.Fatal("test requires an admitted runtime instance")
	}
	wantRuntimeInstanceID := current.Status.ActiveInstance.RuntimeInstanceID
	if current.Labels == nil {
		current.Labels = map[string]string{}
	}
	current.Labels[acpExecutionWorkspaceLinkLabel] = pendingSuspendWorkspaceName
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[acpExecutionWorkspaceUIDAnnotation] = pendingSuspendWorkspaceUID
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("link pool to pending workspace suspension: %v", err)
	}
	linked := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: pool.Namespace, Name: pendingSuspendWorkspaceName, UID: pendingSuspendWorkspaceUID},
		Spec:       workspacev1alpha1.ExecutionWorkspaceSpec{DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredSuspended},
	}
	if err := r.Create(context.Background(), linked); err != nil {
		t.Fatalf("create linked workspace: %v", err)
	}
	currentClaim := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), currentClaim); err != nil {
		t.Fatalf("read SandboxClaim: %v", err)
	}
	currentClaim.Status.Conditions = []metav1.Condition{{
		Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse,
		Reason: sandboxClaimProvisioningFailedReason, Message: sandboxClaimProviderExhaustedMessage,
	}}
	if err := r.Update(context.Background(), currentClaim); err != nil {
		t.Fatalf("publish SandboxClaim failure: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.ActiveInstance == nil || current.Status.ActiveInstance.RuntimeInstanceID != wantRuntimeInstanceID {
		t.Fatalf("ActiveInstance = %+v; the pending suspension must preserve %q across claim failure",
			current.Status.ActiveInstance, wantRuntimeInstanceID)
	}

	// Once the adapter records the pool intent, the retained identity routes
	// the same claim through authenticated suspension instead of unadmitted
	// scale-down and PVC deletion.
	sandboxSuspendTestSetIntent(t, r, pool, true)
	runtimePoolReconcile(t, r, pool)
	if supervisor.drainCalls != 1 {
		t.Fatalf("drain calls = %d, want the retained instance to enter suspension", supervisor.drainCalls)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), &sandboxextv1beta1.SandboxClaim{}); err != nil {
		t.Fatalf("SandboxClaim must survive the claim-failure suspension race: %v", err)
	}
}

func TestWorkspaceRuntimePoolPreservesPendingSuspendFenceDuringPodAmbiguity(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add workspace scheme: %v", err)
	}
	pool := runtimePoolSandboxSuspendTestObject()
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, &fakeRuntimePoolSupervisorClient{}, pool)
	runtimePoolReconcile(t, r, pool)
	template, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if template == nil || claim == nil {
		t.Fatal("workspace template and claim were not materialized")
	}
	first := runtimePoolWorkspaceTestMaterialization(t, r, pool, template, "10.0.0.81")
	second := first.DeepCopy()
	second.ResourceVersion = ""
	second.Name = "sandbox-pod-ambiguous"
	second.UID = types.UID("sandbox-pod-ambiguous-uid")
	second.Status.PodIP = "10.0.0.82"
	second.Status.PodIPs = []corev1.PodIP{{IP: second.Status.PodIP}}
	runtimePoolTestCreatePod(t, r, second)

	current := runtimePoolTestGetPool(t, r, pool)
	if current.Labels == nil {
		current.Labels = map[string]string{}
	}
	current.Labels[acpExecutionWorkspaceLinkLabel] = pendingSuspendWorkspaceName
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[acpExecutionWorkspaceUIDAnnotation] = pendingSuspendWorkspaceUID
	current.Spec.DesiredReplicas = 0
	current.Generation++
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("scale pool to zero: %v", err)
	}
	linked := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: pool.Namespace, Name: pendingSuspendWorkspaceName, UID: pendingSuspendWorkspaceUID},
		Spec:       workspacev1alpha1.ExecutionWorkspaceSpec{DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredSuspended},
	}
	if err := r.Create(context.Background(), linked); err != nil {
		t.Fatalf("create linked workspace: %v", err)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	baseStatus := current.DeepCopy()
	current.Status.ActiveInstance = &corev1alpha1.RuntimePoolActiveInstanceStatus{RuntimeInstanceID: pendingSuspendRuntimeInstanceID}
	if err := r.Status().Patch(context.Background(), &current, client.MergeFrom(baseStatus)); err != nil {
		t.Fatalf("seed admitted instance: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleAmbiguous {
		t.Fatalf("lifecycle = %s, want Ambiguous", current.Status.Lifecycle)
	}
	if current.Status.ActiveInstance == nil || current.Status.ActiveInstance.RuntimeInstanceID != pendingSuspendRuntimeInstanceID {
		t.Fatalf("ActiveInstance = %+v; pending suspension must preserve the fence across Pod ambiguity", current.Status.ActiveInstance)
	}
	preserved := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
		t.Fatalf("claim must survive pre-annotation Pod ambiguity: %v", err)
	}
	if !preserved.DeletionTimestamp.IsZero() {
		t.Fatal("claim must not be deleting during pre-annotation Pod ambiguity")
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
	durablePVC := sandboxSuspendTestDurablePVC(t, r, pool, sandbox, "durable-pvc-uid")
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "workspace-boot", false)
	runtimePoolReconcile(t, r, pool)
	if got := runtimePoolTestGetPool(t, r, pool).Status.Lifecycle; got != corev1alpha1.RuntimePoolLifecycleServing {
		t.Fatalf("lifecycle = %s (msg=%q), want Serving", got, runtimePoolTestGetPool(t, r, pool).Status.Message)
	}
	return sandbox, currentClaim, pod, durablePVC
}

func sandboxSuspendTestReachStopped(
	t *testing.T,
	r *RuntimePoolReconciler,
	pool *corev1alpha1.RuntimePool,
	supervisor *fakeRuntimePoolSupervisorClient,
) (*sandboxv1beta1.Sandbox, *sandboxextv1beta1.SandboxClaim, *corev1.PersistentVolumeClaim) {
	t.Helper()
	sandbox, claim, pod, durablePVC := sandboxSuspendTestReachServing(t, r, pool, supervisor)
	sandboxSuspendTestSetIntent(t, r, pool, true)
	runtimePoolReconcile(t, r, pool)
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "workspace-boot", true)
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if sandboxConsensualSuspendRecord(&current) == nil {
		t.Fatalf("consent record missing before suspension settlement (lifecycle=%s msg=%q)", current.Status.Lifecycle, current.Status.Message)
	}
	if err := r.Delete(context.Background(), &pod); err != nil {
		t.Fatalf("terminate sandbox pod: %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sandbox), sandbox); err != nil {
		t.Fatalf("read suspending Sandbox: %v", err)
	}
	sandbox.Status.Conditions = []metav1.Condition{{
		Type: string(sandboxv1beta1.SandboxConditionSuspended), Status: metav1.ConditionTrue,
		ObservedGeneration: sandbox.Generation,
		Reason:             sandboxv1beta1.SandboxReasonSuspendedPodTerminated,
		LastTransitionTime: metav1.Now(),
	}}
	if err := r.Update(context.Background(), sandbox); err != nil {
		t.Fatalf("mark Sandbox suspended: %v", err)
	}
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped {
		t.Fatalf("suspended lifecycle = %s, want Stopped", current.Status.Lifecycle)
	}
	return sandbox, claim, durablePVC
}

func TestWorkspaceRuntimePoolResumeRecordsTerminalClaimIdentityLoss(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *RuntimePoolReconciler, *sandboxv1beta1.Sandbox, *sandboxextv1beta1.SandboxClaim)
	}{
		{
			name: "missing claim",
			mutate: func(t *testing.T, r *RuntimePoolReconciler, _ *sandboxv1beta1.Sandbox, claim *sandboxextv1beta1.SandboxClaim) {
				t.Helper()
				if err := r.Delete(context.Background(), claim); err != nil {
					t.Fatalf("delete SandboxClaim: %v", err)
				}
			},
		},
		{
			name: "mismatched Sandbox owner",
			mutate: func(t *testing.T, r *RuntimePoolReconciler, sandbox *sandboxv1beta1.Sandbox, _ *sandboxextv1beta1.SandboxClaim) {
				t.Helper()
				current := &sandboxv1beta1.Sandbox{}
				if err := r.Get(context.Background(), client.ObjectKeyFromObject(sandbox), current); err != nil {
					t.Fatalf("read suspended Sandbox: %v", err)
				}
				owner := metav1.GetControllerOf(current)
				if owner == nil {
					t.Fatal("suspended Sandbox has no controller owner")
				}
				for i := range current.OwnerReferences {
					if current.OwnerReferences[i].Controller != nil && *current.OwnerReferences[i].Controller {
						current.OwnerReferences[i].UID = types.UID("foreign-claim-uid")
					}
				}
				if err := r.Update(context.Background(), current); err != nil {
					t.Fatalf("replace suspended Sandbox owner: %v", err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtimePoolWorkspaceTestScheme(t)
			pool := runtimePoolSandboxSuspendTestObject()
			supervisor := &fakeRuntimePoolSupervisorClient{}
			r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
			sandbox, claim, _ := sandboxSuspendTestReachStopped(t, r, pool, supervisor)

			test.mutate(t, r, sandbox, claim)
			sandboxSuspendTestSetIntent(t, r, pool, false)
			for range 4 {
				runtimePoolReconcile(t, r, pool)
				if runtimePoolTestGetPool(t, r, pool).Annotations[runtimePoolWorkspaceResumeLostAnnotation] != "" {
					break
				}
			}

			current := runtimePoolTestGetPool(t, r, pool)
			if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
				t.Fatalf("claim identity loss did not record terminal resume loss (status=%s msg=%q)", current.Status.Lifecycle, current.Status.Message)
			}
			if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
				current.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
				t.Fatalf("status = %s/%s, want Degraded/Closed", current.Status.Lifecycle, current.Status.AdmissionState)
			}
		})
	}
}

// Once a resumed pool reaches Serving, the consent record retires and the
// permanent lineage marker protects the sole durable PVC. Every later
// integrity, attestation, or credential conflict must retain the claim.
func TestWorkspaceRuntimePoolPreservesDurableLineageAcrossPostResumeFailures(t *testing.T) {
	tests := []struct {
		name        string
		wantMessage string
		mutate      func(*testing.T, *RuntimePoolReconciler, *corev1alpha1.RuntimePool, corev1.Pod)
	}{
		{
			name:        "template integrity failure",
			wantMessage: "integrity check",
			mutate: func(t *testing.T, r *RuntimePoolReconciler, pool *corev1alpha1.RuntimePool, _ corev1.Pod) {
				t.Helper()
				template, _, _ := runtimePoolWorkspaceTestChildren(t, r, pool)
				if template == nil {
					t.Fatal("workspace template was not materialized")
				}
				template.Spec.PodTemplate.Spec.Containers[0].Image = runtimePoolTestTamperedImage
				if err := r.Update(context.Background(), template); err != nil {
					t.Fatalf("tamper SandboxTemplate: %v", err)
				}
			},
		},
		{
			name:        "attestation failure",
			wantMessage: "failed attestation",
			mutate: func(t *testing.T, r *RuntimePoolReconciler, _ *corev1alpha1.RuntimePool, pod corev1.Pod) {
				t.Helper()
				currentPod := &corev1.Pod{}
				if err := r.Get(context.Background(), client.ObjectKeyFromObject(&pod), currentPod); err != nil {
					t.Fatalf("read runtime Pod: %v", err)
				}
				currentPod.Spec.Containers[0].Image = runtimePoolTestTamperedImage
				if err := r.Update(context.Background(), currentPod); err != nil {
					t.Fatalf("tamper runtime Pod: %v", err)
				}
			},
		},
		{
			name:        "bootstrap instance conflict",
			wantMessage: "instance conflicted",
			mutate: func(t *testing.T, r *RuntimePoolReconciler, pool *corev1alpha1.RuntimePool, _ corev1.Pod) {
				t.Helper()
				current := runtimePoolTestGetPool(t, r, pool)
				current.Annotations[runtimePoolBootstrapInstanceBindingAnnotation] = `{"authSecretUID":"foreign-auth","workloadUID":"foreign-workload"}`
				if err := r.Update(context.Background(), &current); err != nil {
					t.Fatalf("replace bootstrap instance binding: %v", err)
				}
			},
		},
		{
			name:        "credential conflict",
			wantMessage: "credential-seeded",
			mutate: func(t *testing.T, r *RuntimePoolReconciler, _ *corev1alpha1.RuntimePool, _ corev1.Pod) {
				t.Helper()
				r.WorkspaceCredentialSeeder = func(context.Context, string, string, []byte, harnessv2.CredentialBootstrapRequest) (bool, error) {
					return false, errWorkspaceCredentialConflict
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtimePoolWorkspaceTestScheme(t)
			pool := runtimePoolSandboxSuspendTestObject()
			supervisor := &fakeRuntimePoolSupervisorClient{}
			r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
			_, claim, pod, durablePVC := sandboxSuspendTestReachServing(t, r, pool, supervisor)
			sandboxSuspendTestStampDurableLineage(t, r, pool, durablePVC)

			test.mutate(t, r, pool, pod)
			runtimePoolReconcile(t, r, pool)

			preserved := &sandboxextv1beta1.SandboxClaim{}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
				t.Fatalf("durable lineage claim must survive: %v", err)
			}
			if !preserved.DeletionTimestamp.IsZero() {
				t.Fatal("durable lineage claim must not be deleting")
			}
			status := runtimePoolTestGetPool(t, r, pool).Status
			if status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
				status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed ||
				!strings.Contains(status.Message, test.wantMessage) {
				t.Fatalf("status = %s/%s %q, want Degraded/Closed containing %q", status.Lifecycle, status.AdmissionState, status.Message, test.wantMessage)
			}
		})
	}
}

func TestWorkspaceRuntimePoolRetainsLiveCheckpointClaimAcrossTemplateDrift(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	sandbox, claim, _, durablePVC := sandboxSuspendTestReachServing(t, r, pool, supervisor)
	volume := sandboxSuspendTestDurableLineageRecord(t, r, durablePVC)
	record, err := json.Marshal(sandboxSuspendRecord{
		Name: sandbox.Name, UID: sandbox.UID,
		PVCUID: volume.PVCUID, PVName: volume.PVName, PVUID: volume.PVUID,
		RequestedAt: metav1.Now(),
	})
	if err != nil {
		t.Fatalf("encode checkpoint record: %v", err)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	current.Annotations[sandboxSuspendedAnnotation] = string(record)
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("record pending cold resume: %v", err)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	current.Status.ActiveInstance = nil
	current.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
	current.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	if err := r.Status().Update(context.Background(), &current); err != nil {
		t.Fatalf("mark cold resume pending: %v", err)
	}

	r.ControllerAPIURL = "http://orka-api-replacement.default.svc:8080"
	runtimePoolReconcile(t, r, pool)

	preserved := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
		t.Fatalf("live checkpoint claim must survive template drift: %v", err)
	}
	if !preserved.DeletionTimestamp.IsZero() {
		t.Fatal("live checkpoint claim entered deletion during template drift")
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		current.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed ||
		!strings.Contains(current.Status.Message, "live durable workspace") {
		t.Fatalf("status = %s/%s %q, want a retained live-checkpoint hold",
			current.Status.Lifecycle, current.Status.AdmissionState, current.Status.Message)
	}
	if sandboxConsensualSuspendRecord(&current) == nil {
		t.Fatal("pre-Serving checkpoint record was retired during template drift")
	}
}

// A transient Sandbox read failure after the persisted Quiescent barrier must
// preserve the admitted identity and the SandboxClaim: clearing the instance
// would send the next reconcile into ordinary scale-down, deleting the claim
// and its durable PVC and losing unpublished workspace data to a read outage.
func TestWorkspaceRuntimePoolPreservesClaimAcrossPreCheckpointReadFailure(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
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
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
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
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
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

// A previously admitted instance that transiently loses readiness during a
// requested suspension must hold degraded with the claim retained: ordinary
// scale-down would clear the instance and delete the SandboxClaim plus its
// durable PVC on a readiness blip.
func TestWorkspaceRuntimePoolRetainsClaimOnReadinessLossDuringSuspend(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	_, claim, pod, _ := sandboxSuspendTestReachServing(t, r, pool, supervisor)

	sandboxSuspendTestSetIntent(t, r, pool, true)
	// The admitted Pod vanishes before any pre-suspension probe.
	if err := r.Delete(context.Background(), &pod); err != nil {
		t.Fatalf("delete admitted pod: %v", err)
	}
	for range 3 {
		runtimePoolReconcile(t, r, pool)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.ActiveInstance == nil {
		t.Fatalf("readiness loss during suspension must retain the admitted identity (lifecycle=%s msg=%q)",
			current.Status.Lifecycle, current.Status.Message)
	}
	currentClaim := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), currentClaim); err != nil {
		t.Fatalf("SandboxClaim must survive readiness loss during suspension: %v", err)
	}
	if currentClaim.DeletionTimestamp != nil {
		t.Fatal("SandboxClaim must not be deleting after readiness loss during suspension")
	}
}

func TestWorkspaceRuntimePoolDeletionBypassesReadinessLossHold(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	_, claim, pod, _ := sandboxSuspendTestReachServing(t, r, pool, supervisor)

	sandboxSuspendTestSetIntent(t, r, pool, true)
	if err := r.Delete(context.Background(), &pod); err != nil {
		t.Fatalf("delete admitted pod: %v", err)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if err := r.Delete(context.Background(), &current); err != nil {
		t.Fatalf("delete RuntimePool: %v", err)
	}

	for range 6 {
		runtimePoolReconcile(t, r, pool)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), &sandboxextv1beta1.SandboxClaim{}); !apierrors.IsNotFound(err) {
		t.Fatalf("SandboxClaim survived explicit deletion after readiness loss, err=%v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), &corev1alpha1.RuntimePool{}); !apierrors.IsNotFound(err) {
		t.Fatalf("RuntimePool finalizer did not complete after readiness loss, err=%v", err)
	}
}

func TestWorkspaceRuntimePoolDeletionWaitsForCheckpointSandboxRead(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	sandbox, claim, pod, _ := sandboxSuspendTestReachServing(t, r, pool, supervisor)

	sandboxSuspendTestSetIntent(t, r, pool, true)
	runtimePoolReconcile(t, r, pool)
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "workspace-boot", true)
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if sandboxConsensualSuspendRecord(&current) == nil {
		t.Fatalf("consent record missing before deletion (lifecycle=%s msg=%q)", current.Status.Lifecycle, current.Status.Message)
	}

	// The provider terminated the Pod but its Sandbox status API is
	// unavailable before the Suspended condition can be observed.
	if err := r.Delete(context.Background(), &pod); err != nil {
		t.Fatalf("delete admitted pod: %v", err)
	}
	if err := r.Delete(context.Background(), &current); err != nil {
		t.Fatalf("delete RuntimePool: %v", err)
	}
	r.APIReader = &runtimePoolFailSandboxReadsReader{Reader: r.Client}

	var sandboxReadErr error
	for range 8 {
		_, sandboxReadErr = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
		if sandboxReadErr != nil {
			break
		}
	}
	if sandboxReadErr == nil || !strings.Contains(sandboxReadErr.Error(), "injected Sandbox read failure") {
		t.Fatalf("finalize with unreadable checkpointed Sandbox error = %v, want fail-closed read error", sandboxReadErr)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), &corev1alpha1.RuntimePool{}); err != nil {
		t.Fatalf("RuntimePool finalized without proving the checkpointed Sandbox absent: %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), &sandboxextv1beta1.SandboxClaim{}); !apierrors.IsNotFound(err) {
		t.Fatalf("SandboxClaim survived explicit deletion during checkpoint settlement, err=%v", err)
	}

	// Once the provider API is readable, simulate owner-reference garbage
	// collection removing the exact Sandbox. Finalization can then prove its
	// absence and complete.
	r.APIReader = r.Client
	if err := r.Delete(context.Background(), sandbox); err != nil {
		t.Fatalf("delete checkpointed Sandbox after reads recover: %v", err)
	}
	for range 8 {
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), &corev1alpha1.RuntimePool{}); apierrors.IsNotFound(err) {
			break
		}
		runtimePoolReconcile(t, r, pool)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), &corev1alpha1.RuntimePool{}); !apierrors.IsNotFound(err) {
		t.Fatalf("RuntimePool finalizer did not complete after Sandbox reads recovered, err=%v", err)
	}
}

func TestWorkspaceRuntimePoolSuspensionWaitsForSoleAdmittedPod(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	_, claim, admitted, _ := sandboxSuspendTestReachServing(t, r, pool, supervisor)

	extra := admitted.DeepCopy()
	extra.ResourceVersion = ""
	extra.Name = "sandbox-pod-overlap"
	extra.UID = types.UID("sandbox-pod-overlap-uid")
	extra.Status.PodIP = "10.0.0.82"
	extra.Status.PodIPs = []corev1.PodIP{{IP: extra.Status.PodIP}}
	extra.Status.Conditions = nil
	runtimePoolTestCreatePod(t, r, extra)
	sandboxSuspendTestSetIntent(t, r, pool, true)

	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		!strings.Contains(current.Status.Message, "sole possible workspace writer") {
		t.Fatalf("lifecycle = %s message = %q, want an extra-Pod suspension hold", current.Status.Lifecycle, current.Status.Message)
	}
	if current.Status.ActiveInstance == nil {
		t.Fatal("the admitted identity must survive the extra-Pod suspension hold")
	}
	if supervisor.drainCalls != 0 {
		t.Fatalf("drain calls = %d, want none while another runtime Pod may still write the workspace", supervisor.drainCalls)
	}
	currentClaim := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), currentClaim); err != nil {
		t.Fatalf("SandboxClaim must survive the extra-Pod suspension hold: %v", err)
	}
	if currentClaim.DeletionTimestamp != nil {
		t.Fatal("SandboxClaim must not be deleting while another runtime Pod remains")
	}
}

func TestWorkspaceRuntimePoolRejectsUnpinnedRetainStorageClass(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	const legacyClassName = "legacy-retain-class"
	pool.Spec.ExecutionWorkspace.AgentSandbox.SuspendVolume.StorageClassName = legacyClassName
	pool.Spec.ExecutionWorkspace.AgentSandbox.SuspendVolume.StorageClassUID = ""
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	retain := corev1.PersistentVolumeReclaimRetain
	legacyClass := &storagev1.StorageClass{
		ObjectMeta:    metav1.ObjectMeta{Name: legacyClassName, UID: types.UID("replacement-retain-class-uid")},
		Provisioner:   acpTestStorageProvisioner,
		ReclaimPolicy: &retain,
	}
	if err := r.Create(context.Background(), legacyClass); err != nil {
		t.Fatalf("create legacy StorageClass replacement: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	_, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if claim != nil {
		t.Fatal("an unpinned Retain StorageClass must not provision a durable claim")
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		!strings.Contains(current.Status.Message, "only Delete reclaim is admitted") {
		t.Fatalf("status = %s %q, want fail-closed Retain-class rejection", current.Status.Lifecycle, current.Status.Message)
	}
}

// Attestation re-verifies the pinned StorageClass identity: a class deleted
// and recreated between claim creation and provisioning must fail admission
// instead of executing on storage outside the frozen UID binding.
func TestWorkspaceRuntimePoolAttestationReverifiesPinnedStorageClass(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	const durableClassName = "durable-class"
	pool.Spec.ExecutionWorkspace.AgentSandbox.SuspendVolume.StorageClassName = durableClassName
	pool.Spec.ExecutionWorkspace.AgentSandbox.SuspendVolume.StorageClassUID = "pinned-class-uid"
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme storage: %v", err)
	}
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	reclaim := corev1.PersistentVolumeReclaimDelete
	pinned := &storagev1.StorageClass{
		ObjectMeta:    metav1.ObjectMeta{Name: durableClassName, UID: types.UID("pinned-class-uid")},
		ReclaimPolicy: &reclaim,
	}
	if err := r.Create(context.Background(), pinned); err != nil {
		t.Fatalf("create pinned storage class: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	// The StorageClass is deleted and recreated AFTER claim creation but
	// before provisioning completes: the delayed-binding window.
	if err := r.Delete(context.Background(), pinned); err != nil {
		t.Fatalf("delete pinned storage class: %v", err)
	}
	replacement := &storagev1.StorageClass{
		ObjectMeta:    metav1.ObjectMeta{Name: durableClassName, UID: types.UID("replacement-class-uid")},
		ReclaimPolicy: &reclaim,
	}
	if err := r.Create(context.Background(), replacement); err != nil {
		t.Fatalf("create replacement storage class: %v", err)
	}
	template, _, claim := runtimePoolWorkspaceTestChildren(t, r, pool)
	if template == nil || claim == nil {
		t.Fatal("workspace template and claim were not materialized")
	}
	pod := runtimePoolWorkspaceTestMaterialization(t, r, pool, template, "10.0.0.82")
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
	claimBase := currentClaim.DeepCopy()
	if currentClaim.Annotations == nil {
		currentClaim.Annotations = map[string]string{}
	}
	currentClaim.Annotations[sandboxextv1beta1.AssignedSandboxNameAnnotation] = sandbox.Name
	if err := r.Patch(context.Background(), currentClaim, client.MergeFrom(claimBase)); err != nil {
		t.Fatalf("record adopted sandbox: %v", err)
	}
	// Provisioning races ahead and binds the PVC under the replacement class
	// before the controller observes its first materialization. The first
	// attestation must still compare the live class UID with the frozen one.
	sandboxSuspendTestDurablePVC(t, r, pool, sandbox, "durable-pvc-uid")
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "workspace-boot", false)
	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing {
		t.Fatalf("a replaced pinned StorageClass must never be admitted (msg=%q)", current.Status.Message)
	}
	if !strings.Contains(current.Status.Message, "replaced") {
		t.Fatalf("status message = %q, want the replaced-class rejection", current.Status.Message)
	}
}

func TestWorkspaceRuntimePoolKeepsAttestedPVCWhenStorageClassIsReplaced(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	_, claim, _, _ := sandboxSuspendTestReachServing(t, r, pool, supervisor)

	className := pool.Spec.ExecutionWorkspace.AgentSandbox.SuspendVolume.StorageClassName
	pinned := &storagev1.StorageClass{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: className}, pinned); err != nil {
		t.Fatalf("read pinned StorageClass: %v", err)
	}
	if err := r.Delete(context.Background(), pinned); err != nil {
		t.Fatalf("delete pinned StorageClass: %v", err)
	}
	replacement := pinned.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = types.UID("replacement-storage-class-uid")
	if err := r.Create(context.Background(), replacement); err != nil {
		t.Fatalf("create replacement StorageClass: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing {
		t.Fatalf("lifecycle = %s message = %q, want established storage to remain Serving", current.Status.Lifecycle, current.Status.Message)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), &sandboxextv1beta1.SandboxClaim{}); err != nil {
		t.Fatalf("attested SandboxClaim must survive later StorageClass replacement: %v", err)
	}
}

// A deployed-template identity error during suspension must preserve the
// admitted fence. Clearing it would make the next reconcile treat the live
// writer as unadmitted and delete its durable claim without a checkpoint.
func TestWorkspaceRuntimePoolSuspendPreservesFenceOnDeployedIdentityError(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	_, claim, pod, _ := sandboxSuspendTestReachServing(t, r, pool, supervisor)

	before := runtimePoolTestGetPool(t, r, pool)
	if before.Status.ActiveInstance == nil {
		t.Fatal("serving pool has no admitted instance")
	}
	wantRuntimeInstanceID := before.Status.ActiveInstance.RuntimeInstanceID
	currentPod := &corev1.Pod{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(&pod), currentPod); err != nil {
		t.Fatalf("read admitted pod: %v", err)
	}
	if currentPod.Annotations == nil {
		currentPod.Annotations = map[string]string{}
	}
	currentPod.Annotations[runtimePoolProfileAnnotation] = "sha256:" + strings.Repeat("f", 64)
	if err := r.Update(context.Background(), currentPod); err != nil {
		t.Fatalf("mutate deployed identity metadata: %v", err)
	}
	sandboxSuspendTestSetIntent(t, r, pool, true)

	for range 3 {
		runtimePoolReconcile(t, r, pool)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.ActiveInstance == nil || current.Status.ActiveInstance.RuntimeInstanceID != wantRuntimeInstanceID {
		t.Fatalf("active instance = %+v, want the admitted suspension fence %q preserved",
			current.Status.ActiveInstance, wantRuntimeInstanceID)
	}
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded ||
		current.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed ||
		!strings.Contains(current.Status.Message, "deployed identity validation failed") {
		t.Fatalf("status = %s/%s %q, want a fail-closed deployed-identity hold",
			current.Status.Lifecycle, current.Status.AdmissionState, current.Status.Message)
	}
	preserved := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
		t.Fatalf("SandboxClaim must survive deployed-identity failure during suspension: %v", err)
	}
	if !preserved.DeletionTimestamp.IsZero() {
		t.Fatal("SandboxClaim must not be deleting after deployed-identity failure during suspension")
	}
}

// A probe-validated replacement instance during a requested suspension is
// refused, but the prior admitted identity and the SandboxClaim are retained:
// clearing the identity would route the next reconcile into ordinary
// unadmitted scale-down and delete the claim plus its durable PVC.
func TestWorkspaceRuntimePoolReplacedInstanceRetainsClaim(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	_, claim, pod, _ := sandboxSuspendTestReachServing(t, r, pool, supervisor)

	sandboxSuspendTestSetIntent(t, r, pool, true)
	// The probe now reports a DIFFERENT runtime instance than the admitted one.
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "workspace-replacement", false)
	for range 3 {
		runtimePoolReconcile(t, r, pool)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.ActiveInstance == nil {
		t.Fatalf("replacement detection must retain the admitted identity (lifecycle=%s msg=%q)",
			current.Status.Lifecycle, current.Status.Message)
	}
	currentClaim := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), currentClaim); err != nil {
		t.Fatalf("SandboxClaim must survive replacement detection during suspension: %v", err)
	}
	if currentClaim.DeletionTimestamp != nil {
		t.Fatal("SandboxClaim must not be deleting after replacement detection during suspension")
	}
}

// A claim that drifts from the controller-owned binding while suspension
// intent is pending - before the consent record exists - must be preserved:
// deleting it would cascade the durable PVC before the suspension state
// machine ever authenticates, drains, or records the checkpoint.
func TestWorkspaceRuntimePoolPreservesDriftedClaimWhileSuspendIntentPending(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	_, claim, _, _ := sandboxSuspendTestReachServing(t, r, pool, supervisor)
	before := runtimePoolTestGetPool(t, r, pool)
	if before.Status.ActiveInstance == nil {
		t.Fatal("test requires an admitted runtime instance")
	}
	admittedInstanceID := before.Status.ActiveInstance.RuntimeInstanceID

	// Suspension intent lands, then a provider webhook drifts the claim
	// BEFORE any consent record exists.
	sandboxSuspendTestSetIntent(t, r, pool, true)
	currentClaim := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), currentClaim); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	originalWarmPoolRef := currentClaim.Spec.WarmPoolRef
	currentClaim.Spec.WarmPoolRef = sandboxextv1beta1.SandboxWarmPoolRef{Name: "webhook-injected-pool"}
	if err := r.Update(context.Background(), currentClaim); err != nil {
		t.Fatalf("drift claim: %v", err)
	}

	for range 3 {
		runtimePoolReconcile(t, r, pool)
	}
	preserved := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
		t.Fatalf("SandboxClaim must survive drift while suspension intent is pending: %v", err)
	}
	if preserved.DeletionTimestamp != nil {
		t.Fatal("SandboxClaim must not be deleting while suspension intent is pending")
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.ActiveInstance == nil || current.Status.ActiveInstance.RuntimeInstanceID != admittedInstanceID {
		t.Fatalf("ActiveInstance = %+v after claim-content drift; the suspend fence must survive", current.Status.ActiveInstance)
	}

	preserved.Spec.WarmPoolRef = originalWarmPoolRef
	if err := r.Update(context.Background(), preserved); err != nil {
		t.Fatalf("repair claim contents: %v", err)
	}
	runtimePoolReconcile(t, r, pool)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
		t.Fatalf("SandboxClaim must survive after content repair: %v", err)
	}
	if !preserved.DeletionTimestamp.IsZero() {
		t.Fatal("SandboxClaim must not be deleting after content repair")
	}
}

// Ownership metadata can drift before the suspend state machine records its
// checkpoint. Preserve the admitted identity so repairing the metadata cannot
// send the next reconcile through unadmitted scale-down.
func TestWorkspaceRuntimePoolPreservesFenceAcrossClaimOwnershipDrift(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	_, claim, _, _ := sandboxSuspendTestReachServing(t, r, pool, supervisor)
	before := runtimePoolTestGetPool(t, r, pool)
	if before.Status.ActiveInstance == nil {
		t.Fatal("test requires an admitted runtime instance")
	}
	admittedInstanceID := before.Status.ActiveInstance.RuntimeInstanceID

	sandboxSuspendTestSetIntent(t, r, pool, true)
	currentClaim := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), currentClaim); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	ownerUID := currentClaim.Labels[runtimePoolUIDLabel]
	delete(currentClaim.Labels, runtimePoolUIDLabel)
	if err := r.Update(context.Background(), currentClaim); err != nil {
		t.Fatalf("drift claim ownership: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.ActiveInstance == nil || current.Status.ActiveInstance.RuntimeInstanceID != admittedInstanceID {
		t.Fatalf("ActiveInstance = %+v after claim ownership drift; the suspend fence must survive", current.Status.ActiveInstance)
	}
	preserved := &sandboxextv1beta1.SandboxClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
		t.Fatalf("SandboxClaim must survive ownership drift during suspension: %v", err)
	}
	if !preserved.DeletionTimestamp.IsZero() {
		t.Fatal("SandboxClaim must not be deleting after ownership drift during suspension")
	}

	preserved.Labels[runtimePoolUIDLabel] = ownerUID
	if err := r.Update(context.Background(), preserved); err != nil {
		t.Fatalf("repair claim ownership: %v", err)
	}
	runtimePoolReconcile(t, r, pool)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), preserved); err != nil {
		t.Fatalf("SandboxClaim must survive after ownership repair: %v", err)
	}
	if !preserved.DeletionTimestamp.IsZero() {
		t.Fatal("SandboxClaim must not be deleting after ownership repair")
	}
}

// A durable PVC that enters deletion after admission but before the
// checkpoint must fail the suspension closed terminally: recording consent
// for the doomed claim would briefly report Suspended over vanishing data.
func TestWorkspaceRuntimePoolFailsSuspensionOnTerminatingPVC(t *testing.T) {
	scheme := runtimePoolWorkspaceTestScheme(t)
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, scheme, supervisor, pool)
	_, _, pod, durablePVC := sandboxSuspendTestReachServing(t, r, pool, supervisor)

	// The PVC enters deletion (held by protection) after admission.
	currentPVC := &corev1.PersistentVolumeClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(durablePVC), currentPVC); err != nil {
		t.Fatalf("read durable PVC: %v", err)
	}
	currentPVC.Finalizers = append(currentPVC.Finalizers, "kubernetes.io/pvc-protection")
	if err := r.Update(context.Background(), currentPVC); err != nil {
		t.Fatalf("hold PVC: %v", err)
	}
	if err := r.Delete(context.Background(), currentPVC); err != nil {
		t.Fatalf("delete durable PVC: %v", err)
	}

	sandboxSuspendTestSetIntent(t, r, pool, true)
	runtimePoolReconcile(t, r, pool)
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "workspace-boot", true)
	for range 4 {
		runtimePoolReconcile(t, r, pool)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if sandboxConsensualSuspendRecord(&current) != nil {
		t.Fatal("no consent record may be written for a terminating PVC")
	}
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatalf("a terminating PVC before the checkpoint must record terminal loss (msg=%q)", current.Status.Message)
	}
}
