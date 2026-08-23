/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/pkg/workspaceprovider"
)

const (
	// acpExecutionWorkspaceLinkLabel links a RuntimePool or Task to its
	// controller-first ExecutionWorkspace by Orka-owned name. It never carries
	// a provider-native identifier.
	acpExecutionWorkspaceLinkLabel = "acp.workspace.orka.ai/execution-workspace"
	// acpExecutionWorkspacePoolAnnotation records the deterministic RuntimePool
	// name bound to a class-backed ExecutionWorkspace.
	acpExecutionWorkspacePoolAnnotation = "acp.workspace.orka.ai/runtime-pool"
	// acpWorkspaceDetachActionAnnotation records the frozen effective detach
	// action of the currently attached Task so settlement applies exactly the
	// validated choice without reloading the execution snapshot.
	acpWorkspaceDetachActionAnnotation = "acp.workspace.orka.ai/detach-action"
	// acpTaskWorkspaceSettledAnnotation marks a Task whose detach action has
	// been applied. A terminal Task reconciles indefinitely and its settlement
	// hook must be once-per-Task; recording the marker on each Task (instead
	// of a single latest-settler annotation on the shared workspace) keeps
	// every settled Task recognized, so an older terminal Task re-reconciled
	// after later settlements can never re-suspend a workspace a continuation
	// has already requested to resume.
	acpTaskWorkspaceSettledAnnotation = "acp.workspace.orka.ai/workspace-settled"
	// acpTaskAttachmentEpochAnnotation records, on the Task, the workspace
	// attachment epoch this Task held. Settlement uses it to recognize that a
	// later epoch's receipt supersedes this Task's own displaced receipt.
	acpTaskAttachmentEpochAnnotation = "acp.workspace.orka.ai/attachment-epoch"
	// acpWorkspaceLastSettledTaskAnnotation is the workspace-side settlement
	// receipt written in the SAME patch as a suspension. It backstops the
	// crash window before the Task-side marker lands: only the latest settler
	// needs it (earlier settlers already carry their Task markers), so a
	// single overwritten value is sufficient.
	acpWorkspaceLastSettledTaskAnnotation = "acp.workspace.orka.ai/last-settled-task-uid"
	// acpExecutionWorkspaceUIDAnnotation pins the exact ExecutionWorkspace
	// incarnation a Task attached, alongside the name-bearing link label.
	// Settlement acts only on that incarnation: a Session deleted and
	// recreated under the same name produces a different workspace UID, and
	// an old Task must never revoke or delete the new incarnation's
	// workspace.
	acpExecutionWorkspaceUIDAnnotation = "acp.workspace.orka.ai/execution-workspace-uid"
	// acpWorkspaceNamePrefix prefixes deterministic class-backed workspace names.
	acpWorkspaceNamePrefix = "acp-ws"
	// acpWorkspaceProviderConfigUIDAnnotation pins the exact cluster-scoped
	// RuntimeProviderConfig that selected this workspace's physical backend.
	acpWorkspaceProviderConfigUIDAnnotation = "acp.workspace.orka.ai/provider-config-uid"
	// acpWorkspaceBackendAnnotation records the resolved physical backend so a
	// recreated same-name provider config can never silently re-serve an
	// existing workspace through a different backend.
	acpWorkspaceBackendAnnotation = "acp.workspace.orka.ai/backend"
	// acpWorkspaceDurableAnnotation records at materialization that the
	// frozen class profile provisions durable workspace artifacts (a data-only
	// suspension checkpoint or durable PVC), so terminal dispositions report
	// what actually existed instead of inferring it from allowed detach
	// actions.
	acpWorkspaceDurableAnnotation = "acp.workspace.orka.ai/durable-workspace"
	// acpWorkspaceResumedLineageAnnotation marks a workspace whose current
	// physical runtime was cold-resumed from a preserved data checkpoint. The
	// linked pool then holds the ONLY copy of the session data for the
	// workspace's remaining lifetime, so a missing or foreign pool is
	// terminal data loss even after the adapter projected Ready or Attached.
	acpWorkspaceResumedLineageAnnotation = "acp.workspace.orka.ai/resumed-lineage"
	// acpWorkspaceRuntimeNamespaceAnnotation freezes the runtime namespace
	// the linked pool realizes its provider children (SandboxClaim, durable
	// PVC) in, so deletion proofs probe the ORIGINAL namespace even if the
	// controller's --acp-runtime-namespace changed since creation.
	acpWorkspaceRuntimeNamespaceAnnotation = "acp.workspace.orka.ai/runtime-namespace"
	// acpWorkspaceDurableDataAbsentAnnotation records, on a terminally failed
	// workspace, that the failure PROVED no durable data exists (a missing,
	// foreign, or non-suspend-capable linked pool, or a pool that settled
	// without a consensual checkpoint). Retention frees the suspension-quota
	// slot for such failures while resume-loss failures that preserve a
	// claim stay charged until deletion proves cleanup.
	acpWorkspaceDurableDataAbsentAnnotation = "acp.workspace.orka.ai/durable-data-absent"
	// acpWorkspaceRevocationStartedAnnotation stamps the first revocation
	// attempt so settlement can enforce the frozen detachTimeout instead of
	// requeueing forever behind an adapter that never releases the epoch.
	acpWorkspaceRevocationStartedAnnotation = "acp.workspace.orka.ai/revocation-started-at"
	// acpWorkspaceResumeRequestedAnnotation records that a continuation
	// requested cold resume and has not attached yet. Idle retention treats
	// it as live demand (a cold boot can outlast idleTimeout), and the next
	// successful attachment clears it; the frozen maxLifetime remains the
	// hard bound if the requester dies before attaching.
	acpWorkspaceResumeRequestedAnnotation = "acp.workspace.orka.ai/resume-requested-at"
	// acpWorkspaceAttachmentTTL bounds one ACP Task attachment. The attachment
	// Secret is not the RuntimePool data-plane credential; the epoch enforces
	// the one-writer rule, and class maxLifetime still clamps the expiry.
	acpWorkspaceAttachmentTTL = 24 * time.Hour
)

// errACPWorkspaceBindingConflict marks fail-closed adoption mismatches: an
// existing ExecutionWorkspace does not carry the exact frozen bindings, so the
// Task must fail instead of executing in an unverified workspace.
var errACPWorkspaceBindingConflict = errors.New("execution workspace binding conflict")

// errACPWorkspaceTerminalFailure marks a workspace whose provider adapter
// reported a terminal Failed state (for example an enforced maximum-lifetime
// expiry): the Task waiting on it must fail instead of requeueing forever.
var errACPWorkspaceTerminalFailure = errors.New("execution workspace failed terminally")

// acpClassWorkspaceName derives the deterministic controller-first workspace
// name for a frozen class-backed binding.
func acpClassWorkspaceName(task *corev1alpha1.Task, binding *ACPRuntimeWorkspaceBinding) string {
	sessionUID := types.UID("")
	if binding.ReusePolicy == corev1alpha1.WorkspaceReusePolicySession {
		sessionUID = types.UID(binding.SessionUID)
	}
	identity := workspaceprovider.InteractiveWorkspaceIdentity(
		task.Namespace, sessionUID, task.UID, types.UID(binding.Class.UID), binding.WorkspaceSlot,
	)
	return workspaceprovider.WorkspaceName(acpWorkspaceNamePrefix, identity)
}

// ensureACPClassWorkspace materializes, verifies, and attaches the
// controller-first ExecutionWorkspace for a class-backed plan. It returns the
// workspace name once the workspace is core-admitted and this Task holds the
// exclusive attachment; ready=false requests a requeue while admission,
// adapter status, or a competing attachment settles.
//
//nolint:gocyclo // Materialization, fail-closed adoption, and attachment stay auditable together.
func (r *TaskReconciler) ensureACPClassWorkspace(
	ctx context.Context,
	task *corev1alpha1.Task,
	plan ACPRuntimePlan,
) (string, bool, error) {
	if plan.Workspace == nil || plan.Workspace.Class == nil {
		return "", true, nil
	}
	binding := plan.Workspace
	name := acpClassWorkspaceName(task, binding)
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace)
	if apierrors.IsNotFound(err) {
		workspace, err = r.createACPClassWorkspace(ctx, task, binding, plan.PoolName, name)
		if err != nil {
			if apierrors.IsAlreadyExists(err) {
				return "", false, nil
			}
			return "", false, err
		}
	} else if err != nil {
		return "", false, err
	}
	if !workspace.DeletionTimestamp.IsZero() {
		// The predecessor detach is still deleting the deterministic-name
		// workspace; the replacement is created once deletion completes.
		return "", false, nil
	}
	if err := verifyACPClassWorkspace(workspace, task, binding); err != nil {
		return "", false, err
	}
	// The settlement link is persisted as soon as the deterministic workspace
	// exists: a Task deleted while core admission is still pending must be
	// able to settle (and apply its frozen detach action to) the session
	// workspace it materialized, which carries no Task owner reference.
	if err := r.linkTaskToACPWorkspace(ctx, task, workspace); err != nil {
		return "", false, err
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		// A continuation Task requests cold resume: the same concrete
		// workspace returns to Ready, and the adapter replaces the physical
		// runtime with a fresh boot and credential bootstrap before this
		// Task can attach. The flip waits for the suspension to actually
		// settle: resuming while the backend still drains or checkpoints
		// would lift the pool's suspend intent mid-flight and strand the
		// generic serving path in Draining with no undrain transition.
		switch workspace.Status.State {
		case workspacev1alpha1.ExecutionWorkspaceStateSuspended:
			base := workspace.DeepCopy()
			workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
			if workspace.Annotations == nil {
				workspace.Annotations = map[string]string{}
			}
			// The resume flip restarts the idle window AND records pending
			// demand: a cold boot can outlast idleTimeout, so retention must
			// treat the workspace as demanded until the continuation
			// attaches, not merely until the suspended state clears. The
			// continuation's frozen effective detach action replaces the
			// suspender's in the same patch (a continuation that selects
			// Delete and dies during cold boot must settle with Delete, not
			// resuspend under the predecessor's stale action), and the
			// resumed-lineage marker makes later pool loss terminal even
			// after Ready is projected.
			now := time.Now().UTC().Format(time.RFC3339Nano)
			workspace.Annotations[acpWorkspaceLastDetachedAnnotation] = now
			// The demand record carries the requesting Task's name so
			// retention can retire demand whose requester died before
			// attaching instead of holding the workspace forever.
			workspace.Annotations[acpWorkspaceResumeRequestedAnnotation] = now + " " + task.Name + " " + string(task.UID)
			workspace.Annotations[acpWorkspaceDetachActionAnnotation] = binding.Class.EffectiveOnDetach
			workspace.Annotations[acpWorkspaceResumedLineageAnnotation] = booleanTrueValue
			if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return "", false, client.IgnoreNotFound(err)
			}
			return "", false, nil
		case workspacev1alpha1.ExecutionWorkspaceStateFailed:
			// The suspension failed closed and preserved no data checkpoint;
			// resuming would fabricate a workspace the contract cannot back.
			return "", false, fmt.Errorf(
				"%w: workspace %s suspension failed without a preserved data checkpoint; cold resume is impossible",
				errACPWorkspaceBindingConflict, workspace.Name,
			)
		default:
			// The suspension has not settled yet, but the continuation's
			// demand must already be durable: if idleTimeout expires while
			// the checkpoint drains, retention must see outstanding demand
			// instead of deleting the retained workspace the continuation is
			// about to resume.
			if fields := strings.Fields(workspace.Annotations[acpWorkspaceResumeRequestedAnnotation]); len(fields) < 3 ||
				fields[2] != string(task.UID) {
				base := workspace.DeepCopy()
				if workspace.Annotations == nil {
					workspace.Annotations = map[string]string{}
				}
				workspace.Annotations[acpWorkspaceResumeRequestedAnnotation] =
					time.Now().UTC().Format(time.RFC3339Nano) + " " + task.Name + " " + string(task.UID)
				if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
					return "", false, client.IgnoreNotFound(err)
				}
			}
			return "", false, nil
		}
	}
	if workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed {
		if attachment := workspace.Spec.Attachment; attachment != nil && attachment.TaskRef.UID != task.UID {
			// The failed incarnation is still held by its predecessor (for
			// example maxLifetime expired mid-execution): that Task's own
			// settlement shortly applies its frozen detach action and removes
			// this incarnation, after which the deterministic name is
			// recreated fresh. The continuation queues for that instead of
			// failing permanently.
			return "", false, nil
		}
		// The adapter's Failed state is terminal for this incarnation (a
		// lost cold resume, a torn-down pool of a resumed lineage, or an
		// enforced maximum-lifetime expiry); waiting can never recover it.
		return "", false, fmt.Errorf(
			"%w: workspace %s reported a terminal Failed state", errACPWorkspaceTerminalFailure, workspace.Name)
	}
	if !workspaceCurrentlyAdmittedByCore(workspace) {
		if condition := workspaceprovider.FindCondition(
			workspace.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceAdmitted),
		); condition != nil && condition.Status == metav1.ConditionFalse &&
			condition.ObservedGeneration == workspace.Generation &&
			(condition.Reason == "ClassBindingMismatch" || condition.Reason == reasonProviderBindingMismatch ||
				condition.Reason == "ClassDeleting" || condition.Reason == reasonProviderDeleting ||
				condition.Reason == reasonProfileDrift) {
			// The frozen identity can never become admissible (the class or
			// provider generation moved after the snapshot froze, the class
			// or provider is deleting and admits no new workspaces - holding
			// the unadmitted reference would deadlock their finalizers - or
			// the immutable profile was replaced); requeueing
			// forever would leave the Task permanently Pending.
			return "", false, fmt.Errorf(
				"%w: workspace %s was denied core admission (%s) and its frozen identity can never be admitted; create a new Task",
				errACPWorkspaceBindingConflict, workspace.Name, condition.Reason,
			)
		}
		return "", false, nil
	}
	if workspace.Spec.Attachment != nil {
		if workspace.Spec.Attachment.TaskRef.UID == task.UID {
			if workspace.Status.AttachedEpoch != workspace.Spec.Attachment.Epoch {
				// Attach bumped the generation: core must re-admit it and the
				// adapter must publish the enforced epoch before RuntimePool
				// demand exists, or the prompt could execute under a provider
				// policy core has since withdrawn.
				return "", false, nil
			}
			if maxLifetime := workspace.Spec.Lifecycle.MaxLifetime; maxLifetime != nil && maxLifetime.Duration > 0 &&
				!time.Now().Before(workspace.CreationTimestamp.Add(maxLifetime.Duration)) {
				// The frozen hard deadline elapsed between Attach and the
				// adapter's Failed publication: admitting RuntimePool demand
				// from the stale Ready observation would execute the prompt
				// past the lifetime until the adapter catches up. Both
				// inputs are immutable, so the cached read is authoritative;
				// the adapter's enforcement fails the workspace shortly.
				return "", false, nil
			}
			if err := r.recordACPWorkspaceDetachAction(ctx, workspace, binding); err != nil {
				return "", false, err
			}
			if err := r.markACPTaskAttachmentEpoch(ctx, task, workspace.Spec.Attachment.Epoch); err != nil {
				return "", false, err
			}
			return name, true, r.linkTaskToACPWorkspace(ctx, task, workspace)
		}
		// Another Task holds the one-writer attachment. Session-reused
		// workspaces queue behind it; the deterministic per-Task name makes
		// this unreachable for reuse none.
		return "", false, nil
	}
	if workspace.Annotations[acpWorkspaceRevocationStartedAnnotation] != "" {
		// A settlement is pending on this workspace incarnation: the prior
		// Task's frozen detach action must complete before any continuation
		// runs. For Delete, this filesystem is destroyed and the
		// deterministic name is recreated fresh once the deletion lands. For
		// Suspend, the settlement retires this stamp in the same optimistic
		// patch that lands DesiredState=Suspended, and the continuation then
		// takes the cold-resume flip; attaching while the stamp stands would
		// reuse the workspace warm and let the old settlement observe a
		// foreign attachment as completion, so the requested checkpoint
		// would silently never be taken.
		return "", false, nil
	}
	// Persist the settlement link before attaching: if the controller dies
	// between attachment and a later link patch, a session-scoped workspace
	// would stay attached to a Task that cleanup can no longer associate with
	// it, and every later Task for that Session would queue forever.
	if err := r.linkTaskToACPWorkspace(ctx, task, workspace); err != nil {
		return "", false, err
	}
	manager := WorkspaceAttachmentManager{Client: r.Client, LeaseTTL: acpWorkspaceAttachmentTTL}
	if _, err := manager.Attach(ctx, workspace, task, map[string]string{
		acpWorkspaceDetachActionAnnotation: binding.Class.EffectiveOnDetach,
	}); err != nil {
		if errors.Is(err, ErrWorkspaceAttachmentLocked) {
			return "", false, nil
		}
		if strings.Contains(err.Error(), "not reusable") || strings.Contains(err.Error(), "revalidate workspace") {
			// The adapter has not yet observed the current generation or the
			// workspace is still provisioning; attachment retries.
			return "", false, nil
		}
		return "", false, err
	}
	// The attachment bumped the workspace generation: readiness waits for
	// core re-admission and the adapter's enforced-epoch acknowledgement on
	// the next pass, so RuntimePool demand can never precede them.
	return "", false, nil
}

// recordACPWorkspaceDetachAction records the attached Task's frozen effective
// detach action on the workspace so settlement applies exactly that choice.
func (r *TaskReconciler) recordACPWorkspaceDetachAction(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	binding *ACPRuntimeWorkspaceBinding,
) error {
	action := binding.Class.EffectiveOnDetach
	if workspace.Annotations[acpWorkspaceDetachActionAnnotation] == action &&
		workspace.Annotations[acpWorkspaceRevocationStartedAnnotation] == "" &&
		workspace.Annotations[acpWorkspaceResumeRequestedAnnotation] == "" {
		return nil
	}
	base := workspace.DeepCopy()
	if workspace.Annotations == nil {
		workspace.Annotations = map[string]string{}
	}
	workspace.Annotations[acpWorkspaceDetachActionAnnotation] = action
	// A fresh attachment invalidates any prior revocation stamp and fulfils
	// pending resume demand: the next detach measures its detachTimeout from
	// its own revocation, and retention resumes ordinary idle handling.
	delete(workspace.Annotations, acpWorkspaceRevocationStartedAnnotation)
	delete(workspace.Annotations, acpWorkspaceResumeRequestedAnnotation)
	return r.Patch(ctx, workspace, client.MergeFrom(base))
}

// linkTaskToACPWorkspace records the workspace name on the Task so terminal
// settlement and Task finalization find the workspace without reloading the
// execution snapshot. The label value is an Orka-owned deterministic name.
func (r *TaskReconciler) linkTaskToACPWorkspace(
	ctx context.Context,
	task *corev1alpha1.Task,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) error {
	if task.Labels[acpExecutionWorkspaceLinkLabel] == workspace.Name &&
		task.Annotations[acpExecutionWorkspaceUIDAnnotation] == string(workspace.UID) {
		return nil
	}
	base := task.DeepCopy()
	if task.Labels == nil {
		task.Labels = make(map[string]string)
	}
	if task.Annotations == nil {
		task.Annotations = make(map[string]string)
	}
	task.Labels[acpExecutionWorkspaceLinkLabel] = workspace.Name
	task.Annotations[acpExecutionWorkspaceUIDAnnotation] = string(workspace.UID)
	return r.Patch(ctx, task, client.MergeFrom(base))
}

func (r *TaskReconciler) createACPClassWorkspace(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *ACPRuntimeWorkspaceBinding,
	poolName string,
	name string,
) (*workspacev1alpha1.ExecutionWorkspace, error) {
	lifecycle, err := binding.Class.Lifecycle()
	if err != nil {
		return nil, err
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: task.Namespace,
			Name:      name,
			Labels: map[string]string{
				workspacev1alpha1.ProviderControllerLabel: acpWorkspaceControllerLabelValue,
			},
			Annotations: workspaceCreationAnnotations(binding, poolName, task.Name, string(task.UID),
				resolveACPWorkspaceRuntimeNamespace(r.ACPRuntimeNamespace, task.Namespace)),
		},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
			Mode: workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			ClassBinding: workspacev1alpha1.ImmutableObjectBinding{
				Name:        binding.Class.Name,
				UID:         types.UID(binding.Class.UID),
				Generation:  binding.Class.Generation,
				ProfileHash: binding.Class.ProfileHash,
			},
			ProviderBinding: workspacev1alpha1.ImmutableObjectBinding{
				Name:       binding.Class.ProviderName,
				UID:        types.UID(binding.Class.ProviderUID),
				Generation: binding.Class.ProviderGeneration,
			},
			Slot:         binding.WorkspaceSlot,
			DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredReady,
			Lifecycle:    lifecycle,
		},
	}
	if binding.ReusePolicy == corev1alpha1.WorkspaceReusePolicySession {
		if task.Spec.SessionRef == nil || strings.TrimSpace(task.Spec.SessionRef.Name) == "" {
			return nil, fmt.Errorf("session-reused execution workspace requires spec.sessionRef.name")
		}
		workspace.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{
			Name: strings.TrimSpace(task.Spec.SessionRef.Name),
			UID:  types.UID(binding.SessionUID),
		}
	} else {
		// A per-Task workspace is garbage-collected with its Task even if the
		// explicit settle-time deletion never runs. The reference is
		// deliberately not a controller reference: Task workspace status is
		// projected by the ACP execution state machine, and a controller owner
		// would let the generic owner projection fight it.
		if err := controllerutil.SetOwnerReference(task, workspace, r.Scheme); err != nil {
			return nil, fmt.Errorf("set execution workspace owner: %w", err)
		}
	}
	if err := r.Create(ctx, workspace); err != nil {
		return nil, err
	}
	return workspace, nil
}

// workspaceCreationAnnotations renders the Orka-owned annotations for a fresh
// class-backed workspace: the linked pool name and the frozen retention cap.
// resolveACPWorkspaceRuntimeNamespace resolves the namespace provider
// children realize in: the configured runtime namespace, or the workspace's
// own namespace when the flag is empty.
func resolveACPWorkspaceRuntimeNamespace(configured, workspaceNamespace string) string {
	if resolved := strings.TrimSpace(configured); resolved != "" {
		return resolved
	}
	return workspaceNamespace
}

func workspaceCreationAnnotations(binding *ACPRuntimeWorkspaceBinding, poolName, requesterName, requesterUID, runtimeNamespace string) map[string]string {
	annotations := map[string]string{
		acpExecutionWorkspacePoolAnnotation:     poolName,
		acpWorkspaceProviderConfigUIDAnnotation: binding.Class.ProviderConfigUID,
		acpWorkspaceBackendAnnotation:           string(binding.Provider),
		// The creator's frozen effective detach action is recorded at
		// materialization so retention honors the class default even for a
		// workspace whose Task never reached attachment; the attach path
		// refreshes it with the attached Task's frozen choice.
		acpWorkspaceDetachActionAnnotation: binding.Class.EffectiveOnDetach,
		// Initial materialization is pending demand exactly like a cold
		// resume: a slow first provisioning can outlast idleTimeout, and
		// retention must not suspend or delete the workspace out from under
		// the Task still waiting to attach. The record clears on attachment
		// (or when settlement suspends the workspace); the frozen maxLifetime
		// stays the hard bound if the creator dies before attaching.
		acpWorkspaceResumeRequestedAnnotation: time.Now().UTC().Format(time.RFC3339Nano) + " " + requesterName + " " + requesterUID,
	}
	if binding.Class.MaxSuspendedWorkspaces != nil {
		annotations[acpWorkspaceMaxSuspendedAnnotation] = strconv.FormatInt(int64(*binding.Class.MaxSuspendedWorkspaces), 10)
	}
	if runtimeNamespace != "" {
		// Freeze the realized runtime namespace so deletion proofs probe the
		// ORIGINAL provider-children namespace even if the controller flag
		// changes later. Callers resolve an empty flag to the workspace
		// namespace BEFORE this call so that resolution is frozen too: an
		// absent annotation would let a later non-empty flag redirect
		// deletion proofs to a namespace the original PVC never lived in.
		annotations[acpWorkspaceRuntimeNamespaceAnnotation] = runtimeNamespace
	}
	if binding.Class.SuspendMode == string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly) {
		// The frozen profile provisions durable artifacts (a checkpoint or a
		// durable PVC); the terminal disposition reports what actually
		// existed instead of inferring it from allowed detach actions.
		annotations[acpWorkspaceDurableAnnotation] = booleanTrueValue
	}
	return annotations
}

// verifyACPClassWorkspace fail-closes adoption: an existing workspace must
// carry exactly the frozen class, provider, session, and slot bindings.
func verifyACPClassWorkspace(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	binding *ACPRuntimeWorkspaceBinding,
) error {
	if workspace == nil {
		return fmt.Errorf("%w: workspace is missing", errACPWorkspaceBindingConflict)
	}
	if !workspace.DeletionTimestamp.IsZero() {
		return fmt.Errorf("%w: workspace %s is deleting", errACPWorkspaceBindingConflict, workspace.Name)
	}
	if workspace.Labels[workspacev1alpha1.QuarantinedLabel] == booleanTrueValue {
		return fmt.Errorf("%w: workspace %s is quarantined and must never be reused", errACPWorkspaceBindingConflict, workspace.Name)
	}
	if workspace.Spec.Mode != workspacev1alpha1.ExecutionWorkspaceModeInteractive {
		return fmt.Errorf("%w: workspace %s mode %q is not Interactive", errACPWorkspaceBindingConflict, workspace.Name, workspace.Spec.Mode)
	}
	if workspace.Spec.ClassBinding.Name != binding.Class.Name ||
		string(workspace.Spec.ClassBinding.UID) != binding.Class.UID ||
		workspace.Spec.ClassBinding.Generation != binding.Class.Generation ||
		workspace.Spec.ClassBinding.ProfileHash != binding.Class.ProfileHash {
		return fmt.Errorf("%w: workspace %s class binding does not match the frozen class identity", errACPWorkspaceBindingConflict, workspace.Name)
	}
	if workspace.Spec.ProviderBinding.Name != binding.Class.ProviderName ||
		string(workspace.Spec.ProviderBinding.UID) != binding.Class.ProviderUID ||
		workspace.Spec.ProviderBinding.Generation != binding.Class.ProviderGeneration {
		return fmt.Errorf("%w: workspace %s provider binding does not match the frozen provider identity", errACPWorkspaceBindingConflict, workspace.Name)
	}
	if workspace.Annotations[acpWorkspaceProviderConfigUIDAnnotation] != binding.Class.ProviderConfigUID ||
		workspace.Annotations[acpWorkspaceBackendAnnotation] != string(binding.Provider) {
		return fmt.Errorf("%w: workspace %s provider config or backend does not match the frozen binding", errACPWorkspaceBindingConflict, workspace.Name)
	}
	if workspace.Spec.Slot != binding.WorkspaceSlot {
		return fmt.Errorf("%w: workspace %s slot does not match the frozen binding", errACPWorkspaceBindingConflict, workspace.Name)
	}
	if binding.ReusePolicy == corev1alpha1.WorkspaceReusePolicySession {
		if workspace.Spec.SessionRef == nil || string(workspace.Spec.SessionRef.UID) != binding.SessionUID {
			return fmt.Errorf("%w: workspace %s session identity does not match the immutable Session UID", errACPWorkspaceBindingConflict, workspace.Name)
		}
	} else {
		owned := false
		for _, owner := range workspace.OwnerReferences {
			if owner.UID == task.UID {
				owned = true
				break
			}
		}
		if workspace.Spec.SessionRef != nil || !owned {
			return fmt.Errorf("%w: workspace %s is not owned by this Task", errACPWorkspaceBindingConflict, workspace.Name)
		}
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted ||
		workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined {
		return fmt.Errorf("%w: workspace %s desired state %q cannot admit new work", errACPWorkspaceBindingConflict, workspace.Name, workspace.Spec.DesiredState)
	}
	return nil
}

// settleACPClassWorkspace revokes this Task's attachment and applies the
// Delete detach action once the Task is terminal or finalizing. Every step is
// idempotent and restart-safe; done=false requests a requeue while the
// adapter still enforces the revoked epoch.
func (r *TaskReconciler) settleACPClassWorkspace(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if task == nil {
		return true, nil
	}
	if !r.WorkspaceSettlementProtected {
		// Without Task provenance admission the reserved workspace-link
		// metadata is forgeable by any direct Task writer; the privileged
		// revocation and detach actions below must not run from it. The Task
		// releases and the workspace is cleaned through explicit workspace
		// deletion (adapter finalizers do not depend on Task metadata).
		logf.FromContext(ctx).Info("skipping privileged class-workspace settlement; Task provenance admission is disabled",
			"task", task.Name)
		return true, nil
	}
	name := strings.TrimSpace(task.Labels[acpExecutionWorkspaceLinkLabel])
	if name == "" {
		return true, nil
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	// Absence is proven through the uncached reader: a just-created session
	// workspace can be invisible to the informer cache, and a cache miss must
	// never release the Task finalizer while an orphan without a Task owner
	// reference survives to occupy the deterministic session name.
	settleReader := client.Reader(r.Client)
	if r.APIReader != nil {
		settleReader = r.APIReader
	}
	if err := settleReader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if !workspace.DeletionTimestamp.IsZero() {
		return true, nil
	}
	// The link label is mutable Task metadata: revalidate that the named
	// workspace is an ACP-owned workspace that actually belongs to this Task
	// (or its Session) before revoking or deleting anything with controller
	// privileges. An unverified target is skipped, never acted on.
	if !settlementWorkspaceBelongsToTask(workspace, task) {
		return true, nil
	}
	if task.Annotations[acpTaskWorkspaceSettledAnnotation] != "" {
		// This Task's detach action already settled the workspace; a later
		// state (for example a continuation's resume request) belongs to the
		// newer attachment.
		return true, nil
	}
	receiptUID, receiptEpoch, receiptOK := parseACPWorkspaceSettlementReceipt(
		workspace.Annotations[acpWorkspaceLastSettledTaskAnnotation])
	if receiptOK && receiptUID == string(task.UID) {
		// The workspace-side receipt proves this Task's suspension patch
		// landed even though the controller died before the Task marker did;
		// complete the marker instead of re-applying the detach action.
		return true, r.markACPTaskWorkspaceSettled(ctx, task)
	}
	if receiptOK && receiptEpoch > 0 {
		// Receipts are monotonic by attachment epoch: a receipt from an
		// equal-or-later epoch proves a LATER settlement displaced this
		// Task's own (its suspension patch either landed and was overwritten,
		// or was superseded by newer session state a continuation resumed).
		// Re-applying the detach action now would corrupt the successor's
		// workspace, so the displaced settlement completes as done.
		if taskEpoch := acpTaskRecordedAttachmentEpoch(task); taskEpoch > 0 && receiptEpoch >= taskEpoch {
			return true, r.markACPTaskWorkspaceSettled(ctx, task)
		}
	}
	if task.Annotations[acpExecutionWorkspaceUIDAnnotation] != string(workspace.UID) {
		// The controller-written incarnation pin is REQUIRED before any
		// privileged action: a forged or stripped link (the label and the
		// session name are client-visible metadata) must never let a Task
		// revoke or delete a workspace it did not attach, and a recorded UID
		// from a different incarnation (a Session recreated under the same
		// name) is equally skipped.
		return true, nil
	}
	manager := WorkspaceAttachmentManager{Client: r.Client}
	if attachment := workspace.Spec.Attachment; attachment != nil {
		if attachment.TaskRef.UID != task.UID {
			// Another Task attached the session-reused workspace; its own
			// settle flow owns revocation and the detach action. The done
			// decision is made durable on THIS Task: a later reconcile that
			// finds the workspace unattached again (and this Task's receipt
			// displaced) must never re-apply a stale detach action to the
			// successor's session state.
			return true, r.markACPTaskWorkspaceSettled(ctx, task)
		}
		// The pending-detach stamp becomes durable BEFORE the attachment
		// clears: a continuation observing a nil attachment must also see the
		// pending Delete settlement and wait for the fresh workspace.
		if err := r.markACPWorkspaceRevocationStarted(ctx, workspace, attachment.Epoch); err != nil {
			return false, err
		}
		if err := manager.BeginRevocation(ctx, workspace, attachment.Epoch); err != nil {
			return false, err
		}
		// Settlement stays pending after initiating revocation: an immediate
		// cached re-read can still show the pre-patch attachment and would
		// otherwise report the detach action applied without ever running
		// it. The caller requeues, and the next pass finalizes against the
		// converged object.
		return false, nil
	}
	if workspace.Spec.Attachment == nil && workspace.Spec.AttachmentEpoch > 0 {
		epoch := workspace.Spec.AttachmentEpoch
		// Restart-safe: ensure the deadline stamp exists for exactly this
		// epoch before enforcing it.
		if err := r.markACPWorkspaceRevocationStarted(ctx, workspace, epoch); err != nil {
			return false, err
		}
		if err := manager.FinalizeRevocation(ctx, workspace, epoch, attachmentSecretName(workspace.Name, epoch)); err != nil {
			// The adapter has not cleared the enforced epoch yet. The frozen
			// detachTimeout bounds how long Task finalization waits: past the
			// deadline the workspace is quarantined fail-closed and this Task
			// releases instead of retaining its finalizer forever.
			expired, deadlineErr := r.quarantineACPWorkspacePastDetachTimeout(ctx, workspace)
			if deadlineErr != nil {
				return false, deadlineErr
			}
			return expired, nil
		}
	}
	if workspace.Spec.Attachment != nil {
		return true, nil
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined {
		// Quarantine is terminal: a retry after the adapter released the
		// epoch must complete the credential cleanup idempotently and never
		// execute any detach action on the preserved evidence.
		if epoch := workspace.Spec.AttachmentEpoch; epoch > 0 {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: attachmentSecretName(workspace.Name, epoch), Namespace: workspace.Namespace,
			}}
			if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("delete quarantined attachment Secret: %w", err)
			}
		}
		lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
			Name: attachmentLeaseName(workspace.Name), Namespace: workspace.Namespace,
		}}
		if err := r.Delete(ctx, lease); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete quarantined attachment Lease: %w", err)
		}
		return true, nil
	}
	// Apply the attached Task's frozen effective detach action. Suspend
	// quiesced above: the attachment is revoked and the adapter released the
	// enforced epoch, so no prompt or workspace writer remains. The
	// UID+resourceVersion preconditions keep a concurrent re-attachment from
	// losing its workspace: the API server rejects the write if the object
	// changed after this read, and a Task that attached in between settles at
	// its own settle time anyway.
	if workspace.Annotations[acpWorkspaceDetachActionAnnotation] == string(workspacev1alpha1.WorkspaceOnDetachSuspend) {
		if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
			// The suspension may have been requested by retention (an idle
			// timeout firing between revocation and settlement) with no
			// settlement receipt. Stamp this Task's receipt on the workspace
			// BEFORE the separate Task marker patch: if the controller dies
			// in between, the restarted reconcile finds the receipt and
			// completes the marker instead of re-suspending newer session
			// state a continuation has since resumed.
			if existingUID, _, _ := parseACPWorkspaceSettlementReceipt(
				workspace.Annotations[acpWorkspaceLastSettledTaskAnnotation]); existingUID != string(task.UID) {
				base := workspace.DeepCopy()
				if workspace.Annotations == nil {
					workspace.Annotations = map[string]string{}
				}
				workspace.Annotations[acpWorkspaceLastSettledTaskAnnotation] =
					formatACPWorkspaceSettlementReceipt(string(task.UID), workspace.Spec.AttachmentEpoch)
				if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
					if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
						return false, nil
					}
					return false, err
				}
			}
			return true, r.markACPTaskWorkspaceSettled(ctx, task)
		}
		reader := client.Reader(r.Client)
		if r.APIReader != nil {
			reader = r.APIReader
		}
		err := suspendACPWorkspaceWithinQuota(ctx, r.Client, reader, workspace, time.Now(), string(task.UID))
		switch {
		case errors.Is(err, errACPSuspendQuotaExhausted):
			// The class retention cap is exhausted. The only admitted
			// deletion policy is all-Delete, so falling back to the Delete
			// disposition is exactly the frozen policy rather than a
			// silent downgrade. A live queued continuation is honored here
			// exactly like the ordinary Delete branch: a pre-attachment
			// requester's fallback must not destroy the retained repository
			// under a surviving waiter.
			if deferred, retry, deferErr := r.deferACPSettlementToSuccessor(ctx, workspace, task); deferErr != nil {
				return false, deferErr
			} else if retry {
				return false, nil
			} else if deferred {
				return true, nil
			}
			if r.Recorder != nil {
				if limit := acpWorkspaceSuspendedCapFromAnnotation(workspace); limit != nil {
					r.Recorder.Eventf(workspace, corev1.EventTypeWarning, "SuspendQuotaExhausted",
						"class retention cap of %d suspended workspaces is exhausted; applying the Delete disposition", *limit)
				}
			}
			if err := r.Delete(ctx, workspace, deleteCurrentObjectPreconditions(workspace)...); err != nil &&
				!apierrors.IsNotFound(err) {
				if apierrors.IsConflict(err) {
					// A conflicted quota-fallback delete retries settlement:
					// the Task must not release while the Delete disposition
					// is still owed, and no action is recorded for a delete
					// that did not happen.
					return false, nil
				}
				return false, err
			}
			metrics.RecordACPWorkspaceRetentionAction("delete", "suspend_quota_exhausted")
			return true, nil
		case apierrors.IsConflict(err) || apierrors.IsNotFound(err):
			return false, nil
		case err != nil:
			return false, err
		}
		return true, r.markACPTaskWorkspaceSettled(ctx, task)
	}
	// The destructive path revalidates the frozen policy it is about to
	// execute: an action this controller cannot execute (written by a newer
	// controller) or retained deletion categories must fail closed after a
	// rollback instead of being deleted under a contract this version cannot
	// honor.
	if action := workspace.Annotations[acpWorkspaceDetachActionAnnotation]; action != "" &&
		action != string(workspacev1alpha1.WorkspaceOnDetachDelete) {
		return false, fmt.Errorf(
			"workspace %s froze detach action %q, which this controller cannot execute; refusing destructive settlement",
			workspace.Name, action,
		)
	}
	for _, deletionAction := range []workspacev1alpha1.WorkspaceDeletionAction{
		workspace.Spec.Lifecycle.DeletionPolicy.ProviderResources,
		workspace.Spec.Lifecycle.DeletionPolicy.PersistentVolumes,
		workspace.Spec.Lifecycle.DeletionPolicy.Checkpoints,
	} {
		if deletionAction != workspacev1alpha1.WorkspaceDeletionActionDelete {
			return false, fmt.Errorf(
				"workspace %s deletion policy retains resources; this controller only executes the all-Delete lifecycle and fails closed",
				workspace.Name,
			)
		}
	}
	// A requester that terminated before it ever executed against this
	// workspace (its cold-resume flip stored the Delete action, but no
	// RuntimePool demand was ever stamped) must not destroy the retained
	// repository out from under another live queued continuation: the
	// successor attaches and stamps its own frozen action instead. A Task
	// that actually executed keeps the normal contract - its frozen Delete
	// destroys the filesystem before any continuation runs.
	if deferred, retry, deferErr := r.deferACPSettlementToSuccessor(ctx, workspace, task); deferErr != nil {
		return false, deferErr
	} else if retry {
		return false, nil
	} else if deferred {
		return true, nil
	}
	if err := r.Delete(ctx, workspace, deleteCurrentObjectPreconditions(workspace)...); err != nil &&
		!apierrors.IsNotFound(err) {
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// deferACPSettlementToSuccessor completes a pre-attachment requester's
// settlement without destroying the workspace when another live continuation
// is queued for the same incarnation, transferring the successor's effective
// detach policy. deferred reports completion; retry requests a settle
// requeue. An UNLINKED successor's resolved Session UID must match the
// workspace's immutable Session identity when the durable session stores are
// available - a recreated same-name Session can never adopt the old
// incarnation's cleanup.
func (r *TaskReconciler) deferACPSettlementToSuccessor(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
) (bool, bool, error) {
	if workspace.Spec.SessionRef == nil || !taskNeverHeldACPWorkspaceAttachment(task) {
		return false, false, nil
	}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	successor, successorErr := firstLiveACPSessionContinuation(ctx, reader, workspace, task.UID)
	if successorErr != nil {
		return false, false, successorErr
	}
	if successor == nil {
		return false, false, nil
	}
	linked := strings.TrimSpace(successor.Labels[acpExecutionWorkspaceLinkLabel]) == workspace.Name &&
		strings.TrimSpace(successor.Annotations[acpExecutionWorkspaceUIDAnnotation]) == string(workspace.UID)
	if !linked && r.DurableControlStore != nil && r.SessionManager != nil && r.ControllerEpochManager != nil {
		resolvedUID, resolveErr := r.planACPWorkspaceSessionUID(ctx, successor)
		if resolveErr != nil {
			// The successor's Session identity cannot be proven right now;
			// retry rather than either destroying under a possibly valid
			// waiter or transferring ownership to a recreated Session.
			return false, true, nil //nolint:nilerr // fail closed into a settle retry
		}
		if strings.TrimSpace(resolvedUID) != string(workspace.Spec.SessionRef.UID) {
			// A recreated same-name Session resolves a different immutable
			// UID and can never attach this incarnation: it is not a
			// successor, and this settlement keeps cleanup ownership.
			return false, false, nil
		}
	}
	// The deferral transfers the SUCCESSOR's policy onto the workspace: the
	// surviving waiter stamps its own action only at attachment, and leaving
	// the dead requester's Delete in place would destroy the retained
	// workspace if the successor also terminates before attaching.
	successorAction := string(workspace.Spec.Lifecycle.DefaultOnDetach)
	if successor.Spec.Execution != nil && successor.Spec.Execution.Workspace != nil &&
		strings.TrimSpace(string(successor.Spec.Execution.Workspace.OnDetach)) != "" {
		requested := workspacev1alpha1.WorkspaceOnDetach(strings.TrimSpace(string(successor.Spec.Execution.Workspace.OnDetach)))
		if !slices.Contains(workspace.Spec.Lifecycle.AllowedOnDetach, requested) {
			// The waiter's explicit override is outside the class policy, so
			// its own attachment resolution will reject it: it can never
			// attach this workspace and is NOT a successor - this settlement
			// keeps cleanup ownership instead of transferring a policy the
			// class forbids.
			return false, false, nil
		}
		successorAction = string(requested)
	}
	if workspace.Annotations[acpWorkspaceDetachActionAnnotation] != successorAction {
		base := workspace.DeepCopy()
		if workspace.Annotations == nil {
			workspace.Annotations = map[string]string{}
		}
		workspace.Annotations[acpWorkspaceDetachActionAnnotation] = successorAction
		if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				return false, true, nil
			}
			return false, false, err
		}
	}
	return true, false, nil
}

// taskNeverHeldACPWorkspaceAttachment reports a Task that terminated before
// any RuntimePool demand existed for it: ACP dispatch stamps the pool
// identity into status.execution only after the workspace attachment is
// admitted, so its absence proves the Task never executed against the
// workspace it links.
func taskNeverHeldACPWorkspaceAttachment(task *corev1alpha1.Task) bool {
	return task.Status.Execution == nil || strings.TrimSpace(task.Status.Execution.RuntimePoolName) == ""
}

// settlementWorkspaceBelongsToTask revalidates a settlement target found
// through the mutable Task link label: it must be an ACP-owned workspace whose
// session identity matches this Task's Session, or a per-Task workspace owned
// by exactly this Task.
func settlementWorkspaceBelongsToTask(workspace *workspacev1alpha1.ExecutionWorkspace, task *corev1alpha1.Task) bool {
	if workspace.Labels[workspacev1alpha1.ProviderControllerLabel] != acpWorkspaceControllerLabelValue {
		return false
	}
	if workspace.Spec.SessionRef != nil {
		return task.Spec.SessionRef != nil &&
			strings.TrimSpace(task.Spec.SessionRef.Name) == workspace.Spec.SessionRef.Name
	}
	for _, owner := range workspace.OwnerReferences {
		if owner.UID == task.UID {
			return true
		}
	}
	return false
}

// markACPWorkspaceRevocationStarted stamps the revocation instant for exactly
// one attachment epoch ("<epoch> <RFC3339Nano>"), so the frozen detachTimeout
// is measured per revocation: a stale stamp from a previous epoch can never
// serve as a later attachment's deadline and instantly quarantine a live
// workspace.
func (r *TaskReconciler) markACPWorkspaceRevocationStarted(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	epoch int64,
) error {
	if stampedEpoch, _, ok := parseACPWorkspaceRevocationStamp(workspace.Annotations[acpWorkspaceRevocationStartedAnnotation]); ok && stampedEpoch == epoch {
		return nil
	}
	base := workspace.DeepCopy()
	if workspace.Annotations == nil {
		workspace.Annotations = map[string]string{}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	workspace.Annotations[acpWorkspaceRevocationStartedAnnotation] = fmt.Sprintf("%d %s", epoch, now)
	// The detach instant is stamped as revocation begins so idle retention
	// never falls back to the creation timestamp while the epoch settles.
	workspace.Annotations[acpWorkspaceLastDetachedAnnotation] = now
	if err := r.Patch(ctx, workspace, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// parseACPWorkspaceRevocationStamp parses an epoch-bound revocation stamp.
func parseACPWorkspaceRevocationStamp(raw string) (int64, time.Time, bool) {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) != 2 {
		return 0, time.Time{}, false
	}
	epoch, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, time.Time{}, false
	}
	startedAt, err := time.Parse(time.RFC3339Nano, fields[1])
	if err != nil {
		return 0, time.Time{}, false
	}
	return epoch, startedAt, true
}

// quarantineACPWorkspacePastDetachTimeout enforces the frozen detachTimeout on
// a revocation the adapter never finalizes: past the deadline the workspace is
// quarantined fail-closed and settlement reports done so the Task releases.
func (r *TaskReconciler) quarantineACPWorkspacePastDetachTimeout(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (bool, error) {
	stampedEpoch, startedAt, ok := parseACPWorkspaceRevocationStamp(workspace.Annotations[acpWorkspaceRevocationStartedAnnotation])
	if !ok || stampedEpoch != workspace.Spec.AttachmentEpoch {
		// No stamp for the epoch being finalized: the deadline is measured
		// from this revocation, never from a previous attachment's stamp.
		return false, nil
	}
	if time.Since(startedAt) <= workspace.Spec.Lifecycle.DetachTimeout.Duration {
		return false, nil
	}
	base := workspace.DeepCopy()
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
	if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	// Quarantine is terminal and nothing revisits this workspace's
	// credentials afterwards (the adapter destroys only the pool, and the
	// Task releases now): the attachment Secret and Lease must be removed
	// here or the bearer credential outlives the workspace's usable life.
	epoch := workspace.Spec.AttachmentEpoch
	if epoch > 0 {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: attachmentSecretName(workspace.Name, epoch), Namespace: workspace.Namespace,
		}}
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete quarantined attachment Secret: %w", err)
		}
	}
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: attachmentLeaseName(workspace.Name), Namespace: workspace.Namespace,
	}}
	if err := r.Delete(ctx, lease); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("delete quarantined attachment Lease: %w", err)
	}
	return true, nil
}

// markACPTaskWorkspaceSettled records on the Task itself that its detach
// action is in effect, so the terminal Task's ongoing reconciles never
// re-apply the action against a newer attachment's state. Per-Task markers
// stay valid for every settled Task of a shared session workspace.
// markACPTaskAttachmentEpoch records the attachment epoch this Task holds so
// its later settlement can recognize a displaced receipt.
func (r *TaskReconciler) markACPTaskAttachmentEpoch(
	ctx context.Context,
	task *corev1alpha1.Task,
	epoch int64,
) error {
	value := strconv.FormatInt(epoch, 10)
	if task.Annotations[acpTaskAttachmentEpochAnnotation] == value {
		return nil
	}
	base := task.DeepCopy()
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[acpTaskAttachmentEpochAnnotation] = value
	if err := r.Patch(ctx, task, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// acpTaskRecordedAttachmentEpoch parses the Task-side attachment epoch, or 0.
func acpTaskRecordedAttachmentEpoch(task *corev1alpha1.Task) int64 {
	epoch, err := strconv.ParseInt(strings.TrimSpace(task.Annotations[acpTaskAttachmentEpochAnnotation]), 10, 64)
	if err != nil || epoch < 0 {
		return 0
	}
	return epoch
}

// parseACPWorkspaceSettlementReceipt parses a settlement receipt. The legacy
// single-field form carries only the Task UID (epoch 0); the current form is
// "<taskUID> <attachmentEpoch>".
func parseACPWorkspaceSettlementReceipt(raw string) (string, int64, bool) {
	fields := strings.Fields(strings.TrimSpace(raw))
	switch len(fields) {
	case 1:
		return fields[0], 0, true
	case 2:
		epoch, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || epoch < 0 {
			return fields[0], 0, true
		}
		return fields[0], epoch, true
	default:
		return "", 0, false
	}
}

// formatACPWorkspaceSettlementReceipt renders the epoch-bound receipt form.
func formatACPWorkspaceSettlementReceipt(taskUID string, epoch int64) string {
	return taskUID + " " + strconv.FormatInt(epoch, 10)
}

func (r *TaskReconciler) markACPTaskWorkspaceSettled(
	ctx context.Context,
	task *corev1alpha1.Task,
) error {
	if task.Annotations[acpTaskWorkspaceSettledAnnotation] != "" {
		return nil
	}
	base := task.DeepCopy()
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[acpTaskWorkspaceSettledAnnotation] = booleanTrueValue
	if err := r.Patch(ctx, task, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// reconcileACPClassWorkspaceSettlement applies workspace settlement for a
// terminal ACP Task: it mirrors the Released projection predicate so the
// attachment is revoked and the Delete detach action applied exactly when the
// Task's workspace demand is released.
func (r *TaskReconciler) reconcileACPClassWorkspaceSettlement(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent || task.Status.Execution == nil {
		return true, nil
	}
	// The controller-owned workspace link is the settlement trigger, not the
	// later runtime-name projection: a terminal planning failure can occur
	// after the workspace attached but before RuntimePoolName was written,
	// and that attachment must still be revoked.
	if !taskManagedByACP(task) && strings.TrimSpace(task.Labels[acpExecutionWorkspaceLinkLabel]) == "" {
		return true, nil
	}
	if !taskExecutionStateTerminal(task.Status.Execution.State) {
		return true, nil
	}
	if task.Status.Execution.State == corev1alpha1.TaskExecutionStateSucceeded &&
		(task.Status.Delivery == nil || !store.IsTerminalPromptDeliveryState(store.PromptDeliveryState(task.Status.Delivery.State))) {
		return true, nil
	}
	// Detach actions must wait for the whole Task to settle, not just the
	// prompt: SessionTurn finalization and artifact retirement still need the
	// live runtime, and a Suspend that races them kills the supervisor
	// mid-finalization and fails an otherwise-succeeded Task. Deletion-driven
	// settlement (a deleting Task) proceeds regardless.
	if task.DeletionTimestamp.IsZero() &&
		task.Status.Phase != corev1alpha1.TaskPhaseSucceeded &&
		task.Status.Phase != corev1alpha1.TaskPhaseFailed &&
		task.Status.Phase != corev1alpha1.TaskPhaseCancelled {
		return true, nil
	}
	done, err := r.settleACPClassWorkspace(ctx, task)
	if err != nil || !done {
		return done, err
	}
	return true, r.refreshACPReleasedWorkspaceProjection(ctx, task)
}

// refreshACPReleasedWorkspaceProjection re-projects the class attachment
// identity once settlement completes: the Released transition runs before
// revocation, so without this refresh the terminal status would permanently
// claim state Attached and the pre-revocation attachment epoch.
func (r *TaskReconciler) refreshACPReleasedWorkspaceProjection(ctx context.Context, task *corev1alpha1.Task) error {
	current := task.Status.ExecutionWorkspace
	if current == nil || current.Phase != corev1alpha1.ExecutionWorkspacePhaseReleased ||
		(current.AttachedEpoch == 0 && current.State != string(workspacev1alpha1.ExecutionWorkspaceStateAttached)) {
		return nil
	}
	next := current.DeepCopy()
	next.AttachedEpoch = 0
	next.State = ""
	if name := strings.TrimSpace(task.Labels[acpExecutionWorkspaceLinkLabel]); name != "" {
		workspace := &workspacev1alpha1.ExecutionWorkspace{}
		// A workspace held only by its cleanup finalizer (a settlement Delete
		// in flight) still serves cached pre-delete status; copying it would
		// freeze a permanently stale Ready/Attached claim into the released
		// Task. Its state stays cleared instead.
		if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err == nil &&
			workspace.DeletionTimestamp.IsZero() &&
			workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined &&
			workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredDeleted &&
			task.Annotations[acpExecutionWorkspaceUIDAnnotation] == string(workspace.UID) {
			// A maintenance intent (quarantine or deletion) means the cached
			// status may still show the pre-patch Attached/Ready state; the
			// Task controller never revisits it, so nothing stale is copied.
			next.State = string(workspace.Status.State)
		}
	}
	base := task.DeepCopy()
	task.Status.ExecutionWorkspace = next
	return r.Status().Patch(ctx, task, client.MergeFrom(base))
}
