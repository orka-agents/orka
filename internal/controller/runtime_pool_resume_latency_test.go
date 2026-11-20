/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
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

// stopExternalRuntime retires the served instance of f for a scale-to-zero
// and reports it stopped, leaving the Pod for the caller to remove.
func stopExternalRuntime(t *testing.T, f *externalRuntimePoolFixture) {
	t.Helper()
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	pool.Spec.DesiredReplicas = 0
	pool.Annotations["orka.ai/external-runtime-retirement-requested"] = "true"
	if err := f.r.Update(context.Background(), &pool); err != nil {
		t.Fatal(err)
	}
	workspace := f.currentWorkspace(t)
	workspace.Status.Allocation.State = workspacev1alpha1.AllocationStopped
	workspace.Status.Allocation.Startup = nil
	workspace.Status.ObservedGeneration = workspace.Generation
	if err := f.r.Status().Update(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
}

// reconcileExternalRuntimeUntilStopped reconciles a retiring pool whose
// instance is gone until it reports Stopped.
func reconcileExternalRuntimeUntilStopped(t *testing.T, f *externalRuntimePoolFixture) corev1alpha1.RuntimePool {
	t.Helper()
	var pool corev1alpha1.RuntimePool
	for range 4 {
		runtimePoolReconcile(t, f.r, f.pool)
		if pool = runtimePoolTestGetPool(t, f.r, f.pool); pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleStopped {
			return pool
		}
	}
	t.Fatalf("proven stop did not report Stopped: %s: %s", pool.Status.Lifecycle, pool.Status.Message)
	return pool
}

// The consumed private credentials rotate when the stop is proven, so the
// cold boot that resumes the workspace publishes on its first pass. The
// rotation still waits for the independent proof, and the resumed request
// carries credentials the stopped instance never had.
func TestExternalRuntimePoolRotatesConsumedCredentialsWhenStopIsProven(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	workspace, pod := f.serve(t)
	oldRequest := workspace.Spec.Workload.DeepCopy()
	oldAuth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool)
	stopExternalRuntime(t, f)

	// The provider asserts the stop, but the Pod still exists: nothing rotates.
	runtimePoolReconcile(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleStopped {
		t.Fatal("pool reported Stopped while the retired Pod still exists")
	}
	if binding, err := runtimePoolBootstrapInstanceBindingFromAnnotation(&pool); err != nil || binding == nil || binding.AuthSecretUID != oldAuth.UID {
		t.Fatalf("bootstrap binding before the proven stop = %#v, %v; want the consumed Secret", binding, err)
	}
	if auth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool); auth.UID != oldAuth.UID {
		t.Fatal("credentials rotated before the stop was proven")
	}

	// The independent proof: the exact Pod is gone. The same pass reports
	// Stopped and rotates.
	if err := f.r.Delete(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped || pool.Status.ActiveInstance != nil {
		t.Fatalf("proven stop did not report Stopped: %s: %s", pool.Status.Lifecycle, pool.Status.Message)
	}
	if binding, err := runtimePoolBootstrapInstanceBindingFromAnnotation(&pool); err != nil || binding != nil {
		t.Fatalf("bootstrap binding after the proven stop = %#v, %v; want cleared", binding, err)
	}
	newAuth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool)
	if newAuth.UID == oldAuth.UID {
		t.Fatal("consumed credentials were not rotated at the proven stop")
	}
	for _, key := range []string{runtimePoolControllerTokenKey, runtimePoolCapabilitySecretKey, runtimePoolBootstrapNonceKey, runtimePoolBootstrapSigningSeedKey} {
		if bytes.Equal(newAuth.Data[key], oldAuth.Data[key]) {
			t.Fatalf("rotated credentials reuse %s from the stopped instance", key)
		}
	}
	if err := f.r.Get(context.Background(), client.ObjectKeyFromObject(&oldAuth), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("consumed Secret still exists after rotation: %v", err)
	}

	// The next cold boot publishes on its first pass, with the rotated
	// bootstrap material. Before this change it first spent two passes
	// rotating.
	pool.Spec.DesiredReplicas = 1
	if err := f.r.Update(context.Background(), &pool); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	workspace = f.currentWorkspace(t)
	if workspace.Spec.Workload == nil || workspace.Spec.Workload.Sequence != oldRequest.Sequence+1 {
		t.Fatalf("cold boot did not publish on its first pass: %#v", runtimePoolTestGetPool(t, f.r, f.pool).Status)
	}
	if auth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool); auth.UID != newAuth.UID {
		t.Fatal("publication rotated the already rotated credentials again")
	}
	nonce := ""
	for _, env := range workspace.Spec.Workload.Runtime.Template.Spec.Containers[0].Env {
		if env.Name == runtimePoolBootstrapNonceEnv {
			nonce = env.Value
		}
	}
	if nonce == "" || nonce != string(newAuth.Data[runtimePoolBootstrapNonceKey]) || nonce == string(oldAuth.Data[runtimePoolBootstrapNonceKey]) {
		t.Fatalf("published bootstrap nonce %q is not the rotated one", nonce)
	}
}

// A cleanup-only controller never mints runtime credentials, so a proven stop
// it observes leaves the rotation to the controller that cold-boots next.
func TestExternalRuntimePoolDoesNotRotateCredentialsForCleanupOnlyController(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	_, pod := f.serve(t)
	oldAuth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool)
	stopExternalRuntime(t, f)
	if err := f.r.Delete(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	f.r.WorkspaceCleanupOnly = true
	reconcileExternalRuntimeUntilStopped(t, f)
	if auth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool); auth.UID != oldAuth.UID {
		t.Fatal("a cleanup-only controller minted fresh runtime credentials")
	}
}
