// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"reflect"
	"slices"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestExternalRuntimePoolSpoofedPolicyCannotWidenFrozenNetworkIntent(t *testing.T) {
	for _, native := range []bool{false, true} {
		name := "Pod"
		if native {
			name = "native process"
		}
		t.Run(name, func(t *testing.T) {
			f := newExternalRuntimePoolFixture(t)
			f.r.RuntimeNamespace = "external-runtimes"
			if native {
				f.advertiseNativeProcess(t)
			}
			spoofed := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: f.r.RuntimeNamespace, Name: "spoofed-pool-allow-all",
				Labels: map[string]string{runtimePoolUIDLabel: string(f.pool.UID)}},
				Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{runtimePoolKeyLabel: runtimePoolKey(f.pool.Namespace, f.pool.Name)}},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
					Ingress:     []networkingv1.NetworkPolicyIngressRule{{}}, Egress: []networkingv1.NetworkPolicyEgressRule{{}}}}
			if err := f.r.Create(t.Context(), spoofed); err != nil {
				t.Fatal(err)
			}
			w := f.publish(t)
			network := w.Spec.Workload.Runtime.NetworkPolicy
			if slices.ContainsFunc(network.Ingress, func(rule networkingv1.NetworkPolicyIngressRule) bool {
				return len(rule.From) == 0 && len(rule.Ports) == 0
			}) ||
				slices.ContainsFunc(network.Egress, func(rule networkingv1.NetworkPolicyEgressRule) bool { return len(rule.To) == 0 && len(rule.Ports) == 0 }) {
				t.Fatal("public pool UID label admitted spoofed allow-all permissions")
			}
			assertExternalRuntimePoolMaterializationNetwork(t, w.Spec.Workload, native)
			if native {
				return // Native provider separately enforces these admitted rules.
			}
			frozen := w.Spec.Workload.DeepCopy()
			f.materialize(t)
			runtimePoolReconcile(t, f.r, f.pool)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			if f.seeds != 0 || f.supervisor.probeCalls != 0 || pool.Status.ActiveInstance != nil || pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
				t.Fatal("a selecting spoofed policy received credentials or runtime admission")
			}
			if !reflect.DeepEqual(f.currentWorkspace(t).Spec.Workload, frozen) {
				t.Fatal("observed spoofed policy changed the frozen workload after publication")
			}
		})
	}
}

func TestExternalRuntimePoolFrozenNetworkPreservesInstalledCorePoliciesAcrossNamespaces(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	f.r.RuntimeNamespace = "external-runtimes"
	w, pod := f.materialize(t)
	if w.Namespace == pod.Namespace {
		t.Fatal("fixture does not separate runtime placement from the pool namespace")
	}
	policies := &networkingv1.NetworkPolicyList{}
	if err := f.r.List(t.Context(), policies, client.InNamespace(pod.Namespace), client.MatchingLabels{runtimePoolUIDLabel: string(f.pool.UID)}); err != nil {
		t.Fatal(err)
	}
	if len(policies.Items) != 5 {
		t.Fatalf("Core installed %d runtime policies, want 5", len(policies.Items))
	}
	network := w.Spec.Workload.Runtime.NetworkPolicy
	for _, policy := range policies.Items {
		for _, rule := range policy.Spec.Ingress {
			if !slices.ContainsFunc(network.Ingress, func(admitted networkingv1.NetworkPolicyIngressRule) bool { return reflect.DeepEqual(admitted, rule) }) {
				t.Fatal("installed Core ingress was lost from frozen network intent")
			}
		}
		for _, rule := range policy.Spec.Egress {
			if !slices.ContainsFunc(network.Egress, func(admitted networkingv1.NetworkPolicyEgressRule) bool { return reflect.DeepEqual(admitted, rule) }) {
				t.Fatal("installed Core egress was lost from frozen network intent")
			}
		}
	}
	runtimePoolReconcile(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if f.seeds != 1 || f.supervisor.probeCalls != 1 || pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting {
		t.Fatal("legitimate Core policies prevented attested startup across namespaces")
	}
}
