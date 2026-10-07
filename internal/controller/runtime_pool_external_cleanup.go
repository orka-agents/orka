// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"reflect"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const externalCoreControlPortName = "control"

func externalCoreResourceOwned(pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, object client.Object) bool {
	for key, value := range cfg.labels {
		if object.GetLabels()[key] != value {
			return false
		}
	}
	if object.GetNamespace() != pool.Namespace {
		return len(object.GetOwnerReferences()) == 0
	}
	owner := metav1.GetControllerOf(object)
	return len(object.GetOwnerReferences()) == 1 && owner != nil && owner.APIVersion == corev1alpha1.GroupVersion.String() && owner.Kind == "RuntimePool" && owner.Name == pool.Name && owner.UID == pool.UID
}

func externalCoreServiceMatches(cfg runtimePoolConfig, service *corev1.Service) bool {
	spec := service.Spec.DeepCopy()
	// These fields are defaulted or allocated by the API, not Core permissions.
	spec.IPFamilies, spec.IPFamilyPolicy, spec.InternalTrafficPolicy = nil, nil, nil
	if spec.SessionAffinity == corev1.ServiceAffinityNone {
		spec.SessionAffinity = ""
	}
	if spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyCluster {
		spec.ExternalTrafficPolicy = ""
	}
	expected := corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: corev1.ClusterIPNone, ClusterIPs: []string{corev1.ClusterIPNone},
		Selector: map[string]string{runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel]},
		Ports:    []corev1.ServicePort{{Name: externalCoreControlPortName, Port: runtimePoolPort, TargetPort: intstr.FromString(externalCoreControlPortName), Protocol: corev1.ProtocolTCP}}}
	return reflect.DeepEqual(*spec, expected)
}

func (r *RuntimePoolReconciler) deleteExternalCoreDiscoveryResources(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig) (bool, error) {
	reader := uncachedReader(r.APIReader, r.Client)
	remaining := false
	for _, object := range []client.Object{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.namespace, Name: cfg.baseName}},
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.namespace, Name: runtimePoolChildName(cfg.baseName, "pdb")}},
	} {
		if err := reader.Get(ctx, types.NamespacedName{Namespace: object.GetNamespace(), Name: object.GetName()}, object); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		if !externalCoreResourceOwned(pool, cfg, object) {
			continue
		}
		switch typed := object.(type) {
		case *corev1.Service:
			if !externalCoreServiceMatches(cfg, typed) {
				continue
			}
		case *policyv1.PodDisruptionBudget:
			expected := policyv1.PodDisruptionBudgetSpec{MaxUnavailable: new(intstr.FromInt32(0)), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel]}}}
			if !reflect.DeepEqual(typed.Spec, expected) {
				continue
			}
		}
		pending, err := r.deleteExternalCoreResource(ctx, object)
		if err != nil {
			return false, err
		}
		remaining = remaining || pending
	}
	return remaining, nil
}

func (r *RuntimePoolReconciler) deleteExternalCoreResource(ctx context.Context, object client.Object) (bool, error) {
	if err := r.Delete(ctx, object, deleteCurrentObjectPreconditions(object)...); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	current := object.DeepCopyObject().(client.Object)
	if err := uncachedReader(r.APIReader, r.Client).Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return current.GetUID() == object.GetUID(), nil
}
