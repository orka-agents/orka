package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/orka-agents/orka/internal/store"
)

// recordEventUsage records the usage observation Orka derived from a newly
// appended execution event, in the event's transaction.
func recordEventUsage(ctx context.Context, db taskDataExecutor, event store.ExecutionEvent) error {
	if event.Usage == nil {
		return nil
	}
	observation := event.Usage.Observation
	if event.Usage.KeepFirstObservedAt {
		var existing store.UsageObservation
		err := readUsageJSON(ctx, db, &existing, `SELECT data FROM usage_observations WHERE namespace = ? AND id = ?`, observation.Namespace, observation.ID)
		if err == nil {
			observation.ObservedAt = existing.ObservedAt
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return recordUsage(ctx, db, observation)
}
