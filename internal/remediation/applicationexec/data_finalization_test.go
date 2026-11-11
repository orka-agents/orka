package applicationexec

import (
	"testing"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func finalizingDataFixture(t *testing.T) (*fixture, *run, *networkingv1.Ingress, *batchv1.Job) {
	t.Helper()
	f := newFixture(t)
	f.backend.config.CleanupTimeout = 8 * time.Second
	r := f.newRun()
	require.NoError(t, f.backend.ensureClaim(t.Context(), r))
	require.NoError(t, f.backend.ensureSideInfrastructure(t.Context(), r, pv.Original, true))
	require.NoError(t, f.backend.ensureFixtures(t.Context(), r, pv.Original, "resource", r.state.Request.Checks.Application.Resources))
	job, err := f.backend.ensureJob(t.Context(), r, pv.Original, "subject", struct{}{}, jobOptions{
		role: "subject", mode: "subject", image: f.backend.config.RuntimeImage, taskUID: string(r.state.Task.UID),
		serviceAccount: "subject"})
	require.NoError(t, err)
	object := &networkingv1.Ingress{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKey{Namespace: namespaceName(r, pv.Original), Name: "case"}, object))
	object.Finalizers = []string{"controllers.example.test/data-finalization"}
	require.NoError(t, f.target.Update(t.Context(), object))
	r.state.Cancelled = true
	require.NoError(t, f.backend.save(t.Context(), r))
	return f, r, object, job
}

func TestSandboxDataFinalizesBeforeItsSubjectStops(t *testing.T) {
	f, r, object, subject := finalizingDataFixture(t)
	done, err := f.backend.cleanup(t.Context(), r)
	require.NoError(t, err)
	require.False(t, done)
	current := &networkingv1.Ingress{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKeyFromObject(object), current))
	require.False(t, current.DeletionTimestamp.IsZero())
	require.Equal(t, object.Finalizers, current.Finalizers)
	running := &batchv1.Job{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKeyFromObject(subject), running))
	require.True(t, running.DeletionTimestamp.IsZero(), "the responsible application must remain able to finalize data")
	// Simulate that controller's bounded finalization, not backend stripping.
	current.Finalizers = nil
	require.NoError(t, f.target.Update(t.Context(), current))
	require.NoError(t, f.backend.Cancel(t.Context(), r.receipt()))
	retained, err := f.backend.boundRun(t.Context(), r.receipt())
	require.NoError(t, err)
	require.True(t, retained.state.Settled)
	require.NoError(t, EnsureNoActive(t.Context(), f.control, "control"))
}

func TestBlockedDataFinalizationStopsExecutionButNeverFakesSettlement(t *testing.T) {
	f, r, object, subject := finalizingDataFixture(t)
	_, err := f.backend.cleanup(t.Context(), r)
	require.NoError(t, err)
	expired := time.Now().UTC().Add(-time.Millisecond)
	r.state.DataFinalizationDeadline = &expired
	require.NoError(t, f.backend.save(t.Context(), r))
	restarted, err := f.backend.load(t.Context(), r.object.Name)
	require.NoError(t, err)
	require.Equal(t, expired, *restarted.state.DataFinalizationDeadline)
	var cleanupErr error
	for range 8 {
		done, err := f.backend.cleanup(t.Context(), restarted)
		require.False(t, done)
		if err != nil {
			cleanupErr = err
			break
		}
	}
	require.ErrorIs(t, cleanupErr, errDataFinalizationBlocked)
	require.True(t, restarted.state.DataFinalizationBlocked)
	require.False(t, restarted.state.Settled)
	current := &networkingv1.Ingress{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKeyFromObject(object), current))
	require.Equal(t, object.UID, current.UID)
	require.Equal(t, object.Finalizers, current.Finalizers)
	require.Error(t, f.target.Get(t.Context(), client.ObjectKeyFromObject(subject), &batchv1.Job{}))
	require.ErrorIs(t, EnsureNoActive(t.Context(), f.control, "control"), ErrActive)
	require.NotNil(t, restarted.effect("target-claim"))
	require.False(t, restarted.effect("target-claim").Deleted)
}

func TestDataFinalizationRejectsReplacedUIDAndNeverTouchesStorageFinalizers(t *testing.T) {
	f, r, object, subject := finalizingDataFixture(t)
	pvc := &corev1.PersistentVolumeClaim{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKey{Namespace: object.Namespace, Name: "orka-source"}, pvc))
	pvc.Finalizers = []string{"kubernetes.io/pvc-protection", "storage.example.test/protect"}
	require.NoError(t, f.target.Update(t.Context(), pvc))
	object.Finalizers = nil
	require.NoError(t, f.target.Update(t.Context(), object))
	uid := object.UID
	require.NoError(t, f.target.Delete(t.Context(), object, client.Preconditions{UID: &uid}))
	object.UID, object.ResourceVersion = "unowned-replacement", ""
	object.CreationTimestamp = metav1.Time{}
	object.Finalizers = []string{"operator.example.test/protect"}
	require.NoError(t, f.target.Create(t.Context(), object))
	require.ErrorIs(t, f.backend.Cancel(t.Context(), r.receipt()), contract.ErrConflict)
	require.True(t, apierrors.IsNotFound(f.target.Get(t.Context(), client.ObjectKeyFromObject(subject), &batchv1.Job{})),
		"conflicting data must not keep the independently owned subject running")
	pods := &corev1.PodList{}
	require.NoError(t, f.target.List(t.Context(), pods, client.InNamespace(object.Namespace)))
	require.Empty(t, pods.Items)
	roles := &rbacv1.RoleList{}
	bindings := &rbacv1.RoleBindingList{}
	require.NoError(t, f.target.List(t.Context(), roles, client.InNamespace(object.Namespace)))
	require.NoError(t, f.target.List(t.Context(), bindings, client.InNamespace(object.Namespace)))
	require.Empty(t, roles.Items)
	require.Empty(t, bindings.Items)
	retained, err := f.backend.boundRun(t.Context(), r.receipt())
	require.NoError(t, err)
	require.False(t, retained.state.Settled)
	require.NotNil(t, retained.state.DataFinalizationDeadline)
	require.False(t, retained.effect("target-claim").Deleted)
	retainedPVC := &corev1.PersistentVolumeClaim{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKeyFromObject(pvc), retainedPVC))
	require.Equal(t, pvc.Finalizers, retainedPVC.Finalizers)
	require.True(t, retainedPVC.DeletionTimestamp.IsZero())
	require.NotContains(t, f.deleted, object.UID)
	for _, kind := range []string{"PersistentVolume", "PersistentVolumeClaim", "Namespace", "ServiceAccount", "Secret", "Pod"} {
		require.False(t, sandboxDataKind("v1", kind, nil))
	}
}
