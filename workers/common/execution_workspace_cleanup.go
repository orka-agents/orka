/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"context"
	"fmt"
	"os"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/internal/workspace"
)

type executionWorkspaceCleanupOptions struct {
	skipSubstrateDeleteScrub  bool
	skipSubstrateReleaseScrub bool
	statusOptions             []executionWorkspaceStatusOption
}

func preTerminalExecutionWorkspaceCleanup(
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
	substrateHandoffBootstrapped bool,
	claimedNewWorkspace bool,
) (workerenv.ExecutionWorkspaceEnv, executionWorkspaceCleanupOptions) {
	if substrateHandoffBootstrapped ||
		strings.TrimSpace(workspaceEnv.Provider) != string(corev1alpha1.WorkspaceProviderSubstrate) {
		return workspaceEnv, executionWorkspaceCleanupOptions{}
	}
	options := executionWorkspaceCleanupOptions{
		skipSubstrateDeleteScrub:  true,
		skipSubstrateReleaseScrub: true,
	}
	if claimedNewWorkspace &&
		strings.EqualFold(
			strings.TrimSpace(workspaceEnv.CleanupPolicy),
			string(corev1alpha1.WorkspaceCleanupPolicyRetain),
		) {
		workspaceEnv.CleanupPolicy = string(corev1alpha1.WorkspaceCleanupPolicyDelete)
	}
	return workspaceEnv, options
}

func executionWorkspaceCleanupPolicy(workspaceEnv workerenv.ExecutionWorkspaceEnv) string {
	policy := corev1alpha1.WorkspaceCleanupPolicy(strings.TrimSpace(strings.ToLower(workspaceEnv.CleanupPolicy)))
	if strings.TrimSpace(workspaceEnv.Provider) == string(corev1alpha1.WorkspaceProviderSubstrate) &&
		strings.TrimSpace(workspaceEnv.PoolName) != "" &&
		policy != "" &&
		policy != corev1alpha1.WorkspaceCleanupPolicyDelete {
		return string(corev1alpha1.WorkspaceCleanupPolicyDelete)
	}
	return string(policy)
}

func executionWorkspaceDeferredCleanupSubmitsStatus(workspaceEnv workerenv.ExecutionWorkspaceEnv) bool {
	return strings.TrimSpace(workspaceEnv.Provider) == string(corev1alpha1.WorkspaceProviderSubstrate) &&
		strings.TrimSpace(workspaceEnv.PoolName) != ""
}

const (
	executionWorkspaceCleanupReasonDelete      = "execution workspace cleanup policy delete"
	executionWorkspaceCleanupReasonRetain      = "execution workspace cleanup policy retain"
	executionWorkspaceCleanupReasonUnsupported = "unsupported execution workspace cleanup policy"
)

type executionWorkspaceCleanupAction string

const (
	executionWorkspaceCleanupActionDelete executionWorkspaceCleanupAction = "delete"
	executionWorkspaceCleanupActionRetain executionWorkspaceCleanupAction = "retain"
)

type executionWorkspaceCleanupPlan struct {
	statusEnv       workerenv.ExecutionWorkspaceEnv
	action          executionWorkspaceCleanupAction
	operationReason string
	statusPhase     corev1alpha1.ExecutionWorkspacePhase
	statusReason    corev1alpha1.ExecutionWorkspaceReason
	statusMessage   string
	errorContext    string
	warnUnsupported bool
	preScrub        bool
}

func planExecutionWorkspaceCleanup(workspaceEnv workerenv.ExecutionWorkspaceEnv) executionWorkspaceCleanupPlan {
	cleanupPolicy := executionWorkspaceCleanupPolicy(workspaceEnv)
	statusEnv := workspaceEnv
	if cleanupPolicy != "" {
		statusEnv.CleanupPolicy = cleanupPolicy
	}
	plan := executionWorkspaceCleanupPlan{statusEnv: statusEnv}

	switch cleanupPolicy {
	case "retain":
		plan.action = executionWorkspaceCleanupActionRetain
		plan.operationReason = executionWorkspaceCleanupReasonRetain
		plan.statusPhase = corev1alpha1.ExecutionWorkspacePhaseRetained
		plan.statusReason = corev1alpha1.ExecutionWorkspaceReasonRetained
		plan.statusMessage = "workspace retained"
		plan.errorContext = "retain workspace"
		plan.preScrub = shouldPreScrubExecutionWorkspaceSecrets(workspaceEnv)
	case "", "delete":
		plan.action = executionWorkspaceCleanupActionDelete
		plan.operationReason = executionWorkspaceCleanupReasonDelete
		plan.statusPhase = corev1alpha1.ExecutionWorkspacePhaseDeleted
		plan.statusReason = corev1alpha1.ExecutionWorkspaceReasonDeleted
		plan.statusMessage = "workspace deleted"
		plan.errorContext = "delete workspace"
	default:
		plan.action = executionWorkspaceCleanupActionRetain
		plan.operationReason = executionWorkspaceCleanupReasonUnsupported
		plan.statusPhase = corev1alpha1.ExecutionWorkspacePhaseRetained
		plan.statusReason = corev1alpha1.ExecutionWorkspaceReasonRetained
		plan.statusMessage = "workspace retained"
		plan.errorContext = "retain workspace after unsupported cleanup policy"
		plan.warnUnsupported = true
		plan.preScrub = shouldPreScrubExecutionWorkspaceSecrets(workspaceEnv)
	}
	return plan
}

func executionWorkspaceCheckpointURI(workspaceEnv workerenv.ExecutionWorkspaceEnv) string {
	if !workspaceEnv.SnapshotOnRelease {
		return ""
	}
	return strings.TrimSpace(workspaceEnv.SnapshotCheckpointURI)
}
func cleanupExecutionWorkspace(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
	reused bool,
	submitStatus bool,
) error {
	return cleanupExecutionWorkspaceWithOptions(
		ctx,
		executor,
		ref,
		workspaceEnv,
		reused,
		submitStatus,
		executionWorkspaceCleanupOptions{},
	)
}

func cleanupExecutionWorkspaceWithOptions(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
	reused bool,
	submitStatus bool,
	options executionWorkspaceCleanupOptions,
) error {
	if ref.IsZero() || executor == nil {
		return nil
	}

	plan := planExecutionWorkspaceCleanup(workspaceEnv)
	if plan.warnUnsupported {
		fmt.Fprintf(
			os.Stderr,
			"warning: unsupported workspace cleanup policy %q; retaining workspace to avoid unintended deletion\n",
			workspaceEnv.CleanupPolicy,
		)
	}
	if plan.preScrub {
		if err := scrubExecutionWorkspaceSecrets(ctx, executor, ref, workspaceEnv); err != nil {
			return fmt.Errorf("%w: %w", errExecutionWorkspaceSecretScrubFailed, err)
		}
	}

	switch plan.action {
	case executionWorkspaceCleanupActionRetain:
		if _, err := executor.Release(ctx, workspace.ReleaseRequest{
			Ref:                   ref,
			Retain:                true,
			Reason:                plan.operationReason,
			Timeout:               workspaceEnv.ClaimTimeout,
			SkipScrub:             options.skipSubstrateReleaseScrub,
			SnapshotCheckpointURI: executionWorkspaceCheckpointURI(workspaceEnv),
		}); err != nil {
			return fmt.Errorf("%s: %w", plan.errorContext, err)
		}
	case executionWorkspaceCleanupActionDelete:
		if _, err := executor.Delete(ctx, workspace.DeleteRequest{
			Ref:       ref,
			Reason:    plan.operationReason,
			Timeout:   workspaceEnv.ClaimTimeout,
			SkipScrub: options.skipSubstrateDeleteScrub,
		}); err != nil {
			return fmt.Errorf("%s: %w", plan.errorContext, err)
		}
	}

	if submitStatus {
		submitExecutionWorkspaceStatus(
			plan.statusEnv,
			plan.statusPhase,
			plan.statusReason,
			reused,
			plan.statusMessage,
			options.statusOptions...,
		)
	}
	return nil
}

func shouldPreScrubExecutionWorkspaceSecrets(workspaceEnv workerenv.ExecutionWorkspaceEnv) bool {
	return strings.TrimSpace(workspaceEnv.Provider) != string(corev1alpha1.WorkspaceProviderSubstrate)
}

func scrubExecutionWorkspaceSecrets(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
) error {
	paths := executionWorkspaceScrubPaths()
	if len(paths) == 0 {
		return nil
	}
	_, err := executor.Exec(ctx, workspace.ExecRequest{
		Ref:            ref,
		Command:        append([]string{"rm", "-f"}, paths...),
		Timeout:        workspaceEnv.ClaimTimeout,
		MaxOutputBytes: agentSandboxExecMaxOutputBytes,
	})
	if err != nil {
		return fmt.Errorf("scrub execution workspace staged credentials: %w", err)
	}
	return nil
}

func executionWorkspaceScrubPaths() []string {
	paths := []string{
		agentSandboxWorkerExecPath,
		agentSandboxSATokenExecPath,
		agentSandboxTransactionTokenExecPath,
		agentSandboxContextSubjectTokenExecPath,
		agentSandboxGitAskpassExecPath,
		workspaceHandoffTokenDefaultPath,
	}
	if custom := strings.TrimSpace(os.Getenv(workspaceHandoffTokenFileEnv)); custom != "" {
		paths = appendUniqueString(paths, custom)
	}
	return paths
}
