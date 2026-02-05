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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

const (
	// SessionLabelKey is the label for session ConfigMaps
	SessionLabelKey = "mercan.ai/session"

	// ActiveTaskAnnotation is the annotation for tracking active task
	ActiveTaskAnnotation = "mercan.ai/active-task"

	// MessageCountAnnotation tracks number of messages
	MessageCountAnnotation = "mercan.ai/message-count"

	// InputTokensAnnotation tracks input token usage
	InputTokensAnnotation = "mercan.ai/input-tokens"

	// OutputTokensAnnotation tracks output token usage
	OutputTokensAnnotation = "mercan.ai/output-tokens"

	// TranscriptKey is the key for the transcript data
	TranscriptKey = "transcript.jsonl"
)

// SessionMessage represents a message in the session transcript
type SessionMessage struct {
	Role      string                 `json:"role"`
	Content   string                 `json:"content,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Input     map[string]interface{} `json:"input,omitempty"`
	Timestamp time.Time              `json:"ts"`
}

// SessionManager manages session ConfigMaps for conversation continuity
type SessionManager struct {
	client.Client
}

// NewSessionManager creates a new SessionManager
func NewSessionManager(c client.Client) *SessionManager {
	return &SessionManager{Client: c}
}

// getSessionConfigMapName returns the ConfigMap name for a session
func getSessionConfigMapName(sessionName string) string {
	return fmt.Sprintf("session-%s", sessionName)
}

// IsLocked checks if a session is locked by another task
func (m *SessionManager) IsLocked(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if task.Spec.SessionRef == nil {
		return false, nil
	}

	cmName := getSessionConfigMapName(task.Spec.SessionRef.Name)
	cm := &corev1.ConfigMap{}

	err := m.Get(ctx, types.NamespacedName{
		Name:      cmName,
		Namespace: task.Namespace,
	}, cm)

	if err != nil {
		if apierrors.IsNotFound(err) {
			// Session doesn't exist, not locked
			return false, nil
		}
		return false, err
	}

	// Check if there's an active task
	activeTask, exists := cm.Annotations[ActiveTaskAnnotation]
	if !exists || activeTask == "" {
		return false, nil
	}

	// If the active task is this task, it's not locked for us
	if activeTask == task.Name {
		return false, nil
	}

	return true, nil
}

// AcquireLock acquires the session lock for a task
func (m *SessionManager) AcquireLock(ctx context.Context, task *corev1alpha1.Task) error {
	if task.Spec.SessionRef == nil {
		return nil
	}

	cmName := getSessionConfigMapName(task.Spec.SessionRef.Name)
	cm := &corev1.ConfigMap{}

	err := m.Get(ctx, types.NamespacedName{
		Name:      cmName,
		Namespace: task.Namespace,
	}, cm)

	if err != nil {
		if apierrors.IsNotFound(err) {
			// Create the session if configured
			if task.Spec.SessionRef.Create {
				return m.createSession(ctx, task)
			}
			return fmt.Errorf("session %s not found and create=false", task.Spec.SessionRef.Name)
		}
		return err
	}

	// Check if already locked by another task
	if activeTask, exists := cm.Annotations[ActiveTaskAnnotation]; exists && activeTask != "" && activeTask != task.Name {
		return fmt.Errorf("session locked by task %s", activeTask)
	}

	// Set the lock
	if cm.Annotations == nil {
		cm.Annotations = make(map[string]string)
	}
	cm.Annotations[ActiveTaskAnnotation] = task.Name

	return m.Update(ctx, cm)
}

// ReleaseLock releases the session lock for a task
func (m *SessionManager) ReleaseLock(ctx context.Context, task *corev1alpha1.Task) error {
	if task.Spec.SessionRef == nil {
		return nil
	}

	cmName := getSessionConfigMapName(task.Spec.SessionRef.Name)
	cm := &corev1.ConfigMap{}

	err := m.Get(ctx, types.NamespacedName{
		Name:      cmName,
		Namespace: task.Namespace,
	}, cm)

	if err != nil {
		if apierrors.IsNotFound(err) {
			// Session doesn't exist, nothing to release
			return nil
		}
		return err
	}

	// Only release if we hold the lock
	if activeTask, exists := cm.Annotations[ActiveTaskAnnotation]; exists && activeTask == task.Name {
		cm.Annotations[ActiveTaskAnnotation] = ""
		return m.Update(ctx, cm)
	}

	return nil
}

// createSession creates a new session ConfigMap
func (m *SessionManager) createSession(ctx context.Context, task *corev1alpha1.Task) error {
	cmName := getSessionConfigMapName(task.Spec.SessionRef.Name)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: task.Namespace,
			Labels: map[string]string{
				SessionLabelKey: "true",
			},
			Annotations: map[string]string{
				ActiveTaskAnnotation:   task.Name,
				MessageCountAnnotation: "0",
				InputTokensAnnotation:  "0",
				OutputTokensAnnotation: "0",
				"mercan.ai/created-at": time.Now().Format(time.RFC3339),
				"mercan.ai/updated-at": time.Now().Format(time.RFC3339),
			},
		},
		Data: map[string]string{
			TranscriptKey: "",
		},
	}

	return m.Create(ctx, cm)
}

// AppendMessages appends messages from a completed task to the session
func (m *SessionManager) AppendMessages(ctx context.Context, task *corev1alpha1.Task) error {
	if task.Spec.SessionRef == nil || !task.Spec.SessionRef.Append {
		return nil
	}

	cmName := getSessionConfigMapName(task.Spec.SessionRef.Name)
	cm := &corev1.ConfigMap{}

	err := m.Get(ctx, types.NamespacedName{
		Name:      cmName,
		Namespace: task.Namespace,
	}, cm)

	if err != nil {
		return err
	}

	// Get the result from the task's result ConfigMap
	var prompt, response string

	if task.Spec.AI != nil && task.Spec.AI.Prompt != "" {
		prompt = task.Spec.AI.Prompt
	} else if task.Spec.Prompt != "" {
		prompt = task.Spec.Prompt
	}

	// Try to get the response from the result ConfigMap
	if task.Status.ResultRef != nil {
		resultCM := &corev1.ConfigMap{}
		err := m.Get(ctx, types.NamespacedName{
			Name:      task.Status.ResultRef.ConfigMapName,
			Namespace: task.Namespace,
		}, resultCM)
		if err == nil {
			if result, exists := resultCM.Data[task.Status.ResultRef.Key]; exists {
				response = result
			}
		}
	}

	// Append messages
	var newMessages []string
	now := time.Now()

	if prompt != "" {
		userMsg := SessionMessage{
			Role:      "user",
			Content:   prompt,
			Timestamp: now,
		}
		msgJSON, _ := json.Marshal(userMsg)
		newMessages = append(newMessages, string(msgJSON))
	}

	if response != "" {
		assistantMsg := SessionMessage{
			Role:      "assistant",
			Content:   response,
			Timestamp: now,
		}
		msgJSON, _ := json.Marshal(assistantMsg)
		newMessages = append(newMessages, string(msgJSON))
	}

	if len(newMessages) == 0 {
		return nil
	}

	// Update transcript
	transcript := cm.Data[TranscriptKey]
	if transcript != "" && !strings.HasSuffix(transcript, "\n") {
		transcript += "\n"
	}
	transcript += strings.Join(newMessages, "\n")
	cm.Data[TranscriptKey] = transcript

	// Update annotations
	cm.Annotations["mercan.ai/updated-at"] = now.Format(time.RFC3339)
	// Note: Message count and token tracking would need more sophisticated parsing

	return m.Update(ctx, cm)
}

// LoadTranscript loads the session transcript for a task
func (m *SessionManager) LoadTranscript(ctx context.Context, task *corev1alpha1.Task) ([]SessionMessage, error) {
	if task.Spec.SessionRef == nil {
		return nil, nil
	}

	cmName := getSessionConfigMapName(task.Spec.SessionRef.Name)
	cm := &corev1.ConfigMap{}

	err := m.Get(ctx, types.NamespacedName{
		Name:      cmName,
		Namespace: task.Namespace,
	}, cm)

	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	transcript := cm.Data[TranscriptKey]
	if transcript == "" {
		return nil, nil
	}

	var messages []SessionMessage
	lines := strings.Split(strings.TrimSpace(transcript), "\n")

	for _, line := range lines {
		if line == "" {
			continue
		}
		var msg SessionMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue // Skip malformed lines
		}
		messages = append(messages, msg)
	}

	// Apply maxMessages limit if specified
	maxMessages := int32(50) // Default
	if task.Spec.SessionRef.MaxMessages > 0 {
		maxMessages = task.Spec.SessionRef.MaxMessages
	}

	if int32(len(messages)) > maxMessages {
		messages = messages[len(messages)-int(maxMessages):]
	}

	return messages, nil
}

// GetSession gets a session by name
func (m *SessionManager) GetSession(ctx context.Context, namespace, name string) (*corev1.ConfigMap, error) {
	cmName := getSessionConfigMapName(name)
	cm := &corev1.ConfigMap{}

	err := m.Get(ctx, types.NamespacedName{
		Name:      cmName,
		Namespace: namespace,
	}, cm)

	if err != nil {
		return nil, err
	}

	return cm, nil
}

// DeleteSession deletes a session
func (m *SessionManager) DeleteSession(ctx context.Context, namespace, name string) error {
	cmName := getSessionConfigMapName(name)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: namespace,
		},
	}

	return m.Delete(ctx, cm)
}

// ListSessions lists all sessions in a namespace
func (m *SessionManager) ListSessions(ctx context.Context, namespace string) (*corev1.ConfigMapList, error) {
	cmList := &corev1.ConfigMapList{}

	err := m.List(ctx, cmList, client.InNamespace(namespace), client.MatchingLabels{
		SessionLabelKey: "true",
	})

	if err != nil {
		return nil, err
	}

	return cmList, nil
}
