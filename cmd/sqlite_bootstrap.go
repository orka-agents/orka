package main

import (
	"fmt"

	"github.com/orka-agents/orka/internal/store/sqlite"
)

// openControllerSQLiteStore is the production bootstrap seam: exclusivity,
// schema/identity validation, key loading, authentication of retained encrypted
// records and durable bootstrap completion all precede controller consumers.
func openControllerSQLiteStore(
	path string, loadKey func(sqlite.DatasetInfo) ([]byte, error),
) (*sqlite.Store, []byte, error) {
	s, err := sqlite.OpenLockedStore(path)
	if err != nil {
		return nil, nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = s.Close()
		}
	}()
	info, err := s.DatasetInfo()
	if err != nil {
		return nil, nil, err
	}
	key, err := loadKey(info)
	if err != nil {
		return nil, nil, fmt.Errorf("load SQLite dataset snapshot key: %w", err)
	}
	cipher, err := sqlite.NewAgentExecutionSnapshotCipher(key)
	if err != nil {
		return nil, nil, err
	}
	if err := s.SetAgentExecutionSnapshotCipher(cipher); err != nil {
		return nil, nil, err
	}
	success = true
	return s, key, nil
}
