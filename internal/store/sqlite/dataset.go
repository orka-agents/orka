package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

type DatasetInfo struct {
	SchemaVersion           int
	ID                      string
	CanBootstrapSnapshotKey bool
}

const (
	datasetOriginNew     = "new"
	datasetOriginAdopted = "adopted"
)

type datasetRecord struct {
	id       string
	origin   string
	complete bool
}

type datasetReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readDataset(reader datasetReader) (datasetRecord, error) {
	rows, err := reader.QueryContext(context.Background(), `SELECT singleton, dataset_id, origin, bootstrap_complete FROM store_dataset`)
	if err != nil {
		return datasetRecord{}, fmt.Errorf("read SQLite dataset identity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var record datasetRecord
	count := 0
	for rows.Next() {
		var singleton, complete int
		if err := rows.Scan(&singleton, &record.id, &record.origin, &complete); err != nil {
			return datasetRecord{}, fmt.Errorf("invalid SQLite dataset identity: %w", err)
		}
		id, err := uuid.Parse(record.id)
		if singleton != 1 || err != nil || id == uuid.Nil || id.String() != record.id || (record.origin != datasetOriginNew && record.origin != datasetOriginAdopted) || (complete != 0 && complete != 1) {
			return datasetRecord{}, fmt.Errorf("invalid SQLite dataset identity")
		}
		record.complete = complete == 1
		count++
	}
	if err := rows.Err(); err != nil {
		return datasetRecord{}, err
	}
	if count != 1 {
		return datasetRecord{}, fmt.Errorf("SQLite dataset identity must contain exactly one record")
	}
	return record, nil
}

func (s *Store) DatasetInfo() (DatasetInfo, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return DatasetInfo{}, err
	}
	defer func() { _ = tx.Rollback() }()
	version, err := readSchemaVersion(tx)
	if err != nil {
		return DatasetInfo{}, err
	}
	if version != SchemaVersion {
		return DatasetInfo{}, fmt.Errorf("unsupported SQLite schema version %d", version)
	}
	record, err := readDataset(tx)
	if err != nil {
		return DatasetInfo{}, err
	}
	info := DatasetInfo{ID: record.id, SchemaVersion: SchemaVersion}
	if record.origin != datasetOriginNew || record.complete {
		return info, nil
	}
	populated, err := datasetHasRecords(context.Background(), tx)
	if err != nil {
		return DatasetInfo{}, err
	}
	info.CanBootstrapSnapshotKey = !populated
	return info, nil
}

func datasetHasRecords(ctx context.Context, tx *sql.Tx) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT GLOB 'sqlite_*' AND name <> 'store_dataset'`)
	if err != nil {
		return false, err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return false, err
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, name := range tables {
		var populated bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM "`+strings.ReplaceAll(name, `"`, `""`)+`" LIMIT 1)`).Scan(&populated); err != nil {
			return false, err
		}
		if populated {
			return true, nil
		}
	}
	return false, nil
}

func synchronizeDatasetMetadata(db *sql.DB) error {
	var busy, frames, checkpointed int
	if err := db.QueryRow(`PRAGMA wal_checkpoint(FULL)`).Scan(&busy, &frames, &checkpointed); err != nil {
		return fmt.Errorf("synchronize SQLite dataset metadata: %w", err)
	}
	if busy != 0 || frames != checkpointed {
		return fmt.Errorf("synchronize SQLite dataset metadata: checkpoint incomplete")
	}
	return nil
}
