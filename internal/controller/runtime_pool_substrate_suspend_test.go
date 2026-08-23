/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
)

func substrateSuspendTestPoolIntent(t *testing.T, r *RuntimePoolReconciler, pool *corev1alpha1.RuntimePool, suspend bool) {
	t.Helper()
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	if suspend {
		current.Annotations[substrateWorkspaceSuspendAnnotation] = booleanTrueValue
		current.Spec.DesiredReplicas = 0
	} else {
		delete(current.Annotations, substrateWorkspaceSuspendAnnotation)
		current.Spec.DesiredReplicas = 1
	}
	current.Generation++
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("update pool suspend intent: %v", err)
	}
}

func newSubstrateSuspendTestReconciler(t *testing.T) (*RuntimePoolReconciler, *corev1alpha1.RuntimePool, *fakeRuntimePoolSupervisorClient, *fakeSubstrateActorControl) {
	t.Helper()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	current := runtimePoolTestGetPool(t, r, pool)
	current.Spec.ExecutionWorkspace.Substrate.SuspendMode = string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly)
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("enable pool suspend mode: %v", err)
	}
	return r, pool, supervisor, control
}

func TestRenderSubstrateRuntimeTemplateDataOnlySuspendPolicy(t *testing.T) {
	r, pool, _, _ := newSubstrateSuspendTestReconciler(t)
	runtimePoolReconcile(t, r, pool)
	template := substrateTestDerivedTemplate(t, r, pool)
	if template == nil {
		t.Fatal("derived template was not materialized")
	}
	if err := verifySubstrateDeployedDataSnapshotPolicy(template); err != nil {
		t.Fatalf("rendered snapshot policy: %v", err)
	}
	volumes, _, _ := unstructured.NestedSlice(template.Object, "spec", "volumes")
	foundVolume := false
	for _, raw := range volumes {
		volume, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(volume, "name")
		if name != substrateDurableWorkspaceVolume {
			continue
		}
		foundVolume = true
		if _, hasDurable, _ := unstructured.NestedMap(volume, "durableDir"); !hasDurable {
			t.Fatalf("durable workspace volume is not a DurableDir: %v", volume)
		}
	}
	if !foundVolume {
		t.Fatalf("derived template is missing the controller-owned durable workspace volume: %v", volumes)
	}
	containers, _, _ := unstructured.NestedSlice(template.Object, "spec", "containers")
	if len(containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(containers))
	}
	container := containers[0].(map[string]any)
	mounts, _, _ := unstructured.NestedSlice(container, "volumeMounts")
	if len(mounts) != 1 {
		t.Fatalf("volume mounts = %v, want exactly the durable workspace mount", mounts)
	}
	mount := mounts[0].(map[string]any)
	if name, _, _ := unstructured.NestedString(mount, "name"); name != substrateDurableWorkspaceVolume {
		t.Fatalf("mount name = %q", name)
	}
	if path, _, _ := unstructured.NestedString(mount, "mountPath"); path != substrateDurableWorkspaceMountPath {
		t.Fatalf("mount path = %q", path)
	}
	env, _, _ := unstructured.NestedSlice(container, "env")
	foundEnv := false
	for _, raw := range env {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, _, _ := unstructured.NestedString(item, "name"); name == "ORKA_ACP_DURABLE_WORKSPACE_DIR" {
			foundEnv = true
			if value, _, _ := unstructured.NestedString(item, "value"); value != substrateDurableWorkspaceMountPath {
				t.Fatalf("durable workspace env value = %q", value)
			}
		}
	}
	if !foundEnv {
		t.Fatal("rendered container is missing the durable workspace environment variable")
	}
}

func TestRenderSubstrateRuntimeTemplateWithoutSuspendKeepsBasePolicy(t *testing.T) {
	supervisor := &fakeRuntimePoolSupervisorClient{}
	control := newFakeSubstrateActorControl()
	r, pool := runtimePoolSubstrateTestReconciler(t, supervisor, control)
	runtimePoolReconcile(t, r, pool)
	template := substrateTestDerivedTemplate(t, r, pool)
	if template == nil {
		t.Fatal("derived template was not materialized")
	}
	if err := verifySubstrateDeployedDataSnapshotPolicy(template); err == nil {
		t.Fatal("a non-suspendable pool must keep the base template's snapshot policy")
	}
	containers, _, _ := unstructured.NestedSlice(template.Object, "spec", "containers")
	container := containers[0].(map[string]any)
	if mounts, found, _ := unstructured.NestedSlice(container, "volumeMounts"); found && len(mounts) != 0 {
		t.Fatalf("non-suspendable container mounts = %v, want none", mounts)
	}
}

func TestRenderSubstrateRuntimeTemplateRejectsReservedVolumeCollision(t *testing.T) {
	r, pool, _, _ := newSubstrateSuspendTestReconciler(t)
	base := substrateTestBaseTemplate()
	current := base.DeepCopy()
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(base), current); err != nil {
		t.Fatalf("read base template: %v", err)
	}
	spec := current.Object[substrateObjectSpecField].(map[string]any)
	spec["volumes"] = []any{map[string]any{substrateTestObjectNameField: substrateDurableWorkspaceVolume, "durableDir": map[string]any{}}}
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatalf("update base template: %v", err)
	}
	runtimePoolReconcile(t, r, pool)
	status := runtimePoolTestGetPool(t, r, pool).Status
	if !strings.Contains(status.Message, "reserved volume") {
		t.Fatalf("status message = %q, want the reserved-volume rejection", status.Message)
	}
	if substrateTestDerivedTemplate(t, r, pool) != nil {
		t.Fatal("derived template must not materialize over a reserved volume collision")
	}
}

//nolint:gocyclo // The suspension and cold-resume lifecycle is one auditable end-to-end scenario.
func TestSubstrateRuntimePoolSuspendsAndColdResumesDataOnlyWorkspace(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)

	substrateSuspendTestPoolIntent(t, r, pool, true)
	runtimePoolReconcile(t, r, pool)
	if supervisor.drainCalls != 1 {
		t.Fatalf("drain calls = %d, want 1", supervisor.drainCalls)
	}
	if len(control.dataSuspended) != 0 {
		t.Fatal("actor suspended before authenticated drain quiescence")
	}

	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", true)
	runtimePoolReconcile(t, r, pool)
	if got := runtimePoolTestGetPool(t, r, pool).Status.Lifecycle; got != corev1alpha1.RuntimePoolLifecycleQuiescent {
		t.Fatalf("lifecycle after quiescent probe = %s, want Quiescent", got)
	}
	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if len(control.dataSuspended) != 1 || control.dataSuspended[0] != actorID {
		t.Fatalf("data suspensions = %v, want exactly the drained actor", control.dataSuspended)
	}
	if len(control.settled) != 0 || len(control.deleted) != 0 {
		t.Fatalf("settled=%v deleted=%v, want no teardown during consensual suspension", control.settled, control.deleted)
	}
	if current.Annotations[substrateActorSuspendedAnnotation] != actorID {
		t.Fatalf("consent annotation = %q, want %q", current.Annotations[substrateActorSuspendedAnnotation], actorID)
	}
	if current.Annotations[substrateActorBootedAnnotation] != "" {
		t.Fatal("boot record must be discarded before the data checkpoint")
	}

	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped {
		t.Fatalf("suspended lifecycle = %s, want Stopped", current.Status.Lifecycle)
	}
	if !strings.Contains(current.Status.Message, "data-only workspace checkpoint") {
		t.Fatalf("suspended message = %q", current.Status.Message)
	}
	if current.Annotations[substrateActorWorkerPodFenceAnnotation] != "" {
		t.Fatal("worker Pod fence must clear once the checkpoint settles")
	}
	if len(control.deleted) != 0 {
		t.Fatal("suspension must never delete the actor")
	}

	// Cold resume: intent lifts, bootstrap material rotates, and the actor
	// resumes from its data snapshot with a fresh process boot.
	substrateSuspendTestPoolIntent(t, r, pool, false)
	for range 8 {
		runtimePoolReconcile(t, r, pool)
	}
	resumedFromData := false
	for index, resumed := range control.resumed {
		if resumed == actorID && !control.boots[index] {
			resumedFromData = true
		}
	}
	if !resumedFromData {
		t.Fatalf("resumes = %v boots = %v, want a fromData cold resume", control.resumed, control.boots)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorSuspendedAnnotation] != "" {
		t.Fatal("the consensual checkpoint record must retire after one resume")
	}
	if current.Annotations[substrateActorBootedAnnotation] != actorID {
		t.Fatalf("boot record = %q, want the resumed actor", current.Annotations[substrateActorBootedAnnotation])
	}
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("deleted=%v settled=%v, want the same actor preserved across resume", control.deleted, control.settled)
	}
}

// substrateSuspendTestReachStopped drives a fresh suspend-capable pool through
// boot, drain, quiescence, and the consensual data checkpoint until the pool
// reports Stopped.
func substrateSuspendTestReachStopped(
	t *testing.T,
	r *RuntimePoolReconciler,
	pool *corev1alpha1.RuntimePool,
	supervisor *fakeRuntimePoolSupervisorClient,
) {
	t.Helper()
	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)
	substrateSuspendTestPoolIntent(t, r, pool, true)
	runtimePoolReconcile(t, r, pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", true)
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	if got := runtimePoolTestGetPool(t, r, pool).Status.Lifecycle; got != corev1alpha1.RuntimePoolLifecycleStopped {
		t.Fatalf("lifecycle = %s, want Stopped after consensual suspension", got)
	}
}

func TestSubstrateRuntimePoolRecoversInterruptedSuspensionBookkeeping(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachStopped(t, r, pool, supervisor)

	// Simulate the restart race: consent was persisted but the separate boot
	// and credential-seeded discards were lost before the controller died.
	current := runtimePoolTestGetPool(t, r, pool)
	current.Annotations[substrateActorBootedAnnotation] = actorID
	current.Annotations[substrateActorCredentialSeededAnnotation] = actorID
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("restore stale boot record: %v", err)
	}

	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorBootedAnnotation] != "" ||
		current.Annotations[substrateActorCredentialSeededAnnotation] != "" {
		t.Fatalf("stale boot records must be discarded idempotently, got booted=%q seeded=%q",
			current.Annotations[substrateActorBootedAnnotation], current.Annotations[substrateActorCredentialSeededAnnotation])
	}
	if current.Annotations[substrateActorRecyclingAnnotation] != "" {
		t.Fatal("a consensually suspended actor with a stale boot record must never be classified as foreign and recycled")
	}
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("deleted=%v settled=%v, want the recorded checkpoint preserved across recovery", control.deleted, control.settled)
	}
	if current.Annotations[substrateActorSuspendedAnnotation] != actorID {
		t.Fatalf("consent annotation = %q, want it preserved", current.Annotations[substrateActorSuspendedAnnotation])
	}

	// The recovered pool still cold-resumes from the preserved checkpoint.
	substrateSuspendTestPoolIntent(t, r, pool, false)
	for range 8 {
		runtimePoolReconcile(t, r, pool)
	}
	resumedFromData := false
	for index, resumed := range control.resumed {
		if resumed == actorID && !control.boots[index] {
			resumedFromData = true
		}
	}
	if !resumedFromData {
		t.Fatalf("resumes = %v boots = %v, want a fromData cold resume after recovery", control.resumed, control.boots)
	}
}

func TestSubstrateRuntimePoolRecyclesSuspendedActorOnNonBootstrapTemplateChange(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachStopped(t, r, pool, supervisor)

	// An operator changes the base infrastructure template while the actor is
	// suspended: the data checkpoint no longer matches the infrastructure
	// contract, so resume must recycle instead of updating in place.
	base := &unstructured.Unstructured{}
	base.SetGroupVersionKind(substrateActorTemplateGVK)
	if err := r.Get(context.Background(),
		client.ObjectKey{Namespace: substrateTestTemplateNamespace, Name: substrateTestBaseTemplateName}, base); err != nil {
		t.Fatalf("read base template: %v", err)
	}
	if err := unstructured.SetNestedField(base.Object, "https://example.invalid/runsc-v2",
		"spec", "runsc", "amd64", "url"); err != nil {
		t.Fatalf("mutate base template: %v", err)
	}
	if err := r.Update(context.Background(), base); err != nil {
		t.Fatalf("update base template: %v", err)
	}

	substrateSuspendTestPoolIntent(t, r, pool, false)
	for range 8 {
		runtimePoolReconcile(t, r, pool)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	recycled := current.Annotations[substrateActorRecyclingAnnotation] != "" ||
		len(control.settled) != 0 || len(control.deleted) != 0
	if !recycled {
		t.Fatalf("a non-bootstrap template change must recycle the suspended actor, status=%q", current.Status.Message)
	}
	for index, resumed := range control.resumed {
		if resumed == actorID && !control.boots[index] {
			t.Fatalf("resume %d used the stale data checkpoint despite a non-bootstrap template change", index)
		}
	}
}

func TestSubstrateRuntimePoolProviderInitiatedSuspensionStillRecyclesDataOnlyPool(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)
	if runtimePoolTestGetPool(t, r, pool).Annotations[substrateActorBootedAnnotation] != actorID {
		t.Fatal("fixture did not reach the booted state")
	}

	// The provider suspends the booted actor without a recorded request:
	// even on a data-only pool that stays fail-closed and recycles.
	control.actors[actorID].Status = substrateTestStatusSuspending
	control.actors[actorID].PodIP = ""
	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorRecyclingAnnotation] == "" &&
		current.Annotations[substrateActorBootedAnnotation] == actorID {
		t.Fatal("provider-initiated suspension must recycle the exact booted actor")
	}
	if current.Annotations[substrateActorSuspendedAnnotation] != "" {
		t.Fatal("a provider-initiated suspension must never be recorded as consensual")
	}
}

func TestSubstrateRuntimePoolSnapshotAloneDoesNotRecycleDataOnlyPool(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)

	// A snapshot record on a running actor is expected under the data-only
	// policy (a prior requested checkpoint leaves history); it is not, by
	// itself, evidence of a provider-initiated suspension.
	control.actors[actorID].SnapshotObserved = true
	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorRecyclingAnnotation] != "" {
		t.Fatal("a snapshot record alone must not recycle a data-only pool's running actor")
	}
	if current.Annotations[substrateActorBootedAnnotation] != actorID {
		t.Fatalf("boot record = %q, want the running actor retained", current.Annotations[substrateActorBootedAnnotation])
	}
}
