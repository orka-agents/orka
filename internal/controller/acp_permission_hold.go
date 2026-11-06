package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/liveevents"
	"github.com/orka-agents/orka/internal/store"
)

const (
	acpPermissionApprovalDomain = "acp-permission-approval-v1"
	// acpPermissionHoldMargin ends a hold before the runtime's own permission
	// timer answers the request, so the controller's answer always reaches a
	// request that is still pending.
	acpPermissionHoldMargin = 10 * time.Second
	// acpPermissionHoldMinimum is the shortest hold worth asking a person for.
	acpPermissionHoldMinimum = time.Second
)

// acpPermissionCancelBackoff paces the retries of recording a cancelled
// permission request, all within acpApprovalSettleTimeout.
var acpPermissionCancelBackoff = wait.Backoff{Duration: 100 * time.Millisecond, Factor: 2, Steps: 7, Cap: 2 * time.Second}

// acpPermissionHold is a permission request held for the Task's requester.
type acpPermissionHold struct {
	approvalID  string
	namespace   string
	taskName    string
	taskUID     string
	sessionName string
	agentName   string
	requestID   harnessv2.PermissionRequestID
	toolCallID  string
	toolName    string
	title       string
	rawInput    json.RawMessage
	options     []harnessv2.PermissionOption
	allow       harnessv2.PermissionDecision
	reject      harnessv2.PermissionDecision
	createdAt   time.Time
	expiresAt   time.Time
	binding     *approvals.CallBinding
}

type acpPermissionSend func(context.Context, harnessv2.PermissionDecision) error

// resolvePromptPermission answers a runtime permission request. The frozen
// tool policy decides first. A Task that asks for its requester's decisions
// has every request that policy would allow held instead, off the prompt
// stream, until the requester decides, the hold expires, or holdCtx ends with
// the prompt.
func (d *ACPDispatcher) resolvePromptPermission(
	ctx context.Context,
	holdCtx context.Context,
	runtimeClient *harnessv2.Client,
	sessionID harnessv2.RuntimeSessionID,
	task *corev1alpha1.Task,
	fence harnessv2.Fence,
	mcpConfiguration harnessv2.MCPPolicyConfiguration,
	provider string,
	event harnessv2.Event,
) error {
	permission := event.PermissionRequested
	if permission == nil {
		return nil
	}
	decision, hold := d.planPermissionDecision(holdCtx, task, fence, mcpConfiguration, provider, permission)
	frozen := task.DeepCopy()
	send := func(sendCtx context.Context, decision harnessv2.PermissionDecision) error {
		return d.sendPermissionDecision(sendCtx, runtimeClient, sessionID, frozen, fence, permission.RequestID, decision)
	}
	if hold == nil || !d.claimPermissionHold(hold.approvalID) {
		if hold != nil {
			// A replayed request already has a hold that will answer it.
			return nil
		}
		return send(ctx, decision)
	}
	d.permissionHoldWG.Go(func() {
		defer d.releasePermissionHold(hold.approvalID)
		d.runPermissionHold(holdCtx, send, hold)
	})
	return nil
}

// planPermissionDecision returns the decision to send now, or a hold. A
// Requester Task's frozen MCP configuration carries NativePermissionsAsk, so
// its RuntimeSession was launched to ask before every policy-allowed acting
// tool; the decision still rests only on the request's structured tool name.
func (d *ACPDispatcher) planPermissionDecision(
	holdCtx context.Context,
	task *corev1alpha1.Task,
	fence harnessv2.Fence,
	configuration harnessv2.MCPPolicyConfiguration,
	provider string,
	permission *harnessv2.PermissionRequestedEvent,
) (harnessv2.PermissionDecision, *acpPermissionHold) {
	decision := frozenMCPPermissionDecision(configuration, provider, permission)
	if !taskPermissionDecisionsByRequester(task) || !permissionDecisionAllows(permission, decision) {
		return decision, nil
	}
	// A brokered tool that requires an approval already waits for one at the
	// broker; asking at the runtime's permission prompt too would ask twice.
	if descriptor, ok := configuration.ToolPolicy.Descriptor(permission.ToolName); ok &&
		descriptor.Source.Brokered() && configuration.ApprovalPolicy.Requires(permission.ToolName) {
		return decision, nil
	}
	reject := rejectPermissionDecision(permission)
	if !taskHasVerifiedRequester(task) {
		return reject, nil
	}
	if _, ok := d.EventStore.(store.DeduplicatingExecutionEventStore); !ok || task.Status.Execution == nil {
		return reject, nil
	}
	now := time.Now().UTC()
	expiresAt := permission.ExpiresAt.Add(-acpPermissionHoldMargin)
	if deadline, ok := holdCtx.Deadline(); ok && deadline.Before(expiresAt) {
		expiresAt = deadline.UTC()
	}
	if expiresAt.Sub(now) < acpPermissionHoldMinimum {
		return reject, nil
	}
	metadata := mutationMetadata(fence, task, "permission-"+string(permission.RequestID), true, expiresAt)
	hold := &acpPermissionHold{
		namespace: task.Namespace, taskName: task.Name, taskUID: string(task.UID), sessionName: taskSessionName(task),
		agentName: taskAgentName(task), requestID: permission.RequestID, toolCallID: permission.ToolCallID,
		toolName: permission.ToolName, title: events.RedactExecutionEventText(permission.Title),
		rawInput: append(json.RawMessage(nil), permission.RawInput...),
		options:  append([]harnessv2.PermissionOption(nil), permission.Options...),
		allow:    decision, reject: reject, createdAt: now, expiresAt: expiresAt,
	}
	requestDigest, err := acpDomainDigest(acpPermissionApprovalDomain, struct {
		Namespace string                             `json:"namespace"`
		Metadata  harnessv2.MutationMetadata         `json:"metadata"`
		RequestID harnessv2.PermissionRequestID      `json:"requestID"`
		Request   harnessv2.PermissionRequestedEvent `json:"request"`
	}{Namespace: task.Namespace, Metadata: metadata, RequestID: permission.RequestID, Request: *permission})
	if err != nil {
		return reject, nil
	}
	hold.binding = &approvals.CallBinding{
		TaskAttempt: metadata.TaskAttempt, PromptID: string(metadata.PromptID),
		CallIDDigest:      store.CanonicalBytesDigest([]byte(permission.RequestID)),
		OperationIDDigest: store.CanonicalBytesDigest([]byte(metadata.OperationID)),
		RuntimeSessionUID: string(metadata.Fence.RuntimeSessionUID), RuntimeSessionGeneration: metadata.Fence.RuntimeSessionGeneration,
		RuntimeInstanceIDDigest: store.CanonicalBytesDigest([]byte(metadata.Fence.RuntimeInstanceID)),
		SupervisorBootIDDigest:  store.CanonicalBytesDigest([]byte(metadata.Fence.SupervisorBootID)),
		ControllerEpoch:         metadata.Fence.ControllerEpoch, RequestDigest: requestDigest,
	}
	hold.approvalID = acpPermissionApprovalID(task.Namespace, string(task.UID), metadata.TaskAttempt,
		string(metadata.PromptID), permission.RequestID)
	return decision, hold
}

// acpPermissionApprovalID names a held request. Its own domain keeps it apart
// from brokered tool approvals, whose recovery never adopts it.
func acpPermissionApprovalID(namespace, taskUID string, attempt uint32, promptID string, requestID harnessv2.PermissionRequestID) string {
	return store.CanonicalControlID(acpPermissionApprovalDomain, namespace, taskUID,
		strconv.FormatUint(uint64(attempt), 10), promptID, store.CanonicalBytesDigest([]byte(requestID)))
}

// runPermissionHold records the held request, waits for its durable outcome,
// and answers the runtime. Exactly one terminal approval event wins in the
// store, so the answer always matches what the approvals API reports.
func (d *ACPDispatcher) runPermissionHold(ctx context.Context, send acpPermissionSend, hold *acpPermissionHold) {
	log := logf.FromContext(ctx).WithValues("namespace", hold.namespace, "task", hold.taskName, "approval", hold.approvalID)
	if err := d.appendPermissionApprovalEvent(ctx, hold, events.ExecutionEventTypeApprovalRequested, "requested",
		"Permission requested", hold.requestedPayload()); err != nil {
		log.Error(err, "record held permission request; refusing it")
		if err := send(ctx, hold.reject); err != nil {
			log.Error(err, "refuse permission request")
		}
		return
	}
	d.publishPermissionApproval(hold, liveevents.ApprovalPending, "")

	deadline := time.NewTimer(time.Until(hold.expiresAt))
	defer deadline.Stop()
	ticker := time.NewTicker(d.permissionPollInterval())
	defer ticker.Stop()
	// Past the deadline, the expiry is recorded until it is, or another
	// outcome won. The runtime is answered only with an outcome read back
	// from the store, never one inferred from the timer.
	expiring, expiryRecorded := false, false
	for {
		if expiring && !expiryRecorded {
			err := d.appendPermissionApprovalEvent(ctx, hold, events.ExecutionEventTypeApprovalExpired, "expired",
				"Permission request expired", hold.terminalPayload("nobody answered before the request expired"))
			if err == nil || errors.Is(err, store.ErrConflict) {
				expiryRecorded = true
			} else {
				log.Error(err, "record expired permission request")
			}
		}
		status, err := d.permissionApprovalStatus(ctx, hold)
		if err != nil && ctx.Err() == nil {
			log.Error(err, "read held permission request")
		}
		if status != "" && status != approvals.StatusPending {
			d.answerPermissionHold(ctx, send, hold, status)
			return
		}
		select {
		case <-ctx.Done():
			d.cancelPermissionHold(ctx, hold)
			return
		case <-deadline.C:
			expiring = true
		case <-ticker.C:
		}
	}
}

func (d *ACPDispatcher) answerPermissionHold(ctx context.Context, send acpPermissionSend, hold *acpPermissionHold, status string) {
	decision, state, optionID := hold.reject, liveevents.ApprovalResolved, hold.reject.OptionID
	switch status {
	case approvals.StatusApproved:
		decision, optionID = hold.allow, hold.allow.OptionID
	case approvals.StatusExpired:
		state, optionID = liveevents.ApprovalExpired, ""
	case approvals.StatusCancelled:
		state, optionID = liveevents.ApprovalCancelled, ""
	}
	if err := send(ctx, decision); err != nil {
		logf.FromContext(ctx).Error(err, "answer held permission request",
			"namespace", hold.namespace, "task", hold.taskName, "approval", hold.approvalID, "status", status)
	}
	d.publishPermissionApproval(hold, state, optionID)
}

// cancelPermissionHold records that the prompt ended before the request was
// answered. The runtime already refused it when the prompt settled.
func (d *ACPDispatcher) cancelPermissionHold(ctx context.Context, hold *acpPermissionHold) {
	settleCtx, cancel := acpApprovalDetachedContext(ctx, acpApprovalSettleTimeout)
	defer cancel()
	// No hold remains after this, so a passing store failure would leave the
	// durable request pending until it expires: it is retried within the
	// settle time. A conflict means the request already ended.
	err := retry.OnError(acpPermissionCancelBackoff, func(err error) bool {
		return !errors.Is(err, store.ErrConflict) && settleCtx.Err() == nil
	}, func() error {
		return d.appendPermissionApprovalEvent(settleCtx, hold, events.ExecutionEventTypeApprovalCancelled, "cancelled",
			"Permission request cancelled", hold.terminalPayload("the prompt ended before the request was answered"))
	})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		logf.FromContext(ctx).Error(err, "record cancelled permission request",
			"namespace", hold.namespace, "task", hold.taskName, "approval", hold.approvalID)
	}
	d.publishPermissionApproval(hold, liveevents.ApprovalCancelled, "")
}

func (d *ACPDispatcher) permissionApprovalStatus(ctx context.Context, hold *acpPermissionHold) (string, error) {
	listed, err := approvals.ListEvents(ctx, d.EventStore, hold.namespace, hold.taskName)
	if err != nil {
		return "", err
	}
	for _, approval := range approvals.Derive(approvals.FilterEventsForTaskUID(listed, hold.taskUID), time.Time{}) {
		if approval.ID == hold.approvalID && approval.Kind == approvals.KindPermission &&
			approval.TaskUID == hold.taskUID && approval.Binding != nil && *approval.Binding == *hold.binding {
			return approval.Status, nil
		}
	}
	return "", nil
}

func (d *ACPDispatcher) appendPermissionApprovalEvent(ctx context.Context, hold *acpPermissionHold, eventType, key, summary string, payload any) error {
	eventStore, ok := d.EventStore.(store.DeduplicatingExecutionEventStore)
	if !ok {
		return errors.New("approval storage does not deduplicate events")
	}
	content, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, _, err = eventStore.AppendExecutionEventIfAbsent(ctx, &store.ExecutionEvent{
		Namespace: hold.namespace, StreamType: events.ExecutionEventStreamTypeTask, StreamID: hold.taskName,
		TaskName: hold.taskName, SessionName: hold.sessionName, AgentName: hold.agentName,
		Type: eventType, Severity: events.ExecutionEventSeverityInfo, ToolName: hold.toolName,
		ToolCallID: hold.approvalID, Summary: summary, Content: content,
	}, "acp-permission:"+hold.approvalID+":"+key)
	return err
}

func (h *acpPermissionHold) requestedPayload() any {
	options := make([]approvals.Option, 0, len(h.options))
	for _, option := range h.options {
		options = append(options, approvals.Option{OptionID: option.OptionID, Name: option.Name, Kind: string(option.Kind)})
	}
	action := "Allow the agent to run a tool"
	if h.toolName != "" {
		action = "Allow the agent to run " + h.toolName
	}
	return struct {
		ApprovalID  string                 `json:"approvalID"`
		Kind        string                 `json:"kind"`
		Action      string                 `json:"action"`
		RiskSummary string                 `json:"riskSummary"`
		TaskUID     string                 `json:"taskUID"`
		TargetTool  string                 `json:"targetTool,omitempty"`
		Severity    string                 `json:"severity"`
		ToolCallID  string                 `json:"toolCallID,omitempty"`
		Options     []approvals.Option     `json:"options"`
		Binding     *approvals.CallBinding `json:"binding"`
		Timeout     string                 `json:"timeout"`
		ExpiresAt   time.Time              `json:"expiresAt"`
	}{
		ApprovalID: h.approvalID, Kind: approvals.KindPermission, Action: action, RiskSummary: h.title,
		TaskUID: h.taskUID, TargetTool: h.toolName, Severity: "warning", ToolCallID: h.toolCallID,
		Options: options, Binding: h.binding, Timeout: h.expiresAt.Sub(h.createdAt).Round(time.Second).String(),
		ExpiresAt: h.expiresAt,
	}
}

func (h *acpPermissionHold) terminalPayload(reason string) any {
	return struct {
		ApprovalID string `json:"approvalID"`
		TaskUID    string `json:"taskUID"`
		Reason     string `json:"reason"`
	}{ApprovalID: h.approvalID, TaskUID: h.taskUID, Reason: reason}
}

func (d *ACPDispatcher) publishPermissionApproval(hold *acpPermissionHold, state liveevents.ApprovalState, optionID string) {
	d.LiveEvents.Publish(liveevents.Event{
		Namespace: hold.namespace, Task: hold.taskName, TaskUID: hold.taskUID,
		Approval: &liveevents.Approval{
			ID: hold.approvalID, State: state, ToolCallID: hold.toolCallID, ToolName: hold.toolName, Title: hold.title,
			Input:   append(json.RawMessage(nil), hold.rawInput...),
			Options: append([]harnessv2.PermissionOption(nil), hold.options...), ExpiresAt: hold.expiresAt, OptionID: optionID,
		},
	})
}

func (d *ACPDispatcher) permissionPollInterval() time.Duration {
	if d.permissionHoldPollInterval > 0 {
		return d.permissionHoldPollInterval
	}
	return acpApprovalDefaultPollInterval
}

func (d *ACPDispatcher) claimPermissionHold(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.permissionHolds[id]; exists {
		return false
	}
	if d.permissionHolds == nil {
		d.permissionHolds = make(map[string]struct{})
	}
	d.permissionHolds[id] = struct{}{}
	return true
}

func (d *ACPDispatcher) releasePermissionHold(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.permissionHolds, id)
}

// sendPermissionDecision delivers one answer to the runtime.
func (d *ACPDispatcher) sendPermissionDecision(
	ctx context.Context,
	runtimeClient *harnessv2.Client,
	sessionID harnessv2.RuntimeSessionID,
	task *corev1alpha1.Task,
	fence harnessv2.Fence,
	requestID harnessv2.PermissionRequestID,
	decision harnessv2.PermissionDecision,
) error {
	request := harnessv2.ResolvePermissionRequest{
		Protocol:  harnessv2.ProtocolVersion,
		Metadata:  mutationMetadata(fence, task, "permission-"+string(requestID), true, time.Now().UTC().Add(30*time.Second)),
		RequestID: requestID, Decision: decision,
	}
	return d.resolvePermissionRequest(ctx, runtimeClient, sessionID, request)
}

func taskPermissionDecisionsByRequester(task *corev1alpha1.Task) bool {
	return task != nil && taskNativePermissions(task.Spec.AgentRuntime) == harnessv2.NativePermissionsAsk
}

// taskNativePermissions is the RuntimeSession launch mode a Task's permission
// decisions require. A requester can decide only the requests a runtime
// sends, so a Requester Task's runtime must be launched to ask before each
// policy-allowed native tool that acts.
func taskNativePermissions(runtime *corev1alpha1.AgentRuntimeSpec) harnessv2.NativePermissionMode {
	if runtime != nil && runtime.PermissionDecisions == corev1alpha1.PermissionDecisionsRequester {
		return harnessv2.NativePermissionsAsk
	}
	return harnessv2.NativePermissionsPreapproved
}

// errPermissionDecisionsRuntimeUnsupported rejects a Requester Task whose
// runtime does not advertise that it can launch a session that asks.
var errPermissionDecisionsRuntimeUnsupported = errors.New(
	"the runtime cannot launch a session that asks before each tool, so the requester could not decide its permission requests")

// rejectPermissionDecisionsRuntime fails a Requester Task before any
// RuntimeSession exists, instead of running its tools without asking.
func (d *ACPDispatcher) rejectPermissionDecisionsRuntime(
	ctx context.Context, task *corev1alpha1.Task, attemptID string, fence store.ControllerEpochFence,
) error {
	reason := corev1alpha1.TaskExecutionReason("PermissionDecisionsUnsupported")
	message := errPermissionDecisionsRuntimeUnsupported.Error()
	if err := d.transitionAttemptToFailed(ctx, attemptID, fence, "permission-decisions-unsupported", reason, message); err != nil {
		return err
	}
	return d.failTaskBeforeSessionBinding(ctx, task, corev1alpha1.TaskExecutionStateFailed, corev1alpha1.TaskExecutionOutcomeFailed, reason, message)
}

// taskHasVerifiedRequester reports whether the API server stamped the Task's
// requester from a verified sign-in. Admission keeps that stamp immutable.
func taskHasVerifiedRequester(task *corev1alpha1.Task) bool {
	if task == nil || task.Spec.RequestedBy == nil ||
		task.Annotations[labels.AnnotationRequestedBySource] != labels.RequestedBySourceAPI {
		return false
	}
	return strings.TrimSpace(task.Spec.RequestedBy.Issuer) != "" && strings.TrimSpace(task.Spec.RequestedBy.Subject) != ""
}

func taskAgentName(task *corev1alpha1.Task) string {
	if task.Status.AgentExecutionBinding != nil && task.Status.AgentExecutionBinding.Agent != nil {
		return task.Status.AgentExecutionBinding.Agent.Name
	}
	if task.Spec.AgentRef != nil {
		return task.Spec.AgentRef.Name
	}
	return ""
}

func permissionDecisionAllows(permission *harnessv2.PermissionRequestedEvent, decision harnessv2.PermissionDecision) bool {
	if decision.Outcome != harnessv2.PermissionDecisionSelected {
		return false
	}
	for _, option := range permission.Options {
		if option.OptionID == decision.OptionID {
			return option.Kind == harnessv2.PermissionOptionAllowOnce
		}
	}
	return false
}

func rejectPermissionDecision(permission *harnessv2.PermissionRequestedEvent) harnessv2.PermissionDecision {
	for _, option := range permission.Options {
		if option.Kind == harnessv2.PermissionOptionRejectOnce {
			return harnessv2.PermissionDecision{Outcome: harnessv2.PermissionDecisionSelected, OptionID: option.OptionID}
		}
	}
	return harnessv2.PermissionDecision{Outcome: harnessv2.PermissionDecisionCancelled}
}
