/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	artifactsDir              = "/tmp/artifacts/"
	workspaceArtifactsDirName = ".orka-artifacts"
	maxTotalSize              = 50 << 20 // 50 MB
	maxFileSize               = 10 << 20 // 10 MB
	artifactPath              = "internal/v1/artifacts"
)

// EnsureWorkspaceArtifactsLink exposes /tmp/artifacts inside the repo root so
// runtime agents can write artifacts using a workspace-relative path.
func EnsureWorkspaceArtifactsLink(workspaceDir string) error {
	if workspaceDir == "" {
		return nil
	}
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		return fmt.Errorf("failed to create artifacts directory: %w", err)
	}
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return fmt.Errorf("failed to create workspace directory: %w", err)
	}

	linkPath := filepath.Join(workspaceDir, workspaceArtifactsDirName)
	info, err := os.Lstat(linkPath)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(linkPath)
			if readErr == nil {
				resolved := target
				if !filepath.IsAbs(resolved) {
					resolved = filepath.Join(filepath.Dir(linkPath), resolved)
				}
				if filepath.Clean(resolved) == filepath.Clean(artifactsDir) {
					return nil
				}
			}
		}
		return fmt.Errorf("workspace artifact path %s already exists", linkPath)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("failed to inspect workspace artifact path: %w", err)
	}

	if err := os.Symlink(artifactsDir, linkPath); err != nil {
		return fmt.Errorf("failed to create workspace artifact symlink: %w", err)
	}
	return nil
}

// MissingArtifacts returns required artifact filenames that do not exist yet
// or are present but empty.
func MissingArtifacts(filenames []string) ([]string, error) {
	missing := make([]string, 0, len(filenames))
	for _, filename := range filenames {
		info, err := os.Stat(filepath.Join(artifactsDir, filename))
		switch {
		case os.IsNotExist(err):
			missing = append(missing, filename)
		case err != nil:
			return nil, fmt.Errorf("failed to stat artifact %s: %w", filename, err)
		case info.IsDir() || info.Size() == 0:
			missing = append(missing, filename)
		}
	}
	return missing, nil
}

// WriteArtifactFile writes an artifact file into the shared upload directory.
func WriteArtifactFile(filename string, data []byte) error {
	filename = filepath.Base(filename)
	if filename == "." || filename == ".." || strings.ContainsAny(filename, "/\\") {
		return fmt.Errorf("invalid artifact filename %q", filename)
	}
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		return fmt.Errorf("failed to create artifacts directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(artifactsDir, filename), data, 0o644); err != nil {
		return fmt.Errorf("failed to write artifact %s: %w", filename, err)
	}
	return nil
}

// UploadArtifacts scans /tmp/artifacts/ and uploads each file to the controller.
// It is called after SubmitResult to persist any files the agent wrote.
// Returns nil if the artifacts directory does not exist or is empty.
func UploadArtifacts() error {
	if _, err := os.Stat(artifactsDir); os.IsNotExist(err) {
		return nil
	}

	entries, err := os.ReadDir(artifactsDir)
	if err != nil {
		return fmt.Errorf("failed to read artifacts directory: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}

	// Compute total size, excluding symlinks and oversized files
	var totalSize int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		filePath := filepath.Join(artifactsDir, e.Name())
		fi, err := os.Lstat(filePath)
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if fi.Size() > maxFileSize {
			continue
		}
		totalSize += fi.Size()
	}
	if totalSize > maxTotalSize {
		return fmt.Errorf("total artifact size %d bytes exceeds limit of %d bytes", totalSize, maxTotalSize)
	}

	baseEndpoint, err := artifactEndpointBase()
	if err != nil {
		return fmt.Errorf("failed to construct artifact endpoint: %w", err)
	}

	token, _ := os.ReadFile(saTokenPath)
	saToken := strings.TrimSpace(string(token))

	var uploadErrors []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		filename := filepath.Base(e.Name())
		// Reject filenames with path traversal or special characters
		if filename == "." || filename == ".." || strings.ContainsAny(filename, "/\\") {
			fmt.Fprintf(os.Stderr, "artifact: skipping invalid filename %q\n", filename)
			continue
		}
		filePath := filepath.Join(artifactsDir, filename)

		// Reject symlinks to prevent exfiltration of sensitive files
		fi, err := os.Lstat(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "artifact: failed to stat %s: %v\n", filename, err)
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			fmt.Fprintf(os.Stderr, "artifact: skipping symlink %s\n", filename)
			continue
		}

		data, err := os.ReadFile(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "artifact: failed to read %s: %v\n", filename, err)
			uploadErrors = append(uploadErrors, fmt.Sprintf("%s: %v", filename, err))
			continue
		}

		if len(data) > maxFileSize {
			fmt.Fprintf(os.Stderr, "artifact: skipping %s (%d bytes exceeds %d byte limit)\n", filename, len(data), maxFileSize)
			continue
		}

		contentType := detectContentType(filename, data)
		endpoint := fmt.Sprintf("%s/%s", baseEndpoint, url.PathEscape(filename))

		if err := doPostWithContentType(endpoint, data, saToken, contentType); err != nil {
			fmt.Fprintf(os.Stderr, "artifact: failed to upload %s: %v\n", filename, err)
			uploadErrors = append(uploadErrors, fmt.Sprintf("%s: %v", filename, err))
		} else {
			fmt.Printf("artifact: uploaded %s (%d bytes, %s)\n", filename, len(data), contentType)
		}
	}

	if len(uploadErrors) > 0 {
		return fmt.Errorf("some artifacts failed to upload: %s", strings.Join(uploadErrors, "; "))
	}
	return nil
}

func artifactEndpointBase() (string, error) {
	controllerURL := os.Getenv("ORKA_CONTROLLER_URL")
	if controllerURL == "" {
		return "", fmt.Errorf("ORKA_CONTROLLER_URL must be set")
	}

	namespace := os.Getenv("ORKA_TASK_NAMESPACE")
	if namespace == "" {
		data, err := os.ReadFile(saNamespacePath)
		if err != nil {
			return "", fmt.Errorf("ORKA_TASK_NAMESPACE not set and cannot read namespace from SA: %w", err)
		}
		namespace = strings.TrimSpace(string(data))
	}

	taskName := os.Getenv("ORKA_TASK_NAME")
	if taskName == "" {
		return "", fmt.Errorf("ORKA_TASK_NAME must be set")
	}

	controllerURL = strings.TrimRight(controllerURL, "/")
	return fmt.Sprintf("%s/%s/%s/%s", controllerURL, artifactPath, namespace, taskName), nil
}

func detectContentType(filename string, data []byte) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".pdf":
		return "application/pdf"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".zip":
		return "application/zip"
	case ".json":
		return "application/json"
	case ".csv":
		return "text/csv"
	case ".html":
		return "text/html"
	case ".md":
		return "text/markdown"
	case ".txt":
		return "text/plain"
	}

	// Check for .tar.gz
	if strings.HasSuffix(strings.ToLower(filename), ".tar.gz") {
		return "application/gzip"
	}

	return http.DetectContentType(data)
}

func doPostWithContentType(endpoint string, data []byte, saToken, contentType string) error {
	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt)) * time.Second
			time.Sleep(backoff)
		}

		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", contentType)
		if saToken != "" {
			req.Header.Set("Authorization", "Bearer "+saToken)
		}

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("HTTP request failed: %w", err)
			fmt.Fprintf(os.Stderr, "artifact upload attempt %d/%d failed: %v\n", attempt+1, maxRetries, lastErr)
			continue
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close() //nolint:errcheck
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}

		lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		fmt.Fprintf(os.Stderr, "artifact upload attempt %d/%d failed: %v\n", attempt+1, maxRetries, lastErr)
	}

	return fmt.Errorf("all %d artifact upload attempts failed: %w", maxRetries, lastErr)
}
