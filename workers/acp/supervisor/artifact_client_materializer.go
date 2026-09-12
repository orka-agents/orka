package supervisor

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/orka-agents/orka/internal/artifactcap"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/safesymlink"
)

type WorkspaceMaterializerLimits struct {
	MaxEntries       int
	MaxExpandedBytes int64
	MaxPathBytes     int
}

type remoteWorkspaceMaterializer struct {
	client *ArtifactClient
	limits WorkspaceMaterializerLimits
}

func NewRemoteWorkspaceMaterializer(client *ArtifactClient, limits WorkspaceMaterializerLimits) (WorkspaceMaterializer, error) {
	if client == nil {
		return nil, fmt.Errorf("artifact client is required")
	}
	if limits.MaxEntries == 0 {
		limits.MaxEntries = 100_000
	}
	if limits.MaxExpandedBytes == 0 {
		limits.MaxExpandedBytes = 1 << 30
	}
	if limits.MaxPathBytes == 0 {
		limits.MaxPathBytes = 4096
	}
	if limits.MaxEntries < 1 || limits.MaxExpandedBytes < 1 || limits.MaxPathBytes < 1 {
		return nil, fmt.Errorf("workspace materializer limits must be positive")
	}
	return &remoteWorkspaceMaterializer{client: client, limits: limits}, nil
}

func (m *remoteWorkspaceMaterializer) Materialize(ctx context.Context, request harnessv2.CreateRuntimeSessionRequest, destination string) error {
	workspace := request.Workspace
	if err := workspace.Validate(); err != nil {
		return fmt.Errorf("workspace specification: %w", err)
	}
	if err := requireEmptyDestination(destination); err != nil {
		return err
	}
	if workspace.Baseline.Artifact == nil {
		return nil
	}
	reference := *workspace.Baseline.Artifact
	if reference.MediaType != artifactcap.MediaTypeWorkspaceTar {
		return fmt.Errorf("workspace artifact media type is unsupported")
	}
	parent := filepath.Dir(destination)
	archive, err := os.CreateTemp(parent, ".workspace-artifact-*.tmp")
	if err != nil {
		return fmt.Errorf("create workspace artifact temp file: %w", err)
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath) //nolint:errcheck
	if err := archive.Chmod(0o600); err != nil {
		_ = archive.Close()
		return fmt.Errorf("secure workspace artifact temp file: %w", err)
	}
	if request.WorkspaceArtifactAuthorization == nil {
		_ = archive.Close()
		return fmt.Errorf("workspace artifact authorization is required")
	}
	authorization := artifactcap.Authorization{Capability: request.WorkspaceArtifactAuthorization.Capability, RequestDigest: request.WorkspaceArtifactAuthorization.RequestDigest}
	if err := m.client.DownloadAuthorized(ctx, reference, authorization, archive); err != nil {
		_ = archive.Close()
		return err
	}
	if err := archive.Sync(); err != nil {
		_ = archive.Close()
		return fmt.Errorf("sync workspace artifact temp file: %w", err)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		_ = archive.Close()
		return fmt.Errorf("rewind workspace artifact: %w", err)
	}
	stage, err := os.MkdirTemp(parent, ".workspace-materialize-*")
	if err != nil {
		_ = archive.Close()
		return fmt.Errorf("create workspace materialization directory: %w", err)
	}
	defer os.RemoveAll(stage) //nolint:errcheck
	if err := os.Chmod(stage, 0o700); err != nil {
		_ = archive.Close()
		return err
	}
	if err := m.extract(archive, stage); err != nil {
		_ = archive.Close()
		return err
	}
	var trailing [1]byte
	if count, trailingErr := archive.Read(trailing[:]); count != 0 || !errors.Is(trailingErr, io.EOF) {
		_ = archive.Close()
		return fmt.Errorf("workspace artifact contains trailing data")
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("close workspace artifact temp file: %w", err)
	}
	installRoot := stage
	if workspace.RelativeRoot != "" && workspace.RelativeRoot != "." {
		installRoot, err = materializedWorkspaceRelativeRoot(stage, workspace.RelativeRoot)
		if err != nil {
			return err
		}
		if err := validateMaterializedWorkspaceBoundary(installRoot, m.limits); err != nil {
			return err
		}
	}
	if err := os.Remove(destination); err != nil {
		return fmt.Errorf("replace empty workspace destination: %w", err)
	}
	if err := os.Rename(installRoot, destination); err != nil {
		return fmt.Errorf("atomically install materialized workspace: %w", err)
	}
	return syncMaterializerDirectory(parent)
}

func materializedWorkspaceRelativeRoot(stage, relativeRoot string) (string, error) {
	current := stage
	for component := range strings.SplitSeq(filepath.FromSlash(relativeRoot), string(os.PathSeparator)) {
		if component == "" || component == "." || component == ".." {
			return "", fmt.Errorf("workspace relative root is invalid")
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("workspace relative root component is not a materialized directory")
		}
	}
	return current, nil
}

func (m *remoteWorkspaceMaterializer) extract(reader io.Reader, root string) error {
	tarReader := tar.NewReader(reader)
	seen := make(map[string]struct{})
	links := make(map[string]string)
	entries := 0
	var expanded int64
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read workspace artifact: %w", err)
		}
		entries++
		if entries > m.limits.MaxEntries {
			return fmt.Errorf("workspace artifact exceeds entry limit")
		}
		name, err := validateMaterializedPath(header.Name, m.limits.MaxPathBytes)
		if err != nil {
			return err
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("workspace artifact contains duplicate path")
		}
		seen[name] = struct{}{}
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := ensureWithinMaterializationRoot(root, target); err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 || header.Linkname != "" {
				return fmt.Errorf("workspace directory entry has invalid content")
			}
			if err := mkdirMaterialized(root, target); err != nil {
				return err
			}
		case tar.TypeReg:
			if header.Linkname != "" || header.Size < 0 || header.Size > m.limits.MaxExpandedBytes-expanded {
				return fmt.Errorf("workspace artifact exceeds expanded size limit or has invalid file metadata")
			}
			expanded += header.Size
			if err := writeMaterializedFile(root, target, header.Size, header.FileInfo().Mode().Perm()&0o111 != 0, tarReader); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if header.Size != 0 || int64(len(header.Linkname)) > m.limits.MaxExpandedBytes-expanded {
				return fmt.Errorf("workspace symlink entry has invalid size")
			}
			if _, err := safesymlink.Resolve(name, header.Linkname, m.limits.MaxPathBytes, m.limits.MaxPathBytes); err != nil {
				return fmt.Errorf("workspace artifact contains unsafe symlink: %w", err)
			}
			expanded += int64(len(header.Linkname))
			links[name] = header.Linkname
		default:
			return fmt.Errorf("workspace artifact contains unsupported hardlink or special entry")
		}
	}
	if err := safesymlink.ValidateGraph(seen, links, m.limits.MaxPathBytes, m.limits.MaxPathBytes); err != nil {
		return fmt.Errorf("workspace artifact contains unsafe symlink graph: %w", err)
	}
	return writeMaterializedSymlinks(root, links)
}

func validateMaterializedWorkspaceBoundary(root string, limits WorkspaceMaterializerLimits) error {
	paths := make(map[string]struct{})
	links := make(map[string]string)
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == root {
			return nil
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if _, err := validateMaterializedPath(relative, limits.MaxPathBytes); err != nil {
			return err
		}
		paths[relative] = struct{}{}
		if entry.Type()&os.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(current)
		if err != nil {
			return err
		}
		links[relative] = target
		return nil
	})
	if err != nil {
		return fmt.Errorf("validate workspace relative root: %w", err)
	}
	if err := safesymlink.ValidateGraph(paths, links, limits.MaxPathBytes, limits.MaxPathBytes); err != nil {
		return fmt.Errorf("workspace relative root contains a symlink outside its boundary: %w", err)
	}
	return nil
}

func requireEmptyDestination(destination string) error {
	if destination == "" || !filepath.IsAbs(destination) {
		return fmt.Errorf("workspace destination must be absolute")
	}
	info, err := os.Lstat(destination)
	if err != nil {
		return fmt.Errorf("inspect workspace destination: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("workspace destination is not a no-follow directory")
	}
	directory, err := os.Open(destination)
	if err != nil {
		return err
	}
	defer directory.Close() //nolint:errcheck
	if _, err := directory.Readdirnames(1); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("workspace destination must be empty")
		}
		return err
	}
	return nil
}

func validateMaterializedPath(value string, maxBytes int) (string, error) {
	if value == "" || len(value) > maxBytes || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || strings.ContainsRune(value, 0) {
		return "", fmt.Errorf("workspace artifact contains unsafe path")
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return "", fmt.Errorf("workspace artifact contains unsafe path")
		}
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("workspace artifact contains unsafe path")
	}
	for component := range strings.SplitSeq(cleaned, "/") {
		if component == "" || component == "." || component == ".." {
			return "", fmt.Errorf("workspace artifact contains unsafe path")
		}
	}
	return cleaned, nil
}

func ensureWithinMaterializationRoot(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(filepath.ToSlash(relative), "../") {
		return fmt.Errorf("workspace artifact path escapes materialization root")
	}
	return nil
}

func mkdirMaterialized(root, target string) error {
	if filepath.Clean(target) == filepath.Clean(root) {
		return nil
	}
	if err := ensureWithinMaterializationRoot(root, target); err != nil {
		return err
	}
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("workspace artifact path collides with non-directory")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	return verifyMaterializedParents(root, target)
}

func writeMaterializedFile(root, target string, size int64, executable bool, reader io.Reader) error {
	if err := mkdirMaterialized(root, filepath.Dir(target)); err != nil {
		return err
	}
	if err := verifyMaterializedParents(root, filepath.Dir(target)); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	file, err := openMaterializedFileNoFollow(target, mode)
	if err != nil {
		return fmt.Errorf("create materialized workspace file: %w", err)
	}
	written, copyErr := io.CopyN(file, reader, size)
	if copyErr == nil && written != size {
		copyErr = io.ErrUnexpectedEOF
	}
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr := file.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(target)
		return fmt.Errorf("write materialized workspace file: %w", copyErr)
	}
	return nil
}

func writeMaterializedSymlinks(root string, links map[string]string) error {
	paths := make([]string, 0, len(links))
	for linkPath := range links {
		paths = append(paths, linkPath)
	}
	sort.Strings(paths)
	for _, linkPath := range paths {
		targetPath := filepath.Join(root, filepath.FromSlash(linkPath))
		if err := mkdirMaterialized(root, filepath.Dir(targetPath)); err != nil {
			return err
		}
		if err := verifyMaterializedParents(root, filepath.Dir(targetPath)); err != nil {
			return err
		}
		if _, err := os.Lstat(targetPath); err == nil {
			return fmt.Errorf("workspace symlink path collides with an existing entry")
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Symlink(links[linkPath], targetPath); err != nil {
			return fmt.Errorf("create materialized workspace symlink: %w", err)
		}
		info, err := os.Lstat(targetPath)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			_ = os.Remove(targetPath)
			return fmt.Errorf("revalidate materialized workspace symlink")
		}
		if err := syncMaterializerDirectory(filepath.Dir(targetPath)); err != nil {
			return err
		}
	}
	return nil
}

func verifyMaterializedParents(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(filepath.ToSlash(relative), "../") {
		return fmt.Errorf("workspace materialization path escapes root")
	}
	current := root
	for component := range strings.SplitSeq(filepath.ToSlash(relative), "/") {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("workspace materialization encountered unsafe parent")
		}
	}
	return nil
}

func syncMaterializerDirectory(dirPath string) error {
	directory, err := os.Open(dirPath)
	if err != nil {
		return err
	}
	defer directory.Close() //nolint:errcheck
	return directory.Sync()
}
