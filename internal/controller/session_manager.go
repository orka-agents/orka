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

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// SessionManager manages sessions for conversation continuity.
// It delegates persistence to a store.SessionStore and orchestrates
// the acquire → job → append → release lifecycle.
type SessionManager struct {
	store store.SessionStore
}

const taskSessionType = "task"

type taskSessionFinalizer interface {
	FinalizeTaskSession(ctx context.Context, namespace, name, taskName, taskUID string, messages []store.SessionMessage) error
}

type taskSessionLockStore interface {
	AcquireTaskLock(ctx context.Context, namespace, name, taskName, taskUID string) error
	ReleaseTaskLock(ctx context.Context, namespace, name, taskName, taskUID string) error
}

type taskSessionLockInspector interface {
	IsTaskLocked(ctx context.Context, namespace, name, taskName, taskUID string) (bool, error)
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

	var (
		locked bool
		err    error
	)
	if inspector, ok := m.store.(taskSessionLockInspector); ok {
		locked, err = inspector.IsTaskLocked(ctx, task.Namespace, task.Spec.SessionRef.Name, task.Name, string(task.UID))
	} else {
		locked, err = m.store.IsLocked(ctx, task.Namespace, task.Spec.SessionRef.Name, task.Name)
	}
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

	// Check if session exists
	_, err := m.store.GetSession(ctx, task.Namespace, task.Spec.SessionRef.Name)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		// Session doesn't exist
		if task.Spec.SessionRef.Create {
			return m.createSession(ctx, task)
		}
		return fmt.Errorf("session %s not found and create=false: %w", task.Spec.SessionRef.Name, store.ErrNotFound)
	}

	if lockStore, ok := m.store.(taskSessionLockStore); ok {
		return lockStore.AcquireTaskLock(ctx, task.Namespace, task.Spec.SessionRef.Name, task.Name, string(task.UID))
	}
	return m.store.AcquireLock(ctx, task.Namespace, task.Spec.SessionRef.Name, task.Name)
}

func (m *SessionManager) ownsLock(session *store.SessionRecord, task *corev1alpha1.Task) bool {
	if session == nil || task == nil || session.ActiveTask != task.Name {
		return false
	}
	if _, ok := m.store.(taskSessionLockStore); !ok {
		return true
	}
	return task.UID != "" && session.ActiveTaskUID == string(task.UID)
}

// ReleaseLock releases the session lock for a task.
func (m *SessionManager) ReleaseLock(ctx context.Context, task *corev1alpha1.Task) error {
	if task.Spec.SessionRef == nil {
		return nil
	}

	var err error
	if lockStore, ok := m.store.(taskSessionLockStore); ok {
		err = lockStore.ReleaseTaskLock(ctx, task.Namespace, task.Spec.SessionRef.Name, task.Name, string(task.UID))
	} else {
		err = m.store.ReleaseLock(ctx, task.Namespace, task.Spec.SessionRef.Name, task.Name)
	}
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
		Namespace:     task.Namespace,
		Name:          task.Spec.SessionRef.Name,
		SessionType:   taskSessionType,
		ActiveTask:    task.Name,
		ActiveTaskUID: string(task.UID),
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	return m.store.CreateSession(ctx, session)
}

// AppendMessages appends messages from a completed task to the session.
// The resultStore is used to fetch the task result for the assistant message.
func (m *SessionManager) AppendMessages(ctx context.Context, task *corev1alpha1.Task, resultStore store.ResultStore) error {
	if task.Spec.SessionRef == nil || !task.Spec.SessionRef.Append {
		return nil
	}

	if _, err := m.store.GetSession(ctx, task.Namespace, task.Spec.SessionRef.Name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}

	messages, err := taskSessionMessages(ctx, task, resultStore, false)
	if err != nil {
		return err
	}
	if len(messages) == 0 {
		return nil
	}

	return m.store.AppendMessages(ctx, task.Namespace, task.Spec.SessionRef.Name, messages)
}

// FinalizeTask atomically appends the task transcript and releases its session
// lock when the backing store supports it. The fallback preserves compatibility
// for non-durable test stores.
func (m *SessionManager) FinalizeTask(ctx context.Context, task *corev1alpha1.Task, resultStore store.ResultStore) error {
	if task == nil || task.Spec.SessionRef == nil {
		return nil
	}
	session, err := m.store.GetSession(ctx, task.Namespace, task.Spec.SessionRef.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if !taskCanFinalizeSession(session, task) {
		return nil
	}
	messages, err := taskSessionMessages(ctx, task, resultStore, true)
	if err != nil {
		return err
	}
	if finalizer, ok := m.store.(taskSessionFinalizer); ok {
		err := finalizer.FinalizeTaskSession(ctx, task.Namespace, task.Spec.SessionRef.Name, task.Name, string(task.UID), messages)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if task.Spec.SessionRef.Append && len(messages) > 0 {
		if err := m.store.AppendMessages(ctx, task.Namespace, task.Spec.SessionRef.Name, messages); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}
	}
	return m.ReleaseLock(ctx, task)
}

func taskCanFinalizeSession(session *store.SessionRecord, task *corev1alpha1.Task) bool {
	if session == nil || task == nil || session.ActiveTask != task.Name {
		return false
	}
	if session.ActiveTaskUID == "" {
		// Legacy records predate immutable Task UID ownership and are backfilled by
		// UID-aware stores during finalization. Non-empty UIDs must match exactly.
		return true
	}
	return task.UID != "" && session.ActiveTaskUID == string(task.UID)
}

func taskSessionMessages(ctx context.Context, task *corev1alpha1.Task, resultStore store.ResultStore, strictResult bool) ([]store.SessionMessage, error) {
	if task == nil || task.Spec.SessionRef == nil || !task.Spec.SessionRef.Append {
		return nil, nil
	}
	var (
		prompt            string
		response          string
		responseAvailable bool
	)

	if task.Spec.AI != nil && task.Spec.AI.Prompt != "" {
		prompt = task.Spec.AI.Prompt
	} else if task.Spec.Prompt != "" {
		prompt = task.Spec.Prompt
	}

	// Try to get the response from the result store
	if task.Status.ResultRef != nil && task.Status.ResultRef.Available {
		if resultStore == nil {
			if strictResult {
				return nil, fmt.Errorf("result store is unavailable for task %s/%s", task.Namespace, task.Name)
			}
		} else {
			data, err := resultStore.GetResult(ctx, task.Namespace, task.Name)
			if err != nil {
				if strictResult {
					return nil, fmt.Errorf("getting task result for session finalization: %w", err)
				}
			} else {
				response = string(data)
				responseAvailable = true
			}
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

	if responseAvailable {
		messages = append(messages, store.SessionMessage{
			Role:      "assistant",
			Content:   response,
			Timestamp: now,
		})
	}

	return messages, nil
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
