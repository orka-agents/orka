package controller

import (
	"context"
	"errors"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func nativeActorPoolIdentityHarness(t *testing.T) (*SubstrateActorPoolReconciler, *corev1alpha1.SubstrateActorPool, *recordingSubstratePoolExecutor) {
	t.Helper()
	scheme := newSubstrateActorPoolTestScheme(t)
	pool := &corev1alpha1.SubstrateActorPool{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "native-pool", UID: "pool-uid", Generation: 1},
		Spec: corev1alpha1.SubstrateActorPoolSpec{
			TemplateRef: corev1alpha1.WorkspaceTemplateReference{Namespace: "ate-demo", Name: "native-template"}, PrecreateActors: true,
		},
	}
	executor := &recordingSubstratePoolExecutor{}
	r := &SubstrateActorPoolReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(pool).WithObjects(pool).Build(),
		Scheme: scheme, SubstrateEnabled: true,
		SubstrateTemplateValidator: func(_ context.Context, request *ExecutionWorkspaceRequest) error {
			request.TemplateUID = "original-native-template-uid"
			return nil
		},
		SubstrateExecutorFactory: func(SubstrateConfig) (SubstratePoolExecutor, error) { return executor, nil },
	}
	return r, pool, executor
}

func TestSubstrateActorPoolPinsTemplateWhileEmptyAcrossRestart(t *testing.T) {
	r, pool, executor := nativeActorPoolIdentityHarness(t)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}
	for range 2 {
		_, err := r.Reconcile(t.Context(), req)
		require.NoError(t, err)
	}
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, pool))
	require.Equal(t, "original-native-template-uid", pool.Status.TemplateUID)
	require.Equal(t, corev1alpha1.SubstrateActorPoolPhaseReady, pool.Status.Phase)
	require.Zero(t, executor.convergeTarget)

	// Reconstruct the controller after the native template is replaced under
	// the same name. No actors remain from which to infer the old identity.
	restarted := *r
	restarted.SubstrateTemplateValidator = func(_ context.Context, request *ExecutionWorkspaceRequest) error {
		request.TemplateUID = "replacement-native-template-uid"
		return nil
	}
	r = &restarted
	for _, target := range []int32{0, 2} {
		*executor = recordingSubstratePoolExecutor{}
		pool.Spec.TargetActors = target
		pool.Generation++
		require.NoError(t, r.Update(t.Context(), pool))
		_, err := r.Reconcile(t.Context(), req)
		require.NoError(t, err)
		require.NoError(t, r.Get(t.Context(), req.NamespacedName, pool))
		require.Equal(t, "original-native-template-uid", pool.Status.TemplateUID)
		require.Equal(t, corev1alpha1.SubstrateActorPoolPhaseFailed, pool.Status.Phase)
		require.Contains(t, pool.Status.Message, "ActorTemplate was replaced")
		require.False(t, executor.convergeCalled)
		require.False(t, executor.pruneCalled)
	}
	// Rejecting a replacement must not prevent cleanup of the old pool.
	require.NoError(t, r.Delete(t.Context(), pool))
	_, err := r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	require.True(t, executor.convergeCalled)
	require.Zero(t, executor.convergeTarget)
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), req.NamespacedName, pool)))
}

func TestSubstrateActorPoolRequiresDurableTemplateIdentity(t *testing.T) {
	for _, failure := range []string{"missing UID", "status write failure"} {
		t.Run(failure, func(t *testing.T) {
			r, pool, executor := nativeActorPoolIdentityHarness(t)
			originalClient, originalValidator := r.Client, r.SubstrateTemplateValidator
			var wantErr error
			if failure == "missing UID" {
				r.SubstrateTemplateValidator = func(context.Context, *ExecutionWorkspaceRequest) error { return nil }
			} else {
				wantErr = errors.New("injected template identity write failure")
				r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
					SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
						return wantErr
					},
				})
			}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}
			_, err := r.Reconcile(t.Context(), req)
			require.ErrorIs(t, err, wantErr)
			require.False(t, executor.convergeCalled)
			require.False(t, executor.pruneCalled)
			require.NoError(t, r.Get(t.Context(), req.NamespacedName, pool))
			require.Empty(t, pool.Status.TemplateUID)
			r.Client, r.SubstrateTemplateValidator = originalClient, originalValidator
			for range 2 {
				_, err := r.Reconcile(t.Context(), req)
				require.NoError(t, err)
			}
			require.NoError(t, r.Get(t.Context(), req.NamespacedName, pool))
			require.Equal(t, "original-native-template-uid", pool.Status.TemplateUID)
			require.True(t, executor.convergeCalled)
		})
	}
}
