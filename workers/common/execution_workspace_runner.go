/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"context"
	"errors"
	"fmt"
	"os"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/internal/workspace"
)

func runAgentInWorkspace(
	ctx context.Context,
	name string,
	workspaceDir string,
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
) error {
	handoffToken, err := ensureWorkspaceHandoffToken(workspaceEnv)
	if err != nil {
		return err
	}
	executor, err := executionWorkspaceExecutor(workspaceEnv)
	if err != nil {
		return err
	}
	if executor == nil {
		return fmt.Errorf("execution workspace executor is not configured for provider %q", workspaceEnv.Provider)
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve %s executable for workspace: %w", name, err)
	}
	taskNamespace := os.Getenv(workerenv.TaskNamespace)
	taskName := os.Getenv(workerenv.TaskName)
	templateNamespace := workspaceTemplateNamespace(workspaceEnv, taskNamespace)
	claimNamespace := workspaceClaimNamespace(workspaceEnv, taskNamespace, templateNamespace)
	claimName := workspaceClaimName(workspaceEnv, claimNamespace, taskNamespace, templateNamespace)

	claim, err := executor.Claim(ctx, workspace.ClaimRequest{
		Namespace:       claimNamespace,
		TaskName:        taskName,
		ClaimName:       claimName,
		CreateIfMissing: true,
		Template: workspace.TemplateRef{
			Namespace: templateNamespace,
			Name:      workspaceEnv.TemplateName,
		},
		ReuseKey:       workspaceEnv.ReuseKey,
		WarmPoolPolicy: workspaceWarmPoolPolicy(workspaceEnv),
		Timeout:        workspaceEnv.ClaimTimeout,
		// Size the workspace executor's HTTP transport for long single Exec
		// calls. Without this, the SDK transport's per-attempt response-header
		// timeout is sized for the short claim window and a multi-minute agent
		// exec fails with "timeout awaiting response headers". Mirrors the
		// agent-sandbox-specific path in runAgentInSandbox.
		MaxRequestTimeout: workspaceEnv.CommandTimeout,
	})
	if err != nil {
		submitExecutionWorkspaceStatus(
			workspaceEnv,
			corev1alpha1.ExecutionWorkspacePhaseFailed,
			corev1alpha1.ExecutionWorkspaceReasonClaimFailed,
			false,
			"workspace claim failed",
		)
		return fmt.Errorf("claim execution workspace: %w", err)
	}
	ref := claim.Ref
	cleaned := false
	substrateHandoffBootstrapped := false
	defer func() {
		if cleaned {
			return
		}
		cleanupCtx, cleanupCancel := agentSandboxCleanupContext(workspaceEnv.ClaimTimeout)
		defer cleanupCancel()
		cleanupEnv, cleanupOptions := preTerminalExecutionWorkspaceCleanup(
			workspaceEnv,
			substrateHandoffBootstrapped,
			claim.Created && !claim.Reused,
		)
		if err := cleanupExecutionWorkspaceWithOptions(
			cleanupCtx,
			executor,
			ref,
			cleanupEnv,
			claim.Reused,
			executionWorkspaceDeferredCleanupSubmitsStatus(cleanupEnv),
			cleanupOptions,
		); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to clean up execution workspace: %v\n", err)
		}
	}()
	submitExecutionWorkspaceStatus(
		workspaceEnv,
		corev1alpha1.ExecutionWorkspacePhasePending,
		corev1alpha1.ExecutionWorkspaceReasonClaimed,
		claim.Reused,
		"workspace claimed",
	)

	ready, err := executor.WaitReady(ctx, workspace.WaitReadyRequest{
		Ref:                ref,
		Timeout:            workspaceEnv.ClaimTimeout,
		Boot:               workspaceEnv.Boot,
		SnapshotRestoreURI: workspaceEnv.SnapshotRestoreURI,
	})
	if err != nil {
		submitExecutionWorkspaceStatus(
			workspaceEnv,
			corev1alpha1.ExecutionWorkspacePhaseFailed,
			corev1alpha1.ExecutionWorkspaceReasonReadinessFailed,
			claim.Reused,
			"workspace readiness failed",
		)
		return fmt.Errorf("wait for execution workspace: %w", err)
	}
	readyStatusOptions := []executionWorkspaceStatusOption{withExecutionWorkspaceReadyResult(ready)}
	submitExecutionWorkspaceStatus(
		workspaceEnv,
		corev1alpha1.ExecutionWorkspacePhaseReady,
		corev1alpha1.ExecutionWorkspaceReasonReady,
		claim.Reused,
		"workspace ready",
		readyStatusOptions...,
	)

	if handoffToken != "" {
		if err := bootstrapWorkspaceHandoffToken(ctx, executor, ref, handoffToken, workspaceEnv); err != nil {
			submitExecutionWorkspaceStatus(
				workspaceEnv,
				corev1alpha1.ExecutionWorkspacePhaseFailed,
				corev1alpha1.ExecutionWorkspaceReasonHandoffFailed,
				claim.Reused,
				"workspace handoff failed",
				readyStatusOptions...,
			)
			return err
		}
		substrateHandoffBootstrapped = true
	}

	command, innerEnv, err := stageAgentSandboxExecutable(
		ctx,
		executor,
		ref,
		executable,
		os.Args[1:],
		workspaceInnerEnv(os.Environ(), workspaceEnv),
		workspaceEnv.CommandTimeout,
	)
	if err != nil {
		submitExecutionWorkspaceStatus(
			workspaceEnv,
			corev1alpha1.ExecutionWorkspacePhaseFailed,
			corev1alpha1.ExecutionWorkspaceReasonHandoffFailed,
			claim.Reused,
			"workspace handoff failed",
			readyStatusOptions...,
		)
		return err
	}
	stdoutResultToken := innerEnv[workerenv.ResultStdoutToken]

	execResult, err := executor.Exec(ctx, workspace.ExecRequest{
		Ref:            ref,
		Command:        command,
		Env:            innerEnv,
		WorkDir:        workspaceDir,
		Timeout:        workspaceEnv.CommandTimeout,
		MaxOutputBytes: sandboxExecMaxOutputBytes(),
		Resident:       executionWorkspaceResidentProcess(workspaceEnv),
		ResidentKey:    executionWorkspaceResidentKey(workspaceEnv, ref),
	})
	if err != nil {
		forwardWorkspaceStdoutResultMarkerIfPresent(
			ctx, executor, ref, workspaceEnv.CommandTimeout, execResult, stdoutResultToken,
		)
		submitExecutionWorkspaceStatus(
			workspaceEnv,
			corev1alpha1.ExecutionWorkspacePhaseFailed,
			corev1alpha1.ExecutionWorkspaceReasonCommandFailed,
			claim.Reused,
			"workspace command failed",
			readyStatusOptions...,
		)
		return fmt.Errorf("%s workspace execution failed: %w%s", name, err, formatSandboxExecOutput(execResult))
	}
	if execResult != nil && !execResult.Succeeded() {
		forwardWorkspaceStdoutResultMarkerIfPresent(
			ctx, executor, ref, workspaceEnv.CommandTimeout, execResult, stdoutResultToken,
		)
		submitExecutionWorkspaceStatus(
			workspaceEnv,
			corev1alpha1.ExecutionWorkspacePhaseFailed,
			corev1alpha1.ExecutionWorkspaceReasonCommandFailed,
			claim.Reused,
			"workspace command failed",
			readyStatusOptions...,
		)
		return fmt.Errorf(
			"%s workspace execution failed: command exited with code %d%s",
			name,
			execResult.ExitCode,
			formatSandboxExecOutput(execResult),
		)
	}

	marker, err := workspaceStdoutResultMarker(
		ctx, executor, ref, workspaceEnv.CommandTimeout, execResult, stdoutResultToken,
	)
	if err != nil {
		submitExecutionWorkspaceStatus(
			workspaceEnv,
			corev1alpha1.ExecutionWorkspacePhaseFailed,
			corev1alpha1.ExecutionWorkspaceReasonCommandFailed,
			claim.Reused,
			"workspace command failed",
			readyStatusOptions...,
		)
		return fmt.Errorf("%s workspace execution failed: %w%s", name, err, formatSandboxExecOutput(execResult))
	}

	cleanupCtx, cleanupCancel := agentSandboxCleanupContext(workspaceEnv.ClaimTimeout)
	defer cleanupCancel()
	if err := cleanupExecutionWorkspaceWithOptions(
		cleanupCtx,
		executor,
		ref,
		workspaceEnv,
		claim.Reused,
		true,
		executionWorkspaceCleanupOptions{statusOptions: readyStatusOptions},
	); err != nil {
		reason := corev1alpha1.ExecutionWorkspaceReasonCleanupFailed
		if errors.Is(err, errExecutionWorkspaceSecretScrubFailed) {
			reason = corev1alpha1.ExecutionWorkspaceReasonSecretScrubFailed
		}
		submitExecutionWorkspaceStatus(
			workspaceEnv,
			corev1alpha1.ExecutionWorkspacePhaseFailed,
			reason,
			claim.Reused,
			"workspace cleanup failed",
			readyStatusOptions...,
		)
		return fmt.Errorf("execution workspace cleanup failed: %w", err)
	}
	cleaned = true

	if marker != "" {
		fmt.Println(marker)
	}

	fmt.Println(executionWorkspaceCompletionMessage(taskNamespace, taskName, workspaceEnv, ref))
	return nil
}
