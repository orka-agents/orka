package controller

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func agentRuntimeEndpointPortObjects() (*corev1.Service, *corev1.Pod, *discoveryv1.EndpointSlice) {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "runtime"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "runtime"},
			Ports:    []corev1.ServicePort{{Name: "acp", Port: 8080, TargetPort: intstr.FromInt32(8443)}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "runtime-pod", Labels: map[string]string{"app": "runtime"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime"}}},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.9", PodIPs: []corev1.PodIP{{IP: "10.0.0.9"}},
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "runtime",
			Labels: map[string]string{discoveryv1.LabelServiceName: service.Name},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: new("acp"), Port: new(int32(8443))}},
		Endpoints: []discoveryv1.Endpoint{{
			Addresses: []string{"10.0.0.9"},
			TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: "default", Name: pod.Name},
		}},
	}
	return service, pod, slice
}

func agentRuntimeNamedPortContainer() corev1.Container {
	return corev1.Container{
		Name:  "proxy",
		Ports: []corev1.ContainerPort{{Name: "control", ContainerPort: 8443, Protocol: corev1.ProtocolTCP}},
	}
}

func TestAgentRuntimeServiceBackendPortMatchesTarget(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func(*corev1.Service, *corev1.Pod, *discoveryv1.EndpointSlice)
		wantPin string
		wantErr string
	}{
		{
			name: "numeric target needs no declared container port", wantPin: "10.0.0.9:8443",
		},
		{
			name: "numeric target mismatch", wantErr: "targetPort",
			change: func(_ *corev1.Service, _ *corev1.Pod, slice *discoveryv1.EndpointSlice) {
				slice.Ports[0].Port = new(int32(9443))
			},
		},
		{
			name: "omitted target defaults to Service port", wantPin: "10.0.0.9:8080",
			change: func(service *corev1.Service, _ *corev1.Pod, slice *discoveryv1.EndpointSlice) {
				service.Spec.Ports[0].TargetPort = intstr.IntOrString{}
				slice.Ports[0].Port = new(int32(8080))
			},
		},
		{
			name: "omitted target mismatch", wantErr: "targetPort",
			change: func(service *corev1.Service, _ *corev1.Pod, _ *discoveryv1.EndpointSlice) {
				service.Spec.Ports[0].TargetPort = intstr.IntOrString{}
			},
		},
		{
			name: "named target in regular sidecar", wantPin: "10.0.0.9:8443",
			change: func(service *corev1.Service, pod *corev1.Pod, _ *discoveryv1.EndpointSlice) {
				service.Spec.Ports[0].TargetPort = intstr.FromString("control")
				pod.Spec.Containers = append(pod.Spec.Containers, agentRuntimeNamedPortContainer())
			},
		},
		{
			name: "named target mismatch", wantErr: "targetPort",
			change: func(service *corev1.Service, pod *corev1.Pod, slice *discoveryv1.EndpointSlice) {
				service.Spec.Ports[0].TargetPort = intstr.FromString("control")
				pod.Spec.Containers = append(pod.Spec.Containers, agentRuntimeNamedPortContainer())
				slice.Ports[0].Port = new(int32(9443))
			},
		},
		{
			name: "missing named target", wantErr: "targetPort",
			change: func(service *corev1.Service, _ *corev1.Pod, _ *discoveryv1.EndpointSlice) {
				service.Spec.Ports[0].TargetPort = intstr.FromString("control")
			},
		},
		{
			name: "named target in restartable init sidecar", wantPin: "10.0.0.9:8443",
			change: func(service *corev1.Service, pod *corev1.Pod, _ *discoveryv1.EndpointSlice) {
				service.Spec.Ports[0].TargetPort = intstr.FromString("control")
				pod.Spec.InitContainers = []corev1.Container{agentRuntimeNamedPortContainer()}
				pod.Spec.InitContainers[0].RestartPolicy = new(corev1.ContainerRestartPolicyAlways)
			},
		},
		{
			name: "ordinary init container is not a named target", wantErr: "targetPort",
			change: func(service *corev1.Service, pod *corev1.Pod, _ *discoveryv1.EndpointSlice) {
				service.Spec.Ports[0].TargetPort = intstr.FromString("control")
				pod.Spec.InitContainers = []corev1.Container{agentRuntimeNamedPortContainer()}
			},
		},
		{
			name: "non TCP Service", wantErr: "TCP",
			change: func(service *corev1.Service, _ *corev1.Pod, _ *discoveryv1.EndpointSlice) {
				service.Spec.Ports[0].Protocol = corev1.ProtocolUDP
			},
		},
		{
			name: "TCP Service selected after UDP on same number", wantPin: "10.0.0.9:8443",
			change: func(service *corev1.Service, _ *corev1.Pod, _ *discoveryv1.EndpointSlice) {
				service.Spec.Ports = append([]corev1.ServicePort{{Name: "udp", Port: 8080, Protocol: corev1.ProtocolUDP}}, service.Spec.Ports...)
			},
		},
		{
			name: "non TCP EndpointSlice", wantErr: "TCP",
			change: func(_ *corev1.Service, _ *corev1.Pod, slice *discoveryv1.EndpointSlice) {
				slice.Ports[0].Protocol = new(corev1.ProtocolUDP)
			},
		},
		{
			name: "named container port protocol mismatch", wantErr: "targetPort",
			change: func(service *corev1.Service, pod *corev1.Pod, _ *discoveryv1.EndpointSlice) {
				service.Spec.Ports[0].TargetPort = intstr.FromString("control")
				pod.Spec.Containers = []corev1.Container{agentRuntimeNamedPortContainer()}
				pod.Spec.Containers[0].Ports[0].Protocol = corev1.ProtocolUDP
			},
		},
		{
			name: "missing EndpointSlice port", wantErr: "port",
			change: func(_ *corev1.Service, _ *corev1.Pod, slice *discoveryv1.EndpointSlice) {
				slice.Ports[0].Port = nil
			},
		},
		{
			name: "out of range EndpointSlice port", wantErr: "port",
			change: func(_ *corev1.Service, _ *corev1.Pod, slice *discoveryv1.EndpointSlice) {
				slice.Ports[0].Port = new(int32(65536))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, pod, slice := agentRuntimeEndpointPortObjects()
			if test.change != nil {
				test.change(service, pod, slice)
			}
			r := newAgentRuntimeUnitReconciler(t, service, pod, slice)
			state, err := r.verifiedAgentRuntimeServiceBackendState(t.Context(), service, 8080)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) || len(state.pins) != 0 {
					t.Fatalf("pins=%v error=%v, want no pins and %q rejection", state.pins, err, test.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(state.pins, []string{test.wantPin}) {
				t.Fatalf("pins=%v error=%v, want [%s]", state.pins, err, test.wantPin)
			}
		})
	}
}

func TestAgentRuntimeServiceBackendNamedTargetResolvesPerPod(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		name := "distinct named ports per Pod"
		if mismatch {
			name = "second Pod target mismatch"
		}
		t.Run(name, func(t *testing.T) {
			service, pod, slice := agentRuntimeEndpointPortObjects()
			service.Spec.Ports[0].TargetPort = intstr.FromString("control")
			pod.Spec.Containers = []corev1.Container{agentRuntimeNamedPortContainer()}
			otherPod, otherSlice := pod.DeepCopy(), slice.DeepCopy()
			otherPod.Name = "other-runtime"
			otherPod.Spec.Containers[0].Ports[0].ContainerPort = 9443
			otherPod.Status.PodIP = "10.0.0.10"
			otherPod.Status.PodIPs = []corev1.PodIP{{IP: "10.0.0.10"}}
			otherSlice.Name = "other-runtime"
			otherSlice.Ports[0].Port = new(int32(9443))
			otherSlice.Endpoints[0].TargetRef.Name = otherPod.Name
			otherSlice.Endpoints[0].Addresses = []string{"10.0.0.10"}
			if mismatch {
				otherSlice.Ports[0].Port = new(int32(8443))
			}
			r := newAgentRuntimeUnitReconciler(t, service, pod, slice, otherPod, otherSlice)
			state, err := r.verifiedAgentRuntimeServiceBackendState(t.Context(), service, 8080)
			if mismatch {
				if err == nil || !strings.Contains(err.Error(), "targetPort") || len(state.pins) != 0 {
					t.Fatalf("pins=%v error=%v, want no pins and targetPort rejection", state.pins, err)
				}
				return
			}
			want := []string{"10.0.0.10:9443", "10.0.0.9:8443"}
			if err != nil || !slices.Equal(state.pins, want) || state.endpointCount != 2 {
				t.Fatalf("pins=%v count=%d error=%v, want %v and 2 backends", state.pins, state.endpointCount, err, want)
			}
		})
	}
}
