package artifactcap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const defaultCopyBufferBytes = 128 << 10

type FileObjectStore struct {
	root           string
	objectsDir     string
	metadataDir    string
	tempDir        string
	maxObjectBytes int64
	commitMu       sync.Mutex
}

func NewFileObjectStore(root string, maxObjectBytes int64) (*FileObjectStore, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("%w: artifact root must be absolute", ErrUnsafePath)
	}
	if maxObjectBytes <= 0 {
		return nil, fmt.Errorf("%w: maximum object size must be positive", ErrInvalidRequest)
	}
	store := &FileObjectStore{
		root:           root,
		objectsDir:     filepath.Join(root, "objects"),
		metadataDir:    filepath.Join(root, "metadata"),
		tempDir:        filepath.Join(root, "tmp"),
		maxObjectBytes: maxObjectBytes,
	}
	for _, dir := range []string{store.root, store.objectsDir, store.metadataDir, store.tempDir} {
		if err := ensurePrivateDirectory(dir); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (s *FileObjectStore) Put(ctx context.Context, request OperationRequest, body io.Reader, now time.Time) (Artifact, error) {
	if request.ContentLength > s.maxObjectBytes {
		return Artifact{}, ErrTooLarge
	}
	if body == nil {
		return Artifact{}, ErrPartialUpload
	}
	if err := s.verifyDirectories(); err != nil {
		return Artifact{}, err
	}
	temp, err := os.CreateTemp(s.tempDir, ".upload-*.tmp")
	if err != nil {
		return Artifact{}, fmt.Errorf("create artifact upload temp file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath) //nolint:errcheck
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return Artifact{}, fmt.Errorf("secure artifact upload temp file: %w", err)
	}
	hash := sha256.New()
	limited := io.LimitReader(contextReader{ctx: ctx, r: body}, request.ContentLength+1)
	written, copyErr := io.CopyBuffer(io.MultiWriter(temp, hash), limited, make([]byte, defaultCopyBufferBytes))
	if copyErr != nil {
		_ = temp.Close()
		return Artifact{}, ErrPartialUpload
	}
	if written < request.ContentLength {
		_ = temp.Close()
		return Artifact{}, ErrPartialUpload
	}
	if written > request.ContentLength {
		_ = temp.Close()
		return Artifact{}, fmt.Errorf("%w: received more than declared content length", ErrInvalidRequest)
	}
	actualDigest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if !constantStringEqual(actualDigest, request.ObjectDigest) {
		_ = temp.Close()
		return Artifact{}, ErrDigestMismatch
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return Artifact{}, fmt.Errorf("sync artifact upload temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return Artifact{}, fmt.Errorf("close artifact upload temp file: %w", err)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	artifactID, err := ArtifactIDForDigest(request.ObjectDigest)
	if err != nil {
		return Artifact{}, err
	}
	artifact := Artifact{
		ArtifactID: artifactID,
		Digest:     request.ObjectDigest,
		SizeBytes:  request.ContentLength,
		MediaType:  request.MediaType,
		CreatedAt:  now.UTC(),
	}
	return s.commit(tempPath, artifact)
}

func (s *FileObjectStore) OpenVerified(ctx context.Context, request OperationRequest) (*os.File, Artifact, error) {
	if request.ContentLength > s.maxObjectBytes {
		return nil, Artifact{}, ErrTooLarge
	}
	if err := s.verifyDirectories(); err != nil {
		return nil, Artifact{}, err
	}
	artifact, err := s.readMetadata(request.ObjectDigest)
	if err != nil {
		return nil, Artifact{}, err
	}
	if artifact.SizeBytes != request.ContentLength || !constantStringEqual(artifact.MediaType, request.MediaType) {
		return nil, Artifact{}, ErrCorrupt
	}
	objectPath, err := s.objectPath(request.ObjectDigest)
	if err != nil {
		return nil, Artifact{}, err
	}
	file, err := openFileNoFollow(objectPath, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, Artifact{}, ErrNotFound
	}
	if err != nil {
		return nil, Artifact{}, fmt.Errorf("open artifact object: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != artifact.SizeBytes {
		_ = file.Close()
		return nil, Artifact{}, ErrCorrupt
	}
	hash := sha256.New()
	read, err := io.CopyBuffer(hash, contextReader{ctx: ctx, r: file}, make([]byte, defaultCopyBufferBytes))
	if err != nil || read != artifact.SizeBytes {
		_ = file.Close()
		return nil, Artifact{}, ErrCorrupt
	}
	actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if !constantStringEqual(actual, artifact.Digest) {
		_ = file.Close()
		return nil, Artifact{}, ErrCorrupt
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, Artifact{}, ErrCorrupt
	}
	return file, artifact, nil
}

func (s *FileObjectStore) commit(tempPath string, artifact Artifact) (Artifact, error) {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	if err := s.verifyDirectories(); err != nil {
		return Artifact{}, err
	}
	objectPath, err := s.objectPath(artifact.Digest)
	if err != nil {
		return Artifact{}, err
	}
	if info, statErr := os.Lstat(objectPath); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return Artifact{}, ErrUnsafePath
		}
		existing, verifyErr := s.verifyExistingObject(objectPath, artifact)
		if verifyErr != nil {
			return Artifact{}, verifyErr
		}
		artifact = existing
	} else if !os.IsNotExist(statErr) {
		return Artifact{}, fmt.Errorf("inspect artifact object: %w", statErr)
	} else {
		if err := os.Rename(tempPath, objectPath); err != nil {
			return Artifact{}, fmt.Errorf("atomically commit artifact object: %w", err)
		}
		if err := syncDirectory(s.objectsDir); err != nil {
			return Artifact{}, fmt.Errorf("sync artifact object directory: %w", err)
		}
	}
	metadataPath, err := s.metadataPath(artifact.Digest)
	if err != nil {
		return Artifact{}, err
	}
	if info, statErr := os.Lstat(metadataPath); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return Artifact{}, ErrUnsafePath
		}
		existing, readErr := s.readMetadata(artifact.Digest)
		if readErr != nil || existing.SizeBytes != artifact.SizeBytes || !constantStringEqual(existing.MediaType, artifact.MediaType) {
			return Artifact{}, ErrCorrupt
		}
		return existing, nil
	} else if !os.IsNotExist(statErr) {
		return Artifact{}, fmt.Errorf("inspect artifact metadata: %w", statErr)
	}
	if err := s.writeMetadata(metadataPath, artifact); err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}

func (s *FileObjectStore) verifyExistingObject(objectPath string, expected Artifact) (Artifact, error) {
	file, err := openFileNoFollow(objectPath, os.O_RDONLY, 0)
	if err != nil {
		return Artifact{}, err
	}
	defer file.Close() //nolint:errcheck
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != expected.SizeBytes {
		return Artifact{}, ErrCorrupt
	}
	hash := sha256.New()
	read, err := io.CopyBuffer(hash, file, make([]byte, defaultCopyBufferBytes))
	if err != nil || read != expected.SizeBytes {
		return Artifact{}, ErrCorrupt
	}
	actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if !constantStringEqual(actual, expected.Digest) {
		return Artifact{}, ErrCorrupt
	}
	if metadata, err := s.readMetadata(expected.Digest); err == nil {
		if metadata.SizeBytes != expected.SizeBytes || !constantStringEqual(metadata.MediaType, expected.MediaType) {
			return Artifact{}, ErrCorrupt
		}
		return metadata, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Artifact{}, err
	}
	return expected, nil
}

func (s *FileObjectStore) writeMetadata(path string, artifact Artifact) error {
	data, err := json.Marshal(artifact)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(s.metadataDir, ".metadata-*.tmp")
	if err != nil {
		return fmt.Errorf("create artifact metadata temp file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath) //nolint:errcheck
	if err = temp.Chmod(0o600); err == nil {
		_, err = temp.Write(data)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write artifact metadata temp file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("atomically commit artifact metadata: %w", err)
	}
	if err := syncDirectory(s.metadataDir); err != nil {
		return fmt.Errorf("sync artifact metadata directory: %w", err)
	}
	return nil
}

func (s *FileObjectStore) readMetadata(digest string) (Artifact, error) {
	path, err := s.metadataPath(digest)
	if err != nil {
		return Artifact{}, err
	}
	file, err := openFileNoFollow(path, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return Artifact{}, ErrNotFound
	}
	if err != nil {
		return Artifact{}, fmt.Errorf("open artifact metadata: %w", err)
	}
	defer file.Close() //nolint:errcheck
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return Artifact{}, ErrCorrupt
	}
	data, err := io.ReadAll(io.LimitReader(file, 16<<10+1))
	if err != nil || len(data) > 16<<10 {
		return Artifact{}, ErrCorrupt
	}
	var artifact Artifact
	if err := decodeStrictJSON(data, &artifact); err != nil || artifact.Validate() != nil || !constantStringEqual(artifact.Digest, digest) {
		return Artifact{}, ErrCorrupt
	}
	return artifact, nil
}

func (s *FileObjectStore) verifyDirectories() error {
	for _, dir := range []string{s.root, s.objectsDir, s.metadataDir, s.tempDir} {
		if err := ensurePrivateDirectory(dir); err != nil {
			return err
		}
	}
	return nil
}

func (s *FileObjectStore) objectPath(digest string) (string, error) {
	hexDigest, err := DigestHex(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.objectsDir, hexDigest), nil
}

func (s *FileObjectStore) metadataPath(digest string) (string, error) {
	hexDigest, err := DigestHex(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.metadataDir, hexDigest+".json"), nil
}
