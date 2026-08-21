package bundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func effectiveLimits(requested Limits) (Limits, error) {
	defaults := DefaultLimits()
	if requested.MaxDocumentBytes < 0 || requested.MaxStringBytes < 0 || requested.MaxArrayItems < 0 ||
		requested.MaxFindings < 0 || requested.MaxCoverageEntries < 0 || requested.MaxEvidenceBlobs < 0 ||
		requested.MaxEvidenceBlobBytes < 0 || requested.MaxTotalEvidenceBytes < 0 {
		return Limits{}, fmt.Errorf("limits cannot be negative")
	}
	if requested.MaxDocumentBytes == 0 {
		requested.MaxDocumentBytes = defaults.MaxDocumentBytes
	}
	if requested.MaxStringBytes == 0 {
		requested.MaxStringBytes = defaults.MaxStringBytes
	}
	if requested.MaxArrayItems == 0 {
		requested.MaxArrayItems = defaults.MaxArrayItems
	}
	if requested.MaxFindings == 0 {
		requested.MaxFindings = defaults.MaxFindings
	}
	if requested.MaxCoverageEntries == 0 {
		requested.MaxCoverageEntries = defaults.MaxCoverageEntries
	}
	if requested.MaxEvidenceBlobs == 0 {
		requested.MaxEvidenceBlobs = defaults.MaxEvidenceBlobs
	}
	if requested.MaxEvidenceBlobBytes == 0 {
		requested.MaxEvidenceBlobBytes = defaults.MaxEvidenceBlobBytes
	}
	if requested.MaxTotalEvidenceBytes == 0 {
		requested.MaxTotalEvidenceBytes = defaults.MaxTotalEvidenceBytes
	}
	return requested, nil
}

func normalizeManifest(input ManifestInput, evidence []EvidenceDescriptor, limits Limits) (semanticManifestDocument, runEnvelopeDocument, error) {
	if input.SchemaVersion != SchemaVersion {
		return semanticManifestDocument{}, runEnvelopeDocument{}, fmt.Errorf("schemaVersion must be %d", SchemaVersion)
	}

	provider, err := normalizeRequiredToken("repository.provider", input.Repository.Provider, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	repositoryID, err := normalizeRequiredToken("repository.repositoryId", input.Repository.RepositoryID, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	repoURL, err := normalizeRepositoryURL(input.Repository.RepoURL, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	subPath, err := normalizeOptionalPath("repository.subPath", input.Repository.SubPath, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}

	commitSHA, err := normalizeGitObjectID("target.commitSHA", input.Target.CommitSHA, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	treeDigest, err := normalizeSHA256Digest("target.treeDigest", input.Target.TreeDigest, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	targetID, err := normalizeRequiredToken("target.targetId", input.Target.TargetID, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	originalRef, err := normalizeOptionalSingleLine("target.originalRef", input.Target.OriginalRef, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	receiptID, err := normalizeRequiredToken("target.receiptId", input.Target.ReceiptID, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	receiptDigest, err := normalizeSHA256Digest("target.receiptDigest", input.Target.ReceiptDigest, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}

	threatVersion, err := normalizeRequiredToken("threatModel.version", input.ThreatModel.Version, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	threatContent, err := normalizeRequiredText("threatModel.content", input.ThreatModel.Content, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	threatScope, err := normalizeOptionalPath("threatModel.scope", input.ThreatModel.Scope, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	assumptions, err := normalizeTextSet("threatModel.assumptions", input.ThreatModel.Assumptions, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	limitations, err := normalizeTextSet("threatModel.limitations", input.ThreatModel.Limitations, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}

	quality, err := normalizeQuality(input.Quality, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	versions, err := normalizeVersions(input.Versions, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	occurrenceIDs, err := normalizeIDSet("occurrenceIds", input.OccurrenceIDs, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	assessmentIDs, err := normalizeIDSet("assessmentIds", input.AssessmentIDs, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	stageReceiptIDs, err := normalizeIDSet("stageReceiptIds", input.StageReceiptIDs, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	evidenceReceiptIDs, err := normalizeIDSet("evidenceReceiptIds", input.EvidenceReceiptIDs, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	metadata, err := normalizeStringMap("metadata", input.Metadata, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}
	run, err := normalizeRun(input.Run, limits)
	if err != nil {
		return semanticManifestDocument{}, runEnvelopeDocument{}, err
	}

	return semanticManifestDocument{
		SchemaVersion: SchemaVersion,
		Repository: RepositoryIdentity{
			Provider:     strings.ToLower(provider),
			RepositoryID: repositoryID,
			RepoURL:      repoURL,
			SubPath:      subPath,
		},
		Target: TargetSnapshot{
			CommitSHA:     strings.ToLower(commitSHA),
			TreeDigest:    treeDigest,
			TargetID:      targetID,
			OriginalRef:   originalRef,
			ReceiptID:     receiptID,
			ReceiptDigest: receiptDigest,
		},
		ThreatModel: threatModelDocument{
			Version:       threatVersion,
			Content:       threatContent,
			ContentDigest: digestParts(domainThreatModelContent, []byte(threatContent)),
			Scope:         threatScope,
			Assumptions:   assumptions,
			Limitations:   limitations,
		},
		Quality:            quality,
		Versions:           versions,
		OccurrenceIDs:      occurrenceIDs,
		AssessmentIDs:      assessmentIDs,
		StageReceiptIDs:    stageReceiptIDs,
		EvidenceReceiptIDs: evidenceReceiptIDs,
		Metadata:           metadata,
		Evidence:           cloneEvidenceDescriptors(evidence),
	}, run, nil
}

func normalizeQuality(input QualitySummary, limits Limits) (QualitySummary, error) {
	fields := []struct {
		name  string
		value string
		set   func(string)
	}{
		{"quality.inventoryCoverage", input.InventoryCoverage, func(value string) { input.InventoryCoverage = value }},
		{"quality.candidateCoverage", input.CandidateCoverage, func(value string) { input.CandidateCoverage = value }},
		{"quality.coverage", input.Coverage, func(value string) { input.Coverage = value }},
		{"quality.validationScope", input.ValidationScope, func(value string) { input.ValidationScope = value }},
		{"quality.validationExecution", input.ValidationExecution, func(value string) { input.ValidationExecution = value }},
		{"quality.attackPathExecution", input.AttackPathExecution, func(value string) { input.AttackPathExecution = value }},
		{"quality.analysisAttestation", input.AnalysisAttestation, func(value string) { input.AnalysisAttestation = value }},
		{"quality.targetVerification", input.TargetVerification, func(value string) { input.TargetVerification = value }},
		{"quality.authorization", input.Authorization, func(value string) { input.Authorization = value }},
		{"quality.isolation", input.Isolation, func(value string) { input.Isolation = value }},
	}
	for _, field := range fields {
		value, err := normalizeRequiredToken(field.name, field.value, limits)
		if err != nil {
			return QualitySummary{}, err
		}
		field.set(strings.ToLower(value))
	}
	return input, nil
}

func normalizeVersions(input ComponentVersions, limits Limits) (ComponentVersions, error) {
	var err error
	if input.Producer, err = normalizeOptionalSingleLine("versions.producer", input.Producer, limits); err != nil {
		return ComponentVersions{}, err
	}
	if input.Schema, err = normalizeRequiredToken("versions.schema", input.Schema, limits); err != nil {
		return ComponentVersions{}, err
	}
	if input.Controller, err = normalizeRequiredToken("versions.controller", input.Controller, limits); err != nil {
		return ComponentVersions{}, err
	}
	if input.Mapper, err = normalizeOptionalSingleLine("versions.mapper", input.Mapper, limits); err != nil {
		return ComponentVersions{}, err
	}
	if input.Policy, err = normalizeOptionalSingleLine("versions.policy", input.Policy, limits); err != nil {
		return ComponentVersions{}, err
	}
	if input.RuntimeAdapter, err = normalizeOptionalSingleLine("versions.runtimeAdapter", input.RuntimeAdapter, limits); err != nil {
		return ComponentVersions{}, err
	}
	if input.Additional, err = normalizeStringMap("versions.additional", input.Additional, limits); err != nil {
		return ComponentVersions{}, err
	}
	return input, nil
}

func normalizeRun(input RunEnvelope, limits Limits) (runEnvelopeDocument, error) {
	runUID, err := normalizeRequiredToken("run.runUid", input.RunUID, limits)
	if err != nil {
		return runEnvelopeDocument{}, err
	}
	publicRunID, err := normalizeOptionalSingleLine("run.publicRunId", input.PublicRunID, limits)
	if err != nil {
		return runEnvelopeDocument{}, err
	}
	namespace, err := normalizeRequiredToken("run.namespace", input.Namespace, limits)
	if err != nil {
		return runEnvelopeDocument{}, err
	}
	scanName, err := normalizeRequiredToken("run.repositoryScanName", input.RepositoryScanName, limits)
	if err != nil {
		return runEnvelopeDocument{}, err
	}
	scanUID, err := normalizeRequiredToken("run.repositoryScanUid", input.RepositoryScanUID, limits)
	if err != nil {
		return runEnvelopeDocument{}, err
	}
	if input.RepositoryScanGeneration <= 0 {
		return runEnvelopeDocument{}, fmt.Errorf("run.repositoryScanGeneration must be positive")
	}
	startedAt, err := normalizeTime("run.startedAt", input.StartedAt)
	if err != nil {
		return runEnvelopeDocument{}, err
	}
	completedAt, err := normalizeOptionalTime("run.completedAt", input.CompletedAt)
	if err != nil {
		return runEnvelopeDocument{}, err
	}
	sealedAt, err := normalizeTime("run.sealedAt", input.SealedAt)
	if err != nil {
		return runEnvelopeDocument{}, err
	}
	if input.CompletedAt != nil && input.CompletedAt.Before(input.StartedAt) {
		return runEnvelopeDocument{}, fmt.Errorf("run.completedAt cannot precede run.startedAt")
	}
	if input.SealedAt.Before(input.StartedAt) {
		return runEnvelopeDocument{}, fmt.Errorf("run.sealedAt cannot precede run.startedAt")
	}
	if input.CompletedAt != nil && input.SealedAt.Before(*input.CompletedAt) {
		return runEnvelopeDocument{}, fmt.Errorf("run.sealedAt cannot precede run.completedAt")
	}
	return runEnvelopeDocument{
		RunUID:                   runUID,
		PublicRunID:              publicRunID,
		Namespace:                namespace,
		RepositoryScanName:       scanName,
		RepositoryScanUID:        scanUID,
		RepositoryScanGeneration: input.RepositoryScanGeneration,
		StartedAt:                startedAt,
		CompletedAt:              completedAt,
		SealedAt:                 sealedAt,
	}, nil
}

func normalizeFindings(input FindingsInput, limits Limits) (FindingsInput, error) {
	if input.SchemaVersion != SchemaVersion {
		return FindingsInput{}, fmt.Errorf("schemaVersion must be %d", SchemaVersion)
	}
	if len(input.Findings) > limits.MaxFindings || len(input.Findings) > limits.MaxArrayItems {
		return FindingsInput{}, fmt.Errorf("finding count %d exceeds limit", len(input.Findings))
	}
	if input.Findings != nil {
		findings := make([]Finding, len(input.Findings))
		for i := range input.Findings {
			finding, err := normalizeFinding(input.Findings[i], limits)
			if err != nil {
				return FindingsInput{}, fmt.Errorf("finding %d: %w", i, err)
			}
			findings[i] = finding
		}
		sort.Slice(findings, func(i, j int) bool {
			return findingSortKey(findings[i]) < findingSortKey(findings[j])
		})
		for i := 1; i < len(findings); i++ {
			if findingSortKey(findings[i-1]) == findingSortKey(findings[i]) {
				return FindingsInput{}, fmt.Errorf("duplicate finding identity %q", findingSortKey(findings[i]))
			}
		}
		input.Findings = findings
	}
	metadata, err := normalizeStringMap("metadata", input.Metadata, limits)
	if err != nil {
		return FindingsInput{}, err
	}
	input.Metadata = metadata
	return input, nil
}

func normalizeFinding(input Finding, limits Limits) (Finding, error) {
	var err error
	if input.SemanticFingerprint, err = normalizeRequiredToken("semanticFingerprint", input.SemanticFingerprint, limits); err != nil {
		return Finding{}, err
	}
	if input.OccurrenceID, err = normalizeRequiredToken("occurrenceId", input.OccurrenceID, limits); err != nil {
		return Finding{}, err
	}
	if input.LegacyFingerprint, err = normalizeOptionalSingleLine("legacyFingerprint", input.LegacyFingerprint, limits); err != nil {
		return Finding{}, err
	}
	if input.RuleID, err = normalizeRequiredToken("ruleId", input.RuleID, limits); err != nil {
		return Finding{}, err
	}
	if input.IdentityAnchor, err = normalizeRequiredToken("identityAnchor", input.IdentityAnchor, limits); err != nil {
		return Finding{}, err
	}
	if input.IdentityInstance, err = normalizeOptionalSingleLine("identityInstance", input.IdentityInstance, limits); err != nil {
		return Finding{}, err
	}
	if input.Title, err = normalizeRequiredSingleLine("title", input.Title, limits); err != nil {
		return Finding{}, err
	}
	if input.Summary, err = normalizeOptionalText("summary", input.Summary, limits); err != nil {
		return Finding{}, err
	}
	if input.Severity, err = normalizeRequiredToken("severity", input.Severity, limits); err != nil {
		return Finding{}, err
	}
	input.Severity = strings.ToLower(input.Severity)
	if input.Confidence, err = normalizeRequiredToken("confidence", input.Confidence, limits); err != nil {
		return Finding{}, err
	}
	input.Confidence = strings.ToLower(input.Confidence)
	if len(input.Locations) > limits.MaxArrayItems || len(input.Evidence) > limits.MaxArrayItems || len(input.AssessmentIDs) > limits.MaxArrayItems {
		return Finding{}, fmt.Errorf("finding array exceeds maximum item count")
	}
	if input.Locations != nil {
		locations := make([]FindingLocation, len(input.Locations))
		for i := range input.Locations {
			location, err := normalizeLocation(input.Locations[i], limits)
			if err != nil {
				return Finding{}, fmt.Errorf("location %d: %w", i, err)
			}
			locations[i] = location
		}
		sort.Slice(locations, func(i, j int) bool { return locationSortKey(locations[i]) < locationSortKey(locations[j]) })
		input.Locations = locations
	}
	if input.Evidence != nil {
		references := make([]EvidenceReference, len(input.Evidence))
		for i := range input.Evidence {
			reference, err := normalizeEvidenceReference(input.Evidence[i], limits)
			if err != nil {
				return Finding{}, fmt.Errorf("evidence %d: %w", i, err)
			}
			references[i] = reference
		}
		sort.Slice(references, func(i, j int) bool {
			return evidenceReferenceSortKey(references[i]) < evidenceReferenceSortKey(references[j])
		})
		input.Evidence = references
	}
	if input.AssessmentIDs, err = normalizeIDSet("assessmentIds", input.AssessmentIDs, limits); err != nil {
		return Finding{}, err
	}
	return input, nil
}

func normalizeLocation(input FindingLocation, limits Limits) (FindingLocation, error) {
	var err error
	if input.Path, err = normalizeRequiredPath("path", input.Path, limits); err != nil {
		return FindingLocation{}, err
	}
	if input.StartLine <= 0 || input.EndLine < input.StartLine {
		return FindingLocation{}, fmt.Errorf("line range must be positive and ordered")
	}
	if input.Symbol, err = normalizeOptionalSingleLine("symbol", input.Symbol, limits); err != nil {
		return FindingLocation{}, err
	}
	return input, nil
}

func normalizeEvidenceReference(input EvidenceReference, limits Limits) (EvidenceReference, error) {
	var err error
	if input.BlobName, err = normalizeRequiredPath("blobName", input.BlobName, limits); err != nil {
		return EvidenceReference{}, err
	}
	if input.ReceiptID, err = normalizeOptionalSingleLine("receiptId", input.ReceiptID, limits); err != nil {
		return EvidenceReference{}, err
	}
	if input.Label, err = normalizeOptionalSingleLine("label", input.Label, limits); err != nil {
		return EvidenceReference{}, err
	}
	return input, nil
}

func normalizeCoverage(input CoverageInput, limits Limits) (CoverageInput, error) {
	if input.SchemaVersion != SchemaVersion {
		return CoverageInput{}, fmt.Errorf("schemaVersion must be %d", SchemaVersion)
	}
	var err error
	if input.InventoryStatus, err = normalizeRequiredToken("inventoryStatus", input.InventoryStatus, limits); err != nil {
		return CoverageInput{}, err
	}
	input.InventoryStatus = strings.ToLower(input.InventoryStatus)
	if input.CandidateStatus, err = normalizeRequiredToken("candidateStatus", input.CandidateStatus, limits); err != nil {
		return CoverageInput{}, err
	}
	input.CandidateStatus = strings.ToLower(input.CandidateStatus)
	if input.CoverageStatus, err = normalizeRequiredToken("coverageStatus", input.CoverageStatus, limits); err != nil {
		return CoverageInput{}, err
	}
	input.CoverageStatus = strings.ToLower(input.CoverageStatus)

	total := len(input.Inventory) + len(input.Candidates) + len(input.Stages)
	if total > limits.MaxCoverageEntries || len(input.Inventory) > limits.MaxArrayItems ||
		len(input.Candidates) > limits.MaxArrayItems || len(input.Stages) > limits.MaxArrayItems {
		return CoverageInput{}, fmt.Errorf("coverage entry count %d exceeds limit", total)
	}
	if input.Inventory != nil {
		entries := make([]InventoryCoverageEntry, len(input.Inventory))
		seenPaths := make(map[string]struct{}, len(input.Inventory))
		for i := range input.Inventory {
			entry, err := normalizeInventoryEntry(input.Inventory[i], limits)
			if err != nil {
				return CoverageInput{}, fmt.Errorf("inventory %d: %w", i, err)
			}
			if _, exists := seenPaths[entry.Path]; exists {
				return CoverageInput{}, fmt.Errorf("inventory contains duplicate normalized path %q", entry.Path)
			}
			seenPaths[entry.Path] = struct{}{}
			entries[i] = entry
		}
		sort.Slice(entries, func(i, j int) bool { return inventorySortKey(entries[i]) < inventorySortKey(entries[j]) })
		input.Inventory = entries
	}
	if input.Candidates != nil {
		entries := make([]CandidateCoverageEntry, len(input.Candidates))
		seenCandidateIDs := make(map[string]struct{}, len(input.Candidates))
		for i := range input.Candidates {
			entry, err := normalizeCandidateEntry(input.Candidates[i], limits)
			if err != nil {
				return CoverageInput{}, fmt.Errorf("candidate %d: %w", i, err)
			}
			if _, exists := seenCandidateIDs[entry.CandidateID]; exists {
				return CoverageInput{}, fmt.Errorf("candidates contain duplicate normalized candidateId %q", entry.CandidateID)
			}
			seenCandidateIDs[entry.CandidateID] = struct{}{}
			entries[i] = entry
		}
		sort.Slice(entries, func(i, j int) bool { return candidateSortKey(entries[i]) < candidateSortKey(entries[j]) })
		input.Candidates = entries
	}
	if input.Stages != nil {
		entries := make([]StageCoverageEntry, len(input.Stages))
		seenStages := make(map[stageCoverageIdentity]struct{}, len(input.Stages))
		for i := range input.Stages {
			entry, err := normalizeStageEntry(input.Stages[i], limits)
			if err != nil {
				return CoverageInput{}, fmt.Errorf("stage %d: %w", i, err)
			}
			identity := newStageCoverageIdentity(entry)
			if _, exists := seenStages[identity]; exists {
				return CoverageInput{}, fmt.Errorf("stages contain duplicate normalized stage and scope identity %q", identity.String())
			}
			seenStages[identity] = struct{}{}
			entries[i] = entry
		}
		sort.Slice(entries, func(i, j int) bool { return stageSortKey(entries[i]) < stageSortKey(entries[j]) })
		input.Stages = entries
	}
	if input.Metadata, err = normalizeStringMap("metadata", input.Metadata, limits); err != nil {
		return CoverageInput{}, err
	}
	return input, nil
}

func normalizeInventoryEntry(input InventoryCoverageEntry, limits Limits) (InventoryCoverageEntry, error) {
	var err error
	if input.Path, err = normalizeRequiredPath("path", input.Path, limits); err != nil {
		return InventoryCoverageEntry{}, err
	}
	if input.Classification, err = normalizeRequiredToken("classification", input.Classification, limits); err != nil {
		return InventoryCoverageEntry{}, err
	}
	input.Classification = strings.ToLower(input.Classification)
	if input.Reason, err = normalizeOptionalText("reason", input.Reason, limits); err != nil {
		return InventoryCoverageEntry{}, err
	}
	if input.SliceIDs, err = normalizeIDSet("sliceIds", input.SliceIDs, limits); err != nil {
		return InventoryCoverageEntry{}, err
	}
	if input.ReceiptIDs, err = normalizeIDSet("receiptIds", input.ReceiptIDs, limits); err != nil {
		return InventoryCoverageEntry{}, err
	}
	return input, nil
}

func normalizeCandidateEntry(input CandidateCoverageEntry, limits Limits) (CandidateCoverageEntry, error) {
	var err error
	if input.CandidateID, err = normalizeRequiredToken("candidateId", input.CandidateID, limits); err != nil {
		return CandidateCoverageEntry{}, err
	}
	if input.Disposition, err = normalizeRequiredToken("disposition", input.Disposition, limits); err != nil {
		return CandidateCoverageEntry{}, err
	}
	input.Disposition = strings.ToLower(input.Disposition)
	if input.OccurrenceID, err = normalizeOptionalSingleLine("occurrenceId", input.OccurrenceID, limits); err != nil {
		return CandidateCoverageEntry{}, err
	}
	if input.Reason, err = normalizeOptionalText("reason", input.Reason, limits); err != nil {
		return CandidateCoverageEntry{}, err
	}
	if input.ReceiptIDs, err = normalizeIDSet("receiptIds", input.ReceiptIDs, limits); err != nil {
		return CandidateCoverageEntry{}, err
	}
	return input, nil
}

func normalizeStageEntry(input StageCoverageEntry, limits Limits) (StageCoverageEntry, error) {
	var err error
	if input.Stage, err = normalizeRequiredToken("stage", input.Stage, limits); err != nil {
		return StageCoverageEntry{}, err
	}
	input.Stage = strings.ToLower(input.Stage)
	if input.ScopeID, err = normalizeOptionalSingleLine("scopeId", input.ScopeID, limits); err != nil {
		return StageCoverageEntry{}, err
	}
	if input.Disposition, err = normalizeRequiredToken("disposition", input.Disposition, limits); err != nil {
		return StageCoverageEntry{}, err
	}
	input.Disposition = strings.ToLower(input.Disposition)
	if input.ReceiptID, err = normalizeRequiredToken("receiptId", input.ReceiptID, limits); err != nil {
		return StageCoverageEntry{}, err
	}
	return input, nil
}

type stageCoverageIdentity struct {
	stage    string
	scope    string
	hasScope bool
}

func newStageCoverageIdentity(entry StageCoverageEntry) stageCoverageIdentity {
	identity := stageCoverageIdentity{stage: entry.Stage}
	if entry.ScopeID != nil {
		identity.scope = *entry.ScopeID
		identity.hasScope = true
	}
	return identity
}

func (identity stageCoverageIdentity) String() string {
	if !identity.hasScope {
		return identity.stage
	}
	return identity.stage + ":" + identity.scope
}

func normalizeEvidenceInputs(inputs []EvidenceBlobInput, limits Limits) ([]EvidenceBlob, []EvidenceDescriptor, error) {
	if len(inputs) > limits.MaxEvidenceBlobs || len(inputs) > limits.MaxArrayItems {
		return nil, nil, fmt.Errorf("blob count %d exceeds limit", len(inputs))
	}
	if inputs == nil {
		return nil, nil, nil
	}
	blobs := make([]EvidenceBlob, len(inputs))
	var total int64
	for i := range inputs {
		name, err := normalizeRequiredPath("name", inputs[i].Name, limits)
		if err != nil {
			return nil, nil, fmt.Errorf("blob %d: %w", i, err)
		}
		mediaType, err := normalizeMediaType(inputs[i].MediaType, limits)
		if err != nil {
			return nil, nil, fmt.Errorf("blob %q: %w", name, err)
		}
		data := cloneBytes(inputs[i].Data)
		if textualMediaType(mediaType) {
			if !utf8.Valid(data) {
				return nil, nil, fmt.Errorf("blob %q contains invalid UTF-8", name)
			}
			if bytes.IndexByte(data, 0) >= 0 {
				return nil, nil, fmt.Errorf("blob %q contains NUL in UTF-8 text", name)
			}
			data = normalizeLFBytes(data)
		}
		if len(data) > limits.MaxEvidenceBlobBytes {
			return nil, nil, fmt.Errorf("blob %q exceeds maximum size of %d bytes", name, limits.MaxEvidenceBlobBytes)
		}
		total += int64(len(data))
		if total > limits.MaxTotalEvidenceBytes {
			return nil, nil, fmt.Errorf("evidence exceeds maximum total size of %d bytes", limits.MaxTotalEvidenceBytes)
		}
		blobs[i] = EvidenceBlob{
			Name:      name,
			MediaType: mediaType,
			Size:      len(data),
			Digest:    digestParts(domainEvidence, []byte(name), []byte(mediaType), data),
			Data:      data,
		}
	}
	sort.Slice(blobs, func(i, j int) bool {
		if blobs[i].Name != blobs[j].Name {
			return blobs[i].Name < blobs[j].Name
		}
		return blobs[i].MediaType < blobs[j].MediaType
	})
	for i := 1; i < len(blobs); i++ {
		if blobs[i-1].Name == blobs[i].Name {
			return nil, nil, fmt.Errorf("duplicate evidence blob name %q", blobs[i].Name)
		}
	}
	descriptors := make([]EvidenceDescriptor, len(blobs))
	for i := range blobs {
		descriptors[i] = EvidenceDescriptor{
			Name:      blobs[i].Name,
			MediaType: blobs[i].MediaType,
			Size:      blobs[i].Size,
			Digest:    blobs[i].Digest,
		}
	}
	return blobs, descriptors, nil
}

func validateEvidenceReferences(findings FindingsInput, descriptors []EvidenceDescriptor) error {
	available := make(map[string]struct{}, len(descriptors))
	for _, descriptor := range descriptors {
		available[descriptor.Name] = struct{}{}
	}
	for _, finding := range findings.Findings {
		for _, reference := range finding.Evidence {
			if _, ok := available[reference.BlobName]; !ok {
				return fmt.Errorf("finding %q references missing evidence blob %q", finding.OccurrenceID, reference.BlobName)
			}
		}
	}
	return nil
}

func normalizeRequiredText(name, value string, limits Limits) (string, error) {
	normalized, err := normalizeText(name, value, limits)
	if err != nil {
		return "", err
	}
	if normalized == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return normalized, nil
}

func normalizeRequiredSingleLine(name, value string, limits Limits) (string, error) {
	normalized, err := normalizeRequiredText(name, strings.TrimSpace(value), limits)
	if err != nil {
		return "", err
	}
	if strings.Contains(normalized, "\n") {
		return "", fmt.Errorf("%s must be a single line", name)
	}
	return normalized, nil
}

func normalizeRequiredToken(name, value string, limits Limits) (string, error) {
	normalized, err := normalizeRequiredSingleLine(name, value, limits)
	if err != nil {
		return "", err
	}
	for _, r := range normalized {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", fmt.Errorf("%s contains whitespace or control characters", name)
		}
	}
	return normalized, nil
}

func normalizeGitObjectID(name, value string, limits Limits) (string, error) {
	normalized, err := normalizeRequiredToken(name, value, limits)
	if err != nil {
		return "", err
	}
	normalized = strings.ToLower(normalized)
	if len(normalized) != 40 && len(normalized) != 64 {
		return "", fmt.Errorf("%s must be a full 40- or 64-hex Git object ID", name)
	}
	if _, err := hex.DecodeString(normalized); err != nil {
		return "", fmt.Errorf("%s must be a full 40- or 64-hex Git object ID", name)
	}
	return normalized, nil
}

func normalizeOptionalText(name string, value *string, limits Limits) (*string, error) {
	if value == nil {
		return nil, nil
	}
	normalized, err := normalizeText(name, *value, limits)
	if err != nil {
		return nil, err
	}
	return &normalized, nil
}

func normalizeOptionalSingleLine(name string, value *string, limits Limits) (*string, error) {
	if value == nil {
		return nil, nil
	}
	normalized, err := normalizeText(name, strings.TrimSpace(*value), limits)
	if err != nil {
		return nil, err
	}
	if strings.Contains(normalized, "\n") {
		return nil, fmt.Errorf("%s must be a single line", name)
	}
	return &normalized, nil
}

func normalizeText(name, value string, limits Limits) (string, error) {
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("%s contains invalid UTF-8", name)
	}
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	if strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("%s contains NUL", name)
	}
	if len(value) > limits.MaxStringBytes {
		return "", fmt.Errorf("%s exceeds maximum size of %d bytes", name, limits.MaxStringBytes)
	}
	return value, nil
}

func normalizeRequiredPath(name, value string, limits Limits) (string, error) {
	normalized, err := normalizePath(name, value, false, limits)
	if err != nil {
		return "", err
	}
	return normalized, nil
}

func normalizeOptionalPath(name string, value *string, limits Limits) (*string, error) {
	if value == nil {
		return nil, nil
	}
	normalized, err := normalizePath(name, *value, true, limits)
	if err != nil {
		return nil, err
	}
	return &normalized, nil
}

func repositoryURLForm(value string) bool {
	scheme, ok := repositoryScheme(value)
	if !ok {
		return false
	}
	return strings.HasPrefix(value[len(scheme):], "://")
}

func repositoryScheme(value string) (string, bool) {
	delimiter := strings.IndexByte(value, ':')
	if delimiter <= 0 {
		return "", false
	}
	scheme := value[:delimiter]
	for i := range len(scheme) {
		c := scheme[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && ((c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.')) {
			continue
		}
		return "", false
	}
	return scheme, true
}

func repositoryNetworkScheme(scheme string) bool {
	for component := range strings.SplitSeq(strings.ToLower(scheme), "+") {
		switch component {
		case "git", "http", "https", "ssh", "ftp", "ftps", "sftp", "rsync", "svn", "hg", "bzr":
			return true
		}
	}
	return false
}

func repositoryPathHasAuthorityLikeCredentials(value string) bool {
	value = strings.TrimLeft(value, "/")
	segment, _, _ := strings.Cut(value, "/")
	return strings.IndexByte(segment, '@') > 0
}

func repositoryPathHasPasswordLikeCredentials(value string) bool {
	value = strings.TrimLeft(value, "/")
	segment, _, _ := strings.Cut(value, "/")
	at := strings.IndexByte(segment, '@')
	return at > 0 && strings.ContainsRune(segment[:at], ':')
}

func hostlessRepositoryNetworkURL(value string, parsed *url.URL, scheme string, hasScheme bool) bool {
	if parsed.Hostname() != "" {
		return false
	}
	if parsed.User != nil || strings.HasPrefix(value, "//") {
		return true
	}
	if repositoryURLForm(value) && !strings.EqualFold(scheme, "file") {
		return true
	}
	if hasScheme && repositoryNetworkScheme(scheme) {
		return true
	}
	return hasScheme && (repositoryPathHasAuthorityLikeCredentials(parsed.Path) || repositoryPathHasAuthorityLikeCredentials(parsed.Opaque))
}

func repositoryTopLevelURLForm(value string) bool {
	if delimiter := strings.IndexAny(value, "?#"); delimiter >= 0 {
		value = value[:delimiter]
	}
	return strings.Contains(value, "://")
}

func validSCPRepositoryHost(host string) bool {
	if strings.HasPrefix(host, "[") {
		if !strings.HasSuffix(host, "]") {
			return false
		}
		address, err := netip.ParseAddr(host[1 : len(host)-1])
		return err == nil && address.Is6()
	}
	if strings.ContainsAny(host, "[]:") {
		return false
	}
	hasAlphaNumeric := false
	for _, r := range host {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			hasAlphaNumeric = true
			continue
		}
		if r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return hasAlphaNumeric
}

func normalizeSCPRepository(value string) (string, bool) {
	if delimiter := strings.IndexAny(value, "?#"); delimiter >= 0 {
		value = value[:delimiter]
	}
	if value == "" || strings.Contains(value, "://") || value != strings.TrimLeftFunc(value, unicode.IsSpace) {
		return "", false
	}

	delimiter := scpRepositoryPathDelimiter(value)
	if delimiter <= 0 || delimiter == len(value)-1 {
		return "", false
	}

	authority := value[:delimiter]
	host := authority
	if at := strings.IndexByte(authority, '@'); at >= 0 {
		username := authority[:at]
		if username == "" || at == len(authority)-1 || strings.ContainsAny(username, ":/\\@") ||
			strings.IndexFunc(username, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			return "", false
		}
		host = authority[at+1:]
	}

	repositoryPath := value[delimiter+1:]
	if host != strings.TrimSpace(host) || !validSCPRepositoryHost(host) || strings.TrimSpace(repositoryPath) == "" ||
		strings.ContainsRune(repositoryPath, '\\') ||
		strings.IndexFunc(repositoryPath, unicode.IsControl) >= 0 {
		return "", false
	}
	return value, true
}

func scpRepositoryPathDelimiter(value string) int {
	inBrackets := false
	for index := range len(value) {
		switch value[index] {
		case '[':
			if inBrackets {
				return -1
			}
			inBrackets = true
		case ']':
			if !inBrackets {
				return -1
			}
			inBrackets = false
		case ':':
			if !inBrackets {
				return index
			}
		}
	}
	return -1
}

func normalizeRepositoryURL(value string, limits Limits) (string, error) {
	value, err := normalizeText("repository.repoURL", value, limits)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("repository.repoURL is required")
	}
	if strings.Contains(value, "\n") {
		return "", fmt.Errorf("repository.repoURL must be a single line")
	}

	parseValue := strings.TrimSpace(value)
	// Preserve the previous tolerance for formatting whitespace before the
	// coordinate without trimming whitespace that belongs to the remote path.
	scpValue, scpOK := normalizeSCPRepository(strings.TrimLeftFunc(value, unicode.IsSpace))
	parsed, parseErr := url.Parse(parseValue)
	scheme, hasScheme := repositoryScheme(parseValue)
	urlForm := repositoryURLForm(parseValue)
	if repositoryTopLevelURLForm(parseValue) && !urlForm {
		// A malformed top-level URL-like value must not fall back to opaque or
		// SCP-style handling, because doing so can retain credential-bearing
		// authority text. Keep the error generic so userinfo is not reflected.
		return "", fmt.Errorf("repository.repoURL must be a valid URL")
	}
	if parseErr != nil {
		// URL parsing failures are only eligible for the narrow, positively
		// validated SCP spelling. Never return parseErr because it can echo raw
		// credentials or other attacker-controlled repository text.
		if hasScheme && strings.HasPrefix(parseValue[len(scheme)+1:], "/") &&
			repositoryPathHasPasswordLikeCredentials(parseValue[len(scheme)+1:]) {
			return "", fmt.Errorf("repository.repoURL must be a valid URL")
		}
		if scpOK {
			return scpValue, nil
		}
		return "", fmt.Errorf("repository.repoURL must be a valid URL")
	}
	if hostlessRepositoryNetworkURL(parseValue, parsed, scheme, hasScheme) {
		// Network repository forms require a host. In particular, do not treat
		// malformed authority-like credentials parsed into Path or Opaque as an
		// SCP-style fallback coordinate.
		return "", fmt.Errorf("repository.repoURL must be a valid URL")
	}
	if parsed.Hostname() != "" {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		value = parsed.String()
		if len(value) > limits.MaxStringBytes {
			return "", fmt.Errorf("repository.repoURL exceeds maximum size of %d bytes", limits.MaxStringBytes)
		}
		return value, nil
	}
	if scpOK {
		return scpValue, nil
	}
	value = parseValue
	if cut, _, found := strings.Cut(value, "?"); found {
		value = cut
	}
	if cut, _, found := strings.Cut(value, "#"); found {
		value = cut
	}
	if value == "" {
		return "", fmt.Errorf("repository.repoURL is required after sanitization")
	}
	return value, nil
}

func normalizePath(name, value string, allowEmpty bool, limits Limits) (string, error) {
	value, err := normalizeText(name, value, limits)
	if err != nil {
		return "", err
	}
	if strings.Contains(value, "\\") {
		return "", fmt.Errorf("%s must not contain backslashes", name)
	}
	if value == "" {
		if allowEmpty {
			return "", nil
		}
		return "", fmt.Errorf("%s is required", name)
	}
	if strings.Contains(value, "\n") || path.IsAbs(value) || windowsVolumeQualifiedPath(value) {
		return "", fmt.Errorf("%s must be a relative repository path", name)
	}
	cleaned := path.Clean(value)
	if windowsVolumeQualifiedPath(cleaned) {
		return "", fmt.Errorf("%s must be a relative repository path", name)
	}
	if cleaned == "." {
		if allowEmpty {
			return "", nil
		}
		return "", fmt.Errorf("%s is required", name)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%s escapes the repository", name)
	}
	return cleaned, nil
}

func windowsVolumeQualifiedPath(value string) bool {
	return len(value) >= 2 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':'
}

func normalizeSHA256Digest(name, value string, limits Limits) (string, error) {
	value, err := normalizeRequiredToken(name, value, limits)
	if err != nil {
		return "", err
	}
	value = strings.ToLower(value)
	encoded, ok := strings.CutPrefix(value, "sha256:")
	if !ok || len(encoded) != sha256.Size*2 {
		return "", fmt.Errorf("%s must be a full sha256 digest", name)
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return "", fmt.Errorf("%s must be a full sha256 digest", name)
	}
	return value, nil
}

func normalizeIDSet(name string, values []string, limits Limits) ([]string, error) {
	if len(values) > limits.MaxArrayItems {
		return nil, fmt.Errorf("%s exceeds maximum item count", name)
	}
	if values == nil {
		return nil, nil
	}
	out := make([]string, len(values))
	for i := range values {
		value, err := normalizeRequiredToken(fmt.Sprintf("%s[%d]", name, i), values[i], limits)
		if err != nil {
			return nil, err
		}
		out[i] = value
	}
	slices.Sort(out)
	for i := 1; i < len(out); i++ {
		if out[i-1] == out[i] {
			return nil, fmt.Errorf("%s contains duplicate %q", name, out[i])
		}
	}
	return out, nil
}

func normalizeTextSet(name string, values []string, limits Limits) ([]string, error) {
	if len(values) > limits.MaxArrayItems {
		return nil, fmt.Errorf("%s exceeds maximum item count", name)
	}
	if values == nil {
		return nil, nil
	}
	out := make([]string, len(values))
	for i := range values {
		value, err := normalizeRequiredText(fmt.Sprintf("%s[%d]", name, i), strings.TrimSpace(values[i]), limits)
		if err != nil {
			return nil, err
		}
		out[i] = value
	}
	slices.Sort(out)
	for i := 1; i < len(out); i++ {
		if out[i-1] == out[i] {
			return nil, fmt.Errorf("%s contains duplicate value", name)
		}
	}
	return out, nil
}

func normalizeStringMap(name string, values map[string]string, limits Limits) (map[string]string, error) {
	if len(values) > limits.MaxArrayItems {
		return nil, fmt.Errorf("%s exceeds maximum item count", name)
	}
	if values == nil {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		normalizedKey, err := normalizeRequiredSingleLine(name+" key", key, limits)
		if err != nil {
			return nil, err
		}
		normalizedValue, err := normalizeText(name+"["+normalizedKey+"]", value, limits)
		if err != nil {
			return nil, err
		}
		if _, exists := out[normalizedKey]; exists {
			return nil, fmt.Errorf("%s contains colliding key %q", name, normalizedKey)
		}
		out[normalizedKey] = normalizedValue
	}
	return out, nil
}

func normalizeTime(name string, value time.Time) (string, error) {
	if value.IsZero() {
		return "", fmt.Errorf("%s is required", name)
	}
	utc := value.UTC()
	formatted := utc.Format(time.RFC3339Nano)
	parsed, err := time.Parse(time.RFC3339Nano, formatted)
	if err != nil || !parsed.Equal(utc) || parsed.UTC().Format(time.RFC3339Nano) != formatted {
		return "", fmt.Errorf("%s is outside the canonical RFC3339 range", name)
	}
	return formatted, nil
}

func normalizeOptionalTime(name string, value *time.Time) (*string, error) {
	if value == nil {
		return nil, nil
	}
	normalized, err := normalizeTime(name, *value)
	if err != nil {
		return nil, err
	}
	return &normalized, nil
}

func normalizeMediaType(value string, limits Limits) (string, error) {
	value, err := normalizeRequiredSingleLine("mediaType", value, limits)
	if err != nil {
		return "", err
	}
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil {
		return "", fmt.Errorf("invalid media type: %w", err)
	}
	mediaType = strings.ToLower(mediaType)
	if textualMediaTypeBase(mediaType) {
		if charset, ok := params["charset"]; ok && !strings.EqualFold(strings.TrimSpace(charset), "utf-8") &&
			!strings.EqualFold(strings.TrimSpace(charset), "utf8") {
			return "", fmt.Errorf("textual media type charset must be UTF-8")
		}
		params["charset"] = "utf-8"
	}
	formatted := mime.FormatMediaType(mediaType, params)
	if len(formatted) > limits.MaxStringBytes {
		return "", fmt.Errorf("mediaType exceeds maximum size of %d bytes", limits.MaxStringBytes)
	}
	return formatted, nil
}

func textualMediaType(mediaType string) bool {
	base, _, err := mime.ParseMediaType(mediaType)
	return err == nil && textualMediaTypeBase(strings.ToLower(base))
}

func textualMediaTypeBase(base string) bool {
	return strings.HasPrefix(base, "text/") || strings.HasSuffix(base, "+json") || strings.HasSuffix(base, "+xml") ||
		base == "application/json" || base == "application/xml" || base == "application/yaml" ||
		base == "application/x-yaml" || base == "application/javascript"
}

func normalizeLFBytes(data []byte) []byte {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
}

func cloneEvidenceDescriptors(descriptors []EvidenceDescriptor) []EvidenceDescriptor {
	if descriptors == nil {
		return nil
	}
	out := make([]EvidenceDescriptor, len(descriptors))
	copy(out, descriptors)
	return out
}

func findingSortKey(finding Finding) string {
	return finding.SemanticFingerprint + "\x00" + finding.OccurrenceID
}

func locationSortKey(location FindingLocation) string {
	return canonicalSortKey(location)
}

func evidenceReferenceSortKey(reference EvidenceReference) string {
	return canonicalSortKey(reference)
}

func inventorySortKey(entry InventoryCoverageEntry) string {
	return canonicalSortKey(entry)
}

func candidateSortKey(entry CandidateCoverageEntry) string {
	return canonicalSortKey(entry)
}

func stageSortKey(entry StageCoverageEntry) string {
	return canonicalSortKey(entry)
}

func canonicalSortKey(value any) string {
	data, err := canonicalJSON(value)
	if err != nil {
		panic(fmt.Sprintf("canonical sort key: %v", err))
	}
	return string(data)
}
