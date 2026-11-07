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
	"github.com/orka-agents/orka/internal/agentidentity"
	"github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/labels"
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
// it must name the identity, not be on its way out, and carry the grant time
// the API server recorded.
// enforcedIdentityClaim returns the claim as the current period of Agent use
// enforcement sees it: a grant from before that period began counts from its
// start, so a Task or source created while the check was off is never covered,
// even when the Agent did not reconcile while it was off.
func enforcedIdentityClaim(claim *corev1alpha1.AgentIdentityClaim, config agentidentity.Config) *corev1alpha1.AgentIdentityClaim {
	since := config.EnforcedSince
	if claim == nil || claim.GrantedAt == nil || since.IsZero() || !claim.GrantedAt.Time.Before(since) {
		return claim
	}
	enforced := claim.DeepCopy()
	start := metav1.NewTime(since)
	enforced.GrantedAt = &start
	return enforced
}

func identityClaimBindable(claim *corev1alpha1.AgentIdentityClaim, issuer, id string) bool {
	return identityClaimMatches(claim, issuer, id) && !claim.Releasing && claim.GrantedAt != nil
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
//
// A claim is granted, or granted again after releasing, without a grant
// time; no Task binds it yet. A later pass, reading the claim back, records
// as its grant time the latest write time the API server has recorded on the
// Agent, which is no earlier than the write that granted it. Grant times are
// compared with creation times the API server stamps, so the controller's
// clock never decides which objects a claim covers.
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
	if _, configured := r.Identity.Instance(issuer); declared && (!r.Identity.AgentUseAuthorization || !configured) {
		// Claims exist only while Agent use authorization is enforced and the
		// issuer is configured; without either a held claim is released like
		// one the spec no longer names.
		issuer, id, declared = "", "", false
	}
	if claim != nil && identityClaimMatches(claim, issuer, id) {
		switch {
		case claim.Releasing:
			// The spec names the held identity again, or use authorization is
			// back on. Tasks created while it was leaving were not checked for
			// it, so the claim is granted again.
			claim.Releasing = false
			claim.GrantedAt = nil
			return claim, errIdentityGrantTimePending(claim)
		case claim.GrantedAt == nil:
			granted := apiServerWriteTime(live)
			if granted == nil {
				return claim, errIdentityGrantTimePending(claim)
			}
			claim.GrantedAt = granted
		}
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
	claim = &corev1alpha1.AgentIdentityClaim{Issuer: issuer, ID: id}
	return claim, errIdentityGrantTimePending(claim)
}

// errIdentityGrantTimePending keeps the Agent not Ready while a granted claim
// waits for the grant time the API server records.
func errIdentityGrantTimePending(claim *corev1alpha1.AgentIdentityClaim) error {
	return fmt.Errorf("identity %s/%s is granted and waits for the API server to record the grant time", claim.Issuer, claim.ID)
}

// apiServerWriteTime returns the latest write time the API server recorded
// in the object's managedFields, at one-second precision, or nil when it
// recorded none. The write that granted a claim set its manager's time, so
// the latest time is never earlier than that write.
func apiServerWriteTime(obj metav1.Object) *metav1.Time {
	var latest *metav1.Time
	for _, entry := range obj.GetManagedFields() {
		if entry.Time != nil && (latest == nil || entry.Time.After(latest.Time)) {
			latest = entry.Time.DeepCopy()
		}
	}
	return latest
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
	claim := enforcedIdentityClaim(live.Status.IdentityClaim, r.Identity)
	if !identityClaimCoversTask(claim, task) {
		return nil, permanentACPAgentConfiguration(fmt.Errorf(
			"the Task was created before Agent %q was granted identity %s/%s, so its creation was not checked for it; create the Task again",
			agent.Name, issuer, id))
	}
	ref, err := r.Identity.BindingRef(agent)
	if err != nil {
		return nil, permanentACPAgentConfiguration(err)
	}
	if err := requireTaskSourceCoveredByClaim(ctx, reader, task, agent, ref, claim); err != nil {
		return nil, err
	}
	if parentName := labels.ParentTaskName(task.Labels, task.Annotations); parentName != "" {
		if err := requireParentOwnsIdentityAgent(ctx, reader, task, parentName, agent, ref, claim); err != nil {
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
		// A Task a scan or monitor created keeps its markers even when its
		// owner reference is gone, as after an orphaning delete, so it cannot
		// pass as a root Task.
		if kind := taskAutomationMarker(task); kind != "" {
			return permanentACPAgentConfiguration(fmt.Errorf(
				"a %s's Task cannot use identity-bound Agent %q once its %s no longer controls it", kind, agent.Name, kind))
		}
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

// taskAutomationMarker names the kind of automation whose markers a Task
// carries, or "".
func taskAutomationMarker(task *corev1alpha1.Task) string {
	switch {
	case task.Labels[labels.LabelRepositoryMonitor] != "" || task.Annotations[labels.AnnotationRepositoryMonitorName] != "":
		return "RepositoryMonitor"
	case task.Labels[labels.LabelSecurityScanID] != "":
		return "RepositoryScan"
	}
	return ""
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
	message, err := boundIdentityUnavailable(ctx, uncachedReader(r.APIReader, r.Client), r.Identity, task, binding)
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
	config agentidentity.Config,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) (string, error) {
	if binding == nil || binding.Agent == nil || binding.Agent.Identity == nil {
		return "", nil
	}
	frozen := binding.Agent
	// The controller must still honor the identity it froze: Agent use
	// enforcement on, and the same issuer instance configured.
	if !config.AgentUseAuthorization {
		return fmt.Sprintf("identity %s/%s needs --agent-use-authorization-enabled, which this controller no longer sets",
			frozen.Identity.Issuer, frozen.Identity.ID), nil
	}
	if instance, configured := config.Instance(frozen.Identity.Issuer); !configured || instance != frozen.Identity.Instance {
		return fmt.Sprintf("identity issuer %s of %s/%s is no longer configured as it was when the Task was bound",
			frozen.Identity.Issuer, frozen.Namespace, frozen.Name), nil
	}
	live := &corev1alpha1.Agent{}
	err := reader.Get(ctx, types.NamespacedName{Namespace: frozen.Namespace, Name: frozen.Name}, live)
	if err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("read the Agent's identity claim: %w", err)
	}
	if err == nil && live.UID == frozen.UID && identityClaimBindable(live.Status.IdentityClaim, frozen.Identity.Issuer, frozen.Identity.ID) {
		if identityClaimCoversTask(enforcedIdentityClaim(live.Status.IdentityClaim, config), task) {
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
