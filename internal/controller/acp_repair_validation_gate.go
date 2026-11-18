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
func (d *ACPDispatcher) repositoryMonitorRepairGateMonitor(ctx context.Context, task *corev1alpha1.Task) (*corev1alpha1.RepositoryMonitor, bool, string) {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent || task.Spec.Workspace == nil ||
		task.Spec.Workspace.Intent != corev1alpha1.WorkspaceIntentWrite ||
		task.Labels[labels.LabelCreatedBy] != repositoryMonitorTaskCreatedBy ||
		strings.TrimSpace(task.Annotations[repositoryMonitorIssueAnnotationCommandID]) == "" ||
		task.Annotations[labels.AnnotationMonitorItemKind] != repositoryMonitorPullRequestKind {
		return nil, false, ""
	}
	owner := metav1.GetControllerOf(task)
	if owner == nil || owner.APIVersion != corev1alpha1.GroupVersion.String() || owner.Kind != repositoryMonitorResourceKind {
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
	monitor, required, unavailable := d.repositoryMonitorRepairGateMonitor(ctx, task)
	if !required {
		return corev1alpha1.TaskDeliveryStatus{}, nil
	}
	outcome := repairValidationGateOutcome{required: true, status: tools.RepositoryRepairValidationUnavailable, evidence: unavailable}
	if unavailable == "" {
		outcome = d.runRepositoryMonitorRepairValidation(ctx, task, monitor, delta)
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
	if err := tools.EnsureRepositoryRepairValidationBinding(ctx, d.RepositoryValidationBindings, binding); err != nil {
		return repairValidationUnavailable("The repair validation binding could not be persisted.")
	}
	if recorded, err := tools.FindRepositoryRepairValidationResult(ctx, d.RepositoryValidationBindings, task.Namespace, validationTask.Name); err != nil {
		return repairValidationUnavailable("The recorded repair validation result could not be read.")
	} else if recorded != nil {
		return d.verifiedRepairValidationOutcome(ctx, validationTask.Name, delta.Artifact.Digest, recorded)
	}
	if err := tools.EnsureRepositoryRepairValidationCommandSecret(ctx, d.Client, tools.BuildRepositoryRepairValidationCommandSecret(task, validationTask, command)); err != nil {
		return repairValidationUnavailable("The repair validation command Secret could not be persisted.")
	}
	if err := d.Client.Create(ctx, validationTask); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return repairValidationUnavailable("The repair validation Task could not be created.")
		}
		existing := &corev1alpha1.Task{}
		if err := uncachedReader(d.APIReader, d.Client).Get(ctx, client.ObjectKeyFromObject(validationTask), existing); err != nil {
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

	interval := d.RepairValidationPollInterval
	if interval <= 0 {
		interval = defaultRepairValidationPollInterval
	}
	wait := d.RepairValidationWaitTimeout
	if wait <= 0 {
		wait = tools.RepositoryValidationTimeout + repairValidationWaitSlack
	}
	deadline := time.Now().Add(wait)
	var lastIssued time.Time
	capabilityDone := false
	for {
		current := &corev1alpha1.Task{}
		if err := uncachedReader(d.APIReader, d.Client).Get(ctx, client.ObjectKeyFromObject(validationTask), current); err != nil {
			if apierrors.IsNotFound(err) {
				return d.recordRepairValidationOutcome(ctx, binding, validationTask, input, repairValidationUnavailable("The repair validation Task disappeared before it finished."))
			}
		} else {
			if current.UID != validationTask.UID {
				return repairValidationUnavailable("The repair validation Task was replaced while it ran.")
			}
			if current.Status.Phase.IsTerminal() {
				return d.recordRepairValidationOutcome(ctx, binding, current, input, classifyRepairValidationTask(current, command))
			}
			if !capabilityDone {
				capabilityDone = d.repairValidationPatchDownloadStarted(ctx, current)
			}
			if !capabilityDone && (lastIssued.IsZero() || time.Since(lastIssued) >= repairValidationCapabilityRefresh) {
				if err := d.issueRepairValidationCapability(ctx, current, delta.Artifact); err != nil {
					logf.FromContext(ctx).Error(err, "issue repair validation patch capability", "namespace", current.Namespace, "task", current.Name)
				} else {
					lastIssued = time.Now()
				}
			}
		}
		if cancelled, err := d.taskCancellationRequested(ctx, task); err == nil && cancelled {
			return repairValidationGateOutcome{required: true, status: repairValidationCancelled}
		}
		if time.Now().After(deadline) {
			return d.recordRepairValidationOutcome(ctx, binding, validationTask, input, repairValidationUnavailable("The repair validation Task did not finish within %s.", wait))
		}
		timer := time.NewTimer(interval)
		<-timer.C
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
