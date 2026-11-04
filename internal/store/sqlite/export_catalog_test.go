package sqlite

import (
	"strings"
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// exportExcludedTables are the tables that hold no export family, each with
// the reason.
var exportExcludedTables = map[string]string{
	"store_dataset":               "dataset identity and binding; an import mints its own",
	"controller_epochs":           "controller epoch mirror; the next takeover advances the imported dataset",
	"operation_receipts":          "operation receipts; writes are stopped and the imported dataset has a new ID",
	"sqlite_sequence":             "AUTOINCREMENT heads; exported as sequence_heads",
	"prompt_attempts":             "duplicate SQL control record",
	"prompt_attempt_reclamations": "duplicate SQL control record",
	"session_controls":            "duplicate SQL control record",
	"branch_claims":               "duplicate SQL control record",
	"publications":                "duplicate SQL control record",
	"external_effects":            "duplicate SQL control record",
}

// exportIntegerTimes are INTEGER columns that hold Unix nanoseconds, which
// the export carries as times.
var exportIntegerTimes = map[string]bool{
	"usage_observations.observed_at":  true,
	"usage_pull_requests.observed_at": true,
	"usage_tasks.started_at":          true,
	"usage_work_requests.started_at":  true,
	"usage_retention.retained_since":  true,
}

// TestExportFamiliesCoverEverySQLiteTable pins the export families to the
// embedded store's layout: every table is a family with the same columns,
// kinds, nullability and primary key, or is excluded with a reason, so a new
// table or column cannot be left out of a migration unnoticed.
func TestExportFamiliesCoverEverySQLiteTable(t *testing.T) {
	s := setupDiskStore(t)
	tables := map[string]bool{}
	rows, err := s.db.QueryContext(t.Context(), `SELECT name FROM sqlite_schema WHERE type = 'table'`)
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		tables[name] = true
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())

	for table, reason := range exportExcludedTables {
		require.True(t, tables[table], "excluded table %s exists", table)
		require.NotEmpty(t, reason)
		_, exported := storagev1.LookupExportSchema(table)
		require.False(t, exported, "excluded table %s is not exported", table)
	}
	for _, schema := range storagev1.ExportSchemas() {
		if schema.Name == "sequence_heads" {
			continue
		}
		require.True(t, tables[schema.Name], "family %s is a table", schema.Name)
		delete(tables, schema.Name)
		type column struct {
			kind     storagev1.ExportFieldKind
			nullable bool
		}
		columns := map[string]column{}
		var key []string
		pragma, err := s.db.QueryContext(t.Context(), `SELECT name, type, "notnull", pk FROM pragma_table_info(?) ORDER BY cid`, schema.Name)
		require.NoError(t, err)
		keyOrder := map[int]string{}
		for pragma.Next() {
			var name, declared string
			var notNull bool
			var pk int
			require.NoError(t, pragma.Scan(&name, &declared, &notNull, &pk))
			kind := map[string]storagev1.ExportFieldKind{
				"TEXT": storagev1.ExportString, "INTEGER": storagev1.ExportInt, "BOOLEAN": storagev1.ExportBool,
				"TIMESTAMP": storagev1.ExportTime, "BLOB": storagev1.ExportBytes,
			}[strings.ToUpper(declared)]
			if exportIntegerTimes[schema.Name+"."+name] {
				kind = storagev1.ExportTime
			}
			require.NotZero(t, kind, "%s.%s has declared type %s", schema.Name, name, declared)
			columns[name] = column{kind: kind, nullable: !notNull && pk == 0}
			if pk > 0 {
				keyOrder[pk] = name
			}
		}
		require.NoError(t, pragma.Err())
		require.NoError(t, pragma.Close())
		for i := 1; i <= len(keyOrder); i++ {
			key = append(key, keyOrder[i])
		}
		var schemaKey []string
		for _, field := range schema.Fields {
			if field.Key {
				schemaKey = append(schemaKey, field.Name)
			}
		}
		require.ElementsMatch(t, key, schemaKey, "family %s keys on the table's primary key", schema.Name)
		require.Len(t, schema.Fields, len(columns), "family %s has a field for every column", schema.Name)
		for _, field := range schema.Fields {
			column, ok := columns[field.Name]
			require.True(t, ok, "%s.%s is a column", schema.Name, field.Name)
			require.Equal(t, column.kind, field.Kind, "%s.%s kind", schema.Name, field.Name)
			require.Equal(t, column.nullable, field.Nullable, "%s.%s nullability", schema.Name, field.Name)
		}
	}
	for table := range exportExcludedTables {
		delete(tables, table)
	}
	require.Empty(t, tables, "every table is an export family or excluded with a reason")
}
