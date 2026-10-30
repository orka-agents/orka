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
	schema167 = "01af383a41304d65579d5221976bf479c7e8561a4ec363ba41dcb5f4d571af6c" // #574, harness v1 tables removed
	schema168 = "b7af032523fa2f524c19ceb897fc1807e6850a8c43fa6e0779d059f11fe2c133" // dataset identity
	schema169 = "3f7260f77441a72a48bd82f23fd7b24e1b5252b700731588549c7a841f536731" // Agent Host Protocol store

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

//go:embed migrations/166-to-167.sql
var migration166To167 string

//go:embed migrations/167-to-168.sql
var migration167To168 string

//go:embed migrations/168-to-169.sql
var migration168To169 string

type schemaMigration struct {
	from, to string
	sql      string
	// precheck runs inside the migration transaction before sql.
	precheck func(context.Context, *sql.Conn) error
}

func schemaMigrations() []schemaMigration {
	return []schemaMigration{
		{schema149, schema150, migration149To150, nil},
		{schema150, schema162, migration150To162, nil},
		{schema162, schema166, migration162To166, nil},
		{schema166, schema167, migration166To167, requireNoHarnessV1Records},
		{schema167, schema168, migration167To168, nil},
		{schema168, schema169, migration168To169, nil},
	}
}

// requireNoHarnessV1Records refuses to upgrade a store that holds harness v1
// state: rows in the retired tables, or v1 Session lineages that a harness v2
// controller could never continue. A harness v2 store never writes either, so
// such rows mean the store belonged to an unsupported harness v1 installation.
func requireNoHarnessV1Records(ctx context.Context, conn *sql.Conn) error {
	for _, check := range []struct{ records, query string }{
		{"harness_v1_attempts", `SELECT COUNT(*) FROM harness_v1_attempts`},
		{"runtime_sessions", `SELECT COUNT(*) FROM runtime_sessions`},
		{"session_lineages", `SELECT COUNT(*) FROM session_lineages WHERE contract_version <> 'orka.harness.v2'`},
	} {
		var rows int
		if err := conn.QueryRowContext(ctx, check.query).Scan(&rows); err != nil {
			return err
		}
		if rows != 0 {
			return fmt.Errorf("SQLite store holds %d harness v1 %s records; harness v1 is no longer supported, so install a new harness v2 store", rows, check.records)
		}
	}
	return nil
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
	version, err := readSchemaVersion(conn)
	if err != nil {
		return err
	}
	// Unversioned historical layouts (user_version = 0) take the chain below,
	// which ends by inserting a new adopted identity. A schema version 1 store,
	// written by the release that introduced dataset identity, already has an
	// established store_dataset row and takes upgradeVersion1 instead. Every
	// store this release writes has user_version = SchemaVersion (2).
	//
	// The next additive layout cannot just append a step to schemaMigrations:
	// it must raise SchemaVersion again and add a version-2 path like
	// upgradeVersion1, keeping the identity row and adding no rows. Test it
	// from frozen version-2 stores in every dataset state, as
	// TestOpenLockedStoreUpgradesSchemaVersion1Stores does for version 1.
	if version == 1 {
		return upgradeVersion1(ctx, conn, path, validationErr)
	}
	if version != 0 {
		return validationErr
	}
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

// upgradeVersion1 adds the Agent Host Protocol tables to an exact schema
// version 1 store. It keeps the store's identity row as it is, never calling
// initializeDatasetRecord, and adds no rows, so DatasetInfo is unchanged apart
// from SchemaVersion: an empty incomplete new store can still mint its key.
func upgradeVersion1(ctx context.Context, conn *sql.Conn, path string, validationErr error) error {
	schema, err := readSchema(conn)
	if err != nil {
		return err
	}
	if schemaFingerprint(schema) != schema168 {
		return validationErr
	}
	backup, err := backupSchemaDatabase(ctx, conn, path)
	if err != nil {
		return fmt.Errorf("back up SQLite before upgrade: %w", err)
	}
	log.FromContext(ctx).WithName("sqlite-store").Info("Retained SQLite pre-upgrade backup", "path", backup)
	if err := runVersion1Upgrade(ctx, conn); err != nil {
		return fmt.Errorf("upgrade SQLite (pre-upgrade backup %q): %w", backup, err)
	}
	return nil
}

func runVersion1Upgrade(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	version, err := readSchemaVersion(conn)
	if err != nil {
		return err
	}
	if version != 1 {
		return fmt.Errorf("SQLite schema version changed after backup; refusing upgrade")
	}
	schema, err := readSchema(conn)
	if err != nil {
		return err
	}
	if schemaFingerprint(schema) != schema168 {
		return fmt.Errorf("SQLite schema changed after backup; refusing upgrade")
	}
	if _, err := readDataset(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, migration168To169); err != nil {
		return fmt.Errorf("apply SQLite transition %s: %w", schema168, err)
	}
	if schema, err = readSchema(conn); err != nil {
		return err
	}
	if schemaFingerprint(schema) != schema169 {
		return fmt.Errorf("SQLite transition produced an unexpected layout")
	}
	if err := validateSchema(schema, currentSchemaStatements()); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return err
	}
	if err := checkSchemaIntegrity(ctx, conn); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `COMMIT`)
	return err
}

func runSchemaMigrations(ctx context.Context, conn *sql.Conn, source string, steps []schemaMigration) error {
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	version, err := readSchemaVersion(conn)
	if err != nil {
		return err
	}
	if version != 0 {
		return fmt.Errorf("unsupported SQLite schema version %d before upgrade", version)
	}
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
		if step.precheck != nil {
			if err := step.precheck(ctx, conn); err != nil {
				return err
			}
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
	// Version-0 sources only; see upgradeSchema for version-1 stores.
	if err := initializeDatasetRecord(conn, datasetOriginAdopted); err != nil {
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
