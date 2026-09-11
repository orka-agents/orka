package artifactcap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestServiceUploadDownloadAndPersistentReplay(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	secret := []byte(strings.Repeat("k", MinSecretBytes))
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	service := newTestService(t, root, secret, 1<<20, now)
	data := []byte("durable artifact")
	uploadRequest := testOperationRequest(OperationUpload, data, "upload-operation")
	uploadAuthorization := issueTestAuthorization(t, secret, uploadRequest, now)
	artifact, err := service.Upload(context.Background(), uploadAuthorization.Capability, present(uploadRequest, uploadAuthorization), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Digest != uploadRequest.ObjectDigest || artifact.SizeBytes != int64(len(data)) {
		t.Fatalf("artifact = %#v", artifact)
	}
	hexDigest, _ := DigestHex(artifact.Digest)
	if _, err := os.Stat(filepath.Join(root, "metadata", hexDigest+".json")); err != nil {
		t.Fatalf("metadata was not visible after commit: %v", err)
	}

	downloadRequest := uploadRequest
	downloadRequest.Operation = OperationDownload
	downloadRequest.OperationID = "download-operation"
	downloadAuthorization := issueTestAuthorization(t, secret, downloadRequest, now)
	download, err := service.OpenDownload(context.Background(), downloadAuthorization.Capability, present(downloadRequest, downloadAuthorization))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(download)
	if err != nil {
		t.Fatal(err)
	}
	if err := download.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("download = %q, want %q", got, data)
	}

	if _, err := service.Upload(context.Background(), uploadAuthorization.Capability, present(uploadRequest, uploadAuthorization), bytes.NewReader(data)); !errors.Is(err, ErrReplay) {
		t.Fatalf("replayed upload error = %v, want ErrReplay", err)
	}
	restarted := newTestService(t, root, secret, 1<<20, now)
	if _, err := restarted.OpenDownload(context.Background(), downloadAuthorization.Capability, present(downloadRequest, downloadAuthorization)); !errors.Is(err, ErrReplay) {
		t.Fatalf("replayed download after restart error = %v, want ErrReplay", err)
	}
}

func TestServiceRejectsPartialMismatchOversizeAndDisconnectWithoutVisibility(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		maxBytes  int64
		request   func() OperationRequest
		body      func() io.Reader
		wantError error
	}{
		{
			name: "partial upload", maxBytes: 1024,
			request: func() OperationRequest {
				request := testOperationRequest(OperationUpload, []byte("0123456789"), "partial-operation")
				return request
			},
			body: func() io.Reader { return strings.NewReader("short") }, wantError: ErrPartialUpload,
		},
		{
			name: "digest mismatch", maxBytes: 1024,
			request: func() OperationRequest {
				return testOperationRequest(OperationUpload, []byte("expected"), "mismatch-operation")
			},
			body: func() io.Reader { return strings.NewReader("mismatch") }, wantError: ErrDigestMismatch,
		},
		{
			name: "oversized", maxBytes: 4,
			request: func() OperationRequest {
				return testOperationRequest(OperationUpload, []byte("12345"), "oversize-operation")
			},
			body: func() io.Reader { return strings.NewReader("12345") }, wantError: ErrTooLarge,
		},
		{
			name: "disconnect", maxBytes: 1024,
			request: func() OperationRequest {
				return testOperationRequest(OperationUpload, []byte("0123456789"), "disconnect-operation")
			},
			body: func() io.Reader { return &disconnectReader{data: []byte("0123")} }, wantError: ErrPartialUpload,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			secret := []byte(strings.Repeat("z", MinSecretBytes))
			now := time.Now().UTC()
			service := newTestService(t, root, secret, test.maxBytes, now)
			request := test.request()
			authorization := issueTestAuthorization(t, secret, request, now)
			_, err := service.Upload(context.Background(), authorization.Capability, present(request, authorization), test.body())
			if !errors.Is(err, test.wantError) {
				t.Fatalf("Upload() error = %v, want %v", err, test.wantError)
			}
			hexDigest, _ := DigestHex(request.ObjectDigest)
			if _, statErr := os.Stat(filepath.Join(root, "metadata", hexDigest+".json")); !os.IsNotExist(statErr) {
				t.Fatalf("metadata became visible after failed upload: %v", statErr)
			}
			if _, replayErr := service.Upload(context.Background(), authorization.Capability, present(request, authorization), test.body()); !errors.Is(replayErr, ErrReplay) {
				t.Fatalf("failed operation replay error = %v, want ErrReplay", replayErr)
			}
		})
	}
}

func TestServiceRejectsOperationIDDifferentDigest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	secret := []byte(strings.Repeat("c", MinSecretBytes))
	now := time.Now().UTC()
	service := newTestService(t, root, secret, 1024, now)
	firstData := []byte("first")
	first := testOperationRequest(OperationUpload, firstData, "shared-operation")
	firstAuthorization := issueTestAuthorization(t, secret, first, now)
	if _, err := service.Upload(context.Background(), firstAuthorization.Capability, present(first, firstAuthorization), bytes.NewReader(firstData)); err != nil {
		t.Fatal(err)
	}
	secondData := []byte("second")
	second := testOperationRequest(OperationUpload, secondData, "shared-operation")
	secondAuthorization := issueTestAuthorization(t, secret, second, now)
	if _, err := service.Upload(context.Background(), secondAuthorization.Capability, present(second, secondAuthorization), bytes.NewReader(secondData)); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("different digest with same operation error = %v, want ErrOperationConflict", err)
	}
}

func TestServiceRejectsTraversalAndManagedDirectorySymlink(t *testing.T) {
	t.Parallel()
	secret := []byte(strings.Repeat("p", MinSecretBytes))
	now := time.Now().UTC()
	data := []byte("artifact")

	t.Run("traversal", func(t *testing.T) {
		service := newTestService(t, t.TempDir(), secret, 1024, now)
		request := testOperationRequest(OperationUpload, data, "traversal-operation")
		authorization := issueTestAuthorization(t, secret, request, now)
		presented := present(request, authorization)
		presented.Path = "/internal/v2/acp/artifacts/sha256/../escape"
		_, err := service.Upload(context.Background(), authorization.Capability, presented, bytes.NewReader(data))
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("traversal error = %v, want ErrUnauthorized", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		service := newTestService(t, root, secret, 1024, now)
		if err := os.Remove(filepath.Join(root, "objects")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "objects")); err != nil {
			t.Fatal(err)
		}
		request := testOperationRequest(OperationUpload, data, "symlink-operation")
		authorization := issueTestAuthorization(t, secret, request, now)
		_, err := service.Upload(context.Background(), authorization.Capability, present(request, authorization), bytes.NewReader(data))
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("symlink error = %v, want ErrUnsafePath", err)
		}
		entries, err := os.ReadDir(outside)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("upload followed managed directory symlink: %v", entries)
		}
	})
}

func TestServiceConcurrentOneShotOperation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	secret := []byte(strings.Repeat("q", MinSecretBytes))
	now := time.Now().UTC()
	service := newTestService(t, root, secret, 1024, now)
	data := []byte("concurrent")
	request := testOperationRequest(OperationUpload, data, "concurrent-operation")
	authorization := issueTestAuthorization(t, secret, request, now)
	var successes atomic.Int32
	var unexpected atomic.Int32
	var wait sync.WaitGroup
	for range 16 {
		wait.Go(func() {
			_, err := service.Upload(context.Background(), authorization.Capability, present(request, authorization), bytes.NewReader(data))
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, ErrReplay):
			default:
				unexpected.Add(1)
			}
		})
	}
	wait.Wait()
	if successes.Load() != 1 || unexpected.Load() != 0 {
		t.Fatalf("successes=%d unexpected=%d, want 1/0", successes.Load(), unexpected.Load())
	}
}

func TestServiceDetectsCorruptDownload(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	secret := []byte(strings.Repeat("r", MinSecretBytes))
	now := time.Now().UTC()
	service := newTestService(t, root, secret, 1024, now)
	data := []byte("original")
	upload := testOperationRequest(OperationUpload, data, "corrupt-upload")
	uploadAuthorization := issueTestAuthorization(t, secret, upload, now)
	if _, err := service.Upload(context.Background(), uploadAuthorization.Capability, present(upload, uploadAuthorization), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	hexDigest, _ := DigestHex(upload.ObjectDigest)
	if err := os.WriteFile(filepath.Join(root, "objects", hexDigest), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	download := upload
	download.Operation = OperationDownload
	download.OperationID = "corrupt-download"
	downloadAuthorization := issueTestAuthorization(t, secret, download, now)
	if _, err := service.OpenDownload(context.Background(), downloadAuthorization.Capability, present(download, downloadAuthorization)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("OpenDownload() error = %v, want ErrCorrupt", err)
	}
}

func TestServiceErrorsRedactCapabilityAndSecret(t *testing.T) {
	t.Parallel()
	secretText := strings.Repeat("redaction-secret-", 2)
	secret := []byte(secretText)
	now := time.Now().UTC()
	service := newTestService(t, t.TempDir(), secret, 1024, now)
	request := testOperationRequest(OperationUpload, []byte("artifact"), "redaction-operation")
	authorization := issueTestAuthorization(t, secret, request, now)
	presented := present(request, authorization)
	presented.RequestDigest = DigestBytes([]byte("wrong"))
	_, err := service.Upload(context.Background(), authorization.Capability, presented, strings.NewReader("artifact"))
	if err == nil {
		t.Fatal("expected authorization error")
	}
	if strings.Contains(err.Error(), authorization.Capability) || strings.Contains(err.Error(), secretText) {
		t.Fatalf("error disclosed capability or secret: %q", err)
	}
}

func newTestService(t *testing.T, root string, secret []byte, maxBytes int64, now time.Time) *Service {
	t.Helper()
	service, err := NewService(ServiceConfig{Root: root, Secret: secret, MaxObjectBytes: maxBytes, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func issueTestAuthorization(t *testing.T, secret []byte, request OperationRequest, now time.Time) Authorization {
	t.Helper()
	authorization, err := Issue(secret, request, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return authorization
}

type disconnectReader struct {
	data []byte
	done bool
}

func (r *disconnectReader) Read(buffer []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(buffer, r.data), nil
	}
	return 0, fmt.Errorf("simulated disconnect")
}
