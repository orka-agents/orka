package publisher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	publicationIdentitySchema        = "orka.publisher.publication-cache.v1"
	publicationIdentityName          = "publication.json"
	preparedMetadataName             = "prepared.json"
	bundleFileName                   = "publication.bundle"
	canonicalMetadataMaxBytes        = int64(1 << 20)
	legacyPublicationCacheGeneration = int64(1)
)

type publicationStorageIdentity struct {
	Schema                string `json:"schema"`
	PublicationID         string `json:"publicationId"`
	PublicationGeneration int64  `json:"publicationGeneration"`
}

func (p *Publisher) publicationPath(publicationID string) (string, error) {
	if err := validateIdentifier("publication ID", publicationID); err != nil {
		return "", err
	}
	return filepath.Join(p.artifactRoot, publicationID), nil
}

func (p *Publisher) ensurePublicationDirectory(publicationID string, generation int64) (string, error) {
	if generation < 1 {
		return "", invalid("publication generation", "must be at least 1")
	}
	directory, err := p.publicationPath(publicationID)
	if err != nil {
		return "", err
	}
	request := ReclaimRequest{PublicationID: publicationID, PublicationGeneration: generation}
	staging := p.publicationStagingPath(request)
	identity, exists, identityErr := p.publicationIdentityForRequest(directory, request, true)
	if identityErr != nil {
		return "", identityErr
	}
	if exists {
		if identity.PublicationGeneration != generation {
			return "", operationError(ErrIdempotencyConflict, "open publication cache", "publication generation differs from durable cache identity", nil)
		}
		if err := p.removePublicationStaging(staging, request); err != nil {
			return "", err
		}
		if err := syncPublisherDirectory(p.artifactRoot); err != nil {
			return "", fmt.Errorf("persist publication cache directory: %w", err)
		}
		return directory, nil
	}

	if identity, identityErr := p.readPublicationIdentity(staging, publicationID); identityErr == nil {
		if identity.PublicationGeneration != generation {
			return "", operationError(ErrIdempotencyConflict, "resume publication cache creation", "staged publication generation differs from request", nil)
		}
	} else if errors.Is(identityErr, os.ErrNotExist) {
		if err := ensureTrustedDirectory(staging); err != nil {
			return "", err
		}
		identity := publicationStorageIdentity{
			Schema: publicationIdentitySchema, PublicationID: publicationID, PublicationGeneration: generation,
		}
		if err := writeCanonicalDurable(filepath.Join(staging, publicationIdentityName), identity); err != nil {
			return "", fmt.Errorf("persist publication cache identity: %w", err)
		}
	} else {
		return "", identityErr
	}
	if err := os.Rename(staging, directory); err != nil {
		return "", fmt.Errorf("activate publication cache directory: %w", err)
	}
	if err := syncPublisherDirectory(p.artifactRoot); err != nil {
		return "", fmt.Errorf("persist publication cache directory: %w", err)
	}
	return directory, nil
}

// publicationIdentityForRequest loads the exact cache identity. A live
// directory created by the pre-identity format may be upgraded only for that
// format's sole generation; deterministic staging/tombstone directories may
// recover the generation encoded by their request-derived path.
func (p *Publisher) publicationIdentityForRequest(
	directory string, request ReclaimRequest, legacyLiveDirectory bool,
) (publicationStorageIdentity, bool, error) {
	identity, err := p.readPublicationIdentity(directory, request.PublicationID)
	if err == nil {
		return identity, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return publicationStorageIdentity{}, false, err
	}
	info, statErr := os.Lstat(directory)
	if errors.Is(statErr, os.ErrNotExist) {
		return publicationStorageIdentity{}, false, nil
	}
	if statErr != nil {
		return publicationStorageIdentity{}, false, statErr
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return publicationStorageIdentity{}, false, operationError(
			ErrPreparedArtifactCorrupt, "inspect publication cache", "publication path is not a real directory", nil,
		)
	}
	if legacyLiveDirectory && request.PublicationGeneration != legacyPublicationCacheGeneration {
		return publicationStorageIdentity{}, false, operationError(
			ErrIdempotencyConflict, "recover legacy publication cache",
			"pre-identity publication cache can only belong to generation 1", nil,
		)
	}
	if err := p.ensurePublicationIdentity(directory, request.PublicationID, request.PublicationGeneration); err != nil {
		return publicationStorageIdentity{}, false, err
	}
	identity, err = p.readPublicationIdentityFile(directory)
	if err != nil {
		return publicationStorageIdentity{}, false, err
	}
	return identity, true, nil
}

func (p *Publisher) operationPath(publicationID, kind, operationID string) (string, error) {
	directory, err := p.publicationPath(publicationID)
	if err != nil {
		return "", err
	}
	if _, err := p.readPublicationIdentity(directory, publicationID); err != nil {
		return "", err
	}
	if err := validateIdentifier("operation ID", operationID); err != nil {
		return "", err
	}
	operationDirectory := filepath.Join(directory, kind)
	if err := ensureTrustedDirectory(operationDirectory); err != nil {
		return "", err
	}
	return filepath.Join(operationDirectory, operationID+".json"), nil
}

func writeCanonicalDurable(path string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return writeDurableFile(path, encoded, 0o600)
}

func writeDurableFile(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".orka-publisher-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	directoryFile, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer func() { _ = directoryFile.Close() }()
	return directoryFile.Sync()
}

func readCanonical(path string, value any) error {
	data, err := readBoundedFile(path, canonicalMetadataMaxBytes)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return fmt.Errorf("trailing JSON data: %w", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if !bytes.Equal(encoded, data) {
		return fmt.Errorf("metadata is not canonical JSON")
	}
	return nil
}

func (p *Publisher) loadPrepared(publicationID string) (PreparedPublication, error) {
	directory, err := p.publicationPath(publicationID)
	if err != nil {
		return PreparedPublication{}, err
	}
	identity, err := p.readPublicationIdentity(directory, publicationID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PreparedPublication{}, operationError(ErrPreparedArtifactMissing, "load prepared publication", publicationID, err)
		}
		return PreparedPublication{}, err
	}
	path := filepath.Join(directory, preparedMetadataName)
	var prepared PreparedPublication
	if err := readCanonical(path, &prepared); err != nil {
		if os.IsNotExist(err) {
			return PreparedPublication{}, operationError(ErrPreparedArtifactMissing, "load prepared publication", publicationID, err)
		}
		return PreparedPublication{}, operationError(ErrPreparedArtifactCorrupt, "load prepared publication", publicationID, err)
	}
	if prepared.PublicationID != publicationID || prepared.PublicationGeneration != identity.PublicationGeneration ||
		prepared.BundlePath != filepath.Join(directory, bundleFileName) {
		return PreparedPublication{}, operationError(ErrPreparedArtifactCorrupt, "validate prepared publication", "metadata path or identity mismatch", nil)
	}
	if err := validateSourceRefBaseline(prepared.SourceRef, prepared.BaselineOID); err != nil {
		return PreparedPublication{}, operationError(ErrPreparedArtifactCorrupt, "validate prepared publication", "exact source selector does not match baseline", err)
	}
	bundle, err := readBoundedFile(prepared.BundlePath, p.maxBundleBytes)
	if err != nil {
		return PreparedPublication{}, operationError(ErrPreparedArtifactCorrupt, "read durable bundle", "", err)
	}
	if int64(len(bundle)) != prepared.BundleSize || digestBytes(bundle) != prepared.BundleDigest {
		return PreparedPublication{}, operationError(ErrPreparedArtifactCorrupt, "validate durable bundle", "size or digest mismatch", nil)
	}
	return prepared, nil
}

// RestorePrepared imports a controller-durable prepared bundle into the local
// Publisher cache. The bundle is strictly verified before the local receipt is
// made visible, so loss of the Publisher PVC does not make a Prepared
// Publication unrecoverable.
func (p *Publisher) RestorePrepared(ctx context.Context, prepared PreparedPublication, bundle []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := validateRestoredPrepared(prepared, bundle, p.maxBundleBytes); err != nil {
		return err
	}
	directory, err := p.ensurePublicationDirectory(prepared.PublicationID, prepared.PublicationGeneration)
	if err != nil {
		return err
	}
	prepared.BundlePath = filepath.Join(directory, bundleFileName)
	if existing, loadErr := p.loadPrepared(prepared.PublicationID); loadErr == nil {
		if preparedPublicationsEqual(existing, prepared) {
			return nil
		}
		return operationError(ErrIdempotencyConflict, "restore prepared publication", "local receipt differs from controller-durable receipt", nil)
	}
	box, err := p.newSandbox("restore")
	if err != nil {
		return err
	}
	defer box.Close() //nolint:errcheck
	candidate := filepath.Join(box.root, bundleFileName)
	if err := os.WriteFile(candidate, bundle, 0o600); err != nil {
		return err
	}
	if err := p.verifyBundle(ctx, box, candidate, prepared.CommitOID, prepared.BundleRef); err != nil {
		return operationError(ErrPreparedArtifactCorrupt, "restore prepared publication", "controller-durable bundle verification failed", err)
	}
	if err := writeDurableFile(prepared.BundlePath, bundle, 0o600); err != nil {
		return fmt.Errorf("restore durable Git bundle: %w", err)
	}
	if err := writeCanonicalDurable(filepath.Join(directory, preparedMetadataName), prepared); err != nil {
		return fmt.Errorf("restore prepared publication receipt: %w", err)
	}
	return nil
}

func (p *Publisher) ensurePublicationIdentity(directory, publicationID string, generation int64) error {
	identity, err := p.readPublicationIdentityFile(directory)
	if err == nil {
		if identity.PublicationID != publicationID || identity.PublicationGeneration != generation {
			return operationError(ErrIdempotencyConflict, "validate publication cache identity", "publication identity differs from request", nil)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return operationError(ErrPreparedArtifactCorrupt, "read publication cache identity", publicationID, err)
	}
	identity = publicationStorageIdentity{
		Schema: publicationIdentitySchema, PublicationID: publicationID, PublicationGeneration: generation,
	}
	if err := writeCanonicalDurable(filepath.Join(directory, publicationIdentityName), identity); err != nil {
		return fmt.Errorf("persist publication cache identity: %w", err)
	}
	return nil
}

func (p *Publisher) readPublicationIdentity(directory, publicationID string) (publicationStorageIdentity, error) {
	info, err := os.Lstat(directory)
	if err != nil {
		return publicationStorageIdentity{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return publicationStorageIdentity{}, operationError(ErrPreparedArtifactCorrupt, "inspect publication cache", "publication path is not a real directory", nil)
	}
	identity, err := p.readPublicationIdentityFile(directory)
	if err == nil {
		if identity.PublicationID != publicationID {
			return publicationStorageIdentity{}, operationError(ErrPreparedArtifactCorrupt, "validate publication cache identity", "publication ID differs from directory identity", nil)
		}
		return identity, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return publicationStorageIdentity{}, operationError(ErrPreparedArtifactCorrupt, "read publication cache identity", publicationID, err)
	}

	// Upgrade a complete pre-identity cache in place. Older prepared receipts
	// contain the exact Publication ID and generation, so they remain a safe
	// reclamation fence after this format is introduced.
	var prepared PreparedPublication
	if err := readCanonical(filepath.Join(directory, preparedMetadataName), &prepared); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return publicationStorageIdentity{}, operationError(ErrPreparedArtifactCorrupt, "read publication cache identity", "identity and prepared receipt are both missing", err)
		}
		return publicationStorageIdentity{}, operationError(ErrPreparedArtifactCorrupt, "read legacy publication cache identity", publicationID, err)
	}
	if prepared.PublicationID != publicationID || prepared.PublicationGeneration < 1 {
		return publicationStorageIdentity{}, operationError(ErrPreparedArtifactCorrupt, "validate legacy publication cache identity", "prepared receipt identity is invalid", nil)
	}
	identity = publicationStorageIdentity{
		Schema: publicationIdentitySchema, PublicationID: prepared.PublicationID, PublicationGeneration: prepared.PublicationGeneration,
	}
	if err := writeCanonicalDurable(filepath.Join(directory, publicationIdentityName), identity); err != nil {
		return publicationStorageIdentity{}, fmt.Errorf("persist upgraded publication cache identity: %w", err)
	}
	return identity, nil
}

func (p *Publisher) readPublicationIdentityFile(directory string) (publicationStorageIdentity, error) {
	var identity publicationStorageIdentity
	if err := readCanonical(filepath.Join(directory, publicationIdentityName), &identity); err != nil {
		return publicationStorageIdentity{}, err
	}
	if identity.Schema != publicationIdentitySchema || identity.PublicationGeneration < 1 {
		return publicationStorageIdentity{}, operationError(ErrPreparedArtifactCorrupt, "validate publication cache identity", "identity schema or generation is invalid", nil)
	}
	if err := validateIdentifier("publication ID", identity.PublicationID); err != nil {
		return publicationStorageIdentity{}, operationError(ErrPreparedArtifactCorrupt, "validate publication cache identity", "publication ID is invalid", err)
	}
	return identity, nil
}

func (p *Publisher) removePublicationStaging(path string, request ReclaimRequest) error {
	identity, err := p.readPublicationIdentity(path, request.PublicationID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if identity.PublicationGeneration != request.PublicationGeneration {
		return operationError(ErrIdempotencyConflict, "remove publication cache staging", "staged generation differs from request", nil)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove stale publication cache staging: %w", err)
	}
	return syncPublisherDirectory(p.artifactRoot)
}

func validateRestoredPrepared(prepared PreparedPublication, bundle []byte, maxBundleBytes int64) error {
	if err := validateIdentifier("publication ID", prepared.PublicationID); err != nil {
		return err
	}
	if prepared.PublicationGeneration < 1 || prepared.BranchClaimGeneration < 1 {
		return invalid("publication generation", "publication and branch claim generations must be at least 1")
	}
	if err := validateIdentifier("operation ID", prepared.OperationID); err != nil {
		return err
	}
	if err := validateDigest("prepared request digest", prepared.RequestDigest); err != nil {
		return err
	}
	if err := validateRepository(prepared.Source); err != nil {
		return err
	}
	if err := validateRepository(prepared.Target); err != nil {
		return err
	}
	if err := validateSourceRef(prepared.SourceRef); err != nil {
		return err
	}
	if err := validateBranchRef(prepared.TargetRef); err != nil {
		return err
	}
	if err := validateObjectID("prepared baseline", prepared.BaselineOID); err != nil {
		return err
	}
	if err := validateSourceRefBaseline(prepared.SourceRef, prepared.BaselineOID); err != nil {
		return err
	}
	if err := validateRemoteRef("prepared remote before", prepared.RemoteBefore); err != nil {
		return err
	}
	if err := validateRestoredRelativeRoot(prepared.RelativeRoot); err != nil {
		return err
	}
	for field, digest := range map[string]string{
		"delta artifact digest": prepared.DeltaArtifactDigest,
		"manifest digest":       prepared.ManifestDigest,
		"bundle digest":         prepared.BundleDigest,
	} {
		if err := validateDigest(field, digest); err != nil {
			return err
		}
	}
	if err := validateObjectID("prepared tree", prepared.TreeOID); err != nil {
		return err
	}
	if err := validateObjectID("prepared commit", prepared.CommitOID); err != nil {
		return err
	}
	if !strings.HasPrefix(prepared.BundleRef, "refs/orka/publications/") || len(strings.TrimPrefix(prepared.BundleRef, "refs/orka/publications/")) != 64 {
		return operationError(ErrPreparedArtifactCorrupt, "restore prepared publication", "bundle ref is invalid", nil)
	}
	if prepared.BundlePath != "" || prepared.BundleSize < 1 || prepared.BundleSize > maxBundleBytes || int64(len(bundle)) != prepared.BundleSize || digestBytes(bundle) != prepared.BundleDigest {
		return operationError(ErrPreparedArtifactCorrupt, "restore prepared publication", "bundle size or digest differs from controller receipt", nil)
	}
	if prepared.CommitMessage == "" || prepared.CommitTimestamp.IsZero() || prepared.CommitTimestamp.Location() != time.UTC {
		return operationError(ErrPreparedArtifactCorrupt, "restore prepared publication", "commit metadata is invalid", nil)
	}
	return nil
}

func validateRestoredRelativeRoot(relativeRoot string) error {
	if relativeRoot == "" || relativeRoot == "." {
		return nil
	}
	if len(relativeRoot) > maxRelativeRootBytes {
		return operationError(ErrPreparedArtifactCorrupt, "restore prepared publication", "workspace relative root exceeds limit", nil)
	}
	if err := validateWorkspacePath(relativeRoot); err != nil {
		return operationError(ErrPreparedArtifactCorrupt, "restore prepared publication", "workspace relative root is invalid", err)
	}
	return nil
}

func preparedPublicationsEqual(left, right PreparedPublication) bool {
	left.BundlePath = ""
	right.BundlePath = ""
	return left == right
}
