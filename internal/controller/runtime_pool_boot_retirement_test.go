package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	storekube "github.com/orka-agents/orka/internal/store/kube"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// A normal API deletion must retain the exact Pod long enough to observe the
// container's terminal lifetime. API absence itself is not retirement proof.
func TestRuntimePoolRetirementUnexpectedPodDeletionRetainsEvidence(t *testing.T) {
	pool := runtimePoolTestObject(1)
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), supervisor, pool)
	_, pod := runtimePoolTestStartServing(t, r, pool, supervisor, "unexpected-loss", "unexpected-loss-uid", "10.0.0.82", "unexpected-loss-boot")
	if err := r.Delete(t.Context(), &pod); err != nil {
		t.Fatal(err)
	}
	retained := &corev1.Pod{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(&pod), retained); err != nil {
		t.Fatalf("unexpected deletion lost the original Pod before exact container retirement could be observed: %v", err)
	}
	if retained.UID != pod.UID || retained.DeletionTimestamp.IsZero() || len(retained.Finalizers) == 0 {
		t.Fatal("unexpected deletion did not retain the exact terminating Pod")
	}
}

// The fake API has no Deployment controller or kubelet. Only explicitly marked
// native fixtures acquire real owner identities, running status and durable
// epoch authority; deletion is NEVER synthesized into termination evidence.
func runtimePoolTestPrepareBootEnrollment(t *testing.T, r *RuntimePoolReconciler, pod *corev1.Pod) {
	t.Helper()
	if r.ControlStore == nil {
		if err := coordinationv1.AddToScheme(r.Scheme); err != nil {
			t.Fatal(err)
		}
		base := r.Client
		_, wrappedClient := base.(client.WithWatch)
		if !wrappedClient {
			var ok bool
			base, ok = r.APIReader.(client.Client)
			if !ok {
				t.Fatal("native fixture lacks a watch-capable authority client")
			}
		}
		c := withControllerEpochLeaseUIDs(t, base)
		if wrappedClient {
			r.Client = c
		}
		r.APIReader = c
		control, err := storekube.New(c, pod.Labels[runtimePoolNamespaceLabel], storekube.WithAPIReader(c))
		if err != nil {
			t.Fatal(err)
		}
		epoch := advanceKubernetesControllerEpoch(t, t.Context(), control, r.ControllerEpoch)
		epochs := readyAgentRuntimeTestEpochManager(epoch.Epoch)
		epochs.Store, epochs.HolderID, epochs.current = control, epoch.HolderID, epoch
		r.ControlStore, r.Epochs = control, epochs
	}
	deployment := runtimePoolTestDeployment(t, r, pod.Namespace, runtimePoolResourceName(pod.Labels[runtimePoolNamespaceLabel], pod.Labels[runtimePoolNameLabel]))
	owner := metav1.GetControllerOf(pod)
	if owner == nil {
		t.Fatal("marked native fixture has no ReplicaSet owner")
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: owner.Name, UID: owner.UID,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "Deployment", Name: deployment.Name, UID: deployment.UID, Controller: new(true)}}},
		Spec: appsv1.ReplicaSetSpec{Selector: deployment.Spec.Selector.DeepCopy(), Template: *deployment.Spec.Template.DeepCopy()},
	}
	if err := r.Create(t.Context(), rs); err != nil {
		t.Fatal(err)
	}
}

func runtimePoolTestSyncBootEpoch(t *testing.T, r *RuntimePoolReconciler) {
	t.Helper()
	if r.Epochs == nil || r.ControlStore == nil {
		return
	}
	current, ok := r.Epochs.Current()
	if !ok || current.Epoch >= r.ControllerEpoch {
		return
	}
	for current.Epoch < r.ControllerEpoch {
		holder := fmt.Sprintf("runtime-pool-test-controller-%d", current.Epoch+1)
		next, err := r.ControlStore.CompareAndSwapControllerEpoch(t.Context(), store.ControllerEpochCAS{
			Name: current.Name, ExpectedEpoch: current.Epoch, ExpectedVersion: current.Version, NewEpoch: current.Epoch + 1,
			HolderID: holder, RequestDigest: controllerEpochDigest(current.Name, holder, current.Version, current.Epoch, current.Epoch+1), UpdatedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		current = *next
	}
	r.Epochs.mu.Lock()
	r.Epochs.current, r.Epochs.HolderID = &current, current.HolderID
	r.Epochs.mu.Unlock()
}

func runtimePoolTestTerminateBootPod(t *testing.T, r *RuntimePoolReconciler, pod *corev1.Pod) {
	t.Helper()
	status := &pod.Status.ContainerStatuses[0]
	if status.State.Running == nil {
		t.Fatal("fixture runtime container is not running")
	}
	status.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ContainerID: status.ContainerID, StartedAt: status.State.Running.StartedAt, FinishedAt: metav1.NewTime(runtimePoolTestNow),
	}}
	status.Ready = false
	pod.Status.Phase = corev1.PodFailed
	if err := r.Status().Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
}

// Old unit fixtures supply bare Pod metadata rather than a real Deployment
// controller/kubelet. Materialize only those spec-less fixtures for NEW native
// templates, before any authenticated observation. Explicitly modeled legacy
// Pods (including diagnostic history) are never rewritten by this helper.
func runtimePoolTestMaterializeBarePods(t *testing.T, r *RuntimePoolReconciler, deployment *appsv1.Deployment) {
	t.Helper()
	pool := &corev1alpha1.RuntimePool{}
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: deployment.Labels[runtimePoolNamespaceLabel], Name: deployment.Labels[runtimePoolNameLabel]}, pool); err != nil {
		t.Fatal(err)
	}
	var pods corev1.PodList
	if err := r.List(t.Context(), &pods, client.InNamespace(deployment.Namespace), client.MatchingLabels{runtimePoolKeyLabel: deployment.Labels[runtimePoolKeyLabel]}); err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if len(pod.Spec.Containers) != 0 || len(pod.OwnerReferences) != 0 || !pod.DeletionTimestamp.IsZero() {
			continue
		}
		fresh := runtimePoolReadyPodForDeployment(pool, deployment, pod.Name, string(pod.UID), pod.Status.PodIP)
		status := pod.Status.DeepCopy()
		if status.Phase == corev1.PodRunning {
			status.ContainerStatuses = fresh.Status.ContainerStatuses
		}
		pod.Spec, pod.OwnerReferences = fresh.Spec, fresh.OwnerReferences
		pod.Labels = mergeStringMap(fresh.Labels, pod.Labels)
		pod.Annotations = mergeStringMap(fresh.Annotations, pod.Annotations)
		if err := r.Update(t.Context(), pod); err != nil {
			t.Fatal(err)
		}
		runtimePoolTestPrepareBootEnrollment(t, r, pod)
		pod.Status = *status
		if err := r.Status().Update(t.Context(), pod); err != nil {
			t.Fatal(err)
		}
	}
}

func runtimePoolTestCompleteDeletedBoot(t *testing.T, r *RuntimePoolReconciler, pool *corev1alpha1.RuntimePool, pod *corev1.Pod) {
	t.Helper()
	retained := &corev1.Pod{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(pod), retained); err != nil {
		t.Fatal(err)
	}
	if retained.DeletionTimestamp.IsZero() || len(retained.Finalizers) == 0 {
		t.Fatal("native Pod was not retained before termination evidence")
	}
	runtimePoolTestTerminateBootPod(t, r, retained)
	if err := r.reconcileNativeRuntimePoolRetirement(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimePoolBootReleasedWitnessDoesNotBlockNewBoot(t *testing.T) {
	f := newNativePoolBootFixture(t, false)
	base := f.r.Client
	failPublication := true
	f.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, delegate client.Client, subresource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
			if pool, ok := object.(*corev1alpha1.RuntimePool); ok && failPublication && pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing {
				return errors.New("injected active instance publication failure")
			}
			return delegate.SubResource(subresource).Patch(ctx, object, patch, options...)
		},
	})
	if _, err := f.r.Reconcile(t.Context(), runtimePoolBootTestRequest(f.pool)); err == nil {
		t.Fatal("fixture did not interrupt active instance publication")
	}
	first := f.witness(t)
	current := runtimePoolTestGetPool(t, f.r, f.pool)
	if current.Status.ActiveInstance != nil {
		t.Fatal("failed publication exposed an active instance")
	}
	failPublication = false
	runtimePoolTestRestartBootContainer(t, f, first)
	// The first boot is retired from its exact last termination, and the second
	// enrolls in the same Pod because the first active pointer never published.
	f.serve(t)
	second := f.witness(t)
	task := f.task(t, "second-boot-turn")
	if first.Fence.RuntimeInstanceID == second.Fence.RuntimeInstanceID {
		t.Fatal("fixture did not enroll a distinct boot")
	}
	fence, err := f.r.runtimePoolRetirementFence(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Force the historical-first order, independently of ExternalEffect names.
	if err := f.r.releaseRuntimePoolBootPod(t.Context(), first, []runtimePoolBootWitness{first, second}, fence); err != nil {
		t.Fatalf("already-released history blocked the later retention owner: %v", err)
	}
	f.serve(t)
	pod := &corev1.Pod{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil {
		t.Fatal(err)
	}
	digest, err := runtimePoolBootWitnessDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) || pod.Annotations[runtimePoolBootRetentionAnnotation] != digest {
		t.Fatal("historical release altered the live boot's retention")
	}
	f.assertNoReceipt(t, task)
	f.terminate(t, true)
	runtimePoolReconcile(t, f.r, f.pool)
	if retired, err := verifiedNativeRuntimePoolRetirement(t.Context(), f.control, task, task.UID); err != nil || !retired {
		t.Fatalf("later boot termination was blocked by history: %t %v", retired, err)
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("later retired boot retained its Pod: %v", err)
	}
}

func runtimePoolTestRestartBootContainer(t *testing.T, f *nativePoolBootFixture, first runtimePoolBootWitness) {
	t.Helper()
	pod := &corev1.Pod{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil {
		t.Fatal(err)
	}
	status := &pod.Status.ContainerStatuses[0]
	status.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{
		ContainerID: first.ContainerID, StartedAt: first.StartedAt, FinishedAt: metav1.NewTime(runtimePoolTestNow.Add(-30 * time.Second)),
	}
	status.ContainerID = "containerd://second-native-boot"
	status.RestartCount++
	status.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(runtimePoolTestNow.Add(-15 * time.Second))}}
	if err := f.r.Status().Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	f.pod = *pod.DeepCopy()
	f.supervisor.probe = runtimePoolValidProbe(f.pool, pod, "second-native-boot", false)
}
