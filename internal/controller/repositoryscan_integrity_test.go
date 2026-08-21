package controller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/security"
	securitybundle "github.com/orka-agents/orka/internal/security/bundle"
	"github.com/orka-agents/orka/internal/store"
	sqlitestore "github.com/orka-agents/orka/internal/store/sqlite"
)

func TestBuildRunBundleUsesImmutableInputs(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "ns", UID: types.UID("scan-uid"), Generation: 2},
		Spec: corev1alpha1.RepositoryScanSpec{
			Provider: "github", RepoURL: "https://github.com/example/repo", Owner: "example", Repository: "repo", Branch: "main",
		},
	}
	run := &store.ScanRun{
		ID: "scan_public", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Namespace: "ns", RepositoryScan: "repo", RepositoryScanUID: "scan-uid", RepositoryScanGeneration: 2,
		TaskName: "mapper", Mode: "manual", Phase: scanRunPhaseSucceeded,
		HeadCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TargetReceiptID: "target_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ScannerPolicyVersion: security.ScannerPolicyVersion, PolicyDigest: security.ScannerPolicyDigest(security.ScannerPolicy{}),
		StartedAt: now.Add(-time.Minute), CompletedAt: &now,
		Quality: store.ScanQuality{
			SchemaVersion:           store.SecurityQualitySchemaVersion,
			InventoryCoverageStatus: store.CoverageStatusComplete, CandidateCoverageStatus: store.CoverageStatusComplete,
			CoverageStatus: store.CoverageStatusComplete, ValidationScope: store.ValidationScopeOff,
			ValidationExecution: store.QualityExecutionComplete, AttackPathExecution: store.QualityExecutionDeferred,
			AnalysisAttestationLevel: store.AnalysisAttestationToolObserved,
			TargetVerification:       store.TargetVerificationVerified, BundleStatus: store.BundleStatusSealing,
			AuthorizationStatus: store.AuthorizationStatusLegacyUnverified, IsolationStatus: store.IsolationStatusLegacy,
		},
	}
	if err := s.CreateScanRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveThreatModel(ctx, &store.ThreatModel{Namespace: "ns", RepositoryScan: "repo", Content: "# Threat model\nBounded.", Source: "generated"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSecurityRunThreatModel(ctx, &store.SecurityRunThreatModel{
		RunUID: run.RunUID, Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, ScanRunID: run.ID,
		Version: 1, Content: "# Threat model\nBounded.",
	}); err != nil {
		t.Fatal(err)
	}
	target := &security.MapperTargetReceipt{
		HeadOID: run.HeadCommit, RequestedBranch: "main", CleanTrackedWorktree: true, ObjectFormat: "sha1",
		TreeOID:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TreeDigest:     "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		SnapshotDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		TreeIndex:      []security.MapperTreeIndexEntry{}, TreeEntryCount: 0,
	}
	targetJSON, _ := json.Marshal(target)
	inventory := &security.ReviewSlicesArtifact{
		SchemaVersion: security.SchemaVersionReviewSlicesV2, CoverageStatus: security.MapperCoverageAccountable,
		HeadCommit: run.HeadCommit, DiscoveredFiles: []security.MapperFileInventoryEntry{},
		ReviewableFiles: []security.MapperFileInventoryEntry{}, OmittedFiles: []security.MapperFileInventoryEntry{},
		TargetReceipt: target, Slices: []store.ReviewSlice{},
	}
	inventoryJSON, _ := json.Marshal(inventory)
	if _, err := s.SaveSecurityTargetReceipt(ctx, &store.SecurityTargetReceipt{
		ID: run.TargetReceiptID, Namespace: "ns", RepositoryScan: "repo", ScanRunID: run.ID, RunUID: run.RunUID,
		TargetID: security.RepositoryTargetID(scan), HeadSHA: run.HeadCommit, ObjectFormat: "sha1",
		SnapshotDigest: target.SnapshotDigest, TreeDigest: target.TreeDigest, ReceiptJSON: targetJSON, InventoryJSON: inventoryJSON,
	}); err != nil {
		t.Fatal(err)
	}
	r := &RepositoryScanReconciler{
		SecurityStore: s, IntegrityStore: s, TargetReceiptStore: s, RunThreatModelStore: s, BundleStore: s,
	}
	created, err := r.buildRunBundle(ctx, scan, run)
	if err != nil {
		t.Fatalf("buildRunBundle() error = %v", err)
	}
	if !created {
		t.Fatal("buildRunBundle() created = false")
	}
	sealed, err := s.GetSecurityScanBundle(ctx, "ns", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.ContentDigest == "" || sealed.RunReceiptDigest == "" {
		t.Fatalf("sealed digests = %#v", sealed)
	}
	var evidence []securitybundle.EvidenceBlob
	if err := json.Unmarshal(sealed.EvidenceJSON, &evidence); err != nil {
		t.Fatalf("decode sealed evidence: %v", err)
	}
	if err := securitybundle.Verify(&securitybundle.Bundle{
		ManifestJSON: sealed.ManifestJSON, FindingsJSON: sealed.FindingsJSON, CoverageJSON: sealed.CoverageJSON,
		Evidence: evidence, Roots: securitybundle.RootDigests{
			ContentDigest: sealed.ContentDigest, RunReceiptDigest: sealed.RunReceiptDigest,
		},
	}, securitybundle.DefaultLimits()); err != nil {
		t.Fatalf("verify reloaded bundle: %v", err)
	}
}

func TestValidateFileEvidenceAgainstPinnedTarget(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	ctx := context.Background()
	scan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "ns"}, Spec: corev1alpha1.RepositoryScanSpec{SubPath: "services/api"}}
	run := &store.ScanRun{Namespace: "ns", RepositoryScan: "repo", TargetReceiptID: "target_receipt"}
	receipt := security.MapperTargetReceipt{
		HeadOID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ObjectFormat: "sha1",
		TreeOID:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TreeDigest:     "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		SnapshotDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TreeIndex: []security.MapperTreeIndexEntry{{
			Path: "services/api/main.go", Mode: "100644", Type: "blob",
			ObjectID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Disposition: security.MapperTreeDispositionRegular,
			ContentSize: 100, LineCount: 20,
		}},
	}
	payload, _ := json.Marshal(receipt)
	if _, err := s.SaveSecurityTargetReceipt(ctx, &store.SecurityTargetReceipt{
		ID: run.TargetReceiptID, Namespace: "ns", RepositoryScan: "repo", ScanRunID: "scan",
		RunUID:   "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetID: "repo_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadSHA:  receipt.HeadOID, ObjectFormat: receipt.ObjectFormat,
		SnapshotDigest: receipt.SnapshotDigest,
		TreeDigest:     receipt.TreeDigest,
		ReceiptJSON:    payload,
	}); err != nil {
		t.Fatal(err)
	}
	r := &RepositoryScanReconciler{TargetReceiptStore: s}
	if err := r.validateFileEvidenceAgainstTarget(ctx, scan, run, store.FindingEvidenceRef{
		Path: "main.go", StartLine: 3, EndLine: 7,
	}); err != nil {
		t.Fatalf("valid evidence error = %v", err)
	}
	if err := r.validateFileEvidenceAgainstTarget(ctx, scan, run, store.FindingEvidenceRef{
		Path: "main.go", StartLine: 3, EndLine: 21,
	}); err == nil {
		t.Fatal("out-of-range evidence error = nil")
	}
}

func TestLoadValidationTaskArtifactsCanonicalizesScopedFileEvidence(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	ctx := context.Background()
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "ns", UID: types.UID("scan-uid"), Generation: 2},
		Spec:       corev1alpha1.RepositoryScanSpec{SubPath: "services/api"},
	}
	run := &store.ScanRun{
		ID: "scan_validation_path", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Namespace: scan.Namespace, RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
		RepositoryScanGeneration: scan.Generation, TargetReceiptID: "target_validation_path",
		Quality: store.ScanQuality{TargetVerification: store.TargetVerificationVerified},
	}
	receipt := security.MapperTargetReceipt{
		HeadOID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ObjectFormat: "sha1",
		TreeOID:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TreeDigest:     "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		SnapshotDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TreeIndex: []security.MapperTreeIndexEntry{{
			Path: "services/api/main.go", Mode: "100644", Type: "blob",
			ObjectID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Disposition: security.MapperTreeDispositionRegular,
			ContentSize: 100, LineCount: 20,
		}},
	}
	receiptJSON, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSecurityTargetReceipt(ctx, &store.SecurityTargetReceipt{
		ID: run.TargetReceiptID, Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, ScanRunID: run.ID,
		RunUID: run.RunUID, TargetID: "target_repo", HeadSHA: receipt.HeadOID, ObjectFormat: receipt.ObjectFormat,
		SnapshotDigest: receipt.SnapshotDigest, TreeDigest: receipt.TreeDigest, ReceiptJSON: receiptJSON,
	}); err != nil {
		t.Fatal(err)
	}
	finding := &store.Finding{
		ID: "fnd_validation_path", Namespace: scan.Namespace, RepositoryScan: scan.Name, ScanRunID: run.ID,
		CurrentOccurrenceID: "occ_validation_path", FilePath: "main.go",
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "validation-path", Namespace: scan.Namespace, UID: types.UID("validation-path-uid"),
			Labels: map[string]string{
				labels.LabelSecurityFindingID: finding.ID, labels.LabelSecurityScanID: run.ID,
				labels.LabelSecurityOccurrenceID: finding.CurrentOccurrenceID,
			},
			Annotations: map[string]string{security.AnnotationValidationBindingVersion: security.ValidationBindingVersion},
		},
	}
	artifactJSON, err := json.Marshal(security.ValidationArtifact{
		Version: security.ValidationArtifactSchemaVersion, FindingID: finding.ID, ScanRunID: run.ID,
		OccurrenceID: finding.CurrentOccurrenceID, Status: findingValidationStatusValidated, Summary: "confirmed",
		Evidence: security.ValidationArtifactEvidenceRefs{{Kind: "file", Path: "main.go", StartLine: 2, EndLine: 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactValidation, "application/json", artifactJSON); err != nil {
		t.Fatal(err)
	}
	r := &RepositoryScanReconciler{ArtifactStore: s, TargetReceiptStore: s}

	artifacts, problem, err := r.loadValidationTaskArtifacts(ctx, scan, task, finding, run)
	if err != nil || problem != "" {
		t.Fatalf("loadValidationTaskArtifacts() = (%#v, %q, %v)", artifacts, problem, err)
	}
	if artifacts == nil || len(artifacts.artifact.Evidence) != 1 || artifacts.artifact.Evidence[0].Path != "services/api/main.go" {
		t.Fatalf("canonical evidence = %#v, want services/api/main.go", artifacts)
	}
	if !strings.Contains(artifacts.rawJSON, `"path":"services/api/main.go"`) {
		t.Fatalf("normalized validation JSON = %s, want canonical repo-root path", artifacts.rawJSON)
	}
}

func TestCanonicalValidationEvidencePathRejectsScopedAmbiguity(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{SubPath: "services/api"}}
	finding := &store.Finding{
		FilePath: "main.go",
		Evidence: []store.FindingEvidenceRef{{Kind: "file", Path: "services/api/main.go"}},
	}
	if _, err := canonicalValidationEvidencePath(scan, finding, "services/api/main.go"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("canonicalValidationEvidencePath() error = %v, want ambiguity rejection", err)
	}
}

func TestValidateFileEvidenceAgainstPinnedTargetRejectsAmbiguousScopedPath(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	ctx := context.Background()
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "ns"},
		Spec:       corev1alpha1.RepositoryScanSpec{SubPath: "services/api"},
	}
	run := &store.ScanRun{Namespace: "ns", RepositoryScan: "repo", TargetReceiptID: "target_ambiguous"}
	receipt := security.MapperTargetReceipt{
		HeadOID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ObjectFormat: "sha1",
		TreeOID:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TreeDigest:     "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		SnapshotDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TreeIndex: []security.MapperTreeIndexEntry{
			{Path: "main.go", Mode: "100644", Type: "blob", ObjectID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Disposition: security.MapperTreeDispositionRegular, LineCount: 20},
			{Path: "services/api/main.go", Mode: "100644", Type: "blob", ObjectID: "cccccccccccccccccccccccccccccccccccccccc", Disposition: security.MapperTreeDispositionRegular, LineCount: 20},
		},
	}
	payload, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSecurityTargetReceipt(ctx, &store.SecurityTargetReceipt{
		ID: run.TargetReceiptID, Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, ScanRunID: "scan",
		RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TargetID: "target_repo",
		HeadSHA: receipt.HeadOID, ObjectFormat: receipt.ObjectFormat, SnapshotDigest: receipt.SnapshotDigest,
		TreeDigest: receipt.TreeDigest, ReceiptJSON: payload,
	}); err != nil {
		t.Fatal(err)
	}
	r := &RepositoryScanReconciler{TargetReceiptStore: s}
	if err := r.validateFileEvidenceAgainstTarget(ctx, scan, run, store.FindingEvidenceRef{
		Path: "main.go", StartLine: 1, EndLine: 1,
	}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("validateFileEvidenceAgainstTarget() error = %v, want ambiguity rejection", err)
	}
}

func TestValidationAssessmentAssuranceGap(t *testing.T) {
	if !validationAssessmentHasAssuranceGap(&store.FindingAssessment{Outcome: "skipped"}, store.ValidationScopeAll) {
		t.Fatal("skipped validation was treated as assurance complete")
	}
	if validationAssessmentHasAssuranceGap(&store.FindingAssessment{Outcome: "deferred", FailureClass: "validation_not_selected"}, store.ValidationScopeSampled) {
		t.Fatal("sampled non-selection was treated as an execution failure")
	}
	if !validationAssessmentHasAssuranceGap(&store.FindingAssessment{Outcome: "deferred", FailureClass: "validation_not_selected"}, store.ValidationScopeAll) {
		t.Fatal("full validation non-selection was treated as assurance complete")
	}
	if validationAssessmentHasAssuranceGap(&store.FindingAssessment{Outcome: "validated"}, store.ValidationScopeAll) {
		t.Fatal("validated assessment was treated as an assurance gap")
	}
}

func TestRepositoryScanQualityUsesImmutableRunIdentity(t *testing.T) {
	run := &store.ScanRun{
		RepositoryScanUID: "old-uid", RepositoryScanGeneration: 3,
		Quality: store.LegacyScanQuality(),
	}
	scan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{UID: types.UID("new-uid"), Generation: 4}}
	status := repositoryScanQualityStatus(run, scan)
	if status.ObservedRepositoryScanUID != "old-uid" || status.ObservedGeneration != 3 {
		t.Fatalf("quality identity = %q/%d, want old-uid/3", status.ObservedRepositoryScanUID, status.ObservedGeneration)
	}
	if scanRunMatchesRepositoryScan(run, scan) {
		t.Fatal("stale run unexpectedly matched current RepositoryScan")
	}
}

func TestScanRunQualityDegradedCoversAllQualityDimensions(t *testing.T) {
	run := &store.ScanRun{Quality: store.ScanQuality{
		InventoryCoverageStatus:  store.CoverageStatusComplete,
		CandidateCoverageStatus:  store.CoverageStatusComplete,
		CoverageStatus:           store.CoverageStatusComplete,
		ValidationScope:          store.ValidationScopeOff,
		ValidationExecution:      store.QualityExecutionComplete,
		AttackPathExecution:      store.QualityExecutionDeferred,
		AnalysisAttestationLevel: store.AnalysisAttestationToolObserved,
		TargetVerification:       store.TargetVerificationVerified,
		BundleStatus:             store.BundleStatusNotStarted,
		AuthorizationStatus:      store.AuthorizationStatusVerified,
		IsolationStatus:          store.IsolationStatusHardened,
	}}
	if scanRunQualityDegraded(run) {
		t.Fatal("fully accounted discovery-only run was degraded")
	}
	run.Quality.AuthorizationStatus = store.AuthorizationStatusLegacyUnverified
	if !scanRunQualityDegraded(run) {
		t.Fatal("legacy authorization was treated as verified quality")
	}
	run.Quality.AuthorizationStatus = store.AuthorizationStatusVerified
	run.Quality.BundleStatus = store.BundleStatusFailed
	if !scanRunQualityDegraded(run) {
		t.Fatal("failed bundle was treated as complete quality")
	}
	run.Quality.BundleStatus = store.BundleStatusNotStarted
	run.Quality.ValidationScope = store.ValidationScopeAll
	if !scanRunQualityDegraded(run) {
		t.Fatal("deferred attack path was accepted for full validation")
	}
}

func TestScanRunAssuranceRequiresRecordedFullScopeAndCompleteAttackPath(t *testing.T) {
	run := &store.ScanRun{Quality: store.ScanQuality{
		InventoryCoverageStatus:  store.CoverageStatusComplete,
		CandidateCoverageStatus:  store.CoverageStatusComplete,
		CoverageStatus:           store.CoverageStatusComplete,
		ValidationScope:          store.ValidationScopeAll,
		ValidationExecution:      store.QualityExecutionComplete,
		AttackPathExecution:      store.QualityExecutionComplete,
		AnalysisAttestationLevel: store.AnalysisAttestationToolObserved,
		TargetVerification:       store.TargetVerificationVerified,
		BundleStatus:             store.BundleStatusSealed,
		AuthorizationStatus:      store.AuthorizationStatusAdmitted,
		IsolationStatus:          store.IsolationStatusHardened,
	}}
	if !scanRunAssuranceQualified(run) {
		t.Fatal("fully qualified run was rejected")
	}
	run.Quality.AttackPathExecution = store.QualityExecutionDeferred
	if scanRunAssuranceQualified(run) {
		t.Fatal("deferred attack path qualified for strict assurance")
	}
}

func TestProducerProposedObservationGroupKeyDoesNotMergeDifferentPayloads(t *testing.T) {
	base := store.FindingObservation{
		SemanticFingerprint: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		IdentityQuality:     store.IdentityQualityProducerProposed,
		PayloadDigest:       "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	other := base
	other.PayloadDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	if findingObservationOccurrenceGroupKey(base) == findingObservationOccurrenceGroupKey(other) {
		t.Fatal("different producer-proposed payloads collapsed to one occurrence group")
	}
	other.IdentityQuality = store.IdentityQualityCanonical
	base.IdentityQuality = store.IdentityQualityCanonical
	if findingObservationOccurrenceGroupKey(base) != findingObservationOccurrenceGroupKey(other) {
		t.Fatal("canonical identity did not reconcile by semantic fingerprint")
	}
}

func TestRejectedObservationsDegradeCandidateCoverage(t *testing.T) {
	run := &store.ScanRun{Quality: store.ScanQuality{ReasonCodes: []string{}}}
	degraded := applyObservationCoverageDegradation(run, []store.FindingObservation{{
		Disposition: store.FindingObservationRejected,
		ReasonCode:  "candidate_cap",
	}})
	if !degraded || len(run.Quality.ReasonCodes) != 1 || run.Quality.ReasonCodes[0] != "candidate_cap" {
		t.Fatalf("degraded/reasons = %v/%v", degraded, run.Quality.ReasonCodes)
	}
}

func TestUpdateMapperQualityMarksTruncatedInventoryPartial(t *testing.T) {
	run := &store.ScanRun{Quality: store.ScanQuality{
		InventoryCoverageStatus: store.CoverageStatusPending,
		CandidateCoverageStatus: store.CoverageStatusPending,
		CoverageStatus:          store.CoverageStatusPending,
		ReasonCodes:             []string{},
	}}
	updateMapperQuality(run, &security.ReviewSlicesArtifact{
		SchemaVersion:       security.SchemaVersionReviewSlicesV2,
		CoverageStatus:      security.MapperCoveragePartial,
		CoverageReasonCodes: []string{security.MapperCoverageReasonInventoryEntryLimit},
		InventorySummary: &security.MapperInventorySummary{
			EntryLimit:       2,
			TotalEntries:     3,
			RetainedEntries:  2,
			TruncatedEntries: 1,
			Truncated:        true,
			Reason:           security.MapperCoverageReasonInventoryEntryLimit,
		},
		ReviewableFiles: []security.MapperFileInventoryEntry{{
			Path: "app.go", Disposition: security.MapperDispositionAssigned, Reason: "assigned-to-review-slice",
		}},
	})
	if run.Quality.InventoryCoverageStatus != store.CoverageStatusPartial ||
		run.Quality.CoverageStatus != store.CoverageStatusPartial {
		t.Fatalf("coverage = %#v, want partial inventory and overall coverage", run.Quality)
	}
	if !slices.Contains(run.Quality.ReasonCodes, security.MapperCoverageReasonInventoryEntryLimit) {
		t.Fatalf("reasonCodes = %#v, want stable inventory limit reason", run.Quality.ReasonCodes)
	}
}

func TestValidateRequestedCommitTarget(t *testing.T) {
	sha1 := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sha256 := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, tt := range []struct {
		name         string
		requestedRef string
		objectFormat string
		resolvedHead string
		wantErr      bool
	}{
		{name: "symbolic", requestedRef: "refs/heads/main", objectFormat: "sha1", resolvedHead: sha1},
		{name: "sha1 match", requestedRef: sha1, objectFormat: "sha1", resolvedHead: sha1},
		{name: "sha1 mismatch", requestedRef: sha1, objectFormat: "sha1", resolvedHead: "cccccccccccccccccccccccccccccccccccccccc", wantErr: true},
		{name: "sha1 wrong object format", requestedRef: sha1, objectFormat: "sha256", resolvedHead: sha1, wantErr: true},
		{name: "sha256 match", requestedRef: sha256, objectFormat: "sha256", resolvedHead: sha256},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRequestedCommitTarget(tt.requestedRef, tt.objectFormat, tt.resolvedHead)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateRequestedCommitTarget() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestStageReceiptIDIncludesAttempt(t *testing.T) {
	r := &RepositoryScanReconciler{}
	run := &store.ScanRun{RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", UID: types.UID("task-uid"), Labels: map[string]string{labels.LabelSecurityStage: security.StageReview}}, Status: corev1alpha1.TaskStatus{Attempts: 1}}
	first := r.stageReceiptIDFor(context.Background(), run, task, security.ArtifactFindingsV2, nil, store.StageReceiptRejected)
	task.Status.Attempts = 2
	second := r.stageReceiptIDFor(context.Background(), run, task, security.ArtifactFindingsV2, nil, store.StageReceiptRejected)
	if first == second {
		t.Fatal("stage receipt ID did not bind attempt")
	}
}

func TestDownstreamStageReceiptDoesNotFabricateTargetObservation(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	r := &RepositoryScanReconciler{IntegrityStore: s}
	run := &store.ScanRun{
		ID: "scan_receipt", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Namespace: "ns", RepositoryScan: "repo", StartedAt: time.Now().UTC(),
		HeadCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "review", Namespace: "ns", UID: types.UID("task-uid"),
			Labels: map[string]string{labels.LabelSecurityStage: security.StageReview},
		},
		Spec: corev1alpha1.TaskSpec{
			Workspace: &corev1alpha1.WorkspaceConfig{Ref: run.HeadCommit},
		},
		Status: corev1alpha1.TaskStatus{Attempts: 1},
	}
	if err := r.appendStageReceipt(context.Background(), task, run, security.ArtifactFindingsV2,
		[]byte(`{"schemaVersion":2}`), []byte(`{"schemaVersion":2}`), store.StageReceiptAccepted, "", ""); err != nil {
		t.Fatal(err)
	}
	receipts, _, err := s.ListStageReceipts(context.Background(), store.StageReceiptFilter{
		Namespace: "ns", RepositoryScan: "repo", ScanRunID: run.ID, Limit: 10,
	})
	if err != nil || len(receipts) != 1 {
		t.Fatalf("ListStageReceipts() = (%#v, %v)", receipts, err)
	}
	if receipts[0].ExpectedTargetSHA != run.HeadCommit || receipts[0].ObservedTargetSHA != "" ||
		receipts[0].AttestationLevel != store.AnalysisAttestationDelivered {
		t.Fatalf("downstream receipt target attestation = %#v", receipts[0])
	}
}

func TestLatestSecurityScanRunIDSelectsAuthoritativeRun(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	base := time.Now().UTC().Add(-time.Minute)
	for _, run := range []*store.ScanRun{
		{ID: "scan_old", Namespace: "ns", RepositoryScan: "repo", TaskName: "old", Mode: "manual", Phase: "succeeded", StartedAt: base, Quality: store.LegacyScanQuality()},
		{ID: repositoryScanTestNewRunID, Namespace: "ns", RepositoryScan: "repo", TaskName: "new", Mode: "manual", Phase: "running", StartedAt: base.Add(time.Second), Quality: store.LegacyScanQuality()},
	} {
		if err := s.CreateScanRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
	}
	r := &RepositoryScanReconciler{SecurityStore: s}
	got, err := r.latestSecurityScanRunID(context.Background(), "ns", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got != repositoryScanTestNewRunID {
		t.Fatalf("latestSecurityScanRunID() = %q, want scan_new", got)
	}
}

type failingValidationAssessmentIntegrityStore struct {
	store.SecurityIntegrityStore
	occurrence *store.FindingOccurrence
	err        error
	recorded   *store.FindingAssessment
}

func (s *failingValidationAssessmentIntegrityStore) AppendStageReceipt(context.Context, *store.StageReceipt) (bool, error) {
	return true, nil
}

func (s *failingValidationAssessmentIntegrityStore) GetFindingOccurrence(
	_ context.Context,
	namespace, id string,
) (*store.FindingOccurrence, error) {
	if s.occurrence == nil || s.occurrence.Namespace != namespace || s.occurrence.ID != id {
		return nil, store.ErrNotFound
	}
	copy := *s.occurrence
	copy.DiscoveryPayload = append(json.RawMessage(nil), s.occurrence.DiscoveryPayload...)
	return &copy, nil
}

func (s *failingValidationAssessmentIntegrityStore) RecordFindingAssessment(
	_ context.Context,
	assessment *store.FindingAssessment,
) (bool, error) {
	copy := *assessment
	copy.EvidenceReceiptIDs = append([]string(nil), assessment.EvidenceReceiptIDs...)
	copy.NormalizedPayload = append(json.RawMessage(nil), assessment.NormalizedPayload...)
	copy.ProjectionEvidence = append([]store.FindingEvidenceRef(nil), assessment.ProjectionEvidence...)
	s.recorded = &copy
	return false, s.err
}

type validationIdentityGuardSecurityStore struct {
	store.SecurityStore
	run         *store.ScanRun
	updateCalls int
}

func (s *validationIdentityGuardSecurityStore) GetScanRun(_ context.Context, namespace, id string) (*store.ScanRun, error) {
	if s.run != nil && s.run.Namespace == namespace && s.run.ID == id {
		return s.run, nil
	}
	return nil, store.ErrNotFound
}

func (s *validationIdentityGuardSecurityStore) UpdateScanRun(_ context.Context, _ *store.ScanRun) error {
	s.updateCalls++
	return nil
}

type validationIdentityGuardIntegrityStore struct {
	store.SecurityIntegrityStore
	occurrence  *store.FindingOccurrence
	listCalls   int
	recordCalls int
}

func (s *validationIdentityGuardIntegrityStore) GetFindingOccurrence(
	_ context.Context,
	namespace, id string,
) (*store.FindingOccurrence, error) {
	if s.occurrence == nil || s.occurrence.Namespace != namespace || s.occurrence.ID != id {
		return nil, store.ErrNotFound
	}
	copy := *s.occurrence
	copy.DiscoveryPayload = append(json.RawMessage(nil), s.occurrence.DiscoveryPayload...)
	return &copy, nil
}

func (s *validationIdentityGuardIntegrityStore) ListFindingAssessments(
	_ context.Context,
	_ store.FindingAssessmentFilter,
) ([]store.FindingAssessment, string, error) {
	s.listCalls++
	return nil, "", nil
}

func (s *validationIdentityGuardIntegrityStore) RecordFindingAssessment(
	_ context.Context,
	_ *store.FindingAssessment,
) (bool, error) {
	s.recordCalls++
	return true, nil
}

func TestImmutableValidationRequiresExactRepositoryScanIdentityBeforeAssessmentAccess(t *testing.T) {
	tests := []struct {
		name       string
		runUID     string
		generation int64
	}{
		{name: "missing uid", runUID: "", generation: 7},
		{name: "mismatched uid", runUID: "other-scan-uid", generation: 7},
		{name: "nonpositive generation", runUID: "scan-uid", generation: 0},
		{name: "mismatched generation", runUID: "scan-uid", generation: 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, err := sqlitestore.NewDB(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			backing := sqlitestore.NewStore(db, ":memory:")
			ctx := context.Background()
			scan := &corev1alpha1.RepositoryScan{
				ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "ns", UID: types.UID("scan-uid"), Generation: 7},
			}
			run := &store.ScanRun{
				ID: "scan_identity_guard", RunUID: "run_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
				Namespace: "ns", RepositoryScan: scan.Name, RepositoryScanUID: tt.runUID, RepositoryScanGeneration: tt.generation,
				Quality: store.ScanQuality{
					ValidationExecution:     store.QualityExecutionPending,
					AttackPathExecution:     store.QualityExecutionPending,
					CandidateCoverageStatus: store.CoverageStatusPending,
					CoverageStatus:          store.CoverageStatusPending,
				},
			}
			finding := &store.Finding{
				ID: "fnd_identity_guard", Namespace: scan.Namespace, RepositoryScan: scan.Name, ScanRunID: run.ID,
				CurrentOccurrenceID: "occ_identity_guard", Fingerprint: "identity-guard", Title: "Identity guard",
				Summary: "candidate", Severity: "high", Confidence: "high", ValidationStatus: "unvalidated", State: findingStateOpen,
			}
			if err := backing.UpsertFinding(ctx, finding); err != nil {
				t.Fatal(err)
			}
			discoveryPayload, err := json.Marshal(finding)
			if err != nil {
				t.Fatal(err)
			}
			securityStore := &validationIdentityGuardSecurityStore{SecurityStore: backing, run: run}
			integrityStore := &validationIdentityGuardIntegrityStore{
				SecurityIntegrityStore: backing,
				occurrence: &store.FindingOccurrence{
					ID: finding.CurrentOccurrenceID, Namespace: finding.Namespace, RepositoryScan: finding.RepositoryScan,
					ScanRunID: run.ID, RunUID: run.RunUID, PublicFindingID: finding.ID, DiscoveryPayload: discoveryPayload,
				},
			}
			r := &RepositoryScanReconciler{
				SecurityStore: securityStore, IntegrityStore: integrityStore, ArtifactStore: backing,
			}
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name: "validation", Namespace: scan.Namespace, UID: types.UID("validation-uid"),
					Labels: map[string]string{
						labels.LabelSecurityFindingID: finding.ID, labels.LabelSecurityScanID: run.ID,
						labels.LabelSecurityOccurrenceID: finding.CurrentOccurrenceID,
						labels.LabelSecurityStage:        security.StageValidation,
					},
					Annotations: map[string]string{security.AnnotationValidationBindingVersion: security.ValidationBindingVersion},
				},
				Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
			}

			if err := r.ingestValidationTask(ctx, scan, task); err != nil {
				t.Fatalf("ingestValidationTask() error = %v", err)
			}
			if integrityStore.listCalls != 0 || integrityStore.recordCalls != 0 {
				t.Fatalf("assessment access = list:%d record:%d, want none", integrityStore.listCalls, integrityStore.recordCalls)
			}
			if securityStore.updateCalls != 0 {
				t.Fatalf("UpdateScanRun() calls = %d, want no quality update", securityStore.updateCalls)
			}
			if run.Quality.ValidationExecution != store.QualityExecutionPending ||
				run.Quality.AttackPathExecution != store.QualityExecutionPending ||
				run.Quality.CandidateCoverageStatus != store.CoverageStatusPending ||
				run.Quality.CoverageStatus != store.CoverageStatusPending {
				t.Fatalf("run quality changed = %#v", run.Quality)
			}
			storedFinding, err := backing.GetFinding(ctx, finding.Namespace, finding.ID)
			if err != nil {
				t.Fatal(err)
			}
			if storedFinding.ValidationStatus != finding.ValidationStatus || storedFinding.ValidationJSON != "" {
				t.Fatalf("finding validation changed = %q/%q", storedFinding.ValidationStatus, storedFinding.ValidationJSON)
			}
		})
	}
}

func TestImmutableValidationAssessmentFailureDoesNotPublishFindingFirst(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	backing := sqlitestore.NewStore(db, ":memory:")
	ctx := context.Background()
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "ns", UID: types.UID("scan-uid"), Generation: 1},
	}
	run := &store.ScanRun{
		ID: "scan_atomic_validation", RunUID: "run_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		Namespace: "ns", RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation,
		TaskName: "review", Mode: "manual", Phase: scanRunPhaseSucceeded, StartedAt: time.Now().UTC(),
		Quality: store.LegacyScanQuality(),
	}
	if err := backing.CreateScanRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	finding := &store.Finding{
		ID: "fnd_atomic_validation", Namespace: "ns", RepositoryScan: scan.Name, ScanRunID: run.ID,
		CurrentOccurrenceID: "occ_atomic_validation", Fingerprint: "atomic-validation-fingerprint",
		Title: "Atomic validation", Summary: "candidate", Severity: "high", Confidence: "high",
		ValidationStatus: "unvalidated", State: findingStateOpen,
		Evidence: []store.FindingEvidenceRef{{Kind: "file", Path: "internal/api/handler.go", StartLine: 10, EndLine: 12}},
	}
	if err := backing.UpsertFinding(ctx, finding); err != nil {
		t.Fatal(err)
	}
	discoveryPayload, err := json.Marshal(finding)
	if err != nil {
		t.Fatal(err)
	}
	assessmentErr := errors.New("injected immutable assessment failure")
	integrity := &failingValidationAssessmentIntegrityStore{
		SecurityIntegrityStore: backing,
		err:                    assessmentErr,
		occurrence: &store.FindingOccurrence{
			ID: finding.CurrentOccurrenceID, Namespace: finding.Namespace, RepositoryScan: finding.RepositoryScan,
			ScanRunID: run.ID, RunUID: run.RunUID, PublicFindingID: finding.ID, DiscoveryPayload: discoveryPayload,
		},
	}
	r := &RepositoryScanReconciler{SecurityStore: backing, IntegrityStore: integrity, ArtifactStore: backing}
	validation := security.ValidationArtifact{
		Version: security.ValidationArtifactSchemaVersion, FindingID: finding.ID, ScanRunID: run.ID,
		OccurrenceID: finding.CurrentOccurrenceID, Status: findingValidationStatusValidated, Summary: "confirmed",
		AttackPathAnalysis: "reachable",
	}
	validationJSON, err := json.Marshal(validation)
	if err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "validation", Namespace: finding.Namespace, UID: types.UID("validation-uid"),
			Labels: map[string]string{
				labels.LabelSecurityTarget: scan.Name, labels.LabelSecurityFindingID: finding.ID,
				labels.LabelSecurityStage: security.StageValidation, labels.LabelSecurityMode: security.StageValidation,
				labels.LabelSecurityScanID: run.ID, labels.LabelSecurityOccurrenceID: finding.CurrentOccurrenceID,
			},
			Annotations: map[string]string{security.AnnotationValidationBindingVersion: security.ValidationBindingVersion},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded, Attempts: 1},
	}
	if err := backing.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactValidation, "application/json", validationJSON); err != nil {
		t.Fatal(err)
	}

	if err := r.ingestValidationTask(ctx, scan, task); !errors.Is(err, assessmentErr) {
		t.Fatalf("ingestValidationTask() error = %v, want injected assessment failure", err)
	}
	after, err := backing.GetFinding(ctx, finding.Namespace, finding.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ValidationStatus != finding.ValidationStatus || after.ValidationJSON != finding.ValidationJSON ||
		!slices.Equal(after.Evidence, finding.Evidence) {
		t.Fatalf("finding published before immutable assessment: before=%#v after=%#v", finding, after)
	}
	if integrity.recorded == nil {
		t.Fatal("RecordFindingAssessment() was not called")
	}
	if integrity.recorded.ProjectionValidationStatus != findingValidationStatusValidated {
		t.Fatalf("ProjectionValidationStatus = %q, want validated", integrity.recorded.ProjectionValidationStatus)
	}
	if len(integrity.recorded.ProjectionEvidence) <= len(finding.Evidence) {
		t.Fatalf("ProjectionEvidence = %#v, want full discovery plus validation evidence", integrity.recorded.ProjectionEvidence)
	}
}

//nolint:gocyclo // Validation retry regression intentionally exercises the complete two-attempt lifecycle.
func TestImmutableValidationRetryAppendsCorrectiveAttemptAndReplaysExactly(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	ctx := context.Background()
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "ns", UID: types.UID("scan-uid"), Generation: 2},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo"},
	}
	completed := time.Now().UTC()
	run := &store.ScanRun{
		ID: "scan_validation_retry", RunUID: "run_ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		Namespace: "ns", RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation,
		TaskName: "review", Mode: "manual", Phase: scanRunPhaseSucceeded,
		StartedAt: completed.Add(-time.Minute), CompletedAt: &completed,
		HeadCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TargetReceiptID: "target_validation_retry",
		Quality: store.ScanQuality{
			SchemaVersion: store.SecurityQualitySchemaVersion, InventoryCoverageStatus: store.CoverageStatusComplete,
			CandidateCoverageStatus: store.CoverageStatusPending, CoverageStatus: store.CoverageStatusPending,
			ValidationScope: store.ValidationScopeAll, ValidationExecution: store.QualityExecutionPending,
			AttackPathExecution: store.QualityExecutionPending, TargetVerification: store.TargetVerificationVerified,
			AuthorizationStatus: store.AuthorizationStatusLegacyUnverified, IsolationStatus: store.IsolationStatusLegacy,
		},
	}
	if err := s.CreateScanRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	target := security.MapperTargetReceipt{
		HeadOID: run.HeadCommit, ObjectFormat: "sha1", CleanTrackedWorktree: true,
		TreeOID:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TreeDigest:     "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		SnapshotDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		TreeIndex:      []security.MapperTreeIndexEntry{},
	}
	targetJSON, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSecurityTargetReceipt(ctx, &store.SecurityTargetReceipt{
		ID: run.TargetReceiptID, Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, ScanRunID: run.ID, RunUID: run.RunUID,
		TargetID: "target_repo", HeadSHA: run.HeadCommit, ObjectFormat: "sha1", SnapshotDigest: target.SnapshotDigest,
		TreeDigest: target.TreeDigest, ReceiptJSON: targetJSON,
	}); err != nil {
		t.Fatal(err)
	}
	r := &RepositoryScanReconciler{
		SecurityStore: s, IntegrityStore: s, ArtifactStore: s, ResultStore: s, TargetReceiptStore: s,
		IntegrityConfig: security.IntegrityConfig{FindingObservationWrites: true},
	}
	reviewTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "review", Namespace: run.Namespace, UID: types.UID("review-uid"),
			Labels: map[string]string{labels.LabelSecurityStage: security.StageReview, labels.LabelSecuritySliceID: "slice-1"},
		},
		Spec: corev1alpha1.TaskSpec{
			Workspace: &corev1alpha1.WorkspaceConfig{Ref: run.HeadCommit},
		},
		Status: corev1alpha1.TaskStatus{Attempts: 1},
	}
	finding := store.Finding{
		ID: "fnd_validation_retry", Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, ScanRunID: run.ID, SliceID: "slice-1",
		Fingerprint: "legacy-validation-retry", Title: "Validation retry", Summary: "candidate", Severity: "high",
		Confidence: "high", ValidationStatus: "unvalidated", State: findingStateOpen,
		IdentityQuality: store.IdentityQualityProducerProposed, IdentityAlgorithmVersion: security.SemanticIdentityAlgorithmV1,
		SemanticFingerprint: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
	}
	payload, err := json.Marshal(finding)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.appendStageReceipt(ctx, reviewTask, run, security.ArtifactFindingsV2, payload, payload,
		store.StageReceiptAccepted, "", ""); err != nil {
		t.Fatal(err)
	}
	reviewReceiptID := r.stageReceiptIDFor(ctx, run, reviewTask, security.ArtifactFindingsV2, payload, store.StageReceiptAccepted)
	reviewReceipt, err := s.GetStageReceipt(ctx, run.Namespace, reviewReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	observation := &store.FindingObservation{
		ID: "obs_validation_retry", Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, ScanRunID: run.ID, RunUID: run.RunUID,
		StageReceiptID: reviewReceiptID, TargetReceiptID: run.TargetReceiptID, SliceID: "slice-1",
		Disposition: store.FindingObservationAccepted, RuleID: "validation-retry", IdentityAnchor: "anchor",
		IdentityInstance: "instance", IdentityQuality: store.IdentityQualityProducerProposed,
		IdentityAlgorithmVersion: security.SemanticIdentityAlgorithmV1, SemanticFingerprint: finding.SemanticFingerprint,
		LegacyFingerprint: finding.Fingerprint, NormalizedPayload: payload,
		SourceArtifactName: reviewReceipt.SourceArtifactName, SourceArtifactGeneration: reviewReceipt.SourceArtifactGeneration,
		SourceArtifactDigest: reviewReceipt.SourceArtifactDigest, PolicyDigest: reviewReceipt.ScannerPolicyDigest,
	}
	if _, err := s.AcceptFindingObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	occurrenceID := security.OccurrenceID(run.RunUID, finding.SemanticFingerprint+"\x00"+observation.PayloadDigest)
	finding.CurrentOccurrenceID = occurrenceID
	if _, err := s.FinalizeFindingOccurrence(ctx, &store.FindingOccurrenceFinalization{
		Occurrence: store.FindingOccurrence{
			ID: occurrenceID, Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, ScanRunID: run.ID, RunUID: run.RunUID,
			PublicFindingID: finding.ID, SemanticFindingID: security.SemanticFindingID(finding.SemanticFingerprint),
			SemanticFingerprint: finding.SemanticFingerprint, IdentityQuality: store.IdentityQualityProducerProposed,
			IdentityAlgorithmVersion: security.SemanticIdentityAlgorithmV1, LegacyFingerprint: finding.Fingerprint,
			RuleID: "validation-retry", IdentityAnchor: "anchor", IdentityInstance: "instance",
			TargetReceiptID: run.TargetReceiptID, TargetSHA: run.HeadCommit,
			DiscoveryPayload: observation.NormalizedPayload, PayloadDigest: observation.PayloadDigest,
		},
		ObservationLinks: []store.FindingOccurrenceObservation{{
			ObservationID: observation.ID, Relationship: store.FindingObservationRelationshipContributor,
		}},
		Projection: finding,
	}); err != nil {
		t.Fatal(err)
	}
	validationTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "validation", Namespace: run.Namespace, UID: types.UID("validation-uid"),
			Labels: map[string]string{
				labels.LabelSecurityTarget: scan.Name, labels.LabelSecurityFindingID: finding.ID,
				labels.LabelSecurityStage: security.StageValidation, labels.LabelSecurityMode: security.StageValidation,
				labels.LabelSecurityScanID: run.ID, labels.LabelSecurityOccurrenceID: occurrenceID,
			},
			Annotations: map[string]string{security.AnnotationValidationBindingVersion: security.ValidationBindingVersion},
		},
		Spec: corev1alpha1.TaskSpec{
			Workspace: &corev1alpha1.WorkspaceConfig{Ref: run.HeadCommit},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded, Attempts: 1},
	}
	initialValidation := security.ValidationArtifact{
		Version: security.ValidationArtifactSchemaVersion, FindingID: finding.ID, ScanRunID: run.ID,
		OccurrenceID: occurrenceID, Status: findingValidationStatusFailed, Summary: "first assessment was inconclusive",
	}
	initialValidationJSON, err := json.Marshal(initialValidation)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveArtifact(ctx, validationTask.Namespace, validationTask.Name, security.ArtifactValidation, "application/json", initialValidationJSON); err != nil {
		t.Fatal(err)
	}
	assessmentCount := func() int {
		t.Helper()
		assessments, _, err := s.ListFindingAssessments(ctx, store.FindingAssessmentFilter{
			Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, OccurrenceID: occurrenceID, Limit: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		return len(assessments)
	}

	if err := r.ingestValidationTask(ctx, scan, validationTask); err != nil {
		t.Fatalf("ingestValidationTask(attempt 1) error = %v", err)
	}
	if got := assessmentCount(); got != 2 {
		t.Fatalf("assessment count after attempt 1 = %d, want 2", got)
	}
	if err := r.ingestValidationTask(ctx, scan, validationTask); err != nil {
		t.Fatalf("ingestValidationTask(attempt 1 replay) error = %v", err)
	}
	if got := assessmentCount(); got != 2 {
		t.Fatalf("assessment count after attempt 1 replay = %d, want 2", got)
	}

	validation := security.ValidationArtifact{
		Version: security.ValidationArtifactSchemaVersion, FindingID: finding.ID, ScanRunID: run.ID,
		OccurrenceID: occurrenceID, Status: findingValidationStatusValidated, Summary: "corrected on retry",
		AttackPathAnalysis: "reachable",
	}
	validationJSON, err := json.Marshal(validation)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveArtifact(ctx, validationTask.Namespace, validationTask.Name, security.ArtifactValidation, "application/json", validationJSON); err != nil {
		t.Fatal(err)
	}
	validationTask.Status.Attempts = 2
	if err := r.ingestValidationTask(ctx, scan, validationTask); err != nil {
		t.Fatalf("ingestValidationTask(attempt 2) error = %v", err)
	}
	if got := assessmentCount(); got != 4 {
		t.Fatalf("assessment count after corrective attempt = %d, want 4", got)
	}
	if err := r.ingestValidationTask(ctx, scan, validationTask); err != nil {
		t.Fatalf("ingestValidationTask(attempt 2 replay) error = %v", err)
	}
	if got := assessmentCount(); got != 4 {
		t.Fatalf("assessment count after corrective replay = %d, want 4", got)
	}

	assessments, _, err := s.ListFindingAssessments(ctx, store.FindingAssessmentFilter{
		Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, OccurrenceID: occurrenceID, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	byReceipt := make(map[string]map[store.FindingAssessmentKind]struct{})
	for i := range assessments {
		if byReceipt[assessments[i].StageReceiptID] == nil {
			byReceipt[assessments[i].StageReceiptID] = make(map[store.FindingAssessmentKind]struct{})
		}
		byReceipt[assessments[i].StageReceiptID][assessments[i].Kind] = struct{}{}
	}
	if len(byReceipt) != 2 {
		t.Fatalf("assessment stage receipts = %d, want one per Task attempt", len(byReceipt))
	}
	for receiptID, kinds := range byReceipt {
		if len(kinds) != 2 {
			t.Fatalf("assessment kinds for receipt %q = %#v, want validation and attack path", receiptID, kinds)
		}
	}
	updated, err := s.GetFinding(ctx, finding.Namespace, finding.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ValidationStatus != findingValidationStatusValidated || !strings.Contains(updated.ValidationJSON, "corrected on retry") {
		t.Fatalf("finding validation = %q/%q, want corrective retry projection", updated.ValidationStatus, updated.ValidationJSON)
	}
}

func TestHistoricalValidationIsRejectedAfterRepositoryScanGenerationChange(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	ctx := context.Background()
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "ns", UID: types.UID("scan-uid"), Generation: 2},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo"},
	}
	run := &store.ScanRun{
		ID: "scan_historical", RunUID: "run_dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		Namespace: "ns", RepositoryScan: "repo", RepositoryScanUID: "scan-uid", RepositoryScanGeneration: 1,
		TaskName: "review", Mode: "manual", Phase: scanRunPhaseSucceeded, StartedAt: time.Now().UTC().Add(-time.Minute),
		HeadCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TargetReceiptID: "target_historical",
		Quality: store.ScanQuality{
			SchemaVersion: store.SecurityQualitySchemaVersion, InventoryCoverageStatus: store.CoverageStatusComplete,
			CandidateCoverageStatus: store.CoverageStatusPending, CoverageStatus: store.CoverageStatusPending,
			ValidationScope: store.ValidationScopeAll, ValidationExecution: store.QualityExecutionPending,
			AttackPathExecution: store.QualityExecutionPending, TargetVerification: store.TargetVerificationVerified,
			AuthorizationStatus: store.AuthorizationStatusLegacyUnverified, IsolationStatus: store.IsolationStatusLegacy,
		},
	}
	if err := s.CreateScanRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	target := security.MapperTargetReceipt{
		HeadOID: run.HeadCommit, ObjectFormat: "sha1", CleanTrackedWorktree: true,
		TreeOID:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TreeDigest:     "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		SnapshotDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		TreeIndex:      []security.MapperTreeIndexEntry{},
	}
	targetJSON, _ := json.Marshal(target)
	if _, err := s.SaveSecurityTargetReceipt(ctx, &store.SecurityTargetReceipt{
		ID: run.TargetReceiptID, Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, ScanRunID: run.ID, RunUID: run.RunUID,
		TargetID: "target_repo", HeadSHA: run.HeadCommit, ObjectFormat: "sha1", SnapshotDigest: target.SnapshotDigest,
		TreeDigest: target.TreeDigest, ReceiptJSON: targetJSON,
	}); err != nil {
		t.Fatal(err)
	}
	r := &RepositoryScanReconciler{
		SecurityStore: s, IntegrityStore: s, ArtifactStore: s, ResultStore: s, TargetReceiptStore: s,
		IntegrityConfig: security.IntegrityConfig{FindingObservationWrites: true},
	}
	reviewTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "review", Namespace: "ns", UID: types.UID("review-uid"), Labels: map[string]string{
			labels.LabelSecurityStage: security.StageReview, labels.LabelSecuritySliceID: "slice-1",
		}},
		Spec: corev1alpha1.TaskSpec{
			Workspace: &corev1alpha1.WorkspaceConfig{Ref: run.HeadCommit},
		},
		Status: corev1alpha1.TaskStatus{Attempts: 1},
	}
	finding := store.Finding{
		ID: "fnd_historical", Namespace: "ns", RepositoryScan: "repo", ScanRunID: run.ID, SliceID: "slice-1",
		Fingerprint: "legacy-historical", Title: "Historical validation", Summary: "candidate", Severity: "high",
		Confidence: "high", ValidationStatus: "unvalidated", State: findingStateOpen,
		IdentityQuality: store.IdentityQualityProducerProposed, IdentityAlgorithmVersion: security.SemanticIdentityAlgorithmV1,
		SemanticFingerprint: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
	}
	payload, _ := json.Marshal(finding)
	if err := r.appendStageReceipt(ctx, reviewTask, run, security.ArtifactFindingsV2, payload, payload,
		store.StageReceiptAccepted, "", ""); err != nil {
		t.Fatal(err)
	}
	stageReceiptID := r.stageReceiptIDFor(ctx, run, reviewTask, security.ArtifactFindingsV2, payload, store.StageReceiptAccepted)
	stageReceipt, err := s.GetStageReceipt(ctx, run.Namespace, stageReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	observation := &store.FindingObservation{
		ID: "obs_historical", Namespace: "ns", RepositoryScan: "repo", ScanRunID: run.ID, RunUID: run.RunUID,
		StageReceiptID: stageReceiptID, TargetReceiptID: run.TargetReceiptID, SliceID: "slice-1",
		Disposition: store.FindingObservationAccepted, RuleID: "historical", IdentityAnchor: "anchor",
		IdentityInstance: "instance", IdentityQuality: store.IdentityQualityProducerProposed,
		IdentityAlgorithmVersion: security.SemanticIdentityAlgorithmV1, SemanticFingerprint: finding.SemanticFingerprint,
		LegacyFingerprint: finding.Fingerprint, NormalizedPayload: payload,
		SourceArtifactName: stageReceipt.SourceArtifactName, SourceArtifactGeneration: stageReceipt.SourceArtifactGeneration,
		SourceArtifactDigest: stageReceipt.SourceArtifactDigest, PolicyDigest: stageReceipt.ScannerPolicyDigest,
	}
	if _, err := s.AcceptFindingObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	occurrenceID := security.OccurrenceID(run.RunUID, finding.SemanticFingerprint+"\x00"+observation.PayloadDigest)
	finding.CurrentOccurrenceID = occurrenceID
	if _, err := s.FinalizeFindingOccurrence(ctx, &store.FindingOccurrenceFinalization{
		Occurrence: store.FindingOccurrence{
			ID: occurrenceID, Namespace: "ns", RepositoryScan: "repo", ScanRunID: run.ID, RunUID: run.RunUID,
			PublicFindingID: finding.ID, SemanticFindingID: security.SemanticFindingID(finding.SemanticFingerprint),
			SemanticFingerprint: finding.SemanticFingerprint, IdentityQuality: store.IdentityQualityProducerProposed,
			IdentityAlgorithmVersion: security.SemanticIdentityAlgorithmV1, LegacyFingerprint: finding.Fingerprint,
			RuleID: "historical", IdentityAnchor: "anchor", IdentityInstance: "instance",
			TargetReceiptID: run.TargetReceiptID, TargetSHA: run.HeadCommit,
			DiscoveryPayload: observation.NormalizedPayload, PayloadDigest: observation.PayloadDigest,
		},
		ObservationLinks: []store.FindingOccurrenceObservation{{ObservationID: observation.ID, Relationship: store.FindingObservationRelationshipContributor}},
		Projection:       finding,
	}); err != nil {
		t.Fatal(err)
	}
	validation := security.ValidationArtifact{
		Version: security.ValidationArtifactSchemaVersion, FindingID: finding.ID, ScanRunID: run.ID,
		OccurrenceID: occurrenceID, Status: findingValidationStatusValidated, Summary: "confirmed",
		AttackPathAnalysis: "reachable",
	}
	validationJSON, _ := json.Marshal(validation)
	validationSource := append([]byte(" \n"), validationJSON...)
	validationSource = append(validationSource, '\n')
	validationTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "validation", Namespace: "ns", UID: types.UID("validation-uid"),
			Labels: map[string]string{
				labels.LabelSecurityTarget: "repo", labels.LabelSecurityFindingID: finding.ID,
				labels.LabelSecurityStage: security.StageValidation, labels.LabelSecurityMode: security.StageValidation,
				labels.LabelSecurityScanID: run.ID, labels.LabelSecurityOccurrenceID: occurrenceID,
			},
			Annotations: map[string]string{security.AnnotationValidationBindingVersion: security.ValidationBindingVersion},
		},
		Spec: corev1alpha1.TaskSpec{
			Workspace: &corev1alpha1.WorkspaceConfig{Ref: run.HeadCommit},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded, Attempts: 1},
	}
	if err := s.SaveArtifact(ctx, "ns", validationTask.Name, security.ArtifactValidation, "application/json", validationSource); err != nil {
		t.Fatal(err)
	}
	if err := r.ingestValidationTask(ctx, scan, validationTask); err != nil {
		t.Fatal(err)
	}
	assessments, _, err := s.ListFindingAssessments(ctx, store.FindingAssessmentFilter{
		Namespace: "ns", RepositoryScan: "repo", OccurrenceID: occurrenceID, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(assessments) != 0 {
		t.Fatalf("historical assessments = %d, want generation-mismatched validation rejected", len(assessments))
	}
	storedRun, err := s.GetScanRun(ctx, run.Namespace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedRun.Quality.ValidationExecution != store.QualityExecutionPending ||
		storedRun.Quality.AttackPathExecution != store.QualityExecutionPending ||
		storedRun.Quality.CandidateCoverageStatus != store.CoverageStatusPending ||
		storedRun.Quality.CoverageStatus != store.CoverageStatusPending {
		t.Fatalf("historical run quality changed = %#v", storedRun.Quality)
	}
	storedFinding, err := s.GetFinding(ctx, finding.Namespace, finding.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedFinding.ValidationStatus != finding.ValidationStatus || storedFinding.ValidationJSON != "" {
		t.Fatalf("historical finding validation changed = %q/%q", storedFinding.ValidationStatus, storedFinding.ValidationJSON)
	}
}

func TestCancelledSecurityTaskUsesTerminalFailureClass(t *testing.T) {
	task := &corev1alpha1.Task{Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseCancelled}}
	if !isTerminalScanTask(*task) {
		t.Fatal("cancelled scan task was not terminal")
	}
	if got := securityTaskFailureClass(task); got != securityTaskFailureCancelled {
		t.Fatalf("securityTaskFailureClass() = %q", got)
	}
}
