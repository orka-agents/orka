/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// runtimePoolPatchCounter counts metadata/spec patches of RuntimePools (not
// status writes) made through the wrapped client.
type runtimePoolPatchCounter struct {
	client.Client
	mu      sync.Mutex
	patches int
}

func (c *runtimePoolPatchCounter) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	if _, ok := object.(*corev1alpha1.RuntimePool); ok {
		c.mu.Lock()
		c.patches++
		c.mu.Unlock()
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

func (c *runtimePoolPatchCounter) take() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	patches := c.patches
	c.patches = 0
	return patches
}

func countRuntimePoolPatches(f *externalRuntimePoolFixture) *runtimePoolPatchCounter {
	counter := &runtimePoolPatchCounter{Client: f.r.Client}
	f.r.Client = counter
	return counter
}

// The admission pass records the instance evidence and the bootstrap
// credential binding in one pool write instead of two.
func TestExternalRuntimePoolAdmissionBindsEvidenceAndCredentialsInOneWrite(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	_, pod := f.materialize(t)
	counter := countRuntimePoolPatches(f)

	runtimePoolReconcile(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting {
		t.Fatalf("attested startup did not serve: %s/%s: %s", pool.Status.Lifecycle, pool.Status.AdmissionState, pool.Status.Message)
	}
	// Before: one patch for the evidence and another for the binding.
	if got := counter.take(); got != 1 {
		t.Fatalf("RuntimePool patches on the admission pass = %d, want 1", got)
	}
	evidence, err := externalRuntimeEvidence(&pool)
	if err != nil || evidence == nil || evidence.Pod.UID != pod.UID {
		t.Fatalf("instance evidence = %#v, %v; want Pod %s", evidence, err, pod.UID)
	}
	binding, err := runtimePoolBootstrapInstanceBindingFromAnnotation(&pool)
	auth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool)
	if err != nil || binding == nil || binding.WorkloadUID != pod.UID || binding.AuthSecretUID != auth.UID {
		t.Fatalf("bootstrap binding = %#v, %v; want Secret %s and Pod %s", binding, err, auth.UID, pod.UID)
	}
}

// Merging the writes keeps both conflict checks: credentials bound to another
// workload still close admission before bootstrap, with the evidence recorded
// and the foreign binding untouched, exactly as with two writes.
func TestExternalRuntimePoolAdmissionKeepsBootstrapBindingConflict(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	f.publish(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	foreign := `{"authSecretUID":"foreign-secret-uid","workloadUID":"foreign-pod-uid"}`
	pool.Annotations[runtimePoolBootstrapInstanceBindingAnnotation] = foreign
	if err := f.r.Update(context.Background(), &pool); err != nil {
		t.Fatal(err)
	}
	_, pod := f.materialize(t)

	runtimePoolReconcile(t, f.r, f.pool)
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded || !strings.Contains(pool.Status.Message, "conflicts with the private credential binding") {
		t.Fatalf("status = %s: %s, want Degraded on the credential binding conflict", pool.Status.Lifecycle, pool.Status.Message)
	}
	if pool.Annotations[runtimePoolBootstrapInstanceBindingAnnotation] != foreign {
		t.Fatalf("foreign bootstrap binding was replaced: %q", pool.Annotations[runtimePoolBootstrapInstanceBindingAnnotation])
	}
	if evidence, err := externalRuntimeEvidence(&pool); err != nil || evidence == nil || evidence.Pod.UID != pod.UID {
		t.Fatalf("instance evidence = %#v, %v; want Pod %s", evidence, err, pod.UID)
	}
	if f.seeds != 0 || f.supervisor.probeCalls != 0 {
		t.Fatalf("bootstrap/probe calls = %d/%d, want none after a conflict", f.seeds, f.supervisor.probeCalls)
	}
}
