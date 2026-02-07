/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package common

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	maxRetries     = 3
	saTokenPath    = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// SubmitResult sends the task result to the controller via HTTP POST.
// It reads MERCAN_RESULT_ENDPOINT or constructs the URL from MERCAN_CONTROLLER_URL.
// Retries with exponential backoff (1s, 2s, 4s) on failure.
func SubmitResult(result []byte) error {
	endpoint, err := resultEndpoint()
	if err != nil {
		return err
	}

	token, _ := os.ReadFile(saTokenPath)
	saToken := strings.TrimSpace(string(token))

	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
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
	if ep := os.Getenv("MERCAN_RESULT_ENDPOINT"); ep != "" {
		return ep, nil
	}

	// Construct from controller URL + task identity
	controllerURL := os.Getenv("MERCAN_CONTROLLER_URL")
	if controllerURL == "" {
		return "", fmt.Errorf("MERCAN_RESULT_ENDPOINT or MERCAN_CONTROLLER_URL must be set")
	}

	namespace := os.Getenv("MERCAN_TASK_NAMESPACE")
	if namespace == "" {
		// Fall back to downward API namespace file
		data, err := os.ReadFile(saNamespacePath)
		if err != nil {
			return "", fmt.Errorf("MERCAN_TASK_NAMESPACE not set and cannot read namespace from SA: %w", err)
		}
		namespace = strings.TrimSpace(string(data))
	}

	taskName := os.Getenv("MERCAN_TASK_NAME")
	if taskName == "" {
		return "", fmt.Errorf("MERCAN_TASK_NAME must be set")
	}

	controllerURL = strings.TrimRight(controllerURL, "/")
	return fmt.Sprintf("%s/internal/v1/results/%s/%s", controllerURL, namespace, taskName), nil
}

func doPost(endpoint string, data []byte, saToken string) error {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if saToken != "" {
		req.Header.Set("Authorization", "Bearer "+saToken)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
}
