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

// validateDurableCheckpointIdentityState binds surviving workspace history to
// the allocator high-water state. Missing state and state rolled back below a
// checkpoint's recorded floor both fail closed before a fresh child can reuse
// a pre-suspension UID/GID.
func validateDurableCheckpointIdentityState(durableRoot, identityStateDir string) error {
	state, stateExists, err := readSessionIdentityState(filepath.Join(identityStateDir, sessionIdentityStateFile))
	if err != nil {
		return fmt.Errorf("inspect session identity state file: %w", err)
	}
	entries, err := os.ReadDir(durableRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list durable workspace root: %w", err)
	}
	hasHistory := false
	checkpointHighWater := 0
	coveredWorkspaces := make(map[string]struct{})
	workspaceTrees := make(map[string]struct{})
	for _, entry := range entries {
		name := entry.Name()
		checkpointSessionUID, isCheckpoint := durableWorkspaceCheckpointSessionUID(name)
		if isCheckpoint {
			hasHistory = true
			if !stateExists {
				continue
			}
			if !acp.IsValidSessionPathComponent(checkpointSessionUID) {
				return fmt.Errorf("durable workspace checkpoint has an invalid session component")
			}
			highWater, err := durableWorkspaceCheckpointHighWater(filepath.Join(durableRoot, name))
			if err != nil {
				return err
			}
			checkpointHighWater = max(checkpointHighWater, highWater)
			coveredWorkspaces[checkpointSessionUID] = struct{}{}
			continue
		}
		sessionUID, ok := strings.CutPrefix(name, "ws-")
		if !ok || !acp.IsValidSessionPathComponent(sessionUID) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect durable workspace entry %q: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink == 0 && info.IsDir() {
			hasHistory = true
			workspaceTrees[sessionUID] = struct{}{}
		}
	}
	if hasHistory && !stateExists {
		return fmt.Errorf("the durable workspace volume carries committed session checkpoints but no session identity allocator state; refusing startup instead of risking UID/GID reuse")
	}
	for sessionUID := range workspaceTrees {
		if _, covered := coveredWorkspaces[sessionUID]; !covered {
			return fmt.Errorf(
				"durable workspace tree %q has no checkpoint identity high-water mark; refusing startup instead of risking UID/GID reuse",
				sessionUID,
			)
		}
	}
	if checkpointHighWater > state.Allocated {
		return fmt.Errorf(
			"durable workspace checkpoint requires session identity high-water %d but allocator state records %d; refusing startup instead of risking UID/GID reuse",
			checkpointHighWater,
			state.Allocated,
		)
	}
	return nil
}

func durableWorkspaceCheckpointSessionUID(name string) (string, bool) {
	if !strings.HasPrefix(name, "ws-") {
		return "", false
	}
	for _, suffix := range []string{".binding.json", ".binding.pending.json", ".transition.json"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(strings.TrimPrefix(name, "ws-"), suffix), true
		}
	}
	return "", false
}

func durableWorkspaceCheckpointHighWater(path string) (int, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, fmt.Errorf("inspect durable workspace checkpoint: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 2 || info.Size() > 4096 {
		return 0, fmt.Errorf("durable workspace checkpoint must be a bounded regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read durable workspace checkpoint: %w", err)
	}
	var binding acp.DurableWorkspaceBinding
	if err := json.Unmarshal(raw, &binding); err != nil {
		return 0, fmt.Errorf("decode durable workspace checkpoint: %w", err)
	}
	if binding.SessionIdentityHighWater <= 0 {
		return 0, fmt.Errorf("durable workspace checkpoint is missing its session identity high-water mark")
	}
	return binding.SessionIdentityHighWater, nil
}
