/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

// The full-memory restore gate is hard-closed: no configuration, profile,
// template, or frozen binding may reach a Full snapshot policy through any
// admission or execution boundary until ADR 0030's prerequisites hold.

// substrateTestFullScope is the provider snapshot scope this gate forbids.
const substrateTestFullScope = "Full"

func TestSubstrateFullMemoryRestoreGateIsClosed(t *testing.T) {
	t.Parallel()
	if substrateFullMemoryRestoreGateOpen() {
		t.Fatal("the full-memory restore gate must stay closed until ADR 0030's prerequisites are implemented and adversarially tested")
	}
}

func TestResolveACPWorkspaceClassRejectsFullSuspendMode(t *testing.T) {
	t.Parallel()
	fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendSubstrate, func(f *acpClassFixture) {
		f.profile.Spec.Substrate.Suspend = &acpworkspacev1alpha1.SubstrateSuspendPolicy{Mode: substrateTestFullScope}
	})
	r := acpClassTestReconciler(t, fixture.objects()...)
	_, err := r.resolveACPWorkspaceClass(context.Background(), acpClassTestTask())
	if err == nil || !strings.Contains(err.Error(), "full-memory restore stays gated") {
		t.Fatalf("error = %v, want the explicit full-memory gate rejection", err)
	}
}

func TestResolveACPWorkspaceClassPreservesDormantSubstrateSuspendBindingIdentity(t *testing.T) {
	t.Parallel()
	fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendSubstrate, func(f *acpClassFixture) {
		f.profile.Spec.Substrate.Suspend = &acpworkspacev1alpha1.SubstrateSuspendPolicy{
			Mode: acpworkspacev1alpha1.SubstrateSuspendModeDataOnly,
		}
	})
	r := acpClassTestReconciler(t, fixture.objects()...)
	resolved, err := r.resolveACPWorkspaceClass(context.Background(), acpClassTestTask())
	if err != nil {
		t.Fatalf("resolve Delete-only class: %v", err)
	}
	if resolved.SubstrateSuspendMode != "" ||
		resolved.Binding.SuspendMode != string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly) {
		t.Fatalf("Delete-only class suspension modes = resolved %q binding %q, want dormant execution and legacy DataOnly binding identity",
			resolved.SubstrateSuspendMode, resolved.Binding.SuspendMode)
	}
}

func TestFrozenBindingRejectsFullSuspendModeTamper(t *testing.T) {
	t.Parallel()
	fixture := suspendableSubstrateFixture(t)
	r := acpClassTestReconciler(t, fixture.objects()...)
	resolved, err := r.resolveACPWorkspaceClass(context.Background(), acpClassTestTask())
	if err != nil {
		t.Fatalf("resolve class: %v", err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(suspendableSessionTask(), "", false, "session-uid-1", resolved)
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	tampered := *binding
	tamperedClass := *binding.Class
	tamperedClass.EffectiveOnDetach = string(workspacev1alpha1.WorkspaceOnDetachDelete)
	tamperedClass.SuspendMode = substrateTestFullScope
	tampered.Class = &tamperedClass
	tampered.BindingDigest, err = acpWorkspaceBindingDigest(&tampered)
	if err != nil {
		t.Fatalf("recompute tampered binding digest: %v", err)
	}
	if err := validateACPWorkspaceBindingValues(&tampered); err == nil || !strings.Contains(err.Error(), "suspension mode") {
		t.Fatalf("error = %v, want the explicit Full suspension-mode rejection", err)
	}
}

func setDeployedSubstrateSnapshotPolicy(t *testing.T, r *RuntimePoolReconciler, pool *corev1alpha1.RuntimePool, scope string) {
	t.Helper()
	template := substrateTestDerivedTemplate(t, r, pool)
	if template == nil {
		t.Fatal("derived template is required")
	}
	// Mutate ONLY the pause/commit scopes inside the existing map: replacing
	// the whole snapshotsConfig would also drop location/onResume and the
	// tests would fail on unrelated shape validation instead of the policy.
	snapshots, found, err := unstructured.NestedMap(template.Object, "spec", "snapshotsConfig")
	if err != nil || !found {
		t.Fatalf("read snapshot policy: found=%v err=%v", found, err)
	}
	snapshots["onPause"] = scope
	snapshots["onCommit"] = scope
	if err := unstructured.SetNestedMap(template.Object, snapshots, "spec", "snapshotsConfig"); err != nil {
		t.Fatalf("set snapshot policy: %v", err)
	}
	// Restamp the declared revision so only the policy — not the integrity
	// check — differs from the controller's render.
	revision, err := substrateRuntimeTemplateObjectRevision(template)
	if err != nil {
		t.Fatalf("recompute template revision: %v", err)
	}
	annotations := template.GetAnnotations()
	annotations[runtimePoolTemplateRevisionAnnotation] = revision
	template.SetAnnotations(annotations)
	if err := r.Update(context.Background(), template); err != nil {
		t.Fatalf("update deployed template: %v", err)
	}
	// The Update advanced the template's resourceVersion, which would trip
	// the uid/resourceVersion template fence BEFORE the policy verifier and
	// let these boundary tests pass even with the policy check removed.
	// Restamp the pool's fence to the updated template so reconciliation
	// reaches verifySubstrateDeployedDataSnapshotPolicy itself.
	refreshed := substrateTestDerivedTemplate(t, r, pool)
	if refreshed == nil {
		t.Fatal("updated template is required for fence restamp")
	}
	fence, err := substrateRuntimeTemplateFence(refreshed)
	if err != nil {
		t.Fatalf("compute template fence: %v", err)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[substrateActorTemplateFenceAnnotation] = fence
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("restamp template fence: %v", err)
	}
}

func TestSubstrateSuspendRefusesFullPolicyTemplateSwap(t *testing.T) {
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
	if got := runtimePoolTestGetPool(t, r, pool).Status.Lifecycle; got != corev1alpha1.RuntimePoolLifecycleQuiescent {
		t.Fatalf("lifecycle = %s, want Quiescent before the tamper", got)
	}

	// The deployed template is swapped to a Full policy between quiescence and
	// the provider call. The helper restamps the template fence so the explicit
	// deployed-policy verifier owns the refusal before any provider call.
	setDeployedSubstrateSnapshotPolicy(t, r, pool, substrateTestFullScope)
	runtimePoolReconcile(t, r, pool)
	if len(control.dataSuspended) != 0 {
		t.Fatalf("data suspensions = %v, want none under a Full-policy template", control.dataSuspended)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Annotations[substrateActorSuspendedAnnotation] == actorID &&
		control.actors[actorID] != nil && control.actors[actorID].Status == substrateTestStatusSuspended {
		t.Fatal("a consensual suspension must never settle under a Full-policy template")
	}
	// The refusal must come from the explicit policy verifier - the fence
	// was restamped to the tampered template, so a removed policy check
	// would otherwise let this boundary test pass vacuously.
	if !strings.Contains(current.Status.Message, "data-only snapshot policy") {
		t.Fatalf("status message = %q, want the explicit data-only policy refusal", current.Status.Message)
	}
}

func TestSubstrateResumeRefusesFullPolicyTemplateSwap(t *testing.T) {
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

	// Tamper BEFORE the resume intent lifts. Two independent boundaries
	// guard the resume: the bootstrap-neutral template-revision comparison
	// classifies a policy change while suspended as an infrastructure
	// contract change and recycles the actor (never resuming its data under
	// the changed policy), and verifySubstrateDeployedDataSnapshotPolicy
	// refuses a non-Data render outright (pinned directly by
	// TestVerifySubstrateDeployedDataSnapshotPolicy because the revision
	// boundary always fires first for external tampers).
	setDeployedSubstrateSnapshotPolicy(t, r, pool, substrateTestFullScope)
	substrateSuspendTestPoolIntent(t, r, pool, false)
	contractChangeObserved := false
	for range 10 {
		runtimePoolReconcile(t, r, pool)
		if strings.Contains(runtimePoolTestGetPool(t, r, pool).Status.Message, "no longer matches the infrastructure contract") {
			contractChangeObserved = true
		}
	}
	for index, resumed := range control.resumed {
		if resumed == actorID && !control.boots[index] {
			t.Fatal("a fromData resume must never run while the deployed template policy is not exactly data-only")
		}
	}
	if !contractChangeObserved {
		t.Fatalf("final status message = %q and the suspended-template contract-change refusal was never observed",
			runtimePoolTestGetPool(t, r, pool).Status.Message)
	}
	if !strings.Contains(runtimePoolTestGetPool(t, r, pool).Status.Message, "durable workspace data was lost") {
		t.Fatalf("final status message = %q, want the terminal fail-closed loss after refusing the policy-changed resume",
			runtimePoolTestGetPool(t, r, pool).Status.Message)
	}
}

// TestVerifySubstrateDeployedDataSnapshotPolicy pins the deployed-policy
// verifier directly: the resume flow's earlier revision boundary means an
// external tamper cannot reach it, so its own contract is proven here.
func TestVerifySubstrateDeployedDataSnapshotPolicy(t *testing.T) {
	t.Parallel()
	template := func(onPause, onCommit, fromData string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"spec": map[string]any{
				"snapshotsConfig": map[string]any{
					"onPause": onPause, "onCommit": onCommit,
					"onResume": map[string]any{"fromData": fromData},
				},
			},
		}}
	}
	if err := verifySubstrateDeployedDataSnapshotPolicy(template(substrateSnapshotScopeData, substrateSnapshotScopeData, substrateSnapshotResumeColdBoot)); err != nil {
		t.Fatalf("the exact Data/Data/ColdBoot policy must verify: %v", err)
	}
	for _, tampered := range []*unstructured.Unstructured{
		template(substrateTestFullScope, substrateSnapshotScopeData, substrateSnapshotResumeColdBoot),
		template(substrateSnapshotScopeData, substrateTestFullScope, substrateSnapshotResumeColdBoot),
		template(substrateSnapshotScopeData, substrateSnapshotScopeData, "FromSnapshot"),
		nil,
	} {
		err := verifySubstrateDeployedDataSnapshotPolicy(tampered)
		if err == nil {
			t.Fatalf("policy %v must be refused", tampered)
		}
		if tampered != nil && !strings.Contains(err.Error(), "data-only snapshot policy") {
			t.Fatalf("refusal error = %v, want the explicit data-only policy message", err)
		}
	}
}

func TestSubstrateWorkspaceBindingRejectsFullPoolSuspendMode(t *testing.T) {
	t.Parallel()
	pool := runtimePoolSubstrateTestObject()
	pool.Spec.ExecutionWorkspace.Substrate.SuspendMode = substrateTestFullScope
	if substrateRuntimePoolSuspendCapable(pool) {
		t.Fatal("a pool binding carrying a Full suspend mode must never be suspend-capable")
	}
	if runtimePoolWorkspaceSuspendCapable(pool) {
		t.Fatal("the shared capability check must reject a Full pool suspend mode")
	}
}
