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
	"net/http"
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
	workerexecutor "github.com/orka-agents/orka/internal/worker"
)

const (
	maxConnectorToolRequestBytes = 1 << 20
	connectorToolResultField     = "result"
	errConnectorToolOnly         = "only connector-backed tools execute in the controller"
)

// ConnectorToolExecutionConfig lets native type: ai workers run
// connector-backed tools in the controller, where the person's token lives.
type ConnectorToolExecutionConfig struct {
	Enabled             bool
	OutboundAccess      outboundaccess.Resolver
	KubeClient          kubernetes.Interface
	HTTPClient          *http.Client
	TransactionExchange *workerexecutor.TransactionExchangeConfig
	// EnforceTransactionCredentialAuth and TransactionCredentialReadScopes
	// mirror the broker's Secret-credential authorization settings.
	EnforceTransactionCredentialAuth bool
	TransactionCredentialReadScopes  []string
	// ExternalEffects and ControllerEpochs record every approval-claimed
	// (consequential) connector call in the durable effect ledger. Without
	// both, such calls are refused.
	ExternalEffects  store.ExternalEffectStore
	ControllerEpochs ControllerEpochFenceSource
}

// connectorToolEffectKind is the effect-ledger kind for consequential
// connector-backed calls made on behalf of native type: ai Tasks.
const connectorToolEffectKind = "connector.tool"

var errConnectorEffectLedgerUnavailable = errors.New("the external effect ledger is unavailable; consequential connector calls are refused")

type connectorToolCallRequest struct {
	Arguments      json.RawMessage `json:"arguments"`
	CallID         string          `json:"callId"`
	IdempotencyKey string          `json:"idempotencyKey"`
	// ApprovalID names the approval that authorizes this call when the tool
	// is in the approval-required set frozen with the Task's Job.
	ApprovalID string `json:"approvalId"`
}

// ExecuteConnectorTool runs one connector-backed custom Tool for the calling
// worker's Task. The caller must be the Task's current worker Pod (verified
// through its ServiceAccount token), the Tool must sit behind a
// connection-mode OutboundAccessPolicy and be enabled for the Task after
// readOnly filtering, and the credential is resolved from the Connection
// frozen into Task status. The response carries the tool result only; the
// executor redacts credential material from errors.
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
	// The worker is not trusted to have honored the Tool's schema; the
	// person's credential is attached only to arguments the schema admits.
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
	// The Job's immutable tool list is the upper bound: a Task or Agent
	// edited after dispatch cannot add a capability the worker was not
	// started with.
	if _, dispatched := frozen.tools[toolName]; !dispatched {
		return fiber.NewError(fiber.StatusForbidden, "tool was not in the tool list dispatched with the task's job")
	}
	// Only the definition the worker was dispatched with executes: a Tool
	// retargeted after dispatch (URL, method, headers, schema) or a policy
	// whose credential output changed is refused whether or not the call
	// needs an approval.
	liveDigest, err := controller.ConnectorToolDispatchDigest(tool.Spec, policy.Spec)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to digest tool configuration")
	}
	if dispatchedDigest, ok := frozen.digests[toolName]; !ok || dispatchedDigest != liveDigest {
		return fiber.NewError(fiber.StatusConflict, "tool configuration changed since dispatch; re-dispatch the task")
	}
	binding, ok := frozen.binding(tool.Spec.HTTP.OutboundAccessPolicyRef.Name)
	if !ok {
		return fiber.NewError(fiber.StatusFailedDependency, "no Connection was frozen for the tool's policy when the task was dispatched")
	}
	claim, err := h.enforceConnectorToolApproval(ctx, task, tool, req.Arguments, req.ApprovalID, frozen, binding)
	if err != nil {
		if replay, ok := errors.AsType[*connectorToolReplay](err); ok {
			return c.JSON(fiber.Map{connectorToolResultField: replay.result, "replayed": true})
		}
		return err
	}
	return h.runConnectorTool(c, connectorToolRun{
		task: task, tool: tool, req: req, claim: claim, binding: binding, authorizer: authorizer,
	})
}

// connectorToolRun is one authorized call about to be executed.
type connectorToolRun struct {
	task       *corev1alpha1.Task
	tool       *corev1alpha1.Tool
	req        connectorToolCallRequest
	claim      *connectorApprovalClaim
	binding    corev1alpha1.ConnectionBinding
	authorizer internalCallerAuthorizer
}

// runConnectorTool binds the Task's authority and executes the call. Every
// failure before a provider request hands a claimed approval back, so an
// exact retry or a fresh human decision can claim it again; a request that
// reached the provider keeps its claim spent.
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
	execCtx := ctx
	if strings.TrimSpace(run.req.CallID) != "" {
		execCtx = workerexecutor.WithToolCallID(execCtx, run.req.CallID)
	}
	switch {
	case run.claim != nil:
		// The downstream idempotency key is the claimed approval, not a
		// worker-chosen value, so the provider sees one request per approval.
		execCtx = workerexecutor.WithToolIdempotencyKey(execCtx, run.claim.key)
	case strings.TrimSpace(run.req.IdempotencyKey) != "":
		execCtx = workerexecutor.WithToolIdempotencyKey(execCtx, run.req.IdempotencyKey)
	}
	// The caller's authority is judged again right before the external
	// effect: a Task cancelled or a Job replaced while this request did its
	// lookups must not still reach the provider.
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
		result, err = call(execCtx)
	}
	if err != nil {
		if errors.Is(err, errConnectorEffectLedgerUnavailable) {
			return fail(fiber.StatusServiceUnavailable, err.Error())
		}
		status := fiber.StatusBadGateway
		if !attempted {
			// Resolution failed before any request was sent (no connection,
			// revoked, changed since dispatch): report it as a client-visible
			// precondition failure and hand the approval back.
			status = fiber.StatusFailedDependency
			if releaseErr := fail(status, ""); releaseErr != nil {
				var fiberErr *fiber.Error
				if errors.As(releaseErr, &fiberErr) && fiberErr.Code == fiber.StatusInternalServerError {
					return fiberErr
				}
			}
		}
		return c.Status(status).JSON(fiber.Map{"error": fmt.Sprintf("connector tool %q: %v", toolName, err)})
	}
	response := fiber.Map{connectorToolResultField: result}
	if replayed {
		response["replayed"] = true
	}
	return c.JSON(response)
}

// runConnectorToolEffect records an approval-claimed call in the durable
// effect ledger under the claim's key, with the frozen Connection digest and
// the approval's argument and configuration digests, so the outcome of a
// consequential call survives a controller restart and can be audited. A
// call that fails before any request settles as Failed; one whose outcome
// the provider may have applied settles as OutcomeUnknown.
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
	result, replayed, err := controller.RunExternalEffectWithReplay(ctx, cfg.ExternalEffects, fence, identity, request, call)
	if err != nil && !errors.Is(err, store.ErrConflict) {
		state := store.ExternalEffectOutcomeUnknown
		if !*attempted {
			state = store.ExternalEffectFailed
		}
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if settleErr := controller.SettleExternalEffect(settleCtx, cfg.ExternalEffects, fence, identity, state); settleErr != nil {
			log.Error(settleErr, "connector tool effect could not be settled", "task", run.task.Name, "tool", run.tool.Name)
		}
		cancel()
	}
	return result, replayed, err
}

func connectorToolEffectIdentity(task *corev1alpha1.Task, claim *connectorApprovalClaim) store.ExternalEffectIdentity {
	return store.ExternalEffectIdentity{
		Kind: connectorToolEffectKind, Namespace: task.Namespace,
		AggregateID: string(task.UID), OperationID: claim.key,
	}
}

// connectorToolEffectRequest is the non-secret request record of one
// consequential connector call: what was approved and which person's link
// (by frozen identity digest) it acted under.
func connectorToolEffectRequest(run connectorToolRun) map[string]any {
	return map[string]any{
		"tool": run.tool.Name, "approval": run.claim.approvalID,
		"argsDigest": run.claim.argsDigest, "specDigest": run.claim.specDigest,
		"connection": connectorBindingDigest(run.binding),
	}
}

// connectorBindingDigest names a frozen Connection without exposing more
// than its policy, UID, and generation.
func connectorBindingDigest(binding corev1alpha1.ConnectionBinding) string {
	sum := sha256.Sum256([]byte(binding.PolicyName + "\x00" + binding.UID + "\x00" + strconv.FormatInt(binding.Generation, 10)))
	return hex.EncodeToString(sum[:])
}

// replayConnectorToolEffect returns the committed result of a claim that was
// already spent, when the ledger holds a Succeeded record for exactly this
// call. A worker that lost the response to a crash then receives the result
// it was owed instead of a replay refusal, and no second request is made.
func (h *InternalHandlers) replayConnectorToolEffect(ctx context.Context, run connectorToolRun) (string, bool) {
	cfg := h.connectorTools
	if cfg.ExternalEffects == nil || run.claim == nil {
		return "", false
	}
	identity := connectorToolEffectIdentity(run.task, run.claim)
	id, err := identity.CanonicalID()
	if err != nil {
		return "", false
	}
	effect, err := cfg.ExternalEffects.GetExternalEffect(ctx, id)
	if err != nil || effect == nil || effect.State != store.ExternalEffectSucceeded {
		return "", false
	}
	requestDigest, err := controller.ExternalEffectRequestDigest(identity, connectorToolEffectRequest(run))
	if err != nil || requestDigest != effect.RequestDigest {
		return "", false
	}
	var result string
	if json.Unmarshal(effect.Response, &result) != nil {
		return "", false
	}
	return result, true
}

// loadConnectorBackedTool returns the Tool only when it sits behind a
// connection-mode policy; every other Tool runs in the worker Pod.
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

// connectorToolEnabledForTask re-derives the Task's enabled tools, including
// the readOnly rule, so a worker cannot reach a tool the controller hid.
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
	visible, _, err := controller.FilterConnectorToolsForRequester(ctx, reader, task, aitools.Resolve(task, agent))
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to resolve enabled tools")
	}
	if slices.Contains(visible, toolName) {
		return nil
	}
	return fiber.NewError(fiber.StatusForbidden, "tool is not enabled for this task")
}

// enforceConnectorToolApproval is the controller-side approval boundary.
// The worker's own gate is advisory once the worker is compromised, so the
// controller re-checks against the approval policy frozen into the Job at
// dispatch and the Task's approval history: a write-class tool that the
// frozen policy does not cover (its class changed after dispatch) is
// refused, and a tool the policy covers executes only with an approved
// approval that binds this tool and these exact arguments. The approval is
// then claimed atomically in the Task's event stream before any provider
// request, so one approval authorizes exactly one execution however many
// times a worker presents it. It returns the claimed approval ID, or "" when
// the tool needs no approval.
// connectorApprovalClaim is one claimed execution of an approval. The key
// carries the decision sequence and the number of earlier claims handed back
// before any provider request, so a fresh human decision or an exact retry
// after a pre-request failure can claim again while a spent claim cannot.
type connectorApprovalClaim struct {
	approvalID string
	key        string
	releases   int
	// argsDigest and specDigest are what the approval bound, recorded with
	// the effect ledger entry.
	argsDigest string
	specDigest string
}

func (h *InternalHandlers) enforceConnectorToolApproval(
	ctx context.Context,
	task *corev1alpha1.Task,
	tool *corev1alpha1.Tool,
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
	// The approval also binds the tool's execution configuration (URL,
	// method, headers, schema) and the Connection frozen for its policy, as
	// digested by the worker when it asked; the live Tool and the Job's
	// binding must still match, or a retargeted Tool or a re-linked account
	// would ride an old approval to a different write.
	specDigest, err := approvals.ConnectorTargetSpecDigest(tool.Spec, binding.UID, binding.Generation)
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
		approvalID: approvalID,
		releases:   connectorClaimReleases(history, approvalID),
		argsDigest: digest,
		specDigest: specDigest,
	}
	claim.key = fmt.Sprintf("connector-approval-claim:%s:%d:%d", approvalID, matched.DecisionSeq, claim.releases)
	if err := claimConnectorApproval(ctx, claims, task, tool.Name, claim); err != nil {
		if errors.Is(err, errConnectorApprovalClaimed) {
			if result, ok := h.replayConnectorToolEffect(ctx, connectorToolRun{task: task, tool: tool, claim: claim, binding: binding}); ok {
				return nil, &connectorToolReplay{result: result}
			}
			return nil, fiber.NewError(fiber.StatusConflict, "approval was already used for an execution; request approval again")
		}
		return nil, err
	}
	return claim, nil
}

// errConnectorApprovalClaimed reports a claim key that already exists.
var errConnectorApprovalClaimed = errors.New("approval was already claimed")

// connectorToolReplay carries a committed result for a spent claim back to
// the handler as an error value, so the gate's signature stays one-shaped.
type connectorToolReplay struct{ result string }

func (r *connectorToolReplay) Error() string {
	return "connector tool result replayed from the effect ledger"
}

// connectorClaimReleaseReason marks an execution-update event that hands a
// claim back because no provider request was made.
const connectorClaimReleaseReason = "connector-claim-released"

// connectorClaimReleases counts the claims of approvalID handed back before
// any provider request.
func connectorClaimReleases(history []store.ExecutionEvent, approvalID string) int {
	releases := 0
	for _, event := range history {
		if event.Type != events.ExecutionEventTypeApprovalExecutionUpdated {
			continue
		}
		var payload struct {
			ApprovalID string `json:"approvalID"`
			Reason     string `json:"reason"`
		}
		if err := json.Unmarshal(event.Content, &payload); err != nil {
			continue
		}
		if payload.ApprovalID == approvalID && payload.Reason == connectorClaimReleaseReason {
			releases++
		}
	}
	return releases
}

// releaseConnectorApprovalClaim records that a claimed execution never
// reached the provider, so the approval may be claimed once more.
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
	}{claim.approvalID, string(task.UID), toolName, "not_started", connectorClaimReleaseReason})
	if err != nil {
		return err
	}
	_, _, err = claims.AppendExecutionEventIfAbsent(ctx, &store.ExecutionEvent{
		Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: task.Name, TaskName: task.Name,
		AgentName: taskAgentName(task), Type: events.ExecutionEventTypeApprovalExecutionUpdated, Severity: events.ExecutionEventSeverityInfo,
		ToolName: toolName, ToolCallID: claim.approvalID, Summary: "Connector tool execution released approval " + claim.approvalID, Content: content,
	}, fmt.Sprintf("connector-approval-release:%s:%d", claim.approvalID, claim.releases))
	return err
}

// claimConnectorApproval records the single execution an approval authorizes,
// atomically on a per-approval dedupe key, before any provider request. A
// second claim for the same approval is a replay and is refused; a claim whose
// execution then fails before any request is spent as well, and the person
// must approve again, which is the safe side of the trade.
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

// frozenJobPolicy is the tool policy the Job builder froze into the worker
// environment at dispatch. The Job spec is immutable, so neither a Tool
// that changes class nor a Task or Agent edited afterwards can widen it.
type frozenJobPolicy struct {
	tools            map[string]struct{}
	approvalRequired map[string]struct{}
	// digests pins each connector-backed tool to the spec it was dispatched with.
	digests map[string]string
	// bindings are the Connections frozen for this Job.
	bindings []corev1alpha1.ConnectionBinding
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
	// The Job's bindings are authoritative; Task status must agree with
	// them, or a status rewritten after dispatch could redirect the call.
	if !controller.ConnectionBindingsEqual(bindings, task.Status.ConnectionBindings) {
		return frozenJobPolicy{}, fiber.NewError(fiber.StatusConflict, "the task's connection bindings do not match its dispatched job")
	}
	policy.bindings = bindings
	return policy, nil
}

const workerContainerName = "worker"
