/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
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
	// Re-read through the uncached reader: this pass may have just recorded a
	// resume request or created the workspace, and the start should follow
	// at once rather than one requeue later.
	reader := uncachedReader(r.APIReader, r.Client)
	if err := reader.Get(ctx, key, workspace); err != nil {
		return
	}
	now := time.Now()
	if acpWorkspaceSpeculativeResumePending(workspace, now) {
		_, owner, _ := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation])
		if owner != string(task.UID) {
			return
		}
		// The RuntimePool flips and publishes once it has demand. If it
		// cannot be activated, this Task flips itself instead of waiting.
		if _, _, err := r.ensureACPRuntimePoolDemand(
			ctx, task.Namespace, plan, workspace.Name, string(workspace.UID), string(task.UID), allowCreate, requiredUID, false,
		); err != nil {
			logger.Info("speculative resume demand failed; flipping directly", "workspace", workspace.Name, "error", err.Error())
			if flipErr := r.flipACPWorkspaceForColdResume(ctx, workspace, task, binding.Class.EffectiveOnDetach); flipErr != nil {
				logger.Info("cold-resume flip deferred", "workspace", workspace.Name, "error", flipErr.Error())
			}
		}
		return
	}
	if !acpWorkspaceSpeculativeStartEligible(workspace, task) {
		return
	}
	deadline, owner, recorded := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation])
	if recorded && owner == string(task.UID) && !now.Before(deadline) {
		// Expired: the runtime retires and this Task attaches through the
		// ordinary path.
		return
	}
	_, hasIntent := workspace.Annotations[acpWorkspaceAttachmentIntentAnnotation]
	if !recorded || owner != string(task.UID) || (!hasIntent && workspace.Spec.Workload == nil) {
		if !r.recordACPWorkspaceSpeculativeStart(ctx, reader, workspace, task, binding, plan.PoolName, now) {
			return
		}
	}
	if _, _, err := r.ensureACPRuntimePoolDemand(
		ctx, task.Namespace, plan, workspace.Name, string(workspace.UID), string(task.UID), allowCreate, requiredUID, false,
	); err != nil {
		logger.Info("speculative workspace runtime demand deferred", "workspace", workspace.Name, "error", err.Error())
	}
}

// recordACPWorkspaceSpeculativeStart records this Task's speculative start
// and, for a workspace not yet published, its staged attachment, retrying a
// conflicting write from a fresh read. The deadline is written once per Task
// and never extended. It reports whether the start is recorded.
func (r *TaskReconciler) recordACPWorkspaceSpeculativeStart(
	ctx context.Context,
	reader client.Reader,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	binding *ACPRuntimeWorkspaceBinding,
	poolName string,
	now time.Time,
) bool {
	logger := logf.FromContext(ctx).V(1)
	// The workspace marker is checked by eligibility; Tasks linked to this
	// exact incarnation count too, and an unreadable Task refuses the start.
	if tainted, err := acpWorkspaceOperatorTainted(ctx, reader, workspace); err != nil || tainted {
		return false
	}
	intent := ""
	if _, staged := workspace.Annotations[acpWorkspaceAttachmentIntentAnnotation]; !staged && workspace.Spec.Workload == nil {
		intent = r.stageACPWorkspaceAttachmentIntent(ctx, workspace, task, binding, poolName)
	}
	recorded := false
	first := true
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := workspace
		if !first {
			current = &workspacev1alpha1.ExecutionWorkspace{}
			if err := reader.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
				return err
			}
			if current.UID != workspace.UID || !acpWorkspaceSpeculativeStartEligible(current, task) {
				return nil
			}
		}
		first = false
		base := current.DeepCopy()
		changed := false
		if _, owner, ok := parseACPWorkspaceSpeculativeStart(current.Annotations[acpWorkspaceSpeculativeStartAnnotation]); !ok || owner != string(task.UID) {
			current.Annotations[acpWorkspaceSpeculativeStartAnnotation] =
				formatACPWorkspaceSpeculativeStart(now.Add(acpWorkspaceSpeculativeStartTTL), task.UID)
			changed = true
		}
		if _, staged := current.Annotations[acpWorkspaceAttachmentIntentAnnotation]; intent != "" && !staged && current.Spec.Workload == nil {
			current.Annotations[acpWorkspaceAttachmentIntentAnnotation] = intent
			changed = true
		}
		if changed {
			if err := r.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
			acpPreDispatch.mark(task, acpPreDispatchSpeculativeStart)
		}
		recorded = true
		return nil
	})
	if err != nil {
		logger.Info("speculative workspace start not recorded", "workspace", workspace.Name, "error", err.Error())
	}
	return recorded
}

// acpWorkspaceRetainedDataAvailable reports that the workspace's current
// request is observed stopped, at the current generation, with retained data
// bound to that exact instance: the only state a cold resume may publish from.
func acpWorkspaceRetainedDataAvailable(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	request, observation := workspace.Spec.Workload, workspace.Status.Allocation
	return request != nil && observation != nil && workspace.Status.ObservedGeneration == workspace.Generation &&
		observation.State == workspacev1alpha1.AllocationStopped && observation.Startup == nil &&
		observation.Key == request.Key && observation.Sequence == request.Sequence &&
		observation.Identity.RequestRevision == request.Revision && observation.Identity.Valid() &&
		observation.RetainedData != nil && observation.RetainedData.Valid() &&
		observation.RetainedData.SourceInstance == observation.Identity
}

// acpWorkspaceSpeculativeResumePending reports a settled suspended workspace
// whose continuation recorded an open speculative resume (ADR 0057). The
// RuntimePool controller then flips desired state back to Ready in the same
// write that publishes the next workload, so the provider observes the flip
// and the request together. Every guard of a Task's own flip holds here.
func acpWorkspaceSpeculativeResumePending(workspace *workspacev1alpha1.ExecutionWorkspace, now time.Time) bool {
	if workspace == nil || !workspace.DeletionTimestamp.IsZero() ||
		workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended ||
		workspace.Spec.Attachment != nil ||
		workspace.Status.State != workspacev1alpha1.ExecutionWorkspaceStateSuspended ||
		workspace.Annotations[acpWorkspaceResumedLineageAnnotation] != booleanTrueValue ||
		strings.TrimSpace(workspace.Annotations[acpWorkspaceRevocationStartedAnnotation]) != "" ||
		acpWorkspaceQuarantined(workspace) || operatoraccess.Tainted(workspace.Annotations) ||
		!acpWorkspaceSuspendCapable(workspace) || !acpWorkspaceRetainedDataAvailable(workspace) {
		return false
	}
	deadline, owner, ok := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation])
	return ok && now.Before(deadline) &&
		acpWorkspaceResumeDemandBelongsToTask(workspace.Annotations[acpWorkspaceResumeRequestedAnnotation], owner)
}

// requestACPWorkspaceSpeculativeResume records, in one write, a continuation's
// cold-resume request on a settled suspended workspace: its demand, its frozen
// detach action, the resumed-lineage marker, and an open speculative start.
// Desired state stays Suspended; the RuntimePool controller flips it while
// publishing. A conflicting write is retried from a fresh read, and stops if
// the workspace left the settled suspended state.
func (r *TaskReconciler) requestACPWorkspaceSpeculativeResume(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	binding *ACPRuntimeWorkspaceBinding,
	poolName string,
	now time.Time,
) error {
	reader := uncachedReader(r.APIReader, r.Client)
	// The attachment is staged first so the RuntimePool can install it in the
	// same write as the flip and publication; without it the ordinary attach
	// follows the flip.
	intent := r.stageACPWorkspaceAttachmentIntent(ctx, workspace, task, binding, poolName)
	effectiveOnDetach := binding.Class.EffectiveOnDetach
	first := true
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := workspace
		if !first {
			current = &workspacev1alpha1.ExecutionWorkspace{}
			if err := reader.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
				return err
			}
			if current.UID != workspace.UID || current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended ||
				current.Status.State != workspacev1alpha1.ExecutionWorkspaceStateSuspended || current.Spec.Attachment != nil ||
				acpWorkspaceSpeculativeResumePending(current, now) {
				// Another writer moved the workspace on; the next pass decides.
				return nil
			}
		}
		first = false
		base := current.DeepCopy()
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[acpWorkspaceResumeRequestedAnnotation] = now.UTC().Format(time.RFC3339Nano) + " " + task.Name + " " + string(task.UID)
		current.Annotations[acpWorkspaceDetachActionAnnotation] = effectiveOnDetach
		current.Annotations[acpWorkspaceResumedLineageAnnotation] = booleanTrueValue
		current.Annotations[acpWorkspaceSpeculativeStartAnnotation] = formatACPWorkspaceSpeculativeStart(now.Add(acpWorkspaceSpeculativeStartTTL), task.UID)
		if intent != "" {
			current.Annotations[acpWorkspaceAttachmentIntentAnnotation] = intent
		}
		if err := r.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		acpPreDispatch.mark(task, acpPreDispatchSpeculativeStart)
		return nil
	})
}

// flipACPWorkspaceForColdResume is a continuation's own cold-resume flip: the
// same concrete workspace returns to Ready, and the RuntimePool replaces the
// runtime with a fresh boot and credential bootstrap before this Task can
// attach. It is used when a speculative resume does not apply, or expired
// without the RuntimePool claiming it.
func (r *TaskReconciler) flipACPWorkspaceForColdResume(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	effectiveOnDetach string,
) error {
	base := workspace.DeepCopy()
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
	if workspace.Annotations == nil {
		workspace.Annotations = map[string]string{}
	}
	// The resume flip records pending demand without changing the last
	// detach instant: a cold boot can outlast idleTimeout, so retention must
	// treat the workspace as demanded until the continuation attaches. If
	// that requester dies, idle retention resumes from the actual detach
	// instead of granting another full retention interval. The
	// continuation's frozen effective detach action replaces the suspender's
	// in the same patch (a continuation that selects Delete and dies during
	// cold boot must settle with Delete, not resuspend under the
	// predecessor's stale action), and the resumed-lineage marker makes later
	// pool loss terminal even after Ready is projected. The demand record
	// carries the requesting Task's name so retention can retire demand whose
	// requester died before attaching instead of holding the workspace
	// forever.
	workspace.Annotations[acpWorkspaceResumeRequestedAnnotation] =
		time.Now().UTC().Format(time.RFC3339Nano) + " " + task.Name + " " + string(task.UID)
	workspace.Annotations[acpWorkspaceDetachActionAnnotation] = effectiveOnDetach
	workspace.Annotations[acpWorkspaceResumedLineageAnnotation] = booleanTrueValue
	// A speculative start left by an earlier cycle, or an expired one, never
	// carries over.
	delete(workspace.Annotations, acpWorkspaceSpeculativeStartAnnotation)
	return r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// acpWorkspaceAttachmentIntentAnnotation carries a Task's staged attachment
// (ADR 0057): Lease claimed and epoch-fenced, bearer Secret created, but no
// workspace spec names it yet. It holds only the Secret name and the bearer
// digest. The RuntimePool controller installs it in the same write that
// publishes the workload (and, on a resume, flips desired state), so the
// provider acknowledges the epoch and starts the runtime on one observation.
const acpWorkspaceAttachmentIntentAnnotation = "acp.workspace.orka.ai/attachment-intent"

type acpWorkspaceAttachmentIntent struct {
	TaskName    string    `json:"taskName"`
	TaskUID     types.UID `json:"taskUID"`
	Epoch       int64     `json:"epoch"`
	TokenSHA256 string    `json:"tokenSHA256"`
	ExpiresAt   time.Time `json:"expiresAt"`
	SecretName  string    `json:"secretName"`
}

func encodeACPWorkspaceAttachmentIntent(attachment *workspacev1alpha1.ExecutionWorkspaceAttachment) (string, error) {
	encoded, err := json.Marshal(acpWorkspaceAttachmentIntent{
		TaskName: attachment.TaskRef.Name, TaskUID: attachment.TaskRef.UID, Epoch: attachment.Epoch,
		TokenSHA256: attachment.TokenSHA256, ExpiresAt: attachment.ExpiresAt.UTC(), SecretName: attachment.TokenSecretRef.Name,
	})
	return string(encoded), err
}

func parseACPWorkspaceAttachmentIntent(workspace *workspacev1alpha1.ExecutionWorkspace) (*acpWorkspaceAttachmentIntent, bool) {
	raw, present := workspace.Annotations[acpWorkspaceAttachmentIntentAnnotation]
	if !present {
		return nil, false
	}
	intent := &acpWorkspaceAttachmentIntent{}
	if err := json.Unmarshal([]byte(raw), intent); err != nil || intent.TaskUID == "" || intent.TaskName == "" ||
		intent.Epoch <= 0 || intent.SecretName != attachmentSecretName(workspace.Name, intent.Epoch) ||
		!strings.HasPrefix(intent.TokenSHA256, "sha256:") || intent.ExpiresAt.IsZero() {
		return nil, false
	}
	return intent, true
}

func (i *acpWorkspaceAttachmentIntent) attachment() *workspacev1alpha1.ExecutionWorkspaceAttachment {
	attachment := &workspacev1alpha1.ExecutionWorkspaceAttachment{
		TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: i.TaskName, UID: i.TaskUID},
		Epoch:   i.Epoch, TokenSHA256: i.TokenSHA256, ExpiresAt: metav1.NewTime(i.ExpiresAt),
	}
	attachment.TokenSecretRef.Name = i.SecretName
	return attachment
}

// validateACPWorkspaceAttachmentIntentTarget is the attach rule for a staged
// attachment: the exact frozen bindings, no attachment or pending settlement,
// no taint or quarantine, a Suspend-capable lifecycle, and either a fresh
// workspace or a settled suspension with retained data that this Task's
// resume request will flip.
func validateACPWorkspaceAttachmentIntentTarget(
	ctx context.Context,
	reader client.Reader,
	binding *ACPRuntimeWorkspaceBinding,
	poolName string,
) func(*workspacev1alpha1.ExecutionWorkspace, *corev1alpha1.Task, time.Time) error {
	return func(workspace *workspacev1alpha1.ExecutionWorkspace, task *corev1alpha1.Task, _ time.Time) error {
		if err := verifyACPClassWorkspace(workspace, task, binding, poolName); err != nil {
			return err
		}
		if workspace.Spec.Attachment != nil || strings.TrimSpace(workspace.Annotations[acpWorkspaceRevocationStartedAnnotation]) != "" ||
			workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed || acpWorkspaceQuarantined(workspace) ||
			!acpWorkspaceSuspendCapable(workspace) {
			return fmt.Errorf("workspace %s cannot stage a speculative attachment", workspace.Name)
		}
		fresh := workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredReady &&
			workspace.Spec.AttachmentEpoch == 0 && workspace.Status.AttachedEpoch == 0
		resume := workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended &&
			workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateSuspended && acpWorkspaceRetainedDataAvailable(workspace)
		if !fresh && !resume {
			return fmt.Errorf("workspace %s is neither fresh nor a settled suspension", workspace.Name)
		}
		if operatoraccess.Tainted(workspace.Annotations) {
			return fmt.Errorf("workspace %s: %w", workspace.Name, operatoraccess.ErrTainted)
		}
		return refuseOperatorTaintedACPWorkspaceHandoff(ctx, reader, workspace, task)
	}
}

// stageACPWorkspaceAttachmentIntent stages this Task's next attachment and
// returns its encoded intent, or "" when staging is refused (for example a
// rival holds the Lease); the ordinary attach path then applies.
func (r *TaskReconciler) stageACPWorkspaceAttachmentIntent(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	binding *ACPRuntimeWorkspaceBinding,
	poolName string,
) string {
	if binding == nil || workspace.UID == "" {
		return ""
	}
	reader := uncachedReader(r.APIReader, r.Client)
	manager := WorkspaceAttachmentManager{Client: r.Client, APIReader: r.APIReader, LeaseTTL: acpWorkspaceAttachmentTTL}
	attachment, err := manager.prepareAttachmentIntent(ctx, workspace, task, validateACPWorkspaceAttachmentIntentTarget(ctx, reader, binding, poolName))
	if err != nil {
		logf.FromContext(ctx).V(1).Info("speculative attachment not staged", "workspace", workspace.Name, "error", err.Error())
		return ""
	}
	encoded, err := encodeACPWorkspaceAttachmentIntent(attachment)
	if err != nil {
		return ""
	}
	return encoded
}

// installableACPWorkspaceAttachmentIntent returns the staged attachment the
// RuntimePool controller may install in its publication write, or nil. Every
// guard of an ordinary attach is rechecked against the exact object version
// the write is fenced on, plus the staged Lease and Secret themselves.
func installableACPWorkspaceAttachmentIntent(
	ctx context.Context,
	reader client.Reader,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	resume bool,
	now time.Time,
) (*workspacev1alpha1.ExecutionWorkspaceAttachment, error) {
	intent, ok := parseACPWorkspaceAttachmentIntent(workspace)
	if !ok || workspace.Spec.Attachment != nil || !intent.ExpiresAt.After(now) {
		return nil, nil
	}
	_, owner, marked := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation])
	if !marked || owner != string(intent.TaskUID) ||
		!acpWorkspaceResumeDemandBelongsToTask(workspace.Annotations[acpWorkspaceResumeRequestedAnnotation], owner) {
		return nil, nil
	}
	if next, err := nextWorkspaceAttachmentEpoch(workspace); err != nil || next != intent.Epoch {
		return nil, nil
	}
	if !resume && (workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady || !acpWorkspaceSpeculativeStartAllowed(workspace)) {
		return nil, nil
	}
	if !acpWorkspaceSuspendCapable(workspace) || operatoraccess.Tainted(workspace.Annotations) || acpWorkspaceQuarantined(workspace) {
		return nil, nil
	}
	tainted, err := acpWorkspaceOperatorTainted(ctx, reader, workspace)
	if err != nil || tainted {
		return nil, err
	}
	if staged, err := acpWorkspaceStagedAttachmentObjectsHeld(ctx, reader, workspace, intent, now); err != nil || !staged {
		return nil, err
	}
	return intent.attachment(), nil
}

// acpWorkspaceStagedAttachmentObjectsHeld verifies the staged bearer Secret
// (owner, epoch, and digest) and the Lease still fenced to the intent's Task
// and epoch.
func acpWorkspaceStagedAttachmentObjectsHeld(
	ctx context.Context,
	reader client.Reader,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	intent *acpWorkspaceAttachmentIntent,
	now time.Time,
) (bool, error) {
	epoch := strconv.FormatInt(intent.Epoch, 10)
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: intent.SecretName}, secret); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	secretOwner := metav1.GetControllerOf(secret)
	digest := sha256.Sum256(secret.Data[workspaceAttachmentTokenKey])
	if secretOwner == nil || secretOwner.UID != workspace.UID || secret.Labels[workspaceAttachmentLabel] != string(workspace.UID) ||
		string(secret.Data["workspaceUID"]) != string(workspace.UID) || string(secret.Data[epochField]) != epoch ||
		"sha256:"+hex.EncodeToString(digest[:]) != intent.TokenSHA256 {
		return false, nil
	}
	lease := &coordinationv1.Lease{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name)}, lease); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	leaseOwner := metav1.GetControllerOf(lease)
	return leaseOwner != nil && leaseOwner.UID == workspace.UID && lease.Spec.HolderIdentity != nil &&
		*lease.Spec.HolderIdentity == string(intent.TaskUID) && !leaseExpired(lease, now) &&
		lease.Annotations[workspaceAttachmentLeaseEpochAnnotation] == epoch, nil
}

// releaseACPWorkspaceSpeculativeClaims undoes what a settling Task staged but
// never installed: its attachment intent (then its Secret and Lease) and, on
// a still-suspended workspace, its pending resume request. Removing the
// annotations first fences the RuntimePool's install and flip, which are
// optimistic writes on the same object. changed=true asks the caller to
// settle again on a fresh read.
func (r *TaskReconciler) releaseACPWorkspaceSpeculativeClaims(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
) (bool, error) {
	if workspace.Spec.Attachment != nil {
		return false, nil
	}
	intent, staged := parseACPWorkspaceAttachmentIntent(workspace)
	ownIntent := staged && intent.TaskUID == task.UID
	_, owner, marked := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation])
	pendingResume := workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended && marked && owner == string(task.UID)
	_, malformedIntent := workspace.Annotations[acpWorkspaceAttachmentIntentAnnotation]
	ownMalformed := malformedIntent && !staged && pendingResume
	if !ownIntent && !pendingResume && !ownMalformed {
		return false, nil
	}
	base := workspace.DeepCopy()
	if ownIntent || ownMalformed {
		delete(workspace.Annotations, acpWorkspaceAttachmentIntentAnnotation)
	}
	if pendingResume {
		delete(workspace.Annotations, acpWorkspaceSpeculativeStartAnnotation)
		if acpWorkspaceResumeDemandBelongsToTask(workspace.Annotations[acpWorkspaceResumeRequestedAnnotation], string(task.UID)) {
			delete(workspace.Annotations, acpWorkspaceResumeRequestedAnnotation)
		}
	}
	if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return true, nil
		}
		return true, err
	}
	if ownIntent {
		reader := uncachedReader(r.APIReader, r.Client)
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: workspace.Namespace, Name: intent.SecretName}}
		if err := deleteWorkspaceOwnedAttachmentObject(ctx, reader, r.Client, workspace, secret, "Secret"); err != nil {
			return true, err
		}
		lease := &coordinationv1.Lease{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name)}, lease); err == nil {
			leaseOwner := metav1.GetControllerOf(lease)
			if leaseOwner != nil && leaseOwner.UID == workspace.UID && lease.Spec.HolderIdentity != nil &&
				*lease.Spec.HolderIdentity == string(task.UID) &&
				lease.Annotations[workspaceAttachmentLeaseEpochAnnotation] == strconv.FormatInt(intent.Epoch, 10) {
				if err := r.Delete(ctx, lease, deleteCurrentObjectPreconditions(lease)...); err != nil && !apierrors.IsNotFound(err) {
					return true, err
				}
			}
		} else if !apierrors.IsNotFound(err) {
			return true, err
		}
	}
	return true, nil
}
