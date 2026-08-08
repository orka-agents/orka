package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/store"
)

const maxHarnessV1BrokeredTools = 128

const maxHarnessV1BrokeredToolResultBytes = 1 << 20

// HarnessV1BrokeredToolExecutor is the controller-owned execution boundary for
// Tool CRDs requested by an external harness v1 runtime. Implementations must
// propagate request.IdempotencyKey to the downstream operation.
type HarnessV1BrokeredToolExecutor interface {
	ExecuteHarnessV1BrokeredTool(
		context.Context,
		string,
		*corev1alpha1.Tool,
		harness.ToolCallRequest,
	) (json.RawMessage, error)
}

type HarnessV1BrokeredToolExecutorFunc func(
	context.Context,
	string,
	*corev1alpha1.Tool,
	harness.ToolCallRequest,
) (json.RawMessage, error)

func (f HarnessV1BrokeredToolExecutorFunc) ExecuteHarnessV1BrokeredTool(
	ctx context.Context,
	namespace string,
	tool *corev1alpha1.Tool,
	request harness.ToolCallRequest,
) (json.RawMessage, error) {
	return f(ctx, namespace, tool, request)
}

// agentExecutionSnapshotHarnessV1BrokeredTool is the safe, immutable schema
// exposed to a brokered external runtime. Execution authority remains in Orka:
// the live Tool object must retain this exact identity and definition digest.
type agentExecutionSnapshotHarnessV1BrokeredTool struct {
	Name             string                                     `json:"name"`
	Description      string                                     `json:"description,omitempty"`
	BrokeredClass    corev1alpha1.AgentRuntimeBrokeredToolClass `json:"brokeredClass"`
	Parameters       json.RawMessage                            `json:"parameters"`
	UID              string                                     `json:"uid"`
	Generation       int64                                      `json:"generation"`
	DefinitionDigest string                                     `json:"definitionDigest"`
}

type resolvedHarnessV1ToolGovernance struct {
	mode    harness.ToolExecutionMode
	classes []corev1alpha1.AgentRuntimeBrokeredToolClass
	tools   []agentExecutionSnapshotHarnessV1BrokeredTool
}

func resolveHarnessV1ToolGovernance(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	target resolvedHarnessV1Target,
) (resolvedHarnessV1ToolGovernance, error) {
	if task == nil || agent == nil || agent.Spec.Runtime == nil {
		return resolvedHarnessV1ToolGovernance{}, errors.New("harness v1 tool governance requires Task, Agent, and runtime")
	}
	supportsObserved := slices.Contains(
		target.toolExecutionModes,
		corev1alpha1.AgentRuntimeToolExecutionModeObserved,
	)
	supportsBrokered := slices.Contains(
		target.toolExecutionModes,
		corev1alpha1.AgentRuntimeToolExecutionModeBrokered,
	) && target.supportsContinuation
	brokeredToolsRequested := task.Spec.AgentRuntime != nil && len(task.Spec.AgentRuntime.AllowedTools) > 0

	if supportsBrokered && (brokeredToolsRequested || !supportsObserved || target.runtimeRef != nil) {
		return resolveHarnessV1BrokeredTools(ctx, reader, task, agent, target)
	}
	if !supportsObserved {
		if supportsBrokered {
			return resolveHarnessV1BrokeredTools(ctx, reader, task, agent, target)
		}
		return resolvedHarnessV1ToolGovernance{}, permanentHarnessV1Candidate(
			errors.New("harness v1 runtime does not expose an executable tool mode"),
		)
	}
	if target.runtimeRef != nil {
		return resolvedHarnessV1ToolGovernance{}, permanentHarnessV1Candidate(
			errors.New("harness v1 external runtimes must use brokered tool execution"),
		)
	}
	if err := validateHarnessV1ObservedToolPolicy(task, agent, target); err != nil {
		return resolvedHarnessV1ToolGovernance{}, permanentHarnessV1Candidate(err)
	}
	return resolvedHarnessV1ToolGovernance{mode: harness.ToolExecutionModeObserved}, nil
}

func validateHarnessV1ObservedToolPolicy(
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	target resolvedHarnessV1Target,
) error {
	if target.runtimeRef != nil {
		if agent.Spec.Runtime.DefaultAllowedTools != nil || agent.Spec.Runtime.DefaultAllowBash != nil {
			return errors.New("observed external harness v1 runtimes reject built-in native tool policy metadata")
		}
		if task.Spec.AgentRuntime != nil && len(task.Spec.AgentRuntime.AllowedTools) != 0 {
			return errors.New("observed external harness v1 runtimes do not accept brokered allowedTools")
		}
		if task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.AllowedTools == nil ||
			task.Spec.AgentRuntime.AllowBash == nil || *task.Spec.AgentRuntime.AllowBash {
			return errors.New("new harness v1 observed bindings require an explicit empty allowedTools list and allowBash=false")
		}
		return nil
	}

	allowed := agent.Spec.Runtime.DefaultAllowedTools
	allowBash := agent.Spec.Runtime.DefaultAllowBash
	if task.Spec.AgentRuntime != nil {
		if task.Spec.AgentRuntime.AllowedTools != nil {
			allowed = task.Spec.AgentRuntime.AllowedTools
		}
		if task.Spec.AgentRuntime.AllowBash != nil {
			allowBash = task.Spec.AgentRuntime.AllowBash
		}
	}
	if allowed == nil || len(allowed) != 0 || allowBash == nil || *allowBash {
		return errors.New("new harness v1 observed bindings require an explicit empty allowedTools list and allowBash=false")
	}
	return nil
}

func resolveHarnessV1BrokeredTools(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	target resolvedHarnessV1Target,
) (resolvedHarnessV1ToolGovernance, error) {
	if target.runtimeRef == nil || target.backend != corev1alpha1.AgentExecutionBackendExternalEndpoint {
		return resolvedHarnessV1ToolGovernance{}, permanentHarnessV1Candidate(
			errors.New("brokered harness v1 execution requires an external AgentRuntime"),
		)
	}
	if !target.supportsContinuation {
		return resolvedHarnessV1ToolGovernance{}, permanentHarnessV1Candidate(
			errors.New("brokered harness v1 execution requires durable continuation support"),
		)
	}
	if task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.AllowedTools == nil {
		return resolvedHarnessV1ToolGovernance{}, permanentHarnessV1Candidate(
			errors.New("brokered harness v1 execution requires an explicit task-level allowedTools list"),
		)
	}
	if len(task.Spec.AgentRuntime.AllowedTools) > maxHarnessV1BrokeredTools {
		return resolvedHarnessV1ToolGovernance{}, permanentHarnessV1Candidate(fmt.Errorf(
			"brokered harness v1 allowedTools exceeds %d entries", maxHarnessV1BrokeredTools,
		))
	}
	if len(agent.Spec.Runtime.DefaultAllowedTools) != 0 || agent.Spec.Runtime.DefaultAllowBash != nil ||
		task.Spec.AgentRuntime.AllowBash != nil || len(task.Spec.AgentRuntime.DisallowedTools) != 0 {
		return resolvedHarnessV1ToolGovernance{}, permanentHarnessV1Candidate(
			errors.New("brokered harness v1 execution rejects observed/native tool policy metadata"),
		)
	}

	seen := make(map[string]struct{}, len(task.Spec.AgentRuntime.AllowedTools))
	tools := make([]agentExecutionSnapshotHarnessV1BrokeredTool, 0, len(task.Spec.AgentRuntime.AllowedTools))
	classes := make(map[corev1alpha1.AgentRuntimeBrokeredToolClass]struct{})
	for _, requestedName := range task.Spec.AgentRuntime.AllowedTools {
		name := strings.TrimSpace(requestedName)
		if name == "" || name != requestedName {
			return resolvedHarnessV1ToolGovernance{}, permanentHarnessV1Candidate(
				fmt.Errorf("brokered harness v1 tool name %q is not canonical", requestedName),
			)
		}
		if _, duplicate := seen[name]; duplicate {
			return resolvedHarnessV1ToolGovernance{}, permanentHarnessV1Candidate(
				fmt.Errorf("brokered harness v1 tool %q is duplicated", name),
			)
		}
		seen[name] = struct{}{}

		tool := &corev1alpha1.Tool{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: name}, tool); err != nil {
			return resolvedHarnessV1ToolGovernance{}, fmt.Errorf("load brokered harness v1 Tool %q: %w", name, err)
		}
		if tool.UID == "" || tool.Generation < 1 || !tool.DeletionTimestamp.IsZero() {
			return resolvedHarnessV1ToolGovernance{}, fmt.Errorf("brokered harness v1 Tool %q identity is not ready", name)
		}
		class := tool.Spec.BrokeredToolClass
		if !harness.IsKnownBrokeredToolClass(harness.BrokeredToolClass(class)) {
			return resolvedHarnessV1ToolGovernance{}, permanentHarnessV1Candidate(
				fmt.Errorf("brokered harness v1 Tool %q has unsupported class %q", name, class),
			)
		}
		if class != corev1alpha1.AgentRuntimeBrokeredToolClassRead {
			return resolvedHarnessV1ToolGovernance{}, permanentHarnessV1Candidate(
				fmt.Errorf("harness v1 static safety profile does not allow brokered tool class %q", class),
			)
		}
		if !slices.Contains(target.brokeredToolClasses, class) {
			return resolvedHarnessV1ToolGovernance{}, permanentHarnessV1Candidate(
				fmt.Errorf("harness v1 AgentRuntime does not support brokered tool class %q", class),
			)
		}
		parameters := json.RawMessage(`{"type":"object","properties":{}}`)
		if tool.Spec.Parameters != nil && len(tool.Spec.Parameters.Raw) > 0 {
			parameters = append(json.RawMessage(nil), tool.Spec.Parameters.Raw...)
		}
		if !json.Valid(parameters) {
			return resolvedHarnessV1ToolGovernance{}, permanentHarnessV1Candidate(
				fmt.Errorf("brokered harness v1 Tool %q parameters are invalid JSON", name),
			)
		}
		if _, err := resolveHarnessV1BrokeredToolSchema(parameters); err != nil {
			return resolvedHarnessV1ToolGovernance{}, permanentHarnessV1Candidate(
				fmt.Errorf("brokered harness v1 Tool %q parameters schema is invalid: %w", name, err),
			)
		}
		digest, err := harnessV1BrokeredToolDefinitionDigest(tool)
		if err != nil {
			return resolvedHarnessV1ToolGovernance{}, err
		}
		tools = append(tools, agentExecutionSnapshotHarnessV1BrokeredTool{
			Name: name, Description: tool.Spec.Description, BrokeredClass: class,
			Parameters: parameters, UID: string(tool.UID), Generation: tool.Generation,
			DefinitionDigest: digest,
		})
		classes[class] = struct{}{}
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	classList := make([]corev1alpha1.AgentRuntimeBrokeredToolClass, 0, len(classes))
	for class := range classes {
		classList = append(classList, class)
	}
	slices.Sort(classList)
	return resolvedHarnessV1ToolGovernance{
		mode: harness.ToolExecutionModeBrokered, classes: classList, tools: tools,
	}, nil
}

func harnessV1BrokeredToolDefinitionDigest(tool *corev1alpha1.Tool) (string, error) {
	if tool == nil {
		return "", errors.New("brokered harness v1 Tool is required")
	}
	return acpDomainDigest("harness-v1-brokered-tool-definition", map[string]any{
		"uid": string(tool.UID), "generation": tool.Generation, "spec": tool.Spec,
		"endpoint": tool.Status.Endpoint, "workspace": tool.Status.Workspace, "actor": tool.Status.Actor,
	})
}

func frozenHarnessV1ToolExecutionMode(body agentExecutionSnapshotBody) harness.ToolExecutionMode {
	if body.HarnessV1 == nil || strings.TrimSpace(body.HarnessV1.ToolExecutionMode) == "" {
		// Snapshots created before brokered execution was implemented were
		// observed-only. Preserve their immutable recovery behavior.
		return harness.ToolExecutionModeObserved
	}
	return harness.ToolExecutionMode(body.HarnessV1.ToolExecutionMode)
}

func frozenHarnessV1BrokeredToolDefinitions(body agentExecutionSnapshotBody) ([]harness.ToolDefinition, error) {
	if err := validateFrozenHarnessV1ToolGovernance(body); err != nil {
		return nil, err
	}
	definitions := make([]harness.ToolDefinition, 0, len(body.HarnessV1.BrokeredTools))
	for _, tool := range body.HarnessV1.BrokeredTools {
		definitions = append(definitions, harness.ToolDefinition{
			Name: tool.Name, Description: tool.Description,
			BrokeredClass: harness.BrokeredToolClass(tool.BrokeredClass),
			Parameters:    append(json.RawMessage(nil), tool.Parameters...),
		})
	}
	return definitions, nil
}

func validateFrozenHarnessV1ToolGovernance(body agentExecutionSnapshotBody) error {
	if body.HarnessV1 == nil {
		return errors.New("frozen harness v1 target metadata is required")
	}
	mode := frozenHarnessV1ToolExecutionMode(body)
	switch mode {
	case harness.ToolExecutionModeObserved:
		if len(body.HarnessV1.BrokeredToolClasses) != 0 || len(body.HarnessV1.BrokeredTools) != 0 {
			return errors.New("observed harness v1 snapshot carries brokered tool authority")
		}
		return nil
	case harness.ToolExecutionModeBrokered:
	default:
		return fmt.Errorf("frozen harness v1 tool execution mode %q is unsupported", mode)
	}
	if body.HarnessV1.Backend != string(corev1alpha1.AgentExecutionBackendExternalEndpoint) ||
		body.HarnessV1.RuntimeAuthOnly || len(body.HarnessV1.CredentialRefs) != 0 || body.Workspace != nil {
		return errors.New("brokered harness v1 snapshot carries unsupported runtime or credential authority")
	}
	if len(body.HarnessV1.BrokeredTools) > maxHarnessV1BrokeredTools {
		return fmt.Errorf("brokered harness v1 snapshot exceeds %d tools", maxHarnessV1BrokeredTools)
	}
	classes := body.HarnessV1.BrokeredToolClasses
	for i, class := range classes {
		if !harness.IsKnownBrokeredToolClass(harness.BrokeredToolClass(class)) ||
			i > 0 && classes[i-1] >= class {
			return errors.New("brokered harness v1 snapshot classes are unsupported, duplicated, or unsorted")
		}
	}
	names := make([]string, 0, len(body.HarnessV1.BrokeredTools))
	for i, tool := range body.HarnessV1.BrokeredTools {
		if tool.Name == "" || tool.Name != strings.TrimSpace(tool.Name) || tool.UID == "" || tool.Generation < 1 ||
			!json.Valid(tool.Parameters) || store.ValidateCanonicalDigest("brokered harness v1 Tool definition", tool.DefinitionDigest) != nil ||
			!slices.Contains(classes, tool.BrokeredClass) || i > 0 && body.HarnessV1.BrokeredTools[i-1].Name >= tool.Name {
			return errors.New("brokered harness v1 snapshot Tool authority is incomplete, duplicated, or unsorted")
		}
		if _, err := resolveHarnessV1BrokeredToolSchema(tool.Parameters); err != nil {
			return errors.New("brokered harness v1 snapshot Tool parameters schema is invalid")
		}
		names = append(names, tool.Name)
	}
	if body.RuntimeOverride == nil || body.RuntimeOverride.AllowedTools == nil {
		return errors.New("brokered harness v1 snapshot is missing its explicit allowedTools authority")
	}
	wantNames := append([]string(nil), body.RuntimeOverride.AllowedTools...)
	sort.Strings(wantNames)
	if !slices.Equal(names, wantNames) {
		return errors.New("brokered harness v1 snapshot Tool definitions do not match allowedTools")
	}
	return nil
}

func parseHarnessV1BrokeredToolCall(
	frame harness.HarnessEventFrame,
	turn harness.StartTurnRequest,
	body agentExecutionSnapshotBody,
) (harness.ToolCallRequest, agentExecutionSnapshotHarnessV1BrokeredTool, error) {
	var zeroTool agentExecutionSnapshotHarnessV1BrokeredTool
	if frozenHarnessV1ToolExecutionMode(body) != harness.ToolExecutionModeBrokered {
		return harness.ToolCallRequest{}, zeroTool, errors.New("observed harness v1 turn requested a brokered tool")
	}
	var request harness.ToolCallRequest
	if len(frame.Content) == 0 || json.Unmarshal(frame.Content, &request) != nil {
		return harness.ToolCallRequest{}, zeroTool, errors.New("brokered harness v1 tool call content is invalid")
	}
	if err := request.Validate(); err != nil {
		return harness.ToolCallRequest{}, zeroTool, fmt.Errorf("validate brokered harness v1 tool call: %w", err)
	}
	if request.RuntimeSessionID != turn.RuntimeSessionID || request.TurnID != turn.TurnID ||
		frame.RuntimeSessionID != turn.RuntimeSessionID || frame.TurnID != turn.TurnID ||
		frame.CorrelationID != turn.CorrelationID {
		return harness.ToolCallRequest{}, zeroTool, errors.New("brokered harness v1 tool call identity does not match the frozen turn")
	}
	request.ToolCallID = strings.TrimSpace(request.ToolCallID)
	request.ToolName = strings.TrimSpace(request.ToolName)
	if strings.TrimSpace(frame.ToolCallID) != request.ToolCallID || strings.TrimSpace(frame.ToolName) != request.ToolName {
		return harness.ToolCallRequest{}, zeroTool, errors.New("brokered harness v1 frame and tool call identities do not match")
	}
	wantKey := harness.ToolRequestIdempotencyKey(request.RuntimeSessionID, request.TurnID, request.ToolCallID)
	if request.IdempotencyKey != wantKey {
		return harness.ToolCallRequest{}, zeroTool, errors.New("brokered harness v1 tool call has a non-canonical idempotency key")
	}
	if request.RequiresApproval || request.ApprovalPolicyRef != nil {
		return harness.ToolCallRequest{}, zeroTool, errors.New("brokered harness v1 approval requests are not authorized")
	}
	if len(request.Input) == 0 {
		request.Input = json.RawMessage(`{}`)
	}
	if !json.Valid(request.Input) {
		return harness.ToolCallRequest{}, zeroTool, errors.New("brokered harness v1 tool input is invalid JSON")
	}
	for _, tool := range body.HarnessV1.BrokeredTools {
		if tool.Name == request.ToolName {
			return request, tool, nil
		}
	}
	return harness.ToolCallRequest{}, zeroTool, fmt.Errorf("brokered harness v1 tool %q is outside the frozen allowlist", request.ToolName)
}

func (d *HarnessV1Dispatcher) continueHarnessV1BrokeredToolCall(
	ctx context.Context,
	task *corev1alpha1.Task,
	verified *verifiedHarnessV1Execution,
	protocolClient harnessV1ProtocolClient,
	turn harness.StartTurnRequest,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
	frame harness.HarnessEventFrame,
) error {
	if d == nil {
		return errors.New("harness v1 brokered tool execution is not configured")
	}
	if task == nil || verified == nil || verified.body.HarnessV1 == nil || attempt == nil {
		return errors.New("harness v1 brokered continuation requires Task, snapshot, and attempt")
	}
	if d.ExternalEffects == nil {
		return errors.New("harness v1 brokered tool execution is not configured")
	}
	if attempt.State == store.HarnessV1AttemptCancelRequested {
		return settleCancelledHarnessV1BrokeredToolEffect(
			ctx,
			d.ExternalEffects,
			fence,
			harnessV1BrokeredToolEffectIdentity(task, turn, strings.TrimSpace(frame.ToolCallID)),
		)
	}
	if d.BrokeredToolExecutor == nil {
		return errors.New("harness v1 brokered tool execution is not configured")
	}
	request, frozenTool, err := parseHarnessV1BrokeredToolCall(frame, turn, verified.body)
	if err != nil {
		return fmt.Errorf("%w: %v", errHarnessV1FrameAuthorityViolation, err)
	}
	reader := d.APIReader
	if reader == nil {
		reader = d.Client
	}
	if reader == nil {
		return errors.New("harness v1 brokered tool execution requires a Kubernetes reader")
	}
	tool := &corev1alpha1.Tool{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: request.ToolName}, tool); err != nil {
		return fmt.Errorf("load brokered harness v1 Tool %q: %w", request.ToolName, err)
	}
	digest, err := harnessV1BrokeredToolDefinitionDigest(tool)
	if err != nil {
		return fmt.Errorf("%w: brokered harness v1 Tool %q is invalid: %v", errHarnessV1FrameAuthorityViolation, request.ToolName, err)
	}
	if string(tool.UID) != frozenTool.UID || tool.Generation != frozenTool.Generation || digest != frozenTool.DefinitionDigest ||
		tool.Spec.BrokeredToolClass != frozenTool.BrokeredClass || !tool.DeletionTimestamp.IsZero() {
		return fmt.Errorf("%w: brokered harness v1 Tool %q changed after binding", errHarnessV1FrameAuthorityViolation, request.ToolName)
	}
	if err := validateHarnessV1BrokeredToolInput(frozenTool.Parameters, request.Input); err != nil {
		return fmt.Errorf("%w: validate brokered harness v1 Tool %q input: %v", errHarnessV1FrameAuthorityViolation, request.ToolName, err)
	}
	identity := harnessV1BrokeredToolEffectIdentity(task, turn, request.ToolCallID)
	effectRequest := map[string]any{
		"taskUID": task.UID, "attempt": attempt.Attempt, "bindingDigest": attempt.BindingDigest,
		"snapshotDigest": attempt.SnapshotDigest, "toolDefinitionDigest": frozenTool.DefinitionDigest,
		"request": request,
	}
	result, _, err := runExternalEffectWithReplay(
		ctx,
		d.ExternalEffects,
		fence,
		identity,
		effectRequest,
		func(callCtx context.Context) (harness.ToolCallResult, error) {
			output, executeErr := d.BrokeredToolExecutor.ExecuteHarnessV1BrokeredTool(
				callCtx, task.Namespace, tool.DeepCopy(), request,
			)
			result := harness.ToolCallResult{
				Version: harness.ProtocolVersion, RuntimeSessionID: request.RuntimeSessionID,
				TurnID: request.TurnID, ToolCallID: request.ToolCallID,
				IdempotencyKey: request.IdempotencyKey, Approved: true,
			}
			if executeErr != nil {
				result.Approved = false
				result.Error = &harness.ErrorInfo{Code: "ToolExecutionFailed", Message: "brokered tool execution failed"}
				return result, nil
			}
			if len(output) == 0 || !json.Valid(output) {
				encoded, marshalErr := json.Marshal(string(output))
				if marshalErr != nil {
					return harness.ToolCallResult{}, marshalErr
				}
				output = encoded
			}
			if len(output) > maxHarnessV1BrokeredToolResultBytes {
				result.Approved = false
				result.Error = &harness.ErrorInfo{Code: "ToolResultTooLarge", Message: "brokered tool result exceeded the size limit"}
				return result, nil
			}
			result.Output = append(json.RawMessage(nil), output...)
			return result, nil
		},
	)
	if err != nil {
		return fmt.Errorf("execute brokered harness v1 tool %q: %w", request.ToolName, err)
	}
	continueRequest := harness.ContinueTurnRequest{
		Version: harness.ProtocolVersion, Namespace: turn.Namespace, TaskName: turn.TaskName,
		SessionName: turn.SessionName, RuntimeSessionID: turn.RuntimeSessionID, TurnID: turn.TurnID,
		CorrelationID: turn.CorrelationID, ToolResults: []harness.ToolCallResult{result},
		Metadata: map[string]string{"toolCallFrameSeq": fmt.Sprint(frame.Seq)},
	}
	if _, err := protocolClient.ContinueTurn(ctx, continueRequest); err != nil {
		return fmt.Errorf("continue brokered harness v1 turn: %w", err)
	}
	return nil
}

func harnessV1BrokeredToolEffectIdentity(
	task *corev1alpha1.Task,
	turn harness.StartTurnRequest,
	toolCallID string,
) store.ExternalEffectIdentity {
	return store.ExternalEffectIdentity{
		Kind: "harness-v1-tool", Namespace: task.Namespace, AggregateID: string(task.UID),
		OperationID: store.CanonicalControlID(
			"harness-v1-tool-call", string(turn.RuntimeSessionID), string(turn.TurnID), toolCallID,
		),
	}
}

// settleCancelledHarnessV1BrokeredToolEffect closes any reservation that may
// have crossed its execution boundary before cancellation became durable. A
// missing effect means execution never reserved the call; an already-terminal
// effect keeps its authoritative classification.
func settleCancelledHarnessV1BrokeredToolEffect(
	ctx context.Context,
	effects store.ExternalEffectStore,
	fence store.ControllerEpochFence,
	identity store.ExternalEffectIdentity,
) error {
	id, err := identity.CanonicalID()
	if err != nil {
		return err
	}
	effect, err := effects.GetExternalEffect(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	switch effect.State {
	case store.ExternalEffectSucceeded, store.ExternalEffectFailed, store.ExternalEffectOutcomeUnknown:
		return nil
	default:
		return settleExternalEffectStore(
			ctx, effects, fence, identity, store.ExternalEffectOutcomeUnknown, nil,
		)
	}
}

func resolveHarnessV1BrokeredToolSchema(parameters json.RawMessage) (*jsonschema.Resolved, error) {
	var schema jsonschema.Schema
	if err := json.Unmarshal(parameters, &schema); err != nil {
		return nil, fmt.Errorf("decode JSON Schema: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("resolve JSON Schema: %w", err)
	}
	return resolved, nil
}

func validateHarnessV1BrokeredToolInput(parameters, input json.RawMessage) error {
	resolved, err := resolveHarnessV1BrokeredToolSchema(parameters)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var instance any
	if err := decoder.Decode(&instance); err != nil {
		return fmt.Errorf("decode input JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("input contains more than one JSON value")
	}
	if err := resolved.Validate(instance); err != nil {
		return fmt.Errorf("input does not match the frozen parameters schema: %w", err)
	}
	return nil
}
