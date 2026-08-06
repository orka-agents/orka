package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestAgentExecutionOwnershipLockAcquiresCompleteFenceSet(t *testing.T) {
	now := time.Now().UTC()
	client := fake.NewClientset(append(ownershipCurrentControllerObjects(),
		legacyOwnershipLease("legacy-orka", "legacy-uid", "old-controller", now.Add(-time.Minute), 15),
		legacyOwnershipLease("orka-system", "current-uid", "", time.Time{}, 15),
	)...)
	lock := newTestAgentExecutionOwnershipLock(t, client, "")

	if _, _, err := lock.Get(context.Background()); err == nil {
		t.Fatal("missing global Lease unexpectedly returned without an error")
	}
	record := ownershipLeaderRecord(now)
	if err := lock.Create(context.Background(), record); err != nil {
		t.Fatalf("acquire complete fence set: %v", err)
	}

	for _, namespace := range []string{"legacy-orka", "orka-system"} {
		lease, err := client.CoordinationV1().Leases(namespace).Get(
			context.Background(), corev1alpha1.AgentExecutionLegacyLeaseName, metav1.GetOptions{},
		)
		if err != nil {
			t.Fatal(err)
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "controller-current" {
			t.Fatalf("legacy Lease %s holder = %#v", namespace, lease.Spec.HolderIdentity)
		}
	}
	snapshot, ready := lock.Snapshot()
	if !ready {
		t.Fatal("ownership snapshot is not ready after complete acquisition")
	}
	if snapshot.GlobalLease.Name != corev1alpha1.AgentExecutionOwnershipLeaseName || len(snapshot.Legacy) != 2 {
		t.Fatalf("ownership snapshot = %#v", snapshot)
	}
	if err := lock.ReadyzChecker()(nil); err != nil {
		t.Fatalf("ready check after acquisition: %v", err)
	}
}

func TestAgentExecutionOwnershipLockRejectsActiveLegacyHolder(t *testing.T) {
	now := time.Now().UTC()
	client := fake.NewClientset(append(ownershipCurrentControllerObjects(),
		legacyOwnershipLease("orka-system", "legacy-uid", "old-controller", now, 60),
	)...)
	lock := newTestAgentExecutionOwnershipLock(t, client, "")
	err := lock.Create(context.Background(), ownershipLeaderRecord(now))
	if err == nil || !strings.Contains(err.Error(), "has not expired") {
		t.Fatalf("active legacy holder error = %v", err)
	}
	if readyErr := lock.ReadyzChecker()(nil); readyErr == nil {
		t.Fatal("readiness remained open after active legacy holder rejection")
	}
}

func TestAgentExecutionOwnershipLockRejectsNewLegacyLeaseAfterAcquisition(t *testing.T) {
	now := time.Now().UTC()
	client := fake.NewClientset(append(ownershipCurrentControllerObjects(),
		legacyOwnershipLease("orka-system", "legacy-uid", "", time.Time{}, 15),
	)...)
	lock := newTestAgentExecutionOwnershipLock(t, client, "")
	record := ownershipLeaderRecord(now)
	if err := lock.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoordinationV1().Leases("surprise").Create(context.Background(),
		legacyOwnershipLease("surprise", "surprise-uid", "", time.Time{}, 15), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	record.RenewTime = metav1.NewTime(now.Add(time.Second))
	if err := lock.Update(context.Background(), record); err == nil || !strings.Contains(err.Error(), "fence set changed") {
		t.Fatalf("new legacy Lease error = %v", err)
	}
	if _, ready := lock.Snapshot(); ready {
		t.Fatal("snapshot remained ready after new legacy Lease discovery")
	}
}

func TestAgentExecutionOwnershipLockRejectsRecreatedLegacyLease(t *testing.T) {
	now := time.Now().UTC()
	client := fake.NewClientset(append(ownershipCurrentControllerObjects(),
		legacyOwnershipLease("orka-system", "legacy-uid", "", time.Time{}, 15),
	)...)
	lock := newTestAgentExecutionOwnershipLock(t, client, "")
	record := ownershipLeaderRecord(now)
	if err := lock.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := client.CoordinationV1().Leases("orka-system").Delete(
		context.Background(), corev1alpha1.AgentExecutionLegacyLeaseName, metav1.DeleteOptions{},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoordinationV1().Leases("orka-system").Create(context.Background(),
		legacyOwnershipLease("orka-system", "replacement-uid", "", time.Time{}, 15), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	record.RenewTime = metav1.NewTime(now.Add(time.Second))
	if err := lock.Update(context.Background(), record); err == nil || !strings.Contains(err.Error(), "was replaced") {
		t.Fatalf("recreated legacy Lease error = %v", err)
	}
}

func TestAgentExecutionOwnershipLockRejectsElectionDisabledOverlap(t *testing.T) {
	objects := ownershipCurrentControllerObjects()
	one := int32(1)
	objects = append(objects, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "old-controller", UID: "other-deployment"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"control-plane": "controller-manager"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Args: []string{"--leader-elect=false"}}}},
			},
		},
	})
	client := fake.NewClientset(objects...)
	lock := newTestAgentExecutionOwnershipLock(t, client, "")
	_, _, err := lock.Get(context.Background())
	if err == nil || !strings.Contains(err.Error(), "leader election disabled") {
		t.Fatalf("disabled overlap error = %v", err)
	}
}

func TestAgentExecutionOwnershipLockAllowsDisjointWatchScope(t *testing.T) {
	objects := ownershipCurrentControllerObjects()
	one := int32(1)
	objects = append(objects, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "scoped-controller", UID: "other-deployment"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"control-plane": "controller-manager"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Args: []string{
					"--leader-elect=true", "--watch-namespace=tenant-b",
				}}}},
			},
		},
	})
	client := fake.NewClientset(objects...)
	lock := newTestAgentExecutionOwnershipLock(t, client, "tenant-a")
	if _, _, err := lock.Get(context.Background()); err == nil {
		// The global Lease is intentionally absent. Getting this far proves the
		// disjoint Deployment did not fail overlap preflight.
		return
	} else if !strings.Contains(err.Error(), corev1alpha1.AgentExecutionOwnershipLeaseName) &&
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("disjoint scope failed preflight: %v", err)
	}
}

func newTestAgentExecutionOwnershipLock(
	t *testing.T,
	client *fake.Clientset,
	watchNamespace string,
) *AgentExecutionOwnershipLock {
	t.Helper()
	lock, err := NewAgentExecutionOwnershipLock(client, AgentExecutionOwnershipLockConfig{
		Identity:            "controller-current",
		CurrentPodNamespace: "orka-system",
		CurrentPodName:      "controller-pod",
		WatchNamespace:      watchNamespace,
	})
	if err != nil {
		t.Fatal(err)
	}
	return lock
}

func ownershipCurrentControllerObjects() []runtime.Object {
	one := int32(1)
	controller := true
	return []runtime.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "orka-system", Name: "orka", UID: "deployment-current"},
			Spec: appsv1.DeploymentSpec{
				Replicas: &one,
				Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/component": "controller"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "controller", Args: []string{"--leader-elect=true"}}}},
				},
			},
		},
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "orka-system", Name: "orka-rs", UID: "replicaset-current",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "Deployment", Name: "orka", UID: "deployment-current", Controller: &controller,
				}},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "orka-system", Name: "controller-pod", UID: "pod-current",
				Labels: map[string]string{"app.kubernetes.io/component": "controller"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "orka-rs", UID: "replicaset-current", Controller: &controller,
				}},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "controller", Args: []string{"--leader-elect=true"}}}},
		},
	}
}

func legacyOwnershipLease(namespace string, uid types.UID, holder string, renew time.Time, duration int32) *coordinationv1.Lease {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: corev1alpha1.AgentExecutionLegacyLeaseName, UID: uid,
		},
		Spec: coordinationv1.LeaseSpec{LeaseDurationSeconds: &duration},
	}
	if holder != "" {
		lease.Spec.HolderIdentity = &holder
	}
	if !renew.IsZero() {
		lease.Spec.RenewTime = &metav1.MicroTime{Time: renew}
	}
	return lease
}

func ownershipLeaderRecord(now time.Time) resourcelock.LeaderElectionRecord {
	return resourcelock.LeaderElectionRecord{
		HolderIdentity: "controller-current", LeaseDurationSeconds: 15,
		AcquireTime: metav1.NewTime(now), RenewTime: metav1.NewTime(now), LeaderTransitions: 1,
	}
}
