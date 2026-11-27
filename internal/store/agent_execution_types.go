/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package store

import (
	"context"
	"time"
)

const AgentExecutionSnapshotSchemaVersion int32 = 1

type AgentExecutionSnapshot struct {
	TaskUID       string
	Digest        string
	SchemaVersion int32
	Body          []byte
	CreatedAt     time.Time
}

func CanonicalAgentExecutionSnapshotDigest(body []byte) string {
	return CanonicalBytesDigest(body)
}

type AgentExecutionSnapshotStore interface {
	PersistAgentExecutionSnapshot(ctx context.Context, snapshot AgentExecutionSnapshot) error

	GetAgentExecutionSnapshot(ctx context.Context, key AgentExecutionSnapshotKey) (*AgentExecutionSnapshot, error)

	DeleteAgentExecutionSnapshots(ctx context.Context, taskUID string) error
}
