// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"bytes"
	"reflect"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func externalRuntimePoolVolumeNamespaceFixture(t *testing.T) (*externalRuntimePoolFixture, corev1.Pod, workspacev1alpha1.PersistentVolumeEvidence) {
	t.Helper()
	f := newExternalRuntimePoolFixture(t)
	f.r.RuntimeNamespace = "external-runtimes"
	w := f.currentWorkspace(t)
	w.Spec.Lifecycle.AllowedOnDetach = append(w.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend)
	if err := f.r.Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	w, pod := f.materialize(t)
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: runtimePoolDurableWorkspaceVolume,
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "workspace-data"}}})
	if err := f.r.Update(t.Context(), &pod); err != nil {
		t.Fatal(err)
	}
	var foreign workspacev1alpha1.PersistentVolumeEvidence
	for _, namespace := range []string{pod.Namespace, w.Namespace} {
		prefix := "runtime"
		if namespace == w.Namespace {
			prefix = "release"
		}
		claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "workspace-data", UID: types.UID(prefix + "-claim-uid")},
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: prefix + "-pv", AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
		pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: claim.Spec.VolumeName, UID: types.UID(prefix + "-pv-uid")},
			Spec: corev1.PersistentVolumeSpec{Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
				ClaimRef: &corev1.ObjectReference{Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID}},
			Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound}}
		for _, object := range []client.Object{claim, pv} {
			if err := f.r.Create(t.Context(), object); err != nil {
				t.Fatal(err)
			}
		}
		volume := workspacev1alpha1.PersistentVolumeEvidence{VolumeName: runtimePoolDurableWorkspaceVolume,
			Claim:  workspacev1alpha1.PodReference{Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID},
			Volume: workspacev1alpha1.ObjectIdentityReference{Name: pv.Name, UID: pv.UID}}
		if namespace == pod.Namespace {
			w.Status.Allocation.Startup.PersistentVolumes = []workspacev1alpha1.PersistentVolumeEvidence{volume}
		} else {
			foreign = volume
		}
	}
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	return f, pod, foreign
}

func TestExternalRuntimePoolRejectsCrossNamespaceDurableClaimBeforeBootstrap(t *testing.T) {
	f, _, foreign := externalRuntimePoolVolumeNamespaceFixture(t)
	w := f.currentWorkspace(t)
	w.Status.Allocation.Startup.PersistentVolumes = []workspacev1alpha1.PersistentVolumeEvidence{foreign}
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if f.seeds != 0 || f.supervisor.probeCalls != 0 || pool.Status.ActiveInstance != nil || pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed ||
		pool.Annotations[externalRuntimeEvidenceAnnotation] != "" || pool.Annotations[runtimePoolBootstrapInstanceBindingAnnotation] != "" {
		t.Fatal("same-name claim from another namespace received credentials or an independently trusted instance binding")
	}
}

func TestExternalRuntimePoolRejectsCrossNamespaceDurableClaimDuringDrain(t *testing.T) {
	f, pod, foreign := externalRuntimePoolVolumeNamespaceFixture(t)
	runtimePoolReconcile(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing {
		t.Fatalf("actual runtime claim did not serve: %s", pool.Status.Message)
	}
	active := pool.Status.ActiveInstance.DeepCopy()
	auth, err := f.r.runtimePoolPodTemplateAuthSecret(t.Context(), &pool, pod.Namespace, pod.Spec)
	if err != nil {
		t.Fatal(err)
	}
	probeCalls, seeds := f.supervisor.probeCalls, f.seeds
	w := f.currentWorkspace(t)
	w.Status.Allocation.Startup.PersistentVolumes = []workspacev1alpha1.PersistentVolumeEvidence{foreign}
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	pool.Spec.DesiredReplicas = 0
	if err := f.r.Update(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	if f.seeds != seeds || f.supervisor.probeCalls != probeCalls || f.supervisor.drainCalls != 0 ||
		f.currentWorkspace(t).Spec.Retirement != nil || pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed || !reflect.DeepEqual(pool.Status.ActiveInstance, active) {
		t.Fatal("same-name claim from another namespace was trusted for authenticated runtime drain or retirement")
	}
	keptAuth := &corev1.Secret{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(auth), keptAuth); err != nil || !bytes.Equal(keptAuth.Data[runtimePoolControllerTokenKey], auth.Data[runtimePoolControllerTokenKey]) {
		t.Fatal("invalid storage evidence removed or rotated the active runtime credentials")
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&pod), &corev1.Pod{}); err != nil {
		t.Fatalf("invalid storage evidence removed provider compute: %v", err)
	}
}

func TestExternalRuntimePoolAcceptsDurableClaimInActualRuntimeNamespace(t *testing.T) {
	f, pod, _ := externalRuntimePoolVolumeNamespaceFixture(t)
	w := f.currentWorkspace(t)
	if w.Namespace == pod.Namespace || w.Status.Allocation.Startup.PersistentVolumes[0].Claim.Namespace != pod.Namespace {
		t.Fatal("fixture does not exercise an actual runtime claim outside the release namespace")
	}
	runtimePoolReconcile(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if f.seeds != 1 || f.supervisor.probeCalls != 1 || pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting {
		t.Fatal("exact runtime namespace claim prevented startup across separate release and runtime namespaces")
	}
	binding, err := externalRuntimeEvidence(&pool)
	if err != nil || binding == nil || binding.Pod.Namespace != pod.Namespace || binding.Pod.UID != pod.UID {
		t.Fatal("serving startup lost its exact runtime namespace and Pod identity")
	}
}
