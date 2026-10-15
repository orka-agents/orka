package sqlite

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGatewayMessageQuotaQueryPlansAreTaskScoped(t *testing.T) {
	s := setupTestStore(t)
	for name, query := range map[string]string{
		"admission": gatewayMessageCountSQL,
		"budget":    gatewayMessageBudgetSQL,
	} {
		t.Run(name, func(t *testing.T) {
			args := make([]any, strings.Count(query, "?"))
			for i := range args {
				args[i] = "scope"
			}
			rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
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
			require.Contains(t, plan, "idx_gateway_events_task_identity (namespace=? AND task_name=? AND task_uid=?)")
			require.Contains(t, plan, "idx_gateway_deliveries_event (namespace=? AND event_id=?)")
			require.Less(t, strings.Index(plan, "idx_gateway_events_task_identity"), strings.Index(plan, "idx_gateway_deliveries_event"))
		})
	}
}
