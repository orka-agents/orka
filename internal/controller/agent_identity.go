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
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/store"
)

var errIdentityClaimPending = errors.New("the Agent does not hold its identity yet")

func identityClaimMatches(claim *corev1alpha1.AgentIdentityClaim, issuer, id string) bool {
	return claim != nil && claim.Issuer == issuer && claim.ID == id
}

func identityClaimCoversTask(claim *corev1alpha1.AgentIdentityClaim, task *corev1alpha1.Task) bool {
	return identityClaimCoversCreation(claim, task.CreationTimestamp)
}

func identityClaimCoversCreation(claim *corev1alpha1.AgentIdentityClaim, created metav1.Time) bool {
	return claim != nil && claim.GrantedAt != nil && created.After(claim.GrantedAt.Time)
}

func enforcedIdentityClaim(claim *corev1alpha1.AgentIdentityClaim, config agentidentity.Config) (*corev1alpha1.AgentIdentityClaim, error) {
	since, started := config.EnforcementStart()
	if !started {
		return nil, errEnforcementPeriodPending
	}
	if claim == nil || claim.GrantedAt == nil || since.IsZero() || !claim.GrantedAt.Time.Before(since) {
		return claim, nil
	}
	enforced := claim.DeepCopy()
	start := metav1.NewTime(since)
	enforced.GrantedAt = &start
	return enforced, nil
}

var errEnforcementPeriodPending = errors.New("the Agent use enforcement period has not started yet")

func identityClaimBindable(claim *corev1alpha1.AgentIdentityClaim, issuer, id string) bool {
	return identityClaimMatches(claim, issuer, id) && !claim.Releasing && claim.GrantedAt != nil
}

func (r *AgentReconciler) validateAgentIdentity(_ context.Context, agent *corev1alpha1.Agent) error {
	return r.Identity.ValidateAgent(agent)
}

func (r *AgentReconciler) reconcileIdentityClaim(ctx context.Context, agent *corev1alpha1.Agent) (*corev1alpha1.AgentIdentityClaim, error) {
	reader := uncachedReader(r.APIReader, r.Client)
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
		issuer, id, declared = "", "", false
	}
	if claim != nil && identityClaimMatches(claim, issuer, id) {
		switch {
		case claim.Releasing:
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
	if err := reader.List(ctx, agents, client.InNamespace(agent.Namespace)); err != nil {
		return nil, fmt.Errorf("list Agents holding identity %s/%s: %w", issuer, id, err)
	}
	for i := range agents.Items {
		other := &agents.Items[i]
		if other.UID != agent.UID && identityClaimMatches(other.Status.IdentityClaim, issuer, id) {
			return nil, fmt.Errorf("identity %s/%s is already bound to Agent %s/%s", issuer, id, other.Namespace, other.Name)
		}
	}
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

func errIdentityGrantTimePending(claim *corev1alpha1.AgentIdentityClaim) error {
	return fmt.Errorf("identity %s/%s is granted and waits for the API server to record the grant time", claim.Issuer, claim.ID)
}

func apiServerWriteTime(obj metav1.Object) *metav1.Time {
	var latest *metav1.Time
	for _, entry := range obj.GetManagedFields() {
		if entry.Time != nil && (latest == nil || entry.Time.After(latest.Time)) {
			latest = entry.Time.DeepCopy()
		}
	}
	return latest
}

func errIdentityClaimReleasing(claim *corev1alpha1.AgentIdentityClaim, declared bool) error {
	if !declared {
		return nil
	}
	return fmt.Errorf("identity %s/%s is being released before this Agent can claim its new identity", claim.Issuer, claim.ID)
}

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

func unfinishedIdentityBinding(task *corev1alpha1.Task, issuer, id string) *corev1alpha1.AgentExecutionAgentRef {
	binding := task.Status.AgentExecutionBinding
	if task.Status.Phase.IsTerminal() || binding == nil || binding.Agent == nil || binding.Agent.Identity == nil ||
		binding.Agent.Identity.Issuer != issuer || binding.Agent.Identity.ID != id {
		return nil
	}
	return binding.Agent
}

const agentIdentityReverifyInterval = time.Hour

const (
	reasonIdentityVerified           = "Verified"
	reasonIdentityVerificationFailed = outboundaccess.AgentReasonIdentityVerificationFailed
	reasonIdentityIssuerUnavailable  = "IssuerUnavailable"
)

const (
	agentIdentityVerifyTimeout = 5 * time.Second
	agentIdentityIssuerBackoff = time.Minute
)

type agentIdentityVerification struct {
	uid        types.UID
	generation int64
	issuer, id string
	state      agentIdentityState
	until      time.Time
}

type agentIdentityState struct {
	declared    bool
	verified    bool
	unavailable bool
	retryAfter  time.Duration
	err         error
}

func (r *AgentReconciler) verifyAgentIdentity(ctx context.Context, agent *corev1alpha1.Agent, validationErr error) agentIdentityState {
	issuer, id, ok := agent.IdentityRef()
	if !ok {
		return agentIdentityState{}
	}
	state := agentIdentityState{declared: true}
	key := types.NamespacedName{Namespace: agent.Namespace, Name: agent.Name}
	refusal, refused := r.lastIdentityRefusal(key, agent, issuer, id)
	if validationErr != nil || r.IdentityVerifier == nil {
		if refused {
			return refusal
		}
		return state
	}
	unavailable := func(wait time.Duration) agentIdentityState {
		if refused {
			refusal.retryAfter = wait
			return refusal
		}
		state.unavailable, state.retryAfter = true, wait
		state.err = fmt.Errorf("identity %s/%s could not be verified: the issuer is unavailable; retrying in %s",
			issuer, id, wait.Round(time.Second))
		return state
	}
	r.verifications.Lock()
	cached, found := r.verifications.byAgent[key]
	r.verifications.Unlock()
	if found && cached.uid == agent.UID && cached.generation == agent.Generation &&
		cached.issuer == issuer && cached.id == id {
		if remaining := time.Until(cached.until); remaining > 0 {
			answer := cached.state
			answer.retryAfter = remaining
			return answer
		}
	}
	r.issuerBackoff.Lock()
	wait := time.Until(r.issuerBackoff.until)
	r.issuerBackoff.Unlock()
	if wait > 0 {
		return unavailable(wait)
	}
	verifyCtx, cancel := context.WithTimeout(ctx, agentIdentityVerifyTimeout)
	defer cancel()
	remember := func(answer agentIdentityState) agentIdentityState {
		r.verifications.Lock()
		if r.verifications.byAgent == nil {
			r.verifications.byAgent = map[types.NamespacedName]agentIdentityVerification{}
		}
		r.verifications.byAgent[key] = agentIdentityVerification{
			uid: agent.UID, generation: agent.Generation, issuer: issuer, id: id,
			state: answer, until: time.Now().Add(agentIdentityReverifyInterval),
		}
		r.verifications.Unlock()
		return answer
	}
	if err := r.IdentityVerifier.Verify(verifyCtx, agent); err != nil {
		if errors.Is(err, agentidentity.ErrIssuerUnavailable) || errors.Is(err, context.DeadlineExceeded) {
			r.issuerBackoff.Lock()
			r.issuerBackoff.until = time.Now().Add(agentIdentityIssuerBackoff)
			r.issuerBackoff.Unlock()
			return unavailable(agentIdentityIssuerBackoff)
		}
		state.err = fmt.Errorf("%s: %w", identityRefusalPrefix(issuer, id), err)
		return remember(state)
	}
	state.verified = true
	return remember(state)
}

func (r *AgentReconciler) lastIdentityRefusal(key types.NamespacedName, agent *corev1alpha1.Agent, issuer, id string) (agentIdentityState, bool) {
	r.verifications.Lock()
	cached, found := r.verifications.byAgent[key]
	r.verifications.Unlock()
	if found && cached.uid == agent.UID && cached.issuer == issuer && cached.id == id {
		if cached.state.err == nil || cached.state.unavailable || cached.state.verified {
			return agentIdentityState{}, false
		}
		refusal := cached.state
		refusal.retryAfter = 0
		return refusal, true
	}
	condition := meta.FindStatusCondition(agent.Status.Conditions, conditionTypeIdentityVerified)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != reasonIdentityVerificationFailed ||
		!strings.HasPrefix(condition.Message, identityRefusalPrefix(issuer, id)) {
		return agentIdentityState{}, false
	}
	return agentIdentityState{declared: true, err: errors.New(condition.Message)}, true
}

func identityRefusalPrefix(issuer, id string) string {
	return fmt.Sprintf("identity %s/%s failed verification", issuer, id)
}

func (r *AgentReconciler) forgetAgentIdentityVerification(key types.NamespacedName) {
	r.verifications.Lock()
	delete(r.verifications.byAgent, key)
	r.verifications.Unlock()
}

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
	case identity.unavailable:
		condition.Status = metav1.ConditionUnknown
		condition.Reason = reasonIdentityIssuerUnavailable
		condition.Message = identity.err.Error()
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
	if task.Labels[labels.LabelContinuationOf] != "" || strings.TrimSpace(task.Annotations[labels.AnnotationContinuationOfUID]) != "" {
		return nil, permanentACPAgentConfiguration(fmt.Errorf(
			"a parked turn's continuation cannot use identity-bound Agent %q; create a new Task to use it", agent.Name))
	}
	reader := uncachedReader(r.APIReader, r.Client)
	live := &corev1alpha1.Agent{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: agent.Name}, live); err != nil {
		return nil, fmt.Errorf("resolve the Agent's identity claim: %w", err)
	}
	if live.UID != agent.UID || !identityClaimBindable(live.Status.IdentityClaim, issuer, id) {
		return nil, fmt.Errorf("identity %s/%s: %w", issuer, id, errIdentityClaimPending)
	}
	claim, err := enforcedIdentityClaim(live.Status.IdentityClaim, r.Identity)
	if err != nil {
		return nil, fmt.Errorf("identity %s/%s: %w", issuer, id, err)
	}
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
		if kind := taskAutomationMarker(task); kind != "" {
			return permanentACPAgentConfiguration(fmt.Errorf(
				"a %s's Task cannot use identity-bound Agent %q once its %s no longer controls it", kind, agent.Name, kind))
		}
		return nil
	}
	var source client.Object
	switch owner.Kind {
	case repositoryScanResourceKind:
		source = &corev1alpha1.RepositoryScan{}
	case repositoryMonitorResourceKind:
		source = &corev1alpha1.RepositoryMonitor{}
	default:
		if kind := taskAutomationMarker(task); kind != "" {
			return permanentACPAgentConfiguration(fmt.Errorf(
				"a %s's Task cannot use identity-bound Agent %q once its %s no longer controls it", kind, agent.Name, kind))
		}
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

const (
	repositoryScanResourceKind    = "RepositoryScan"
	repositoryMonitorResourceKind = "RepositoryMonitor"
)

func taskAutomationMarker(task *corev1alpha1.Task) string {
	switch {
	case task.Labels[labels.LabelRepositoryMonitor] != "" || task.Annotations[labels.AnnotationRepositoryMonitorName] != "":
		return repositoryMonitorResourceKind
	case task.Labels[labels.LabelSecurityScanID] != "":
		return repositoryScanResourceKind
	}
	return ""
}

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
			return permanentACPAgentConfiguration(fmt.Errorf(
				"a fork does not carry its source Task's identity-bound Agent %q; create a new Task to use it", agent.Name))
		}
		return permanentACPAgentConfiguration(fmt.Errorf(
			"a child Task may use identity-bound Agent %q only while its parent Task %q still controls it", agent.Name, parentName))
	}
	if parent.Spec.Schedule != "" && parent.Spec.Type == corev1alpha1.TaskTypeAgent {
		if parent.Spec.AgentRef == nil || task.Spec.AgentRef == nil || *parent.Spec.AgentRef != *task.Spec.AgentRef {
			return permanentACPAgentConfiguration(fmt.Errorf(
				"a scheduled run may use identity-bound Agent %q only when its scheduled Task names it", agent.Name))
		}
		if !identityClaimCoversTask(claim, parent) {
			return permanentACPAgentConfiguration(fmt.Errorf(
				"scheduled Task %q was created before Agent %q was granted identity %s/%s, so its creation was not checked for it; re-create the scheduled Task",
				parent.Name, agent.Name, ref.Issuer, ref.ID))
		}
		if err := requireTaskSourceCoveredByClaim(ctx, reader, parent, agent, ref, claim); err != nil {
			return err
		}
		if grandparent := labels.ParentTaskName(parent.Labels, parent.Annotations); grandparent != "" {
			return requireParentOwnsIdentityAgent(ctx, reader, parent, grandparent, agent, ref, claim)
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
	if !identityClaimCoversTask(claim, parent) {
		return permanentACPAgentConfiguration(fmt.Errorf(
			"parent Task %q was created before Agent %q was granted identity %s/%s, so its delegated Tasks cannot use it; re-create the parent Task",
			parent.Name, agent.Name, ref.Issuer, ref.ID))
	}
	return nil
}

func agentIdentityBindingFromTask(ctx context.Context, reader client.Reader, task *corev1alpha1.Task) (*outboundaccess.AgentIdentityBinding, error) {
	if task == nil || task.Status.AgentExecutionBinding == nil || task.Status.AgentExecutionBinding.Agent == nil ||
		task.Status.AgentExecutionBinding.Agent.Identity == nil {
		return nil, nil
	}
	binding := task.Status.AgentExecutionBinding
	agent := binding.Agent
	var start time.Time
	if agent.Identity.ActsForRequester {
		var err error
		if start, err = delegationStart(ctx, reader, task); err != nil {
			return nil, err
		}
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

const maxDelegationChainDepth = 32

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

var errDelegationChainBroken = errors.New("the delegation chain is broken")

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

const taskExecutionReasonIdentityUnavailable = corev1alpha1.TaskExecutionReason("IdentityUnavailable")

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
		claim, err := enforcedIdentityClaim(live.Status.IdentityClaim, config)
		if err != nil {
			return "", err
		}
		if identityClaimCoversTask(claim, task) {
			return "", nil
		}
		return fmt.Sprintf("Agent %s/%s was granted identity %s/%s again after the Task was created",
			frozen.Namespace, frozen.Name, frozen.Identity.Issuer, frozen.Identity.ID), nil
	}
	return fmt.Sprintf("Agent %s/%s no longer holds identity %s/%s",
		frozen.Namespace, frozen.Name, frozen.Identity.Issuer, frozen.Identity.ID), nil
}

const agentIdentityReleaseRecheck = 10 * time.Second

func classificationActsForRequester(classified map[string]connectorToolInfo) bool {
	for _, info := range classified {
		if info.Delegated {
			return true
		}
	}
	return false
}

type DelegationLiveness struct {
	Reader   client.Reader
	Sessions store.SessionStore
}

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

func (d DelegationLiveness) requireLiveSessions(ctx context.Context, task *corev1alpha1.Task, subject string) error {
	if task.Spec.SessionRef != nil {
		if name := strings.TrimSpace(task.Spec.SessionRef.Name); name != "" {
			if err := d.requireSessionExists(ctx, task.Namespace, name, subject); err != nil {
				return err
			}
			if err := d.requireNoSessionCleanup(ctx, task.Namespace, name, subject); err != nil {
				return err
			}
		}
	}
	chat := strings.TrimSpace(task.Labels[labels.LabelChatSession])
	if chat == "" {
		return nil
	}
	chatNamespace := strings.TrimSpace(task.Labels[labels.LabelChatSessionNamespace])
	if chatNamespace == "" {
		chatNamespace = task.Namespace
	}
	reader, ok := d.Sessions.(store.SessionCreationReader)
	if !ok {
		return fmt.Errorf("the Session store cannot identify %s", subject)
	}
	createdAt, err := reader.GetSessionCreatedAt(ctx, chatNamespace, chat)
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
	return d.requireNoSessionCleanup(ctx, chatNamespace, chat, subject)
}

type sessionCleanupIntentReader interface {
	HasSessionCleanupIntent(ctx context.Context, namespace, sessionName string) (bool, error)
}

func (d DelegationLiveness) requireNoSessionCleanup(ctx context.Context, namespace, name, subject string) error {
	reader, ok := d.Sessions.(sessionCleanupIntentReader)
	if !ok {
		return fmt.Errorf("the Session store cannot tell whether %s is being deleted", subject)
	}
	pending, err := reader.HasSessionCleanupIntent(ctx, namespace, name)
	if err != nil {
		return fmt.Errorf("read the deletion state of %s: %w", subject, err)
	}
	if pending {
		return fmt.Errorf("%s is being deleted", subject)
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
	case task.Status.Phase.IsTerminal():
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
