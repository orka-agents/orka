package controller

import (
	"context"
	"strconv"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// runtimePoolTestAdoptUnenrolledBoot reproduces a cold start that loses the
// race with scale-down: desiredReplicas reaches zero before the marked Pod is
// Ready, so the serving path never enrolls the boot. Scale-down then adopts
// the Ready Pod as the active instance and requests its drain. The returned
// fixture's supervisor reports that drain as quiescent.
func runtimePoolTestAdoptUnenrolledBoot(t *testing.T) *nativePoolBootFixture {
	t.Helper()
	f := newNativePoolBootFixture(t, false)
	current := runtimePoolTestGetPool(t, f.r, f.pool)
	current.Spec.DesiredReplicas = 0
	if err := f.r.Update(t.Context(), &current); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	active := pool.Status.ActiveInstance
	if f.supervisor.drainCalls != 1 || pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDraining ||
		active == nil || active.PodUID != string(f.pod.UID) || active.ControllerEpoch != f.r.ControllerEpoch {
		t.Fatalf("scale-down did not adopt and drain the unenrolled boot: drains=%d lifecycle=%s active=%#v",
			f.supervisor.drainCalls, pool.Status.Lifecycle, active)
	}
	runtimePoolTestAssertNoBootEvidence(t, f)
	f.supervisor.probe = runtimePoolValidProbe(f.pool, &f.pod, "native-boot", true)
	return f
}

func runtimePoolTestAssertNoBootEvidence(t *testing.T, f *nativePoolBootFixture) {
	t.Helper()
	var effects corev1alpha1.ExternalEffectList
	if err := f.r.List(t.Context(), &effects, client.InNamespace(f.pool.Namespace)); err != nil {
		t.Fatal(err)
	}
	for i := range effects.Items {
		switch effects.Items[i].Spec.Kind {
		case runtimePoolBootPreparationKind, runtimePoolBootWitnessKind, runtimePoolBootRetirementKind:
			t.Fatalf("unenrolled boot acquired %s evidence", effects.Items[i].Spec.Kind)
		}
	}
	pod := &corev1.Pod{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); apierrors.IsNotFound(err) {
		return
	} else if err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) || pod.Annotations[runtimePoolBootRetentionAnnotation] != "" {
		t.Fatal("unenrolled boot acquired a retention barrier")
	}
}

func runtimePoolTestStopUnenrolledBoot(t *testing.T, f *nativePoolBootFixture) {
	t.Helper()
	deployment := runtimePoolTestDeployment(t, f.r, f.pool.Namespace, runtimePoolResourceName(f.pool.Namespace, f.pool.Name))
	if got := ptr.Deref(deployment.Spec.Replicas, -1); got != 0 {
		t.Fatalf("quiescent unenrolled boot left Deployment replicas at %d, want 0", got)
	}
	// Without a retention barrier the stopped Pod terminates immediately.
	if err := f.r.Delete(t.Context(), &f.pod); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("unenrolled runtime Pod was retained: %v", err)
	}
}

// A marked boot with neither a witness nor a retention barrier never admitted
// work. Its exact authenticated quiescent drain must complete scale-to-zero
// instead of failing on the missing witness on every reconcile.
func TestRuntimePoolBootUnenrolledScaleDownCompletesWithoutWitness(t *testing.T) {
	f := runtimePoolTestAdoptUnenrolledBoot(t)
	runtimePoolReconcile(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleQuiescent {
		t.Fatalf("lifecycle after quiescent drain = %s, want Quiescent", pool.Status.Lifecycle)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	runtimePoolTestStopUnenrolledBoot(t, f)
	runtimePoolReconcile(t, f.r, f.pool)
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped || pool.Status.ActiveInstance != nil {
		t.Fatalf("status after scale-to-zero = %s active=%#v, want Stopped without an active instance", pool.Status.Lifecycle, pool.Status.ActiveInstance)
	}
	if f.supervisor.drainCalls != 1 {
		t.Fatalf("drain calls = %d, want 1", f.supervisor.drainCalls)
	}
	runtimePoolTestAssertNoBootEvidence(t, f)
}

// A new leader renders a new per-epoch template, so the adopted boot drains
// through the rollout path. That path must also complete, moving the pool to
// the new epoch so the next planned upgrade drain can settle.
func TestRuntimePoolBootUnenrolledBootRetiresAfterControllerTakeover(t *testing.T) {
	f := runtimePoolTestAdoptUnenrolledBoot(t)
	f.r.ControllerEpoch++
	epoch := f.r.ControllerEpoch
	runtimePoolReconcile(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleQuiescent || pool.Status.ControllerEpoch != epoch {
		t.Fatalf("rollout drain status = %s at epoch %d, want Quiescent at epoch %d", pool.Status.Lifecycle, pool.Status.ControllerEpoch, epoch)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	runtimePoolTestStopUnenrolledBoot(t, f)
	runtimePoolReconcile(t, f.r, f.pool)
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped || pool.Status.ActiveInstance != nil || pool.Status.ControllerEpoch != epoch {
		t.Fatalf("status after rollout = %s at epoch %d active=%#v, want Stopped at epoch %d without an active instance",
			pool.Status.Lifecycle, pool.Status.ControllerEpoch, pool.Status.ActiveInstance, epoch)
	}
	deployment := runtimePoolTestDeployment(t, f.r, f.pool.Namespace, runtimePoolResourceName(f.pool.Namespace, f.pool.Name))
	if got := runtimePoolLiteralEnvironment(deployment.Spec.Template.Spec.Containers[0].Env)["ORKA_ACP_CONTROLLER_EPOCH"]; got != strconv.FormatInt(epoch, 10) {
		t.Fatalf("staged Deployment controller epoch = %q, want %d", got, epoch)
	}
	runtimePoolTestAssertNoBootEvidence(t, f)

	fence, err := f.r.Epochs.CurrentFence(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	drain := NewACPUpgradeDrainCoordinator(f.r.Client, f.r.APIReader, fixedUpgradeDrainEpochSource{fence: fence}, nil,
		ACPUpgradeDrainBarrierObserverFunc(func(context.Context) (ACPUpgradeDrainBarrierSnapshot, error) {
			return ACPUpgradeDrainBarrierSnapshot{}, nil
		}), nil, testUpgradeDrainOptions())
	snapshot, err := drain.reconcileDrainPass(t.Context(), fence)
	if err != nil || !snapshot.Quiescent() {
		t.Fatalf("planned drain pass at epoch %d = %#v, %v; want quiescent", fence.Epoch, snapshot, err)
	}
}

// A controller restart while a pool cold-starts renders a new per-epoch
// template before the Pod ever serves, even though desiredReplicas stays 1.
// The rollout path then adopts the Ready, never-enrolled boot and drains it;
// that drain must complete so the new template can start.
func TestRuntimePoolBootUnenrolledColdStartDrainsAcrossControllerRestart(t *testing.T) {
	f := newNativePoolBootFixture(t, false)
	previous := f.r.ControllerEpoch
	f.r.ControllerEpoch++
	epoch := f.r.ControllerEpoch
	runtimePoolReconcile(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	active := pool.Status.ActiveInstance
	if f.supervisor.drainCalls != 1 || pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDraining ||
		active == nil || active.PodUID != string(f.pod.UID) || active.ControllerEpoch != previous {
		t.Fatalf("rollout did not adopt and drain the cold-started boot: drains=%d lifecycle=%s active=%#v",
			f.supervisor.drainCalls, pool.Status.Lifecycle, active)
	}
	runtimePoolTestAssertNoBootEvidence(t, f)
	f.supervisor.probe = runtimePoolValidProbe(f.pool, &f.pod, "native-boot", true)
	runtimePoolReconcile(t, f.r, f.pool)
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleQuiescent || pool.Status.ControllerEpoch != epoch {
		t.Fatalf("rollout drain status = %s at epoch %d, want Quiescent at epoch %d", pool.Status.Lifecycle, pool.Status.ControllerEpoch, epoch)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	runtimePoolTestStopUnenrolledBoot(t, f)
	runtimePoolReconcile(t, f.r, f.pool)
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStarting || pool.Status.ActiveInstance != nil {
		t.Fatalf("status after rollout = %s active=%#v, want Starting the new template", pool.Status.Lifecycle, pool.Status.ActiveInstance)
	}
	deployment := runtimePoolTestDeployment(t, f.r, f.pool.Namespace, runtimePoolResourceName(f.pool.Namespace, f.pool.Name))
	if got := ptr.Deref(deployment.Spec.Replicas, -1); got != 1 {
		t.Fatalf("new template replicas = %d, want 1", got)
	}
	if got := runtimePoolLiteralEnvironment(deployment.Spec.Template.Spec.Containers[0].Env)["ORKA_ACP_CONTROLLER_EPOCH"]; got != strconv.FormatInt(epoch, 10) {
		t.Fatalf("new Deployment controller epoch = %q, want %d", got, epoch)
	}
	runtimePoolTestAssertNoBootEvidence(t, f)
}

// The never-enrolled path needs both the witness and the retention barrier to
// be absent. Either half of the barrier, or a reserved but uncommitted witness,
// shows enrollment reached retention, so the drain keeps failing closed and
// writes no cleanup receipt.
func TestRuntimePoolBootUnenrolledDrainRequiresAbsentEnrollmentEvidence(t *testing.T) {
	patchPod := func(t *testing.T, f *nativePoolBootFixture, mutate func(*corev1.Pod)) {
		t.Helper()
		pod := &corev1.Pod{}
		if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil {
			t.Fatal(err)
		}
		base := pod.DeepCopy()
		mutate(pod)
		if err := f.r.Patch(t.Context(), pod, client.MergeFrom(base)); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name    string
		mutate  func(*testing.T, *nativePoolBootFixture, *corev1alpha1.RuntimePool)
		wantErr bool
	}{
		{name: "no enrollment evidence", mutate: func(*testing.T, *nativePoolBootFixture, *corev1alpha1.RuntimePool) {}},
		{name: "retention finalizer", wantErr: true, mutate: func(t *testing.T, f *nativePoolBootFixture, _ *corev1alpha1.RuntimePool) {
			patchPod(t, f, func(pod *corev1.Pod) { controllerutil.AddFinalizer(pod, runtimePoolBootPodFinalizer) })
		}},
		{name: "retention annotation", wantErr: true, mutate: func(t *testing.T, f *nativePoolBootFixture, _ *corev1alpha1.RuntimePool) {
			patchPod(t, f, func(pod *corev1.Pod) {
				if pod.Annotations == nil {
					pod.Annotations = map[string]string{}
				}
				pod.Annotations[runtimePoolBootRetentionAnnotation] = store.CanonicalBytesDigest([]byte("other-boot"))
			})
		}},
		{name: "pending witness", wantErr: true, mutate: func(t *testing.T, f *nativePoolBootFixture, pool *corev1alpha1.RuntimePool) {
			fence, err := f.r.Epochs.CurrentFence(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.control.ReserveExternalEffect(t.Context(), store.ReserveExternalEffectRequest{
				Identity:      agentRuntimeRecoveryIdentity(runtimePoolBootWitnessKind, pool.UID, pool.Namespace, pool.Status.ActiveInstance.RuntimeInstanceID),
				RequestDigest: store.CanonicalBytesDigest([]byte("interrupted-witness")), Fence: fence, CreatedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := runtimePoolTestAdoptUnenrolledBoot(t)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			task := f.task(t, "unenrolled-drain-turn")
			test.mutate(t, f, &pool)
			err := f.r.recordNativeRuntimePoolDrain(t.Context(), &pool, pool.Status.ActiveInstance, f.supervisor.probe.Status)
			if !test.wantErr {
				if err != nil {
					t.Fatalf("never-enrolled boot drain: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("partial enrollment evidence took the never-enrolled drain path")
			}
			f.assertNoReceipt(t, task)
		})
	}
}

// An enrolled boot keeps its retention barrier until retirement is proven, so
// a lost witness must keep its drain failing closed: no receipt, no retirement
// record and no release of the retained Pod.
func TestRuntimePoolBootEnrolledDrainStillRequiresWitness(t *testing.T) {
	f := newNativePoolBootFixture(t, false)
	f.serve(t)
	task := f.task(t, "enrolled-drain-turn")
	w := f.witness(t)
	var effects corev1alpha1.ExternalEffectList
	if err := f.r.List(t.Context(), &effects, client.InNamespace(f.pool.Namespace)); err != nil {
		t.Fatal(err)
	}
	for i := range effects.Items {
		if effects.Items[i].Spec.Kind == runtimePoolBootWitnessKind {
			if err := f.r.Delete(t.Context(), &effects.Items[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	current := runtimePoolTestGetPool(t, f.r, f.pool)
	status := runtimePoolValidProbe(f.pool, &f.pod, "native-boot", true).Status
	if err := f.r.recordNativeRuntimePoolDrain(t.Context(), &current, current.Status.ActiveInstance, status); err == nil {
		t.Fatal("enrolled boot drained without its witness")
	}
	f.assertNoReceipt(t, task)
	pod := &corev1.Pod{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil {
		t.Fatal(err)
	}
	digest, err := runtimePoolBootWitnessDigest(w)
	if err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) || pod.Annotations[runtimePoolBootRetentionAnnotation] != digest {
		t.Fatal("enrolled Pod lost its retention barrier without a witness")
	}
	if retired, err := loadRuntimePoolBootRetirement(t.Context(), f.control, w); err != nil || retired {
		t.Fatalf("enrolled boot retired without its witness: %t %v", retired, err)
	}
}
