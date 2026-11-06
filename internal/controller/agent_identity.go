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
	"github.com/orka-agents/orka/internal/gateway"
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

// identityClaimCoversTask reports whether the Task was created strictly after
// the claim was granted. Only such a Task was checked for Agent use while the
// Agent named the identity and the check was enforced (ADR 0039, stage 1).
// Both times have one-second precision, so a Task created in the grant's
// second may predate the grant and is refused.
func identityClaimCoversTask(claim *corev1alpha1.AgentIdentityClaim, task *corev1alpha1.Task) bool {
	return identityClaimCoversCreation(claim, task.CreationTimestamp)
}

// identityClaimCoversCreation reports whether an object created at created
// was created strictly after the claim was granted.
func identityClaimCoversCreation(claim *corev1alpha1.AgentIdentityClaim, created metav1.Time) bool {
	return claim != nil && claim.GrantedAt != nil && created.After(claim.GrantedAt.Time)
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
	if declared && !r.Identity.AgentUseAuthorization {
		// Claims exist only while Agent use authorization is enforced; without
		// it a held claim is released like one the spec no longer names.
		issuer, id, declared = "", "", false
	}
	if claim != nil && identityClaimMatches(claim, issuer, id) && (claim.Releasing || claim.GrantedAt == nil) {
		// The spec names the held identity again, or use authorization is back
		// on. Tasks created while it was leaving were not checked for it, so
		// the claim counts as granted now.
		claim.Releasing = false
		claim.GrantedAt = identityClaimGrantTime()
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
	// A deleted Agent's claim goes with it, but its bound Tasks still hold the
	// identity until they finish, so it stays taken until then.
	if holder, err := identityTaskOfOtherAgent(ctx, reader, agent, issuer, id); err != nil || holder != nil {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("identity %s/%s is still held by Task %s/%s of Agent %s until it finishes",
			issuer, id, holder.Namespace, holder.Name, holder.Status.AgentExecutionBinding.Agent.Name)
	}
	return &corev1alpha1.AgentIdentityClaim{Issuer: issuer, ID: id, GrantedAt: identityClaimGrantTime()}, nil
}

// identityClaimGrantTime is the grant time recorded now, at the second
// precision the API server stores it with.
func identityClaimGrantTime() *metav1.Time {
	now := metav1.Now().Rfc3339Copy()
	return &now
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
		if bound := unfinishedIdentityBinding(&tasks.Items[i], claim.Issuer, claim.ID); bound != nil && bound.UID == agent.UID {
			count++
		}
	}
	return count, nil
}

// identityTaskOfOtherAgent returns an unfinished Task bound to the identity
// through any Agent UID other than agent's, such as a deleted Agent's.
func identityTaskOfOtherAgent(ctx context.Context, reader client.Reader, agent *corev1alpha1.Agent, issuer, id string) (*corev1alpha1.Task, error) {
	tasks := &corev1alpha1.TaskList{}
	if err := reader.List(ctx, tasks, client.InNamespace(agent.Namespace)); err != nil {
		return nil, fmt.Errorf("list Tasks bound to identity %s/%s: %w", issuer, id, err)
	}
	for i := range tasks.Items {
		if bound := unfinishedIdentityBinding(&tasks.Items[i], issuer, id); bound != nil && bound.UID != agent.UID {
			return &tasks.Items[i], nil
		}
	}
	return nil, nil
}

// unfinishedIdentityBinding returns the Agent an unfinished Task is bound to
// when that binding froze the identity.
func unfinishedIdentityBinding(task *corev1alpha1.Task, issuer, id string) *corev1alpha1.AgentExecutionAgentRef {
	binding := task.Status.AgentExecutionBinding
	if isTerminalTaskPhase(task.Status.Phase) || binding == nil || binding.Agent == nil || binding.Agent.Identity == nil ||
		binding.Agent.Identity.Issuer != issuer || binding.Agent.Identity.ID != id {
		return nil
	}
	return binding.Agent
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
	if !identityClaimCoversTask(live.Status.IdentityClaim, task) {
		return nil, permanentACPAgentConfiguration(fmt.Errorf(
			"the Task was created before Agent %q was granted identity %s/%s, so its creation was not checked for it; create the Task again",
			agent.Name, issuer, id))
	}
	ref, err := r.Identity.BindingRef(agent)
	if err != nil {
		return nil, permanentACPAgentConfiguration(err)
	}
	if err := requireTaskSourceCoveredByClaim(ctx, reader, task, agent, ref, live.Status.IdentityClaim); err != nil {
		return nil, err
	}
	if parentName := labels.ParentTaskName(task.Labels, task.Annotations); parentName != "" {
		if err := requireParentOwnsIdentityAgent(ctx, reader, task, parentName, agent, ref, live.Status.IdentityClaim); err != nil {
			return nil, err
		}
	}
	return ref, nil
}

// requireTaskSourceCoveredByClaim refuses a Task that Orka created from a
// durable object naming the Agent, unless that object was created after the
// claim was granted. Orka creates such Tasks under its own broad Agent use
// grant, so only the object's own creation was checked for the Agent, and an
// object created while the check was off or before the Agent named the
// identity was never checked for it. The source is the Task's controlling
// RepositoryScan or RepositoryMonitor, matched by UID. A GatewayBinding's
// Tasks record the binding only by name, so they cannot be tied to the
// binding the check judged and are refused. Scheduled runs and delegated
// children are judged by requireParentOwnsIdentityAgent.
func requireTaskSourceCoveredByClaim(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	ref *corev1alpha1.AgentExecutionIdentityRef,
	claim *corev1alpha1.AgentIdentityClaim,
) error {
	if task.Labels[gateway.TaskGatewayBindingLabel] != "" || task.Annotations[gateway.TaskGatewayBindingAnnotation] != "" {
		return permanentACPAgentConfiguration(fmt.Errorf(
			"a GatewayBinding's Task cannot use identity-bound Agent %q: the Task does not record which binding created it", agent.Name))
	}
	owner := metav1.GetControllerOf(task)
	if owner == nil || owner.APIVersion != corev1alpha1.GroupVersion.String() {
		return nil
	}
	var source client.Object
	switch owner.Kind {
	case "RepositoryScan":
		source = &corev1alpha1.RepositoryScan{}
	case "RepositoryMonitor":
		source = &corev1alpha1.RepositoryMonitor{}
	default:
		return nil
	}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: owner.Name}, source); err != nil {
		if apierrors.IsNotFound(err) {
			return permanentACPAgentConfiguration(fmt.Errorf("the %s %q that created the Task is gone", owner.Kind, owner.Name))
		}
		return fmt.Errorf("resolve the %s that created the Task: %w", owner.Kind, err)
	}
	if source.GetUID() != owner.UID {
		return permanentACPAgentConfiguration(fmt.Errorf("the %s %q that created the Task was replaced", owner.Kind, owner.Name))
	}
	if !identityClaimCoversCreation(claim, source.GetCreationTimestamp()) {
		return permanentACPAgentConfiguration(fmt.Errorf(
			"%s %q was created before Agent %q was granted identity %s/%s, so its creation was not checked for it; re-create the %s",
			owner.Kind, owner.Name, agent.Name, ref.Issuer, ref.ID, owner.Kind))
	}
	return nil
}

// requireParentOwnsIdentityAgent lets a delegated child Task use an
// identity-bound Agent only when the parent is bound to that exact Agent and
// identity, issuer instance included, so a child can never borrow an identity
// its root requester was not checked against, including through an Agent
// recreated under the same name. The parent must be the Task that controls
// the child, not one recreated under its name. A scheduled run is not
// delegation: it runs its scheduled Task's own Agent, which that Task's
// creator was checked for. Its scheduled Task never binds, so the scheduled
// Task itself must have been created after the claim was granted.
func requireParentOwnsIdentityAgent(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	parentName string,
	agent *corev1alpha1.Agent,
	ref *corev1alpha1.AgentExecutionIdentityRef,
	claim *corev1alpha1.AgentIdentityClaim,
) error {
	parent := &corev1alpha1.Task{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: parentName}, parent); err != nil {
		if apierrors.IsNotFound(err) {
			return permanentACPAgentConfiguration(fmt.Errorf("the parent Task of an identity-bound Agent Task is gone"))
		}
		return fmt.Errorf("resolve the parent Task: %w", err)
	}
	owner := metav1.GetControllerOf(task)
	recorded := strings.TrimSpace(task.Annotations[labels.AnnotationParentTaskUID])
	if owner == nil || owner.Kind != taskResourceKind || owner.APIVersion != corev1alpha1.GroupVersion.String() ||
		owner.Name != parentName || owner.UID != parent.UID || (recorded != "" && recorded != string(parent.UID)) {
		if owner == nil && strings.TrimSpace(task.Annotations[labels.AnnotationForkSourceTask]) != "" {
			// The message only explains the refusal; the decision above
			// never depends on the fork annotation.
			return permanentACPAgentConfiguration(fmt.Errorf(
				"a fork does not carry its source Task's identity-bound Agent %q; create a new Task to use it", agent.Name))
		}
		return permanentACPAgentConfiguration(fmt.Errorf(
			"a child Task may use identity-bound Agent %q only while its parent Task %q still controls it", agent.Name, parentName))
	}
	if parent.Spec.Schedule != "" {
		if parent.Spec.AgentRef == nil || task.Spec.AgentRef == nil || *parent.Spec.AgentRef != *task.Spec.AgentRef {
			return permanentACPAgentConfiguration(fmt.Errorf(
				"a scheduled run may use identity-bound Agent %q only when its scheduled Task names it", agent.Name))
		}
		if !identityClaimCoversTask(claim, parent) {
			return permanentACPAgentConfiguration(fmt.Errorf(
				"scheduled Task %q was created before Agent %q was granted identity %s/%s, so its creation was not checked for it; re-create the scheduled Task",
				parent.Name, agent.Name, ref.Issuer, ref.ID))
		}
		return nil
	}
	binding := parent.Status.AgentExecutionBinding
	if binding == nil || binding.Agent == nil || binding.Agent.UID != agent.UID || binding.Agent.Identity == nil ||
		binding.Agent.Identity.Issuer != ref.Issuer || binding.Agent.Identity.ID != ref.ID ||
		binding.Agent.Identity.Instance != ref.Instance {
		return permanentACPAgentConfiguration(fmt.Errorf(
			"a delegated child Task may use identity-bound Agent %q only when it is the parent's own bound Agent", agent.Name))
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
		TaskCreated:     task.CreationTimestamp.Time,
		DelegationStart: start,
		ManagedEgress:   binding.Backend == corev1alpha1.AgentExecutionBackendRuntimePool && !workspaceBacked,
	}, nil
}

// maxDelegationChainDepth bounds the parent walk to the root requester's Task.
const maxDelegationChainDepth = 32

// delegationStart returns when the root requester last acted in the Task
// chain: the creation of the chain's root Task, which the requester created
// directly or with a Session message. Child Tasks, retries, and agents woken
// later never move it. A chain that cannot be proven (including a fork, which
// names its source without a UID pin), or that passes through a Task acting
// for another requester, returns the zero time, which Delegated mode refuses.
func delegationStart(ctx context.Context, reader client.Reader, task *corev1alpha1.Task) (time.Time, error) {
	current := task
	for range maxDelegationChainDepth {
		if !sameRequester(current.Spec.RequestedBy, task.Spec.RequestedBy) {
			return time.Time{}, nil
		}
		if !namesDelegatingTask(current) {
			return current.CreationTimestamp.Time, nil
		}
		if reader == nil {
			return time.Time{}, nil
		}
		parent, err := delegatingTask(ctx, reader, current)
		if errors.Is(err, errDelegationChainBroken) {
			return time.Time{}, nil
		}
		if err != nil {
			return time.Time{}, err
		}
		current = parent
	}
	return time.Time{}, nil
}

// errDelegationChainBroken marks a Task chain that cannot be proven.
var errDelegationChainBroken = errors.New("the delegation chain is broken")

// namesDelegatingTask reports whether a Task names a Task it was delegated
// from, by parent metadata, a pinned parent UID, or a Task controller owner.
func namesDelegatingTask(task *corev1alpha1.Task) bool {
	return labels.ParentTaskName(task.Labels, task.Annotations) != "" ||
		strings.TrimSpace(task.Annotations[labels.AnnotationParentTaskUID]) != "" ||
		taskControllerOwner(task) != nil
}

func taskControllerOwner(task *corev1alpha1.Task) *metav1.OwnerReference {
	owner := metav1.GetControllerOf(task)
	if owner == nil || owner.APIVersion != corev1alpha1.GroupVersion.String() || owner.Kind != taskResourceKind {
		return nil
	}
	return owner
}

// delegatingTask returns the Task current was delegated from. The link counts
// only when an immutable UID pins it: the parent UID a brokered delegation
// records, or the controller owner reference. A parent named without a pin
// (such as a child Kubernetes orphaned), gone, or recreated under its name
// cannot be proven, and fails with errDelegationChainBroken.
func delegatingTask(ctx context.Context, reader client.Reader, current *corev1alpha1.Task) (*corev1alpha1.Task, error) {
	name := labels.ParentTaskName(current.Labels, current.Annotations)
	pinned := strings.TrimSpace(current.Annotations[labels.AnnotationParentTaskUID])
	if owner := taskControllerOwner(current); owner != nil {
		if name == "" {
			name = owner.Name
		}
		if owner.Name != name || (pinned != "" && pinned != string(owner.UID)) {
			return nil, fmt.Errorf("%w: a Task it was delegated from is named inconsistently", errDelegationChainBroken)
		}
		pinned = string(owner.UID)
	}
	if name == "" || pinned == "" {
		return nil, fmt.Errorf("%w: no UID pins a Task it was delegated from", errDelegationChainBroken)
	}
	parent := &corev1alpha1.Task{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: current.Namespace, Name: name}, parent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: a Task it was delegated from is gone", errDelegationChainBroken)
		}
		return nil, fmt.Errorf("read the delegating Task: %w", err)
	}
	if string(parent.UID) != pinned {
		return nil, fmt.Errorf("%w: a Task it was delegated from was replaced", errDelegationChainBroken)
	}
	return parent, nil
}

func sameRequester(a, b *corev1alpha1.RequestedBy) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Issuer == b.Issuer && a.Subject == b.Subject
}

// taskExecutionReasonIdentityUnavailable fails a Task whose bound identity
// may no longer be used before its runtime admission.
const taskExecutionReasonIdentityUnavailable = corev1alpha1.TaskExecutionReason("IdentityUnavailable")

// requireBoundIdentityClaim re-checks, after the binding is persisted and
// until the Task is queued, that the Agent still holds the identity the
// binding froze. A claim released between the binding decision and its write
// could otherwise leave two Agents with Tasks acting as one identity; once the
// binding is persisted, any later release sees it and keeps the claim. A
// queued Task is checked by the ACPDispatcher right before every runtime
// admission instead, which settles its durable attempt for either backend.
func (r *TaskReconciler) requireBoundIdentityClaim(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) (ctrl.Result, error, bool) {
	if task.Status.Execution != nil {
		return ctrl.Result{}, nil, false
	}
	message, err := boundIdentityUnavailable(ctx, uncachedReader(r.APIReader, r.Client), task, binding)
	if err != nil {
		log.FromContext(ctx).Error(err, "Agent identity claim check failed")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
	}
	if message == "" {
		return ctrl.Result{}, nil, false
	}
	result, failErr := r.failACPPlanningTask(ctx, task, taskExecutionReasonIdentityUnavailable, message)
	return result, failErr, true
}

// boundIdentityUnavailable returns why a Task may not start with the identity
// its binding froze, or "" when it may: the bound Agent must still hold the
// identity, not be releasing it, and have granted it before the Task was
// created. Only a failed read returns an error.
func boundIdentityUnavailable(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) (string, error) {
	if binding == nil || binding.Agent == nil || binding.Agent.Identity == nil {
		return "", nil
	}
	frozen := binding.Agent
	live := &corev1alpha1.Agent{}
	err := reader.Get(ctx, types.NamespacedName{Namespace: frozen.Namespace, Name: frozen.Name}, live)
	if err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("read the Agent's identity claim: %w", err)
	}
	if err == nil && live.UID == frozen.UID && identityClaimBindable(live.Status.IdentityClaim, frozen.Identity.Issuer, frozen.Identity.ID) {
		if identityClaimCoversTask(live.Status.IdentityClaim, task) {
			return "", nil
		}
		// The claim was released and granted again after the Task was created.
		return fmt.Sprintf("Agent %s/%s was granted identity %s/%s again after the Task was created",
			frozen.Namespace, frozen.Name, frozen.Identity.Issuer, frozen.Identity.ID), nil
	}
	return fmt.Sprintf("Agent %s/%s no longer holds identity %s/%s",
		frozen.Namespace, frozen.Name, frozen.Identity.Issuer, frozen.Identity.ID), nil
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

// DelegationLiveness checks that a Delegated call's Task is still running and
// its Session still open, reading both live.
type DelegationLiveness struct {
	Reader   client.Reader
	Sessions store.SessionStore
}

// requireLiveAncestors refuses a delegated call from a child Task once any
// Task above it in the delegation chain was cancelled, had its cancellation
// requested, is being deleted, is gone, or had its Session deleted: stopping
// a parent stops every child acting for the same requester. A parent that
// finished normally does not stop its children. A link no UID proves, such
// as a fork's or an orphaned child's, fails closed.
func (d DelegationLiveness) requireLiveAncestors(ctx context.Context, task *corev1alpha1.Task) error {
	current := task
	for range maxDelegationChainDepth {
		if !namesDelegatingTask(current) {
			return nil
		}
		parent, err := delegatingTask(ctx, d.Reader, current)
		if err != nil {
			return err
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
		if err := d.requireLiveSessions(ctx, parent, "the Session of a Task it was delegated from"); err != nil {
			return err
		}
		current = parent
	}
	return errors.New("the delegating Task chain is too deep")
}

// requireLiveSessions refuses once a Session the Task runs in is gone: its
// spec.sessionRef, or the chat Session the chat API recorded in the
// orka.ai/chat-session label. A chat Session must also be the incarnation
// the chat API pinned in orka.ai/chat-session-epoch when it created the
// Task, so a chat deleted and recreated under its name does not keep the
// Task acting for the requester; admission keeps both labels immutable. It
// reads Session metadata only, never the transcript.
func (d DelegationLiveness) requireLiveSessions(ctx context.Context, task *corev1alpha1.Task, subject string) error {
	if task.Spec.SessionRef != nil {
		if name := strings.TrimSpace(task.Spec.SessionRef.Name); name != "" {
			if err := d.requireSessionExists(ctx, task.Namespace, name, subject); err != nil {
				return err
			}
		}
	}
	chat := strings.TrimSpace(task.Labels[labels.LabelChatSession])
	if chat == "" {
		return nil
	}
	reader, ok := d.Sessions.(store.SessionCreationReader)
	if !ok {
		return fmt.Errorf("the Session store cannot identify %s", subject)
	}
	createdAt, err := reader.GetSessionCreatedAt(ctx, task.Namespace, chat)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%s was deleted", subject)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", subject, err)
	}
	pinned := strings.TrimSpace(task.Labels[labels.LabelChatSessionEpoch])
	if pinned == "" {
		return fmt.Errorf("%s has no pinned incarnation", subject)
	}
	if labels.SessionEpoch(createdAt) != pinned {
		return fmt.Errorf("%s was deleted and replaced", subject)
	}
	return nil
}

func (d DelegationLiveness) requireSessionExists(ctx context.Context, namespace, name, subject string) error {
	if d.Sessions == nil {
		return errors.New("no Session store is configured")
	}
	var err error
	if reader, ok := d.Sessions.(sessionTypeReader); ok {
		_, err = reader.GetSessionType(ctx, namespace, name)
	} else {
		var session *store.SessionRecord
		if session, err = d.Sessions.GetSession(ctx, namespace, name); err == nil && session == nil {
			err = store.ErrNotFound
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%s was deleted", subject)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", subject, err)
	}
	return nil
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
	if err := d.requireLiveSessions(ctx, task, "the Task's Session"); err != nil {
		return err
	}
	return d.requireLiveAncestors(ctx, task)
}
