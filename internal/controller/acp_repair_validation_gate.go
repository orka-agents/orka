/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/artifactcap"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
)

const (
	defaultRepairValidationPollInterval = 3 * time.Second
	// The validation Task's own timeout bounds the command; the slack covers
	// scheduling, image pulls, the checkout, and the network gate.
	repairValidationWaitSlack = 15 * time.Minute
	// Re-issue the bound download capability well inside its maximum lifetime
	// so kubelet's Secret propagation always delivers an unexpired token.
	repairValidationCapabilityRefresh = 2 * time.Minute
	// repairValidationCallTimeout bounds each API or storage call made
	// outside the validation wait, including recording the outcome after
	// that wait has expired.
	repairValidationCallTimeout = 30 * time.Second
)

// repairValidationCancelled marks a gate interrupted by Task cancellation;
// it is never recorded as a validation result.
const repairValidationCancelled tools.RepositoryRepairValidationStatus = "cancelled"

// repairValidationGateOutcome is the pre-publication verdict for one patch.
type repairValidationGateOutcome struct {
	required bool
	status   tools.RepositoryRepairValidationStatus
	evidence string
}

// repositoryMonitorRepairGateMonitor returns the RepositoryMonitor that gates
// task's publication, or nil when task is not a repository monitor repair or
// its monitor sets no repair command. A repair whose monitor cannot be read is
// gated and fails closed.
//
// A repair is recognised only from facts its editors cannot change: the
// immutable spec (an agent Task writing to an exact pull request head, which
// only repairs set) and the RepositoryMonitor controller reference, which
// admission holds. Labels and annotations are mutable metadata, so removing
// them can never disable the gate.
func (d *ACPDispatcher) repositoryMonitorRepairGateMonitor(ctx context.Context, task *corev1alpha1.Task) (*corev1alpha1.RepositoryMonitor, bool, string) {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent || task.Spec.Workspace == nil ||
		task.Spec.Workspace.Intent != corev1alpha1.WorkspaceIntentWrite ||
		strings.TrimSpace(task.Spec.Workspace.ExpectedRemoteSHA) == "" {
		return nil, false, ""
	}
	owner := metav1.GetControllerOf(task)
	if owner == nil || owner.APIVersion != corev1alpha1.GroupVersion.String() || owner.Kind != repositoryMonitorResourceKind {
		// Orphan deletion of a RepositoryMonitor drops the owner reference
		// but keeps the admission-held monitor markers. Such a repair can no
		// longer be checked against its monitor, so it fails closed.
		if taskAutomationMarker(task) == repositoryMonitorResourceKind {
			return nil, true, "The RepositoryMonitor that owns this repair is missing."
		}
		return nil, false, ""
	}
	monitor := &corev1alpha1.RepositoryMonitor{}
	if err := uncachedReader(d.APIReader, d.Client).Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: owner.Name}, monitor); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, true, "The RepositoryMonitor that owns this repair is missing."
		}
		return nil, true, "The RepositoryMonitor that owns this repair could not be read."
	}
	if monitor.UID != owner.UID {
		return nil, true, "The RepositoryMonitor that owns this repair was replaced."
	}
	if monitor.Spec.Validation.RepairCommand == "" {
		return nil, false, ""
	}
	return monitor, true, ""
}

// boundedRepairGateMonitor is repositoryMonitorRepairGateMonitor under its
// own call deadline, since the gate's context is detached from cancellation.
func (d *ACPDispatcher) boundedRepairGateMonitor(ctx context.Context, task *corev1alpha1.Task) (*corev1alpha1.RepositoryMonitor, bool, string) {
	callCtx, cancel := context.WithTimeout(ctx, repairValidationCallTimeout)
	defer cancel()
	return d.repositoryMonitorRepairGateMonitor(callCtx, task)
}

// recheckRepairValidationConfiguration keeps a pass only while the live
// RepositoryMonitor still requires the image and command it was validated
// with. A pass under a check the operator has since replaced does not
// satisfy the new one, so it fails closed.
func (d *ACPDispatcher) recheckRepairValidationConfiguration(
	ctx context.Context, task *corev1alpha1.Task, validated *corev1alpha1.RepositoryMonitor, outcome repairValidationGateOutcome,
) repairValidationGateOutcome {
	live, required, unavailable := d.boundedRepairGateMonitor(ctx, task)
	switch {
	case unavailable != "":
		return repairValidationUnavailable("%s", unavailable)
	case !required || live == nil:
		return repairValidationUnavailable("spec.validation.repairCommand was removed while the repair patch was being validated. Nothing was pushed.")
	case live.UID != validated.UID ||
		strings.TrimSpace(live.Spec.Validation.Image) != strings.TrimSpace(validated.Spec.Validation.Image) ||
		tools.RepositoryValidationCommandDigest(live.Spec.Validation.RepairCommand) != tools.RepositoryValidationCommandDigest(validated.Spec.Validation.RepairCommand):
		return repairValidationUnavailable("spec.validation changed while the repair patch was being validated. Nothing was pushed.")
	}
	return outcome
}

// gateRepositoryMonitorRepairPublication runs the operator's repair command
// against the exact pull request head plus this patch before anything is
// pushed (ADR 0038). It returns nil only when a passing result is durably
// recorded for the exact patch digest about to be published, or when no gate
// applies. Every other outcome is an *acpDeliveryError or
// errACPPublicationCancelled, which the caller handles without publishing.
func (d *ACPDispatcher) gateRepositoryMonitorRepairPublication(
	ctx context.Context, task *corev1alpha1.Task, attemptID string, fence store.ControllerEpochFence, delta harnessv2.WorkspaceDeltaDescriptor,
) (corev1alpha1.TaskDeliveryStatus, error) {
	ctx = context.WithoutCancel(ctx)
	monitor, required, unavailable := d.boundedRepairGateMonitor(ctx, task)
	if !required {
		return corev1alpha1.TaskDeliveryStatus{}, nil
	}
	outcome := repairValidationGateOutcome{required: true, status: tools.RepositoryRepairValidationUnavailable, evidence: unavailable}
	if unavailable == "" {
		outcome = d.runRepositoryMonitorRepairValidation(ctx, task, monitor, delta)
	}
	if outcome.status == tools.RepositoryRepairValidationPassed {
		outcome = d.recheckRepairValidationConfiguration(ctx, task, monitor, outcome)
	}
	logger := logf.FromContext(ctx)
	switch outcome.status {
	case tools.RepositoryRepairValidationPassed:
		logger.Info("repair patch passed pre-publication validation", "namespace", task.Namespace, "task", task.Name)
		return corev1alpha1.TaskDeliveryStatus{}, nil
	case repairValidationCancelled:
		status := corev1alpha1.TaskDeliveryStatus{
			State: corev1alpha1.TaskDeliveryStateCancelledBeforePublish, Outcome: corev1alpha1.TaskDeliveryOutcomeCancelledBeforePublish,
			Message: "cancellation was requested while the repair patch was being validated; nothing was pushed", LastTransitionTime: nowMeta(),
		}
		if err := d.transitionDelivery(ctx, attemptID, fence, store.PromptDeliveryValidating, store.PromptDeliveryCancelledBeforePublish, "cancel-before-publish", status.Message); err != nil {
			return corev1alpha1.TaskDeliveryStatus{}, err
		}
		return status, errACPPublicationCancelled
	case tools.RepositoryRepairValidationFailed:
		logger.Info("repair patch failed pre-publication validation; not publishing", "namespace", task.Namespace, "task", task.Name)
		return corev1alpha1.TaskDeliveryStatus{}, &acpDeliveryError{
			state: corev1alpha1.TaskDeliveryStateDeliveryConflict, outcome: corev1alpha1.TaskDeliveryOutcomeDeliveryConflict,
			reason: repositoryMonitorRepairValidationFailedDeliveryReason, message: repairValidationDeliveryMessage(outcome.evidence),
		}
	default:
		logger.Info("repair patch validation was unavailable; failing closed", "namespace", task.Namespace, "task", task.Name)
		return corev1alpha1.TaskDeliveryStatus{}, &acpDeliveryError{
			state: corev1alpha1.TaskDeliveryStateDeliveryConflict, outcome: corev1alpha1.TaskDeliveryOutcomeDeliveryConflict,
			reason: repositoryMonitorRepairValidationUnavailableDeliveryReason, message: repairValidationDeliveryMessage(outcome.evidence),
		}
	}
}

// repairValidationDeliveryMessageLimit keeps the evidence inside the Task
// status.delivery.message schema bound (1024 characters).
const repairValidationDeliveryMessageLimit = 960

func repairValidationDeliveryMessage(evidence string) string {
	evidence = strings.TrimSpace(evidence)
	if evidence == "" {
		evidence = "The repair patch did not pass validation. Nothing was pushed."
	}
	if len([]rune(evidence)) <= repairValidationDeliveryMessageLimit {
		return evidence
	}
	return string([]rune(evidence)[:repairValidationDeliveryMessageLimit]) + " [truncated]"
}

func repairValidationUnavailable(format string, args ...any) repairValidationGateOutcome {
	return repairValidationGateOutcome{
		required: true, status: tools.RepositoryRepairValidationUnavailable,
		evidence: boundRepositoryMonitorValidationEvidence(fmt.Sprintf(format, args...)),
	}
}

//nolint:gocyclo // The ordered bind/create/wait/record steps are easier to audit together.
func (d *ACPDispatcher) runRepositoryMonitorRepairValidation(
	ctx context.Context, task *corev1alpha1.Task, monitor *corev1alpha1.RepositoryMonitor, delta harnessv2.WorkspaceDeltaDescriptor,
) repairValidationGateOutcome {
	if d.RepositoryValidationBindings == nil || d.Client == nil || len(d.ArtifactCapabilitySecret) < artifactcap.MinSecretBytes || d.ArtifactReservations == nil {
		return repairValidationUnavailable("Repair validation is not configured on this controller.")
	}
	if delta.Artifact == nil || delta.Artifact.MediaType != artifactcap.MediaTypeWorkspaceDelta {
		return repairValidationUnavailable("The repair patch artifact is unavailable.")
	}
	interval := d.RepairValidationPollInterval
	if interval <= 0 {
		interval = defaultRepairValidationPollInterval
	}
	wait := d.RepairValidationWaitTimeout
	if wait <= 0 {
		wait = tools.RepositoryValidationTimeout + repairValidationWaitSlack
	}
	deadline := time.Now().Add(wait)
	// Every call until the verdict runs under the wait deadline, so a
	// stalled API server cannot hold the runtime session past it. The
	// verdict is recorded under a separate bound once that deadline passes.
	runCtx, cancelRun := context.WithDeadline(ctx, deadline)
	defer cancelRun()
	record := func(validationTask *corev1alpha1.Task, outcome repairValidationGateOutcome, binding *store.MonitorEvent, input tools.RepositoryRepairValidationInput) repairValidationGateOutcome {
		recordCtx, cancel := context.WithTimeout(ctx, repairValidationCallTimeout)
		defer cancel()
		return d.recordRepairValidationOutcome(recordCtx, binding, validationTask, input, outcome)
	}

	command := monitor.Spec.Validation.RepairCommand
	input := tools.RepositoryRepairValidationInput{
		Monitor: monitor, RepairTask: task, Image: strings.TrimSpace(monitor.Spec.Validation.Image),
		Command: command, DeltaDigest: delta.Artifact.Digest, RelativeRoot: delta.RelativeRoot,
	}
	validationTask, err := tools.BuildRepositoryRepairValidationTask(input)
	if err != nil {
		return repairValidationUnavailable("The repair validation request is invalid: %v.", err)
	}
	binding, err := tools.RepositoryRepairValidationBindingEvent(input, validationTask)
	if err != nil {
		return repairValidationUnavailable("The repair validation binding is invalid: %v.", err)
	}
	if err := tools.EnsureRepositoryRepairValidationBinding(runCtx, d.RepositoryValidationBindings, binding); err != nil {
		return repairValidationUnavailable("The repair validation binding could not be persisted.")
	}
	if recorded, err := tools.FindRepositoryRepairValidationResult(runCtx, d.RepositoryValidationBindings, task.Namespace, validationTask.Name); err != nil {
		return repairValidationUnavailable("The recorded repair validation result could not be read.")
	} else if recorded != nil {
		verifyCtx, cancel := context.WithTimeout(ctx, repairValidationCallTimeout)
		defer cancel()
		return d.verifiedRepairValidationOutcome(verifyCtx, validationTask.Name, delta.Artifact.Digest, recorded)
	}
	if err := tools.EnsureRepositoryRepairValidationCommandSecret(runCtx, d.Client, tools.BuildRepositoryRepairValidationCommandSecret(task, validationTask, command)); err != nil {
		return repairValidationUnavailable("The repair validation command Secret could not be persisted.")
	}
	if err := d.Client.Create(runCtx, validationTask); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return repairValidationUnavailable("The repair validation Task could not be created.")
		}
		existing := &corev1alpha1.Task{}
		if err := uncachedReader(d.APIReader, d.Client).Get(runCtx, client.ObjectKeyFromObject(validationTask), existing); err != nil {
			return repairValidationUnavailable("The existing repair validation Task could not be read.")
		}
		if !tools.RepositoryRepairValidationTaskMatches(existing, input) {
			return repairValidationUnavailable("An existing repair validation Task does not match this repair.")
		}
		validationTask = existing
	}
	if validationTask.UID == "" {
		return repairValidationUnavailable("The repair validation Task has no identity to bind its patch capability to.")
	}

	var lastIssued time.Time
	capabilityDone := false
	for {
		current := &corev1alpha1.Task{}
		if err := uncachedReader(d.APIReader, d.Client).Get(runCtx, client.ObjectKeyFromObject(validationTask), current); err != nil {
			if apierrors.IsNotFound(err) {
				return record(validationTask, repairValidationUnavailable("The repair validation Task disappeared before it finished."), binding, input)
			}
		} else {
			if current.UID != validationTask.UID {
				return repairValidationUnavailable("The repair validation Task was replaced while it ran.")
			}
			if current.Status.Phase.IsTerminal() {
				return record(current, classifyRepairValidationTask(current, command), binding, input)
			}
			if !capabilityDone {
				capabilityDone = d.repairValidationPatchDownloadStarted(runCtx, current)
			}
			if !capabilityDone && (lastIssued.IsZero() || time.Since(lastIssued) >= repairValidationCapabilityRefresh) {
				if err := d.issueRepairValidationCapability(runCtx, current, delta.Artifact); err != nil {
					logf.FromContext(ctx).Error(err, "issue repair validation patch capability", "namespace", current.Namespace, "task", current.Name)
				} else {
					lastIssued = time.Now()
				}
			}
		}
		if cancelled, err := d.taskCancellationRequested(runCtx, task); err == nil && cancelled {
			return repairValidationGateOutcome{required: true, status: repairValidationCancelled}
		}
		if runCtx.Err() != nil || time.Now().After(deadline) {
			return record(validationTask, repairValidationUnavailable("The repair validation Task did not finish within %s.", wait), binding, input)
		}
		timer := time.NewTimer(interval)
		select {
		case <-runCtx.Done():
		case <-timer.C:
		}
		timer.Stop()
	}
}

// repairValidationPatchDownloadStarted reports whether the validation Pod's
// patch container has already started or finished, after which no further
// capability needs to be issued.
func (d *ACPDispatcher) repairValidationPatchDownloadStarted(ctx context.Context, validationTask *corev1alpha1.Task) bool {
	var pods corev1.PodList
	if err := uncachedReader(d.APIReader, d.Client).List(ctx, &pods, client.InNamespace(validationTask.Namespace),
		client.MatchingLabels{labels.LabelTask: labels.SelectorValue(validationTask.Name)}); err != nil {
		return false
	}
	for i := range pods.Items {
		for _, status := range pods.Items[i].Status.InitContainerStatuses {
			if status.Name == repositoryMonitorRepairValidationPatchContainer && status.State.Terminated != nil {
				return true
			}
		}
	}
	return false
}

// issueRepairValidationCapability stores a fresh capability for the one bound
// patch download. The request (digest, Task UID, operation ID) never changes,
// so the artifact ledger still admits a single download however many tokens
// are issued.
func (d *ACPDispatcher) issueRepairValidationCapability(ctx context.Context, validationTask *corev1alpha1.Task, artifact *harnessv2.ArtifactReference) error {
	request := artifactcap.OperationRequest{
		Operation:     artifactcap.OperationDownload,
		ObjectDigest:  artifact.Digest,
		Identity:      artifactcap.Identity{Namespace: validationTask.Namespace, TaskID: string(validationTask.UID)},
		ContentLength: artifact.SizeBytes,
		MediaType:     artifact.MediaType,
		OperationID:   "repair-validation-patch-" + string(validationTask.UID),
	}
	issuedAt := time.Now().UTC()
	authorization, err := artifactcap.Issue(d.ArtifactCapabilitySecret, request, issuedAt, artifactcap.MaxCapabilityTTL)
	if err != nil {
		return fmt.Errorf("authorize repair patch download: %w", err)
	}
	if err := d.ArtifactReservations.Reserve(ctx, request, issuedAt.Add(artifactcap.MaxCapabilityTTL+artifactcap.MaxClockSkew)); err != nil {
		return fmt.Errorf("reserve repair patch download: %w", err)
	}
	document, err := json.Marshal(tools.RepositoryRepairValidationCapability{
		Capability: authorization.Capability, RequestDigest: authorization.RequestDigest,
		Request: tools.RepositoryRepairPatchRequest{
			Operation: string(request.Operation), ObjectDigest: request.ObjectDigest,
			Namespace: request.Identity.Namespace, TaskID: request.Identity.TaskID,
			ContentLength: request.ContentLength, MediaType: request.MediaType, OperationID: request.OperationID,
		},
	})
	if err != nil {
		return err
	}
	return tools.EnsureRepositoryRepairValidationCapabilitySecret(ctx, d.Client, validationTask, document)
}

// classifyRepairValidationTask maps a terminal validation Task to a verdict
// the same way review validation does: only a command that ran and failed is
// a failure; anything else that did not succeed could not validate.
func classifyRepairValidationTask(task *corev1alpha1.Task, command string) repairValidationGateOutcome {
	message := strings.TrimSpace(task.Status.Message)
	switch {
	case task.Status.Phase == corev1alpha1.TaskPhaseSucceeded:
		return repairValidationGateOutcome{required: true, status: tools.RepositoryRepairValidationPassed}
	case task.Status.Phase == corev1alpha1.TaskPhaseFailed && task.Status.ExecutionOutcome != nil &&
		task.Status.ExecutionOutcome.Phase == corev1alpha1.TaskPhaseFailed:
		evidence := fmt.Sprintf("The repair patch did not pass spec.validation.repairCommand %q in validation Task %s. Nothing was pushed.", command, task.Name)
		if message != "" {
			evidence += "\n" + message
		}
		return repairValidationGateOutcome{required: true, status: tools.RepositoryRepairValidationFailed, evidence: boundRepositoryMonitorValidationEvidence(evidence)}
	default:
		evidence := fmt.Sprintf("The repair validation Task %s could not run spec.validation.repairCommand and ended in phase %s. Nothing was pushed.", task.Name, task.Status.Phase)
		if message != "" {
			evidence += "\n" + message
		}
		return repairValidationGateOutcome{required: true, status: tools.RepositoryRepairValidationUnavailable, evidence: boundRepositoryMonitorValidationEvidence(evidence)}
	}
}

// recordRepairValidationOutcome appends the verdict against the exact patch
// digest and then re-reads it, so publication only follows a durable pass.
func (d *ACPDispatcher) recordRepairValidationOutcome(
	ctx context.Context, binding *store.MonitorEvent, validationTask *corev1alpha1.Task,
	input tools.RepositoryRepairValidationInput, outcome repairValidationGateOutcome,
) repairValidationGateOutcome {
	result := tools.RepositoryRepairValidationResult{
		RepairTaskName: input.RepairTask.Name, RepairTaskUID: string(input.RepairTask.UID),
		ValidationTaskName: validationTask.Name, ValidationTaskUID: string(validationTask.UID),
		PatchDigest: input.DeltaDigest, CommandDigest: tools.RepositoryValidationCommandDigest(input.Command),
		Image: input.Image, Status: outcome.status, Evidence: outcome.evidence,
	}
	if err := tools.RecordRepositoryRepairValidationResult(ctx, d.RepositoryValidationBindings, binding, result); err != nil {
		return repairValidationUnavailable("The repair validation result could not be recorded.")
	}
	recorded, err := tools.FindRepositoryRepairValidationResult(ctx, d.RepositoryValidationBindings, validationTask.Namespace, validationTask.Name)
	if err != nil || recorded == nil {
		return repairValidationUnavailable("The recorded repair validation result could not be read back.")
	}
	return d.verifiedRepairValidationOutcome(ctx, validationTask.Name, input.DeltaDigest, recorded)
}

func (d *ACPDispatcher) verifiedRepairValidationOutcome(_ context.Context, validationTaskName, patchDigest string, recorded *tools.RepositoryRepairValidationResult) repairValidationGateOutcome {
	if recorded.ValidationTaskName != validationTaskName || recorded.PatchDigest != patchDigest {
		return repairValidationUnavailable("The recorded repair validation result is for a different patch.")
	}
	switch recorded.Status {
	case tools.RepositoryRepairValidationPassed, tools.RepositoryRepairValidationFailed, tools.RepositoryRepairValidationUnavailable:
		evidence := recorded.Evidence
		if evidence == "" && recorded.Status != tools.RepositoryRepairValidationPassed {
			evidence = "The repair patch did not pass validation. Nothing was pushed."
		}
		return repairValidationGateOutcome{required: true, status: recorded.Status, evidence: evidence}
	default:
		return repairValidationUnavailable("The recorded repair validation result is invalid.")
	}
}
