package patchverification

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
)

func applicationReceiverRouteFixture(t *testing.T) (ApplicationServiceIdentity, ApplicationContainerIdentity, *discoveryv1.EndpointSlice) {
	t.Helper()
	service := ApplicationServiceIdentity{Namespace: "protected-run", NamespaceUID: "protected-uid",
		Name: "receiver", UID: "service-uid", Port: 8080, Selector: map[string]string{"app": "receiver"}}
	receiver := applicationTestIdentity("observer", Digest([]byte("observer-image")))
	receiver.Namespace, receiver.NamespaceUID = service.Namespace, service.NamespaceUID
	endpointSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "receiver-generated", Namespace: service.Namespace,
			UID: "slice-uid", ResourceVersion: "17",
			Labels: map[string]string{"kubernetes.io/service-name": "receiver",
				"endpointslice.kubernetes.io/managed-by": "endpointslice-controller.k8s.io"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: "receiver",
				UID: "service-uid", Controller: new(true)}}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: new("tcp"), Protocol: new(corev1.ProtocolTCP), Port: new(int32(8080))}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"127.0.0.1"},
			Conditions: discoveryv1.EndpointConditions{Ready: new(true), Serving: new(true), Terminating: new(false)},
			TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: service.Namespace,
				Name: receiver.PodName, UID: types.UID(receiver.PodUID)}}},
	}
	return service, receiver, endpointSlice
}

func TestApplicationReceiverRouteCanonicalBinding(t *testing.T) {
	service, receiver, endpointSlice := applicationReceiverRouteFixture(t)
	route, err := FreezeApplicationReceiverRoute(service, receiver, "127.0.0.1", "tcp", endpointSlice)
	require.NoError(t, err)
	content, err := json.Marshal(route)
	require.NoError(t, err)
	require.JSONEq(t, `{"version":1,"namespace":"protected-run","serviceUID":"service-uid","podUID":"observer-uid",
		"endpointSliceName":"receiver-generated","endpointSliceUID":"slice-uid","endpointSliceResourceVersion":"17"}`, string(content))
	decoded, err := DecodeApplicationReceiverRoute(string(content), service, receiver)
	require.NoError(t, err)
	require.Equal(t, route, decoded)
	annotation := ApplicationReceiverRouteAnnotation(receiver.PodUID)
	require.True(t, strings.HasPrefix(annotation, "applicationexec.orka.ai/receiver-route-"))
	require.Empty(t, kvalidation.IsQualifiedName(annotation))
	require.Equal(t, annotation, ApplicationReceiverRouteAnnotation(receiver.PodUID))
	require.NotEqual(t, annotation, ApplicationReceiverRouteAnnotation("replacement-pod-uid"))
	require.Empty(t, ApplicationReceiverRouteAnnotation("../foreign"))
	require.Equal(t, 2048, MaxApplicationReceiverRouteBytes)
}

func TestApplicationReceiverRouteRejectsAmbiguousOrUnboundCapsules(t *testing.T) {
	service, receiver, endpointSlice := applicationReceiverRouteFixture(t)
	route, err := FreezeApplicationReceiverRoute(service, receiver, "127.0.0.1", "tcp", endpointSlice)
	require.NoError(t, err)
	content, err := json.Marshal(route)
	require.NoError(t, err)
	for name, modify := range map[string]func(*ApplicationReceiverRoute){
		"version":       func(r *ApplicationReceiverRoute) { r.Version++ },
		"namespace":     func(r *ApplicationReceiverRoute) { r.Namespace = "other-namespace" },
		"service UID":   func(r *ApplicationReceiverRoute) { r.ServiceUID = "other-service" },
		"Pod UID":       func(r *ApplicationReceiverRoute) { r.PodUID = "other-pod" },
		"empty slice":   func(r *ApplicationReceiverRoute) { r.EndpointSliceName = "" },
		"slice path":    func(r *ApplicationReceiverRoute) { r.EndpointSliceName = "../foreign" },
		"empty UID":     func(r *ApplicationReceiverRoute) { r.EndpointSliceUID = "" },
		"empty version": func(r *ApplicationReceiverRoute) { r.EndpointSliceResourceVersion = "" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := route
			modify(&changed)
			raw, err := json.Marshal(changed)
			require.NoError(t, err)
			_, err = DecodeApplicationReceiverRoute(string(raw), service, receiver)
			require.Error(t, err)
		})
	}
	for name, raw := range map[string]string{
		"duplicate key":     strings.Replace(string(content), `"version":1`, `"version":1,"version":1`, 1),
		"unknown key":       strings.Replace(string(content), `"version":1`, `"unknown":true,"version":1`, 1),
		"trailing document": string(content) + `{}`,
		"noncanonical":      string(content) + "\n",
		"missing version":   strings.Replace(string(content), `"version":1,`, "", 1),
		"null version":      strings.Replace(string(content), `"version":1`, `"version":null`, 1),
		"null resource UID": strings.Replace(string(content), `"slice-uid"`, "null", 1),
		"case alias":        strings.Replace(string(content), `"serviceUID"`, `"ServiceUID"`, 1),
		"oversized capsule": strings.Repeat(" ", 2049),
		"whole-value null":  "null",
		"partial document":  string(content[:len(content)-1]),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeApplicationReceiverRoute(raw, service, receiver)
			require.Error(t, err)
		})
	}
	receiver.NamespaceUID = "replaced-namespace"
	_, err = DecodeApplicationReceiverRoute(string(content), service, receiver)
	require.Error(t, err)
}

func TestApplicationReceiverEndpointSliceRejectsUnsafeRoutes(t *testing.T) {
	service, receiver, original := applicationReceiverRouteFixture(t)
	for name, modify := range map[string]func(*discoveryv1.EndpointSlice){
		"namespace":     func(s *discoveryv1.EndpointSlice) { s.Namespace = "other-namespace" },
		"empty UID":     func(s *discoveryv1.EndpointSlice) { s.UID = "" },
		"empty version": func(s *discoveryv1.EndpointSlice) { s.ResourceVersion = "" },
		"deleting":      func(s *discoveryv1.EndpointSlice) { s.DeletionTimestamp = new(metav1.Now()) },
		"wrong service": func(s *discoveryv1.EndpointSlice) { s.Labels["kubernetes.io/service-name"] = "other" },
		"wrong manager": func(s *discoveryv1.EndpointSlice) { s.Labels["endpointslice.kubernetes.io/managed-by"] = "untrusted" },
		"no owner":      func(s *discoveryv1.EndpointSlice) { s.OwnerReferences = nil },
		"extra owner": func(s *discoveryv1.EndpointSlice) {
			s.OwnerReferences = append(s.OwnerReferences, s.OwnerReferences[0])
		},
		"owner UID":         func(s *discoveryv1.EndpointSlice) { s.OwnerReferences[0].UID = "wrong-service-uid" },
		"owner name":        func(s *discoveryv1.EndpointSlice) { s.OwnerReferences[0].Name = "other" },
		"owner kind":        func(s *discoveryv1.EndpointSlice) { s.OwnerReferences[0].Kind = "Pod" },
		"owner version":     func(s *discoveryv1.EndpointSlice) { s.OwnerReferences[0].APIVersion = "other/v1" },
		"not controller":    func(s *discoveryv1.EndpointSlice) { s.OwnerReferences[0].Controller = new(false) },
		"missing owner bit": func(s *discoveryv1.EndpointSlice) { s.OwnerReferences[0].Controller = nil },
		"wrong family":      func(s *discoveryv1.EndpointSlice) { s.AddressType = discoveryv1.AddressTypeIPv6 },
		"no endpoints":      func(s *discoveryv1.EndpointSlice) { s.Endpoints = nil },
		"extra endpoint":    func(s *discoveryv1.EndpointSlice) { s.Endpoints = append(s.Endpoints, s.Endpoints[0]) },
		"no address":        func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].Addresses = nil },
		"extra address": func(s *discoveryv1.EndpointSlice) {
			s.Endpoints[0].Addresses = append(s.Endpoints[0].Addresses, "127.0.0.2")
		},
		"wrong address":     func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].Addresses[0] = "127.0.0.2" },
		"unknown ready":     func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].Conditions.Ready = nil },
		"not ready":         func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].Conditions.Ready = new(false) },
		"not serving":       func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].Conditions.Serving = new(false) },
		"terminating":       func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].Conditions.Terminating = new(true) },
		"no target":         func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].TargetRef = nil },
		"target UID":        func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].TargetRef.UID = "other-pod-uid" },
		"target name":       func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].TargetRef.Name = "other-pod" },
		"target namespace":  func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].TargetRef.Namespace = "other-namespace" },
		"target kind":       func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].TargetRef.Kind = "Service" },
		"target version":    func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].TargetRef.APIVersion = "other/v1" },
		"no ports":          func(s *discoveryv1.EndpointSlice) { s.Ports = nil },
		"extra port":        func(s *discoveryv1.EndpointSlice) { s.Ports = append(s.Ports, s.Ports[0]) },
		"wrong port":        func(s *discoveryv1.EndpointSlice) { s.Ports[0].Port = new(int32(8081)) },
		"missing port":      func(s *discoveryv1.EndpointSlice) { s.Ports[0].Port = nil },
		"wrong port name":   func(s *discoveryv1.EndpointSlice) { s.Ports[0].Name = new("other") },
		"missing port name": func(s *discoveryv1.EndpointSlice) { s.Ports[0].Name = nil },
		"wrong protocol":    func(s *discoveryv1.EndpointSlice) { s.Ports[0].Protocol = new(corev1.ProtocolUDP) },
		"missing protocol":  func(s *discoveryv1.EndpointSlice) { s.Ports[0].Protocol = nil },
	} {
		t.Run(name, func(t *testing.T) {
			changed := original.DeepCopy()
			modify(changed)
			_, err := FreezeApplicationReceiverRoute(service, receiver, "127.0.0.1", "tcp", changed)
			require.Error(t, err)
		})
	}
	_, err := FreezeApplicationReceiverRoute(service, receiver, "127.0.0.1", "tcp", nil)
	require.Error(t, err)
	original.AddressType = discoveryv1.AddressTypeIPv6
	original.Endpoints[0].Addresses = []string{"::1"}
	_, err = FreezeApplicationReceiverRoute(service, receiver, "::1", "tcp", original)
	require.NoError(t, err, "matching native IPv6 routes retain exact address-family binding")
}
