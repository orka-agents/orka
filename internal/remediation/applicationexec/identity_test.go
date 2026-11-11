package applicationexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestUncertainCreatesAreNeverReplayedOrAdopted(t *testing.T) {
	f := newFixture(t)
	r := f.newRun()
	require.NoError(t, f.backend.ensureClaim(t.Context(), r))
	creates := 0
	f.createHook = func(ctx context.Context, c client.WithWatch, object client.Object) error {
		if _, ok := object.(*corev1.Namespace); !ok {
			return nil
		}
		creates++
		require.NoError(t, c.Create(ctx, object))
		return apierrors.NewTimeoutError("fixture response lost after commit", 0)
	}
	err := f.backend.ensureNamespace(t.Context(), r, pv.Original)
	require.ErrorIs(t, err, ErrUncertain)
	entry := r.effect(resourceKey(pv.Original, "namespace"))
	require.NotNil(t, entry)
	require.Empty(t, entry.UID)
	require.True(t, entry.Attempted)
	f.createHook = nil
	require.ErrorIs(t, f.backend.ensureNamespace(t.Context(), r, pv.Original), ErrUncertain)
	require.Equal(t, 1, creates)
	require.ErrorIs(t, f.backend.Cancel(t.Context(), r.receipt()), ErrUncertain)
	retained, err := f.backend.boundRun(t.Context(), r.receipt())
	require.NoError(t, err)
	require.True(t, retained.state.Cancelled)
	require.False(t, retained.state.Settled)
	require.ErrorIs(t, EnsureNoActive(t.Context(), f.control, "control"), ErrActive)
}

func TestKnownRejectedCreateDoesNotDeleteForeignObject(t *testing.T) {
	f := newFixture(t)
	r := f.newRun()
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName(r, pv.Original), UID: "foreign-namespace"}}
	// Bypass admission through the fake underlying storage only for this
	// identity-replacement fixture, not through the production backend.
	require.NoError(t, f.target.Create(t.Context(), foreign))
	err := f.backend.ensureNamespace(t.Context(), r, pv.Original)
	require.Error(t, err)
	require.NoError(t, f.backend.Cancel(t.Context(), r.receipt()))
	current := &corev1.Namespace{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKeyFromObject(foreign), current))
	require.Equal(t, foreign.UID, current.UID)
	require.NotContains(t, f.deleted, foreign.UID)
}

func TestTargetNamespaceReplacementCannotAuthorizeCleanup(t *testing.T) {
	f := newFixture(t)
	r := f.newRun()
	require.NoError(t, f.backend.ensureClaim(t.Context(), r))
	require.NoError(t, f.backend.ensureNamespace(t.Context(), r, pv.Original))
	entry := r.effect(resourceKey(pv.Original, "namespace"))
	current := &corev1.Namespace{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKey{Name: entry.Name}, current))
	uid := current.UID
	require.NoError(t, f.target.Delete(t.Context(), current, client.Preconditions{UID: &uid}))
	current.UID, current.ResourceVersion = "replacement-namespace", ""
	current.CreationTimestamp = metav1.Time{}
	require.NoError(t, f.target.Create(t.Context(), current))
	before := len(f.deleted)
	require.ErrorIs(t, f.backend.Cancel(t.Context(), r.receipt()), contract.ErrConflict)
	require.Len(t, f.deleted, before)
	require.NotContains(t, f.deleted, types.UID("replacement-namespace"))
}

func TestJobReplacementAndAmbiguousPodOwnersAreRejected(t *testing.T) {
	for _, which := range []string{"job", "pod-owner"} {
		t.Run(which, func(t *testing.T) {
			f := newFixture(t)
			f.autoPods = false
			r := f.newRun()
			require.NoError(t, f.backend.ensureClaim(t.Context(), r))
			require.NoError(t, f.backend.ensureSideInfrastructure(t.Context(), r, pv.Original, false))
			input := pv.ApplicationBoundaryInput{Version: pv.ApplicationProtocolVersion, ID: "identity-probe", TaskUID: string(r.state.Task.UID),
				Target: pv.ApplicationTarget{ClusterUID: r.state.TargetUID, Namespace: namespaceName(r, pv.Original),
					NamespaceUID: string(r.effect(resourceKey(pv.Original, "namespace")).UID)},
				CanaryHost: f.config.CanaryHost, CanaryPort: f.config.CanaryPort}
			job, err := f.backend.ensureJob(t.Context(), r, pv.Original, "probe", input, jobOptions{
				role: "subject", mode: "boundary", image: f.config.WorkerImage, taskUID: string(r.state.Task.UID), serviceAccount: "subject", deadline: 30})
			require.NoError(t, err)
			if which == "job" {
				uid := job.UID
				require.NoError(t, f.target.Delete(t.Context(), job, client.Preconditions{UID: &uid}))
				job.UID, job.ResourceVersion = "replacement-job", ""
				job.CreationTimestamp = metav1.Time{}
				require.NoError(t, f.target.Create(t.Context(), job))
			} else {
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: "foreign-pod", UID: "foreign-pod",
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: job.Name, UID: job.UID, Controller: new(true)}}}}
				require.NoError(t, f.target.Create(t.Context(), pod))
			}
			require.ErrorIs(t, f.backend.Cancel(t.Context(), r.receipt()), contract.ErrConflict)
			require.NotContains(t, f.deleted, types.UID("replacement-job"))
			require.NotContains(t, f.deleted, types.UID("foreign-pod"))
		})
	}
}

func TestCleanupFindsExactOrphanPodsWithoutLabels(t *testing.T) {
	f := newFixture(t)
	f.autoPods = false
	r := f.newRun()
	require.NoError(t, f.backend.ensureClaim(t.Context(), r))
	require.NoError(t, f.backend.ensureSideInfrastructure(t.Context(), r, pv.Original, false))
	job, err := f.backend.ensureJob(t.Context(), r, pv.Original, "orphan", struct{}{}, jobOptions{
		role: "build", mode: "build", image: f.config.ToolchainImage, taskUID: string(r.state.Task.UID), serviceAccount: "worker", deadline: 30})
	require.NoError(t, err)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unlabelled-owned-pod", Namespace: job.Namespace, UID: "owned-pod",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: new(true)}}}}
	require.NoError(t, f.target.Create(t.Context(), pod))
	uid := job.UID
	require.NoError(t, f.target.Delete(t.Context(), job, client.Preconditions{UID: &uid}))
	require.NoError(t, f.backend.Cancel(t.Context(), r.receipt()))
	require.Contains(t, f.deleted, pod.UID)
	retained, err := f.backend.boundRun(t.Context(), r.receipt())
	require.NoError(t, err)
	require.NotNil(t, retained.effect("cleanup-pod/"+string(pod.UID)))
	require.True(t, retained.effect("cleanup-pod/"+string(pod.UID)).Deleted)
}

func TestInheritedOrForeignRBACCannotWidenTestIdentities(t *testing.T) {
	for _, which := range []string{"cluster-write", "foreign-namespace", "owned-role-drift"} {
		t.Run(which, func(t *testing.T) {
			f := newFixture(t)
			r := f.newRun()
			require.NoError(t, f.backend.ensureClaim(t.Context(), r))
			require.NoError(t, f.backend.ensureSideInfrastructure(t.Context(), r, pv.Original, false))
			namespace := namespaceName(r, pv.Original)
			switch which {
			case "cluster-write":
				role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "unapproved", UID: "unapproved-role"},
					Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"create"}}}}
				require.NoError(t, f.target.Create(t.Context(), role))
				require.NoError(t, f.target.Create(t.Context(), &rbacv1.ClusterRoleBinding{
					ObjectMeta: metav1.ObjectMeta{Name: "unapproved", UID: "unapproved-binding"},
					RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
					Subjects:   []rbacv1.Subject{{Kind: "Group", Name: "system:serviceaccounts:" + namespace}}}))
			case "foreign-namespace":
				require.NoError(t, f.target.Create(t.Context(), &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "kube-system"},
					RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "edit"},
					Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: namespace, Name: "orka-subject"}}}))
			case "owned-role-drift":
				role := &rbacv1.Role{}
				require.NoError(t, f.target.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: "orka-subject"}, role))
				role.Rules = []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"create"}}}
				require.NoError(t, f.target.Update(t.Context(), role))
			}
			err := f.backend.ensureSideInfrastructure(t.Context(), r, pv.Original, false)
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrIsolation) || errors.Is(err, contract.ErrConflict))
			// Revocation/drift prevents new execution, not deleting exact
			// resources already authorized by the retained run.
			require.NoError(t, f.backend.Cancel(t.Context(), r.receipt()))
		})
	}
}

func TestStateRequestMutationAndBackendRemovalFailClosed(t *testing.T) {
	f := newFixture(t)
	r := f.newRun()
	r.state.Request.Source.Tree = "modified"
	content, err := json.Marshal(r.state)
	require.NoError(t, err)
	object := r.object.DeepCopy()
	object.Data[stateKey] = string(content)
	_, err = decodeRun(object, "control")
	require.ErrorIs(t, err, contract.ErrConflict)
	require.ErrorIs(t, EnsureNoActive(t.Context(), f.control, ""), ErrActive)
	require.NoError(t, f.backend.Cancel(t.Context(), r.receipt()))
	require.NoError(t, EnsureNoActive(t.Context(), f.control, ""))
}

func TestNestedAPIErrorsRetainClassificationWithoutPayload(t *testing.T) {
	underlying := fmt.Errorf("private response body: %w", apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "value", errors.New("private")))
	err := safeError(contract.ErrUnavailable, underlying)
	require.True(t, apierrors.IsForbidden(err))
	require.Equal(t, contract.ErrUnavailable.Error(), err.Error())
}
