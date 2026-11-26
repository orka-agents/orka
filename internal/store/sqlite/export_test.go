package sqlite

import storagev1 "github.com/orka-agents/orka/pkg/storage/v1"

func SetDatasetStateForTest(s *Store, state storagev1.DatasetState) {
	if state == storagev1.DatasetReady {
		s.writeState = nil
		return
	}
	s.writeState = func() storagev1.DatasetState { return state }
}

func DatabasePathForTest(s *Store) string {
	return s.dbPath
}
