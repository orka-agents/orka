/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/internal/workspace"
)

func TestRunAgentInWorkspaceRetriesTransientDeleteAfterPrimaryFailure(t *testing.T) {
	setExecutionWorkspaceRunnerTestEnv(t)

	recorder := newCleanupFaultWorkspaceExecutor()
	recorder.waitReadyErr = fmt.Errorf("readiness boom")
	recorder.deleteFailures = []error{
		workspace.NewError("delete", workspace.ErrorKindUnknown, "temporary delete failure", true, nil),
	}
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = runAgentInWorkspace(
			context.Background(),
			"test-agent",
			"/workspace",
			executionWorkspaceRunnerTestEnv(
				string(corev1alpha1.WorkspaceProviderAgentSandbox),
				string(corev1alpha1.WorkspaceCleanupPolicyDelete),
			),
		)
	})
	if runErr == nil {
		t.Fatal("expected readiness failure")
	}
	if !strings.Contains(runErr.Error(), "wait for execution workspace") ||
		!strings.Contains(runErr.Error(), "readiness boom") {
		t.Fatalf("runAgentInWorkspace() error = %q, want readiness failure", runErr.Error())
	}
	if strings.Contains(runErr.Error(), "temporary delete failure") {
		t.Fatalf("runAgentInWorkspace() error = %q, want recovered cleanup failure omitted", runErr.Error())
	}

	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "delete", "delete")
	deleteCtxErrs := recorder.deleteContextErrors()
	if len(deleteCtxErrs) != 2 {
		t.Fatalf("recorded %d delete context errors, want 2", len(deleteCtxErrs))
	}
	for i, ctxErr := range deleteCtxErrs {
		if ctxErr != nil {
			t.Fatalf("delete attempt %d context error = %v, want nil", i+1, ctxErr)
		}
	}
	if !strings.Contains(stderr, "retrying") || !strings.Contains(stderr, "temporary delete failure") {
		t.Fatalf("stderr = %q, want transient cleanup retry warning", stderr)
	}
}

func TestRunAgentInWorkspaceRetriesTransientReleaseWithRetainedScrub(t *testing.T) {
	setExecutionWorkspaceRunnerTestEnv(t)

	recorder := newCleanupFaultWorkspaceExecutor()
	recorder.waitReadyErr = fmt.Errorf("readiness boom")
	recorder.releaseFailures = []error{
		workspace.NewError("release", workspace.ErrorKindUnknown, "temporary release failure", true, nil),
	}
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = runAgentInWorkspace(
			context.Background(),
			"test-agent",
			"/workspace",
			executionWorkspaceRunnerTestEnv(
				string(corev1alpha1.WorkspaceProviderAgentSandbox),
				string(corev1alpha1.WorkspaceCleanupPolicyRetain),
			),
		)
	})
	if runErr == nil {
		t.Fatal("expected readiness failure")
	}
	if strings.Contains(runErr.Error(), "temporary release failure") {
		t.Fatalf("runAgentInWorkspace() error = %q, want recovered cleanup failure omitted", runErr.Error())
	}

	assertOperationOrder(
		t,
		recorder.operations(),
		"claim", "waitReady", "exec", "release", "describe", "describe", "release",
	)
	execReqs := recorder.execRequests()
	if len(execReqs) != 1 {
		t.Fatalf("recorded %d scrub requests, want one before release retries", len(execReqs))
	}
	if req := execReqs[0]; len(req.Command) < 3 || req.Command[0] != "rm" || req.Command[1] != "-f" {
		t.Fatalf("scrub command = %#v, want rm -f paths", req.Command)
	}
	releaseReqs := recorder.releaseRequests()
	if len(releaseReqs) != 2 {
		t.Fatalf("recorded %d release requests, want 2", len(releaseReqs))
	}
	for i, req := range releaseReqs {
		if !req.Retain {
			t.Fatalf("release attempt %d Retain = false, want true", i+1)
		}
		if req.SkipScrub {
			t.Fatalf("release attempt %d SkipScrub = true, want runner pre-scrub", i+1)
		}
	}
	for i, ctxErr := range recorder.releaseContextErrors() {
		if ctxErr != nil {
			t.Fatalf("release attempt %d context error = %v, want nil", i+1, ctxErr)
		}
	}
	if !strings.Contains(stderr, "retrying") || !strings.Contains(stderr, "temporary release failure") {
		t.Fatalf("stderr = %q, want transient cleanup retry warning", stderr)
	}
}

func TestRunAgentInWorkspaceReconcilesDelayedAmbiguousRetainRelease(t *testing.T) {
	setExecutionWorkspaceRunnerTestEnv(t)

	recorder := newCleanupFaultWorkspaceExecutor()
	recorder.waitReadyErr = fmt.Errorf("readiness boom")
	recorder.releaseFailuresAfterSuccess = []error{
		workspace.NewError("release", workspace.ErrorKindUnknown, "lost release response", true, nil),
	}
	recorder.describePhases = []workspace.Phase{workspace.PhaseReady, workspace.PhaseRetained}
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = runAgentInWorkspace(
			context.Background(),
			"test-agent",
			"/workspace",
			executionWorkspaceRunnerTestEnv(
				string(corev1alpha1.WorkspaceProviderAgentSandbox),
				string(corev1alpha1.WorkspaceCleanupPolicyRetain),
			),
		)
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "readiness boom") {
		t.Fatalf("runAgentInWorkspace() error = %v, want primary readiness failure", runErr)
	}
	if strings.Contains(runErr.Error(), "lost release response") {
		t.Fatalf("runAgentInWorkspace() error = %q, want reconciled cleanup failure omitted", runErr.Error())
	}

	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "exec", "release", "describe", "describe")
	if got := len(recorder.execRequests()); got != 1 {
		t.Fatalf("scrub requests = %d, want 1", got)
	}
	if got := len(recorder.releaseRequests()); got != 1 {
		t.Fatalf("release requests = %d, want no duplicate after retained state observed", got)
	}
	if !strings.Contains(stderr, "desired cleanup state was observed") {
		t.Fatalf("stderr = %q, want reconciliation warning", stderr)
	}
}

func TestRunAgentInWorkspaceDoesNotReconcilePermanentRetainScrubFailure(t *testing.T) {
	setExecutionWorkspaceRunnerTestEnv(t)

	primaryErr := errors.New("readiness boom")
	scrubCause := errors.New("scrub forbidden")
	recorder := newCleanupFaultWorkspaceExecutor()
	recorder.waitReadyErr = primaryErr
	recorder.execErr = workspace.NewError(
		"exec",
		workspace.ErrorKindFailedPrecondition,
		"permanent scrub failure",
		false,
		scrubCause,
	)
	recorder.describePhases = []workspace.Phase{workspace.PhaseRetained}
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	var runErr error
	captureStderr(t, func() {
		runErr = runAgentInWorkspace(
			context.Background(),
			"test-agent",
			"/workspace",
			executionWorkspaceRunnerTestEnv(
				string(corev1alpha1.WorkspaceProviderAgentSandbox),
				string(corev1alpha1.WorkspaceCleanupPolicyRetain),
			),
		)
	})
	if runErr == nil {
		t.Fatal("expected readiness and scrub failure")
	}
	if !errors.Is(runErr, primaryErr) || !errors.Is(runErr, scrubCause) {
		t.Fatalf("runAgentInWorkspace() error = %v, want primary and scrub failures", runErr)
	}
	if !errors.Is(runErr, errExecutionWorkspaceSecretScrubFailed) {
		t.Fatalf("runAgentInWorkspace() error = %v, want secret scrub sentinel", runErr)
	}

	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "exec")
}

func TestRunAgentInWorkspaceJoinsPermanentCleanupFailureWithPrimaryFailure(t *testing.T) {
	setExecutionWorkspaceRunnerTestEnv(t)

	primaryErr := errors.New("readiness boom")
	cleanupCause := errors.New("delete forbidden")
	recorder := newCleanupFaultWorkspaceExecutor()
	recorder.waitReadyErr = primaryErr
	recorder.deleteFailures = []error{
		workspace.NewError("delete", workspace.ErrorKindFailedPrecondition, "permanent delete failure", false, cleanupCause),
	}
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = runAgentInWorkspace(
			context.Background(),
			"test-agent",
			"/workspace",
			executionWorkspaceRunnerTestEnv(
				string(corev1alpha1.WorkspaceProviderAgentSandbox),
				string(corev1alpha1.WorkspaceCleanupPolicyDelete),
			),
		)
	})
	if runErr == nil {
		t.Fatal("expected readiness and cleanup failure")
	}
	if !errors.Is(runErr, primaryErr) {
		t.Fatalf("runAgentInWorkspace() error = %v, want primary failure preserved", runErr)
	}
	if !errors.Is(runErr, cleanupCause) {
		t.Fatalf("runAgentInWorkspace() error = %v, want cleanup failure joined", runErr)
	}
	if !strings.Contains(runErr.Error(), "execution workspace cleanup failed") ||
		!strings.Contains(runErr.Error(), "permanent delete failure") {
		t.Fatalf("runAgentInWorkspace() error = %q, want cleanup failure context", runErr.Error())
	}

	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "delete")
	if strings.Contains(stderr, "retrying") {
		t.Fatalf("stderr = %q, want permanent cleanup failure not retried", stderr)
	}
	if !strings.Contains(stderr, "failed to clean up execution workspace") {
		t.Fatalf("stderr = %q, want permanent cleanup warning", stderr)
	}
}

func TestRunAgentInWorkspaceCleanupIgnoresCanceledPrimaryContext(t *testing.T) {
	setExecutionWorkspaceRunnerTestEnv(t)

	ctx, cancel := context.WithCancel(context.Background())
	recorder := newCleanupFaultWorkspaceExecutor()
	recorder.waitReadyErr = context.Canceled
	recorder.afterWaitReady = cancel
	recorder.deleteFailures = []error{
		workspace.NewError("delete", workspace.ErrorKindUnknown, "temporary delete failure", true, nil),
	}
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = runAgentInWorkspace(
			ctx,
			"test-agent",
			"/workspace",
			executionWorkspaceRunnerTestEnv(
				string(corev1alpha1.WorkspaceProviderAgentSandbox),
				string(corev1alpha1.WorkspaceCleanupPolicyDelete),
			),
		)
	})
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("runAgentInWorkspace() error = %v, want canceled primary failure", runErr)
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("primary context error = %v, want context canceled", ctx.Err())
	}
	if strings.Contains(runErr.Error(), "temporary delete failure") {
		t.Fatalf("runAgentInWorkspace() error = %q, want recovered cleanup failure omitted", runErr.Error())
	}

	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "delete", "delete")
	deleteCtxErrs := recorder.deleteContextErrors()
	if len(deleteCtxErrs) != 2 {
		t.Fatalf("recorded %d delete context errors, want 2", len(deleteCtxErrs))
	}
	for i, ctxErr := range deleteCtxErrs {
		if ctxErr != nil {
			t.Fatalf("delete attempt %d context error = %v, want fresh cleanup context", i+1, ctxErr)
		}
	}
	if !strings.Contains(stderr, "retrying") {
		t.Fatalf("stderr = %q, want retry warning after canceled primary context", stderr)
	}
}

func TestRunAgentInWorkspaceCleanupRetriesPreserveSubstratePoolContracts(t *testing.T) {
	tests := []struct {
		name         string
		poolName     string
		seedReused   bool
		wantAction   executionWorkspaceCleanupAction
		wantAttempts int
	}{
		{
			name:         "non-pooled new retained workspace deletes before handoff",
			wantAction:   executionWorkspaceCleanupActionDelete,
			wantAttempts: 2,
		},
		{
			name:         "pooled new retained workspace deletes before handoff",
			poolName:     "codex-pool",
			wantAction:   executionWorkspaceCleanupActionDelete,
			wantAttempts: 2,
		},
		{
			name:         "non-pooled reused retained workspace releases before handoff",
			seedReused:   true,
			wantAction:   executionWorkspaceCleanupActionRetain,
			wantAttempts: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setExecutionWorkspaceRunnerTestEnv(t)
			t.Setenv(workspaceHandoffTokenEnv, "handoff-token")

			env := executionWorkspaceRunnerTestEnv(
				string(corev1alpha1.WorkspaceProviderSubstrate),
				string(corev1alpha1.WorkspaceCleanupPolicyRetain),
			)
			env.TemplateNamespace = "ate-demo"
			env.ClaimNamespace = "ate-demo"
			env.ClaimName = "actor-1"
			env.PoolName = tt.poolName

			recorder := newCleanupFaultWorkspaceExecutor()
			if tt.seedReused {
				seed, err := recorder.fake.Claim(context.Background(), workspace.ClaimRequest{
					Namespace:       env.ClaimNamespace,
					ClaimName:       env.ClaimName,
					CreateIfMissing: true,
					Template: workspace.TemplateRef{
						Namespace: env.TemplateNamespace,
						Name:      env.TemplateName,
					},
					Timeout: env.ClaimTimeout,
				})
				if err != nil {
					t.Fatalf("seed reused workspace: %v", err)
				}
				if _, err := recorder.fake.Release(context.Background(), workspace.ReleaseRequest{
					Ref:     seed.Ref,
					Retain:  true,
					Reason:  "seed retained workspace",
					Timeout: env.ClaimTimeout,
				}); err != nil {
					t.Fatalf("retain seeded workspace: %v", err)
				}
			}
			recorder.waitReadyErr = fmt.Errorf("readiness boom")
			transientErr := workspace.NewError(
				string(tt.wantAction),
				workspace.ErrorKindUnknown,
				"temporary cleanup failure",
				true,
				nil,
			)
			if tt.wantAction == executionWorkspaceCleanupActionDelete {
				recorder.deleteFailures = []error{transientErr}
			} else {
				recorder.releaseFailures = []error{transientErr}
			}
			restoreExecutor := setSubstrateWorkspaceExecutorForTest(recorder)
			t.Cleanup(restoreExecutor)

			var runErr error
			stderr := captureStderr(t, func() {
				runErr = runAgentInWorkspace(context.Background(), "test-agent", "/workspace", env)
			})
			if runErr == nil || !strings.Contains(runErr.Error(), "readiness boom") {
				t.Fatalf("runAgentInWorkspace() error = %v, want readiness failure", runErr)
			}
			if strings.Contains(runErr.Error(), "temporary cleanup failure") {
				t.Fatalf("runAgentInWorkspace() error = %q, want recovered cleanup failure omitted", runErr.Error())
			}
			if !strings.Contains(stderr, "retrying") {
				t.Fatalf("stderr = %q, want transient cleanup retry warning", stderr)
			}

			switch tt.wantAction {
			case executionWorkspaceCleanupActionDelete:
				assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "delete", "delete")
				if got := len(recorder.deleteRequests()); got != tt.wantAttempts {
					t.Fatalf("delete requests = %d, want %d", got, tt.wantAttempts)
				}
				for i, req := range recorder.deleteRequests() {
					if !req.SkipScrub {
						t.Fatalf("delete attempt %d SkipScrub = false, want true before handoff", i+1)
					}
				}
				if got := len(recorder.releaseRequests()); got != 0 {
					t.Fatalf("release requests = %d, want 0", got)
				}
			case executionWorkspaceCleanupActionRetain:
				assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "release", "describe", "describe", "release")
				if got := len(recorder.releaseRequests()); got != tt.wantAttempts {
					t.Fatalf("release requests = %d, want %d", got, tt.wantAttempts)
				}
				for i, req := range recorder.releaseRequests() {
					if !req.Retain {
						t.Fatalf("release attempt %d Retain = false, want true", i+1)
					}
					if !req.SkipScrub {
						t.Fatalf("release attempt %d SkipScrub = false, want true before handoff", i+1)
					}
				}
				if got := len(recorder.deleteRequests()); got != 0 {
					t.Fatalf("delete requests = %d, want 0", got)
				}
			}
		})
	}
}

func TestRunAgentInWorkspaceBoundsTransientCleanupRetries(t *testing.T) {
	setExecutionWorkspaceRunnerTestEnv(t)

	recorder := newCleanupFaultWorkspaceExecutor()
	recorder.waitReadyErr = fmt.Errorf("readiness boom")
	for range executionWorkspacePreterminalCleanupMaxAttempts + 1 {
		recorder.deleteFailures = append(
			recorder.deleteFailures,
			workspace.NewError("delete", workspace.ErrorKindUnknown, "temporary delete failure", true, nil),
		)
	}
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = runAgentInWorkspace(
			context.Background(),
			"test-agent",
			"/workspace",
			executionWorkspaceRunnerTestEnv(
				string(corev1alpha1.WorkspaceProviderAgentSandbox),
				string(corev1alpha1.WorkspaceCleanupPolicyDelete),
			),
		)
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "execution workspace cleanup failed") {
		t.Fatalf("runAgentInWorkspace() error = %v, want exhausted cleanup failure", runErr)
	}
	if got := len(recorder.deleteRequests()); got != executionWorkspacePreterminalCleanupMaxAttempts {
		t.Fatalf("delete attempts = %d, want bounded maximum %d", got, executionWorkspacePreterminalCleanupMaxAttempts)
	}
	if got := strings.Count(stderr, "retrying"); got != executionWorkspacePreterminalCleanupMaxAttempts-1 {
		t.Fatalf("retry warnings = %d, want %d; stderr = %q", got, executionWorkspacePreterminalCleanupMaxAttempts-1, stderr)
	}
}

func TestRunAgentInWorkspaceCleanupBackoffStopsAtFreshContextDeadline(t *testing.T) {
	setExecutionWorkspaceRunnerTestEnv(t)

	recorder := newCleanupFaultWorkspaceExecutor()
	recorder.waitReadyErr = fmt.Errorf("readiness boom")
	recorder.deleteFailures = []error{
		workspace.NewError("delete", workspace.ErrorKindUnknown, "temporary delete failure", true, nil),
	}
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	env := executionWorkspaceRunnerTestEnv(
		string(corev1alpha1.WorkspaceProviderAgentSandbox),
		string(corev1alpha1.WorkspaceCleanupPolicyDelete),
	)
	env.ClaimTimeout = 25 * time.Millisecond

	var runErr error
	captureStderr(t, func() {
		runErr = runAgentInWorkspace(context.Background(), "test-agent", "/workspace", env)
	})
	if runErr == nil {
		t.Fatal("expected readiness and cleanup deadline failure")
	}
	if !errors.Is(runErr, context.DeadlineExceeded) {
		t.Fatalf("runAgentInWorkspace() error = %v, want cleanup deadline exceeded", runErr)
	}
	if got := len(recorder.deleteRequests()); got != 1 {
		t.Fatalf("delete attempts = %d, want retry backoff interrupted before attempt 2", got)
	}
}

type cleanupFaultWorkspaceExecutor struct {
	*recordingWorkspaceExecutor

	deleteFailures              []error
	releaseFailures             []error
	releaseFailuresAfterSuccess []error
	releaseCtxErrs              []error
	describePhases              []workspace.Phase
	afterWaitReady              func()
}

func newCleanupFaultWorkspaceExecutor() *cleanupFaultWorkspaceExecutor {
	return &cleanupFaultWorkspaceExecutor{recordingWorkspaceExecutor: newRecordingWorkspaceExecutor()}
}

func (r *cleanupFaultWorkspaceExecutor) WaitReady(
	ctx context.Context,
	req workspace.WaitReadyRequest,
) (*workspace.ReadyResult, error) {
	result, err := r.recordingWorkspaceExecutor.WaitReady(ctx, req)
	if r.afterWaitReady != nil {
		r.afterWaitReady()
	}
	return result, err
}

func (r *cleanupFaultWorkspaceExecutor) Release(
	ctx context.Context,
	req workspace.ReleaseRequest,
) (*workspace.ReleaseResult, error) {
	r.mu.Lock()
	r.ops = append(r.ops, "release")
	r.releaseReqs = append(r.releaseReqs, req)
	r.releaseCtxErrs = append(r.releaseCtxErrs, ctx.Err())
	err := popCleanupFailure(&r.releaseFailures)
	afterSuccessErr := popCleanupFailure(&r.releaseFailuresAfterSuccess)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	result, err := r.fake.Release(ctx, req)
	if err != nil {
		return nil, err
	}
	if afterSuccessErr != nil {
		return nil, afterSuccessErr
	}
	return result, nil
}

func (r *cleanupFaultWorkspaceExecutor) Describe(
	ctx context.Context,
	req workspace.DescribeRequest,
) (*workspace.Description, error) {
	r.mu.Lock()
	r.ops = append(r.ops, "describe")
	r.describeReqs = append(r.describeReqs, req)
	err := r.describeErr
	var phase workspace.Phase
	if len(r.describePhases) > 0 {
		phase = r.describePhases[0]
		r.describePhases = r.describePhases[1:]
	}
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if phase != "" {
		return &workspace.Description{Ref: req.Ref, Phase: phase, Retained: phase == workspace.PhaseRetained}, nil
	}
	return r.fake.Describe(ctx, req)
}

func (r *cleanupFaultWorkspaceExecutor) Delete(
	ctx context.Context,
	req workspace.DeleteRequest,
) (*workspace.DeleteResult, error) {
	r.mu.Lock()
	r.ops = append(r.ops, "delete")
	r.deleteReqs = append(r.deleteReqs, req)
	r.deleteCtxErrs = append(r.deleteCtxErrs, ctx.Err())
	err := popCleanupFailure(&r.deleteFailures)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return r.fake.Delete(ctx, req)
}

func (r *cleanupFaultWorkspaceExecutor) releaseContextErrors() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]error(nil), r.releaseCtxErrs...)
}

func popCleanupFailure(failures *[]error) error {
	if len(*failures) == 0 {
		return nil
	}
	err := (*failures)[0]
	*failures = (*failures)[1:]
	return err
}

func setExecutionWorkspaceRunnerTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv(workerenv.TaskName, "task-name")
	t.Setenv(workerenv.TaskNamespace, testTaskNamespace)
}

func executionWorkspaceRunnerTestEnv(provider, cleanupPolicy string) workerenv.ExecutionWorkspaceEnv {
	return workerenv.ExecutionWorkspaceEnv{
		Provider:          provider,
		TemplateName:      "workspace-template",
		TemplateNamespace: testAgentSandboxTemplateNamespace,
		ClaimNamespace:    testAgentSandboxTemplateNamespace,
		ClaimName:         "workspace-claim",
		ClaimTimeout:      2 * time.Second,
		CommandTimeout:    5 * time.Second,
		CleanupPolicy:     cleanupPolicy,
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stderr pipe: %v", err)
	}

	done := make(chan []byte)
	go func() {
		out, _ := io.ReadAll(reader)
		done <- out
	}()

	os.Stderr = writer
	defer func() {
		os.Stderr = original
	}()

	fn()

	if err := writer.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}
	out := <-done
	if err := reader.Close(); err != nil {
		t.Fatalf("close stderr reader: %v", err)
	}
	return string(out)
}
