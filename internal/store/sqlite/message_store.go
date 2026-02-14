/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"context"
	"time"

	"github.com/sozercan/orka/internal/store"
)

// SendMessage stores a new inter-agent message.
func (s *Store) SendMessage(ctx context.Context, msg *store.Message) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (namespace, from_task, to_task, parent_task, content, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		msg.Namespace, msg.FromTask, msg.ToTask, msg.ParentTask, msg.Content, time.Now().UTC(),
	)
	return err
}

// GetMessages returns unread messages for a task, including broadcasts to siblings.
// parentTask scopes broadcasts: only messages with matching parent_task where to_task="*" are included.
// If markRead is true, messages are marked as read atomically.
func (s *Store) GetMessages(ctx context.Context, namespace, taskName, parentTask string, markRead bool) ([]store.Message, error) {
	// Fetch messages addressed directly to this task OR broadcast to siblings (same parent)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, namespace, from_task, to_task, parent_task, content, read, created_at
		 FROM messages
		 WHERE namespace = ? AND read = FALSE
		   AND from_task != ?
		   AND (to_task = ? OR (to_task = '*' AND parent_task = ?))
		 ORDER BY id ASC`,
		namespace, taskName, taskName, parentTask,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var messages []store.Message
	var ids []int64
	for rows.Next() {
		var m store.Message
		if err := rows.Scan(&m.ID, &m.Namespace, &m.FromTask, &m.ToTask, &m.ParentTask, &m.Content, &m.Read, &m.CreatedAt); err != nil {
			return nil, err
		}
		messages = append(messages, m)
		ids = append(ids, m.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Mark as read if requested
	if markRead && len(ids) > 0 {
		for _, id := range ids {
			if _, err := s.db.ExecContext(ctx, `UPDATE messages SET read = TRUE WHERE id = ?`, id); err != nil {
				return nil, err
			}
		}
	}

	return messages, nil
}

// DeleteTaskMessages deletes all messages involving a task (sent or received).
func (s *Store) DeleteTaskMessages(ctx context.Context, namespace, taskName string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM messages WHERE namespace = ? AND (from_task = ? OR to_task = ?)`,
		namespace, taskName, taskName,
	)
	return err
}

// DeleteParentMessages deletes all messages for children of a parent task.
func (s *Store) DeleteParentMessages(ctx context.Context, namespace, parentTask string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM messages WHERE namespace = ? AND parent_task = ?`,
		namespace, parentTask,
	)
	return err
}
