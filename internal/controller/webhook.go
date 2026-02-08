/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

// WebhookPayload is the payload sent to webhook URLs
type WebhookPayload struct {
	TaskName       string                 `json:"taskName"`
	TaskNamespace  string                 `json:"taskNamespace"`
	Phase          corev1alpha1.TaskPhase `json:"phase"`
	Message        string                 `json:"message,omitempty"`
	StartTime      *string                `json:"startTime,omitempty"`
	CompletionTime *string                `json:"completionTime,omitempty"`
	Attempts       int32                  `json:"attempts"`
	ResultRef      *ResultRefPayload      `json:"resultRef,omitempty"`
	RuntimeType    string                 `json:"runtimeType,omitempty"`
}

// ResultRefPayload is the result reference in the webhook payload
type ResultRefPayload struct {
	Available bool `json:"available"`
}

// WebhookNotifier sends webhook notifications for task completion
type WebhookNotifier struct {
	client  *http.Client
	timeout time.Duration
}

// NewWebhookNotifier creates a new WebhookNotifier
func NewWebhookNotifier() *WebhookNotifier {
	return &WebhookNotifier{
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		timeout: 30 * time.Second,
	}
}

// Notify sends a webhook notification for a completed task
func (w *WebhookNotifier) Notify(ctx context.Context, task *corev1alpha1.Task) error {
	if task.Spec.WebhookURL == "" {
		return nil
	}

	payload := WebhookPayload{
		TaskName:      task.Name,
		TaskNamespace: task.Namespace,
		Phase:         task.Status.Phase,
		Message:       task.Status.Message,
		Attempts:      task.Status.Attempts,
	}

	// Set RuntimeType for agent tasks
	if task.Spec.Type == corev1alpha1.TaskTypeAgent {
		payload.RuntimeType = string(task.Spec.Type)
	}

	if task.Status.StartTime != nil {
		startTime := task.Status.StartTime.Format(time.RFC3339)
		payload.StartTime = &startTime
	}

	if task.Status.CompletionTime != nil {
		completionTime := task.Status.CompletionTime.Format(time.RFC3339)
		payload.CompletionTime = &completionTime
	}

	if task.Status.ResultRef != nil {
		payload.ResultRef = &ResultRefPayload{
			Available: task.Status.ResultRef.Available,
		}
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, task.Spec.WebhookURL, bytes.NewReader(jsonPayload))
	if err != nil {
		return fmt.Errorf("failed to create webhook request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mercan-Controller/1.0")
	req.Header.Set("X-Mercan-Task", task.Name)
	req.Header.Set("X-Mercan-Namespace", task.Namespace)

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned non-2xx status: %d", resp.StatusCode)
	}

	return nil
}
