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
	"time"
)

type OperationState string

const (
	OperationStarted   OperationState = "started"
	OperationCompleted OperationState = "completed"
	OperationFailed    OperationState = "failed"
)

type OperationRecord struct {
	OperationID   string           `json:"operationID"`
	RequestDigest string           `json:"requestDigest"`
	Request       OperationRequest `json:"request"`
	State         OperationState   `json:"state"`
	FailureCode   string           `json:"failureCode,omitempty"`
	StartedAt     time.Time        `json:"startedAt"`
	FinishedAt    *time.Time       `json:"finishedAt,omitempty"`
}

type FileLedger struct {
	dir string
}

func NewFileLedger(root string) (*FileLedger, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("%w: ledger root must be absolute", ErrUnsafePath)
	}
	if err := ensurePrivateDirectory(root); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, "operations")
	if err := ensurePrivateDirectory(dir); err != nil {
		return nil, err
	}
	return &FileLedger{dir: dir}, nil
}

func (l *FileLedger) Begin(ctx context.Context, record OperationRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateOperationRecord(record, OperationStarted); err != nil {
		return err
	}
	if err := ensurePrivateDirectory(l.dir); err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(l.dir, ".operation-reservation-*.tmp")
	if err != nil {
		return fmt.Errorf("create artifact operation reservation: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath) //nolint:errcheck
	if err = temp.Chmod(0o600); err == nil {
		_, err = temp.Write(encoded)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("persist artifact operation reservation: %w", err)
	}
	path := l.recordPath(record.OperationID)
	if err := os.Link(tempPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, readErr := l.Get(ctx, record.OperationID)
			if readErr != nil {
				return readErr
			}
			if constantStringEqual(existing.RequestDigest, record.RequestDigest) {
				return ErrReplay
			}
			return ErrOperationConflict
		}
		return fmt.Errorf("commit artifact operation reservation: %w", err)
	}
	if err := syncDirectory(l.dir); err != nil {
		return fmt.Errorf("sync artifact operation reservation: %w", err)
	}
	return nil
}

func (l *FileLedger) Finish(ctx context.Context, operationID, requestDigest string, state OperationState, failureCode string, finishedAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if state != OperationCompleted && state != OperationFailed {
		return fmt.Errorf("%w: invalid terminal operation state", ErrInvalidRequest)
	}
	record, err := l.Get(ctx, operationID)
	if err != nil {
		return err
	}
	if !constantStringEqual(record.RequestDigest, requestDigest) {
		return ErrOperationConflict
	}
	if record.State == state {
		return nil
	}
	if record.State != OperationStarted {
		return ErrOperationConflict
	}
	finishedAt = finishedAt.UTC()
	record.State = state
	record.FailureCode = failureCode
	record.FinishedAt = &finishedAt
	return l.replace(record)
}

func (l *FileLedger) Get(ctx context.Context, operationID string) (OperationRecord, error) {
	if err := ctx.Err(); err != nil {
		return OperationRecord{}, err
	}
	if err := validateIdentifier("operation ID", operationID, 512); err != nil {
		return OperationRecord{}, err
	}
	if err := ensurePrivateDirectory(l.dir); err != nil {
		return OperationRecord{}, err
	}
	file, err := openFileNoFollow(l.recordPath(operationID), os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return OperationRecord{}, ErrNotFound
	}
	if err != nil {
		return OperationRecord{}, fmt.Errorf("read artifact operation record: %w", err)
	}
	defer file.Close() //nolint:errcheck
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return OperationRecord{}, ErrCorrupt
	}
	data, err := io.ReadAll(io.LimitReader(file, 64<<10+1))
	if err != nil || len(data) > 64<<10 {
		return OperationRecord{}, ErrCorrupt
	}
	var record OperationRecord
	if err := decodeStrictJSON(data, &record); err != nil {
		return OperationRecord{}, ErrCorrupt
	}
	if err := validateOperationRecord(record, record.State); err != nil || record.OperationID != operationID {
		return OperationRecord{}, ErrCorrupt
	}
	return record, nil
}

func (l *FileLedger) replace(record OperationRecord) error {
	if err := ensurePrivateDirectory(l.dir); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(l.dir, ".operation-*.tmp")
	if err != nil {
		return fmt.Errorf("create artifact operation temp record: %w", err)
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
		return fmt.Errorf("write artifact operation temp record: %w", err)
	}
	if err := replaceRegularFile(tempPath, l.recordPath(record.OperationID)); err != nil {
		return fmt.Errorf("commit artifact operation record: %w", err)
	}
	return syncDirectory(l.dir)
}

func (l *FileLedger) recordPath(operationID string) string {
	return filepath.Join(l.dir, operationRecordName(operationID))
}

func operationRecordName(operationID string) string {
	sum := sha256.Sum256([]byte(operationID))
	return hex.EncodeToString(sum[:]) + ".json"
}

func validateOperationRecord(record OperationRecord, state OperationState) error {
	if err := validateIdentifier("operation ID", record.OperationID, 512); err != nil {
		return err
	}
	if !IsRequestDigest(record.RequestDigest) {
		return fmt.Errorf("%w: invalid operation request digest", ErrInvalidRequest)
	}
	if err := record.Request.Validate(); err != nil {
		return err
	}
	if record.State != state || record.StartedAt.IsZero() {
		return fmt.Errorf("%w: invalid operation record state", ErrInvalidRequest)
	}
	return nil
}
