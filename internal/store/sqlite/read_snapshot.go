package sqlite

import (
	"context"
	"database/sql"
)

// readSnapshot runs read against one consistent snapshot, so a read that
// issues several queries cannot observe a commit between them. Inside a
// task-data transaction it reuses that transaction. Otherwise it opens a
// transaction, which in WAL mode fixes its snapshot at the first query, and
// rolls it back afterwards.
func (s *Store) readSnapshot(ctx context.Context, read func(taskDataExecutor) error) error {
	if tx := s.taskDataTx(ctx); tx != nil {
		return read(tx)
	}
	return readInTransaction(ctx, s.db, func(tx *sql.Tx) error { return read(tx) })
}

// securityReadSnapshot is readSnapshot for the security family, which reuses
// a scan ingestion transaction rather than a task-data one.
func (s *Store) securityReadSnapshot(ctx context.Context, read func(securityDB) error) error {
	if s.securityTx != nil {
		return read(s.securityTx)
	}
	return readInTransaction(ctx, s.db, func(tx *sql.Tx) error { return read(tx) })
}

// writeTransaction runs write in one transaction and commits it, so a method
// that changes a record and reads it back returns the state its own commit
// produced. Inside a task-data transaction it reuses that transaction.
func (s *Store) writeTransaction(ctx context.Context, write func(taskDataExecutor) error) error {
	if tx := s.taskDataTx(ctx); tx != nil {
		return write(tx)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := write(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func readInTransaction(ctx context.Context, db *sql.DB, read func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return read(tx)
}
