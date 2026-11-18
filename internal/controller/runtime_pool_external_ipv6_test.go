// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"reflect"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestExternalRuntimePoolAttestsExactIPv6PodEndpoint(t *testing.T) {
	for _, test := range []struct {
		name, endpoint string
		serve          bool
	}{
		{name: "exact bracketed IPv6 endpoint", endpoint: "http://[fd00::71]:8080", serve: true},
		{name: "another IPv6 authority", endpoint: "http://[fd00::72]:8080"},
		{name: "another bootstrap port", endpoint: "http://[fd00::71]:8081"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newExternalRuntimePoolFixture(t)
			w, pod := f.materialize(t)
			frozen := w.Spec.Workload.DeepCopy()
			pod.Status.PodIP = "fd00::71"
			if err := f.r.Status().Update(t.Context(), &pod); err != nil {
				t.Fatal(err)
			}
			w.Status.Allocation.Startup.Endpoint = test.endpoint
			if err := f.r.Status().Update(t.Context(), w); err != nil {
				t.Fatal(err)
			}
			f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "external-boot", false)
			runtimePoolReconcile(t, f.r, f.pool)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			if test.serve {
				if f.seeds != 1 || f.supervisor.probeCalls != 1 || pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing ||
					pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting || pool.Status.ActiveInstance == nil || pool.Status.ActiveInstance.PodAddress != pod.Status.PodIP {
					t.Fatal("exact IPv6 Pod endpoint prevented credential binding and authenticated startup")
				}
			} else if f.seeds != 0 || f.supervisor.probeCalls != 0 || pool.Status.ActiveInstance != nil || pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
				t.Fatal("wrong IPv6 authority received credentials or runtime admission")
			}
			if !reflect.DeepEqual(f.currentWorkspace(t).Spec.Workload, frozen) {
				t.Fatal("endpoint attestation changed frozen runtime intent")
			}
		})
	}
}
