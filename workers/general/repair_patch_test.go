package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/artifactcap"
	"github.com/orka-agents/orka/internal/workspacedelta"
)

func patchArgs(serverURL, capabilityFile, checkout, digest, deny string) []string {
	return []string{serverURL, capabilityFile, checkout, digest, ".", deny}
}

func buildRepairPatchFixture(t *testing.T) (artifact []byte, digest, checkout string) {
	t.Helper()
	scratch := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratch, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := workspacedelta.Capture(scratch, workspacedelta.Options{})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "main.go"), []byte("package main // fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	delta, err := workspacedelta.BuildWithLimits(
		snapshot, scratch, workspacedelta.IntentWrite, workspacedelta.BuildLimits{})
	if err != nil {
		t.Fatalf("BuildWithLimits: %v", err)
	}
	checkout = t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return delta.Artifact, delta.ArtifactDigest, checkout
}

func writeRepairPatchCapability(t *testing.T, path, token, digest string, length int64, operation string) {
	t.Helper()
	document := repairPatchCapability{
		Capability:    token,
		RequestDigest: "sha256:" + strings.Repeat("ab", 32),
		Request: repairPatchRequest{
			Operation: operation, ObjectDigest: digest, Namespace: "default", TaskID: "uid-1",
			ContentLength: length, MediaType: artifactcap.MediaTypeWorkspaceDelta, OperationID: "repair-validation-download",
		},
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func repairPatchServer(
	t *testing.T, digest string, artifact []byte, accept func(token string, attempt int32) int,
) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var attempts atomic.Int32
	objectPath, err := artifactcap.ObjectPath(digest)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := attempts.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != objectPath ||
			r.Header.Get(artifactcap.ContentLengthHeader) != strconv.Itoa(len(artifact)) ||
			r.Header.Get(artifactcap.MediaTypeHeader) != artifactcap.MediaTypeWorkspaceDelta ||
			r.Header.Get(artifactcap.RequestDigestHeader) == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if status := accept(r.Header.Get(artifactcap.CapabilityHeader), attempt); status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write(artifact)
	}))
	t.Cleanup(server.Close)
	return server, &attempts
}

func TestApplyRepositoryRepairPatchAppliesBoundPatch(t *testing.T) {
	artifact, digest, checkout := buildRepairPatchFixture(t)
	capabilityFile := filepath.Join(t.TempDir(), "capability.json")
	writeRepairPatchCapability(t, capabilityFile, "token-1", digest, int64(len(artifact)), "download")
	server, _ := repairPatchServer(t, digest, artifact, func(token string, _ int32) int {
		if token != "token-1" {
			return http.StatusForbidden
		}
		return http.StatusOK
	})
	args := patchArgs(server.URL, capabilityFile, checkout, digest, "true")
	if err := applyRepositoryRepairPatch(context.Background(), args); err != nil {
		t.Fatalf("applyRepositoryRepairPatch: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(checkout, "main.go"))
	if err != nil || string(data) != "package main // fixed\n" {
		t.Fatalf("patched file = %q, %v", data, err)
	}
}

func TestApplyRepositoryRepairPatchRereadsCapabilityAfterForbidden(t *testing.T) {
	previousInterval, previousWindow := repairPatchRetryInterval, repairPatchRetryWindow
	repairPatchRetryInterval, repairPatchRetryWindow = 10*time.Millisecond, 5*time.Second
	t.Cleanup(func() { repairPatchRetryInterval, repairPatchRetryWindow = previousInterval, previousWindow })

	artifact, digest, checkout := buildRepairPatchFixture(t)
	capabilityFile := filepath.Join(t.TempDir(), "capability.json")
	writeRepairPatchCapability(t, capabilityFile, "stale", digest, int64(len(artifact)), "download")
	server, attempts := repairPatchServer(t, digest, artifact, func(token string, attempt int32) int {
		if token != "fresh" {
			if attempt == 1 {
				// The controller refreshes the mounted capability.
				writeRepairPatchCapability(t, capabilityFile, "fresh", digest, int64(len(artifact)), "download")
			}
			return http.StatusForbidden
		}
		return http.StatusOK
	})
	args := patchArgs(server.URL, capabilityFile, checkout, digest, "false")
	if err := applyRepositoryRepairPatch(context.Background(), args); err != nil {
		t.Fatalf("applyRepositoryRepairPatch: %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("download attempts = %d, want 2", attempts.Load())
	}
}

func TestApplyRepositoryRepairPatchRejectsDigestMismatchWithoutApplying(t *testing.T) {
	artifact, digest, checkout := buildRepairPatchFixture(t)
	capabilityFile := filepath.Join(t.TempDir(), "capability.json")
	writeRepairPatchCapability(t, capabilityFile, "token", digest, int64(len(artifact)), "download")
	tampered := append([]byte(nil), artifact...)
	tampered[len(tampered)-1] ^= 0xff
	server, _ := repairPatchServer(t, digest, tampered, func(string, int32) int { return http.StatusOK })
	args := patchArgs(server.URL, capabilityFile, checkout, digest, "false")
	err := applyRepositoryRepairPatch(context.Background(), args)
	if err == nil || errors.Is(err, errValidationCommandUnavailable) {
		t.Fatalf("digest mismatch error = %v, want a non-unavailable failure", err)
	}
	if generalWorkerExitCode(err) != 1 {
		t.Fatalf("digest mismatch exit code = %d, want 1", generalWorkerExitCode(err))
	}
	data, _ := os.ReadFile(filepath.Join(checkout, "main.go"))
	if string(data) != "package main\n" {
		t.Fatalf("checkout changed after digest mismatch: %q", data)
	}
}

func TestApplyRepositoryRepairPatchRejectsUploadCapability(t *testing.T) {
	artifact, digest, checkout := buildRepairPatchFixture(t)
	capabilityFile := filepath.Join(t.TempDir(), "capability.json")
	writeRepairPatchCapability(t, capabilityFile, "token", digest, int64(len(artifact)), "upload")
	server, attempts := repairPatchServer(t, digest, artifact, func(string, int32) int { return http.StatusOK })
	args := patchArgs(server.URL, capabilityFile, checkout, digest, "false")
	err := applyRepositoryRepairPatch(context.Background(), args)
	if !errors.Is(err, errValidationCommandUnavailable) {
		t.Fatalf("upload capability error = %v, want unavailable", err)
	}
	if attempts.Load() != 0 {
		t.Fatalf("an upload capability reached the artifact API")
	}
}

func TestApplyRepositoryRepairPatchTreatsOtherStatusesAsFinal(t *testing.T) {
	artifact, digest, checkout := buildRepairPatchFixture(t)
	capabilityFile := filepath.Join(t.TempDir(), "capability.json")
	writeRepairPatchCapability(t, capabilityFile, "token", digest, int64(len(artifact)), "download")
	server, attempts := repairPatchServer(t, digest, artifact, func(string, int32) int { return http.StatusConflict })
	args := patchArgs(server.URL, capabilityFile, checkout, digest, "false")
	err := applyRepositoryRepairPatch(context.Background(), args)
	if !errors.Is(err, errValidationCommandUnavailable) || generalWorkerExitCode(err) != 125 {
		t.Fatalf("replayed download error = %v (exit %d), want unavailable", err, generalWorkerExitCode(err))
	}
	if attempts.Load() != 1 {
		t.Fatalf("download attempts = %d, want 1", attempts.Load())
	}
}
