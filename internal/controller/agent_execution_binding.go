/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

// The binding stage implements the coexistence plan's write-once execution
// binding: every executable agent Task freezes its protocol, backend, and an
// immutable content-addressed execution snapshot before any executor-specific
// side effect. The stage is active when an encrypted snapshot store is
// configured; pre-coexistence deployments without a snapshot key keep the
// legacy direct-queue path until the coexistence release wires the key.

const agentExecutionBindingConflictReason = "BindingConflict"

// agentExecutionCandidate is the pure resolution product: the prospective
// binding plus the plaintext snapshot body it references. Resolution performs
// reads only; no durable writes or runtime side effects.
type agentExecutionCandidate struct {
	binding      corev1alpha1.AgentExecutionBinding
	snapshotBody []byte
}

// agentExecutionSnapshotBody is the canonical non-secret executable input
// record frozen into the immutable snapshot. Credentials remain references
// only; raw credential values and TxTokens never enter this structure.
type agentExecutionSnapshotBody struct {
	SchemaVersion   int32                             `json:"schemaVersion"`
	ContractVersion string                            `json:"contractVersion"`
	Backend         string                            `json:"backend"`
	RuntimeType     string                            `json:"runtimeType"`
	Agent           agentExecutionSnapshotAgent       `json:"agent"`
	Configuration   agentExecutionSnapshotConfig      `json:"configuration"`
	RuntimeImage    string                            `json:"runtimeImage"`
	ProfileDigest   string                            `json:"profileDigest"`
	PoolName        string                            `json:"poolName"`
	Prompt          string                            `json:"prompt"`
	Timeout         string                            `json:"timeout,omitempty"`
	SessionRef      *corev1alpha1.SessionReference    `json:"sessionRef,omitempty"`
	Workspace       *corev1alpha1.WorkspaceConfig     `json:"workspace,omitempty"`
	RuntimeOverride *corev1alpha1.AgentRuntimeSpec    `json:"runtimeOverride,omitempty"`
	DefaultTools    *agentExecutionSnapshotToolPolicy `json:"defaultTools,omitempty"`
}

type agentExecutionSnapshotAgent struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
}

type agentExecutionSnapshotConfig struct {
	ProviderKind    string `json:"providerKind"`
	Model           string `json:"model"`
	MaxTurns        int32  `json:"maxTurns"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	SystemPrompt    string `json:"systemPrompt,omitempty"`
}

// agentExecutionSnapshotToolPolicy preserves the omitted-versus-explicit-empty
// allowlist distinction from the Agent defaults.
type agentExecutionSnapshotToolPolicy struct {
	AllowedToolsOmitted bool     `json:"allowedToolsOmitted"`
	AllowedTools        []string `json:"allowedTools"`
	AllowBash           *bool    `json:"allowBash,omitempty"`
}

// agentExecutionBindingEnabled reports whether the write-once binding stage is
// active for this controller process.
func (r *TaskReconciler) agentExecutionBindingEnabled() bool {
	return r.AgentExecutionSnapshots != nil
}

// resolveAgentExecutionCandidate performs pure candidate resolution for an
// explicitly v2-classified built-in agent Task: it resolves the frozen session
// configuration and runtime plan, assembles the snapshot body, and computes
// the canonical binding. It performs no durable writes.
func (r *TaskReconciler) resolveAgentExecutionCandidate(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) (*agentExecutionCandidate, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	configuration, err := resolveACPAgentSessionConfiguration(ctx, reader, task, agent)
	if err != nil {
		return nil, err
	}
	plan, err := PlanACPRuntimeWithConfiguration(task, agent, r.ACPRuntimeImages, configuration)
	if err != nil {
		return nil, err
	}

	namespace := &corev1.Namespace{}
	if err := reader.Get(ctx, types.NamespacedName{Name: task.Namespace}, namespace); err != nil {
		return nil, fmt.Errorf("resolve task namespace identity: %w", err)
	}

	body := agentExecutionSnapshotBody{
		SchemaVersion:   store.AgentExecutionSnapshotSchemaVersion,
		ContractVersion: string(corev1alpha1.AgentRuntimeContractHarnessV2),
		Backend:         string(corev1alpha1.AgentExecutionBackendRuntimePool),
		RuntimeType:     string(agent.Spec.Runtime.Type),
		Agent: agentExecutionSnapshotAgent{
			Namespace:  agent.Namespace,
			Name:       agent.Name,
			UID:        string(agent.UID),
			Generation: agent.Generation,
		},
		Configuration: agentExecutionSnapshotConfig{
			ProviderKind:    configuration.ProviderKind,
			Model:           configuration.Model,
			MaxTurns:        configuration.MaxTurns,
			ReasoningEffort: configuration.ReasoningEffort,
			SystemPrompt:    configuration.SystemPrompt,
		},
		RuntimeImage:    plan.Image,
		ProfileDigest:   string(plan.Digest),
		PoolName:        plan.PoolName,
		Prompt:          task.Spec.Prompt,
		SessionRef:      task.Spec.SessionRef.DeepCopy(),
		Workspace:       task.Spec.Workspace.DeepCopy(),
		RuntimeOverride: task.Spec.AgentRuntime.DeepCopy(),
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
		SchemaVersion:   1,
		Mode:            corev1alpha1.AgentExecutionBindingModeExecute,
		ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV2,
		Backend:         corev1alpha1.AgentExecutionBackendRuntimePool,
		Provenance:      corev1alpha1.AgentExecutionProvenanceNewlyBound,
		Task: corev1alpha1.AgentExecutionBindingTaskRef{
			NamespaceUID:        namespace.UID,
			UID:                 task.UID,
			BoundSpecGeneration: max(task.Generation, 1),
		},
		Agent: &corev1alpha1.AgentExecutionAgentRef{
			Namespace:  agent.Namespace,
			Name:       agent.Name,
			UID:        agent.UID,
			Generation: max(agent.Generation, 1),
		},
		Snapshot: corev1alpha1.AgentExecutionSnapshotRef{
			ID:            string(task.UID) + "/" + snapshotDigest,
			Digest:        snapshotDigest,
			SchemaVersion: store.AgentExecutionSnapshotSchemaVersion,
		},
		RuntimeType:                       agent.Spec.Runtime.Type,
		RuntimeProfileDigest:              string(plan.Digest),
		RuntimeProfileDigestSchemaVersion: 1,
	}

	backendControl, err := r.resolveAgentExecutionBackendControl(ctx, reader)
	if err != nil {
		return nil, err
	}
	binding.BackendControl = backendControl

	digest, err := canonicalAgentExecutionBindingDigest(binding)
	if err != nil {
		return nil, err
	}
	binding.BindingDigest = digest
	binding.BoundAt = metav1.Now()

	return &agentExecutionCandidate{binding: binding, snapshotBody: encoded}, nil
}

// resolveAgentExecutionBackendControl reads the durable backend admission
// control object uncached. When the singleton exists, the v2 backend must be
// effectively enabled and its admission revision is frozen into the binding.
// A pre-coexistence deployment without the singleton binds without a control
// reference.
func (r *TaskReconciler) resolveAgentExecutionBackendControl(
	ctx context.Context,
	reader client.Reader,
) (*corev1alpha1.AgentExecutionBackendControlRef, error) {
	control := &corev1alpha1.AgentExecutionControl{}
	err := reader.Get(ctx, types.NamespacedName{
		Namespace: corev1alpha1.AgentExecutionControlNamespace,
		Name:      corev1alpha1.AgentExecutionControlName,
	}, control)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read AgentExecutionControl: %w", err)
	}
	if control.Status.Backends == nil {
		return nil, fmt.Errorf("AgentExecutionControl %s/%s has no observed backend modes; binding admission fails closed",
			control.Namespace, control.Name)
	}
	v2 := control.Status.Backends.V2
	if v2.EffectiveMode != corev1alpha1.AgentExecutionEffectiveModeEnabled {
		return nil, fmt.Errorf("harness v2 backend admission is %s; new bindings are rejected and never fall back", v2.EffectiveMode)
	}
	return &corev1alpha1.AgentExecutionBackendControlRef{
		Name:         control.Name,
		UID:          control.UID,
		Generation:   max(control.Generation, 1),
		ModeRevision: v2.ModeRevision,
		AdmittedMode: v2.EffectiveMode,
	}, nil
}

// canonicalAgentExecutionBindingDigest computes the canonical binding digest
// over the binding with its digest and timestamp cleared, so re-resolution of
// identical inputs is digest-stable across reconciles.
func canonicalAgentExecutionBindingDigest(binding corev1alpha1.AgentExecutionBinding) (string, error) {
	normalized := *binding.DeepCopy()
	normalized.BindingDigest = ""
	normalized.BoundAt = metav1.Time{}
	return acpDomainDigest("agent-execution-binding", normalized)
}

func canonicalAgentExecutionSnapshotBody(body agentExecutionSnapshotBody) ([]byte, error) {
	encoded, err := harnessv2.CanonicalValue(body)
	if err != nil {
		return nil, fmt.Errorf("canonicalize snapshot body: %w", err)
	}
	return encoded, nil
}

// persistAgentExecutionSnapshot idempotently stores the immutable snapshot.
func (r *TaskReconciler) persistAgentExecutionSnapshot(
	ctx context.Context,
	task *corev1alpha1.Task,
	candidate *agentExecutionCandidate,
) error {
	return r.AgentExecutionSnapshots.PersistAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshot{
		TaskUID:       string(task.UID),
		Digest:        candidate.binding.Snapshot.Digest,
		SchemaVersion: candidate.binding.Snapshot.SchemaVersion,
		Body:          candidate.snapshotBody,
	})
}

// errAgentExecutionBindingConflict marks a permanent conflict: an existing
// binding never gets overwritten and the Task never dispatches under a
// mismatched candidate.
type errAgentExecutionBindingConflict struct {
	existingDigest  string
	candidateDigest string
}

func (e *errAgentExecutionBindingConflict) Error() string {
	return fmt.Sprintf("task already carries immutable execution binding %s; refusing to replace it with %s",
		e.existingDigest, e.candidateDigest)
}

// persistAgentExecutionBinding implements the uncached compare-if-absent
// write-once binding CAS. It never overwrites an existing binding.
func (r *TaskReconciler) persistAgentExecutionBinding(
	ctx context.Context,
	task *corev1alpha1.Task,
	candidate *agentExecutionCandidate,
) (*corev1alpha1.AgentExecutionBinding, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	current := &corev1alpha1.Task{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, current); err != nil {
		return nil, fmt.Errorf("uncached task read before binding: %w", err)
	}
	if current.UID != task.UID {
		return nil, fmt.Errorf("task UID changed from %s to %s; never dispatching", task.UID, current.UID)
	}
	if !current.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("task is deleting; a deleting task may be classified only for cleanup and never dispatches")
	}
	if existing := current.Status.AgentExecutionBinding; existing != nil {
		if existing.BindingDigest == candidate.binding.BindingDigest {
			return existing, nil
		}
		return nil, &errAgentExecutionBindingConflict{
			existingDigest:  existing.BindingDigest,
			candidateDigest: candidate.binding.BindingDigest,
		}
	}

	base := current.DeepCopy()
	current.Status.AgentExecutionBinding = candidate.binding.DeepCopy()
	if err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return nil, fmt.Errorf("write-once binding patch: %w", err)
	}
	return current.Status.AgentExecutionBinding, nil
}

// verifyBoundExecution re-reads the Task and backend control uncached
// immediately before the first executor side effect.
func (r *TaskReconciler) verifyBoundExecution(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	current := &corev1alpha1.Task{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, current); err != nil {
		return fmt.Errorf("uncached task read before executor side effect: %w", err)
	}
	if current.UID != task.UID {
		return fmt.Errorf("task UID changed after binding; never dispatching")
	}
	if !current.DeletionTimestamp.IsZero() {
		return fmt.Errorf("task began deleting after binding; dispatch is cancelled")
	}
	persisted := current.Status.AgentExecutionBinding
	if persisted == nil || persisted.BindingDigest != binding.BindingDigest {
		return fmt.Errorf("persisted binding does not match the verified candidate; refusing to dispatch")
	}
	if persisted.Mode != corev1alpha1.AgentExecutionBindingModeExecute {
		return fmt.Errorf("binding mode %s does not authorize execution", persisted.Mode)
	}
	if persisted.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV2 {
		return fmt.Errorf("binding contract %s is not dispatchable by the ACP executor", persisted.ContractVersion)
	}
	if persisted.BackendControl != nil {
		control := &corev1alpha1.AgentExecutionControl{}
		if err := reader.Get(ctx, types.NamespacedName{
			Namespace: corev1alpha1.AgentExecutionControlNamespace,
			Name:      corev1alpha1.AgentExecutionControlName,
		}, control); err != nil {
			return fmt.Errorf("uncached backend control read before executor side effect: %w", err)
		}
		if control.UID != persisted.BackendControl.UID {
			return fmt.Errorf("AgentExecutionControl was recreated; bound admission is void and the task requires operator reconciliation")
		}
		if control.Status.Backends == nil ||
			control.Status.Backends.V2.EffectiveMode != corev1alpha1.AgentExecutionEffectiveModeEnabled {
			return fmt.Errorf("harness v2 backend admission closed after binding; dispatch is withheld")
		}
	}
	return nil
}

// ensureAgentExecutionBinding runs the binding stage for the ACP execution
// path. It returns handled=true when the caller must return the result
// immediately (failure or requeue) and handled=false when dispatch may
// proceed under a verified binding.
func (r *TaskReconciler) ensureAgentExecutionBinding(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) (ctrl.Result, error, bool) {
	log := logf.FromContext(ctx)

	if existing := task.Status.AgentExecutionBinding; existing != nil {
		if err := r.verifyBoundExecution(ctx, task, existing); err != nil {
			log.Error(err, "bound execution verification failed")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
		}
		return ctrl.Result{}, nil, false
	}

	candidate, err := r.resolveAgentExecutionCandidate(ctx, task, agent)
	if err != nil {
		if isPermanentACPAgentConfigurationError(err) {
			result, failErr := r.failTask(ctx, task, err.Error())
			return result, failErr, true
		}
		log.Error(err, "agent execution candidate resolution failed")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
	}
	if err := r.persistAgentExecutionSnapshot(ctx, task, candidate); err != nil {
		log.Error(err, "immutable execution snapshot persistence failed")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
	}
	binding, err := r.persistAgentExecutionBinding(ctx, task, candidate)
	if err != nil {
		conflict := &errAgentExecutionBindingConflict{}
		if errors.As(err, &conflict) {
			r.Recorder.Eventf(task, corev1.EventTypeWarning, agentExecutionBindingConflictReason, "%s", err.Error())
			result, failErr := r.failTask(ctx, task, err.Error())
			return result, failErr, true
		}
		log.Error(err, "write-once binding persistence failed")
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil, true
	}
	task.Status.AgentExecutionBinding = binding
	if err := r.verifyBoundExecution(ctx, task, binding); err != nil {
		log.Error(err, "bound execution verification failed after binding")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
	}
	return ctrl.Result{}, nil, false
}
