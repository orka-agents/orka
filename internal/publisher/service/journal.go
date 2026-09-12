package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const journalVersion = "orka.workspace-publisher.journal.v1"

const journalReplayRetention = 24 * time.Hour

type journalState string

const (
	journalStarted   journalState = "started"
	journalCompleted journalState = "completed"
	journalFailed    journalState = "failed"
)

type journalRecord struct {
	Version       string                    `json:"version"`
	Operation     Operation                 `json:"operation"`
	Metadata      OperationMetadata         `json:"metadata"`
	RequestDigest string                    `json:"requestDigest"`
	State         journalState              `json:"state"`
	StatusCode    int                       `json:"statusCode,omitempty"`
	Response      json.RawMessage           `json:"response,omitempty"`
	RemoteAttempt int                       `json:"remoteAttempt,omitempty"`
	Workspace     *WorkspacePrepareResponse `json:"workspace,omitempty"`
	StartedAt     time.Time                 `json:"startedAt"`
	UpdatedAt     time.Time                 `json:"updatedAt"`
}

type journalStore struct {
	root     string
	objects  string
	maxBytes int64
	now      func() time.Time
	mu       sync.Mutex
}

func newJournalStore(root string, maxBytes int64, now func() time.Time) (*journalStore, error) {
	if err := ensurePrivateDirectory(root); err != nil {
		return nil, fmt.Errorf("prepare journal root: %w", err)
	}
	objects := filepath.Join(root, "workspace-objects")
	if err := ensurePrivateDirectory(objects); err != nil {
		return nil, fmt.Errorf("prepare workspace object root: %w", err)
	}
	store := &journalStore{root: root, objects: objects, maxBytes: maxBytes, now: now}
	if err := store.reclaimLocked("", ""); err != nil {
		return nil, err
	}
	if _, err := store.totalBytes(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *journalStore) begin(ctx context.Context, operation Operation, metadata OperationMetadata, requestDigest string) (journalRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return journalRecord{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reclaimLocked(metadata.OperationID, ""); err != nil {
		return journalRecord{}, false, err
	}
	record, found, err := s.readLocked(metadata.OperationID)
	if err != nil {
		return journalRecord{}, false, err
	}
	if found {
		if record.Operation != operation || record.Metadata != metadata || !constantEqual(record.RequestDigest, requestDigest) {
			return journalRecord{}, false, apiError(ErrOperationConflict, "operation_conflict", "operation ID was reused with different content", 409, false, nil)
		}
		return record, true, nil
	}
	now := s.now().UTC()
	record = journalRecord{
		Version: journalVersion, Operation: operation, Metadata: metadata, RequestDigest: requestDigest,
		State: journalStarted, StartedAt: now, UpdatedAt: now,
	}
	if err := s.writeLocked(record); err != nil {
		return journalRecord{}, false, err
	}
	if err := s.reclaimLocked(metadata.OperationID, ""); err != nil {
		return journalRecord{}, false, err
	}
	return record, false, nil
}

func (s *journalStore) complete(ctx context.Context, operationID, requestDigest string, status int, response []byte) error {
	return s.transition(ctx, operationID, requestDigest, journalCompleted, status, response)
}

func (s *journalStore) fail(ctx context.Context, operationID, requestDigest string, status int, response []byte) error {
	return s.transition(ctx, operationID, requestDigest, journalFailed, status, response)
}

func (s *journalStore) transition(ctx context.Context, operationID, requestDigest string, state journalState, status int, response []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if state != journalCompleted && state != journalFailed || status < 100 || status > 599 || len(response) == 0 {
		return fmt.Errorf("invalid journal transition")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, found, err := s.readLocked(operationID)
	if err != nil {
		return err
	}
	if !found || !constantEqual(record.RequestDigest, requestDigest) {
		return ErrOperationConflict
	}
	if record.State == state && record.StatusCode == status && bytes.Equal(record.Response, response) {
		return s.reclaimLocked(operationID, "")
	}
	if record.State != journalStarted {
		return ErrOperationConflict
	}
	record.State = state
	record.StatusCode = status
	record.Response = append(json.RawMessage(nil), response...)
	record.Workspace = nil
	record.UpdatedAt = s.now().UTC()
	if err := s.writeLocked(record); err != nil {
		return err
	}
	return s.reclaimLocked(operationID, "")
}

func (s *journalStore) setWorkspaceObjectFile(
	ctx context.Context,
	operationID, requestDigest string,
	response WorkspacePrepareResponse,
	source string,
	size int64,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.workspaceObjectPath(response.Artifact.Digest)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, found, err := s.readLocked(operationID)
	if err != nil {
		return err
	}
	if !found || record.State != journalStarted || !constantEqual(record.RequestDigest, requestDigest) {
		return ErrOperationConflict
	}
	if record.Workspace != nil {
		if *record.Workspace != response {
			return ErrOperationConflict
		}
		return nil
	}
	if err := s.putWorkspaceObjectFileLocked(path, response.Artifact.Digest, source, size, operationID); err != nil {
		return err
	}
	record.Workspace = &response
	record.UpdatedAt = s.now().UTC()
	if err := s.writeLocked(record); err != nil {
		return err
	}
	return nil
}

func (s *journalStore) nextRemoteAttempt(ctx context.Context, operationID, requestDigest string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, found, err := s.readLocked(operationID)
	if err != nil {
		return 0, err
	}
	if !found || record.State != journalStarted || !constantEqual(record.RequestDigest, requestDigest) {
		return 0, ErrOperationConflict
	}
	record.RemoteAttempt++
	if record.RemoteAttempt > 1_000_000 {
		return 0, ErrOperationConflict
	}
	record.UpdatedAt = s.now().UTC()
	if err := s.writeLocked(record); err != nil {
		return 0, err
	}
	return record.RemoteAttempt, nil
}

func (s *journalStore) workspaceObjectPath(digest string) (string, error) {
	if !isDigest(digest) {
		return "", ErrInvalidRequest
	}
	return filepath.Join(s.objects, digest[len("sha256:"):]+".tar"), nil
}

func (s *journalStore) putWorkspaceObjectFileLocked(path, digest, source string, size int64, operationID string) error {
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != size {
			return ErrOperationConflict
		}
		return nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err := s.ensureCapacityLocked(0, size, operationID, digest); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close() //nolint:errcheck
	temporary, err := os.CreateTemp(s.objects, ".workspace-object-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath) //nolint:errcheck
	err = temporary.Chmod(0o600)
	if err == nil {
		_, err = io.CopyN(temporary, input, size)
	}
	if err == nil {
		var extra [1]byte
		if count, readErr := input.Read(extra[:]); count != 0 || !errors.Is(readErr, io.EOF) {
			err = fmt.Errorf("workspace object source size changed")
		}
	}
	if err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directory, err := os.Open(s.objects)
	if err != nil {
		return err
	}
	defer directory.Close() //nolint:errcheck
	if err := directory.Sync(); err != nil {
		return err
	}
	return nil
}

func (s *journalStore) openWorkspaceObject(digest string, size int64) (*os.File, error) {
	path, err := s.workspaceObjectPath(digest)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != size {
		return nil, fmt.Errorf("workspace object is missing or corrupt")
	}
	return os.Open(path)
}

func (s *journalStore) readLocked(operationID string) (journalRecord, bool, error) {
	path := s.recordPath(operationID)
	data, err := readRegularFile(path, 4<<20)
	if errors.Is(err, os.ErrNotExist) {
		return journalRecord{}, false, nil
	}
	if err != nil {
		return journalRecord{}, false, err
	}
	var record journalRecord
	if err := decodeStrict(data, &record); err != nil {
		return journalRecord{}, false, fmt.Errorf("decode durable operation journal: %w", err)
	}
	if err := validateJournalRecord(record, operationID); err != nil {
		return journalRecord{}, false, err
	}
	return record, true, nil
}

func (s *journalStore) writeLocked(record journalRecord) error {
	if err := validateJournalRecord(record, record.Metadata.OperationID); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := s.recordPath(record.Metadata.OperationID)
	var oldSize int64
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("unsafe durable operation record")
		}
		oldSize = info.Size()
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err := s.ensureCapacityLocked(oldSize, int64(len(data)), record.Metadata.OperationID, journalWorkspaceDigest(record)); err != nil {
		return err
	}
	return writeDurable(path, data, 0o600)
}

func (s *journalStore) ensureCapacityLocked(replacing, adding int64, protectedOperationID, protectedWorkspaceDigest string) error {
	if err := s.reclaimLocked(protectedOperationID, protectedWorkspaceDigest); err != nil {
		return err
	}
	total, err := s.totalBytes()
	if err != nil {
		return err
	}
	if total-replacing+adding > s.maxBytes {
		return apiError(ErrJournalFull, "journal_full", "durable publisher volume is full", 507, true, nil)
	}
	return nil
}

type journalDiskRecord struct {
	path   string
	record journalRecord
}

// reclaimLocked removes workspace archives after no started operation can
// need them for an upload retry. It also keeps at most the latest replay record
// at each durable owner-progress boundary: beginning a later operation proves
// the controller consumed and persisted every earlier terminal result for that
// Task or Publication. The active operation is protected so a delayed replay
// cannot lose its record while it is being served.
func (s *journalStore) reclaimLocked(protectedOperationID, protectedWorkspaceDigest string) error {
	records, err := s.journalRecordsLocked()
	if err != nil {
		return err
	}

	latestRankByOwner := make(map[string]int, len(records))
	liveWorkspaceObjects := make(map[string]struct{})
	if protectedWorkspaceDigest != "" {
		liveWorkspaceObjects[protectedWorkspaceDigest] = struct{}{}
	}
	for _, entry := range records {
		record := entry.record
		owner := journalOwnerKey(record)
		if rank := journalOperationRank(record.Operation); rank > latestRankByOwner[owner] {
			latestRankByOwner[owner] = rank
		}
		if record.State == journalStarted && record.Workspace != nil && isDigest(record.Workspace.Artifact.Digest) {
			liveWorkspaceObjects[record.Workspace.Artifact.Digest] = struct{}{}
		}
	}

	if err := s.reclaimJournalRecordsLocked(
		records, latestRankByOwner, protectedOperationID, s.now().UTC().Add(-journalReplayRetention),
	); err != nil {
		return err
	}
	return s.reclaimWorkspaceObjectsLocked(liveWorkspaceObjects)
}

func (s *journalStore) reclaimJournalRecordsLocked(
	records []journalDiskRecord,
	latestRankByOwner map[string]int,
	protectedOperationID string,
	replayCutoff time.Time,
) error {
	rootChanged := false
	for _, entry := range records {
		record := entry.record
		if record.Metadata.OperationID == protectedOperationID || record.State == journalStarted {
			continue
		}
		if record.UpdatedAt.After(replayCutoff) {
			continue
		}
		if journalOperationRank(record.Operation) >= latestRankByOwner[journalOwnerKey(record)] {
			continue
		}
		if err := os.Remove(entry.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		rootChanged = true
	}
	if rootChanged {
		return syncDirectory(s.root)
	}
	return nil
}

func (s *journalStore) reclaimWorkspaceObjectsLocked(liveWorkspaceObjects map[string]struct{}) error {
	objectEntries, err := os.ReadDir(s.objects)
	if err != nil {
		return err
	}
	objectsChanged := false
	for _, entry := range objectEntries {
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		name := entry.Name()
		if info.Mode().IsRegular() && strings.HasPrefix(name, ".workspace-object-") && strings.HasSuffix(name, ".tmp") {
			if err := os.Remove(filepath.Join(s.objects, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			objectsChanged = true
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !strings.HasSuffix(name, ".tar") {
			return fmt.Errorf("durable publisher volume contains an unsafe workspace object")
		}
		digest := "sha256:" + strings.TrimSuffix(name, ".tar")
		if !isDigest(digest) {
			return fmt.Errorf("durable publisher volume contains an invalid workspace object")
		}
		if _, live := liveWorkspaceObjects[digest]; live {
			continue
		}
		if err := os.Remove(filepath.Join(s.objects, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		objectsChanged = true
	}
	if objectsChanged {
		return syncDirectory(s.objects)
	}
	return nil
}

func (s *journalStore) journalRecordsLocked() ([]journalDiskRecord, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	records := make([]journalDiskRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() == filepath.Base(s.objects) {
			if !entry.IsDir() {
				return nil, fmt.Errorf("workspace object root is not a directory")
			}
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil, infoErr
		}
		if info.Mode().IsRegular() && strings.HasPrefix(entry.Name(), ".orka-publisher-") && strings.HasSuffix(entry.Name(), ".tmp") {
			if err := os.Remove(filepath.Join(s.root, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil, fmt.Errorf("durable publisher volume contains an unsafe journal entry")
		}
		path := filepath.Join(s.root, entry.Name())
		data, readErr := readRegularFile(path, 4<<20)
		if readErr != nil {
			return nil, readErr
		}
		var record journalRecord
		if decodeErr := decodeStrict(data, &record); decodeErr != nil {
			return nil, fmt.Errorf("decode durable operation journal: %w", decodeErr)
		}
		if validateErr := validateJournalRecord(record, record.Metadata.OperationID); validateErr != nil || s.recordPath(record.Metadata.OperationID) != path {
			return nil, fmt.Errorf("durable operation journal is corrupt")
		}
		records = append(records, journalDiskRecord{path: path, record: record})
	}
	return records, nil
}

func journalWorkspaceDigest(record journalRecord) string {
	if record.Workspace == nil || !isDigest(record.Workspace.Artifact.Digest) {
		return ""
	}
	return record.Workspace.Artifact.Digest
}

func journalOwnerKey(record journalRecord) string {
	metadata := record.Metadata
	if metadata.TaskID != "" {
		incarnation := metadata.OperationID
		for _, prefix := range []string{"workspace-resolve-", "workspace-prepare-"} {
			if record.Operation == OperationWorkspaceResolve && prefix != "workspace-resolve-" {
				continue
			}
			if record.Operation == OperationWorkspacePrepare && prefix != "workspace-prepare-" {
				continue
			}
			if strings.HasPrefix(metadata.OperationID, prefix) && len(metadata.OperationID) > len(prefix) {
				incarnation = strings.TrimPrefix(metadata.OperationID, prefix)
				break
			}
		}
		return "task\x00" + metadata.Namespace + "\x00" + metadata.TaskID + "\x00" + incarnation
	}
	return "publication\x00" + metadata.Namespace + "\x00" + metadata.PublicationID
}

func journalOperationRank(operation Operation) int {
	switch operation {
	case OperationWorkspaceResolve, OperationPublicationPreflight:
		return 1
	case OperationWorkspacePrepare, OperationPublicationPrepare:
		return 2
	case OperationPublicationPublish:
		return 3
	case OperationPublicationVerify:
		return 4
	case OperationPullRequestReconcile:
		return 5
	case OperationPublicationReclaim:
		return 6
	default:
		return 0
	}
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close() //nolint:errcheck
	return directory.Sync()
}

func (s *journalStore) totalBytes() (int64, error) {
	var total int64
	err := filepath.WalkDir(s.root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!entry.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("durable publisher volume contains an unsafe entry")
		}
		if info.Mode().IsRegular() {
			if info.Size() > s.maxBytes-total {
				return ErrJournalFull
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func (s *journalStore) recordPath(operationID string) string {
	return filepath.Join(s.root, hashID("operation", operationID)+".json")
}

func validateJournalRecord(record journalRecord, operationID string) error {
	if record.Version != journalVersion || record.Operation.Path() == "" || record.Metadata.OperationID != operationID ||
		record.Metadata.validateFor(record.Operation) != nil || !isDigest(record.RequestDigest) || record.StartedAt.IsZero() || record.UpdatedAt.IsZero() {
		return fmt.Errorf("durable operation journal is corrupt")
	}
	switch record.State {
	case journalStarted:
		if record.StatusCode != 0 || len(record.Response) != 0 {
			return fmt.Errorf("started durable operation has a terminal response")
		}
	case journalCompleted, journalFailed:
		if record.StatusCode < 100 || record.StatusCode > 599 || len(record.Response) == 0 {
			return fmt.Errorf("terminal durable operation is incomplete")
		}
	default:
		return fmt.Errorf("durable operation state is invalid")
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("path is not a private no-follow directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func readRegularFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit {
		return nil, fmt.Errorf("unsafe or oversized durable file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, fmt.Errorf("read durable file: %w", err)
	}
	return data, nil
}

func writeDurable(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := ensurePrivateDirectory(directory); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".orka-publisher-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath) //nolint:errcheck
	err = temporary.Chmod(mode)
	if err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directoryFile, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer directoryFile.Close() //nolint:errcheck
	return directoryFile.Sync()
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}
