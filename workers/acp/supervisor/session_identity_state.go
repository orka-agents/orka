package supervisor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/orka-agents/orka/internal/acp"
)

const (
	sessionIdentityStateFile       = ".orka-session-identity-high-water"
	sessionIdentityStateTempPrefix = ".orka-session-identity-state-"
	sessionIdentityLockFile        = ".orka-session-identity.lock"
	sessionIdentityStateVersion    = 1
)

type sessionIdentityState struct {
	Version   int `json:"version"`
	FirstUID  int `json:"firstUid"`
	LastUID   int `json:"lastUid"`
	FirstGID  int `json:"firstGid"`
	LastGID   int `json:"lastGid"`
	Allocated int `json:"allocated"`
}

func prepareSessionIdentityState(baseDir string, allocator *acp.UIDAllocator) (io.Closer, error) {
	baseDir = filepath.Clean(strings.TrimSpace(baseDir))
	if baseDir == "." || !filepath.IsAbs(baseDir) {
		return nil, fmt.Errorf("session base directory must be absolute")
	}
	info, err := os.Lstat(baseDir)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(baseDir, 0o711); err != nil {
			return nil, fmt.Errorf("create session base directory: %w", err)
		}
		info, err = os.Lstat(baseDir)
	}
	if err != nil {
		return nil, fmt.Errorf("inspect session base directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("session base directory must be a real directory")
	}

	lock, err := lockSessionIdentityState(baseDir)
	if err != nil {
		return nil, err
	}
	keepLock := false
	defer func() {
		if !keepLock {
			_ = lock.Close()
		}
	}()
	if err := removeOrphanedSessionIdentityTemps(baseDir); err != nil {
		return nil, err
	}

	firstUID, lastUID, firstGID, lastGID := allocator.Range()
	expected := sessionIdentityState{
		Version: sessionIdentityStateVersion, FirstUID: firstUID, LastUID: lastUID,
		FirstGID: firstGID, LastGID: lastGID,
	}
	statePath := filepath.Join(baseDir, sessionIdentityStateFile)
	state, exists, err := readSessionIdentityState(statePath)
	if err != nil {
		return nil, err
	}
	if !exists {
		entries, err := os.ReadDir(baseDir)
		if err != nil {
			return nil, fmt.Errorf("list session base directory: %w", err)
		}
		for _, entry := range entries {
			if entry.Name() != sessionIdentityLockFile {
				return nil, fmt.Errorf("session identity state is missing while stale session entries exist")
			}
		}
		state = expected
		if err := persistSessionIdentityState(baseDir, state); err != nil {
			return nil, err
		}
	} else if state.Version != expected.Version || state.FirstUID != expected.FirstUID || state.LastUID != expected.LastUID ||
		state.FirstGID != expected.FirstGID || state.LastGID != expected.LastGID {
		return nil, fmt.Errorf("session identity state does not match the configured UID/GID range")
	}
	if state.Allocated < 0 || state.Allocated > allocator.Capacity() {
		return nil, fmt.Errorf("session identity state allocation count is invalid")
	}
	if err := allocator.ConfigurePersistence(state.Allocated, func(next int) error {
		nextState := expected
		nextState.Allocated = next
		return persistSessionIdentityState(baseDir, nextState)
	}); err != nil {
		return nil, err
	}
	keepLock = true
	return lock, nil
}

func readSessionIdentityState(path string) (sessionIdentityState, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return sessionIdentityState{}, false, nil
	}
	if err != nil {
		return sessionIdentityState{}, false, fmt.Errorf("inspect session identity state: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 2 || info.Size() > 512 {
		return sessionIdentityState{}, false, fmt.Errorf("session identity state must be a bounded regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return sessionIdentityState{}, false, fmt.Errorf("read session identity state: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state sessionIdentityState
	if err := decoder.Decode(&state); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return sessionIdentityState{}, false, fmt.Errorf("session identity state is invalid")
	}
	canonical, err := json.Marshal(state)
	if err != nil || !bytes.Equal(bytes.TrimSpace(data), canonical) {
		return sessionIdentityState{}, false, fmt.Errorf("session identity state is non-canonical")
	}
	return state, true, nil
}

func persistSessionIdentityState(baseDir string, state sessionIdentityState) error {
	statePath := filepath.Join(baseDir, sessionIdentityStateFile)
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode session identity state: %w", err)
	}
	payload = append(payload, '\n')
	temporary, err := os.CreateTemp(baseDir, sessionIdentityStateTempPrefix+"*")
	if err != nil {
		return fmt.Errorf("create session identity state: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := true
	defer func() {
		_ = temporary.Close()
		if cleanup {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod session identity state: %w", err)
	}
	if _, err := temporary.Write(payload); err != nil {
		return fmt.Errorf("write session identity state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync session identity state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close session identity state: %w", err)
	}
	if err := os.Rename(temporaryPath, statePath); err != nil {
		return fmt.Errorf("publish session identity state: %w", err)
	}
	cleanup = false
	directory, err := os.Open(baseDir)
	if err != nil {
		return fmt.Errorf("open session base directory for sync: %w", err)
	}
	defer directory.Close() //nolint:errcheck
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync session base directory: %w", err)
	}
	return nil
}

func removeOrphanedSessionIdentityTemps(baseDir string) error {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return fmt.Errorf("list session identity artifacts: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), sessionIdentityStateTempPrefix) {
			continue
		}
		path := filepath.Join(baseDir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect orphaned session identity state: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("orphaned session identity state must be a regular file")
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove orphaned session identity state: %w", err)
		}
	}
	return nil
}

// durableCheckpointsWithoutIdentityState reports a durable workspace root that
// carries committed (or pending) session checkpoint markers while the
// allocator high-water state file is absent - the signature of a partial
// restore whose fresh allocator could reuse pre-suspension UIDs/GIDs.
func durableCheckpointsWithoutIdentityState(durableRoot, identityStateDir string) (bool, error) {
	if _, err := os.Lstat(filepath.Join(identityStateDir, sessionIdentityStateFile)); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect session identity state file: %w", err)
	}
	entries, err := os.ReadDir(durableRoot)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("list durable workspace root: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "ws-") && strings.HasSuffix(name, ".json") &&
			(strings.Contains(name, ".binding") || strings.HasSuffix(name, ".transition.json")) {
			return true, nil
		}
	}
	return false, nil
}
