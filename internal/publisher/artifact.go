package publisher

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/workspacedelta"
)

const (
	manifestArchivePath  = "meta/manifest.json"
	deletionsArchivePath = "meta/deletions.json"
	symlinksArchivePath  = "meta/symlinks.json"
	fileArchivePrefix    = "files/"
	maxDeltaEntries      = 100_000
	maxDeltaPathBytes    = 4096
	maxRelativeRootBytes = 1024
)

type deletionDocument struct {
	Schema    string                    `json:"schema"`
	Deletions []workspacedelta.Deletion `json:"deletions"`
}

type symlinkDocument struct {
	Schema   string                   `json:"schema"`
	Symlinks []workspacedelta.Symlink `json:"symlinks"`
}

type parsedDelta struct {
	manifest       workspacedelta.Manifest
	manifestDigest string
	deletions      []workspacedelta.Deletion
	symlinks       []workspacedelta.Symlink
	files          map[string][]byte
}

//nolint:gocyclo // Every branch is a fail-closed validation of one hostile archive condition.
func parseDeltaArtifact(artifact []byte, expectedDigest string, maxBytes int64) (parsedDelta, error) {
	if int64(len(artifact)) > maxBytes {
		return parsedDelta{}, operationError(ErrUnsafeDelta, "read workspace delta", fmt.Sprintf("artifact exceeds %d bytes", maxBytes), nil)
	}
	if err := validateDigest("workspace delta digest", expectedDigest); err != nil {
		return parsedDelta{}, err
	}
	if digestBytes(artifact) != expectedDigest {
		return parsedDelta{}, operationError(ErrUnsafeDelta, "validate workspace delta", "artifact digest mismatch", nil)
	}
	reader := tar.NewReader(bytes.NewReader(artifact))
	entries := make(map[string][]byte)
	for count := 0; ; count++ {
		if count > maxDeltaEntries {
			return parsedDelta{}, operationError(ErrUnsafeDelta, "read workspace delta", "too many tar entries", nil)
		}
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return parsedDelta{}, operationError(ErrUnsafeDelta, "read workspace delta tar", "", err)
		}
		if header.Typeflag != tar.TypeReg {
			return parsedDelta{}, operationError(ErrUnsafeDelta, "validate workspace delta tar", fmt.Sprintf("entry %q is not a regular file", header.Name), nil)
		}
		if header.Size < 0 || header.Size > maxBytes {
			return parsedDelta{}, operationError(ErrUnsafeDelta, "validate workspace delta tar", fmt.Sprintf("entry %q has invalid size", header.Name), nil)
		}
		if err := validateTarEntryName(header.Name); err != nil {
			return parsedDelta{}, err
		}
		if _, duplicate := entries[header.Name]; duplicate {
			return parsedDelta{}, operationError(ErrUnsafeDelta, "validate workspace delta tar", fmt.Sprintf("duplicate entry %q", header.Name), nil)
		}
		data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
		if err != nil {
			return parsedDelta{}, operationError(ErrUnsafeDelta, "read workspace delta entry", header.Name, err)
		}
		if int64(len(data)) != header.Size || int64(len(data)) > maxBytes {
			return parsedDelta{}, operationError(ErrUnsafeDelta, "validate workspace delta entry", fmt.Sprintf("entry %q size mismatch", header.Name), nil)
		}
		entries[header.Name] = data
	}
	manifestBytes, ok := entries[manifestArchivePath]
	if !ok {
		return parsedDelta{}, operationError(ErrUnsafeDelta, "validate workspace delta", "manifest is missing", nil)
	}
	deletionBytes, ok := entries[deletionsArchivePath]
	if !ok {
		return parsedDelta{}, operationError(ErrUnsafeDelta, "validate workspace delta", "deletion manifest is missing", nil)
	}
	symlinkBytes, ok := entries[symlinksArchivePath]
	if !ok {
		return parsedDelta{}, operationError(ErrUnsafeDelta, "validate workspace delta", "symlink manifest is missing", nil)
	}
	var manifest workspacedelta.Manifest
	if err := decodeCanonicalJSON(manifestBytes, &manifest); err != nil {
		return parsedDelta{}, operationError(ErrUnsafeDelta, "decode workspace manifest", "", err)
	}
	var deletions deletionDocument
	if err := decodeCanonicalJSON(deletionBytes, &deletions); err != nil {
		return parsedDelta{}, operationError(ErrUnsafeDelta, "decode deletion manifest", "", err)
	}
	var symlinks symlinkDocument
	if err := decodeCanonicalJSON(symlinkBytes, &symlinks); err != nil {
		return parsedDelta{}, operationError(ErrUnsafeDelta, "decode symlink manifest", "", err)
	}
	if manifest.Schema != workspacedelta.ManifestSchema || deletions.Schema != workspacedelta.ManifestSchema || symlinks.Schema != workspacedelta.ManifestSchema {
		return parsedDelta{}, operationError(ErrUnsafeDelta, "validate workspace delta schema", "unsupported schema", nil)
	}
	if manifest.DeletionsDigest != digestBytes(deletionBytes) || manifest.SymlinksDigest != digestBytes(symlinkBytes) {
		return parsedDelta{}, operationError(ErrUnsafeDelta, "validate workspace delta manifests", "nested manifest digest mismatch", nil)
	}
	if err := validateSortedDelta(manifest.Entries, deletions.Deletions, symlinks.Symlinks); err != nil {
		return parsedDelta{}, err
	}
	result := parsedDelta{
		manifest: manifest, manifestDigest: digestBytes(manifestBytes),
		deletions: deletions.Deletions, symlinks: symlinks.Symlinks, files: make(map[string][]byte),
	}
	for name, data := range entries {
		switch name {
		case manifestArchivePath, deletionsArchivePath, symlinksArchivePath:
			continue
		}
		if !strings.HasPrefix(name, fileArchivePrefix) {
			return parsedDelta{}, operationError(ErrUnsafeDelta, "validate workspace delta tar", fmt.Sprintf("unexpected entry %q", name), nil)
		}
		workspacePath := strings.TrimPrefix(name, fileArchivePrefix)
		if err := validateWorkspacePath(workspacePath); err != nil {
			return parsedDelta{}, err
		}
		result.files[workspacePath] = data
	}
	if err := result.validateCrossReferences(); err != nil {
		return parsedDelta{}, err
	}
	return result, nil
}

// withRelativeRoot translates artifact-relative paths into repository-relative
// paths exactly once. The immutable artifact and its manifest digest remain
// unchanged; only the trusted in-memory view used for full-tree validation and
// application is rooted beneath the session's declared workspace subpath.
func (d parsedDelta) withRelativeRoot(relativeRoot string) (parsedDelta, error) {
	if relativeRoot == "" || relativeRoot == "." {
		return d, nil
	}
	if len(relativeRoot) > maxRelativeRootBytes {
		return parsedDelta{}, operationError(ErrUnsafeDelta, "validate workspace relative root", "relative root exceeds limit", nil)
	}
	if err := validateWorkspacePath(relativeRoot); err != nil {
		return parsedDelta{}, operationError(ErrUnsafeDelta, "validate workspace relative root", "relative root is not canonical", err)
	}
	prefix := func(value string) (string, error) {
		rooted := path.Join(relativeRoot, value)
		if err := validateWorkspacePath(rooted); err != nil {
			return "", err
		}
		return rooted, nil
	}

	rooted := parsedDelta{
		manifest:       d.manifest,
		manifestDigest: d.manifestDigest,
		deletions:      make([]workspacedelta.Deletion, len(d.deletions)),
		symlinks:       make([]workspacedelta.Symlink, len(d.symlinks)),
		files:          make(map[string][]byte, len(d.files)),
	}
	rooted.manifest.Entries = make([]workspacedelta.Change, len(d.manifest.Entries))
	for index, change := range d.manifest.Entries {
		rootedPath, err := prefix(change.Path)
		if err != nil {
			return parsedDelta{}, err
		}
		change.Path = rootedPath
		rooted.manifest.Entries[index] = change
	}
	for index, deletion := range d.deletions {
		rootedPath, err := prefix(deletion.Path)
		if err != nil {
			return parsedDelta{}, err
		}
		deletion.Path = rootedPath
		rooted.deletions[index] = deletion
	}
	for index, symlink := range d.symlinks {
		rootedPath, err := prefix(symlink.Path)
		if err != nil {
			return parsedDelta{}, err
		}
		symlink.Path = rootedPath
		rooted.symlinks[index] = symlink
	}
	for filePath, data := range d.files {
		rootedPath, err := prefix(filePath)
		if err != nil {
			return parsedDelta{}, err
		}
		rooted.files[rootedPath] = data
	}
	if err := validateSortedDelta(rooted.manifest.Entries, rooted.deletions, rooted.symlinks); err != nil {
		return parsedDelta{}, err
	}
	if err := rooted.validateCrossReferences(); err != nil {
		return parsedDelta{}, err
	}
	return rooted, nil
}

func validateTarEntryName(name string) error {
	if name == manifestArchivePath || name == deletionsArchivePath || name == symlinksArchivePath {
		return nil
	}
	if !strings.HasPrefix(name, fileArchivePrefix) {
		return operationError(ErrUnsafeDelta, "validate workspace delta path", fmt.Sprintf("unexpected tar path %q", name), nil)
	}
	return validateWorkspacePath(strings.TrimPrefix(name, fileArchivePrefix))
}

func validateWorkspacePath(value string) error {
	if value == "" || value == "." || len(value) > maxDeltaPathBytes || strings.HasPrefix(value, "/") ||
		strings.Contains(value, "\\") || !utf8.ValidString(value) || hasControl(value) || path.Clean(value) != value {
		return operationError(ErrUnsafeDelta, "validate workspace path", fmt.Sprintf("%q is not a canonical relative path", value), nil)
	}
	for component := range strings.SplitSeq(value, "/") {
		if component == "" || component == "." || component == ".." {
			return operationError(ErrUnsafeDelta, "validate workspace path", fmt.Sprintf("%q contains traversal", value), nil)
		}
		if strings.EqualFold(component, ".git") || strings.EqualFold(component, ".gitmodules") {
			return operationError(ErrUnsafeDelta, "validate workspace path", fmt.Sprintf("%q contains child-controlled Git metadata", value), nil)
		}
	}
	return nil
}

func validateSortedDelta(changes []workspacedelta.Change, deletions []workspacedelta.Deletion, symlinks []workspacedelta.Symlink) error {
	if !sort.SliceIsSorted(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path }) ||
		!sort.SliceIsSorted(deletions, func(i, j int) bool { return deletions[i].Path < deletions[j].Path }) ||
		!sort.SliceIsSorted(symlinks, func(i, j int) bool { return symlinks[i].Path < symlinks[j].Path }) {
		return operationError(ErrUnsafeDelta, "validate workspace delta order", "manifest arrays must be strictly path-sorted", nil)
	}
	for _, group := range [][]string{changePaths(changes), deletionPaths(deletions), symlinkPaths(symlinks)} {
		for index, current := range group {
			if err := validateWorkspacePath(current); err != nil {
				return err
			}
			if index > 0 && group[index-1] == current {
				return operationError(ErrUnsafeDelta, "validate workspace delta order", fmt.Sprintf("duplicate path %q", current), nil)
			}
		}
	}
	return nil
}

func changePaths(values []workspacedelta.Change) []string {
	result := make([]string, len(values))
	for index := range values {
		result[index] = values[index].Path
	}
	return result
}

func deletionPaths(values []workspacedelta.Deletion) []string {
	result := make([]string, len(values))
	for index := range values {
		result[index] = values[index].Path
	}
	return result
}

func symlinkPaths(values []workspacedelta.Symlink) []string {
	result := make([]string, len(values))
	for index := range values {
		result[index] = values[index].Path
	}
	return result
}

//nolint:gocyclo // Cross-document consistency is intentionally checked in one fail-closed pass.
func (d parsedDelta) validateCrossReferences() error {
	changes := make(map[string]workspacedelta.Change, len(d.manifest.Entries))
	for _, change := range d.manifest.Entries {
		changes[change.Path] = change
		switch change.Kind {
		case workspacedelta.EntryFile:
			data, ok := d.files[change.Path]
			if !ok || int64(len(data)) != change.Size || digestBytes(data) != change.Digest || change.Target != "" {
				return operationError(ErrUnsafeDelta, "validate workspace file", fmt.Sprintf("payload metadata mismatch for %q", change.Path), nil)
			}
			if change.Mode != 0o644 && change.Mode != 0o755 {
				return operationError(ErrUnsafeDelta, "validate workspace file", fmt.Sprintf("unsupported mode for %q", change.Path), nil)
			}
		case workspacedelta.EntrySymlink:
			if _, ok := d.files[change.Path]; ok || change.Target == "" || change.Mode != 0o777 {
				return operationError(ErrUnsafeDelta, "validate workspace symlink", fmt.Sprintf("invalid symlink payload for %q", change.Path), nil)
			}
			if err := validateSymlinkTarget(change.Path, change.Target); err != nil {
				return err
			}
		case workspacedelta.EntryDirectory:
			if _, ok := d.files[change.Path]; ok || change.Size != 0 || change.Digest != "" || change.Target != "" || change.Mode != 0o755 {
				return operationError(ErrUnsafeDelta, "validate workspace directory", fmt.Sprintf("invalid directory metadata for %q", change.Path), nil)
			}
		default:
			return operationError(ErrUnsafeDelta, "validate workspace entry", fmt.Sprintf("unsupported kind %q", change.Kind), nil)
		}
		switch change.Operation {
		case workspacedelta.ChangeAdded, workspacedelta.ChangeModified, workspacedelta.ChangeReplaced:
		default:
			return operationError(ErrUnsafeDelta, "validate workspace entry", fmt.Sprintf("unsupported operation %q", change.Operation), nil)
		}
	}
	for filePath := range d.files {
		change, ok := changes[filePath]
		if !ok || change.Kind != workspacedelta.EntryFile {
			return operationError(ErrUnsafeDelta, "validate workspace file", fmt.Sprintf("unreferenced payload %q", filePath), nil)
		}
	}
	links := make(map[string]workspacedelta.Symlink, len(d.symlinks))
	for _, link := range d.symlinks {
		if err := validateSymlinkTarget(link.Path, link.Target); err != nil {
			return err
		}
		links[link.Path] = link
		change, ok := changes[link.Path]
		if !ok || change.Kind != workspacedelta.EntrySymlink || change.Target != link.Target || change.Mode != link.Mode {
			return operationError(ErrUnsafeDelta, "validate workspace symlink", fmt.Sprintf("symlink manifest mismatch for %q", link.Path), nil)
		}
	}
	for _, change := range d.manifest.Entries {
		_, hasLink := links[change.Path]
		if (change.Kind == workspacedelta.EntrySymlink) != hasLink {
			return operationError(ErrUnsafeDelta, "validate workspace symlink", fmt.Sprintf("missing or extra symlink metadata for %q", change.Path), nil)
		}
	}
	return nil
}

func validateSymlinkTarget(linkPath, target string) error {
	if target == "" || strings.HasPrefix(target, "/") || strings.Contains(target, "\\") || !utf8.ValidString(target) || hasControl(target) {
		return operationError(ErrUnsafeDelta, "validate workspace symlink", fmt.Sprintf("unsafe target %q for %q", target, linkPath), nil)
	}
	resolved := path.Clean(path.Join(path.Dir(linkPath), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return operationError(ErrUnsafeDelta, "validate workspace symlink", fmt.Sprintf("target for %q escapes repository", linkPath), nil)
	}
	return nil
}

func decodeCanonicalJSON(data []byte, value any) error {
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
	if !bytes.Equal(data, encoded) {
		return fmt.Errorf("document is not canonical JSON")
	}
	return nil
}

func validateDigest(field, value string) error {
	if !strings.HasPrefix(value, DigestPrefix) {
		return invalid(field, "must be a sha256 digest")
	}
	return validateObjectID(field, strings.TrimPrefix(value, DigestPrefix))
}
