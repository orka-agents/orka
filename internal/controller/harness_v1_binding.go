/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/store"
)

const (
	defaultHarnessV1PolicyName  = "compatibility"
	defaultHarnessV1SessionName = "task"

	harnessV1SessionBootstrapSchemaVersion   = 1
	harnessV1SessionBootstrapMaxMessages     = 64
	harnessV1SessionBootstrapMaxBytes        = 128 * 1024
	harnessV1SessionBootstrapMaxMessageBytes = 32 * 1024
	harnessV1CredentialFlagYes               = "yes"
)

type resolvedHarnessV1Target struct {
	endpoint      string
	backend       corev1alpha1.AgentExecutionBackend
	runtimeName   string
	authSecret    *corev1.Secret
	authSecretKey string
	runtimeRef    *corev1alpha1.AgentRuntime
}

type verifiedHarnessV1Execution struct {
	binding    *corev1alpha1.AgentExecutionBinding
	snapshot   *store.AgentExecutionSnapshot
	body       agentExecutionSnapshotBody
	frozenTask *corev1alpha1.Task
}

// permanentHarnessV1CandidateError marks deterministic Task, Agent, or
// compatibility-policy violations. Candidate resolution errors without this
// marker remain retryable because they may depend on mutable control-plane
// state.
type permanentHarnessV1CandidateError struct{ err error }

func (e *permanentHarnessV1CandidateError) Error() string { return e.err.Error() }
func (e *permanentHarnessV1CandidateError) Unwrap() error { return e.err }

func permanentHarnessV1Candidate(err error) error {
	if err == nil {
		return nil
	}
	return &permanentHarnessV1CandidateError{err: err}
}

func isPermanentHarnessV1CandidateError(err error) bool {
	var permanent *permanentHarnessV1CandidateError
	return errors.As(err, &permanent)
}

//nolint:gocyclo // Candidate resolution keeps the fail-closed admission checks auditable in one path.
func (r *TaskReconciler) resolveHarnessV1ExecutionCandidate(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) (*agentExecutionCandidate, error) {
	if r.AgentExecutionSnapshots == nil {
		return nil, errors.New("encrypted agent execution snapshot store is required; v1 admission fails closed")
	}
	if task == nil || task.UID == "" || task.Generation < 1 || agent == nil || agent.UID == "" || agent.Generation < 1 {
		return nil, errors.New("task and Agent immutable identities are required for a harness v1 binding")
	}
	if agent.Spec.Runtime == nil {
		return nil, permanentHarnessV1Candidate(errors.New("agent runtime configuration is required for a harness v1 binding"))
	}
	if err := validateNewHarnessV1Workload(task, agent); err != nil {
		return nil, permanentHarnessV1Candidate(err)
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if reader == nil {
		return nil, errors.New("API reader is required for harness v1 binding")
	}

	namespace := &corev1.Namespace{}
	if err := reader.Get(ctx, types.NamespacedName{Name: task.Namespace}, namespace); err != nil {
		return nil, fmt.Errorf("resolve Task namespace identity: %w", err)
	}
	policy, policyDigest, err := r.resolveHarnessV1Policy(ctx, reader, task, agent)
	if err != nil {
		return nil, err
	}
	target, err := r.resolveHarnessV1Target(ctx, reader, task, agent)
	if err != nil {
		return nil, err
	}
	workspace, err := resolveHarnessV1PublicReadOnlyWorkspace(task, agent, policy, target)
	if err != nil {
		return nil, permanentHarnessV1Candidate(err)
	}
	runtimeAuthOnly, err := validateHarnessV1RuntimeAuthOnly(task, agent, target)
	if err != nil {
		return nil, err
	}
	systemPrompt, err := resolveACPSystemPrompt(ctx, reader, agent)
	if err != nil {
		if isPermanentACPAgentConfigurationError(err) {
			return nil, permanentHarnessV1Candidate(err)
		}
		return nil, err
	}
	sessionPrompt, sessionBootstrap, err := r.resolveHarnessV1SessionInput(ctx, task)
	if err != nil {
		return nil, fmt.Errorf("resolve frozen harness v1 Session input: %w", err)
	}

	credentialRefs, err := resolveHarnessV1CredentialRefs(ctx, reader, agent, target, runtimeAuthOnly)
	if err != nil {
		return nil, err
	}
	if runtimeAuthOnly && len(credentialRefs) == 0 {
		return nil, permanentHarnessV1Candidate(errors.New("harness v1 runtime-auth-only binding requires frozen provider credentials"))
	}
	maxTurns := int32(50)
	if agent.Spec.Runtime.DefaultMaxTurns != nil {
		maxTurns = *agent.Spec.Runtime.DefaultMaxTurns
	}
	if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.MaxTurns != nil {
		maxTurns = *task.Spec.AgentRuntime.MaxTurns
	}
	model := ""
	provider := ""
	if agent.Spec.Model != nil {
		model = strings.TrimSpace(agent.Spec.Model.Name)
		provider = strings.TrimSpace(agent.Spec.Model.Provider)
	}
	if provider == "" {
		provider = target.runtimeName
	}
	duplicateSafe := policy.Spec.RetryEligibility == corev1alpha1.AgentExecutionRetryDuplicateSafeOnly &&
		target.backend == corev1alpha1.AgentExecutionBackendHarnessWrapper && len(credentialRefs) == 0
	if task.Spec.RetryPolicy != nil && task.Spec.RetryPolicy.MaxRetries > 0 && !duplicateSafe {
		return nil, permanentHarnessV1Candidate(errors.New("harness v1 retry requires a duplicate-safe built-in workload without provider credentials"))
	}
	body := agentExecutionSnapshotBody{
		SchemaVersion:   store.AgentExecutionSnapshotSchemaVersion,
		ContractVersion: string(corev1alpha1.AgentRuntimeContractHarnessV1),
		Backend:         string(target.backend),
		RuntimeType:     string(agent.Spec.Runtime.Type),
		Agent: agentExecutionSnapshotAgent{
			Namespace: agent.Namespace, Name: agent.Name, UID: string(agent.UID), Generation: agent.Generation,
		},
		Configuration: agentExecutionSnapshotConfig{
			AgentUID: string(agent.UID), AgentGeneration: agent.Generation,
			ProviderKind: provider, Model: model, MaxTurns: maxTurns,
			ReasoningEffort: strings.TrimSpace(agent.Spec.Runtime.DefaultReasoningEffort),
			SystemPrompt:    systemPrompt,
		},
		Prompt:          sessionPrompt,
		RetryPolicy:     task.Spec.RetryPolicy.DeepCopy(),
		SessionRef:      task.Spec.SessionRef.DeepCopy(),
		Workspace:       workspace,
		RuntimeOverride: task.Spec.AgentRuntime.DeepCopy(),
		HarnessV1: &agentExecutionSnapshotHarnessV1{
			Endpoint: target.endpoint, Backend: string(target.backend), RuntimeName: target.runtimeName,
			RuntimeAuthOnly:     runtimeAuthOnly,
			AuthSecretNamespace: target.authSecret.Namespace, AuthSecretName: target.authSecret.Name,
			AuthSecretKey: target.authSecretKey, AuthSecretUID: string(target.authSecret.UID),
			AuthSecretResourceVersion: target.authSecret.ResourceVersion,
			DuplicateSafe:             duplicateSafe,
			SessionName:               harnessV1SessionName(task), SessionBootstrap: sessionBootstrap,
			CredentialRefs: credentialRefs,
		},
	}
	if task.Spec.Timeout != nil {
		body.Timeout = task.Spec.Timeout.Duration.String()
	}
	if agent.Spec.Runtime.DefaultAllowedTools != nil || agent.Spec.Runtime.DefaultAllowBash != nil {
		body.DefaultTools = &agentExecutionSnapshotToolPolicy{
			AllowedToolsOmitted: agent.Spec.Runtime.DefaultAllowedTools == nil,
			AllowedTools:        append([]string(nil), agent.Spec.Runtime.DefaultAllowedTools...),
			AllowBash:           agent.Spec.Runtime.DefaultAllowBash,
		}
	}
	encoded, err := canonicalAgentExecutionSnapshotBody(body)
	if err != nil {
		return nil, err
	}
	snapshotDigest := store.CanonicalAgentExecutionSnapshotDigest(encoded)
	binding := corev1alpha1.AgentExecutionBinding{
		SchemaVersion: 1, Mode: corev1alpha1.AgentExecutionBindingModeExecute,
		ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV1,
		Backend:         target.backend, Provenance: corev1alpha1.AgentExecutionProvenanceNewlyBound,
		Task: corev1alpha1.AgentExecutionBindingTaskRef{
			NamespaceUID: namespace.UID, UID: task.UID, BoundSpecGeneration: task.Generation,
		},
		Policy: &corev1alpha1.AgentExecutionPolicyRef{
			Name: policy.Name, UID: policy.UID, Generation: policy.Generation, Digest: policyDigest,
		},
		Agent: &corev1alpha1.AgentExecutionAgentRef{
			Namespace: agent.Namespace, Name: agent.Name, UID: agent.UID, Generation: agent.Generation,
		},
		Snapshot: corev1alpha1.AgentExecutionSnapshotRef{
			ID: string(task.UID) + "/" + snapshotDigest, Digest: snapshotDigest,
			SchemaVersion: store.AgentExecutionSnapshotSchemaVersion,
		},
		RuntimeType: agent.Spec.Runtime.Type,
	}
	if target.runtimeRef != nil {
		binding.RuntimeType = ""
		binding.RuntimeRef = &corev1alpha1.AgentExecutionRuntimeRef{
			Name: target.runtimeRef.Name, UID: target.runtimeRef.UID, Generation: target.runtimeRef.Generation,
		}
	}
	binding.BackendControl, err = r.resolveAgentExecutionBackendControlFor(ctx, reader, store.AgentExecutionBackendV1)
	if err != nil {
		return nil, err
	}
	binding.BindingDigest, err = canonicalAgentExecutionBindingDigest(binding)
	if err != nil {
		return nil, err
	}
	binding.BoundAt = metav1.Now()
	return &agentExecutionCandidate{binding: binding, snapshotBody: encoded}, nil
}

func validateHarnessV1RuntimeAuthOnly(
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	target resolvedHarnessV1Target,
) (bool, error) {
	if !taskRequestsRuntimeAuthOnly(task) {
		return false, nil
	}
	if target.backend != corev1alpha1.AgentExecutionBackendHarnessWrapper || target.runtimeRef != nil {
		return false, permanentHarnessV1Candidate(errors.New("harness v1 runtime-auth-only binding requires the built-in wrapper"))
	}
	switch agent.Spec.Runtime.Type {
	case corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude:
		return true, nil
	default:
		return false, permanentHarnessV1Candidate(fmt.Errorf(
			"harness v1 runtime-auth-only binding does not support runtime %q",
			agent.Spec.Runtime.Type,
		))
	}
}

func validateNewHarnessV1Workload(task *corev1alpha1.Task, agent *corev1alpha1.Agent) error {
	if task.Spec.Transaction != nil {
		return errors.New("harness v1 agent Tasks do not accept transaction tokens")
	}
	if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.Workspace != nil {
		return errors.New("new harness v1 bindings do not accept the legacy agentRuntime.workspace surface")
	}
	if task.Spec.SecretRef != nil || len(task.Spec.Env) != 0 {
		return errors.New("new harness v1 bindings reject arbitrary Task Secret and env delivery")
	}
	if task.Spec.PriorTaskRef != nil || task.Spec.Execution != nil || len(task.Spec.Resources.Requests) != 0 || len(task.Spec.Resources.Limits) != 0 {
		return errors.New("new harness v1 bindings reject prior-task, placement, and custom resource authority")
	}
	if len(agent.Spec.Skills) != 0 {
		return errors.New("new harness v1 bindings reject unresolved Agent skills")
	}
	for _, tool := range agent.Spec.Tools {
		if tool.Enabled == nil || *tool.Enabled {
			return errors.New("new harness v1 pure-prompt bindings reject enabled Agent tools")
		}
	}
	if agent.Spec.Coordination != nil && agent.Spec.Coordination.Enabled {
		return errors.New("new harness v1 pure-prompt bindings reject agent coordination")
	}
	if agent.Spec.ProviderRef != nil {
		return errors.New("new harness v1 bindings do not resolve mutable Provider references")
	}
	if agent.Spec.Model != nil && len(agent.Spec.Model.Fallbacks) != 0 {
		return errors.New("new harness v1 bindings reject model fallbacks")
	}
	switch agent.Spec.Runtime.Type {
	case corev1alpha1.AgentRuntimeCopilot:
		return errors.New("new harness v1 Copilot bindings are prohibited because provider authentication requires a GitHub mutation-capable credential")
	case corev1alpha1.AgentRuntimeOpencode:
		return errors.New("new harness v1 OpenCode bindings are prohibited")
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
		return errors.New("new harness v1 pure-prompt bindings require an explicit empty allowedTools list and allowBash=false")
	}
	return nil
}

func (r *TaskReconciler) resolveHarnessV1Policy(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) (*corev1alpha1.AgentExecutionPolicy, string, error) {
	name := strings.TrimSpace(r.HarnessV1PolicyName)
	if name == "" {
		name = defaultHarnessV1PolicyName
	}
	policy := &corev1alpha1.AgentExecutionPolicy{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, policy); err != nil {
		return nil, "", fmt.Errorf("read harness v1 compatibility policy %s/%s: %w", task.Namespace, name, err)
	}
	if policy.UID == "" || policy.Generation < 1 {
		return nil, "", errors.New("harness v1 compatibility policy identity is not ready")
	}
	if !policy.Spec.AllowNewV1Bindings {
		return nil, "", permanentHarnessV1Candidate(errors.New("harness v1 compatibility policy does not authorize new bindings"))
	}
	if agent.Spec.Runtime.RuntimeRef == nil {
		if !slices.Contains(policy.Spec.AllowedBuiltInRuntimeTypes, agent.Spec.Runtime.Type) {
			return nil, "", permanentHarnessV1Candidate(fmt.Errorf("harness v1 policy does not allow built-in runtime %q", agent.Spec.Runtime.Type))
		}
	} else if !policy.Spec.AllowTrustedObservedModeRuntimes {
		return nil, "", permanentHarnessV1Candidate(errors.New("harness v1 policy does not allow trusted observed-mode external runtimes"))
	}
	mandatory := []corev1alpha1.AgentExecutionProhibitedField{
		corev1alpha1.AgentExecutionProhibitWorkspaceCredentials,
		corev1alpha1.AgentExecutionProhibitForgeCredentials,
		corev1alpha1.AgentExecutionProhibitDirectPublication,
		corev1alpha1.AgentExecutionProhibitTransactionTokens,
	}
	for _, field := range mandatory {
		if !slices.Contains(policy.Spec.ProhibitedFields, field) {
			return nil, "", permanentHarnessV1Candidate(fmt.Errorf("harness v1 policy is missing mandatory prohibition %q", field))
		}
	}
	digest, err := store.CanonicalAgentExecutionPolicyDigest(policy.Spec)
	if err != nil {
		return nil, "", err
	}
	return policy, digest, nil
}

// validateLiveHarnessV1PolicyBinding revalidates the exact compatibility
// policy frozen into a prospective binding. The initial candidate read is not
// an admission fence: an administrator may revoke or replace the policy while
// snapshot and reservation persistence is in flight.
func (r *TaskReconciler) validateLiveHarnessV1PolicyBinding(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) error {
	if task == nil || binding == nil || binding.Policy == nil ||
		binding.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV1 ||
		binding.Mode != corev1alpha1.AgentExecutionBindingModeExecute {
		return permanentHarnessV1Candidate(errors.New(
			"new harness v1 execution binding requires an exact compatibility policy reference",
		))
	}
	if reader == nil {
		return errors.New("API reader is required to revalidate the harness v1 compatibility policy")
	}
	ref := binding.Policy
	if strings.TrimSpace(ref.Name) == "" || ref.UID == "" || ref.Generation < 1 ||
		store.ValidateCanonicalDigest("harness v1 compatibility policy digest", ref.Digest) != nil {
		return permanentHarnessV1Candidate(errors.New(
			"harness v1 execution binding carries an incomplete compatibility policy identity",
		))
	}
	policy := &corev1alpha1.AgentExecutionPolicy{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: ref.Name}, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return permanentHarnessV1Candidate(errors.New(
				"harness v1 compatibility policy was removed before binding admission",
			))
		}
		return fmt.Errorf("revalidate harness v1 compatibility policy: %w", err)
	}
	digest, err := store.CanonicalAgentExecutionPolicyDigest(policy.Spec)
	if err != nil {
		return fmt.Errorf("digest live harness v1 compatibility policy: %w", err)
	}
	if policy.UID != ref.UID || policy.Generation != ref.Generation || digest != ref.Digest ||
		!policy.Spec.AllowNewV1Bindings {
		return permanentHarnessV1Candidate(errors.New(
			"harness v1 compatibility policy changed or revoked before binding admission",
		))
	}
	return nil
}

func (r *TaskReconciler) resolveHarnessV1Target(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) (resolvedHarnessV1Target, error) {
	runtimeRef := agent.Spec.Runtime.RuntimeRef
	if runtimeRef == nil || strings.TrimSpace(runtimeRef.Name) == "" {
		endpoint := strings.TrimSpace(r.HarnessV1Endpoint)
		namespace := strings.TrimSpace(r.HarnessV1AuthSecretNamespace)
		name := strings.TrimSpace(r.HarnessV1AuthSecretName)
		key := strings.TrimSpace(r.HarnessV1AuthSecretKey)
		if endpoint == "" || namespace == "" || name == "" || key == "" {
			return resolvedHarnessV1Target{}, errors.New("built-in harness v1 endpoint and auth Secret coordinates are required")
		}
		if _, err := harness.NewClient(endpoint); err != nil {
			return resolvedHarnessV1Target{}, fmt.Errorf("validate built-in harness v1 endpoint: %w", err)
		}
		secret := &corev1.Secret{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
			return resolvedHarnessV1Target{}, fmt.Errorf("read built-in harness v1 auth Secret: %w", err)
		}
		if secret.UID == "" || secret.ResourceVersion == "" || len(secret.Data[key]) == 0 {
			return resolvedHarnessV1Target{}, errors.New("built-in harness v1 auth Secret identity/key is incomplete")
		}
		return resolvedHarnessV1Target{
			endpoint: endpoint, backend: corev1alpha1.AgentExecutionBackendHarnessWrapper,
			runtimeName: string(agent.Spec.Runtime.Type), authSecret: secret, authSecretKey: key,
		}, nil
	}

	runtime := &corev1alpha1.AgentRuntime{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: strings.TrimSpace(runtimeRef.Name)}, runtime); err != nil {
		return resolvedHarnessV1Target{}, fmt.Errorf("read harness v1 AgentRuntime: %w", err)
	}
	if runtime.RegisteredContractVersion() != corev1alpha1.AgentRuntimeContractHarnessV1 ||
		runtime.Spec.Deployment.Mode != corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint ||
		!runtime.Status.Ready || runtime.Status.ObservedGeneration != runtime.Generation {
		return resolvedHarnessV1Target{}, errors.New("harness v1 AgentRuntime is not current-generation Ready with the exact v1 contract")
	}
	if !supportsHarnessV1ObservedToolExecution(runtime) {
		return resolvedHarnessV1Target{}, errors.New("harness v1 AgentRuntime does not advertise the required observed tool execution mode")
	}
	if err := validateHarnessV1AgentRuntimeEndpointSpec(runtime.Spec.Deployment.Endpoint); err != nil {
		return resolvedHarnessV1Target{}, fmt.Errorf("validate harness v1 AgentRuntime endpoint: %w", err)
	}
	ref := runtime.Spec.ClientAuth.BearerAuthRef
	if ref == nil || strings.TrimSpace(ref.Name) == "" || strings.TrimSpace(ref.Key) == "" {
		return resolvedHarnessV1Target{}, errors.New("harness v1 AgentRuntime bearer auth reference is required")
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: runtime.Namespace, Name: ref.Name}, secret); err != nil {
		return resolvedHarnessV1Target{}, fmt.Errorf("read harness v1 AgentRuntime auth Secret: %w", err)
	}
	if err := validateAgentRuntimeAuthSecretUse(runtime.Name, runtime.Spec.Deployment.Endpoint, secret); err != nil {
		return resolvedHarnessV1Target{}, err
	}
	if secret.UID == "" || secret.ResourceVersion == "" || len(secret.Data[ref.Key]) == 0 ||
		runtime.Status.ObservedAuthRefResourceVersion != secret.ResourceVersion {
		return resolvedHarnessV1Target{}, errors.New("harness v1 AgentRuntime auth Secret identity does not match its current readiness observation")
	}
	runtimeName := runtime.Name
	if runtime.Status.ObservedCapabilities != nil && strings.TrimSpace(runtime.Status.ObservedCapabilities.RuntimeName) != "" {
		runtimeName = strings.TrimSpace(runtime.Status.ObservedCapabilities.RuntimeName)
	}
	return resolvedHarnessV1Target{
		endpoint: strings.TrimSpace(runtime.Spec.Deployment.Endpoint),
		backend:  corev1alpha1.AgentExecutionBackendExternalEndpoint, runtimeName: runtimeName,
		authSecret: secret, authSecretKey: strings.TrimSpace(ref.Key), runtimeRef: runtime,
	}, nil
}

func supportsHarnessV1ObservedToolExecution(runtime *corev1alpha1.AgentRuntime) bool {
	capabilities := runtime.Status.ObservedCapabilities
	return capabilities != nil && slices.Contains(
		capabilities.ToolExecutionModes,
		corev1alpha1.AgentRuntimeToolExecutionModeObserved,
	)
}

func resolveHarnessV1CredentialRefs(
	ctx context.Context,
	reader client.Reader,
	agent *corev1alpha1.Agent,
	target resolvedHarnessV1Target,
	runtimeAuthOnly bool,
) ([]agentExecutionSnapshotSecretRef, error) {
	if target.runtimeRef != nil || agent.Spec.SecretRef == nil || strings.TrimSpace(agent.Spec.SecretRef.Name) == "" {
		return nil, nil
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: agent.Spec.SecretRef.Name}, secret); err != nil {
		return nil, fmt.Errorf("read harness v1 provider credential Secret: %w", err)
	}
	if secret.UID == "" || secret.ResourceVersion == "" {
		return nil, errors.New("harness v1 provider credential Secret identity is incomplete")
	}
	if runtimeAuthOnly && agent.Spec.Runtime.Type == corev1alpha1.AgentRuntimeClaude &&
		harnessV1ClaudeFoundryConfigured(secret.Data) {
		return nil, permanentHarnessV1Candidate(errors.New(
			"harness v1 runtime-auth-only Claude binding does not support Azure AI Foundry credentials",
		))
	}
	allowedKeys, err := readOnlyAgentRuntimeSecretKeys(agent)
	if err != nil {
		return nil, permanentHarnessV1Candidate(fmt.Errorf("resolve harness v1 provider credential keys: %w", err))
	}
	allowedKeySet := make(map[string]struct{}, len(allowedKeys))
	for _, key := range allowedKeys {
		allowedKeySet[key] = struct{}{}
	}
	keys := make([]string, 0, len(secret.Data))
	for key, value := range secret.Data {
		key = strings.TrimSpace(key)
		if key == "" || len(value) == 0 {
			continue
		}
		if _, ok := allowedKeySet[key]; !ok {
			return nil, permanentHarnessV1Candidate(fmt.Errorf(
				"provider credential Secret key %q is not allowed for harness v1 runtime %q",
				key, agent.Spec.Runtime.Type,
			))
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if len(keys) == 0 {
		return nil, errors.New("harness v1 provider credential Secret has no usable keys")
	}
	return []agentExecutionSnapshotSecretRef{{
		Role: "provider-runtime", Namespace: secret.Namespace, Name: secret.Name,
		UID: string(secret.UID), ResourceVersion: secret.ResourceVersion, Keys: keys,
	}}, nil
}

func harnessV1ClaudeFoundryConfigured(data map[string][]byte) bool {
	if strings.TrimSpace(string(data["ANTHROPIC_FOUNDRY_API_KEY"])) != "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(string(data["CLAUDE_CODE_USE_FOUNDRY"]))) {
	case "1", scheduledRunLabelValue, harnessV1CredentialFlagYes, "on":
		return true
	default:
		return false
	}
}

func harnessV1SessionName(task *corev1alpha1.Task) string {
	if task != nil && task.Spec.SessionRef != nil && strings.TrimSpace(task.Spec.SessionRef.Name) != "" {
		return strings.TrimSpace(task.Spec.SessionRef.Name)
	}
	if task != nil {
		return task.Name
	}
	return defaultHarnessV1SessionName
}

// resolveHarnessV1SessionInput freezes the canonical transcript suffix and the
// exact current user prompt before the binding is persisted. Harness v1 CLI
// adapters start a fresh process for every turn, so dispatch must not depend on
// either mutable transcript reads or provider-native session continuation.
//
//nolint:gocyclo // Freezing spans the control and transcript stores and keeps every cross-store fence in one boundary.
func (r *TaskReconciler) resolveHarnessV1SessionInput(
	ctx context.Context,
	task *corev1alpha1.Task,
) (string, *agentExecutionSnapshotHarnessV1SessionBootstrap, error) {
	if task == nil {
		return "", nil, store.ValidationErrorf("Task is required")
	}
	prompt := task.Spec.Prompt
	ref := task.Spec.SessionRef
	if ref == nil {
		return prompt, nil, nil
	}
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return "", nil, store.ValidationErrorf("harness v1 SessionRef name is required")
	}
	if r.DurableControlStore == nil {
		return "", nil, errors.New("session control store is required")
	}
	requiresTranscript := ref.Append || ref.PromptIncluded
	if r.SessionManager == nil || r.SessionManager.store == nil {
		return "", nil, errors.New("session transcript store is required")
	}
	throughMessageID := strings.TrimSpace(ref.ThroughMessageID)
	if ref.PromptIncluded && throughMessageID == "" {
		return "", nil, store.ValidationErrorf("prompt-included harness v1 Session requires a through-message ID")
	}

	control, err := r.DurableControlStore.GetSessionControl(ctx, task.Namespace, name)
	newSession := false
	if errors.Is(err, store.ErrNotFound) {
		if !ref.Create {
			return "", nil, fmt.Errorf("session %s/%s does not exist and create=false: %w", task.Namespace, name, store.ErrNotFound)
		}
		newSession = true
		uid, uidErr := newACPSessionUID()
		if uidErr != nil {
			return "", nil, fmt.Errorf("generate frozen harness v1 Session UID: %w", uidErr)
		}
		control = &store.SessionControl{
			Namespace: task.Namespace, SessionName: name, SessionUID: uid,
			Availability: store.SessionAvailable,
		}
	} else if err != nil {
		return "", nil, fmt.Errorf("load harness v1 Session control: %w", err)
	} else if err := validateAvailableUnleasedHarnessV1SessionControl(control, task.Namespace, name); err != nil {
		return "", nil, err
	}

	maxMessages := acpSessionReferenceMaxMessages(ref)
	var messages []store.SessionMessage
	if requiresTranscript {
		if throughMessageID != "" {
			messages, err = r.SessionManager.store.LoadTranscriptThrough(
				ctx, task.Namespace, name, throughMessageID, maxMessages,
			)
		} else {
			messages, err = r.SessionManager.store.LoadTranscript(ctx, task.Namespace, name, maxMessages)
		}
		if err != nil && (!newSession || !ref.Create || ref.PromptIncluded || !errors.Is(err, store.ErrNotFound)) {
			return "", nil, fmt.Errorf("load canonical Session transcript: %w", err)
		}
		if errors.Is(err, store.ErrNotFound) {
			messages = nil
		}
	} else if newSession {
		record, getErr := r.SessionManager.store.GetSession(ctx, task.Namespace, name)
		if getErr == nil && record.MessageCount > 0 {
			return "", nil, fmt.Errorf(
				"%w: control-less Session %s/%s has a non-empty transcript",
				store.ErrConflict, task.Namespace, name,
			)
		}
		if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
			return "", nil, fmt.Errorf("inspect new harness v1 Session transcript: %w", getErr)
		}
	}
	if newSession && len(messages) > 0 {
		return "", nil, fmt.Errorf(
			"%w: control-less Session %s/%s has a non-empty transcript",
			store.ErrConflict, task.Namespace, name,
		)
	}
	if throughMessageID != "" && (len(messages) == 0 || messages[len(messages)-1].ID != throughMessageID) {
		return "", nil, fmt.Errorf(
			"%w: bounded harness v1 transcript does not end at message %q",
			store.ErrConflict, throughMessageID,
		)
	}
	totalMessages := len(messages)
	bootstrapMessages := messages
	if ref.PromptIncluded {
		throughMessage := messages[len(messages)-1]
		if throughMessage.Role != acpBootstrapRoleUser || strings.TrimSpace(throughMessage.Content) == "" {
			return "", nil, store.ValidationErrorf(
				"prompt-included harness v1 Session cutoff must end at a non-empty user message",
			)
		}
		prompt = throughMessage.Content
		bootstrapMessages = messages[:len(messages)-1]
	}
	limits, err := harnessV1SessionBootstrapLimits(harnessV1SessionBootstrapSchemaVersion)
	if err != nil {
		return "", nil, err
	}
	bootstrap, err := buildACPBootstrapTranscript(bootstrapMessages, limits)
	if err != nil {
		return "", nil, fmt.Errorf("build bounded harness v1 Session bootstrap: %w", err)
	}
	if newSession {
		if winner, getErr := r.DurableControlStore.GetSessionControl(ctx, task.Namespace, name); getErr == nil {
			return "", nil, fmt.Errorf(
				"%w: session %s/%s was created as UID %q while freezing UID %q",
				store.ErrConflict, task.Namespace, name, winner.SessionUID, control.SessionUID,
			)
		} else if !errors.Is(getErr, store.ErrNotFound) {
			return "", nil, fmt.Errorf("recheck new harness v1 Session control: %w", getErr)
		}
	} else {
		current, getErr := r.DurableControlStore.GetSessionControl(ctx, task.Namespace, name)
		if getErr != nil {
			return "", nil, fmt.Errorf("recheck harness v1 Session control: %w", getErr)
		}
		if err := validateAvailableUnleasedHarnessV1SessionControl(current, task.Namespace, name); err != nil {
			return "", nil, err
		}
		if current.SessionUID != control.SessionUID || current.Version != control.Version ||
			current.LeaseGeneration != control.LeaseGeneration {
			return "", nil, fmt.Errorf("%w: harness v1 Session control changed while freezing its transcript", store.ErrConflict)
		}
	}
	return prompt, &agentExecutionSnapshotHarnessV1SessionBootstrap{
		SchemaVersion: harnessV1SessionBootstrapSchemaVersion,
		SessionUID:    control.SessionUID, ControlVersion: control.Version,
		LeaseGeneration: control.LeaseGeneration,
		Artifact:        string(bootstrap.Artifact), Digest: bootstrap.Digest,
		MessageCount: bootstrap.MessageCount, TotalMessages: totalMessages,
		Truncated: bootstrap.Truncated,
	}, nil
}

func harnessV1SessionBootstrapLimits(schemaVersion int) (ACPBootstrapLimits, error) {
	if schemaVersion != harnessV1SessionBootstrapSchemaVersion {
		return ACPBootstrapLimits{}, store.ValidationErrorf(
			"unsupported harness v1 Session bootstrap schema version %d", schemaVersion,
		)
	}
	return ACPBootstrapLimits{
		MaxMessages:     harnessV1SessionBootstrapMaxMessages,
		MaxBytes:        harnessV1SessionBootstrapMaxBytes,
		MaxMessageBytes: harnessV1SessionBootstrapMaxMessageBytes,
	}, nil
}

func validateAvailableUnleasedHarnessV1SessionControl(
	control *store.SessionControl,
	namespace, name string,
) error {
	if control == nil || control.Namespace != namespace || control.SessionName != name ||
		strings.TrimSpace(control.SessionUID) == "" || control.Version < 1 || control.LeaseGeneration < 0 {
		return fmt.Errorf("%w: harness v1 Session control identity is incomplete", store.ErrConflict)
	}
	if control.Availability != store.SessionAvailable || control.Lease != nil {
		return fmt.Errorf("%w: harness v1 Session %s/%s is not available and unleased", store.ErrConflict, namespace, name)
	}
	return nil
}

//nolint:gocyclo // Snapshot validation intentionally checks every frozen v1 field in one boundary.
func validateHarnessV1Snapshot(
	binding *corev1alpha1.AgentExecutionBinding,
	snapshot *store.AgentExecutionSnapshot,
	body agentExecutionSnapshotBody,
) error {
	if binding == nil || snapshot == nil || body.HarnessV1 == nil {
		return errors.New("harness v1 binding, snapshot, and target metadata are required")
	}
	key := store.AgentExecutionSnapshotKey{TaskUID: string(binding.Task.UID), Digest: binding.Snapshot.Digest}
	if binding.Snapshot.ID != key.ID() || snapshot.TaskUID != key.TaskUID || snapshot.Digest != key.Digest ||
		binding.Snapshot.SchemaVersion != store.AgentExecutionSnapshotSchemaVersion ||
		snapshot.SchemaVersion != binding.Snapshot.SchemaVersion || body.SchemaVersion != binding.Snapshot.SchemaVersion {
		return errors.New("harness v1 execution snapshot identity/schema does not exactly match the binding")
	}
	if store.CanonicalAgentExecutionSnapshotDigest(snapshot.Body) != binding.Snapshot.Digest {
		return errors.New("harness v1 execution snapshot body digest does not match the binding")
	}
	canonical, err := canonicalAgentExecutionSnapshotBody(body)
	if err != nil || !bytes.Equal(canonical, snapshot.Body) {
		return errors.New("harness v1 execution snapshot body is not canonical")
	}
	if binding.SchemaVersion != 1 || binding.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV1 ||
		body.ContractVersion != string(binding.ContractVersion) || body.Backend != string(binding.Backend) ||
		body.HarnessV1.Backend != string(binding.Backend) || binding.Agent == nil ||
		body.Agent.Namespace != binding.Agent.Namespace || body.Agent.Name != binding.Agent.Name ||
		body.Agent.UID != string(binding.Agent.UID) || body.Agent.Generation != binding.Agent.Generation {
		return errors.New("harness v1 snapshot route or Agent identity does not exactly match the binding")
	}
	if _, err := harness.NewClient(body.HarnessV1.Endpoint); err != nil {
		return fmt.Errorf("validate frozen harness v1 endpoint: %w", err)
	}
	if body.HarnessV1.AuthSecretNamespace == "" || body.HarnessV1.AuthSecretName == "" ||
		body.HarnessV1.AuthSecretKey == "" || body.HarnessV1.AuthSecretUID == "" ||
		body.HarnessV1.AuthSecretResourceVersion == "" || body.HarnessV1.SessionName == "" {
		return errors.New("harness v1 snapshot has incomplete frozen endpoint/auth/session identity")
	}
	if err := validateFrozenHarnessV1SessionInput(body); err != nil {
		return err
	}
	if body.HarnessV1.RuntimeAuthOnly {
		if body.HarnessV1.Backend != string(corev1alpha1.AgentExecutionBackendHarnessWrapper) ||
			len(body.HarnessV1.CredentialRefs) == 0 {
			return errors.New("harness v1 runtime-auth-only snapshot is not bound to the built-in wrapper and frozen credentials")
		}
		switch corev1alpha1.AgentRuntimeType(body.RuntimeType) {
		case corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude:
		default:
			return errors.New("harness v1 runtime-auth-only snapshot uses an unsupported runtime")
		}
	}
	if body.Workspace != nil && binding.Policy == nil {
		return errors.New("harness v1 public workspace snapshot is missing its compatibility policy binding")
	}
	if err := validateFrozenHarnessV1PublicReadOnlyWorkspace(body); err != nil {
		return err
	}
	if body.RetryPolicy != nil {
		if body.RetryPolicy.MaxRetries < 0 ||
			(body.RetryPolicy.InitialDelay != nil && body.RetryPolicy.InitialDelay.Duration < 0) {
			return errors.New("harness v1 snapshot has an invalid frozen retry policy")
		}
		if body.RetryPolicy.MaxRetries > 0 && !body.HarnessV1.DuplicateSafe {
			return errors.New("harness v1 snapshot retry policy is not duplicate-safe")
		}
	}
	return nil
}

//nolint:gocyclo // Snapshot validation intentionally checks every frozen Session field in one boundary.
func validateFrozenHarnessV1SessionInput(body agentExecutionSnapshotBody) error {
	if body.HarnessV1 == nil {
		return errors.New("harness v1 target metadata is required")
	}
	ref := body.SessionRef
	bootstrap := body.HarnessV1.SessionBootstrap
	if ref == nil {
		if bootstrap != nil {
			return errors.New("harness v1 snapshot has a Session bootstrap without a SessionRef")
		}
		return nil
	}
	if bootstrap == nil {
		return errors.New("harness v1 snapshot is missing its frozen Session identity/bootstrap")
	}
	if body.HarnessV1.SessionName != strings.TrimSpace(ref.Name) {
		return errors.New("harness v1 snapshot Session name does not match its frozen SessionRef")
	}
	limits, err := harnessV1SessionBootstrapLimits(bootstrap.SchemaVersion)
	if err != nil {
		return err
	}
	if err := store.ValidateControlIdentifier("frozen harness v1 Session UID", bootstrap.SessionUID); err != nil {
		return err
	}
	if bootstrap.ControlVersion < 0 || bootstrap.LeaseGeneration < 0 ||
		(bootstrap.ControlVersion == 0 && bootstrap.LeaseGeneration != 0) {
		return errors.New("harness v1 snapshot has an invalid frozen Session control revision")
	}
	if bootstrap.ControlVersion == 0 && (!ref.Create || bootstrap.MessageCount != 0 ||
		bootstrap.TotalMessages != 0 || bootstrap.Truncated || bootstrap.Artifact != "") {
		return errors.New("new harness v1 Session bootstrap must freeze an empty create-only transcript")
	}
	if strings.TrimSpace(body.Prompt) == "" {
		return errors.New("harness v1 snapshot has an empty frozen Session prompt")
	}
	if ref.PromptIncluded && strings.TrimSpace(ref.ThroughMessageID) == "" {
		return errors.New("prompt-included harness v1 snapshot is missing its through-message ID")
	}
	requiresTranscript := ref.Append || ref.PromptIncluded
	minimumTotal := int(bootstrap.MessageCount)
	if ref.PromptIncluded {
		minimumTotal++
	}
	if (!requiresTranscript && (bootstrap.MessageCount != 0 || bootstrap.TotalMessages != 0 || bootstrap.Truncated)) ||
		bootstrap.TotalMessages < minimumTotal || bootstrap.TotalMessages > acpSessionReferenceMaxMessages(ref) {
		return errors.New("harness v1 snapshot has inconsistent Session bootstrap message counts")
	}
	artifact := []byte(bootstrap.Artifact)
	if bootstrap.MessageCount == 0 {
		if len(artifact) != 0 {
			return errors.New("empty harness v1 Session bootstrap has a non-empty artifact")
		}
	} else if len(artifact) == 0 || artifact[len(artifact)-1] != '\n' {
		return errors.New("harness v1 Session bootstrap artifact is incomplete")
	}
	lines := bytes.Split(artifact, []byte{'\n'})
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) != int(bootstrap.MessageCount) {
		return errors.New("harness v1 Session bootstrap artifact count does not match its metadata")
	}
	messages := make([]store.SessionMessage, 0, len(lines))
	for _, line := range lines {
		canonicalLine := append(append([]byte(nil), line...), '\n')
		message, err := decodeACPBootstrapLine(canonicalLine)
		if err != nil {
			return fmt.Errorf("validate frozen harness v1 Session bootstrap: %w", err)
		}
		messages = append(messages, store.SessionMessage{
			Role: message.Role, Content: message.Content, Name: message.Name, ToolCallID: message.ToolCallID,
		})
	}
	rebuilt, err := buildACPBootstrapTranscript(messages, limits)
	if err != nil {
		return fmt.Errorf("validate frozen harness v1 Session bootstrap bounds: %w", err)
	}
	if !bytes.Equal(rebuilt.Artifact, artifact) || rebuilt.Digest != bootstrap.Digest ||
		rebuilt.MessageCount != bootstrap.MessageCount {
		return errors.New("harness v1 Session bootstrap artifact is not canonical or digest-consistent")
	}
	return nil
}

func (r *TaskReconciler) loadVerifiedHarnessV1Execution(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) (*verifiedHarnessV1Execution, error) {
	return r.loadHarnessV1ExecutionWithOptions(ctx, task, binding, false, false, true, true)
}

// loadVerifiedHarnessV1ExecutionForRecovery performs the same immutable
// binding/snapshot/Secret verification as admission, while permitting the v1
// dispatcher to observe, cancel, and settle already-admitted work after Task
// deletion or after the backend enters disabled mode. Callers must not use the
// relaxed flags to submit a new turn.
//
//nolint:gocyclo // Recovery verification mirrors the complete immutable v1 admission boundary.
func (r *TaskReconciler) loadVerifiedHarnessV1ExecutionForRecovery(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
	allowDeleting bool,
) (*verifiedHarnessV1Execution, error) {
	return r.loadHarnessV1ExecutionWithOptions(ctx, task, binding, allowDeleting, true, true, true)
}

// loadHarnessV1ExecutionForSettlement validates only immutable Task,
// reservation, namespace, binding, and encrypted-snapshot authority. Terminal
// Session/outbox settlement does not read live Agent/configuration objects and
// does not require credentials that are no longer used for executor effects.
func (d *HarnessV1Dispatcher) loadHarnessV1ExecutionForSettlement(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) (*verifiedHarnessV1Execution, error) {
	verifier := TaskReconciler{
		Client: d.Client, APIReader: d.APIReader, AgentExecutionSnapshots: d.Snapshots,
		AgentExecutionBindingReservations: d.BindingReservations,
	}
	return verifier.loadHarnessV1ExecutionWithOptions(ctx, task, binding, true, true, false, false)
}

//nolint:gocyclo // The options separate executor authorization from terminal-only settlement verification.
func (r *TaskReconciler) loadHarnessV1ExecutionWithOptions(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
	allowDeleting bool,
	allowDisabled bool,
	verifyBackendControl bool,
	verifySecrets bool,
) (*verifiedHarnessV1Execution, error) {
	if r.AgentExecutionSnapshots == nil || task == nil || binding == nil {
		return nil, errors.New("task, v1 binding, and encrypted snapshot store are required")
	}
	if err := r.verifyBoundAgentExecutionReservation(ctx, task, binding); err != nil {
		return nil, err
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	current := &corev1alpha1.Task{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, current); err != nil {
		return nil, fmt.Errorf("uncached Task read before harness v1 dispatch: %w", err)
	}
	if current.UID != task.UID || (!allowDeleting && !current.DeletionTimestamp.IsZero()) || current.Generation != binding.Task.BoundSpecGeneration ||
		current.Status.AgentExecutionBinding == nil || current.Status.AgentExecutionBinding.BindingDigest != binding.BindingDigest {
		return nil, errors.New("task identity, generation, deletion state, or persisted harness v1 binding changed")
	}
	canonicalDigest, err := canonicalAgentExecutionBindingDigest(*current.Status.AgentExecutionBinding)
	if err != nil || canonicalDigest != binding.BindingDigest {
		return nil, errors.New("persisted harness v1 binding failed canonical integrity verification")
	}
	namespace := &corev1.Namespace{}
	if err := reader.Get(ctx, types.NamespacedName{Name: current.Namespace}, namespace); err != nil {
		return nil, err
	}
	if namespace.UID == "" || namespace.UID != binding.Task.NamespaceUID || binding.Mode != corev1alpha1.AgentExecutionBindingModeExecute {
		return nil, errors.New("harness v1 binding namespace identity or mode is invalid")
	}
	if binding.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV1 ||
		(binding.Backend != corev1alpha1.AgentExecutionBackendHarnessWrapper && binding.Backend != corev1alpha1.AgentExecutionBackendExternalEndpoint) {
		return nil, errors.New("binding is not dispatchable by HarnessV1Dispatcher")
	}
	if !verifyBackendControl {
		if binding.BackendControl == nil || binding.BackendControl.AdmittedMode != corev1alpha1.AgentExecutionEffectiveModeEnabled {
			return nil, errors.New("harness v1 binding lacks an enabled admission revision")
		}
	} else if allowDisabled {
		if binding.BackendControl == nil || binding.BackendControl.AdmittedMode != corev1alpha1.AgentExecutionEffectiveModeEnabled {
			return nil, errors.New("harness v1 binding lacks an enabled admission revision")
		}
		control := &corev1alpha1.AgentExecutionControl{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: corev1alpha1.AgentExecutionControlNamespace, Name: corev1alpha1.AgentExecutionControlName}, control); err != nil {
			return nil, err
		}
		if control.UID != binding.BackendControl.UID ||
			control.Status.ObservedGeneration < binding.BackendControl.Generation ||
			control.Status.ObservedGeneration > control.Generation ||
			control.Status.Backends == nil ||
			control.Status.Backends.V1.ModeRevision < binding.BackendControl.ModeRevision {
			return nil, errors.New("harness v1 backend control no longer authorizes admitted cleanup recovery")
		}
	} else if err := r.verifyBoundAgentExecutionBackendMode(
		ctx, reader, current, binding, store.AgentExecutionBackendV1,
	); err != nil {
		return nil, err
	}
	snapshot, err := r.AgentExecutionSnapshots.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{
		TaskUID: string(binding.Task.UID), Digest: binding.Snapshot.Digest,
	})
	if err != nil {
		return nil, err
	}
	body, err := decodeAgentExecutionSnapshot(snapshot.Body)
	if err != nil {
		return nil, err
	}
	if err := validateHarnessV1Snapshot(binding, snapshot, body); err != nil {
		return nil, err
	}
	if verifySecrets {
		allRefs := append([]agentExecutionSnapshotSecretRef(nil), body.HarnessV1.CredentialRefs...)
		allRefs = append(allRefs, agentExecutionSnapshotSecretRef{
			Role: "harness-auth", Namespace: body.HarnessV1.AuthSecretNamespace,
			Name: body.HarnessV1.AuthSecretName, UID: body.HarnessV1.AuthSecretUID,
			ResourceVersion: body.HarnessV1.AuthSecretResourceVersion, Keys: []string{body.HarnessV1.AuthSecretKey},
		})
		for _, ref := range allRefs {
			secret := &corev1.Secret{}
			if err := reader.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, secret); err != nil {
				return nil, fmt.Errorf("read frozen %s Secret identity: %w", ref.Role, err)
			}
			if string(secret.UID) != ref.UID || secret.ResourceVersion != ref.ResourceVersion {
				return nil, fmt.Errorf("frozen %s Secret identity or resourceVersion changed", ref.Role)
			}
			for _, key := range ref.Keys {
				if len(secret.Data[key]) == 0 {
					return nil, fmt.Errorf("frozen %s Secret key %q is empty or missing", ref.Role, key)
				}
			}
		}
	}
	return &verifiedHarnessV1Execution{
		binding: binding.DeepCopy(), snapshot: snapshot, body: body,
		frozenTask: frozenTaskFromAgentExecutionSnapshot(current, binding, body),
	}, nil
}

func (r *TaskReconciler) ensureHarnessV1ExecutionBinding(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) (ctrl.Result, error, bool) {
	log := logf.FromContext(ctx)
	if task == nil {
		return ctrl.Result{}, errors.New("task is required for harness v1 execution binding"), true
	}
	if err := r.checkAgentExecutionClassification(ctx); err != nil {
		return ctrl.Result{RequeueAfter: time.Second}, nil, true
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if task.Status.AgentExecutionBinding == nil {
		current := &corev1alpha1.Task{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, current); err != nil {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil, true
		}
		if current.UID != task.UID {
			return ctrl.Result{}, errors.New("task UID changed before harness v1 binding"), true
		}
		task = current
	}
	if existing := task.Status.AgentExecutionBinding; existing != nil {
		if err := r.recoverBoundAgentExecutionReservation(ctx, task, existing); err != nil {
			log.Error(err, "harness v1 binding reservation recovery failed")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
		}
		if _, err := r.loadVerifiedHarnessV1Execution(ctx, task, existing); err != nil {
			log.Error(err, "harness v1 bound execution verification failed")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
		}
		return ctrl.Result{}, nil, false
	}

	candidate, err := r.resolveHarnessV1ExecutionCandidate(ctx, task, agent)
	if err != nil {
		if isPermanentHarnessV1CandidateError(err) {
			result, failErr := r.failTask(ctx, task, err.Error())
			return result, failErr, true
		}
		log.Error(err, "harness v1 execution candidate resolution failed")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
	}
	if err := r.persistAgentExecutionSnapshot(ctx, task, candidate); err != nil {
		log.Error(err, "harness v1 immutable snapshot persistence failed")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
	}
	reservation, err := r.createAgentExecutionBindingReservation(ctx, task, &candidate.binding)
	if err != nil {
		log.Error(err, "harness v1 binding reservation failed")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
	}
	binding, err := r.persistAgentExecutionBinding(ctx, task, candidate)
	if err != nil {
		conflict := &errAgentExecutionBindingConflict{}
		if errors.As(err, &conflict) {
			if settleErr := r.settleAgentExecutionBindingReservation(
				ctx, reservation, store.AgentExecutionBindingReservationRejected, "binding-conflict",
			); settleErr != nil {
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil, true
			}
			result, failErr := r.failTask(ctx, task, err.Error())
			return result, failErr, true
		}
		if isPermanentHarnessV1CandidateError(err) {
			if settleErr := r.settleAgentExecutionBindingReservation(
				ctx, reservation, store.AgentExecutionBindingReservationRejected, "policy-invalidated",
			); settleErr != nil {
				log.Error(settleErr, "reject harness v1 binding reservation after policy invalidation")
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil, true
			}
			result, failErr := r.failTask(ctx, task, err.Error())
			return result, failErr, true
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil, true
	}
	task.Status.AgentExecutionBinding = binding
	if err := r.settleAgentExecutionBindingReservation(
		ctx, reservation, store.AgentExecutionBindingReservationBound, "",
	); err != nil {
		log.Error(err, "settle harness v1 binding reservation")
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil, true
	}
	if _, err := r.loadVerifiedHarnessV1Execution(ctx, task, binding); err != nil {
		log.Error(err, "verify harness v1 binding after persistence")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
	}
	return ctrl.Result{}, nil, false
}

func (r *TaskReconciler) queueHarnessV1Task(
	ctx context.Context,
	task *corev1alpha1.Task,
) (ctrl.Result, error) {
	if err := r.checkAgentExecutionClassification(ctx); err != nil {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if r.HarnessV1Attempts == nil || r.ControllerEpochManager == nil {
		return r.failTask(ctx, task, "durable harness v1 attempt store and controller epoch manager are required")
	}
	binding := task.Status.AgentExecutionBinding
	verified, err := r.loadVerifiedHarnessV1Execution(ctx, task, binding)
	if err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	existing, err := r.HarnessV1Attempts.ListHarnessV1AttemptsByTask(ctx, task.Namespace, string(task.UID))
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(existing) != 0 {
		latest := existing[len(existing)-1]
		if latest.BindingDigest != binding.BindingDigest || latest.SnapshotDigest != binding.Snapshot.Digest {
			return r.failTask(ctx, task, "durable harness v1 attempt does not match the immutable binding")
		}
		if task.Status.Phase != corev1alpha1.TaskPhaseRunning && !store.IsTerminalHarnessV1AttemptState(latest.State) {
			if err := r.patchHarnessV1QueuedStatus(ctx, task, verified, &latest); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	const attemptNumber int32 = 1
	turnID := harnessV1TurnID(task, attemptNumber)
	runtimeSessionID := harness.ResolveRuntimeSessionIdentity(harness.RuntimeSessionIdentityInput{
		Namespace: task.Namespace, TaskName: task.Name, TaskUID: string(task.UID),
		SessionName: func() string {
			if task.Spec.SessionRef != nil {
				return strings.TrimSpace(task.Spec.SessionRef.Name)
			}
			return ""
		}(),
		RuntimeName: verified.body.HarnessV1.RuntimeName, ActiveTask: task.Name,
		AgentName: verified.body.Agent.Name, Provider: harness.ProviderKindKubernetesService,
	}).ID
	fence, err := r.ControllerEpochManager.CurrentFence(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	retryClass := store.HarnessV1RetryClassNone
	if verified.body.HarnessV1.DuplicateSafe {
		retryClass = store.HarnessV1RetryClassDuplicateSafe
	}
	attempt := &store.HarnessV1Attempt{
		Namespace: task.Namespace, TaskName: task.Name, TaskUID: string(task.UID), Attempt: attemptNumber,
		BindingDigest: binding.BindingDigest, SnapshotDigest: binding.Snapshot.Digest,
		TurnID: string(turnID), RuntimeSessionID: string(runtimeSessionID),
		CorrelationID: string(task.UID), Backend: string(binding.Backend),
		BackendEndpoint:           verified.body.HarnessV1.Endpoint,
		AuthSecretNamespace:       verified.body.HarnessV1.AuthSecretNamespace,
		AuthSecretName:            verified.body.HarnessV1.AuthSecretName,
		AuthSecretKey:             verified.body.HarnessV1.AuthSecretKey,
		AuthSecretUID:             verified.body.HarnessV1.AuthSecretUID,
		AuthSecretResourceVersion: verified.body.HarnessV1.AuthSecretResourceVersion,
		State:                     store.HarnessV1AttemptPrepared, DuplicateSafe: verified.body.HarnessV1.DuplicateSafe,
		RetryClass: retryClass, ControllerEpochName: fence.Name, ControllerEpoch: fence.Epoch,
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	env, err := resolveFrozenHarnessV1Env(ctx, reader, verified.body.HarnessV1.CredentialRefs)
	if err != nil {
		return ctrl.Result{}, err
	}
	request, err := buildHarnessV1StartTurnRequest(task, verified, attempt, env)
	if err != nil {
		return ctrl.Result{}, err
	}
	attempt.RequestDigest, err = harness.CanonicalStartTurnRequestDigest(request)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.HarnessV1Attempts.CreateHarnessV1Attempt(ctx, attempt, fence); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.patchHarnessV1QueuedStatus(ctx, task, verified, attempt); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func (r *TaskReconciler) patchHarnessV1QueuedStatus(
	ctx context.Context,
	task *corev1alpha1.Task,
	verified *verifiedHarnessV1Execution,
	attempt *store.HarnessV1Attempt,
) error {
	if attempt == nil {
		return errors.New("harness v1 queued attempt is required")
	}
	now := metav1.Now()
	target := verified.body.HarnessV1
	return r.updateStatusWithRetry(ctx, task, func(current *corev1alpha1.Task) {
		if current.UID != task.UID || current.Status.AgentExecutionBinding == nil ||
			current.Status.AgentExecutionBinding.BindingDigest != verified.binding.BindingDigest ||
			(current.Status.Phase != corev1alpha1.TaskPhasePending && current.Status.Phase != corev1alpha1.TaskPhaseRunning) {
			return
		}
		current.Status.Phase = corev1alpha1.TaskPhaseRunning
		if current.Status.StartTime == nil {
			current.Status.StartTime = &now
		}
		current.Status.Attempts = attempt.Attempt
		current.Status.JobName = ""
		current.Status.Message = "queued for harness v1 dispatcher"
		current.Status.HarnessRuntime = &corev1alpha1.HarnessRuntimeStatus{
			RuntimeName: target.RuntimeName, ContractVersion: harness.ProtocolVersion,
			Endpoint: target.Endpoint, AuthRefName: target.AuthSecretName,
			AuthRefField: target.AuthSecretKey, AuthRefResourceVersion: target.AuthSecretResourceVersion,
			Attempt: attempt.Attempt, TurnID: attempt.TurnID, RuntimeSessionID: attempt.RuntimeSessionID,
			State: corev1alpha1.TaskExecutionStateQueued, RequestDigest: attempt.RequestDigest,
			ControllerEpoch: attempt.ControllerEpoch, Message: "queued for harness v1 dispatcher",
			LastTransitionTime: &now,
		}
		if verified.binding.RuntimeRef != nil {
			current.Status.HarnessRuntime.RuntimeRefName = verified.binding.RuntimeRef.Name
			current.Status.HarnessRuntime.RuntimeGeneration = verified.binding.RuntimeRef.Generation
		}
	})
}

func harnessV1TurnID(task *corev1alpha1.Task, attempt int32) harness.HarnessTurnID {
	identity := task.Namespace + "/" + task.Name + "/" + string(task.UID) + "/" + strconv.FormatInt(int64(attempt), 10)
	sum := sha256.Sum256([]byte(identity))
	prefix := strings.ToLower(strings.Trim(task.Name, "-_."))
	if prefix == "" {
		prefix = "turn"
	}
	if len(prefix) > 40 {
		prefix = prefix[:40]
	}
	return harness.HarnessTurnID(fmt.Sprintf("%s-%s-%d", prefix, hex.EncodeToString(sum[:])[:12], attempt))
}
