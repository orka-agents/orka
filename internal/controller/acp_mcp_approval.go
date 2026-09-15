package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

const (
	acpApprovalSecretKey      = "call.json"
	acpApprovalCodeCancelled  = "approval_cancelled"
	acpApprovalCodeExpired    = "approval_expired"
	acpApprovalCodeDeclined   = "approval_declined"
	acpApprovalCodeStale      = "approval_stale"
	acpApprovalStaleMessage   = "The original task or tool authority is no longer valid."
	acpApprovalCodeUnknown    = "tool_outcome_unknown"
	acpApprovalOutcomeFailed  = "failed"
	acpApprovalOutcomeUnknown = "unknown"
	acpMCPToolEffectKind      = "acp-mcp-tool"
)

// acpMCPApprovalCall is private executable input, never an event payload. The
// immutable, Task-owned Secret keeps the original arguments across redelivery
// and controller restart without exposing them in Task status or approval APIs.
type acpMCPApprovalCall struct {
	ID            string                         `json:"id"`
	RequestDigest string                         `json:"requestDigest"`
	Task          ACPMCPAuthenticatedTask        `json:"task"`
	Request       harnessv2.MCPBrokerCallRequest `json:"request"`
	Descriptor    harnessv2.MCPToolDescriptor    `json:"descriptor"`
	CreatedAt     time.Time                      `json:"createdAt"`
	ExpiresAt     time.Time                      `json:"expiresAt"`
}

func acpMCPApprovalIdentity(request harnessv2.MCPBrokerCallRequest) string {
	// Bind the transport call ID independently of its proposed arguments. A
	// changed payload with the same call identity conflicts instead of silently
	// creating a second review. Different genuine calls always need a new review.
	return acpMCPApprovalIdentityFromCallDigest(request.Namespace,
		string(request.Metadata.TaskUID), fmt.Sprint(request.Metadata.TaskAttempt),
		string(request.Metadata.PromptID), store.CanonicalBytesDigest([]byte(request.Call.CallID)))
}

func acpMCPApprovalIdentityFromCallDigest(namespace, taskUID, taskAttempt, promptID, callIDDigest string) string {
	// The digest survives event redaction even when the call ID contains a URL
	// query or other sensitive text. A distinct domain separates legacy raw IDs.
	return store.CanonicalControlID("acp-tool-approval-v2", namespace, taskUID, taskAttempt, promptID, callIDDigest)
}

func acpMCPApprovalRequestDigest(request harnessv2.MCPBrokerCallRequest, descriptor harnessv2.MCPToolDescriptor) (string, error) {
	return acpDomainDigest("acp-tool-approval-request", struct {
		Namespace   string                           `json:"namespace"`
		Fence       harnessv2.Fence                  `json:"fence"`
		TaskUID     harnessv2.TaskUID                `json:"taskUID"`
		TaskAttempt uint32                           `json:"taskAttempt"`
		PromptID    harnessv2.PromptID               `json:"promptID"`
		OperationID harnessv2.OperationID            `json:"operationID"`
		Call        harnessv2.MCPToolCall            `json:"call"`
		Descriptor  harnessv2.MCPToolDescriptor      `json:"descriptor"`
		Policy      harnessv2.MCPPolicyConfiguration `json:"policy"`
	}{
		Namespace: request.Namespace, Fence: request.Metadata.Fence,
		TaskUID: request.Metadata.TaskUID, TaskAttempt: request.Metadata.TaskAttempt,
		PromptID: request.Metadata.PromptID, OperationID: request.Metadata.OperationID,
		Call: request.Call, Descriptor: descriptor, Policy: request.Authorization.Configuration(),
	})
}

func (b *ACPMCPBroker) serveApprovedCall(w http.ResponseWriter, ctx context.Context, request harnessv2.MCPBrokerCallRequest, descriptor harnessv2.MCPToolDescriptor, credentials ACPMCPBrokerCredentials) {
	if b.ApprovalEvents == nil || b.ApprovalSecrets == nil || b.EpochMutations == nil ||
		credentials.Task.Name == "" || credentials.Task.UID != string(request.Metadata.TaskUID) {
		writeACPMCPError(w, http.StatusServiceUnavailable, "MCP approval storage is unavailable")
		return
	}
	call, secretUID, err := b.persistApprovalCall(ctx, request, descriptor, credentials.Task)
	if err != nil {
		writeACPMCPError(w, http.StatusConflict, "MCP approval call does not match its stored action")
		return
	}
	identity := store.ExternalEffectIdentity{
		Kind: acpMCPToolEffectKind, Namespace: request.Namespace,
		AggregateID: string(request.Authorization.RuntimeSessionUID), OperationID: string(request.Metadata.OperationID),
	}
	effect, err := b.Effects.ReserveExternalEffect(ctx, store.ReserveExternalEffectRequest{
		Identity: identity, RequestDigest: call.RequestDigest, Fence: credentials.ControllerFence, CreatedAt: call.CreatedAt,
		ApprovalTaskUID: call.Task.UID,
	})
	if err != nil {
		writeACPMCPError(w, http.StatusConflict, "MCP approval operation conflicts with a previous call")
		return
	}
	if err := b.requestToolApproval(ctx, call); err != nil {
		writeACPMCPError(w, http.StatusServiceUnavailable, "MCP approval could not be recorded")
		return
	}
	result, replayed, err := b.waitAndExecuteApproval(ctx, call, secretUID, effect, credentials)
	if err != nil {
		writeACPMCPError(w, http.StatusServiceUnavailable, "MCP approval continuation is unavailable; the original call must not be repeated")
		return
	}
	writeACPMCPJSON(w, http.StatusOK, harnessv2.MCPBrokerCallResponse{
		Protocol: harnessv2.ProtocolVersion, CallID: request.Call.CallID,
		Result: result, IsError: mcpToolResultIsError(result), Replayed: replayed,
	})
}

func (b *ACPMCPBroker) persistApprovalCall(ctx context.Context, request harnessv2.MCPBrokerCallRequest, descriptor harnessv2.MCPToolDescriptor, task ACPMCPAuthenticatedTask) (*acpMCPApprovalCall, types.UID, error) {
	digest, err := acpMCPApprovalRequestDigest(request, descriptor)
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	wait := b.ApprovalWaitTimeout
	if wait <= 0 || wait > harnessv2.MCPApprovalWaitTimeout {
		wait = harnessv2.MCPApprovalWaitTimeout
	}
	call := acpMCPApprovalCall{
		ID: acpMCPApprovalIdentity(request), RequestDigest: digest, Task: task,
		Request: request, Descriptor: descriptor, CreatedAt: now, ExpiresAt: now.Add(wait),
	}
	if !task.Deadline.IsZero() && task.Deadline.Before(call.ExpiresAt) {
		call.ExpiresAt = task.Deadline
	}
	body, err := json.Marshal(call)
	if err != nil {
		return nil, "", err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: acpApprovalSecretName(call.ID), Namespace: task.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: taskResourceKind, Name: task.Name,
				UID: types.UID(task.UID), Controller: new(true), BlockOwnerDeletion: new(true),
			}},
		},
		Type: corev1.SecretType("orka.ai/tool-approval"), Immutable: new(true),
		Data: map[string][]byte{acpApprovalSecretKey: body},
	}
	_, err = b.ApprovalSecrets.CoreV1().Secrets(task.Namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, "", err
	}
	return b.loadApprovalCall(ctx, &call)
}

func acpApprovalSecretName(id string) string {
	return "acp-approval-" + strings.TrimPrefix(store.CanonicalBytesDigest([]byte(id)), "sha256:")
}

func (b *ACPMCPBroker) loadApprovalCall(ctx context.Context, expected *acpMCPApprovalCall) (*acpMCPApprovalCall, types.UID, error) {
	secret, err := b.ApprovalSecrets.CoreV1().Secrets(expected.Task.Namespace).Get(ctx, acpApprovalSecretName(expected.ID), metav1.GetOptions{})
	if err != nil {
		return nil, "", err
	}
	owner := metav1.GetControllerOf(secret)
	if secret.Immutable == nil || !*secret.Immutable || secret.Type != corev1.SecretType("orka.ai/tool-approval") ||
		!secret.DeletionTimestamp.IsZero() || owner == nil || owner.APIVersion != corev1alpha1.GroupVersion.String() ||
		owner.Kind != taskResourceKind || owner.Name != expected.Task.Name || string(owner.UID) != expected.Task.UID {
		return nil, "", errors.New("stored approval ownership is invalid")
	}
	var stored acpMCPApprovalCall
	if err := json.Unmarshal(secret.Data[acpApprovalSecretKey], &stored); err != nil {
		return nil, "", errors.New("stored approval call is invalid")
	}
	digest, err := acpMCPApprovalRequestDigest(stored.Request, stored.Descriptor)
	if err != nil || stored.ID != expected.ID || stored.ID != acpMCPApprovalIdentity(stored.Request) ||
		stored.Task.UID != expected.Task.UID || stored.Task.Name != expected.Task.Name || stored.Task.Namespace != expected.Task.Namespace ||
		stored.RequestDigest != expected.RequestDigest || digest != expected.RequestDigest || stored.CreatedAt.IsZero() ||
		stored.ExpiresAt.IsZero() || stored.ExpiresAt.After(stored.CreatedAt.Add(harnessv2.MCPApprovalWaitTimeout)) {
		return nil, "", errors.New("stored approval call binding changed")
	}
	return &stored, secret.UID, nil
}

func approvalCallBinding(call *acpMCPApprovalCall) *approvals.CallBinding {
	m := call.Request.Metadata
	return &approvals.CallBinding{
		TaskAttempt: m.TaskAttempt, PromptID: string(m.PromptID), OperationID: string(m.OperationID),
		CallIDDigest:      store.CanonicalBytesDigest([]byte(call.Request.Call.CallID)),
		RuntimeSessionUID: string(m.Fence.RuntimeSessionUID), RuntimeSessionGeneration: m.Fence.RuntimeSessionGeneration,
		RuntimeInstanceID: string(m.Fence.RuntimeInstanceID), SupervisorBootID: string(m.Fence.SupervisorBootID),
		ControllerEpoch: m.Fence.ControllerEpoch, RequestDigest: call.RequestDigest,
	}
}

func (b *ACPMCPBroker) requestToolApproval(ctx context.Context, call *acpMCPApprovalCall) error {
	specDigest, err := approvals.TargetSpecDigest(struct {
		Descriptor harnessv2.MCPToolDescriptor      `json:"descriptor"`
		Policy     harnessv2.MCPPolicyConfiguration `json:"policy"`
	}{
		Descriptor: call.Descriptor, Policy: call.Request.Authorization.Configuration(),
	})
	if err != nil {
		return err
	}
	action := "Execute " + call.Request.Call.ToolName
	if call.Request.Call.ToolName == "call-tool" || call.Request.Call.ToolName == "call_tool" {
		var args struct {
			Name      json.RawMessage `json:"name"`
			ToolName  json.RawMessage `json:"toolName"`
			Tool      json.RawMessage `json:"tool"`
			Operation json.RawMessage `json:"operation"`
		}
		if json.Unmarshal(call.Request.Call.Arguments, &args) == nil {
			for _, value := range []json.RawMessage{args.Name, args.ToolName, args.Tool, args.Operation} {
				var operation string
				if json.Unmarshal(value, &operation) == nil && strings.TrimSpace(operation) != "" {
					action = "Execute " + operation + " via " + call.Request.Call.ToolName
					break
				}
			}
		}
	}
	target, err := approvals.NewApprovalTarget(call.Task.Namespace, call.Task.Name, call.Task.UID,
		call.Request.Call.ToolName, call.Request.Call.Arguments, action,
		"Review this exact operation and its inputs. The tool has not run.", "warning", specDigest)
	if err != nil {
		return err
	}
	target.ApprovalID = call.ID
	return b.appendApprovalEvent(ctx, call, events.ExecutionEventTypeApprovalRequested, "requested", target.Action, struct {
		approvals.ApprovalTarget
		ToolCallID string                 `json:"toolCallID"`
		Binding    *approvals.CallBinding `json:"binding"`
		Timeout    string                 `json:"timeout"`
		ExpiresAt  time.Time              `json:"expiresAt"`
	}{target, call.Request.Call.CallID, approvalCallBinding(call), call.ExpiresAt.Sub(call.CreatedAt).String(), call.ExpiresAt})
}

func (b *ACPMCPBroker) appendApprovalEvent(ctx context.Context, call *acpMCPApprovalCall, eventType, key, summary string, payload any) error {
	content, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, _, err = b.ApprovalEvents.AppendExecutionEventIfAbsent(ctx, &store.ExecutionEvent{
		Namespace: call.Task.Namespace, StreamType: events.ExecutionEventStreamTypeTask, StreamID: call.Task.Name,
		TaskName: call.Task.Name, SessionName: call.Task.SessionName, AgentName: call.Task.AgentName,
		Type: eventType, Severity: events.ExecutionEventSeverityInfo, ToolName: call.Request.Call.ToolName,
		ToolCallID: call.ID, Summary: summary, Content: content,
	}, "acp-approval:"+call.ID+":"+key)
	return err
}

func (b *ACPMCPBroker) approvalOutcome(ctx context.Context, call *acpMCPApprovalCall, outcome, reason string, result json.RawMessage) error {
	payload := struct {
		ApprovalID       string `json:"approvalID"`
		TaskUID          string `json:"taskUID"`
		ExecutionOutcome string `json:"executionOutcome"`
		Reason           string `json:"reason"`
		ResultDigest     string `json:"resultDigest,omitempty"`
	}{ApprovalID: call.ID, TaskUID: call.Task.UID, ExecutionOutcome: outcome, Reason: reason}
	if len(result) > 0 {
		payload.ResultDigest = store.CanonicalBytesDigest(result)
	}
	return b.appendApprovalEvent(ctx, call, events.ExecutionEventTypeApprovalExecutionUpdated, "execution:"+outcome,
		"Approved tool execution "+outcome, payload)
}

func (b *ACPMCPBroker) approvalDecision(ctx context.Context, call *acpMCPApprovalCall, eventType, reason string) error {
	err := b.appendApprovalEvent(ctx, call, eventType, eventType, reason,
		struct {
			ApprovalID string `json:"approvalID"`
			TaskUID    string `json:"taskUID"`
			Reason     string `json:"reason"`
		}{ApprovalID: call.ID, TaskUID: call.Task.UID, Reason: reason})
	if errors.Is(err, store.ErrConflict) {
		return nil // A reviewer decision may have won; execution still rechecks authority and expiry.
	}
	return err
}

func (b *ACPMCPBroker) waitAndExecuteApproval(ctx context.Context, call *acpMCPApprovalCall, secretUID types.UID, effect *store.ExternalEffect, credentials ACPMCPBrokerCredentials) (json.RawMessage, bool, error) {
	poll := b.ApprovalPollInterval
	if poll <= 0 {
		poll = time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return b.interruptedApproval(ctx, call, effect, credentials)
		}
		if effect.State == store.ExternalEffectFailed || effect.State == store.ExternalEffectSucceeded {
			result, err := b.replayApprovalResult(ctx, call, effect)
			return result, true, err
		}
		if effect.State == store.ExternalEffectInFlight && effect.LeaseExpiresAt != nil && time.Now().UTC().Before(*effect.LeaseExpiresAt) {
			// Join a concurrent delivery until it records a result. Waiting never
			// transfers its execution lease or starts another action.
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case <-ticker.C:
			}
			var err error
			effect, err = b.Effects.GetExternalEffect(ctx, effect.ID)
			if err != nil {
				return nil, false, err
			}
			continue
		}
		if effect.State == store.ExternalEffectInFlight || effect.State == store.ExternalEffectOutcomeUnknown {
			// Never reclaim a started approval call, even after its lease expires.
			// The action may already have reached an external system.
			result := acpApprovalError(call.ID, acpApprovalCodeUnknown)
			if err := b.approvalOutcome(ctx, call, acpApprovalOutcomeUnknown, acpMCPApprovalUnknownReason, nil); err != nil {
				return nil, false, err
			}
			return result, false, nil
		}
		decision, err := b.loadApprovalDecision(ctx, call)
		if err != nil {
			if ctx.Err() != nil {
				return b.interruptedApproval(ctx, call, effect, credentials)
			}
			return nil, false, err
		}
		switch decision.Status {
		case approvals.StatusDeclined:
			return b.blockApproval(ctx, call, credentials, acpApprovalCodeDeclined)
		case approvals.StatusExpired:
			return b.blockApproval(ctx, call, credentials, acpApprovalCodeExpired)
		case approvals.StatusCancelled:
			return b.blockApproval(ctx, call, credentials, acpApprovalCodeCancelled)
		}
		if !time.Now().UTC().Before(call.ExpiresAt) {
			if err := b.approvalDecision(ctx, call, events.ExecutionEventTypeApprovalExpired, "Approval wait expired"); err != nil {
				return nil, false, err
			}
			return b.blockApproval(ctx, call, credentials, acpApprovalCodeExpired)
		}
		switch decision.Status {
		case approvals.StatusApproved:
			// The prompt authority watcher cancels a pending wait when its
			// durable lease ends. Resolve credentials and policy only when
			// approval could authorize execution, outside the polling path.
			if err := b.revalidateApproval(ctx, call, credentials); err != nil {
				if ctx.Err() != nil {
					return b.interruptedApproval(ctx, call, effect, credentials)
				}
				return b.revokeApproval(ctx, call, credentials, err)
			}
			if effect.State != store.ExternalEffectPending {
				return b.blockApproval(ctx, call, credentials, acpApprovalCodeStale)
			}
			return b.executeApprovedCall(ctx, call, secretUID, effect, credentials)
		}
		select {
		case <-ctx.Done():
			return b.interruptedApproval(ctx, call, effect, credentials)
		case <-ticker.C:
		}
		// A second delivery may have executed while this handler waited.
		current, err := b.Effects.GetExternalEffect(ctx, effect.ID)
		if err != nil {
			if ctx.Err() != nil {
				return b.interruptedApproval(ctx, call, effect, credentials)
			}
			return nil, false, err
		}
		effect = current
	}
}

func (b *ACPMCPBroker) loadApprovalDecision(ctx context.Context, call *acpMCPApprovalCall) (*approvals.Approval, error) {
	listed, err := approvals.ListEvents(ctx, b.ApprovalEvents, call.Task.Namespace, call.Task.Name)
	if err != nil {
		return nil, err
	}
	for _, decision := range approvals.Derive(approvals.FilterEventsForTaskUID(listed, call.Task.UID), time.Time{}) {
		if decision.ID != call.ID {
			continue
		}
		if decision.TaskUID != call.Task.UID || decision.Binding == nil ||
			*decision.Binding != *approvalCallBinding(call) || decision.ExpiresAt == nil || !decision.ExpiresAt.Equal(call.ExpiresAt) {
			return nil, errors.New("approval decision binding is invalid")
		}
		return &decision, nil
	}
	return nil, errors.New("approval decision is missing")
}

func (b *ACPMCPBroker) replayApprovalResult(ctx context.Context, call *acpMCPApprovalCall, effect *store.ExternalEffect) (json.RawMessage, error) {
	outcome, reason, result, err := acpMCPApprovalReceiptOutcome(effect, call.ID)
	if err != nil {
		return nil, err
	}
	if err := b.approvalOutcome(ctx, call, outcome, reason, result); err != nil {
		return nil, err
	}
	return result, nil
}

func (b *ACPMCPBroker) interruptedApproval(ctx context.Context, call *acpMCPApprovalCall, effect *store.ExternalEffect, credentials ACPMCPBrokerCredentials) (json.RawMessage, bool, error) {
	// A transport interruption alone must not erase a pending review. Exact
	// redelivery can resume it while the original authority survives.
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	current, err := b.Effects.GetExternalEffect(checkCtx, effect.ID)
	if err != nil || (current.State != store.ExternalEffectPending && current.State != store.ExternalEffectFailed) {
		// A concurrent delivery may already have started. Its execution owner
		// records the result or unknown outcome; do not call it unstarted here.
		return nil, false, ctx.Err()
	}
	if err := b.revalidateApproval(checkCtx, call, credentials); err != nil {
		return b.revokeApproval(checkCtx, call, credentials, err)
	}
	return nil, false, ctx.Err()
}

func (b *ACPMCPBroker) revokeApproval(ctx context.Context, call *acpMCPApprovalCall, credentials ACPMCPBrokerCredentials, cause error) (json.RawMessage, bool, error) {
	// Revocation records evidence only. The authority watcher can cancel the
	// call after revalidation fails, so settlement needs its own bounded context.
	settleCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	code := acpApprovalCodeStale
	eventType := events.ExecutionEventTypeApprovalCancelled
	if errors.Is(cause, errACPMCPTaskCancelled) {
		code = acpApprovalCodeCancelled
	} else if errors.Is(cause, errACPMCPTaskExpired) {
		code = acpApprovalCodeExpired
		eventType = events.ExecutionEventTypeApprovalExpired
	}
	if err := b.approvalDecision(settleCtx, call, eventType, code); err != nil {
		return nil, false, err
	}
	return b.blockApproval(settleCtx, call, credentials, code)
}

func (b *ACPMCPBroker) revalidateApproval(ctx context.Context, call *acpMCPApprovalCall, credentials ACPMCPBrokerCredentials) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !credentials.Task.Deadline.IsZero() && !time.Now().UTC().Before(credentials.Task.Deadline) {
		return errACPMCPTaskExpired
	}
	return b.taskDataGuard(call.Request, credentials)(ctx, func(checkCtx context.Context) error {
		return b.validateApprovalTool(checkCtx, call)
	})
}

func (b *ACPMCPBroker) validateApprovalTool(ctx context.Context, call *acpMCPApprovalCall) error {
	if validator, ok := b.Executor.(interface {
		ValidateACPMCPTool(context.Context, harnessv2.MCPBrokerCallRequest, harnessv2.MCPToolDescriptor) error
	}); ok {
		return validator.ValidateACPMCPTool(ctx, call.Request, call.Descriptor)
	}
	return nil
}

func (b *ACPMCPBroker) blockApproval(ctx context.Context, call *acpMCPApprovalCall, credentials ACPMCPBrokerCredentials, code string) (json.RawMessage, bool, error) {
	identity := store.ExternalEffectIdentity{
		Kind: acpMCPToolEffectKind, Namespace: call.Request.Namespace,
		AggregateID: string(call.Request.Authorization.RuntimeSessionUID), OperationID: string(call.Request.Metadata.OperationID),
	}
	id, err := identity.CanonicalID()
	if err != nil {
		return nil, false, err
	}
	current, err := b.Effects.GetExternalEffect(ctx, id)
	if err != nil {
		return nil, false, err
	}
	result := acpApprovalError(call.ID, code)
	switch current.State {
	case store.ExternalEffectPending:
		// A delivered denial is final even when the reviewer had already
		// approved and transiently revoked authority later becomes valid again.
		_, err = b.Effects.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
			ID: id, Fence: credentials.ControllerFence, ExpectedVersion: current.Version,
			ExpectedState: store.ExternalEffectPending, NewState: store.ExternalEffectFailed,
			RequestDigest: call.RequestDigest, Response: result, ResponseDigest: store.CanonicalBytesDigest(result),
			UpdatedAt: time.Now().UTC(),
		})
		if err != nil {
			return nil, false, err
		}
	case store.ExternalEffectFailed:
		_, code, result, err = acpMCPApprovalReceiptOutcome(current, call.ID)
		if err != nil {
			return nil, false, err
		}
	default:
		// Another delivery may have claimed the call. It owns the execution
		// outcome; this waiter must not report that an action was unstarted.
		return nil, false, store.ErrConflict
	}
	if err := b.approvalOutcome(ctx, call, "not_started", code, result); err != nil {
		return nil, false, err
	}
	return result, false, nil
}

func (b *ACPMCPBroker) executeApprovedCall(ctx context.Context, call *acpMCPApprovalCall, secretUID types.UID, effect *store.ExternalEffect, credentials ACPMCPBrokerCredentials) (json.RawMessage, bool, error) {
	stored, currentUID, err := b.loadApprovalCall(ctx, call)
	if err != nil || currentUID != secretUID || !stored.ExpiresAt.Equal(call.ExpiresAt) || !time.Now().UTC().Before(call.ExpiresAt) {
		return b.blockApproval(ctx, call, credentials, acpApprovalCodeStale)
	}
	// Execute only the private persisted request, never the review preview or
	// the redelivered caller's arguments.
	call = stored
	now := time.Now().UTC()
	leaseOwner := externalEffectLeaseOwner(credentials.ControllerFence, effect.Identity, now)
	leaseExpiry := now.Add(harnessv2.MCPApprovalExecutionTimeout + externalEffectLeaseSettlementMargin)
	claimed, err := b.Effects.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
		ID: effect.ID, Fence: credentials.ControllerFence, ExpectedVersion: effect.Version,
		ExpectedState: store.ExternalEffectPending, NewState: store.ExternalEffectInFlight,
		RequestDigest: call.RequestDigest, ExpectedLeaseOwner: effect.LeaseOwner,
		LeaseOwner: leaseOwner, LeaseExpiresAt: &leaseExpiry, UpdatedAt: now,
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			current, readErr := b.Effects.GetExternalEffect(ctx, effect.ID)
			if readErr == nil && current.State != store.ExternalEffectPending {
				return b.waitAndExecuteApproval(ctx, call, secretUID, current, credentials)
			}
		}
		return nil, false, err
	}
	callCtx, cancel := context.WithTimeout(ctx, harnessv2.MCPApprovalExecutionTimeout)
	defer cancel()
	if !credentials.Task.Deadline.IsZero() {
		var stop context.CancelFunc
		callCtx, stop = context.WithDeadline(callCtx, credentials.Task.Deadline)
		defer stop()
	}
	// Cancellation uses this same short epoch interlock. Record authorization
	// to start under it, then release it before doing any remote tool I/O.
	err = b.taskDataGuard(call.Request, credentials)(callCtx, func(guardCtx context.Context) error {
		if !time.Now().UTC().Before(call.ExpiresAt) {
			return errACPMCPTaskExpired
		}
		if err := b.validateApprovalTool(guardCtx, call); err != nil {
			return err
		}
		return b.approvalOutcome(guardCtx, call, "running", "Approved action started", nil)
	})
	if err == nil {
		// Tool validation and the running event may block on storage. Check
		// the live lease again after those reads and writes before execution.
		err = b.Prompts.AuthorizeACPMCPPrompt(callCtx, call.Request)
	}
	if err != nil {
		settleCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		result := acpApprovalError(call.ID, acpApprovalCodeStale)
		if errors.Is(err, errACPMCPTaskCancelled) {
			result = acpApprovalError(call.ID, acpApprovalCodeCancelled)
		} else if errors.Is(err, errACPMCPTaskExpired) {
			result = acpApprovalError(call.ID, acpApprovalCodeExpired)
		}
		if settleErr := settleExternalEffectStore(settleCtx, b.Effects, credentials.ControllerFence, effect.Identity, store.ExternalEffectFailed, result); settleErr != nil {
			return nil, false, settleErr
		}
		return b.revokeApproval(settleCtx, call, credentials, err)
	}
	result, executeErr := b.Executor.ExecuteACPMCPTool(withACPMCPAuthenticatedTask(callCtx, credentials.Task), call.Request, call.Descriptor)
	if executeErr == nil {
		executeErr = callCtx.Err()
	}
	if executeErr == nil {
		executeErr = b.revalidateApproval(callCtx, call, credentials)
	}
	if executeErr == nil {
		result, executeErr = canonicalMCPApprovalResult(result)
	}
	if executeErr == nil {
		_, executeErr = b.Effects.TransitionExternalEffect(callCtx, store.ExternalEffectTransition{
			ID: claimed.ID, Fence: credentials.ControllerFence, ExpectedVersion: claimed.Version,
			ExpectedState: store.ExternalEffectInFlight, NewState: store.ExternalEffectSucceeded,
			RequestDigest: call.RequestDigest, ExpectedLeaseOwner: leaseOwner,
			Response: result, ResponseDigest: store.CanonicalBytesDigest(result), UpdatedAt: time.Now().UTC(),
		})
	}
	settleCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	if executeErr != nil {
		_ = settleExternalEffectStore(settleCtx, b.Effects, credentials.ControllerFence, effect.Identity, store.ExternalEffectOutcomeUnknown, nil)
		if err := b.approvalOutcome(settleCtx, call, acpApprovalOutcomeUnknown, acpMCPApprovalUnknownReason, nil); err != nil {
			return nil, false, err
		}
		return acpApprovalError(call.ID, acpApprovalCodeUnknown), false, nil
	}
	outcome := "succeeded"
	if mcpToolResultIsError(result) {
		outcome = acpApprovalOutcomeFailed
	}
	if err := b.approvalOutcome(settleCtx, call, outcome, "Recorded tool result", result); err != nil {
		return nil, false, err
	}
	return result, false, nil
}

// Kubernetes stores responses as JSON values and can change their field order,
// number spelling, and escaping. Hash and replay the same canonical form on
// both sides of storage so a valid saved receipt remains verifiable.
func canonicalMCPApprovalResult(result json.RawMessage) (json.RawMessage, error) {
	if len(result) == 0 || len(result) > harnessv2.MaxMCPResultBytes {
		return nil, errors.New("approved tool returned an invalid result")
	}
	canonical, err := harnessv2.CanonicalJSON(result)
	if err != nil || len(canonical) > harnessv2.MaxMCPResultBytes {
		return nil, errors.New("approved tool returned an invalid result")
	}
	return canonical, nil
}

func acpApprovalError(id, code string) json.RawMessage {
	messages := map[string]string{
		acpApprovalCodeDeclined:  "Tool execution was declined by the reviewer.",
		acpApprovalCodeExpired:   "The approval expired before tool execution.",
		acpApprovalCodeCancelled: "The approval was cancelled before tool execution.",
		acpApprovalCodeStale:     acpApprovalStaleMessage,
		acpApprovalCodeUnknown:   "The tool may have run. Do not repeat it automatically.",
	}
	result, _ := harnessv2.CanonicalValue(struct {
		IsError    bool   `json:"isError"`
		Code       string `json:"code"`
		ApprovalID string `json:"approvalID"`
		Error      string `json:"error"`
	}{IsError: true, Code: code, ApprovalID: id, Error: messages[code]})
	return result
}
