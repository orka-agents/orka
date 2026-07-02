package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func BenchmarkGetMessagesMarkRead100(b *testing.B) {
	ctx := context.Background()
	s := setupBenchmarkStore(b)
	const (
		namespace  = "bench-messages"
		taskName   = "reader"
		parentTask = "parent"
		count      = 100
	)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ { //nolint:intrange,modernize // b.Loop requires the timer to be running at loop boundaries.
		b.StopTimer()
		resetBenchmarkMessages(ctx, b, s, namespace, taskName, parentTask, count)
		b.StartTimer()

		messages, err := s.GetMessages(ctx, namespace, taskName, parentTask, true)
		b.StopTimer()

		if err != nil {
			b.Fatalf("GetMessages: %v", err)
		}
		if len(messages) != count {
			b.Fatalf("got %d messages, want %d", len(messages), count)
		}
	}
}

func setupBenchmarkStore(b *testing.B) *Store {
	b.Helper()
	db, err := NewDB(":memory:")
	if err != nil {
		b.Fatalf("NewDB(:memory:): %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	return NewStore(db, ":memory:")
}

func resetBenchmarkMessages(ctx context.Context, b *testing.B, s *Store, namespace, taskName, parentTask string, count int) {
	b.Helper()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE namespace = ?`, namespace); err != nil {
		b.Fatalf("delete benchmark messages: %v", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatalf("begin seed tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO messages (namespace, from_task, to_task, parent_task, content, read, created_at)
		 VALUES (?, ?, ?, ?, ?, FALSE, ?)`,
	)
	if err != nil {
		b.Fatalf("prepare seed insert: %v", err)
	}
	defer func() { _ = stmt.Close() }()

	createdAt := time.Unix(1_700_000_000, 0).UTC()
	for i := range count {
		toTask := taskName
		if i%4 == 0 {
			toTask = "*"
		}
		if _, err := stmt.ExecContext(
			ctx,
			namespace,
			fmt.Sprintf("sender-%02d", i%10),
			toTask,
			parentTask,
			fmt.Sprintf("message-%03d", i),
			createdAt,
		); err != nil {
			b.Fatalf("insert benchmark message %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit seed tx: %v", err)
	}
}
