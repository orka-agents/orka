package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/store"
	"k8s.io/apimachinery/pkg/types"
)

// recoverAbandonedWorkspaceValidation always constructs the exact, epoch-guarded
// cleanup transport, including when the runtime and owner epochs still match.
// The ordinary admission client must never carry these recovery mutations.
func (d *ACPDispatcher) recoverAbandonedWorkspaceValidation(ctx context.Context, task *corev1alpha1.Task, taskUID types.UID, pool *corev1alpha1.RuntimePool) error {
	if d.Epochs == nil {
		return fmt.Errorf("%w: abandoned validation requires current cleanup ownership", store.ErrConflict)
	}
	owner, err := d.Epochs.CurrentFence(ctx)
	if err != nil {
		return err
	}
	runtimeClient, runtimeFence, err := d.runtimePoolRetainedCleanupClient(ctx, task, taskUID, pool, owner, false)
	if err != nil {
		return err
	}
	return d.finishAbandonedWorkspaceValidation(ctx, task, taskUID, runtimeClient, runtimeFence)
}

// finishAbandonedWorkspaceValidation finishes the existing validation barrier
// when a native prompt completed after its controller lost the result. It never
// upgrades that Task's durable terminal outcome, fetches/clones a repository,
// publishes a delta, or invents runtime retirement evidence. Destructive cleanup
// still requires the normal exact-session DELETE and its authenticated receipt.
//
//nolint:gocyclo // Keep the terminal-authority checks and ordered non-publishing recovery steps in one auditable path.
func (d *ACPDispatcher) finishAbandonedWorkspaceValidation(
	ctx context.Context, task *corev1alpha1.Task, taskUID types.UID,
	runtimeClient *harnessv2.Client, runtimeFence harnessv2.Fence,
) error {
	if d.Epochs == nil || d.Store == nil || runtimeClient == nil || task == nil || task.Status.Execution == nil {
		return fmt.Errorf("%w: abandoned validation requires complete recovery authority", store.ErrConflict)
	}
	execution := task.Status.Execution
	attemptID, err := promptAttemptIDFromTaskUID(task, taskUID)
	if err != nil {
		return err
	}
	attempt, err := d.Store.GetPromptAttempt(ctx, attemptID)
	if err != nil {
		return err
	}
	switch attempt.ExecutionState {
	case store.PromptExecutionFailed, store.PromptExecutionCancelled, store.PromptExecutionOutcomeUnknown:
	default:
		return fmt.Errorf("%w: live or successful execution cannot abandon workspace validation", store.ErrConflict)
	}
	binding := executionBinding(task, corev1alpha1.AgentRuntimeContractHarnessV2)
	if binding == nil || attempt.Key.TaskUID != string(taskUID) || attempt.Key.Attempt != int64(execution.Attempt) ||
		attempt.Key.PromptID != execution.PromptID || attempt.RequestDigest != execution.RequestDigest ||
		attempt.BindingDigest != binding.BindingDigest || attempt.SnapshotDigest != binding.Snapshot.Digest ||
		attempt.RuntimeInstanceID != execution.RuntimeInstanceID ||
		corev1alpha1.TaskExecutionState(attempt.ExecutionState) != execution.State ||
		attempt.DeliveryState != store.PromptDeliveryNotRequested || task.Status.Delivery == nil ||
		task.Status.Delivery.State != corev1alpha1.TaskDeliveryStateNotRequested {
		return fmt.Errorf("%w: abandoned validation does not match the terminal Task attempt", store.ErrConflict)
	}
	if runtimeFence.RuntimeInstanceID != harnessv2.RuntimeInstanceID(execution.RuntimeInstanceID) || runtimeFence.SupervisorBootID != harnessv2.SupervisorBootID(execution.RuntimeSessionSupervisorBootID) ||
		runtimeFence.RuntimeSessionUID != harnessv2.RuntimeSessionUID(execution.RuntimeSessionUID) || runtimeFence.RuntimeSessionGeneration != uint64(execution.RuntimeSessionGeneration) ||
		runtimeFence.RuntimePoolUID != harnessv2.RuntimePoolUID(execution.RuntimePoolUID) || runtimeFence.RuntimeProfileDigest != harnessv2.ProfileDigest(binding.RuntimeProfileDigest) ||
		(task.Spec.SessionRef != nil && (attempt.SessionUID != execution.RuntimeSessionUID || attempt.SessionLeaseGeneration < 1)) {
		return fmt.Errorf("%w: abandoned validation runtime/session fence changed", store.ErrConflict)
	}
	baseline, workspace, err := d.recoveredValidationWorkspace(ctx, task, taskUID)
	if err != nil {
		return err
	}
	fence, err := d.Epochs.CurrentFence(ctx)
	if err != nil {
		return err
	}
	if runtimeFence.ControllerEpoch == 0 || runtimeFence.ControllerEpoch > uint64(fence.Epoch) {
		return fmt.Errorf("%w: abandoned validation runtime epoch is outside current cleanup authority", store.ErrConflict)
	}
	sessionID := harnessv2.RuntimeSessionID(runtimeSessionID(runtimeFence))
	now := time.Now().UTC()
	request := harnessv2.CancelPromptRequest{
		Protocol: harnessv2.ProtocolVersion,
		Metadata: mutationMetadataForTaskUID(runtimeFence, task, taskUID, "rc-"+strconv.FormatInt(now.UnixNano(), 36), true, now.Add(30*time.Second)),
		Reason:   harnessv2.CancelReasonStreamDisconnected, SettlementDeadline: now.Add(20 * time.Second),
	}
	if err := sealMutation(&request.Metadata.RequestDigest, request); err != nil {
		return err
	}
	if err := requireAgentRuntimeRecoveryFence(ctx, d.Store, fence); err != nil {
		return err
	}
	response, err := runtimeClient.CancelPrompt(ctx, sessionID, request)
	if err != nil {
		return fmt.Errorf("observe abandoned prompt settlement: %w", err)
	}
	if !response.SettlementProven || response.Settlement.TerminalEvent != harnessv2.EventCompleted {
		return fmt.Errorf("%w: abandoned validation lacks an exact completed settlement", store.ErrNotReady)
	}
	digest, err := harnessv2.CanonicalPromptSettlementDigest(response.Settlement)
	if err != nil {
		return err
	}
	deltaRequest := harnessv2.CreateWorkspaceDeltaRequest{
		Protocol: harnessv2.ProtocolVersion,
		Metadata: mutationMetadataForTaskUID(runtimeFence, task, taskUID, "rv-"+strconv.FormatInt(time.Now().UnixNano(), 36), true, time.Now().UTC().Add(30*time.Second)),
		DeltaID:  harnessv2.WorkspaceDeltaID("delta-" + execution.PromptID), Intent: workspace.Intent,
		VerifiedBaseline: baseline, PromptSettlementDigest: digest, Limits: acpWorkspaceDeltaLimits(task),
	}
	if err := sealMutation(&deltaRequest.Metadata.RequestDigest, deltaRequest); err != nil {
		return err
	}
	if err := requireAgentRuntimeRecoveryFence(ctx, d.Store, fence); err != nil {
		return err
	}
	delta, err := runtimeClient.CreateWorkspaceDelta(ctx, sessionID, deltaRequest)
	if err != nil {
		return fmt.Errorf("validate abandoned workspace: %w", err)
	}
	if delta.Delta.State == harnessv2.WorkspaceDeltaPrepared {
		// Only local, unexported work is abandoned. A published/in-flight
		// publication cannot reach this path: its durable delivery state is
		// not NotRequested and its runtime is no longer Validating.
		finalization, err := abandonedValidationFinalization(task, taskUID, delta.Delta.DeltaID)
		if err != nil {
			return err
		}
		if err := requireAgentRuntimeRecoveryFence(ctx, d.Store, fence); err != nil {
			return err
		}
		if err := d.finalizeRuntimeSessionPublicationForTaskUID(ctx, runtimeClient, sessionID, task, taskUID, runtimeFence, finalization); err != nil {
			return err
		}
	}
	return requireAgentRuntimeRecoveryFence(ctx, d.Store, fence)
}

// recoveredValidationWorkspace reconstructs only a previously committed
// workspace baseline. Its digest must equal the admitted runtime binding. A
// deleted branch, changed Agent, or expired Git credential is not consulted.
func (d *ACPDispatcher) recoveredValidationWorkspace(ctx context.Context, task *corev1alpha1.Task, taskUID types.UID) (harnessv2.WorkspaceBaseline, harnessv2.WorkspaceSpec, error) {
	fail := func(err error) (harnessv2.WorkspaceBaseline, harnessv2.WorkspaceSpec, error) {
		return harnessv2.WorkspaceBaseline{}, harnessv2.WorkspaceSpec{}, err
	}
	if task == nil || task.Status.Execution == nil || d.Snapshots == nil {
		return fail(fmt.Errorf("%w: workspace recovery requires an immutable execution snapshot", store.ErrConflict))
	}
	binding := executionBinding(task, corev1alpha1.AgentRuntimeContractHarnessV2)
	if binding == nil || binding.Task.UID != taskUID || binding.Backend != corev1alpha1.AgentExecutionBackendRuntimePool {
		return fail(fmt.Errorf("%w: abandoned validation requires a built-in runtime binding", store.ErrConflict))
	}
	digest, err := canonicalAgentExecutionBindingDigest(*binding)
	if err != nil || digest != binding.BindingDigest {
		return fail(fmt.Errorf("%w: workspace recovery binding integrity failed", store.ErrConflict))
	}
	snapshot, err := d.Snapshots.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{TaskUID: string(taskUID), Digest: binding.Snapshot.Digest})
	if err != nil {
		return fail(err)
	}
	body, err := decodeAgentExecutionSnapshot(snapshot.Body)
	if err != nil {
		return fail(err)
	}
	_, _, _, err = validateAgentExecutionSnapshot(binding, snapshot, body)
	if err != nil {
		return fail(err)
	}
	if body.ExecutionWorkspace != nil || body.PoolName != task.Status.Execution.RuntimePoolName || body.ProfileDigest != binding.RuntimeProfileDigest {
		return fail(fmt.Errorf("%w: workspace recovery does not match the original managed pool", store.ErrConflict))
	}
	frozen := task.DeepCopy()
	frozen.Spec.Workspace = body.Workspace.DeepCopy()
	frozen.Spec.SessionRef = body.SessionRef.DeepCopy()
	sourceRef := ""
	var baseline harnessv2.WorkspaceBaseline
	var workspace harnessv2.WorkspaceSpec
	if body.Workspace == nil || strings.TrimSpace(body.Workspace.GitRepo) == "" {
		scope := string(taskUID)
		if body.SessionRef != nil {
			scope = task.Status.Execution.RuntimeSessionUID
		}
		baseline, workspace, err = emptyRuntimeWorkspace(frozen, scope)
		if err != nil {
			return fail(err)
		}
	} else {
		var prepared publisherservice.WorkspacePrepareResponse
		identity := store.ExternalEffectIdentity{Kind: "workspace.prepare", Namespace: task.Namespace, AggregateID: string(taskUID), OperationID: "workspace-prepare-" + task.Status.Execution.PromptID}
		if err := readCommittedWorkspacePreparation(ctx, d.Store, identity, &prepared); err != nil {
			return fail(err)
		}
		if prepared.OperationID != identity.OperationID || prepared.RepositoryID == "" || prepared.SourceRef == "" || prepared.BaselineOID == "" {
			return fail(fmt.Errorf("%w: committed workspace preparation identity is incomplete", store.ErrConflict))
		}
		baseline = harnessv2.WorkspaceBaseline{RepositoryIdentity: prepared.RepositoryID, Revision: prepared.BaselineOID, TreeDigest: prepared.ManifestDigest, Artifact: &prepared.Artifact}
		workspace = harnessv2.WorkspaceSpec{Intent: harnessv2.WorkspaceIntent(effectiveACPWorkspaceIntent(frozen)), Baseline: baseline, RelativeRoot: strings.TrimSpace(body.Workspace.SubPath)}
		sourceRef = prepared.SourceRef
	}
	if err := workspace.Validate(); err != nil {
		return fail(err)
	}
	digest, err = acpRuntimeWorkspaceBindingDigest(sourceRef, workspace)
	if err != nil || digest != task.Status.Execution.RuntimeSessionWorkspaceDigest {
		return fail(fmt.Errorf("%w: recovered baseline differs from the frozen runtime workspace", store.ErrConflict))
	}
	return baseline, workspace, nil
}

func readCommittedWorkspacePreparation(ctx context.Context, effects store.ExternalEffectStore, identity store.ExternalEffectIdentity, into *publisherservice.WorkspacePrepareResponse) error {
	if effects == nil {
		return errors.New("workspace recovery requires durable preparation evidence")
	}
	id, err := identity.CanonicalID()
	if err != nil {
		return err
	}
	effect, err := effects.GetExternalEffect(ctx, id)
	if err != nil {
		return err
	}
	if effect == nil || effect.Identity != identity || effect.State != store.ExternalEffectSucceeded || len(effect.Response) == 0 {
		return fmt.Errorf("%w: workspace preparation evidence is missing or corrupt", store.ErrConflict)
	}
	decoder := json.NewDecoder(bytes.NewReader(effect.Response))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("decode committed workspace preparation: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("%w: workspace preparation has trailing JSON", store.ErrConflict)
	}
	if effect.ResponseDigest != store.CanonicalBytesDigest(effect.Response) {
		// The producer commits json.Marshal(WorkspacePrepareResponse), but
		// Kubernetes stores this field as JSON and may reorder object keys.
		// Reconstruct that exact, closed producer schema rather than dropping
		// digest verification or accepting arbitrary semantic normalization.
		original, err := json.Marshal(into)
		if err != nil || effect.ResponseDigest != store.CanonicalBytesDigest(original) {
			return fmt.Errorf("%w: workspace preparation evidence is missing or corrupt", store.ErrConflict)
		}
	}
	return nil
}

func abandonedValidationFinalization(task *corev1alpha1.Task, taskUID types.UID, deltaID harnessv2.WorkspaceDeltaID) (runtimeSessionPublicationFinalization, error) {
	return runtimeSessionDeltaAbandonmentFinalizationForTaskUID(task, taskUID, deltaID, corev1alpha1.TaskDeliveryStatus{
		State: corev1alpha1.TaskDeliveryStateDeliveryConflict, Outcome: corev1alpha1.TaskDeliveryOutcomeDeliveryConflict,
		Reason: "ExecutionResultUnavailable", Message: "terminal execution has no recoverable result; workspace was not published",
	})
}

func abandonedValidationRecoveryTask(task *corev1alpha1.Task) bool {
	return task != nil && task.Status.Execution != nil && task.Status.Execution.State == corev1alpha1.TaskExecutionStateOutcomeUnknown && task.Status.Delivery != nil && task.Status.Delivery.State == corev1alpha1.TaskDeliveryStateNotRequested
}
