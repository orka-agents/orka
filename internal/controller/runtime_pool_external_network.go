// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const externalProcessIngressTargetsAnnotation = "orka.ai/external-native-ingress-targets"

// Endpoint targets are fixed before any policy write, including its namespace.
// Policy UIDs remain Kubernetes identities and are fenced on each write/delete.
type externalProcessIngressTarget struct {
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Selector  map[string]string `json:"selector"`
	Port      int32             `json:"port"`
}

//nolint:gocyclo // Accept only the exact frozen endpoint peer/port shapes, excluding DNS.
func externalFrozenIngressTargets(pool *corev1alpha1.RuntimePool, runtime *workspacev1alpha1.RuntimeWorkload) ([]externalProcessIngressTarget, error) {
	if runtime == nil || runtime.NetworkPolicy == nil {
		return nil, workspaceprovider.ErrStaleIdentity
	}
	var controller, proxy *externalProcessIngressTarget
	for _, rule := range runtime.NetworkPolicy.Egress {
		// Core's DNS rule has two ports. The two endpoint rules each have one
		// exact TCP port and one namespace/Pod peer; no observed policy is input.
		if len(rule.Ports) != 1 || len(rule.To) != 1 {
			continue
		}
		peer, port := rule.To[0], rule.Ports[0]
		if peer.IPBlock != nil || peer.NamespaceSelector == nil || peer.PodSelector == nil ||
			len(peer.NamespaceSelector.MatchExpressions) != 0 || len(peer.NamespaceSelector.MatchLabels) != 1 ||
			peer.NamespaceSelector.MatchLabels[corev1.LabelMetadataName] == "" ||
			len(peer.PodSelector.MatchExpressions) != 0 || len(peer.PodSelector.MatchLabels) == 0 ||
			port.Protocol == nil || *port.Protocol != corev1.ProtocolTCP || port.Port == nil ||
			port.Port.Type != intstr.Int || port.Port.IntVal < 1 || port.Port.IntVal > 65535 || port.EndPort != nil {
			return nil, workspaceprovider.ErrStaleIdentity
		}
		target := &externalProcessIngressTarget{Namespace: peer.NamespaceSelector.MatchLabels[corev1.LabelMetadataName], Selector: cloneStringMap(peer.PodSelector.MatchLabels), Port: port.Port.IntVal}
		if reflect.DeepEqual(target.Selector, map[string]string{runtimePoolNetworkRoleLabel: controllerNameValue}) {
			if controller != nil {
				return nil, workspaceprovider.ErrStaleIdentity
			}
			target.Name = runtimePoolChildName(runtimePoolResourceName(pool.Namespace, pool.Name), "external-controller-ingress")
			controller = target
		} else {
			if proxy != nil {
				return nil, workspaceprovider.ErrStaleIdentity
			}
			target.Name = runtimePoolChildName(runtimePoolResourceName(pool.Namespace, pool.Name), "external-provider-ingress")
			proxy = target
		}
	}
	if controller == nil || proxy == nil {
		return nil, workspaceprovider.ErrStaleIdentity
	}
	return []externalProcessIngressTarget{*proxy, *controller}, nil
}

func externalSavedIngressTargets(pool *corev1alpha1.RuntimePool) ([]externalProcessIngressTarget, error) {
	encoded := pool.Annotations[externalProcessIngressTargetsAnnotation]
	if encoded == "" {
		return nil, nil
	}
	var targets []externalProcessIngressTarget
	if err := json.Unmarshal([]byte(encoded), &targets); err != nil || len(targets) != 2 {
		return nil, workspaceprovider.ErrStaleIdentity
	}
	for i, suffix := range []string{"external-provider-ingress", "external-controller-ingress"} {
		target := targets[i]
		if target.Namespace == "" || target.Name != runtimePoolChildName(runtimePoolResourceName(pool.Namespace, pool.Name), suffix) || len(target.Selector) == 0 || target.Port < 1 || target.Port > 65535 {
			return nil, workspaceprovider.ErrStaleIdentity
		}
	}
	return targets, nil
}

func (r *RuntimePoolReconciler) freezeExternalIngressTargets(ctx context.Context, pool *corev1alpha1.RuntimePool, runtime *workspacev1alpha1.RuntimeWorkload) ([]externalProcessIngressTarget, error) {
	desired, err := externalFrozenIngressTargets(pool, runtime)
	if err != nil {
		return nil, err
	}
	if saved, err := externalSavedIngressTargets(pool); err != nil || saved != nil {
		if err == nil && !reflect.DeepEqual(saved, desired) {
			err = workspaceprovider.ErrStaleIdentity
		}
		return saved, err
	}
	encoded, err := json.Marshal(desired)
	if err != nil {
		return nil, err
	}
	before := pool.DeepCopy()
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	pool.Annotations[externalProcessIngressTargetsAnnotation] = string(encoded)
	if err := r.Patch(ctx, pool, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return nil, err
	}
	return desired, nil
}

func externalWorkerLabelsBindIdentity(labels map[string]string, identity workspacev1alpha1.InstanceIdentity) bool {
	allocation, instance := false, false
	for _, value := range labels {
		allocation = allocation || value == identity.AllocationID
		instance = instance || value == identity.InstanceID
	}
	return identity.Valid() && allocation && instance
}

func (r *RuntimePoolReconciler) externalUniqueIngressWorker(ctx context.Context, process *workspacev1alpha1.NativeProcessEvidence, identity workspacev1alpha1.InstanceIdentity) (*corev1.Pod, error) {
	reader := uncachedReader(r.APIReader, r.Client)
	worker := &corev1.Pod{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: process.Worker.Namespace, Name: process.Worker.Name}, worker); err != nil {
		return nil, err
	}
	if worker.UID != process.Worker.UID || !worker.DeletionTimestamp.IsZero() || !externalWorkerLabelsBindIdentity(worker.Labels, identity) {
		return nil, workspaceprovider.ErrStaleIdentity
	}
	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods, client.InNamespace(worker.Namespace), client.MatchingLabels(worker.Labels)); err != nil {
		return nil, err
	}
	if len(pods.Items) != 1 || pods.Items[0].UID != worker.UID || pods.Items[0].Name != worker.Name {
		return nil, fmt.Errorf("native ingress selector does not identify only the attested worker: %w", workspaceprovider.ErrStaleIdentity)
	}
	current := &corev1.Pod{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(worker), current); err != nil {
		return nil, err
	}
	if current.UID != worker.UID || current.ResourceVersion != worker.ResourceVersion || !current.DeletionTimestamp.IsZero() || !reflect.DeepEqual(current.Labels, worker.Labels) {
		return nil, workspaceprovider.ErrStaleIdentity
	}
	return worker, nil
}

func externalIngressPolicySpec(target externalProcessIngressTarget, workerNamespace string, workerLabels map[string]string) networkingv1.NetworkPolicySpec {
	return networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: cloneStringMap(target.Selector)}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: workerNamespace}}, PodSelector: &metav1.LabelSelector{MatchLabels: cloneStringMap(workerLabels)}}}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: new(corev1.ProtocolTCP), Port: new(intstr.FromInt32(target.Port))}}}}}
}

func externalIngressPolicyMatchesIdentity(policy *networkingv1.NetworkPolicy, target externalProcessIngressTarget, binding *externalRuntimeInstanceEvidence) bool {
	if binding == nil || !binding.NativeProcess || len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].From) != 1 {
		return false
	}
	peer := policy.Spec.Ingress[0].From[0]
	if peer.PodSelector == nil || len(peer.PodSelector.MatchExpressions) != 0 || !externalWorkerLabelsBindIdentity(peer.PodSelector.MatchLabels, binding.Identity) {
		return false
	}
	return reflect.DeepEqual(policy.Spec, externalIngressPolicySpec(target, binding.Pod.Namespace, peer.PodSelector.MatchLabels))
}

func externalCorePolicyOwned(pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, policy *networkingv1.NetworkPolicy) bool {
	for key, value := range cfg.labels {
		if policy.Labels[key] != value {
			return false
		}
	}
	if policy.Namespace != pool.Namespace {
		return len(policy.OwnerReferences) == 0
	}
	owner := metav1.GetControllerOf(policy)
	return len(policy.OwnerReferences) == 1 && owner != nil && owner.APIVersion == corev1alpha1.GroupVersion.String() && owner.Kind == "RuntimePool" && owner.Name == pool.Name && owner.UID == pool.UID
}

func (r *RuntimePoolReconciler) ensureExternalIngressPolicy(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, target externalProcessIngressTarget, worker *corev1.Pod) error {
	policy := &networkingv1.NetworkPolicy{}
	key := types.NamespacedName{Namespace: target.Namespace, Name: target.Name}
	desired := externalIngressPolicySpec(target, worker.Namespace, worker.Labels)
	err := uncachedReader(r.APIReader, r.Client).Get(ctx, key, policy)
	if apierrors.IsNotFound(err) {
		policy = &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: cloneStringMap(cfg.labels)}, Spec: desired}
		if err := r.setRuntimePoolControllerReference(pool, policy); err != nil {
			return err
		}
		return r.Create(ctx, policy)
	}
	if err != nil {
		return err
	}
	if !policy.DeletionTimestamp.IsZero() || !externalCorePolicyOwned(pool, cfg, policy) {
		return workspaceprovider.ErrStaleIdentity
	}
	if reflect.DeepEqual(policy.Spec, desired) {
		return nil
	}
	return workspaceprovider.ErrStaleIdentity
}

func (r *RuntimePoolReconciler) deleteExternalIngressPolicies(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, targets []externalProcessIngressTarget, binding *externalRuntimeInstanceEvidence) (bool, error) {
	remaining := false
	for _, target := range targets {
		policy := &networkingv1.NetworkPolicy{}
		if err := uncachedReader(r.APIReader, r.Client).Get(ctx, types.NamespacedName{Namespace: target.Namespace, Name: target.Name}, policy); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		if !externalCorePolicyOwned(pool, cfg, policy) {
			// Another object's labels never authorize its deletion or block ours.
			continue
		}
		if !externalIngressPolicyMatchesIdentity(policy, target, binding) {
			return false, workspaceprovider.ErrStaleIdentity
		}
		pending, err := r.deleteExternalCorePolicy(ctx, policy)
		if err != nil {
			return false, err
		}
		remaining = remaining || pending
	}
	return remaining, nil
}

func (r *RuntimePoolReconciler) deleteExternalCorePolicy(ctx context.Context, policy *networkingv1.NetworkPolicy) (bool, error) {
	if err := r.Delete(ctx, policy, deleteCurrentObjectPreconditions(policy)...); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	current := &networkingv1.NetworkPolicy{}
	if err := uncachedReader(r.APIReader, r.Client).Get(ctx, client.ObjectKeyFromObject(policy), current); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	// A same-name replacement is not the policy whose deletion was authorized.
	return current.UID == policy.UID, nil
}

func (r *RuntimePoolReconciler) deleteExternalCoreNetworkPolicies(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, w *workspacev1alpha1.ExecutionWorkspace) (bool, error) {
	var frozenTargets []externalProcessIngressTarget
	if w.Spec.Workload != nil {
		if err := workspaceprovider.ValidateWorkspaceWorkload(w); err != nil || w.Spec.Workload.Runtime == nil || w.Spec.Workload.Runtime.Template.Namespace == "" {
			return false, workspaceprovider.ErrStaleIdentity
		}
		var err error
		frozenTargets, err = externalFrozenIngressTargets(pool, w.Spec.Workload.Runtime)
		if err != nil {
			return false, err
		}
		cfg.namespace = w.Spec.Workload.Runtime.Template.Namespace
		cfg.providerProxy.namespace, cfg.providerProxy.podLabels, cfg.providerProxy.port = frozenTargets[0].Namespace, frozenTargets[0].Selector, frozenTargets[0].Port
	}
	policyReconciler := &RuntimePoolReconciler{ControllerNamespace: r.ControllerNamespace, ControllerAPIPort: r.ControllerAPIPort}
	if len(frozenTargets) == 2 {
		policyReconciler.ControllerNamespace, policyReconciler.ControllerAPIPort = frozenTargets[1].Namespace, frozenTargets[1].Port
	}
	remaining := false
	for _, expected := range policyReconciler.runtimePoolNetworkPolicies(cfg) {
		policy := &networkingv1.NetworkPolicy{}
		if err := uncachedReader(r.APIReader, r.Client).Get(ctx, client.ObjectKeyFromObject(&expected), policy); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		if !externalCorePolicyOwned(pool, cfg, policy) {
			continue
		}
		if w.Spec.Workload != nil && !reflect.DeepEqual(policy.Spec, expected.Spec) {
			return false, workspaceprovider.ErrStaleIdentity
		}
		pending, err := r.deleteExternalCorePolicy(ctx, policy)
		if err != nil {
			return false, err
		}
		remaining = remaining || pending
	}
	binding, err := externalRuntimeEvidence(pool)
	if err != nil {
		return false, err
	}
	if binding == nil || !binding.NativeProcess {
		return remaining, nil
	}
	targets, err := externalSavedIngressTargets(pool)
	if err != nil {
		return false, err
	}
	if targets == nil {
		// Legacy native pools can use their immutable published target intent.
		targets = frozenTargets
	}
	pending, err := r.deleteExternalIngressPolicies(ctx, pool, cfg, targets, binding)
	return remaining || pending, err
}
