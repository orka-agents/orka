package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"sort"
	"strings"

	verification "github.com/orka-agents/orka/internal/patchverification"
)

type patchVerificationCreation struct {
	Manifest   verification.Manifest        `json:"manifest"`
	Binding    verification.Binding         `json:"binding"`
	Provenance []verification.BlobReference `json:"provenance"`
}

const patchVerificationWriteLockSchema = `CREATE TABLE IF NOT EXISTS patch_verification_write_lock (id INTEGER PRIMARY KEY CHECK (id = 1))`

func patchVerificationSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS patch_verification_runs (
				run_id TEXT PRIMARY KEY,
				binding BLOB NOT NULL CHECK (length(binding) <= 4096),
				manifest BLOB NOT NULL CHECK (length(manifest) <= 1048576),
				provenance BLOB NOT NULL CHECK (length(provenance) <= 32768),
				creation_digest TEXT NOT NULL,
				state TEXT NOT NULL CHECK (state IN ('running', 'finalized', 'cancelled', 'interrupted', 'invalid')),
				sealed INTEGER NOT NULL DEFAULT 0 CHECK (sealed IN (0, 1)),
				seal_content BLOB CHECK (length(seal_content) <= 65536),
				seal_digest TEXT
			)`,
		`CREATE TABLE IF NOT EXISTS patch_verification_blobs (
				run_id TEXT NOT NULL REFERENCES patch_verification_runs(run_id),
				digest TEXT NOT NULL,
				data BLOB NOT NULL CHECK (length(data) <= 33554432),
				PRIMARY KEY (run_id, digest)
			)`,
		`CREATE TABLE IF NOT EXISTS patch_verification_evidence (
				run_id TEXT NOT NULL REFERENCES patch_verification_runs(run_id),
				slot TEXT NOT NULL,
				observation BLOB NOT NULL CHECK (length(observation) <= 32768),
				digest TEXT NOT NULL,
				rejection TEXT NOT NULL DEFAULT '',
				PRIMARY KEY (run_id, slot)
			)`,
		`CREATE TABLE IF NOT EXISTS patch_verification_incidents (
				run_id TEXT NOT NULL REFERENCES patch_verification_runs(run_id),
				kind TEXT NOT NULL CHECK (length(kind) <= 32),
				slot TEXT NOT NULL CHECK (length(slot) <= 256),
				digest TEXT NOT NULL CHECK (length(digest) <= 71),
				reason TEXT NOT NULL CHECK (length(reason) <= 256),
				PRIMARY KEY (run_id, kind, slot, digest)
			)`,
	}
}

func (storage *Store) InitializePatchVerificationStore(ctx context.Context) error {
	if _, err := storage.taskDataExecutor(ctx).ExecContext(ctx, patchVerificationWriteLockSchema); err != nil {
		return err
	}
	if _, err := storage.taskDataExecutor(ctx).ExecContext(ctx, `INSERT OR IGNORE INTO patch_verification_write_lock (id) VALUES (1)`); err != nil {
		return err
	}
	return storage.withPatchVerificationTx(ctx, func(transaction *sql.Tx) error {
		for _, statement := range patchVerificationSchemaStatements() {
			if _, err := transaction.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	})
}

func (storage *Store) withPatchVerificationTx(ctx context.Context, operation func(*sql.Tx) error) error {
	transaction, err := storage.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	result, err := transaction.ExecContext(ctx, `UPDATE patch_verification_write_lock SET id = id WHERE id = 1`)
	if err != nil {
		return err
	}
	if err := rowsAffectedExactlyOne(result, "verification store lock"); err != nil {
		return err
	}
	if err := operation(transaction.Tx); err != nil {
		return err
	}
	return transaction.Commit()
}

func (storage *Store) createPatchVerificationRunWrite(ctx context.Context, manifest verification.Manifest, binding verification.Binding, provenance map[string][]byte) error {
	manifestContent, manifestErr := patchVerificationJSON(manifest, verification.MaxManifestBytes)
	bindingContent, err := patchVerificationJSON(binding, 4096)
	if err != nil {
		return err
	}
	var rejected error
	err = storage.withPatchVerificationTx(ctx, func(transaction *sql.Tx) error {
		var existingManifest, existingProvenance []byte
		lookupErr := transaction.QueryRowContext(ctx, `SELECT manifest, provenance FROM patch_verification_runs WHERE run_id = ?`, binding.RunID).Scan(&existingManifest, &existingProvenance)
		var existing *verification.Record
		if lookupErr == nil {
			var err error
			existing, err = requirePatchVerificationBinding(ctx, transaction, binding)
			if err != nil {
				if !errors.Is(err, verification.ErrIntegrity) {
					return err
				}
				rejected = err
				existing.Binding.RunID = binding.RunID
				return patchVerificationIncident(ctx, transaction, existing, "integrity", "storage", "", "stored evidence is unavailable or corrupt", verification.RunInvalid)
			}
			if manifestErr != nil || !bytes.Equal(existingManifest, manifestContent) {
				rejected = verification.ErrConflict
				return patchVerificationIncident(ctx, transaction, existing, "conflict", "manifest", verification.Digest(manifestContent), "conflicting frozen manifest delivery", verification.RunInvalid)
			}
		} else if !errors.Is(lookupErr, sql.ErrNoRows) {
			return lookupErr
		}
		if manifestErr != nil {
			return manifestErr
		}
		if err := validatePatchVerificationCreation(manifest, binding, manifestContent); err != nil {
			return err
		}
		if existing == nil && manifest.EarlierValidation != nil {
			if err := validateLinkedPatchVerification(ctx, transaction, manifest); err != nil {
				return err
			}
		}
		refs, blobs, err := patchVerificationProvenance(manifest, provenance)
		if err != nil {
			if existing == nil {
				return err
			}
			rejected = err
			deliveryDigest := patchVerificationDeliveryDigest(binding.ManifestDigest, provenance, verification.MaxFrozenFiles+4, verification.MaxProvenanceBytes, verification.MaxRunBlobBytes)
			return patchVerificationIncident(ctx, transaction, existing, "invalid", "provenance", deliveryDigest, "invalid provenance delivery", verification.RunInvalid)
		}
		refsContent, err := patchVerificationJSON(refs, 32768)
		if err != nil {
			return err
		}
		if existing != nil {
			if !bytes.Equal(existingProvenance, refsContent) {
				rejected = verification.ErrConflict
				return patchVerificationIncident(ctx, transaction, existing, "conflict", "provenance", verification.Digest(refsContent), "conflicting provenance delivery", verification.RunInvalid)
			}
			return nil
		}
		creationContent, err := patchVerificationJSON(patchVerificationCreation{Manifest: manifest, Binding: binding, Provenance: refs}, 2*verification.MaxManifestBytes)
		if err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `INSERT INTO patch_verification_runs (run_id, binding, manifest, provenance, creation_digest, state) VALUES (?, ?, ?, ?, ?, ?)`, binding.RunID, bindingContent, manifestContent, refsContent, verification.Digest(creationContent), verification.RunRunning); err != nil {
			return err
		}
		for _, reference := range refs {
			if _, err := transaction.ExecContext(ctx, `INSERT INTO patch_verification_blobs (run_id, digest, data) VALUES (?, ?, ?)`, binding.RunID, reference.Digest, blobs[reference.Digest]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return rejected
}

func validatePatchVerificationCreation(manifest verification.Manifest, binding verification.Binding, content []byte) error {
	if len(manifest.Files) > verification.MaxFrozenFiles || len(manifest.Environment.Services) > verification.MaxServices {
		return verification.ErrLimit
	}
	if verification.ValidateManifest(manifest) != nil {
		return verification.ErrEvidence
	}
	if err := verification.ValidateRunBinding(manifest, binding); err != nil {
		return err
	}
	repositoryURL, err := url.Parse(manifest.Sources.Repository)
	if err != nil || repositoryURL.User != nil || patchVerificationCredentials.Match(content) {
		return verification.ErrEvidence
	}
	for name := range manifest.Environment.Variables {
		upper := strings.ToUpper(name)
		for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PRIVATE_KEY", "AUTHORIZATION"} {
			if strings.Contains(upper, marker) {
				return verification.ErrEvidence
			}
		}
	}
	return nil
}

func (storage *Store) getPatchVerificationRunWrite(ctx context.Context, runID string) (*verification.Record, error) {
	var record *verification.Record
	var integrityErr error
	err := storage.withPatchVerificationTx(ctx, func(transaction *sql.Tx) error {
		var err error
		record, err = loadPatchVerificationRecord(ctx, transaction, runID)
		if errors.Is(err, verification.ErrIntegrity) {
			integrityErr = err
			record.Binding.RunID = runID
			return patchVerificationIncident(ctx, transaction, record, "integrity", "storage", "", "stored evidence is unavailable or corrupt", verification.RunInvalid)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return record, integrityErr
}

func (storage *Store) GetCompletedReportValidation(ctx context.Context, runID string) (*verification.Record, error) {
	transaction, err := storage.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transaction.Rollback() }()
	record, err := loadPatchVerificationRecord(ctx, transaction.Tx, runID)
	if err != nil {
		return nil, err
	}
	if !completedReportValidation(record) {
		return nil, verification.ErrEvidence
	}
	return record, nil
}

func completedReportValidation(record *verification.Record) bool {
	return record.Manifest.Action == verification.ValidateReport && record.State == verification.RunFinalized &&
		record.Seal != nil && len(record.Incidents) == 0
}

func validateLinkedPatchVerification(ctx context.Context, transaction *sql.Tx, manifest verification.Manifest) error {
	reference := manifest.EarlierValidation
	earlier, err := loadPatchVerificationRecord(ctx, transaction, reference.RunID)
	if err != nil {
		return err
	}
	if !completedReportValidation(earlier) {
		return verification.ErrEvidence
	}
	if reference.ReportDigest != earlier.Manifest.ReportDigest ||
		reference.ManifestDigest != earlier.Binding.ManifestDigest || reference.SealDigest != earlier.Seal.Digest ||
		manifest.Sources.Repository != earlier.Manifest.Sources.Repository ||
		manifest.Sources.Original != earlier.Manifest.Sources.Original ||
		manifest.Problem != earlier.Manifest.Problem || !reflect.DeepEqual(manifest.Scope, earlier.Manifest.Scope) ||
		!reflect.DeepEqual(manifest.Gaps, earlier.Manifest.Gaps) ||
		!reflect.DeepEqual(manifest.Environment, earlier.Manifest.Environment) ||
		!reflect.DeepEqual(manifest.Checks, earlier.Manifest.Checks) ||
		!reflect.DeepEqual(manifest.Files, earlier.Manifest.Files) {
		return verification.ErrEvidence
	}
	return nil
}

func (storage *Store) getPatchVerificationBlobWrite(ctx context.Context, runID, digest string) ([]byte, error) {
	var content []byte
	var integrityErr error
	err := storage.withPatchVerificationTx(ctx, func(transaction *sql.Tx) error {
		record, err := loadPatchVerificationRecord(ctx, transaction, runID)
		if errors.Is(err, verification.ErrIntegrity) {
			integrityErr = err
			record.Binding.RunID = runID
			return patchVerificationIncident(ctx, transaction, record, "integrity", "storage", "", "stored evidence is unavailable or corrupt", verification.RunInvalid)
		}
		if err != nil {
			return err
		}
		content, err = readPatchVerificationBlob(ctx, transaction, runID, digest)
		return err
	})
	if err != nil {
		return nil, err
	}
	return content, integrityErr
}

func loadPatchVerificationRecord(ctx context.Context, transaction *sql.Tx, runID string) (*verification.Record, error) {
	record := &verification.Record{Binding: verification.Binding{RunID: runID}, Assessment: verification.Assessment{Conclusion: verification.UnableToVerify, Reason: "run is not finalized"}}
	var bindingContent, manifestContent, refsContent, sealContent []byte
	var creationDigest string
	var sealDigest sql.NullString
	var sealed bool
	err := transaction.QueryRowContext(ctx, `SELECT binding, manifest, provenance, creation_digest, state, sealed, seal_content, seal_digest FROM patch_verification_runs WHERE run_id = ?`, runID).Scan(&bindingContent, &manifestContent, &refsContent, &creationDigest, &record.State, &sealed, &sealContent, &sealDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, verification.ErrRunNotFound
	}
	if err != nil {
		return record, err
	}
	if len(sealContent) != 0 {
		record.Seal = &verification.Seal{Content: sealContent, Digest: sealDigest.String}
	}
	if sealed != (len(sealContent) != 0 && sealDigest.Valid) {
		return record, verification.ErrIntegrity
	}
	if len(bindingContent) > 4096 || len(manifestContent) > verification.MaxManifestBytes || len(refsContent) > 32768 ||
		json.Unmarshal(manifestContent, &record.Manifest) != nil {
		return record, verification.ErrIntegrity
	}
	record.Assessment = patchVerificationTerminalAssessment(record, record.State)
	if json.Unmarshal(bindingContent, &record.Binding) != nil || json.Unmarshal(refsContent, &record.Provenance) != nil {
		return record, verification.ErrIntegrity
	}
	canonicalBinding, err := patchVerificationJSON(record.Binding, 4096)
	if err != nil || !bytes.Equal(bindingContent, canonicalBinding) {
		return record, verification.ErrIntegrity
	}
	content, err := patchVerificationJSON(patchVerificationCreation{Manifest: record.Manifest, Binding: record.Binding, Provenance: record.Provenance}, 2*verification.MaxManifestBytes)
	if err != nil || verification.Digest(content) != creationDigest || verification.Digest(manifestContent) != record.Binding.ManifestDigest || record.Binding.RunID != runID || verification.ValidateManifest(record.Manifest) != nil || verification.ValidateRunBinding(record.Manifest, record.Binding) != nil {
		return record, verification.ErrIntegrity
	}
	references, err := loadPatchVerificationEvidence(ctx, transaction, record)
	if err != nil {
		return record, err
	}
	if err := loadPatchVerificationIncidents(ctx, transaction, record); err != nil {
		return record, err
	}
	if (record.State == verification.RunRunning || record.State == verification.RunFinalized) && len(record.Incidents) != 0 {
		return record, verification.ErrIntegrity
	}
	if err := patchVerificationCheckBlobs(ctx, transaction, runID, references); err != nil {
		return record, err
	}
	return record, checkPatchVerificationSeal(record, sealContent, sealDigest)
}

func patchVerificationProvenance(manifest verification.Manifest, supplied map[string][]byte) ([]verification.BlobReference, map[string][]byte, error) {
	digests, err := verification.RequiredProvenanceDigests(manifest)
	if err != nil {
		return nil, nil, verification.ErrIntegrity
	}
	required := make(map[string]bool, len(digests))
	for _, digest := range digests {
		required[digest] = true
	}
	blobs := make(map[string][]byte, len(required)+len(manifest.Files))
	for _, file := range manifest.Files {
		blobs[file.Digest] = file.Content
	}
	if len(supplied) > len(required) {
		return nil, nil, verification.ErrEvidence
	}
	for digest, content := range supplied {
		if !required[digest] {
			return nil, nil, verification.ErrEvidence
		}
		blobs[digest] = content
	}
	references := make([]verification.BlobReference, 0, len(required))
	total := 0
	for digest := range required {
		content, found := blobs[digest]
		if !found {
			return nil, nil, verification.ErrIntegrity
		}
		if len(content) > verification.MaxProvenanceBytes || total > verification.MaxRunBlobBytes-len(content) {
			return nil, nil, verification.ErrLimit
		}
		if verification.Digest(content) != digest || patchVerificationCredentials.Match(content) {
			return nil, nil, verification.ErrIntegrity
		}
		if content == nil {
			blobs[digest] = []byte{}
		}
		total += len(content)
		references = append(references, verification.BlobReference{Digest: digest, Bytes: len(content)})
	}
	sort.Slice(references, func(left, right int) bool { return references[left].Digest < references[right].Digest })
	return references, blobs, nil
}

func patchVerificationCheckBlobs(ctx context.Context, transaction *sql.Tx, runID string, references []verification.BlobReference) error {
	total := 0
	seen := make(map[string]int, len(references))
	for _, reference := range references {
		if previous, found := seen[reference.Digest]; found {
			if previous != reference.Bytes {
				return verification.ErrIntegrity
			}
			continue
		}
		seen[reference.Digest] = reference.Bytes
		if reference.Bytes < 0 || reference.Bytes > verification.MaxProvenanceBytes || total > verification.MaxRunBlobBytes-reference.Bytes {
			return verification.ErrIntegrity
		}
		content, err := readPatchVerificationBlob(ctx, transaction, runID, reference.Digest)
		if err != nil {
			return err
		}
		if len(content) != reference.Bytes {
			return verification.ErrIntegrity
		}
		total += len(content)
	}
	var persistedCount, persistedBytes int
	if err := transaction.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(length(data)), 0) FROM patch_verification_blobs WHERE run_id = ?`, runID).Scan(&persistedCount, &persistedBytes); err != nil {
		return err
	}
	if persistedCount != len(seen) || persistedBytes != total {
		return verification.ErrIntegrity
	}
	return nil
}

func readPatchVerificationBlob(ctx context.Context, transaction *sql.Tx, runID, digest string) ([]byte, error) {
	var length int
	err := transaction.QueryRowContext(ctx, `SELECT length(data) FROM patch_verification_blobs WHERE run_id = ? AND digest = ?`, runID, digest).Scan(&length)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, verification.ErrIntegrity
	}
	if err != nil {
		return nil, err
	}
	if length < 0 || length > verification.MaxProvenanceBytes {
		return nil, verification.ErrIntegrity
	}
	var content []byte
	if err := transaction.QueryRowContext(ctx, `SELECT data FROM patch_verification_blobs WHERE run_id = ? AND digest = ?`, runID, digest).Scan(&content); err != nil {
		return nil, err
	}
	if verification.Digest(content) != digest {
		return nil, verification.ErrIntegrity
	}
	return content, nil
}

func patchVerificationJSON(value any, limit int) ([]byte, error) {
	remaining := limit
	if !patchVerificationBudget(reflect.ValueOf(value), &remaining, 0) {
		return nil, verification.ErrLimit
	}
	content, err := json.Marshal(value)
	if err != nil {
		return nil, verification.ErrEvidence
	}
	if len(content) > limit {
		return nil, verification.ErrLimit
	}
	return content, nil
}

func patchVerificationBudget(value reflect.Value, remaining *int, depth int) bool {
	if depth > 24 || *remaining < 0 {
		return false
	}
	if !value.IsValid() {
		return true
	}
	*remaining -= 8
	switch value.Kind() {
	case reflect.String:
		*remaining -= value.Len()
	case reflect.Interface, reflect.Pointer:
		if !value.IsNil() && !patchVerificationBudget(value.Elem(), remaining, depth+1) {
			return false
		}
	case reflect.Slice, reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			*remaining -= value.Len()
			break
		}
		if value.Len() > *remaining/8 {
			return false
		}
		for index := 0; index < value.Len(); index++ {
			if !patchVerificationBudget(value.Index(index), remaining, depth+1) {
				return false
			}
		}
	case reflect.Map:
		if value.Len() > *remaining/16 {
			return false
		}
		iterator := value.MapRange()
		for iterator.Next() {
			if !patchVerificationBudget(iterator.Key(), remaining, depth+1) || !patchVerificationBudget(iterator.Value(), remaining, depth+1) {
				return false
			}
		}
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			if value.Type().Field(index).IsExported() && !patchVerificationBudget(value.Field(index), remaining, depth+1) {
				return false
			}
		}
	}
	return *remaining >= 0
}
