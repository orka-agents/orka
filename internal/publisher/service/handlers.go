package service

import (
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/orka-agents/orka/internal/artifactcap"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/publisher"
)

func (s *Server) handleWorkspaceResolve(writer http.ResponseWriter, request *http.Request) {
	s.serveOperation(writer, request, OperationWorkspaceResolve,
		func(body []byte) (OperationMetadata, any, error) {
			var value WorkspaceResolveRequest
			if err := decodeStrict(body, &value); err != nil {
				return value.Metadata, nil, invalidRequest("workspace resolve JSON is invalid", err)
			}
			if err := value.Metadata.validateFor(OperationWorkspaceResolve); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateRepository(value.Source, s.config.AllowFileRepositories, s.config.AllowedSCMHosts); err != nil {
				return value.Metadata, nil, err
			}
			if value.SourceRef != "" {
				canonicalRef, err := CanonicalWorkspaceSourceRef(value.SourceRef)
				if err != nil {
					return value.Metadata, nil, err
				}
				value.SourceRef = canonicalRef
			}
			if err := validateCredentialReference(value.CredentialRef, CredentialHTTPExtraHeader); err != nil {
				return value.Metadata, nil, err
			}
			return value.Metadata, value, nil
		},
		func(ctx context.Context, metadata OperationMetadata, digest string, _ journalRecord, decoded any) (int, any, error) {
			value := decoded.(WorkspaceResolveRequest)
			credential, err := s.credentials.gitCredential(ctx, OperationWorkspaceResolve, metadata, value.Source, value.CredentialRef)
			if err != nil {
				return 0, nil, err
			}
			defer credential.cleanup()
			runner, err := newGitRunner(
				credential.gitBinary, s.config.TempRoot, s.config.MaxCommandOutput, s.config.ProxyEnvironment,
			)
			if err != nil {
				return 0, nil, apiError(ErrSCMTransport, "git_unavailable", "Git runtime could not be initialized", 503, false, err)
			}
			resolvedRef, oid, err := runner.resolveSource(ctx, value.Source.URL, value.SourceRef)
			if err != nil {
				return 0, nil, err
			}
			return http.StatusOK, WorkspaceResolveResponse{OperationID: metadata.OperationID, RequestDigest: digest, RepositoryID: value.Source.ID, SourceRef: resolvedRef, BaselineOID: oid}, nil
		})
}

func (s *Server) handleWorkspacePrepare(writer http.ResponseWriter, request *http.Request) {
	s.serveOperation(writer, request, OperationWorkspacePrepare,
		func(body []byte) (OperationMetadata, any, error) {
			var value WorkspacePrepareRequest
			if err := decodeStrict(body, &value); err != nil {
				return value.Metadata, nil, invalidRequest("workspace request JSON is invalid", err)
			}
			if err := value.Metadata.validateFor(OperationWorkspacePrepare); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateRepository(value.Source, s.config.AllowFileRepositories, s.config.AllowedSCMHosts); err != nil {
				return value.Metadata, nil, err
			}
			canonicalRef, err := CanonicalWorkspaceSourceRef(value.SourceRef)
			if err != nil {
				return value.Metadata, nil, err
			}
			value.SourceRef = canonicalRef
			if err := validateObjectID(value.BaselineOID); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateSourceRefBaseline(value.SourceRef, value.BaselineOID); err != nil {
				return value.Metadata, nil, err
			}
			if _, err := mergeWorkspaceLimits(value.Limits, s.config.WorkspaceLimits); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateCredentialReference(value.CredentialRef, CredentialHTTPExtraHeader); err != nil {
				return value.Metadata, nil, err
			}
			return value.Metadata, value, nil
		},
		func(ctx context.Context, metadata OperationMetadata, digest string, record journalRecord, decoded any) (int, any, error) {
			value := decoded.(WorkspacePrepareRequest)
			response := record.Workspace
			if response == nil {
				limits, err := mergeWorkspaceLimits(value.Limits, s.config.WorkspaceLimits)
				if err != nil {
					return 0, nil, err
				}
				credential, err := s.credentials.gitCredential(ctx, OperationWorkspacePrepare, metadata, value.Source, value.CredentialRef)
				if err != nil {
					return 0, nil, err
				}
				defer credential.cleanup()
				runner, err := newGitRunner(
					credential.gitBinary, s.config.TempRoot, s.config.MaxCommandOutput, s.config.ProxyEnvironment,
				)
				if err != nil {
					return 0, nil, apiError(ErrSCMTransport, "git_unavailable", "Git runtime could not be initialized", 503, false, err)
				}
				sourceRef := value.SourceRef
				if isBareWorkspaceSourceRef(sourceRef) {
					resolvedRef, observedOID, err := runner.resolveSource(ctx, value.Source.URL, sourceRef)
					if err != nil {
						return 0, nil, err
					}
					if observedOID != value.BaselineOID {
						return 0, nil, apiError(ErrSCMTransport, "source_moved", "source ref moved from the persisted baseline", 409, false, nil)
					}
					sourceRef = resolvedRef
				}
				archive, err := buildWorkspaceArchive(ctx, runner, value.Source.ID, value.Source.URL, sourceRef, value.BaselineOID, limits)
				if err != nil {
					return 0, nil, err
				}
				defer os.Remove(archive.path) //nolint:errcheck
				reference, err := workspaceArtifactReference(archive)
				if err != nil {
					return 0, nil, err
				}
				prepared := WorkspacePrepareResponse{
					OperationID: metadata.OperationID, RequestDigest: digest, RepositoryID: value.Source.ID,
					SourceRef: sourceRef, BaselineOID: value.BaselineOID, TreeOID: archive.treeOID,
					ManifestDigest: archive.manifestDigest, EntryCount: archive.entryCount,
					ExpandedBytes: archive.expandedBytes, Artifact: reference,
				}
				if err := s.journal.setWorkspaceObjectFile(
					context.WithoutCancel(ctx), metadata.OperationID, digest, prepared, archive.path, archive.size,
				); err != nil {
					return 0, nil, err
				}
				response = &prepared
			}
			object, err := s.journal.openWorkspaceObject(response.Artifact.Digest, response.Artifact.SizeBytes)
			if err != nil {
				return 0, nil, apiError(ErrJournalFull, "workspace_object_missing", "durable workspace artifact is missing or corrupt", 500, false, err)
			}
			defer object.Close() //nolint:errcheck
			if err := verifyWorkspaceObject(object, response.Artifact); err != nil {
				return 0, nil, apiError(ErrJournalFull, "workspace_object_corrupt", "durable workspace artifact is corrupt", 500, false, err)
			}
			attempt, err := s.journal.nextRemoteAttempt(context.WithoutCancel(ctx), metadata.OperationID, digest)
			if err != nil {
				return 0, nil, err
			}
			if err := s.artifacts.upload(ctx, OperationWorkspacePrepare, metadata, attempt, response.Artifact, object); err != nil {
				return 0, nil, err
			}
			return http.StatusOK, *response, nil
		})
}

func (s *Server) handlePublicationPreflight(writer http.ResponseWriter, request *http.Request) {
	s.serveOperation(writer, request, OperationPublicationPreflight,
		func(body []byte) (OperationMetadata, any, error) {
			var value PublicationPreflightRequest
			if err := decodeStrict(body, &value); err != nil {
				return value.Metadata, nil, invalidRequest("preflight request JSON is invalid", err)
			}
			if err := s.validatePublicationMetadata(value.Metadata, OperationPublicationPreflight, "", ""); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateRepository(value.Request.Target, s.config.AllowFileRepositories, s.config.AllowedSCMHosts); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateBranchRef(value.Request.Claim.Ref); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateCredentialReference(value.CredentialRef, CredentialHTTPExtraHeader); err != nil {
				return value.Metadata, nil, err
			}
			return value.Metadata, value, nil
		},
		func(ctx context.Context, metadata OperationMetadata, digest string, _ journalRecord, decoded any) (int, any, error) {
			value := decoded.(PublicationPreflightRequest)
			credential, runtime, err := s.publisherFor(ctx, OperationPublicationPreflight, metadata, value.Request.Target, value.CredentialRef)
			if err != nil {
				return 0, nil, err
			}
			defer credential.cleanup()
			s.publicationMu.Lock()
			result, preflightErr := runtime.Preflight(ctx, value.Request)
			s.publicationMu.Unlock()
			if preflightErr != nil && !errors.Is(preflightErr, publisher.ErrBranchMoved) {
				return 0, nil, preflightErr
			}
			return http.StatusOK, PublicationPreflightResponse{OperationID: metadata.OperationID, RequestDigest: digest, Result: result}, nil
		})
}

func (s *Server) handlePublicationPrepare(writer http.ResponseWriter, request *http.Request) {
	s.serveOperation(writer, request, OperationPublicationPrepare,
		func(body []byte) (OperationMetadata, any, error) {
			var value PublicationPrepareRequest
			if err := decodeStrict(body, &value); err != nil {
				return value.Metadata, nil, invalidRequest("publication prepare request JSON is invalid", err)
			}
			if err := s.validatePublicationMetadata(value.Metadata, OperationPublicationPrepare, value.Request.PublicationID, value.Request.OperationID); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateRepository(value.Request.Source, s.config.AllowFileRepositories, s.config.AllowedSCMHosts); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateRepository(value.Request.Target, s.config.AllowFileRepositories, s.config.AllowedSCMHosts); err != nil {
				return value.Metadata, nil, err
			}
			canonicalRef, err := CanonicalWorkspaceSourceRef(value.Request.SourceRef)
			if err != nil {
				return value.Metadata, nil, err
			}
			value.Request.SourceRef = canonicalRef
			if err := validateBranchRef(value.Request.TargetRef); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateObjectID(value.Request.BaselineOID); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateSourceRefBaseline(value.Request.SourceRef, value.Request.BaselineOID); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateArtifactTransportReference(value.DeltaArtifact); err != nil || value.DeltaArtifact.MediaType != artifactcap.MediaTypeWorkspaceDelta ||
				!constantEqual(value.DeltaArtifact.Digest, value.Request.DeltaArtifactDigest) || value.DeltaArtifact.SizeBytes > s.config.MaxDeltaBytes {
				return value.Metadata, nil, invalidRequest("workspace delta artifact reference is invalid", err)
			}
			if err := validateCredentialReference(value.SourceCredentialRef, CredentialHTTPExtraHeader); err != nil {
				return value.Metadata, nil, err
			}
			return value.Metadata, value, nil
		},
		func(ctx context.Context, metadata OperationMetadata, digest string, _ journalRecord, decoded any) (int, any, error) {
			value := decoded.(PublicationPrepareRequest)
			attempt, err := s.journal.nextRemoteAttempt(context.WithoutCancel(ctx), metadata.OperationID, digest)
			if err != nil {
				return 0, nil, err
			}
			delta, err := s.artifacts.download(ctx, OperationPublicationPrepare, metadata, attempt, value.DeltaArtifact, s.config.MaxDeltaBytes)
			if err != nil {
				return 0, nil, err
			}
			value.Request.DeltaArtifact = delta
			credential, runtime, err := s.publisherFor(ctx, OperationPublicationPrepare, metadata, value.Request.Source, value.SourceCredentialRef)
			if err != nil {
				return 0, nil, err
			}
			defer credential.cleanup()
			if isBareWorkspaceSourceRef(value.Request.SourceRef) {
				runner, err := newGitRunner(
					credential.gitBinary, s.config.TempRoot, s.config.MaxCommandOutput, s.config.ProxyEnvironment,
				)
				if err != nil {
					return 0, nil, apiError(ErrSCMTransport, "git_unavailable", "Git runtime could not be initialized", 503, false, err)
				}
				resolvedRef, observedOID, err := runner.resolveSource(ctx, value.Request.Source.URL, value.Request.SourceRef)
				if err != nil {
					return 0, nil, err
				}
				if observedOID != value.Request.BaselineOID {
					return 0, nil, apiError(ErrSCMTransport, "source_moved", "source ref moved from the persisted baseline", 409, false, nil)
				}
				value.Request.SourceRef = resolvedRef
			}
			s.publicationMu.Lock()
			prepared, prepareErr := runtime.Prepare(ctx, value.Request)
			s.publicationMu.Unlock()
			if prepareErr != nil {
				return 0, nil, prepareErr
			}
			bundleArtifact, err := preparedBundleArtifact(prepared)
			if err != nil {
				return 0, nil, err
			}
			bundle, err := os.Open(prepared.BundlePath)
			if err != nil {
				return 0, nil, apiError(ErrArtifactTransport, "prepared_bundle_missing", "prepared bundle could not be opened for durable upload", 500, false, err)
			}
			uploadErr := s.artifacts.upload(ctx, OperationPublicationPrepare, metadata, attempt, bundleArtifact, bundle)
			_ = bundle.Close()
			if uploadErr != nil {
				return 0, nil, uploadErr
			}
			return http.StatusOK, PublicationPrepareResponse{
				OperationID: metadata.OperationID, RequestDigest: digest, Prepared: externalPrepared(prepared, bundleArtifact),
			}, nil
		})
}

func (s *Server) handlePublicationPublish(writer http.ResponseWriter, request *http.Request) {
	s.serveOperation(writer, request, OperationPublicationPublish,
		func(body []byte) (OperationMetadata, any, error) {
			var value PublicationPublishRequest
			if err := decodeStrict(body, &value); err != nil {
				return value.Metadata, nil, invalidRequest("publication publish request JSON is invalid", err)
			}
			if err := s.validatePublicationMetadata(value.Metadata, OperationPublicationPublish, value.Request.PublicationID, value.Request.OperationID); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateRepository(value.Request.Target, s.config.AllowFileRepositories, s.config.AllowedSCMHosts); err != nil {
				return value.Metadata, nil, err
			}
			if err := validatePreparedTransport(value.Prepared, value.Request.PublicationID, value.Request.ExpectedCommitOID, value.Request.BundleDigest); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateCredentialReference(value.CredentialRef, CredentialHTTPExtraHeader); err != nil {
				return value.Metadata, nil, err
			}
			return value.Metadata, value, nil
		},
		func(ctx context.Context, metadata OperationMetadata, digest string, _ journalRecord, decoded any) (int, any, error) {
			value := decoded.(PublicationPublishRequest)
			credential, runtime, err := s.publisherFor(ctx, OperationPublicationPublish, metadata, value.Request.Target, value.CredentialRef)
			if err != nil {
				return 0, nil, err
			}
			defer credential.cleanup()
			if err := s.restorePreparedBundle(ctx, runtime, OperationPublicationPublish, metadata, digest, value.Prepared); err != nil {
				return 0, nil, err
			}
			s.publicationMu.Lock()
			receipt, publishErr := runtime.Publish(ctx, value.Request)
			s.publicationMu.Unlock()
			if publishErr != nil && !errors.Is(publishErr, publisher.ErrCASRejected) && !errors.Is(publishErr, publisher.ErrPublicationUnknown) {
				return 0, nil, publishErr
			}
			return http.StatusOK, PublicationPublishResponse{OperationID: metadata.OperationID, RequestDigest: digest, Receipt: receipt}, nil
		})
}

func (s *Server) handlePublicationVerify(writer http.ResponseWriter, request *http.Request) {
	s.serveOperation(writer, request, OperationPublicationVerify,
		func(body []byte) (OperationMetadata, any, error) {
			var value PublicationVerifyRequest
			if err := decodeStrict(body, &value); err != nil {
				return value.Metadata, nil, invalidRequest("publication verify request JSON is invalid", err)
			}
			if err := s.validatePublicationMetadata(value.Metadata, OperationPublicationVerify, value.Request.PublicationID, value.Request.OperationID); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateRepository(value.Request.Target, s.config.AllowFileRepositories, s.config.AllowedSCMHosts); err != nil {
				return value.Metadata, nil, err
			}
			if err := validatePreparedTransport(value.Prepared, value.Request.PublicationID, value.Request.ExpectedCommitOID, value.Request.BundleDigest); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateCredentialReference(value.CredentialRef, CredentialHTTPExtraHeader); err != nil {
				return value.Metadata, nil, err
			}
			return value.Metadata, value, nil
		},
		func(ctx context.Context, metadata OperationMetadata, digest string, _ journalRecord, decoded any) (int, any, error) {
			value := decoded.(PublicationVerifyRequest)
			credential, runtime, err := s.publisherFor(ctx, OperationPublicationVerify, metadata, value.Request.Target, value.CredentialRef)
			if err != nil {
				return 0, nil, err
			}
			defer credential.cleanup()
			if err := s.restorePreparedBundle(ctx, runtime, OperationPublicationVerify, metadata, digest, value.Prepared); err != nil {
				return 0, nil, err
			}
			s.publicationMu.Lock()
			receipt, verifyErr := runtime.Verify(ctx, value.Request)
			s.publicationMu.Unlock()
			if verifyErr != nil && !errors.Is(verifyErr, publisher.ErrVerificationUnknown) {
				return 0, nil, verifyErr
			}
			return http.StatusOK, PublicationVerifyResponse{OperationID: metadata.OperationID, RequestDigest: digest, Receipt: receipt}, nil
		})
}

func (s *Server) handlePublicationReclaim(writer http.ResponseWriter, request *http.Request) {
	s.serveOperation(writer, request, OperationPublicationReclaim,
		func(body []byte) (OperationMetadata, any, error) {
			var value PublicationReclaimRequest
			if err := decodeStrict(body, &value); err != nil {
				return value.Metadata, nil, invalidRequest("publication reclaim request JSON is invalid", err)
			}
			if err := s.validatePublicationMetadata(value.Metadata, OperationPublicationReclaim, value.Request.PublicationID, ""); err != nil {
				return value.Metadata, nil, err
			}
			if value.Request.PublicationGeneration < 1 {
				return value.Metadata, nil, invalidRequest("publication generation must be at least 1", nil)
			}
			return value.Metadata, value, nil
		},
		func(ctx context.Context, metadata OperationMetadata, digest string, _ journalRecord, decoded any) (int, any, error) {
			value := decoded.(PublicationReclaimRequest)
			runtime, err := s.newPublisher(s.gitBinary)
			if err != nil {
				return 0, nil, apiError(ErrSCMTransport, "publisher_unavailable", "clean-room publisher could not be initialized", 503, false, err)
			}
			s.publicationMu.Lock()
			result, reclaimErr := runtime.Reclaim(ctx, value.Request)
			s.publicationMu.Unlock()
			if reclaimErr != nil {
				return 0, nil, reclaimErr
			}
			return http.StatusOK, PublicationReclaimResponse{
				OperationID: metadata.OperationID, RequestDigest: digest, Result: result,
			}, nil
		})
}

func (s *Server) handlePullRequestReconcile(writer http.ResponseWriter, request *http.Request) {
	s.serveOperation(writer, request, OperationPullRequestReconcile,
		func(body []byte) (OperationMetadata, any, error) {
			var value PullRequestReconcileRequest
			if err := decodeStrict(body, &value); err != nil {
				return value.Metadata, nil, invalidRequest("pull request reconcile JSON is invalid", err)
			}
			if err := s.validatePublicationMetadata(value.Metadata, OperationPullRequestReconcile, "", ""); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateRepository(value.Intent.BaseRepository, s.config.AllowFileRepositories, s.config.AllowedSCMHosts); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateRepository(value.Intent.HeadRepository, s.config.AllowFileRepositories, s.config.AllowedSCMHosts); err != nil {
				return value.Metadata, nil, err
			}
			if _, err := value.Intent.Key(); err != nil {
				return value.Metadata, nil, err
			}
			if err := validateCredentialReference(value.CredentialRef, CredentialForgeToken); err != nil {
				return value.Metadata, nil, err
			}
			return value.Metadata, value, nil
		},
		func(ctx context.Context, metadata OperationMetadata, digest string, _ journalRecord, decoded any) (int, any, error) {
			if s.config.PRFactory == nil {
				return 0, nil, apiError(ErrNotConfigured, "pr_reconciliation_unavailable", "pull request reconciliation is not configured", http.StatusNotImplemented, false, nil)
			}
			value := decoded.(PullRequestReconcileRequest)
			credential, err := s.credentials.forgeCredential(ctx, OperationPullRequestReconcile, metadata, value.CredentialRef)
			if err != nil {
				return 0, nil, err
			}
			defer credential.cleanup()
			reconciler, err := s.config.PRFactory.New(ctx, credential.filePath)
			if err != nil || reconciler == nil {
				return 0, nil, apiError(nil, "pr_reconciler_unavailable", "pull request reconciler could not be initialized", 503, true, err)
			}
			receipt, err := publisher.ReconcilePullRequest(ctx, value.Intent, reconciler)
			if err != nil {
				return 0, nil, err
			}
			if err := validatePullRequestReceipt(receipt); err != nil {
				return 0, nil, err
			}
			return http.StatusOK, PullRequestReconcileResponse{OperationID: metadata.OperationID, RequestDigest: digest, Receipt: receipt}, nil
		})
}

func (s *Server) validatePublicationMetadata(metadata OperationMetadata, operation Operation, publicationID, operationID string) error {
	if err := metadata.validateFor(operation); err != nil {
		return err
	}
	if publicationID != "" && !constantEqual(metadata.PublicationID, publicationID) {
		return invalidRequest("publication identity does not match request", nil)
	}
	if operationID != "" && !constantEqual(metadata.OperationID, operationID) {
		return invalidRequest("operation identity does not match request", nil)
	}
	return nil
}

func (s *Server) publisherFor(ctx context.Context, operation Operation, metadata OperationMetadata, repository publisher.Repository, reference *CredentialReference) (operationCredential, *publisher.Publisher, error) {
	credential, err := s.credentials.gitCredential(ctx, operation, metadata, repository, reference)
	if err != nil {
		return operationCredential{}, nil, err
	}
	runtime, err := s.newPublisher(credential.gitBinary)
	if err != nil {
		credential.cleanup()
		return operationCredential{}, nil, apiError(ErrSCMTransport, "publisher_unavailable", "clean-room publisher could not be initialized", 503, false, err)
	}
	return credential, runtime, nil
}

func validatePreparedTransport(prepared PreparedPublication, publicationID, commitOID, bundleDigest string) error {
	if prepared.PublicationID != publicationID || prepared.CommitOID != commitOID || prepared.BundleDigest != bundleDigest {
		return invalidRequest("prepared publication transport identity does not match request", nil)
	}
	if err := validateSourceRef(prepared.SourceRef); err != nil {
		return err
	}
	if err := validateBranchRef(prepared.TargetRef); err != nil {
		return err
	}
	if err := validateObjectID(prepared.BaselineOID); err != nil {
		return err
	}
	if err := validateSourceRefBaseline(prepared.SourceRef, prepared.BaselineOID); err != nil {
		return err
	}
	if err := validateArtifactTransportReference(prepared.BundleArtifact); err != nil || prepared.BundleArtifact.MediaType != artifactcap.MediaTypeGitBundle ||
		prepared.BundleArtifact.Digest != prepared.BundleDigest || prepared.BundleArtifact.SizeBytes != prepared.BundleSize {
		return invalidRequest("prepared publication bundle artifact is invalid", err)
	}
	return nil
}

func (s *Server) restorePreparedBundle(
	ctx context.Context,
	runtime *publisher.Publisher,
	operation Operation,
	metadata OperationMetadata,
	requestDigest string,
	prepared PreparedPublication,
) error {
	attempt, err := s.journal.nextRemoteAttempt(context.WithoutCancel(ctx), metadata.OperationID, requestDigest)
	if err != nil {
		return err
	}
	bundle, err := s.artifacts.download(ctx, operation, metadata, attempt, prepared.BundleArtifact, s.config.MaxBundleBytes)
	if err != nil {
		return err
	}
	return runtime.RestorePrepared(ctx, internalPrepared(prepared), bundle)
}

func internalPrepared(value PreparedPublication) publisher.PreparedPublication {
	return publisher.PreparedPublication{
		PublicationID: value.PublicationID, PublicationGeneration: value.PublicationGeneration,
		OperationID: value.OperationID, RequestDigest: value.RequestDigest, Source: value.Source,
		SourceRef: value.SourceRef, Target: value.Target, TargetRef: value.TargetRef,
		BranchClaimGeneration: value.BranchClaimGeneration, BaselineOID: value.BaselineOID,
		RemoteBefore: value.RemoteBefore, DeltaArtifactDigest: value.DeltaArtifactDigest,
		RelativeRoot:   value.RelativeRoot,
		ManifestDigest: value.ManifestDigest, TreeOID: value.TreeOID, CommitOID: value.CommitOID,
		BundleDigest: value.BundleDigest, BundleSize: value.BundleSize, BundleRef: value.BundleRef,
		CommitMessage: value.CommitMessage, CommitTimestamp: value.CommitTimestamp,
	}
}

func preparedBundleArtifact(prepared publisher.PreparedPublication) (harnessv2.ArtifactReference, error) {
	artifactID, err := artifactcap.ArtifactIDForDigest(prepared.BundleDigest)
	if err != nil || prepared.BundleSize < 1 {
		return harnessv2.ArtifactReference{}, invalidRequest("prepared bundle identity is invalid", err)
	}
	reference := harnessv2.ArtifactReference{
		ArtifactID: harnessv2.ArtifactID(artifactID), Digest: prepared.BundleDigest,
		SizeBytes: prepared.BundleSize, MediaType: artifactcap.MediaTypeGitBundle,
	}
	if err := validateArtifactTransportReference(reference); err != nil {
		return harnessv2.ArtifactReference{}, err
	}
	return reference, nil
}

func externalPrepared(value publisher.PreparedPublication, bundleArtifact harnessv2.ArtifactReference) PreparedPublication {
	return PreparedPublication{
		PublicationID: value.PublicationID, PublicationGeneration: value.PublicationGeneration,
		OperationID: value.OperationID, RequestDigest: value.RequestDigest, Source: value.Source,
		SourceRef: value.SourceRef, Target: value.Target, TargetRef: value.TargetRef,
		BranchClaimGeneration: value.BranchClaimGeneration, BaselineOID: value.BaselineOID,
		RemoteBefore: value.RemoteBefore, DeltaArtifactDigest: value.DeltaArtifactDigest,
		RelativeRoot:   value.RelativeRoot,
		ManifestDigest: value.ManifestDigest, TreeOID: value.TreeOID, CommitOID: value.CommitOID,
		BundleDigest: value.BundleDigest, BundleSize: value.BundleSize, BundleRef: value.BundleRef, BundleArtifact: bundleArtifact,
		CommitMessage: value.CommitMessage, CommitTimestamp: value.CommitTimestamp,
	}
}
