package workspacedelta

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

const (
	manifestArchivePath  = "meta/manifest.json"
	deletionsArchivePath = "meta/deletions.json"
	symlinksArchivePath  = "meta/symlinks.json"
	fileArchivePrefix    = "files/"
)

type deletionDocument struct {
	Schema    string     `json:"schema"`
	Deletions []Deletion `json:"deletions"`
}

type symlinkDocument struct {
	Schema   string    `json:"schema"`
	Symlinks []Symlink `json:"symlinks"`
}

type archiveEntry struct {
	name string
	mode int64
	data []byte
}

func buildArtifact(
	changes []Change,
	deletions []Deletion,
	symlinks []Symlink,
	post map[string]entry,
	limits Limits,
) ([]byte, string, []byte, string, error) {
	deletionBytes, err := marshalCanonical(deletionDocument{Schema: ManifestSchema, Deletions: deletions})
	if err != nil {
		return nil, "", nil, "", fmt.Errorf("encode deletion manifest: %w", err)
	}
	symlinkBytes, err := marshalCanonical(symlinkDocument{Schema: ManifestSchema, Symlinks: symlinks})
	if err != nil {
		return nil, "", nil, "", fmt.Errorf("encode symlink manifest: %w", err)
	}
	manifest := Manifest{
		Schema: ManifestSchema, Entries: changes,
		DeletionsDigest: digestBytes(deletionBytes), SymlinksDigest: digestBytes(symlinkBytes),
	}
	manifestBytes, err := marshalCanonical(manifest)
	if err != nil {
		return nil, "", nil, "", fmt.Errorf("encode workspace manifest: %w", err)
	}

	archiveEntries := []archiveEntry{
		{name: manifestArchivePath, mode: 0o644, data: manifestBytes},
		{name: deletionsArchivePath, mode: 0o644, data: deletionBytes},
		{name: symlinksArchivePath, mode: 0o644, data: symlinkBytes},
	}
	for _, change := range changes {
		if change.Kind != EntryFile {
			continue
		}
		current, found := post[change.Path]
		if !found || current.kind != EntryFile || current.protected || current.digest != change.Digest {
			return nil, "", nil, "", pathError("archive", change.Path, ErrInvalidBaseline)
		}
		archiveEntries = append(archiveEntries, archiveEntry{
			name: fileArchivePrefix + change.Path,
			mode: change.Mode,
			data: append([]byte(nil), current.content...),
		})
	}
	sort.Slice(archiveEntries, func(i, j int) bool { return archiveEntries[i].name < archiveEntries[j].name })

	buffer := &boundedBuffer{max: limits.MaxArtifactBytes}
	writer := tar.NewWriter(buffer)
	fixedTime := time.Unix(0, 0).UTC()
	for _, current := range archiveEntries {
		header := &tar.Header{
			Name: current.name, Mode: current.mode, Size: int64(len(current.data)),
			ModTime: fixedTime, Typeflag: tar.TypeReg, Format: tar.FormatPAX,
			Uid: 0, Gid: 0, Uname: "", Gname: "",
		}
		if err := writer.WriteHeader(header); err != nil {
			_ = writer.Close()
			return nil, "", nil, "", fmt.Errorf("write tar header %q: %w", current.name, err)
		}
		if _, err := writer.Write(current.data); err != nil {
			_ = writer.Close()
			return nil, "", nil, "", fmt.Errorf("write tar entry %q: %w", current.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", nil, "", fmt.Errorf("close workspace tar: %w", err)
	}
	artifact := append([]byte(nil), buffer.Bytes()...)
	return manifestBytes, digestBytes(manifestBytes), artifact, digestBytes(artifact), nil
}

func marshalCanonical(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return DigestPrefix + hex.EncodeToString(sum[:])
}

type boundedBuffer struct {
	bytes.Buffer
	max int64
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	if int64(b.Len()) > b.max-int64(len(value)) {
		return 0, fmt.Errorf("%w: artifact exceeds %d bytes", ErrLimitExceeded, b.max)
	}
	return b.Buffer.Write(value)
}
