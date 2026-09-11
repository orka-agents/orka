package sqlite

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// initializeSchema creates an empty database or checks the existing layout.
// Existing databases are never upgraded or rewritten during startup.
func initializeSchema(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := readSchema(tx)
	if err != nil {
		return err
	}
	statements := currentSchemaStatements()
	if len(existing) > 0 {
		return validateSchema(existing, statements)
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("create current schema: %w", err)
		}
	}
	return tx.Commit()
}

func readSchema(tx *sql.Tx) (map[string]string, error) {
	rows, err := tx.Query(`SELECT name, sql FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'`)
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
	for _, statement := range statements {
		// Every definition uses CREATE ... IF NOT EXISTS <name>. SQLite omits
		// IF NOT EXISTS when it stores the definition in sqlite_schema.
		prefix, definition, ok := strings.Cut(statement, "IF NOT EXISTS ")
		if !ok {
			return fmt.Errorf("invalid current schema definition")
		}
		name := strings.Fields(definition)[0]
		if normalizeSchemaSQL(existing[name]) != normalizeSchemaSQL(prefix+definition) {
			return fmt.Errorf("unsupported SQLite schema: %s is missing or incompatible; historical layouts are not upgraded", name)
		}
	}
	if len(existing) != len(statements) {
		return fmt.Errorf("unsupported SQLite schema: unexpected schema objects; historical layouts are not upgraded")
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
