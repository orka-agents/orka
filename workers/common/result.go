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
	resultMaxRetries           = 9
	maxBackoff                 = 60 * time.Second
	deliveryErrorBodyLimit     = 4 << 10
	deliveryResponseDrainLimit = 64 << 10
	saTokenPath                = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saNamespacePath            = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	MaxStructuredSummaryChars = 32 * 1024
)

var retryWait retryWaitFunc = waitForRetry

func SubmitResultContext(ctx context.Context, result []byte) error {
	if len(bytes.TrimSpace(result)) == 0 {
		return fmt.Errorf("result must not be blank")
	}
	ctx = contextOrBackground(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}

	if workerenv.IsTrue(os.Getenv(workerenv.ResultStdout)) {
		fmt.Println(workerenv.ResultStdoutPrefix + base64.StdEncoding.EncodeToString(result))
		return nil
	}

	endpoint, err := resultEndpoint()
	if err != nil {
		return err
	}

	return doPostWithRetry(
		ctx, "result submission", endpoint, result, workerServiceAccountToken(),
		"application/octet-stream", 30*time.Second, retryWait, resultMaxRetries, nil,
	)
}

type retryWaitFunc func(context.Context, time.Duration) error

//nolint:unparam // Results and artifacts supply independent retry budgets, which currently have the same limit.
func doPostWithRetry(
	ctx context.Context,
	operation, endpoint string,
	data []byte,
	saToken, contentType string,
	timeout time.Duration,
	wait retryWaitFunc,
	maxAttempts int,
	authorize func(*http.Request, []byte) error,
) error {
	ctx = contextOrBackground(ctx)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s canceled: %w", operation, err)
	}
	if wait == nil {
		wait = waitForRetry
	}

	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			backoff := min(time.Duration(1<<uint(attempt))*time.Second, maxBackoff)
			if err := wait(ctx, backoff); err != nil {
				return fmt.Errorf("%s canceled: %w", operation, err)
			}
		}

		client := &http.Client{Timeout: timeout}
		if authorize != nil {
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		}
		lastErr = doPostOnce(ctx, client, endpoint, data, saToken, contentType, authorize)
		if lastErr == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s canceled: %w", operation, err)
		}
		if !isRetryableDeliveryError(lastErr) {
			return fmt.Errorf("%s rejected permanently: %w", operation, lastErr)
		}
		fmt.Fprintf(os.Stderr, "%s attempt %d/%d failed: %v\n", operation, attempt+1, maxAttempts, lastErr)
	}

	return fmt.Errorf("all %d %s attempts failed: %w", maxAttempts, operation, lastErr)
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
	if ep := os.Getenv(workerenv.ResultEndpoint); ep != "" {
		return ep, nil
	}

	controllerURL := os.Getenv(workerenv.ControllerURL)
	if controllerURL == "" {
		return "", fmt.Errorf("%s or %s must be set", workerenv.ResultEndpoint, workerenv.ControllerURL)
	}

	namespace := os.Getenv(workerenv.TaskNamespace)
	if namespace == "" {
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

func doPostOnce(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	data []byte,
	saToken, contentType string,
	authorize func(*http.Request, []byte) error,
) error {
	ctx = contextOrBackground(ctx)
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return permanentDeliveryError(fmt.Errorf("failed to create request: %w", err))
	}
	req.Header.Set("Content-Type", contentType)
	if saToken != "" {
		req.Header.Set("Authorization", "Bearer "+saToken)
	}

	if authorize != nil {
		if err := authorize(req, data); err != nil {
			return permanentDeliveryError(fmt.Errorf("artifact request authorization failed: %w", err))
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return retryableDeliveryError(fmt.Errorf("HTTP request failed: %w", err))
	}
	defer resp.Body.Close() //nolint:errcheck

	bodyPreview := readAndDrainDeliveryResponse(resp.Body)
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	statusErr := fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyPreview)))
	if isRetryableHTTPStatus(resp.StatusCode) {
		return retryableDeliveryError(statusErr)
	}
	return permanentDeliveryError(statusErr)
}

func readAndDrainDeliveryResponse(body io.Reader) []byte {
	if body == nil {
		return nil
	}
	preview, _ := io.ReadAll(io.LimitReader(body, deliveryErrorBodyLimit))
	remaining := int64(deliveryResponseDrainLimit - len(preview))
	if remaining > 0 {
		_, _ = io.CopyN(io.Discard, body, remaining)
	}
	return preview
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
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
	return statusCode == http.StatusRequestTimeout || statusCode == http.StatusTooManyRequests ||
		(statusCode >= http.StatusInternalServerError && statusCode != http.StatusNotImplemented)
}

type StructuredResult struct {
	Version    int            `json:"version"`
	Summary    string         `json:"summary"`
	BaseSHA    string         `json:"baseSHA,omitempty"`
	HeadSHA    string         `json:"headSHA,omitempty"`
	Diff       string         `json:"diff,omitempty"`
	Verdict    string         `json:"verdict,omitempty"`
	Feedback   string         `json:"feedback,omitempty"`
	Files      []string       `json:"files,omitempty"`
	PushBranch string         `json:"pushBranch,omitempty"`
	PushError  string         `json:"pushError,omitempty"`
	Data       map[string]any `json:"data,omitempty"`
	Artifacts  []ArtifactRef  `json:"artifacts,omitempty"`
}

type ArtifactRef struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType,omitempty"`
	Size        int64  `json:"size,omitempty"`
	Description string `json:"description,omitempty"`
}

func FormatStructuredResult(r *StructuredResult) ([]byte, error) {
	if r.Version == 0 {
		r.Version = 1
	}
	return json.Marshal(r)
}

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

func TruncateStructuredSummary(summary string) string {
	return TruncateSummary(summary, MaxStructuredSummaryChars)
}

func TruncateSummary(summary string, limit int) string {
	if len(summary) <= limit {
		return summary
	}
	return summary[:limit] + fmt.Sprintf(
		"\n[summary truncated, full summary: %d chars]",
		len(summary),
	)
}
