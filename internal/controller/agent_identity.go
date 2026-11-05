// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentidentity"
	"github.com/orka-agents/orka/internal/labels"
)

// validateAgentIdentity refuses an identity this controller cannot honor and
// an identity another, older Agent already holds.
func (r *AgentReconciler) validateAgentIdentity(ctx context.Context, agent *corev1alpha1.Agent) error {
	return validateIdentityClaim(ctx, r.Client, r.Identity, agent)
}

func validateIdentityClaim(ctx context.Context, reader client.Reader, config agentidentity.Config, agent *corev1alpha1.Agent) error {
	issuer, id, ok := agent.IdentityRef()
	if !ok {
		return nil
	}
	if err := config.ValidateAgent(agent); err != nil {
		return err
	}
	agents := &corev1alpha1.AgentList{}
	if err := reader.List(ctx, agents); err != nil {
		return fmt.Errorf("list Agents claiming identity %s/%s: %w", issuer, id, err)
	}
	if owner := agentidentity.Claimant(agents.Items, issuer, id); owner != nil && owner.UID != agent.UID {
		return fmt.Errorf("identity %s/%s is already bound to Agent %s/%s", issuer, id, owner.Namespace, owner.Name)
	}
	return nil
}

// setAgentIdentityCondition reports that a declared identity is not yet
// verified with its issuer, and clears the condition when there is none.
func setAgentIdentityCondition(agent *corev1alpha1.Agent, now metav1.Time) {
	if _, _, ok := agent.IdentityRef(); !ok {
		meta.RemoveStatusCondition(&agent.Status.Conditions, conditionTypeIdentityVerified)
		return
	}
	meta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type:               conditionTypeIdentityVerified,
		Status:             metav1.ConditionFalse,
		Reason:             reasonIdentityDeclared,
		Message:            "declared by an authorized Agent editor; not verified with the issuer",
		LastTransitionTime: now,
		ObservedGeneration: agent.Generation,
	})
}

// agentsSharingIdentity re-validates every other Agent claiming the same
// identity as the changed one.
func (r *AgentReconciler) agentsSharingIdentity(ctx context.Context, obj client.Object) []reconcile.Request {
	changed, ok := obj.(*corev1alpha1.Agent)
	if !ok {
		return nil
	}
	issuer, id, ok := changed.IdentityRef()
	if !ok {
		return nil
	}
	agents := &corev1alpha1.AgentList{}
	if err := r.List(ctx, agents); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list Agents sharing an identity", "agent", changed.Name)
		return nil
	}
	var requests []reconcile.Request
	for i := range agents.Items {
		other := &agents.Items[i]
		otherIssuer, otherID, ok := other.IdentityRef()
		if !ok || otherIssuer != issuer || otherID != id || other.UID == changed.UID {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: other.Namespace, Name: other.Name}})
	}
	return requests
}

// agentExecutionIdentity returns the identity a Task binding freezes for an
// identity-bound Agent, and refuses a binding the identity rules forbid.
func (r *TaskReconciler) agentExecutionIdentity(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) (*corev1alpha1.AgentExecutionIdentityRef, error) {
	if _, _, ok := agent.IdentityRef(); !ok {
		return nil, nil
	}
	reader := uncachedReader(r.APIReader, r.Client)
	if err := r.Identity.ValidateAgent(agent); err != nil {
		return nil, permanentACPAgentConfiguration(err)
	}
	agents := &corev1alpha1.AgentList{}
	if err := reader.List(ctx, agents); err != nil {
		return nil, fmt.Errorf("list Agents claiming the identity: %w", err)
	}
	issuer, id, _ := agent.IdentityRef()
	if owner := agentidentity.Claimant(agents.Items, issuer, id); owner == nil || owner.UID != agent.UID {
		return nil, permanentACPAgentConfiguration(fmt.Errorf("identity %s/%s is bound to another Agent", issuer, id))
	}
	if parentName := labels.ParentTaskName(task.Labels, task.Annotations); parentName != "" {
		if err := r.requireParentOwnsIdentityAgent(ctx, reader, task, parentName, agent); err != nil {
			return nil, err
		}
	}
	return r.Identity.BindingRef(agent)
}

// requireParentOwnsIdentityAgent lets a delegated child Task use an
// identity-bound Agent only when it is the parent's own Agent, so a child can
// never borrow an identity its root requester was not checked against.
func (r *TaskReconciler) requireParentOwnsIdentityAgent(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	parentName string,
	agent *corev1alpha1.Agent,
) error {
	parent := &corev1alpha1.Task{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: parentName}, parent); err != nil {
		if apierrors.IsNotFound(err) {
			return permanentACPAgentConfiguration(fmt.Errorf("the parent Task of an identity-bound Agent Task is gone"))
		}
		return fmt.Errorf("resolve the parent Task: %w", err)
	}
	if parent.Spec.AgentRef == nil || parent.Spec.AgentRef.Name != agent.Name ||
		agentRefNamespaceOf(parent.Spec.AgentRef, parent.Namespace) != agent.Namespace {
		return permanentACPAgentConfiguration(fmt.Errorf(
			"a delegated child Task may use identity-bound Agent %q only when it is the parent's own Agent", agent.Name))
	}
	return nil
}

func agentRefNamespaceOf(ref *corev1alpha1.AgentReference, fallback string) string {
	if ref != nil && ref.Namespace != "" {
		return ref.Namespace
	}
	return fallback
}
