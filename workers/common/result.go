/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/workerenv"
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

var resultStdoutMarkerPath = agentSandboxResultMarkerExecPath

// SubmitResult sends the task result to the controller via HTTP POST.
// It preserves the legacy background-context behavior for callers that do not
// have a worker lifecycle context.
func SubmitResult(result []byte) error {
	return SubmitResultContext(context.Background(), result)
}

// SubmitResultContext sends the task result to the controller via HTTP POST.
// It reads ORKA_RESULT_ENDPOINT or constructs the URL from ORKA_CONTROLLER_URL.
// Retries up to 5 times with exponential backoff (2s, 4s, 8s, 16s) on failure.
func SubmitResultContext(ctx context.Context, result []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if workerenv.IsTrue(os.Getenv(workerenv.ResultStdout)) {
		marker := workerenv.ResultStdoutPrefix + base64.StdEncoding.EncodeToString(result)
		fileData := marker + "\n"
		if token := strings.TrimSpace(os.Getenv(workerenv.ResultStdoutToken)); token != "" {
			fileData = agentSandboxResultTokenPrefix + token + "\n" + fileData
		}
		if err := os.WriteFile(resultStdoutMarkerPath, []byte(fileData), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to write stdout result marker file: %v\n", err)
		}
		fmt.Println(marker)
		return nil
	}

	endpoint, err := resultEndpoint()
	if err != nil {
		return err
	}

	saToken := workerServiceAccountToken()
	return doPostWithRetryContext(
		ctx, "result submission", endpoint, result, saToken, "application/octet-stream", 30*time.Second,
	)
}

type retryWaitFunc func(context.Context, time.Duration) error

func doPostWithRetryContext(
	ctx context.Context,
	operation string,
	endpoint string,
	data []byte,
	saToken, contentType string,
	timeout time.Duration,
) error {
	return doPostWithRetry(
		ctx, operation, endpoint, data, saToken, contentType, timeout, waitForRetry,
	)
}

func doPostWithRetry(
	ctx context.Context,
	operation string,
	endpoint string,
	data []byte,
	saToken, contentType string,
	timeout time.Duration,
	wait retryWaitFunc,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s canceled: %w", operation, err)
	}

	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt)) * time.Second
			if err := wait(ctx, backoff); err != nil {
				return fmt.Errorf("%s canceled: %w", operation, err)
			}
		}

		lastErr = doPostOnceWithContentTypeContext(ctx, endpoint, data, saToken, contentType, timeout)
		if lastErr == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s canceled: %w", operation, err)
		}
		if !isRetryableDeliveryError(lastErr) {
			return fmt.Errorf("%s failed: %w", operation, lastErr)
		}
		fmt.Fprintf(os.Stderr, "%s attempt %d/%d failed: %v\n", operation, attempt+1, maxRetries, lastErr)
	}

	return fmt.Errorf("all %d %s attempts failed: %w", maxRetries, operation, lastErr)
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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

func doPostOnceWithContentType(endpoint string, data []byte, saToken, contentType string, timeout time.Duration) error {
	return doPostOnceWithContentTypeContext(
		context.Background(), endpoint, data, saToken, contentType, timeout,
	)
}

func doPostOnceWithContentTypeContext(
	ctx context.Context,
	endpoint string,
	data []byte,
	saToken, contentType string,
	timeout time.Duration,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return permanentDeliveryError(fmt.Errorf("failed to create request: %w", err))
	}
	req.Header.Set("Content-Type", contentType)
	if saToken != "" {
		req.Header.Set("Authorization", "Bearer "+saToken)
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return retryableDeliveryError(fmt.Errorf("HTTP request failed: %w", err))
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	statusErr := fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	if isRetryableHTTPStatus(resp.StatusCode) {
		return retryableDeliveryError(statusErr)
	}
	return permanentDeliveryError(statusErr)
}

type deliveryError struct {
	err       error
	retryable bool
}

func (e *deliveryError) Error() string {
	return e.err.Error()
}

func (e *deliveryError) Unwrap() error {
	return e.err
}

func retryableDeliveryError(err error) error {
	return &deliveryError{err: err, retryable: true}
}

func permanentDeliveryError(err error) error {
	return &deliveryError{err: err}
}

func isRetryableDeliveryError(err error) bool {
	var deliveryErr *deliveryError
	return errors.As(err, &deliveryErr) && deliveryErr.retryable
}

func isRetryableHTTPStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
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
	// Data carries generic machine-readable task output. Keep large payloads in
	// artifacts and put references here; parent/coordinator summaries may bound it.
	Data      map[string]any `json:"data,omitempty"`
	Artifacts []ArtifactRef  `json:"artifacts,omitempty"`
}

// ArtifactRef is a safe structured reference to a task artifact. The artifact
// bytes remain in Orka artifact storage; this envelope carries only metadata
// that coordinators and remote runtimes can use to fetch or reason about it.
type ArtifactRef struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType,omitempty"`
	Size        int64  `json:"size,omitempty"`
	Description string `json:"description,omitempty"`
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
