/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/operatoraccess"
)

// ADR 0057: a Task that will attach an unattached Ready workspace it alone
// demands (a Session continuation's cold resume, or the first turn of a new
// workspace) may let the provider start the runtime before the attachment
// handshake completes. The start is recorded on the workspace as an absolute,
// never-extended deadline plus the requesting Task UID. While it is open the
// linked RuntimePool keeps its replicas without an attachment; the runtime
// still cold-boots with rotated credentials and passes every startup
// attestation, and no prompt reaches it until the Task holds the attachment
// and the post-attach handshake passes.

const (
	// acpWorkspaceSpeculativeStartAnnotation records one speculative start as
	// "<RFC3339Nano deadline> <Task UID>".
	acpWorkspaceSpeculativeStartAnnotation = "acp.workspace.orka.ai/speculative-start"
	// acpWorkspaceSpeculativeStartTTL bounds how long an unattached
	// speculative runtime may run. A Task normally attaches within seconds;
	// the bound covers a controller restart without letting an unclaimed
	// runtime idle on pool credentials.
	acpWorkspaceSpeculativeStartTTL = 2 * time.Minute
)

func formatACPWorkspaceSpeculativeStart(deadline time.Time, taskUID types.UID) string {
	return deadline.UTC().Format(time.RFC3339Nano) + " " + string(taskUID)
}

// parseACPWorkspaceSpeculativeStart returns the deadline and requesting Task
// UID of a recorded speculative start.
func parseACPWorkspaceSpeculativeStart(raw string) (time.Time, string, bool) {
	fields := strings.Fields(raw)
	if len(fields) != 2 || fields[1] == "" {
		return time.Time{}, "", false
	}
	deadline, err := time.Parse(time.RFC3339Nano, fields[0])
	if err != nil {
		return time.Time{}, "", false
	}
	return deadline, fields[1], true
}

// acpWorkspaceSpeculativeStartAllowed is the workspace state in which an
// unattached runtime may run for a pending requester: desired Ready, no
// attachment, outstanding demand, no pending settlement, and not Failed,
// quarantined, or tainted.
func acpWorkspaceSpeculativeStartAllowed(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if workspace == nil || !workspace.DeletionTimestamp.IsZero() ||
		workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady ||
		workspace.Spec.Attachment != nil ||
		strings.TrimSpace(workspace.Annotations[acpWorkspaceResumeRequestedAnnotation]) == "" ||
		strings.TrimSpace(workspace.Annotations[acpWorkspaceRevocationStartedAnnotation]) != "" {
		return false
	}
	return workspace.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed &&
		!acpWorkspaceQuarantined(workspace) && !operatoraccess.Tainted(workspace.Annotations)
}

// acpWorkspaceSpeculativeStartHoldsPool is the RuntimePool demand rule for a
// speculative start: the pool keeps its replicas until the recorded deadline
// while the workspace stays in the allowed state. It returns the deadline so
// the caller can requeue at expiry.
func acpWorkspaceSpeculativeStartHoldsPool(workspace *workspacev1alpha1.ExecutionWorkspace, now time.Time) (time.Time, bool) {
	if !acpWorkspaceSpeculativeStartAllowed(workspace) {
		return time.Time{}, false
	}
	deadline, _, ok := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation])
	if !ok || !now.Before(deadline) {
		return time.Time{}, false
	}
	return deadline, true
}

// acpWorkspaceSpeculativeStartAttachable reports a recorded speculative start
// whose exact published request is still booting. Attaching it is the same
// one-writer handoff as attaching before demand: the runtime is reachable only
// through the post-attach handshake, and a Ready allocation already satisfies
// the generic attachment rule.
func acpWorkspaceSpeculativeStartAttachable(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if _, _, ok := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation]); !ok {
		return false
	}
	request, observation, retirement := workspace.Spec.Workload, workspace.Status.Allocation, workspace.Spec.Retirement
	if request == nil {
		return false
	}
	if observation == nil {
		return retirement == nil && request.Sequence == 1 && request.PreviousInstance == nil &&
			workspace.Spec.AttachmentEpoch == 0 && workspace.Status.AttachedEpoch == 0
	}
	if observation.Key != request.Key || observation.Sequence != request.Sequence ||
		(observation.Identity.RequestRevision != "" && observation.Identity.RequestRevision != request.Revision) {
		return false
	}
	if observation.State == workspacev1alpha1.AllocationPending {
		// A request whose retirement is authorized waits for it to finish.
		return retirement == nil
	}
	// A class without Suspend restarts a stopped request without retained
	// data. Speculative starts no longer run for such classes, but one left
	// by an earlier build and retired by Stop must not strand its requester.
	return observation.State == workspacev1alpha1.AllocationStopped && observation.Startup == nil &&
		observation.Identity.Valid() && !acpWorkspaceSuspendCapable(workspace) &&
		(retirement == nil || retirement.Sequence == request.Sequence)
}

// acpWorkspaceSuspendCapable reports a workspace whose frozen lifecycle allows
// Suspend with a DataOnly profile. Only such a workspace retires an unclaimed
// runtime with a data-preserving Suspend, so only it may start speculatively:
// a Stop retirement of a never-attached first turn would leave a request the
// attachment rule cannot accept.
func acpWorkspaceSuspendCapable(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	return slices.Contains(workspace.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend) &&
		workspace.Annotations[acpWorkspaceSuspendModeAnnotation] == acpWorkspaceSuspendDataOnly
}

// acpWorkspaceRetainedDataLost reports an unattached Suspend-capable workspace
// whose current request stopped without retained data, as a Stop retirement
// leaves it. Every later request of such a workspace must carry the exact
// stopped instance's retained data, so no continuation can attach or restart
// it; the waiting Task fails instead of stalling.
func acpWorkspaceRetainedDataLost(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	request, observation := workspace.Spec.Workload, workspace.Status.Allocation
	return workspace.Spec.Attachment == nil && request != nil && observation != nil && acpWorkspaceSuspendCapable(workspace) &&
		workspace.Status.ObservedGeneration == workspace.Generation &&
		observation.State == workspacev1alpha1.AllocationStopped && observation.Startup == nil && observation.RetainedData == nil &&
		observation.Key == request.Key && observation.Sequence == request.Sequence
}

// acpWorkspaceSpeculativeStartEligible reports whether this Task may record a
// speculative start: it alone owns the outstanding demand on a workspace in
// the allowed state, which is either fresh (never published or attached) or a
// resumed lineage whose cold resume this Task requested.
func acpWorkspaceSpeculativeStartEligible(workspace *workspacev1alpha1.ExecutionWorkspace, task *corev1alpha1.Task) bool {
	if task == nil || task.UID == "" || !acpWorkspaceSpeculativeStartAllowed(workspace) ||
		!acpWorkspaceResumeDemandBelongsToTask(workspace.Annotations[acpWorkspaceResumeRequestedAnnotation], string(task.UID)) ||
		strings.TrimSpace(task.Annotations[acpExecutionWorkspaceUIDAnnotation]) != string(workspace.UID) {
		return false
	}
	if !acpWorkspaceSuspendCapable(workspace) {
		// Other classes keep the attach-first path.
		return false
	}
	if workspace.Annotations[acpWorkspaceResumedLineageAnnotation] == booleanTrueValue {
		return workspace.Spec.Workload != nil
	}
	return workspace.Spec.Workload == nil && workspace.Spec.AttachmentEpoch == 0 && workspace.Status.AttachedEpoch == 0
}

// startACPWorkspaceSpeculativeRuntime records a speculative start for the
// Task's class-backed workspace and activates its RuntimePool. It is
// best-effort: any failure leaves the ordinary attach-then-demand path in
// force, so it never fails the Task. The deadline is written once per Task
// and never extended.
func (r *TaskReconciler) startACPWorkspaceSpeculativeRuntime(
	ctx context.Context,
	task *corev1alpha1.Task,
	plan ACPRuntimePlan,
	allowCreate bool,
	requiredUID types.UID,
) {
	binding := plan.Workspace
	if binding == nil || binding.Class == nil || binding.Class.ControllerName == "" || task == nil {
		return
	}
	logger := logf.FromContext(ctx).V(1)
	key := types.NamespacedName{Namespace: task.Namespace, Name: acpClassWorkspaceName(task, binding)}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, key, workspace); err != nil || workspace.Spec.Attachment != nil {
		// Another Task holds the attachment: nothing to start, and no API
		// read for a Task queued behind a long turn.
		return
	}
	// Re-read through the uncached reader: this pass may have just flipped
	// the workspace back to Ready or created it, and the start should follow
	// at once rather than one requeue later.
	reader := uncachedReader(r.APIReader, r.Client)
	if err := reader.Get(ctx, key, workspace); err != nil {
		return
	}
	if !acpWorkspaceSpeculativeStartEligible(workspace, task) {
		return
	}
	now := time.Now()
	deadline, owner, recorded := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation])
	if recorded && owner == string(task.UID) {
		if !now.Before(deadline) {
			// Expired: the runtime retires and this Task attaches through the
			// ordinary path.
			return
		}
	} else {
		// The workspace marker is checked above; Tasks linked to this exact
		// incarnation count too, and an unreadable Task refuses the start.
		tainted, err := acpWorkspaceOperatorTainted(ctx, reader, workspace)
		if err != nil || tainted {
			return
		}
		base := workspace.DeepCopy()
		workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation] =
			formatACPWorkspaceSpeculativeStart(now.Add(acpWorkspaceSpeculativeStartTTL), task.UID)
		// The hold is durable before any pool demand: the pool controller
		// re-reads the workspace before it scales an unattached pool down.
		if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			logger.Info("speculative workspace start not recorded", "workspace", workspace.Name, "error", err.Error())
			return
		}
		acpPreDispatch.mark(task, acpPreDispatchSpeculativeStart)
	}
	if _, _, err := r.ensureACPRuntimePoolDemand(
		ctx, task.Namespace, plan, workspace.Name, string(workspace.UID), string(task.UID), allowCreate, requiredUID, false,
	); err != nil {
		logger.Info("speculative workspace runtime demand deferred", "workspace", workspace.Name, "error", err.Error())
	}
}
