/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sozercan/orka/internal/workerenv"
)

const (
	maxRetries      = 5
	saTokenPath     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	// MaxStructuredSummaryChars bounds agent-written summaries stored in structured
	// results. Diffs remain intact for workspace handoff, but oversized summaries
	// can otherwise blow up coordinator context windows and provider request limits.
	MaxStructuredSummaryChars = 32 * 1024
)

// SubmitResult sends the task result to the controller via HTTP POST.
// It reads ORKA_RESULT_ENDPOINT or constructs the URL from ORKA_CONTROLLER_URL.
// Retries up to 5 times with exponential backoff (2s, 4s, 8s, 16s) on failure.
func SubmitResult(result []byte) error {
	endpoint, err := resultEndpoint()
	if err != nil {
		return err
	}

	saToken := workerServiceAccountToken()

	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt)) * time.Second
			time.Sleep(backoff)
		}

		lastErr = doPost(endpoint, result, saToken)
		if lastErr == nil {
			return nil
		}
		fmt.Fprintf(os.Stderr, "result submission attempt %d/%d failed: %v\n", attempt+1, maxRetries, lastErr)
	}

	return fmt.Errorf("all %d result submission attempts failed: %w", maxRetries, lastErr)
}

func resultEndpoint() (string, error) {
	// Prefer explicit endpoint
	if ep := os.Getenv(workerenv.ResultEndpoint); ep != "" {
		return ep, nil
	}

	// Construct from controller URL + task identity
	controllerURL := os.Getenv(workerenv.ControllerURL)
	if controllerURL == "" {
		return "", fmt.Errorf("%s or %s must be set", workerenv.ResultEndpoint, workerenv.ControllerURL)
	}

	namespace := os.Getenv(workerenv.TaskNamespace)
	if namespace == "" {
		// Fall back to downward API namespace file
		data, err := os.ReadFile(saNamespacePath)
		if err != nil {
			return "", fmt.Errorf("%s not set and cannot read namespace from SA: %w", workerenv.TaskNamespace, err)
		}
		namespace = strings.TrimSpace(string(data))
	}

	taskName := os.Getenv(workerenv.TaskName)
	if taskName == "" {
		return "", fmt.Errorf("%s must be set", workerenv.TaskName)
	}

	controllerURL = strings.TrimRight(controllerURL, "/")
	return fmt.Sprintf("%s/internal/v1/results/%s/%s", controllerURL, namespace, taskName), nil
}

func workerServiceAccountToken() string {
	if path := strings.TrimSpace(os.Getenv(workerenv.ServiceAccountTokenPath)); path != "" {
		if token, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(token))
		}
	}

	if token, err := os.ReadFile(saTokenPath); err == nil {
		return strings.TrimSpace(string(token))
	}

	return strings.TrimSpace(os.Getenv(workerenv.ServiceAccountToken))
}

func doPost(endpoint string, data []byte, saToken string) error {
	return doPostOnceWithContentType(endpoint, data, saToken, "application/octet-stream", 30*time.Second)
}

func doPostOnceWithContentType(endpoint string, data []byte, saToken, contentType string, timeout time.Duration) error {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	if saToken != "" {
		req.Header.Set("Authorization", "Bearer "+saToken)
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
}

// StructuredResult is an optional structured envelope for task results.
// Workers can use this to include diffs, verdicts, and metadata alongside
// the human-readable summary. Plain-text results remain backward compatible.
type StructuredResult struct {
	Version    int      `json:"version"`
	Summary    string   `json:"summary"`
	BaseSHA    string   `json:"baseSHA,omitempty"`
	HeadSHA    string   `json:"headSHA,omitempty"`
	Diff       string   `json:"diff,omitempty"`
	Verdict    string   `json:"verdict,omitempty"`
	Feedback   string   `json:"feedback,omitempty"`
	Files      []string `json:"files,omitempty"`
	PushBranch string   `json:"pushBranch,omitempty"`
	PushError  string   `json:"pushError,omitempty"`
}

// FormatStructuredResult serializes a StructuredResult to JSON bytes.
func FormatStructuredResult(r *StructuredResult) ([]byte, error) {
	if r.Version == 0 {
		r.Version = 1
	}
	return json.Marshal(r)
}

// ParseStructuredResult attempts to parse a result string as a StructuredResult.
// If the input is not valid JSON or doesn't have the expected structure,
// it returns a StructuredResult with the raw input as Summary (backward compatible).
func ParseStructuredResult(raw string) *StructuredResult {
	var sr StructuredResult
	if err := json.Unmarshal([]byte(raw), &sr); err != nil || sr.Version == 0 {
		return &StructuredResult{
			Version: 1,
			Summary: raw,
		}
	}
	return &sr
}

// TruncateStructuredSummary bounds human-readable result summaries while making
// truncation explicit to downstream coordinators.
func TruncateStructuredSummary(summary string) string {
	if len(summary) <= MaxStructuredSummaryChars {
		return summary
	}
	return summary[:MaxStructuredSummaryChars] + fmt.Sprintf(
		"\n[summary truncated, full summary: %d chars]",
		len(summary),
	)
}
