package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// SchemaVersion records the metadata layout. It never replaces validation of
// every schema object; every upgrade, from an unversioned layout or from an
// earlier version, requires the locked migration path.
const SchemaVersion = 4

const datasetSchema = `CREATE TABLE IF NOT EXISTS store_dataset (
 singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
 dataset_id TEXT NOT NULL,
 origin TEXT NOT NULL CHECK(origin IN ('new', 'adopted')),
 bootstrap_complete INTEGER NOT NULL CHECK(bootstrap_complete IN (0, 1)),
 installation_id TEXT NOT NULL DEFAULT '',
 record_uid TEXT NOT NULL DEFAULT '',
 switch_generation INTEGER NOT NULL DEFAULT 0 CHECK(switch_generation >= 0),
 key_fingerprint TEXT NOT NULL DEFAULT '',
 binding_state TEXT NOT NULL DEFAULT '' CHECK(binding_state IN ('', 'bound', 'detached')),
 bound_at_ns INTEGER NOT NULL DEFAULT 0 CHECK(bound_at_ns >= 0)
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
		core, _, err := splitOptionalValidationSchema(existing)
		if err != nil {
			return err
		}
		if err := validateSchema(core, statements); err != nil {
			return err
		}
		if version != SchemaVersion {
			return fmt.Errorf("unsupported SQLite schema: dataset metadata requires schema version %d", SchemaVersion)
		}
		if _, err := readDataset(tx); err != nil {
			return err
		}
		_, err = readDatasetBinding(context.Background(), tx)
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

// Only complete known optional layouts, or the standalone initialization lock,
// are separated from core. Unknown objects remain in core and fail its exact
// validation/fingerprint checks; a prefix never exempts an object.
func splitOptionalValidationSchema(existing map[string]string) (core, optional map[string]string, err error) {
	var statements []string
	_, patchInitialized := existing["patch_verification_runs"]
	_, kubernetesInitialized := existing["validation_submissions"]
	if patchInitialized || kubernetesInitialized {
		statements = append(statements, patchVerificationWriteLockSchema)
		statements = append(statements, patchVerificationSchemaStatements()...)
	} else if _, locked := existing["patch_verification_write_lock"]; locked {
		statements = append(statements, patchVerificationWriteLockSchema)
	}
	if kubernetesInitialized {
		statements = append(statements, kubernetesValidationTableStatements()...)
		statements = append(statements, kubernetesValidationIndexStatements()...)
	}
	core, optional = maps.Clone(existing), make(map[string]string)
	for _, statement := range statements {
		name, _, err := schemaObjectDefinition(statement)
		if err != nil {
			return nil, nil, err
		}
		if definition, exists := core[name]; exists {
			optional[name] = definition
			delete(core, name)
		}
	}
	if err := validateSchema(optional, statements); err != nil {
		return nil, nil, err
	}
	return core, optional, nil
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
		name, definition, err := schemaObjectDefinition(statement)
		if err != nil {
			return err
		}
		if normalizeSchemaSQL(existing[name]) != normalizeSchemaSQL(definition) {
			return fmt.Errorf("unsupported SQLite schema: %s is missing or incompatible; only exact documented layouts can be upgraded through OpenLockedStore", name)
		}
	}
	if len(existing) != len(statements) {
		return fmt.Errorf("unsupported SQLite schema: unexpected schema objects; only exact documented layouts can be upgraded through OpenLockedStore")
	}
	return nil
}

func schemaObjectDefinition(statement string) (name, definition string, err error) {
	// Every definition uses CREATE ... IF NOT EXISTS <name>. SQLite omits
	// IF NOT EXISTS when it stores the definition in sqlite_schema.
	prefix, definition, ok := strings.Cut(statement, "IF NOT EXISTS ")
	fields := strings.Fields(definition)
	if !ok || len(fields) == 0 {
		return "", "", fmt.Errorf("invalid current schema definition")
	}
	return strings.ToLower(fields[0]), prefix + definition, nil
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
