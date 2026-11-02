package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/protectedset"
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
	mcpConfiguration harnessv2.MCPPolicyConfiguration,
) (string, error) {
	if task == nil || taskImprovementRole(task.Annotations) == "" {
		return "", nil
	}
	set, err := r.ProtectedSet.Load(ctx)
	if err != nil {
		if errors.Is(err, protectedset.ErrUnavailable) {
			return "proposer flows are disabled: " + err.Error(), nil
		}
		return "", err
	}
	if violation := proposerConstraintViolation(task, mcpConfiguration.ToolPolicy.AllowedToolNames, set); violation != "" {
		return violation, nil
	}
	logf.FromContext(ctx).Info("proposer Task constraints verified",
		"namespace", task.Namespace, "task", task.Name, "protectedSetDigest", set.Digest())
	if r.Recorder != nil {
		r.Recorder.Eventf(task, corev1.EventTypeNormal, "ProposerConstraintsVerified",
			"Proposer constraints verified against protected set %s", set.Digest())
	}
	return "", nil
}

// proposerConstraintViolation checks a marked Task against every proposer
// constraint. effectiveTools is the exact tool allowlist frozen into the
// Task's runtime profile.
//
//nolint:gocyclo // Every proposer constraint is listed together so none is skipped.
func proposerConstraintViolation(task *corev1alpha1.Task, effectiveTools []string, set *protectedset.Set) string {
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
	// Held-out cases and evaluation transcripts are namespace-scoped, so a
	// proposer never runs where a case set lives.
	if set.HoldsCaseSets(task.Namespace) {
		return "a proposer cannot run in a namespace that holds an evaluation case set"
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
