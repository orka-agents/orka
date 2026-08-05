package artifactcap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

type ServiceConfig struct {
	Root           string
	Secret         []byte
	MaxObjectBytes int64
	Ledger         ReplayLedger
	Now            func() time.Time
}

type Service struct {
	secret      []byte
	store       *FileObjectStore
	ledger      ReplayLedger
	retirements *retirementStore
	now         func() time.Time
}

func NewService(config ServiceConfig) (*Service, error) {
	if len(config.Secret) < MinSecretBytes {
		return nil, ErrUnauthorized
	}
	objectStore, err := NewFileObjectStore(config.Root, config.MaxObjectBytes)
	if err != nil {
		return nil, err
	}
	ledger := config.Ledger
	if ledger == nil {
		ledger, err = NewFileLedger(config.Root)
		if err != nil {
			return nil, err
		}
	}
	retirements, err := newRetirementStore(config.Root)
	if err != nil {
		return nil, err
	}
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		secret: append([]byte(nil), config.Secret...), store: objectStore, ledger: ledger, retirements: retirements, now: now,
	}, nil
}

func (s *Service) Upload(ctx context.Context, token string, request PresentedRequest, body io.Reader) (Artifact, error) {
	claims, err := Verify(s.secret, token, request, s.now())
	if err != nil {
		return Artifact{}, err
	}
	if claims.Request.Operation != OperationUpload {
		return Artifact{}, ErrUnauthorized
	}
	release, err := acquireArtifactRootLock(ctx, s.store.root, false)
	if err != nil {
		return Artifact{}, err
	}
	defer func() { _ = release() }()
	retired, err := s.retirements.isRetired(ctx, claims.Request.Identity)
	if err != nil {
		return Artifact{}, err
	}
	if retired {
		return Artifact{}, ErrUnauthorized
	}
	record := newOperationRecord(claims, s.now())
	if err := s.ledger.Begin(ctx, record); err != nil {
		return Artifact{}, err
	}
	artifact, putErr := s.store.Put(ctx, claims.Request, body, s.now())
	if putErr != nil {
		s.finish(record, OperationFailed, failureCode(putErr))
		return Artifact{}, putErr
	}
	if err := s.ledger.Finish(context.WithoutCancel(ctx), record.OperationID, record.RequestDigest, OperationCompleted, "", s.now()); err != nil {
		return Artifact{}, fmt.Errorf("finalize artifact upload operation: %w", err)
	}
	return artifact, nil
}

func (s *Service) OpenDownload(ctx context.Context, token string, request PresentedRequest) (*Download, error) {
	claims, err := Verify(s.secret, token, request, s.now())
	if err != nil {
		return nil, err
	}
	if claims.Request.Operation != OperationDownload {
		return nil, ErrUnauthorized
	}
	release, err := acquireArtifactRootLock(ctx, s.store.root, false)
	if err != nil {
		return nil, err
	}
	releaseOnError := true
	defer func() {
		if releaseOnError {
			_ = release()
		}
	}()
	retired, err := s.retirements.isRetired(ctx, claims.Request.Identity)
	if err != nil {
		return nil, err
	}
	if retired {
		return nil, ErrUnauthorized
	}
	record := newOperationRecord(claims, s.now())
	if err := s.ledger.Begin(ctx, record); err != nil {
		return nil, err
	}
	file, artifact, openErr := s.store.OpenVerified(ctx, claims.Request)
	if openErr != nil {
		s.finish(record, OperationFailed, failureCode(openErr))
		return nil, openErr
	}
	releaseOnError = false
	return &Download{
		Artifact: artifact, file: file, ledger: s.ledger, record: record, now: s.now, releaseStorage: release,
	}, nil
}

func (s *Service) finish(record OperationRecord, state OperationState, code string) {
	_ = s.ledger.Finish(context.Background(), record.OperationID, record.RequestDigest, state, code, s.now())
}

func newOperationRecord(claims CapabilityClaims, now time.Time) OperationRecord {
	return OperationRecord{
		OperationID:   claims.Request.OperationID,
		RequestDigest: claims.RequestDigest,
		Request:       claims.Request,
		State:         OperationStarted,
		StartedAt:     now.UTC(),
	}
}

func failureCode(err error) string {
	switch {
	case errors.Is(err, ErrTooLarge):
		return "too_large"
	case errors.Is(err, ErrPartialUpload):
		return "partial_upload"
	case errors.Is(err, ErrDigestMismatch):
		return "digest_mismatch"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrUnsafePath):
		return "unsafe_path"
	case errors.Is(err, ErrCorrupt):
		return "corrupt"
	default:
		return "io_failure"
	}
}

type Download struct {
	Artifact Artifact

	file   *os.File
	ledger ReplayLedger
	record OperationRecord
	now    func() time.Time

	mu             sync.Mutex
	readBytes      int64
	finished       bool
	finishErr      error
	releaseStorage func() error
	releaseOnce    sync.Once
	releaseErr     error
}

func (d *Download) Read(buffer []byte) (int, error) {
	d.mu.Lock()
	if d.finished {
		err := d.finishErr
		d.mu.Unlock()
		if err != nil {
			return 0, err
		}
		return 0, io.EOF
	}
	d.mu.Unlock()

	n, err := d.file.Read(buffer)
	d.mu.Lock()
	d.readBytes += int64(n)
	readBytes := d.readBytes
	d.mu.Unlock()
	if readBytes > d.Artifact.SizeBytes {
		return n, d.finish(OperationFailed, "length_mismatch", ErrCorrupt)
	}
	if readBytes == d.Artifact.SizeBytes && err == nil {
		if finishErr := d.finish(OperationCompleted, "", nil); finishErr != nil {
			return n, finishErr
		}
	}
	if errors.Is(err, io.EOF) {
		if readBytes != d.Artifact.SizeBytes {
			return n, d.finish(OperationFailed, "partial_download", ErrCorrupt)
		}
		finishErr := d.finish(OperationCompleted, "", nil)
		if finishErr != nil {
			return n, finishErr
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, d.finish(OperationFailed, "io_failure", err)
	}
	return n, err
}

func (d *Download) Close() error {
	d.mu.Lock()
	readBytes := d.readBytes
	finished := d.finished
	finishErr := d.finishErr
	d.mu.Unlock()
	if !finished {
		state := OperationFailed
		code := "partial_download"
		if readBytes == d.Artifact.SizeBytes {
			state = OperationCompleted
			code = ""
		}
		finishErr = d.finish(state, code, nil)
	}
	closeErr := d.file.Close()
	d.releaseOnce.Do(func() {
		if d.releaseStorage != nil {
			d.releaseErr = d.releaseStorage()
		}
	})
	return errors.Join(finishErr, closeErr, d.releaseErr)
}

func (d *Download) finish(state OperationState, code string, cause error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finished {
		if d.finishErr != nil {
			return d.finishErr
		}
		return cause
	}
	d.finished = true
	if err := d.ledger.Finish(context.Background(), d.record.OperationID, d.record.RequestDigest, state, code, d.now()); err != nil {
		d.finishErr = fmt.Errorf("finalize artifact download operation: %w", err)
		return d.finishErr
	}
	d.finishErr = cause
	return cause
}
