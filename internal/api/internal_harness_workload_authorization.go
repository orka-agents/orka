package api

import (
	"context"
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"github.com/orka-agents/orka/internal/store"
)

// The bearer capability alone is insufficient in namespaces where workers can
// read Secrets. Require a Deployment Pod selected by the dispatched wrapper
// Service. Job creators cannot create ReplicaSets or their controlled Pods.
func (a internalCallerAuthorizer) verifyHarnessWrapperPod(ctx context.Context, user *UserInfo, attempt *store.HarnessV1Attempt) error {
	denied := fiber.NewError(fiber.StatusForbidden, "harness artifact authorization failed")
	pod, err := a.resolveCallerPod(ctx, user, attempt.AuthSecretNamespace)
	if err != nil {
		return denied
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != appsv1.SchemeGroupVersion.String() || owner.Kind != "ReplicaSet" || owner.UID == "" {
		return denied
	}
	rs := &appsv1.ReplicaSet{}
	if err := a.k8sReader.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: owner.Name}, rs); err != nil ||
		rs.UID != owner.UID || !rs.DeletionTimestamp.IsZero() {
		return denied
	}
	owner = metav1.GetControllerOf(rs)
	if owner == nil || owner.APIVersion != appsv1.SchemeGroupVersion.String() || owner.Kind != "Deployment" || owner.UID == "" {
		return denied
	}
	deployment := &appsv1.Deployment{}
	if err := a.k8sReader.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: owner.Name}, deployment); err != nil ||
		deployment.UID != owner.UID || !deployment.DeletionTimestamp.IsZero() {
		return denied
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil || selector.Empty() || !selector.Matches(labels.Set(pod.Labels)) {
		return denied
	}
	serviceKey, ok := harnessWrapperServiceKey(attempt.BackendEndpoint, pod.Namespace)
	if !ok {
		return denied
	}
	service := &corev1.Service{}
	if err := a.k8sReader.Get(ctx, serviceKey, service); err != nil ||
		service.UID == "" || !service.DeletionTimestamp.IsZero() || service.Spec.Type == corev1.ServiceTypeExternalName ||
		len(service.Spec.Selector) == 0 {
		return denied
	}
	serviceSelector := labels.SelectorFromSet(service.Spec.Selector)
	if !serviceSelector.Matches(labels.Set(pod.Labels)) || !serviceSelector.Matches(labels.Set(deployment.Spec.Template.Labels)) {
		return denied
	}
	return nil
}

func harnessWrapperServiceKey(rawEndpoint, namespace string) (types.NamespacedName, bool) {
	endpoint, err := url.Parse(rawEndpoint)
	if err != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return types.NamespacedName{}, false
	}
	host := strings.Split(strings.TrimSuffix(endpoint.Hostname(), "."), ".")
	if host[0] == "" || (len(host) > 1 && host[1] != namespace) || (len(host) > 2 && host[2] != "svc") {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Namespace: namespace, Name: host[0]}, true
}
