/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

// The binding stage implements the coexistence plan's write-once execution
// binding: every executable agent Task freezes its protocol, backend, and an
// immutable content-addressed execution snapshot before any executor-specific
// side effect. The snapshot store and revisioned backend control are mandatory:
// a controller that cannot prove either one never queues executor demand.

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
	SchemaVersion    int32                             `json:"schemaVersion"`
	ContractVersion  string                            `json:"contractVersion"`
	Backend          string                            `json:"backend"`
	RuntimeType      string                            `json:"runtimeType"`
	Agent            agentExecutionSnapshotAgent       `json:"agent"`
	Configuration    agentExecutionSnapshotConfig      `json:"configuration"`
	RuntimeImage     string                            `json:"runtimeImage"`
	RuntimeProfile   harnessv2.RuntimeProfile          `json:"runtimeProfile"`
	ProfileDigest    string                            `json:"profileDigest"`
	PoolName         string                            `json:"poolName"`
	MCPConfiguration *harnessv2.MCPPolicyConfiguration `json:"mcpConfiguration,omitempty"`
	Prompt           string                            `json:"prompt"`
	Timeout          string                            `json:"timeout,omitempty"`
	RetryPolicy      *corev1alpha1.RetryPolicy         `json:"retryPolicy,omitempty"`
	SessionRef       *corev1alpha1.SessionReference    `json:"sessionRef,omitempty"`
	Workspace        *corev1alpha1.WorkspaceConfig     `json:"workspace,omitempty"`
	RuntimeOverride  *corev1alpha1.AgentRuntimeSpec    `json:"runtimeOverride,omitempty"`
	DefaultTools     *agentExecutionSnapshotToolPolicy `json:"defaultTools,omitempty"`
	HarnessV1        *agentExecutionSnapshotHarnessV1  `json:"harnessV1,omitempty"`
}

type agentExecutionSnapshotAgent struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
}

type agentExecutionSnapshotConfig struct {
	AgentUID        string `json:"agentUID"`
	AgentGeneration int64  `json:"agentGeneration"`
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

// agentExecutionSnapshotHarnessV1 freezes the non-secret wrapper or external
// endpoint identity used by the v1 dispatcher. Secret contents never enter the
// snapshot; the exact Secret UID/resourceVersion is verified again before the
// first executor side effect.
type agentExecutionSnapshotHarnessV1 struct {
	Endpoint                  string                            `json:"endpoint"`
	Backend                   string                            `json:"backend"`
	RuntimeName               string                            `json:"runtimeName"`
	AuthSecretNamespace       string                            `json:"authSecretNamespace"`
	AuthSecretName            string                            `json:"authSecretName"`
	AuthSecretKey             string                            `json:"authSecretKey"`
	AuthSecretUID             string                            `json:"authSecretUID"`
	AuthSecretResourceVersion string                            `json:"authSecretResourceVersion"`
	DuplicateSafe             bool                              `json:"duplicateSafe"`
	SessionName               string                            `json:"sessionName"`
	CredentialRefs            []agentExecutionSnapshotSecretRef `json:"credentialRefs,omitempty"`
}

type agentExecutionSnapshotSecretRef struct {
	Role            string   `json:"role"`
	Namespace       string   `json:"namespace"`
	Name            string   `json:"name"`
	UID             string   `json:"uid"`
	ResourceVersion string   `json:"resourceVersion"`
	Keys            []string `json:"keys"`
}

type verifiedAgentExecution struct {
	binding          *corev1alpha1.AgentExecutionBinding
	snapshot         *store.AgentExecutionSnapshot
	body             agentExecutionSnapshotBody
	plan             ACPRuntimePlan
	frozenTask       *corev1alpha1.Task
	configuration    harnessv2.AgentSessionConfiguration
	mcpConfiguration harnessv2.MCPPolicyConfiguration
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
	if r.AgentExecutionSnapshots == nil {
		return nil, errors.New("encrypted agent execution snapshot store is required; execution admission fails closed")
	}
	if task == nil || task.UID == "" || task.Generation < 1 {
		return nil, errors.New("task UID and positive spec generation are required for execution binding")
	}
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
		return nil, permanentACPAgentConfiguration(err)
	}
	var mcpConfiguration harnessv2.MCPPolicyConfiguration
	if r.MCPRegistry != nil {
		mcpConfiguration, err = buildRuntimeSessionMCPConfigurationWithRegistry(
			ctx, reader, task, agent, plan.Profile, r.MCPRegistry,
		)
	} else {
		mcpConfiguration, err = buildRuntimeSessionMCPConfiguration(
			ctx, reader, task, agent, plan.Profile,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve frozen ACP MCP configuration: %w", err)
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
			AgentUID:        configuration.AgentUID,
			AgentGeneration: configuration.AgentGeneration,
			ProviderKind:    configuration.ProviderKind,
			Model:           configuration.Model,
			MaxTurns:        configuration.MaxTurns,
			ReasoningEffort: configuration.ReasoningEffort,
			SystemPrompt:    configuration.SystemPrompt,
		},
		RuntimeImage:     plan.Image,
		RuntimeProfile:   plan.Profile,
		ProfileDigest:    string(plan.Digest),
		PoolName:         plan.PoolName,
		MCPConfiguration: &mcpConfiguration,
		Prompt:           task.Spec.Prompt,
		RetryPolicy:      task.Spec.RetryPolicy.DeepCopy(),
		SessionRef:       task.Spec.SessionRef.DeepCopy(),
		Workspace:        task.Spec.Workspace.DeepCopy(),
		RuntimeOverride:  task.Spec.AgentRuntime.DeepCopy(),
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
			BoundSpecGeneration: task.Generation,
		},
		Agent: &corev1alpha1.AgentExecutionAgentRef{
			Namespace:  agent.Namespace,
			Name:       agent.Name,
			UID:        agent.UID,
			Generation: agent.Generation,
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
// effectively enabled at its current observed generation and its admission
// revision is frozen into the binding. Missing or stale control fails closed.
func (r *TaskReconciler) resolveAgentExecutionBackendControl(
	ctx context.Context,
	reader client.Reader,
) (*corev1alpha1.AgentExecutionBackendControlRef, error) {
	return r.resolveAgentExecutionBackendControlFor(ctx, reader, store.AgentExecutionBackendV2)
}

func (r *TaskReconciler) resolveAgentExecutionBackendControlFor(
	ctx context.Context,
	reader client.Reader,
	backend store.AgentExecutionBackendKey,
) (*corev1alpha1.AgentExecutionBackendControlRef, error) {
	control := &corev1alpha1.AgentExecutionControl{}
	err := reader.Get(ctx, types.NamespacedName{
		Namespace: corev1alpha1.AgentExecutionControlNamespace,
		Name:      corev1alpha1.AgentExecutionControlName,
	}, control)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("required AgentExecutionControl %s/%s is missing; binding admission fails closed",
			corev1alpha1.AgentExecutionControlNamespace, corev1alpha1.AgentExecutionControlName)
	}
	if err != nil {
		return nil, fmt.Errorf("read AgentExecutionControl: %w", err)
	}
	if control.Status.Backends == nil {
		return nil, fmt.Errorf("AgentExecutionControl %s/%s has no observed backend modes; binding admission fails closed",
			control.Namespace, control.Name)
	}
	if control.UID == "" || control.Generation < 1 || control.Status.ObservedGeneration != control.Generation {
		return nil, fmt.Errorf("AgentExecutionControl %s/%s generation %d is not exactly observed (observedGeneration=%d); binding admission fails closed",
			control.Namespace, control.Name, control.Generation, control.Status.ObservedGeneration)
	}
	var observed corev1alpha1.AgentExecutionBackendStatus
	switch backend {
	case store.AgentExecutionBackendV1:
		observed = control.Status.Backends.V1
	case store.AgentExecutionBackendV2:
		observed = control.Status.Backends.V2
	default:
		return nil, fmt.Errorf("unsupported agent execution backend %q", backend)
	}
	if observed.ModeRevision < 1 {
		return nil, fmt.Errorf("AgentExecutionControl %s/%s has invalid harness %s mode revision %d; binding admission fails closed",
			control.Namespace, control.Name, backend, observed.ModeRevision)
	}
	if observed.EffectiveMode != corev1alpha1.AgentExecutionEffectiveModeEnabled {
		return nil, fmt.Errorf("harness %s backend admission is %s; new bindings are rejected and never fall back", backend, observed.EffectiveMode)
	}
	return &corev1alpha1.AgentExecutionBackendControlRef{
		Name:         control.Name,
		UID:          control.UID,
		Generation:   control.Generation,
		ModeRevision: observed.ModeRevision,
		AdmittedMode: observed.EffectiveMode,
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

func agentExecutionBindingControlRevision(binding *corev1alpha1.AgentExecutionBinding) (store.AgentExecutionControlRevision, error) {
	if binding == nil || binding.BackendControl == nil {
		return store.AgentExecutionControlRevision{}, errors.New("new execution binding requires an exact backend control revision")
	}
	backend := store.AgentExecutionBackendV2
	if binding.ContractVersion == corev1alpha1.AgentRuntimeContractHarnessV1 {
		backend = store.AgentExecutionBackendV1
	} else if binding.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV2 {
		return store.AgentExecutionControlRevision{}, fmt.Errorf("unsupported execution binding contract %q", binding.ContractVersion)
	}
	ref := binding.BackendControl
	revision := store.AgentExecutionControlRevision{
		ControlUID: string(ref.UID), ControlGeneration: ref.Generation,
		Backend: backend, ModeRevision: ref.ModeRevision,
	}
	if err := revision.Validate(); err != nil {
		return store.AgentExecutionControlRevision{}, err
	}
	return revision, nil
}

func agentExecutionBindingReservationFor(
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) (store.AgentExecutionBindingReservation, error) {
	if task == nil || binding == nil || task.UID == "" {
		return store.AgentExecutionBindingReservation{}, errors.New("task identity and execution binding are required for reservation")
	}
	revision, err := agentExecutionBindingControlRevision(binding)
	if err != nil {
		return store.AgentExecutionBindingReservation{}, err
	}
	reservation := store.AgentExecutionBindingReservation{
		TaskNamespace:  task.Namespace,
		TaskName:       task.Name,
		TaskUID:        string(task.UID),
		Revision:       revision,
		BindingDigest:  binding.BindingDigest,
		SnapshotDigest: binding.Snapshot.Digest,
		State:          store.AgentExecutionBindingReservationOpen,
	}
	reservation.ID = reservation.CanonicalID()
	return reservation, reservation.Validate()
}

func (r *TaskReconciler) createAgentExecutionBindingReservation(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) (*store.AgentExecutionBindingReservation, error) {
	if r.AgentExecutionBindingReservations == nil {
		return nil, errors.New("durable agent execution binding reservation store is required; admission fails closed")
	}
	reservation, err := agentExecutionBindingReservationFor(task, binding)
	if err != nil {
		return nil, err
	}
	created, err := r.AgentExecutionBindingReservations.CreateAgentExecutionBindingReservation(ctx, reservation)
	if err != nil {
		return nil, fmt.Errorf("create execution binding reservation: %w", err)
	}
	return created, nil
}

func (r *TaskReconciler) settleAgentExecutionBindingReservation(
	ctx context.Context,
	reservation *store.AgentExecutionBindingReservation,
	target store.AgentExecutionBindingReservationState,
	reason string,
) error {
	if r.AgentExecutionBindingReservations == nil || reservation == nil {
		return errors.New("durable execution binding reservation is required")
	}
	_, err := r.AgentExecutionBindingReservations.SettleAgentExecutionBindingReservation(ctx,
		store.SettleAgentExecutionBindingReservationRequest{
			ID: reservation.ID, ExpectedVersion: reservation.Version, TargetState: target,
			BindingDigest: reservation.BindingDigest, TerminalReason: reason,
		})
	if err != nil {
		return fmt.Errorf("settle execution binding reservation: %w", err)
	}
	return nil
}

func (r *TaskReconciler) verifyBoundAgentExecutionReservation(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) error {
	_, err := r.loadBoundAgentExecutionReservation(ctx, task, binding)
	return err
}

func (r *TaskReconciler) loadBoundAgentExecutionReservation(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) (*store.AgentExecutionBindingReservation, error) {
	if r.AgentExecutionBindingReservations == nil {
		return nil, errors.New("durable agent execution binding reservation store is required; execution fails closed")
	}
	want, err := agentExecutionBindingReservationFor(task, binding)
	if err != nil {
		return nil, err
	}
	got, err := r.AgentExecutionBindingReservations.GetAgentExecutionBindingReservation(ctx, want.ID)
	if err != nil {
		return nil, fmt.Errorf("load execution binding reservation: %w", err)
	}
	if got.State != store.AgentExecutionBindingReservationBound || got.TaskNamespace != want.TaskNamespace ||
		got.TaskName != want.TaskName || got.TaskUID != want.TaskUID || got.Revision != want.Revision ||
		got.BindingDigest != want.BindingDigest || got.SnapshotDigest != want.SnapshotDigest {
		return nil, errors.New("execution binding reservation is not durably Bound to the exact Task, revision, binding, and snapshot")
	}
	return got, nil
}

// verifyBoundAgentExecutionBackendMode authorizes a first executor side effect.
// Enabled admission requires the exact current control revision. Drain-only
// admits only a binding whose exact pre-cutoff reservation is already durably
// Bound; closing and disabled never start new executor work.
func (r *TaskReconciler) verifyBoundAgentExecutionBackendMode(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
	backend store.AgentExecutionBackendKey,
) error {
	reservation, err := r.loadBoundAgentExecutionReservation(ctx, task, binding)
	if err != nil {
		return err
	}
	if binding.BackendControl == nil || binding.BackendControl.AdmittedMode != corev1alpha1.AgentExecutionEffectiveModeEnabled {
		return errors.New("execution binding lacks an enabled backend admission revision")
	}
	control := &corev1alpha1.AgentExecutionControl{}
	if err := reader.Get(ctx, types.NamespacedName{
		Namespace: corev1alpha1.AgentExecutionControlNamespace,
		Name:      corev1alpha1.AgentExecutionControlName,
	}, control); err != nil {
		return fmt.Errorf("uncached backend control read before executor side effect: %w", err)
	}
	ref := binding.BackendControl
	if ref.Name != control.Name || ref.UID == "" || control.UID != ref.UID ||
		control.Status.Backends == nil || control.Status.ObservedGeneration != control.Generation {
		return errors.New("AgentExecutionControl identity or observed generation does not authorize execution")
	}
	var observed corev1alpha1.AgentExecutionBackendStatus
	switch backend {
	case store.AgentExecutionBackendV1:
		observed = control.Status.Backends.V1
	case store.AgentExecutionBackendV2:
		observed = control.Status.Backends.V2
	default:
		return fmt.Errorf("unsupported agent execution backend %q", backend)
	}
	switch observed.EffectiveMode {
	case corev1alpha1.AgentExecutionEffectiveModeEnabled:
		if control.Generation != ref.Generation || observed.ModeRevision != ref.ModeRevision {
			return errors.New("enabled backend mode revision is stale or does not exactly match the binding admission revision")
		}
		return nil
	case corev1alpha1.AgentExecutionEffectiveModeDrainOnly:
		if observed.AdmissionClosedAt == nil || observed.CutoffInventoryDigest == "" ||
			ref.Generation > control.Generation || ref.ModeRevision >= observed.ModeRevision ||
			reservation.ReservedAt.After(observed.AdmissionClosedAt.Time) || reservation.SettledAt == nil ||
			reservation.SettledAt.After(observed.AdmissionClosedAt.Time) {
			return errors.New("drain-only backend cannot prove the binding reservation preceded its cutoff")
		}
		return nil
	case corev1alpha1.AgentExecutionEffectiveModeClosing:
		return errors.New("backend admission is closing; executor demand remains withheld until cutoff proof completes")
	case corev1alpha1.AgentExecutionEffectiveModeDisabled:
		return errors.New("backend execution is disabled")
	default:
		return fmt.Errorf("backend effective mode %q is unsupported", observed.EffectiveMode)
	}
}

func (r *TaskReconciler) recoverBoundAgentExecutionReservation(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) error {
	if r.AgentExecutionBindingReservations == nil {
		return errors.New("durable agent execution binding reservation store is required; execution fails closed")
	}
	want, err := agentExecutionBindingReservationFor(task, binding)
	if err != nil {
		return err
	}
	current, err := r.AgentExecutionBindingReservations.GetAgentExecutionBindingReservation(ctx, want.ID)
	if err != nil {
		return fmt.Errorf("load execution binding reservation for recovery: %w", err)
	}
	if current.TaskNamespace != want.TaskNamespace || current.TaskName != want.TaskName ||
		current.TaskUID != want.TaskUID || current.Revision != want.Revision ||
		current.BindingDigest != want.BindingDigest || current.SnapshotDigest != want.SnapshotDigest {
		return errors.New("execution binding reservation does not match the immutable Task binding")
	}
	switch current.State {
	case store.AgentExecutionBindingReservationBound:
		return nil
	case store.AgentExecutionBindingReservationOpen:
		err = r.settleAgentExecutionBindingReservation(
			ctx, current, store.AgentExecutionBindingReservationBound, "",
		)
		return err
	default:
		return fmt.Errorf("execution binding reservation is terminal in non-executable state %s", current.State)
	}
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
	if current.Generation != candidate.binding.Task.BoundSpecGeneration {
		return nil, fmt.Errorf("task spec generation changed from bound candidate %d to %d; refusing stale binding",
			candidate.binding.Task.BoundSpecGeneration, current.Generation)
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

func decodeAgentExecutionSnapshot(body []byte) (agentExecutionSnapshotBody, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var decoded agentExecutionSnapshotBody
	if err := decoder.Decode(&decoded); err != nil {
		return agentExecutionSnapshotBody{}, fmt.Errorf("decode agent execution snapshot: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return agentExecutionSnapshotBody{}, errors.New("decode agent execution snapshot: multiple JSON values")
		}
		return agentExecutionSnapshotBody{}, fmt.Errorf("decode agent execution snapshot trailer: %w", err)
	}
	return decoded, nil
}

//nolint:gocyclo // Snapshot validation intentionally checks every immutable v2 execution field together.
func validateAgentExecutionSnapshot(
	binding *corev1alpha1.AgentExecutionBinding,
	snapshot *store.AgentExecutionSnapshot,
	body agentExecutionSnapshotBody,
) (ACPRuntimePlan, harnessv2.AgentSessionConfiguration, harnessv2.MCPPolicyConfiguration, error) {
	if binding == nil || snapshot == nil {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, errors.New("binding and execution snapshot are required")
	}
	key := store.AgentExecutionSnapshotKey{TaskUID: string(binding.Task.UID), Digest: binding.Snapshot.Digest}
	if binding.Snapshot.ID != key.ID() || snapshot.TaskUID != key.TaskUID || snapshot.Digest != key.Digest {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, errors.New("execution snapshot identity does not exactly match the binding")
	}
	if binding.Snapshot.SchemaVersion != store.AgentExecutionSnapshotSchemaVersion ||
		snapshot.SchemaVersion != binding.Snapshot.SchemaVersion || body.SchemaVersion != binding.Snapshot.SchemaVersion {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, fmt.Errorf("unsupported or mismatched execution snapshot schema version")
	}
	if binding.SchemaVersion != 1 || binding.RuntimeProfileDigestSchemaVersion != 1 {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, errors.New("unsupported execution binding or RuntimeProfile digest schema version")
	}
	if computed := store.CanonicalAgentExecutionSnapshotDigest(snapshot.Body); computed != binding.Snapshot.Digest {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, fmt.Errorf("execution snapshot body digest %s does not match binding digest %s", computed, binding.Snapshot.Digest)
	}
	canonicalBody, err := canonicalAgentExecutionSnapshotBody(body)
	if err != nil || !bytes.Equal(canonicalBody, snapshot.Body) {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, errors.New("execution snapshot body is not canonical")
	}
	if body.ContractVersion != string(binding.ContractVersion) || body.Backend != string(binding.Backend) ||
		body.RuntimeType != string(binding.RuntimeType) || binding.Agent == nil ||
		body.Agent.Namespace != binding.Agent.Namespace || body.Agent.Name != binding.Agent.Name ||
		body.Agent.UID != string(binding.Agent.UID) || body.Agent.Generation != binding.Agent.Generation {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, errors.New("execution snapshot route or Agent identity does not exactly match the binding")
	}
	configuration := harnessv2.AgentSessionConfiguration{
		AgentUID: body.Configuration.AgentUID, AgentGeneration: body.Configuration.AgentGeneration,
		ProviderKind: body.Configuration.ProviderKind, Model: body.Configuration.Model,
		MaxTurns: body.Configuration.MaxTurns, ReasoningEffort: body.Configuration.ReasoningEffort,
		SystemPrompt: body.Configuration.SystemPrompt,
	}
	if configuration.AgentUID != body.Agent.UID || configuration.AgentGeneration != body.Agent.Generation {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, errors.New("execution snapshot configuration Agent identity is inconsistent")
	}
	if body.RuntimeType != body.Configuration.ProviderKind || body.RuntimeType != body.RuntimeProfile.ProviderKind {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, errors.New("execution snapshot runtime type is inconsistent with the frozen configuration/profile")
	}
	if body.Timeout != "" {
		if _, err := time.ParseDuration(body.Timeout); err != nil {
			return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, fmt.Errorf("validate frozen Task timeout: %w", err)
		}
	}
	if err := body.RuntimeProfile.Validate(); err != nil {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, fmt.Errorf("validate frozen RuntimeProfile: %w", err)
	}
	if err := configuration.ValidateProfile(body.RuntimeProfile); err != nil {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, fmt.Errorf("validate frozen Agent configuration: %w", err)
	}
	if body.MCPConfiguration == nil {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, errors.New("execution snapshot is missing the frozen MCP policy configuration")
	}
	mcpConfiguration := *body.MCPConfiguration
	if err := mcpConfiguration.ValidateProfile(body.RuntimeProfile); err != nil {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, fmt.Errorf("validate frozen MCP policy configuration: %w", err)
	}
	profileDigest, err := harnessv2.CanonicalProfileDigest(body.RuntimeProfile)
	if err != nil {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, fmt.Errorf("digest frozen RuntimeProfile: %w", err)
	}
	if string(profileDigest) != body.ProfileDigest || body.ProfileDigest != binding.RuntimeProfileDigest {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, errors.New("frozen RuntimeProfile digest does not exactly match the binding")
	}
	if !ACPRuntimeImageAvailable(body.RuntimeImage) {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, errors.New("frozen runtime image is not digest pinned")
	}
	poolIdentityDigest, err := acpDomainDigest("runtime-pool-identity", map[string]string{
		"profileDigest": body.ProfileDigest, "runtimeImage": body.RuntimeImage,
	})
	if err != nil {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, err
	}
	wantPoolName := acpRuntimePoolName(body.RuntimeType, harnessv2.ProfileDigest(poolIdentityDigest))
	if strings.TrimSpace(body.PoolName) == "" || body.PoolName != wantPoolName {
		return ACPRuntimePlan{}, harnessv2.AgentSessionConfiguration{}, harnessv2.MCPPolicyConfiguration{}, errors.New("frozen RuntimePool identity is inconsistent")
	}
	return ACPRuntimePlan{
		PoolName: body.PoolName, Image: body.RuntimeImage, Profile: body.RuntimeProfile, Digest: profileDigest,
	}, configuration, mcpConfiguration, nil
}

func frozenTaskFromAgentExecutionSnapshot(task *corev1alpha1.Task, binding *corev1alpha1.AgentExecutionBinding, body agentExecutionSnapshotBody) *corev1alpha1.Task {
	frozen := task.DeepCopy()
	frozen.Generation = binding.Task.BoundSpecGeneration
	frozen.Spec.Prompt = body.Prompt
	frozen.Spec.Timeout = nil
	if body.Timeout != "" {
		if duration, err := time.ParseDuration(body.Timeout); err == nil {
			frozen.Spec.Timeout = &metav1.Duration{Duration: duration}
		}
	}
	frozen.Spec.SessionRef = body.SessionRef.DeepCopy()
	frozen.Spec.Workspace = body.Workspace.DeepCopy()
	frozen.Spec.AgentRuntime = body.RuntimeOverride.DeepCopy()
	frozen.Spec.RetryPolicy = body.RetryPolicy.DeepCopy()
	return frozen
}

// loadVerifiedBoundExecution re-reads the Task, backend control, and encrypted
// snapshot uncached immediately before executor demand is persisted.
func (r *TaskReconciler) loadVerifiedBoundExecution(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) (*verifiedAgentExecution, error) {
	if r.AgentExecutionSnapshots == nil {
		return nil, errors.New("encrypted agent execution snapshot store is required; execution fails closed")
	}
	if task == nil || binding == nil {
		return nil, errors.New("task and execution binding are required")
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	current := &corev1alpha1.Task{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, current); err != nil {
		return nil, fmt.Errorf("uncached task read before executor side effect: %w", err)
	}
	if current.UID != task.UID {
		return nil, fmt.Errorf("task UID changed after binding; never dispatching")
	}
	if !current.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("task began deleting after binding; dispatch is cancelled")
	}
	persisted := current.Status.AgentExecutionBinding
	if persisted == nil || persisted.BindingDigest != binding.BindingDigest {
		return nil, fmt.Errorf("persisted binding does not match the verified candidate; refusing to dispatch")
	}
	canonicalDigest, err := canonicalAgentExecutionBindingDigest(*persisted)
	if err != nil || canonicalDigest != persisted.BindingDigest {
		return nil, fmt.Errorf("persisted binding failed canonical integrity verification")
	}
	if persisted.Task.UID != current.UID || persisted.Task.BoundSpecGeneration != current.Generation {
		return nil, fmt.Errorf("task UID/spec generation no longer exactly matches the immutable binding")
	}
	namespace := &corev1.Namespace{}
	if err := reader.Get(ctx, types.NamespacedName{Name: current.Namespace}, namespace); err != nil {
		return nil, fmt.Errorf("uncached namespace identity read before executor side effect: %w", err)
	}
	if namespace.UID == "" || namespace.UID != persisted.Task.NamespaceUID {
		return nil, errors.New("task namespace identity no longer exactly matches the immutable binding")
	}
	if persisted.Mode != corev1alpha1.AgentExecutionBindingModeExecute {
		return nil, fmt.Errorf("binding mode %s does not authorize execution", persisted.Mode)
	}
	if persisted.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV2 || persisted.Backend != corev1alpha1.AgentExecutionBackendRuntimePool {
		return nil, fmt.Errorf("binding route %s/%s is not dispatchable by the ACP executor", persisted.ContractVersion, persisted.Backend)
	}
	if err := r.verifyBoundAgentExecutionBackendMode(
		ctx, reader, current, persisted, store.AgentExecutionBackendV2,
	); err != nil {
		return nil, err
	}
	snapshot, err := r.AgentExecutionSnapshots.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{
		TaskUID: string(persisted.Task.UID), Digest: persisted.Snapshot.Digest,
	})
	if err != nil {
		return nil, fmt.Errorf("load immutable execution snapshot: %w", err)
	}
	body, err := decodeAgentExecutionSnapshot(snapshot.Body)
	if err != nil {
		return nil, err
	}
	plan, configuration, mcpConfiguration, err := validateAgentExecutionSnapshot(persisted, snapshot, body)
	if err != nil {
		return nil, err
	}
	return &verifiedAgentExecution{
		binding: persisted.DeepCopy(), snapshot: snapshot, body: body, plan: plan,
		frozenTask: frozenTaskFromAgentExecutionSnapshot(current, persisted, body), configuration: configuration,
		mcpConfiguration: mcpConfiguration,
	}, nil
}

// verifyBoundExecution preserves the narrow verification API used by binding
// reconciliation while sharing the exact queue-time verification path.
func (r *TaskReconciler) verifyBoundExecution(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) error {
	_, err := r.loadVerifiedBoundExecution(ctx, task, binding)
	return err
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
	if task == nil {
		return ctrl.Result{}, errors.New("task is required for execution binding"), true
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
			log.Error(err, "uncached Task read before execution binding")
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil, true
		}
		if current.UID != task.UID {
			log.Error(errors.New("task UID changed"), "execution binding withheld")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
		}
		if current.Status.AgentExecutionBinding != nil {
			task = current
		}
	}
	if existing := task.Status.AgentExecutionBinding; existing != nil {
		if err := r.recoverBoundAgentExecutionReservation(ctx, task, existing); err != nil {
			log.Error(err, "bound execution reservation recovery failed")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
		}
		if err := r.verifyBoundAgentExecutionReservation(ctx, task, existing); err != nil {
			log.Error(err, "bound execution reservation verification failed")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
		}
		if err := r.verifyBoundExecution(ctx, task, existing); err != nil {
			log.Error(err, "bound execution verification failed")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
		}
		return ctrl.Result{}, nil, false
	}

	candidate, err := r.resolveAgentExecutionCandidate(ctx, task, agent)
	if err != nil {
		if isPermanentACPAgentConfigurationError(err) {
			result, failErr := r.failACPPlanningTask(
				ctx, task, corev1alpha1.TaskExecutionReason("InvalidRuntimeProfile"), err.Error(),
			)
			return result, failErr, true
		}
		log.Error(err, "agent execution candidate resolution failed")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
	}
	if err := r.persistAgentExecutionSnapshot(ctx, task, candidate); err != nil {
		log.Error(err, "immutable execution snapshot persistence failed")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
	}
	reservation, err := r.createAgentExecutionBindingReservation(ctx, task, &candidate.binding)
	if err != nil {
		log.Error(err, "durable execution binding reservation failed")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
	}
	binding, err := r.persistAgentExecutionBinding(ctx, task, candidate)
	if err != nil {
		conflict := &errAgentExecutionBindingConflict{}
		if errors.As(err, &conflict) {
			if settleErr := r.settleAgentExecutionBindingReservation(
				ctx, reservation, store.AgentExecutionBindingReservationRejected, "binding-conflict",
			); settleErr != nil {
				log.Error(settleErr, "failed to reject conflicting execution binding reservation")
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil, true
			}
			if r.Recorder != nil {
				r.Recorder.Eventf(task, corev1.EventTypeWarning, agentExecutionBindingConflictReason, "%s", err.Error())
			}
			result, failErr := r.failTask(ctx, task, err.Error())
			return result, failErr, true
		}
		log.Error(err, "write-once binding persistence failed")
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil, true
	}
	task.Status.AgentExecutionBinding = binding
	if err := r.settleAgentExecutionBindingReservation(
		ctx, reservation, store.AgentExecutionBindingReservationBound, "",
	); err != nil {
		log.Error(err, "failed to settle execution binding reservation after binding CAS")
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil, true
	}
	if err := r.verifyBoundAgentExecutionReservation(ctx, task, binding); err != nil {
		log.Error(err, "bound execution reservation verification failed after binding")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
	}
	if err := r.verifyBoundExecution(ctx, task, binding); err != nil {
		log.Error(err, "bound execution verification failed after binding")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
	}
	return ctrl.Result{}, nil, false
}
