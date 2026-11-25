package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/publisher"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/store"
)

const (
	repositoryMonitorUpdateBranchStrategyMerge  = "merge"
	repositoryMonitorUpdateBranchStrategyRebase = "rebase"
	publisherBranchRebaseOperation              = "publisher.branch-rebase"
	repositoryMonitorRebaseAggregatePrefix      = "rm-rebase-"
)

// RepositoryMonitorBranchRebaser executes a clean-room non-interactive rebase
// of a pull request branch onto its base commit.
type RepositoryMonitorBranchRebaser interface {
	RebaseBranch(ctx context.Context, request publisherservice.BranchRebaseRequest) (publisherservice.BranchRebaseResponse, error)
}

func repositoryMonitorUpdateBranchUsesRebase(monitor *corev1alpha1.RepositoryMonitor) bool {
	return monitor != nil && strings.EqualFold(strings.TrimSpace(monitor.Spec.Repair.UpdateBranchStrategy), repositoryMonitorUpdateBranchStrategyRebase)
}

func (r *RepositoryMonitorReconciler) submitRepositoryMonitorUpdateBranchRebase(
	ctx context.Context,
	monitor *corev1alpha1.RepositoryMonitor,
	run *store.MonitorRun,
	command *store.CommandEvent,
	owner, repository string,
	pr repositoryMonitorPullRequest,
	item *store.MonitorItem,
	mutation *store.GitHubMutationRecord,
) (bool, int, error) {
	if r.BranchRebaser == nil {
		return true, 0, r.failRepositoryMonitorUpdateBranch(ctx, monitor, run, command, item, mutation, "updateBranchStrategy=rebase requires a configured clean-room workspace publisher")
	}
	credentialRefs, credErr := repositoryMonitorCredentialRefsForWrite(monitor)
	if credErr != nil {
		return true, 0, r.failRepositoryMonitorUpdateBranch(ctx, monitor, run, command, item, mutation, credErr.Error())
	}
	targetRepoSlug, reason := repositoryMonitorRepairRepository(monitor, owner+"/"+repository, pr)
	if reason != "" {
		return true, 0, r.failRepositoryMonitorUpdateBranch(ctx, monitor, run, command, item, mutation, reason)
	}
	if !strings.EqualFold(targetRepoSlug, owner+"/"+repository) {
		return true, 0, r.failRepositoryMonitorUpdateBranch(ctx, monitor, run, command, item, mutation, "updateBranchStrategy=rebase does not support cross-repository fork PRs")
	}
	targetOwner, targetName, ok := strings.Cut(targetRepoSlug, "/")
	if !ok || targetOwner == "" || targetName == "" {
		return true, 0, r.failRepositoryMonitorUpdateBranch(ctx, monitor, run, command, item, mutation, "invalid target repository for rebase")
	}
	authorName, authorEmail := repositoryMonitorCommitAuthor(monitor)

	// Derive commitTimestamp deterministically from the persisted mutation/command
	// so recovery replays produce the exact same immutable ExternalEffect digest.
	commitTimestamp := mutation.CreatedAt.UTC().Truncate(time.Second)
	if commitTimestamp.IsZero() && mutation.PendingAt != nil {
		commitTimestamp = mutation.PendingAt.UTC().Truncate(time.Second)
	}
	if commitTimestamp.IsZero() {
		commitTimestamp = command.CreatedAt.UTC().Truncate(time.Second)
	}
	if commitTimestamp.IsZero() {
		commitTimestamp = time.Now().UTC().Truncate(time.Second)
	}

	aggregateID := repositoryMonitorRebaseAggregatePrefix + monitor.Name
	metadata := publisherservice.OperationMetadata{
		Namespace:     monitor.Namespace,
		PublicationID: aggregateID,
		OperationID:   mutation.ID,
	}
	var signingCredRef *publisherservice.CredentialReference
	if signingName := localObjectReferenceName(credentialRefs.commitSigning); signingName != "" {
		signingCredRef = &publisherservice.CredentialReference{
			Name: signingName,
			Kind: publisherservice.CredentialSSHSigningKey,
			Role: publisherservice.CredentialRoleCommitSign,
		}
	}
	// Reuse the captured base SHA stored on the mutation so retries submit the
	// exact same immutable ExternalEffect digest even if base advances.
	baseOID := strings.TrimSpace(mutation.ExternalID)
	if baseOID == "" {
		baseOID = strings.TrimSpace(pr.BaseSHA)
	}
	serviceReq := publisherservice.BranchRebaseRequest{
		Metadata: metadata,
		CredentialRef: &publisherservice.CredentialReference{
			Name: localObjectReferenceName(credentialRefs.publication),
			Kind: publisherservice.CredentialHTTPExtraHeader,
			Role: publisherservice.CredentialRoleTargetWrite,
		},
		SigningCredentialRef: signingCredRef,
		Request: publisher.RebaseBranchRequest{
			OperationID:       mutation.ID,
			Target:            publisher.Repository{Provider: workspaceRepositoryProviderGitHub, ID: "github.com/" + strings.ToLower(targetRepoSlug), URL: repositoryMonitorHTTPSCloneURL(targetOwner, targetName)},
			TargetRef:         "refs/heads/" + strings.TrimPrefix(strings.TrimSpace(pr.HeadBranch), "refs/heads/"),
			BaseRef:           "refs/heads/" + strings.TrimPrefix(strings.TrimSpace(pr.BaseBranch), "refs/heads/"),
			BaseOID:           baseOID,
			HeadOID:           strings.TrimSpace(command.HeadSHA),
			CommitAuthorName:  authorName,
			CommitAuthorEmail: authorEmail,
			CommitTimestamp:   commitTimestamp,
			SignOff:           monitor.Spec.Commit.SignOff,
		},
	}

	res, err := r.executeRepositoryMonitorBranchRebase(ctx, metadata, serviceReq)
	if err != nil {
		if repositoryMonitorRebaseErrorPermanent(err) {
			return true, 0, r.failRepositoryMonitorUpdateBranch(ctx, monitor, run, command, item, mutation, fmt.Sprintf("clean-room rebase aborted: %v", err))
		}
		// Retryable publisher error (e.g. HTTP 429 busy or transport error):
		// reset to started only when the publisher explicitly rejected execution
		// without a transport error; otherwise keep submitting for reconciliation.
		return r.recordRepositoryMonitorRebaseRetryableError(ctx, monitor, run, command, item, mutation, pr, err)
	}

	updatedPR := pr
	if strings.TrimSpace(res.Result.NewHeadOID) != "" {
		updatedPR.HeadSHA = res.Result.NewHeadOID
	}
	return true, 0, r.completeRepositoryMonitorUpdateBranch(ctx, monitor, run, command, item, mutation, updatedPR)
}

func (r *RepositoryMonitorReconciler) executeRepositoryMonitorBranchRebase(
	ctx context.Context,
	metadata publisherservice.OperationMetadata,
	req publisherservice.BranchRebaseRequest,
) (publisherservice.BranchRebaseResponse, error) {
	if r.DurableControlStore == nil || r.ControllerEpochManager == nil {
		return r.BranchRebaser.RebaseBranch(ctx, req)
	}
	fence, err := r.ControllerEpochManager.CurrentFence(ctx)
	if err != nil {
		return publisherservice.BranchRebaseResponse{}, err
	}
	identity := store.ExternalEffectIdentity{
		Kind:        publisherBranchRebaseOperation,
		Namespace:   metadata.Namespace,
		AggregateID: metadata.PublicationID,
		OperationID: metadata.OperationID,
	}
	return runExternalEffect(ctx, r.DurableControlStore, fence, identity, req, func(callCtx context.Context) (publisherservice.BranchRebaseResponse, error) {
		return r.BranchRebaser.RebaseBranch(callCtx, req)
	})
}

func (r *RepositoryMonitorReconciler) recordRepositoryMonitorRebaseRetryableError(
	ctx context.Context,
	monitor *corev1alpha1.RepositoryMonitor,
	run *store.MonitorRun,
	command *store.CommandEvent,
	item *store.MonitorItem,
	mutation *store.GitHubMutationRecord,
	pr repositoryMonitorPullRequest,
	rebaseErr error,
) (bool, int, error) {
	mutation.Status = repositoryMonitorUpdateBranchSubmitting
	var clientErr *publisherservice.ClientError
	if errors.As(rebaseErr, &clientErr) && clientErr.Response.Retryable && publisherservice.TransportCause(rebaseErr) == nil {
		// Publisher explicitly rejected the request (e.g. HTTP 429 busy) without
		// executing the push; safe to reset to started and retry the same operation.
		mutation.Status = repositoryMonitorAutomergeStateStarted
		mutation.PendingAt = nil
	}
	mutation.GitHubRequestID = ""
	mutation.Error = rebaseErr.Error()
	if updateErr := r.updateRepositoryMonitorGitHubMutation(ctx, monitor, mutation); updateErr != nil {
		return true, 0, fmt.Errorf("rebase retry failed: %w; additionally failed to update mutation audit: %v", rebaseErr, updateErr)
	}
	item.RepairState = repositoryMonitorRepairPhaseQueued
	item.SkipReason = repositoryMonitorRunRetryScheduled
	if updateErr := r.Store.UpsertMonitorItem(ctx, item); updateErr != nil {
		return true, 0, updateErr
	}
	if actionErr := r.recordRepositoryMonitorWorkActionState(ctx, monitor, run, command, repositoryMonitorPullRequestKind, pr.Number, command.HeadSHA, "", command.Intent, repositoryMonitorWorkActionStatusRunning, repositoryMonitorRunRetryScheduled, "", rebaseErr.Error()); actionErr != nil {
		return true, 0, actionErr
	}
	return true, 0, rebaseErr
}

func repositoryMonitorRebaseErrorPermanent(err error) bool {
	if errors.Is(err, publisher.ErrRebaseRefused) || errors.Is(err, publisher.ErrCASRejected) || errors.Is(err, publisher.ErrBranchMoved) || errors.Is(err, publisher.ErrSourceMoved) {
		return true
	}
	var clientErr *publisherservice.ClientError
	if errors.As(err, &clientErr) && !clientErr.Response.Retryable {
		return true
	}
	return false
}
