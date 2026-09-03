package publisher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Reclaim durably removes one exact Publication incarnation from the local
// Publisher cache. The caller must already have settled all publication and
// session effects. Once the directory is renamed out of its live name, cleanup
// is completed even if the request context is canceled so a successful return
// is a durable reclamation boundary.
func (p *Publisher) Reclaim(ctx context.Context, request ReclaimRequest) (ReclaimResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := validateReclaimRequest(request); err != nil {
		return ReclaimResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ReclaimResult{}, err
	}

	directory, err := p.publicationPath(request.PublicationID)
	if err != nil {
		return ReclaimResult{}, err
	}
	staging := p.publicationStagingPath(request)
	if err := p.finishPublicationReclaim(staging, request); err != nil {
		return ReclaimResult{}, err
	}
	tombstone := p.publicationReclaimPath(request)
	if err := p.finishPublicationReclaim(tombstone, request); err != nil {
		return ReclaimResult{}, err
	}

	identity, exists, err := p.publicationIdentityForRequest(directory, request, true)
	if err != nil {
		return ReclaimResult{}, err
	}
	if !exists {
		if err := syncPublisherDirectory(p.artifactRoot); err != nil {
			return ReclaimResult{}, fmt.Errorf("persist absent publication cache state: %w", err)
		}
		return reclaimedPublication(request), nil
	}
	if identity.PublicationGeneration != request.PublicationGeneration {
		return ReclaimResult{}, operationError(
			ErrIdempotencyConflict,
			"reclaim publication",
			fmt.Sprintf("durable publication generation %d differs from requested generation %d", identity.PublicationGeneration, request.PublicationGeneration),
			nil,
		)
	}
	if err := os.Rename(directory, tombstone); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if finishErr := p.finishPublicationReclaim(tombstone, request); finishErr != nil {
				return ReclaimResult{}, finishErr
			}
			return reclaimedPublication(request), nil
		}
		return ReclaimResult{}, fmt.Errorf("stage publication reclamation: %w", err)
	}
	if err := syncPublisherDirectory(p.artifactRoot); err != nil {
		return ReclaimResult{}, fmt.Errorf("persist staged publication reclamation: %w", err)
	}
	if err := p.finishPublicationReclaim(tombstone, request); err != nil {
		return ReclaimResult{}, err
	}
	return reclaimedPublication(request), nil
}

func validateReclaimRequest(request ReclaimRequest) error {
	if err := validateIdentifier("publication ID", request.PublicationID); err != nil {
		return err
	}
	if request.PublicationGeneration < 1 {
		return invalid("publication generation", "must be at least 1")
	}
	return nil
}

func reclaimedPublication(request ReclaimRequest) ReclaimResult {
	return ReclaimResult{
		PublicationID: request.PublicationID, PublicationGeneration: request.PublicationGeneration, Reclaimed: true,
	}
}

func (p *Publisher) publicationStagingPath(request ReclaimRequest) string {
	return filepath.Join(p.artifactRoot, ".publication-"+publicationCacheIdentityDigest(request))
}

func (p *Publisher) publicationReclaimPath(request ReclaimRequest) string {
	return filepath.Join(p.artifactRoot, ".reclaim-"+publicationCacheIdentityDigest(request))
}

func publicationCacheIdentityDigest(request ReclaimRequest) string {
	hash := sha256.Sum256([]byte(request.PublicationID + "\x00" + strconv.FormatInt(request.PublicationGeneration, 10)))
	return hex.EncodeToString(hash[:])
}

func (p *Publisher) finishPublicationReclaim(path string, request ReclaimRequest) error {
	identity, exists, err := p.publicationIdentityForRequest(path, request, false)
	if err != nil {
		return err
	}
	if !exists {
		if err := syncPublisherDirectory(p.artifactRoot); err != nil {
			return fmt.Errorf("persist absent publication cache state: %w", err)
		}
		return nil
	}
	if identity.PublicationGeneration != request.PublicationGeneration {
		return operationError(ErrIdempotencyConflict, "finish publication reclamation", "staged publication generation differs from request", nil)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove staged publication cache: %w", err)
	}
	if err := syncPublisherDirectory(p.artifactRoot); err != nil {
		return fmt.Errorf("persist publication cache removal: %w", err)
	}
	return nil
}
