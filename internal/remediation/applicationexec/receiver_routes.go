package applicationexec

import (
	"context"
	"encoding/json"
	"net"
	"reflect"
	"strings"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	maxReceiverSlices      = 4
	resourceEndpointSlices = "endpointslices"
	receiverRoleSuffix     = "-receiver-role"
	receiverBindingSuffix  = "-receiver-binding"
)

type receiverRouteReceipt struct {
	Service   pv.ApplicationServiceIdentity   `json:"service"`
	Receiver  pv.ApplicationContainerIdentity `json:"receiver"`
	Route     pv.ApplicationReceiverRoute     `json:"route"`
	Published bool                            `json:"published,omitempty"`
}

func (b *Backend) ensureReceiverRoute(ctx context.Context, r *run, side, role string,
	service pv.ApplicationServiceIdentity, receiver pv.ApplicationContainerIdentity) (bool, error) {
	if err := b.authorizeEffect(ctx, r); err != nil {
		return false, err
	}
	physicalSide := observerSide(r, side)
	key := resourceKey(physicalSide, role)
	current, portName, err := b.receiverService(ctx, r, service)
	if err != nil {
		return false, err
	}
	receipt, retained := r.state.ReceiverRoutes[key]
	if retained && (!reflect.DeepEqual(receipt.Service, service) || receipt.Receiver != receiver) {
		return false, contract.ErrConflict
	}
	if !retained && current.Annotations[pv.ApplicationReceiverRouteAnnotation(receiver.PodUID)] != "" {
		return false, contract.ErrConflict
	}
	pod, running, err := b.receiverPod(ctx, r, key, receiver)
	if err != nil {
		return false, err
	}
	if !running {
		if !retained {
			return false, contract.ErrConflict
		}
		// Native endpoints withdraw after a successful worker exits. The final
		// worker receipt must attest the frozen route before closing its listener.
		if err := b.confirmCompletedReceiverPublication(ctx, r, key, role, current, receipt); err != nil {
			return false, err
		}
		return true, nil
	}
	if !pod.Status.ContainerStatuses[0].Ready {
		return false, nil
	}
	slice, route, err := b.receiverSlice(ctx, service, receiver, pod.Status.PodIP, portName)
	if err != nil {
		return false, err
	}
	if retained && (slice == nil || receipt.Route != route) {
		return b.receiverCompletedAfterRouteRead(ctx, r, key, role, receiver, receipt)
	}
	if slice == nil {
		return false, nil
	}
	if !retained {
		if len(r.state.ReceiverRoutes) >= 16 {
			return false, contract.ErrInvalid
		}
		if err := b.recordGenerated(ctx, r, physicalSide, "receiver-slice", slice, current.UID); err != nil {
			return false, err
		}
		receipt = receiverRouteReceipt{Service: service, Receiver: receiver, Route: route}
		if r.state.ReceiverRoutes == nil {
			r.state.ReceiverRoutes = make(map[string]receiverRouteReceipt)
		}
		r.state.ReceiverRoutes[key] = receipt
		if err := b.save(ctx, r); err != nil {
			return false, err
		}
	}
	if err := b.ensureReceiverAccess(ctx, r, physicalSide, role, receipt); err != nil {
		return false, err
	}
	if err := b.publishReceiverRoute(ctx, r, key, receipt); err != nil {
		return false, err
	}
	return true, nil
}

func (b *Backend) receiverCompletedAfterRouteRead(ctx context.Context, r *run, key, role string,
	receiver pv.ApplicationContainerIdentity, receipt receiverRouteReceipt) (bool, error) {
	_, running, err := b.receiverPod(ctx, r, key, receiver)
	if err != nil {
		return false, err
	}
	if running {
		return false, contract.ErrConflict
	}
	current, _, err := b.receiverService(ctx, r, receipt.Service)
	if err != nil {
		return false, err
	}
	if err := b.confirmCompletedReceiverPublication(ctx, r, key, role, current, receipt); err != nil {
		return false, err
	}
	return true, nil
}

func (b *Backend) receiverService(ctx context.Context, r *run, identity pv.ApplicationServiceIdentity) (*corev1.Service, string, error) {
	entry := r.namedEffect("v1", serviceKind, identity.Namespace, identity.Name)
	if entry == nil || string(entry.UID) != identity.UID {
		return nil, "", contract.ErrConflict
	}
	object, err := b.boundObject(ctx, r, entry.Key)
	if err != nil {
		return nil, "", err
	}
	service := object.(*corev1.Service)
	if service.Spec.Type != corev1.ServiceTypeClusterIP || net.ParseIP(service.Spec.ClusterIP) == nil ||
		service.Spec.ExternalName != "" || len(service.Spec.ExternalIPs) != 0 || len(service.Spec.Ports) != 1 ||
		!reflect.DeepEqual(service.Spec.Selector, identity.Selector) {
		return nil, "", contract.ErrConflict
	}
	port := service.Spec.Ports[0]
	if port.Name == "" || port.Protocol != corev1.ProtocolTCP || int(port.Port) != identity.Port ||
		port.TargetPort.IntValue() != identity.Port || port.NodePort != 0 {
		return nil, "", contract.ErrConflict
	}
	return service, port.Name, nil
}

func (b *Backend) receiverPod(ctx context.Context, r *run, key string, identity pv.ApplicationContainerIdentity) (*corev1.Pod, bool, error) {
	if receipt, found := r.state.Pods[key]; !found || receipt.Identity != identity {
		return nil, false, contract.ErrConflict
	}
	pod := &corev1.Pod{}
	if err := b.targetReader.Get(ctx, client.ObjectKey{Namespace: identity.Namespace, Name: identity.PodName}, pod); err != nil {
		return nil, false, safeError(contract.ErrUnavailable, err)
	}
	if string(pod.UID) != identity.PodUID || !pod.DeletionTimestamp.IsZero() ||
		pod.Spec.NodeName != r.state.Target.NodeName || len(pod.Status.ContainerStatuses) != 1 {
		return nil, false, contract.ErrConflict
	}
	status := pod.Status.ContainerStatuses[0]
	running := status.State.Running != nil
	if !containerIdentityMatches(status, identity, running) {
		return nil, false, contract.ErrConflict
	}
	if running && pod.Status.Phase != corev1.PodRunning {
		return nil, false, contract.ErrConflict
	}
	if !running && pod.Status.Phase != corev1.PodSucceeded {
		return nil, false, contract.ErrConflict
	}
	if err := b.livePodOwner(ctx, r, identity, pod); err != nil {
		return nil, false, err
	}
	return pod, running, nil
}

func (b *Backend) receiverSlice(ctx context.Context, service pv.ApplicationServiceIdentity,
	receiver pv.ApplicationContainerIdentity, receiverIP, portName string) (*discoveryv1.EndpointSlice, pv.ApplicationReceiverRoute, error) {
	slices := &discoveryv1.EndpointSliceList{}
	if err := b.targetReader.List(ctx, slices, client.InNamespace(service.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: service.Name}, client.Limit(maxReceiverSlices+1)); err != nil {
		return nil, pv.ApplicationReceiverRoute{}, safeError(contract.ErrUnavailable, err)
	}
	if slices.Continue != "" || len(slices.Items) > maxReceiverSlices {
		return nil, pv.ApplicationReceiverRoute{}, ErrIsolation
	}
	var selected *discoveryv1.EndpointSlice
	var route pv.ApplicationReceiverRoute
	for index := range slices.Items {
		slice := &slices.Items[index]
		if !receiverSliceOwnerMatches(service, slice) {
			return nil, route, contract.ErrConflict
		}
		if len(slice.Endpoints) == 0 || !slice.DeletionTimestamp.IsZero() {
			continue
		}
		candidate, err := pv.FreezeApplicationReceiverRoute(service, receiver, receiverIP, portName, slice)
		if err != nil {
			if receiverSliceNotReady(service, receiver, receiverIP, portName, slice) {
				continue
			}
			return nil, route, contract.ErrConflict
		}
		if selected != nil {
			return nil, route, ErrIsolation
		}
		selected, route = slice, candidate
	}
	return selected, route, nil
}

func receiverSliceOwnerMatches(service pv.ApplicationServiceIdentity, slice *discoveryv1.EndpointSlice) bool {
	if slice.Namespace != service.Namespace || slice.UID == "" || len(slice.OwnerReferences) != 1 ||
		slice.Labels[discoveryv1.LabelServiceName] != service.Name ||
		slice.Labels[discoveryv1.LabelManagedBy] != "endpointslice-controller.k8s.io" {
		return false
	}
	owner := slice.OwnerReferences[0]
	return owner.APIVersion == "v1" && owner.Kind == serviceKind && owner.Name == service.Name &&
		string(owner.UID) == service.UID && owner.Controller != nil && *owner.Controller
}

func receiverSliceNotReady(service pv.ApplicationServiceIdentity, receiver pv.ApplicationContainerIdentity,
	receiverIP, portName string, slice *discoveryv1.EndpointSlice) bool {
	if len(slice.Endpoints) != 1 || slice.Endpoints[0].Conditions.Ready != nil && *slice.Endpoints[0].Conditions.Ready {
		return false
	}
	copy := slice.DeepCopy()
	copy.Endpoints[0].Conditions.Ready = new(true)
	return pv.ValidateApplicationReceiverEndpointSlice(service, receiver, receiverIP, portName, copy) == nil
}

func (b *Backend) ensureReceiverAccess(ctx context.Context, r *run, physicalSide, role string, receipt receiverRouteReceipt) error {
	if receipt.Route.EndpointSliceName == "" || receipt.Route.EndpointSliceUID == "" {
		return contract.ErrConflict
	}
	access, binding := receiverAccessObjects(role, receipt)
	if _, err := b.ensureObject(ctx, r, resourceKey(physicalSide, role+receiverRoleSuffix), access); err != nil {
		return err
	}
	_, err := b.ensureObject(ctx, r, resourceKey(physicalSide, role+receiverBindingSuffix), binding)
	return err
}

func receiverAccessObjects(role string, receipt receiverRouteReceipt) (*rbacv1.Role, *rbacv1.RoleBinding) {
	name := jobName(role) + "-receiver-route"
	access := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: receipt.Route.Namespace},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{discoveryv1.GroupName}, Resources: []string{resourceEndpointSlices},
			Verbs: []string{verbGet}, ResourceNames: []string{receipt.Route.EndpointSliceName}}}}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: receipt.Route.Namespace},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: roleKind, Name: name},
		Subjects: []rbacv1.Subject{{Kind: serviceAccountKind, Namespace: receipt.Receiver.Namespace,
			Name: receipt.Receiver.ServiceAccountName}}}
	return access, binding
}

func (b *Backend) confirmCompletedReceiverPublication(ctx context.Context, r *run, key, role string,
	service *corev1.Service, receipt receiverRouteReceipt) error {
	if err := receiverPublicationMatches(service, receipt); err != nil {
		return err
	}
	if receipt.Published {
		return nil
	}
	access, binding := receiverAccessObjects(role, receipt)
	for _, expected := range []struct {
		key    string
		object client.Object
	}{{key + receiverRoleSuffix, access}, {key + receiverBindingSuffix, binding}} {
		actual, err := b.boundObject(ctx, r, expected.key)
		if err != nil {
			return err
		}
		if actual.GetNamespace() != expected.object.GetNamespace() || actual.GetName() != expected.object.GetName() ||
			!staticObjectMatches(expected.object, actual) {
			return contract.ErrConflict
		}
	}
	receipt.Published = true
	r.state.ReceiverRoutes[key] = receipt
	return b.save(ctx, r)
}

func receiverPublicationMatches(service *corev1.Service, receipt receiverRouteReceipt) error {
	content := service.Annotations[pv.ApplicationReceiverRouteAnnotation(receipt.Receiver.PodUID)]
	route, err := pv.DecodeApplicationReceiverRoute(content, receipt.Service, receipt.Receiver)
	if err != nil || route != receipt.Route {
		return contract.ErrConflict
	}
	return nil
}

func (b *Backend) publishReceiverRoute(ctx context.Context, r *run, key string, receipt receiverRouteReceipt) error {
	service, _, err := b.receiverService(ctx, r, receipt.Service)
	if err != nil {
		return err
	}
	annotation := pv.ApplicationReceiverRouteAnnotation(receipt.Receiver.PodUID)
	content, err := json.Marshal(receipt.Route)
	if annotation == "" || err != nil || len(content) > pv.MaxApplicationReceiverRouteBytes {
		return contract.ErrInvalid
	}
	current := service.Annotations[annotation]
	if receipt.Published || current != "" {
		if err := receiverPublicationMatches(service, receipt); err != nil {
			return err
		}
	} else {
		patch, err := receiverRoutePatch(service, annotation, string(content))
		if err != nil {
			return err
		}
		if err := b.target.Patch(ctx, service, client.RawPatch(types.JSONPatchType, patch)); err != nil {
			return safeError(ErrUncertain, err)
		}
	}
	if !receipt.Published {
		receipt.Published = true
		r.state.ReceiverRoutes[key] = receipt
		return b.save(ctx, r)
	}
	return nil
}

func receiverRoutePatch(service *corev1.Service, annotation, content string) ([]byte, error) {
	path := "/metadata/annotations/" + strings.ReplaceAll(strings.ReplaceAll(annotation, "~", "~0"), "/", "~1")
	var value any = content
	if len(service.Annotations) == 0 {
		path, value = "/metadata/annotations", map[string]string{annotation: content}
	}
	patch, err := json.Marshal([]map[string]any{
		{"op": jsonPatchTest, jsonPatchPathKey: "/metadata/uid", jsonPatchValueKey: string(service.UID)},
		{"op": jsonPatchTest, jsonPatchPathKey: "/metadata/resourceVersion", jsonPatchValueKey: service.ResourceVersion},
		{"op": jsonPatchAdd, jsonPatchPathKey: path, jsonPatchValueKey: value},
	})
	if err != nil {
		return nil, contract.ErrInvalid
	}
	return patch, nil
}

func (b *Backend) stopReceiverAccess(ctx context.Context, r *run, physicalSide, role string) (bool, error) {
	gone := true
	for _, suffix := range []string{receiverBindingSuffix, receiverRoleSuffix} {
		key := resourceKey(physicalSide, role+suffix)
		for index := range r.state.Effects {
			if r.state.Effects[index].Key != key {
				continue
			}
			done, err := b.deleteEffect(ctx, r, index)
			if err != nil {
				return false, err
			}
			gone = gone && done
		}
	}
	return gone, nil
}

func (b *Backend) validateReceiverReceipt(ctx context.Context, r *run, service pv.ApplicationServiceIdentity,
	receiver pv.ApplicationContainerIdentity, route pv.ApplicationReceiverRoute) error {
	for _, receipt := range r.state.ReceiverRoutes {
		if receipt.Receiver != receiver || !reflect.DeepEqual(receipt.Service, service) {
			continue
		}
		if !receipt.Published || receipt.Route != route {
			return contract.ErrConflict
		}
		current, _, err := b.receiverService(ctx, r, service)
		if err != nil {
			return err
		}
		return receiverPublicationMatches(current, receipt)
	}
	return contract.ErrConflict
}
