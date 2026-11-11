package applicationexec

import (
	"reflect"
	"strings"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func staticObjectMatches(expected, observed client.Object) bool {
	if !reflect.DeepEqual(expected.GetOwnerReferences(), observed.GetOwnerReferences()) {
		return false
	}
	for key, value := range expected.GetLabels() {
		if observed.GetLabels()[key] != value {
			return false
		}
	}
	if matches, applicable := rbacObjectMatches(expected, observed); applicable {
		return matches
	}
	switch wanted := expected.(type) {
	case *corev1.Namespace:
		_, ok := observed.(*corev1.Namespace)
		return ok
	case *corev1.ServiceAccount:
		actual, ok := observed.(*corev1.ServiceAccount)
		return ok && !ptr.Deref(actual.AutomountServiceAccountToken, true) &&
			len(actual.Secrets) == 0 && len(actual.ImagePullSecrets) == 0
	case *admissionv1.ValidatingAdmissionPolicy:
		actual, ok := observed.(*admissionv1.ValidatingAdmissionPolicy)
		return ok && apiequality.Semantic.DeepEqual(wanted.Spec, actual.Spec)
	case *admissionv1.ValidatingAdmissionPolicyBinding:
		actual, ok := observed.(*admissionv1.ValidatingAdmissionPolicyBinding)
		return ok && apiequality.Semantic.DeepEqual(wanted.Spec, actual.Spec)
	case *networkingv1.NetworkPolicy:
		actual, ok := observed.(*networkingv1.NetworkPolicy)
		return ok && apiequality.Semantic.DeepEqual(wanted.Spec, actual.Spec)
	case *corev1.ResourceQuota:
		actual, ok := observed.(*corev1.ResourceQuota)
		return ok && apiequality.Semantic.DeepEqual(wanted.Spec, actual.Spec)
	case *corev1.PersistentVolumeClaim:
		return pvcObjectMatches(wanted, observed)
	case *corev1.ConfigMap:
		return configMapObjectMatches(wanted, observed)
	case *corev1.Secret:
		actual, ok := observed.(*corev1.Secret)
		return ok && ptr.Deref(actual.Immutable, false) && actual.Type == wanted.Type &&
			reflect.DeepEqual(wanted.Data, actual.Data)
	case *corev1.Service:
		return serviceObjectMatches(wanted, observed)
	case *coordinationv1.Lease:
		actual, ok := observed.(*coordinationv1.Lease)
		return ok && reflect.DeepEqual(wanted.Spec, actual.Spec)
	default:
		// Jobs receive a complete execution-policy comparison at ensureJob.
		// Mutable Service/Ingress data is independently observed, never trusted
		// as authority for workload, credential, or network policy admission.
		return reflect.TypeOf(expected) == reflect.TypeOf(observed)
	}
}

func rbacObjectMatches(expected, observed client.Object) (bool, bool) {
	switch wanted := expected.(type) {
	case *rbacv1.Role:
		actual, ok := observed.(*rbacv1.Role)
		return ok && apiequality.Semantic.DeepEqual(wanted.Rules, actual.Rules), true
	case *rbacv1.RoleBinding:
		actual, ok := observed.(*rbacv1.RoleBinding)
		return ok && reflect.DeepEqual(wanted.Subjects, actual.Subjects) && wanted.RoleRef == actual.RoleRef, true
	case *rbacv1.ClusterRole:
		actual, ok := observed.(*rbacv1.ClusterRole)
		return ok && actual.AggregationRule == nil && apiequality.Semantic.DeepEqual(wanted.Rules, actual.Rules), true
	case *rbacv1.ClusterRoleBinding:
		actual, ok := observed.(*rbacv1.ClusterRoleBinding)
		return ok && reflect.DeepEqual(wanted.Subjects, actual.Subjects) && wanted.RoleRef == actual.RoleRef, true
	default:
		return false, false
	}
}

func pvcObjectMatches(wanted *corev1.PersistentVolumeClaim, observed client.Object) bool {
	actual, ok := observed.(*corev1.PersistentVolumeClaim)
	if !ok {
		return false
	}
	spec := *actual.Spec.DeepCopy()
	// Binding is controller-owned; record its exact PV UID separately.
	spec.VolumeName = wanted.Spec.VolumeName
	return apiequality.Semantic.DeepEqual(wanted.Spec, spec)
}

func configMapObjectMatches(wanted *corev1.ConfigMap, observed client.Object) bool {
	actual, ok := observed.(*corev1.ConfigMap)
	if !ok {
		return false
	}
	if ptr.Deref(wanted.Immutable, false) {
		return ptr.Deref(actual.Immutable, false) && reflect.DeepEqual(wanted.Data, actual.Data) &&
			reflect.DeepEqual(wanted.BinaryData, actual.BinaryData)
	}
	// Mutable data fixtures are UID-fenced, not expected-verdict-fenced.
	return true
}

func serviceObjectMatches(wanted *corev1.Service, observed client.Object) bool {
	actual, ok := observed.(*corev1.Service)
	if !ok {
		return false
	}
	if !strings.HasPrefix(wanted.Name, "orka-") {
		return true
	}
	if actual.Spec.Type != corev1.ServiceTypeClusterIP || actual.Spec.ExternalName != "" ||
		len(actual.Spec.ExternalIPs) != 0 || !reflect.DeepEqual(wanted.Spec.Selector, actual.Spec.Selector) ||
		len(actual.Spec.Ports) != len(wanted.Spec.Ports) {
		return false
	}
	for i, port := range actual.Spec.Ports {
		if port.Name != wanted.Spec.Ports[i].Name || port.Port != wanted.Spec.Ports[i].Port ||
			port.TargetPort != wanted.Spec.Ports[i].TargetPort || port.Protocol != wanted.Spec.Ports[i].Protocol ||
			port.NodePort != 0 || port.AppProtocol != nil {
			return false
		}
	}
	return true
}
