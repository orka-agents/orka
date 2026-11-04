package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// SchemaVersion records the metadata layout. It never replaces validation of
// every schema object; version zero upgrades require the locked migration path.
const SchemaVersion = 1

const datasetSchema = `CREATE TABLE IF NOT EXISTS store_dataset (
 singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
 dataset_id TEXT NOT NULL,
 origin TEXT NOT NULL CHECK(origin IN ('new', 'adopted')),
 bootstrap_complete INTEGER NOT NULL CHECK(bootstrap_complete IN (0, 1))
)`

// initializeSchema creates an empty database or checks the existing layout.
// Callers without the controller lock only validate; OpenLockedStore can also
// migrate exact known layouts after taking a consistent backup.
func initializeSchema(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	version, err := readSchemaVersion(tx)
	if err != nil {
		return err
	}
	if version != 0 && version != SchemaVersion {
		return fmt.Errorf("unsupported SQLite schema version %d (supported: %d)", version, SchemaVersion)
	}
	existing, err := readSchema(tx)
	if err != nil {
		return err
	}
	statements := currentSchemaStatements()
	if len(existing) > 0 {
		if err := validateSchema(existing, statements); err != nil {
			return err
		}
		if version != SchemaVersion {
			return fmt.Errorf("unsupported SQLite schema: dataset metadata requires schema version %d", SchemaVersion)
		}
		_, err := readDataset(tx)
		return err
	}
	if version != 0 {
		return fmt.Errorf("unsupported SQLite schema: versioned dataset has no schema")
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("create current schema: %w", err)
		}
	}
	if err := initializeDatasetRecord(tx, datasetOriginNew); err != nil {
		return err
	}
	return tx.Commit()
}

func readSchemaVersion(q schemaQuerier) (int, error) {
	var version int
	err := q.QueryRowContext(context.Background(), `PRAGMA user_version`).Scan(&version)
	return version, err
}

type datasetInitializer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// Called only within the fresh-schema or exact-upgrade transaction. No repair
// path may regenerate a missing identity in an established dataset.
func initializeDatasetRecord(q datasetInitializer, origin string) error {
	id, err := uuid.NewRandom()
	if err != nil {
		return fmt.Errorf("generate SQLite dataset identity: %w", err)
	}
	if _, err := q.ExecContext(context.Background(), `INSERT INTO store_dataset(singleton, dataset_id, origin, bootstrap_complete) VALUES(1, ?, ?, 0)`, id.String(), origin); err != nil {
		return err
	}
	_, err = q.ExecContext(context.Background(), fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion))
	return err
}

type schemaQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readSchema(q schemaQuerier) (map[string]string, error) {
	rows, err := q.QueryContext(context.Background(), `SELECT name, sql FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	schema := make(map[string]string)
	for rows.Next() {
		var name, statement string
		if err := rows.Scan(&name, &statement); err != nil {
			return nil, err
		}
		schema[strings.ToLower(name)] = statement
	}
	return schema, rows.Err()
}

func validateSchema(existing map[string]string, statements []string) error {
	if schemaFingerprint(existing) == unsupportedConnectorSchema {
		return fmt.Errorf("unsupported SQLite schema: #700 layout at b5265055 (161 objects) requires changes to existing connector tables; no rewrite path is supported; see website/docs/operations/upgrading.md (https://github.com/orka-agents/orka/blob/main/website/docs/operations/upgrading.md)")
	}
	for _, statement := range statements {
		// Every definition uses CREATE ... IF NOT EXISTS <name>. SQLite omits
		// IF NOT EXISTS when it stores the definition in sqlite_schema.
		prefix, definition, ok := strings.Cut(statement, "IF NOT EXISTS ")
		if !ok {
			return fmt.Errorf("invalid current schema definition")
		}
		name := strings.Fields(definition)[0]
		if normalizeSchemaSQL(existing[name]) != normalizeSchemaSQL(prefix+definition) {
			return fmt.Errorf("unsupported SQLite schema: %s is missing or incompatible; only exact documented layouts can be upgraded through OpenLockedStore", name)
		}
	}
	if len(existing) != len(statements) {
		return fmt.Errorf("unsupported SQLite schema: unexpected schema objects; only exact documented layouts can be upgraded through OpenLockedStore")
	}
	return nil
}

// Compare SQL tokens so formatting does not affect reopening. Preserve string
// literals and compare the whole definition, including defaults and constraints.
var schemaSQLTokens = regexp.MustCompile("'([^']|'')*'|\"([^\"]|\"\")*\"|`[^`]*`|\\[[^]]*\\]|[a-zA-Z_0-9]+|[^\\s]")

func normalizeSchemaSQL(statement string) string {
	tokens := schemaSQLTokens.FindAllString(statement, -1)
	for i, token := range tokens {
		if token[0] == '\'' {
			continue
		}
		// SQLite quotes object names after a rename. Only unwrap those names:
		// quoted keywords elsewhere can be type names instead of constraints.
		if i > 0 && len(token) > 1 {
			switch tokens[i-1] {
			case "TABLE", "INDEX", "ON", "REFERENCES":
				switch token[0] {
				case '"':
					token = strings.ReplaceAll(token[1:len(token)-1], `""`, `"`)
				case '`', '[':
					token = token[1 : len(token)-1]
				}
			}
		}
		tokens[i] = strings.ToUpper(token)
	}
	return strings.Join(tokens, "\x00")
}
