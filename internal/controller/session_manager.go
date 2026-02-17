/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/store"
)

// SessionManager manages sessions for conversation continuity.
// It delegates persistence to a store.SessionStore and orchestrates
// the acquire → job → append → release lifecycle.
type SessionManager struct {
	store store.SessionStore
}

// NewSessionManager creates a new SessionManager backed by the given store.
func NewSessionManager(ss store.SessionStore) *SessionManager {
	return &SessionManager{store: ss}
}

// IsLocked checks if a session is locked by another task.
func (m *SessionManager) IsLocked(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if task.Spec.SessionRef == nil {
		return false, nil
	}

	locked, err := m.store.IsLocked(ctx, task.Namespace, task.Spec.SessionRef.Name, task.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Session doesn't exist yet, not locked
			return false, nil
		}
		return false, err
	}

	return locked, nil
}

// AcquireLock acquires the session lock for a task.
func (m *SessionManager) AcquireLock(ctx context.Context, task *corev1alpha1.Task) error {
	if task.Spec.SessionRef == nil {
		return nil
	}

	sessionName := task.Spec.SessionRef.Name
	err := m.store.AcquireLock(ctx, task.Namespace, sessionName, task.Name)
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	if !task.Spec.SessionRef.Create {
		return fmt.Errorf("session %s not found and create=false", sessionName)
	}

	if err := m.createSession(ctx, task); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			// Lost a concurrent create race; attempt normal lock acquisition.
			return m.store.AcquireLock(ctx, task.Namespace, sessionName, task.Name)
		}
		return err
	}
	return nil
}

// ReleaseLock releases the session lock for a task.
func (m *SessionManager) ReleaseLock(ctx context.Context, task *corev1alpha1.Task) error {
	if task.Spec.SessionRef == nil {
		return nil
	}

	err := m.store.ReleaseLock(ctx, task.Namespace, task.Spec.SessionRef.Name, task.Name)
	if err != nil {
		// Ignore not-found errors (session may have been deleted)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}

// createSession creates a new session and acquires the lock in one step.
func (m *SessionManager) createSession(ctx context.Context, task *corev1alpha1.Task) error {
	now := time.Now()
	session := &store.SessionRecord{
		Namespace:   task.Namespace,
		Name:        task.Spec.SessionRef.Name,
		SessionType: "task",
		ActiveTask:  task.Name,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	return m.store.CreateSession(ctx, session)
}

// AppendMessages appends messages from a completed task to the session.
// The resultStore is used to fetch the task result for the assistant message.
func (m *SessionManager) AppendMessages(ctx context.Context, task *corev1alpha1.Task, resultStore store.ResultStore) error {
	if task.Spec.SessionRef == nil || !task.Spec.SessionRef.Append {
		return nil
	}

	var prompt, response string

	if task.Spec.AI != nil && task.Spec.AI.Prompt != "" {
		prompt = task.Spec.AI.Prompt
	} else if task.Spec.Prompt != "" {
		prompt = task.Spec.Prompt
	}

	// Try to get the response from the result store
	if task.Status.ResultRef != nil && task.Status.ResultRef.Available {
		data, err := resultStore.GetResult(ctx, task.Namespace, task.Name)
		if err == nil {
			response = string(data)
		}
	}

	now := time.Now()
	var messages []store.SessionMessage

	if prompt != "" {
		messages = append(messages, store.SessionMessage{
			Role:      "user",
			Content:   prompt,
			Timestamp: now,
		})
	}

	if response != "" {
		messages = append(messages, store.SessionMessage{
			Role:      "assistant",
			Content:   response,
			Timestamp: now,
		})
	}

	if len(messages) == 0 {
		return nil
	}

	return m.store.AppendMessages(ctx, task.Namespace, task.Spec.SessionRef.Name, messages)
}

// LoadTranscript loads the session transcript for a task.
func (m *SessionManager) LoadTranscript(ctx context.Context, task *corev1alpha1.Task) ([]store.SessionMessage, error) {
	if task.Spec.SessionRef == nil {
		return nil, nil
	}

	maxMessages := 50 // Default
	if task.Spec.SessionRef.MaxMessages > 0 {
		maxMessages = int(task.Spec.SessionRef.MaxMessages)
	}

	messages, err := m.store.LoadTranscript(ctx, task.Namespace, task.Spec.SessionRef.Name, maxMessages)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}

	return messages, nil
}

// GetSession gets a session by name.
func (m *SessionManager) GetSession(ctx context.Context, namespace, name string) (*store.SessionRecord, error) {
	return m.store.GetSession(ctx, namespace, name)
}

// DeleteSession deletes a session.
func (m *SessionManager) DeleteSession(ctx context.Context, namespace, name string) error {
	return m.store.DeleteSession(ctx, namespace, name)
}

// ListSessions lists all sessions in a namespace.
func (m *SessionManager) ListSessions(ctx context.Context, namespace string) ([]store.SessionMetadata, error) {
	return m.store.ListSessions(ctx, namespace)
}
