package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

// validateMemoryProposalChanges checks the targets and sources a new proposal
// names. Only consolidation types carry targets, and each needs its own count.
func validateMemoryProposalChanges(proposal *store.MemoryProposal) error {
	consolidation := store.IsMemoryConsolidationProposalType(proposal.Type)
	if !consolidation && len(proposal.Targets) > 0 {
		return store.ValidationErrorf("a %q proposal names no target memories", proposal.Type)
	}
	if len(proposal.Targets) > store.MaxMemoryProposalTargets {
		return store.ValidationErrorf("a proposal names at most %d target memories, not %d", store.MaxMemoryProposalTargets, len(proposal.Targets))
	}
	seen := make(map[string]struct{}, len(proposal.Targets))
	for _, target := range proposal.Targets {
		if strings.TrimSpace(target.MemoryID) == "" {
			return store.ValidationErrorf("a target memory needs an ID")
		}
		if target.Generation < 1 {
			return store.ValidationErrorf("target memory %q needs the generation the proposal read", target.MemoryID)
		}
		if _, duplicate := seen[target.MemoryID]; duplicate {
			return store.ValidationErrorf("target memory %q is named twice", target.MemoryID)
		}
		seen[target.MemoryID] = struct{}{}
	}
	switch proposal.Type {
	case store.MemoryProposalTypeMerge:
		if len(proposal.Targets) < 2 {
			return store.ValidationErrorf("a merge proposal names at least two target memories")
		}
	case store.MemoryProposalTypeSupersede:
		if len(proposal.Targets) != 1 {
			return store.ValidationErrorf("a supersede proposal names exactly one target memory")
		}
	case store.MemoryProposalTypeRetire:
		if len(proposal.Targets) < 1 {
			return store.ValidationErrorf("a retire proposal names at least one target memory")
		}
	}
	if (proposal.Type == store.MemoryProposalTypeMerge || proposal.Type == store.MemoryProposalTypeSupersede) &&
		strings.TrimSpace(proposal.Content) == "" {
		return store.ValidationErrorf("a %q proposal needs the content of the memory it creates", proposal.Type)
	}
	if len(proposal.Sources) > store.MaxMemoryProposalSources {
		return store.ValidationErrorf("a proposal cites at most %d sources, not %d", store.MaxMemoryProposalSources, len(proposal.Sources))
	}
	for _, source := range proposal.Sources {
		if strings.TrimSpace(source.SessionName) == "" {
			return store.ValidationErrorf("a proposal source names a Session")
		}
	}
	return nil
}

func marshalMemoryProposalChanges(proposal *store.MemoryProposal) (string, string, error) {
	targets := proposal.Targets
	if targets == nil {
		targets = []store.MemoryProposalTarget{}
	}
	sources := proposal.Sources
	if sources == nil {
		sources = []store.MemoryProposalSource{}
	}
	targetsJSON, err := json.Marshal(targets)
	if err != nil {
		return "", "", fmt.Errorf("encode memory proposal targets: %w", err)
	}
	sourcesJSON, err := json.Marshal(sources)
	if err != nil {
		return "", "", fmt.Errorf("encode memory proposal sources: %w", err)
	}
	return string(targetsJSON), string(sourcesJSON), nil
}

func unmarshalMemoryProposalChanges(proposal *store.MemoryProposal, targetsJSON, sourcesJSON string) error {
	if targetsJSON != "" && targetsJSON != "[]" {
		if err := json.Unmarshal([]byte(targetsJSON), &proposal.Targets); err != nil {
			return fmt.Errorf("decode memory proposal targets: %w", err)
		}
	}
	if sourcesJSON != "" && sourcesJSON != "[]" {
		if err := json.Unmarshal([]byte(sourcesJSON), &proposal.Sources); err != nil {
			return fmt.Errorf("decode memory proposal sources: %w", err)
		}
	}
	return nil
}

// checkConsolidationTargets refuses a consolidation proposal whose targets
// are missing, deleted or at another generation than the proposal read. The
// proposal is stale, so it returns ErrConflict rather than a precondition a
// new resolve could satisfy.
func checkConsolidationTargets(ctx context.Context, tx *sql.Tx, proposal *store.MemoryProposal) error {
	for _, target := range proposal.Targets {
		var generation uint64
		var deleted bool
		err := tx.QueryRowContext(ctx, `SELECT generation, deleted FROM memories WHERE namespace = ? AND id = ?`,
			proposal.Namespace, target.MemoryID).Scan(&generation, &deleted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return store.ConflictErrorf("memory proposal %q targets memory %q, which does not exist", proposal.ID, target.MemoryID)
		case err != nil:
			return err
		case deleted:
			return store.ConflictErrorf("memory proposal %q targets memory %q, which was deleted", proposal.ID, target.MemoryID)
		case generation != target.Generation:
			return store.ConflictErrorf("memory proposal %q read memory %q at generation %d, but it is at generation %d",
				proposal.ID, target.MemoryID, target.Generation, generation)
		}
	}
	return nil
}

// retireConsolidationTargets changes every target of a checked proposal:
// a merge or supersede deletes and disables each, superseded by replacement,
// and a retire disables each. Every target gains one generation.
func retireConsolidationTargets(ctx context.Context, tx *sql.Tx, proposal *store.MemoryProposal, replacement string, at time.Time) error {
	for _, target := range proposal.Targets {
		query := `UPDATE memories
			 SET deleted = TRUE, disabled = TRUE, superseded_by = ?, generation = generation + 1, updated_at = ?
			 WHERE namespace = ? AND id = ? AND generation = ? AND deleted = FALSE`
		args := []any{replacement, at, proposal.Namespace, target.MemoryID, target.Generation}
		if replacement == "" {
			// Disabling a memory a person already disabled changes nothing, so
			// it keeps its generation, and other proposals that read it stay
			// current.
			query = `UPDATE memories
				 SET generation = generation + CASE WHEN disabled = TRUE THEN 0 ELSE 1 END,
				     updated_at = CASE WHEN disabled = TRUE THEN updated_at ELSE ? END,
				     disabled = TRUE
				 WHERE namespace = ? AND id = ? AND generation = ? AND deleted = FALSE`
			args = args[1:]
		}
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return store.ConflictErrorf("memory proposal %q target memory %q changed during apply", proposal.ID, target.MemoryID)
		}
	}
	return nil
}

// refuseLinkedConsolidationMemory refuses to apply a merge or supersede
// proposal that a memory already names as its source. A consolidation
// proposal creates its memory in the same transaction that retires its
// targets, so such a memory was not created by applying it: anyone who can
// create a memory can name any proposal, and adopting it would retire the
// targets into content nobody reviewed.
func refuseLinkedConsolidationMemory(proposal *store.MemoryProposal, linked *store.Memory) error {
	return store.ConflictErrorf("memory %q names memory proposal %q as its source, but a %s proposal creates its memory only when it is applied",
		linked.ID, proposal.ID, proposal.Type)
}

// applyRetireProposal disables every target of an accepted retire proposal
// and records the proposal as applied to its first target, in tx.
func applyRetireProposal(ctx context.Context, tx *writeTx, application store.MemoryProposalApplication, proposal *store.MemoryProposal) (*store.Memory, error) {
	if err := checkConsolidationTargets(ctx, tx.Tx, proposal); err != nil {
		return nil, err
	}
	if err := retireConsolidationTargets(ctx, tx.Tx, proposal, "", application.AppliedAt); err != nil {
		return nil, err
	}
	first := application.Target.MemoryID
	if err := markMemoryProposalApplied(ctx, tx.Tx, application, first, false); err != nil {
		return nil, err
	}
	memory, err := scanMemory(tx.QueryRowContext(ctx, selectMemorySQL()+` WHERE namespace = ? AND id = ?`, proposal.Namespace, first))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return memory, nil
}
