package main

import (
	"context"
	"fmt"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// openControllerSQLiteStore is the production bootstrap seam: exclusivity,
// schema/identity validation, key loading, authentication of retained sealed
// records and durable bootstrap completion all precede controller consumers.
// The storage client owns the snapshot key: it authenticates every retained
// sealed record before bootstrap completes, and SQLite only ever stores
// sealed bytes.
func openControllerSQLiteStore(
	path string, loadKey func(sqlite.DatasetInfo) ([]byte, error),
) (*sqlite.Store, *storageclient.Client, []byte, error) {
	s, err := sqlite.OpenLockedStore(path)
	if err != nil {
		return nil, nil, nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = s.Close()
		}
	}()
	info, err := s.DatasetInfo()
	if err != nil {
		return nil, nil, nil, err
	}
	key, err := loadKey(info)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load SQLite dataset snapshot key: %w", err)
	}
	client, err := storageclient.NewEmbedded(s)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := client.ActivateSnapshotKey(context.Background(), key); err != nil {
		return nil, nil, nil, err
	}
	if err := s.CompleteDatasetBootstrap(); err != nil {
		return nil, nil, nil, fmt.Errorf("complete SQLite dataset bootstrap: %w", err)
	}
	success = true
	return s, client, key, nil
}
