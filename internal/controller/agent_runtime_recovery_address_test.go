package controller

import (
	"net"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAgentRuntimeRecoveryBackendSelectsSecondaryPodAddress(t *testing.T) {
	for _, test := range []struct {
		name        string
		primary     string
		addresses   []corev1.PodIP
		endpoint    string
		addressType discoveryv1.AddressType
		family      corev1.IPFamily
	}{
		{
			name: "secondary_ipv6", primary: "192.0.2.10", endpoint: "2001:db8::10",
			addresses:   []corev1.PodIP{{IP: "192.0.2.10"}, {IP: "2001:db8::10"}},
			addressType: discoveryv1.AddressTypeIPv6, family: corev1.IPv6Protocol,
		},
		{
			name: "secondary_ipv4", primary: "2001:db8::10", endpoint: "192.0.2.10",
			addresses:   []corev1.PodIP{{IP: "2001:db8::10"}, {IP: "192.0.2.10"}},
			addressType: discoveryv1.AddressTypeIPv4, family: corev1.IPv4Protocol,
		},
		{
			name: "legacy_primary", primary: "192.0.2.10", endpoint: "192.0.2.10",
			addressType: discoveryv1.AddressTypeIPv4, family: corev1.IPv4Protocol,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRuntimeRecoveryFixture(t)
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil {
				t.Fatal(err)
			}
			f.pod.Status.PodIP, f.pod.Status.PodIPs = test.primary, test.addresses
			if err := f.r.Status().Update(t.Context(), f.pod); err != nil {
				t.Fatal(err)
			}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.slice), f.slice); err != nil {
				t.Fatal(err)
			}
			f.slice.AddressType = test.addressType
			f.slice.Endpoints[0].Addresses = []string{test.endpoint}
			if err := f.r.Update(t.Context(), f.slice); err != nil {
				t.Fatal(err)
			}
			service := &corev1.Service{}
			if err := f.r.Get(t.Context(), client.ObjectKey{Namespace: f.runtime.Namespace, Name: "runtime"}, service); err != nil {
				t.Fatal(err)
			}
			service.Spec.IPFamilies = []corev1.IPFamily{test.family}
			service.Spec.Ports[0].TargetPort = intstr.FromInt32(*f.slice.Ports[0].Port)
			if err := f.r.Update(t.Context(), service); err != nil {
				t.Fatal(err)
			}

			backend, err := f.r.recoveryBackend(t.Context(), f.runtime)
			if err != nil {
				t.Fatalf("single-family backend using a Pod address was rejected: %v", err)
			}
			wantPin := net.JoinHostPort(test.endpoint, strconv.Itoa(int(*f.slice.Ports[0].Port)))
			if backend.pod.UID != f.pod.UID || backend.witness.PodUID != f.pod.UID ||
				len(backend.witness.Pins) != 1 || backend.witness.Pins[0] != wantPin {
				t.Fatal("recovery did not retain the exact Pod and selected address")
			}
		})
	}
}
