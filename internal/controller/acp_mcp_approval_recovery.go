package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

const acpMCPApprovalUnknownReason = "The action may have run. Do not repeat it automatically."

type acpMCPApprovalTaskKey struct {
	namespace string
	uid       types.UID
}

type acpMCPApprovalRecoveryTask struct {
	task     *corev1alpha1.Task
	effects  map[string]acpMCPApprovalEffect
	versions map[string]acpMCPApprovalEffectVersion
	pending  bool
}

type acpMCPApprovalRecoveryProgress struct {
	fence    store.ControllerEpochFence
	versions map[string]acpMCPApprovalEffectVersion
	eventSeq int64
}

type acpMCPApprovalEffectVersion struct {
	version         int64
	resourceVersion string
}

type acpMCPApprovalEffect struct {
	store.ExternalEffect
	taskUID         types.UID
	resourceVersion string
}

// acpMCPApprovalReceiptOutcome interprets a saved tool response without granting
// permission to repeat the call. Failed records are definitive pre-execution
// denials; a completed tool that returned an error has a Succeeded effect.
func acpMCPApprovalReceiptOutcome(effect *store.ExternalEffect, approvalID string) (string, string, json.RawMessage, error) {
	if effect == nil {
		return "", "", nil, errors.New("approval receipt is invalid")
	}
	result, err := canonicalMCPApprovalResult(effect.Response)
	if err != nil || effect.ResponseDigest != store.CanonicalBytesDigest(result) {
		return "", "", nil, errors.New("approval receipt is invalid")
	}
	switch effect.State {
	case store.ExternalEffectSucceeded:
		outcome := "succeeded"
		if mcpToolResultIsError(result) {
			outcome = acpApprovalOutcomeFailed
		}
		return outcome, "Recorded tool result", result, nil
	case store.ExternalEffectFailed:
		var denial struct {
			Code             string `json:"code"`
			ApprovalID       string `json:"approvalID"`
			ExternalEffectID string `json:"externalEffectID"`
			RequestDigest    string `json:"requestDigest"`
			Error            string `json:"error"`
		}
		if json.Unmarshal(result, &denial) != nil || approvalID == "" || !mcpToolResultIsError(result) {
			return "", "", nil, errors.New("approval denial receipt is invalid")
		}
		if denial.ApprovalID == "" {
			// A crash can precede the approval event. Recovery can still prove
			// that an abandoned Pending effect never started, and bind its denial
			// to the exact effect instead of inventing an approval identity.
			id, err := effect.Identity.CanonicalID()
			if err != nil || effect.Identity.Kind != acpMCPToolEffectKind || effect.ID != id ||
				denial.ExternalEffectID != id || denial.Code != acpApprovalCodeStale || denial.Error != acpApprovalStaleMessage ||
				store.ValidateCanonicalDigest("approval request digest", denial.RequestDigest) != nil ||
				denial.RequestDigest != effect.RequestDigest {
				return "", "", nil, errors.New("approval denial receipt is invalid")
			}
		} else if denial.ApprovalID != approvalID || denial.ExternalEffectID != "" || denial.RequestDigest != "" {
			return "", "", nil, errors.New("approval denial receipt is invalid")
		}
		switch denial.Code {
		case acpApprovalCodeDeclined, acpApprovalCodeExpired, acpApprovalCodeCancelled, acpApprovalCodeStale:
			return "not_started", denial.Code, result, nil
		default:
			return "", "", nil, errors.New("approval denial receipt is invalid")
		}
	default:
		return "", "", nil, errors.New("approval effect has no terminal receipt")
	}
}

// Prefer the immutable Task binding for recovery discovery. Legacy labels only
// select candidates; projection still verifies the complete approval binding.
func mcpApprovalEffectTaskUID(effect *corev1alpha1.ExternalEffect) types.UID {
	if effect.Spec.ApprovalTaskUID != "" {
		return types.UID(effect.Spec.ApprovalTaskUID)
	}
	return types.UID(effect.Labels[corev1alpha1.ControlRecordTaskUIDLabel])
}

func mcpApprovalEffectSnapshot(effect *corev1alpha1.ExternalEffect) acpMCPApprovalEffect {
	result := store.ExternalEffect{
		ID: effect.Spec.ID,
		Identity: store.ExternalEffectIdentity{
			Kind: effect.Spec.Kind, Namespace: effect.Spec.IdentityNamespace,
			AggregateID: effect.Spec.AggregateID, OperationID: effect.Spec.OperationID,
		},
		RequestDigest: effect.Spec.RequestDigest, State: store.ExternalEffectState(effect.Status.State),
		ResponseDigest: effect.Status.ResponseDigest, LeaseOwner: effect.Status.LeaseOwner,
		ControllerEpochName: effect.Status.ControllerEpochName, ControllerEpoch: effect.Status.ControllerEpoch,
		Version: effect.Status.Version,
	}
	if effect.Status.Response != nil {
		result.Response = effect.Status.Response.Raw
	}
	if effect.Status.LeaseExpiresAt != nil {
		result.LeaseExpiresAt = &effect.Status.LeaseExpiresAt.Time
	}
	return acpMCPApprovalEffect{
		ExternalEffect: result, taskUID: mcpApprovalEffectTaskUID(effect),
		resourceVersion: effect.ResourceVersion,
	}
}

// Recovery joins already-listed effects with safe approval bindings. It never
// loads executable Secret contents or asks a stale runtime to redeliver a call.
func (d *ACPDispatcher) reconcileMCPApprovalExecutions(
	ctx context.Context,
	fence store.ControllerEpochFence,
	tasks []corev1alpha1.Task,
	effects map[string]acpMCPApprovalEffect,
) error {
	if d.EventStore == nil {
		return nil
	}
	d.approvalRecoveryMu.Lock()
	defer d.approvalRecoveryMu.Unlock()
	if len(effects) == 0 {
		clear(d.approvalRecovery)
		return nil
	}
	if d.approvalRecovery == nil {
		d.approvalRecovery = make(map[acpMCPApprovalTaskKey]acpMCPApprovalRecoveryProgress)
	}
	candidates := mcpApprovalRecoveryTasks(tasks, effects, fence)
	for key := range d.approvalRecovery {
		if _, exists := candidates[key]; !exists {
			delete(d.approvalRecovery, key)
		}
	}
	sequences, err := d.mcpApprovalRecoveryEventSequences(ctx, candidates)
	if err != nil {
		return err
	}
	for key, candidate := range candidates {
		task := candidate.task
		seq := sequences[task.Namespace][task.Name]
		previous, seen := d.approvalRecovery[key]
		if !candidate.pending && seen && previous.fence == fence && previous.eventSeq == seq && maps.Equal(previous.versions, candidate.versions) {
			continue
		}
		if err := d.reconcileMCPApprovalTask(ctx, fence, task, candidate.effects); err != nil {
			return err
		}
		// Capture the sequence from before projection. A late stale event or
		// our own append must invalidate the cache on the next scan, even if
		// the terminal effect's version has not changed.
		if candidate.pending {
			// Expiry and durable prompt termination can change without an
			// effect mutation or a Task event. Keep checking active reviews.
			delete(d.approvalRecovery, key)
		} else {
			d.approvalRecovery[key] = acpMCPApprovalRecoveryProgress{fence: fence, versions: candidate.versions, eventSeq: seq}
		}
	}
	return nil
}

func (d *ACPDispatcher) mcpApprovalRecoveryEventSequences(ctx context.Context, candidates map[acpMCPApprovalTaskKey]*acpMCPApprovalRecoveryTask) (map[string]map[string]int64, error) {
	streams := make(map[string][]string)
	for _, candidate := range candidates {
		if !candidate.pending {
			task := candidate.task
			streams[task.Namespace] = append(streams[task.Namespace], task.Name)
		}
	}
	sequences := make(map[string]map[string]int64, len(streams))
	for namespace, names := range streams {
		latest, err := d.EventStore.GetLatestExecutionEventSeqs(ctx, namespace, events.ExecutionEventStreamTypeTask, names)
		if err != nil {
			return nil, err
		}
		sequences[namespace] = latest
	}
	return sequences, nil
}

func mcpApprovalRecoveryTasks(tasks []corev1alpha1.Task, effects map[string]acpMCPApprovalEffect, fence store.ControllerEpochFence) map[acpMCPApprovalTaskKey]*acpMCPApprovalRecoveryTask {
	owners := make(map[acpMCPApprovalTaskKey]*corev1alpha1.Task)
	for i := range tasks {
		task := &tasks[i]
		if task.Spec.Type == corev1alpha1.TaskTypeAgent && task.UID != "" {
			owners[acpMCPApprovalTaskKey{namespace: task.Namespace, uid: task.UID}] = task
		}
	}
	candidates := make(map[acpMCPApprovalTaskKey]*acpMCPApprovalRecoveryTask)
	now := time.Now().UTC()
	for _, effect := range effects {
		key := acpMCPApprovalTaskKey{namespace: effect.Identity.Namespace, uid: effect.taskUID}
		task := owners[key]
		if task == nil || !mcpApprovalEffectNeedsRecovery(&effect.ExternalEffect, fence, now) {
			continue
		}
		if candidates[key] == nil {
			candidates[key] = &acpMCPApprovalRecoveryTask{
				task: task, effects: make(map[string]acpMCPApprovalEffect), versions: make(map[string]acpMCPApprovalEffectVersion),
			}
		}
		candidates[key].effects[effect.ID] = effect
		candidates[key].versions[effect.ID] = acpMCPApprovalEffectVersion{version: effect.Version, resourceVersion: effect.resourceVersion}
		candidates[key].pending = candidates[key].pending || effect.State == store.ExternalEffectPending
	}
	return candidates
}

func (d *ACPDispatcher) reconcileMCPApprovalTask(ctx context.Context, fence store.ControllerEpochFence, task *corev1alpha1.Task, effects map[string]acpMCPApprovalEffect) error {
	listed, err := approvals.ListEvents(ctx, d.EventStore, task.Namespace, task.Name)
	if err != nil {
		return err
	}
	// Index safe operation digests, but retain the exact durable identity for
	// receipt verification and projection. Runtime operation IDs may be URLs.
	byBinding := make(map[string]acpMCPApprovalEffect, len(effects))
	for id, effect := range effects {
		canonical, err := effect.Identity.CanonicalID()
		if err != nil || id != canonical || effect.ID != canonical || effect.Identity.Kind != acpMCPToolEffectKind ||
			effect.Identity.Namespace != task.Namespace || effect.taskUID != task.UID {
			continue
		}
		key := mcpApprovalEffectBindingKey(task.Namespace, effect.Identity.AggregateID,
			store.CanonicalBytesDigest([]byte(effect.Identity.OperationID)))
		byBinding[key] = effect
	}
	matched := make(map[string]struct{})
	for _, approval := range approvals.Derive(approvals.FilterEventsForTaskUID(listed, string(task.UID)), time.Time{}) {
		key, ok := mcpApprovalRecoveryBindingKey(task, approval)
		if !ok {
			continue
		}
		effect, exists := byBinding[key]
		if !exists || effect.RequestDigest != approval.Binding.RequestDigest {
			continue
		}
		matched[effect.ID] = struct{}{}
		if err := d.reconcileMCPApprovalExecution(ctx, fence, task, approval, &effect.ExternalEffect); err != nil {
			return err
		}
	}
	for id, effect := range effects {
		if _, exists := matched[id]; exists || effect.taskUID != task.UID || effect.Identity.Namespace != task.Namespace ||
			effect.State != store.ExternalEffectPending {
			continue
		}
		abandoned, err := d.mcpApprovalUnboundPendingAbandoned(ctx, fence, task, &effect.ExternalEffect)
		if err != nil {
			return err
		}
		if !abandoned {
			continue
		}
		// Reserve succeeds before ApprovalRequested is appended. Settle a call
		// in that gap only after proving its authority ended. A delayed request
		// event can later join this receipt without loading its Secret.
		if _, err := d.failMCPApprovalPending(ctx, fence, &effect.ExternalEffect, acpMCPAbandonedPendingReceipt(&effect.ExternalEffect)); err != nil &&
			!errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	return nil
}

func mcpApprovalEffectBindingKey(namespace, sessionUID, operationDigest string) string {
	return store.CanonicalControlID("acp-approval-effect-binding", namespace, sessionUID, operationDigest)
}

func mcpApprovalRecoveryBindingKey(task *corev1alpha1.Task, approval approvals.Approval) (string, bool) {
	binding := approval.Binding
	if binding == nil || approval.TaskUID != string(task.UID) || binding.TaskAttempt == 0 || binding.PromptID == "" ||
		approval.ToolCallID == "" || binding.RuntimeSessionUID == "" ||
		store.ValidateCanonicalDigest("approval operation digest", binding.OperationIDDigest) != nil ||
		store.ValidateCanonicalDigest("approval request digest", binding.RequestDigest) != nil {
		return "", false
	}
	if store.ValidateCanonicalDigest("approval call ID digest", binding.CallIDDigest) != nil {
		return "", false
	}
	expected := acpMCPApprovalIdentityFromCallDigest(task.Namespace, string(task.UID),
		fmt.Sprint(binding.TaskAttempt), binding.PromptID, binding.CallIDDigest)
	return mcpApprovalEffectBindingKey(task.Namespace, binding.RuntimeSessionUID, binding.OperationIDDigest), approval.ID == expected
}

func mcpApprovalEffectNeedsRecovery(effect *store.ExternalEffect, fence store.ControllerEpochFence, now time.Time) bool {
	if effect.Identity.Kind != acpMCPToolEffectKind {
		return false
	}
	switch effect.State {
	case store.ExternalEffectSucceeded, store.ExternalEffectFailed, store.ExternalEffectOutcomeUnknown:
		return true
	case store.ExternalEffectPending:
		return effect.ControllerEpochName == fence.Name && effect.ControllerEpoch > 0 && effect.ControllerEpoch <= fence.Epoch
	default:
		return mcpApprovalEffectOrphaned(effect, fence, now)
	}
}

func mcpApprovalEffectOrphaned(effect *store.ExternalEffect, fence store.ControllerEpochFence, now time.Time) bool {
	if effect.State != store.ExternalEffectInFlight || effect.ControllerEpochName != fence.Name ||
		effect.ControllerEpoch <= 0 || effect.ControllerEpoch > fence.Epoch {
		return false
	}
	return effect.ControllerEpoch < fence.Epoch || effect.LeaseExpiresAt == nil ||
		!now.Before(effect.LeaseExpiresAt.Add(acpExternalEffectReconcileGrace))
}

func mcpApprovalRecoveredOutcome(effect *store.ExternalEffect, approvalID string) (outcome, reason string, result json.RawMessage) {
	switch effect.State {
	case store.ExternalEffectSucceeded, store.ExternalEffectFailed:
		outcome, reason, result, err := acpMCPApprovalReceiptOutcome(effect, approvalID)
		if err == nil {
			return outcome, reason, result
		}
		return acpApprovalOutcomeUnknown, "The saved action result could not be verified. Do not repeat it automatically.", nil
	case store.ExternalEffectOutcomeUnknown:
		return acpApprovalOutcomeUnknown, acpMCPApprovalUnknownReason, nil
	default:
		return "", "", nil
	}
}

func (d *ACPDispatcher) reconcileMCPApprovalExecution(
	ctx context.Context,
	fence store.ControllerEpochFence,
	task *corev1alpha1.Task,
	approval approvals.Approval,
	effect *store.ExternalEffect,
) error {
	if effect.State == store.ExternalEffectPending {
		code, err := d.acpMCPApprovalPendingDenial(ctx, fence, task, approval, effect, time.Now().UTC())
		if err != nil || code == "" {
			return err
		}
		updated, err := d.failMCPApprovalPending(ctx, fence, effect, acpApprovalError(approval.ID, code))
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				return nil // A concurrent execution claim or settlement wins.
			}
			return err
		}
		effect = updated
	}
	if mcpApprovalEffectOrphaned(effect, fence, time.Now().UTC()) {
		// An old epoch cannot commit a result. Seal the uncertain outcome with
		// the exact lease CAS; a crash here is repaired by the next scan.
		updated, err := d.Store.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
			ID: effect.ID, Fence: fence, ExpectedVersion: effect.Version,
			ExpectedState: store.ExternalEffectInFlight, NewState: store.ExternalEffectOutcomeUnknown,
			RequestDigest: effect.RequestDigest, ExpectedLeaseOwner: effect.LeaseOwner, UpdatedAt: time.Now().UTC(),
		})
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				return nil // A concurrent settlement wins; reread it on the next scan.
			}
			return err
		}
		effect = updated
	}
	outcome, reason, _ := mcpApprovalRecoveredOutcome(effect, approval.ID)
	if mcpApprovalProjectionCurrent(approval, outcome, reason) {
		return nil
	}
	guard, ok := d.Store.(store.ControllerEpochMutationStore)
	if !ok {
		return errors.New("approval recovery requires the controller epoch mutation guard")
	}
	return guard.WithControllerEpochMutation(ctx, fence, func(guardCtx context.Context) error {
		return d.projectMCPApprovalExecution(guardCtx, task, approval, effect.Identity)
	})
}

func (d *ACPDispatcher) projectMCPApprovalExecution(
	ctx context.Context,
	task *corev1alpha1.Task,
	expected approvals.Approval,
	identity store.ExternalEffectIdentity,
) error {
	current, err := d.mcpApprovalCurrentTask(ctx, task)
	if err != nil || current == nil {
		return err
	}
	effectReader, ok := d.Store.(store.ExternalEffectIdentityReader)
	if !ok {
		return errors.New("approval recovery requires exact effect reads")
	}
	effect, err := effectReader.GetExternalEffectByIdentity(ctx, identity)
	if err != nil {
		return err
	}
	if effect.Identity != identity || effect.RequestDigest != expected.Binding.RequestDigest {
		return store.ErrConflict
	}
	_, err = withRetainedMCPApproval(ctx, d.EventStore, task.Namespace, task.Name, expected,
		func(txCtx context.Context, approval approvals.Approval, listed []store.ExecutionEvent) error {
			outcome, reason, result := mcpApprovalRecoveredOutcome(effect, approval.ID)
			if mcpApprovalProjectionCurrent(approval, outcome, reason) {
				return nil
			}
			return d.appendMCPApprovalRecoveryOutcome(txCtx, task, approval, effect.Version, listed, outcome, reason, result)
		})
	return err
}

// mcpApprovalProjectionCurrent reports whether the approval's public
// execution projection already reflects the recovered receipt.
func mcpApprovalProjectionCurrent(approval approvals.Approval, outcome, reason string) bool {
	return outcome == "" || (approval.ExecutionOutcome == outcome && approval.ExecutionReason == reason &&
		mcpApprovalRecoveryDecisionType(approval, outcome, reason) == "")
}

func mcpApprovalRecoveryDecisionType(approval approvals.Approval, outcome, reason string) string {
	if approval.Status != approvals.StatusPending || outcome != "not_started" {
		return ""
	}
	switch reason {
	case acpApprovalCodeDeclined:
		return events.ExecutionEventTypeApprovalDeclined
	case acpApprovalCodeExpired:
		return events.ExecutionEventTypeApprovalExpired
	case acpApprovalCodeCancelled, acpApprovalCodeStale:
		return events.ExecutionEventTypeApprovalCancelled
	default:
		return ""
	}
}

// Called only inside withRetainedMCPApproval: the decision and execution event
// commit together against its fresh retained request and observed history.
func (d *ACPDispatcher) appendMCPApprovalRecoveryOutcome(
	ctx context.Context,
	task *corev1alpha1.Task,
	approval approvals.Approval,
	version int64,
	listed []store.ExecutionEvent,
	outcome, reason string,
	result json.RawMessage,
) error {
	var source store.ExecutionEvent
	var lastSeq int64
	for _, event := range listed {
		if store.ApprovalIDFromExecutionEvent(event) != approval.ID {
			continue
		}
		if event.Type == events.ExecutionEventTypeApprovalRequested && source.ID == "" {
			source = event
		}
		lastSeq = max(lastSeq, event.Seq)
	}
	content, err := acpMCPApprovalOutcomeContent(approval.ID, string(task.UID), outcome, reason, result)
	if err != nil {
		return err
	}
	eventStore, ok := d.EventStore.(store.DeduplicatingExecutionEventStore)
	if !ok {
		return errors.New("approval recovery requires deduplicating execution events")
	}
	if decisionType := mcpApprovalRecoveryDecisionType(approval, outcome, reason); decisionType != "" {
		// Close an undecided review once its verified receipt proves that the
		// action cannot start. An existing reviewer decision remains authoritative.
		_, _, err := eventStore.AppendExecutionEventIfAbsent(ctx, &store.ExecutionEvent{
			Namespace: task.Namespace, StreamType: events.ExecutionEventStreamTypeTask, StreamID: task.Name,
			TaskName: task.Name, SessionName: source.SessionName, AgentName: source.AgentName,
			Type: decisionType, Severity: events.ExecutionEventSeverityInfo,
			ToolName: approval.TargetTool, ToolCallID: approval.ID, Summary: reason, Content: content,
		}, "acp-approval:"+approval.ID+":"+decisionType)
		if err != nil && !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	// Include the observed history position so a late stale writer cannot make
	// an earlier dedupe key prevent the next scan from repairing its projection.
	key := fmt.Sprintf("acp-approval:%s:recovery:%d:%d", approval.ID, version, lastSeq)
	_, _, err = eventStore.AppendExecutionEventIfAbsent(ctx, &store.ExecutionEvent{
		Namespace: task.Namespace, StreamType: events.ExecutionEventStreamTypeTask, StreamID: task.Name,
		TaskName: task.Name, SessionName: source.SessionName, AgentName: source.AgentName,
		Type: events.ExecutionEventTypeApprovalExecutionUpdated, Severity: events.ExecutionEventSeverityInfo,
		ToolName: approval.TargetTool, ToolCallID: approval.ID, Summary: "Tool execution " + outcome, Content: content,
	}, key)
	return err
}
