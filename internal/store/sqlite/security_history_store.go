package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/store"
)

const (
	findingHistoryDefaultLimit = 50
	findingHistoryMaxLimit     = 500

	findingProjectionStateOpen  = "open"
	assessmentStatusPending     = "pending"
	assessmentStatusValidated   = "validated"
	assessmentStatusFailed      = "failed"
	assessmentStatusSkipped     = "skipped"
	assessmentStatusUnvalidated = "unvalidated"
	assessmentOutcomeConfirmed  = "confirmed"
	securityScopeOccurrence     = "occurrence"
)

func boundedFindingHistoryLimit(limit int) int {
	if limit <= 0 {
		return findingHistoryDefaultLimit
	}
	if limit > findingHistoryMaxLimit {
		return findingHistoryMaxLimit
	}
	return limit
}

func normalizeOccurrenceLinks(links []store.FindingOccurrenceObservation) ([]store.FindingOccurrenceObservation, error) {
	if len(links) == 0 {
		return nil, store.ValidationErrorf("observationLinks must not be empty")
	}
	if len(links) > maxOccurrenceObservationLinks {
		return nil, store.ValidationErrorf("observationLinks exceeds %d items", maxOccurrenceObservationLinks)
	}
	out := append([]store.FindingOccurrenceObservation(nil), links...)
	seen := make(map[string]struct{}, len(out))
	for i := range out {
		out[i].ObservationID = strings.TrimSpace(out[i].ObservationID)
		out[i].Relationship = strings.TrimSpace(out[i].Relationship)
		if err := validateSecurityID(out[i].ObservationID, "observationID", true); err != nil {
			return nil, err
		}
		if out[i].Relationship != store.FindingObservationRelationshipContributor &&
			out[i].Relationship != store.FindingObservationRelationshipAbsorbed {
			return nil, store.ValidationErrorf("unsupported observation relationship %q", out[i].Relationship)
		}
		if out[i].Ordinal < 0 {
			return nil, store.ValidationErrorf("observation link ordinal must be non-negative")
		}
		if _, ok := seen[out[i].ObservationID]; ok {
			return nil, store.ValidationErrorf("duplicate observation link %q", out[i].ObservationID)
		}
		seen[out[i].ObservationID] = struct{}{}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ordinal != out[j].Ordinal {
			return out[i].Ordinal < out[j].Ordinal
		}
		return out[i].ObservationID < out[j].ObservationID
	})
	return out, nil
}

func normalizeFindingOccurrence(occurrence *store.FindingOccurrence) error {
	if occurrence == nil {
		return store.ValidationErrorf("finding occurrence is required")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"id", occurrence.ID},
		{"namespace", occurrence.Namespace},
		{"repositoryScan", occurrence.RepositoryScan},
		{"scanRunID", occurrence.ScanRunID},
		{"runUID", occurrence.RunUID},
		{"publicFindingID", occurrence.PublicFindingID},
		{"semanticFindingID", occurrence.SemanticFindingID},
	} {
		if err := validateSecurityID(field.value, field.name, true); err != nil {
			return err
		}
	}
	if err := validateSecurityRunUID(occurrence.RunUID, true); err != nil {
		return err
	}
	if err := validateSecurityID(occurrence.TargetReceiptID, "targetReceiptID", false); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"identityAlgorithmVersion", occurrence.IdentityAlgorithmVersion},
		{"legacyFingerprint", occurrence.LegacyFingerprint},
		{"ruleID", occurrence.RuleID},
		{"identityAnchor", occurrence.IdentityAnchor},
		{"identityInstance", occurrence.IdentityInstance},
		{"targetSHA", occurrence.TargetSHA},
	} {
		if err := validateSecurityText(field.value, field.name, maxSecurityCoordinateBytes, field.name == "identityAlgorithmVersion"); err != nil {
			return err
		}
	}
	if occurrence.IdentityQuality != store.IdentityQualityCanonical &&
		occurrence.IdentityQuality != store.IdentityQualityProducerProposed &&
		occurrence.IdentityQuality != store.IdentityQualityLegacy {
		return store.ValidationErrorf("unsupported identityQuality %q", occurrence.IdentityQuality)
	}
	var err error
	occurrence.SemanticFingerprint, err = normalizeSecurityDigest(occurrence.SemanticFingerprint, true, "semanticFingerprint")
	if err != nil {
		return err
	}
	occurrence.DiscoveryPayload, occurrence.PayloadDigest, err = normalizeSecurityPayload(
		occurrence.DiscoveryPayload, occurrence.PayloadDigest, true, "discoveryPayload",
	)
	return err
}

func normalizeOccurrenceProjection(finalization *store.FindingOccurrenceFinalization) error {
	if finalization == nil {
		return store.ValidationErrorf("occurrence finalization is required")
	}
	occurrence := &finalization.Occurrence
	projection := &finalization.Projection
	if projection.ID == "" {
		projection.ID = occurrence.PublicFindingID
	}
	if projection.Namespace == "" {
		projection.Namespace = occurrence.Namespace
	}
	if projection.RepositoryScan == "" {
		projection.RepositoryScan = occurrence.RepositoryScan
	}
	if projection.ScanRunID == "" {
		projection.ScanRunID = occurrence.ScanRunID
	}
	projection.ValidationStatus = assessmentStatusUnvalidated
	projection.ValidationJSON = ""
	projection.State = findingProjectionStateOpen
	projection.DecisionVersion = 0
	projection.IdentityQuality = occurrence.IdentityQuality
	projection.IdentityAlgorithmVersion = occurrence.IdentityAlgorithmVersion
	projection.SemanticFingerprint = occurrence.SemanticFingerprint
	projection.LegacyFingerprint = occurrence.LegacyFingerprint
	projection.HistoryStatus = store.FindingHistoryCanonical
	projection.CurrentOccurrenceID = occurrence.ID
	projection.CommitSHA = occurrence.TargetSHA
	if projection.ID != occurrence.PublicFindingID || projection.Namespace != occurrence.Namespace ||
		projection.RepositoryScan != occurrence.RepositoryScan || projection.ScanRunID != occurrence.ScanRunID {
		return store.ValidationErrorf("finding projection does not match occurrence binding")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"projection.id", projection.ID},
		{"projection.namespace", projection.Namespace},
		{"projection.repositoryScan", projection.RepositoryScan},
		{"projection.scanRunID", projection.ScanRunID},
	} {
		if err := validateSecurityID(field.value, field.name, true); err != nil {
			return err
		}
	}
	if err := sanitizeFindingProjection(projection); err != nil {
		return err
	}
	if err := validateSecurityText(projection.Fingerprint, "projection.fingerprint", maxSecurityCoordinateBytes, true); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"projection.title", projection.Title},
		{"projection.summary", projection.Summary},
		{"projection.severity", projection.Severity},
		{"projection.confidence", projection.Confidence},
	} {
		if err := validateSecurityText(field.value, field.name, maxSecuritySummaryBytes, true); err != nil {
			return err
		}
	}
	return normalizeFindingIntegrityFields(projection)
}

func occurrenceFinalizationDigest(finalization store.FindingOccurrenceFinalization) (string, error) {
	finalization.Occurrence.RecordDigest = ""
	finalization.Occurrence.CreatedAt = time.Time{}
	finalization.Occurrence.ObservationLinks = append([]store.FindingOccurrenceObservation(nil), finalization.ObservationLinks...)
	finalization.Projection.CreatedAt = time.Time{}
	finalization.Projection.UpdatedAt = time.Time{}
	finalization.Projection.DecisionVersion = 0
	return securityRecordDigest(finalization)
}

// FinalizeFindingOccurrence atomically inserts immutable occurrence history, links, alias, and projection.
//
//nolint:gocyclo // transactional finalization validates each immutable boundary
func (s *Store) FinalizeFindingOccurrence(ctx context.Context, finalization *store.FindingOccurrenceFinalization) (bool, error) {
	if finalization == nil {
		return false, store.ValidationErrorf("occurrence finalization is required")
	}
	if err := normalizeFindingOccurrence(&finalization.Occurrence); err != nil {
		return false, err
	}
	links, err := normalizeOccurrenceLinks(finalization.ObservationLinks)
	if err != nil {
		return false, err
	}
	finalization.ObservationLinks = links
	finalization.Occurrence.ObservationLinks = append([]store.FindingOccurrenceObservation(nil), links...)
	if err := normalizeOccurrenceProjection(finalization); err != nil {
		return false, err
	}
	digest, err := occurrenceFinalizationDigest(*finalization)
	if err != nil {
		return false, err
	}
	occurrence := &finalization.Occurrence
	if occurrence.CreatedAt.IsZero() {
		occurrence.CreatedAt = time.Now().UTC()
	} else {
		occurrence.CreatedAt = occurrence.CreatedAt.UTC()
	}
	occurrence.RecordDigest = digest

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var existingDigest string
	err = tx.QueryRowContext(ctx, `SELECT record_digest FROM security_finding_occurrences WHERE id = ?`, occurrence.ID).Scan(&existingDigest)
	if err == nil {
		return immutableReplayResult(existingDigest, digest, "finding occurrence", occurrence.ID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var runRepositoryScan, authoritativeRunUID, authoritativeTargetReceiptID, authoritativeHeadSHA string
	err = tx.QueryRowContext(ctx, `SELECT repository_scan, run_uid, target_receipt_id, head_commit
		FROM security_scan_runs WHERE namespace = ? AND id = ?`, occurrence.Namespace, occurrence.ScanRunID).
		Scan(&runRepositoryScan, &authoritativeRunUID, &authoritativeTargetReceiptID, &authoritativeHeadSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return false, store.ValidationErrorf("scan run %q does not exist", occurrence.ScanRunID)
	}
	if err != nil {
		return false, err
	}
	if runRepositoryScan != occurrence.RepositoryScan || authoritativeRunUID != occurrence.RunUID ||
		authoritativeTargetReceiptID != occurrence.TargetReceiptID || authoritativeHeadSHA != occurrence.TargetSHA {
		return false, store.ValidationErrorf("finalized occurrence target does not match authoritative scan run")
	}

	var proposedPayload []byte
	var proposedPayloadDigest string
	for _, link := range links {
		var namespace, repositoryScan, scanRunID, runUID, disposition, semanticFingerprint string
		var stageReceiptID, targetReceiptID string
		var identityQuality, identityAlgorithmVersion, legacyFingerprint, ruleID, identityAnchor, identityInstance string
		var normalizedPayload []byte
		var payloadDigest string
		err := tx.QueryRowContext(ctx, `SELECT namespace, repository_scan, scan_run_id, run_uid, stage_receipt_id,
			disposition, semantic_fingerprint, target_receipt_id, identity_quality, identity_algorithm_version,
			legacy_fingerprint, rule_id, identity_anchor, identity_instance, normalized_payload, payload_digest
			FROM security_finding_observations WHERE id = ?`, link.ObservationID).
			Scan(&namespace, &repositoryScan, &scanRunID, &runUID, &stageReceiptID, &disposition, &semanticFingerprint,
				&targetReceiptID, &identityQuality, &identityAlgorithmVersion, &legacyFingerprint, &ruleID, &identityAnchor,
				&identityInstance, &normalizedPayload, &payloadDigest)
		if errors.Is(err, sql.ErrNoRows) {
			return false, store.ValidationErrorf("observation %q does not exist", link.ObservationID)
		}
		if err != nil {
			return false, err
		}
		if namespace != occurrence.Namespace || repositoryScan != occurrence.RepositoryScan ||
			scanRunID != occurrence.ScanRunID || runUID != occurrence.RunUID ||
			disposition != string(store.FindingObservationAccepted) || semanticFingerprint != occurrence.SemanticFingerprint ||
			targetReceiptID != occurrence.TargetReceiptID ||
			identityQuality != occurrence.IdentityQuality || identityAlgorithmVersion != occurrence.IdentityAlgorithmVersion ||
			legacyFingerprint != occurrence.LegacyFingerprint || ruleID != occurrence.RuleID ||
			identityAnchor != occurrence.IdentityAnchor || identityInstance != occurrence.IdentityInstance {
			return false, store.ValidationErrorf("observation %q does not match finalized occurrence", link.ObservationID)
		}
		if occurrence.IdentityQuality == store.IdentityQualityProducerProposed {
			if payloadDigest != securityDigestBytes(normalizedPayload) {
				return false, store.ValidationErrorf("observation %q payload digest does not match normalized payload", link.ObservationID)
			}
			if proposedPayloadDigest == "" {
				proposedPayload = append([]byte(nil), normalizedPayload...)
				proposedPayloadDigest = payloadDigest
			} else if payloadDigest != proposedPayloadDigest || !bytes.Equal(normalizedPayload, proposedPayload) {
				return false, store.ValidationErrorf("producer-proposed observations must use one exact normalized payload and digest")
			}
		}
		var receiptNamespace, receiptRepositoryScan, receiptScanRunID, receiptRunUID, receiptTargetReceiptID, receiptDisposition string
		var receiptExpectedTargetSHA, receiptObservedTargetSHA string
		err = tx.QueryRowContext(ctx, `SELECT namespace, repository_scan, scan_run_id, run_uid, target_receipt_id,
			expected_target_sha, observed_target_sha, disposition FROM security_stage_receipts WHERE id = ?`, stageReceiptID).
			Scan(&receiptNamespace, &receiptRepositoryScan, &receiptScanRunID, &receiptRunUID, &receiptTargetReceiptID,
				&receiptExpectedTargetSHA, &receiptObservedTargetSHA, &receiptDisposition)
		if errors.Is(err, sql.ErrNoRows) {
			return false, store.ValidationErrorf("stage receipt %q does not exist", stageReceiptID)
		}
		if err != nil {
			return false, err
		}
		if receiptNamespace != occurrence.Namespace || receiptRepositoryScan != occurrence.RepositoryScan ||
			receiptScanRunID != occurrence.ScanRunID || receiptRunUID != occurrence.RunUID ||
			receiptTargetReceiptID != occurrence.TargetReceiptID || receiptExpectedTargetSHA != occurrence.TargetSHA ||
			receiptDisposition != string(store.StageReceiptAccepted) ||
			(receiptObservedTargetSHA != "" && receiptObservedTargetSHA != occurrence.TargetSHA) {
			return false, store.ValidationErrorf("stage receipt %q target does not match finalized occurrence", stageReceiptID)
		}
	}
	if occurrence.IdentityQuality == store.IdentityQualityProducerProposed &&
		(occurrence.PayloadDigest != proposedPayloadDigest || !bytes.Equal(occurrence.DiscoveryPayload, proposedPayload)) {
		return false, store.ValidationErrorf("producer-proposed occurrence payload must exactly match linked observations")
	}
	if occurrence.TargetReceiptID != "" {
		var targetNamespace, targetRepositoryScan, targetScanRunID, targetRunUID, targetHeadSHA string
		err := tx.QueryRowContext(ctx, `SELECT namespace, repository_scan, scan_run_id, run_uid, head_sha
			FROM security_target_receipts WHERE id = ?`, occurrence.TargetReceiptID).
			Scan(&targetNamespace, &targetRepositoryScan, &targetScanRunID, &targetRunUID, &targetHeadSHA)
		if errors.Is(err, sql.ErrNoRows) {
			return false, store.ValidationErrorf("target receipt %q does not exist", occurrence.TargetReceiptID)
		}
		if err != nil {
			return false, err
		}
		if targetNamespace != occurrence.Namespace || targetRepositoryScan != occurrence.RepositoryScan ||
			targetScanRunID != occurrence.ScanRunID || targetRunUID != occurrence.RunUID || targetHeadSHA != occurrence.TargetSHA {
			return false, store.ValidationErrorf("target receipt does not match finalized occurrence")
		}
	}

	if _, err := validateFindingProjectionIdentityTx(ctx, tx, &finalization.Projection); err != nil {
		return false, err
	}

	aliasMissing := false
	if occurrence.IdentityQuality == store.IdentityQualityCanonical {
		var aliasPublicID, aliasSemanticID string
		err = tx.QueryRowContext(ctx, `SELECT public_finding_id, semantic_finding_id FROM security_finding_aliases
			WHERE namespace = ? AND repository_scan = ? AND semantic_fingerprint = ?`,
			occurrence.Namespace, occurrence.RepositoryScan, occurrence.SemanticFingerprint).
			Scan(&aliasPublicID, &aliasSemanticID)
		switch {
		case err == nil && (aliasPublicID != occurrence.PublicFindingID || aliasSemanticID != occurrence.SemanticFindingID):
			return false, fmt.Errorf("%w: semantic finding alias conflicts with existing public finding", store.ErrDuplicateMismatch)
		case errors.Is(err, sql.ErrNoRows):
			aliasMissing = true
		case err != nil:
			return false, err
		}
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO security_finding_occurrences
		(id, namespace, repository_scan, scan_run_id, run_uid, public_finding_id, semantic_finding_id,
		 semantic_fingerprint, identity_quality, identity_algorithm_version, legacy_fingerprint, rule_id,
		 identity_anchor, identity_instance, target_receipt_id, target_sha, discovery_payload, payload_digest,
		 record_digest, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		occurrence.ID, occurrence.Namespace, occurrence.RepositoryScan, occurrence.ScanRunID, occurrence.RunUID,
		occurrence.PublicFindingID, occurrence.SemanticFindingID, occurrence.SemanticFingerprint,
		occurrence.IdentityQuality, occurrence.IdentityAlgorithmVersion, occurrence.LegacyFingerprint,
		occurrence.RuleID, occurrence.IdentityAnchor, occurrence.IdentityInstance, occurrence.TargetReceiptID,
		occurrence.TargetSHA, securityPayloadBytes(occurrence.DiscoveryPayload), occurrence.PayloadDigest, occurrence.RecordDigest,
		occurrence.CreatedAt,
	); err != nil {
		return false, err
	}
	if aliasMissing {
		if _, err := tx.ExecContext(ctx, `INSERT INTO security_finding_aliases
			(namespace, repository_scan, semantic_fingerprint, semantic_finding_id, public_finding_id,
			 identity_quality, identity_algorithm_version, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			occurrence.Namespace, occurrence.RepositoryScan, occurrence.SemanticFingerprint,
			occurrence.SemanticFindingID, occurrence.PublicFindingID, occurrence.IdentityQuality,
			occurrence.IdentityAlgorithmVersion, occurrence.CreatedAt,
		); err != nil {
			return false, err
		}
	}
	for _, link := range links {
		if _, err := tx.ExecContext(ctx, `INSERT INTO security_finding_occurrence_observations
			(namespace, occurrence_id, observation_id, relationship, ordinal, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, occurrence.Namespace, occurrence.ID, link.ObservationID,
			link.Relationship, link.Ordinal, occurrence.CreatedAt); err != nil {
			return false, err
		}
	}
	if err := upsertFindingProjectionTx(ctx, tx, &finalization.Projection); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

type findingProjectionRunOrder struct {
	id                       string
	runUID                   string
	repositoryScanUID        string
	repositoryScanGeneration int64
	startedAt                time.Time
	found                    bool
}

func loadFindingProjectionRunOrder(
	ctx context.Context,
	tx *sql.Tx,
	namespace string,
	repositoryScan string,
	runID string,
) (findingProjectionRunOrder, error) {
	order := findingProjectionRunOrder{id: runID}
	var startedAt sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT run_uid, repository_scan_uid, repository_scan_generation, started_at
		FROM security_scan_runs WHERE namespace = ? AND repository_scan = ? AND id = ?`,
		namespace, repositoryScan, runID).
		Scan(&order.runUID, &order.repositoryScanUID, &order.repositoryScanGeneration, &startedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return order, nil
	}
	if err != nil {
		return order, err
	}
	order.found = true
	if startedAt.Valid {
		order.startedAt = startedAt.Time.UTC()
	}
	return order, nil
}

func (order findingProjectionRunOrder) fullyBound() bool {
	return order.found && order.runUID != "" && order.repositoryScanUID != "" &&
		order.repositoryScanGeneration > 0 && !order.startedAt.IsZero()
}

func findingProjectionRunWins(incoming, current findingProjectionRunOrder) bool {
	if incoming.id == current.id {
		return true
	}
	incomingBound := incoming.fullyBound()
	currentBound := current.fullyBound()
	if incomingBound != currentBound {
		return incomingBound
	}
	if !incoming.found {
		return false
	}
	if !current.found {
		return true
	}
	if incoming.repositoryScanUID != "" && incoming.repositoryScanUID == current.repositoryScanUID &&
		incoming.repositoryScanGeneration != current.repositoryScanGeneration {
		return incoming.repositoryScanGeneration > current.repositoryScanGeneration
	}
	if !incoming.startedAt.Equal(current.startedAt) {
		return incoming.startedAt.After(current.startedAt)
	}
	return incoming.id > current.id
}

func findingProjectionRunWinsTx(
	ctx context.Context,
	tx *sql.Tx,
	namespace string,
	repositoryScan string,
	incomingRunID string,
	currentRunID string,
) (bool, error) {
	if incomingRunID == currentRunID {
		return true, nil
	}
	incoming, err := loadFindingProjectionRunOrder(ctx, tx, namespace, repositoryScan, incomingRunID)
	if err != nil {
		return false, err
	}
	current, err := loadFindingProjectionRunOrder(ctx, tx, namespace, repositoryScan, currentRunID)
	if err != nil {
		return false, err
	}
	return findingProjectionRunWins(incoming, current), nil
}

type findingProjectionIdentityState struct {
	repositoryScan           string
	scanRunID                string
	fingerprint              string
	semanticFingerprint      string
	identityQuality          string
	identityAlgorithmVersion string
	decisionVersion          int64
	found                    bool
}

func validateFindingProjectionIdentityTx(
	ctx context.Context,
	tx *sql.Tx,
	finding *store.Finding,
) (findingProjectionIdentityState, error) {
	var existingID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM security_findings
		WHERE namespace = ? AND repository_scan = ? AND fingerprint = ?`,
		finding.Namespace, finding.RepositoryScan, finding.Fingerprint).Scan(&existingID)
	if err == nil && existingID != finding.ID {
		return findingProjectionIdentityState{}, fmt.Errorf("%w: finding fingerprint already maps to public ID %q", store.ErrDuplicateMismatch, existingID)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return findingProjectionIdentityState{}, err
	}

	var state findingProjectionIdentityState
	err = tx.QueryRowContext(ctx, `SELECT repository_scan, scan_run_id, fingerprint, semantic_fingerprint,
		identity_quality, identity_algorithm_version, decision_version
		FROM security_findings WHERE namespace = ? AND id = ?`, finding.Namespace, finding.ID).
		Scan(&state.repositoryScan, &state.scanRunID, &state.fingerprint, &state.semanticFingerprint,
			&state.identityQuality, &state.identityAlgorithmVersion, &state.decisionVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return findingProjectionIdentityState{}, err
	}
	state.found = true
	if state.repositoryScan != finding.RepositoryScan {
		return findingProjectionIdentityState{}, fmt.Errorf("%w: public finding ID %q belongs to repository scan %q",
			store.ErrDuplicateMismatch, finding.ID, state.repositoryScan)
	}
	if state.fingerprint != finding.Fingerprint {
		return findingProjectionIdentityState{}, fmt.Errorf("%w: public finding ID %q already maps to a different fingerprint",
			store.ErrDuplicateMismatch, finding.ID)
	}
	if state.semanticFingerprint != "" && finding.SemanticFingerprint != "" &&
		state.semanticFingerprint != finding.SemanticFingerprint {
		return findingProjectionIdentityState{}, fmt.Errorf("%w: public finding ID %q already maps to semantic fingerprint %q",
			store.ErrDuplicateMismatch, finding.ID, state.semanticFingerprint)
	}
	if state.identityQuality == store.IdentityQualityCanonical && finding.IdentityQuality != store.IdentityQualityCanonical {
		return findingProjectionIdentityState{}, fmt.Errorf("%w: canonical public finding ID %q cannot be downgraded",
			store.ErrDuplicateMismatch, finding.ID)
	}
	if state.identityQuality == store.IdentityQualityCanonical &&
		state.identityAlgorithmVersion != finding.IdentityAlgorithmVersion {
		return findingProjectionIdentityState{}, fmt.Errorf("%w: canonical public finding ID %q uses identity algorithm version %q",
			store.ErrDuplicateMismatch, finding.ID, state.identityAlgorithmVersion)
	}
	return state, nil
}

func upsertFindingProjectionTx(ctx context.Context, tx *sql.Tx, finding *store.Finding) error {
	if err := sanitizeFindingProjection(finding); err != nil {
		return err
	}
	evidenceJSON, err := marshalEvidence(finding.Evidence)
	if err != nil {
		return err
	}
	existing, err := validateFindingProjectionIdentityTx(ctx, tx, finding)
	if err != nil {
		return err
	}
	switch {
	case existing.found:
		wins, orderErr := findingProjectionRunWinsTx(
			ctx, tx, finding.Namespace, finding.RepositoryScan, finding.ScanRunID, existing.scanRunID,
		)
		if orderErr != nil {
			return orderErr
		}
		if !wins {
			return nil
		}
		finding.DecisionVersion = existing.decisionVersion
		finding.UpdatedAt = time.Now().UTC()
		_, err = tx.ExecContext(ctx, `UPDATE security_findings SET
			scan_run_id = ?, slice_id = ?, identity_quality = ?, identity_algorithm_version = ?,
			semantic_fingerprint = ?, legacy_fingerprint = ?, history_status = ?, current_occurrence_id = ?,
			title = ?, category = ?, summary = ?, severity = ?, confidence = ?, triage = ?,
			validation_status = ?, state = ?, file_path = ?, line = ?,
			commit_sha = ?, root_cause = ?, reproduction = ?, remediation = ?, suggested_action = ?,
			why_tests_do_not_cover = ?, suggested_regression_test = ?, minimum_fix_scope = ?, evidence_json = ?,
			validation_json = ?, patch_proposal_id = ?, pr_number = ?, pr_url = ?,
			decision_version = ?, updated_at = ? WHERE namespace = ? AND id = ?`,
			finding.ScanRunID, finding.SliceID, finding.IdentityQuality, finding.IdentityAlgorithmVersion,
			finding.SemanticFingerprint, finding.LegacyFingerprint, finding.HistoryStatus, finding.CurrentOccurrenceID,
			finding.Title, finding.Category, finding.Summary, finding.Severity, finding.Confidence, finding.Triage,
			finding.ValidationStatus, finding.State, finding.FilePath, finding.Line, finding.CommitSHA, finding.RootCause, finding.Reproduction,
			finding.Remediation, finding.SuggestedAction, finding.WhyTestsDoNotAlreadyCoverThis,
			finding.SuggestedRegressionTest, finding.MinimumFixScope, evidenceJSON, finding.ValidationJSON,
			finding.PatchProposalID, finding.PRNumber, finding.PRURL, finding.DecisionVersion, finding.UpdatedAt,
			finding.Namespace, finding.ID,
		)
		return err
	default:
		if finding.DecisionVersion != 0 {
			return store.ValidationErrorf("new finding projection decisionVersion must be zero")
		}
		if finding.CreatedAt.IsZero() {
			finding.CreatedAt = time.Now().UTC()
		} else {
			finding.CreatedAt = finding.CreatedAt.UTC()
		}
		finding.UpdatedAt = finding.CreatedAt
		_, err = tx.ExecContext(ctx, `INSERT INTO security_findings
			(id, namespace, repository_scan, scan_run_id, slice_id, fingerprint, identity_quality,
			 identity_algorithm_version, semantic_fingerprint, legacy_fingerprint, history_status,
			 current_occurrence_id, decision_version, title, category, summary, severity, confidence, triage,
			 validation_status, state, file_path, line, commit_sha, root_cause, reproduction, remediation,
			 suggested_action, why_tests_do_not_cover, suggested_regression_test, minimum_fix_scope,
			 evidence_json, validation_json, patch_proposal_id, pr_number, pr_url, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			finding.ID, finding.Namespace, finding.RepositoryScan, finding.ScanRunID, finding.SliceID,
			finding.Fingerprint, finding.IdentityQuality, finding.IdentityAlgorithmVersion,
			finding.SemanticFingerprint, finding.LegacyFingerprint, finding.HistoryStatus,
			finding.CurrentOccurrenceID, finding.DecisionVersion, finding.Title, finding.Category,
			finding.Summary, finding.Severity, finding.Confidence, finding.Triage, finding.ValidationStatus,
			finding.State, finding.FilePath, finding.Line, finding.CommitSHA, finding.RootCause,
			finding.Reproduction, finding.Remediation, finding.SuggestedAction,
			finding.WhyTestsDoNotAlreadyCoverThis, finding.SuggestedRegressionTest, finding.MinimumFixScope,
			evidenceJSON, finding.ValidationJSON, finding.PatchProposalID, finding.PRNumber, finding.PRURL,
			finding.CreatedAt, finding.UpdatedAt,
		)
		return err
	}
}

const findingOccurrenceSelectColumns = `id, namespace, repository_scan, scan_run_id, run_uid, public_finding_id,
	semantic_finding_id, semantic_fingerprint, identity_quality, identity_algorithm_version, legacy_fingerprint,
	rule_id, identity_anchor, identity_instance, target_receipt_id, target_sha, discovery_payload, payload_digest,
	record_digest, created_at`

func scanFindingOccurrence(scanner interface{ Scan(dest ...any) error }) (*store.FindingOccurrence, error) {
	var occurrence store.FindingOccurrence
	var payload []byte
	err := scanner.Scan(
		&occurrence.ID, &occurrence.Namespace, &occurrence.RepositoryScan, &occurrence.ScanRunID,
		&occurrence.RunUID, &occurrence.PublicFindingID, &occurrence.SemanticFindingID,
		&occurrence.SemanticFingerprint, &occurrence.IdentityQuality, &occurrence.IdentityAlgorithmVersion,
		&occurrence.LegacyFingerprint, &occurrence.RuleID, &occurrence.IdentityAnchor, &occurrence.IdentityInstance,
		&occurrence.TargetReceiptID, &occurrence.TargetSHA, &payload, &occurrence.PayloadDigest,
		&occurrence.RecordDigest, &occurrence.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	occurrence.DiscoveryPayload = append(json.RawMessage(nil), payload...)
	return &occurrence, nil
}

func loadOccurrenceLinks(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, namespace, occurrenceID string) ([]store.FindingOccurrenceObservation, error) {
	rows, err := db.QueryContext(ctx, `SELECT observation_id, relationship, ordinal
		FROM security_finding_occurrence_observations WHERE namespace = ? AND occurrence_id = ?
		ORDER BY ordinal ASC, observation_id ASC`, namespace, occurrenceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	links := make([]store.FindingOccurrenceObservation, 0)
	for rows.Next() {
		var link store.FindingOccurrenceObservation
		if err := rows.Scan(&link.ObservationID, &link.Relationship, &link.Ordinal); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// GetFindingOccurrence returns one finalized occurrence and its observation links.
func (s *Store) GetFindingOccurrence(ctx context.Context, namespace, id string) (*store.FindingOccurrence, error) {
	occurrence, err := scanFindingOccurrence(s.db.QueryRowContext(ctx,
		`SELECT `+findingOccurrenceSelectColumns+` FROM security_finding_occurrences WHERE namespace = ? AND id = ?`,
		namespace, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	occurrence.ObservationLinks, err = loadOccurrenceLinks(ctx, s.db, namespace, id)
	if err != nil {
		return nil, err
	}
	return occurrence, nil
}

// ListFindingOccurrences lists finalized occurrences oldest first.
func (s *Store) ListFindingOccurrences(ctx context.Context, filter store.FindingOccurrenceFilter) ([]store.FindingOccurrence, string, error) {
	offset, err := parseOffsetCursor(filter.Cursor)
	if err != nil {
		return nil, "", err
	}
	filter.Limit = boundedFindingHistoryLimit(filter.Limit)
	query := strings.Builder{}
	query.WriteString(`SELECT ` + findingOccurrenceSelectColumns + ` FROM security_finding_occurrences WHERE namespace = ?`)
	args := []any{filter.Namespace}
	appendSecurityStringFilter(&query, &args, "repository_scan", filter.RepositoryScan)
	appendSecurityStringFilter(&query, &args, "scan_run_id", filter.ScanRunID)
	appendSecurityStringFilter(&query, &args, "public_finding_id", filter.PublicFindingID)
	appendSecurityStringFilter(&query, &args, "semantic_fingerprint", filter.SemanticFingerprint)
	query.WriteString(` ORDER BY created_at ASC, id ASC LIMIT ? OFFSET ?`)
	args = append(args, filter.Limit, offset)
	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, "", err
	}
	items := make([]store.FindingOccurrence, 0)
	for rows.Next() {
		item, err := scanFindingOccurrence(rows)
		if err != nil {
			_ = rows.Close()
			return nil, "", err
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, "", err
	}
	if err := rows.Close(); err != nil {
		return nil, "", err
	}
	for i := range items {
		items[i].ObservationLinks, err = loadOccurrenceLinks(ctx, s.db, items[i].Namespace, items[i].ID)
		if err != nil {
			return nil, "", err
		}
	}
	return items, nextOffsetCursor(offset, len(items), filter.Limit), nil
}

// GetFindingAlias resolves canonical identity to the public compatibility finding ID.
func (s *Store) GetFindingAlias(ctx context.Context, namespace, repositoryScan, semanticFingerprint string) (*store.FindingAlias, error) {
	normalized, err := normalizeSecurityDigest(semanticFingerprint, true, "semanticFingerprint")
	if err != nil {
		return nil, err
	}
	var alias store.FindingAlias
	err = s.db.QueryRowContext(ctx, `SELECT namespace, repository_scan, semantic_fingerprint,
		semantic_finding_id, public_finding_id, identity_quality, identity_algorithm_version, created_at
		FROM security_finding_aliases WHERE namespace = ? AND repository_scan = ? AND semantic_fingerprint = ?`,
		namespace, repositoryScan, normalized).Scan(
		&alias.Namespace, &alias.RepositoryScan, &alias.SemanticFingerprint, &alias.SemanticFindingID,
		&alias.PublicFindingID, &alias.IdentityQuality, &alias.IdentityAlgorithmVersion, &alias.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &alias, nil
}

func normalizeFindingAssessment(assessment *store.FindingAssessment) error {
	if assessment == nil {
		return store.ValidationErrorf("finding assessment is required")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"id", assessment.ID},
		{"namespace", assessment.Namespace},
		{"repositoryScan", assessment.RepositoryScan},
		{"scanRunID", assessment.ScanRunID},
		{"runUID", assessment.RunUID},
		{"occurrenceID", assessment.OccurrenceID},
		{"publicFindingID", assessment.PublicFindingID},
		{"stageReceiptID", assessment.StageReceiptID},
	} {
		if err := validateSecurityID(field.value, field.name, true); err != nil {
			return err
		}
	}
	if assessment.Kind != store.FindingAssessmentValidation && assessment.Kind != store.FindingAssessmentAttackPath {
		return store.ValidationErrorf("unsupported assessment kind %q", assessment.Kind)
	}
	assessment.Method = strings.TrimSpace(assessment.Method)
	assessment.Outcome = strings.TrimSpace(assessment.Outcome)
	assessment.FailureClass = strings.TrimSpace(assessment.FailureClass)
	if err := validateSecurityRunUID(assessment.RunUID, true); err != nil {
		return err
	}
	if err := validateSecurityID(assessment.TargetReceiptID, "targetReceiptID", false); err != nil {
		return err
	}
	assessment.Summary = redact.SensitiveText(assessment.Summary)
	assessment.ProofGap = redact.SensitiveText(assessment.ProofGap)
	for _, field := range []struct {
		name     string
		value    string
		maxBytes int
		required bool
	}{
		{"targetSHA", assessment.TargetSHA, maxSecurityNameBytes, false},
		{"method", assessment.Method, maxSecurityNameBytes, false},
		{"outcome", assessment.Outcome, maxSecurityNameBytes, true},
		{"failureClass", assessment.FailureClass, maxSecurityNameBytes, false},
		{"summary", assessment.Summary, maxSecuritySummaryBytes, false},
		{"proofGap", assessment.ProofGap, maxSecuritySummaryBytes, false},
	} {
		if err := validateSecurityText(field.value, field.name, field.maxBytes, field.required); err != nil {
			return err
		}
	}
	var err error
	assessment.EvidenceReceiptIDs, err = normalizeSecurityStringList(
		assessment.EvidenceReceiptIDs, "evidenceReceiptIDs", maxSecurityEvidenceReceiptIDs,
	)
	if err != nil {
		return err
	}
	requirePayload := assessment.FailureClass == ""
	assessment.NormalizedPayload, assessment.PayloadDigest, err = normalizeSecurityPayload(
		assessment.NormalizedPayload, assessment.PayloadDigest, requirePayload, "normalizedPayload",
	)
	if err != nil {
		return err
	}
	assessment.ProjectionValidationStatus = strings.TrimSpace(assessment.ProjectionValidationStatus)
	hasProjection := assessment.ProjectionValidationStatus != "" || len(assessment.ProjectionEvidence) > 0
	if hasProjection && assessment.Kind != store.FindingAssessmentValidation {
		return store.ValidationErrorf("only validation assessments may update validation or evidence projection")
	}
	if len(assessment.ProjectionEvidence) > 0 && assessment.ProjectionValidationStatus == "" {
		return store.ValidationErrorf("projectionEvidence requires projectionValidationStatus")
	}
	if assessment.ProjectionValidationStatus != "" {
		switch assessment.ProjectionValidationStatus {
		case assessmentStatusValidated, assessmentStatusFailed, assessmentStatusSkipped, assessmentStatusPending, assessmentStatusUnvalidated:
		default:
			return store.ValidationErrorf("unsupported projectionValidationStatus %q", assessment.ProjectionValidationStatus)
		}
		if len(assessment.NormalizedPayload) == 0 {
			return store.ValidationErrorf("validation projection requires normalizedPayload")
		}
		projection := &store.Finding{
			Evidence: append([]store.FindingEvidenceRef(nil), assessment.ProjectionEvidence...),
		}
		if err := sanitizeFindingProjection(projection); err != nil {
			return err
		}
		assessment.ProjectionEvidence = projection.Evidence
		if assessment.ProjectionValidationStatus == assessmentStatusValidated {
			if assessment.FailureClass != "" {
				return store.ValidationErrorf("validated projection requires a failure-free assessment payload")
			}
			switch assessment.Outcome {
			case assessmentStatusValidated, assessmentOutcomeConfirmed, "complete":
			default:
				return store.ValidationErrorf("validated projection is incompatible with assessment outcome %q", assessment.Outcome)
			}
		}
	} else {
		assessment.ProjectionEvidence = nil
	}
	return nil
}

func findingAssessmentDigest(assessment store.FindingAssessment) (string, error) {
	assessment.RecordDigest = ""
	assessment.CreatedAt = time.Time{}
	return securityRecordDigest(assessment)
}

// RecordFindingAssessment appends an immutable assessment and atomically updates its derived projection.
func (s *Store) RecordFindingAssessment(ctx context.Context, assessment *store.FindingAssessment) (bool, error) {
	if err := normalizeFindingAssessment(assessment); err != nil {
		return false, err
	}
	digest, err := findingAssessmentDigest(*assessment)
	if err != nil {
		return false, err
	}
	if assessment.CreatedAt.IsZero() {
		assessment.CreatedAt = time.Now().UTC()
	} else {
		assessment.CreatedAt = assessment.CreatedAt.UTC()
	}
	assessment.RecordDigest = digest
	evidenceJSON, err := json.Marshal(assessment.EvidenceReceiptIDs)
	if err != nil {
		return false, err
	}
	projectionEvidenceJSON, err := marshalEvidence(assessment.ProjectionEvidence)
	if err != nil {
		return false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var existingDigest string
	err = tx.QueryRowContext(ctx, `SELECT record_digest FROM security_finding_assessments WHERE id = ?`, assessment.ID).Scan(&existingDigest)
	if err == nil {
		return immutableReplayResult(existingDigest, digest, "finding assessment", assessment.ID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var occurrenceNamespace, occurrenceRepositoryScan, occurrenceScanRunID, occurrenceRunUID, publicFindingID string
	var occurrenceTargetReceiptID, occurrenceTargetSHA string
	err = tx.QueryRowContext(ctx, `SELECT namespace, repository_scan, scan_run_id, run_uid, public_finding_id,
		target_receipt_id, target_sha
		FROM security_finding_occurrences WHERE id = ?`, assessment.OccurrenceID).
		Scan(&occurrenceNamespace, &occurrenceRepositoryScan, &occurrenceScanRunID, &occurrenceRunUID, &publicFindingID,
			&occurrenceTargetReceiptID, &occurrenceTargetSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return false, store.ValidationErrorf("occurrenceID %q does not exist", assessment.OccurrenceID)
	}
	if err != nil {
		return false, err
	}
	if occurrenceNamespace != assessment.Namespace || occurrenceRepositoryScan != assessment.RepositoryScan ||
		occurrenceScanRunID != assessment.ScanRunID || occurrenceRunUID != assessment.RunUID ||
		publicFindingID != assessment.PublicFindingID {
		return false, store.ValidationErrorf("assessment binding does not match occurrence")
	}
	if assessment.TargetReceiptID != occurrenceTargetReceiptID || assessment.TargetSHA != occurrenceTargetSHA {
		return false, store.ValidationErrorf("assessment target binding does not match occurrence")
	}
	if err := validateAssessmentStageReceiptTx(ctx, tx, assessment); err != nil {
		return false, err
	}
	if err := validateReceiptBindingsTx(ctx, tx, assessment.Namespace, assessment.RepositoryScan,
		assessment.ScanRunID, assessment.RunUID, assessment.TargetReceiptID, assessment.TargetSHA,
		assessment.EvidenceReceiptIDs); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO security_finding_assessments
		(id, namespace, repository_scan, scan_run_id, run_uid, occurrence_id, public_finding_id, kind,
		 stage_receipt_id, target_receipt_id, target_sha, method, outcome, failure_class, summary, proof_gap,
		 evidence_receipt_ids_json, normalized_payload, payload_digest, projection_validation_status,
		 projection_evidence_json, record_digest, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		assessment.ID, assessment.Namespace, assessment.RepositoryScan, assessment.ScanRunID, assessment.RunUID,
		assessment.OccurrenceID, assessment.PublicFindingID, assessment.Kind, assessment.StageReceiptID,
		assessment.TargetReceiptID, assessment.TargetSHA, assessment.Method, assessment.Outcome,
		assessment.FailureClass, assessment.Summary, assessment.ProofGap, string(evidenceJSON),
		securityPayloadBytes(assessment.NormalizedPayload), assessment.PayloadDigest, assessment.ProjectionValidationStatus,
		projectionEvidenceJSON, assessment.RecordDigest, assessment.CreatedAt,
	); err != nil {
		return false, err
	}
	if assessment.ProjectionValidationStatus != "" {
		result, err := tx.ExecContext(ctx, `UPDATE security_findings
			SET validation_status = ?, validation_json = ?, evidence_json = ?, updated_at = ?
			WHERE namespace = ? AND id = ? AND current_occurrence_id = ?`,
			assessment.ProjectionValidationStatus, string(assessment.NormalizedPayload), projectionEvidenceJSON, assessment.CreatedAt,
			assessment.Namespace, assessment.PublicFindingID, assessment.OccurrenceID,
		)
		if err != nil {
			return false, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		if affected == 0 {
			return false, fmt.Errorf("%w: assessment occurrence is not the current materialized occurrence", store.ErrConflict)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func validateAssessmentStageReceiptTx(ctx context.Context, tx *sql.Tx, assessment *store.FindingAssessment) error {
	var namespace, repositoryScan, scanRunID, runUID, stage, scopeKind, scopeID, disposition string
	var targetReceiptID, expectedTargetSHA, observedTargetSHA, normalizedOutputDigest string
	err := tx.QueryRowContext(ctx, `SELECT namespace, repository_scan, scan_run_id, run_uid, stage, scope_kind,
		scope_id, disposition, target_receipt_id, expected_target_sha, observed_target_sha, normalized_output_digest
		FROM security_stage_receipts WHERE id = ?`, assessment.StageReceiptID).
		Scan(&namespace, &repositoryScan, &scanRunID, &runUID, &stage, &scopeKind, &scopeID, &disposition,
			&targetReceiptID, &expectedTargetSHA, &observedTargetSHA, &normalizedOutputDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ValidationErrorf("receipt %q does not exist", assessment.StageReceiptID)
	}
	if err != nil {
		return err
	}
	if namespace != assessment.Namespace || repositoryScan != assessment.RepositoryScan ||
		scanRunID != assessment.ScanRunID || runUID != assessment.RunUID ||
		targetReceiptID != assessment.TargetReceiptID || expectedTargetSHA != assessment.TargetSHA {
		return store.ValidationErrorf("primary assessment receipt does not match assessment binding")
	}
	if observedTargetSHA != "" && observedTargetSHA != assessment.TargetSHA {
		return store.ValidationErrorf("primary assessment receipt observed target does not match assessment")
	}
	successfulAssessment := assessment.FailureClass == "" &&
		(assessment.Kind == store.FindingAssessmentValidation || assessment.Kind == store.FindingAssessmentAttackPath)
	strictPrimaryReceipt := assessment.Method == "validator-agent" ||
		assessment.ProjectionValidationStatus != "" || successfulAssessment
	if !strictPrimaryReceipt {
		return nil
	}
	if (successfulAssessment || assessment.PayloadDigest != "") && normalizedOutputDigest != assessment.PayloadDigest {
		return store.ValidationErrorf("assessment payload digest does not match primary stage receipt")
	}
	if stage != "validation" || scopeKind != securityScopeOccurrence || scopeID != assessment.OccurrenceID {
		return store.ValidationErrorf("primary assessment receipt is not scoped to the assessment occurrence")
	}
	expectedDisposition := string(store.StageReceiptAccepted)
	if assessment.FailureClass == "artifact_invalid" || assessment.FailureClass == "task_failed" ||
		assessment.FailureClass == "task_cancelled" {
		expectedDisposition = string(store.StageReceiptRejected)
	}
	if disposition != expectedDisposition {
		return store.ValidationErrorf("primary assessment receipt disposition %q does not match assessment outcome", disposition)
	}
	return nil
}

func validateReceiptBindingsTx(
	ctx context.Context,
	tx *sql.Tx,
	namespace, repositoryScan, scanRunID, runUID, targetReceiptID, targetSHA string,
	ids []string,
) error {
	seen := map[string]struct{}{}
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		var gotNamespace, gotRepositoryScan, gotScanRunID, gotRunUID, gotTargetReceiptID, gotExpectedTargetSHA, gotObservedTargetSHA string
		err := tx.QueryRowContext(ctx, `SELECT namespace, repository_scan, scan_run_id, run_uid, target_receipt_id,
			expected_target_sha, observed_target_sha FROM security_stage_receipts WHERE id = ?`, id).
			Scan(&gotNamespace, &gotRepositoryScan, &gotScanRunID, &gotRunUID, &gotTargetReceiptID, &gotExpectedTargetSHA,
				&gotObservedTargetSHA)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ValidationErrorf("receipt %q does not exist", id)
		}
		if err != nil {
			return err
		}
		if gotNamespace != namespace || gotRepositoryScan != repositoryScan || gotScanRunID != scanRunID ||
			gotRunUID != runUID || gotTargetReceiptID != targetReceiptID || gotExpectedTargetSHA != targetSHA {
			return store.ValidationErrorf("receipt %q does not match assessment binding", id)
		}
		if gotObservedTargetSHA != "" && gotObservedTargetSHA != targetSHA {
			return store.ValidationErrorf("receipt %q observed target does not match assessment binding", id)
		}
	}
	return nil
}

const findingAssessmentSelectColumns = `id, namespace, repository_scan, scan_run_id, run_uid, occurrence_id,
	public_finding_id, kind, stage_receipt_id, target_receipt_id, target_sha, method, outcome, failure_class,
	summary, proof_gap, evidence_receipt_ids_json, normalized_payload, payload_digest,
	projection_validation_status, projection_evidence_json, record_digest, created_at`

func scanFindingAssessment(scanner interface{ Scan(dest ...any) error }) (*store.FindingAssessment, error) {
	var assessment store.FindingAssessment
	var evidenceJSON string
	var projectionEvidenceJSON string
	var payload []byte
	err := scanner.Scan(
		&assessment.ID, &assessment.Namespace, &assessment.RepositoryScan, &assessment.ScanRunID,
		&assessment.RunUID, &assessment.OccurrenceID, &assessment.PublicFindingID, &assessment.Kind,
		&assessment.StageReceiptID, &assessment.TargetReceiptID, &assessment.TargetSHA, &assessment.Method,
		&assessment.Outcome, &assessment.FailureClass, &assessment.Summary, &assessment.ProofGap,
		&evidenceJSON, &payload, &assessment.PayloadDigest, &assessment.ProjectionValidationStatus,
		&projectionEvidenceJSON, &assessment.RecordDigest, &assessment.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(evidenceJSON), &assessment.EvidenceReceiptIDs); err != nil {
		return nil, err
	}
	assessment.ProjectionEvidence, err = unmarshalEvidence(projectionEvidenceJSON)
	if err != nil {
		return nil, err
	}
	assessment.NormalizedPayload = append(json.RawMessage(nil), payload...)
	return &assessment, nil
}

// GetFindingAssessment returns one immutable assessment.
func (s *Store) GetFindingAssessment(ctx context.Context, namespace, id string) (*store.FindingAssessment, error) {
	assessment, err := scanFindingAssessment(s.db.QueryRowContext(ctx,
		`SELECT `+findingAssessmentSelectColumns+` FROM security_finding_assessments WHERE namespace = ? AND id = ?`,
		namespace, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return assessment, err
}

// ListFindingAssessments lists immutable assessments oldest first.
func (s *Store) ListFindingAssessments(ctx context.Context, filter store.FindingAssessmentFilter) ([]store.FindingAssessment, string, error) {
	offset, err := parseOffsetCursor(filter.Cursor)
	if err != nil {
		return nil, "", err
	}
	filter.Limit = boundedFindingHistoryLimit(filter.Limit)
	query := strings.Builder{}
	query.WriteString(`SELECT ` + findingAssessmentSelectColumns + ` FROM security_finding_assessments WHERE namespace = ?`)
	args := []any{filter.Namespace}
	appendSecurityStringFilter(&query, &args, "repository_scan", filter.RepositoryScan)
	appendSecurityStringFilter(&query, &args, "occurrence_id", filter.OccurrenceID)
	appendSecurityStringFilter(&query, &args, "public_finding_id", filter.PublicFindingID)
	appendSecurityStringFilter(&query, &args, "kind", string(filter.Kind))
	query.WriteString(` ORDER BY created_at ASC, id ASC LIMIT ? OFFSET ?`)
	args = append(args, filter.Limit, offset)
	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close() //nolint:errcheck
	items := make([]store.FindingAssessment, 0)
	for rows.Next() {
		item, err := scanFindingAssessment(rows)
		if err != nil {
			return nil, "", err
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return items, nextOffsetCursor(offset, len(items), filter.Limit), nil
}

func normalizeFindingDecision(decision *store.FindingDecision) error {
	if decision == nil {
		return store.ValidationErrorf("finding decision is required")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"decisionID", decision.ID},
		{"namespace", decision.Namespace},
		{"repositoryScan", decision.RepositoryScan},
		{"publicFindingID", decision.PublicFindingID},
	} {
		if err := validateSecurityID(field.value, field.name, true); err != nil {
			return err
		}
	}
	if decision.ExpectedDecisionVersion < 0 {
		return store.ValidationErrorf("expectedDecisionVersion must be non-negative")
	}
	if decision.Scope != store.FindingDecisionOccurrence && decision.Scope != store.FindingDecisionLogicalFinding {
		return store.ValidationErrorf("unsupported decision scope %q", decision.Scope)
	}
	if decision.Scope == store.FindingDecisionOccurrence {
		if err := validateSecurityID(decision.OccurrenceID, "occurrenceID", true); err != nil {
			return err
		}
		if decision.Applicability != nil {
			return store.ValidationErrorf("occurrence decisions must not set applicability")
		}
	} else {
		return store.ValidationErrorf("logical finding decisions remain unavailable until applicability revalidation is implemented")
	}
	switch decision.Action {
	case store.FindingDecisionReopen,
		store.FindingDecisionCloseFixed,
		store.FindingDecisionCloseFalsePositive,
		store.FindingDecisionCloseWontFix,
		store.FindingDecisionSuppress:
	default:
		return store.ValidationErrorf("unsupported decision action %q", decision.Action)
	}
	reasonRequired := (decision.Action == store.FindingDecisionCloseFalsePositive ||
		decision.Action == store.FindingDecisionCloseWontFix) && decision.Source != "legacy-api"
	decision.Reason = redact.SensitiveText(decision.Reason)
	if err := validateSecurityReason(decision.ReasonCode, decision.Reason, reasonRequired); err != nil {
		return err
	}
	var err error
	decision.EvidenceReceiptIDs, err = normalizeSecurityStringList(
		decision.EvidenceReceiptIDs, "evidenceReceiptIDs", maxSecurityEvidenceReceiptIDs,
	)
	if err != nil {
		return err
	}
	if err := validateSecurityID(decision.SupersedesDecisionID, "supersedesDecisionID", false); err != nil {
		return err
	}
	for _, field := range []struct {
		name     string
		value    string
		required bool
	}{
		{"actorSubject", decision.ActorSubject, true},
		{"actorIssuer", decision.ActorIssuer, false},
		{"authenticationSource", decision.AuthenticationSource, true},
		{"source", decision.Source, false},
	} {
		if err := validateSecurityText(field.value, field.name, maxSecurityNameBytes, field.required); err != nil {
			return err
		}
	}
	decision.FeedbackEligible = strings.TrimSpace(decision.Reason) != ""
	return nil
}

func findingDecisionDigest(decision store.FindingDecision) (string, error) {
	decision.DecisionVersion = 0
	decision.RecordDigest = ""
	decision.CreatedAt = time.Time{}
	return securityRecordDigest(decision)
}

func decisionProjectionState(action store.FindingDecisionAction) string {
	switch action {
	case store.FindingDecisionReopen:
		return findingProjectionStateOpen
	case store.FindingDecisionCloseFixed:
		return "fixed"
	case store.FindingDecisionCloseFalsePositive:
		return "false_positive"
	case store.FindingDecisionCloseWontFix:
		return "dismissed"
	case store.FindingDecisionSuppress:
		return "suppressed"
	default:
		return ""
	}
}

// AppendFindingDecision appends an authenticated decision and updates the projection in one transaction.
//
//nolint:gocyclo // append-only decision transaction validates concurrency and applicability
func (s *Store) AppendFindingDecision(ctx context.Context, decision *store.FindingDecision) (*store.FindingDecision, error) {
	if err := normalizeFindingDecision(decision); err != nil {
		return nil, err
	}
	evidenceJSON, err := json.Marshal(decision.EvidenceReceiptIDs)
	if err != nil {
		return nil, err
	}
	applicabilityJSON := ""
	if decision.Applicability != nil {
		data, err := json.Marshal(decision.Applicability)
		if err != nil {
			return nil, err
		}
		applicabilityJSON = string(data)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var repositoryScan, currentOccurrenceID, currentState string
	var currentVersion int64
	err = tx.QueryRowContext(ctx, `SELECT repository_scan, decision_version, current_occurrence_id, state FROM security_findings
		WHERE namespace = ? AND id = ?`, decision.Namespace, decision.PublicFindingID).
		Scan(&repositoryScan, &currentVersion, &currentOccurrenceID, &currentState)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if repositoryScan != decision.RepositoryScan {
		return nil, store.ValidationErrorf("decision repositoryScan does not match public finding")
	}
	if existing, err := getFindingDecisionTx(ctx, tx, decision.Namespace, decision.ID); err == nil {
		decision.FeedbackEligible = existing.FeedbackEligible
		digest, digestErr := findingDecisionDigest(*decision)
		if digestErr != nil {
			return nil, digestErr
		}
		if existing.RecordDigest != digest {
			return nil, fmt.Errorf("%w: finding decision %q was replayed with different canonical content", store.ErrDuplicateMismatch, decision.ID)
		}
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if currentVersion != decision.ExpectedDecisionVersion {
		return nil, fmt.Errorf("%w: expected decision version %d, current version %d", store.ErrConflict,
			decision.ExpectedDecisionVersion, currentVersion)
	}
	decisionOccurrenceRunID := ""
	decisionOccurrenceRunUID := ""
	decisionOccurrenceTargetReceiptID := ""
	decisionOccurrenceTargetSHA := ""
	decisionOccurrenceIdentityQuality := ""
	if decision.Scope == store.FindingDecisionOccurrence {
		var occurrencePublicID, occurrenceRepositoryScan string
		err := tx.QueryRowContext(ctx, `SELECT public_finding_id, repository_scan, scan_run_id, run_uid, target_receipt_id, target_sha,
			identity_quality FROM security_finding_occurrences
			WHERE namespace = ? AND id = ?`, decision.Namespace, decision.OccurrenceID).
			Scan(&occurrencePublicID, &occurrenceRepositoryScan, &decisionOccurrenceRunID, &decisionOccurrenceRunUID,
				&decisionOccurrenceTargetReceiptID, &decisionOccurrenceTargetSHA, &decisionOccurrenceIdentityQuality)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ValidationErrorf("occurrenceID %q does not exist", decision.OccurrenceID)
		}
		if err != nil {
			return nil, err
		}
		if occurrencePublicID != decision.PublicFindingID || occurrenceRepositoryScan != decision.RepositoryScan {
			return nil, store.ValidationErrorf("occurrence does not belong to public finding")
		}
	}
	decision.FeedbackEligible = strings.TrimSpace(decision.Reason) != "" &&
		decisionOccurrenceIdentityQuality == store.IdentityQualityCanonical
	digest, err := findingDecisionDigest(*decision)
	if err != nil {
		return nil, err
	}
	if decision.SupersedesDecisionID != "" {
		var supersededPublicID string
		err := tx.QueryRowContext(ctx, `SELECT public_finding_id FROM security_finding_decisions
			WHERE namespace = ? AND id = ?`, decision.Namespace, decision.SupersedesDecisionID).Scan(&supersededPublicID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ValidationErrorf("supersedesDecisionID %q does not exist", decision.SupersedesDecisionID)
		}
		if err != nil {
			return nil, err
		}
		if supersededPublicID != decision.PublicFindingID {
			return nil, store.ValidationErrorf("superseded decision belongs to a different finding")
		}
	}
	if len(decision.EvidenceReceiptIDs) > 0 {
		for _, id := range decision.EvidenceReceiptIDs {
			var namespace, receiptRepositoryScan, receiptScanRunID, receiptRunUID, receiptTargetReceiptID string
			var receiptScopeKind, receiptScopeID string
			var receiptExpectedTargetSHA, receiptObservedTargetSHA string
			err := tx.QueryRowContext(ctx, `SELECT namespace, repository_scan, scan_run_id, run_uid, target_receipt_id,
				expected_target_sha, observed_target_sha, scope_kind, scope_id FROM security_stage_receipts WHERE id = ?`, id).
				Scan(&namespace, &receiptRepositoryScan, &receiptScanRunID, &receiptRunUID, &receiptTargetReceiptID,
					&receiptExpectedTargetSHA, &receiptObservedTargetSHA, &receiptScopeKind, &receiptScopeID)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, store.ValidationErrorf("evidence receipt %q does not exist", id)
			}
			if err != nil {
				return nil, err
			}
			if namespace != decision.Namespace {
				return nil, store.ValidationErrorf("evidence receipt %q belongs to a different namespace", id)
			}
			if receiptRepositoryScan != decision.RepositoryScan {
				return nil, store.ValidationErrorf("evidence receipt %q belongs to a different repository scan", id)
			}
			if decisionOccurrenceRunID != "" && (receiptScanRunID != decisionOccurrenceRunID || receiptRunUID != decisionOccurrenceRunUID ||
				receiptTargetReceiptID != decisionOccurrenceTargetReceiptID || receiptExpectedTargetSHA != decisionOccurrenceTargetSHA) {
				return nil, store.ValidationErrorf("evidence receipt %q belongs to a different occurrence run", id)
			}
			if decisionOccurrenceRunID != "" && receiptObservedTargetSHA != "" && receiptObservedTargetSHA != decisionOccurrenceTargetSHA {
				return nil, store.ValidationErrorf("evidence receipt %q observed a different occurrence target", id)
			}
			if decisionOccurrenceRunID != "" && receiptScopeKind == securityScopeOccurrence && receiptScopeID != decision.OccurrenceID {
				return nil, store.ValidationErrorf("evidence receipt %q is scoped to a different occurrence", id)
			}
		}
	}
	decision.DecisionVersion = currentVersion + 1
	if decision.CreatedAt.IsZero() {
		decision.CreatedAt = time.Now().UTC()
	} else {
		decision.CreatedAt = decision.CreatedAt.UTC()
	}
	decision.RecordDigest = digest
	if _, err := tx.ExecContext(ctx, `INSERT INTO security_finding_decisions
		(id, namespace, repository_scan, public_finding_id, scope, occurrence_id, action, reason_code, reason,
		 evidence_receipt_ids_json, supersedes_decision_id, expected_decision_version, decision_version,
		 applicability_json, actor_subject, actor_issuer, authentication_source, source, feedback_eligible,
		 record_digest, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		decision.ID, decision.Namespace, decision.RepositoryScan, decision.PublicFindingID, decision.Scope,
		decision.OccurrenceID, decision.Action, decision.ReasonCode, decision.Reason, string(evidenceJSON),
		decision.SupersedesDecisionID, decision.ExpectedDecisionVersion, decision.DecisionVersion,
		applicabilityJSON, decision.ActorSubject, decision.ActorIssuer, decision.AuthenticationSource,
		decision.Source, decision.FeedbackEligible, decision.RecordDigest, decision.CreatedAt,
	); err != nil {
		return nil, err
	}
	projectionState := decisionProjectionState(decision.Action)
	if decision.Scope == store.FindingDecisionOccurrence && decision.OccurrenceID != currentOccurrenceID {
		projectionState = currentState
	}
	result, err := tx.ExecContext(ctx, `UPDATE security_findings SET state = ?, decision_version = ?, updated_at = ?
		WHERE namespace = ? AND id = ? AND decision_version = ?`,
		projectionState, decision.DecisionVersion, decision.CreatedAt,
		decision.Namespace, decision.PublicFindingID, decision.ExpectedDecisionVersion,
	)
	if err != nil {
		return nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, fmt.Errorf("%w: finding decision version changed concurrently", store.ErrConflict)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	copy := *decision
	return &copy, nil
}

const findingDecisionSelectColumns = `id, namespace, repository_scan, public_finding_id, scope, occurrence_id,
	action, reason_code, reason, evidence_receipt_ids_json, supersedes_decision_id,
	expected_decision_version, decision_version, applicability_json, actor_subject, actor_issuer,
	authentication_source, source, feedback_eligible, record_digest, created_at`

func scanFindingDecision(scanner interface{ Scan(dest ...any) error }) (*store.FindingDecision, error) {
	var decision store.FindingDecision
	var evidenceJSON, applicabilityJSON string
	err := scanner.Scan(
		&decision.ID, &decision.Namespace, &decision.RepositoryScan, &decision.PublicFindingID,
		&decision.Scope, &decision.OccurrenceID, &decision.Action, &decision.ReasonCode, &decision.Reason,
		&evidenceJSON, &decision.SupersedesDecisionID, &decision.ExpectedDecisionVersion,
		&decision.DecisionVersion, &applicabilityJSON, &decision.ActorSubject, &decision.ActorIssuer,
		&decision.AuthenticationSource, &decision.Source, &decision.FeedbackEligible, &decision.RecordDigest,
		&decision.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(evidenceJSON), &decision.EvidenceReceiptIDs); err != nil {
		return nil, err
	}
	if applicabilityJSON != "" {
		decision.Applicability = &store.FindingDecisionApplicability{}
		if err := json.Unmarshal([]byte(applicabilityJSON), decision.Applicability); err != nil {
			return nil, err
		}
	}
	return &decision, nil
}

func getFindingDecisionTx(ctx context.Context, tx *sql.Tx, namespace, id string) (*store.FindingDecision, error) {
	decision, err := scanFindingDecision(tx.QueryRowContext(ctx,
		`SELECT `+findingDecisionSelectColumns+` FROM security_finding_decisions WHERE namespace = ? AND id = ?`,
		namespace, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return decision, err
}

// GetFindingDecision returns one immutable decision.
func (s *Store) GetFindingDecision(ctx context.Context, namespace, id string) (*store.FindingDecision, error) {
	decision, err := scanFindingDecision(s.db.QueryRowContext(ctx,
		`SELECT `+findingDecisionSelectColumns+` FROM security_finding_decisions WHERE namespace = ? AND id = ?`,
		namespace, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return decision, err
}

// ListFindingDecisions lists decisions in controller-assigned order.
func (s *Store) ListFindingDecisions(ctx context.Context, filter store.FindingDecisionFilter) ([]store.FindingDecision, string, error) {
	offset, err := parseOffsetCursor(filter.Cursor)
	if err != nil {
		return nil, "", err
	}
	filter.Limit = boundedFindingHistoryLimit(filter.Limit)
	query := strings.Builder{}
	query.WriteString(`SELECT ` + findingDecisionSelectColumns + ` FROM security_finding_decisions WHERE namespace = ?`)
	args := []any{filter.Namespace}
	appendSecurityStringFilter(&query, &args, "repository_scan", filter.RepositoryScan)
	appendSecurityStringFilter(&query, &args, "public_finding_id", filter.PublicFindingID)
	appendSecurityStringFilter(&query, &args, "occurrence_id", filter.OccurrenceID)
	appendSecurityStringFilter(&query, &args, "scope", string(filter.Scope))
	query.WriteString(` ORDER BY public_finding_id ASC, decision_version ASC, id ASC LIMIT ? OFFSET ?`)
	args = append(args, filter.Limit, offset)
	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close() //nolint:errcheck
	items := make([]store.FindingDecision, 0)
	for rows.Next() {
		item, err := scanFindingDecision(rows)
		if err != nil {
			return nil, "", err
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return items, nextOffsetCursor(offset, len(items), filter.Limit), nil
}
