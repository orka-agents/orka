package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"

	verification "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/store"
)

const patchVerificationUnknownCheckSlot = "unknown-check"

type patchVerificationSeal struct {
	CreationDigest string                  `json:"creationDigest"`
	EvidenceDigest string                  `json:"evidenceDigest"`
	State          verification.RunState   `json:"state"`
	Assessment     verification.Assessment `json:"assessment"`
	ExcludedSlots  []int                   `json:"excludedSlots,omitempty"`
}

var patchVerificationCredentials = verification.CredentialMatcher{}

func requirePatchVerificationBinding(ctx context.Context, transaction *sql.Tx, binding verification.Binding) (*verification.Record, error) {
	if _, err := patchVerificationJSON(binding, 4096); err != nil {
		return nil, verification.ErrBinding
	}
	record, err := loadPatchVerificationRecord(ctx, transaction, binding.RunID)
	if err != nil {
		return record, err
	}
	if record.Binding != binding {
		return nil, verification.ErrBinding
	}
	return record, nil
}

func patchVerificationObservationIdentity(binding verification.Binding, observation verification.Observation) bool {
	expectedTask := binding.OriginalTaskID
	if observation.Side == verification.Patched {
		expectedTask = binding.PatchedTaskID
	} else if observation.Side != verification.Original {
		return false
	}
	return expectedTask != "" && observation.RunID == binding.RunID && observation.AttemptID == binding.AttemptID && observation.TaskID == expectedTask && observation.ManifestDigest == binding.ManifestDigest
}

func patchVerificationSlot(manifest verification.Manifest, observation verification.Observation) string {
	if manifest.Action == verification.ValidateReport && observation.Side != verification.Original {
		return patchVerificationUnknownCheckSlot
	}
	for _, check := range manifest.Checks {
		if check.ID == observation.CheckID {
			return observation.Side + ":" + check.ID
		}
	}
	return patchVerificationUnknownCheckSlot
}

func (storage *Store) recordPatchVerificationEvidenceWrite(ctx context.Context, binding verification.Binding, evidence verification.ExecutionEvidence) error {
	if !patchVerificationObservationIdentity(binding, evidence.Observation) {
		return verification.ErrBinding
	}
	content, contentErr := patchVerificationJSON(evidence.Observation, verification.MaxObservationBytes)
	deliveryDigest := patchVerificationDeliveryDigest(verification.Digest(content), evidence.Blobs, verification.MaxServices+2, verification.MaxOutputBytes, (verification.MaxServices+2)*verification.MaxOutputBytes)
	var rejected error
	err := storage.withPatchVerificationTx(ctx, func(transaction *sql.Tx) error {
		record, err := requirePatchVerificationBinding(ctx, transaction, binding)
		if err != nil {
			if !errors.Is(err, verification.ErrIntegrity) {
				return err
			}
			rejected = err
			record.Binding.RunID = binding.RunID
			return patchVerificationIncident(ctx, transaction, record, "integrity", "storage", "", "stored evidence is unavailable or corrupt", verification.RunInvalid)
		}
		slot := patchVerificationSlot(record.Manifest, evidence.Observation)
		digest := verification.Digest(content)
		for _, previous := range record.Evidence {
			if patchVerificationSlot(record.Manifest, previous.Observation) != slot {
				continue
			}
			if contentErr != nil || previous.Digest != digest {
				rejected = verification.ErrConflict
				return patchVerificationIncident(ctx, transaction, record, "conflict", slot, deliveryDigest, "conflicting observation delivery", verification.RunInvalid)
			}
			if previous.Rejection != "" {
				rejected = verification.ErrEvidence
				return patchVerificationIncident(ctx, transaction, record, "invalid", slot, deliveryDigest, "invalid execution evidence", verification.RunInvalid)
			}
			if err := persistPatchVerificationOutputs(ctx, transaction, record, evidence, false); err != nil {
				rejected = err
				return patchVerificationIncident(ctx, transaction, record, "invalid", slot, deliveryDigest, "invalid output delivery", verification.RunInvalid)
			}
			return nil
		}
		if record.State != verification.RunRunning || record.Seal != nil {
			return verification.ErrClosed
		}
		validationErr := contentErr
		if validationErr == nil && patchVerificationCredentials.Match(content) {
			validationErr = verification.ErrEvidence
			content = nil
		}
		if validationErr == nil {
			if verification.ValidateObservation(record.Manifest, binding, evidence.Observation) != nil {
				validationErr = verification.ErrEvidence
			}
		}
		if validationErr == nil {
			validationErr = persistPatchVerificationOutputs(ctx, transaction, record, evidence, true)
		}
		rejection := ""
		if validationErr != nil {
			if !errors.Is(validationErr, verification.ErrEvidence) && !errors.Is(validationErr, verification.ErrIntegrity) && !errors.Is(validationErr, verification.ErrLimit) {
				return validationErr
			}
			rejection = "invalid execution evidence"
			rejected = verification.ErrEvidence
		}
		if len(content) != 0 && slot != patchVerificationUnknownCheckSlot {
			if _, err := transaction.ExecContext(ctx, `INSERT INTO patch_verification_evidence (run_id, slot, observation, digest, rejection) VALUES (?, ?, ?, ?, ?)`, binding.RunID, slot, content, digest, rejection); err != nil {
				return err
			}
		}
		if rejected != nil {
			return patchVerificationIncident(ctx, transaction, record, "invalid", slot, deliveryDigest, rejection, verification.RunInvalid)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return rejected
}

func patchVerificationOutputReferences(manifest verification.Manifest, observation verification.Observation) ([]verification.BlobReference, error) {
	if len(observation.ServiceOutputs) > len(manifest.Environment.Services) {
		return nil, verification.ErrEvidence
	}
	references := []verification.BlobReference{}
	for _, reference := range []verification.BlobReference{{Digest: observation.StdoutDigest, Bytes: observation.StdoutBytes}, {Digest: observation.StderrDigest, Bytes: observation.StderrBytes}} {
		if reference.Digest == "" && reference.Bytes == 0 && !observation.Executed {
			continue
		}
		if store.ValidateCanonicalDigest("output", reference.Digest) != nil || reference.Bytes < 0 || reference.Bytes > verification.MaxOutputBytes {
			return nil, verification.ErrEvidence
		}
		references = append(references, reference)
	}
	for _, service := range manifest.Environment.Services {
		output, found := observation.ServiceOutputs[service.ID]
		if !found && !observation.Executed {
			continue
		}
		if !found || store.ValidateCanonicalDigest("service output", output.Digest) != nil || output.Bytes < 0 || output.Bytes > verification.MaxOutputBytes {
			return nil, verification.ErrEvidence
		}
		references = append(references, verification.BlobReference{Digest: output.Digest, Bytes: output.Bytes})
	}
	for serviceID := range observation.ServiceOutputs {
		found := false
		for _, service := range manifest.Environment.Services {
			found = found || service.ID == serviceID
		}
		if !found {
			return nil, verification.ErrEvidence
		}
	}
	return references, nil
}

func persistPatchVerificationOutputs(ctx context.Context, transaction *sql.Tx, record *verification.Record, evidence verification.ExecutionEvidence, allowInsert bool) error {
	references, err := patchVerificationOutputReferences(record.Manifest, evidence.Observation)
	if err != nil {
		return err
	}
	expected := make(map[string]int, len(references))
	for _, reference := range references {
		if previous, found := expected[reference.Digest]; found && previous != reference.Bytes {
			return verification.ErrIntegrity
		}
		expected[reference.Digest] = reference.Bytes
	}
	if len(evidence.Blobs) > len(expected) {
		return verification.ErrEvidence
	}
	for digest, content := range evidence.Blobs {
		length, found := expected[digest]
		if !found || len(content) != length || verification.Digest(content) != digest || patchVerificationCredentials.Match(content) {
			return verification.ErrEvidence
		}
	}
	var total int
	if err := transaction.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(data)), 0) FROM patch_verification_blobs WHERE run_id = ?`, record.Binding.RunID).Scan(&total); err != nil {
		return err
	}
	pending := make(map[string][]byte)
	for digest, length := range expected {
		var persistedLength int
		err := transaction.QueryRowContext(ctx, `SELECT length(data) FROM patch_verification_blobs WHERE run_id = ? AND digest = ?`, record.Binding.RunID, digest).Scan(&persistedLength)
		if err == nil {
			if persistedLength != length {
				return verification.ErrIntegrity
			}
			if _, err := readPatchVerificationBlob(ctx, transaction, record.Binding.RunID, digest); err != nil {
				return err
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		content, found := evidence.Blobs[digest]
		if !found || !allowInsert {
			return verification.ErrIntegrity
		}
		if total > verification.MaxRunBlobBytes-length {
			return verification.ErrLimit
		}
		total += length
		if content == nil {
			content = []byte{}
		}
		pending[digest] = content
	}
	for digest, content := range pending {
		if _, err := transaction.ExecContext(ctx, `INSERT INTO patch_verification_blobs (run_id, digest, data) VALUES (?, ?, ?)`, record.Binding.RunID, digest, content); err != nil {
			return err
		}
	}
	return nil
}

func patchVerificationIncident(ctx context.Context, transaction *sql.Tx, record *verification.Record, kind, slot, digest, reason string, state verification.RunState) error {
	var count int
	if err := transaction.QueryRowContext(ctx, `SELECT COUNT(*) FROM patch_verification_incidents WHERE run_id = ?`, record.Binding.RunID).Scan(&count); err != nil {
		return err
	}
	if count >= verification.MaxIncidents {
		var found bool
		if err := transaction.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM patch_verification_incidents WHERE run_id = ? AND kind = ? AND slot = ? AND digest = ?)`, record.Binding.RunID, kind, slot, digest).Scan(&found); err != nil {
			return err
		}
		if !found {
			kind, slot, digest, reason = "overflow", "receipts", "", "additional rejected deliveries omitted at incident limit"
		}
	}
	if _, err := transaction.ExecContext(ctx, `INSERT OR IGNORE INTO patch_verification_incidents (run_id, kind, slot, digest, reason) VALUES (?, ?, ?, ?, ?)`, record.Binding.RunID, kind, slot, digest, reason); err != nil {
		return err
	}
	if state == verification.RunInvalid && (record.State == verification.RunCancelled || record.State == verification.RunInterrupted) {
		state = record.State
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE patch_verification_runs SET state = ? WHERE run_id = ?`, state, record.Binding.RunID); err != nil {
		return err
	}
	record.State = state
	for _, previous := range record.Incidents {
		if previous.Kind == kind && previous.Slot == slot && previous.Digest == digest {
			record.Assessment = patchVerificationTerminalAssessment(record, state)
			return nil
		}
	}
	record.Incidents = append(record.Incidents, verification.Incident{Kind: kind, Slot: slot, Digest: digest, Reason: reason})
	sort.Slice(record.Incidents, func(left, right int) bool {
		return record.Incidents[left].Kind+":"+record.Incidents[left].Slot+":"+record.Incidents[left].Digest < record.Incidents[right].Kind+":"+record.Incidents[right].Slot+":"+record.Incidents[right].Digest
	})
	record.Assessment = patchVerificationTerminalAssessment(record, state)
	return nil
}

func patchVerificationDeliveryDigest(identity string, blobs map[string][]byte, maxCount, maxBlobBytes, maxTotal int) string {
	overflowDigest := verification.Digest([]byte(identity + ":oversized delivery"))
	if len(blobs) > maxCount {
		return overflowDigest
	}
	claims := make(map[string]verification.BlobReference, len(blobs))
	total := 0
	for digest, content := range blobs {
		if len(digest) > 71 || len(content) > maxBlobBytes || total > maxTotal-len(content) {
			return overflowDigest
		}
		total += len(content)
		claims[digest] = verification.BlobReference{Digest: verification.Digest(content), Bytes: len(content)}
	}
	content, _ := json.Marshal(struct {
		Identity string                                `json:"identity"`
		Claims   map[string]verification.BlobReference `json:"claims"`
	}{Identity: identity, Claims: claims})
	return verification.Digest(content)
}

func patchVerificationTerminalAssessment(record *verification.Record, state verification.RunState) verification.Assessment {
	observations := patchVerificationCaseObservations(record, patchVerificationExcludedSlots(record))
	return patchVerificationUnavailableAssessment(record, state, observations)
}

func patchVerificationUnavailableAssessment(record *verification.Record, state verification.RunState,
	observations []verification.Observation) verification.Assessment {
	reason := "verification run contains rejected or conflicting evidence"
	switch state {
	case verification.RunRunning:
		reason = "run is not finalized"
	case verification.RunCancelled:
		reason = "verification run was cancelled"
	case verification.RunInterrupted:
		reason = "local execution was interrupted; a new run is required"
	}
	assessment := verification.Assessment{Conclusion: verification.UnavailableAction(record.Manifest.Action), Reason: reason}
	if record.Manifest.Action != "" {
		assessment.Checks = verification.Evaluate(record.Manifest, record.Binding, observations).Checks
	}
	return assessment
}

func patchVerificationExcludedSlots(record *verification.Record) []int {
	if record.Manifest.Action == "" {
		return nil
	}
	disputed := make(map[string]bool)
	for _, entry := range record.Evidence {
		if entry.Rejection != "" {
			disputed[patchVerificationSlot(record.Manifest, entry.Observation)] = true
		}
	}
	all := false
	for _, incident := range record.Incidents {
		switch incident.Kind {
		case "conflict", "invalid":
			disputed[incident.Slot] = true
			all = all || incident.Slot == "manifest" || incident.Slot == "provenance" || incident.Slot == "storage"
		case "integrity", "overflow":
			all = true
		}
	}
	var excluded []int
	for sideIndex, side := range verification.ActionSides(record.Manifest.Action) {
		for checkIndex, check := range record.Manifest.Checks {
			if all || disputed[side+":"+check.ID] {
				excluded = append(excluded, sideIndex*len(record.Manifest.Checks)+checkIndex)
			}
		}
	}
	return excluded
}

func patchVerificationCaseObservations(record *verification.Record, excluded []int) []verification.Observation {
	slots := make(map[string]bool, len(excluded))
	sides := verification.ActionSides(record.Manifest.Action)
	for _, position := range excluded {
		if position >= 0 && position < len(sides)*len(record.Manifest.Checks) {
			side := sides[position/len(record.Manifest.Checks)]
			check := record.Manifest.Checks[position%len(record.Manifest.Checks)]
			slots[side+":"+check.ID] = true
		}
	}
	observations := make([]verification.Observation, 0, len(record.Evidence))
	for _, entry := range record.Evidence {
		if !slots[patchVerificationSlot(record.Manifest, entry.Observation)] {
			observations = append(observations, entry.Observation)
		}
	}
	return observations
}

func (storage *Store) finalizePatchVerificationRunWrite(ctx context.Context, binding verification.Binding) (*verification.Record, error) {
	var record *verification.Record
	var integrityErr error
	err := storage.withPatchVerificationTx(ctx, func(transaction *sql.Tx) error {
		var err error
		record, err = requirePatchVerificationBinding(ctx, transaction, binding)
		if err != nil {
			if !errors.Is(err, verification.ErrIntegrity) {
				return err
			}
			integrityErr = err
			record.Binding.RunID = binding.RunID
			return patchVerificationIncident(ctx, transaction, record, "integrity", "storage", "", "stored evidence is unavailable or corrupt", verification.RunInvalid)
		}
		if record.Seal != nil {
			return nil
		}
		if record.State == verification.RunRunning {
			record.State = verification.RunFinalized
			record.Assessment = verification.Evaluate(record.Manifest, binding, patchVerificationObservations(record.Evidence))
		} else {
			record.Assessment = patchVerificationTerminalAssessment(record, record.State)
		}
		seal, err := makePatchVerificationSeal(record)
		if err != nil {
			return err
		}
		result, err := transaction.ExecContext(ctx, `UPDATE patch_verification_runs SET state = ?, sealed = 1, seal_content = ?, seal_digest = ? WHERE run_id = ? AND sealed = 0`, record.State, seal.Content, seal.Digest, binding.RunID)
		if err != nil {
			return err
		}
		if err := rowsAffectedExactlyOne(result, "verification finalization"); err != nil {
			return err
		}
		record.Seal = seal
		return nil
	})
	if err != nil {
		return nil, err
	}
	return record, integrityErr
}

func (storage *Store) cancelPatchVerificationRunWrite(ctx context.Context, binding verification.Binding) (*verification.Record, error) {
	return storage.stopPatchVerificationRun(ctx, binding, verification.RunCancelled)
}

func (storage *Store) recoverInterruptedPatchVerificationRunWrite(ctx context.Context, binding verification.Binding) (*verification.Record, error) {
	return storage.stopPatchVerificationRun(ctx, binding, verification.RunInterrupted)
}

func (storage *Store) stopPatchVerificationRun(ctx context.Context, binding verification.Binding, state verification.RunState) (*verification.Record, error) {
	var record *verification.Record
	var integrityErr error
	err := storage.withPatchVerificationTx(ctx, func(transaction *sql.Tx) error {
		var err error
		record, err = requirePatchVerificationBinding(ctx, transaction, binding)
		if err != nil {
			if !errors.Is(err, verification.ErrIntegrity) {
				return err
			}
			integrityErr = err
			record.Binding.RunID = binding.RunID
			if state == verification.RunInterrupted && record.State != verification.RunRunning {
				state = verification.RunInvalid
			}
			return patchVerificationIncident(ctx, transaction, record, "integrity", "storage", "", "stored evidence is unavailable or corrupt", state)
		}
		if record.State != verification.RunRunning {
			return nil
		}
		return patchVerificationIncident(ctx, transaction, record, string(state), "lifecycle", "", patchVerificationTerminalAssessment(record, state).Reason, state)
	})
	if err != nil {
		return nil, err
	}
	return record, integrityErr
}

func patchVerificationObservations(evidence []verification.EvidenceRecord) []verification.Observation {
	observations := make([]verification.Observation, 0, len(evidence))
	for _, entry := range evidence {
		observations = append(observations, entry.Observation)
	}
	return observations
}

func makePatchVerificationSeal(record *verification.Record) (*verification.Seal, error) {
	return makePatchVerificationSealSnapshot(record, patchVerificationExcludedSlots(record))
}

func makePatchVerificationSealSnapshot(record *verification.Record, excluded []int) (*verification.Seal, error) {
	creation, err := patchVerificationJSON(patchVerificationCreation{Manifest: record.Manifest, Binding: record.Binding, Provenance: record.Provenance}, 2*verification.MaxManifestBytes)
	if err != nil {
		return nil, err
	}
	evidence, err := patchVerificationJSON(record.Evidence, 200*(verification.MaxObservationBytes+1024))
	if err != nil {
		return nil, err
	}
	payload := patchVerificationSeal{CreationDigest: verification.Digest(creation), EvidenceDigest: verification.Digest(evidence), State: record.State, Assessment: record.Assessment, ExcludedSlots: excluded}
	content, err := patchVerificationJSON(payload, 65536)
	if err != nil {
		return nil, err
	}
	return &verification.Seal{Content: content, Digest: verification.Digest(content), Assessment: record.Assessment}, nil
}

func loadPatchVerificationEvidence(ctx context.Context, transaction *sql.Tx, record *verification.Record) ([]verification.BlobReference, error) {
	rows, err := transaction.QueryContext(ctx, `SELECT slot, observation, digest, rejection FROM patch_verification_evidence WHERE run_id = ? ORDER BY slot LIMIT 201`, record.Binding.RunID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	references := append([]verification.BlobReference{}, record.Provenance...)
	for rows.Next() {
		var slot string
		var content []byte
		var entry verification.EvidenceRecord
		if err := rows.Scan(&slot, &content, &entry.Digest, &entry.Rejection); err != nil {
			return nil, err
		}
		if len(record.Evidence) >= len(verification.ActionSides(record.Manifest.Action))*len(record.Manifest.Checks) || len(content) > verification.MaxObservationBytes || verification.Digest(content) != entry.Digest || json.Unmarshal(content, &entry.Observation) != nil || !patchVerificationObservationIdentity(record.Binding, entry.Observation) || slot != patchVerificationSlot(record.Manifest, entry.Observation) || slot == patchVerificationUnknownCheckSlot {
			return nil, verification.ErrIntegrity
		}
		if entry.Rejection == "" {
			if verification.ValidateObservation(record.Manifest, record.Binding, entry.Observation) != nil {
				return nil, verification.ErrIntegrity
			}
			outputs, err := patchVerificationOutputReferences(record.Manifest, entry.Observation)
			if err != nil {
				return nil, verification.ErrIntegrity
			}
			references = append(references, outputs...)
		} else if entry.Rejection != "invalid execution evidence" || record.State == verification.RunRunning || record.State == verification.RunFinalized {
			return nil, verification.ErrIntegrity
		}
		record.Evidence = append(record.Evidence, entry)
	}
	return references, rows.Err()
}

func loadPatchVerificationIncidents(ctx context.Context, transaction *sql.Tx, record *verification.Record) error {
	rows, err := transaction.QueryContext(ctx, `SELECT kind, slot, digest, reason FROM patch_verification_incidents WHERE run_id = ? ORDER BY kind, slot, digest LIMIT ?`, record.Binding.RunID, verification.MaxIncidents+2)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var incident verification.Incident
		if err := rows.Scan(&incident.Kind, &incident.Slot, &incident.Digest, &incident.Reason); err != nil {
			return err
		}
		if len(record.Incidents) > verification.MaxIncidents || len(incident.Kind)+len(incident.Slot)+len(incident.Digest)+len(incident.Reason) > 1024 {
			return verification.ErrIntegrity
		}
		record.Incidents = append(record.Incidents, incident)
	}
	return rows.Err()
}

func checkPatchVerificationSeal(record *verification.Record, content []byte, digest sql.NullString) error {
	if len(content) == 0 {
		if digest.Valid || record.State == verification.RunFinalized {
			return verification.ErrIntegrity
		}
		record.Assessment = patchVerificationTerminalAssessment(record, record.State)
		return nil
	}
	record.Seal = &verification.Seal{Content: content, Digest: digest.String}
	var payload patchVerificationSeal
	if len(content) > 65536 || !digest.Valid || verification.Digest(content) != digest.String || json.Unmarshal(content, &payload) != nil || record.State == verification.RunRunning {
		return verification.ErrIntegrity
	}
	if !patchVerificationSealExclusionsValid(record, payload.ExcludedSlots) {
		return verification.ErrIntegrity
	}
	expectedAssessment := patchVerificationUnavailableAssessment(record, payload.State,
		patchVerificationCaseObservations(record, payload.ExcludedSlots))
	if payload.State == verification.RunFinalized {
		if len(payload.ExcludedSlots) != 0 {
			return verification.ErrIntegrity
		}
		expectedAssessment = verification.Evaluate(record.Manifest, record.Binding, patchVerificationObservations(record.Evidence))
	} else if payload.State != verification.RunCancelled && payload.State != verification.RunInterrupted && payload.State != verification.RunInvalid {
		return verification.ErrIntegrity
	}
	if !patchVerificationSealAssessmentMatches(record.Manifest, payload, expectedAssessment) {
		return verification.ErrIntegrity
	}
	snapshot := *record
	snapshot.State, snapshot.Assessment = payload.State, payload.Assessment
	expected, err := makePatchVerificationSealSnapshot(&snapshot, payload.ExcludedSlots)
	if err != nil || expected.Digest != digest.String || !bytes.Equal(expected.Content, content) {
		return verification.ErrIntegrity
	}
	record.Seal.Assessment = payload.Assessment
	if record.State == verification.RunFinalized && len(record.Incidents) == 0 {
		record.Assessment = payload.Assessment
	} else {
		record.Assessment = patchVerificationTerminalAssessment(record, record.State)
	}
	return nil
}

func patchVerificationSealExclusionsValid(record *verification.Record, excluded []int) bool {
	previous := -1
	currentExcluded := patchVerificationExcludedSlots(record)
	for _, position := range excluded {
		if position <= previous || position >= len(verification.ActionSides(record.Manifest.Action))*len(record.Manifest.Checks) {
			return false
		}
		found := false
		for _, current := range currentExcluded {
			found = found || current == position
		}
		if !found {
			return false
		}
		previous = position
	}
	return true
}

func patchVerificationSealAssessmentMatches(manifest verification.Manifest, payload patchVerificationSeal,
	expected verification.Assessment) bool {
	if reflect.DeepEqual(payload.Assessment, expected) {
		return true
	}
	missing := verification.MissingRequirements(manifest.Environment)
	if manifest.Action != verification.ValidateReport || len(payload.ExcludedSlots) != 0 || len(missing) == 0 {
		return false
	}
	previous := expected
	previous.Checks = append([]verification.CheckResult(nil), expected.Checks...)
	for index := range previous.Checks {
		previous.Checks[index].Reason = strings.Join(missing, "; ")
	}
	return reflect.DeepEqual(payload.Assessment, previous)
}
