/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package cliwrapper

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/workerenv"
)

func TestUploadTurnArtifactsCancelsRetryBackoff(t *testing.T) {
	artifactDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(artifactDir, "evidence.txt"), []byte("evidence"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	firstAttempt := make(chan struct{})
	var attempts atomic.Int32
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			close(firstAttempt)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer controller.Close()
	t.Setenv(workerenv.ControllerURL, controller.URL)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- UploadTurnArtifacts(ctx, TurnContext{
			Namespace: "test-ns",
			TaskName:  "test-task",
		}, artifactDir)
	}()

	select {
	case <-firstAttempt:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first artifact upload attempt")
	}
	// The first retry delay is two seconds. Give the response time to reach the
	// uploader, then verify cancellation interrupts the delay rather than waiting
	// for another attempt.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("UploadTurnArtifacts() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("UploadTurnArtifacts() did not stop after cancellation")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("artifact upload attempts = %d, want 1 before cancellation", got)
	}
}
