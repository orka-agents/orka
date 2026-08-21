package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

const testReviewStage = "review"

const proposedSecondCandidateKey = "candidate_2"

func TestFindingHistoryListLimitsAreStoreBounded(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	seedFindingHistoryRows(t, s, findingHistoryMaxLimit+1)

	tests := []struct {
		name string
		list func(int) (int, string, error)
	}{
		{
			name: "occurrences",
			list: func(limit int) (int, string, error) {
				items, next, err := s.ListFindingOccurrences(ctx, store.FindingOccurrenceFilter{
					Namespace: "ns1", PublicFindingID: "finding1", Limit: limit,
				})
				return len(items), next, err
			},
		},
		{
			name: "decisions",
			list: func(limit int) (int, string, error) {
				items, next, err := s.ListFindingDecisions(ctx, store.FindingDecisionFilter{
					Namespace: "ns1", PublicFindingID: "finding1", Limit: limit,
				})
				return len(items), next, err
			},
		},
		{
			name: "assessments",
			list: func(limit int) (int, string, error) {
				items, next, err := s.ListFindingAssessments(ctx, store.FindingAssessmentFilter{
					Namespace: "ns1", PublicFindingID: "finding1", Limit: limit,
				})
				return len(items), next, err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/default", func(t *testing.T) {
			count, next, err := tt.list(0)
			if err != nil {
				t.Fatalf("list default limit: %v", err)
			}
			if count != findingHistoryDefaultLimit || next != fmt.Sprint(findingHistoryDefaultLimit) {
				t.Fatalf("default page = (%d, %q), want (%d, %q)",
					count, next, findingHistoryDefaultLimit, fmt.Sprint(findingHistoryDefaultLimit))
			}
		})
		t.Run(tt.name+"/clamped", func(t *testing.T) {
			count, next, err := tt.list(findingHistoryMaxLimit + 100)
			if err != nil {
				t.Fatalf("list oversized limit: %v", err)
			}
			if count != findingHistoryMaxLimit || next != fmt.Sprint(findingHistoryMaxLimit) {
				t.Fatalf("clamped page = (%d, %q), want (%d, %q)",
					count, next, findingHistoryMaxLimit, fmt.Sprint(findingHistoryMaxLimit))
			}
		})
	}
}

func seedFindingHistoryRows(t *testing.T, s *Store, count int) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin seed transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	occurrences, err := tx.Prepare(`INSERT INTO security_finding_occurrences (
		id, namespace, repository_scan, scan_run_id, run_uid, public_finding_id,
		semantic_finding_id, semantic_fingerprint, identity_quality, identity_algorithm_version,
		record_digest, created_at
	) VALUES (?, 'ns1', 'repo1', 'run1', 'uid1', 'finding1', ?, ?, 'producer_proposed', 'v1', ?, ?)`)
	if err != nil {
		t.Fatalf("prepare occurrence seed: %v", err)
	}
	defer occurrences.Close() //nolint:errcheck

	assessments, err := tx.Prepare(`INSERT INTO security_finding_assessments (
		id, namespace, repository_scan, scan_run_id, run_uid, occurrence_id, public_finding_id,
		kind, stage_receipt_id, outcome, record_digest, created_at
	) VALUES (?, 'ns1', 'repo1', 'run1', 'uid1', 'occurrence1', 'finding1',
		'validation', 'receipt1', 'validated', ?, ?)`)
	if err != nil {
		t.Fatalf("prepare assessment seed: %v", err)
	}
	defer assessments.Close() //nolint:errcheck

	decisions, err := tx.Prepare(`INSERT INTO security_finding_decisions (
		id, namespace, repository_scan, public_finding_id, scope, occurrence_id, action,
		expected_decision_version, decision_version, actor_subject, authentication_source,
		record_digest, created_at
	) VALUES (?, 'ns1', 'repo1', 'finding1', 'occurrence', 'occurrence1', 'reopen',
		?, ?, 'user:test', 'test', ?, ?)`)
	if err != nil {
		t.Fatalf("prepare decision seed: %v", err)
	}
	defer decisions.Close() //nolint:errcheck

	createdAt := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	for i := range count {
		id := fmt.Sprintf("%04d", i)
		if _, err := occurrences.Exec("occurrence_"+id, "semantic_"+id, "fingerprint_"+id, "digest_"+id, createdAt); err != nil {
			t.Fatalf("seed occurrence %d: %v", i, err)
		}
		if _, err := assessments.Exec("assessment_"+id, "digest_"+id, createdAt); err != nil {
			t.Fatalf("seed assessment %d: %v", i, err)
		}
		version := i + 1
		if _, err := decisions.Exec("decision_"+id, i, version, "digest_"+id, createdAt); err != nil {
			t.Fatalf("seed decision %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed transaction: %v", err)
	}
}

func TestProducerProposedOccurrenceRejectsMixedObservationPayloads(t *testing.T) {
	s := setupTestStore(t)
	integrityTestSaveTargetReceipt(t, s)
	ctx := context.Background()
	receipt := integrityTestStageReceipt("receipt_proposed_payload_mismatch", testReviewStage)
	if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	semantic := integrityTestDigest("producer-payload-binding")
	first := integrityTestObservation("observation_proposed_payload_first", receipt.ID, semantic)
	first.IdentityQuality = store.IdentityQualityProducerProposed
	first.NormalizedPayload = json.RawMessage(`{"title":"first"}`)
	first.PayloadDigest = ""
	if _, err := s.AcceptFindingObservation(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := integrityTestObservation("observation_proposed_payload_second", receipt.ID, semantic)
	second.IdentityQuality = store.IdentityQualityProducerProposed
	second.CandidateKey = proposedSecondCandidateKey
	second.Ordinal = 1
	second.NormalizedPayload = json.RawMessage(`{"title":"second"}`)
	second.PayloadDigest = ""
	if _, err := s.AcceptFindingObservation(ctx, second); err != nil {
		t.Fatal(err)
	}
	finalization := integrityTestFinalization(first, "occurrence_proposed_payload_mismatch", "fnd_proposed_payload_mismatch", "semantic_proposed_payload_mismatch")
	finalization.Occurrence.IdentityQuality = store.IdentityQualityProducerProposed
	finalization.Projection.IdentityQuality = store.IdentityQualityProducerProposed
	finalization.Occurrence.DiscoveryPayload = append(json.RawMessage(nil), first.NormalizedPayload...)
	finalization.Occurrence.PayloadDigest = first.PayloadDigest
	finalization.ObservationLinks = append(finalization.ObservationLinks, store.FindingOccurrenceObservation{
		ObservationID: second.ID,
		Relationship:  store.FindingObservationRelationshipAbsorbed,
		Ordinal:       1,
	})
	if _, err := s.FinalizeFindingOccurrence(ctx, finalization); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("FinalizeFindingOccurrence(mixed proposed payloads) error = %v, want ErrValidation", err)
	}
	if _, err := s.GetFindingOccurrence(ctx, first.Namespace, finalization.Occurrence.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetFindingOccurrence(after rejected finalization) error = %v, want ErrNotFound", err)
	}
}

func TestProducerProposedOccurrenceAcceptsMatchingObservationPayloads(t *testing.T) {
	s := setupTestStore(t)
	integrityTestSaveTargetReceipt(t, s)
	ctx := context.Background()
	receipt := integrityTestStageReceipt("receipt_proposed_payload_match", testReviewStage)
	if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	semantic := integrityTestDigest("producer-payload-binding-match")
	first := integrityTestObservation("observation_proposed_payload_match_first", receipt.ID, semantic)
	first.IdentityQuality = store.IdentityQualityProducerProposed
	first.NormalizedPayload = json.RawMessage(`{"title":"same"}`)
	first.PayloadDigest = ""
	if _, err := s.AcceptFindingObservation(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := integrityTestObservation("observation_proposed_payload_match_second", receipt.ID, semantic)
	second.IdentityQuality = store.IdentityQualityProducerProposed
	second.CandidateKey = proposedSecondCandidateKey
	second.Ordinal = 1
	second.NormalizedPayload = append(json.RawMessage(nil), first.NormalizedPayload...)
	second.PayloadDigest = first.PayloadDigest
	if _, err := s.AcceptFindingObservation(ctx, second); err != nil {
		t.Fatal(err)
	}
	finalization := integrityTestFinalization(first, "occurrence_proposed_payload_match", "fnd_proposed_payload_match", "semantic_proposed_payload_match")
	finalization.Occurrence.IdentityQuality = store.IdentityQualityProducerProposed
	finalization.Projection.IdentityQuality = store.IdentityQualityProducerProposed
	finalization.Occurrence.DiscoveryPayload = append(json.RawMessage(nil), first.NormalizedPayload...)
	finalization.Occurrence.PayloadDigest = first.PayloadDigest
	finalization.ObservationLinks = append(finalization.ObservationLinks, store.FindingOccurrenceObservation{
		ObservationID: second.ID,
		Relationship:  store.FindingObservationRelationshipAbsorbed,
		Ordinal:       1,
	})
	created, err := s.FinalizeFindingOccurrence(ctx, finalization)
	if err != nil || !created {
		t.Fatalf("FinalizeFindingOccurrence(matching proposed payloads) = %v, %v", created, err)
	}
}

func TestProducerProposedOccurrenceRejectsOccurrencePayloadMismatch(t *testing.T) {
	s := setupTestStore(t)
	integrityTestSaveTargetReceipt(t, s)
	ctx := context.Background()
	receipt := integrityTestStageReceipt("receipt_proposed_occurrence_payload_mismatch", testReviewStage)
	if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	observation := integrityTestObservation(
		"observation_proposed_occurrence_payload_mismatch", receipt.ID, integrityTestDigest("producer-occurrence-payload-binding"),
	)
	observation.IdentityQuality = store.IdentityQualityProducerProposed
	observation.NormalizedPayload = json.RawMessage(`{"title":"trusted observation"}`)
	observation.PayloadDigest = ""
	if _, err := s.AcceptFindingObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	finalization := integrityTestFinalization(
		observation, "occurrence_proposed_payload_tampered", "fnd_proposed_payload_tampered", "semantic_proposed_payload_tampered",
	)
	finalization.Occurrence.IdentityQuality = store.IdentityQualityProducerProposed
	finalization.Projection.IdentityQuality = store.IdentityQualityProducerProposed
	finalization.Occurrence.DiscoveryPayload = json.RawMessage(`{"title":"different occurrence"}`)
	finalization.Occurrence.PayloadDigest = ""
	if _, err := s.FinalizeFindingOccurrence(ctx, finalization); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("FinalizeFindingOccurrence() error = %v, want ErrValidation", err)
	}
}

func setupSuccessfulAttackPathAssessment(t *testing.T, suffix string, mutateReceipt func(*store.StageReceipt)) (*Store, *store.FindingAssessment) {
	t.Helper()
	s := setupTestStore(t)
	integrityTestSaveTargetReceipt(t, s)
	ctx := context.Background()

	reviewReceipt := integrityTestStageReceipt("receipt_attack_review_"+suffix, testReviewStage)
	if _, err := s.AppendStageReceipt(ctx, reviewReceipt); err != nil {
		t.Fatalf("AppendStageReceipt(review): %v", err)
	}
	observation := integrityTestObservation(
		"observation_attack_"+suffix,
		reviewReceipt.ID,
		integrityTestDigest("attack-semantic-"+suffix),
	)
	if _, err := s.AcceptFindingObservation(ctx, observation); err != nil {
		t.Fatalf("AcceptFindingObservation(): %v", err)
	}
	occurrenceID := "occurrence_attack_" + suffix
	publicFindingID := "fnd_attack_" + suffix
	finalization := integrityTestFinalization(observation, occurrenceID, publicFindingID, "semantic_attack_"+suffix)
	if _, err := s.FinalizeFindingOccurrence(ctx, finalization); err != nil {
		t.Fatalf("FinalizeFindingOccurrence(): %v", err)
	}

	payload := json.RawMessage(`{"attackPath":"complete"}`)
	primaryReceipt := integrityTestStageReceipt("receipt_attack_primary_"+suffix, "validation")
	primaryReceipt.ScopeKind = securityScopeOccurrence
	primaryReceipt.ScopeID = occurrenceID
	primaryReceipt.NormalizedOutputDigest = securityDigestBytes(payload)
	if mutateReceipt != nil {
		mutateReceipt(primaryReceipt)
	}
	if _, err := s.AppendStageReceipt(ctx, primaryReceipt); err != nil {
		t.Fatalf("AppendStageReceipt(primary): %v", err)
	}

	return s, &store.FindingAssessment{
		ID:                "assessment_attack_" + suffix,
		Namespace:         observation.Namespace,
		RepositoryScan:    observation.RepositoryScan,
		ScanRunID:         observation.ScanRunID,
		RunUID:            observation.RunUID,
		OccurrenceID:      occurrenceID,
		PublicFindingID:   publicFindingID,
		Kind:              store.FindingAssessmentAttackPath,
		StageReceiptID:    primaryReceipt.ID,
		TargetReceiptID:   observation.TargetReceiptID,
		TargetSHA:         primaryReceipt.ExpectedTargetSHA,
		Method:            " policy ",
		Outcome:           " complete ",
		FailureClass:      " ",
		Summary:           "The attack path is complete.",
		NormalizedPayload: payload,
	}
}

func TestSuccessfulAttackPathAssessmentRequiresBoundPrimaryReceipt(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mutateReceipt func(*store.StageReceipt)
		wantErr       bool
	}{
		{name: "accepted"},
		{
			name: "wrong stage",
			mutateReceipt: func(receipt *store.StageReceipt) {
				receipt.Stage = testReviewStage
			},
			wantErr: true,
		},
		{
			name: "wrong scope kind",
			mutateReceipt: func(receipt *store.StageReceipt) {
				receipt.ScopeKind = "slice"
			},
			wantErr: true,
		},
		{
			name: "wrong scope id",
			mutateReceipt: func(receipt *store.StageReceipt) {
				receipt.ScopeID = "occurrence_other"
			},
			wantErr: true,
		},
		{
			name: "rejected disposition",
			mutateReceipt: func(receipt *store.StageReceipt) {
				receipt.Disposition = store.StageReceiptRejected
				receipt.ReasonCode = "artifact_invalid"
				receipt.Reason = "The attack-path artifact was rejected."
			},
			wantErr: true,
		},
		{
			name: "wrong output digest",
			mutateReceipt: func(receipt *store.StageReceipt) {
				receipt.NormalizedOutputDigest = integrityTestDigest("different attack payload")
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			suffix := strings.ReplaceAll(tc.name, " ", "_")
			s, assessment := setupSuccessfulAttackPathAssessment(t, suffix, tc.mutateReceipt)
			created, err := s.RecordFindingAssessment(context.Background(), assessment)
			if tc.wantErr {
				if !errors.Is(err, store.ErrValidation) || created {
					t.Fatalf("RecordFindingAssessment() = (%v, %v), want (false, ErrValidation)", created, err)
				}
				if _, getErr := s.GetFindingAssessment(context.Background(), assessment.Namespace, assessment.ID); !errors.Is(getErr, store.ErrNotFound) {
					t.Fatalf("GetFindingAssessment(after rejection) error = %v, want ErrNotFound", getErr)
				}
				return
			}
			if err != nil || !created {
				t.Fatalf("RecordFindingAssessment() = (%v, %v), want (true, nil)", created, err)
			}
			stored, err := s.GetFindingAssessment(context.Background(), assessment.Namespace, assessment.ID)
			if err != nil {
				t.Fatalf("GetFindingAssessment() error = %v", err)
			}
			if stored.Method != "policy" || stored.Outcome != "complete" || stored.FailureClass != "" {
				t.Fatalf("normalized assessment policy fields = (%q, %q, %q), want (policy, complete, empty)",
					stored.Method, stored.Outcome, stored.FailureClass)
			}
		})
	}
}

func TestSuccessfulValidationAssessmentRequiresBoundPrimaryReceipt(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mutateReceipt func(*store.StageReceipt)
		wantErr       bool
	}{
		{name: "accepted"},
		{
			name: "wrong stage",
			mutateReceipt: func(receipt *store.StageReceipt) {
				receipt.Stage = testReviewStage
			},
			wantErr: true,
		},
		{
			name: "wrong output digest",
			mutateReceipt: func(receipt *store.StageReceipt) {
				receipt.NormalizedOutputDigest = integrityTestDigest("different validation payload")
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			suffix := "validation_" + strings.ReplaceAll(tc.name, " ", "_")
			s, assessment := setupSuccessfulAttackPathAssessment(t, suffix, tc.mutateReceipt)
			assessment.Kind = store.FindingAssessmentValidation
			assessment.Outcome = " confirmed "
			created, err := s.RecordFindingAssessment(context.Background(), assessment)
			if tc.wantErr {
				if !errors.Is(err, store.ErrValidation) || created {
					t.Fatalf("RecordFindingAssessment() = (%v, %v), want (false, ErrValidation)", created, err)
				}
				return
			}
			if err != nil || !created {
				t.Fatalf("RecordFindingAssessment() = (%v, %v), want (true, nil)", created, err)
			}
			stored, err := s.GetFindingAssessment(context.Background(), assessment.Namespace, assessment.ID)
			if err != nil {
				t.Fatalf("GetFindingAssessment() error = %v", err)
			}
			if stored.Method != "policy" || stored.Outcome != assessmentOutcomeConfirmed || stored.FailureClass != "" {
				t.Fatalf("normalized validation policy fields = (%q, %q, %q), want (policy, confirmed, empty)",
					stored.Method, stored.Outcome, stored.FailureClass)
			}
		})
	}
}

func TestValidationAssessmentAtomicallyProjectsPayloadAndFullEvidence(t *testing.T) {
	s, assessment := setupSuccessfulAttackPathAssessment(t, "atomic_projection", nil)
	ctx := context.Background()
	before, err := s.GetFinding(ctx, assessment.Namespace, assessment.PublicFindingID)
	if err != nil {
		t.Fatalf("GetFinding(before) error = %v", err)
	}
	projectedEvidence := append([]store.FindingEvidenceRef(nil), before.Evidence...)
	projectedEvidence = append(projectedEvidence, store.FindingEvidenceRef{
		Kind: "artifact", TaskName: "validation-task", Name: "security-validation.json", Label: "Validation JSON",
	})
	assessment.Kind = store.FindingAssessmentValidation
	assessment.Outcome = " confirmed "
	assessment.ProjectionValidationStatus = " validated "
	assessment.ProjectionEvidence = projectedEvidence

	created, err := s.RecordFindingAssessment(ctx, assessment)
	if err != nil || !created {
		t.Fatalf("RecordFindingAssessment(first) = (%v, %v), want (true, nil)", created, err)
	}
	projected, err := s.GetFinding(ctx, assessment.Namespace, assessment.PublicFindingID)
	if err != nil {
		t.Fatalf("GetFinding(after) error = %v", err)
	}
	if projected.ValidationStatus != assessmentStatusValidated {
		t.Fatalf("ValidationStatus = %q, want %q", projected.ValidationStatus, assessmentStatusValidated)
	}
	if projected.ValidationJSON != string(assessment.NormalizedPayload) {
		t.Fatalf("ValidationJSON = %q, want canonical assessment payload %q", projected.ValidationJSON, assessment.NormalizedPayload)
	}
	if !slices.Equal(projected.Evidence, assessment.ProjectionEvidence) {
		t.Fatalf("Evidence = %#v, want full projected evidence %#v", projected.Evidence, assessment.ProjectionEvidence)
	}
	stored, err := s.GetFindingAssessment(ctx, assessment.Namespace, assessment.ID)
	if err != nil {
		t.Fatalf("GetFindingAssessment() error = %v", err)
	}
	if !slices.Equal(stored.ProjectionEvidence, assessment.ProjectionEvidence) {
		t.Fatalf("stored ProjectionEvidence = %#v, want %#v", stored.ProjectionEvidence, assessment.ProjectionEvidence)
	}
	if stored.RecordDigest != assessment.RecordDigest {
		t.Fatalf("stored RecordDigest = %q, want %q", stored.RecordDigest, assessment.RecordDigest)
	}

	created, err = s.RecordFindingAssessment(ctx, assessment)
	if err != nil || created {
		t.Fatalf("RecordFindingAssessment(exact replay) = (%v, %v), want (false, nil)", created, err)
	}
	mismatch := *assessment
	mismatch.ProjectionEvidence = append([]store.FindingEvidenceRef(nil), assessment.ProjectionEvidence...)
	mismatch.ProjectionEvidence[len(mismatch.ProjectionEvidence)-1].Label = "Different validation artifact"
	if created, err = s.RecordFindingAssessment(ctx, &mismatch); !errors.Is(err, store.ErrDuplicateMismatch) || created {
		t.Fatalf("RecordFindingAssessment(evidence mismatch) = (%v, %v), want (false, ErrDuplicateMismatch)", created, err)
	}
}

func TestValidationAssessmentProjectionFailureRollsBackAssessmentAndFinding(t *testing.T) {
	s, assessment := setupSuccessfulAttackPathAssessment(t, "projection_rollback", nil)
	ctx := context.Background()
	before, err := s.GetFinding(ctx, assessment.Namespace, assessment.PublicFindingID)
	if err != nil {
		t.Fatalf("GetFinding(before) error = %v", err)
	}
	assessment.Kind = store.FindingAssessmentValidation
	assessment.Outcome = assessmentOutcomeConfirmed
	assessment.ProjectionValidationStatus = assessmentStatusValidated
	assessment.ProjectionEvidence = append([]store.FindingEvidenceRef(nil), before.Evidence...)
	assessment.ProjectionEvidence = append(assessment.ProjectionEvidence, store.FindingEvidenceRef{
		Kind: "artifact", TaskName: "validation-task", Name: "security-validation.json", Label: "Validation JSON",
	})
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_validation_assessment_projection
		BEFORE UPDATE OF validation_status, validation_json, evidence_json ON security_findings
		BEGIN SELECT RAISE(ABORT, 'injected validation assessment projection failure'); END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	created, err := s.RecordFindingAssessment(ctx, assessment)
	if err == nil || created {
		t.Fatalf("RecordFindingAssessment() = (%v, %v), want injected failure", created, err)
	}
	after, getErr := s.GetFinding(ctx, assessment.Namespace, assessment.PublicFindingID)
	if getErr != nil {
		t.Fatalf("GetFinding(after failure) error = %v", getErr)
	}
	if after.ValidationStatus != before.ValidationStatus || after.ValidationJSON != before.ValidationJSON ||
		!slices.Equal(after.Evidence, before.Evidence) {
		t.Fatalf("finding after failed transaction = status %q JSON %q evidence %#v, want prior status %q JSON %q evidence %#v",
			after.ValidationStatus, after.ValidationJSON, after.Evidence,
			before.ValidationStatus, before.ValidationJSON, before.Evidence)
	}
	if _, getErr := s.GetFindingAssessment(ctx, assessment.Namespace, assessment.ID); !errors.Is(getErr, store.ErrNotFound) {
		t.Fatalf("GetFindingAssessment(after rollback) error = %v, want ErrNotFound", getErr)
	}
}

func TestAttackPathAssessmentDoesNotProjectValidationOrEvidence(t *testing.T) {
	s, assessment := setupSuccessfulAttackPathAssessment(t, "no_projection", nil)
	ctx := context.Background()
	before, err := s.GetFinding(ctx, assessment.Namespace, assessment.PublicFindingID)
	if err != nil {
		t.Fatalf("GetFinding(before) error = %v", err)
	}
	created, err := s.RecordFindingAssessment(ctx, assessment)
	if err != nil || !created {
		t.Fatalf("RecordFindingAssessment(attack path) = (%v, %v), want (true, nil)", created, err)
	}
	after, err := s.GetFinding(ctx, assessment.Namespace, assessment.PublicFindingID)
	if err != nil {
		t.Fatalf("GetFinding(after) error = %v", err)
	}
	if after.ValidationStatus != before.ValidationStatus || after.ValidationJSON != before.ValidationJSON ||
		!slices.Equal(after.Evidence, before.Evidence) {
		t.Fatalf("attack-path assessment changed validation projection: before=%#v after=%#v", before, after)
	}
	stored, err := s.GetFindingAssessment(ctx, assessment.Namespace, assessment.ID)
	if err != nil {
		t.Fatalf("GetFindingAssessment() error = %v", err)
	}
	if stored.ProjectionValidationStatus != "" || len(stored.ProjectionEvidence) != 0 {
		t.Fatalf("stored attack-path projection = %q/%#v, want empty", stored.ProjectionValidationStatus, stored.ProjectionEvidence)
	}

	invalid := *assessment
	invalid.ID += "_invalid_projection"
	invalid.RecordDigest = ""
	invalid.CreatedAt = time.Time{}
	invalid.ProjectionValidationStatus = assessmentStatusValidated
	invalid.ProjectionEvidence = append([]store.FindingEvidenceRef(nil), before.Evidence...)
	if created, err = s.RecordFindingAssessment(ctx, &invalid); !errors.Is(err, store.ErrValidation) || created {
		t.Fatalf("RecordFindingAssessment(projecting attack path) = (%v, %v), want (false, ErrValidation)", created, err)
	}
}

func TestValidatorMethodWhitespaceCannotBypassReceiptPolicy(t *testing.T) {
	s, assessment := setupSuccessfulAttackPathAssessment(t, "validator_method_whitespace", func(receipt *store.StageReceipt) {
		receipt.Stage = testReviewStage
		receipt.Disposition = store.StageReceiptRejected
		receipt.ReasonCode = "task_failed"
		receipt.Reason = "The validator task failed."
	})
	assessment.Kind = store.FindingAssessmentValidation
	assessment.Method = " validator-agent "
	assessment.Outcome = " failed "
	assessment.FailureClass = " task_failed "
	assessment.NormalizedPayload = nil
	assessment.PayloadDigest = ""

	created, err := s.RecordFindingAssessment(context.Background(), assessment)
	if !errors.Is(err, store.ErrValidation) || created {
		t.Fatalf("RecordFindingAssessment() = (%v, %v), want (false, ErrValidation)", created, err)
	}
}

func TestFindingDecisionIDsAreScopedByNamespace(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	const sharedDecisionID = "decision_shared"

	fixtures := []struct {
		namespace    string
		findingID    string
		occurrenceID string
		action       store.FindingDecisionAction
	}{
		{namespace: "tenant-a", findingID: "finding-a", occurrenceID: "occurrence-a", action: store.FindingDecisionReopen},
		{namespace: "tenant-b", findingID: "finding-b", occurrenceID: "occurrence-b", action: store.FindingDecisionCloseFixed},
	}
	decisions := make(map[string]*store.FindingDecision, len(fixtures))
	for _, fixture := range fixtures {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO security_findings
			(id, namespace, repository_scan, scan_run_id, fingerprint, title, summary, severity,
			 confidence, validation_status, state, current_occurrence_id)
			VALUES (?, ?, 'repo1', ?, ?, 'Finding', 'Summary', 'medium', 'high', 'unvalidated', 'open', ?)`,
			fixture.findingID, fixture.namespace, "run-"+fixture.namespace, "fingerprint-"+fixture.namespace,
			fixture.occurrenceID); err != nil {
			t.Fatalf("insert finding for %s: %v", fixture.namespace, err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO security_finding_occurrences
			(id, namespace, repository_scan, scan_run_id, run_uid, public_finding_id, semantic_finding_id,
			 semantic_fingerprint, identity_quality, identity_algorithm_version, record_digest)
			VALUES (?, ?, 'repo1', ?, ?, ?, ?, ?, ?, ?, ?)`,
			fixture.occurrenceID, fixture.namespace, "run-"+fixture.namespace, "run-uid-"+fixture.namespace,
			fixture.findingID, "semantic-"+fixture.namespace, "semantic-fingerprint-"+fixture.namespace,
			store.IdentityQualityCanonical, semanticIdentityAlgorithmV1, "digest-"+fixture.namespace); err != nil {
			t.Fatalf("insert occurrence for %s: %v", fixture.namespace, err)
		}

		decision := &store.FindingDecision{
			ID:                      sharedDecisionID,
			Namespace:               fixture.namespace,
			RepositoryScan:          "repo1",
			PublicFindingID:         fixture.findingID,
			Scope:                   store.FindingDecisionOccurrence,
			OccurrenceID:            fixture.occurrenceID,
			Action:                  fixture.action,
			ExpectedDecisionVersion: 0,
			ActorSubject:            "user:reviewer",
			AuthenticationSource:    "oidc",
			Source:                  "api",
		}
		appended, err := s.AppendFindingDecision(ctx, decision)
		if err != nil {
			t.Fatalf("AppendFindingDecision(%s) error = %v", fixture.namespace, err)
		}
		if appended.Namespace != fixture.namespace || appended.PublicFindingID != fixture.findingID || appended.DecisionVersion != 1 {
			t.Fatalf("AppendFindingDecision(%s) = %#v", fixture.namespace, appended)
		}
		decisions[fixture.namespace] = decision
	}

	for _, fixture := range fixtures {
		got, err := s.GetFindingDecision(ctx, fixture.namespace, sharedDecisionID)
		if err != nil {
			t.Fatalf("GetFindingDecision(%s) error = %v", fixture.namespace, err)
		}
		if got.Namespace != fixture.namespace || got.PublicFindingID != fixture.findingID || got.Action != fixture.action {
			t.Fatalf("GetFindingDecision(%s) = %#v", fixture.namespace, got)
		}
		replayed, err := s.AppendFindingDecision(ctx, decisions[fixture.namespace])
		if err != nil {
			t.Fatalf("AppendFindingDecision(%s replay) error = %v", fixture.namespace, err)
		}
		if replayed.Namespace != fixture.namespace || replayed.PublicFindingID != fixture.findingID || replayed.DecisionVersion != 1 {
			t.Fatalf("AppendFindingDecision(%s replay) = %#v", fixture.namespace, replayed)
		}
	}

	mismatch := *decisions[fixtures[0].namespace]
	mismatch.Action = store.FindingDecisionCloseFixed
	if _, err := s.AppendFindingDecision(ctx, &mismatch); !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("AppendFindingDecision(same-namespace mismatch) error = %v, want ErrDuplicateMismatch", err)
	}
}

func TestFindingDecisionFeedbackEligibilityUsesScopedOccurrenceIdentity(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		occurrenceIdentityQuality string
		projectionIdentityQuality string
		wantEligible              bool
	}{
		{
			name:                      "canonical occurrence with producer projection",
			occurrenceIdentityQuality: store.IdentityQualityCanonical,
			projectionIdentityQuality: store.IdentityQualityProducerProposed,
			wantEligible:              true,
		},
		{
			name:                      "producer occurrence with canonical projection",
			occurrenceIdentityQuality: store.IdentityQualityProducerProposed,
			projectionIdentityQuality: store.IdentityQualityCanonical,
			wantEligible:              false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestStore(t)
			integrityTestSaveTargetReceipt(t, s)
			ctx := context.Background()
			suffix := strings.ReplaceAll(tc.name, " ", "_")
			receipt := integrityTestStageReceipt("receipt_feedback_"+suffix, testReviewStage)
			if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
				t.Fatalf("AppendStageReceipt() error = %v", err)
			}
			observation := integrityTestObservation(
				"observation_feedback_"+suffix,
				receipt.ID,
				integrityTestDigest("feedback-"+suffix),
			)
			observation.IdentityQuality = tc.occurrenceIdentityQuality
			if tc.occurrenceIdentityQuality == store.IdentityQualityProducerProposed {
				observation.IdentityAlgorithmVersion = "producer-v1"
			}
			if _, err := s.AcceptFindingObservation(ctx, observation); err != nil {
				t.Fatalf("AcceptFindingObservation() error = %v", err)
			}
			finalization := integrityTestFinalization(
				observation,
				"occurrence_feedback_"+suffix,
				"fnd_feedback_"+suffix,
				"semantic_feedback_"+suffix,
			)
			finalization.Occurrence.DiscoveryPayload = append(json.RawMessage(nil), observation.NormalizedPayload...)
			finalization.Occurrence.PayloadDigest = observation.PayloadDigest
			if _, err := s.FinalizeFindingOccurrence(ctx, finalization); err != nil {
				t.Fatalf("FinalizeFindingOccurrence() error = %v", err)
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE security_findings SET identity_quality = ? WHERE namespace = ? AND id = ?`,
				tc.projectionIdentityQuality, finalization.Projection.Namespace, finalization.Projection.ID); err != nil {
				t.Fatalf("set projection identity quality: %v", err)
			}

			decision := &store.FindingDecision{
				ID:                      "decision_feedback_" + suffix,
				Namespace:               finalization.Projection.Namespace,
				RepositoryScan:          finalization.Projection.RepositoryScan,
				PublicFindingID:         finalization.Projection.ID,
				Scope:                   store.FindingDecisionOccurrence,
				OccurrenceID:            finalization.Occurrence.ID,
				Action:                  store.FindingDecisionCloseFalsePositive,
				ReasonCode:              "reviewed",
				Reason:                  "The scoped occurrence was reviewed.",
				ExpectedDecisionVersion: 0,
				ActorSubject:            "user:reviewer",
				AuthenticationSource:    "oidc",
			}
			created, err := s.AppendFindingDecision(ctx, decision)
			if err != nil {
				t.Fatalf("AppendFindingDecision() error = %v", err)
			}
			if created.FeedbackEligible != tc.wantEligible {
				t.Fatalf("FeedbackEligible = %v, want %v", created.FeedbackEligible, tc.wantEligible)
			}
		})
	}
}

func TestFinalizeFindingOccurrenceRedactsProjectionBeforePersistence(t *testing.T) {
	s := setupTestStore(t)
	integrityTestSaveTargetReceipt(t, s)
	ctx := context.Background()
	receipt := integrityTestStageReceipt("receipt_projection_redaction", testReviewStage)
	if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	observation := integrityTestObservation("observation_projection_redaction", receipt.ID, integrityTestDigest("projection-redaction"))
	if _, err := s.AcceptFindingObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	credential := strings.Join([]string{"github", "_pat_", "abcdefghijklmnopqrstuvwxyz1234567890"}, "")
	finalization := integrityTestFinalization(observation, "occurrence_projection_redaction", "fnd_projection_history_redaction", "semantic_projection_redaction")
	finalization.Projection.RootCause = strings.Join([]string{"root to", "ken=", credential}, "")
	finalization.Projection.Reproduction = strings.Join([]string{"reproduce to", "ken=", credential}, "")
	finalization.Projection.Evidence = []store.FindingEvidenceRef{{
		Kind:  "note",
		Label: strings.Join([]string{"label to", "ken=", credential}, ""),
		Quote: strings.Join([]string{"quote to", "ken=", credential}, ""),
	}}
	created, err := s.FinalizeFindingOccurrence(ctx, finalization)
	if err != nil || !created {
		t.Fatalf("FinalizeFindingOccurrence() = %v, %v", created, err)
	}
	got, err := s.GetFinding(ctx, finalization.Projection.Namespace, finalization.Projection.ID)
	if err != nil {
		t.Fatal(err)
	}
	persisted := fmt.Sprintf("%#v", got)
	if strings.Contains(persisted, credential) {
		t.Fatalf("persisted occurrence projection retained credential: %s", persisted)
	}
	if !strings.Contains(got.RootCause, "[REDACTED]") || !strings.Contains(got.Reproduction, "[REDACTED]") ||
		len(got.Evidence) != 1 || !strings.Contains(got.Evidence[0].Label, "[REDACTED]") ||
		!strings.Contains(got.Evidence[0].Quote, "[REDACTED]") {
		t.Fatalf("projection was not redacted before persistence: %#v", got)
	}
}

type findingProjectionRunFixture struct {
	ID                       string
	RepositoryScan           string
	RepositoryScanUID        string
	RepositoryScanGeneration int64
	StartedAt                time.Time
}

func prepareFindingProjectionRun(
	t *testing.T,
	s *Store,
	run findingProjectionRunFixture,
	occurrenceID string,
	projectionTitle string,
	mutateObservation func(*store.FindingObservation),
) *store.FindingOccurrenceFinalization {
	t.Helper()
	ctx := context.Background()
	repositoryScan := run.RepositoryScan
	if repositoryScan == "" {
		repositoryScan = "repo1"
	}
	runUID := "run_" + integrityTestRawDigest("finding-projection-run:"+run.ID)
	targetReceiptID := "target_receipt_" + run.ID
	headSHA := strings.Repeat("a", 40)
	if err := s.CreateScanRun(ctx, &store.ScanRun{
		ID:                       run.ID,
		RunUID:                   runUID,
		Namespace:                "ns1",
		RepositoryScan:           repositoryScan,
		RepositoryScanUID:        run.RepositoryScanUID,
		RepositoryScanGeneration: run.RepositoryScanGeneration,
		TaskName:                 "task_" + run.ID,
		Mode:                     "manual",
		Phase:                    "succeeded",
		HeadCommit:               headSHA,
		TargetReceiptID:          targetReceiptID,
		StartedAt:                run.StartedAt,
		Quality:                  store.LegacyScanQuality(),
	}); err != nil {
		t.Fatalf("CreateScanRun(%s) error = %v", run.ID, err)
	}
	if _, err := s.SaveSecurityTargetReceipt(ctx, &store.SecurityTargetReceipt{
		ID:             targetReceiptID,
		Namespace:      "ns1",
		RepositoryScan: repositoryScan,
		ScanRunID:      run.ID,
		RunUID:         runUID,
		TargetID:       "target_projection_ordering",
		HeadSHA:        headSHA,
		ObjectFormat:   "sha1",
		SnapshotDigest: integrityTestDigest("snapshot:" + run.ID),
		TreeDigest:     integrityTestDigest("tree:" + run.ID),
		ReceiptJSON:    json.RawMessage(`{"headOID":"` + headSHA + `"}`),
	}); err != nil {
		t.Fatalf("SaveSecurityTargetReceipt(%s) error = %v", run.ID, err)
	}

	receipt := integrityTestStageReceipt("receipt_"+run.ID, testReviewStage)
	receipt.RepositoryScan = repositoryScan
	receipt.ScanRunID = run.ID
	receipt.RunUID = runUID
	receipt.TargetReceiptID = targetReceiptID
	receipt.RecordDigest = ""
	if _, err := s.AppendStageReceipt(ctx, receipt); err != nil {
		t.Fatalf("AppendStageReceipt(%s) error = %v", run.ID, err)
	}
	observation := integrityTestObservation(
		"observation_"+occurrenceID,
		receipt.ID,
		integrityTestDigest("projection-ordering:"+occurrenceID),
	)
	observation.RepositoryScan = repositoryScan
	observation.ScanRunID = run.ID
	observation.RunUID = runUID
	observation.TargetReceiptID = targetReceiptID
	observation.RecordDigest = ""
	if mutateObservation != nil {
		mutateObservation(observation)
	}
	if _, err := s.AcceptFindingObservation(ctx, observation); err != nil {
		t.Fatalf("AcceptFindingObservation(%s) error = %v", run.ID, err)
	}
	finalization := integrityTestFinalization(
		observation,
		occurrenceID,
		"fnd_projection_ordering",
		"semantic_projection_ordering",
	)
	if observation.IdentityQuality == store.IdentityQualityProducerProposed {
		finalization.Occurrence.DiscoveryPayload = append(json.RawMessage(nil), observation.NormalizedPayload...)
		finalization.Occurrence.PayloadDigest = observation.PayloadDigest
	}
	finalization.Projection.Title = projectionTitle
	finalization.Projection.Summary = projectionTitle + " summary"
	return finalization
}

func finalizeFindingProjectionRun(
	t *testing.T,
	s *Store,
	run findingProjectionRunFixture,
	occurrenceID string,
	projectionTitle string,
) *store.FindingOccurrenceFinalization {
	t.Helper()
	finalization := prepareFindingProjectionRun(t, s, run, occurrenceID, projectionTitle, nil)
	created, err := s.FinalizeFindingOccurrence(context.Background(), finalization)
	if err != nil || !created {
		t.Fatalf("FinalizeFindingOccurrence(%s) = (%v, %v)", run.ID, created, err)
	}
	return finalization
}

func assertFindingProjectionRun(
	t *testing.T,
	s *Store,
	wantRunID string,
	wantOccurrenceID string,
	wantTitle string,
	wantState string,
	wantDecisionVersion int64,
) {
	t.Helper()
	finding, err := s.GetFinding(context.Background(), "ns1", "fnd_projection_ordering")
	if err != nil {
		t.Fatalf("GetFinding() error = %v", err)
	}
	if finding.ScanRunID != wantRunID || finding.CurrentOccurrenceID != wantOccurrenceID || finding.Title != wantTitle ||
		finding.State != wantState || finding.DecisionVersion != wantDecisionVersion {
		t.Fatalf("finding projection = %#v, want run=%q occurrence=%q title=%q state=%q decisionVersion=%d",
			finding, wantRunID, wantOccurrenceID, wantTitle, wantState, wantDecisionVersion)
	}
}

func TestFindingProjectionRejectsDelayedOlderGenerationAndRetainsOccurrence(t *testing.T) {
	s := setupTestStore(t)
	started := time.Date(2026, time.August, 2, 9, 0, 0, 0, time.UTC)
	newer := finalizeFindingProjectionRun(t, s, findingProjectionRunFixture{
		ID:                       "scan_generation_newer",
		RepositoryScanUID:        "repository-scan-uid-generation",
		RepositoryScanGeneration: 2,
		StartedAt:                started,
	}, "occurrence_generation_newer", "newer generation")
	if _, err := s.db.ExecContext(context.Background(), `UPDATE security_findings
		SET state = 'false_positive', decision_version = 4 WHERE namespace = ? AND id = ?`,
		"ns1", newer.Projection.ID); err != nil {
		t.Fatalf("seed decided projection: %v", err)
	}

	stale := finalizeFindingProjectionRun(t, s, findingProjectionRunFixture{
		ID:                       "scan_generation_stale",
		RepositoryScanUID:        "repository-scan-uid-generation",
		RepositoryScanGeneration: 1,
		StartedAt:                started.Add(2 * time.Hour),
	}, "occurrence_generation_stale", "stale generation")

	assertFindingProjectionRun(t, s, newer.Projection.ScanRunID, newer.Occurrence.ID, "newer generation", "false_positive", 4)
	if got, err := s.GetFindingOccurrence(context.Background(), "ns1", stale.Occurrence.ID); err != nil ||
		got.ScanRunID != stale.Occurrence.ScanRunID || len(got.ObservationLinks) != 1 ||
		got.ObservationLinks[0].ObservationID != stale.ObservationLinks[0].ObservationID {
		t.Fatalf("stale occurrence retention = (%#v, %v)", got, err)
	}
	occurrences, _, err := s.ListFindingOccurrences(context.Background(), store.FindingOccurrenceFilter{
		Namespace: "ns1", PublicFindingID: newer.Projection.ID, Limit: 10,
	})
	if err != nil || len(occurrences) != 2 {
		t.Fatalf("ListFindingOccurrences() = (%#v, %v), want two immutable occurrences", occurrences, err)
	}
}

func TestFindingProjectionValidatesIdentityBeforeStaleRunOrdering(t *testing.T) {
	started := time.Date(2026, time.August, 2, 9, 30, 0, 0, time.UTC)
	tests := []struct {
		name                string
		staleRepositoryScan string
		mutateObservation   func(*store.FindingObservation, *store.FindingOccurrenceFinalization)
		mutateFinalization  func(*store.FindingOccurrenceFinalization)
		wantAliasRollback   bool
	}{
		{
			name:                "repository mismatch",
			staleRepositoryScan: "repo2",
			wantAliasRollback:   true,
		},
		{
			name: "fingerprint mismatch",
			mutateFinalization: func(finalization *store.FindingOccurrenceFinalization) {
				finalization.Projection.Fingerprint = "different-compat-fingerprint"
			},
		},
		{
			name: "semantic fingerprint mismatch",
			mutateObservation: func(observation *store.FindingObservation, _ *store.FindingOccurrenceFinalization) {
				observation.IdentityAnchor = "different-handler-exec"
			},
			wantAliasRollback: true,
		},
		{
			name: "canonical identity downgrade",
			mutateObservation: func(observation *store.FindingObservation, current *store.FindingOccurrenceFinalization) {
				observation.IdentityQuality = store.IdentityQualityProducerProposed
				observation.SemanticFingerprint = current.Occurrence.SemanticFingerprint
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := setupTestStore(t)
			current := finalizeFindingProjectionRun(t, s, findingProjectionRunFixture{
				ID:                       "scan_identity_current",
				RepositoryScanUID:        "repository-scan-uid-identity",
				RepositoryScanGeneration: 2,
				StartedAt:                started,
			}, "occurrence_identity_current", "current identity")

			stale := prepareFindingProjectionRun(t, s, findingProjectionRunFixture{
				ID:                       "scan_identity_stale",
				RepositoryScan:           tt.staleRepositoryScan,
				RepositoryScanUID:        "repository-scan-uid-identity",
				RepositoryScanGeneration: 1,
				StartedAt:                started.Add(time.Hour),
			}, "occurrence_identity_stale", "stale conflicting identity", func(observation *store.FindingObservation) {
				if tt.mutateObservation != nil {
					tt.mutateObservation(observation, current)
				}
			})
			if tt.mutateFinalization != nil {
				tt.mutateFinalization(stale)
			}

			created, err := s.FinalizeFindingOccurrence(context.Background(), stale)
			if !errors.Is(err, store.ErrDuplicateMismatch) || created {
				t.Fatalf("FinalizeFindingOccurrence(stale identity conflict) = (%v, %v), want (false, ErrDuplicateMismatch)", created, err)
			}
			assertFindingProjectionRun(t, s, current.Projection.ScanRunID, current.Occurrence.ID, "current identity", "open", 0)
			if _, err := s.GetFindingOccurrence(context.Background(), "ns1", stale.Occurrence.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetFindingOccurrence(rolled back stale conflict) error = %v, want ErrNotFound", err)
			}
			var linkCount int
			if err := s.db.QueryRowContext(context.Background(), `SELECT COUNT(*)
				FROM security_finding_occurrence_observations WHERE namespace = ? AND occurrence_id = ?`,
				"ns1", stale.Occurrence.ID).Scan(&linkCount); err != nil {
				t.Fatalf("count rolled back occurrence links: %v", err)
			}
			if linkCount != 0 {
				t.Fatalf("rolled back stale conflict retained %d occurrence links", linkCount)
			}
			if tt.wantAliasRollback {
				if _, err := s.GetFindingAlias(context.Background(), stale.Occurrence.Namespace,
					stale.Occurrence.RepositoryScan, stale.Occurrence.SemanticFingerprint); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("GetFindingAlias(rolled back stale conflict) error = %v, want ErrNotFound", err)
				}
			}
		})
	}
}

func TestFindingProjectionRejectsCanonicalIdentityAlgorithmVersionChange(t *testing.T) {
	s := setupTestStore(t)
	started := time.Date(2026, time.August, 2, 9, 30, 0, 0, time.UTC)
	current := finalizeFindingProjectionRun(t, s, findingProjectionRunFixture{
		ID:                       "scan_algorithm_current",
		RepositoryScanUID:        "repository-scan-uid-algorithm",
		RepositoryScanGeneration: 1,
		StartedAt:                started,
	}, "occurrence_algorithm_current", "current algorithm")
	if _, err := s.db.ExecContext(context.Background(), `UPDATE security_findings
		SET identity_algorithm_version = 'semantic-v0' WHERE namespace = ? AND id = ?`,
		current.Projection.Namespace, current.Projection.ID); err != nil {
		t.Fatalf("seed prior canonical algorithm version: %v", err)
	}

	incoming := prepareFindingProjectionRun(t, s, findingProjectionRunFixture{
		ID:                       "scan_algorithm_incoming",
		RepositoryScanUID:        "repository-scan-uid-algorithm",
		RepositoryScanGeneration: 2,
		StartedAt:                started.Add(time.Hour),
	}, "occurrence_algorithm_incoming", "incoming algorithm", func(observation *store.FindingObservation) {
		observation.SemanticFingerprint = current.Occurrence.SemanticFingerprint
	})
	created, err := s.FinalizeFindingOccurrence(context.Background(), incoming)
	if !errors.Is(err, store.ErrDuplicateMismatch) || created {
		t.Fatalf("FinalizeFindingOccurrence(algorithm change) = (%v, %v), want (false, ErrDuplicateMismatch)", created, err)
	}
	if _, err := s.GetFindingOccurrence(context.Background(), "ns1", incoming.Occurrence.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetFindingOccurrence(rolled back algorithm change) error = %v, want ErrNotFound", err)
	}
	got, err := s.GetFinding(context.Background(), current.Projection.Namespace, current.Projection.ID)
	if err != nil {
		t.Fatalf("GetFinding() error = %v", err)
	}
	if got.IdentityAlgorithmVersion != "semantic-v0" || got.ScanRunID != current.Projection.ScanRunID ||
		got.CurrentOccurrenceID != current.Occurrence.ID {
		t.Fatalf("canonical projection changed after rejected algorithm version: %#v", got)
	}
}

func TestFindingProjectionOrdersRecreatedRepositoryScanByStartTime(t *testing.T) {
	s := setupTestStore(t)
	started := time.Date(2026, time.August, 2, 10, 0, 0, 0, time.UTC)
	finalizeFindingProjectionRun(t, s, findingProjectionRunFixture{
		ID:                       "scan_recreated_old",
		RepositoryScanUID:        "repository-scan-uid-old",
		RepositoryScanGeneration: 99,
		StartedAt:                started,
	}, "occurrence_recreated_old", "old incarnation")
	newer := finalizeFindingProjectionRun(t, s, findingProjectionRunFixture{
		ID:                       "scan_recreated_new",
		RepositoryScanUID:        "repository-scan-uid-new",
		RepositoryScanGeneration: 1,
		StartedAt:                started.Add(time.Hour),
	}, "occurrence_recreated_new", "new incarnation")
	assertFindingProjectionRun(t, s, newer.Projection.ScanRunID, newer.Occurrence.ID, "new incarnation", "open", 0)

	delayed := finalizeFindingProjectionRun(t, s, findingProjectionRunFixture{
		ID:                       "scan_recreated_delayed",
		RepositoryScanUID:        "repository-scan-uid-old",
		RepositoryScanGeneration: 100,
		StartedAt:                started.Add(30 * time.Minute),
	}, "occurrence_recreated_delayed", "delayed old incarnation")
	assertFindingProjectionRun(t, s, newer.Projection.ScanRunID, newer.Occurrence.ID, "new incarnation", "open", 0)
	if _, err := s.GetFindingOccurrence(context.Background(), "ns1", delayed.Occurrence.ID); err != nil {
		t.Fatalf("GetFindingOccurrence(delayed recreated UID) error = %v", err)
	}
}

func TestFindingProjectionOrdersSameGenerationByStartTimeThenRunID(t *testing.T) {
	s := setupTestStore(t)
	started := time.Date(2026, time.August, 2, 11, 0, 0, 0, time.UTC)
	base := finalizeFindingProjectionRun(t, s, findingProjectionRunFixture{
		ID:                       "scan_same_generation_a",
		RepositoryScanUID:        "repository-scan-uid-same-generation",
		RepositoryScanGeneration: 7,
		StartedAt:                started,
	}, "occurrence_same_generation_a", "same generation base")
	if _, err := s.db.ExecContext(context.Background(), `UPDATE security_findings
		SET state = 'fixed', decision_version = 6 WHERE namespace = ? AND id = ?`, "ns1", base.Projection.ID); err != nil {
		t.Fatalf("seed decided projection: %v", err)
	}

	later := finalizeFindingProjectionRun(t, s, findingProjectionRunFixture{
		ID:                       "scan_same_generation_b",
		RepositoryScanUID:        "repository-scan-uid-same-generation",
		RepositoryScanGeneration: 7,
		StartedAt:                started.Add(time.Hour),
	}, "occurrence_same_generation_b", "same generation later")
	assertFindingProjectionRun(t, s, later.Projection.ScanRunID, later.Occurrence.ID, "same generation later", "open", 6)

	lowerTie := finalizeFindingProjectionRun(t, s, findingProjectionRunFixture{
		ID:                       "scan_same_generation_aa",
		RepositoryScanUID:        "repository-scan-uid-same-generation",
		RepositoryScanGeneration: 7,
		StartedAt:                started.Add(time.Hour),
	}, "occurrence_same_generation_aa", "same generation lower tie")
	assertFindingProjectionRun(t, s, later.Projection.ScanRunID, later.Occurrence.ID, "same generation later", "open", 6)
	if _, err := s.GetFindingOccurrence(context.Background(), "ns1", lowerTie.Occurrence.ID); err != nil {
		t.Fatalf("GetFindingOccurrence(lower run ID tie) error = %v", err)
	}

	higherTie := finalizeFindingProjectionRun(t, s, findingProjectionRunFixture{
		ID:                       "scan_same_generation_z",
		RepositoryScanUID:        "repository-scan-uid-same-generation",
		RepositoryScanGeneration: 7,
		StartedAt:                started.Add(time.Hour),
	}, "occurrence_same_generation_z", "same generation higher tie")
	assertFindingProjectionRun(t, s, higherTie.Projection.ScanRunID, higherTie.Occurrence.ID, "same generation higher tie", "open", 6)
}

func TestFindingProjectionRejectsLegacyRunAfterFullyBoundProjectionAndAllowsReplay(t *testing.T) {
	s := setupTestStore(t)
	started := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	bound := finalizeFindingProjectionRun(t, s, findingProjectionRunFixture{
		ID:                       "scan_bound_projection",
		RepositoryScanUID:        "repository-scan-uid-bound",
		RepositoryScanGeneration: 3,
		StartedAt:                started,
	}, "occurrence_bound_projection", "fully bound projection")
	legacy := finalizeFindingProjectionRun(t, s, findingProjectionRunFixture{
		ID:        "scan_legacy_projection",
		StartedAt: started.Add(24 * time.Hour),
	}, "occurrence_legacy_projection", "legacy projection")
	assertFindingProjectionRun(t, s, bound.Projection.ScanRunID, bound.Occurrence.ID, "fully bound projection", "open", 0)
	if _, err := s.GetFindingOccurrence(context.Background(), "ns1", legacy.Occurrence.ID); err != nil {
		t.Fatalf("GetFindingOccurrence(legacy loser) error = %v", err)
	}

	created, err := s.FinalizeFindingOccurrence(context.Background(), bound)
	if err != nil || created {
		t.Fatalf("FinalizeFindingOccurrence(same-run replay) = (%v, %v), want (false, nil)", created, err)
	}
}
