package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func runtimePoolEnforcementFixture(t *testing.T) (*RuntimePoolReconciler, *corev1alpha1.RuntimePool, *corev1.Pod, *recordingEnforcementCoordinator) {
	t.Helper()
	pool := runtimePoolTestObject(1)
	pool.Finalizers = []string{runtimePoolFinalizer}
	pool.Status.EnforcementBinding = &corev1alpha1.ContainerEnforcementBinding{
		BindingID: "container:retained", RequirementDigest: "sha256:" + strings.Repeat("a", 64),
		ProviderSetDigest: "sha256:" + strings.Repeat("b", 64),
		NodeName:          "node", NodeUID: "node-uid",
		PodNamespace: pool.Namespace, PodName: "runtime", PodUID: "pod-uid",
		ContainerName: runtimeField, ContainerID: "containerd://old",
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: pool.Namespace, Name: "runtime", UID: "pod-uid"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: runtimeField, ContainerID: "containerd://old",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool)
	coordinator := &recordingEnforcementCoordinator{}
	r.Enforcement = coordinator
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pool), pool))
	return r, pool, pod, coordinator
}

func TestRuntimePoolEnforcementRetainsLiveBindingFromFreshReader(t *testing.T) {
	r, pool, pod, coordinator := runtimePoolEnforcementFixture(t)
	// The ordinary client has no Pod; only the uncached reader sees it live.
	r.APIReader = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(pool.DeepCopy(), pod).Build()
	pending, err := r.reconcileRuntimePoolEnforcementRetirement(t.Context(), pool, false)
	require.NoError(t, err)
	require.False(t, pending)
	require.Zero(t, coordinator.retireCalls)
	require.NotNil(t, pool.Status.EnforcementBinding)
	pending, err = r.reconcileRuntimePoolEnforcementRetirement(t.Context(), pool, true)
	require.NoError(t, err)
	require.True(t, pending)
	require.Zero(t, coordinator.retireCalls)
}

func TestRuntimePoolEnforcementLookupFailureRetainsIntent(t *testing.T) {
	r, pool, _, coordinator := runtimePoolEnforcementFixture(t)
	lookupErr := errors.New("API unavailable")
	r.APIReader = interceptor.NewClient(fake.NewClientBuilder().WithScheme(r.Scheme).Build(), interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return lookupErr
		},
	})
	_, err := r.reconcileRuntimePoolEnforcementRetirement(t.Context(), pool, false)
	require.ErrorIs(t, err, lookupErr)
	require.Zero(t, coordinator.retireCalls)
	require.NotNil(t, pool.Status.EnforcementBinding)
}

func TestRuntimePoolEnforcementRetirementPreservesConcurrentStatus(t *testing.T) {
	r, pool, _, coordinator := runtimePoolEnforcementFixture(t)
	pending, err := r.reconcileRuntimePoolEnforcementRetirement(t.Context(), pool, false)
	require.NoError(t, err)
	require.True(t, pending)
	require.NotNil(t, pool.Status.EnforcementBinding)
	coordinator.retired = true
	coordinator.beforeRetire = func() {
		current := &corev1alpha1.RuntimePool{}
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pool), current))
		current.Status.Capacity.QueuedTasks = 7
		require.NoError(t, r.Status().Update(t.Context(), current))
	}
	pending, err = r.reconcileRuntimePoolEnforcementRetirement(t.Context(), pool, false)
	require.NoError(t, err)
	require.False(t, pending)
	require.Nil(t, pool.Status.EnforcementBinding)
	require.EqualValues(t, 7, pool.Status.Capacity.QueuedTasks)
}

func TestRuntimePoolEnforcementStaleRetirementCannotEraseNewBinding(t *testing.T) {
	r, pool, _, coordinator := runtimePoolEnforcementFixture(t)
	coordinator.retired = true
	coordinator.beforeRetire = func() {
		current := &corev1alpha1.RuntimePool{}
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pool), current))
		current.Status.EnforcementBinding.BindingID = "container:new"
		require.NoError(t, r.Status().Update(t.Context(), current))
	}
	_, err := r.reconcileRuntimePoolEnforcementRetirement(t.Context(), pool, false)
	require.ErrorIs(t, err, store.ErrConflict)
	current := &corev1alpha1.RuntimePool{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pool), current))
	require.Equal(t, "container:new", current.Status.EnforcementBinding.BindingID)
}

func TestRuntimePoolEnforcementCleanupErrorsPreserveBinding(t *testing.T) {
	for _, failure := range []string{"missing-reader", "missing-provider", "provider-unavailable"} {
		t.Run(failure, func(t *testing.T) {
			r, pool, _, coordinator := runtimePoolEnforcementFixture(t)
			switch failure {
			case "missing-reader":
				r.APIReader = nil
			case "missing-provider":
				r.Enforcement = nil
			case "provider-unavailable":
				coordinator.retireErr = errors.New("provider unavailable")
			}
			_, err := r.reconcileRuntimePoolEnforcementRetirement(t.Context(), pool, false)
			require.Error(t, err)
			require.NotNil(t, runtimePoolTestGetPool(t, r, pool).Status.EnforcementBinding)
		})
	}
}

func TestRuntimePoolEnforcementRetiresOldIncarnationWithoutDeletingReplacement(t *testing.T) {
	r, pool, pod, coordinator := runtimePoolEnforcementFixture(t)
	pod.UID = "replacement-pod"
	pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
	require.NoError(t, r.Create(t.Context(), pod))
	coordinator.retired = true
	pending, err := r.reconcileRuntimePoolEnforcementRetirement(t.Context(), pool, false)
	require.NoError(t, err)
	require.False(t, pending)
	require.Nil(t, pool.Status.EnforcementBinding)
	current := &corev1.Pod{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pod), current))
	require.Equal(t, pod.UID, current.UID)
	require.Nil(t, current.DeletionTimestamp)
}

func TestRuntimePoolEnforcementPendingRetirementBlocksReplacement(t *testing.T) {
	r, pool, _, coordinator := runtimePoolEnforcementFixture(t)
	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	require.NoError(t, err)
	require.NotZero(t, result.RequeueAfter)
	require.Equal(t, 1, coordinator.retireCalls)
	var pods corev1.PodList
	require.NoError(t, r.List(t.Context(), &pods))
	require.Empty(t, pods.Items)
	require.NotNil(t, runtimePoolTestGetPool(t, r, pool).Status.EnforcementBinding)
}

func TestRuntimePoolEnforcementFinalizationWaitsForProvider(t *testing.T) {
	r, pool, _, coordinator := runtimePoolEnforcementFixture(t)
	require.ErrorIs(t, r.removeRuntimePoolFinalizer(t.Context(), pool), store.ErrNotReady)
	require.NoError(t, r.Delete(t.Context(), pool))
	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	require.NoError(t, err)
	require.NotZero(t, result.RequeueAfter)
	require.Contains(t, runtimePoolTestGetPool(t, r, pool).Finalizers, runtimePoolFinalizer)
	coordinator.retired = true
	_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(pool), &corev1alpha1.RuntimePool{})))
}

func TestRuntimePoolEnforcementGuardsEveryExplicitStart(t *testing.T) {
	for _, operation := range []string{"create", "restart", "replace-template", "continue-serving", "stop"} {
		t.Run(operation, func(t *testing.T) {
			r, pool, _, _ := runtimePoolEnforcementFixture(t)
			cfg, err := r.runtimePoolConfig(pool)
			require.NoError(t, err)
			template := r.runtimePoolPodTemplate(pool, cfg, map[string]string{runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel]}, "auth", "provider")
			replicas := int32(1)
			if operation != "create" {
				deployment := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Namespace: cfg.namespace, Name: cfg.baseName, Labels: cloneStringMap(cfg.labels)},
					Spec:       appsv1.DeploymentSpec{Replicas: new(int32(1)), Template: *template.DeepCopy()},
				}
				switch operation {
				case "restart":
					deployment.Spec.Replicas = new(int32(0))
				case "replace-template":
					deployment.Spec.Template.Annotations[runtimePoolTemplateRevisionAnnotation] = "old-revision"
				case "stop":
					replicas = 0
				}
				require.NoError(t, r.Create(t.Context(), deployment))
			}
			_, err = r.ensureRuntimePoolDeployment(t.Context(), pool, cfg, template, replicas, true)
			if operation == "continue-serving" || operation == "stop" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, errRuntimePoolEnforcementRetirementPending)
			}
		})
	}
}

func TestRuntimePoolEnforcementStopAfterSweepCannotRestartRollout(t *testing.T) {
	r, pool, pod, coordinator := runtimePoolEnforcementFixture(t)
	r.APIReader = interceptor.NewClient(fake.NewClientBuilder().WithScheme(r.Scheme).Build(), interceptor.Funcs{
		Get: func(ctx context.Context, _ client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if observed, ok := object.(*corev1.Pod); ok && key == client.ObjectKeyFromObject(pod) {
				pod.DeepCopyInto(observed)
				return nil
			}
			return r.Get(ctx, key, object, opts...)
		},
	})
	cfg, err := r.runtimePoolConfig(pool)
	require.NoError(t, err)
	template := r.runtimePoolPodTemplate(pool, cfg, map[string]string{runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel]}, "auth", "provider")
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: cfg.namespace, Name: cfg.baseName, Labels: cloneStringMap(cfg.labels)},
		Spec:       appsv1.DeploymentSpec{Replicas: new(int32(0)), Template: *template.DeepCopy()},
	}
	deployment.Spec.Template.Annotations[runtimePoolTemplateRevisionAnnotation] = "old-revision"
	require.NoError(t, r.Create(t.Context(), deployment))
	// The initial fresh observation is live. A later list sees no Pod, as
	// when the stop completes between the sweep and rollout reconciliation.
	pending, err := r.reconcileRuntimePoolEnforcementRetirement(t.Context(), pool, false)
	require.NoError(t, err)
	require.False(t, pending)
	pods, err := r.listRuntimePoolPods(t.Context(), cfg)
	require.NoError(t, err)
	require.Empty(t, pods)
	result, err := r.reconcileStoppedRuntimePoolRollout(t.Context(), pool, cfg, pods, template, r.baseRuntimePoolStatus(pool, 0))
	require.NoError(t, err)
	require.NotZero(t, result.RequeueAfter)
	current := &appsv1.Deployment{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(deployment), current))
	require.Zero(t, *current.Spec.Replicas)
	require.Equal(t, "old-revision", current.Spec.Template.Annotations[runtimePoolTemplateRevisionAnnotation])
	require.NotNil(t, runtimePoolTestGetPool(t, r, pool).Status.EnforcementBinding)
	require.Zero(t, coordinator.retireCalls)
}
