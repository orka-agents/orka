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
	tamperedClass.SuspendMode = substrateTestFullScope
	tampered.Class = &tamperedClass
	if err := validateACPWorkspaceBindingValues(&tampered); err == nil {
		t.Fatal("a frozen binding tampered to a Full suspension mode must fail canonical verification")
	}
}

func setDeployedSubstrateSnapshotPolicy(t *testing.T, r *RuntimePoolReconciler, pool *corev1alpha1.RuntimePool, scope string) {
	t.Helper()
	template := substrateTestDerivedTemplate(t, r, pool)
	if template == nil {
		t.Fatal("derived template is required")
	}
	if err := unstructured.SetNestedMap(template.Object, map[string]any{
		"onPause": scope, "onCommit": scope,
	}, "spec", "snapshotsConfig"); err != nil {
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
	// the provider call. The template fence catches the foreign write first;
	// either way no live actor is ever suspended under a non-Data policy.
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

	substrateSuspendTestPoolIntent(t, r, pool, false)
	for range 10 {
		runtimePoolReconcile(t, r, pool)
		// Keep forcing the deployed template's policy to Full: the resume
		// refresh rewrites it toward the Data render, and every attempt to
		// resume from data must re-prove the policy first.
		if template := substrateTestDerivedTemplate(t, r, pool); template != nil {
			onPause, _, _ := unstructured.NestedString(template.Object, "spec", "snapshotsConfig", "onPause")
			if onPause == substrateSnapshotScopeData {
				setDeployedSubstrateSnapshotPolicy(t, r, pool, substrateTestFullScope)
			}
		}
	}
	for index, resumed := range control.resumed {
		if resumed == actorID && !control.boots[index] {
			t.Fatal("a fromData resume must never run while the deployed template policy is not exactly data-only")
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
