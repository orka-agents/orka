package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/protectedset"
	"github.com/orka-agents/orka/internal/store"
)

const (
	// proposerConstraintViolationReason fails a proposer Task that lost any
	// of its constraints instead of running it.
	proposerConstraintViolationReason = corev1alpha1.TaskExecutionReason("ProposerConstraintViolation")

	// proposerPushBranchPrefix is the fixed prefix every proposer push
	// branch must use. Controller-derived publication branches and the
	// RepositoryMonitor default implementation prefix both live under it, so
	// a proposer can never name a default or release branch.
	proposerPushBranchPrefix = "orka/"
)

// checkProposerConstraints returns a non-empty violation when a Task marked
// with an improvement role must not run. It loads the current protected set
// on every call, so a protected-set change applies to the next dispatch. A
// transient read error is returned so the caller retries.
func (r *TaskReconciler) checkProposerConstraints(
	ctx context.Context,
	task *corev1alpha1.Task,
	bound *verifiedAgentExecution,
) (string, error) {
	return verifyProposerConstraints(ctx, r.ProtectedSet, r.Recorder, task, bound)
}

// verifyProposerConstraints loads the current protected set from source and
// checks a marked Task against it. Both queue admission and the dispatcher,
// immediately before it starts a reserved attempt, call it.
func verifyProposerConstraints(
	ctx context.Context,
	source protectedset.Source,
	recorder record.EventRecorder,
	task *corev1alpha1.Task,
	bound *verifiedAgentExecution,
) (string, error) {
	if task == nil || taskImprovementRole(task.Annotations) == "" {
		return "", nil
	}
	if bound == nil {
		return "a proposer requires a verified execution binding", nil
	}
	set, err := source.Load(ctx)
	if err != nil {
		if errors.Is(err, protectedset.ErrUnavailable) {
			return "proposer flows are disabled: " + err.Error(), nil
		}
		return "", err
	}
	externalEndpoint := bound.binding != nil && bound.binding.Backend == corev1alpha1.AgentExecutionBackendExternalEndpoint
	if violation := proposerConstraintViolation(task, bound.mcpConfiguration.ToolPolicy.AllowedToolNames, externalEndpoint, set); violation != "" {
		return violation, nil
	}
	logf.FromContext(ctx).Info("proposer Task constraints verified",
		"namespace", task.Namespace, "task", task.Name, "protectedSetDigest", set.Digest())
	if recorder != nil {
		recorder.Eventf(task, corev1.EventTypeNormal, "ProposerConstraintsVerified",
			"Proposer constraints verified against protected set %s", set.Digest())
	}
	return "", nil
}

// rejectProposerBeforeExecution re-checks a marked Task against the current
// protected set right before the dispatcher creates its Session or prompt. A
// queued or recovered attempt may have waited while the protected set changed
// or became unavailable, so a violation fails the attempt and the Task instead
// of running with stale constraints. It reports whether the Task was handled.
func (d *ACPDispatcher) rejectProposerBeforeExecution(
	ctx context.Context,
	task *corev1alpha1.Task,
	bound *verifiedAgentExecution,
	attemptID string,
	fence store.ControllerEpochFence,
) (bool, error) {
	violation, err := verifyProposerConstraints(ctx, d.ProtectedSet, nil, task, bound)
	if err != nil {
		return true, d.requeueReservedTask(ctx, task, acpReservedRetryProposerConstraints, err)
	}
	if violation == "" {
		return false, nil
	}
	message := boundACPStatusMessage(violation)
	if err := d.transitionAttemptToFailed(
		ctx, attemptID, fence, "proposer-constraint-violation", proposerConstraintViolationReason, message,
	); err != nil {
		return true, err
	}
	return true, d.failTaskBeforeSessionBinding(
		ctx, task, corev1alpha1.TaskExecutionStateFailed, corev1alpha1.TaskExecutionOutcomeFailed,
		proposerConstraintViolationReason, message,
	)
}

// proposerConstraintViolation checks a marked Task against every proposer
// constraint. effectiveTools is the exact tool allowlist frozen into the
// Task's runtime profile, and externalEndpoint reports whether its execution
// binding dispatches to an external runtime endpoint.
//
//nolint:gocyclo // Every proposer constraint is listed together so none is skipped.
func proposerConstraintViolation(task *corev1alpha1.Task, effectiveTools []string, externalEndpoint bool, set *protectedset.Set) string {
	role := taskImprovementRole(task.Annotations)
	if role != labels.ImprovementRoleProposer {
		return fmt.Sprintf("improvement role %q is not supported", role)
	}
	if set == nil {
		return "proposer flows are disabled: no protected set is available"
	}
	if task.Spec.Type != corev1alpha1.TaskTypeAgent {
		return "a proposer must be an agent Task"
	}
	// The token mount, network isolation and startup attestation a proposer
	// relies on come from a controller-created RuntimePool. An external
	// endpoint or an execution workspace class runs outside that pool.
	if externalEndpoint {
		return "a proposer must run on the built-in RuntimePool backend, not an external runtime endpoint"
	}
	if taskRequestsWorkspaceClass(task) {
		return "a proposer cannot use an execution workspace class"
	}
	// Held-out fixtures sit in a private repository that only trial Tasks get
	// a read credential for, so a proposer never holds one. It has no
	// Kubernetes or Orka API token and only propose_memory among brokered
	// tools, so it can reach neither held-out cases nor trial transcripts in
	// the namespace it shares with them.
	if workspace := task.Spec.Workspace; workspace != nil && workspace.ReadCredentialRef != nil {
		return "a proposer cannot have a workspace read credential"
	}
	// Continuing a Session or forking another Task would hand the proposer a
	// transcript it was not built from.
	if task.Spec.SessionRef != nil {
		return "a proposer cannot continue a Session"
	}
	if _, forked := task.Annotations[labels.AnnotationForkSourceTask]; forked {
		return "a proposer cannot fork another Task's transcript"
	}
	if task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.AllowedTools == nil {
		return "a proposer requires an explicit agentRuntime.allowedTools list"
	}
	for _, name := range append(append([]string(nil), task.Spec.AgentRuntime.AllowedTools...), effectiveTools...) {
		if !proposerAllowedToolName(name) {
			return fmt.Sprintf("tool %q is not permitted for a proposer", name)
		}
	}
	if effectiveACPWorkspaceIntent(task) != corev1alpha1.WorkspaceIntentWrite {
		return ""
	}
	workspace := task.Spec.Workspace
	if workspace == nil {
		return "a write-intent proposer requires a workspace"
	}
	if !workspace.DenyRepositoryControlPaths {
		return "a write-intent proposer requires denyRepositoryControlPaths"
	}
	source, err := workspaceRepository(workspace)
	if err != nil {
		return "a write-intent proposer requires a valid source repository"
	}
	target, err := workspacePublicationRepository(workspace)
	if err != nil {
		return "a write-intent proposer requires a valid publication repository"
	}
	if missing := set.MissingDeniedPaths(workspace.DeniedPaths, source.ID, target.ID); len(missing) > 0 {
		return fmt.Sprintf("deniedPaths does not cover the current protected set (%d of its patterns missing, first %q)", len(missing), missing[0])
	}
	branch := strings.TrimPrefix(strings.TrimSpace(workspace.PushBranch), "refs/heads/")
	if !strings.HasPrefix(branch, proposerPushBranchPrefix) || len(branch) == len(proposerPushBranchPrefix) {
		return fmt.Sprintf("a write-intent proposer requires a pushBranch under %s", proposerPushBranchPrefix)
	}
	return ""
}
