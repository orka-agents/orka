package controller

import (
	"context"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestExternalRuntimePoolWaitsForAuthoritativeEpochBeforeMaterialization(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	f.r.Epochs = &ControllerEpochManager{}
	for range 3 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	if f.currentWorkspace(t).Spec.Workload != nil || f.seeds != 0 {
		t.Fatal("runtime materialized before the configured epoch authority was ready")
	}
	secrets := &corev1.SecretList{}
	if err := f.r.List(context.Background(), secrets, client.MatchingLabels{runtimePoolUIDLabel: string(f.pool.UID)}); err != nil {
		t.Fatal(err)
	}
	if len(secrets.Items) != 0 {
		t.Fatal("private credentials were created using the fallback epoch")
	}
	f.r.Epochs.current = &store.ControllerEpoch{Epoch: 9}
	w := f.publish(t)
	_, cfg, err := runtimePoolPodTemplateValidationTarget(f.pool, w.Spec.Workload.Runtime.Template)
	if err != nil || cfg.controllerEpoch != 9 {
		t.Fatalf("published epoch = %d, error = %v", cfg.controllerEpoch, err)
	}
}

func TestExternalRuntimePoolClosesAdmissionWhenAuthoritativeEpochAdvances(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	f.serve(t)
	seeds := f.seeds
	f.r.Epochs = &ControllerEpochManager{current: &store.ControllerEpoch{Epoch: 9}}
	runtimePoolReconcile(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed || pool.Annotations["orka.ai/external-runtime-retirement-requested"] != "true" || f.seeds != seeds {
		t.Fatal("old runtime fence remained eligible after controller epoch advance")
	}
}
