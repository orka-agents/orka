package v2

import (
	"bytes"
	"fmt"
	"path"
	"strings"
	"time"
)

type WorkspaceDeltaLimits struct {
	MaxBytes                   int64    `json:"maxBytes"`
	MaxEntries                 uint32   `json:"maxEntries"`
	MaxChangedFiles            uint32   `json:"maxChangedFiles,omitempty"`
	AllowedPaths               []string `json:"allowedPaths,omitempty"`
	DenyRepositoryControlPaths bool     `json:"denyRepositoryControlPaths,omitempty"`
	RejectBinaryFiles          bool     `json:"rejectBinaryFiles,omitempty"`
	RejectSecretLikeContent    bool     `json:"rejectSecretLikeContent,omitempty"`
}

func (l WorkspaceDeltaLimits) Validate() error {
	if l.MaxBytes <= 0 {
		return fmt.Errorf("workspace delta max bytes must be positive")
	}
	if l.MaxEntries == 0 {
		return fmt.Errorf("workspace delta max entries must be positive")
	}
	if len(l.AllowedPaths) > 256 {
		return fmt.Errorf("workspace delta allowed paths exceed limit")
	}
	for _, pattern := range l.AllowedPaths {
		cleaned := strings.TrimPrefix(strings.TrimSpace(pattern), "./")
		if cleaned == "" || len(cleaned) > 1024 || strings.ContainsAny(cleaned, "\\\x00") || strings.HasPrefix(cleaned, "/") || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
			return fmt.Errorf("workspace delta allowed path pattern is invalid")
		}
		if _, err := path.Match(cleaned, "validation-probe"); err != nil {
			return fmt.Errorf("workspace delta allowed path pattern is invalid")
		}
	}
	return nil
}

type CreateWorkspaceDeltaRequest struct {
	Protocol                    string                 `json:"protocol"`
	Metadata                    MutationMetadata       `json:"metadata"`
	DeltaID                     WorkspaceDeltaID       `json:"deltaID"`
	Intent                      WorkspaceIntent        `json:"intent"`
	VerifiedBaseline            WorkspaceBaseline      `json:"verifiedBaseline"`
	PromptSettlementDigest      string                 `json:"promptSettlementDigest"`
	Limits                      WorkspaceDeltaLimits   `json:"limits"`
	ArtifactUploadAuthorization *ArtifactAuthorization `json:"artifactUploadAuthorization,omitempty"`
}

func (r CreateWorkspaceDeltaRequest) ValidateAt(now time.Time) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Metadata.validateAt(now, metadataRequirements{session: true, task: true, prompt: true}); err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	if err := requireIdentifier("workspace delta ID", string(r.DeltaID)); err != nil {
		return err
	}
	if err := r.Intent.Validate(); err != nil {
		return err
	}
	if err := r.VerifiedBaseline.Validate(); err != nil {
		return fmt.Errorf("verified baseline: %w", err)
	}
	if err := validateSHA256Digest(r.PromptSettlementDigest); err != nil {
		return fmt.Errorf("prompt settlement digest: %w", err)
	}
	if err := r.Limits.Validate(); err != nil {
		return err
	}
	if r.ArtifactUploadAuthorization != nil {
		if err := r.ArtifactUploadAuthorization.Validate(); err != nil {
			return fmt.Errorf("artifact upload authorization: %w", err)
		}
	}
	return r.Metadata.ValidateDigest(r)
}

type WorkspaceDeltaState string

const (
	WorkspaceDeltaNoChange         WorkspaceDeltaState = "no_change"
	WorkspaceDeltaPrepared         WorkspaceDeltaState = "prepared"
	WorkspaceDeltaReadOnlyModified WorkspaceDeltaState = "read_only_modified"
)

type WorkspaceDeltaDescriptor struct {
	DeltaID           WorkspaceDeltaID    `json:"deltaID"`
	RuntimeSessionUID RuntimeSessionUID   `json:"runtimeSessionUID"`
	SessionGeneration uint64              `json:"sessionGeneration"`
	State             WorkspaceDeltaState `json:"state"`
	Intent            WorkspaceIntent     `json:"intent"`
	VerifiedBaseline  WorkspaceBaseline   `json:"verifiedBaseline"`
	RelativeRoot      string              `json:"relativeRoot,omitempty"`
	ManifestDigest    string              `json:"manifestDigest,omitempty"`
	Artifact          *ArtifactReference  `json:"artifact,omitempty"`
	EntryCount        uint32              `json:"entryCount"`
	ChangedFileCount  uint32              `json:"changedFileCount"`
	DeletedEntryCount uint32              `json:"deletedEntryCount"`
	SymlinkEntryCount uint32              `json:"symlinkEntryCount"`
	NoFollowVerified  bool                `json:"noFollowVerified"`
	PublicationSafe   bool                `json:"publicationSafe"`
	FrozenAt          time.Time           `json:"frozenAt"`
}

//nolint:gocyclo // The explicit state-machine branches are easier to audit together.
func (d WorkspaceDeltaDescriptor) Validate() error {
	if err := requireIdentifier("workspace delta ID", string(d.DeltaID)); err != nil {
		return err
	}
	if err := requireIdentifier("runtime session UID", string(d.RuntimeSessionUID)); err != nil {
		return err
	}
	if d.SessionGeneration == 0 {
		return fmt.Errorf("session generation must be positive")
	}
	if err := d.Intent.Validate(); err != nil {
		return err
	}
	if err := d.VerifiedBaseline.Validate(); err != nil {
		return err
	}
	if err := validateWorkspaceRelativeRoot(d.RelativeRoot); err != nil {
		return err
	}
	if err := validateTimestamp("workspace freeze timestamp", d.FrozenAt); err != nil {
		return err
	}
	switch d.State {
	case WorkspaceDeltaNoChange:
		if d.Artifact != nil || d.ManifestDigest != "" || d.EntryCount != 0 || d.ChangedFileCount != 0 ||
			d.DeletedEntryCount != 0 || d.SymlinkEntryCount != 0 {
			return fmt.Errorf("no-change workspace delta must not carry artifact or changed entries")
		}
		if !d.NoFollowVerified || !d.PublicationSafe {
			return fmt.Errorf("no-change workspace delta must still prove no-follow and publication safety")
		}
	case WorkspaceDeltaPrepared:
		if d.Intent != WorkspaceIntentWrite {
			return fmt.Errorf("prepared workspace delta requires write intent")
		}
		if d.Artifact == nil {
			return fmt.Errorf("prepared workspace delta requires artifact")
		}
		if err := d.Artifact.Validate(); err != nil {
			return fmt.Errorf("workspace delta artifact: %w", err)
		}
		if err := validateSHA256Digest(d.ManifestDigest); err != nil {
			return fmt.Errorf("workspace delta manifest digest: %w", err)
		}
		if d.EntryCount == 0 {
			return fmt.Errorf("prepared workspace delta requires entries")
		}
		if !d.NoFollowVerified || !d.PublicationSafe {
			return fmt.Errorf("prepared workspace delta must be no-follow verified and publication safe")
		}
	case WorkspaceDeltaReadOnlyModified:
		if d.Intent != WorkspaceIntentRead {
			return fmt.Errorf("read-only modification state requires read intent")
		}
		if d.Artifact != nil || d.PublicationSafe {
			return fmt.Errorf("read-only modification must not produce a publishable artifact")
		}
		if d.EntryCount == 0 {
			return fmt.Errorf("read-only modification requires changed entries")
		}
		if !d.NoFollowVerified {
			return fmt.Errorf("read-only modification detection must be no-follow verified")
		}
	default:
		return fmt.Errorf("unsupported workspace delta state %q", d.State)
	}
	if d.ChangedFileCount+d.DeletedEntryCount+d.SymlinkEntryCount > d.EntryCount {
		return fmt.Errorf("workspace delta subtype counts exceed total entries")
	}
	return nil
}

type CreateWorkspaceDeltaResponse struct {
	Protocol       string                   `json:"protocol"`
	Classification Classification           `json:"classification"`
	Delta          WorkspaceDeltaDescriptor `json:"delta"`
}

func (r CreateWorkspaceDeltaResponse) ValidateFor(request CreateWorkspaceDeltaRequest) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Classification.Validate(); err != nil {
		return fmt.Errorf("classification: %w", err)
	}
	if err := r.Delta.Validate(); err != nil {
		return fmt.Errorf("workspace delta: %w", err)
	}
	if r.Delta.DeltaID != request.DeltaID ||
		r.Delta.RuntimeSessionUID != request.Metadata.Fence.RuntimeSessionUID ||
		r.Delta.SessionGeneration != request.Metadata.Fence.RuntimeSessionGeneration ||
		r.Delta.Intent != request.Intent {
		return fmt.Errorf("workspace delta response identity does not match request")
	}
	wantBaseline, err := CanonicalValue(request.VerifiedBaseline)
	if err != nil {
		return fmt.Errorf("canonicalize requested baseline: %w", err)
	}
	gotBaseline, err := CanonicalValue(r.Delta.VerifiedBaseline)
	if err != nil {
		return fmt.Errorf("canonicalize response baseline: %w", err)
	}
	if !bytes.Equal(gotBaseline, wantBaseline) {
		return fmt.Errorf("workspace delta response baseline does not match request")
	}
	if r.Delta.EntryCount > request.Limits.MaxEntries {
		return fmt.Errorf("workspace delta entry count exceeds request limit")
	}
	if r.Delta.Artifact != nil && r.Delta.Artifact.SizeBytes > request.Limits.MaxBytes {
		return fmt.Errorf("workspace delta artifact exceeds request byte limit")
	}
	return nil
}
