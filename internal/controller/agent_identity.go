// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/store"
)

// errIdentityClaimPending marks a Task waiting for its Agent to hold the
// identity it declares; the binding retries instead of failing.
var errIdentityClaimPending = errors.New("the Agent does not hold its identity yet")

func identityClaimMatches(claim *corev1alpha1.AgentIdentityClaim, issuer, id string) bool {
	return claim != nil && claim.Issuer == issuer && claim.ID == id
}

// identityClaimBindable reports whether new Task bindings may use the claim:
// it must name the identity and not be on its way out.
func identityClaimBindable(claim *corev1alpha1.AgentIdentityClaim, issuer, id string) bool {
	return identityClaimMatches(claim, issuer, id) && !claim.Releasing
}

// validateAgentIdentity refuses an identity this controller cannot honor.
// Ownership between Agents is decided by reconcileIdentityClaim.
func (r *AgentReconciler) validateAgentIdentity(_ context.Context, agent *corev1alpha1.Agent) error {
	return r.Identity.ValidateAgent(agent)
}

// reconcileIdentityClaim decides the identity claim to record for the Agent.
// A claim is granted only when no other Agent holds it, so two Agents never
// act as one identity at the same time. Agent reconciles run one at a time
// and read live state, so two Agents cannot claim at once.
//
// A claim the spec no longer names is released in two passes. The first only
// marks it releasing, which every new binding and every unstarted Task
// re-checking after its binding refuses. A later pass releases it once a live
// scan finds no unfinished Task bound to it: a binding that passed its check
// before the mark was persisted before it, so that scan sees it.
func (r *AgentReconciler) reconcileIdentityClaim(ctx context.Context, agent *corev1alpha1.Agent) (*corev1alpha1.AgentIdentityClaim, error) {
	reader := uncachedReader(r.APIReader, r.Client)
	// Start from the live claim: a cached copy can still show a claim this
	// Agent already released to another Agent.
	live := &corev1alpha1.Agent{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: agent.Name}, live); err != nil {
		return agent.Status.IdentityClaim.DeepCopy(), fmt.Errorf("read the Agent's live identity claim: %w", err)
	}
	if live.UID != agent.UID {
		return nil, fmt.Errorf("the Agent was replaced while its identity claim was being decided")
	}
	agent.Status.IdentityClaim = live.Status.IdentityClaim.DeepCopy()
	claim := live.Status.IdentityClaim.DeepCopy()
	issuer, id, declared := live.IdentityRef()
	if claim != nil && identityClaimMatches(claim, issuer, id) {
		// The spec names the held identity again; it is no longer leaving.
		claim.Releasing = false
	}
	if claim != nil && !identityClaimMatches(claim, issuer, id) {
		if !claim.Releasing {
			claim.Releasing = true
			return claim, errIdentityClaimReleasing(claim, declared)
		}
		bound, err := activeIdentityTasks(ctx, reader, agent, *claim)
		if err != nil {
			return claim, err
		}
		if bound > 0 {
			if declared {
				return claim, fmt.Errorf("identity %s/%s stays with this Agent until %d Tasks bound to it finish",
					claim.Issuer, claim.ID, bound)
			}
			return claim, nil
		}
		claim = nil
	}
	if !declared || claim != nil {
		return claim, nil
	}
	agents := &corev1alpha1.AgentList{}
	// The controller watches one namespace and holds Agent RBAC only there,
	// so identity ownership is decided within it.
	if err := reader.List(ctx, agents, client.InNamespace(agent.Namespace)); err != nil {
		return nil, fmt.Errorf("list Agents holding identity %s/%s: %w", issuer, id, err)
	}
	for i := range agents.Items {
		other := &agents.Items[i]
		if other.UID != agent.UID && identityClaimMatches(other.Status.IdentityClaim, issuer, id) {
			return nil, fmt.Errorf("identity %s/%s is already bound to Agent %s/%s", issuer, id, other.Namespace, other.Name)
		}
	}
	return &corev1alpha1.AgentIdentityClaim{Issuer: issuer, ID: id}, nil
}

// errIdentityClaimReleasing reports the first release pass. It is an error only
// when the Agent now names another identity it cannot use yet.
func errIdentityClaimReleasing(claim *corev1alpha1.AgentIdentityClaim, declared bool) error {
	if !declared {
		return nil
	}
	return fmt.Errorf("identity %s/%s is being released before this Agent can claim its new identity", claim.Issuer, claim.ID)
}

// activeIdentityTasks counts unfinished Tasks bound to this Agent as claim.
func activeIdentityTasks(ctx context.Context, reader client.Reader, agent *corev1alpha1.Agent, claim corev1alpha1.AgentIdentityClaim) (int, error) {
	tasks := &corev1alpha1.TaskList{}
	if err := reader.List(ctx, tasks, client.InNamespace(agent.Namespace)); err != nil {
		return 0, fmt.Errorf("list Tasks bound to identity %s/%s: %w", claim.Issuer, claim.ID, err)
	}
	count := 0
	for i := range tasks.Items {
		task := &tasks.Items[i]
		binding := task.Status.AgentExecutionBinding
		if isTerminalTaskPhase(task.Status.Phase) || binding == nil || binding.Agent == nil ||
			binding.Agent.UID != agent.UID || binding.Agent.Identity == nil {
			continue
		}
		if binding.Agent.Identity.Issuer == claim.Issuer && binding.Agent.Identity.ID == claim.ID {
			count++
		}
	}
	return count, nil
}

// agentIdentityReverifyInterval bounds how long a verified identity stays
// trusted without asking its issuer again.
const agentIdentityReverifyInterval = time.Hour

const (
	reasonIdentityVerified           = "Verified"
	reasonIdentityVerificationFailed = "VerificationFailed"
)

// agentIdentityState is the outcome of verifying an Agent's identity.
type agentIdentityState struct {
	declared bool
	verified bool
	err      error
}

// verifyAgentIdentity asks the issuer to prove a declared identity. A
// configuration that already failed validation is not sent to the issuer.
func (r *AgentReconciler) verifyAgentIdentity(ctx context.Context, agent *corev1alpha1.Agent, validationErr error) agentIdentityState {
	issuer, id, ok := agent.IdentityRef()
	if !ok {
		return agentIdentityState{}
	}
	state := agentIdentityState{declared: true}
	if validationErr != nil || r.IdentityVerifier == nil {
		return state
	}
	if err := r.IdentityVerifier.Verify(ctx, agent); err != nil {
		state.err = fmt.Errorf("identity %s/%s failed verification: %w", issuer, id, err)
		return state
	}
	state.verified = true
	return state
}

// setAgentIdentityCondition reports whether a declared identity is verified
// with its issuer, and clears the condition when there is none.
func setAgentIdentityCondition(agent *corev1alpha1.Agent, now metav1.Time, identity agentIdentityState) {
	if !identity.declared {
		meta.RemoveStatusCondition(&agent.Status.Conditions, conditionTypeIdentityVerified)
		return
	}
	condition := metav1.Condition{
		Type:               conditionTypeIdentityVerified,
		Status:             metav1.ConditionFalse,
		Reason:             reasonIdentityDeclared,
		Message:            "declared by an authorized Agent editor; not verified with the issuer",
		LastTransitionTime: now,
		ObservedGeneration: agent.Generation,
	}
	switch {
	case identity.err != nil:
		condition.Reason = reasonIdentityVerificationFailed
		condition.Message = identity.err.Error()
	case identity.verified:
		condition.Status = metav1.ConditionTrue
		condition.Reason = reasonIdentityVerified
		condition.Message = "the issuer issued a token for this identity"
	}
	meta.SetStatusCondition(&agent.Status.Conditions, condition)
}

// agentsSharingIdentity re-validates every other Agent that declares or holds
// an identity the changed Agent declares or holds, so a released claim is
// picked up by the next Agent waiting for it.
func (r *AgentReconciler) agentsSharingIdentity(ctx context.Context, obj client.Object) []reconcile.Request {
	changed, ok := obj.(*corev1alpha1.Agent)
	if !ok {
		return nil
	}
	identities := map[string]bool{}
	if issuer, id, ok := changed.IdentityRef(); ok {
		identities[issuer+"/"+id] = true
	}
	if claim := changed.Status.IdentityClaim; claim != nil {
		identities[claim.Issuer+"/"+claim.ID] = true
	}
	if len(identities) == 0 {
		return nil
	}
	agents := &corev1alpha1.AgentList{}
	if err := r.List(ctx, agents, client.InNamespace(changed.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list Agents sharing an identity", "agent", changed.Name)
		return nil
	}
	var requests []reconcile.Request
	for i := range agents.Items {
		other := &agents.Items[i]
		if other.UID == changed.UID {
			continue
		}
		match := false
		if issuer, id, ok := other.IdentityRef(); ok && identities[issuer+"/"+id] {
			match = true
		}
		if claim := other.Status.IdentityClaim; claim != nil && identities[claim.Issuer+"/"+claim.ID] {
			match = true
		}
		if match {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: other.Namespace, Name: other.Name}})
		}
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
	issuer, id, ok := agent.IdentityRef()
	if !ok {
		return nil, nil
	}
	if err := r.Identity.ValidateAgent(agent); err != nil {
		return nil, permanentACPAgentConfiguration(err)
	}
	reader := uncachedReader(r.APIReader, r.Client)
	// Judge the Agent's live claim, not the copy the Task resolved earlier.
	live := &corev1alpha1.Agent{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: agent.Name}, live); err != nil {
		return nil, fmt.Errorf("resolve the Agent's identity claim: %w", err)
	}
	if live.UID != agent.UID || !identityClaimBindable(live.Status.IdentityClaim, issuer, id) {
		return nil, fmt.Errorf("identity %s/%s: %w", issuer, id, errIdentityClaimPending)
	}
	if parentName := labels.ParentTaskName(task.Labels, task.Annotations); parentName != "" {
		if err := requireParentOwnsIdentityAgent(ctx, reader, task, parentName, agent); err != nil {
			return nil, err
		}
	}
	return r.Identity.BindingRef(agent)
}

// requireParentOwnsIdentityAgent lets a delegated child Task use an
// identity-bound Agent only when the parent is bound to that exact Agent and
// identity, so a child can never borrow an identity its root requester was not
// checked against, including through an Agent recreated under the same name.
func requireParentOwnsIdentityAgent(
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
	refuse := permanentACPAgentConfiguration(fmt.Errorf(
		"a delegated child Task may use identity-bound Agent %q only when it is the parent's own bound Agent", agent.Name))
	binding := parent.Status.AgentExecutionBinding
	if binding == nil || binding.Agent == nil || binding.Agent.UID != agent.UID || binding.Agent.Identity == nil {
		return refuse
	}
	issuer, id, _ := agent.IdentityRef()
	if binding.Agent.Identity.Issuer != issuer || binding.Agent.Identity.ID != id {
		return refuse
	}
	return nil
}

// agentIdentityBindingFromTask returns the identity frozen into an ACP Task's
// binding, for agentIdentity-mode outbound access, with what Delegated mode
// needs: when the root requester last acted, and whether the Task runs where
// Orka controls the network.
func agentIdentityBindingFromTask(ctx context.Context, reader client.Reader, task *corev1alpha1.Task) (*outboundaccess.AgentIdentityBinding, error) {
	if task == nil || task.Status.AgentExecutionBinding == nil || task.Status.AgentExecutionBinding.Agent == nil ||
		task.Status.AgentExecutionBinding.Agent.Identity == nil {
		return nil, nil
	}
	binding := task.Status.AgentExecutionBinding
	agent := binding.Agent
	start, err := delegationStart(ctx, reader, task)
	if err != nil {
		return nil, err
	}
	workspaceBacked := task.Spec.Execution != nil && task.Spec.Execution.Workspace != nil
	return &outboundaccess.AgentIdentityBinding{
		Identity: *agent.Identity, AgentNamespace: agent.Namespace, AgentName: agent.Name,
		AgentUID: string(agent.UID), AgentGeneration: agent.Generation,
		TaskNamespace: task.Namespace, TaskName: task.Name, TaskUID: string(task.UID),
		DelegationStart: start,
		ManagedEgress:   binding.Backend == corev1alpha1.AgentExecutionBackendRuntimePool && !workspaceBacked,
	}, nil
}

// maxDelegationChainDepth bounds the parent walk to the root requester's Task.
const maxDelegationChainDepth = 32

// delegationStart returns when the root requester last acted in the Task
// chain: the creation of the chain's root Task, which the requester created
// directly or with a Session message. Child Tasks, retries, and agents woken
// later never move it. A broken chain returns the zero time, which Delegated
// mode refuses.
func delegationStart(ctx context.Context, reader client.Reader, task *corev1alpha1.Task) (time.Time, error) {
	current := task
	for range maxDelegationChainDepth {
		parentName := labels.ParentTaskName(current.Labels, current.Annotations)
		if parentName == "" {
			return current.CreationTimestamp.Time, nil
		}
		if reader == nil {
			return time.Time{}, nil
		}
		parent := &corev1alpha1.Task{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: current.Namespace, Name: parentName}, parent); err != nil {
			if apierrors.IsNotFound(err) {
				return time.Time{}, nil
			}
			return time.Time{}, fmt.Errorf("resolve the delegating Task chain: %w", err)
		}
		current = parent
	}
	return time.Time{}, nil
}

// requireBoundIdentityClaim re-checks, after the binding is persisted and
// until the Task starts, that the Agent still holds the identity the binding
// froze. A claim released between the binding decision and its write could
// otherwise leave two Agents with Tasks acting as one identity; once the
// binding is persisted, any later release sees it and keeps the claim.
func (r *TaskReconciler) requireBoundIdentityClaim(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) (ctrl.Result, error, bool) {
	if task.Status.Execution != nil || binding == nil || binding.Agent == nil || binding.Agent.Identity == nil {
		return ctrl.Result{}, nil, false
	}
	frozen := binding.Agent
	live := &corev1alpha1.Agent{}
	err := uncachedReader(r.APIReader, r.Client).Get(ctx, types.NamespacedName{Namespace: frozen.Namespace, Name: frozen.Name}, live)
	if err != nil && !apierrors.IsNotFound(err) {
		log.FromContext(ctx).Error(err, "Agent identity claim check failed")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
	}
	if err == nil && live.UID == frozen.UID && identityClaimBindable(live.Status.IdentityClaim, frozen.Identity.Issuer, frozen.Identity.ID) {
		return ctrl.Result{}, nil, false
	}
	result, failErr := r.failACPPlanningTask(ctx, task, corev1alpha1.TaskExecutionReason("IdentityUnavailable"),
		fmt.Sprintf("Agent %s/%s no longer holds identity %s/%s", frozen.Namespace, frozen.Name, frozen.Identity.Issuer, frozen.Identity.ID))
	return result, failErr, true
}

// agentIdentityReleaseRecheck is how soon a releasing claim is checked again.
const agentIdentityReleaseRecheck = 10 * time.Second

// classificationActsForRequester reports whether a frozen connector
// classification includes a Tool that acts for the requester through the
// Agent's identity.
func classificationActsForRequester(classified map[string]connectorToolInfo) bool {
	for _, info := range classified {
		if info.Delegated {
			return true
		}
	}
	return false
}

// DelegationLiveness checks that a Delegated call's Task is still running,
// with no cancellation requested, and that its Session still exists, reading
// both live. Stopping a Session's turn cancels its Task, so the Task check
// covers it.
type DelegationLiveness struct {
	Reader   client.Reader
	Sessions store.SessionStore
}

// requireLiveAncestors refuses a delegated call from a child Task once any
// Task above it in the delegation chain was cancelled, had its cancellation
// requested, is being deleted, or is gone: stopping a parent stops every
// child acting for the same requester. A parent that finished normally does
// not stop its children.
func (d DelegationLiveness) requireLiveAncestors(ctx context.Context, task *corev1alpha1.Task) error {
	current := task
	for range maxDelegationChainDepth {
		parentName := labels.ParentTaskName(current.Labels, current.Annotations)
		if parentName == "" {
			return nil
		}
		parent := &corev1alpha1.Task{}
		if err := d.Reader.Get(ctx, types.NamespacedName{Namespace: current.Namespace, Name: parentName}, parent); err != nil {
			if apierrors.IsNotFound(err) {
				return errors.New("a Task it was delegated from is gone")
			}
			return fmt.Errorf("read the delegating Task: %w", err)
		}
		if owner := metav1.GetControllerOf(current); owner != nil && owner.UID != parent.UID {
			return errors.New("a Task it was delegated from was replaced")
		}
		switch {
		case !parent.DeletionTimestamp.IsZero():
			return errors.New("a Task it was delegated from is being deleted")
		case parent.Status.Phase == corev1alpha1.TaskPhaseCancelled:
			return errors.New("a Task it was delegated from was cancelled")
		}
		if _, requested := parent.Annotations[labels.AnnotationCancelRequested]; requested {
			return errors.New("the cancellation of a Task it was delegated from was requested")
		}
		current = parent
	}
	return errors.New("the delegating Task chain is too deep")
}

// Check implements outboundaccess.KubernetesResolver.DelegationLive.
func (d DelegationLiveness) Check(ctx context.Context, binding outboundaccess.AgentIdentityBinding) error {
	if d.Reader == nil {
		return errors.New("no Task reader is configured")
	}
	task := &corev1alpha1.Task{}
	if err := d.Reader.Get(ctx, types.NamespacedName{Namespace: binding.TaskNamespace, Name: binding.TaskName}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return errors.New("the Task is gone")
		}
		return fmt.Errorf("read the Task: %w", err)
	}
	switch {
	case string(task.UID) != binding.TaskUID:
		return errors.New("the Task was replaced")
	case !task.DeletionTimestamp.IsZero():
		return errors.New("the Task is being deleted")
	case isTerminalTaskPhase(task.Status.Phase):
		return fmt.Errorf("the Task is %s", task.Status.Phase)
	}
	if _, requested := task.Annotations[labels.AnnotationCancelRequested]; requested {
		return errors.New("the Task's cancellation was requested")
	}
	if err := d.requireLiveAncestors(ctx, task); err != nil {
		return err
	}
	if task.Spec.SessionRef == nil || strings.TrimSpace(task.Spec.SessionRef.Name) == "" {
		return nil
	}
	if d.Sessions == nil {
		return errors.New("no Session store is configured")
	}
	session, err := d.Sessions.GetSession(ctx, task.Namespace, task.Spec.SessionRef.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errors.New("the Task's Session was deleted")
		}
		return fmt.Errorf("read the Task's Session: %w", err)
	}
	if session == nil {
		return errors.New("the Task's Session was deleted")
	}
	return nil
}
