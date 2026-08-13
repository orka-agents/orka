package controller

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
	workerexecutor "github.com/orka-agents/orka/internal/worker"
)

type ACPMCPAuthenticatedTask struct {
	Name         string
	Namespace    string
	UID          string
	ParentTaskID string
	AgentName    string
}

type acpMCPAuthenticatedTaskContextKey struct{}

func withACPMCPAuthenticatedTask(ctx context.Context, task ACPMCPAuthenticatedTask) context.Context {
	return context.WithValue(ctx, acpMCPAuthenticatedTaskContextKey{}, task)
}

func ACPMCPAuthenticatedTaskFromContext(ctx context.Context) (ACPMCPAuthenticatedTask, bool) {
	task, ok := ctx.Value(acpMCPAuthenticatedTaskContextKey{}).(ACPMCPAuthenticatedTask)
	return task, ok && task.Name != "" && task.Namespace != "" && task.UID != ""
}

type ACPMCPBrokerCredentials struct {
	ControllerBearerToken string
	CapabilitySecret      []byte
	ExpectedFence         harnessv2.Fence
	RuntimeProfile        harnessv2.RuntimeProfile
	ControllerFence       store.ControllerEpochFence
	Task                  ACPMCPAuthenticatedTask
}

type ACPMCPBrokerCredentialResolver interface {
	ResolveACPMCPBrokerCredentials(context.Context, harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error)
}

type ACPMCPBrokerCredentialResolverFunc func(context.Context, harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error)

func (f ACPMCPBrokerCredentialResolverFunc) ResolveACPMCPBrokerCredentials(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error) {
	return f(ctx, request)
}

type ACPMCPPromptAuthorizer interface {
	AuthorizeACPMCPPrompt(context.Context, harnessv2.MCPBrokerCallRequest) error
}

type ACPMCPPromptAuthorizerFunc func(context.Context, harnessv2.MCPBrokerCallRequest) error

func (f ACPMCPPromptAuthorizerFunc) AuthorizeACPMCPPrompt(ctx context.Context, request harnessv2.MCPBrokerCallRequest) error {
	return f(ctx, request)
}

type ACPMCPToolExecutor interface {
	ExecuteACPMCPTool(context.Context, harnessv2.MCPBrokerCallRequest, harnessv2.MCPToolDescriptor) (json.RawMessage, error)
}

type ACPMCPToolExecutorFunc func(context.Context, harnessv2.MCPBrokerCallRequest, harnessv2.MCPToolDescriptor) (json.RawMessage, error)

func (f ACPMCPToolExecutorFunc) ExecuteACPMCPTool(ctx context.Context, request harnessv2.MCPBrokerCallRequest, descriptor harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
	return f(ctx, request, descriptor)
}

type RegistryACPMCPToolExecutor struct {
	Registry       *tools.Registry
	Reader         client.Reader
	KubeClient     kubernetes.Interface
	HTTPClient     *http.Client
	ContextFactory func(context.Context, harnessv2.MCPBrokerCallRequest) (*tools.ToolContext, error)
}

func (e RegistryACPMCPToolExecutor) ExecuteACPMCPTool(
	ctx context.Context,
	request harnessv2.MCPBrokerCallRequest,
	descriptor harnessv2.MCPToolDescriptor,
) (json.RawMessage, error) {
	var result string
	var err error
	switch descriptor.Source {
	case harnessv2.MCPToolSourceBrokeredBuiltin:
		registry := e.Registry
		if registry == nil {
			registry = tools.DefaultRegistry
		}
		if _, ok := registry.Get(descriptor.Name); !ok {
			return nil, fmt.Errorf("MCP tool %q is not registered", descriptor.Name)
		}
		if e.ContextFactory != nil {
			toolContext, contextErr := e.ContextFactory(ctx, request)
			if contextErr != nil {
				return nil, contextErr
			}
			if toolContext != nil {
				copy := *toolContext
				copy.Namespace = request.Namespace
				copy.TaskUID = string(request.Metadata.TaskUID)
				copy.ToolCallID = request.Call.CallID
				if copy.Tenant == "" {
					copy.Tenant = request.Namespace
				}
				ctx = tools.WithToolContext(ctx, &copy)
			}
		}
		result, err = registry.Execute(ctx, descriptor.Name, request.Call.Arguments)
	case harnessv2.MCPToolSourceBrokeredCustom:
		if e.Reader == nil || e.KubeClient == nil {
			return nil, fmt.Errorf("custom MCP tool executor is not configured")
		}
		tool := &corev1alpha1.Tool{}
		if getErr := e.Reader.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: descriptor.Name}, tool); getErr != nil {
			return nil, fmt.Errorf("load custom MCP tool %q: %w", descriptor.Name, getErr)
		}
		currentDescriptor, descriptorErr := customACPMCPToolDescriptor(tool)
		if descriptorErr != nil {
			return nil, descriptorErr
		}
		expected, expectedErr := harnessv2.CanonicalValue(descriptor)
		current, currentErr := harnessv2.CanonicalValue(currentDescriptor)
		if expectedErr != nil || currentErr != nil || !bytes.Equal(expected, current) {
			return nil, fmt.Errorf("custom MCP tool %q changed after prompt authorization", descriptor.Name)
		}
		executor := workerexecutor.NewToolExecutorForNamespace(request.Namespace, e.KubeClient, e.HTTPClient)
		execCtx := workerexecutor.WithToolCallID(ctx, request.Call.CallID)
		execCtx = workerexecutor.WithToolIdempotencyKey(execCtx, string(request.Metadata.OperationID))
		result, err = executor.Execute(execCtx, tool, request.Call.Arguments)
	default:
		return nil, fmt.Errorf("MCP tool %q is not broker-executable", descriptor.Name)
	}
	if err != nil {
		return nil, err
	}
	raw := json.RawMessage(result)
	if _, err := harnessv2.CanonicalJSON(raw); err != nil {
		encoded, encodeErr := json.Marshal(result)
		if encodeErr != nil {
			return nil, encodeErr
		}
		raw = encoded
	}
	if len(raw) > harnessv2.MaxMCPResultBytes {
		return nil, fmt.Errorf("MCP tool result exceeds %d bytes", harnessv2.MaxMCPResultBytes)
	}
	return raw, nil
}

type ACPMCPBrokerDependencies struct {
	Reader         client.Reader
	Epochs         *ControllerEpochManager
	ControlStore   store.DurableControlStore
	KubeClient     kubernetes.Interface
	HTTPClient     *http.Client
	Registry       *tools.Registry
	ContextFactory func(context.Context, harnessv2.MCPBrokerCallRequest) (*tools.ToolContext, error)
}

func NewProductionACPMCPBroker(dependencies ACPMCPBrokerDependencies) (*ACPMCPBroker, error) {
	if dependencies.Reader == nil || dependencies.Epochs == nil || dependencies.ControlStore == nil || dependencies.KubeClient == nil {
		return nil, fmt.Errorf("production ACP MCP broker dependencies are incomplete")
	}
	broker := &ACPMCPBroker{
		Credentials: KubernetesACPMCPBrokerCredentialResolver{Reader: dependencies.Reader, Epochs: dependencies.Epochs},
		Prompts:     DurableACPMCPPromptAuthorizer{Attempts: dependencies.ControlStore},
		Executor: RegistryACPMCPToolExecutor{
			Registry: dependencies.Registry, Reader: dependencies.Reader, KubeClient: dependencies.KubeClient,
			HTTPClient: dependencies.HTTPClient, ContextFactory: dependencies.ContextFactory,
		},
		Effects: dependencies.ControlStore,
	}
	if err := broker.Validate(); err != nil {
		return nil, err
	}
	return broker, nil
}

type ACPMCPBroker struct {
	Credentials  ACPMCPBrokerCredentialResolver
	Prompts      ACPMCPPromptAuthorizer
	Executor     ACPMCPToolExecutor
	Effects      store.ExternalEffectStore
	MaxBodyBytes int64
}

func (b *ACPMCPBroker) Validate() error {
	if b == nil || b.Credentials == nil || b.Prompts == nil || b.Executor == nil || b.Effects == nil {
		return fmt.Errorf("ACP MCP broker requires credentials, prompt authorization, tool executor, and external-effect store")
	}
	return nil
}

func (b *ACPMCPBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != harnessv2.MCPBrokerCallPath {
		http.NotFound(w, r)
		return
	}
	if err := b.Validate(); err != nil {
		writeACPMCPError(w, http.StatusServiceUnavailable, "MCP broker is unavailable")
		return
	}
	limit := b.MaxBodyBytes
	if limit <= 0 || limit > harnessv2.MaxCanonicalJSONBytes {
		limit = harnessv2.MaxCanonicalJSONBytes
	}
	body := http.MaxBytesReader(w, r.Body, limit)
	defer body.Close() //nolint:errcheck
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	var request harnessv2.MCPBrokerCallRequest
	if err := decoder.Decode(&request); err != nil {
		writeACPMCPError(w, http.StatusBadRequest, "invalid MCP broker request")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeACPMCPError(w, http.StatusBadRequest, "invalid MCP broker request")
		return
	}
	now := time.Now().UTC()
	descriptor, err := request.ValidateAt(now)
	if err != nil {
		writeACPMCPError(w, http.StatusBadRequest, "invalid MCP broker request")
		return
	}
	credentials, err := b.Credentials.ResolveACPMCPBrokerCredentials(r.Context(), request)
	if err != nil || !constantTimeBearerMatch(r.Header.Get("Authorization"), credentials.ControllerBearerToken) {
		writeACPMCPError(w, http.StatusUnauthorized, "MCP broker authentication failed")
		return
	}
	if mismatch := harnessv2.CompareFence(credentials.ExpectedFence, request.Metadata.Fence, true); mismatch != harnessv2.FenceMatch {
		writeACPMCPError(w, http.StatusGone, "MCP broker fence is stale")
		return
	}
	if err := request.Authorization.ValidateProfile(credentials.RuntimeProfile); err != nil {
		writeACPMCPError(w, http.StatusGone, "MCP broker policy is stale")
		return
	}
	if err := harnessv2.VerifyOperationCapability(
		credentials.CapabilitySecret, r.Header.Get(harnessv2.OperationCapabilityHeader), request.Metadata, true, now,
	); err != nil {
		writeACPMCPError(w, http.StatusForbidden, "MCP broker operation authorization failed")
		return
	}
	if err := b.Prompts.AuthorizeACPMCPPrompt(r.Context(), request); err != nil {
		writeACPMCPError(w, http.StatusForbidden, "MCP prompt is not active")
		return
	}
	call := func(ctx context.Context) (json.RawMessage, error) {
		ctx = withACPMCPAuthenticatedTask(ctx, credentials.Task)
		result, executeErr := b.Executor.ExecuteACPMCPTool(ctx, request, descriptor)
		if executeErr != nil {
			return nil, executeErr
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := b.Prompts.AuthorizeACPMCPPrompt(ctx, request); err != nil {
			return nil, fmt.Errorf("MCP prompt authority was revoked during tool execution")
		}
		return result, nil
	}
	var result json.RawMessage
	replayed := false
	var effectIdentity *store.ExternalEffectIdentity
	if descriptor.Effect == harnessv2.MCPToolEffectConsequential {
		identity := store.ExternalEffectIdentity{
			Kind: "acp-mcp-tool", Namespace: request.Namespace,
			AggregateID: string(request.Authorization.RuntimeSessionUID),
			OperationID: string(request.Metadata.OperationID),
		}
		effectIdentity = &identity
		result, replayed, err = runExternalEffectWithReplay(
			r.Context(), b.Effects, credentials.ControllerFence, identity,
			map[string]any{"call": request.Call, "descriptor": descriptor}, call,
		)
	} else {
		result, err = call(r.Context())
	}
	if err != nil {
		if effectIdentity != nil && !errors.Is(err, store.ErrConflict) {
			settleCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
			_ = settleExternalEffectStore(settleCtx, b.Effects, credentials.ControllerFence, *effectIdentity, store.ExternalEffectOutcomeUnknown, nil)
			cancel()
		}
		writeACPMCPError(w, http.StatusBadGateway, "MCP tool execution failed")
		return
	}
	response := harnessv2.MCPBrokerCallResponse{
		Protocol: harnessv2.ProtocolVersion, CallID: request.Call.CallID, Result: result,
		IsError: mcpToolResultIsError(result), Replayed: replayed,
	}
	if err := response.Validate(); err != nil {
		writeACPMCPError(w, http.StatusBadGateway, "MCP tool returned an invalid result")
		return
	}
	writeACPMCPJSON(w, http.StatusOK, response)
}

func mcpToolResultIsError(result json.RawMessage) bool {
	var envelope struct {
		Success *bool `json:"success"`
		IsError bool  `json:"isError"`
	}
	if json.Unmarshal(result, &envelope) != nil {
		return false
	}
	return envelope.IsError || (envelope.Success != nil && !*envelope.Success)
}

func constantTimeBearerMatch(header, expected string) bool {
	value := strings.TrimSpace(header)
	if len(value) >= len("Bearer ") && strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
		value = strings.TrimSpace(value[len("Bearer "):])
	}
	expected = strings.TrimSpace(expected)
	return len(value) == len(expected) && len(expected) >= 32 && subtle.ConstantTimeCompare([]byte(value), []byte(expected)) == 1
}

type DurableACPMCPPromptAuthorizer struct {
	Attempts store.PromptAttemptStore
}

func (a DurableACPMCPPromptAuthorizer) AuthorizeACPMCPPrompt(ctx context.Context, request harnessv2.MCPBrokerCallRequest) error {
	if a.Attempts == nil {
		return fmt.Errorf("prompt-attempt store is required")
	}
	key := store.PromptAttemptKey{
		Namespace: request.Namespace, TaskUID: string(request.Metadata.TaskUID),
		Attempt: int64(request.Metadata.TaskAttempt), PromptID: string(request.Metadata.PromptID),
	}
	id, err := key.CanonicalID()
	if err != nil {
		return err
	}
	attempt, err := a.Attempts.GetPromptAttempt(ctx, id)
	if err != nil {
		return err
	}
	if attempt.ExecutionState != store.PromptExecutionSubmitting &&
		attempt.ExecutionState != store.PromptExecutionAccepted && attempt.ExecutionState != store.PromptExecutionRunning {
		return fmt.Errorf("prompt attempt is in state %s", attempt.ExecutionState)
	}
	if attempt.SessionUID != string(request.Authorization.RuntimeSessionUID) ||
		attempt.RuntimeInstanceID != string(request.Metadata.Fence.RuntimeInstanceID) ||
		attempt.ControllerEpoch != int64(request.Metadata.Fence.ControllerEpoch) {
		return fmt.Errorf("prompt attempt identity does not match MCP authorization")
	}
	return nil
}

type KubernetesACPMCPBrokerCredentialResolver struct {
	Reader client.Reader
	Epochs *ControllerEpochManager
}

func (r KubernetesACPMCPBrokerCredentialResolver) ResolveACPMCPBrokerCredentials(
	ctx context.Context,
	request harnessv2.MCPBrokerCallRequest,
) (ACPMCPBrokerCredentials, error) {
	if r.Reader == nil || r.Epochs == nil {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("kubernetes MCP credential resolver is not configured")
	}
	controllerFence, err := r.Epochs.CurrentFence(ctx)
	if err != nil {
		return ACPMCPBrokerCredentials{}, err
	}
	if uint64(controllerFence.Epoch) != request.Metadata.Fence.ControllerEpoch {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("MCP request uses a stale controller epoch")
	}
	task, execution, err := findACPMCPTaskExecution(ctx, r.Reader, request, controllerFence)
	if err != nil {
		return ACPMCPBrokerCredentials{}, err
	}
	var credentials ACPMCPBrokerCredentials
	switch {
	case strings.TrimSpace(execution.RuntimePoolName) != "":
		credentials, err = r.resolveRuntimePoolCredentials(ctx, request, execution, controllerFence)
	case strings.TrimSpace(execution.AgentRuntimeName) != "":
		credentials, err = r.resolveExternalRuntimeCredentials(ctx, request, execution, controllerFence)
	default:
		return ACPMCPBrokerCredentials{}, fmt.Errorf("active MCP task has no runtime target")
	}
	if err != nil {
		return ACPMCPBrokerCredentials{}, err
	}
	credentials.Task = ACPMCPAuthenticatedTask{
		Name: task.Name, Namespace: task.Namespace, UID: string(task.UID),
		ParentTaskID: labels.ParentTaskName(task.Labels, task.Annotations),
	}
	if task.Spec.AgentRef != nil {
		credentials.Task.AgentName = strings.TrimSpace(task.Spec.AgentRef.Name)
	}
	return credentials, nil
}

func (r KubernetesACPMCPBrokerCredentialResolver) resolveRuntimePoolCredentials(
	ctx context.Context,
	request harnessv2.MCPBrokerCallRequest,
	execution *corev1alpha1.TaskExecutionStatus,
	controllerFence store.ControllerEpochFence,
) (ACPMCPBrokerCredentials, error) {
	pool, err := findACPMCPRuntimePool(ctx, r.Reader, request.Namespace, request.Metadata.Fence.RuntimePoolUID)
	if err != nil {
		return ACPMCPBrokerCredentials{}, err
	}
	if execution.RuntimePoolUID != string(pool.UID) || execution.RuntimePoolName != pool.Name {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("active MCP task is bound to a different runtime pool")
	}
	active := pool.Status.ActiveInstance
	if active.ControllerEpoch != controllerFence.Epoch || active.ProfileDigest != pool.Spec.Runtime.Profile.Digest ||
		active.ProtocolVersion != corev1alpha1.RuntimePoolProtocolHarnessV2 {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("runtime pool active instance is stale")
	}
	expected := expectedACPMCPFence(pool, execution, controllerFence)
	if mismatch := harnessv2.CompareFence(expected, request.Metadata.Fence, true); mismatch != harnessv2.FenceMatch {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("runtime pool active instance does not match MCP request")
	}
	bearer, capability, err := runtimePoolACPMCPAuthMaterial(ctx, r.Reader, pool)
	if err != nil {
		return ACPMCPBrokerCredentials{}, err
	}
	return ACPMCPBrokerCredentials{
		ControllerBearerToken: bearer, CapabilitySecret: capability, ExpectedFence: expected,
		RuntimeProfile: runtimeProfileFromPool(pool.Spec.Runtime.Profile), ControllerFence: controllerFence,
	}, nil
}

func (r KubernetesACPMCPBrokerCredentialResolver) resolveExternalRuntimeCredentials(
	ctx context.Context,
	request harnessv2.MCPBrokerCallRequest,
	execution *corev1alpha1.TaskExecutionStatus,
	controllerFence store.ControllerEpochFence,
) (ACPMCPBrokerCredentials, error) {
	runtime := &corev1alpha1.AgentRuntime{}
	key := client.ObjectKey{Namespace: request.Namespace, Name: execution.AgentRuntimeName}
	if err := r.Reader.Get(ctx, key, runtime); err != nil {
		return ACPMCPBrokerCredentials{}, err
	}
	observed := runtime.Status.ObservedCapabilities
	if string(runtime.UID) != execution.AgentRuntimeUID || !runtime.Status.Ready || runtime.Status.ObservedGeneration != runtime.Generation ||
		runtime.RegisteredContractVersion() != corev1alpha1.AgentRuntimeContractHarnessV2 || runtime.Spec.Capabilities == nil ||
		runtime.Spec.Capabilities.WorkspaceGovernance == nil || runtime.Spec.Capabilities.Profile == nil ||
		!runtime.Spec.Capabilities.WorkspaceGovernance.Strict() || observed == nil {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("external AgentRuntime is not ready for MCP brokering")
	}
	if observed.ControllerEpoch != controllerFence.Epoch || observed.RuntimeProfileDigest != runtime.Spec.Capabilities.Profile.Digest ||
		observed.ProtocolVersion != string(corev1alpha1.RuntimePoolProtocolHarnessV2) {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("external AgentRuntime observation is stale")
	}
	profile, err := agentRuntimeProfile(*runtime.Spec.Capabilities.Profile)
	if err != nil {
		return ACPMCPBrokerCredentials{}, err
	}
	profileDigest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil || string(profileDigest) != runtime.Spec.Capabilities.Profile.Digest {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("external AgentRuntime profile digest is invalid")
	}
	expected := harnessv2.Fence{
		RuntimeInstanceID: harnessv2.RuntimeInstanceID(observed.RuntimeInstanceID),
		SupervisorBootID:  harnessv2.SupervisorBootID(observed.SupervisorBootID),
		ControllerEpoch:   uint64(controllerFence.Epoch), RuntimePoolUID: harnessv2.RuntimePoolUID(observed.RuntimePoolUID),
		RuntimePoolGeneration:      uint64(observed.RuntimePoolGeneration),
		RuntimeSessionUID:          harnessv2.RuntimeSessionUID(execution.RuntimeSessionUID),
		RuntimeSessionGeneration:   uint64(execution.RuntimeSessionGeneration),
		RuntimeProfileDigest:       harnessv2.ProfileDigest(observed.RuntimeProfileDigest),
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	if mismatch := harnessv2.CompareFence(expected, request.Metadata.Fence, true); mismatch != harnessv2.FenceMatch {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("external AgentRuntime fence does not match MCP request")
	}
	if runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef == nil || runtime.Spec.ClientAuth.OperationCapabilitySecretRef == nil {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("external AgentRuntime v2 client auth references are required")
	}
	bearerSecret, err := readAgentRuntimeMCPSecret(ctx, r.Reader, runtime.Namespace, *runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef)
	if err != nil {
		return ACPMCPBrokerCredentials{}, err
	}
	capabilitySecret, err := readAgentRuntimeMCPSecret(ctx, r.Reader, runtime.Namespace, *runtime.Spec.ClientAuth.OperationCapabilitySecretRef)
	if err != nil {
		return ACPMCPBrokerCredentials{}, err
	}
	if bearerSecret.ResourceVersion != runtime.Status.ObservedControllerAuthRefResourceVersion ||
		capabilitySecret.ResourceVersion != runtime.Status.ObservedOperationCapabilityRefResourceVersion {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("external AgentRuntime auth material changed after conformance")
	}
	bearer := strings.TrimSpace(string(bearerSecret.Data[runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef.Key]))
	capability := append([]byte(nil), capabilitySecret.Data[runtime.Spec.ClientAuth.OperationCapabilitySecretRef.Key]...)
	if len(bearer) < 32 || len(capability) < harnessv2.MinCapabilitySecretBytes {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("external AgentRuntime auth material is incomplete")
	}
	return ACPMCPBrokerCredentials{
		ControllerBearerToken: bearer, CapabilitySecret: capability, ExpectedFence: expected,
		RuntimeProfile: profile, ControllerFence: controllerFence,
	}, nil
}

func readAgentRuntimeMCPSecret(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	reference corev1alpha1.AgentRuntimeSecretKeyReference,
) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: reference.Name}, secret); err != nil {
		return nil, err
	}
	if strings.TrimSpace(reference.Key) == "" {
		return nil, fmt.Errorf("external AgentRuntime auth key is missing")
	}
	return secret, nil
}

func findACPMCPRuntimePool(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	poolUID harnessv2.RuntimePoolUID,
) (*corev1alpha1.RuntimePool, error) {
	var pools corev1alpha1.RuntimePoolList
	if err := reader.List(ctx, &pools, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var pool *corev1alpha1.RuntimePool
	for index := range pools.Items {
		candidate := &pools.Items[index]
		if string(candidate.UID) != string(poolUID) {
			continue
		}
		if pool != nil {
			return nil, fmt.Errorf("runtime pool UID is ambiguous")
		}
		pool = candidate.DeepCopy()
	}
	if pool == nil || pool.Status.ActiveInstance == nil {
		return nil, fmt.Errorf("runtime pool active instance was not found")
	}
	return pool, nil
}

func findACPMCPTaskExecution(
	ctx context.Context,
	reader client.Reader,
	request harnessv2.MCPBrokerCallRequest,
	controllerFence store.ControllerEpochFence,
) (*corev1alpha1.Task, *corev1alpha1.TaskExecutionStatus, error) {
	var tasks corev1alpha1.TaskList
	if err := reader.List(ctx, &tasks, client.InNamespace(request.Namespace)); err != nil {
		return nil, nil, err
	}
	var task *corev1alpha1.Task
	var execution *corev1alpha1.TaskExecutionStatus
	for index := range tasks.Items {
		candidate := &tasks.Items[index]
		if string(candidate.UID) != string(request.Metadata.TaskUID) {
			continue
		}
		if task != nil {
			return nil, nil, fmt.Errorf("task UID is ambiguous")
		}
		task = candidate.DeepCopy()
		if candidate.Status.Execution != nil {
			copy := *candidate.Status.Execution
			execution = &copy
		}
	}
	if execution == nil {
		return nil, nil, fmt.Errorf("active MCP task was not found")
	}
	if execution.State != corev1alpha1.TaskExecutionStateSubmitting &&
		execution.State != corev1alpha1.TaskExecutionStateAccepted && execution.State != corev1alpha1.TaskExecutionStateRunning {
		return nil, nil, fmt.Errorf("active MCP task is not running")
	}
	if execution.Attempt != int32(request.Metadata.TaskAttempt) || execution.PromptID != string(request.Metadata.PromptID) ||
		execution.RuntimeSessionUID != string(request.Authorization.RuntimeSessionUID) ||
		execution.RuntimeSessionGeneration != int64(request.Authorization.SessionGeneration) ||
		execution.RuntimeInstanceID != string(request.Metadata.Fence.RuntimeInstanceID) ||
		execution.ControllerEpoch != controllerFence.Epoch {
		return nil, nil, fmt.Errorf("active MCP task identity does not match request")
	}
	return task, execution, nil
}

func expectedACPMCPFence(
	pool *corev1alpha1.RuntimePool,
	execution *corev1alpha1.TaskExecutionStatus,
	controllerFence store.ControllerEpochFence,
) harnessv2.Fence {
	active := pool.Status.ActiveInstance
	return harnessv2.Fence{
		RuntimeInstanceID: harnessv2.RuntimeInstanceID(active.RuntimeInstanceID),
		SupervisorBootID:  harnessv2.SupervisorBootID(active.BootID),
		ControllerEpoch:   uint64(controllerFence.Epoch), RuntimePoolUID: harnessv2.RuntimePoolUID(pool.UID),
		RuntimePoolGeneration:      uint64(pool.Generation),
		RuntimeSessionUID:          harnessv2.RuntimeSessionUID(execution.RuntimeSessionUID),
		RuntimeSessionGeneration:   uint64(execution.RuntimeSessionGeneration),
		RuntimeProfileDigest:       harnessv2.ProfileDigest(pool.Spec.Runtime.Profile.Digest),
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
}

func runtimePoolACPMCPAuthMaterial(
	ctx context.Context,
	reader client.Reader,
	pool *corev1alpha1.RuntimePool,
) (string, []byte, error) {
	active := pool.Status.ActiveInstance
	namespace := pool.Spec.RuntimeNamespace
	if namespace == "" {
		namespace = active.PodNamespace
	}
	var secrets corev1.SecretList
	if err := reader.List(ctx, &secrets, client.InNamespace(namespace), client.MatchingLabels{
		runtimePoolAuthLabel: "true", runtimePoolUIDLabel: string(pool.UID),
	}); err != nil {
		return "", nil, err
	}
	if len(secrets.Items) != 1 {
		return "", nil, fmt.Errorf("runtime pool requires exactly one auth Secret")
	}
	secret := secrets.Items[0]
	bearer := strings.TrimSpace(string(secret.Data[runtimePoolControllerTokenKey]))
	capability := append([]byte(nil), secret.Data[runtimePoolCapabilitySecretKey]...)
	if len(bearer) < 32 || len(capability) < harnessv2.MinCapabilitySecretBytes {
		return "", nil, fmt.Errorf("runtime pool auth Secret is incomplete")
	}
	return bearer, capability, nil
}

func writeACPMCPError(w http.ResponseWriter, status int, message string) {
	writeACPMCPJSON(w, status, map[string]any{"error": message})
}

func writeACPMCPJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
