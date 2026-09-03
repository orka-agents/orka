package service

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/artifactcap"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const workspaceManifestSchema = "orka.workspace-source.v1"

var normalizedArchiveTime = time.Unix(0, 0).UTC()

type workspaceEntry struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	OID    string `json:"oid"`
	Size   int64  `json:"size"`
	Target string `json:"target,omitempty"`
}

type workspaceManifest struct {
	Schema       string           `json:"schema"`
	RepositoryID string           `json:"repositoryId"`
	SourceRef    string           `json:"sourceRef"`
	BaselineOID  string           `json:"baselineOid"`
	TreeOID      string           `json:"treeOid"`
	Entries      []workspaceEntry `json:"entries"`
}

type workspaceArchive struct {
	path           string
	digest         string
	size           int64
	manifestDigest string
	treeOID        string
	entryCount     int
	expandedBytes  int64
}

func buildWorkspaceArchive(
	ctx context.Context,
	runner *gitRunner,
	repositoryID, repositoryURL, sourceRef, baselineOID string,
	limits WorkspaceLimits,
) (workspaceArchive, error) {
	box, repositoryPath, treeOID, err := runner.prepareExactRepository(ctx, repositoryURL, sourceRef, baselineOID)
	if err != nil {
		return workspaceArchive{}, err
	}
	defer box.close()
	batch, err := runner.startBlobBatch(ctx, box, repositoryPath, limits.MaxExpandedBytes, limits.MaxEntries)
	if err != nil {
		return workspaceArchive{}, err
	}
	defer batch.abort()
	entries, err := runner.listTree(ctx, box, batch, repositoryPath, baselineOID, limits)
	if err != nil {
		return workspaceArchive{}, err
	}
	manifest := workspaceManifest{
		Schema: workspaceManifestSchema, RepositoryID: repositoryID, SourceRef: sourceRef,
		BaselineOID: baselineOID, TreeOID: treeOID, Entries: entries,
	}
	manifestBytes, err := harnessv2.CanonicalValue(manifest)
	if err != nil {
		return workspaceArchive{}, invalidRequest("workspace manifest could not be canonicalized", err)
	}
	manifestHash := sha256.Sum256(manifestBytes)
	manifestDigest := "sha256:" + hex.EncodeToString(manifestHash[:])
	archive, err := os.CreateTemp(runner.tempRoot, "workspace-*.tar")
	if err != nil {
		return workspaceArchive{}, apiError(ErrSCMTransport, "workspace_archive_failed", "workspace archive could not be created", 500, false, err)
	}
	archivePath := archive.Name()
	cleanup := true
	defer func() {
		_ = archive.Close()
		if cleanup {
			_ = os.Remove(archivePath)
		}
	}()
	if err := archive.Chmod(0o600); err != nil {
		return workspaceArchive{}, err
	}
	hasher := sha256.New()
	limited := &boundedArchiveWriter{writer: io.MultiWriter(archive, hasher), limit: limits.MaxArtifactBytes}
	tarWriter := tar.NewWriter(limited)
	directories := workspaceDirectories(entries)
	entryCount := len(directories) + len(entries)
	if entryCount > limits.MaxEntries {
		return workspaceArchive{}, invalidRequest("workspace archive exceeds the entry limit after directory expansion", nil)
	}
	for _, directory := range directories {
		if err := writeWorkspaceHeader(tarWriter, directory, 0o755, 0, tar.TypeDir, ""); err != nil {
			return workspaceArchive{}, archiveError(err)
		}
	}
	var expanded int64
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return workspaceArchive{}, err
		}
		if entry.Mode == "120000" {
			if entry.Target == "" || int64(len(entry.Target)) != entry.Size {
				return workspaceArchive{}, invalidRequest("workspace symlink metadata is inconsistent", nil)
			}
			if err := writeWorkspaceHeader(tarWriter, entry.Path, 0o777, 0, tar.TypeSymlink, entry.Target); err != nil {
				return workspaceArchive{}, archiveError(err)
			}
			expanded += entry.Size
			continue
		}
		mode := int64(0o644)
		if entry.Mode == "100755" {
			mode = 0o755
		}
		if err := writeWorkspaceHeader(tarWriter, entry.Path, mode, entry.Size, tar.TypeReg, ""); err != nil {
			return workspaceArchive{}, archiveError(err)
		}
		blob, err := batch.readBlob(entry, limits.MaxFileBytes)
		if err != nil {
			return workspaceArchive{}, err
		}
		if _, err := tarWriter.Write(blob); err != nil {
			return workspaceArchive{}, archiveError(err)
		}
		expanded += entry.Size
	}
	if err := batch.finish(); err != nil {
		return workspaceArchive{}, err
	}
	if err := tarWriter.Close(); err != nil {
		return workspaceArchive{}, archiveError(err)
	}
	if err := archive.Sync(); err != nil {
		return workspaceArchive{}, archiveError(err)
	}
	if err := archive.Close(); err != nil {
		return workspaceArchive{}, archiveError(err)
	}
	cleanup = false
	return workspaceArchive{
		path: archivePath, digest: "sha256:" + hex.EncodeToString(hasher.Sum(nil)), size: limited.written,
		manifestDigest: manifestDigest, treeOID: treeOID, entryCount: entryCount, expandedBytes: expanded,
	}, nil
}

func workspaceDirectories(entries []workspaceEntry) []string {
	set := make(map[string]struct{})
	for _, entry := range entries {
		parent := path.Dir(entry.Path)
		for parent != "." && parent != "/" {
			set[parent] = struct{}{}
			parent = path.Dir(parent)
		}
	}
	result := make([]string, 0, len(set))
	for directory := range set {
		result = append(result, directory)
	}
	sort.Slice(result, func(i, j int) bool {
		leftDepth := strings.Count(result[i], "/")
		rightDepth := strings.Count(result[j], "/")
		if leftDepth == rightDepth {
			return result[i] < result[j]
		}
		return leftDepth < rightDepth
	})
	return result
}

func writeWorkspaceHeader(writer *tar.Writer, name string, mode, size int64, typeFlag byte, linkName string) error {
	return writer.WriteHeader(&tar.Header{
		Typeflag: typeFlag, Name: name, Linkname: linkName, Mode: mode, Size: size,
		ModTime: normalizedArchiveTime, AccessTime: normalizedArchiveTime, ChangeTime: normalizedArchiveTime,
		Uid: 0, Gid: 0, Uname: "", Gname: "", Format: tar.FormatPAX,
	})
}

func archiveError(err error) error {
	return apiError(ErrSCMTransport, "workspace_archive_failed", "workspace archive exceeded limits or could not be written", 500, false, err)
}

type boundedArchiveWriter struct {
	writer  io.Writer
	limit   int64
	written int64
}

func (w *boundedArchiveWriter) Write(value []byte) (int, error) {
	if int64(len(value)) > w.limit-w.written {
		return 0, fmt.Errorf("workspace archive exceeds %d bytes", w.limit)
	}
	count, err := w.writer.Write(value)
	w.written += int64(count)
	return count, err
}

func verifyWorkspaceObject(file *os.File, reference harnessv2.ArtifactReference) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hasher := sha256.New()
	copied, err := io.CopyN(hasher, file, reference.SizeBytes)
	if err != nil || copied != reference.SizeBytes {
		return fmt.Errorf("read workspace object: %w", err)
	}
	var extra [1]byte
	if count, readErr := file.Read(extra[:]); count != 0 || !errors.Is(readErr, io.EOF) {
		return fmt.Errorf("workspace object size changed")
	}
	actual := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if !constantEqual(actual, reference.Digest) {
		return fmt.Errorf("workspace object digest mismatch")
	}
	_, err = file.Seek(0, io.SeekStart)
	return err
}

func workspaceArtifactReference(archive workspaceArchive) (harnessv2.ArtifactReference, error) {
	artifactID, err := artifactcap.ArtifactIDForDigest(archive.digest)
	if err != nil {
		return harnessv2.ArtifactReference{}, err
	}
	reference := harnessv2.ArtifactReference{
		ArtifactID: harnessv2.ArtifactID(artifactID), Digest: archive.digest, SizeBytes: archive.size,
		MediaType: artifactcap.MediaTypeWorkspaceTar,
	}
	if err := reference.Validate(); err != nil {
		return harnessv2.ArtifactReference{}, err
	}
	return reference, nil
}
