/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/workspace"
)

func TestSubstrateActorPoolReportsResourceExhaustedAsARetryableCapacityWait(t *testing.T) {
	scheme := newSubstrateActorPoolTestScheme(t)
	pool := &corev1alpha1.SubstrateActorPool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.SubstrateActorPool{}).WithObjects(pool).Build()
	r := &SubstrateActorPoolReconciler{Client: c, Scheme: scheme}
	before := metrics.CounterVecValue(metrics.WaitingForCapacityTotal, "substrate_actor_pool", "ResourceExhausted")

	exhausted := workspace.NewError("create actor", workspace.ErrorKindResourceExhausted, "no worker capacity", true, nil)
	for range 2 {
		current := &corev1alpha1.SubstrateActorPool{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
			t.Fatal(err)
		}
		result, err := r.updateSubstrateActorPoolExecutorError(context.Background(), current, exhausted)
		if err != nil || result.RequeueAfter <= 0 {
			t.Fatalf("a capacity wait must requeue, got %v %v", result, err)
		}
	}
	got := &corev1alpha1.SubstrateActorPool{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pool), got); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(got.Status.Conditions, conditionReasonReady)
	if got.Status.Phase != corev1alpha1.SubstrateActorPoolPhasePending || condition == nil ||
		condition.Reason != corev1alpha1.TaskConditionWaitingForCapacity {
		t.Fatalf("capacity exhaustion must be Pending/WaitingForCapacity, got %s %+v", got.Status.Phase, condition)
	}
	if delta := metrics.CounterVecValue(metrics.WaitingForCapacityTotal, "substrate_actor_pool", "ResourceExhausted") - before; delta != 1 {
		t.Fatalf("capacity metric recorded %v transitions, want 1", delta)
	}

	if _, err := r.updateSubstrateActorPoolExecutorError(context.Background(), got, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != corev1alpha1.SubstrateActorPoolPhaseFailed {
		t.Fatalf("other executor errors still fail the pool, got %s", got.Status.Phase)
	}
}
