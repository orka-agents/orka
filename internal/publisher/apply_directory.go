package publisher

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/orka-agents/orka/internal/artifactcap"
	"github.com/orka-agents/orka/internal/workspacedelta"
)

// ApplyDeltaToDirectory applies one immutable workspace delta artifact to a
// plain filesystem checkout using the publisher's path rules. It is the
// filesystem counterpart of the clean-room publisher's index application and
// is used to stage a repair patch for validation before publication.
//
// The artifact digest is verified before anything is written. Paths are
// rooted beneath relativeRoot exactly once, repository control paths are
// rejected when denyRepositoryControlPaths is set, every operation must match
// the on-disk baseline, and no operation traverses or writes through a
// symbolic link. All filesystem access goes through an os.Root, so nothing
// outside repositoryRoot is reachable.
func ApplyDeltaToDirectory(ctx context.Context, repositoryRoot string, artifact []byte, expectedDigest, relativeRoot string, denyRepositoryControlPaths bool) error {
	delta, err := parseDeltaArtifact(artifact, expectedDigest, artifactcap.DefaultWorkspaceArtifactMaxBytes)
	if err != nil {
		return err
	}
	relativeRoot = strings.TrimSpace(relativeRoot)
	if relativeRoot == "." {
		relativeRoot = ""
	}
	delta, err = delta.withRelativeRoot(relativeRoot)
	if err != nil {
		return err
	}
	if denyRepositoryControlPaths {
		if err := rejectRepositoryControlPaths(delta); err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(repositoryRoot)
	if err != nil {
		return fmt.Errorf("open repository root: %w", err)
	}
	defer root.Close() //nolint:errcheck
	applier := directoryApplier{root: root, delta: delta}
	if err := applier.validate(ctx); err != nil {
		return err
	}
	return applier.apply(ctx)
}

func rejectRepositoryControlPaths(delta parsedDelta) error {
	paths := make([]string, 0, len(delta.manifest.Entries)+len(delta.deletions)+len(delta.symlinks))
	paths = append(paths, changePaths(delta.manifest.Entries)...)
	paths = append(paths, deletionPaths(delta.deletions)...)
	paths = append(paths, symlinkPaths(delta.symlinks)...)
	for _, value := range paths {
		if workspacedelta.RepositoryControlPath(value) {
			return operationError(ErrUnsafeDelta, "validate workspace delta path", fmt.Sprintf("%q is a denied repository control path", value), nil)
		}
	}
	return nil
}

type directoryApplier struct {
	root  *os.Root
	delta parsedDelta

	changes   map[string]workspacedelta.Change
	deletions map[string]workspacedelta.Deletion
	// removeDirectories holds every directory removed by a directory
	// deletion, including unlisted descendant directories.
	removeDirectories map[string]struct{}
}

func (a *directoryApplier) validate(ctx context.Context) error {
	a.changes = make(map[string]workspacedelta.Change, len(a.delta.manifest.Entries))
	for _, change := range a.delta.manifest.Entries {
		a.changes[change.Path] = change
	}
	a.deletions = make(map[string]workspacedelta.Deletion, len(a.delta.deletions))
	for _, deletion := range a.delta.deletions {
		a.deletions[deletion.Path] = deletion
	}
	a.removeDirectories = map[string]struct{}{}

	for _, deletion := range a.delta.deletions {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := a.validateParents(deletion.Path, false); err != nil {
			return err
		}
		kind, exists, err := a.baselineKind(deletion.Path)
		if err != nil {
			return err
		}
		if !exists || kind != deletion.Kind {
			return operationError(ErrUnsafeDelta, "validate delta deletion", fmt.Sprintf("%q does not match the checkout", deletion.Path), nil)
		}
		if deletion.Kind == workspacedelta.EntryDirectory {
			if err := a.validateDirectoryDeletion(ctx, deletion.Path); err != nil {
				return err
			}
		}
	}
	for _, change := range a.delta.manifest.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		fresh, err := a.validateParents(change.Path, true)
		if err != nil {
			return err
		}
		var kind workspacedelta.EntryKind
		exists := false
		if !fresh {
			kind, exists, err = a.baselineKind(change.Path)
			if err != nil {
				return err
			}
		}
		_, deleted := a.deletions[change.Path]
		switch change.Operation {
		case workspacedelta.ChangeAdded:
			if deleted {
				return operationError(ErrUnsafeDelta, "validate delta addition", fmt.Sprintf("%q is both added and deleted", change.Path), nil)
			}
			if exists {
				return operationError(ErrUnsafeDelta, "validate delta addition", fmt.Sprintf("%q already exists in the checkout", change.Path), nil)
			}
		case workspacedelta.ChangeModified:
			if deleted {
				return operationError(ErrUnsafeDelta, "validate delta modification", fmt.Sprintf("%q is both modified and deleted", change.Path), nil)
			}
			if !exists || kind != change.Kind {
				return operationError(ErrUnsafeDelta, "validate delta modification", fmt.Sprintf("%q does not match the checkout kind", change.Path), nil)
			}
		case workspacedelta.ChangeReplaced:
			if !exists || kind == change.Kind {
				return operationError(ErrUnsafeDelta, "validate delta replacement", fmt.Sprintf("%q is not a type replacement", change.Path), nil)
			}
			if !deleted {
				return operationError(ErrUnsafeDelta, "validate delta replacement", fmt.Sprintf("%q lacks matching deletion metadata", change.Path), nil)
			}
		default:
			return operationError(ErrUnsafeDelta, "validate delta entry", fmt.Sprintf("unsupported operation for %q", change.Path), nil)
		}
	}
	return nil
}

// validateParents requires every proper parent of value to be a real
// directory after the delta is applied: a parent the delta turns into a file
// or symlink, or deletes without re-adding as a directory, is unsafe, and an
// existing parent must be a real directory, never a symbolic link. It reports
// fresh when a parent is missing from the checkout or created by this delta,
// so value itself cannot exist on disk.
func (a *directoryApplier) validateParents(value string, forChange bool) (bool, error) {
	components := strings.Split(value, "/")
	for index := 1; index < len(components); index++ {
		parent := strings.Join(components[:index], "/")
		if change, ok := a.changes[parent]; ok {
			if change.Kind != workspacedelta.EntryDirectory {
				return false, operationError(ErrUnsafeDelta, "validate delta parent", fmt.Sprintf("parent %q of %q is not a directory", parent, value), nil)
			}
			if change.Operation != workspacedelta.ChangeModified {
				// An added or replacing directory is created by this delta.
				return true, nil
			}
		}
		if _, deleted := a.deletions[parent]; deleted && forChange {
			return false, operationError(ErrUnsafeDelta, "validate delta parent", fmt.Sprintf("parent %q of %q is deleted", parent, value), nil)
		}
		info, err := a.root.Lstat(parent)
		if errors.Is(err, fs.ErrNotExist) {
			// Missing parents are created as real directories; deeper
			// components cannot exist on disk either.
			return true, nil
		}
		if err != nil {
			return false, operationError(ErrUnsafeDelta, "validate delta parent", fmt.Sprintf("inspect parent %q", parent), err)
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return false, operationError(ErrUnsafeDelta, "validate delta parent", fmt.Sprintf("parent %q of %q is not a real directory", parent, value), nil)
		}
	}
	return false, nil
}

func (a *directoryApplier) baselineKind(value string) (workspacedelta.EntryKind, bool, error) {
	info, err := a.root.Lstat(value)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, operationError(ErrUnsafeDelta, "inspect checkout entry", fmt.Sprintf("%q", value), err)
	}
	kind, err := directoryEntryKind(value, info.Mode())
	return kind, true, err
}

func directoryEntryKind(value string, mode fs.FileMode) (workspacedelta.EntryKind, error) {
	switch {
	case mode&fs.ModeSymlink != 0:
		return workspacedelta.EntrySymlink, nil
	case mode.IsDir():
		return workspacedelta.EntryDirectory, nil
	case mode.IsRegular():
		return workspacedelta.EntryFile, nil
	default:
		return "", operationError(ErrUnsafeDelta, "inspect checkout entry", fmt.Sprintf("%q is not a regular file, directory, or symlink", value), nil)
	}
}

func (a *directoryApplier) validateDirectoryDeletion(ctx context.Context, directory string) error {
	return fs.WalkDir(a.root.FS(), directory, func(entryPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return operationError(ErrUnsafeDelta, "inspect deleted directory", fmt.Sprintf("%q", directory), walkErr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		kind, err := directoryEntryKind(entryPath, entry.Type())
		if err != nil {
			return err
		}
		if kind == workspacedelta.EntryDirectory {
			a.removeDirectories[entryPath] = struct{}{}
			return nil
		}
		if _, ok := a.deletions[entryPath]; !ok {
			return operationError(ErrUnsafeDelta, "validate directory deletion", fmt.Sprintf("%q omits descendant %q", directory, entryPath), nil)
		}
		return nil
	})
}

func (a *directoryApplier) apply(ctx context.Context) error {
	for _, deletion := range a.delta.deletions {
		if err := ctx.Err(); err != nil {
			return err
		}
		if deletion.Kind == workspacedelta.EntryDirectory {
			continue
		}
		if err := a.root.Remove(deletion.Path); err != nil {
			return fmt.Errorf("remove %q: %w", deletion.Path, err)
		}
	}
	directories := make([]string, 0, len(a.removeDirectories))
	for directory := range a.removeDirectories {
		directories = append(directories, directory)
	}
	sort.Slice(directories, func(i, j int) bool {
		left, right := strings.Count(directories[i], "/"), strings.Count(directories[j], "/")
		if left != right {
			return left > right
		}
		return directories[i] > directories[j]
	})
	for _, directory := range directories {
		if err := a.root.Remove(directory); err != nil {
			return fmt.Errorf("remove directory %q: %w", directory, err)
		}
	}
	for _, change := range a.delta.manifest.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.ensureParents(change.Path); err != nil {
			return err
		}
		switch change.Kind {
		case workspacedelta.EntryDirectory:
			if err := a.applyDirectory(change); err != nil {
				return err
			}
		case workspacedelta.EntryFile:
			if err := a.applyFile(change); err != nil {
				return err
			}
		case workspacedelta.EntrySymlink:
			if err := a.applySymlink(change); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureParents re-checks at application time that no parent is a symbolic
// link and creates missing parents as real directories.
func (a *directoryApplier) ensureParents(value string) error {
	parent := path.Dir(value)
	if parent == "." {
		return nil
	}
	components := strings.Split(parent, "/")
	for index := 1; index <= len(components); index++ {
		current := strings.Join(components[:index], "/")
		info, err := a.root.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err := a.root.Mkdir(current, 0o755); err != nil {
				return fmt.Errorf("create parent %q: %w", current, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect parent %q: %w", current, err)
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return operationError(ErrUnsafeDelta, "apply delta parent", fmt.Sprintf("parent %q of %q is not a real directory", current, value), nil)
		}
	}
	return nil
}

func (a *directoryApplier) applyDirectory(change workspacedelta.Change) error {
	info, err := a.root.Lstat(change.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := a.root.Mkdir(change.Path, 0o755); err != nil {
			return fmt.Errorf("create directory %q: %w", change.Path, err)
		}
	case err != nil:
		return fmt.Errorf("inspect directory %q: %w", change.Path, err)
	case !info.IsDir() || info.Mode()&fs.ModeSymlink != 0:
		return operationError(ErrUnsafeDelta, "apply delta directory", fmt.Sprintf("%q is not a real directory", change.Path), nil)
	}
	return a.root.Chmod(change.Path, 0o755)
}

func (a *directoryApplier) applyFile(change workspacedelta.Change) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	info, err := a.root.Lstat(change.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("inspect file %q: %w", change.Path, err)
	case !info.Mode().IsRegular():
		return operationError(ErrUnsafeDelta, "apply delta file", fmt.Sprintf("%q is not a regular file", change.Path), nil)
	default:
		// Replace the regular file instead of opening it, so the write can
		// never follow a link created after validation.
		if err := a.root.Remove(change.Path); err != nil {
			return fmt.Errorf("replace file %q: %w", change.Path, err)
		}
	}
	mode := fs.FileMode(0o644)
	if change.Mode&0o111 != 0 {
		mode = 0o755
	}
	file, err := a.root.OpenFile(change.Path, flags, mode)
	if err != nil {
		return fmt.Errorf("create file %q: %w", change.Path, err)
	}
	_, writeErr := file.Write(a.delta.files[change.Path])
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write file %q: %w", change.Path, err)
	}
	return a.root.Chmod(change.Path, mode)
}

func (a *directoryApplier) applySymlink(change workspacedelta.Change) error {
	info, err := a.root.Lstat(change.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("inspect symlink %q: %w", change.Path, err)
	case info.Mode()&fs.ModeSymlink == 0:
		return operationError(ErrUnsafeDelta, "apply delta symlink", fmt.Sprintf("%q is not a symlink", change.Path), nil)
	default:
		if err := a.root.Remove(change.Path); err != nil {
			return fmt.Errorf("replace symlink %q: %w", change.Path, err)
		}
	}
	if err := a.root.Symlink(change.Target, change.Path); err != nil {
		return fmt.Errorf("create symlink %q: %w", change.Path, err)
	}
	return nil
}
