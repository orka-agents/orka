/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"

	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/workerenv"
	batchv1 "k8s.io/api/batch/v1"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/tools"
	workerexecutor "github.com/orka-agents/orka/internal/worker"
)

const (
	maxConnectorToolRequestBytes = 1 << 20
	connectorToolResultField     = "result"
	errConnectorToolOnly         = "only connector-backed tools execute in the controller"
)

type ConnectorToolExecutionConfig struct {
	Enabled                          bool
	OutboundAccess                   outboundaccess.Resolver
	KubeClient                       kubernetes.Interface
	HTTPClient                       *http.Client
	TransactionExchange              *workerexecutor.TransactionExchangeConfig
	EnforceTransactionCredentialAuth bool
	TransactionCredentialReadScopes  []string
	ExternalEffects                  store.ExternalEffectStore
	ControllerEpochs                 ControllerEpochFenceSource
}

const connectorToolEffectKind = "connector.tool"

var errConnectorEffectLedgerUnavailable = errors.New("the external effect ledger is unavailable; consequential connector calls are refused")

const connectorToolResultRetention = 256 << 10

const connectorToolDefaultTimeout = 30 * time.Second

type connectorToolOutcome struct {
	Result  string                `json:"result,omitempty"`
	Receipt *connectorToolReceipt `json:"receipt,omitempty"`
}

type connectorToolReceipt struct {
	Digest string `json:"digest"`
	Bytes  int    `json:"bytes"`
}

func connectorToolOutcomeFor(result string) connectorToolOutcome {
	full := connectorToolOutcome{Result: result}
	if encoded, err := json.Marshal(full); err == nil && len(encoded) <= connectorToolResultRetention {
		return full
	}
	sum := sha256.Sum256([]byte(result))
	return connectorToolOutcome{Receipt: &connectorToolReceipt{Digest: hex.EncodeToString(sum[:]), Bytes: len(result)}}
}

type connectorToolCallRequest struct {
	Arguments      json.RawMessage `json:"arguments"`
	CallID         string          `json:"callId"`
	IdempotencyKey string          `json:"idempotencyKey"`
	ApprovalID     string          `json:"approvalId"`
}

func (h *InternalHandlers) ExecuteConnectorTool(c fiber.Ctx) error {
	namespace := strings.TrimSpace(c.Params("namespace"))
	taskName := strings.TrimSpace(c.Params("taskName"))
	toolName := strings.TrimSpace(c.Params("tool"))
	if namespace == "" || taskName == "" || strings.TrimSpace(toolName) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace, taskName, and tool are required")
	}
	cfg := h.connectorTools
	if !cfg.Enabled || cfg.OutboundAccess == nil || cfg.KubeClient == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "connector tool execution is not enabled on this controller")
	}
	authorizer := h.internalCallerAuthorizer()
	task, err := authorizer.verifyTaskCaller(c, namespace, taskName)
	if err != nil {
		return err
	}
	if task.Spec.Type != corev1alpha1.TaskTypeAI {
		return fiber.NewError(fiber.StatusForbidden, "connector tools are available to type: ai tasks only")
	}
	body := c.Body()
	if len(body) > maxConnectorToolRequestBytes {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "request body is too large")
	}
	var req connectorToolCallRequest
	if len(bytes.TrimSpace(body)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return fiber.NewError(fiber.StatusBadRequest, "request body must be a single JSON object")
		}
	}
	if len(req.Arguments) == 0 {
		req.Arguments = json.RawMessage(`{}`)
	}
	ctx := c.Context()
	reader := h.apiReader
	if reader == nil {
		reader = h.k8sClient
	}
	tool, policy, err := loadConnectorBackedTool(ctx, reader, namespace, toolName)
	if err != nil {
		return err
	}
	if err := connectors.ValidateToolArguments(tool.Spec.Parameters, req.Arguments); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tool arguments: "+err.Error())
	}
	if err := connectorToolEnabledForTask(ctx, reader, task, toolName); err != nil {
		return err
	}
	frozen, err := frozenJobToolPolicy(ctx, reader, task)
	if err != nil {
		return err
	}
	if _, dispatched := frozen.tools[toolName]; !dispatched {
		return fiber.NewError(fiber.StatusForbidden, "tool was not in the tool list dispatched with the task's job")
	}
	specDigest, err := controller.ConnectorToolDispatchDigest(tool.Spec, policy.Spec)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to digest tool configuration")
	}
	liveDigest := controller.NativeConnectorToolDispatchDigest(string(tool.UID), string(policy.UID), specDigest)
	if dispatchedDigest, ok := frozen.digests[toolName]; !ok || dispatchedDigest != liveDigest {
		return fiber.NewError(fiber.StatusConflict, "tool configuration changed since dispatch; re-dispatch the task")
	}
	binding, ok := frozen.binding(tool.Spec.HTTP.OutboundAccessPolicyRef.Name)
	if !ok {
		return fiber.NewError(fiber.StatusFailedDependency, "no Connection was frozen for the tool's policy when the task was dispatched")
	}
	if binding.GrantSequence <= 0 {
		return fiber.NewError(fiber.StatusFailedDependency, "the Connection frozen for the tool's policy carries no grant")
	}
	claim, err := h.enforceConnectorToolApproval(ctx, task, tool, policy, req.Arguments, req.ApprovalID, frozen, binding)
	if err != nil {
		if replay, ok := errors.AsType[*connectorToolReplay](err); ok {
			return c.JSON(fiber.Map{connectorToolResultField: replay.result, "replayed": true})
		}
		return err
	}
	return h.runConnectorTool(c, connectorToolRun{
		task: task, tool: tool, policy: policy, req: req, claim: claim, binding: binding, authorizer: authorizer,
	})
}

type connectorToolRun struct {
	task       *corev1alpha1.Task
	tool       *corev1alpha1.Tool
	policy     *corev1alpha1.OutboundAccessPolicy
	req        connectorToolCallRequest
	claim      *connectorApprovalClaim
	binding    corev1alpha1.ConnectionBinding
	authorizer internalCallerAuthorizer
}

func (h *InternalHandlers) runConnectorTool(c fiber.Ctx, run connectorToolRun) error {
	ctx := c.Context()
	cfg := h.connectorTools
	reader := h.apiReader
	if reader == nil {
		reader = h.k8sClient
	}
	toolName := run.tool.Name
	attempted := false
	fail := func(status int, message string) error {
		if run.claim != nil && !attempted {
			if err := h.settleReservedConnectorEffect(ctx, run, store.ExternalEffectFailed); err != nil {
				if errors.Is(err, controller.ErrExternalEffectNotPending) {
					return fiber.NewError(fiber.StatusConflict, "the approved action is being handled by another request; retry to learn its outcome")
				}
				log.Error(err, "reserved connector effect could not be settled", "task", run.task.Name, "tool", toolName)
				return fiber.NewError(fiber.StatusServiceUnavailable, message+"; the effect record could not be settled, retry")
			}
			if err := h.releaseConnectorApprovalClaim(ctx, run.task, toolName, run.claim); err != nil {
				log.Error(err, "connector approval claim could not be released", "task", run.task.Name, "approval", run.claim.approvalID)
				return fiber.NewError(fiber.StatusInternalServerError, message+"; the approval claim could not be released, request approval again")
			}
		}
		return fiber.NewError(status, message)
	}
	executor := workerexecutor.NewToolExecutorForNamespace(run.task.Namespace, cfg.KubeClient, cfg.HTTPClient, cfg.OutboundAccess)
	executor.SetTransactionExchangeConfig(cfg.TransactionExchange)
	if err := controller.BindNativeTaskConnectorAuthority(ctx, reader, run.task, cfg.TransactionCredentialReadScopes, cfg.EnforceTransactionCredentialAuth, executor); err != nil {
		log.Error(err, "connector tool authority binding failed", "task", run.task.Name, "tool", toolName)
		return fail(fiber.StatusInternalServerError, "failed to bind task authority")
	}
	if run.policy == nil {
		return fail(fiber.StatusInternalServerError, "connector tool run carries no checked policy")
	}
	executor.SetCheckedPolicy(run.policy.Name, outboundaccess.PolicyIdentity{UID: string(run.policy.UID), Generation: run.policy.Generation})
	executor.SetSendGate(connectorSendGate(c, reader, run))
	execCtx := ctx
	if strings.TrimSpace(run.req.CallID) != "" {
		execCtx = workerexecutor.WithToolCallID(execCtx, run.req.CallID)
	}
	switch {
	case run.claim != nil:
		execCtx = workerexecutor.WithToolIdempotencyKey(execCtx, run.claim.key)
	case strings.TrimSpace(run.req.IdempotencyKey) != "":
		execCtx = workerexecutor.WithToolIdempotencyKey(execCtx, run.req.IdempotencyKey)
	}
	if _, err := run.authorizer.verifyTaskCaller(c, run.task.Namespace, run.task.Name); err != nil {
		status, message := fiber.StatusForbidden, "task caller is no longer authorized"
		if fiberErr, ok := errors.AsType[*fiber.Error](err); ok {
			status, message = fiberErr.Code, fiberErr.Message
		}
		return fail(status, message)
	}
	call := func(ctx context.Context) (string, error) {
		result, err := executor.Execute(ctx, run.tool, run.req.Arguments)
		var executionErr workerexecutor.ToolExecutionError
		if err == nil || errors.As(err, &executionErr) || workerexecutor.ToolRequestWasAttempted(err) {
			attempted = true
		}
		return result, err
	}
	var (
		result   string
		replayed bool
		err      error
	)
	if run.claim != nil {
		result, replayed, err = h.runConnectorToolEffect(execCtx, run, call, &attempted)
	} else {
		callCtx, cancel := context.WithTimeout(execCtx, connectorToolTimeout(run.tool))
		result, err = call(callCtx)
		cancel()
	}
	if err != nil {
		if errors.Is(err, errConnectorEffectLedgerUnavailable) {
			return fail(fiber.StatusServiceUnavailable, err.Error())
		}
		if !attempted {
			if refusal, handled := unattemptedConnectorCallRefusal(run, err, fail); handled {
				return refusal
			}
		}
		status := fiber.StatusBadGateway
		if !attempted {
			status = fiber.StatusFailedDependency
			if releaseErr := fail(status, ""); releaseErr != nil {
				var fiberErr *fiber.Error
				if errors.As(releaseErr, &fiberErr) && fiberErr.Code != status {
					return fiberErr
				}
			}
		}
		return fiber.NewError(status, fmt.Sprintf("connector tool %q: %v", toolName, err))
	}
	response := fiber.Map{connectorToolResultField: result}
	if replayed {
		response["replayed"] = true
	}
	return c.JSON(response)
}

func unattemptedConnectorCallRefusal(run connectorToolRun, err error, fail func(int, string) error) (error, bool) {
	if revoked, ok := errors.AsType[connectorSendRefusedError](err); ok {
		status, message := fiber.StatusForbidden, "task caller is no longer authorized"
		if fiberErr, ok := errors.AsType[*fiber.Error](revoked.err); ok {
			status, message = fiberErr.Code, fiberErr.Message
		}
		return fail(status, message), true
	}
	if run.claim != nil && errors.Is(err, store.ErrConflict) {
		return fiber.NewError(fiber.StatusConflict, "the approved action is being handled by another request; retry to learn its outcome"), true
	}
	return nil, false
}

func (h *InternalHandlers) runConnectorToolEffect(
	ctx context.Context,
	run connectorToolRun,
	call func(context.Context) (string, error),
	attempted *bool,
) (string, bool, error) {
	cfg := h.connectorTools
	if cfg.ExternalEffects == nil || cfg.ControllerEpochs == nil {
		return "", false, errConnectorEffectLedgerUnavailable
	}
	fence, err := cfg.ControllerEpochs.CurrentFence(ctx)
	if err != nil {
		return "", false, fmt.Errorf("%w: %w", errConnectorEffectLedgerUnavailable, err)
	}
	identity := connectorToolEffectIdentity(run.task, run.claim)
	request := connectorToolEffectRequest(run)
	var liveResult string
	recordedCall := func(ctx context.Context) (connectorToolOutcome, error) {
		result, err := call(ctx)
		if err != nil {
			return connectorToolOutcome{}, err
		}
		liveResult = result
		return connectorToolOutcomeFor(result), nil
	}
	outcome, replayed, err := controller.RunExternalEffectWithReplayCallTimeout(ctx, cfg.ExternalEffects, fence, identity, request, connectorToolTimeout(run.tool), recordedCall)
	result := outcome.Result
	if err == nil && !replayed && outcome.Receipt != nil {
		result = liveResult
	}
	if err == nil && replayed && outcome.Receipt != nil {
		*attempted = true
		return "", true, fmt.Errorf("the approved action succeeded but its %d-byte result exceeded the retained size and cannot be replayed", outcome.Receipt.Bytes)
	}
	if err != nil && !errors.Is(err, store.ErrConflict) {
		state := store.ExternalEffectOutcomeUnknown
		if !*attempted {
			state = store.ExternalEffectFailed
		}
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if settleErr := controller.SettleExternalEffect(settleCtx, cfg.ExternalEffects, fence, identity, state); settleErr != nil {
			log.Error(settleErr, "connector tool effect could not be settled", "task", run.task.Name, "tool", run.tool.Name)
			err = errors.Join(err, fmt.Errorf("%w: the effect record could not be settled; retry", errConnectorEffectLedgerUnavailable))
		}
		cancel()
	}
	return result, replayed, err
}

const connectorToolMaxTimeout = 10 * time.Minute

func connectorToolTimeout(tool *corev1alpha1.Tool) time.Duration {
	if tool != nil && tool.Spec.HTTP != nil && tool.Spec.HTTP.Timeout != nil && tool.Spec.HTTP.Timeout.Duration > 0 {
		return min(tool.Spec.HTTP.Timeout.Duration, connectorToolMaxTimeout)
	}
	return connectorToolDefaultTimeout
}

func (h *InternalHandlers) settleReservedConnectorEffect(ctx context.Context, run connectorToolRun, state store.ExternalEffectState) error {
	cfg := h.connectorTools
	if cfg.ExternalEffects == nil || cfg.ControllerEpochs == nil || run.claim == nil {
		return errConnectorEffectLedgerUnavailable
	}
	fence, err := cfg.ControllerEpochs.CurrentFence(ctx)
	if err != nil {
		return err
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return controller.SettlePendingExternalEffect(settleCtx, cfg.ExternalEffects, fence, connectorToolEffectIdentity(run.task, run.claim), state)
}

type connectorSendRefusedError struct{ err error }

func (e connectorSendRefusedError) Error() string {
	return "the call was refused before sending: " + e.err.Error()
}
func (e connectorSendRefusedError) Unwrap() error { return e.err }

func connectorObjectUnchanged(ctx context.Context, reader client.Reader, checked client.Object, kind string) error {
	if reader == nil || checked == nil || reflect.ValueOf(checked).IsNil() {
		return fiber.NewError(fiber.StatusInternalServerError, "the "+kind+" cannot be re-read before sending")
	}
	live, ok := checked.DeepCopyObject().(client.Object)
	if !ok {
		return fiber.NewError(fiber.StatusInternalServerError, "the "+kind+" cannot be re-read before sending")
	}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(checked), live); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusConflict, kind+" was withdrawn since dispatch; re-dispatch the task")
		}
		return fiber.NewError(fiber.StatusServiceUnavailable, "the "+kind+" could not be re-read before sending; retry")
	}
	if live.GetUID() != checked.GetUID() || live.GetGeneration() != checked.GetGeneration() || live.GetDeletionTimestamp() != nil {
		return fiber.NewError(fiber.StatusConflict, kind+" configuration changed since dispatch; re-dispatch the task")
	}
	return nil
}

func connectorSendGate(c fiber.Ctx, reader client.Reader, run connectorToolRun) func(context.Context) error {
	return func(gateCtx context.Context) error {
		if _, err := run.authorizer.verifyTaskCaller(c, run.task.Namespace, run.task.Name); err != nil {
			return connectorSendRefusedError{err: err}
		}
		if err := connectorObjectUnchanged(gateCtx, reader, run.tool, "tool"); err != nil {
			return connectorSendRefusedError{err: err}
		}
		if err := connectorObjectUnchanged(gateCtx, reader, run.policy, "outbound access policy"); err != nil {
			return connectorSendRefusedError{err: err}
		}
		if err := connectorConnectionUnchanged(gateCtx, reader, run.task, run.binding); err != nil {
			return connectorSendRefusedError{err: err}
		}
		return nil
	}
}

func connectorConnectionUnchanged(ctx context.Context, reader client.Reader, task *corev1alpha1.Task, binding corev1alpha1.ConnectionBinding) error {
	name := binding.ConnectionName
	if name == "" && task.Spec.RequestedBy != nil {
		name = connectors.ConnectionName(binding.Provider, task.Spec.RequestedBy.Issuer, task.Spec.RequestedBy.Subject)
	}
	if reader == nil || name == "" {
		return fiber.NewError(fiber.StatusInternalServerError, "the connection cannot be re-read before sending")
	}
	live := &corev1alpha1.Connection{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, live); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusConflict, "connection was disconnected since dispatch")
		}
		return fiber.NewError(fiber.StatusServiceUnavailable, "the connection could not be re-read before sending; retry")
	}
	if string(live.UID) != binding.UID || live.Generation != binding.Generation || !live.DeletionTimestamp.IsZero() {
		return fiber.NewError(fiber.StatusConflict, "connection was disconnected or changed since dispatch; re-dispatch the task")
	}
	if !connectors.ConnectionLinked(live) || live.Status.GrantSequence != binding.GrantSequence {
		return fiber.NewError(fiber.StatusConflict, "connection is no longer linked with the grant frozen at dispatch; re-dispatch the task")
	}
	return nil
}

func connectorToolEffectIdentity(task *corev1alpha1.Task, claim *connectorApprovalClaim) store.ExternalEffectIdentity {
	return store.ExternalEffectIdentity{
		Kind: connectorToolEffectKind, Namespace: task.Namespace,
		AggregateID: string(task.UID), OperationID: claim.key,
	}
}

func connectorToolEffectRequest(run connectorToolRun) map[string]any {
	return map[string]any{
		"tool": run.tool.Name, "approval": run.claim.approvalID,
		"argsDigest": run.claim.argsDigest, "specDigest": run.claim.specDigest,
		"connection": connectorBindingDigest(run.binding),
	}
}

func connectorBindingDigest(binding corev1alpha1.ConnectionBinding) string {
	sum := sha256.Sum256([]byte(binding.PolicyName + "\x00" + binding.UID + "\x00" + strconv.FormatInt(binding.Generation, 10) +
		"\x00" + strconv.FormatInt(binding.GrantSequence, 10)))
	return hex.EncodeToString(sum[:])
}

func loadConnectorBackedTool(ctx context.Context, reader client.Reader, namespace, toolName string) (*corev1alpha1.Tool, *corev1alpha1.OutboundAccessPolicy, error) {
	tool := &corev1alpha1.Tool{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: toolName}, tool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, fiber.NewError(fiber.StatusNotFound, "tool not found")
		}
		return nil, nil, fiber.NewError(fiber.StatusInternalServerError, "failed to read tool")
	}
	if tool.Spec.HTTP == nil || tool.Spec.HTTP.OutboundAccessPolicyRef == nil {
		return nil, nil, fiber.NewError(fiber.StatusForbidden, errConnectorToolOnly)
	}
	policy := &corev1alpha1.OutboundAccessPolicy{}
	policyKey := types.NamespacedName{Namespace: namespace, Name: tool.Spec.HTTP.OutboundAccessPolicyRef.Name}
	if err := reader.Get(ctx, policyKey, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, fiber.NewError(fiber.StatusForbidden, errConnectorToolOnly)
		}
		return nil, nil, fiber.NewError(fiber.StatusInternalServerError, "failed to read outbound access policy")
	}
	if policy.Spec.Connection == nil {
		return nil, nil, fiber.NewError(fiber.StatusForbidden, errConnectorToolOnly)
	}
	return tool, policy, nil
}

func connectorToolEnabledForTask(ctx context.Context, reader client.Reader, task *corev1alpha1.Task, toolName string) error {
	var agent *corev1alpha1.Agent
	if task.Spec.AgentRef != nil && strings.TrimSpace(task.Spec.AgentRef.Name) != "" {
		agent = &corev1alpha1.Agent{}
		agentNamespace := strings.TrimSpace(task.Spec.AgentRef.Namespace)
		if agentNamespace == "" {
			agentNamespace = task.Namespace
		}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: agentNamespace, Name: task.Spec.AgentRef.Name}, agent); err != nil {
			if apierrors.IsNotFound(err) {
				return fiber.NewError(fiber.StatusForbidden, "task agent not found")
			}
			return fiber.NewError(fiber.StatusInternalServerError, "failed to read agent")
		}
	}
	visible, _, err := controller.FilterConnectorToolsForRequester(ctx, reader, tools.DefaultRegistry, task, aitools.Resolve(task, agent))
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to resolve enabled tools")
	}
	if slices.Contains(visible, toolName) {
		return nil
	}
	return fiber.NewError(fiber.StatusForbidden, "tool is not enabled for this task")
}

type connectorApprovalClaim struct {
	approvalID  string
	key         string
	decisionSeq int64
	releases    int
	argsDigest  string
	specDigest  string
}

func (h *InternalHandlers) enforceConnectorToolApproval(
	ctx context.Context,
	task *corev1alpha1.Task,
	tool *corev1alpha1.Tool,
	policy *corev1alpha1.OutboundAccessPolicy,
	arguments json.RawMessage,
	approvalID string,
	frozen frozenJobPolicy,
	binding corev1alpha1.ConnectionBinding,
) (*connectorApprovalClaim, error) {
	_, approvalRequired := frozen.approvalRequired[tool.Name]
	if tool.Spec.BrokeredToolClass == corev1alpha1.AgentRuntimeBrokeredToolClassWrite && !approvalRequired {
		return nil, fiber.NewError(fiber.StatusConflict, "tool is write-class but the approval policy frozen at dispatch does not cover it; re-dispatch the task")
	}
	if !approvalRequired {
		return nil, nil
	}
	approvalID = strings.TrimSpace(approvalID)
	if approvalID == "" {
		return nil, fiber.NewError(fiber.StatusForbidden, "approval is required for this tool")
	}
	claims, ok := h.executionEventStore.(store.DeduplicatingExecutionEventStore)
	if !ok || h.executionEventStore == nil {
		return nil, fiber.NewError(fiber.StatusForbidden, "approval history is unavailable; the call cannot be authorized")
	}
	history, err := approvals.ListEvents(ctx, h.executionEventStore, task.Namespace, task.Name)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to read approval history")
	}
	targetArgs, err := approvals.TargetArguments(arguments, tool)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "invalid tool arguments")
	}
	digest, err := approvals.TargetArgsDigest(targetArgs)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "invalid tool arguments")
	}
	specDigest, err := approvals.ConnectorTargetSpecDigest(tool.Spec, policy.Spec, binding.UID, binding.Generation, binding.GrantSequence)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to digest tool configuration")
	}
	now := time.Now()
	var matched *approvals.Approval
	for _, approval := range approvals.Derive(history, now) {
		if approval.ID == approvalID {
			matched = &approval
			break
		}
	}
	if matched == nil {
		return nil, fiber.NewError(fiber.StatusForbidden, "approval not found for this task")
	}
	switch {
	case matched.Status != approvals.StatusApproved:
		return nil, fiber.NewError(fiber.StatusForbidden, fmt.Sprintf("approval %s is %s", approvalID, matched.Status))
	case matched.TaskUID != "" && matched.TaskUID != string(task.UID):
		return nil, fiber.NewError(fiber.StatusForbidden, "approval belongs to another task")
	case matched.TargetTool != tool.Name:
		return nil, fiber.NewError(fiber.StatusForbidden, "approval is for another tool")
	case matched.TargetArgsDigest != digest:
		return nil, fiber.NewError(fiber.StatusForbidden, "approval does not bind these arguments")
	case matched.TargetSpecDigest == "" || matched.TargetSpecDigest != specDigest:
		return nil, fiber.NewError(fiber.StatusConflict, "tool configuration or the linked account changed since approval; request approval again")
	case matched.ExpiresAt != nil && matched.ExpiresAt.Before(now):
		return nil, fiber.NewError(fiber.StatusForbidden, "approval has expired")
	}
	claim := &connectorApprovalClaim{
		approvalID:  approvalID,
		decisionSeq: matched.DecisionSeq,
		releases:    connectorClaimReleases(history, approvalID, matched.DecisionSeq),
		argsDigest:  digest,
		specDigest:  specDigest,
	}
	claim.key = fmt.Sprintf("connector-approval-claim:%s:%d:%d", approvalID, matched.DecisionSeq, claim.releases)
	run := connectorToolRun{task: task, tool: tool, policy: policy, claim: claim, binding: binding}
	if err := h.reserveConnectorToolEffect(ctx, run); err != nil {
		return nil, err
	}
	if err := claimConnectorApproval(ctx, claims, task, tool.Name, claim); err != nil {
		if errors.Is(err, errConnectorApprovalClaimed) {
			return h.reconcileSpentConnectorClaim(ctx, claims, run)
		}
		return nil, err
	}
	return claim, nil
}

func (h *InternalHandlers) reserveConnectorToolEffect(ctx context.Context, run connectorToolRun) error {
	cfg := h.connectorTools
	if cfg.ExternalEffects == nil || cfg.ControllerEpochs == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, errConnectorEffectLedgerUnavailable.Error())
	}
	fence, err := cfg.ControllerEpochs.CurrentFence(ctx)
	if err != nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, errConnectorEffectLedgerUnavailable.Error())
	}
	identity := connectorToolEffectIdentity(run.task, run.claim)
	requestDigest, err := controller.ExternalEffectRequestDigest(identity, connectorToolEffectRequest(run))
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to digest the effect request")
	}
	if _, err := cfg.ExternalEffects.ReserveExternalEffect(ctx, store.ReserveExternalEffectRequest{
		Identity: identity, RequestDigest: requestDigest, Fence: fence, CreatedAt: time.Now().UTC(),
	}); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return fiber.NewError(fiber.StatusConflict, "approval was already used for a different execution; request approval again")
		}
		return fiber.NewError(fiber.StatusServiceUnavailable, "the external effect ledger refused the reservation; retry")
	}
	return nil
}

func (h *InternalHandlers) reconcileSpentConnectorClaim(ctx context.Context, claims store.DeduplicatingExecutionEventStore, run connectorToolRun) (*connectorApprovalClaim, error) {
	cfg := h.connectorTools
	if cfg.ExternalEffects == nil {
		return nil, fiber.NewError(fiber.StatusConflict, "approval was already used for an execution; request approval again")
	}
	identity := connectorToolEffectIdentity(run.task, run.claim)
	id, err := identity.CanonicalID()
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to name the effect record")
	}
	effect, err := cfg.ExternalEffects.GetExternalEffect(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, fiber.NewError(fiber.StatusConflict, "approval was already used for an execution; request approval again")
	case err != nil:
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to read the effect ledger")
	}
	requestDigest, err := controller.ExternalEffectRequestDigest(identity, connectorToolEffectRequest(run))
	if err != nil || requestDigest != effect.RequestDigest {
		return nil, fiber.NewError(fiber.StatusConflict, "approval was already used for a different execution; request approval again")
	}
	switch effect.State {
	case store.ExternalEffectPending:
		if cfg.ControllerEpochs == nil {
			return nil, fiber.NewError(fiber.StatusConflict, "the approved action is still executing")
		}
		fence, err := cfg.ControllerEpochs.CurrentFence(ctx)
		if err != nil {
			return nil, fiber.NewError(fiber.StatusServiceUnavailable, errConnectorEffectLedgerUnavailable.Error())
		}
		if _, err := cfg.ExternalEffects.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
			ID: effect.ID, Fence: fence, ExpectedVersion: effect.Version, ExpectedState: store.ExternalEffectPending,
			NewState: store.ExternalEffectFailed, RequestDigest: effect.RequestDigest,
			ExpectedLeaseOwner: effect.LeaseOwner, UpdatedAt: time.Now().UTC(),
		}); err != nil {
			return nil, fiber.NewError(fiber.StatusConflict, "the approved action is still executing")
		}
		return h.reclaimConnectorApproval(ctx, claims, run)
	case store.ExternalEffectFailed:
		return h.reclaimConnectorApproval(ctx, claims, run)
	case store.ExternalEffectSucceeded:
		var outcome connectorToolOutcome
		if json.Unmarshal(effect.Response, &outcome) != nil {
			return nil, fiber.NewError(fiber.StatusConflict, "the committed result of this approval cannot be read")
		}
		if outcome.Receipt != nil {
			return nil, fiber.NewError(fiber.StatusConflict, fmt.Sprintf("the approved action succeeded but its %d-byte result exceeded the retained size and cannot be replayed", outcome.Receipt.Bytes))
		}
		return nil, &connectorToolReplay{result: outcome.Result}
	case store.ExternalEffectInFlight:
		if effect.LeaseExpiresAt != nil && effect.LeaseExpiresAt.Before(time.Now()) && cfg.ControllerEpochs != nil {
			fence, err := cfg.ControllerEpochs.CurrentFence(ctx)
			if err != nil {
				return nil, fiber.NewError(fiber.StatusServiceUnavailable, "the interrupted action's outcome could not be recorded; retry")
			}
			if err := controller.SettleExternalEffect(ctx, cfg.ExternalEffects, fence, identity, store.ExternalEffectOutcomeUnknown); err != nil {
				return nil, fiber.NewError(fiber.StatusServiceUnavailable, "the interrupted action's outcome could not be recorded; retry")
			}
			return nil, fiber.NewError(fiber.StatusBadGateway, "the approved action was interrupted and its outcome is unknown; the approval is spent")
		}
		return nil, fiber.NewError(fiber.StatusConflict, "the approved action is still executing")
	default:
		return nil, fiber.NewError(fiber.StatusBadGateway, "the approved action's outcome is unknown; the approval is spent")
	}
}

func (h *InternalHandlers) reclaimConnectorApproval(ctx context.Context, claims store.DeduplicatingExecutionEventStore, run connectorToolRun) (*connectorApprovalClaim, error) {
	if err := h.releaseConnectorApprovalClaim(ctx, run.task, run.tool.Name, run.claim); err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "an unstarted approval claim could not be released; retry")
	}
	next := &connectorApprovalClaim{
		approvalID: run.claim.approvalID, decisionSeq: run.claim.decisionSeq, releases: run.claim.releases + 1,
		argsDigest: run.claim.argsDigest, specDigest: run.claim.specDigest,
	}
	next.key = fmt.Sprintf("connector-approval-claim:%s:%d:%d", next.approvalID, next.decisionSeq, next.releases)
	nextRun := run
	nextRun.claim = next
	if err := h.reserveConnectorToolEffect(ctx, nextRun); err != nil {
		return nil, err
	}
	if err := claimConnectorApproval(ctx, claims, run.task, run.tool.Name, next); err != nil {
		if errors.Is(err, errConnectorApprovalClaimed) {
			return nil, fiber.NewError(fiber.StatusConflict, "approval was already used for an execution; request approval again")
		}
		return nil, err
	}
	return next, nil
}

var errConnectorApprovalClaimed = errors.New("approval was already claimed")

type connectorToolReplay struct{ result string }

func (r *connectorToolReplay) Error() string {
	return "connector tool result replayed from the effect ledger"
}

const connectorClaimReleaseReason = "connector-claim-released"

func connectorClaimReleases(history []store.ExecutionEvent, approvalID string, decisionSeq int64) int {
	releases := 0
	for _, event := range history {
		if event.Type != events.ExecutionEventTypeApprovalExecutionUpdated {
			continue
		}
		var payload struct {
			ApprovalID  string `json:"approvalID"`
			Reason      string `json:"reason"`
			DecisionSeq int64  `json:"decisionSeq"`
		}
		if err := json.Unmarshal(event.Content, &payload); err != nil {
			continue
		}
		if payload.ApprovalID == approvalID && payload.Reason == connectorClaimReleaseReason && payload.DecisionSeq == decisionSeq {
			releases++
		}
	}
	return releases
}

func (h *InternalHandlers) releaseConnectorApprovalClaim(ctx context.Context, task *corev1alpha1.Task, toolName string, claim *connectorApprovalClaim) error {
	claims, ok := h.executionEventStore.(store.DeduplicatingExecutionEventStore)
	if !ok {
		return errors.New("approval history is unavailable")
	}
	content, err := json.Marshal(struct {
		ApprovalID       string `json:"approvalID"`
		TaskUID          string `json:"taskUID"`
		TargetTool       string `json:"targetTool"`
		ExecutionOutcome string `json:"executionOutcome"`
		Reason           string `json:"reason"`
		DecisionSeq      int64  `json:"decisionSeq"`
	}{claim.approvalID, string(task.UID), toolName, "not_started", connectorClaimReleaseReason, claim.decisionSeq})
	if err != nil {
		return err
	}
	_, _, err = claims.AppendExecutionEventIfAbsent(ctx, &store.ExecutionEvent{
		Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: task.Name, TaskName: task.Name,
		AgentName: taskAgentName(task), Type: events.ExecutionEventTypeApprovalExecutionUpdated, Severity: events.ExecutionEventSeverityInfo,
		ToolName: toolName, ToolCallID: claim.approvalID, Summary: "Connector tool execution released approval " + claim.approvalID, Content: content,
	}, fmt.Sprintf("connector-approval-release:%s:%d:%d", claim.approvalID, claim.decisionSeq, claim.releases))
	return err
}

func claimConnectorApproval(ctx context.Context, claims store.DeduplicatingExecutionEventStore, task *corev1alpha1.Task, toolName string, claim *connectorApprovalClaim) error {
	approvalID := claim.approvalID
	content, err := json.Marshal(struct {
		ApprovalID       string `json:"approvalID"`
		TaskUID          string `json:"taskUID"`
		TargetTool       string `json:"targetTool"`
		ExecutionOutcome string `json:"executionOutcome"`
		Reason           string `json:"reason"`
	}{approvalID, string(task.UID), toolName, "running", "claimed by the controller connector tool endpoint"})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to record approval claim")
	}
	_, appended, err := claims.AppendExecutionEventIfAbsent(ctx, &store.ExecutionEvent{
		Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: task.Name, TaskName: task.Name,
		AgentName: taskAgentName(task),
		Type:      events.ExecutionEventTypeApprovalExecutionUpdated, Severity: events.ExecutionEventSeverityInfo,
		ToolName: toolName, ToolCallID: approvalID, Summary: "Connector tool execution claimed approval " + approvalID, Content: content,
	}, claim.key)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to record approval claim")
	}
	if !appended {
		return errConnectorApprovalClaimed
	}
	return nil
}

func taskAgentName(task *corev1alpha1.Task) string {
	if task.Spec.AgentRef != nil {
		return task.Spec.AgentRef.Name
	}
	return ""
}

type frozenJobPolicy struct {
	tools            map[string]struct{}
	approvalRequired map[string]struct{}
	digests          map[string]string
	bindings         []corev1alpha1.ConnectionBinding
}

func (p frozenJobPolicy) binding(policyName string) (corev1alpha1.ConnectionBinding, bool) {
	for _, binding := range p.bindings {
		if binding.PolicyName == policyName && strings.TrimSpace(binding.UID) != "" {
			return binding, true
		}
	}
	return corev1alpha1.ConnectionBinding{}, false
}

func frozenJobToolPolicy(ctx context.Context, reader client.Reader, task *corev1alpha1.Task) (frozenJobPolicy, error) {
	jobName := strings.TrimSpace(task.Status.JobName)
	if jobName == "" {
		return frozenJobPolicy{}, fiber.NewError(fiber.StatusConflict, "task has no dispatched job")
	}
	job := &batchv1.Job{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: jobName}, job); err != nil {
		return frozenJobPolicy{}, fiber.NewError(fiber.StatusFailedDependency, "the task's dispatched job is unavailable")
	}
	if task.Status.JobUID != "" && string(job.UID) != task.Status.JobUID {
		return frozenJobPolicy{}, fiber.NewError(fiber.StatusConflict, "the task's job was replaced")
	}
	policy := frozenJobPolicy{tools: map[string]struct{}{}, approvalRequired: map[string]struct{}{}, digests: map[string]string{}}
	for _, container := range job.Spec.Template.Spec.Containers {
		if container.Name != workerContainerName {
			continue
		}
		for _, env := range container.Env {
			var target map[string]struct{}
			switch env.Name {
			case workerenv.ApprovalRequiredTools:
				target = policy.approvalRequired
			case workerenv.AITools:
				target = policy.tools
			case workerenv.ConnectorToolDigests:
				if strings.TrimSpace(env.Value) != "" && json.Unmarshal([]byte(env.Value), &policy.digests) != nil {
					return frozenJobPolicy{}, fiber.NewError(fiber.StatusConflict, "the task's job carries unreadable connector tool digests")
				}
				continue
			default:
				continue
			}
			for _, name := range workerenv.SplitCSV(env.Value) {
				if trimmed := strings.TrimSpace(name); trimmed != "" {
					target[trimmed] = struct{}{}
				}
			}
		}
	}
	bindings, _, err := controller.FrozenConnectionBindingsFromJob(job)
	if err != nil {
		return frozenJobPolicy{}, fiber.NewError(fiber.StatusConflict, "the task's job carries unreadable connection bindings")
	}
	if !controller.ConnectionBindingsEqual(bindings, task.Status.ConnectionBindings) {
		return frozenJobPolicy{}, fiber.NewError(fiber.StatusConflict, "the task's connection bindings do not match its dispatched job")
	}
	policy.bindings = bindings
	return policy, nil
}

const workerContainerName = "worker"
