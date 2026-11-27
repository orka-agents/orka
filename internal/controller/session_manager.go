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
	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/store"
)

type SessionManager struct {
	store             store.SessionStore
	gatewayEventStore store.GatewayEventStore
	cleanupStore      store.SessionCleanupStore
	epochs            *ControllerEpochManager
}

func NewSessionManager(ss store.SessionStore) *SessionManager {
	return &SessionManager{store: ss}
}

func (m *SessionManager) SetGatewayEventStore(events store.GatewayEventStore) {
	if m != nil {
		m.gatewayEventStore = events
	}
}

func (m *SessionManager) SetACPSessionCleanup(cleanup store.SessionCleanupStore, epochs *ControllerEpochManager) {
	if m != nil {
		m.cleanupStore = cleanup
		m.epochs = epochs
	}
}

func (m *SessionManager) IsLocked(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if event, ok, err := m.gatewayEventForTask(ctx, task); err != nil {
		return false, err
	} else if ok {
		if err := validateGatewayTaskSessionPolicy(task, event); err != nil {
			return false, err
		}
		return m.store.IsLocked(ctx, event.Namespace, event.SessionName, task.Name, string(task.UID), time.Now().UTC())
	}
	if task.Spec.SessionRef == nil {
		return false, nil
	}

	locked, err := m.store.IsLocked(ctx, task.Namespace, task.Spec.SessionRef.Name, task.Name, string(task.UID), time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}

	return locked, nil
}

func (m *SessionManager) AcquireLock(ctx context.Context, task *corev1alpha1.Task) error {
	if event, ok, err := m.gatewayEventForTask(ctx, task); err != nil {
		return err
	} else if ok {
		if err := validateGatewayTaskSessionPolicy(task, event); err != nil {
			return err
		}
		return m.store.AcquireLock(ctx, event.Namespace, event.SessionName, task.Name, string(task.UID), time.Now().UTC())
	}
	if task.Spec.SessionRef == nil {
		return nil
	}

	_, err := m.store.GetSession(ctx, task.Namespace, task.Spec.SessionRef.Name)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if task.Spec.SessionRef.Create {
			return m.createSession(ctx, task)
		}
		return fmt.Errorf("session %s not found and create=false: %w", task.Spec.SessionRef.Name, store.ErrNotFound)
	}

	return m.store.AcquireLock(ctx, task.Namespace, task.Spec.SessionRef.Name, task.Name, string(task.UID), time.Now().UTC())
}

func (m *SessionManager) ReleaseLock(ctx context.Context, task *corev1alpha1.Task) error {
	return m.ReleaseLockWithTokenUsage(ctx, task, 0, 0)
}

func (m *SessionManager) ReleaseLockWithTokenUsage(ctx context.Context, task *corev1alpha1.Task, inputTokens, outputTokens int64) error {
	if _, ok, err := m.gatewayEventForTask(ctx, task); err != nil {
		return err
	} else if ok {
		return nil
	}
	if task.Spec.SessionRef == nil {
		return nil
	}

	if task.UID != "" {
		session, getErr := m.store.GetSession(ctx, task.Namespace, task.Spec.SessionRef.Name)
		if getErr == nil && session.ActiveTask == task.Name && session.ActiveTaskUID == "" {
			if acquireErr := m.store.AcquireLock(ctx, task.Namespace, task.Spec.SessionRef.Name, task.Name, string(task.UID), time.Now().UTC()); acquireErr != nil {
				return acquireErr
			}
		} else if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
			return getErr
		}
	}
	var err error
	if fenced, ok := m.store.(store.FencedSessionWriteStore); ok && (inputTokens != 0 || outputTokens != 0) {
		err = fenced.ReleaseLockWithTokenUsage(ctx, taskSessionOwner(task), int(inputTokens), int(outputTokens))
	} else {
		err = m.store.ReleaseLock(ctx, task.Namespace, task.Spec.SessionRef.Name, task.Name, string(task.UID))
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}

func (m *SessionManager) gatewayEventForTask(
	ctx context.Context, task *corev1alpha1.Task,
) (*store.GatewayEvent, bool, error) {
	if m == nil || m.gatewayEventStore == nil || task == nil || task.UID == "" {
		return nil, false, nil
	}
	event, err := m.gatewayEventStore.GetGatewayEventForTask(ctx, task.Namespace, task.Name, string(task.UID))
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return event, true, nil
}

func validateGatewayTaskSessionPolicy(task *corev1alpha1.Task, event *store.GatewayEvent) error {
	if task == nil || event == nil || task.Spec.SessionRef == nil {
		return store.ValidationErrorf("gateway Task session policy is missing")
	}
	ref := task.Spec.SessionRef
	if ref.Name != event.SessionName || ref.Create || ref.Append ||
		ref.MaxMessages != store.GatewayTranscriptMessageLimit ||
		ref.ThroughMessageID != store.GatewayUserMessageID(event.ID) || !ref.PromptIncluded {
		return store.ValidationErrorf("gateway Task session policy was modified")
	}
	return nil
}

func (m *SessionManager) createSession(ctx context.Context, task *corev1alpha1.Task) error {
	now := time.Now()
	session := &store.SessionRecord{
		Namespace:     task.Namespace,
		Name:          task.Spec.SessionRef.Name,
		SessionType:   acpCancelLogKeyTask,
		ActiveTask:    task.Name,
		ActiveTaskUID: string(task.UID),
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	return m.store.CreateSession(ctx, session)
}

func (m *SessionManager) AppendMessages(ctx context.Context, task *corev1alpha1.Task, resultStore store.ResultStore) error {
	if _, ok, err := m.gatewayEventForTask(ctx, task); err != nil {
		return err
	} else if ok {
		return nil
	}
	if task.Spec.SessionRef == nil || !task.Spec.SessionRef.Append {
		return nil
	}

	session, err := m.store.GetSession(ctx, task.Namespace, task.Spec.SessionRef.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if task.Name == "" || session.ActiveTask != task.Name || session.ActiveTaskUID != string(task.UID) {
		return nil
	}

	var prompt, response string

	if !task.Spec.SessionRef.PromptIncluded {
		if task.Spec.AI != nil && task.Spec.AI.Prompt != "" {
			prompt = task.Spec.AI.Prompt
		} else if task.Spec.Prompt != "" {
			prompt = task.Spec.Prompt
		}
	}

	if resultStore != nil && task.Status.ResultRef != nil && task.Status.ResultRef.Available {
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

	if digest := agentcontext.SessionDigest(task.Status.SoulBinding); digest != "" {
		for i := range messages {
			messages[i].Metadata = map[string]string{store.SessionSoulDigestMetadata: digest}
		}
	}

	if len(messages) == 0 {
		return nil
	}
	if task.UID != "" {
		for i := range messages {
			messages[i].ID = taskSessionMessageID(string(task.UID), messages[i].Role)
			messages[i].Timestamp = time.Time{}
		}
	}

	fenced, ok := m.store.(store.FencedSessionWriteStore)
	if !ok {
		return fmt.Errorf("session store does not support fenced transcript writes")
	}
	return fenced.AppendMessagesWithLock(ctx, taskSessionOwner(task), messages)
}

func taskSessionOwner(task *corev1alpha1.Task) store.SessionOwnerWrite {
	return store.SessionOwnerWrite{
		Namespace: task.Namespace, Name: task.Spec.SessionRef.Name,
		OwnerName: task.Name, OwnerUID: string(task.UID), Now: time.Now().UTC(),
	}
}

func taskSessionMessageID(taskUID, role string) string {
	return "task:" + taskUID + ":" + role
}

func (m *SessionManager) LoadTranscript(ctx context.Context, task *corev1alpha1.Task) ([]store.SessionMessage, error) {
	if task == nil {
		return nil, nil
	}
	if m.gatewayEventStore != nil && task.UID != "" {
		event, err := m.gatewayEventStore.GetGatewayEventForTask(ctx, task.Namespace, task.Name, string(task.UID))
		if err == nil {
			return m.store.LoadTranscriptThrough(
				ctx,
				event.Namespace,
				event.SessionName,
				store.GatewayUserMessageID(event.ID),
				store.GatewayTranscriptMessageLimit,
			)
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	if task.Spec.SessionRef == nil {
		return nil, nil
	}

	maxMessages := 50
	if task.Spec.SessionRef.MaxMessages > 0 {
		maxMessages = int(task.Spec.SessionRef.MaxMessages)
	}

	var messages []store.SessionMessage
	var err error
	if task.Spec.SessionRef.ThroughMessageID != "" {
		messages, err = m.store.LoadTranscriptThrough(
			ctx, task.Namespace, task.Spec.SessionRef.Name, task.Spec.SessionRef.ThroughMessageID, maxMessages,
		)
	} else {
		messages, err = m.store.LoadTranscript(ctx, task.Namespace, task.Spec.SessionRef.Name, maxMessages)
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}

	return messages, nil
}

func (m *SessionManager) GetSession(ctx context.Context, namespace, name string) (*store.SessionRecord, error) {
	return m.store.GetSession(ctx, namespace, name)
}

func (m *SessionManager) DeleteSession(ctx context.Context, namespace, name string) error {
	if m.cleanupStore == nil {
		return m.store.DeleteSession(ctx, namespace, name, time.Now().UTC())
	}
	if m.epochs == nil {
		return fmt.Errorf("ACP session cleanup requires a controller epoch manager")
	}
	fence, err := m.epochs.CurrentFence(ctx)
	if err != nil {
		return err
	}
	operationID := store.CanonicalControlID("session-cleanup", namespace, name)
	operationDigest, err := acpDomainDigest("session-cleanup", map[string]string{
		acpCancelLogKeyNamespace: namespace, "sessionName": name, "operationID": operationID,
	})
	if err != nil {
		return err
	}
	return m.cleanupStore.ReclaimSession(ctx, store.ReclaimSessionRequest{
		Namespace: namespace, SessionName: name, Fence: fence,
		OperationID: operationID, OperationDigest: operationDigest, RequestedAt: time.Now().UTC(),
	})
}

func (m *SessionManager) ListSessions(ctx context.Context, namespace string) ([]store.SessionMetadata, error) {
	return m.store.ListSessions(ctx, namespace)
}

func (m *SessionManager) ListSessionsPage(ctx context.Context, namespace string, after *store.SessionPageCursor, limit int, excludeType string) ([]store.SessionMetadata, bool, error) {
	return m.store.ListSessionsPage(ctx, namespace, after, limit, excludeType)
}
