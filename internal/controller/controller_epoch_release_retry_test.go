package controller

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
	kubestore "github.com/orka-agents/orka/internal/store/kube"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

type lostReleaseResponse struct {
	*kubestore.Store
	releases atomic.Int32
}

func (s *lostReleaseResponse) ReleaseStorageHold(
	ctx context.Context, fence store.ControllerEpochFence, id string, acks []store.StorageAcknowledgement,
) error {
	if err := s.Store.ReleaseStorageHold(ctx, fence, id, acks); err != nil {
		return err
	}
	if s.releases.Add(1) == 1 {
		return fmt.Errorf("read the release response: %w", io.ErrUnexpectedEOF)
	}
	return nil
}

func TestReleaseWithALostResponseReopensTheFences(t *testing.T) {
	authority, kubeClient := newKubernetesControllerEpochAuthority(t)
	original := newRecordingEpochDataset("dataset-a")
	for _, holder := range []string{"controller-a", "controller-b"} {
		manager := fastEpochManager(authority, holder).WithStorage(storagev1.StateContract, original)
		cancel, done := startEpochManager(t, manager)
		if _, err := fenceWithTimeout(manager); err != nil {
			t.Fatal(err)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	restored := newRecordingEpochDataset("dataset-a")
	restored.requests = original.advanceRequests()[:1]
	records := &lostReleaseResponse{Store: authority}
	successor := fastEpochManager(records, "controller-c").WithStorage(storagev1.StateContract, restored)
	_, done := startEpochManager(t, successor)
	eventually(t, "a recorded hold", func() bool { return recordedHold(t, authority) != nil })
	acknowledgeHold(t, kubeClient, recordedHold(t, authority).ID)

	eventually(t, "the acknowledged hold to clear", func() bool { return successor.StorageHold() == nil })
	if _, err := fenceWithTimeout(successor); err != nil {
		t.Fatalf("fence after the release: %v", err)
	}
	require.GreaterOrEqual(t, records.releases.Load(), int32(2), "the release was retried after its lost response")
	require.Nil(t, recordedHold(t, authority))
	select {
	case err := <-done:
		t.Fatalf("the manager stopped: %v", err)
	default:
	}
}
