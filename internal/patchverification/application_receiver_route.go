package patchverification

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"strings"

	verificationv1 "github.com/orka-agents/orka/pkg/storage/v1/verification"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
)

const (
	MaxApplicationReceiverRouteBytes = 2 << 10
	applicationEndpointSliceManager  = "endpointslice-controller.k8s.io"
)

type ApplicationReceiverRoute = verificationv1.ApplicationReceiverRoute

func ApplicationReceiverRouteAnnotation(podUID string) string {
	if !identifierPattern.MatchString(podUID) {
		return ""
	}
	hash := sha256.Sum256([]byte(podUID))
	return "applicationexec.orka.ai/receiver-route-" + hex.EncodeToString(hash[:24])
}

// DecodeApplicationReceiverRoute accepts only bounded canonical encoding/json
// output for this struct. Canonical encoding also rejects duplicate fields.
func DecodeApplicationReceiverRoute(content string, service ApplicationServiceIdentity,
	receiver ApplicationContainerIdentity) (ApplicationReceiverRoute, error) {
	var route ApplicationReceiverRoute
	if err := decodePreparationJSON(strings.NewReader(content), &route, MaxApplicationReceiverRouteBytes); err != nil {
		return route, err
	}
	canonical, err := json.Marshal(route)
	if err != nil || !bytes.Equal(canonical, []byte(content)) ||
		!applicationReceiverServiceIdentityValid(service, receiver) ||
		!applicationReceiverRouteIdentityValid(route, receiver) || route.ServiceUID != service.UID {
		return ApplicationReceiverRoute{}, ErrBinding
	}
	return route, nil
}

func applicationReceiverRouteIdentityValid(route ApplicationReceiverRoute, receiver ApplicationContainerIdentity) bool {
	return route.Version == ApplicationProtocolVersion && applicationName(route.Namespace) &&
		route.Namespace == receiver.Namespace && identifierPattern.MatchString(route.ServiceUID) &&
		route.PodUID == receiver.PodUID && identifierPattern.MatchString(route.PodUID) &&
		applicationName(route.EndpointSliceName) && identifierPattern.MatchString(route.EndpointSliceUID) &&
		identifierPattern.MatchString(route.EndpointSliceResourceVersion)
}

func FreezeApplicationReceiverRoute(service ApplicationServiceIdentity, receiver ApplicationContainerIdentity,
	receiverIP, portName string, endpointSlice *discoveryv1.EndpointSlice) (ApplicationReceiverRoute, error) {
	if err := ValidateApplicationReceiverEndpointSlice(service, receiver, receiverIP, portName, endpointSlice); err != nil {
		return ApplicationReceiverRoute{}, err
	}
	return ApplicationReceiverRoute{
		Version: ApplicationProtocolVersion, Namespace: service.Namespace,
		ServiceUID: service.UID, PodUID: receiver.PodUID, EndpointSliceName: endpointSlice.Name,
		EndpointSliceUID: string(endpointSlice.UID), EndpointSliceResourceVersion: endpointSlice.ResourceVersion,
	}, nil
}

func applicationReceiverServiceIdentityValid(service ApplicationServiceIdentity, receiver ApplicationContainerIdentity) bool {
	return applicationName(service.Namespace) && identifierPattern.MatchString(service.NamespaceUID) &&
		service.Namespace == receiver.Namespace && service.NamespaceUID == receiver.NamespaceUID &&
		applicationName(service.Name) && identifierPattern.MatchString(service.UID) &&
		applicationName(receiver.PodName) && identifierPattern.MatchString(receiver.PodUID) &&
		service.Port >= 1 && service.Port <= 65535
}

// ValidateApplicationReceiverEndpointSlice checks the sole ready route to an
// exact receiver Pod. Only the backend discovers slices; the observer GETs the
// frozen name and separately requires its exact UID and resource version.
func ValidateApplicationReceiverEndpointSlice(service ApplicationServiceIdentity, receiver ApplicationContainerIdentity,
	receiverIP, portName string, endpointSlice *discoveryv1.EndpointSlice) error {
	ip := net.ParseIP(receiverIP)
	if !applicationReceiverServiceIdentityValid(service, receiver) || ip == nil || endpointSlice == nil ||
		endpointSlice.Namespace != service.Namespace || endpointSlice.DeletionTimestamp != nil ||
		!applicationName(endpointSlice.Name) || !identifierPattern.MatchString(string(endpointSlice.UID)) ||
		!identifierPattern.MatchString(endpointSlice.ResourceVersion) ||
		endpointSlice.Labels[discoveryv1.LabelServiceName] != service.Name ||
		endpointSlice.Labels[discoveryv1.LabelManagedBy] != applicationEndpointSliceManager ||
		len(endpointSlice.OwnerReferences) != 1 || len(endpointSlice.Endpoints) != 1 || len(endpointSlice.Ports) != 1 {
		return ErrBinding
	}
	owner := endpointSlice.OwnerReferences[0]
	if owner.APIVersion != "v1" || owner.Kind != "Service" || owner.Name != service.Name ||
		string(owner.UID) != service.UID || owner.Controller == nil || !*owner.Controller {
		return ErrBinding
	}
	family := discoveryv1.AddressTypeIPv6
	if ip.To4() != nil {
		family = discoveryv1.AddressTypeIPv4
	}
	if endpointSlice.AddressType != family ||
		!applicationReceiverEndpointValid(endpointSlice.Endpoints[0], service, receiver, receiverIP) ||
		!applicationReceiverPortValid(endpointSlice.Ports[0], service.Port, portName) {
		return ErrBinding
	}
	return nil
}

func applicationReceiverEndpointValid(endpoint discoveryv1.Endpoint, service ApplicationServiceIdentity,
	receiver ApplicationContainerIdentity, receiverIP string) bool {
	conditions := endpoint.Conditions
	target := endpoint.TargetRef
	return len(endpoint.Addresses) == 1 && endpoint.Addresses[0] == receiverIP &&
		conditions.Ready != nil && *conditions.Ready &&
		(conditions.Serving == nil || *conditions.Serving) &&
		(conditions.Terminating == nil || !*conditions.Terminating) &&
		target != nil && (target.APIVersion == "" || target.APIVersion == "v1") &&
		target.Kind == "Pod" && target.Namespace == service.Namespace &&
		target.Name == receiver.PodName && string(target.UID) == receiver.PodUID
}

func applicationReceiverPortValid(port discoveryv1.EndpointPort, expectedPort int, expectedName string) bool {
	name := ""
	if port.Name != nil {
		name = *port.Name
	}
	return name == expectedName && port.Protocol != nil && *port.Protocol == corev1.ProtocolTCP &&
		port.Port != nil && int(*port.Port) == expectedPort
}
