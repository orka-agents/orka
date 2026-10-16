package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Fingerprints cover every non-internal object, not just object counts or a
// version marker. Keep these and the transition SQL immutable: deriving them
// from currentSchemaStatements would accidentally recognize unknown layouts.
const (
	schema149 = "ed423eaa2f45dedb1bd935dff5840016ea0d5976709a2ffecc9d2efb6a9d5ff7" // 0e567418
	schema150 = "66de67c84e6cf135ea10f5aa98ebb0a65eb920f2742be8c0bd27c5d651167df8" // #731, 1f18614c
	schema162 = "b8ddc48210dbf5cce95a315cff79b2bad966a97e299e396f5574040e0732f6d7" // #701, 57583975
	schema166 = "1bf39f576acdc853d6512b62e0c526734fdf4e8a972dc1435b881b609f9a0c36" // #732, abd2c040

	// #700's tables were subsequently changed by #701. Adding empty tables
	// alone cannot upgrade this layout, even when its custody tables are empty.
	unsupportedConnectorSchema = "c82fb870c7ed92488fbe33daf9f0693ffdee5aebff9f48db6fa115fdf99ce83b"
)

//go:embed migrations/149-to-150.sql
var migration149To150 string

//go:embed migrations/150-to-162.sql
var migration150To162 string

//go:embed migrations/162-to-166.sql
var migration162To166 string

type schemaMigration struct {
	from, to string
	sql      string
}

func schemaMigrations() []schemaMigration {
	return []schemaMigration{
		{schema149, schema150, migration149To150},
		{schema150, schema162, migration150To162},
		{schema162, schema166, migration162To166},
	}
}

func schemaFingerprint(schema map[string]string) string {
	names := make([]string, 0, len(schema))
	for name := range schema {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		_, _ = fmt.Fprintf(hash, "%s\x00%s\n", name, normalizeSchemaSQL(schema[name]))
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

// upgradeSchema is called only while OpenLockedStore owns the process lock.
// VACUUM INTO must precede BEGIN IMMEDIATE: SQLite disallows it in a transaction.
// Pin the connection throughout, then recheck the complete source under the
// SQLite write lock before executing any DDL.
func upgradeSchema(db *sql.DB, path string, validationErr error) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	schema, err := readSchema(conn)
	if err != nil {
		return err
	}
	source := schemaFingerprint(schema)
	steps := schemaMigrations()
	first := -1
	for i, step := range steps {
		if step.from == source {
			first = i
			break
		}
	}
	if first < 0 {
		return validationErr
	}
	backup, err := backupSchemaDatabase(ctx, conn, path)
	if err != nil {
		return fmt.Errorf("back up SQLite before upgrade: %w", err)
	}
	log.FromContext(ctx).WithName("sqlite-store").Info("Retained SQLite pre-upgrade backup", "path", backup)
	if err := runSchemaMigrations(ctx, conn, source, steps[first:]); err != nil {
		return fmt.Errorf("upgrade SQLite (pre-upgrade backup %q): %w", backup, err)
	}
	return nil
}

func runSchemaMigrations(ctx context.Context, conn *sql.Conn, source string, steps []schemaMigration) error {
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	schema, err := readSchema(conn)
	if err != nil {
		return err
	}
	if schemaFingerprint(schema) != source {
		return fmt.Errorf("SQLite schema changed after backup; refusing upgrade")
	}
	for _, step := range steps {
		if step.from != source {
			return fmt.Errorf("SQLite migration chain does not match source layout")
		}
		if _, err := conn.ExecContext(ctx, step.sql); err != nil {
			return fmt.Errorf("apply SQLite transition %s: %w", step.from, err)
		}
		schema, err = readSchema(conn)
		if err != nil {
			return err
		}
		if schemaFingerprint(schema) != step.to {
			return fmt.Errorf("SQLite transition produced an unexpected layout")
		}
		source = step.to
	}
	if err := validateSchema(schema, currentSchemaStatements()); err != nil {
		return err
	}
	if err := checkSchemaIntegrity(ctx, conn); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `COMMIT`)
	return err
}

func backupSchemaDatabase(ctx context.Context, conn *sql.Conn, path string) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".pre-upgrade-*.db")
	if err != nil {
		return "", err
	}
	backup := file.Name()
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(backup)
		}
	}()
	if err := file.Close(); err != nil {
		return "", err
	}
	// CreateTemp reserves an empty, private file; VACUUM INTO accepts that file
	// but will never overwrite a nonempty backup. It includes committed WAL data.
	if _, err := conn.ExecContext(ctx, `VACUUM INTO ?`, backup); err != nil {
		return "", err
	}
	copyDB, err := sql.Open("sqlite", backup)
	if err != nil {
		return "", err
	}
	integrityErr := checkSchemaIntegrity(ctx, copyDB)
	closeErr := copyDB.Close()
	if integrityErr != nil {
		return "", fmt.Errorf("verify SQLite backup: %w", integrityErr)
	}
	if closeErr != nil {
		return "", closeErr
	}
	// The file and directory entry must both be durable before changing schema.
	for _, name := range []string{backup, filepath.Dir(backup)} {
		f, err := os.Open(name)
		if err != nil {
			return "", err
		}
		syncErr := f.Sync()
		closeErr := f.Close()
		if syncErr != nil {
			return "", syncErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	complete = true
	return backup, nil
}

func checkSchemaIntegrity(ctx context.Context, q schemaQuerier) error {
	var result string
	if err := q.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return fmt.Errorf("SQLite integrity_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("SQLite integrity_check failed")
	}
	rows, err := q.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		return fmt.Errorf("SQLite foreign_key_check failed")
	}
	return rows.Err()
}
