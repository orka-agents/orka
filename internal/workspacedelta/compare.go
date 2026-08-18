package workspacedelta

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// BuildWithLimits compares a trusted baseline with a frozen post-prompt tree
// while applying limits that may only narrow the baseline's captured limits.
func BuildWithLimits(baseline *Snapshot, postRoot string, intent Intent, limits BuildLimits) (Result, error) {
	if err := intent.validate(); err != nil {
		return Result{}, err
	}
	if err := validateBaseline(baseline); err != nil {
		return Result{}, err
	}
	artifactLimit, err := effectiveArtifactLimit(baseline.options.limits.MaxArtifactBytes, limits.MaxArtifactBytes)
	if err != nil {
		return Result{}, err
	}
	post, err := capture(postRoot, baseline.options, false)
	if err != nil {
		return Result{}, err
	}
	if err := compareProtectedEntries(baseline.entries, post.entries); err != nil {
		return Result{}, err
	}
	changes, deletions, symlinks := compareEntries(baseline.entries, post.entries)
	if len(changes) == 0 && len(deletions) == 0 {
		return Result{Classification: ClassificationNoChange}, nil
	}
	result := Result{Changes: changes, Deletions: deletions, Symlinks: symlinks}
	if intent == IntentRead {
		result.Classification = ClassificationReadOnlyModified
		return result, nil
	}
	result.Classification = ClassificationWriteDelta
	if err := retainChangedContents(postRoot, post, changes, artifactLimit); err != nil {
		return Result{}, err
	}
	artifactLimits := baseline.options.limits
	artifactLimits.MaxArtifactBytes = artifactLimit
	manifest, manifestDigest, artifact, artifactDigest, err := buildArtifact(changes, deletions, symlinks, post.entries, artifactLimits)
	if err != nil {
		return Result{}, err
	}
	result.Manifest = manifest
	result.ManifestDigest = manifestDigest
	result.Artifact = artifact
	result.ArtifactDigest = artifactDigest
	return result, nil
}

func effectiveArtifactLimit(baselineLimit, requestedLimit int64) (int64, error) {
	if requestedLimit < 0 {
		return 0, fmt.Errorf("build max artifact bytes must not be negative")
	}
	if requestedLimit > 0 && requestedLimit < baselineLimit {
		return requestedLimit, nil
	}
	return baselineLimit, nil
}

func validateBaseline(baseline *Snapshot) error {
	if baseline == nil || baseline.version != snapshotVersion || baseline.entries == nil || baseline.optionsDigest == "" || baseline.manifestDigest == "" {
		return ErrInvalidBaseline
	}
	optionsDigest, err := baseline.options.digest()
	if err != nil || optionsDigest != baseline.optionsDigest {
		return ErrInvalidBaseline
	}
	manifestDigest, err := snapshotDigest(baseline)
	if err != nil || manifestDigest != baseline.manifestDigest {
		return ErrInvalidBaseline
	}
	return nil
}

func compareProtectedEntries(baseline, post map[string]entry) error {
	paths := make(map[string]struct{})
	for entryPath, current := range baseline {
		if current.protected {
			paths[entryPath] = struct{}{}
		}
	}
	for entryPath, current := range post {
		if current.protected {
			paths[entryPath] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(paths))
	for entryPath := range paths {
		ordered = append(ordered, entryPath)
	}
	sort.Strings(ordered)
	for _, entryPath := range ordered {
		before, hadBefore := baseline[entryPath]
		after, hasAfter := post[entryPath]
		if !hadBefore || !hasAfter || !protectedEntryEqual(before, after) {
			return pathError("compare protected path", entryPath, ErrExcludedPathModified)
		}
	}
	return nil
}

func compareEntries(baseline, post map[string]entry) ([]Change, []Deletion, []Symlink) {
	paths := make(map[string]struct{}, len(baseline)+len(post))
	for entryPath, current := range baseline {
		if !current.protected {
			paths[entryPath] = struct{}{}
		}
	}
	for entryPath, current := range post {
		if !current.protected {
			paths[entryPath] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(paths))
	for entryPath := range paths {
		ordered = append(ordered, entryPath)
	}
	sort.Strings(ordered)

	changes := make([]Change, 0)
	deletions := make([]Deletion, 0)
	symlinks := make([]Symlink, 0)
	for _, entryPath := range ordered {
		before, hadBefore := baseline[entryPath]
		after, hasAfter := post[entryPath]
		if hadBefore && (!hasAfter || before.kind != after.kind) {
			deletions = append(deletions, Deletion{Path: entryPath, Kind: before.kind})
		}
		if !hasAfter || (hadBefore && entryEqual(before, after)) {
			continue
		}
		operation := ChangeAdded
		if hadBefore {
			if before.kind == after.kind {
				operation = ChangeModified
			} else {
				operation = ChangeReplaced
			}
		}
		change := Change{
			Path: entryPath, Operation: operation, Kind: after.kind, Mode: after.mode,
			Size: after.size, Digest: after.digest, Target: after.target,
		}
		changes = append(changes, change)
		if after.kind == EntrySymlink {
			symlinks = append(symlinks, Symlink{Path: entryPath, Target: after.target, Mode: after.mode})
		}
	}
	return changes, deletions, symlinks
}

func entryEqual(left, right entry) bool {
	return left.kind == right.kind && left.mode == right.mode && left.size == right.size &&
		left.digest == right.digest && left.target == right.target && left.protected == right.protected
}

func protectedEntryEqual(left, right entry) bool {
	return entryEqual(left, right) && left.sourceMode == right.sourceMode
}

func retainChangedContents(postRoot string, post *Snapshot, changes []Change, maxContentBytes int64) error {
	var changedBytes int64
	for _, change := range changes {
		if change.Kind != EntryFile {
			continue
		}
		if change.Size < 0 || changedBytes > maxContentBytes-change.Size {
			return pathError(
				"retain changed content",
				change.Path,
				fmt.Errorf("%w: changed file content exceeds %d bytes", ErrLimitExceeded, maxContentBytes),
			)
		}
		changedBytes += change.Size
	}

	absolute, err := filepath.Abs(filepath.Clean(postRoot))
	if err != nil {
		return fmt.Errorf("absolute workspace root: %w", err)
	}
	retentionOptions := post.options
	if maxContentBytes < retentionOptions.limits.MaxFileBytes {
		retentionOptions.limits.MaxFileBytes = maxContentBytes
	}
	if maxContentBytes < retentionOptions.limits.MaxTotalBytes {
		retentionOptions.limits.MaxTotalBytes = maxContentBytes
	}
	seen := make(map[fileIdentity]string)
	var retainedBytes int64
	for _, change := range changes {
		if change.Kind != EntryFile {
			continue
		}
		current, found := post.entries[change.Path]
		if !found || current.protected {
			return pathError("retain changed content", change.Path, ErrInvalidBaseline)
		}
		filePath := filepath.Join(absolute, filepath.FromSlash(change.Path))
		initial, err := os.Lstat(filePath)
		if err != nil {
			return pathError("retain changed content", change.Path, err)
		}
		captured, err := captureRegular(filePath, change.Path, initial, false, retentionOptions, true, retainedBytes, seen)
		if err != nil {
			return err
		}
		if !entryEqual(current, captured) {
			return pathError("retain changed content", change.Path, ErrInvalidBaseline)
		}
		retainedBytes += captured.size
		current.content = captured.content
		post.entries[change.Path] = current
	}
	return nil
}
