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
	"github.com/orka-agents/orka/internal/tools"
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

// connectorToolResultRetention bounds the result retained in an effect
// record for replay: the record is one API object, so a larger result is
// replaced by a receipt (digest and size) and cannot be replayed.
const connectorToolResultRetention = 256 << 10

// connectorToolDefaultTimeout is the call deadline when the Tool declares
// none, matching the executor's default request timeout.
const connectorToolDefaultTimeout = 30 * time.Second

// connectorToolOutcome is what the effect ledger retains for one
// approval-claimed call.
type connectorToolOutcome struct {
	Result  string                `json:"result,omitempty"`
	Receipt *connectorToolReceipt `json:"receipt,omitempty"`
}

// connectorToolReceipt stands in for a result too large to retain.
type connectorToolReceipt struct {
	Digest string `json:"digest"`
	Bytes  int    `json:"bytes"`
}

func connectorToolOutcomeFor(result string) connectorToolOutcome {
	// The bound applies to the marshaled record, not the raw string: JSON
	// escaping of control characters can multiply the size several times,
	// and the record must fit one API object.
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
	// retargeted after dispatch (URL, method, headers, schema), a policy
	// whose credential output changed, or either object deleted and
	// recreated (even with the same spec) is refused whether or not the
	// call needs an approval.
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

// connectorToolRun is one authorized call about to be executed.
type connectorToolRun struct {
	task       *corev1alpha1.Task
	tool       *corev1alpha1.Tool
	policy     *corev1alpha1.OutboundAccessPolicy
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
			// The reserved effect settles as Failed first so the ledger never
			// keeps an orphaned Pending record; the claim is handed back only
			// once that terminal transition is durable, else the retry is
			// asked to try again with the claim still held.
			if err := h.settleReservedConnectorEffect(ctx, run, store.ExternalEffectFailed); err != nil {
				if errors.Is(err, controller.ErrExternalEffectNotPending) {
					// Another execution holds the record; it settles it, and
					// the claim stays spent until then.
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
	// The policy whose dispatch digest was checked above is the only one
	// resolution may inject under; a policy replaced or edited in between
	// is refused rather than executed under a configuration never checked.
	executor.SetCheckedPolicy(run.policy.Name, outboundaccess.PolicyIdentity{UID: string(run.policy.UID), Generation: run.policy.Generation})
	// Policy reads and credential resolution (possibly a token refresh) run
	// inside Execute; the caller's authority is judged once more right
	// before the request leaves, so a Task cancelled or a Job replaced in
	// that window never reaches the provider.
	executor.SetSendGate(func(context.Context) error {
		if _, err := run.authorizer.verifyTaskCaller(c, run.task.Namespace, run.task.Name); err != nil {
			return connectorCallerRevokedError{err: err}
		}
		return nil
	})
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
		if !attempted {
			if refusal, handled := unattemptedConnectorCallRefusal(run, err, fail); handled {
				return refusal
			}
		}
		status := fiber.StatusBadGateway
		if !attempted {
			// Resolution failed before any request was sent (no connection,
			// revoked, changed since dispatch): report it as a client-visible
			// precondition failure and hand the approval back.
			status = fiber.StatusFailedDependency
			if releaseErr := fail(status, ""); releaseErr != nil {
				// Anything other than the plain 424 the helper hands back
				// (a settlement or release that could not be made durable)
				// is what the worker must see, so it retries appropriately.
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

// unattemptedConnectorCallRefusal maps a call that sent nothing to its
// response when the reason is not an ordinary precondition failure: a caller
// that lost its authority at the send gate is refused (and the claim handed
// back through fail), and a claim whose effect record another request moved
// is reported without settling or releasing anything, since that request
// owns the record.
func unattemptedConnectorCallRefusal(run connectorToolRun, err error, fail func(int, string) error) (error, bool) {
	if revoked, ok := errors.AsType[connectorCallerRevokedError](err); ok {
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
	// The ledger retains a bounded outcome: a result past the retention
	// bound is recorded as a receipt so the record stays one API object.
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
		err = fmt.Errorf("the approved action succeeded but its %d-byte result exceeded the retained size and cannot be replayed", outcome.Receipt.Bytes)
	}
	if err != nil && !errors.Is(err, store.ErrConflict) {
		state := store.ExternalEffectOutcomeUnknown
		if !*attempted {
			state = store.ExternalEffectFailed
		}
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if settleErr := controller.SettleExternalEffect(settleCtx, cfg.ExternalEffects, fence, identity, state); settleErr != nil {
			log.Error(settleErr, "connector tool effect could not be settled", "task", run.task.Name, "tool", run.tool.Name)
			// The ledger still shows the call in flight; the worker must
			// retry (503) rather than treat the failure as a settled 502 and
			// spend its approval on a record nothing will reconcile.
			err = errors.Join(err, fmt.Errorf("%w: the effect record could not be settled; retry", errConnectorEffectLedgerUnavailable))
		}
		cancel()
	}
	return result, replayed, err
}

// connectorToolMaxTimeout is the longest a connector-backed call and its
// effect lease may run: the same bound the ConnectorProvider declaration
// enforces on curated tools.
const connectorToolMaxTimeout = 10 * time.Minute

// connectorToolTimeout is the Tool's declared request timeout, the same
// deadline the executor applies, so the effect lease covers the whole call;
// a Tool CR carries no upper bound of its own, so it is clamped here.
func connectorToolTimeout(tool *corev1alpha1.Tool) time.Duration {
	if tool != nil && tool.Spec.HTTP != nil && tool.Spec.HTTP.Timeout != nil && tool.Spec.HTTP.Timeout.Duration > 0 {
		return min(tool.Spec.HTTP.Timeout.Duration, connectorToolMaxTimeout)
	}
	return connectorToolDefaultTimeout
}

// settleReservedConnectorEffect moves the claim's reserved effect record to a
// terminal state when the call never started. Only a record still exactly
// Pending is moved; one another execution holds in flight, or settled, is
// reported with controller.ErrExternalEffectNotPending and left alone.
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

// connectorCallerRevokedError is the send gate's refusal: the caller lost
// its authority before the request left, so nothing was attempted.
type connectorCallerRevokedError struct{ err error }

func (e connectorCallerRevokedError) Error() string {
	return "task caller is no longer authorized: " + e.err.Error()
}
func (e connectorCallerRevokedError) Unwrap() error { return e.err }

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
// than its policy, UID, generation, and grant sequence. The grant sequence
// separates consents to the same Connection object, matching the digest the
// ACP broker records, so audit can tell which consent authorized a call.
func connectorBindingDigest(binding corev1alpha1.ConnectionBinding) string {
	sum := sha256.Sum256([]byte(binding.PolicyName + "\x00" + binding.UID + "\x00" + strconv.FormatInt(binding.Generation, 10) +
		"\x00" + strconv.FormatInt(binding.GrantSequence, 10)))
	return hex.EncodeToString(sum[:])
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
	visible, _, err := controller.FilterConnectorToolsForRequester(ctx, reader, tools.DefaultRegistry, task, aitools.Resolve(task, agent))
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
	// decisionSeq is the human decision this claim executes; releases are
	// counted per decision so a late release from an older decision can
	// never reopen a newer, already executed one.
	decisionSeq int64
	releases    int
	// argsDigest and specDigest are what the approval bound, recorded with
	// the effect ledger entry.
	argsDigest string
	specDigest string
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
	// The approval also binds the tool's execution configuration (URL,
	// method, headers, schema) and the Connection frozen for its policy, as
	// digested by the worker when it asked; the live Tool and the Job's
	// binding must still match, or a retargeted Tool or a re-linked account
	// would ride an old approval to a different write.
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
	// The effect record is reserved before the claim is recorded, so every
	// claim has a record whose state says whether its call started; a
	// claim with no record is never assumed unstarted.
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

// reserveConnectorToolEffect creates (or finds) the Pending effect record
// for the claim about to be taken. Reservation is idempotent for the same
// claim and request digest.
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

// reconcileSpentConnectorClaim decides what a claim that already exists
// means from the effect ledger, so a controller that stopped between the
// claim and the call, or during the call, never leaves the approval stuck:
//
//   - a Succeeded record for exactly this call replays its result;
//   - no record at all means the claim never reached the ledger, so it is
//     handed back and claimed again (nothing was executed);
//   - an InFlight record whose lease expired means the call may have
//     reached the provider; it settles OutcomeUnknown;
//   - a terminal Failed or OutcomeUnknown record, or a live InFlight lease,
//     is reported as such rather than as a replay refusal.
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
		// Every claim is preceded by its reservation; a claim without a
		// record cannot be shown to be unstarted and stays refused.
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
		// Reserved and claimed but never started, or a request that is still
		// between its claim and its call. Moving the record to Failed is the
		// fence: it succeeds only while nothing has started, and afterwards
		// the original request's own transition to InFlight fails, so it
		// cannot reach the provider once the claim is handed back.
		if cfg.ControllerEpochs == nil {
			return nil, fiber.NewError(fiber.StatusConflict, "the approved action is still executing")
		}
		fence, err := cfg.ControllerEpochs.CurrentFence(ctx)
		if err != nil {
			return nil, fiber.NewError(fiber.StatusServiceUnavailable, errConnectorEffectLedgerUnavailable.Error())
		}
		// The CAS is pinned to the Pending state and version observed above:
		// a request that moved the record to InFlight meanwhile makes it
		// fail, and the claim is then reported as still executing.
		if _, err := cfg.ExternalEffects.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
			ID: effect.ID, Fence: fence, ExpectedVersion: effect.Version, ExpectedState: store.ExternalEffectPending,
			NewState: store.ExternalEffectFailed, RequestDigest: effect.RequestDigest,
			ExpectedLeaseOwner: effect.LeaseOwner, UpdatedAt: time.Now().UTC(),
		}); err != nil {
			return nil, fiber.NewError(fiber.StatusConflict, "the approved action is still executing")
		}
		return h.reclaimConnectorApproval(ctx, claims, run)
	case store.ExternalEffectFailed:
		// Nothing reached the provider; the claim is handed back.
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
			// The controller that made the call is gone; the provider may
			// have applied it. The unknown outcome is recorded durably
			// before the approval is reported spent: until then the worker
			// is asked to retry rather than told a verdict the ledger does
			// not hold yet.
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

// reclaimConnectorApproval hands a claim back whose call provably never
// started and takes the next one, reserving its record first.
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

// connectorClaimReleases counts the claims of one decision of approvalID
// handed back before any provider request. Releases belonging to another
// decision of the same approval are not counted.
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
