/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

type retirementAttemptInventory struct {
	mu     sync.RWMutex
	active []store.HarnessV1Attempt
}

func (i *retirementAttemptInventory) ListActiveHarnessV1Attempts(context.Context) ([]store.HarnessV1Attempt, error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return append([]store.HarnessV1Attempt(nil), i.active...), nil
}

func (i *retirementAttemptInventory) setActive(attempts ...store.HarnessV1Attempt) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.active = append([]store.HarnessV1Attempt(nil), attempts...)
}

func TestHarnessV1RetirementWaitsForDisabledControlAndEmptyAttemptInventory(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	control := &corev1alpha1.AgentExecutionControl{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: corev1alpha1.AgentExecutionControlNamespace,
			Name:      corev1alpha1.AgentExecutionControlName,
			UID:       types.UID("control-uid"),
		},
		Spec: corev1alpha1.AgentExecutionControlSpec{Backends: corev1alpha1.AgentExecutionBackendsSpec{
			V1: corev1alpha1.AgentExecutionBackendSpec{DesiredMode: corev1alpha1.AgentExecutionModeEnabled},
			V2: corev1alpha1.AgentExecutionBackendSpec{DesiredMode: corev1alpha1.AgentExecutionModeEnabled},
		}},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.AgentExecutionControl{}).
		WithObjects(control).
		Build()
	inventory := &retirementAttemptInventory{}
	inventory.setActive(store.HarnessV1Attempt{Namespace: "test", TaskUID: "task-uid", Attempt: 1})
	coordinator := &HarnessV1RetirementCoordinator{
		Client: kube, APIReader: kube, Attempts: inventory, PollInterval: 5 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- coordinator.Retire(ctx) }()

	key := client.ObjectKeyFromObject(control)
	eventually(t, time.Second, func() bool {
		current := &corev1alpha1.AgentExecutionControl{}
		return kube.Get(ctx, key, current) == nil &&
			current.Spec.Backends.V1.DesiredMode == corev1alpha1.AgentExecutionModeDisabled
	})
	current := &corev1alpha1.AgentExecutionControl{}
	if err := kube.Get(ctx, key, current); err != nil {
		t.Fatalf("get disabled control: %v", err)
	}
	now := metav1.Now()
	current.Status.ObservedGeneration = current.Generation
	current.Status.Backends = &corev1alpha1.AgentExecutionBackendsStatus{
		V1: corev1alpha1.AgentExecutionBackendStatus{
			EffectiveMode:         corev1alpha1.AgentExecutionEffectiveModeDisabled,
			ModeRevision:          2,
			AdmissionClosedAt:     &now,
			CutoffInventoryDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		V2: corev1alpha1.AgentExecutionBackendStatus{
			EffectiveMode: corev1alpha1.AgentExecutionEffectiveModeEnabled,
			ModeRevision:  1,
		},
	}
	if err := kube.Status().Update(ctx, current); err != nil {
		t.Fatalf("publish disabled status: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("retirement completed with an active attempt: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	inventory.setActive()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("retire: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("retirement did not complete after attempts settled")
	}
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}
