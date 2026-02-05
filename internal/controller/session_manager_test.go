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
