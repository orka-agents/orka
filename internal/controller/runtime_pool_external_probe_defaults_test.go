// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"reflect"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func TestExternalRuntimePoolAttestsDefaultedProbeProtocol(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	workspace, pod := f.materialize(t)
	frozen := workspace.Spec.Workload.DeepCopy()
	container := &pod.Spec.Containers[0]
	for _, probe := range []*corev1.Probe{container.LivenessProbe, container.ReadinessProbe, container.StartupProbe} {
		// Kubernetes writes HTTP1 when H2CContainerProbe is enabled. Core's
		// published template omits Protocol but already means HTTP/1.1.
		probe.HTTPGet.Protocol = new(corev1.HTTPProtocolHTTP1)
	}
	if err := f.r.Update(t.Context(), &pod); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	status := runtimePoolTestGetPool(t, f.r, f.pool).Status
	if status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting ||
		status.ActiveInstance == nil || status.ActiveInstance.PodUID != string(pod.UID) || f.seeds != 1 || f.supervisor.probeCalls != 1 {
		t.Fatal("API-defaulted probes prevented exact-instance authenticated startup")
	}
	if !reflect.DeepEqual(frozen, f.currentWorkspace(t).Spec.Workload) {
		t.Fatal("Pod comparison changed the frozen request")
	}
}

func TestExternalRuntimePoolRejectsProbeDriftBeforeCredentials(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*corev1.Container)
	}{
		{"protocol", func(c *corev1.Container) { c.StartupProbe.HTTPGet.Protocol = new(corev1.HTTPProtocolHTTP2) }},
		{"timeout", func(c *corev1.Container) { c.ReadinessProbe.TimeoutSeconds++ }},
		{"period", func(c *corev1.Container) { c.ReadinessProbe.PeriodSeconds++ }},
		{"failure threshold", func(c *corev1.Container) { c.StartupProbe.FailureThreshold++ }},
		{"path", func(c *corev1.Container) { c.LivenessProbe.HTTPGet.Path = "/unadmitted" }},
		{"scheme", func(c *corev1.Container) { c.StartupProbe.HTTPGet.Scheme = corev1.URISchemeHTTPS }},
		{"handler", func(c *corev1.Container) {
			c.LivenessProbe.HTTPGet = nil
			c.LivenessProbe.Exec = &corev1.ExecAction{Command: []string{"/unadmitted"}}
		}},
		{"missing probe", func(c *corev1.Container) { c.ReadinessProbe = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newExternalRuntimePoolFixture(t)
			workspace, pod := f.materialize(t)
			frozen := workspace.Spec.Workload.DeepCopy()
			test.mutate(&pod.Spec.Containers[0])
			if err := f.r.Update(t.Context(), &pod); err != nil {
				t.Fatal(err)
			}
			runtimePoolReconcile(t, f.r, f.pool)
			status := runtimePoolTestGetPool(t, f.r, f.pool).Status
			if status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed || status.ActiveInstance != nil || f.seeds != 0 || f.supervisor.probeCalls != 0 {
				t.Fatal("unadmitted probe drift received runtime credentials or admission")
			}
			if !reflect.DeepEqual(frozen, f.currentWorkspace(t).Spec.Workload) {
				t.Fatal("failed Pod comparison changed the frozen request")
			}
		})
	}
}
