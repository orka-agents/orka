package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/worker"
	"github.com/orka-agents/orka/internal/workerenv"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type nativeRemoteToolsKey struct{}

type nativeRemoteTools struct {
	executor *worker.ToolExecutor
	task     *corev1alpha1.Task
	agent    *corev1alpha1.Agent
	tools    map[string]*corev1alpha1.Tool
}

// prepareNativeRemoteTools is a fail-closed phase before any model exposure.
// Its separate executor leaves legacy backend authority/defaults untouched.
func prepareNativeRemoteTools(
	ctx context.Context,
	reader client.Client,
	namespace, taskName string,
	enabled []string,
	loaded map[string]*corev1alpha1.Tool,
	executor *worker.ToolExecutor,
) (context.Context, error) {
	state := &nativeRemoteTools{executor: executor, tools: map[string]*corev1alpha1.Tool{}}
	for _, name := range enabled {
		knownRemote := aitools.IsRemoteMCP(loaded[name])
		if loaded[name] != nil && !knownRemote {
			continue
		}
		if reader == nil {
			if knownRemote {
				return ctx, errors.New("remote MCP definition reader is unavailable")
			}
			continue
		}
		live := &corev1alpha1.Tool{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, live); err != nil {
			// Only known remote definitions make rereads mandatory. Built-in
			// collision checks and unresolved legacy selections remain best-effort.
			if !knownRemote {
				continue
			}
			return ctx, fmt.Errorf("cannot verify enabled remote Tool %q", name)
		}
		if !aitools.IsRemoteMCP(live) && !aitools.IsRemoteMCP(loaded[name]) {
			continue
		}
		if _, collision := tools.DefaultRegistry.Get(name); collision {
			return ctx, fmt.Errorf("remote MCP alias %q collides with a built-in tool", name)
		}
		if !sameNativeRemoteTool(loaded[name], live) {
			return ctx, fmt.Errorf("remote MCP Tool %q was not loaded unchanged", name)
		}
		if err := aitools.ValidateRemoteMCPConfiguration(live); err != nil {
			return ctx, err
		}
		state.tools[name] = loaded[name]
	}
	if len(state.tools) == 0 {
		return ctx, nil
	}
	if reader == nil || taskName == "" || executor == nil {
		return ctx, errors.New("remote MCP requires native Task execution context")
	}
	task, err := loadNativeRemoteLaunchTask(ctx, reader, namespace, taskName)
	if err != nil {
		return ctx, err
	}
	if err := aitools.ValidateRemoteMCPAgentReference(task); err != nil {
		return ctx, err
	}
	agent := &corev1alpha1.Agent{}
	agentKey := client.ObjectKey{Namespace: task.Namespace, Name: task.Spec.AgentRef.Name}
	if err := reader.Get(ctx, agentKey, agent); err != nil {
		return ctx, errors.New("remote MCP Agent is unavailable")
	}
	if agent.UID == "" {
		return ctx, errors.New("remote MCP Agent identity is unavailable")
	}
	for _, tool := range state.tools {
		if err := aitools.ValidateRemoteMCPSelection(task, agent, tool); err != nil {
			return ctx, err
		}
	}
	if err := bindNativeRemoteAuthority(task, executor); err != nil {
		return ctx, err
	}
	state.task = task.DeepCopy()
	state.agent = agent.DeepCopy()
	// Kubernetes UID/resource versions cannot revert to a bound version after
	// mutation. Recheck after resolution, while the actual transport is frozen,
	// so discovery cannot send credentials through a newly resolved binding.
	executor.SetRemoteMCPPreparationFence(func(ctx context.Context, tool *corev1alpha1.Tool) error {
		return state.validate(ctx, reader, tool)
	})
	for name, tool := range state.tools {
		if err := bindNativeRemoteDependencies(ctx, reader, tool, task.Spec.Transaction.Context["secret"], nil); err != nil {
			return ctx, err
		}
		state.tools[name] = tool.DeepCopy()
		if err := executor.VerifyRemoteMCPTool(ctx, tool); err != nil {
			return ctx, fmt.Errorf("verify remote MCP Tool %q before exposure: %w", name, err)
		}
	}
	ctx = context.WithValue(ctx, nativeRemoteToolsKey{}, state)
	// Recheck after network discovery: a concurrent edit must not become the
	// meaning of the already prepared model definition.
	for _, tool := range state.tools {
		if err := state.validate(ctx, reader, tool); err != nil {
			return ctx, err
		}
	}
	return ctx, nil
}

func loadNativeRemoteLaunchTask(
	ctx context.Context, reader client.Client, namespace, taskName string,
) (*corev1alpha1.Task, error) {
	task := &corev1alpha1.Task{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: taskName}, task); err != nil {
		return nil, errors.New("remote MCP Task is unavailable")
	}
	launchUID := os.Getenv(workerenv.TaskUID)
	if launchUID == "" || string(task.UID) != launchUID {
		return nil, errors.New("remote MCP Task UID does not match the controller-issued launch identity")
	}
	if !task.DeletionTimestamp.IsZero() || task.Spec.AgentRef == nil {
		return nil, errors.New("remote MCP requires a live Task with a referenced native Agent")
	}
	return task, nil
}

func bindNativeRemoteAuthority(task *corev1alpha1.Task, executor *worker.ToolExecutor) error {
	tx := task.Spec.Transaction
	if tx == nil {
		return errors.New("remote MCP requires Task transaction metadata")
	}
	if namespace := strings.TrimSpace(tx.Context["namespace"]); namespace != "" && namespace != task.Namespace {
		return errors.New("remote MCP Task namespace does not match transaction authority")
	}
	scopes := append([]string(nil), tx.Scopes...)
	scopes = append(scopes, strings.Fields(tx.Scope)...)
	required := workerenv.SplitCSV(os.Getenv(workerenv.TransactionCredentialReadScopes))
	if len(required) == 0 {
		required = []string{outboundaccess.DefaultCredentialReadScope}
	}
	allowed := false
	for _, scope := range required {
		if tools.TransactionHasScope(tx, scope) {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("remote MCP Task transaction does not authorize credentials")
	}
	token, present, err := workerenv.ReadTokenFileEnv(workerenv.TransactionTokenFile, "Task transaction token")
	if err != nil || !present || strings.TrimSpace(token) == "" {
		return errors.New("remote MCP requires the mounted Task transaction token")
	}
	executor.SetTransactionAuthority(token, scopes)
	executor.SetTransactionCredentialAuthority(true, true, tx.Context["secret"])
	return nil
}

func bindNativeRemoteDependencies(
	ctx context.Context, reader client.Client, tool *corev1alpha1.Tool, credentialSecret string,
	frozen *corev1alpha1.Tool,
) error {
	policy := &corev1alpha1.OutboundAccessPolicy{}
	key := client.ObjectKey{Namespace: tool.Namespace, Name: tool.Spec.HTTP.OutboundAccessPolicyRef.Name}
	if err := reader.Get(ctx, key, policy); err != nil {
		return errors.New("remote MCP outbound policy is unavailable")
	}
	if frozen != nil && (string(policy.UID) != frozen.Annotations[approvalOutboundPolicyUIDAnnotation] ||
		strconv.FormatInt(policy.Generation, 10) != frozen.Annotations[approvalOutboundPolicyGenerationAnnotation] ||
		policy.ResourceVersion != frozen.Annotations[approvalOutboundPolicyResourceVersionAnnotation]) {
		return errors.New("remote MCP outbound policy changed after exposure")
	}
	if policy.Spec.Gateway == nil || policy.Spec.Direct != nil || !policy.DeletionTimestamp.IsZero() {
		return errors.New("remote MCP requires a live gateway policy")
	}
	secrets := []string{tool.Spec.HTTP.AuthSecretRef.Name}
	for _, ref := range approvalOutboundPolicySecretRefs(policy) {
		if ref == nil || ref.Name == "" || ref.Key == "" {
			return errors.New("remote MCP policy credential selector is incomplete")
		}
		if ref.Namespace != "" && ref.Namespace != tool.Namespace {
			return errors.New("remote MCP policy credentials must match the Task namespace")
		}
		secrets = append(secrets, ref.Name)
	}
	if err := outboundaccess.ValidateCredentialAuthority(true, true, credentialSecret, secrets, false); err != nil {
		return err
	}
	bindApprovalAuthRefVersion(ctx, reader, tool.Namespace, tool)
	if tool.Annotations[approvalAuthRefUIDAnnotation] == "" ||
		tool.Annotations[approvalAuthRefResourceVersionAnnotation] == "" {
		return errors.New("remote MCP named credential binding is unavailable")
	}
	if err := bindApprovalResolvedOutboundPolicyVersion(ctx, reader, tool.Namespace, tool, policy); err != nil {
		return errors.New("remote MCP outbound policy binding is unavailable")
	}
	return nil
}

func sameNativeRemoteTool(frozen, live *corev1alpha1.Tool) bool {
	return frozen != nil && live != nil && frozen.UID != "" && frozen.UID == live.UID &&
		frozen.Generation == live.Generation && frozen.Name == live.Name && frozen.Namespace == live.Namespace &&
		live.DeletionTimestamp.IsZero() && reflect.DeepEqual(frozen.Spec, live.Spec)
}

func (s *nativeRemoteTools) validate(ctx context.Context, reader client.Client, advertised *corev1alpha1.Tool) error {
	if reader == nil {
		return errors.New("remote MCP live definition reader is unavailable")
	}
	frozen := s.tools[advertised.Name]
	if !sameNativeRemoteTool(frozen, advertised) {
		return errors.New("remote MCP advertised definition changed")
	}
	live := &corev1alpha1.Tool{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(frozen), live); err != nil ||
		!sameNativeRemoteTool(frozen, live) {
		return errors.New("remote MCP Tool changed after exposure")
	}
	task := &corev1alpha1.Task{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(s.task), task); err != nil ||
		task.UID != s.task.UID || !task.DeletionTimestamp.IsZero() || task.Spec.Type != s.task.Spec.Type ||
		!reflect.DeepEqual(task.Spec.AgentRef, s.task.Spec.AgentRef) ||
		!reflect.DeepEqual(task.Spec.Transaction, s.task.Spec.Transaction) {
		return errors.New("remote MCP Task authority changed after exposure")
	}
	agent := &corev1alpha1.Agent{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(s.agent), agent); err != nil ||
		agent.UID != s.agent.UID || agent.Generation != s.agent.Generation || !reflect.DeepEqual(agent.Spec, s.agent.Spec) {
		return errors.New("remote MCP Agent changed after exposure")
	}
	if err := aitools.ValidateRemoteMCPSelection(task, agent, live); err != nil {
		return err
	}
	credentialSecret := task.Spec.Transaction.Context["secret"]
	if err := bindNativeRemoteDependencies(ctx, reader, live, credentialSecret, frozen); err != nil {
		return err
	}
	for _, key := range []string{
		approvalAuthRefUIDAnnotation, approvalAuthRefResourceVersionAnnotation,
		approvalOutboundPolicyUIDAnnotation, approvalOutboundPolicyGenerationAnnotation,
		approvalOutboundPolicyResourceVersionAnnotation, approvalOutboundPolicySecretsDigestAnnotation,
	} {
		if live.Annotations[key] != frozen.Annotations[key] {
			return errors.New("remote MCP credential or outbound policy binding changed after exposure")
		}
	}
	return nil
}

func executeNativeCustomTool(
	ctx context.Context, toolContext *tools.ToolContext, legacyExecutor *worker.ToolExecutor,
	tool *corev1alpha1.Tool, args json.RawMessage,
) (string, error) {
	if aitools.IsRemoteMCP(tool) {
		return executeNativeRemoteTool(ctx, toolContext, tool, args)
	}
	return legacyExecutor.Execute(ctx, tool, args)
}

func executeNativeRemoteTool(
	ctx context.Context, toolContext *tools.ToolContext, tool *corev1alpha1.Tool, args json.RawMessage,
) (string, error) {
	state, err := validateNativeRemoteTool(ctx, toolContext, tool)
	if err != nil {
		return "", err
	}
	return state.executor.Execute(ctx, tool, args)
}

func validateNativeRemoteTool(
	ctx context.Context, toolContext *tools.ToolContext, tool *corev1alpha1.Tool,
) (*nativeRemoteTools, error) {
	state, _ := ctx.Value(nativeRemoteToolsKey{}).(*nativeRemoteTools)
	if state == nil || toolContext == nil ||
		toolContext.Namespace != state.task.Namespace || toolContext.TaskID != state.task.Name ||
		toolContext.TaskUID != string(state.task.UID) {
		return nil, errors.New("remote MCP Tool was not verified for this native Task")
	}
	if err := state.validate(ctx, toolContext.Client, tool); err != nil {
		return nil, err
	}
	return state, nil
}
