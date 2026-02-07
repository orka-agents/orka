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

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNewWebhookNotifier(t *testing.T) {
	notifier := NewWebhookNotifier()
	if notifier == nil {
		t.Fatal("NewWebhookNotifier returned nil")
	}
	if notifier.client == nil {
		t.Error("client is nil")
	}
	if notifier.timeout != 30*time.Second {
		t.Errorf("timeout = %v, want 30s", notifier.timeout)
	}
}

func TestWebhookNotifier_Notify_NoURL(t *testing.T) {
	notifier := NewWebhookNotifier()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			WebhookURL: "", // No URL
		},
	}

	err := notifier.Notify(context.Background(), task)
	if err != nil {
		t.Errorf("Notify() error = %v, want nil for empty URL", err)
	}
}

func TestWebhookNotifier_Notify_Success(t *testing.T) {
	var receivedPayload WebhookPayload
	var receivedHeaders http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header
		json.NewDecoder(r.Body).Decode(&receivedPayload)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	notifier := &WebhookNotifier{
		client:  server.Client(),
		timeout: 30 * time.Second,
	}

	startTime := metav1.Now()
	completionTime := metav1.Now()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			WebhookURL: server.URL,
		},
		Status: corev1alpha1.TaskStatus{
			Phase:          corev1alpha1.TaskPhaseSucceeded,
			Message:        "Task completed successfully",
			Attempts:       1,
			StartTime:      &startTime,
			CompletionTime: &completionTime,
			ResultRef: &corev1alpha1.ResultReference{
				Available: true,
			},
		},
	}

	err := notifier.Notify(context.Background(), task)
	if err != nil {
		t.Fatalf("Notify() error = %v", err)
	}

	// Verify payload
	if receivedPayload.TaskName != "test-task" {
		t.Errorf("TaskName = %s, want test-task", receivedPayload.TaskName)
	}
	if receivedPayload.TaskNamespace != "default" {
		t.Errorf("TaskNamespace = %s, want default", receivedPayload.TaskNamespace)
	}
	if receivedPayload.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Errorf("Phase = %s, want Succeeded", receivedPayload.Phase)
	}
	if receivedPayload.Message != "Task completed successfully" {
		t.Errorf("Message = %s, want 'Task completed successfully'", receivedPayload.Message)
	}
	if receivedPayload.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", receivedPayload.Attempts)
	}
	if receivedPayload.StartTime == nil {
		t.Error("StartTime should not be nil")
	}
	if receivedPayload.CompletionTime == nil {
		t.Error("CompletionTime should not be nil")
	}
	if receivedPayload.ResultRef == nil {
		t.Fatal("ResultRef should not be nil")
	}
	if !receivedPayload.ResultRef.Available {
		t.Error("ResultRef.Available should be true")
	}

	// Verify headers
	if receivedHeaders.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %s, want application/json", receivedHeaders.Get("Content-Type"))
	}
	if receivedHeaders.Get("User-Agent") != "Mercan-Controller/1.0" {
		t.Errorf("User-Agent = %s, want Mercan-Controller/1.0", receivedHeaders.Get("User-Agent"))
	}
	if receivedHeaders.Get("X-Mercan-Task") != "test-task" {
		t.Errorf("X-Mercan-Task = %s, want test-task", receivedHeaders.Get("X-Mercan-Task"))
	}
	if receivedHeaders.Get("X-Mercan-Namespace") != "default" {
		t.Errorf("X-Mercan-Namespace = %s, want default", receivedHeaders.Get("X-Mercan-Namespace"))
	}
}

func TestWebhookNotifier_Notify_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	notifier := &WebhookNotifier{
		client:  server.Client(),
		timeout: 30 * time.Second,
	}

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			WebhookURL: server.URL,
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseFailed,
		},
	}

	err := notifier.Notify(context.Background(), task)
	if err == nil {
		t.Error("Notify() expected error for non-2xx response")
	}
}

func TestWebhookNotifier_Notify_Non2xxStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"301 redirect", http.StatusMovedPermanently},
		{"400 bad request", http.StatusBadRequest},
		{"404 not found", http.StatusNotFound},
		{"500 internal error", http.StatusInternalServerError},
		{"503 unavailable", http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer server.Close()

			notifier := &WebhookNotifier{
				client:  server.Client(),
				timeout: 30 * time.Second,
			}

			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					WebhookURL: server.URL,
				},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhaseSucceeded,
				},
			}

			err := notifier.Notify(context.Background(), task)
			if err == nil {
				t.Errorf("Notify() expected error for status %d", tt.status)
			}
		})
	}
}

func TestWebhookNotifier_Notify_2xxStatuses(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"200 OK", http.StatusOK},
		{"201 Created", http.StatusCreated},
		{"202 Accepted", http.StatusAccepted},
		{"204 No Content", http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer server.Close()

			notifier := &WebhookNotifier{
				client:  server.Client(),
				timeout: 30 * time.Second,
			}

			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					WebhookURL: server.URL,
				},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhaseSucceeded,
				},
			}

			err := notifier.Notify(context.Background(), task)
			if err != nil {
				t.Errorf("Notify() error = %v, want nil for status %d", err, tt.status)
			}
		})
	}
}

func TestWebhookNotifier_Notify_ConnectionError(t *testing.T) {
	notifier := NewWebhookNotifier()

	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			WebhookURL: "http://localhost:99999", // Invalid port
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
		},
	}

	err := notifier.Notify(context.Background(), task)
	if err == nil {
		t.Error("Notify() expected error for connection failure")
	}
}

func TestWebhookNotifier_Notify_InvalidURL(t *testing.T) {
	notifier := NewWebhookNotifier()

	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			WebhookURL: "://invalid-url",
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
		},
	}

	err := notifier.Notify(context.Background(), task)
	if err == nil {
		t.Error("Notify() expected error for invalid URL")
	}
}

func TestWebhookNotifier_Notify_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	notifier := &WebhookNotifier{
		client:  server.Client(),
		timeout: 30 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			WebhookURL: server.URL,
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
		},
	}

	err := notifier.Notify(ctx, task)
	if err == nil {
		t.Error("Notify() expected error for cancelled context")
	}
}

func TestWebhookNotifier_Notify_NoResultRef(t *testing.T) {
	var receivedPayload WebhookPayload

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedPayload)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	notifier := &WebhookNotifier{
		client:  server.Client(),
		timeout: 30 * time.Second,
	}

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			WebhookURL: server.URL,
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: nil, // No result ref
		},
	}

	err := notifier.Notify(context.Background(), task)
	if err != nil {
		t.Fatalf("Notify() error = %v", err)
	}

	if receivedPayload.ResultRef != nil {
		t.Error("ResultRef should be nil when task has no result ref")
	}
}

func TestWebhookNotifier_Notify_NoStartOrCompletionTime(t *testing.T) {
	var receivedPayload WebhookPayload

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedPayload)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	notifier := &WebhookNotifier{
		client:  server.Client(),
		timeout: 30 * time.Second,
	}

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			WebhookURL: server.URL,
		},
		Status: corev1alpha1.TaskStatus{
			Phase:          corev1alpha1.TaskPhasePending,
			StartTime:      nil,
			CompletionTime: nil,
		},
	}

	err := notifier.Notify(context.Background(), task)
	if err != nil {
		t.Fatalf("Notify() error = %v", err)
	}

	if receivedPayload.StartTime != nil {
		t.Error("StartTime should be nil")
	}
	if receivedPayload.CompletionTime != nil {
		t.Error("CompletionTime should be nil")
	}
}

func TestWebhookPayload_Structure(t *testing.T) {
	payload := WebhookPayload{
		TaskName:      "test",
		TaskNamespace: "default",
		Phase:         corev1alpha1.TaskPhaseSucceeded,
		Message:       "done",
		Attempts:      1,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var decoded WebhookPayload
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if decoded.TaskName != payload.TaskName {
		t.Errorf("TaskName = %s, want %s", decoded.TaskName, payload.TaskName)
	}
}

func TestResultRefPayload_Structure(t *testing.T) {
	payload := ResultRefPayload{
		Available: true,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var decoded ResultRefPayload
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if decoded.Available != payload.Available {
		t.Errorf("Available = %v, want %v", decoded.Available, payload.Available)
	}
}
