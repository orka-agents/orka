/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

var _ store.MessageStore = (*Store)(nil)

const initialMessageBatchCapacity = 32

// sentMessage is the identity SendMessage assigns. The operation receipt keeps
// only this, not the message's content, so the content leaves the store with
// the message when Task cleanup deletes it.
type sentMessage struct {
	ID        int64
	CreatedAt time.Time
}

// SendMessage stores a new inter-agent message and, once it has committed,
// writes its assigned ID and creation time back into msg. A caller's creation
// time is kept; a zero one gets the store clock.
func (s *Store) SendMessage(ctx context.Context, msg *store.Message) error {
	if msg == nil {
		return store.ValidationErrorf("message is required")
	}
	request := *msg
	sent, err := writeOperation(ctx, s, "SendMessage", nil, func(ctx context.Context) (sentMessage, error) {
		return s.sendMessageWrite(ctx, request)
	})
	if err != nil {
		return err
	}
	msg.ID, msg.CreatedAt = sent.ID, sent.CreatedAt
	return nil
}

func (s *Store) sendMessageWrite(ctx context.Context, msg store.Message) (sentMessage, error) {
	var sent sentMessage
	err := s.withTaskDataFence(ctx, func(ctx context.Context) error {
		var err error
		sent, err = s.sendMessageUnfenced(ctx, msg)
		return err
	})
	return sent, err
}

func (s *Store) sendMessageUnfenced(ctx context.Context, msg store.Message) (sentMessage, error) {
	createdAt := msg.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = s.now()
	}
	result, err := s.taskDataExecutor(ctx).ExecContext(ctx,
		`INSERT INTO messages (namespace, from_task, to_task, parent_task, content, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		msg.Namespace, msg.FromTask, msg.ToTask, msg.ParentTask, msg.Content, createdAt,
	)
	if err != nil {
		return sentMessage{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return sentMessage{}, err
	}
	return sentMessage{ID: id, CreatedAt: createdAt}, nil
}

// GetMessages implements Orka's store.MessageStore for callers that use the
// store directly: it consumes the oldest batch of the inbox when markRead is
// set and lists it otherwise.
func (s *Store) GetMessages(ctx context.Context, namespace, taskName, parentTask string, markRead bool) ([]store.Message, error) {
	return store.ReadMessages(ctx, s, namespace, taskName, parentTask, markRead)
}

// ListUnreadMessages returns up to limit of the oldest unread messages in a
// Task's inbox, including broadcasts to its siblings, without marking them.
// parentTask scopes both direct and broadcast delivery to one coordinator
// family.
func (s *Store) ListUnreadMessages(ctx context.Context, namespace, taskName, parentTask string, limit int) ([]store.Message, error) {
	limit, err := storagev1.MessageBatchLimit(limit)
	if err != nil {
		return nil, err
	}
	var messages []store.Message
	err = s.withTaskDataFence(ctx, func(ctx context.Context) error {
		var err error
		messages, err = selectUnreadMessages(ctx, s.taskDataExecutor(ctx), namespace, taskName, parentTask, limit)
		return err
	})
	return messages, err
}

// ConsumeMessages returns up to request.Limit of the oldest unread messages in
// a Task's inbox and marks exactly those messages read, by ID. The operation
// receipt keeps only the IDs. A replay reads those messages again in its own
// transaction, after the Task data fence check, so it leaves out a message
// deleted since and returns nothing past a cleanup or Job revocation the
// fence detects.
func (s *Store) ConsumeMessages(ctx context.Context, request store.ConsumeMessagesRequest) ([]store.Message, error) {
	limit, err := storagev1.MessageBatchLimit(request.Limit)
	if err != nil {
		return nil, err
	}
	var messages []store.Message
	_, err = writeOperationRereading(ctx, s, "ConsumeMessages",
		func(ctx context.Context) ([]int64, error) {
			err := s.withTaskDataFence(ctx, func(ctx context.Context) error {
				var err error
				messages, err = s.consumeMessagesUnfenced(ctx, request, limit)
				return err
			})
			if err != nil {
				return nil, err
			}
			return messageIDs(messages), nil
		},
		func(ctx context.Context, ids []int64) ([]int64, error) {
			messages = nil
			if len(ids) == 0 {
				return ids, nil
			}
			var err error
			messages, err = selectMessagesByID(ctx, s.taskDataExecutor(ctx), request.Namespace, ids)
			return ids, err
		},
	)
	if err != nil {
		return nil, err
	}
	return messages, nil
}

func (s *Store) consumeMessagesUnfenced(ctx context.Context, request store.ConsumeMessagesRequest, limit int) ([]store.Message, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	messages, err := selectUnreadMessages(ctx, tx, request.Namespace, request.TaskName, request.ParentTask, limit)
	if err != nil {
		return nil, err
	}
	// Mark exactly the selected messages, by ID, so a message the selection
	// did not return is never marked, whatever the isolation level. The batch
	// limit bounds the placeholders.
	if len(messages) > 0 {
		args := make([]any, 0, len(messages)+1)
		args = append(args, request.Namespace)
		for _, message := range messages {
			args = append(args, message.ID)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE messages SET read = TRUE WHERE namespace = ? AND id IN (`+messageIDPlaceholders(len(messages))+`)`,
			args...,
		); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return messages, nil
}

func messageIDs(messages []store.Message) []int64 {
	if len(messages) == 0 {
		return nil
	}
	ids := make([]int64, len(messages))
	for i, message := range messages {
		ids[i] = message.ID
	}
	return ids
}

func messageIDPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func selectUnreadMessages(
	ctx context.Context, executor taskDataExecutor, namespace, taskName, parentTask string, limit int,
) ([]store.Message, error) {
	rows, err := executor.QueryContext(ctx,
		`SELECT id, from_task, to_task, parent_task, content, created_at
		 FROM messages
		 WHERE namespace = ? AND read = FALSE
		   AND from_task != ?
		   AND parent_task = ?
		   AND (to_task = ? OR to_task = '*')
		 ORDER BY id ASC
		 LIMIT ?`,
		namespace, taskName, parentTask, taskName, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	return scanMessages(rows, namespace)
}

// selectMessagesByID reads the messages a consuming read returned, in ID
// order. A message deleted since is left out. They are returned as that read
// returned them, without their read flag.
func selectMessagesByID(ctx context.Context, executor taskDataExecutor, namespace string, ids []int64) ([]store.Message, error) {
	args := make([]any, 0, len(ids)+1)
	args = append(args, namespace)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := executor.QueryContext(ctx,
		`SELECT id, from_task, to_task, parent_task, content, created_at
		 FROM messages WHERE namespace = ? AND id IN (`+messageIDPlaceholders(len(ids))+`)
		 ORDER BY id ASC`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	return scanMessages(rows, namespace)
}

func scanMessages(rows *sql.Rows, namespace string) ([]store.Message, error) {
	var messages []store.Message
	for rows.Next() {
		if messages == nil {
			messages = make([]store.Message, 0, initialMessageBatchCapacity)
		}
		m := store.Message{Namespace: namespace}
		if err := rows.Scan(&m.ID, &m.FromTask, &m.ToTask, &m.ParentTask, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.CreatedAt = m.CreatedAt.UTC()
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}

// DeleteTaskMessages deletes all messages involving a task (sent or received).
func (s *Store) DeleteTaskMessages(ctx context.Context, namespace, taskName string) error {
	return writeOperationErr(ctx, s, "DeleteTaskMessages", func(ctx context.Context) error {
		return s.deleteTaskMessagesWrite(ctx, namespace, taskName)
	})
}

func (s *Store) deleteTaskMessagesWrite(ctx context.Context, namespace, taskName string) error {
	return s.deleteTaskData(ctx, namespace, taskName,
		`DELETE FROM messages WHERE namespace = ? AND (from_task = ? OR to_task = ?)`,
		namespace, taskName, taskName,
	)
}

// DeleteParentMessages deletes all messages for children of a parent task.
func (s *Store) DeleteParentMessages(ctx context.Context, namespace, parentTask string) error {
	return writeOperationErr(ctx, s, "DeleteParentMessages", func(ctx context.Context) error {
		return s.deleteParentMessagesWrite(ctx, namespace, parentTask)
	})
}

func (s *Store) deleteParentMessagesWrite(ctx context.Context, namespace, parentTask string) error {
	return s.deleteTaskData(ctx, namespace, parentTask,
		`DELETE FROM messages WHERE namespace = ? AND parent_task = ?`,
		namespace, parentTask,
	)
}
