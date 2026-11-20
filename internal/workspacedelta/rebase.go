package workspacedelta

import (
	"context"
	"errors"
	"fmt"
	"maps"
)

// ErrRebaseMismatch reports a published delta that does not apply exactly to
// the trusted baseline it was built from.
var ErrRebaseMismatch = errors.New("workspace delta does not apply to the trusted baseline")

// Rebase returns a new trusted baseline: this snapshot with one exactly
// published delta applied (ADR 0059). It never reads the live tree, so the
// result comes only from controller-verified state: the trusted baseline and
// the delta built from it, which the controller verified as published
// exactly. Anything the live tree holds beyond that delta therefore appears in
// the next delta built from the result.
//
// Derived entries carry no content-policy flag or fingerprint, because no
// content was read for them. A later delta that touches one is checked
// without the baseline exemption, which is stricter, never looser.
//
//nolint:gocyclo // The add/modify/replace/delete rules are audited together.
func (s *Snapshot) Rebase(ctx context.Context, changes []Change, deletions []Deletion) (*Snapshot, error) {
	if err := validateBaseline(ctx, s); err != nil {
		return nil, err
	}
	entries := make(map[string]entry, len(s.entries)+len(changes))
	maps.Copy(entries, s.entries)
	deleted := make(map[string]EntryKind, len(deletions))
	for _, deletion := range deletions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		before, ok := entries[deletion.Path]
		if !ok || before.protected || before.kind != deletion.Kind {
			return nil, pathError("rebase delete", deletion.Path, ErrRebaseMismatch)
		}
		if _, repeated := deleted[deletion.Path]; repeated {
			return nil, pathError("rebase delete", deletion.Path, ErrRebaseMismatch)
		}
		deleted[deletion.Path] = before.kind
		delete(entries, deletion.Path)
	}
	seen := make(map[string]struct{}, len(changes))
	for _, change := range changes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, repeated := seen[change.Path]; repeated {
			return nil, pathError("rebase change", change.Path, ErrRebaseMismatch)
		}
		seen[change.Path] = struct{}{}
		protected, err := s.options.classifyPath(change.Path)
		if err != nil {
			return nil, err
		}
		if protected {
			return nil, pathError("rebase change", change.Path, ErrRebaseMismatch)
		}
		before, existed := s.entries[change.Path]
		deletedKind, wasDeleted := deleted[change.Path]
		switch change.Operation {
		case ChangeAdded:
			if existed {
				return nil, pathError("rebase add", change.Path, ErrRebaseMismatch)
			}
		case ChangeModified:
			if !existed || wasDeleted || before.kind != change.Kind {
				return nil, pathError("rebase modify", change.Path, ErrRebaseMismatch)
			}
		case ChangeReplaced:
			if !existed || !wasDeleted || deletedKind == change.Kind {
				return nil, pathError("rebase replace", change.Path, ErrRebaseMismatch)
			}
		default:
			return nil, pathError("rebase change", change.Path, fmt.Errorf("%w: operation %q", ErrRebaseMismatch, change.Operation))
		}
		next := entry{
			path: change.Path, kind: change.Kind, mode: change.Mode,
			sourceMode: uint32(change.Mode),
		}
		switch change.Kind {
		case EntryFile:
			if change.Size < 0 || change.Size > s.options.limits.MaxFileBytes || change.Digest == "" || change.Target != "" {
				return nil, pathError("rebase file", change.Path, ErrRebaseMismatch)
			}
			next.size, next.digest = change.Size, change.Digest
		case EntrySymlink:
			if change.Target == "" || change.Digest != "" || change.Size != 0 {
				return nil, pathError("rebase symlink", change.Path, ErrRebaseMismatch)
			}
			next.target = change.Target
		case EntryDirectory:
			if change.Target != "" || change.Digest != "" || change.Size != 0 {
				return nil, pathError("rebase directory", change.Path, ErrRebaseMismatch)
			}
		default:
			return nil, pathError("rebase change", change.Path, fmt.Errorf("%w: kind %q", ErrRebaseMismatch, change.Kind))
		}
		entries[change.Path] = next
	}
	if len(entries) > s.options.limits.MaxEntries {
		return nil, fmt.Errorf("%w: rebased entry count exceeds %d", ErrLimitExceeded, s.options.limits.MaxEntries)
	}
	var totalBytes int64
	for _, current := range entries {
		if current.kind != EntryFile {
			continue
		}
		if totalBytes > s.options.limits.MaxTotalBytes-current.size {
			return nil, fmt.Errorf("%w: rebased total bytes exceed %d", ErrLimitExceeded, s.options.limits.MaxTotalBytes)
		}
		totalBytes += current.size
	}
	if err := validateSymlinkGraph(ctx, entries, s.options); err != nil {
		return nil, err
	}
	rebased := &Snapshot{
		version: s.version, entries: entries, options: s.options, optionsDigest: s.optionsDigest,
		entryCount: len(entries), totalBytes: totalBytes,
	}
	digest, err := snapshotDigest(ctx, rebased)
	if err != nil {
		return nil, err
	}
	rebased.manifestDigest = digest
	return rebased, nil
}
