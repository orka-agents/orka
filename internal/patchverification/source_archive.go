package patchverification

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	sourceMaxArchiveBytes  = 32 << 20
	sourceMaxManifestBytes = 1 << 20
	sourceMaxTreeEntries   = 8192
	sourceMaxPathBytes     = 1024
	sourceMaxPathDepth     = 32
)

var (
	sourceCredentials  = CredentialMatcher{}
	errSourceInput     = errors.New("invalid source request")
	errSourcePath      = errors.New("unsafe source path")
	errSourceLimit     = errors.New("source input exceeds limits")
	errSourceGit       = errors.New("bounded Git object operation failed")
	errSourceIntegrity = errors.New("source object integrity check failed")
)

func sourcePathsOverlap(first, second string) bool {
	return first == second || strings.HasPrefix(first, second+string(filepath.Separator)) ||
		strings.HasPrefix(second, first+string(filepath.Separator))
}

type sourceContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader sourceContextReader) Read(content []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(content)
}

func sourceCleanText(content string) bool {
	return utf8.ValidString(content) && !strings.ContainsFunc(content, func(character rune) bool {
		return unicode.IsControl(character) && character != '\n' && character != '\r' && character != '\t'
	})
}

func sourceWriteSnapshot(directory, name string, content []byte, mode os.FileMode) error {
	filename := filepath.Join(directory, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return errSourcePath
	}
	opened, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errSourcePath
	}
	n, writeErr := opened.Write(content)
	readonly := os.FileMode(0444)
	if mode.Perm()&0111 != 0 {
		readonly = 0555
	}
	chmodErr, closeErr := opened.Chmod(readonly), opened.Close()
	if writeErr != nil || n != len(content) || chmodErr != nil || closeErr != nil {
		return errSourcePath
	}
	return nil
}

func sourceSealDirectories(directory string) error {
	return filepath.WalkDir(directory, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || (entry.IsDir() && os.Chmod(filename, 0555) != nil) {
			return errSourcePath
		}
		return nil
	})
}

func sourceExtractArchive(ctx context.Context, content []byte, directory string) error {
	return sourceExtractArchiveBounds(ctx, content, directory, sourceMaxArchiveBytes, sourceMaxArchiveBytes, sourceMaxTreeEntries)
}

func sourceExtractArchiveBounds(ctx context.Context, content []byte, directory string, archiveLimit, contentLimit, entryLimit int) error {
	if len(content) > archiveLimit {
		return errSourceLimit
	}
	trailer := [1024]byte{}
	if len(content) < len(trailer) || len(content)%512 != 0 || !bytes.Equal(content[len(content)-len(trailer):], trailer[:]) {
		return errSourceIntegrity
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return errSourcePath
	}
	input := bytes.NewReader(content)
	reader := tar.NewReader(sourceContextReader{ctx: ctx, reader: input})
	seen := make(map[string]bool)
	contentBytes := int64(0)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			if input.Len() != 0 {
				return errSourceIntegrity
			}
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return errSourceIntegrity
		}
		if err := sourceValidateArchiveHeaderLimit(header, contentLimit); err != nil {
			return err
		}
		name := header.Name
		if header.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if !sourceValidPath(name) || seen[name] || len(seen) >= entryLimit {
			return errSourcePath
		}
		seen[name] = true
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Mode != 0755 || header.Size != 0 || os.MkdirAll(filepath.Join(directory, filepath.FromSlash(name)), 0700) != nil {
				return errSourcePath
			}
		case tar.TypeReg:
			if header.Size > int64(contentLimit)-contentBytes {
				return errSourceLimit
			}
			contentBytes += header.Size
			if header.Mode != 0644 && header.Mode != 0755 {
				return errSourcePath
			}
			blob, err := io.ReadAll(reader)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil || int64(len(blob)) != header.Size {
				return errSourceIntegrity
			}
			if err := sourceWriteSnapshot(directory, name, blob, os.FileMode(header.Mode)); err != nil {
				return err
			}
		default:
			return errSourcePath
		}
	}
	return sourceSealDirectories(directory)
}

func sourceValidateArchiveHeader(header *tar.Header) error {
	return sourceValidateArchiveHeaderLimit(header, sourceMaxArchiveBytes)
}

func sourceValidateArchiveHeaderLimit(header *tar.Header, contentLimit int) error {
	if header.Linkname != "" || header.Size < 0 || header.Size > int64(contentLimit) {
		return errSourceIntegrity
	}
	for name := range header.PAXRecords {
		if strings.HasPrefix(name, "SCHILY.xattr.") && header.Typeflag != tar.TypeXGlobalHeader {
			return errSourceIntegrity
		}
	}
	for name := range header.PAXRecords {
		if name != "path" {
			return errSourcePath
		}
	}
	return nil
}

func sourceValidPath(name string) bool {
	if name == "" || len(name) > sourceMaxPathBytes || !utf8.ValidString(name) || path.IsAbs(name) || path.Clean(name) != name || strings.ContainsAny(name, "\\:") {
		return false
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		return false
	}
	components := strings.Split(name, "/")
	if len(components) > sourceMaxPathDepth {
		return false
	}
	for _, component := range components {
		if component == "." || component == ".." || len(component) > 255 || strings.EqualFold(strings.TrimRight(component, " ."), ".git") {
			return false
		}
	}
	return true
}

func sourceValidCommit(commit string) bool {
	return gitIdentityPattern.MatchString(commit)
}
