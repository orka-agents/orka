package sqlite

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
)

func TestGitHubMutationReadinessLookupUsesIndex(t *testing.T) {
	s := setupTestStore(t)
	// Match the global operation/head lookup and ordering used by
	// ListGitHubMutationRecords for readiness ownership.
	query := githubMutationRecordSelectSQL() + ` WHERE 1 = 1 AND operation = ? AND target_sha = ? ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, "readiness_status", "head", 100, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	plan := strings.Join(details, "\n")
	t.Log(plan)
	require.Contains(t, plan, "idx_github_mutation_records_operation_sha (operation=? AND target_sha=?)")
	require.NotContains(t, plan, "SCAN github_mutation_records")
	require.NotContains(t, plan, "USE TEMP B-TREE")
}

func TestGitHubMutationReadinessLookupPreservesPagination(t *testing.T) {
	s := setupTestStore(t)
	createdAt := time.Now().UTC().Truncate(time.Second)
	for _, record := range []store.GitHubMutationRecord{
		{ID: "a", MonitorNamespace: "one", Operation: "readiness_status", TargetSHA: "head", CreatedAt: createdAt},
		{ID: "b", MonitorNamespace: "two", Operation: "readiness_status", TargetSHA: "head", CreatedAt: createdAt},
		{ID: "c", MonitorNamespace: "one", Operation: "readiness_status", TargetSHA: "head", CreatedAt: createdAt.Add(-time.Second)},
		{ID: "other-operation", MonitorNamespace: "one", Operation: "update_branch", TargetSHA: "head", CreatedAt: createdAt},
		{ID: "other-head", MonitorNamespace: "two", Operation: "readiness_status", TargetSHA: "old-head", CreatedAt: createdAt},
	} {
		record.MonitorName = "monitor"
		require.NoError(t, s.CreateGitHubMutationRecord(t.Context(), &record))
	}
	filter := store.GitHubMutationRecordFilter{AllNamespaces: true, Operation: "readiness_status", TargetSHA: "head", Limit: 2}
	first, cursor, err := s.ListGitHubMutationRecords(t.Context(), filter)
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.Equal(t, "b", first[0].ID)
	require.Equal(t, "a", first[1].ID)
	require.NotEmpty(t, cursor)
	filter.Cursor = cursor
	second, cursor, err := s.ListGitHubMutationRecords(t.Context(), filter)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Equal(t, "c", second[0].ID)
	require.Empty(t, cursor)
}

func TestNewDBRequiresCurrentGitHubMutationReadinessIndex(t *testing.T) {
	s := setupDiskStore(t)
	record := &store.GitHubMutationRecord{ID: "saved", MonitorNamespace: "one", MonitorName: "monitor", Operation: "readiness_status", TargetSHA: "head", CreatedAt: time.Now().UTC().Truncate(time.Second)}
	require.NoError(t, s.CreateGitHubMutationRecord(t.Context(), record))
	require.NoError(t, s.db.Close())

	reopened, err := NewDB(s.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	_, err = reopened.Exec(`DROP INDEX idx_github_mutation_records_operation_sha`)
	require.NoError(t, err)
	before := storedSchemaSQL(t, reopened)
	require.NoError(t, reopened.Close())

	rejected, err := NewDB(s.dbPath)
	require.ErrorContains(t, err, "idx_github_mutation_records_operation_sha is missing or incompatible")
	require.Nil(t, rejected)
	// The existing strict schema policy rejects the old layout without migration.
	raw, err := sql.Open("sqlite", s.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	require.Equal(t, before, storedSchemaSQL(t, raw))
	got, err := NewStore(raw, s.dbPath).GetGitHubMutationRecord(t.Context(), "one", "saved")
	require.NoError(t, err)
	require.Equal(t, record, got)
}
