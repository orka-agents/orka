package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// DatasetInfo identifies the durable dataset, independently of its filename.
// CanBootstrapSnapshotKey is true only for a never-bootstrapped, newly created
// dataset containing no application, artifact or control records. It is not a
// general-purpose emptiness check or permission to replace an installation.
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

// DatasetInfo reads durable bootstrap history while the production caller holds
// the process lock. No file-existence check is used. An interrupted first start
// can resume only if no application data has appeared in the meantime.
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
	// Use SQLite's object type, not formatting of its CREATE statement. The
	// validated layout may contain equivalent SQL with newlines/quoted names.
	rows, err := tx.Query(`SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT GLOB 'sqlite_*' AND name <> 'store_dataset'`)
	if err != nil {
		return DatasetInfo{}, err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return DatasetInfo{}, err
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return DatasetInfo{}, err
	}
	if err := rows.Close(); err != nil {
		return DatasetInfo{}, err
	}
	for _, name := range tables {
		var populated bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM "` + strings.ReplaceAll(name, `"`, `""`) + `" LIMIT 1)`).Scan(&populated); err != nil {
			return DatasetInfo{}, err
		}
		if populated {
			return info, nil
		}
	}
	info.CanBootstrapSnapshotKey = true
	return info, nil
}

// completeDatasetBootstrap records successful key activation before controller
// consumers can start. Encryption authentication and permitted key rotation
// remain the responsibility of SetAgentExecutionSnapshotCipher.
func (s *Store) completeDatasetBootstrap() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := readDataset(tx)
	if err != nil {
		return err
	}
	if !record.complete {
		if _, err := tx.Exec(`UPDATE store_dataset SET bootstrap_complete = 1 WHERE singleton = 1`); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return synchronizeDatasetMetadata(s.db)
}

// NORMAL synchronous mode does not sync each WAL commit. A full checkpoint
// synchronizes the WAL and database before startup can rely on dataset identity
// or completed bootstrap history. Keep the existing mode for ordinary writes.
// A busy/incomplete checkpoint is a startup failure, including on a retry whose
// previous completion transaction committed but whose checkpoint failed.
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
