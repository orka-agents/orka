/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workspace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	substrateTestStatusRunning  = "STATUS_RUNNING"
	substrateTestStatusResuming = "STATUS_RESUMING"
	substrateTestChangedValue   = "changed"
)

func substrateSuspendTestPoolIntent(t *testing.T, r *RuntimePoolReconciler, pool *corev1alpha1.RuntimePool, suspend bool) {
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
	// The data-only policy overrides only its own keys: the operator's base
	// snapshot storage location must survive or the provider cannot persist
	// the checkpoint.
	if location, _, _ := unstructured.NestedString(template.Object, "spec", "snapshotsConfig", "location"); location != substrateTestSnapshotLocation {
		t.Fatalf("snapshot location = %q, want the operator's base location preserved", location)
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

func TestSubstrateSuspendCapablePoolFailsClosedWhenProviderPrunesPolicy(t *testing.T) {
	r, pool, _, control := newSubstrateSuspendTestReconciler(t)
	r.Client = &substratePolicyPruningClient{Client: r.Client}

	runtimePoolReconcile(t, r, pool)
	if len(control.created) != 0 {
		t.Fatalf("suspend-capable pool booted %d actor(s) under a policy-pruning provider", len(control.created))
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing {
		t.Fatalf("lifecycle = %s, want a closed non-serving state", current.Status.Lifecycle)
	}
	if !strings.Contains(current.Status.Message, "cannot express DataOnly suspension") {
		t.Fatalf("status message does not name the provider capability gap: %q", current.Status.Message)
	}
}

func TestSubstrateSuspendCapablePoolFailsClosedWithoutAtomicSnapshotResume(t *testing.T) {
	r, pool, _, control := newSubstrateSuspendTestReconciler(t)
	control.dataResumeFencingSupported = false

	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	if len(control.created) != 0 {
		t.Fatalf("suspend-capable pool booted %d actor(s) without atomic snapshot resume fencing", len(control.created))
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing {
		t.Fatalf("lifecycle = %s, want a closed non-serving state", current.Status.Lifecycle)
	}
	if !strings.Contains(current.Status.Message, "atomically bind ResumeActor") {
		t.Fatalf("status message does not name the protocol capability gap: %q", current.Status.Message)
	}
}

func TestSubstrateSuspendCapablePoolFailsClosedWithoutAtomicDataCheckpoint(t *testing.T) {
	r, pool, _, control := newSubstrateSuspendTestReconciler(t)
	control.dataCheckpointFencingSupported = false

	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	if len(control.created) != 0 {
		t.Fatalf("suspend-capable pool booted %d actor(s) without atomic data-checkpoint fencing", len(control.created))
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing {
		t.Fatalf("lifecycle = %s, want a closed non-serving state", current.Status.Lifecycle)
	}
	if !strings.Contains(current.Status.Message, "atomically bind SuspendActor") {
		t.Fatalf("status message does not name the checkpoint capability gap: %q", current.Status.Message)
	}
}

func TestSubstrateExistingUnbootedActorRechecksAtomicDataCheckpointBeforeBoot(t *testing.T) {
	r, pool, _, control := newSubstrateSuspendTestReconciler(t)
	control.resumeErr = errors.New("injected initial boot failure")

	runtimePoolReconcile(t, r, pool)
	if len(control.created) != 1 {
		t.Fatalf("created actors = %v, want one actor before the retry", control.created)
	}
	actorID := substrateTestActorID(pool)
	if actor := control.actors[actorID]; actor == nil || !actor.Suspended() {
		t.Fatalf("actor after failed boot = %+v, want the exact unbooted actor preserved", actor)
	}
	attempts := len(control.resumed)
	control.resumeErr = nil
	control.dataCheckpointFencingSupported = false

	runtimePoolReconcile(t, r, pool)
	if len(control.resumed) != attempts {
		t.Fatalf("provider resume calls = %v, want no retry after checkpoint fencing disappeared", control.resumed)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if !strings.Contains(current.Status.Message, "atomically bind SuspendActor") {
		t.Fatalf("status message does not name the checkpoint capability gap: %q", current.Status.Message)
	}
}

func TestSubstrateDurableStateProtectionIncludesCheckpointOperationMetadata(t *testing.T) {
	for _, annotation := range []string{
		substrateActorSuspendOperationAnnotation,
		substrateActorSuspendOperationIdentityDigestAnnotation,
	} {
		t.Run(annotation, func(t *testing.T) {
			pool := runtimePoolSubstrateTestObject()
			pool.Annotations = map[string]string{annotation: "partial-checkpoint-state"}
			if !substrateWorkspaceDurableStateProtectionPresent(pool) {
				t.Fatalf("checkpoint metadata %q did not preserve durable-state protection", annotation)
			}
		})
	}
}

func TestSubstrateExistingActorFailsClosedWithoutAtomicSnapshotResume(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)
	if actor := control.actors[actorID]; actor == nil || !actor.Running() {
		t.Fatalf("fixture did not boot the pre-upgrade actor: %+v", actor)
	}

	control.dataResumeFencingSupported = false
	substrateSuspendTestPoolIntent(t, r, pool, true)
	runtimePoolReconcile(t, r, pool)

	if len(control.dataSuspended) != 0 {
		t.Fatalf("provider checkpoint mutations = %v, want none without atomic resume fencing", control.dataSuspended)
	}
	if len(control.settled) != 0 || len(control.deleted) != 0 {
		t.Fatalf("settled=%v deleted=%v, want the running actor preserved", control.settled, control.deleted)
	}
	if actor := control.actors[actorID]; actor == nil || !actor.Running() {
		t.Fatalf("the capability refusal did not preserve the running actor: %+v", actor)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if !strings.Contains(current.Status.Message, "atomically bind ResumeActor") {
		t.Fatalf("status message does not name the protocol capability gap: %q", current.Status.Message)
	}
}

func TestSubstrateExistingActorFailsClosedWithoutAtomicDataCheckpoint(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)
	if actor := control.actors[actorID]; actor == nil || !actor.Running() {
		t.Fatalf("fixture did not boot the pre-upgrade actor: %+v", actor)
	}

	control.dataCheckpointFencingSupported = false
	substrateSuspendTestPoolIntent(t, r, pool, true)
	runtimePoolReconcile(t, r, pool)

	if len(control.dataSuspended) != 0 {
		t.Fatalf("provider checkpoint mutations = %v, want none without atomic checkpoint fencing", control.dataSuspended)
	}
	if len(control.settled) != 0 || len(control.deleted) != 0 {
		t.Fatalf("settled=%v deleted=%v, want the running actor preserved", control.settled, control.deleted)
	}
	if actor := control.actors[actorID]; actor == nil || !actor.Running() {
		t.Fatalf("the capability refusal did not preserve the running actor: %+v", actor)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if !strings.Contains(current.Status.Message, "atomically bind SuspendActor") {
		t.Fatalf("status message does not name the checkpoint capability gap: %q", current.Status.Message)
	}
}

type substratePolicyPruningClient struct {
	client.Client
}

type substrateResumeAcceptancePatchFailureClient struct {
	client.Client
	actorID string
	failed  bool
}

func (c *substrateResumeAcceptancePatchFailureClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	pool, ok := obj.(*corev1alpha1.RuntimePool)
	if ok && !c.failed && strings.TrimSpace(pool.Annotations[substrateActorResumingAnnotation]) == c.actorID {
		c.failed = true
		return errors.New("injected resume acceptance annotation failure")
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *substratePolicyPruningClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	pruneSubstrateSnapshotPolicy(obj)
	return c.Client.Create(ctx, obj, opts...)
}

func (c *substratePolicyPruningClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	pruneSubstrateSnapshotPolicy(obj)
	return c.Client.Update(ctx, obj, opts...)
}

func pruneSubstrateSnapshotPolicy(obj client.Object) {
	template, ok := obj.(*unstructured.Unstructured)
	if !ok || template.GroupVersionKind().Kind != substrateActorTemplateGVK.Kind {
		return
	}
	snapshots, found, _ := unstructured.NestedMap(template.Object, "spec", "snapshotsConfig")
	if !found {
		return
	}
	delete(snapshots, "onPause")
	delete(snapshots, "onCommit")
	delete(snapshots, "onResume")
	_ = unstructured.SetNestedMap(template.Object, snapshots, "spec", "snapshotsConfig")
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
		t.Fatalf("suspend intent annotation = %q, want %q", current.Annotations[substrateActorSuspendedAnnotation], actorID)
	}
	if current.Annotations[substrateActorSuspendAcceptedAnnotation] != substrateActorSuspendConsentValue(actorID) {
		t.Fatalf("suspend acceptance annotation = %q, want versioned consent for %q", current.Annotations[substrateActorSuspendAcceptedAnnotation], actorID)
	}
	if current.Annotations[substrateActorSuspendCallAcceptedAnnotation] != "" {
		t.Fatalf("suspend call acceptance annotation = %q, want the settled phase cleared", current.Annotations[substrateActorSuspendCallAcceptedAnnotation])
	}
	if current.Annotations[substrateActorBootedAnnotation] != "" {
		t.Fatal("boot record must be discarded after provider acceptance")
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
	if len(control.dataResumeFences) != 1 {
		t.Fatalf("atomic data resume fences = %d, want exactly one", len(control.dataResumeFences))
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorSuspendedAnnotation] != "" ||
		current.Annotations[substrateActorSuspendAcceptedAnnotation] != "" {
		t.Fatal("the consensual checkpoint record must retire after one resume")
	}
	if current.Annotations[substrateActorBootedAnnotation] != actorID {
		t.Fatalf("boot record = %q, want the resumed actor", current.Annotations[substrateActorBootedAnnotation])
	}
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("deleted=%v settled=%v, want the same actor preserved across resume", control.deleted, control.settled)
	}
}

func TestSubstrateRuntimePoolWaitsForNewSettledSnapshotGeneration(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachQuiescent(t, r, pool, supervisor)

	actor := control.actors[actorID]
	actor.SnapshotObserved = true
	actor.DataSnapshot = &workspace.SubstrateDataSnapshotFence{
		ActorID:            actorID,
		ActorUID:           actor.ActorUID,
		ActorVersion:       actor.ActorVersion,
		SnapshotAtespace:   substrateTestSnapshotAtespace,
		SnapshotName:       "prior-snapshot",
		SnapshotUID:        "prior-snapshot-uid",
		SnapshotVersion:    1,
		SourceActorUID:     actor.ActorUID,
		SourceActorVersion: actor.ActorVersion - 1,
		ContentScope:       workspace.SubstrateSnapshotContentScopeData,
	}
	_, priorDigest, err := actor.VerifiedDataSnapshotFence(actorID)
	if err != nil {
		t.Fatalf("verify prior snapshot: %v", err)
	}
	control.onDataSuspend = func(actor *workspace.SubstrateRuntimeActor, _ int) *workspace.SubstrateRuntimeActor {
		actor.Status = substrateTestStatusSuspending
		actor.PodNamespace = ""
		actor.PodName = ""
		actor.PodIP = ""
		view := *actor
		snapshot := *actor.DataSnapshot
		view.DataSnapshot = &snapshot
		return &view
	}

	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorSuspendCallAcceptedAnnotation] != actorID {
		t.Fatalf("suspend call acceptance = %q, want %q", current.Annotations[substrateActorSuspendCallAcceptedAnnotation], actorID)
	}
	if current.Annotations[substrateActorSuspendAcceptedAnnotation] != "" ||
		current.Annotations[substrateActorSnapshotDigestAnnotation] != "" {
		t.Fatal("an in-progress response carrying the prior snapshot was recorded as suspension consent")
	}
	if current.Annotations[substrateActorLastSnapshotDigestAnnotation] != priorDigest {
		t.Fatalf("prior snapshot digest = %q, want %q", current.Annotations[substrateActorLastSnapshotDigestAnnotation], priorDigest)
	}
	if current.Annotations[substrateActorBootedAnnotation] != actorID {
		t.Fatal("boot identity cleared before a new settled snapshot was proven")
	}
	attempts := len(control.dataSuspended)
	runtimePoolReconcile(t, r, pool)
	if len(control.dataSuspended) != attempts {
		t.Fatal("accepted asynchronous suspension replayed the provider call while settling")
	}

	actor = control.actors[actorID]
	actor.Status = substrateTestStatusSuspended
	actor.ActorVersion++
	actor.DataSnapshot = &workspace.SubstrateDataSnapshotFence{
		ActorID:            actorID,
		ActorUID:           actor.ActorUID,
		ActorVersion:       actor.ActorVersion,
		SnapshotAtespace:   substrateTestSnapshotAtespace,
		SnapshotName:       "completed-snapshot",
		SnapshotUID:        "completed-snapshot-uid",
		SnapshotVersion:    1,
		SourceActorUID:     actor.ActorUID,
		SourceActorVersion: actor.ActorVersion - 1,
		ContentScope:       workspace.SubstrateSnapshotContentScopeData,
	}
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	completedDigest := current.Annotations[substrateActorSnapshotDigestAnnotation]
	if completedDigest == "" || completedDigest == priorDigest {
		t.Fatalf("completed snapshot digest = %q, want a new immutable generation", completedDigest)
	}
	if current.Annotations[substrateActorSuspendAcceptedAnnotation] != substrateActorSuspendConsentValue(actorID) {
		t.Fatalf("settled suspension consent = %q, want versioned consent for %q", current.Annotations[substrateActorSuspendAcceptedAnnotation], actorID)
	}
	if current.Annotations[substrateActorSuspendCallAcceptedAnnotation] != "" {
		t.Fatal("call-acceptance phase remained after settled consent")
	}
	if current.Annotations[substrateActorBootedAnnotation] != "" {
		t.Fatal("boot identity survived settled snapshot consent")
	}
	if len(control.dataSuspended) != attempts {
		t.Fatal("settlement observation replayed the provider suspension")
	}
}

func TestSubstrateRuntimePoolRejectsActorVersionOnlySnapshotChange(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachQuiescent(t, r, pool, supervisor)

	actor := control.actors[actorID]
	actor.SnapshotObserved = true
	actor.DataSnapshot = &workspace.SubstrateDataSnapshotFence{
		ActorID: actorID, ActorUID: actor.ActorUID, ActorVersion: actor.ActorVersion,
		SnapshotAtespace: substrateTestSnapshotAtespace, SnapshotName: "prior-snapshot",
		SnapshotUID: "prior-snapshot-uid", SnapshotVersion: 1,
		SourceActorUID: actor.ActorUID, SourceActorVersion: actor.ActorVersion - 1,
		ContentScope: workspace.SubstrateSnapshotContentScopeData,
	}
	control.onDataSuspend = func(actor *workspace.SubstrateRuntimeActor, _ int) *workspace.SubstrateRuntimeActor {
		actor.Status = substrateTestStatusSuspending
		actor.PodIP = ""
		view := *actor
		snapshot := *actor.DataSnapshot
		view.DataSnapshot = &snapshot
		return &view
	}

	runtimePoolReconcile(t, r, pool)
	actor.Status = substrateTestStatusSuspended
	actor.ActorVersion++
	actor.DataSnapshot.ActorVersion = actor.ActorVersion
	runtimePoolReconcile(t, r, pool)

	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorSuspendAcceptedAnnotation] != "" {
		t.Fatal("an ActorVersion-only transition was accepted as a new snapshot")
	}
	if !strings.Contains(current.Status.Message, "pre-call version") {
		t.Fatalf("status message = %q, want pre-call Actor version refusal", current.Status.Message)
	}
}

func TestSubstrateRuntimePoolRejectsReplacementActorLifetimeDuringAcceptedCheckpoint(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID, sourceVersion := substrateSuspendTestStartAcceptedCheckpoint(t, r, pool, supervisor, control)

	replacement := *control.actors[actorID]
	replacement.ActorUID = "replacement-" + replacement.ActorUID
	replacement.ActorVersion = sourceVersion + 1
	replacement.Status = substrateTestStatusSuspended
	replacement.SnapshotObserved = true
	replacement.DataSnapshot = &workspace.SubstrateDataSnapshotFence{
		ActorID: actorID, ActorUID: replacement.ActorUID, ActorVersion: replacement.ActorVersion,
		SnapshotAtespace: substrateTestSnapshotAtespace, SnapshotName: "replacement-snapshot",
		SnapshotUID: "replacement-snapshot-uid", SnapshotVersion: 1,
		SourceActorUID: replacement.ActorUID, SourceActorVersion: sourceVersion,
		ContentScope: workspace.SubstrateSnapshotContentScopeData,
	}
	control.actors[actorID] = &replacement

	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorSuspendAcceptedAnnotation] != "" {
		t.Fatal("a replacement Actor lifetime was accepted as the original checkpoint source")
	}
	if !strings.Contains(current.Status.Message, "exact actor lifetime") {
		t.Fatalf("status message = %q, want exact Actor lifetime refusal", current.Status.Message)
	}
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("replacement lifetime triggered destructive recovery: deleted=%v settled=%v", control.deleted, control.settled)
	}
}

func TestSubstrateRuntimePoolSettlesAcceptedSuspensionAfterReadyRequest(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachQuiescent(t, r, pool, supervisor)

	actor := control.actors[actorID]
	sourceVersion := actor.ActorVersion
	control.onDataSuspend = func(actor *workspace.SubstrateRuntimeActor, _ int) *workspace.SubstrateRuntimeActor {
		actor.Status = substrateTestStatusSuspending
		actor.PodIP = ""
		view := *actor
		return &view
	}
	runtimePoolReconcile(t, r, pool)
	substrateSuspendTestPoolIntent(t, r, pool, false)
	runtimePoolReconcile(t, r, pool)
	if len(control.dataSuspended) != 1 {
		t.Fatalf("accepted suspension calls = %d, want one", len(control.dataSuspended))
	}

	actor.Status = substrateTestStatusSuspended
	actor.ActorVersion++
	actor.DataSnapshot = &workspace.SubstrateDataSnapshotFence{
		ActorID: actorID, ActorUID: actor.ActorUID, ActorVersion: actor.ActorVersion,
		SnapshotAtespace: substrateTestSnapshotAtespace, SnapshotName: "cancelled-snapshot",
		SnapshotUID: "cancelled-snapshot-uid", SnapshotVersion: 1,
		SourceActorUID: actor.ActorUID, SourceActorVersion: sourceVersion,
		ContentScope: workspace.SubstrateSnapshotContentScopeData,
	}
	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorSuspendAcceptedAnnotation] != substrateActorSuspendConsentValue(actorID) {
		t.Fatalf("settled consent = %q, want versioned consent", current.Annotations[substrateActorSuspendAcceptedAnnotation])
	}
	for range 12 {
		runtimePoolReconcile(t, r, pool)
	}
	if len(control.dataResumeFences) != 1 {
		t.Fatalf("data resumes = %d, want one after asynchronous settlement", len(control.dataResumeFences))
	}
}

func TestSubstrateRuntimePoolPersistsResumeAcceptanceBeforeRunning(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachStopped(t, r, pool, supervisor)
	control.afterResume = func(actor *workspace.SubstrateRuntimeActor) {
		actor.Status = substrateTestStatusResuming
		actor.PodNamespace = ""
		actor.PodName = ""
		actor.PodIP = ""
	}

	substrateSuspendTestPoolIntent(t, r, pool, false)
	for range 10 {
		runtimePoolReconcile(t, r, pool)
		if len(control.dataResumeFences) == 1 {
			break
		}
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorResumingAnnotation] != actorID {
		t.Fatalf("resume acceptance = %q, want %q", current.Annotations[substrateActorResumingAnnotation], actorID)
	}
	if len(control.dataResumeFences) != 1 {
		t.Fatalf("resume calls = %d, want one accepted call", len(control.dataResumeFences))
	}
	operationID := current.Annotations[substrateActorResumeOperationAnnotation]
	if !validSubstrateDataResumeOperationID(operationID) {
		t.Fatalf("resume operation id = %q, want a valid persisted operation", operationID)
	}
	if control.dataResumeFences[0].OperationID != operationID {
		t.Fatalf("provider resume operation = %q, want %q", control.dataResumeFences[0].OperationID, operationID)
	}
	if !validSHA256Digest(current.Annotations[substrateActorResumeIdentityDigestAnnotation]) {
		t.Fatalf("resume identity digest = %q, want a safe Actor lifetime digest", current.Annotations[substrateActorResumeIdentityDigestAnnotation])
	}
	fence := control.dataResumeFences[0].Template
	template := substrateTestDerivedTemplate(t, r, pool)
	revision, err := substrateRuntimeTemplateIntegrity(template)
	if err != nil {
		t.Fatalf("template integrity: %v", err)
	}
	if fence.UID != string(template.GetUID()) || fence.ResourceVersion != template.GetResourceVersion() || fence.Revision != revision {
		t.Fatalf("resume template fence = %+v, want live template %s/%s revision %s", fence, template.GetUID(), template.GetResourceVersion(), revision)
	}
	for range 3 {
		runtimePoolReconcile(t, r, pool)
	}
	if len(control.dataResumeFences) != 1 {
		t.Fatalf("accepted resume replayed %d times", len(control.dataResumeFences))
	}
}

func TestSubstrateRuntimePoolRecoversResumeAcceptanceAfterAnnotationPatchFailure(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachStopped(t, r, pool, supervisor)
	control.afterResume = func(actor *workspace.SubstrateRuntimeActor) {
		actor.Status = substrateTestStatusResuming
		actor.PodNamespace = ""
		actor.PodName = ""
		actor.PodIP = ""
	}
	substrateSuspendTestPoolIntent(t, r, pool, false)

	failingClient := &substrateResumeAcceptancePatchFailureClient{Client: r.Client, actorID: actorID}
	r.Client = failingClient
	var reconcileErr error
	for range 12 {
		_, reconcileErr = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
		if reconcileErr != nil {
			break
		}
	}
	if reconcileErr == nil || !failingClient.failed {
		t.Fatalf("resume acceptance annotation patch failure was not injected: %v", reconcileErr)
	}
	if len(control.dataResumeFences) != 1 || !control.actors[actorID].Resuming() {
		t.Fatalf("provider resume state = calls %d actor %+v, want one accepted in-flight resume", len(control.dataResumeFences), control.actors[actorID])
	}
	failedState := runtimePoolTestGetPool(t, r, pool)
	operationID := failedState.Annotations[substrateActorResumeOperationAnnotation]
	if !validSubstrateDataResumeOperationID(operationID) ||
		control.actors[actorID].DataResumeOperation == nil ||
		control.actors[actorID].DataResumeOperation.OperationID != operationID {
		t.Fatalf("resume patch failure lost durable provider operation proof: operation=%q actor=%+v", operationID, control.actors[actorID])
	}

	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorResumingAnnotation] != actorID {
		t.Fatalf("recovered resume acceptance = %q, want %q", current.Annotations[substrateActorResumingAnnotation], actorID)
	}
	if !validSHA256Digest(current.Annotations[substrateActorResumeIdentityDigestAnnotation]) {
		t.Fatalf("recovered resume identity digest = %q, want valid digest", current.Annotations[substrateActorResumeIdentityDigestAnnotation])
	}
	for range 3 {
		runtimePoolReconcile(t, r, pool)
	}
	if len(control.dataResumeFences) != 1 {
		t.Fatalf("accepted resume replayed after annotation recovery: %d calls", len(control.dataResumeFences))
	}
}

func TestSubstrateRuntimePoolRejectsUnprovenProviderResume(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachStopped(t, r, pool, supervisor)

	actor := control.actors[actorID]
	actor.Status = substrateTestStatusRunning
	actor.PodNamespace = substrateTestWorkerNamespace
	actor.PodName = substrateTestWorkerPodName
	actor.PodIP = "10.99.0.5"
	substrateSuspendTestPoolIntent(t, r, pool, false)

	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatal("provider-side resume without the controller operation proof did not record terminal workspace loss")
	}
	if current.Annotations[substrateActorCredentialSeededAnnotation] != "" || current.Status.ActiveInstance != nil {
		t.Fatalf("unproven provider resume reached admission: credentials=%q active=%+v",
			current.Annotations[substrateActorCredentialSeededAnnotation], current.Status.ActiveInstance)
	}
	if len(control.dataResumeFences) != 0 {
		t.Fatalf("controller replayed the resume after observing an unproven provider transition: %d calls", len(control.dataResumeFences))
	}
}

func TestSubstrateRuntimePoolRejectsReplacementActorAfterResumeAcceptance(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachStopped(t, r, pool, supervisor)
	substrateSuspendTestPoolIntent(t, r, pool, false)
	for range 12 {
		runtimePoolReconcile(t, r, pool)
		if runtimePoolTestGetPool(t, r, pool).Annotations[substrateActorResumingAnnotation] == actorID {
			break
		}
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorResumingAnnotation] != actorID {
		t.Fatalf("resume acceptance = %q, want %q", current.Annotations[substrateActorResumingAnnotation], actorID)
	}

	original := control.actors[actorID]
	replacement := *original
	replacement.ActorUID = "replacement-actor-uid"
	replacement.ActorVersion++
	control.actors[actorID] = &replacement
	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatal("same-ID replacement Actor did not record terminal workspace loss")
	}
	if current.Annotations[substrateActorCredentialSeededAnnotation] != "" || current.Status.ActiveInstance != nil {
		t.Fatalf("replacement Actor reached admission: credentials=%q active=%+v",
			current.Annotations[substrateActorCredentialSeededAnnotation], current.Status.ActiveInstance)
	}
}

func TestSubstrateRuntimePoolRecyclesCrashAfterResumeAcceptance(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachStopped(t, r, pool, supervisor)
	control.afterResume = func(actor *workspace.SubstrateRuntimeActor) {
		actor.Status = substrateTestStatusResuming
		actor.PodIP = ""
	}

	substrateSuspendTestPoolIntent(t, r, pool, false)
	for range 10 {
		runtimePoolReconcile(t, r, pool)
		if runtimePoolTestGetPool(t, r, pool).Annotations[substrateActorResumingAnnotation] == actorID {
			break
		}
	}
	if len(control.dataResumeFences) != 1 {
		t.Fatalf("data resume calls = %d, want one accepted call", len(control.dataResumeFences))
	}
	control.afterResume = nil
	control.actors[actorID].Status = substrateTestStatusCrashed

	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatal("a crash after accepted resume did not record terminal workspace loss")
	}
	if actor := control.actors[actorID]; actor != nil &&
		current.Annotations[substrateActorRecyclingAnnotation] != actorID {
		t.Fatalf("crashed resumed actor remains without exact-instance teardown: actor=%+v annotations=%v", actor, current.Annotations)
	}
	for range 8 {
		if control.actors[actorID] == nil {
			break
		}
		runtimePoolReconcile(t, r, pool)
	}
	if control.actors[actorID] != nil {
		t.Fatalf("crashed resumed actor survived credential-safe teardown: %+v", control.actors[actorID])
	}
	if len(control.dataResumeFences) != 1 {
		t.Fatalf("accepted resume was replayed after the crash: %d calls", len(control.dataResumeFences))
	}
}

func TestSubstrateRuntimePoolPassesExactDataCheckpointFence(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachQuiescent(t, r, pool, supervisor)

	runtimePoolReconcile(t, r, pool)
	if len(control.dataCheckpointFences) != 1 {
		t.Fatalf("data checkpoint fences = %d, want one", len(control.dataCheckpointFences))
	}
	fence := control.dataCheckpointFences[0]
	actor := control.actors[actorID]
	if actor == nil || actor.DataSnapshot == nil {
		t.Fatalf("checkpointed actor = %+v, want immutable snapshot proof", actor)
	}
	if fence.ActorID != actorID || fence.ActorUID != actor.ActorUID ||
		fence.ActorVersion != actor.DataSnapshot.SourceActorVersion {
		t.Fatalf("checkpoint actor fence = %+v, want actor %s/%s version %d", fence, actorID, actor.ActorUID, actor.DataSnapshot.SourceActorVersion)
	}
	template := substrateTestDerivedTemplate(t, r, pool)
	revision, err := substrateRuntimeTemplateIntegrity(template)
	if err != nil {
		t.Fatalf("template integrity: %v", err)
	}
	if fence.Template.Namespace != template.GetNamespace() || fence.Template.Name != template.GetName() ||
		fence.Template.UID != string(template.GetUID()) ||
		fence.Template.ResourceVersion != template.GetResourceVersion() ||
		fence.Template.Revision != revision {
		t.Fatalf("checkpoint template fence = %+v, want live template %s/%s revision %s", fence.Template, template.GetUID(), template.GetResourceVersion(), revision)
	}
}

func TestSubstrateRuntimePoolAtomicCheckpointRejectsTemplateRace(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachQuiescent(t, r, pool, supervisor)
	control.beforeDataSuspend = func(_ *workspace.SubstrateRuntimeActor) {
		template := substrateTestDerivedTemplate(t, r, pool)
		annotations := template.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["test.orka.ai/checkpoint-race"] = substrateTestChangedValue
		template.SetAnnotations(annotations)
		if err := r.Update(context.Background(), template); err != nil {
			t.Fatalf("mutate template during checkpoint: %v", err)
		}
		control.beforeDataSuspend = nil
	}
	control.validateDataCheckpointFence = func(expected workspace.SubstrateDataCheckpointFence) error {
		template := substrateTestDerivedTemplate(t, r, pool)
		if expected.Template.ResourceVersion != template.GetResourceVersion() {
			return workspace.NewError(
				"suspend actor",
				workspace.ErrorKindFailedPrecondition,
				"ActorTemplate fence changed before checkpoint",
				true,
				nil,
			)
		}
		return nil
	}

	runtimePoolReconcile(t, r, pool)
	if len(control.dataCheckpointFences) != 1 {
		t.Fatalf("atomic checkpoint attempts = %d, want one rejected race", len(control.dataCheckpointFences))
	}
	actor := control.actors[actorID]
	if actor == nil || !actor.Running() || actor.DataSnapshot != nil {
		t.Fatalf("template race mutated the actor before rejection: %+v", actor)
	}
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("template race triggered destructive recovery: deleted=%v settled=%v", control.deleted, control.settled)
	}
}

func TestSubstrateSuspendConsentRejectsLegacyValue(t *testing.T) {
	pool := &corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		substrateActorSuspendedAnnotation:       runtimePoolSubstrateActorSuffix,
		substrateActorSuspendAcceptedAnnotation: runtimePoolSubstrateActorSuffix,
	}}}
	pool.Spec.ExecutionWorkspace = &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{
		Provider:  corev1alpha1.WorkspaceProviderSubstrate,
		Substrate: &corev1alpha1.RuntimePoolSubstrateWorkspaceSpec{SuspendMode: string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly)},
	}
	if substrateActorConsensuallySuspended(pool, "actor") {
		t.Fatal("legacy unversioned acceptance was treated as resumable consent")
	}
	if !substrateActorHasLegacySuspensionConsent(pool) {
		t.Fatal("legacy unversioned acceptance was not classified for quarantine")
	}
	pool.Annotations[substrateActorSuspendAcceptedAnnotation] = substrateActorSuspendConsentValue(runtimePoolSubstrateActorSuffix)
	if substrateActorConsensuallySuspended(pool, runtimePoolSubstrateActorSuffix) || runtimePoolWorkspaceSuspendConsentRecorded(pool) {
		t.Fatal("versioned acceptance without settled snapshot proof was treated as consent")
	}
	pool.Annotations[substrateActorSnapshotDigestAnnotation] = "sha256:" + strings.Repeat("a", 64)
	pool.Annotations[substrateActorLastSnapshotDigestAnnotation] = pool.Annotations[substrateActorSnapshotDigestAnnotation]
	pool.Annotations[substrateActorLastSnapshotIdentityDigestAnnotation] = "sha256:" + strings.Repeat("b", 64)
	if !substrateActorConsensuallySuspended(pool, runtimePoolSubstrateActorSuffix) {
		t.Fatal("complete versioned acceptance was not recognized")
	}
	pool.Annotations[substrateActorSuspendCallAcceptedAnnotation] = runtimePoolSubstrateActorSuffix
	if runtimePoolWorkspaceSuspendConsentRecorded(pool) {
		t.Fatal("in-progress checkpoint markers were treated as settled consent")
	}
}

func TestSubstrateRuntimePoolDeletionBypassesCheckpointQuarantine(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*corev1alpha1.RuntimePool, string)
	}{
		{
			name: "legacy consent",
			configure: func(pool *corev1alpha1.RuntimePool, actorID string) {
				pool.Annotations[substrateActorSuspendedAnnotation] = actorID
				pool.Annotations[substrateActorSuspendAcceptedAnnotation] = actorID
			},
		},
		{
			name: "accepted checkpoint call",
			configure: func(pool *corev1alpha1.RuntimePool, actorID string) {
				pool.Annotations[substrateActorSuspendedAnnotation] = actorID
				pool.Annotations[substrateActorSuspendCallAcceptedAnnotation] = actorID
			},
		},
		{
			name: "incomplete immutable proof",
			configure: func(pool *corev1alpha1.RuntimePool, actorID string) {
				pool.Annotations[substrateActorSuspendedAnnotation] = actorID
				pool.Annotations[substrateActorSuspendAcceptedAnnotation] = substrateActorSuspendConsentValue(actorID)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
			runtimePoolReconcile(t, r, pool)
			probePod := substrateTestProbePod(pool)
			supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-delete-quarantine", false)
			runtimePoolReconcile(t, r, pool)
			actorID := substrateTestActorID(pool)
			actor := control.actors[actorID]
			if actor == nil {
				t.Fatal("test requires a provider actor")
			}
			actor.Status = substrateTestStatusSuspended

			current := runtimePoolTestGetPool(t, r, pool)
			test.configure(&current, actorID)
			if err := r.Update(context.Background(), &current); err != nil {
				t.Fatalf("record checkpoint quarantine state: %v", err)
			}
			if err := r.Delete(context.Background(), &current); err != nil {
				t.Fatalf("delete RuntimePool: %v", err)
			}

			gone := false
			for range 20 {
				_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
				if err != nil {
					t.Fatalf("reconcile deletion: %v", err)
				}
				var observed corev1alpha1.RuntimePool
				err = r.Get(context.Background(), client.ObjectKeyFromObject(pool), &observed)
				if apierrors.IsNotFound(err) {
					gone = true
					break
				}
				if err != nil {
					t.Fatalf("read deleting RuntimePool: %v", err)
				}
			}
			if !gone {
				current = runtimePoolTestGetPool(t, r, pool)
				t.Fatalf("checkpoint quarantine stranded RuntimePool finalization: lifecycle=%s message=%q",
					current.Status.Lifecycle, current.Status.Message)
			}
			if len(control.deleted) != 1 || control.deleted[0] != actorID {
				t.Fatalf("deleted actors = %v, want explicit cleanup of %q", control.deleted, actorID)
			}
		})
	}
}

func TestSubstrateRuntimePoolRefusesRetargetedSnapshotGeneration(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachStopped(t, r, pool, supervisor)

	current := runtimePoolTestGetPool(t, r, pool)
	recorded := current.Annotations[substrateActorSnapshotDigestAnnotation]
	if recorded == "" {
		t.Fatal("suspension consent did not record the provider snapshot generation")
	}
	control.actors[actorID].DataSnapshot.SnapshotUID = "retargeted-snapshot-uid"
	substrateSuspendTestPoolIntent(t, r, pool, false)
	for range 12 {
		runtimePoolReconcile(t, r, pool)
	}

	for index, resumed := range control.resumed {
		if resumed == actorID && !control.boots[index] {
			t.Fatal("the controller restored a provider snapshot whose generation no longer matched suspension consent")
		}
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if !strings.Contains(current.Status.Message, "unverified provider snapshot") {
		t.Fatalf("status message = %q, want the snapshot-generation refusal", current.Status.Message)
	}
	if current.Annotations[substrateActorSnapshotDigestAnnotation] != recorded {
		t.Fatal("the consent-bound snapshot digest changed after provider retargeting")
	}
}

func TestSubstrateRuntimePoolRefusesFullSnapshotUnderDataOnlyClass(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachStopped(t, r, pool, supervisor)
	control.actors[actorID].DataSnapshot.ContentScope = workspace.SubstrateSnapshotContentScopeFull

	substrateSuspendTestPoolIntent(t, r, pool, false)
	for range 8 {
		runtimePoolReconcile(t, r, pool)
	}
	if len(control.dataResumeFences) != 0 {
		t.Fatalf("atomic data resume calls = %d, want none for a Full snapshot", len(control.dataResumeFences))
	}
	for index, resumed := range control.resumed {
		if resumed == actorID && !control.boots[index] {
			t.Fatal("a DataOnly class restored a Full provider snapshot")
		}
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if !strings.Contains(current.Status.Message, "content scope is not Data") {
		t.Fatalf("status message = %q, want the observed Full-scope refusal", current.Status.Message)
	}
	if _, exists := control.actors[actorID]; !exists {
		t.Fatal("the refused Full snapshot was deleted instead of quarantined")
	}
}

func TestSubstrateRuntimePoolRefusesLegacyCheckpointWithoutImmutableProof(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachStopped(t, r, pool, supervisor)

	current := runtimePoolTestGetPool(t, r, pool)
	current.Annotations[substrateActorSnapshotDigestAnnotation] = ""
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("remove immutable snapshot proof: %v", err)
	}
	substrateSuspendTestPoolIntent(t, r, pool, false)
	for range 8 {
		runtimePoolReconcile(t, r, pool)
	}
	if len(control.dataResumeFences) != 0 {
		t.Fatalf("atomic data resume calls = %d, want none for a legacy checkpoint", len(control.dataResumeFences))
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if !strings.Contains(current.Status.Message, "unsafe legacy backfill") {
		t.Fatalf("status message = %q, want the legacy-proof refusal", current.Status.Message)
	}
	if _, exists := control.actors[actorID]; !exists {
		t.Fatal("the legacy checkpoint was deleted instead of left quarantined")
	}
}

func TestSubstrateRuntimePoolAtomicResumeRejectsSnapshotRace(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachStopped(t, r, pool, supervisor)
	control.beforeDataResume = func(actor *workspace.SubstrateRuntimeActor) {
		actor.DataSnapshot.SnapshotUID = "raced-snapshot-uid"
		control.beforeDataResume = nil
	}

	substrateSuspendTestPoolIntent(t, r, pool, false)
	for range 8 {
		runtimePoolReconcile(t, r, pool)
	}
	if len(control.dataResumeFences) != 1 {
		t.Fatalf("atomic data resume attempts = %d, want one rejected race", len(control.dataResumeFences))
	}
	for index, resumed := range control.resumed {
		if resumed == actorID && !control.boots[index] {
			t.Fatal("snapshot mutation raced past the atomic resume fence")
		}
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if !strings.Contains(current.Status.Message, "unverified provider snapshot") {
		t.Fatalf("status message = %q, want the post-race snapshot refusal", current.Status.Message)
	}
	if _, exists := control.actors[actorID]; !exists {
		t.Fatal("the raced checkpoint was deleted instead of quarantined")
	}
}

// A resumed actor remains the only copy of its DurableDir data until the next
// consensual checkpoint. Bootstrap-only rollouts must checkpoint that data,
// update the suspended actor's template, and cold-boot it again instead of
// taking the destructive Recreate path.
func TestSubstrateRuntimePoolBootstrapOnlyRolloutRecheckpointsResumedData(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	probePod := substrateTestProbePod(pool)
	substrateSuspendTestReachStopped(t, r, pool, supervisor)

	substrateSuspendTestPoolIntent(t, r, pool, false)
	current := runtimePoolTestGetPool(t, r, pool)
	supervisor.probe = runtimePoolValidProbe(&current, &probePod, "first-resume", false)
	for range 10 {
		runtimePoolReconcile(t, r, pool)
		current = runtimePoolTestGetPool(t, r, pool)
		if current.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing {
			break
		}
	}
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || current.Status.ActiveInstance == nil {
		t.Fatalf("resumed pool status = %s active=%v, want Serving before rollout", current.Status.Lifecycle, current.Status.ActiveInstance)
	}
	if current.Annotations[substrateActorResumingAnnotation] != actorID {
		t.Fatalf("resume lineage = %q, want %q", current.Annotations[substrateActorResumingAnnotation], actorID)
	}

	checkpointsBefore := len(control.dataSuspended)
	r.ControllerEpoch++
	runtimePoolReconcile(t, r, pool)
	if supervisor.drainReason != "runtime_pool_workspace_suspend" {
		t.Fatalf("rollout drain reason = %q, want data-preserving suspension", supervisor.drainReason)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	supervisor.probe = runtimePoolValidProbe(&current, &probePod, "first-resume", true)
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	if len(control.dataSuspended) != checkpointsBefore+1 {
		t.Fatalf("data checkpoints = %v, want one checkpoint before the bootstrap-only rollout", control.dataSuspended)
	}
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("deleted=%v settled=%v, want the checkpoint-bearing actor preserved", control.deleted, control.settled)
	}

	current = runtimePoolTestGetPool(t, r, pool)
	supervisor.probe = runtimePoolValidProbe(&current, &probePod, "second-resume", false)
	supervisor.probe.Status.Fence.ControllerEpoch = uint64(r.ControllerEpoch)
	for range 12 {
		runtimePoolReconcile(t, r, pool)
		current = runtimePoolTestGetPool(t, r, pool)
		if current.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing &&
			current.Status.ActiveInstance != nil &&
			current.Status.ActiveInstance.ControllerEpoch == r.ControllerEpoch {
			break
		}
	}
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || current.Status.ActiveInstance == nil ||
		current.Status.ActiveInstance.ControllerEpoch != r.ControllerEpoch {
		t.Fatalf("post-rollout status = %s active=%+v message=%q annotations=%v resumes=%v boots=%v, want Serving on controller epoch %d",
			current.Status.Lifecycle, current.Status.ActiveInstance, current.Status.Message, current.Annotations,
			control.resumed, control.boots, r.ControllerEpoch)
	}
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] != "" {
		t.Fatalf("bootstrap-only rollout recorded terminal resume loss: %q", current.Annotations[runtimePoolWorkspaceResumeLostAnnotation])
	}
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("deleted=%v settled=%v after rollout, want the same actor cold-booted from data", control.deleted, control.settled)
	}
	if len(control.boots) == 0 || control.boots[len(control.boots)-1] {
		t.Fatalf("resume boot modes = %v, want the rollout restart to restore from data", control.boots)
	}
}

func TestSubstrateRuntimePoolPermanentBootstrapCheckpointFailureRecordsResumeLoss(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	probePod := substrateTestProbePod(pool)
	substrateSuspendTestReachStopped(t, r, pool, supervisor)

	substrateSuspendTestPoolIntent(t, r, pool, false)
	current := runtimePoolTestGetPool(t, r, pool)
	supervisor.probe = runtimePoolValidProbe(&current, &probePod, "first-resume", false)
	for range 10 {
		runtimePoolReconcile(t, r, pool)
		current = runtimePoolTestGetPool(t, r, pool)
		if current.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing {
			break
		}
	}
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing ||
		current.Annotations[substrateActorResumingAnnotation] != actorID {
		t.Fatalf("resumed pool = lifecycle %s annotations=%v, want Serving resumed actor %q",
			current.Status.Lifecycle, current.Annotations, actorID)
	}

	control.suspendErr = workspace.NewError(
		"suspend actor", workspace.ErrorKindFailedPrecondition, "checkpoint rejected", false,
		errors.New("injected permanent preservation checkpoint failure"),
	)
	r.ControllerEpoch++
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	supervisor.probe = runtimePoolValidProbe(&current, &probePod, "first-resume", true)
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)

	current = runtimePoolTestGetPool(t, r, pool)
	if current.Spec.DesiredReplicas != 1 {
		t.Fatalf("bootstrap-only preservation replicas = %d, want 1", current.Spec.DesiredReplicas)
	}
	if current.Annotations[substrateWorkspaceSuspendFailedAnnotation] != actorID {
		t.Fatalf("checkpoint failure = %q, want %q", current.Annotations[substrateWorkspaceSuspendFailedAnnotation], actorID)
	}
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
		t.Fatal("permanent preservation checkpoint failure did not record terminal resume loss")
	}
	attempts := len(control.dataSuspended)
	for range 8 {
		runtimePoolReconcile(t, r, pool)
	}
	if len(control.dataSuspended) != attempts {
		t.Fatalf("permanent preservation checkpoint failure was retried: attempts %d -> %d", attempts, len(control.dataSuspended))
	}
	if _, exists := control.actors[actorID]; exists {
		t.Fatal("actor survived fail-closed preservation checkpoint teardown")
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
	substrateSuspendTestReachQuiescent(t, r, pool, supervisor)
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	if got := runtimePoolTestGetPool(t, r, pool).Status.Lifecycle; got != corev1alpha1.RuntimePoolLifecycleStopped {
		t.Fatalf("lifecycle = %s, want Stopped after consensual suspension", got)
	}
}

func substrateSuspendTestReachQuiescent(
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
	if got := runtimePoolTestGetPool(t, r, pool).Status.Lifecycle; got != corev1alpha1.RuntimePoolLifecycleQuiescent {
		t.Fatalf("lifecycle = %s, want Quiescent before provider suspension", got)
	}
}

func substrateSuspendTestStartAcceptedCheckpoint(
	t *testing.T,
	r *RuntimePoolReconciler,
	pool *corev1alpha1.RuntimePool,
	supervisor *fakeRuntimePoolSupervisorClient,
	control *fakeSubstrateActorControl,
) (string, int64) {
	t.Helper()
	substrateSuspendTestReachQuiescent(t, r, pool, supervisor)
	actorID := substrateTestActorID(pool)
	actor := control.actors[actorID]
	if actor == nil {
		t.Fatal("test requires a running provider actor")
	}
	sourceVersion := actor.ActorVersion
	control.onDataSuspend = func(actor *workspace.SubstrateRuntimeActor, _ int) *workspace.SubstrateRuntimeActor {
		actor.Status = substrateTestStatusSuspending
		actor.PodNamespace = ""
		actor.PodName = ""
		actor.PodIP = ""
		view := *actor
		return &view
	}
	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorSuspendCallAcceptedAnnotation] != actorID {
		t.Fatalf("suspend call acceptance = %q, want %q", current.Annotations[substrateActorSuspendCallAcceptedAnnotation], actorID)
	}
	return actorID, sourceVersion
}

func substrateSuspendTestSettleAcceptedCheckpoint(
	control *fakeSubstrateActorControl,
	actorID string,
	sourceVersion int64,
	snapshotName string,
) {
	actor := control.actors[actorID]
	actor.Status = substrateTestStatusSuspended
	actor.ActorVersion++
	actor.PodNamespace = ""
	actor.PodName = ""
	actor.PodIP = ""
	actor.SnapshotObserved = true
	actor.DataSnapshot = &workspace.SubstrateDataSnapshotFence{
		ActorID: actorID, ActorUID: actor.ActorUID, ActorVersion: actor.ActorVersion,
		SnapshotAtespace: substrateTestSnapshotAtespace, SnapshotName: snapshotName,
		SnapshotUID: snapshotName + "-uid", SnapshotVersion: 1,
		SourceActorUID: actor.ActorUID, SourceActorVersion: sourceVersion,
		ContentScope: workspace.SubstrateSnapshotContentScopeData,
	}
	control.onDataSuspend = nil
}

func TestSubstrateRuntimePoolPrerequisiteFailuresPreserveSuspendFence(t *testing.T) {
	for _, stage := range []string{
		runtimePoolPrerequisiteStageNamespace,
		runtimePoolPrerequisiteStageCredentials,
	} {
		t.Run(stage, func(t *testing.T) {
			r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
			runtimePoolReconcile(t, r, pool)
			probePod := substrateTestProbePod(pool)
			supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
			runtimePoolReconcile(t, r, pool)
			before := runtimePoolTestGetPool(t, r, pool)
			if before.Status.ActiveInstance == nil {
				t.Fatal("test requires an admitted runtime instance")
			}
			admittedInstanceID := before.Status.ActiveInstance.RuntimeInstanceID
			actorID := substrateTestActorID(pool)
			substrateSuspendTestPoolIntent(t, r, pool, true)

			delegate := r.Client
			r.Client = &runtimePoolSuspendPrerequisiteFailureClient{Client: delegate, stage: stage}
			runtimePoolReconcile(t, r, pool)
			current := runtimePoolTestGetPool(t, r, pool)
			if current.Status.ActiveInstance == nil || current.Status.ActiveInstance.RuntimeInstanceID != admittedInstanceID {
				t.Fatalf("ActiveInstance = %+v after %s prerequisite failure; the Substrate suspend fence must survive", current.Status.ActiveInstance, stage)
			}

			r.Client = delegate
			runtimePoolReconcile(t, r, pool)
			if _, exists := control.actors[actorID]; !exists {
				t.Fatalf("actor %q was deleted after the %s prerequisite recovered", actorID, stage)
			}
		})
	}
}

func TestWorkspacePoolFailurePreservesSubstrateDurableStateRecords(t *testing.T) {
	for _, annotation := range []string{
		runtimePoolWorkspaceSuspendAnnotation,
		substrateActorSuspendedAnnotation,
		substrateActorSuspendCallAcceptedAnnotation,
		substrateActorSuspendAcceptedAnnotation,
		substrateActorResumeRejectedAnnotation,
		substrateActorResumingAnnotation,
		substrateActorResumeOperationAnnotation,
		substrateActorResumeIdentityDigestAnnotation,
	} {
		t.Run(annotation, func(t *testing.T) {
			r, pool, _, _ := newSubstrateSuspendTestReconciler(t)
			current := runtimePoolTestGetPool(t, r, pool)
			if current.Annotations == nil {
				current.Annotations = map[string]string{}
			}
			current.Annotations[annotation] = substrateTestActorID(pool)
			if err := r.Update(context.Background(), &current); err != nil {
				t.Fatalf("record Substrate durable-state marker: %v", err)
			}
			preserve, err := r.workspacePoolFailureRequiresDurableStatePreservation(context.Background(), &current)
			if err != nil {
				t.Fatalf("check durable-state preservation: %v", err)
			}
			if !preserve {
				t.Fatalf("annotation %q did not preserve the Substrate durable-state fence", annotation)
			}
		})
	}
}

func TestSubstrateRuntimePoolPermanentCheckpointFailureIsTerminal(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachQuiescent(t, r, pool, supervisor)
	control.suspendErr = workspace.NewError(
		"suspend actor", workspace.ErrorKindFailedPrecondition, "checkpoint rejected", false,
		errors.New("injected permanent checkpoint failure"),
	)

	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateWorkspaceSuspendFailedAnnotation] != actorID {
		t.Fatalf("suspension failure = %q, want %q", current.Annotations[substrateWorkspaceSuspendFailedAnnotation], actorID)
	}
	if current.Annotations[substrateActorSuspendedAnnotation] != "" ||
		current.Annotations[substrateActorSuspendAcceptedAnnotation] != "" {
		t.Fatal("permanently rejected checkpoint retained consensual suspension")
	}
	attempts := len(control.dataSuspended)
	for range 8 {
		runtimePoolReconcile(t, r, pool)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if len(control.dataSuspended) != attempts {
		t.Fatalf("permanent checkpoint failure was retried: attempts %d -> %d", attempts, len(control.dataSuspended))
	}
	if _, exists := control.actors[actorID]; exists {
		t.Fatal("actor survived terminal checkpoint failure teardown")
	}
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped {
		t.Fatalf("lifecycle = %s, want Stopped after terminal checkpoint failure", current.Status.Lifecycle)
	}
}

func TestSubstrateRuntimePoolTransientCheckpointFailureRetries(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachQuiescent(t, r, pool, supervisor)
	control.suspendErr = workspace.NewError(
		"suspend actor", workspace.ErrorKindTimeout, "checkpoint timed out", true,
		errors.New("injected transient checkpoint failure"),
	)

	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateWorkspaceSuspendFailedAnnotation] != "" {
		t.Fatal("transient checkpoint failure was marked terminal")
	}
	if current.Annotations[substrateActorSuspendedAnnotation] != actorID {
		t.Fatalf("suspend intent = %q, want %q retained for retry", current.Annotations[substrateActorSuspendedAnnotation], actorID)
	}
	if current.Annotations[substrateActorSuspendAcceptedAnnotation] != "" {
		t.Fatalf("transient provider error recorded acceptance = %q", current.Annotations[substrateActorSuspendAcceptedAnnotation])
	}
	if current.Annotations[substrateActorBootedAnnotation] != actorID {
		t.Fatalf("boot record = %q, want it retained until provider acceptance", current.Annotations[substrateActorBootedAnnotation])
	}
	operationID := current.Annotations[substrateActorSuspendOperationAnnotation]
	if !validSubstrateDataCheckpointOperationID(operationID) {
		t.Fatalf("checkpoint operation id = %q, want a valid persisted operation", operationID)
	}
	attempts := len(control.dataSuspended)
	control.suspendErr = nil
	probePod := substrateTestProbePod(pool)
	probesBefore := supervisor.probeCalls
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)
	if supervisor.probeCalls <= probesBefore {
		t.Fatalf("probe calls stayed at %d; checkpoint retry bypassed authenticated observation", probesBefore)
	}
	if len(control.dataSuspended) != attempts {
		t.Fatalf("checkpoint retried before a fresh drain barrier: attempts %d -> %d", attempts, len(control.dataSuspended))
	}

	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", true)
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleQuiescent {
		t.Fatalf("lifecycle = %s message=%q, want a fresh persisted Quiescent barrier", current.Status.Lifecycle, current.Status.Message)
	}
	if len(control.dataSuspended) != attempts {
		t.Fatalf("checkpoint crossed the fresh quiescence observation: attempts %d -> %d", attempts, len(control.dataSuspended))
	}

	runtimePoolReconcile(t, r, pool)
	if len(control.dataSuspended) != attempts+1 {
		t.Fatalf("checkpoint attempts = %d, want one retry after fresh quiescence", len(control.dataSuspended))
	}
	if got := control.dataCheckpointFences[len(control.dataCheckpointFences)-1].OperationID; got != operationID {
		t.Fatalf("retried checkpoint operation id = %q, want persisted %q", got, operationID)
	}
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("deleted=%v settled=%v, want no teardown for a transient checkpoint failure", control.deleted, control.settled)
	}
}

func TestSubstrateRuntimePoolRecoversAcceptedCheckpointAfterResponseLoss(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachQuiescent(t, r, pool, supervisor)
	control.dataCheckpointResponseErr = workspace.NewError(
		"suspend actor", workspace.ErrorKindTimeout, "checkpoint response lost", true,
		errors.New("injected post-acceptance response loss"),
	)

	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	operationID := current.Annotations[substrateActorSuspendOperationAnnotation]
	if !validSubstrateDataCheckpointOperationID(operationID) {
		t.Fatalf("checkpoint operation id = %q, want a valid persisted operation", operationID)
	}
	actor := control.actors[actorID]
	if actor == nil || actor.DataCheckpointOperation == nil ||
		actor.DataCheckpointOperation.OperationID != operationID || actor.LatestDataOperationID != operationID {
		t.Fatalf("provider checkpoint proof = %+v, want durable acceptance for %q", actor, operationID)
	}
	if current.Annotations[substrateActorSuspendCallAcceptedAnnotation] != "" {
		t.Fatalf("checkpoint call acceptance = %q, want response-loss bookkeeping gap", current.Annotations[substrateActorSuspendCallAcceptedAnnotation])
	}
	attempts := len(control.dataSuspended)
	control.dataCheckpointResponseErr = nil

	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if len(control.dataSuspended) != attempts {
		t.Fatalf("accepted checkpoint replayed after response loss: attempts %d -> %d", attempts, len(control.dataSuspended))
	}
	if current.Annotations[substrateActorSuspendAcceptedAnnotation] != substrateActorSuspendConsentValue(actorID) {
		t.Fatalf("recovered suspension consent = %q, want versioned acceptance", current.Annotations[substrateActorSuspendAcceptedAnnotation])
	}
	if current.Annotations[substrateActorSuspendOperationAnnotation] != "" ||
		current.Annotations[substrateActorSuspendOperationIdentityDigestAnnotation] != "" {
		t.Fatalf("settled checkpoint retained operation bookkeeping: annotations=%v", current.Annotations)
	}
}

func TestSubstrateRuntimePoolCheckpointFailureRedactsProviderIdentifiers(t *testing.T) {
	const providerIdentifier = "provider-snapshot-namespace/name/uid"
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "retryable structured error",
			err: workspace.NewError(
				"suspend actor", workspace.ErrorKindTimeout, providerIdentifier, true,
				errors.New(providerIdentifier),
			),
		},
		{name: "unstructured error", err: errors.New(providerIdentifier)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
			substrateSuspendTestReachQuiescent(t, r, pool, supervisor)
			control.suspendErr = test.err

			runtimePoolReconcile(t, r, pool)
			message := runtimePoolTestGetPool(t, r, pool).Status.Message
			if strings.Contains(message, providerIdentifier) {
				t.Fatalf("status message exposed provider snapshot identity: %q", message)
			}
			if !strings.Contains(message, "provider data-only checkpoint is temporarily unavailable") {
				t.Fatalf("status message = %q, want fixed safe checkpoint failure", message)
			}
		})
	}
}

func TestSubstrateRuntimePoolAcceptedCheckpointReadFailureRedactsProviderIdentifiers(t *testing.T) {
	const providerIdentifier = "provider-snapshot-atespace/name/uid"
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID, _ := substrateSuspendTestStartAcceptedCheckpoint(t, r, pool, supervisor, control)
	control.getErr = errors.New(providerIdentifier)

	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if strings.Contains(current.Status.Message, providerIdentifier) {
		t.Fatalf("status message exposed provider snapshot identity: %q", current.Status.Message)
	}
	if !strings.Contains(current.Status.Message, "provider accepted checkpoint state is temporarily unavailable") {
		t.Fatalf("status message = %q, want fixed safe checkpoint read failure", current.Status.Message)
	}
	if current.Annotations[substrateActorSuspendCallAcceptedAnnotation] != actorID {
		t.Fatalf("accepted checkpoint marker = %q, want %q preserved for retry",
			current.Annotations[substrateActorSuspendCallAcceptedAnnotation], actorID)
	}
}

func TestSubstrateRuntimePoolPermanentCheckpointRetryFailureIsTerminal(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachQuiescent(t, r, pool, supervisor)
	control.suspendErr = workspace.NewError(
		"suspend actor", workspace.ErrorKindTimeout, "checkpoint timed out", true,
		errors.New("injected transient checkpoint failure"),
	)
	runtimePoolReconcile(t, r, pool)
	if current := runtimePoolTestGetPool(t, r, pool); current.Annotations[substrateActorSuspendedAnnotation] != actorID {
		t.Fatalf("suspend intent = %q, want persisted retry state", current.Annotations[substrateActorSuspendedAnnotation])
	} else if current.Annotations[substrateActorSuspendAcceptedAnnotation] != "" {
		t.Fatalf("transient provider error recorded acceptance = %q", current.Annotations[substrateActorSuspendAcceptedAnnotation])
	}

	control.suspendErr = workspace.NewError(
		"suspend actor", workspace.ErrorKindInvalidArgument, "checkpoint policy rejected", false,
		errors.New("injected permanent retry failure"),
	)
	for range 4 {
		runtimePoolReconcile(t, r, pool)
		if runtimePoolTestGetPool(t, r, pool).Annotations[substrateWorkspaceSuspendFailedAnnotation] == actorID {
			break
		}
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateWorkspaceSuspendFailedAnnotation] != actorID {
		t.Fatalf("suspension failure = %q, want %q", current.Annotations[substrateWorkspaceSuspendFailedAnnotation], actorID)
	}
	attempts := len(control.dataSuspended)
	for range 3 {
		runtimePoolReconcile(t, r, pool)
	}
	if len(control.dataSuspended) != attempts {
		t.Fatalf("permanent retry failure replayed the checkpoint call: attempts %d -> %d", attempts, len(control.dataSuspended))
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
		t.Fatalf("suspend intent annotation = %q, want it preserved", current.Annotations[substrateActorSuspendedAnnotation])
	}
	if current.Annotations[substrateActorSuspendAcceptedAnnotation] != substrateActorSuspendConsentValue(actorID) {
		t.Fatalf("suspend acceptance annotation = %q, want it preserved", current.Annotations[substrateActorSuspendAcceptedAnnotation])
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

func TestSubstrateRuntimePoolHonorsLegacySuspendAnnotation(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)

	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)

	// A pool suspended under the pre-rename key upgrades mid-suspension: the
	// intent must still route into the consensual checkpoint path, never the
	// ordinary teardown that destroys the durable data.
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[runtimePoolLegacySubstrateSuspendAnnotation] = booleanTrueValue
	current.Spec.DesiredReplicas = 0
	current.Generation++
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("record legacy suspend intent: %v", err)
	}
	runtimePoolReconcile(t, r, pool)
	if supervisor.drainCalls != 1 {
		t.Fatalf("drain calls = %d, want the legacy intent to enter the suspend drain", supervisor.drainCalls)
	}
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", true)
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if len(control.dataSuspended) != 1 || control.dataSuspended[0] != actorID {
		t.Fatalf("data suspensions = %v, want the consensual checkpoint under the legacy key", control.dataSuspended)
	}
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("deleted=%v settled=%v, want no teardown for a legacy-keyed suspension", control.deleted, control.settled)
	}
}

// A workspace suspend intent that has not reached the pool annotation yet
// (crash window, or the idle reaper scaling to zero first) must hold ordinary
// teardown instead of deleting the actor and its data.
func TestSubstrateRuntimePoolScaleDownWaitsForPendingWorkspaceSuspendIntent(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	runtimePoolReconcile(t, r, pool)
	probePod := substrateTestProbePod(pool)
	supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
	runtimePoolReconcile(t, r, pool)

	// Link a workspace whose DesiredState is Suspended, scale the pool to
	// zero WITHOUT the suspend annotation (the reaper's ordinary path).
	current := runtimePoolTestGetPool(t, r, pool)
	workspaceName := current.Labels[acpExecutionWorkspaceLinkLabel]
	if workspaceName == "" {
		workspaceName = "acp-ws-pending-suspend"
		if current.Labels == nil {
			current.Labels = map[string]string{}
		}
		current.Labels[acpExecutionWorkspaceLinkLabel] = workspaceName
	}
	// The hold is honored only for the pool's exact workspace incarnation.
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[acpExecutionWorkspaceUIDAnnotation] = "pending-suspend-uid"
	current.Spec.DesiredReplicas = 0
	current.Generation++
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("scale pool to zero: %v", err)
	}
	linked := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: pool.Namespace, Name: workspaceName, UID: "pending-suspend-uid"},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
			Mode:         workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredSuspended,
		},
	}
	if err := r.Create(context.Background(), linked); err != nil {
		t.Fatalf("create linked workspace: %v", err)
	}

	for range 3 {
		runtimePoolReconcile(t, r, pool)
	}
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("deleted=%v settled=%v, want teardown held while the suspension intent is pending", control.deleted, control.settled)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if len(control.dataSuspended) == 0 && current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDraining {
		t.Fatalf("lifecycle = %s, want Draining while waiting for the durable suspend intent", current.Status.Lifecycle)
	}
	_ = actorID
}

func TestLinkedWorkspaceSuspendIntentPendingUsesAPIReader(t *testing.T) {
	r, pool, _, _ := newSubstrateSuspendTestReconciler(t)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Labels == nil {
		current.Labels = map[string]string{}
	}
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Labels[acpExecutionWorkspaceLinkLabel] = "acp-ws-uncached-suspend"
	current.Annotations[acpExecutionWorkspaceUIDAnnotation] = "uncached-suspend-uid"
	linked := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: current.Namespace,
			Name:      current.Labels[acpExecutionWorkspaceLinkLabel],
			UID:       "uncached-suspend-uid",
		},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
			Mode:         workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredSuspended,
		},
	}
	authoritative := r.Client
	if err := authoritative.Create(context.Background(), linked); err != nil {
		t.Fatalf("create linked workspace: %v", err)
	}
	stale := linked.DeepCopy()
	stale.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
	r.APIReader = authoritative
	r.Client = &staleExecutionWorkspaceGetClient{Client: authoritative, workspace: stale}

	pending, err := r.linkedWorkspaceSuspendIntentPending(context.Background(), &current)
	if err != nil {
		t.Fatalf("read uncached suspend intent: %v", err)
	}
	if !pending {
		t.Fatal("fresh suspended intent did not hold ordinary actor teardown")
	}
	r.APIReader = nil
	pending, err = r.linkedWorkspaceSuspendIntentPending(context.Background(), &current)
	if err != nil {
		t.Fatalf("read stale cached suspend intent: %v", err)
	}
	if pending {
		t.Fatal("stale Ready workspace unexpectedly reported pending suspension")
	}
}

func TestSubstrateRuntimePoolColdResumeErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retryable bool
		kind      workspace.ErrorKind
	}{
		{name: "fence rejection", retryable: false, kind: workspace.ErrorKindFailedPrecondition},
		{name: "invalid resume request", retryable: false, kind: workspace.ErrorKindInvalidArgument},
		{name: "transient provider failure", retryable: true, kind: workspace.ErrorKindTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
			substrateSuspendTestReachStopped(t, r, pool, supervisor)
			substrateSuspendTestPoolIntent(t, r, pool, false)
			priorResumeAttempts := len(control.resumed)
			providerDetail := "private-snapshot-name private-snapshot-uid private-atespace"
			control.resumeErr = workspace.NewError(
				"resume actor",
				tc.kind,
				providerDetail,
				tc.retryable,
				errors.New("injected provider resume failure: "+providerDetail),
			)

			for range 8 {
				runtimePoolReconcile(t, r, pool)
				if len(control.resumed) > priorResumeAttempts {
					break
				}
			}
			if len(control.resumed) == priorResumeAttempts {
				t.Fatal("cold resume was never attempted")
			}
			current := runtimePoolTestGetPool(t, r, pool)
			if lost := strings.TrimSpace(current.Annotations[runtimePoolWorkspaceResumeLostAnnotation]); lost != "" {
				t.Fatalf("resume rejection declared the preserved checkpoint lost: %q", lost)
			}
			if strings.Contains(current.Status.Message, providerDetail) {
				t.Fatalf("status leaked provider snapshot identifiers: %q", current.Status.Message)
			}
			if current.Annotations[substrateActorSuspendAcceptedAnnotation] != substrateActorSuspendConsentValue(substrateTestActorID(pool)) ||
				current.Annotations[substrateActorSnapshotDigestAnnotation] == "" {
				t.Fatal("resume failure discarded the consensual checkpoint fence")
			}
			if _, exists := control.actors[substrateTestActorID(pool)]; !exists {
				t.Fatal("resume failure deleted the checkpoint-bearing actor")
			}
			if rejected := current.Annotations[substrateActorResumeRejectedAnnotation]; tc.retryable && rejected != "" {
				t.Fatalf("retryable resume failure was quarantined as terminal: %q", rejected)
			} else if !tc.retryable && rejected != substrateTestActorID(pool) {
				t.Fatalf("non-retryable resume rejection = %q, want actor quarantine", rejected)
			}
			attempts := len(control.resumed)
			for range 3 {
				runtimePoolReconcile(t, r, pool)
			}
			if tc.retryable && len(control.resumed) <= attempts {
				t.Fatal("transient cold-resume failure was not retried")
			}
			if !tc.retryable && len(control.resumed) != attempts {
				t.Fatal("quarantined cold-resume rejection was retried")
			}
		})
	}
}

// A consensual suspension whose actor vanished must clear the stale consent
// so the workspace adapter fails the suspension closed instead of reporting a
// resumable checkpoint that no longer exists.
func TestSubstrateRuntimePoolClearsStaleConsentWhenActorVanishes(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachStopped(t, r, pool, supervisor)

	// The provider loses the suspended actor entirely.
	delete(control.actors, actorID)
	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorSuspendedAnnotation] != "" ||
		current.Annotations[substrateActorSuspendAcceptedAnnotation] != "" {
		t.Fatal("stale consent must be cleared once the suspended actor is proven gone")
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
	if current.Annotations[substrateActorSuspendedAnnotation] != "" ||
		current.Annotations[substrateActorSuspendAcceptedAnnotation] != "" {
		t.Fatal("a provider-initiated suspension must never be recorded as consensual")
	}
}

func TestSubstrateRuntimePoolIntentOnlySuspensionRecyclesAmbiguousProviderTransition(t *testing.T) {
	for _, status := range []string{substrateTestStatusSuspending, substrateTestStatusSuspended} {
		t.Run(status, func(t *testing.T) {
			r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
			actorID := substrateTestActorID(pool)
			substrateSuspendTestReachQuiescent(t, r, pool, supervisor)

			// Simulate a controller stop after persisting intent but before it
			// durably records a successful provider response. The provider then
			// reports a transition that could be either the lost request or an
			// independent suspension. The ambiguity must recycle fail-closed.
			current := runtimePoolTestGetPool(t, r, pool)
			current.Annotations[substrateActorSuspendedAnnotation] = actorID
			if err := r.Update(context.Background(), &current); err != nil {
				t.Fatalf("record suspend intent: %v", err)
			}
			control.actors[actorID].Status = status
			control.actors[actorID].PodIP = ""

			runtimePoolReconcile(t, r, pool)
			current = runtimePoolTestGetPool(t, r, pool)
			if current.Annotations[substrateActorSuspendAcceptedAnnotation] != "" {
				t.Fatalf("ambiguous transition recorded provider acceptance: %v", current.Annotations)
			}
			if len(control.dataSuspended) != 0 {
				t.Fatalf("ambiguous transition replayed suspension: %v", control.dataSuspended)
			}
			_, actorExists := control.actors[actorID]
			if actorExists && current.Annotations[substrateActorRecyclingAnnotation] == "" {
				t.Fatalf("ambiguous provider transition was accepted instead of recycled: status=%s annotations=%v", current.Status.Lifecycle, current.Annotations)
			}
		})
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

// A suspension re-requested while the prior checkpoint's cold resume is still
// booting must keep the restored actor: it already consumed the only consent
// record, so it holds the sole copy of the checkpoint data. Ordinary
// scale-down would recycle it and stamp the resume lost; instead the actor is
// carried through admission and the preserved data is re-suspended.
func TestSubstrateRuntimePoolResuspendDuringInFlightResumeKeepsActor(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachStopped(t, r, pool, supervisor)

	// Cold resume starts: the consent record is consumed and the restored
	// actor boots, but its authenticated admission has not completed.
	substrateSuspendTestPoolIntent(t, r, pool, false)
	supervisor.probeErr = errors.New("supervisor still booting")
	for range 6 {
		runtimePoolReconcile(t, r, pool)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorResumingAnnotation] != actorID {
		t.Fatalf("resuming annotation = %q, want %q", current.Annotations[substrateActorResumingAnnotation], actorID)
	}
	if current.Status.ActiveInstance != nil {
		t.Fatal("admission must not complete while the boot probe fails")
	}

	// The continuation is cancelled: settlement re-requests suspension while
	// the resume is still in flight.
	substrateSuspendTestPoolIntent(t, r, pool, true)
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("deleted=%v settled=%v, want the restored actor preserved through the re-requested suspension",
			control.deleted, control.settled)
	}
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] != "" {
		t.Fatalf("resume-lost = %q; an in-flight resume must never be declared lost by a re-requested suspension",
			current.Annotations[runtimePoolWorkspaceResumeLostAnnotation])
	}

	// A transitional Running-without-route provider response must hold the
	// same way instead of entering ordinary scale-down.
	control.actors[actorID].Status = substrateTestStatusRunning
	control.actors[actorID].PodIP = ""
	for range 2 {
		runtimePoolReconcile(t, r, pool)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("deleted=%v settled=%v; a routeless resumed actor must be preserved", control.deleted, control.settled)
	}
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] != "" {
		t.Fatalf("resume-lost = %q for a routeless resumed actor, want empty", current.Annotations[runtimePoolWorkspaceResumeLostAnnotation])
	}
	control.actors[actorID].PodIP = "10.0.0.99"

	// The suspension holds at the authenticated admission gate instead of
	// scaling down: every retry re-probes the booting supervisor so the
	// quiescent checkpoint path can re-suspend the preserved data once the
	// boot completes.
	probesBefore := supervisor.probeCalls
	for range 3 {
		runtimePoolReconcile(t, r, pool)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if supervisor.probeCalls <= probesBefore {
		t.Fatalf("probe calls stayed at %d; the re-requested suspension must keep driving the restored actor through admission", probesBefore)
	}
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("deleted=%v settled=%v while admission retries, want none", control.deleted, control.settled)
	}
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] != "" {
		t.Fatalf("resume-lost = %q while admission retries, want empty", current.Annotations[runtimePoolWorkspaceResumeLostAnnotation])
	}
}

func TestSubstrateRuntimePoolReestablishesQuiescenceBeforeResuspendingResumedActor(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	probePod := substrateTestProbePod(pool)
	deployedProbe := func(draining bool) RuntimePoolProbeResult {
		t.Helper()
		current := runtimePoolTestGetPool(t, r, pool)
		cfg, err := r.runtimePoolConfig(&current)
		if err != nil {
			t.Fatalf("runtime pool config: %v", err)
		}
		validationPool, _, _, err := r.substrateRuntimePoolDeployedValidationTarget(
			context.Background(), &current, cfg, substrateTestDerivedTemplate(t, r, pool),
		)
		if err != nil {
			t.Fatalf("deployed validation target: %v", err)
		}
		return runtimePoolValidProbe(validationPool, &probePod, "resumed-boot", draining)
	}
	substrateSuspendTestReachStopped(t, r, pool, supervisor)

	// Stop immediately after the atomic resume acceptance. The restored actor
	// is Running, but boot-time work has not crossed authenticated admission.
	substrateSuspendTestPoolIntent(t, r, pool, false)
	for range 12 {
		runtimePoolReconcile(t, r, pool)
		if runtimePoolTestGetPool(t, r, pool).Annotations[substrateActorResumingAnnotation] == actorID {
			break
		}
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorResumingAnnotation] != actorID || !control.actors[actorID].Running() {
		t.Fatalf("resume state = annotations=%v actor=%+v, want accepted Running resume", current.Annotations, control.actors[actorID])
	}
	checkpointsBefore := len(control.dataSuspended)

	// A cancellation re-requests suspension. The first observation must issue
	// an authenticated drain and must not checkpoint the still-active actor.
	substrateSuspendTestPoolIntent(t, r, pool, true)
	supervisor.probe = deployedProbe(false)
	runtimePoolReconcile(t, r, pool)
	if len(control.dataSuspended) != checkpointsBefore {
		t.Fatalf("checkpoint started before authenticated drain: %d -> %d", checkpointsBefore, len(control.dataSuspended))
	}
	if supervisor.drainReason != "runtime_pool_workspace_suspend" {
		t.Fatalf("drain reason = %q, want workspace suspension", supervisor.drainReason)
	}

	// Even a quiescent probe must persist the Quiescent lifecycle across one
	// reconcile boundary before the provider checkpoint mutation.
	supervisor.probe = deployedProbe(true)
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleQuiescent {
		t.Fatalf("lifecycle = %s message=%q, want persisted Quiescent barrier", current.Status.Lifecycle, current.Status.Message)
	}
	if len(control.dataSuspended) != checkpointsBefore {
		t.Fatalf("checkpoint crossed the first quiescent observation: %d -> %d", checkpointsBefore, len(control.dataSuspended))
	}

	runtimePoolReconcile(t, r, pool)
	if len(control.dataSuspended) != checkpointsBefore+1 {
		t.Fatalf("checkpoint count = %d, want one after persisted quiescence", len(control.dataSuspended))
	}
}

// The pending-suspend hold applies only to the pool's exact workspace
// incarnation: a workspace deleted and recreated under the same deterministic
// name (UID mismatch) or a terminally Failed suspension must release the
// hold so the stale pool can settle instead of draining forever.
func TestSubstrateRuntimePoolSuspendIntentHoldRequiresExactIncarnation(t *testing.T) {
	for name, mutate := range map[string]func(*workspacev1alpha1.ExecutionWorkspace){
		"replacement workspace UID": func(workspace *workspacev1alpha1.ExecutionWorkspace) {
			workspace.UID = "replacement-uid"
		},
		"terminally failed suspension": func(workspace *workspacev1alpha1.ExecutionWorkspace) {
			workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, pool, supervisor, _ := newSubstrateSuspendTestReconciler(t)
			runtimePoolReconcile(t, r, pool)
			probePod := substrateTestProbePod(pool)
			supervisor.probe = runtimePoolValidProbe(pool, &probePod, "actor-boot", false)
			runtimePoolReconcile(t, r, pool)

			current := runtimePoolTestGetPool(t, r, pool)
			if current.Labels == nil {
				current.Labels = map[string]string{}
			}
			current.Labels[acpExecutionWorkspaceLinkLabel] = "acp-ws-stale-hold"
			if current.Annotations == nil {
				current.Annotations = map[string]string{}
			}
			current.Annotations[acpExecutionWorkspaceUIDAnnotation] = "original-uid"
			if err := r.Update(context.Background(), &current); err != nil {
				t.Fatalf("link pool: %v", err)
			}
			linked := &workspacev1alpha1.ExecutionWorkspace{
				ObjectMeta: metav1.ObjectMeta{Namespace: pool.Namespace, Name: "acp-ws-stale-hold", UID: "original-uid"},
				Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
					Mode:         workspacev1alpha1.ExecutionWorkspaceModeInteractive,
					DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredSuspended,
				},
			}
			mutate(linked)
			if err := r.Create(context.Background(), linked); err != nil {
				t.Fatalf("create linked workspace: %v", err)
			}
			pending, err := r.linkedWorkspaceSuspendIntentPending(context.Background(), &current)
			if err != nil {
				t.Fatalf("intent predicate: %v", err)
			}
			if pending {
				t.Fatal("a foreign incarnation or failed suspension must not hold the pool")
			}
		})
	}
}

// A cold resume the provider reports as STATUS_RESUMING is a checkpoint being
// consumed, not a crash: a suspension re-requested in that window (a
// cancelled continuation) must hold with the consent intact instead of
// clearing it and scale-down destroying the sole DurableDir checkpoint.
func TestSubstrateRuntimePoolHoldsSuspensionWhileActorResuming(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachStopped(t, r, pool, supervisor)

	// The provider accepts the controller-issued cold resume operation, but the
	// workload is not fully Running. Both transitional shapes must hold:
	// STATUS_RESUMING, and STATUS_RUNNING whose route is not yet populated.
	control.afterResume = func(actor *workspace.SubstrateRuntimeActor) {
		actor.Status = substrateTestStatusResuming
		actor.PodNamespace = ""
		actor.PodName = ""
		actor.PodIP = ""
	}
	substrateSuspendTestPoolIntent(t, r, pool, false)
	for range 12 {
		runtimePoolReconcile(t, r, pool)
		if runtimePoolTestGetPool(t, r, pool).Annotations[substrateActorResumingAnnotation] == actorID {
			break
		}
	}
	if current := runtimePoolTestGetPool(t, r, pool); current.Annotations[substrateActorResumingAnnotation] != actorID {
		t.Fatalf("accepted resume operation was not recorded: annotations=%v", current.Annotations)
	}
	substrateSuspendTestPoolIntent(t, r, pool, true)
	for range 3 {
		runtimePoolReconcile(t, r, pool)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorSuspendedAnnotation] != actorID {
		t.Fatalf("consent = %q; a resuming actor must never have its consent cleared", current.Annotations[substrateActorSuspendedAnnotation])
	}
	if current.Annotations[substrateActorSuspendAcceptedAnnotation] != substrateActorSuspendConsentValue(actorID) {
		t.Fatalf("accepted consent = %q; a resuming actor must retain it", current.Annotations[substrateActorSuspendAcceptedAnnotation])
	}
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("deleted=%v settled=%v; a resuming actor must be preserved", control.deleted, control.settled)
	}
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] != "" {
		t.Fatalf("resume-lost = %q while the provider resume is in flight, want empty", current.Annotations[runtimePoolWorkspaceResumeLostAnnotation])
	}

	// Liveness without route readiness is equally transitional.
	control.actors[actorID].Status = substrateTestStatusRunning
	control.actors[actorID].PodIP = ""
	for range 3 {
		runtimePoolReconcile(t, r, pool)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorSuspendedAnnotation] != actorID {
		t.Fatalf("consent = %q; a Running actor without a route must never have its consent cleared", current.Annotations[substrateActorSuspendedAnnotation])
	}
	if current.Annotations[substrateActorSuspendAcceptedAnnotation] != substrateActorSuspendConsentValue(actorID) {
		t.Fatalf("accepted consent = %q; a Running actor without a route must retain it", current.Annotations[substrateActorSuspendAcceptedAnnotation])
	}
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("deleted=%v settled=%v; a Running-without-route actor must be preserved", control.deleted, control.settled)
	}
}

// substrateSuspendTestDeleteBoundAuthSecret removes the Secret named by the
// pool's current private-auth binding and returns that annotation key.
func substrateSuspendTestDeleteBoundAuthSecret(
	t *testing.T,
	r *RuntimePoolReconciler,
	pool *corev1alpha1.RuntimePool,
) string {
	t.Helper()
	current := runtimePoolTestGetPool(t, r, pool)
	bindingKey := ""
	boundSecret := ""
	for key, value := range current.Annotations {
		if strings.HasPrefix(key, runtimePoolPrivateAuthBindingPrefix) && strings.TrimSpace(value) != "" {
			bindingKey = key
			boundSecret = strings.TrimSpace(value)
		}
	}
	if boundSecret == "" {
		t.Fatalf("no private auth binding recorded, annotations=%v", current.Annotations)
	}
	// The binding value pins "name/uid"; the object name is the first half.
	boundSecret, _, _ = strings.Cut(boundSecret, "/")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: pool.Spec.RuntimeNamespace, Name: boundSecret,
	}}
	if secret.Namespace == "" {
		secret.Namespace = pool.Namespace
	}
	if err := r.Delete(context.Background(), secret); err != nil {
		t.Fatalf("delete bound auth secret: %v", err)
	}
	return bindingKey
}

// A deleted control-plane auth Secret must never destroy a consensually
// suspended checkpoint: no credential-bearing process exists in the
// suspended actor and cold resume rotates bootstrap material anyway, so the
// recovery clears the stale binding and rotates credentials while the actor
// and its durable data stay preserved.
func TestSubstrateMissingAuthSecretPreservesSuspendedCheckpoint(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)

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
	if control.actors[actorID] == nil || control.actors[actorID].Status != substrateTestStatusSuspended {
		t.Fatalf("fixture did not reach the suspended state: %+v", control.actors[actorID])
	}

	// The bound auth Secret disappears out of band while suspended.
	substrateSuspendTestDeleteBoundAuthSecret(t, r, pool)

	for range 3 {
		runtimePoolReconcile(t, r, pool)
	}
	if control.actors[actorID] == nil {
		t.Fatal("the consensually suspended actor must survive auth-Secret loss")
	}
	if control.actors[actorID].Status != substrateTestStatusSuspended {
		t.Fatalf("suspended actor status = %q; credential rotation must not touch the checkpoint", control.actors[actorID].Status)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorSuspendedAnnotation] != actorID {
		t.Fatalf("consent record = %q; the checkpoint consent must survive credential rotation", current.Annotations[substrateActorSuspendedAnnotation])
	}
	if current.Annotations[substrateActorSuspendAcceptedAnnotation] != substrateActorSuspendConsentValue(actorID) {
		t.Fatalf("accepted consent = %q; the checkpoint must survive credential rotation", current.Annotations[substrateActorSuspendAcceptedAnnotation])
	}
	if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] != "" {
		t.Fatal("credential rotation must never record a terminal resume loss for the preserved checkpoint")
	}
}

func TestSubstrateMissingAuthSecretSettlesAcceptedCheckpoint(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID, sourceVersion := substrateSuspendTestStartAcceptedCheckpoint(t, r, pool, supervisor, control)
	substrateSuspendTestDeleteBoundAuthSecret(t, r, pool)
	substrateSuspendTestSettleAcceptedCheckpoint(control, actorID, sourceVersion, "missing-auth-snapshot")

	for range 3 {
		runtimePoolReconcile(t, r, pool)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped ||
		!runtimePoolWorkspaceSuspendConsentRecorded(&current) {
		t.Fatalf("accepted checkpoint with missing auth = lifecycle %s consent %v message %q, want settled suspension",
			current.Status.Lifecycle, runtimePoolWorkspaceSuspendConsentRecorded(&current), current.Status.Message)
	}
	if control.actors[actorID] == nil || !control.actors[actorID].Suspended() {
		t.Fatalf("accepted checkpoint actor was not preserved: %+v", control.actors[actorID])
	}
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("missing auth destroyed an accepted checkpoint: deleted=%v settled=%v", control.deleted, control.settled)
	}
}

func TestSubstrateMissingDerivedTemplatePreservesAcceptedCheckpoint(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID, sourceVersion := substrateSuspendTestStartAcceptedCheckpoint(t, r, pool, supervisor, control)
	template := substrateTestDerivedTemplate(t, r, pool)
	if err := r.Delete(context.Background(), template); err != nil {
		t.Fatalf("delete derived ActorTemplate: %v", err)
	}
	substrateSuspendTestSettleAcceptedCheckpoint(control, actorID, sourceVersion, "missing-template-snapshot")

	for range 3 {
		runtimePoolReconcile(t, r, pool)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped ||
		!runtimePoolWorkspaceSuspendConsentRecorded(&current) {
		t.Fatalf("accepted checkpoint with missing template = lifecycle %s consent %v message %q, want settled suspension",
			current.Status.Lifecycle, runtimePoolWorkspaceSuspendConsentRecorded(&current), current.Status.Message)
	}
	if control.actors[actorID] == nil || !control.actors[actorID].Suspended() {
		t.Fatalf("accepted checkpoint actor was not preserved: %+v", control.actors[actorID])
	}
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("missing template destroyed an accepted checkpoint: deleted=%v settled=%v", control.deleted, control.settled)
	}
}

func TestSubstrateMissingAuthSecretQuarantinesLegacySuspensionConsent(t *testing.T) {
	r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
	actorID := substrateTestActorID(pool)
	substrateSuspendTestReachStopped(t, r, pool, supervisor)

	current := runtimePoolTestGetPool(t, r, pool)
	current.Annotations[substrateActorSuspendAcceptedAnnotation] = actorID
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("install legacy suspension consent: %v", err)
	}
	bindingKey := substrateSuspendTestDeleteBoundAuthSecret(t, r, pool)
	runtimePoolReconcile(t, r, pool)

	current = runtimePoolTestGetPool(t, r, pool)
	if control.actors[actorID] == nil || control.actors[actorID].Status != substrateTestStatusSuspended {
		t.Fatalf("legacy checkpoint was not quarantined: %+v", control.actors[actorID])
	}
	if len(control.deleted) != 0 || len(control.settled) != 0 {
		t.Fatalf("legacy consent entered destructive credential recovery: deleted=%v settled=%v", control.deleted, control.settled)
	}
	if strings.TrimSpace(current.Annotations[bindingKey]) == "" {
		t.Fatal("legacy consent cleared the missing auth binding instead of preserving quarantine evidence")
	}
	if !strings.Contains(current.Status.Message, "legacy unversioned Substrate suspension consent") {
		t.Fatalf("status message = %q, want explicit legacy-consent quarantine", current.Status.Message)
	}
}

// STATUS_RESUMING and STATUS_RUNNING without a route are live transition
// states, not settled suspension. Missing credentials must recycle those
// actors instead of treating their process state as absent and preserving an
// unsafe checkpoint.
func TestSubstrateMissingAuthSecretRecyclesTransitionalActor(t *testing.T) {
	tests := []struct {
		name   string
		status string
	}{
		{name: "resuming", status: substrateTestStatusResuming},
		{name: "running without route", status: substrateTestStatusRunning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, pool, supervisor, control := newSubstrateSuspendTestReconciler(t)
			actorID := substrateTestActorID(pool)
			substrateSuspendTestReachStopped(t, r, pool, supervisor)
			control.actors[actorID].Status = tt.status
			control.actors[actorID].PodNamespace = substrateTestWorkerNamespace
			control.actors[actorID].PodName = substrateTestWorkerPodName
			control.actors[actorID].PodIP = ""

			bindingKey := substrateSuspendTestDeleteBoundAuthSecret(t, r, pool)
			runtimePoolReconcile(t, r, pool)

			current := runtimePoolTestGetPool(t, r, pool)
			if strings.TrimSpace(current.Annotations[bindingKey]) == "" {
				t.Fatal("credential binding cleared before the transitional actor entered exact-instance teardown")
			}
			if current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] == "" {
				t.Fatal("transitional actor was treated as a safely suspended checkpoint")
			}
		})
	}
}
