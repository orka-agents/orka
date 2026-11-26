package gateway

import (
	"context"
	"fmt"
	"testing"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// lostReceiptDeletionStore deletes a Gateway Task cleanup receipt and then
// reports the outcome unknown, as the storage client does when it cannot
// settle the write.
type lostReceiptDeletionStore struct {
	*sqlite.Store
	lose    int
	deletes int
}

func (s *lostReceiptDeletionStore) DeleteGatewayTaskCleanupReceipt(ctx context.Context, namespace, taskName, taskUID string) error {
	s.deletes++
	if err := s.Store.DeleteGatewayTaskCleanupReceipt(ctx, namespace, taskName, taskUID); err != nil {
		return err
	}
	if s.lose > 0 {
		s.lose--
		return fmt.Errorf("%w: receipt deletion response lost", store.ErrOutcomeUnknown)
	}
	return nil
}

// P7: a lost receipt deletion is reported, never treated as done, and the
// next pass converges: the receipt is gone and is not deleted again.
func TestGatewayTaskReceiptPruningConvergesAfterLostDeletion(t *testing.T) {
	ctx := t.Context()
	service, db, _ := newGatewayServiceFixture(t)
	service.Config.Namespace = "default"
	event := compactTaskReceiptForPruning(t, db, "default", "old-task", "task-uid")
	receipts := &lostReceiptDeletionStore{Store: db, lose: 1}
	service.EventStore = receipts
	service.APIReader = fake.NewClientBuilder().WithScheme(service.Client.Scheme()).Build()

	require.ErrorIs(t, service.pruneGatewayTaskCleanupReceipts(ctx), store.ErrOutcomeUnknown)
	_, err := db.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID)
	require.ErrorIs(t, err, store.ErrNotFound, "the lost deletion committed")

	require.NoError(t, service.pruneGatewayTaskCleanupReceipts(ctx))
	require.NoError(t, service.pruneGatewayTaskCleanupReceipts(ctx))
	require.Equal(t, 1, receipts.deletes, "a deleted receipt was deleted again")
	duplicate, err := db.GetGatewayEventDuplicate(ctx, &event, event.ExpiresAt)
	require.NoError(t, err, "receipt pruning must keep the event's replay protection")
	require.Equal(t, event.ID, duplicate.ID)
}
