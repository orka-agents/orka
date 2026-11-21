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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
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

// A published cold boot is a wait, not a failure, until the provider reports
// the new allocation. Core re-admits the publication's generation before the
// provider's next status write lands, so passes in between still see the
// predecessor's stopped allocation. They used to report Degraded ("stopped
// allocation does not prove the current runtime terminated"), which is the
// reopenedFrom=Degraded of spare and shared resumes in round 9c: those
// providers report the new allocation only with its startup evidence, so the
// label lasted until admission. The wait still never publishes again.
func TestExternalRuntimePoolPublishedResumeWaitsForProviderWithoutDegrading(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	_, pod := f.serve(t)
	stopExternalRuntime(t, f)
	if err := f.r.Delete(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	pool := reconcileExternalRuntimeUntilStopped(t, f)
	pool.Spec.DesiredReplicas = 1
	if err := f.r.Update(context.Background(), &pool); err != nil {
		t.Fatal(err)
	}
	stopped := f.currentWorkspace(t).Status.Allocation.DeepCopy()
	runtimePoolReconcile(t, f.r, f.pool)
	workspace := f.currentWorkspace(t)
	request := workspace.Spec.Workload.DeepCopy()
	if request.Sequence != stopped.Sequence+1 || request.PreviousInstance == nil || *request.PreviousInstance != stopped.Identity {
		t.Fatalf("cold boot did not publish the successor request: %#v", request)
	}

	// Core re-admits the published generation (the fake client keeps the
	// generation, so that is the admitted one); the provider has not yet
	// reported the new allocation.
	if workspace.Spec.CoreAdmission.AdmittedGeneration != workspace.Generation {
		workspace.Spec.CoreAdmission.AdmittedGeneration = workspace.Generation
		if err := f.r.Update(context.Background(), workspace); err != nil {
			t.Fatal(err)
		}
		workspace = f.currentWorkspace(t)
	}
	if !workspaceCurrentlyAdmittedByCore(workspace) {
		t.Fatalf("fixture did not re-admit generation %d: %#v", workspace.Generation, workspace.Spec.CoreAdmission)
	}
	for _, observed := range []int64{workspace.Status.ObservedGeneration, workspace.Generation} {
		workspace.Status.ObservedGeneration = observed
		if err := f.r.Status().Update(context.Background(), workspace); err != nil {
			t.Fatal(err)
		}
		runtimePoolReconcile(t, f.r, f.pool)
		pool = runtimePoolTestGetPool(t, f.r, f.pool)
		if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStarting || pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
			t.Fatalf("observed generation %d: status = %s/%s: %s, want Starting/Closed", observed, pool.Status.Lifecycle, pool.Status.AdmissionState, pool.Status.Message)
		}
		workspace = f.currentWorkspace(t)
		if workspace.Spec.Workload.Sequence != request.Sequence || workspace.Spec.Workload.Revision != request.Revision {
			t.Fatalf("the wait republished: sequence %d, want %d", workspace.Spec.Workload.Sequence, request.Sequence)
		}
	}

	// A stopped allocation that does not continue the published request is
	// still a failure.
	workspace.Status.Allocation.Sequence = request.Sequence
	if err := f.r.Status().Update(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded || !strings.Contains(pool.Status.Message, "does not prove the current runtime terminated") {
		t.Fatalf("inconsistent stopped allocation: status = %s: %s, want Degraded", pool.Status.Lifecycle, pool.Status.Message)
	}
}

// runtimePoolStatusWriteCounter counts RuntimePool status writes made
// through the wrapped client, and how many of them conflicted.
type runtimePoolStatusWriteCounter struct {
	client.Client
	mu        sync.Mutex
	writes    int
	conflicts int
}

type runtimePoolCountingStatusWriter struct {
	client.SubResourceWriter
	counter *runtimePoolStatusWriteCounter
}

func (c *runtimePoolStatusWriteCounter) Status() client.SubResourceWriter {
	return &runtimePoolCountingStatusWriter{SubResourceWriter: c.Client.Status(), counter: c}
}

func (c *runtimePoolStatusWriteCounter) record(object client.Object, err error) error {
	if _, ok := object.(*corev1alpha1.RuntimePool); ok {
		c.mu.Lock()
		c.writes++
		if apierrors.IsConflict(err) {
			c.conflicts++
		}
		c.mu.Unlock()
	}
	return err
}

func (c *runtimePoolStatusWriteCounter) take() (writes, conflicts int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	writes, conflicts = c.writes, c.conflicts
	c.writes, c.conflicts = 0, 0
	return writes, conflicts
}

func (w *runtimePoolCountingStatusWriter) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
	return w.counter.record(object, w.SubResourceWriter.Patch(ctx, object, patch, options...))
}

func (w *runtimePoolCountingStatusWriter) Update(ctx context.Context, object client.Object, options ...client.SubResourceUpdateOption) error {
	return w.counter.record(object, w.SubResourceWriter.Update(ctx, object, options...))
}

// resumeExternalRuntime takes a served pool through a proven stop and the
// next cold boot, as a resume does: the replica changes advance the pool's
// generation, core re-admits the published successor, and the provider
// reports its attested startup on a new Pod. It returns that Pod.
func resumeExternalRuntime(t *testing.T, f *externalRuntimePoolFixture, oldPod corev1.Pod) corev1.Pod {
	t.Helper()
	ctx := context.Background()
	stopExternalRuntime(t, f)
	if err := f.r.Delete(ctx, &oldPod); err != nil {
		t.Fatal(err)
	}
	pool := reconcileExternalRuntimeUntilStopped(t, f)
	// The API server advances the generation for the scale-down and again for
	// the scale-up; the fake client does neither.
	pool.Generation += 2
	pool.Spec.DesiredReplicas = 1
	if err := f.r.Update(ctx, &pool); err != nil {
		t.Fatal(err)
	}
	if got := runtimePoolTestGetPool(t, f.r, f.pool); got.Generation != pool.Generation {
		t.Fatalf("fixture pool generation = %d, want %d", got.Generation, pool.Generation)
	}
	stopped := f.currentWorkspace(t).Status.Allocation.DeepCopy()
	runtimePoolReconcile(t, f.r, f.pool)
	workspace := f.currentWorkspace(t)
	request := workspace.Spec.Workload
	if request == nil || request.Sequence != stopped.Sequence+1 {
		t.Fatalf("cold boot did not publish the successor request: %#v", request)
	}
	if workspace.Spec.CoreAdmission.AdmittedGeneration != workspace.Generation {
		workspace.Spec.CoreAdmission.AdmittedGeneration = workspace.Generation
		if err := f.r.Update(ctx, workspace); err != nil {
			t.Fatal(err)
		}
		workspace = f.currentWorkspace(t)
		request = workspace.Spec.Workload
	}
	pod := runtimePoolReadyPod(f.pool, request.Runtime.Template.Namespace, "external-runtime-resumed", "external-pod-uid-2", "10.0.0.72")
	pod.Labels = cloneStringMap(request.Runtime.Template.Labels)
	pod.Annotations = cloneStringMap(request.Runtime.Template.Annotations)
	pod.Spec = *request.Runtime.Template.Spec.DeepCopy()
	runtimePoolTestCreatePod(t, f.r, &pod)
	identity := workspacev1alpha1.InstanceIdentity{AllocationID: stopped.Identity.AllocationID, InstanceID: string(pod.UID), RequestRevision: request.Revision}
	workspace.Status.Allocation = &workspacev1alpha1.AllocationObservation{Sequence: request.Sequence, Key: request.Key, Identity: identity, State: workspacev1alpha1.AllocationReady,
		Startup: &workspacev1alpha1.StartupEvidence{ContractVersion: workspacev1alpha1.LifecycleContractV1, Identity: identity, Endpoint: "http://10.0.0.72:8080", Pod: &workspacev1alpha1.PodReference{Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID}}}
	workspace.Status.ObservedGeneration = workspace.Generation
	if err := f.r.Status().Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	return pod
}

// The probe→Serving sequence of a resumed runtime, pinned pass by pass.
//
// In round 11 the provider's Ready report woke the pool to admit the resumed
// runtime and, at the same moment, the waiting Task to finish its workspace
// handshake. That handshake re-stamped the pool's image provenance because
// the resume's replica change had advanced the pool's generation. The status
// write landed while the pool probed, so the pool's optimistic Serving write
// conflicted and the whole admission pass (attest, bootstrap, probe, fence)
// ran again: admit.probeToServingMs rose from 179 to 434 ms on private
// resumes and from 242 to 510 ms on shared ones.
//
// Here the Task's handshake runs inside every probe. A ready supervisor must
// serve on the first pass and a booting one on the pass its one-second
// re-probe starts, with one probe per pass and no conflicted status write.
func TestExternalRuntimePoolResumeServesWhileTheTaskHandshakeLands(t *testing.T) {
	for _, test := range []struct {
		name    string
		booting bool
	}{
		{name: "ready supervisor"},
		{name: "booting then ready supervisor", booting: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			f := newExternalRuntimePoolFixture(t)
			_, oldPod := f.serve(t)
			taskWrites := &runtimePoolStatusWriteCounter{Client: f.r.Client}
			task := &TaskReconciler{Client: taskWrites}
			// The first turn's Task proved the pool's image provenance.
			served := runtimePoolTestGetPool(t, f.r, f.pool)
			if err := task.recordACPRuntimePoolImageProvenance(ctx, &served); err != nil {
				t.Fatal(err)
			}
			if writes, _ := taskWrites.take(); writes != 1 {
				t.Fatalf("first-turn provenance writes = %d, want 1", writes)
			}

			pod := resumeExternalRuntime(t, f, oldPod)
			resumed := runtimePoolTestGetPool(t, f.r, f.pool)
			ready := runtimePoolValidProbe(&resumed, &pod, "external-boot-2", false)
			f.supervisor.probe = ready
			if test.booting {
				booting := ready
				booting.Status.Lifecycle = harnessv2.SupervisorLifecycleBooting
				booting.Status.Drain.AcceptingNewSessions = false
				f.supervisor.probe = booting
			}
			handshakes := 0
			f.supervisor.afterProbe = func() {
				handshakes++
				current := runtimePoolTestGetPool(t, f.r, f.pool)
				if err := task.recordACPRuntimePoolImageProvenance(ctx, &current); err != nil {
					t.Errorf("Task handshake: %v", err)
				}
			}
			poolWrites := &runtimePoolStatusWriteCounter{Client: f.r.Client}
			f.r.Client = poolWrites
			f.supervisor.probeCalls = 0

			if test.booting {
				result := runtimePoolReconcile(t, f.r, f.pool)
				pool := runtimePoolTestGetPool(t, f.r, f.pool)
				if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStarting || pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed ||
					pool.Status.Message != "runtime supervisor is booting" {
					t.Fatalf("booting probe: status = %s/%s: %q, want Starting/Closed booting", pool.Status.Lifecycle, pool.Status.AdmissionState, pool.Status.Message)
				}
				if result.RequeueAfter != runtimePoolBootingRequeue {
					t.Fatalf("booting probe requeue = %s, want %s", result.RequeueAfter, runtimePoolBootingRequeue)
				}
				if f.supervisor.probeCalls != 1 {
					t.Fatalf("booting pass probes = %d, want 1", f.supervisor.probeCalls)
				}
				if _, conflicts := poolWrites.take(); conflicts != 0 {
					t.Fatalf("booting pass status conflicts = %d, want 0", conflicts)
				}
				f.supervisor.probe = ready
				f.supervisor.probeCalls = 0
			}

			runtimePoolReconcile(t, f.r, f.pool)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			if !runtimePoolAdmitting(&pool) {
				t.Fatalf("resumed runtime did not serve on its ready pass: %s/%s: %s", pool.Status.Lifecycle, pool.Status.AdmissionState, pool.Status.Message)
			}
			if pool.Status.ActiveInstance == nil || pool.Status.ActiveInstance.PodUID != string(pod.UID) || pool.Status.ActiveInstance.BootID != "external-boot-2" {
				t.Fatalf("active fence = %#v, want the resumed Pod", pool.Status.ActiveInstance)
			}
			if f.supervisor.probeCalls != 1 {
				t.Fatalf("ready pass probes = %d, want 1", f.supervisor.probeCalls)
			}
			if _, conflicts := poolWrites.take(); conflicts != 0 {
				t.Fatalf("ready pass status conflicts = %d, want 0", conflicts)
			}
			wantHandshakes := 1
			if test.booting {
				wantHandshakes = 2
			}
			if handshakes != wantHandshakes {
				t.Fatalf("Task handshakes = %d, want %d", handshakes, wantHandshakes)
			}
			// The proof from the first turn still holds; the Task wrote nothing.
			if writes, _ := taskWrites.take(); writes != 0 {
				t.Fatalf("Task provenance writes during the resumed admission = %d, want 0", writes)
			}
			if !runtimePoolImageProvenanceRecorded(&pool) {
				t.Fatal("resumed pool lost its image provenance")
			}
		})
	}
}

// The Task stamps image provenance once per pool. A replica-only generation
// change keeps the proof; a pool without a true proof is still stamped.
func TestRecordACPRuntimePoolImageProvenanceKeepsAnEarlierGenerationProof(t *testing.T) {
	ctx := context.Background()
	f := newExternalRuntimePoolFixture(t)
	writes := &runtimePoolStatusWriteCounter{Client: f.r.Client}
	task := &TaskReconciler{Client: writes}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if err := task.recordACPRuntimePoolImageProvenance(ctx, &pool); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(pool.Status.Conditions, acpRuntimePoolImageProvenanceCondition)
	if n, _ := writes.take(); n != 1 || condition == nil || condition.ObservedGeneration != pool.Generation {
		t.Fatalf("first provenance: writes = %d, condition = %#v", n, condition)
	}
	proven := pool.Generation

	pool.Generation += 2
	pool.Spec.DesiredReplicas = 0
	if err := f.r.Update(ctx, &pool); err != nil {
		t.Fatal(err)
	}
	if err := task.recordACPRuntimePoolImageProvenance(ctx, &pool); err != nil {
		t.Fatal(err)
	}
	condition = meta.FindStatusCondition(pool.Status.Conditions, acpRuntimePoolImageProvenanceCondition)
	if n, _ := writes.take(); n != 0 || condition == nil || condition.ObservedGeneration != proven {
		t.Fatalf("after a replica change: writes = %d, condition = %#v; want the generation-%d proof kept", n, condition, proven)
	}

	for _, mutate := range []func(*metav1.Condition){
		func(c *metav1.Condition) { c.Status = metav1.ConditionFalse },
		func(c *metav1.Condition) { c.Reason = "Other" },
		func(c *metav1.Condition) { c.ObservedGeneration = pool.Generation + 1 },
	} {
		current := runtimePoolTestGetPool(t, f.r, f.pool)
		for i := range current.Status.Conditions {
			if current.Status.Conditions[i].Type == acpRuntimePoolImageProvenanceCondition {
				mutate(&current.Status.Conditions[i])
			}
		}
		if err := f.r.Status().Update(ctx, &current); err != nil {
			t.Fatal(err)
		}
		if err := task.recordACPRuntimePoolImageProvenance(ctx, &current); err != nil {
			t.Fatal(err)
		}
		if n, _ := writes.take(); n != 1 || !runtimePoolImageProvenanceRecorded(&current) {
			t.Fatalf("unproven pool: writes = %d, recorded = %v; want it stamped", n, runtimePoolImageProvenanceRecorded(&current))
		}
	}
}
