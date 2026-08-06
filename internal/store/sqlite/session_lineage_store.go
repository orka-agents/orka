/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

// ClaimSessionLineage implements store.SessionLineageStore. The claim is
// serialized by the single-writer SQLite connection plus an immediate
// transaction, so two concurrent first-use claims converge on exactly one
// lineage: the second claim either verifies successfully against the first or
// fails with ErrConflict.
func (s *Store) ClaimSessionLineage(ctx context.Context, request store.ClaimSessionLineageRequest) (*store.SessionLineage, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin session lineage claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanSessionLineage(tx.QueryRowContext(ctx, sessionLineageSelectSQL+`
		WHERE namespace = ? AND session_name = ?`, request.Namespace, request.SessionName))
	switch {
	case err == nil:
		if mismatch := sessionLineageMismatch(existing, request); mismatch != "" {
			return nil, fmt.Errorf("%w: session %s/%s lineage %s", store.ErrConflict,
				request.Namespace, request.SessionName, mismatch)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit session lineage verification: %w", err)
		}
		return existing, nil
	case errors.Is(err, sql.ErrNoRows):
	default:
		return nil, fmt.Errorf("read session lineage: %w", err)
	}

	if !request.EstablishIfAbsent {
		return nil, fmt.Errorf("%w: session %s/%s has no established lineage and this claim may not establish one",
			store.ErrNotFound, request.Namespace, request.SessionName)
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_lineages
		(namespace, session_name, namespace_uid, session_uid, contract_version,
		 lineage_generation, runtime_identity, config_digest, provenance, version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, 1, ?, ?)`,
		request.Namespace, request.SessionName, request.NamespaceUID, request.SessionUID,
		request.ContractVersion, request.RuntimeIdentity, request.ConfigDigest,
		string(request.Provenance), now, now); err != nil {
		return nil, fmt.Errorf("establish session lineage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit session lineage claim: %w", err)
	}
	return &store.SessionLineage{
		Namespace:         request.Namespace,
		SessionName:       request.SessionName,
		NamespaceUID:      request.NamespaceUID,
		SessionUID:        request.SessionUID,
		ContractVersion:   request.ContractVersion,
		LineageGeneration: 1,
		RuntimeIdentity:   request.RuntimeIdentity,
		ConfigDigest:      request.ConfigDigest,
		Provenance:        request.Provenance,
		Version:           1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}, nil
}

// GetSessionLineage implements store.SessionLineageStore.
func (s *Store) GetSessionLineage(ctx context.Context, namespace, sessionName string) (*store.SessionLineage, error) {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(sessionName) == "" {
		return nil, store.ValidationErrorf("session lineage namespace and session name are required")
	}
	lineage, err := scanSessionLineage(s.db.QueryRowContext(ctx, sessionLineageSelectSQL+`
		WHERE namespace = ? AND session_name = ?`, namespace, sessionName))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get session lineage: %w", err)
	}
	return lineage, nil
}

// DeleteSessionLineage implements store.SessionLineageStore.
func (s *Store) DeleteSessionLineage(ctx context.Context, namespace, sessionName string) error {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(sessionName) == "" {
		return store.ValidationErrorf("session lineage namespace and session name are required")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM session_lineages
		WHERE namespace = ? AND session_name = ?`, namespace, sessionName); err != nil {
		return fmt.Errorf("delete session lineage: %w", err)
	}
	return nil
}

const sessionLineageSelectSQL = `SELECT namespace, session_name, namespace_uid, session_uid,
	contract_version, lineage_generation, runtime_identity, config_digest, provenance,
	version, created_at, updated_at
	FROM session_lineages`

type sessionLineageScanner interface {
	Scan(dest ...any) error
}

func scanSessionLineage(row sessionLineageScanner) (*store.SessionLineage, error) {
	var (
		lineage    store.SessionLineage
		provenance string
	)
	if err := row.Scan(&lineage.Namespace, &lineage.SessionName, &lineage.NamespaceUID,
		&lineage.SessionUID, &lineage.ContractVersion, &lineage.LineageGeneration,
		&lineage.RuntimeIdentity, &lineage.ConfigDigest, &provenance,
		&lineage.Version, &lineage.CreatedAt, &lineage.UpdatedAt); err != nil {
		return nil, err
	}
	lineage.Provenance = store.SessionLineageProvenance(provenance)
	lineage.CreatedAt = lineage.CreatedAt.UTC()
	lineage.UpdatedAt = lineage.UpdatedAt.UTC()
	return &lineage, nil
}

// sessionLineageMismatch reports the first identity mismatch between an
// existing lineage and a claim, or empty when the claim verifies. ConfigDigest
// is intentionally not compared: configuration may evolve within one lineage,
// and profile-identity changes are enforced at dispatch, not here.
func sessionLineageMismatch(existing *store.SessionLineage, request store.ClaimSessionLineageRequest) string {
	switch {
	case existing.ContractVersion != request.ContractVersion:
		return fmt.Sprintf("is bound to contract %s, not %s", existing.ContractVersion, request.ContractVersion)
	case existing.SessionUID != request.SessionUID:
		return "belongs to a different Session UID; a recreated same-name Session never attaches to old runtime state"
	case existing.NamespaceUID != request.NamespaceUID:
		return "belongs to a different namespace UID; a recreated same-name namespace never attaches to old runtime state"
	case existing.RuntimeIdentity != request.RuntimeIdentity:
		return fmt.Sprintf("is bound to runtime identity %q, not %q", existing.RuntimeIdentity, request.RuntimeIdentity)
	default:
		return ""
	}
}
