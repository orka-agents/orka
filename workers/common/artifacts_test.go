/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func cleanupArtifactsDir(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(artifactsDir); err != nil {
		t.Fatalf("failed to clean artifacts dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(artifactsDir)
	})
}

func prepareArtifactsDir(t *testing.T) {
	t.Helper()
	cleanupArtifactsDir(t)
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		t.Fatalf("failed to create artifacts dir: %v", err)
	}
}

func writeArtifactFile(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(artifactsDir, name), data, 0o644); err != nil {
		t.Fatalf("failed to write artifact %q: %v", name, err)
	}
}

func createSparseArtifactFile(t *testing.T, name string, size int64) {
	t.Helper()
	f, err := os.Create(filepath.Join(artifactsDir, name))
	if err != nil {
		t.Fatalf("failed to create artifact %q: %v", name, err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		t.Fatalf("failed to size artifact %q: %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("failed to close artifact %q: %v", name, err)
	}
}

func TestArtifactEndpointBase_HappyPath(t *testing.T) {
	cleanupArtifactsDir(t)
	t.Setenv("ORKA_CONTROLLER_URL", "http://controller.example/")
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")
	t.Setenv("ORKA_TASK_NAME", "test-task")

	got, err := artifactEndpointBase()
	if err != nil {
		t.Fatalf("artifactEndpointBase() error = %v", err)
	}

	want := "http://controller.example/internal/v1/artifacts/test-ns/test-task"
	if got != want {
		t.Fatalf("artifactEndpointBase() = %q, want %q", got, want)
	}
}

func TestDetectContentType_KeyExtensions(t *testing.T) {
	cleanupArtifactsDir(t)
	tests := []struct {
		filename string
		want     string
	}{
		{
			filename: "slides.pptx",
			want:     "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		},
		{filename: "doc.pdf", want: "application/pdf"},
		{filename: "notes.txt", want: "text/plain"},
	}

	for _, tt := range tests {
		got := detectContentType(tt.filename, []byte("x"))
		if got != tt.want {
			t.Errorf("detectContentType(%q) = %q, want %q", tt.filename, got, tt.want)
		}
	}
}

func TestUploadArtifacts_UploadsFilesToInternalEndpoint(t *testing.T) {
	prepareArtifactsDir(t)
	writeArtifactFile(t, "deck.pptx", []byte("pptx-bytes"))
	writeArtifactFile(t, "notes.txt", []byte("hello"))

	var mu sync.Mutex
	received := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expectedPrefix := "/" + artifactPath + "/test-ns/test-task/"
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, expectedPrefix) {
			t.Errorf("path = %q, want prefix %q", r.URL.Path, expectedPrefix)
		}
		filename := strings.TrimPrefix(r.URL.Path, expectedPrefix)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read body: %v", err)
		}
		if len(body) == 0 {
			t.Errorf("body for %q is empty", filename)
		}
		mu.Lock()
		received[filename] = r.Header.Get("Content-Type")
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	t.Setenv("ORKA_CONTROLLER_URL", srv.URL)
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")
	t.Setenv("ORKA_TASK_NAME", "test-task")

	if err := UploadArtifacts(); err != nil {
		t.Fatalf("UploadArtifacts() error = %v", err)
	}

	if len(received) != 2 {
		t.Fatalf("uploaded files = %d, want 2", len(received))
	}
	if got := received["deck.pptx"]; got != "application/vnd.openxmlformats-officedocument.presentationml.presentation" {
		t.Errorf("deck.pptx Content-Type = %q", got)
	}
	if got := received["notes.txt"]; got != "text/plain" {
		t.Errorf("notes.txt Content-Type = %q, want text/plain", got)
	}
}

func TestUploadArtifacts_URLEscapesFilename(t *testing.T) {
	prepareArtifactsDir(t)
	writeArtifactFile(t, "my file.txt", []byte("space"))

	var requestURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURI = r.RequestURI
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	t.Setenv("ORKA_CONTROLLER_URL", srv.URL)
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")
	t.Setenv("ORKA_TASK_NAME", "test-task")

	if err := UploadArtifacts(); err != nil {
		t.Fatalf("UploadArtifacts() error = %v", err)
	}
	if !strings.Contains(requestURI, "my%20file.txt") {
		t.Fatalf("request URI = %q, want escaped filename", requestURI)
	}
}

func TestUploadArtifacts_ReturnsErrorWhenTotalSizeExceeded(t *testing.T) {
	prepareArtifactsDir(t)
	// Create multiple files each under maxFileSize but totalling over maxTotalSize
	fileSize := int64(maxFileSize - 1)
	numFiles := (maxTotalSize / fileSize) + 2
	for i := int64(0); i < numFiles; i++ {
		createSparseArtifactFile(t, fmt.Sprintf("file-%d.bin", i), fileSize)
	}

	t.Setenv("ORKA_CONTROLLER_URL", "http://controller.example")
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")
	t.Setenv("ORKA_TASK_NAME", "test-task")

	err := UploadArtifacts()
	if err == nil {
		t.Fatal("expected error when total artifact size exceeds limit")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("error = %q, want size limit error", err.Error())
	}
}

func TestUploadArtifacts_SkipsOversizedIndividualFiles(t *testing.T) {
	prepareArtifactsDir(t)
	createSparseArtifactFile(t, "too-large.bin", maxFileSize+1)
	writeArtifactFile(t, "small.txt", []byte("ok"))

	var mu sync.Mutex
	uploaded := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/" + artifactPath + "/test-ns/test-task/"
		filename := strings.TrimPrefix(r.URL.Path, prefix)
		mu.Lock()
		uploaded[filename] = true
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	t.Setenv("ORKA_CONTROLLER_URL", srv.URL)
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")
	t.Setenv("ORKA_TASK_NAME", "test-task")

	if err := UploadArtifacts(); err != nil {
		t.Fatalf("UploadArtifacts() error = %v", err)
	}

	if len(uploaded) != 1 {
		t.Fatalf("uploaded files = %d, want 1", len(uploaded))
	}
	if !uploaded["small.txt"] {
		t.Fatalf("expected small.txt to be uploaded, got %v", uploaded)
	}
	if uploaded["too-large.bin"] {
		t.Fatalf("too-large.bin should have been skipped")
	}
}
