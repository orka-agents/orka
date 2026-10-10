// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"

	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	externalDiscoveryIntentsAnnotation = "orka.ai/external-discovery-intents"
	externalDiscoveryService           = "Service"
	externalDiscoveryPDB               = "PodDisruptionBudget"
)

type externalDiscoveryIntent struct {
	Namespace    string    `json:"namespace"`
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	CreateIssued bool      `json:"createIssued"`
	UID          types.UID `json:"uid,omitempty"`
}

func externalDiscoveryObject(cfg runtimePoolConfig, kind string) client.Object {
	if kind == externalDiscoveryService {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.namespace, Name: cfg.baseName, Labels: cloneStringMap(cfg.labels)}, Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP, ClusterIP: corev1.ClusterIPNone, ClusterIPs: []string{corev1.ClusterIPNone},
			Selector: map[string]string{runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel]}, Ports: []corev1.ServicePort{{Name: externalCoreControlPortName, Port: runtimePoolPort, TargetPort: intstr.FromString(externalCoreControlPortName), Protocol: corev1.ProtocolTCP}},
		}}
	}
	return &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.namespace, Name: runtimePoolChildName(cfg.baseName, "pdb"), Labels: cloneStringMap(cfg.labels)}, Spec: policyv1.PodDisruptionBudgetSpec{
		MaxUnavailable: new(intstr.FromInt32(0)), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel]}},
	}}
}

func externalDiscoveryMatches(cfg runtimePoolConfig, object client.Object) bool {
	switch typed := object.(type) {
	case *corev1.Service:
		return externalCoreServiceMatches(cfg, typed)
	case *policyv1.PodDisruptionBudget:
		return reflect.DeepEqual(typed.Spec, externalDiscoveryObject(cfg, externalDiscoveryPDB).(*policyv1.PodDisruptionBudget).Spec)
	default:
		return false
	}
}

func externalDiscoveryIntents(pool *corev1alpha1.RuntimePool) ([]externalDiscoveryIntent, error) {
	encoded := pool.Annotations[externalDiscoveryIntentsAnnotation]
	if encoded == "" {
		return nil, nil
	}
	var intents []externalDiscoveryIntent
	if json.Unmarshal([]byte(encoded), &intents) != nil || len(intents) > 2 {
		return nil, workspaceprovider.ErrStaleIdentity
	}
	seen := map[string]bool{}
	base := runtimePoolResourceName(pool.Namespace, pool.Name)
	for _, intent := range intents {
		name := base
		if intent.Kind == externalDiscoveryPDB {
			name = runtimePoolChildName(base, "pdb")
		} else if intent.Kind != externalDiscoveryService {
			return nil, workspaceprovider.ErrStaleIdentity
		}
		if intent.Name != name || len(validation.IsDNS1123Label(intent.Namespace)) != 0 || seen[intent.Kind] || intent.UID != "" && !intent.CreateIssued {
			return nil, workspaceprovider.ErrStaleIdentity
		}
		seen[intent.Kind] = true
	}
	return intents, nil
}

func (r *RuntimePoolReconciler) saveExternalDiscoveryIntents(ctx context.Context, pool *corev1alpha1.RuntimePool, intents []externalDiscoveryIntent) error {
	encoded, err := json.Marshal(intents)
	if err != nil {
		return err
	}
	before := pool.DeepCopy()
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	if len(intents) == 0 {
		delete(pool.Annotations, externalDiscoveryIntentsAnnotation)
	} else {
		pool.Annotations[externalDiscoveryIntentsAnnotation] = string(encoded)
	}
	return r.Patch(ctx, pool, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func externalDiscoveryIntentMatches(pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, intent externalDiscoveryIntent, object client.Object) bool {
	switch object.(type) {
	case *corev1.Service:
		if intent.Kind != externalDiscoveryService {
			return false
		}
	case *policyv1.PodDisruptionBudget:
		if intent.Kind != externalDiscoveryPDB {
			return false
		}
	default:
		return false
	}
	return intent.CreateIssued && intent.UID != "" && intent.UID == object.GetUID() &&
		object.GetNamespace() == intent.Namespace && object.GetName() == intent.Name &&
		externalCoreResourceOwned(pool, cfg, object) && externalDiscoveryMatches(cfg, object)
}

// Cross-namespace objects have no valid ownerReference. Only a durably saved
// UID from an acknowledged create authorizes reuse or deletion. An unknown
// creation outcome stays closed; public labels and specs cannot recover a UID.
func (r *RuntimePoolReconciler) ensureExternalDiscoveryResource(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, kind string) error {
	intents, err := externalDiscoveryIntents(pool)
	if err != nil {
		return err
	}
	index := slices.IndexFunc(intents, func(intent externalDiscoveryIntent) bool { return intent.Kind == kind })
	object := externalDiscoveryObject(cfg, kind)
	err = uncachedReader(r.APIReader, r.Client).Get(ctx, client.ObjectKeyFromObject(object), object)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil {
		if index < 0 || !externalDiscoveryIntentMatches(pool, cfg, intents[index], object) || object.GetDeletionTimestamp() != nil {
			return workspaceprovider.ErrStaleIdentity
		}
		return nil
	}
	if index >= 0 {
		if intents[index].CreateIssued && intents[index].UID == "" {
			return fmt.Errorf("discovery resource creation outcome is unresolved: %w", workspaceprovider.ErrStaleIdentity)
		}
		intents = slices.Delete(intents, index, index+1)
		if err := r.saveExternalDiscoveryIntents(ctx, pool, intents); err != nil {
			return err
		}
	}
	object = externalDiscoveryObject(cfg, kind)
	intent := externalDiscoveryIntent{Namespace: cfg.namespace, Name: object.GetName(), Kind: kind}
	intents = append(intents, intent)
	index = len(intents) - 1
	if err := r.saveExternalDiscoveryIntents(ctx, pool, intents); err != nil {
		return err
	}
	intents[index].CreateIssued = true
	if err := r.saveExternalDiscoveryIntents(ctx, pool, intents); err != nil {
		return err
	}
	if err := r.Create(ctx, object); err != nil {
		if externalCredentialCreateRejected(err) {
			intents[index].CreateIssued = false
			if saveErr := r.saveExternalDiscoveryIntents(ctx, pool, intents); saveErr != nil {
				return saveErr
			}
		}
		return err
	}
	if object.GetUID() == "" || !externalCoreResourceOwned(pool, cfg, object) || !externalDiscoveryMatches(cfg, object) {
		return workspaceprovider.ErrStaleIdentity
	}
	intents[index].UID = object.GetUID()
	return r.saveExternalDiscoveryIntents(ctx, pool, intents)
}

func externalDiscoveryResourceOwned(pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, object client.Object) (bool, error) {
	if object.GetNamespace() == pool.Namespace {
		return externalCoreResourceOwned(pool, cfg, object), nil
	}
	intents, err := externalDiscoveryIntents(pool)
	if err != nil {
		return false, err
	}
	for _, intent := range intents {
		if intent.Namespace != object.GetNamespace() || intent.Name != object.GetName() {
			continue
		}
		if intent.CreateIssued && intent.UID == "" {
			return false, fmt.Errorf("discovery resource creation identity is unresolved: %w", workspaceprovider.ErrStaleIdentity)
		}
		return externalDiscoveryIntentMatches(pool, cfg, intent, object), nil
	}
	return false, nil
}
