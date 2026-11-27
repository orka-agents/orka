package sqlite

import (
	"context"
	"database/sql"
)

func (s *Store) readSnapshot(ctx context.Context, read func(taskDataExecutor) error) error {
	if tx := s.taskDataTx(ctx); tx != nil {
		return read(tx)
	}
	return readInTransaction(ctx, s.db, func(tx *sql.Tx) error { return read(tx) })
}

func (s *Store) securityReadSnapshot(ctx context.Context, read func(securityDB) error) error {
	if s.securityTx != nil {
		return read(s.securityTx)
	}
	return readInTransaction(ctx, s.db, func(tx *sql.Tx) error { return read(tx) })
}

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
