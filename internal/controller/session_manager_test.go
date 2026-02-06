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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

func setupSessionManager(objs ...runtime.Object) *SessionManager {
	scheme := runtime.NewScheme()
	corev1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return NewSessionManager(fakeClient)
}

func TestNewSessionManager(t *testing.T) {
	sm := setupSessionManager()
	if sm == nil {
		t.Fatal("NewSessionManager returned nil")
	}
}

func TestSessionManager_IsLocked_NoSessionRef(t *testing.T) {
	sm := setupSessionManager()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			SessionRef: nil, // No session
		},
	}

	locked, err := sm.IsLocked(context.Background(), task)
	if err != nil {
		t.Fatalf("IsLocked() error = %v", err)
	}
	if locked {
		t.Error("IsLocked() should return false for task without session ref")
	}
}

func TestSessionManager_IsLocked_SessionNotFound(t *testing.T) {
	sm := setupSessionManager()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "nonexistent-session",
			},
		},
	}

	locked, err := sm.IsLocked(context.Background(), task)
	if err != nil {
		t.Fatalf("IsLocked() error = %v", err)
	}
	if locked {
		t.Error("IsLocked() should return false for nonexistent session")
	}
}

func TestSessionManager_IsLocked_NotLocked(t *testing.T) {
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-test-session",
			Namespace: "default",
			Labels:    map[string]string{SessionLabelKey: "true"},
			Annotations: map[string]string{
				ActiveTaskAnnotation: "", // No active task
			},
		},
	}

	sm := setupSessionManager(sessionCM)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "test-session",
			},
		},
	}

	locked, err := sm.IsLocked(context.Background(), task)
	if err != nil {
		t.Fatalf("IsLocked() error = %v", err)
	}
	if locked {
		t.Error("IsLocked() should return false when no active task")
	}
}

func TestSessionManager_IsLocked_LockedByOther(t *testing.T) {
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-test-session",
			Namespace: "default",
			Labels:    map[string]string{SessionLabelKey: "true"},
			Annotations: map[string]string{
				ActiveTaskAnnotation: "other-task", // Locked by another task
			},
		},
	}

	sm := setupSessionManager(sessionCM)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "test-session",
			},
		},
	}

	locked, err := sm.IsLocked(context.Background(), task)
	if err != nil {
		t.Fatalf("IsLocked() error = %v", err)
	}
	if !locked {
		t.Error("IsLocked() should return true when locked by another task")
	}
}

func TestSessionManager_IsLocked_LockedBySelf(t *testing.T) {
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-test-session",
			Namespace: "default",
			Labels:    map[string]string{SessionLabelKey: "true"},
			Annotations: map[string]string{
				ActiveTaskAnnotation: "test-task", // Locked by this task
			},
		},
	}

	sm := setupSessionManager(sessionCM)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "test-session",
			},
		},
	}

	locked, err := sm.IsLocked(context.Background(), task)
	if err != nil {
		t.Fatalf("IsLocked() error = %v", err)
	}
	if locked {
		t.Error("IsLocked() should return false when locked by self")
	}
}

func TestSessionManager_AcquireLock_NoSessionRef(t *testing.T) {
	sm := setupSessionManager()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			SessionRef: nil,
		},
	}

	err := sm.AcquireLock(context.Background(), task)
	if err != nil {
		t.Errorf("AcquireLock() error = %v", err)
	}
}

func TestSessionManager_AcquireLock_CreateSession(t *testing.T) {
	sm := setupSessionManager()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "new-session",
				Create: true,
			},
		},
	}

	err := sm.AcquireLock(context.Background(), task)
	if err != nil {
		t.Fatalf("AcquireLock() error = %v", err)
	}

	// Verify session was created
	cm, err := sm.GetSession(context.Background(), "default", "new-session")
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if cm.Annotations[ActiveTaskAnnotation] != "test-task" {
		t.Errorf("ActiveTaskAnnotation = %s, want test-task", cm.Annotations[ActiveTaskAnnotation])
	}
}

func TestSessionManager_AcquireLock_SessionNotFound_NoCreate(t *testing.T) {
	sm := setupSessionManager()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "nonexistent",
				Create: false,
			},
		},
	}

	err := sm.AcquireLock(context.Background(), task)
	if err == nil {
		t.Error("AcquireLock() expected error for nonexistent session with create=false")
	}
}

func TestSessionManager_AcquireLock_AlreadyLockedByOther(t *testing.T) {
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-test-session",
			Namespace: "default",
			Labels:    map[string]string{SessionLabelKey: "true"},
			Annotations: map[string]string{
				ActiveTaskAnnotation: "other-task",
			},
		},
	}

	sm := setupSessionManager(sessionCM)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "test-session",
			},
		},
	}

	err := sm.AcquireLock(context.Background(), task)
	if err == nil {
		t.Error("AcquireLock() expected error when locked by another task")
	}
}

func TestSessionManager_ReleaseLock_NoSessionRef(t *testing.T) {
	sm := setupSessionManager()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			SessionRef: nil,
		},
	}

	err := sm.ReleaseLock(context.Background(), task)
	if err != nil {
		t.Errorf("ReleaseLock() error = %v", err)
	}
}

func TestSessionManager_ReleaseLock_SessionNotFound(t *testing.T) {
	sm := setupSessionManager()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "nonexistent",
			},
		},
	}

	err := sm.ReleaseLock(context.Background(), task)
	if err != nil {
		t.Errorf("ReleaseLock() error = %v", err)
	}
}

func TestSessionManager_ReleaseLock_NotOwner(t *testing.T) {
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-test-session",
			Namespace: "default",
			Labels:    map[string]string{SessionLabelKey: "true"},
			Annotations: map[string]string{
				ActiveTaskAnnotation: "other-task",
			},
		},
	}

	sm := setupSessionManager(sessionCM)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "test-session",
			},
		},
	}

	err := sm.ReleaseLock(context.Background(), task)
	if err != nil {
		t.Errorf("ReleaseLock() error = %v", err)
	}

	// Verify lock was not released
	cm, _ := sm.GetSession(context.Background(), "default", "test-session")
	if cm.Annotations[ActiveTaskAnnotation] != "other-task" {
		t.Error("Lock should not be released by non-owner")
	}
}

func TestSessionManager_LoadTranscript_NoSessionRef(t *testing.T) {
	sm := setupSessionManager()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			SessionRef: nil,
		},
	}

	msgs, err := sm.LoadTranscript(context.Background(), task)
	if err != nil {
		t.Fatalf("LoadTranscript() error = %v", err)
	}
	if msgs != nil {
		t.Error("LoadTranscript() should return nil for task without session ref")
	}
}

func TestSessionManager_LoadTranscript_SessionNotFound(t *testing.T) {
	sm := setupSessionManager()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "nonexistent",
			},
		},
	}

	msgs, err := sm.LoadTranscript(context.Background(), task)
	if err != nil {
		t.Fatalf("LoadTranscript() error = %v", err)
	}
	if msgs != nil {
		t.Error("LoadTranscript() should return nil for nonexistent session")
	}
}

func TestSessionManager_LoadTranscript_WithMessages(t *testing.T) {
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-test-session",
			Namespace: "default",
			Labels:    map[string]string{SessionLabelKey: "true"},
		},
		Data: map[string]string{
			TranscriptKey: `{"role":"user","content":"hello","ts":"2024-01-01T00:00:00Z"}
{"role":"assistant","content":"hi","ts":"2024-01-01T00:00:01Z"}`,
		},
	}

	sm := setupSessionManager(sessionCM)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "test-session",
			},
		},
	}

	msgs, err := sm.LoadTranscript(context.Background(), task)
	if err != nil {
		t.Fatalf("LoadTranscript() error = %v", err)
	}
	if len(msgs) != 2 {
		t.Errorf("LoadTranscript() returned %d messages, want 2", len(msgs))
	}
}

func TestSessionManager_LoadTranscript_MaxMessages(t *testing.T) {
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-test-session",
			Namespace: "default",
			Labels:    map[string]string{SessionLabelKey: "true"},
		},
		Data: map[string]string{
			TranscriptKey: `{"role":"user","content":"msg1","ts":"2024-01-01T00:00:00Z"}
{"role":"assistant","content":"msg2","ts":"2024-01-01T00:00:01Z"}
{"role":"user","content":"msg3","ts":"2024-01-01T00:00:02Z"}
{"role":"assistant","content":"msg4","ts":"2024-01-01T00:00:03Z"}
{"role":"user","content":"msg5","ts":"2024-01-01T00:00:04Z"}`,
		},
	}

	sm := setupSessionManager(sessionCM)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name:        "test-session",
				MaxMessages: 3, // Only want last 3
			},
		},
	}

	msgs, err := sm.LoadTranscript(context.Background(), task)
	if err != nil {
		t.Fatalf("LoadTranscript() error = %v", err)
	}
	if len(msgs) != 3 {
		t.Errorf("LoadTranscript() returned %d messages, want 3", len(msgs))
	}
	// Should be the last 3 messages
	if msgs[0].Content != "msg3" {
		t.Errorf("First message content = %s, want msg3", msgs[0].Content)
	}
}

func TestSessionManager_LoadTranscript_MalformedLines(t *testing.T) {
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-test-session",
			Namespace: "default",
			Labels:    map[string]string{SessionLabelKey: "true"},
		},
		Data: map[string]string{
			TranscriptKey: `{"role":"user","content":"hello","ts":"2024-01-01T00:00:00Z"}
{invalid json}
{"role":"assistant","content":"hi","ts":"2024-01-01T00:00:01Z"}`,
		},
	}

	sm := setupSessionManager(sessionCM)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name: "test-session",
			},
		},
	}

	msgs, err := sm.LoadTranscript(context.Background(), task)
	if err != nil {
		t.Fatalf("LoadTranscript() error = %v", err)
	}
	// Should skip malformed line and return 2 valid messages
	if len(msgs) != 2 {
		t.Errorf("LoadTranscript() returned %d messages, want 2", len(msgs))
	}
}

func TestSessionManager_GetSession(t *testing.T) {
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-test-session",
			Namespace: "default",
			Labels:    map[string]string{SessionLabelKey: "true"},
		},
	}

	sm := setupSessionManager(sessionCM)

	cm, err := sm.GetSession(context.Background(), "default", "test-session")
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if cm.Name != "session-test-session" {
		t.Errorf("GetSession() returned wrong ConfigMap")
	}
}

func TestSessionManager_DeleteSession(t *testing.T) {
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-test-session",
			Namespace: "default",
			Labels:    map[string]string{SessionLabelKey: "true"},
		},
	}

	sm := setupSessionManager(sessionCM)

	err := sm.DeleteSession(context.Background(), "default", "test-session")
	if err != nil {
		t.Fatalf("DeleteSession() error = %v", err)
	}

	// Verify session was deleted
	_, err = sm.GetSession(context.Background(), "default", "test-session")
	if err == nil {
		t.Error("Session should be deleted")
	}
}

func TestSessionManager_ListSessions(t *testing.T) {
	session1 := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-s1",
			Namespace: "default",
			Labels:    map[string]string{SessionLabelKey: "true"},
		},
	}
	session2 := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-s2",
			Namespace: "default",
			Labels:    map[string]string{SessionLabelKey: "true"},
		},
	}
	otherCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-cm",
			Namespace: "default",
		},
	}

	sm := setupSessionManager(session1, session2, otherCM)

	cmList, err := sm.ListSessions(context.Background(), "default")
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(cmList.Items) != 2 {
		t.Errorf("ListSessions() returned %d items, want 2", len(cmList.Items))
	}
}

func TestSessionMessage_Fields(t *testing.T) {
	msg := SessionMessage{
		Role:    "user",
		Content: "Hello",
		Name:    "test",
	}

	if msg.Role != "user" {
		t.Errorf("Role = %s, want user", msg.Role)
	}
	if msg.Content != "Hello" {
		t.Errorf("Content = %s, want Hello", msg.Content)
	}
}

func TestGetSessionConfigMapName(t *testing.T) {
	name := getSessionConfigMapName("my-session")
	if name != "session-my-session" {
		t.Errorf("getSessionConfigMapName() = %s, want session-my-session", name)
	}
}

func TestSessionManager_AppendMessages_NoSessionRef(t *testing.T) {
	sm := setupSessionManager()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			SessionRef: nil,
		},
	}

	err := sm.AppendMessages(context.Background(), task)
	if err != nil {
		t.Errorf("AppendMessages() error = %v", err)
	}
}

func TestSessionManager_AppendMessages_AppendFalse(t *testing.T) {
	sm := setupSessionManager()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "test-session",
				Append: false,
			},
		},
	}

	err := sm.AppendMessages(context.Background(), task)
	if err != nil {
		t.Errorf("AppendMessages() error = %v", err)
	}
}

func TestSessionManager_AppendMessages_WithPromptAndResult(t *testing.T) {
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-test-session",
			Namespace: "default",
			Labels:    map[string]string{SessionLabelKey: "true"},
			Annotations: map[string]string{
				"mercan.ai/updated-at": "2024-01-01T00:00:00Z",
			},
		},
		Data: map[string]string{
			TranscriptKey: "",
		},
	}

	resultCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-result",
			Namespace: "default",
		},
		Data: map[string]string{
			"result": "Here is the answer",
		},
	}

	sm := setupSessionManager(sessionCM, resultCM)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Prompt: "What is the answer?",
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "test-session",
				Append: true,
			},
		},
		Status: corev1alpha1.TaskStatus{
			ResultRef: &corev1alpha1.ResultReference{
				ConfigMapName: "task-result",
				Key:           "result",
			},
		},
	}

	err := sm.AppendMessages(context.Background(), task)
	if err != nil {
		t.Fatalf("AppendMessages() error = %v", err)
	}

	// Verify the transcript was updated
	cm, err := sm.GetSession(context.Background(), "default", "test-session")
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}

	transcript := cm.Data[TranscriptKey]
	if transcript == "" {
		t.Fatal("transcript should not be empty after appending messages")
	}

	// Parse the transcript lines
	lines := strings.Split(strings.TrimSpace(transcript), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 transcript lines, got %d: %q", len(lines), transcript)
	}

	// Verify user message
	var userMsg SessionMessage
	if err := json.Unmarshal([]byte(lines[0]), &userMsg); err != nil {
		t.Fatalf("failed to parse user message: %v", err)
	}
	if userMsg.Role != "user" {
		t.Errorf("user message role = %q, want 'user'", userMsg.Role)
	}
	if userMsg.Content != "What is the answer?" {
		t.Errorf("user message content = %q, want 'What is the answer?'", userMsg.Content)
	}

	// Verify assistant message
	var assistantMsg SessionMessage
	if err := json.Unmarshal([]byte(lines[1]), &assistantMsg); err != nil {
		t.Fatalf("failed to parse assistant message: %v", err)
	}
	if assistantMsg.Role != "assistant" {
		t.Errorf("assistant message role = %q, want 'assistant'", assistantMsg.Role)
	}
	if assistantMsg.Content != "Here is the answer" {
		t.Errorf("assistant message content = %q, want 'Here is the answer'", assistantMsg.Content)
	}
}

func TestSessionManager_AppendMessages_WithAIPrompt(t *testing.T) {
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-test-session",
			Namespace: "default",
			Labels:    map[string]string{SessionLabelKey: "true"},
			Annotations: map[string]string{
				"mercan.ai/updated-at": "2024-01-01T00:00:00Z",
			},
		},
		Data: map[string]string{
			TranscriptKey: "",
		},
	}

	sm := setupSessionManager(sessionCM)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			AI: &corev1alpha1.AISpec{
				Prompt: "AI prompt here",
			},
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "test-session",
				Append: true,
			},
		},
	}

	err := sm.AppendMessages(context.Background(), task)
	if err != nil {
		t.Fatalf("AppendMessages() error = %v", err)
	}

	cm, err := sm.GetSession(context.Background(), "default", "test-session")
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}

	transcript := cm.Data[TranscriptKey]
	lines := strings.Split(strings.TrimSpace(transcript), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 transcript line (user only, no result), got %d", len(lines))
	}

	var msg SessionMessage
	if err := json.Unmarshal([]byte(lines[0]), &msg); err != nil {
		t.Fatalf("failed to parse message: %v", err)
	}
	if msg.Content != "AI prompt here" {
		t.Errorf("message content = %q, want 'AI prompt here'", msg.Content)
	}
}

func TestSessionManager_AppendMessages_AppendsToExistingTranscript(t *testing.T) {
	existingTranscript := `{"role":"user","content":"first question","ts":"2024-01-01T00:00:00Z"}
{"role":"assistant","content":"first answer","ts":"2024-01-01T00:00:01Z"}`

	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-test-session",
			Namespace: "default",
			Labels:    map[string]string{SessionLabelKey: "true"},
			Annotations: map[string]string{
				"mercan.ai/updated-at": "2024-01-01T00:00:00Z",
			},
		},
		Data: map[string]string{
			TranscriptKey: existingTranscript,
		},
	}

	sm := setupSessionManager(sessionCM)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Prompt: "second question",
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "test-session",
				Append: true,
			},
		},
	}

	err := sm.AppendMessages(context.Background(), task)
	if err != nil {
		t.Fatalf("AppendMessages() error = %v", err)
	}

	cm, err := sm.GetSession(context.Background(), "default", "test-session")
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}

	transcript := cm.Data[TranscriptKey]
	lines := strings.Split(strings.TrimSpace(transcript), "\n")
	// 2 existing + 1 new user message
	if len(lines) != 3 {
		t.Fatalf("expected 3 transcript lines, got %d: %q", len(lines), transcript)
	}

	var newMsg SessionMessage
	if err := json.Unmarshal([]byte(lines[2]), &newMsg); err != nil {
		t.Fatalf("failed to parse new message: %v", err)
	}
	if newMsg.Content != "second question" {
		t.Errorf("new message content = %q, want 'second question'", newMsg.Content)
	}
}

func TestSessionManager_AppendMessages_NoPromptNoResult(t *testing.T) {
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-test-session",
			Namespace: "default",
			Labels:    map[string]string{SessionLabelKey: "true"},
			Annotations: map[string]string{
				"mercan.ai/updated-at": "2024-01-01T00:00:00Z",
			},
		},
		Data: map[string]string{
			TranscriptKey: "",
		},
	}

	sm := setupSessionManager(sessionCM)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "test-session",
				Append: true,
			},
		},
	}

	err := sm.AppendMessages(context.Background(), task)
	if err != nil {
		t.Fatalf("AppendMessages() error = %v", err)
	}

	// Transcript should remain empty when there's no prompt and no result
	cm, err := sm.GetSession(context.Background(), "default", "test-session")
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if cm.Data[TranscriptKey] != "" {
		t.Errorf("transcript should be empty, got %q", cm.Data[TranscriptKey])
	}
}
