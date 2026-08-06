package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	securityidentity "github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

const validatedStatusJSON = `{"status":"validated"}`

func integrityTestDigest(value string) string {
	return securityDigestBytes([]byte(value))
}

func integrityTestRawDigest(value string) string {
	return strings.TrimPrefix(integrityTestDigest(value), "sha256:")
}

func activeRepositoryMigrationRun(
	id string,
	ordinal int,
	requestKey string,
	phase string,
	bundleStatus store.BundleStatus,
	startedAt time.Time,
) *store.ScanRun {
	quality := store.LegacyScanQuality()
	quality.BundleStatus = bundleStatus
	return &store.ScanRun{
		ID:                       id,
		RunUID:                   fmt.Sprintf("run_%064x", ordinal),
		Namespace:                "ns-migration",
		RepositoryScan:           "repo-migration",
		RepositoryScanUID:        "repository-scan-uid-migration",
		RepositoryScanGeneration: 1,
		TaskName:                 "task-" + id,
		Mode:                     "manual",
		Phase:                    phase,
		RequestIdempotencyKey:    requestKey,
		IdempotencyKey:           requestKey,
		StartedAt:                startedAt,
		Quality:                  quality,
		Summary:                  "preserved summary for " + id,
	}
}

const (
	testSecondRunUID = "run_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testSecondScanID = "scan_second"
)

func TestActiveRepositoryIndexMigrationFailsDuplicateSealingRunsClosed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "duplicate-sealing.db")
	db, err := NewDB(dbPath)
	if err != nil {
		t.Fatalf("NewDB() error = %v", err)
	}
	s := NewStore(db, dbPath)
	ctx := context.Background()
	if _, err := db.Exec(`DROP INDEX idx_security_scan_runs_active_repository`); err != nil {
		_ = db.Close()
		t.Fatalf("drop active-repository index: %v", err)
	}

	started := time.Date(2026, time.August, 2, 8, 0, 0, 0, time.UTC)
	running := activeRepositoryMigrationRun("run-active", 1, "request-a", "running", store.BundleStatusNotStarted, started)
	sealingWithActive := activeRepositoryMigrationRun("run-sealing-with-active", 2, "request-b", "succeeded", store.BundleStatusSealing, started.Add(time.Minute))
	sealingWithActive.ErrorMessage = "existing audit detail"
	firstSealing := activeRepositoryMigrationRun("run-sealing-first", 3, "request-c", "succeeded", store.BundleStatusSealing, started.Add(2*time.Minute))
	firstSealing.RepositoryScan = "repo-migration-sealing"
	secondSealing := activeRepositoryMigrationRun("run-sealing-second", 4, "request-d", "running", store.BundleStatusSealing, started.Add(3*time.Minute))
	secondSealing.RepositoryScan = firstSealing.RepositoryScan
	uniqueSealing := activeRepositoryMigrationRun("run-sealing-unique", 5, "request-e", "succeeded", store.BundleStatusSealing, started.Add(4*time.Minute))
	uniqueSealing.RepositoryScan = "repo-migration-unique"
	for _, run := range []*store.ScanRun{running, sealingWithActive, firstSealing, secondSealing, uniqueSealing} {
		if err := s.CreateScanRun(ctx, run); err != nil {
			_ = db.Close()
			t.Fatalf("CreateScanRun(%s) error = %v", run.ID, err)
		}
	}

	receiptJSON := testSecurityTargetReceiptJSON(t, testTargetHeadOID, testSnapshotDigest, testSHA256DigestB, nil)
	receipt := &store.SecurityTargetReceipt{
		ID:             "target_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Namespace:      sealingWithActive.Namespace,
		RepositoryScan: sealingWithActive.RepositoryScan,
		ScanRunID:      sealingWithActive.ID,
		RunUID:         sealingWithActive.RunUID,
		TargetID:       "repo_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadSHA:        testTargetHeadOID,
		ObjectFormat:   testTargetObjectFmt,
		SnapshotDigest: testSnapshotDigest,
		TreeDigest:     testSHA256DigestB,
		ReceiptJSON:    receiptJSON,
		InventoryJSON:  json.RawMessage(`{"files":["main.go"]}`),
	}
	if created, err := s.SaveSecurityTargetReceipt(ctx, receipt); err != nil || !created {
		_ = db.Close()
		t.Fatalf("SaveSecurityTargetReceipt() = (%v, %v)", created, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	migratedDB, err := NewDB(dbPath)
	if err != nil {
		t.Fatalf("NewDB(migrate duplicate sealing groups) error = %v", err)
	}
	migrated := NewStore(migratedDB, dbPath)

	assertRun := func(id, wantPhase string, wantStatus store.BundleStatus, wantError string, wantCompleted bool) *store.ScanRun {
		t.Helper()
		run, err := migrated.GetScanRun(ctx, "ns-migration", id)
		if err != nil {
			t.Fatalf("GetScanRun(%s) error = %v", id, err)
		}
		if run.Phase != wantPhase || run.Quality.BundleStatus != wantStatus || run.ErrorMessage != wantError {
			t.Fatalf("GetScanRun(%s) = phase %q, bundle %q, error %q; want %q, %q, %q",
				id, run.Phase, run.Quality.BundleStatus, run.ErrorMessage, wantPhase, wantStatus, wantError)
		}
		if (run.CompletedAt != nil) != wantCompleted {
			t.Fatalf("GetScanRun(%s).CompletedAt = %v, want completed=%v", id, run.CompletedAt, wantCompleted)
		}
		if run.Summary != "preserved summary for "+id {
			t.Fatalf("GetScanRun(%s).Summary = %q, want preserved summary", id, run.Summary)
		}
		return run
	}
	assertRun(running.ID, "running", store.BundleStatusNotStarted, "", false)
	failedWithAudit := assertRun(sealingWithActive.ID, "succeeded", store.BundleStatusFailed,
		"existing audit detail; "+duplicateSealingMigrationMessage, true)
	assertRun(firstSealing.ID, "succeeded", store.BundleStatusFailed, duplicateSealingMigrationMessage, true)
	assertRun(secondSealing.ID, "failed", store.BundleStatusFailed, duplicateSealingMigrationMessage, true)
	assertRun(uniqueSealing.ID, "succeeded", store.BundleStatusSealing, "", false)

	if got, err := migrated.GetSecurityTargetReceipt(ctx, receipt.Namespace, receipt.ID); err != nil || got.ScanRunID != sealingWithActive.ID {
		t.Fatalf("GetSecurityTargetReceipt(after migration) = (%#v, %v), want preserved receipt", got, err)
	}
	var runCount int
	if err := migratedDB.QueryRow(`SELECT COUNT(*) FROM security_scan_runs WHERE namespace = 'ns-migration'`).Scan(&runCount); err != nil {
		t.Fatalf("count migrated scan runs: %v", err)
	}
	if runCount != 5 {
		t.Fatalf("migrated scan run count = %d, want 5", runCount)
	}

	conflict := activeRepositoryMigrationRun("run-conflict", 6, "request-f", "pending", store.BundleStatusNotStarted, started.Add(5*time.Minute))
	conflict.RepositoryScan = uniqueSealing.RepositoryScan
	if err := migrated.CreateScanRun(ctx, conflict); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("CreateScanRun(conflicting with retained sealing run) error = %v, want ErrConflict", err)
	}
	if err := migratedDB.Close(); err != nil {
		t.Fatalf("close migrated database: %v", err)
	}

	reopenedDB, err := NewDB(dbPath)
	if err != nil {
		t.Fatalf("NewDB(second migration) error = %v", err)
	}
	defer reopenedDB.Close() //nolint:errcheck
	reopened := NewStore(reopenedDB, dbPath)
	replayed, err := reopened.GetScanRun(ctx, failedWithAudit.Namespace, failedWithAudit.ID)
	if err != nil {
		t.Fatalf("GetScanRun(after second migration) error = %v", err)
	}
	if replayed.ErrorMessage != failedWithAudit.ErrorMessage {
		t.Fatalf("second migration changed audit message from %q to %q", failedWithAudit.ErrorMessage, replayed.ErrorMessage)
	}
}

func TestActiveRepositoryIndexMigrationRejectsUnresolvedNonSealingDuplicates(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "duplicate-active.db")
	db, err := NewDB(dbPath)
	if err != nil {
		t.Fatalf("NewDB() error = %v", err)
	}
	s := NewStore(db, dbPath)
	if _, err := db.Exec(`DROP INDEX idx_security_scan_runs_active_repository`); err != nil {
		_ = db.Close()
		t.Fatalf("drop active-repository index: %v", err)
	}
	started := time.Date(2026, time.August, 2, 9, 0, 0, 0, time.UTC)
	for i, phase := range []string{"pending", "running"} {
		run := activeRepositoryMigrationRun("duplicate-active-"+phase, 20+i, fmt.Sprintf("request-duplicate-active-%d", i), phase, store.BundleStatusNotStarted, started.Add(time.Duration(i)*time.Minute))
		if err := s.CreateScanRun(context.Background(), run); err != nil {
			_ = db.Close()
			t.Fatalf("CreateScanRun(%s) error = %v", phase, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	if reopened, err := NewDB(dbPath); err == nil {
		_ = reopened.Close()
		t.Fatal("NewDB() error = nil, want unresolved duplicate preflight failure")
	} else if !strings.Contains(err.Error(), "duplicate active repository groups remain") {
		t.Fatalf("NewDB() error = %v, want duplicate-group preflight failure", err)
	}
}

func integrityTestStageReceipt(id, stage string) *store.StageReceipt {
	started := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	completed := started.Add(time.Minute)
	return &store.StageReceipt{
		ID:                         id,
		Namespace:                  "ns1",
		RepositoryScan:             "repo1",
		ScanRunID:                  "scan_legacy",
		RunUID:                     "run_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Stage:                      stage,
		ScopeKind:                  "slice",
		ScopeID:                    "slice_api",
		ExpectedTargetSHA:          strings.Repeat("a", 40),
		ObservedTargetSHA:          strings.Repeat("a", 40),
		TargetReceiptID:            "target_receipt_1",
		AttestationLevel:           store.AnalysisAttestationDelivered,
		ScannerPolicyDigest:        integrityTestDigest("policy"),
		NormalizedInputDigest:      integrityTestDigest("input"),
		SourceArtifactName:         "security-findings.v2.json",
		SourceArtifactMediaType:    "application/json",
		SourceArtifactSize:         42,
		SourceArtifactGeneration:   1,
		SourceArtifactDigest:       integrityTestDigest("artifact"),
		ControllerIngestionVersion: "security-ingestion-v1",
		NormalizedOutputDigest:     integrityTestDigest("output"),
		Disposition:                store.StageReceiptAccepted,
		StartedAt:                  started,
		IngestedAt:                 completed,
		CompletedAt:                &completed,
		Provenance: store.ExecutionProvenance{
			Kind: store.ExecutionProvenanceKubernetes,
			Kubernetes: &store.KubernetesExecutionProvenance{
				TaskName: "review-task",
				TaskUID:  "task-uid-1",
				Attempt:  1,
				JobName:  "review-job",
				JobUID:   "job-uid-1",
				PodName:  "review-pod",
				PodUID:   "pod-uid-1",
			},
		},
	}
}

func integrityTestSaveTargetReceipt(t *testing.T, s *Store) {
	t.Helper()
	head := strings.Repeat("a", 40)
	if err := s.CreateScanRun(context.Background(), &store.ScanRun{
		ID: "scan_legacy", RunUID: "run_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Namespace: "ns1", RepositoryScan: "repo1", TaskName: "review-task", Mode: "manual", Phase: "succeeded",
		HeadCommit: head, TargetReceiptID: "target_receipt_1", StartedAt: time.Now().UTC(), Quality: store.LegacyScanQuality(),
	}); err != nil && !errors.Is(err, store.ErrConflict) {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	if _, err := s.SaveSecurityTargetReceipt(context.Background(), &store.SecurityTargetReceipt{
		ID: "target_receipt_1", Namespace: "ns1", RepositoryScan: "repo1", ScanRunID: "scan_legacy",
		RunUID:   "run_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		TargetID: "target_test_1", HeadSHA: head, ObjectFormat: "sha1",
		SnapshotDigest: integrityTestDigest("snapshot"), TreeDigest: integrityTestDigest("tree"),
		ReceiptJSON: json.RawMessage(`{"headOID":"` + head + `"}`),
	}); err != nil {
		t.Fatalf("SaveSecurityTargetReceipt() error = %v", err)
	}
}

func integrityTestObservation(id, receiptID, semanticFingerprint string) *store.FindingObservation {
	return &store.FindingObservation{
		ID:                       id,
		Namespace:                "ns1",
		RepositoryScan:           "repo1",
		ScanRunID:                "scan_legacy",
		RunUID:                   "run_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		StageReceiptID:           receiptID,
		TargetReceiptID:          "target_receipt_1",
		SliceID:                  "slice_api",
		CandidateKey:             "candidate_1",
		SourceArtifactName:       "security-findings.v2.json",
		SourceArtifactGeneration: 1,
		SourceArtifactDigest:     integrityTestDigest("artifact"),
		PolicyDigest:             integrityTestDigest("policy"),
		Ordinal:                  0,
		Disposition:              store.FindingObservationAccepted,
		RuleID:                   "command-injection",
		IdentityAnchor:           "handler-exec",
		IdentityInstance:         "instance-1",
		IdentityQuality:          store.IdentityQualityCanonical,
		IdentityAlgorithmVersion: semanticIdentityAlgorithmV1,
		SemanticFingerprint:      semanticFingerprint,
		NormalizedPayload:        json.RawMessage(`{"title":"command injection"}`),
	}
}

func TestCanonicalObservationRecomputesSemanticIdentityFromTargetReceipt(t *testing.T) {
	s := setupTestStore(t)
	integrityTestSaveTargetReceipt(t, s)
	ctx := context.Background()
	receipt := integrityTestStageReceipt("receipt_canonical_recompute", "review")
	if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	observation := integrityTestObservation("observation_canonical_recompute", receipt.ID, integrityTestDigest("caller-controlled"))
	created, err := s.AcceptFindingObservation(ctx, observation)
	if err != nil || !created {
		t.Fatalf("AcceptFindingObservation() = (%v, %v)", created, err)
	}
	expected, ok := securityidentity.DeriveCanonicalSemanticIdentity(
		"target_test_1", observation.RuleID, observation.IdentityAnchor, observation.IdentityInstance,
	)
	if !ok {
		t.Fatal("DeriveCanonicalSemanticIdentity() failed")
	}
	if observation.SemanticFingerprint != expected.SemanticFingerprint ||
		observation.SemanticFingerprint == integrityTestDigest("caller-controlled") {
		t.Fatalf("canonical semantic fingerprint = %q, want recomputed %q", observation.SemanticFingerprint, expected.SemanticFingerprint)
	}
	stored, err := s.GetFindingObservation(ctx, observation.Namespace, observation.ID)
	if err != nil || stored.SemanticFingerprint != expected.SemanticFingerprint {
		t.Fatalf("GetFindingObservation() = (%#v, %v)", stored, err)
	}
}

func TestCanonicalObservationRequiresTrustedTargetReceipt(t *testing.T) {
	s := setupTestStore(t)
	observation := integrityTestObservation("observation_canonical_missing_target", "receipt_missing_target", integrityTestDigest("caller-controlled"))
	observation.TargetReceiptID = "target_receipt_missing"
	if _, err := s.AcceptFindingObservation(context.Background(), observation); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("AcceptFindingObservation() error = %v, want ErrValidation", err)
	}
}

func integrityTestFinalization(observation *store.FindingObservation, occurrenceID, publicID, semanticID string) *store.FindingOccurrenceFinalization {
	return &store.FindingOccurrenceFinalization{
		Occurrence: store.FindingOccurrence{
			ID:                       occurrenceID,
			Namespace:                observation.Namespace,
			RepositoryScan:           observation.RepositoryScan,
			ScanRunID:                observation.ScanRunID,
			RunUID:                   observation.RunUID,
			PublicFindingID:          publicID,
			SemanticFindingID:        semanticID,
			SemanticFingerprint:      observation.SemanticFingerprint,
			IdentityQuality:          observation.IdentityQuality,
			IdentityAlgorithmVersion: observation.IdentityAlgorithmVersion,
			LegacyFingerprint:        observation.LegacyFingerprint,
			RuleID:                   observation.RuleID,
			IdentityAnchor:           observation.IdentityAnchor,
			IdentityInstance:         observation.IdentityInstance,
			TargetReceiptID:          observation.TargetReceiptID,
			TargetSHA:                strings.Repeat("a", 40),
			DiscoveryPayload:         json.RawMessage(`{"summary":"finalized discovery"}`),
		},
		ObservationLinks: []store.FindingOccurrenceObservation{{
			ObservationID: observation.ID,
			Relationship:  store.FindingObservationRelationshipContributor,
			Ordinal:       0,
		}},
		Projection: store.Finding{
			ID:               publicID,
			Namespace:        observation.Namespace,
			RepositoryScan:   observation.RepositoryScan,
			ScanRunID:        observation.ScanRunID,
			Fingerprint:      "compat-fingerprint-1",
			Title:            "Command injection",
			Summary:          "Untrusted input reaches process execution.",
			Severity:         "high",
			Confidence:       "high",
			ValidationStatus: "unvalidated",
			State:            "open",
			IdentityQuality:  observation.IdentityQuality,
			Evidence: []store.FindingEvidenceRef{{
				Kind:          "file",
				Path:          "internal/api/handler.go",
				StartLine:     10,
				EndLine:       12,
				ContentSHA256: integrityTestRawDigest("file contents"),
				ContentSize:   128,
			}},
		},
	}
}

func TestSecurityImmutableContentRejectsCredentialShapes(t *testing.T) {
	tests := map[string]string{
		"authorization header": strings.Join([]string{"Author", "ization: Bearer value-for-redaction"}, ""),
		"transaction header":   strings.Join([]string{"Txn", "-To", "ken: value-for-redaction"}, ""),
		"sensitive assignment": strings.Join([]string{"api", "_key=", "sk", "-abcdefghijklmnopqrstuvwxyz123456"}, ""),
		"github token":         strings.Join([]string{"github", "_pat_", "abcdefghijklmnopqrstuvwxyz1234567890"}, ""),
		"jwt":                  strings.Join([]string{"eyJhbGciOiJSUzI1NiJ9", "eyJzdWIiOiJ3b3JrbG9hZCJ9", "signaturevalue1234567890"}, "."),
		"credentialed url":     strings.Join([]string{"https://username:", "pass", "word@example.com/org/repo.git"}, ""),
	}
	for name, value := range tests {
		t.Run(name+"_text", func(t *testing.T) {
			if err := validateSecurityText(value, "summary", maxSecuritySummaryBytes, true); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("validateSecurityText() error = %v, want ErrValidation", err)
			}
		})
		t.Run(name+"_payload", func(t *testing.T) {
			payload, err := json.Marshal(map[string]string{"summary": value})
			if err != nil {
				t.Fatal(err)
			}
			normalized, digest, err := normalizeSecurityPayload(payload, securityDigestBytes(payload), true, "normalizedPayload")
			if err != nil {
				t.Fatalf("normalizeSecurityPayload() error = %v", err)
			}
			if strings.Contains(string(normalized), value) || !strings.Contains(string(normalized), "[REDACTED]") {
				t.Fatalf("normalized payload retained credential: %s", normalized)
			}
			if digest != securityDigestBytes(normalized) {
				t.Fatalf("digest = %q, want redacted payload digest", digest)
			}
		})
	}

	receipt := integrityTestStageReceipt("receipt-redacted", "review")
	receipt.Disposition = store.StageReceiptRejected
	receipt.ReasonCode = "producer_error"
	receipt.Reason = tests["authorization header"]
	if err := normalizeStageReceipt(receipt); err != nil {
		t.Fatalf("normalizeStageReceipt() error = %v", err)
	}
	if strings.Contains(receipt.Reason, "value-for-redaction") {
		t.Fatalf("stage receipt retained credential: %q", receipt.Reason)
	}

	decision := &store.FindingDecision{
		ID: "decision-redacted", Namespace: "ns1", RepositoryScan: "repo1", PublicFindingID: "finding-1",
		Scope: store.FindingDecisionOccurrence, OccurrenceID: "occurrence-1",
		Action: store.FindingDecisionCloseFalsePositive, ReasonCode: "not_reproducible",
		Reason: tests["transaction header"], ActorSubject: "tester", AuthenticationSource: "test", Source: "api",
	}
	if err := normalizeFindingDecision(decision); err != nil {
		t.Fatalf("normalizeFindingDecision() error = %v", err)
	}
	if strings.Contains(decision.Reason, "value-for-redaction") {
		t.Fatalf("finding decision retained credential: %q", decision.Reason)
	}

	const safe = "Review token-based authorization boundaries without embedding credentials."
	if err := validateSecurityText(safe, "summary", maxSecuritySummaryBytes, true); err != nil {
		t.Fatalf("validateSecurityText(safe) error = %v", err)
	}
	payload, err := json.Marshal(map[string]string{"summary": safe})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := normalizeSecurityPayload(payload, "", true, "normalizedPayload"); err != nil {
		t.Fatalf("normalizeSecurityPayload(safe) error = %v", err)
	}
}

func TestNormalizeSecurityPayloadCanonicalizesAndRedactsEscapedCredentials(t *testing.T) {
	payload := json.RawMessage(`{
		"z":"safe",
		"nested":{"Authorizatio\u006e":"Bearer value-for-redaction"},
		"note":"https://username:password\u0040example.com/org/repo.git",
		"api_k\u0065y":"plain-secret"
	}`)
	normalized, digest, err := normalizeSecurityPayload(payload, securityDigestBytes(payload), true, "normalizedPayload")
	if err != nil {
		t.Fatalf("normalizeSecurityPayload() error = %v", err)
	}
	wantBytes, err := json.Marshal(map[string]any{
		strings.Join([]string{"api", "_key"}, ""): "[REDACTED]",
		"nested": map[string]string{
			strings.Join([]string{"Author", "ization"}, ""): "[REDACTED]",
		},
		"note": "https://[REDACTED]@example.com/org/repo.git",
		"z":    "safe",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := json.RawMessage(wantBytes)
	if string(normalized) != string(want) {
		t.Fatalf("normalized payload = %s, want %s", normalized, want)
	}
	if digest != securityDigestBytes(want) {
		t.Fatalf("digest = %q, want canonical payload digest %q", digest, securityDigestBytes(want))
	}
}

func TestNormalizeSecurityPayloadRedactsEntireSensitiveFieldValues(t *testing.T) {
	payload := json.RawMessage(`{
		"api_key":123,
		"token":true,
		"secret":null,
		"credentials":[1,false,null,{"ttl":300}],
		"Authorization":{"scheme":"Bearer","value":123},
		"safe":{"count":3,"enabled":true,"items":[1,false,null]}
	}`)
	normalized, digest, err := normalizeSecurityPayload(payload, securityDigestBytes(payload), true, "normalizedPayload")
	if err != nil {
		t.Fatalf("normalizeSecurityPayload() error = %v", err)
	}
	redacted := "[REDACTED]"
	wantBytes, err := json.Marshal(map[string]any{
		strings.Join([]string{"api", "_key"}, ""):       redacted,
		strings.Join([]string{"to", "ken"}, ""):         redacted,
		strings.Join([]string{"se", "cret"}, ""):        redacted,
		strings.Join([]string{"creden", "tials"}, ""):   redacted,
		strings.Join([]string{"Author", "ization"}, ""): redacted,
		"safe": map[string]any{
			"count":   3,
			"enabled": true,
			"items":   []any{1, false, nil},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := json.RawMessage(wantBytes)
	if string(normalized) != string(want) {
		t.Fatalf("normalized payload = %s, want %s", normalized, want)
	}
	if digest != securityDigestBytes(want) {
		t.Fatalf("digest = %q, want redacted payload digest %q", digest, securityDigestBytes(want))
	}
}

func TestNormalizeSecurityPayloadCanonicalizesSafeJSON(t *testing.T) {
	payload := json.RawMessage(" { \"z\": 1, \"a\": [\"safe\", true] } ")
	normalized, digest, err := normalizeSecurityPayload(payload, securityDigestBytes(payload), true, "normalizedPayload")
	if err != nil {
		t.Fatalf("normalizeSecurityPayload() error = %v", err)
	}
	want := json.RawMessage(`{"a":["safe",true],"z":1}`)
	if string(normalized) != string(want) {
		t.Fatalf("normalized payload = %s, want %s", normalized, want)
	}
	if digest != securityDigestBytes(want) {
		t.Fatalf("digest = %q, want canonical payload digest %q", digest, securityDigestBytes(want))
	}
}

func TestNormalizeSecurityPayloadRejectsInvalidUTF8(t *testing.T) {
	payload := json.RawMessage([]byte{'{', '"', 'n', 'o', 't', 'e', '"', ':', '"', 0xff, '"', '}'})
	if !json.Valid(payload) {
		t.Fatal("test payload must remain JSON-valid under encoding/json")
	}
	if _, _, err := normalizeSecurityPayload(payload, securityDigestBytes(payload), true, "normalizedPayload"); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("normalizeSecurityPayload() error = %v, want ErrValidation", err)
	}
}

func TestScanRunIntegrityFieldsAndImmutableTarget(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	run := &store.ScanRun{
		ID:                       "scan_legacy",
		RunUID:                   "run_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Namespace:                "ns1",
		RepositoryScan:           "repo1",
		RepositoryScanUID:        "repository-scan-uid-1",
		RepositoryScanGeneration: 7,
		TaskName:                 "task-1",
		Mode:                     "initial",
		Phase:                    "pending",
		RequestIdempotencyKey:    "request-key-1",
		ResolvedTargetKey:        "github:repository-id:123@aaaaaaaa",
		TargetReceiptID:          "target_receipt_1",
		Quality: store.ScanQuality{
			SchemaVersion:            store.SecurityQualitySchemaVersion,
			InventoryCoverageStatus:  store.CoverageStatusComplete,
			CandidateCoverageStatus:  store.CoverageStatusPending,
			CoverageStatus:           store.CoverageStatusPending,
			ValidationScope:          store.ValidationScopeAll,
			ValidationExecution:      store.QualityExecutionPending,
			AttackPathExecution:      store.QualityExecutionNotStarted,
			AnalysisAttestationLevel: store.AnalysisAttestationDelivered,
			TargetVerification:       store.TargetVerificationVerified,
			BundleStatus:             store.BundleStatusNotStarted,
			AuthorizationStatus:      store.AuthorizationStatusVerified,
			IsolationStatus:          store.IsolationStatusHardened,
			ReasonCodes:              []string{"candidate-pending"},
		},
	}
	if err := s.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	got, err := s.GetScanRun(ctx, "ns1", run.ID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if got.RunUID != run.RunUID || got.RepositoryScanUID != run.RepositoryScanUID || got.RequestIdempotencyKey != run.RequestIdempotencyKey {
		t.Fatalf("integrity fields = %#v", got)
	}
	if got.Quality.TargetVerification != store.TargetVerificationVerified || got.Quality.IsolationStatus != store.IsolationStatusHardened {
		t.Fatalf("quality = %#v", got.Quality)
	}

	got.ResolvedTargetKey = "github:repository-id:123@bbbbbbbb"
	if err := s.UpdateScanRun(ctx, got); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("UpdateScanRun() error = %v, want ErrConflict", err)
	}
	unchanged, err := s.GetScanRun(ctx, "ns1", run.ID)
	if err != nil {
		t.Fatalf("GetScanRun(after conflict) error = %v", err)
	}
	if unchanged.ResolvedTargetKey != run.ResolvedTargetKey {
		t.Fatalf("ResolvedTargetKey = %q, want %q", unchanged.ResolvedTargetKey, run.ResolvedTargetKey)
	}
}

func TestStageReceiptReplayIdempotencyAndConflict(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	receipt := integrityTestStageReceipt("receipt_review_1", "review")
	created, err := s.AppendStageReceipt(ctx, receipt)
	if err != nil || !created {
		t.Fatalf("AppendStageReceipt(first) = (%v, %v), want (true, nil)", created, err)
	}
	created, err = s.AppendStageReceipt(ctx, receipt)
	if err != nil || created {
		t.Fatalf("AppendStageReceipt(replay) = (%v, %v), want (false, nil)", created, err)
	}
	conflict := *receipt
	conflict.NormalizedOutputDigest = integrityTestDigest("different output")
	if _, err := s.AppendStageReceipt(ctx, &conflict); !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("AppendStageReceipt(conflict) error = %v, want ErrDuplicateMismatch", err)
	}
}

func TestStageReceiptAndObservationListLimitsAreStoreBounded(t *testing.T) {
	s := setupTestStore(t)
	integrityTestSaveTargetReceipt(t, s)
	ctx := context.Background()
	receipt := integrityTestStageReceipt("receipt_bounded_000", "review")
	if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= findingHistoryMaxLimit; i++ {
		item := integrityTestStageReceipt(fmt.Sprintf("receipt_bounded_%03d", i), "review")
		if _, err := s.AppendStageReceipt(ctx, item); err != nil {
			t.Fatalf("AppendStageReceipt(%d) error = %v", i, err)
		}
	}
	for i := 0; i <= findingHistoryMaxLimit; i++ {
		observation := integrityTestObservation(
			fmt.Sprintf("observation_bounded_%03d", i),
			receipt.ID,
			integrityTestDigest(fmt.Sprintf("bounded-semantic-%d", i)),
		)
		observation.Ordinal = i
		observation.CandidateKey = fmt.Sprintf("candidate_bounded_%03d", i)
		observation.IdentityInstance = fmt.Sprintf("instance_bounded_%03d", i)
		if _, err := s.AcceptFindingObservation(ctx, observation); err != nil {
			t.Fatalf("AcceptFindingObservation(%d) error = %v", i, err)
		}
	}

	receipts, next, err := s.ListStageReceipts(ctx, store.StageReceiptFilter{
		Namespace: "ns1",
		Limit:     findingHistoryMaxLimit + 100,
	})
	if err != nil {
		t.Fatalf("ListStageReceipts() error = %v", err)
	}
	if len(receipts) != findingHistoryMaxLimit || next != fmt.Sprint(findingHistoryMaxLimit) {
		t.Fatalf("ListStageReceipts() = (%d, %q), want (%d, %q)",
			len(receipts), next, findingHistoryMaxLimit, fmt.Sprint(findingHistoryMaxLimit))
	}

	observations, next, err := s.ListFindingObservations(ctx, store.FindingObservationFilter{
		Namespace: "ns1",
		Limit:     findingHistoryMaxLimit + 100,
	})
	if err != nil {
		t.Fatalf("ListFindingObservations() error = %v", err)
	}
	if len(observations) != findingHistoryMaxLimit || next != fmt.Sprint(findingHistoryMaxLimit) {
		t.Fatalf("ListFindingObservations() = (%d, %q), want (%d, %q)",
			len(observations), next, findingHistoryMaxLimit, fmt.Sprint(findingHistoryMaxLimit))
	}
}

func TestFindingHistoryTransactionsAndProjection(t *testing.T) {
	s := setupTestStore(t)
	integrityTestSaveTargetReceipt(t, s)
	ctx := context.Background()
	receipt := integrityTestStageReceipt("receipt_review_1", "review")
	if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
		t.Fatalf("AppendStageReceipt() error = %v", err)
	}
	semantic := integrityTestDigest("semantic-command-injection")
	observation := integrityTestObservation("observation_1", receipt.ID, semantic)
	created, err := s.AcceptFindingObservation(ctx, observation)
	if err != nil || !created {
		t.Fatalf("AcceptFindingObservation(first) = (%v, %v)", created, err)
	}
	created, err = s.AcceptFindingObservation(ctx, observation)
	if err != nil || created {
		t.Fatalf("AcceptFindingObservation(replay) = (%v, %v)", created, err)
	}
	conflictingObservation := *observation
	conflictingObservation.NormalizedPayload = json.RawMessage(`{"title":"different"}`)
	conflictingObservation.PayloadDigest = ""
	if _, err := s.AcceptFindingObservation(ctx, &conflictingObservation); !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("AcceptFindingObservation(conflict) error = %v, want ErrDuplicateMismatch", err)
	}

	finalization := integrityTestFinalization(observation, "occurrence_1", "fnd_public_1", "semantic_finding_1")
	finalization.Projection.State = "suppressed"
	finalization.Projection.ValidationStatus = assessmentStatusValidated
	finalization.Projection.ValidationJSON = validatedStatusJSON
	created, err = s.FinalizeFindingOccurrence(ctx, finalization)
	if err != nil || !created {
		t.Fatalf("FinalizeFindingOccurrence(first) = (%v, %v)", created, err)
	}
	created, err = s.FinalizeFindingOccurrence(ctx, finalization)
	if err != nil || created {
		t.Fatalf("FinalizeFindingOccurrence(replay) = (%v, %v)", created, err)
	}
	occurrence, err := s.GetFindingOccurrence(ctx, "ns1", "occurrence_1")
	if err != nil {
		t.Fatalf("GetFindingOccurrence() error = %v", err)
	}
	if len(occurrence.ObservationLinks) != 1 || occurrence.ObservationLinks[0].ObservationID != observation.ID {
		t.Fatalf("occurrence links = %#v", occurrence.ObservationLinks)
	}
	alias, err := s.GetFindingAlias(ctx, "ns1", "repo1", observation.SemanticFingerprint)
	if err != nil || alias.PublicFindingID != "fnd_public_1" {
		t.Fatalf("GetFindingAlias() = (%#v, %v)", alias, err)
	}
	finding, err := s.GetFinding(ctx, "ns1", "fnd_public_1")
	if err != nil {
		t.Fatalf("GetFinding() error = %v", err)
	}
	if finding.CurrentOccurrenceID != "occurrence_1" || finding.IdentityQuality != store.IdentityQualityCanonical ||
		finding.HistoryStatus != store.FindingHistoryCanonical {
		t.Fatalf("finding identity projection = %#v", finding)
	}
	if finding.State != findingProjectionStateOpen || finding.ValidationStatus != assessmentStatusUnvalidated || finding.ValidationJSON != "" {
		t.Fatalf("assessment/decision-owned projection fields were trusted: %#v", finding)
	}
	if len(finding.Evidence) != 1 || finding.Evidence[0].ContentSHA256 != integrityTestRawDigest("file contents") ||
		finding.Evidence[0].ContentSize != 128 {
		t.Fatalf("finding evidence = %#v", finding.Evidence)
	}

	semantic2 := integrityTestDigest("semantic-second")
	observation2 := integrityTestObservation("observation_2", receipt.ID, semantic2)
	observation2.Ordinal = 1
	observation2.CandidateKey = "candidate_2"
	observation2.IdentityInstance = "instance-2"
	if _, err := s.AcceptFindingObservation(ctx, observation2); err != nil {
		t.Fatalf("AcceptFindingObservation(second) error = %v", err)
	}
	rollback := integrityTestFinalization(observation2, "occurrence_rollback", "fnd_other", "semantic_finding_2")
	if _, err := s.FinalizeFindingOccurrence(ctx, rollback); !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("FinalizeFindingOccurrence(rollback) error = %v, want ErrDuplicateMismatch", err)
	}
	if _, err := s.GetFindingOccurrence(ctx, "ns1", "occurrence_rollback"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetFindingOccurrence(rolled back) error = %v, want ErrNotFound", err)
	}
	if _, err := s.GetFindingAlias(ctx, "ns1", "repo1", observation2.SemanticFingerprint); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetFindingAlias(rolled back) error = %v, want ErrNotFound", err)
	}
}

func TestProducerProposedOccurrenceDoesNotCreateCrossRunAlias(t *testing.T) {
	s := setupTestStore(t)
	integrityTestSaveTargetReceipt(t, s)
	ctx := context.Background()
	receipt := integrityTestStageReceipt("receipt_review_proposed", "review")
	if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	semantic := integrityTestDigest("producer-proposed-semantic")
	observation := integrityTestObservation("observation_proposed", receipt.ID, semantic)
	observation.IdentityQuality = store.IdentityQualityProducerProposed
	if _, err := s.AcceptFindingObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	finalization := integrityTestFinalization(observation, "occurrence_proposed", "fnd_proposed", "semantic_proposed")
	finalization.Occurrence.IdentityQuality = store.IdentityQualityProducerProposed
	finalization.Projection.IdentityQuality = store.IdentityQualityProducerProposed
	finalization.Projection.Fingerprint = "compat-fingerprint-proposed"
	finalization.Occurrence.DiscoveryPayload = append(json.RawMessage(nil), observation.NormalizedPayload...)
	finalization.Occurrence.PayloadDigest = observation.PayloadDigest
	if _, err := s.FinalizeFindingOccurrence(ctx, finalization); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetFindingAlias(ctx, observation.Namespace, observation.RepositoryScan, semantic); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetFindingAlias(producer-proposed) error = %v, want ErrNotFound", err)
	}
}

func TestProducerProposedObservationCannotBePromotedToCanonicalAlias(t *testing.T) {
	s := setupTestStore(t)
	integrityTestSaveTargetReceipt(t, s)
	ctx := context.Background()
	receipt := integrityTestStageReceipt("receipt_review_promotion", "review")
	if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	semantic := integrityTestDigest("producer-proposed-promotion")
	observation := integrityTestObservation("observation_promotion", receipt.ID, semantic)
	observation.IdentityQuality = store.IdentityQualityProducerProposed
	if _, err := s.AcceptFindingObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	finalization := integrityTestFinalization(observation, "occurrence_promotion", "fnd_promotion", "semantic_promotion")
	finalization.Occurrence.IdentityQuality = store.IdentityQualityCanonical
	finalization.Projection.IdentityQuality = store.IdentityQualityCanonical
	if _, err := s.FinalizeFindingOccurrence(ctx, finalization); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("FinalizeFindingOccurrence(promote proposed) error = %v, want ErrValidation", err)
	}
	if _, err := s.GetFindingAlias(ctx, observation.Namespace, observation.RepositoryScan, semantic); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetFindingAlias(promote proposed) error = %v, want ErrNotFound", err)
	}
}

//nolint:gocyclo // integration test exercises the full append/project transaction sequence
//nolint:gocyclo // integration-style reducer coverage intentionally exercises the full lifecycle
func TestAssessmentAndDecisionProjectionTransactions(t *testing.T) {
	s := setupTestStore(t)
	integrityTestSaveTargetReceipt(t, s)
	ctx := context.Background()
	reviewReceipt := integrityTestStageReceipt("receipt_review_1", "review")
	validationReceipt := integrityTestStageReceipt("receipt_validation_1", "validation")
	validationReceipt.ScopeKind = "occurrence"
	validationReceipt.ScopeID = "occurrence_1"
	validationReceipt.NormalizedOutputDigest = integrityTestDigest(`{"status":"validated","summary":"confirmed"}`)
	for _, receipt := range []*store.StageReceipt{reviewReceipt, validationReceipt} {
		if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
			t.Fatalf("AppendStageReceipt(%s) error = %v", receipt.ID, err)
		}
	}
	observation := integrityTestObservation("observation_1", reviewReceipt.ID, integrityTestDigest("semantic"))
	if _, err := s.AcceptFindingObservation(ctx, observation); err != nil {
		t.Fatalf("AcceptFindingObservation() error = %v", err)
	}
	finalization := integrityTestFinalization(observation, "occurrence_1", "fnd_public_1", "semantic_finding_1")
	if _, err := s.FinalizeFindingOccurrence(ctx, finalization); err != nil {
		t.Fatalf("FinalizeFindingOccurrence() error = %v", err)
	}

	assessment := &store.FindingAssessment{
		ID:                         "assessment_validation_1",
		Namespace:                  "ns1",
		RepositoryScan:             "repo1",
		ScanRunID:                  observation.ScanRunID,
		RunUID:                     observation.RunUID,
		OccurrenceID:               "occurrence_1",
		PublicFindingID:            "fnd_public_1",
		Kind:                       store.FindingAssessmentValidation,
		StageReceiptID:             validationReceipt.ID,
		TargetReceiptID:            "target_receipt_1",
		TargetSHA:                  strings.Repeat("a", 40),
		Method:                     "static",
		Outcome:                    assessmentOutcomeConfirmed,
		Summary:                    "The path is reachable.",
		EvidenceReceiptIDs:         []string{reviewReceipt.ID},
		NormalizedPayload:          json.RawMessage(`{"status":"validated","summary":"confirmed"}`),
		ProjectionValidationStatus: assessmentStatusValidated,
	}
	created, err := s.RecordFindingAssessment(ctx, assessment)
	if err != nil || !created {
		t.Fatalf("RecordFindingAssessment(first) = (%v, %v)", created, err)
	}
	created, err = s.RecordFindingAssessment(ctx, assessment)
	if err != nil || created {
		t.Fatalf("RecordFindingAssessment(replay) = (%v, %v)", created, err)
	}
	finding, err := s.GetFinding(ctx, "ns1", "fnd_public_1")
	if err != nil || finding.ValidationStatus != assessmentStatusValidated {
		t.Fatalf("validation projection = (%#v, %v)", finding, err)
	}

	closeDecision := &store.FindingDecision{
		ID:                      "decision_close_1",
		Namespace:               "ns1",
		RepositoryScan:          "repo1",
		PublicFindingID:         "fnd_public_1",
		Scope:                   store.FindingDecisionOccurrence,
		OccurrenceID:            "occurrence_1",
		Action:                  store.FindingDecisionCloseFalsePositive,
		ReasonCode:              "not-reachable",
		Reason:                  "The controller-owned validation evidence disproves reachability.",
		EvidenceReceiptIDs:      []string{validationReceipt.ID},
		ExpectedDecisionVersion: 0,
		ActorSubject:            "user:reviewer",
		ActorIssuer:             "https://issuer.example",
		AuthenticationSource:    "oidc",
		Source:                  "api",
	}
	closed, err := s.AppendFindingDecision(ctx, closeDecision)
	if err != nil || closed.DecisionVersion != 1 {
		t.Fatalf("AppendFindingDecision(close) = (%#v, %v)", closed, err)
	}
	finding, err = s.GetFinding(ctx, "ns1", "fnd_public_1")
	if err != nil || finding.State != "false_positive" || finding.DecisionVersion != 1 {
		t.Fatalf("closed finding projection = (%#v, %v)", finding, err)
	}

	stale := *closeDecision
	stale.ID = "decision_stale"
	stale.Action = store.FindingDecisionReopen
	stale.Reason = ""
	stale.ReasonCode = ""
	stale.EvidenceReceiptIDs = nil
	if _, err := s.AppendFindingDecision(ctx, &stale); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("AppendFindingDecision(stale) error = %v, want ErrConflict", err)
	}
	if _, err := s.GetFindingDecision(ctx, "ns1", stale.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetFindingDecision(stale) error = %v, want ErrNotFound", err)
	}

	reopen := &store.FindingDecision{
		ID:                      "decision_reopen_2",
		Namespace:               "ns1",
		RepositoryScan:          "repo1",
		PublicFindingID:         "fnd_public_1",
		Scope:                   store.FindingDecisionOccurrence,
		OccurrenceID:            "occurrence_1",
		Action:                  store.FindingDecisionReopen,
		ExpectedDecisionVersion: 1,
		ActorSubject:            "user:reviewer",
		ActorIssuer:             "https://issuer.example",
		AuthenticationSource:    "oidc",
		Source:                  "api",
	}
	reopened, err := s.AppendFindingDecision(ctx, reopen)
	if err != nil || reopened.DecisionVersion != 2 {
		t.Fatalf("AppendFindingDecision(reopen) = (%#v, %v)", reopened, err)
	}
	finding, err = s.GetFinding(ctx, "ns1", "fnd_public_1")
	if err != nil || finding.State != "open" || finding.DecisionVersion != 2 {
		t.Fatalf("reopened finding projection = (%#v, %v)", finding, err)
	}

	secondRunUID := testSecondRunUID
	secondTargetReceiptID := "target_receipt_2"
	if err := s.CreateScanRun(ctx, &store.ScanRun{
		ID: testSecondScanID, RunUID: secondRunUID, Namespace: "ns1", RepositoryScan: "repo1", TaskName: "review-task-2",
		Mode: "manual", Phase: "succeeded", HeadCommit: strings.Repeat("a", 40), TargetReceiptID: secondTargetReceiptID,
		StartedAt: time.Now().UTC().Add(time.Second), Quality: store.LegacyScanQuality(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSecurityTargetReceipt(ctx, &store.SecurityTargetReceipt{
		ID: secondTargetReceiptID, Namespace: "ns1", RepositoryScan: "repo1", ScanRunID: testSecondScanID, RunUID: secondRunUID,
		TargetID: "target_test_1", HeadSHA: strings.Repeat("a", 40), ObjectFormat: "sha1",
		SnapshotDigest: integrityTestDigest("snapshot-2"), TreeDigest: integrityTestDigest("tree-2"),
		ReceiptJSON: json.RawMessage(`{"headOID":"` + strings.Repeat("a", 40) + `"}`),
	}); err != nil {
		t.Fatal(err)
	}
	reviewReceipt2 := *reviewReceipt
	reviewReceipt2.ID = "receipt_review_2"
	reviewReceipt2.ScanRunID = testSecondScanID
	reviewReceipt2.RunUID = secondRunUID
	reviewReceipt2.TargetReceiptID = secondTargetReceiptID
	reviewReceipt2.RecordDigest = ""
	if _, err := s.AppendStageReceipt(ctx, &reviewReceipt2); err != nil {
		t.Fatal(err)
	}
	observation2 := integrityTestObservation("observation_2", reviewReceipt2.ID, observation.SemanticFingerprint)
	observation2.ScanRunID = testSecondScanID
	observation2.RunUID = secondRunUID
	observation2.TargetReceiptID = secondTargetReceiptID
	observation2.LegacyFingerprint = "compat-fingerprint-1"
	if _, err := s.AcceptFindingObservation(ctx, observation2); err != nil {
		t.Fatalf("AcceptFindingObservation(second occurrence) = %v", err)
	}
	finalization2 := integrityTestFinalization(observation2, "occurrence_2", "fnd_public_1", "semantic_finding_1")
	if _, err := s.FinalizeFindingOccurrence(ctx, finalization2); err != nil {
		t.Fatalf("FinalizeFindingOccurrence(second) = %v", err)
	}
	historical := &store.FindingDecision{
		ID: "decision_historical", Namespace: "ns1", RepositoryScan: "repo1", PublicFindingID: "fnd_public_1",
		Scope: store.FindingDecisionOccurrence, OccurrenceID: "occurrence_1", Action: store.FindingDecisionCloseFixed,
		ExpectedDecisionVersion: 2, ActorSubject: "user:reviewer", AuthenticationSource: "oidc", Source: "api",
	}
	historicalResult, err := s.AppendFindingDecision(ctx, historical)
	if err != nil || historicalResult.DecisionVersion != 3 {
		t.Fatalf("AppendFindingDecision(historical) = (%#v, %v)", historicalResult, err)
	}
	finding, err = s.GetFinding(ctx, "ns1", "fnd_public_1")
	if err != nil || finding.State != "open" || finding.CurrentOccurrenceID != "occurrence_2" {
		t.Fatalf("historical decision changed current projection = (%#v, %v)", finding, err)
	}

	replayed, err := s.AppendFindingDecision(ctx, closeDecision)
	if err != nil || replayed.DecisionVersion != 1 {
		t.Fatalf("AppendFindingDecision(replay after reopen) = (%#v, %v)", replayed, err)
	}
	decisions, _, err := s.ListFindingDecisions(ctx, store.FindingDecisionFilter{
		Namespace: "ns1", PublicFindingID: "fnd_public_1", Limit: 10,
	})
	if err != nil || len(decisions) != 3 || decisions[0].DecisionVersion != 1 || decisions[1].DecisionVersion != 2 ||
		decisions[2].DecisionVersion != 3 {
		t.Fatalf("ListFindingDecisions() = (%#v, %v)", decisions, err)
	}

	otherReceipt := integrityTestStageReceipt("receipt_other_repo", "review")
	otherReceipt.RepositoryScan = "repo2"
	if _, err := s.AppendStageReceipt(ctx, otherReceipt); err != nil {
		t.Fatal(err)
	}
	badEvidence := &store.FindingDecision{
		ID: "decision_bad_evidence", Namespace: "ns1", RepositoryScan: "repo1", PublicFindingID: "fnd_public_1",
		Scope: store.FindingDecisionOccurrence, OccurrenceID: "occurrence_2", Action: store.FindingDecisionCloseFixed,
		EvidenceReceiptIDs: []string{otherReceipt.ID}, ExpectedDecisionVersion: 3,
		ActorSubject: "user:reviewer", AuthenticationSource: "oidc", Source: "api",
	}
	if _, err := s.AppendFindingDecision(ctx, badEvidence); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("cross-repository evidence error = %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE security_findings SET identity_quality = ? WHERE namespace = ? AND id = ?`,
		store.IdentityQualityProducerProposed, "ns1", "fnd_public_1"); err != nil {
		t.Fatal(err)
	}
	logical := &store.FindingDecision{
		ID: "decision_producer_feedback", Namespace: "ns1", RepositoryScan: "repo1", PublicFindingID: "fnd_public_1",
		Scope: store.FindingDecisionLogicalFinding, Action: store.FindingDecisionCloseFalsePositive,
		ReasonCode: "reviewed", Reason: "Reasoned decision.", ExpectedDecisionVersion: 3,
		Applicability: &store.FindingDecisionApplicability{
			TargetLineage: "lineage", Scope: "repo", PolicyVersion: "v1", PredicateDigest: integrityTestDigest("predicate"),
		},
		EvidenceReceiptIDs: []string{reviewReceipt2.ID},
		ActorSubject:       "user:reviewer", AuthenticationSource: "oidc", Source: "legacy-api",
	}
	if _, err := s.AppendFindingDecision(ctx, logical); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("AppendFindingDecision(logical unavailable) error = %v, want ErrValidation", err)
	}
}

func TestFindingAssessmentProjectionEvidenceMigration(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `DROP TABLE security_finding_assessments`); err != nil {
		t.Fatalf("drop current assessment table: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE security_finding_assessments (
		id TEXT PRIMARY KEY,
		namespace TEXT NOT NULL,
		repository_scan TEXT NOT NULL,
		scan_run_id TEXT NOT NULL,
		run_uid TEXT NOT NULL,
		occurrence_id TEXT NOT NULL,
		public_finding_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		stage_receipt_id TEXT NOT NULL,
		target_receipt_id TEXT NOT NULL DEFAULT '',
		target_sha TEXT NOT NULL DEFAULT '',
		method TEXT NOT NULL DEFAULT '',
		outcome TEXT NOT NULL,
		failure_class TEXT NOT NULL DEFAULT '',
		summary TEXT NOT NULL DEFAULT '',
		proof_gap TEXT NOT NULL DEFAULT '',
		evidence_receipt_ids_json TEXT NOT NULL DEFAULT '[]',
		normalized_payload BLOB NOT NULL DEFAULT X'',
		payload_digest TEXT NOT NULL DEFAULT '',
		projection_validation_status TEXT NOT NULL DEFAULT '',
		record_digest TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("create legacy assessment table: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO security_finding_assessments
		(id, namespace, repository_scan, scan_run_id, run_uid, occurrence_id, public_finding_id, kind,
		 stage_receipt_id, outcome, failure_class, record_digest, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"assessment_legacy", "ns1", "repo1", "scan_legacy",
		"run_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"occurrence_legacy", "finding_legacy", string(store.FindingAssessmentValidation),
		"receipt_legacy", "failed", "legacy", integrityTestDigest("legacy-assessment"), time.Now().UTC(),
	); err != nil {
		t.Fatalf("insert legacy assessment: %v", err)
	}

	if err := ensureSecurityIntegritySchema(s.db); err != nil {
		t.Fatalf("ensureSecurityIntegritySchema() error = %v", err)
	}
	var evidenceJSON string
	if err := s.db.QueryRowContext(ctx, `SELECT projection_evidence_json
		FROM security_finding_assessments WHERE id = ?`, "assessment_legacy").Scan(&evidenceJSON); err != nil {
		t.Fatalf("read migrated projection evidence: %v", err)
	}
	if evidenceJSON != "[]" {
		t.Fatalf("projection_evidence_json = %q, want []", evidenceJSON)
	}
	assessment, err := s.GetFindingAssessment(ctx, "ns1", "assessment_legacy")
	if err != nil {
		t.Fatalf("GetFindingAssessment(migrated) error = %v", err)
	}
	if len(assessment.ProjectionEvidence) != 0 {
		t.Fatalf("ProjectionEvidence = %#v, want empty migrated evidence", assessment.ProjectionEvidence)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE security_finding_assessments
		SET projection_evidence_json = projection_evidence_json WHERE id = ?`, "assessment_legacy"); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("UPDATE migrated assessment error = %v, want immutable trigger", err)
	}
}

func TestSecurityIntegrityTablesRejectUpdateAndDelete(t *testing.T) {
	s := setupTestStore(t)
	integrityTestSaveTargetReceipt(t, s)
	ctx := context.Background()
	reviewReceipt := integrityTestStageReceipt("receipt_review_1", "review")
	validationReceipt := integrityTestStageReceipt("receipt_validation_1", "validation")
	for _, receipt := range []*store.StageReceipt{reviewReceipt, validationReceipt} {
		if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
			t.Fatalf("AppendStageReceipt(%s) error = %v", receipt.ID, err)
		}
	}
	observation := integrityTestObservation("observation_1", reviewReceipt.ID, integrityTestDigest("semantic"))
	if _, err := s.AcceptFindingObservation(ctx, observation); err != nil {
		t.Fatalf("AcceptFindingObservation() error = %v", err)
	}
	finalization := integrityTestFinalization(observation, "occurrence_1", "fnd_public_1", "semantic_finding_1")
	if _, err := s.FinalizeFindingOccurrence(ctx, finalization); err != nil {
		t.Fatalf("FinalizeFindingOccurrence() error = %v", err)
	}
	assessment := &store.FindingAssessment{
		ID: "assessment_1", Namespace: "ns1", RepositoryScan: "repo1", ScanRunID: observation.ScanRunID,
		RunUID: observation.RunUID, OccurrenceID: "occurrence_1", PublicFindingID: "fnd_public_1",
		Kind: store.FindingAssessmentValidation, StageReceiptID: validationReceipt.ID, Outcome: "deferred",
		FailureClass: "validation_disabled", TargetReceiptID: observation.TargetReceiptID, TargetSHA: strings.Repeat("a", 40),
	}
	if _, err := s.RecordFindingAssessment(ctx, assessment); err != nil {
		t.Fatalf("RecordFindingAssessment() error = %v", err)
	}
	decision := &store.FindingDecision{
		ID: "decision_1", Namespace: "ns1", RepositoryScan: "repo1", PublicFindingID: "fnd_public_1",
		Scope: store.FindingDecisionOccurrence, OccurrenceID: "occurrence_1", Action: store.FindingDecisionReopen,
		ExpectedDecisionVersion: 0, ActorSubject: "user:reviewer", AuthenticationSource: "oidc",
	}
	if _, err := s.AppendFindingDecision(ctx, decision); err != nil {
		t.Fatalf("AppendFindingDecision() error = %v", err)
	}

	for _, table := range []string{
		"security_stage_receipts",
		"security_finding_observations",
		"security_finding_occurrences",
		"security_finding_occurrence_observations",
		"security_finding_aliases",
		"security_finding_assessments",
		"security_finding_decisions",
	} {
		t.Run(table, func(t *testing.T) {
			if _, err := s.db.ExecContext(ctx, "UPDATE "+table+" SET created_at = created_at"); err == nil || !strings.Contains(err.Error(), "immutable") {
				t.Fatalf("UPDATE %s error = %v, want immutable trigger", table, err)
			}
			if _, err := s.db.ExecContext(ctx, "DELETE FROM "+table); err == nil || !strings.Contains(err.Error(), "immutable") {
				t.Fatalf("DELETE %s error = %v, want immutable trigger", table, err)
			}
		})
	}
}

func TestFindingObservationMustMatchStageReceiptProvenance(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	receipt := integrityTestStageReceipt("receipt_observation_binding", "review")
	if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	observation := integrityTestObservation("observation_binding", receipt.ID, integrityTestDigest("semantic-binding"))
	observation.TargetReceiptID = "different_target"
	if _, err := s.AcceptFindingObservation(ctx, observation); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("mismatched observation error = %v", err)
	}
}

func TestFindingObservationSourceCoordinateRejectsRotatedID(t *testing.T) {
	s := setupTestStore(t)
	integrityTestSaveTargetReceipt(t, s)
	ctx := context.Background()
	receipt := integrityTestStageReceipt("receipt_observation_source", "review")
	if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	observation := integrityTestObservation(
		"observation_source_original",
		receipt.ID,
		integrityTestDigest("observation-source"),
	)
	if _, err := s.AcceptFindingObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	rotated := *observation
	rotated.ID = "observation_source_rotated"
	rotated.NormalizedPayload = json.RawMessage(`{"title":"rotated content"}`)
	rotated.PayloadDigest = ""
	rotated.RecordDigest = ""
	rotated.CreatedAt = time.Time{}
	if _, err := s.AcceptFindingObservation(ctx, &rotated); !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("AcceptFindingObservation(rotated ID) error = %v, want ErrDuplicateMismatch", err)
	}
	items, _, err := s.ListFindingObservations(ctx, store.FindingObservationFilter{
		Namespace:      observation.Namespace,
		StageReceiptID: receipt.ID,
		Limit:          10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != observation.ID {
		t.Fatalf("observations = %#v, want only %q", items, observation.ID)
	}
}

func TestSecurityValidatorsRejectWhitespaceIDsAndCanonicalIdentityWithoutFingerprint(t *testing.T) {
	if err := validateSecurityID(" id ", "id", true); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("whitespace ID error = %v", err)
	}
	finding := &store.Finding{IdentityQuality: store.IdentityQualityCanonical, IdentityAlgorithmVersion: semanticIdentityAlgorithmV1}
	if err := normalizeFindingIntegrityFields(finding); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("canonical missing fingerprint error = %v", err)
	}
}

func TestNonLegacyFindingIdentityRequiresExplicitAlgorithm(t *testing.T) {
	for _, quality := range []string{
		store.IdentityQualityCanonical,
		store.IdentityQualityProducerProposed,
	} {
		t.Run(quality, func(t *testing.T) {
			finding := &store.Finding{
				IdentityQuality:     quality,
				SemanticFingerprint: integrityTestDigest("explicit-algorithm-" + quality),
			}
			if err := normalizeFindingIntegrityFields(finding); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("normalizeFindingIntegrityFields() error = %v, want ErrValidation", err)
			}
			if finding.IdentityAlgorithmVersion != "" {
				t.Fatalf("identityAlgorithmVersion = %q, want empty", finding.IdentityAlgorithmVersion)
			}
			finding.IdentityAlgorithmVersion = semanticIdentityAlgorithmV1
			if err := normalizeFindingIntegrityFields(finding); err != nil {
				t.Fatalf("normalizeFindingIntegrityFields(explicit) error = %v", err)
			}
		})
	}

	legacy := &store.Finding{}
	if err := normalizeFindingIntegrityFields(legacy); err != nil {
		t.Fatalf("normalizeFindingIntegrityFields(legacy) error = %v", err)
	}
	if legacy.IdentityAlgorithmVersion != store.IdentityAlgorithmLegacyV2 {
		t.Fatalf("legacy identityAlgorithmVersion = %q, want %q",
			legacy.IdentityAlgorithmVersion, store.IdentityAlgorithmLegacyV2)
	}
}

func TestFindingProjectionRejectsPublicIDAcrossRepositoryScans(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projection := integrityTestFinalization(
		integrityTestObservation("observation_projection", "receipt_projection", integrityTestDigest("projection")),
		"occurrence_projection", "fnd_shared", "semantic_projection",
	).Projection
	projection.RepositoryScan = "repo-a"
	projection.Fingerprint = "fingerprint-shared"
	projection.SemanticFingerprint = integrityTestDigest("projection-semantic")
	projection.IdentityAlgorithmVersion = semanticIdentityAlgorithmV1
	if err := s.UpsertFinding(ctx, &projection); err != nil {
		t.Fatal(err)
	}
	conflict := projection
	conflict.RepositoryScan = "repo-b"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertFindingProjectionTx(ctx, tx, &conflict); !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("upsertFindingProjectionTx(cross-repository ID) error = %v, want ErrDuplicateMismatch", err)
	}
}

func TestFindingProjectionDecisionVersionIsControllerOwned(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projection := integrityTestFinalization(
		integrityTestObservation("observation_version", "receipt_version", integrityTestDigest("version")),
		"occurrence_version", "fnd_version", "semantic_version",
	).Projection
	projection.Fingerprint = "fingerprint-version"
	projection.SemanticFingerprint = integrityTestDigest("version-semantic")
	projection.IdentityAlgorithmVersion = semanticIdentityAlgorithmV1
	if err := s.UpsertFinding(ctx, &projection); err != nil {
		t.Fatal(err)
	}
	update := projection
	update.DecisionVersion = 99
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertFindingProjectionTx(ctx, tx, &update); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetFinding(ctx, projection.Namespace, projection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DecisionVersion != 0 {
		t.Fatalf("stored decisionVersion = %d, want 0", stored.DecisionVersion)
	}
	newProjection := projection
	newProjection.ID = "fnd_version_new"
	newProjection.Fingerprint = "fingerprint-version-new"
	newProjection.DecisionVersion = 1
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertFindingProjectionTx(ctx, tx, &newProjection); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("upsertFindingProjectionTx(new nonzero version) error = %v, want ErrValidation", err)
	}
}

func TestAcceptedObservationRequiresFinalizableIdentityMetadata(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	receipt := integrityTestStageReceipt("receipt_identity", "review")
	if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*store.FindingObservation){
		func(observation *store.FindingObservation) { observation.IdentityQuality = "" },
		func(observation *store.FindingObservation) { observation.IdentityQuality = "invented" },
		func(observation *store.FindingObservation) { observation.IdentityAlgorithmVersion = "" },
	} {
		observation := integrityTestObservation("observation_identity", receipt.ID, integrityTestDigest("identity"))
		mutate(observation)
		if _, err := s.AcceptFindingObservation(ctx, observation); !errors.Is(err, store.ErrValidation) {
			t.Fatalf("AcceptFindingObservation(invalid identity) error = %v, want ErrValidation", err)
		}
	}
}

func TestOccurrenceTargetMustMatchImmutableReceipt(t *testing.T) {
	s := setupTestStore(t)
	integrityTestSaveTargetReceipt(t, s)
	ctx := context.Background()
	receipt := integrityTestStageReceipt("receipt_target", "review")
	if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	observation := integrityTestObservation("observation_target", receipt.ID, integrityTestDigest("target-semantic"))
	if _, err := s.AcceptFindingObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	finalization := integrityTestFinalization(observation, "occurrence_target", "fnd_target", "semantic_target")
	finalization.Occurrence.TargetSHA = strings.Repeat("b", 40)
	if _, err := s.FinalizeFindingOccurrence(ctx, finalization); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("FinalizeFindingOccurrence(target mismatch) error = %v, want ErrValidation", err)
	}
}

func TestValidationAssessmentRequiresAcceptedOccurrenceScopedReceipt(t *testing.T) {
	s := setupTestStore(t)
	integrityTestSaveTargetReceipt(t, s)
	ctx := context.Background()
	reviewReceipt := integrityTestStageReceipt("receipt_assessment_review", "review")
	if _, err := s.AppendStageReceipt(ctx, reviewReceipt); err != nil {
		t.Fatal(err)
	}
	observation := integrityTestObservation("observation_assessment", reviewReceipt.ID, integrityTestDigest("assessment-semantic"))
	if _, err := s.AcceptFindingObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	finalization := integrityTestFinalization(observation, "occurrence_assessment", "fnd_assessment", "semantic_assessment")
	if _, err := s.FinalizeFindingOccurrence(ctx, finalization); err != nil {
		t.Fatal(err)
	}
	assessment := &store.FindingAssessment{
		ID: "assessment_wrong_receipt", Namespace: "ns1", RepositoryScan: "repo1", ScanRunID: observation.ScanRunID,
		RunUID: observation.RunUID, OccurrenceID: finalization.Occurrence.ID, PublicFindingID: finalization.Occurrence.PublicFindingID,
		Kind: store.FindingAssessmentValidation, StageReceiptID: reviewReceipt.ID,
		TargetReceiptID: observation.TargetReceiptID, TargetSHA: strings.Repeat("a", 40), Method: "validator-agent",
		Outcome: assessmentStatusValidated, NormalizedPayload: json.RawMessage(validatedStatusJSON),
		ProjectionValidationStatus: assessmentStatusValidated,
	}
	if _, err := s.RecordFindingAssessment(ctx, assessment); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("RecordFindingAssessment(review receipt) error = %v, want ErrValidation", err)
	}
}

func TestOccurrenceDecisionEvidenceRequiresMatchingRunUID(t *testing.T) {
	s := setupTestStore(t)
	integrityTestSaveTargetReceipt(t, s)
	ctx := context.Background()
	reviewReceipt := integrityTestStageReceipt("receipt_decision_review", "review")
	if _, err := s.AppendStageReceipt(ctx, reviewReceipt); err != nil {
		t.Fatal(err)
	}
	observation := integrityTestObservation("observation_decision", reviewReceipt.ID, integrityTestDigest("decision-semantic"))
	if _, err := s.AcceptFindingObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	finalization := integrityTestFinalization(observation, "occurrence_decision", "fnd_decision", "semantic_decision")
	if _, err := s.FinalizeFindingOccurrence(ctx, finalization); err != nil {
		t.Fatal(err)
	}
	foreignReceipt := *reviewReceipt
	foreignReceipt.ID = "receipt_decision_foreign"
	foreignReceipt.RunUID = "run_ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	foreignReceipt.RecordDigest = ""
	if _, err := s.AppendStageReceipt(ctx, &foreignReceipt); err != nil {
		t.Fatal(err)
	}
	decision := &store.FindingDecision{
		ID: "decision_foreign_evidence", Namespace: "ns1", RepositoryScan: "repo1", PublicFindingID: finalization.Occurrence.PublicFindingID,
		Scope: store.FindingDecisionOccurrence, OccurrenceID: finalization.Occurrence.ID, Action: store.FindingDecisionReopen,
		ExpectedDecisionVersion: 0, EvidenceReceiptIDs: []string{foreignReceipt.ID}, ActorSubject: "user:reviewer",
		AuthenticationSource: "oidc",
	}
	if _, err := s.AppendFindingDecision(ctx, decision); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("AppendFindingDecision(foreign run evidence) error = %v, want ErrValidation", err)
	}
}

func TestGeneralFindingUpsertCannotFabricateDecisionVersion(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projection := integrityTestFinalization(
		integrityTestObservation("observation_general_version", "receipt_general_version", integrityTestDigest("general-version")),
		"occurrence_general_version", "fnd_general_version", "semantic_general_version",
	).Projection
	projection.Fingerprint = "fingerprint-general-version"
	projection.SemanticFingerprint = integrityTestDigest("general-version-semantic")
	projection.IdentityAlgorithmVersion = semanticIdentityAlgorithmV1
	projection.DecisionVersion = 99
	if err := s.UpsertFinding(ctx, &projection); err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetFinding(ctx, projection.Namespace, projection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DecisionVersion != 0 {
		t.Fatalf("new finding decisionVersion = %d, want 0", stored.DecisionVersion)
	}
	projection.DecisionVersion = 77
	if err := s.UpsertFinding(ctx, &projection); err != nil {
		t.Fatal(err)
	}
	stored, err = s.GetFinding(ctx, projection.Namespace, projection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DecisionVersion != 0 {
		t.Fatalf("updated finding decisionVersion = %d, want 0", stored.DecisionVersion)
	}
}

func TestProducerProposedOccurrencesMayShareSemanticProposalWithinRun(t *testing.T) {
	s := setupTestStore(t)
	integrityTestSaveTargetReceipt(t, s)
	ctx := context.Background()
	receipt := integrityTestStageReceipt("receipt_proposed_siblings", "review")
	if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	semantic := integrityTestDigest("shared-proposal")
	for i, payload := range []string{`{"title":"first"}`, `{"title":"second"}`} {
		id := fmt.Sprintf("observation_proposed_sibling_%d", i)
		observation := integrityTestObservation(id, receipt.ID, semantic)
		observation.IdentityQuality = store.IdentityQualityProducerProposed
		observation.Ordinal = i
		observation.CandidateKey = fmt.Sprintf("candidate_proposed_sibling_%d", i)
		observation.IdentityInstance = fmt.Sprintf("instance_proposed_sibling_%d", i)
		observation.NormalizedPayload = json.RawMessage(payload)
		observation.PayloadDigest = ""
		if _, err := s.AcceptFindingObservation(ctx, observation); err != nil {
			t.Fatal(err)
		}
		finalization := integrityTestFinalization(
			observation,
			fmt.Sprintf("occurrence_proposed_sibling_%d", i),
			fmt.Sprintf("fnd_proposed_sibling_%d", i),
			fmt.Sprintf("semantic_proposed_sibling_%d", i),
		)
		finalization.Occurrence.IdentityQuality = store.IdentityQualityProducerProposed
		finalization.Projection.IdentityQuality = store.IdentityQualityProducerProposed
		finalization.Projection.Fingerprint = fmt.Sprintf("compat-proposed-sibling-%d", i)
		finalization.Projection.SemanticFingerprint = semantic
		finalization.Occurrence.DiscoveryPayload = append(json.RawMessage(nil), observation.NormalizedPayload...)
		finalization.Occurrence.PayloadDigest = observation.PayloadDigest
		if _, err := s.FinalizeFindingOccurrence(ctx, finalization); err != nil {
			t.Fatalf("FinalizeFindingOccurrence(%d) error = %v", i, err)
		}
	}
}

func TestScanRunRejectsConflictingIdempotencyAliases(t *testing.T) {
	s := setupTestStore(t)
	run := &store.ScanRun{
		ID: "scan_idempotency_conflict", Namespace: "ns", RepositoryScan: "repo", TaskName: "task",
		Mode: "manual", Phase: "pending", StartedAt: time.Now().UTC(), Quality: store.LegacyScanQuality(),
		IdempotencyKey: "legacy-key", RequestIdempotencyKey: "request-key",
	}
	if err := s.CreateScanRun(context.Background(), run); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("CreateScanRun(conflicting idempotency) error = %v, want ErrValidation", err)
	}
}
