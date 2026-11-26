package sqlite

import storagev1 "github.com/orka-agents/orka/pkg/storage/v1"

// SetDatasetStateForTest makes s report state and refuse new writes unless
// state is ready, as a dataset that is being migrated does. SQLite migrates
// its schema only while it opens, so tests use this to hold the dataset in
// another state.
func SetDatasetStateForTest(s *Store, state storagev1.DatasetState) {
	if state == storagev1.DatasetReady {
		s.writeState = nil
		return
	}
	s.writeState = func() storagev1.DatasetState { return state }
}

// DatabasePathForTest returns the file s opened, so a test can open it again.
func DatabasePathForTest(s *Store) string {
	return s.dbPath
}
