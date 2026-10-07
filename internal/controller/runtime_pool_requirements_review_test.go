package controller

import (
	"slices"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestExternalRuntimePoolFrozenKindSurvivesCapabilityDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		native bool
	}{
		{name: "native required feature removed", native: true},
		{name: "native capability added to planned Pod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExternalRuntimePoolFixture(t)
			if tc.native {
				f.advertiseNativeProcess(t)
			}
			provider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.provider), provider); err != nil {
				t.Fatal(err)
			}
			if tc.native {
				provider.Status.SupportedFeatures = slices.DeleteFunc(provider.Status.SupportedFeatures, func(feature workspacev1alpha1.ExecutionWorkspaceFeature) bool {
					return feature == workspacev1alpha1.WorkspaceFeatureNativeProcess
				})
			} else {
				provider.Status.SupportedFeatures = append(provider.Status.SupportedFeatures, workspacev1alpha1.WorkspaceFeatureNativeProcess)
			}
			if err := f.r.Update(t.Context(), provider); err != nil {
				t.Fatal(err)
			}
			for range 6 {
				_, _ = f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pool)})
			}
			request := f.currentWorkspace(t).Spec.Workload
			if tc.native {
				if request != nil || f.seeds != 0 || f.supervisor.probeCalls != 0 {
					t.Fatal("removed required capability downgraded native intent or released credentials")
				}
			} else if request == nil || slices.Contains(request.Runtime.RequiredFeatures, workspacev1alpha1.WorkspaceFeatureNativeProcess) || request.Runtime.BootstrapPort != runtimePoolPort || len(request.Runtime.Template.Spec.Volumes) == 0 {
				t.Fatal("late native capability changed the planned Pod materialization")
			}
		})
	}
}

func TestExternalRuntimePoolUnpublishedLegacyKindFailsClosed(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	f.advertiseNativeProcess(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	pool.Spec.ExecutionWorkspace.Workload.RequiredFeatures = nil
	if err := f.r.Update(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		_, _ = f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pool)})
	}
	if f.currentWorkspace(t).Spec.Workload != nil || f.seeds != 0 || f.supervisor.probeCalls != 0 {
		t.Fatal("unproven legacy intent selected a live provider capability")
	}
}

func TestExternalRuntimePoolPublishedLegacyPodSurvivesRestartAndResume(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	_, pod := f.serve(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	pool.Spec.ExecutionWorkspace.Workload.RequiredFeatures = nil
	if err := f.r.Update(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	f.advertiseNativeProcess(t)
	runtimePoolReconcile(t, f.r, f.pool)
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing {
		t.Fatal("established legacy Pod pool lost admission after restart")
	}
	w := f.currentWorkspace(t)
	w.Status.Allocation.State = workspacev1alpha1.AllocationStopped
	w.Status.Allocation.Startup = nil
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Delete(t.Context(), &pod); err != nil {
		t.Fatal(err)
	}
	cfg, err := f.r.runtimePoolConfig(&pool)
	if err != nil {
		t.Fatal(err)
	}
	auth, providerSecret, err := f.r.ensureRuntimePoolSecrets(t.Context(), &pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.publishExternalWorkspaceWorkload(t.Context(), &pool, cfg, w, auth, providerSecret); err != nil {
		t.Fatal(err)
	}
	request := f.currentWorkspace(t).Spec.Workload
	if request.Sequence != 2 || slices.Contains(request.Runtime.RequiredFeatures, workspacev1alpha1.WorkspaceFeatureNativeProcess) || request.Runtime.BootstrapPort != runtimePoolPort {
		t.Fatal("cold resume reselected legacy Pod intent from live native capability")
	}
}
