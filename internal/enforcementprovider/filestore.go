package enforcementprovider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const (
	fileStoreVersion = 4
	maxStoreBytes    = 16 << 20
)

var (
	ErrStoreClosed = errors.New("enforcement store is closed")
	openStoreMu    sync.Mutex
	openStorePaths = map[string]struct{}{}
)

// FileStore is a crash-safe, exclusively owned local Store implementation.
type FileStore struct {
	mu            sync.Mutex
	path          string
	lock          *os.File
	stateLocks    []*os.File
	attempts      map[string]Attempt
	syncDirectory func(string) error
	fatalErr      error
	closed        bool
}
type fileStoreDocument struct {
	Version   int                `json:"version"`
	StorePath string             `json:"storePath"`
	Attempts  map[string]Attempt `json:"attempts"`
}

func NewFileStore(path string) (*FileStore, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve enforcement store: %w", err)
	}
	absolute = filepath.Clean(absolute)
	directory := filepath.Dir(absolute)
	if err = os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create enforcement store directory: %w", err)
	}
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve enforcement store directory: %w", err)
	}
	absolute = filepath.Join(directory, filepath.Base(absolute))
	openStoreMu.Lock()
	if _, exists := openStorePaths[absolute]; exists {
		openStoreMu.Unlock()
		return nil, errors.New("enforcement store is already open in this process")
	}
	openStorePaths[absolute] = struct{}{}
	openStoreMu.Unlock()
	releaseRegistry := true
	defer func() {
		if releaseRegistry {
			openStoreMu.Lock()
			delete(openStorePaths, absolute)
			openStoreMu.Unlock()
		}
	}()
	lock, err := acquireStoreLock(absolute + ".lock")
	if err != nil {
		return nil, err
	}
	s := &FileStore{
		path:          absolute,
		lock:          lock,
		attempts:      map[string]Attempt{},
		syncDirectory: syncStoreDirectory,
	}
	stateLock, lockErr := acquireStoreStateLock(absolute)
	if lockErr != nil {
		_ = releaseStoreLock(lock)
		return nil, lockErr
	}
	if stateLock != nil {
		s.stateLocks = append(s.stateLocks, stateLock)
	}
	if err = s.load(stateLock); err != nil {
		_ = releaseStoreStateLocks(s.stateLocks)
		_ = releaseStoreLock(lock)
		return nil, err
	}
	releaseRegistry = false
	return s, nil
}

func strictDecodeStore(data []byte, out *fileStoreDocument) error {
	if err := rejectDuplicateJSONKeys(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing data in enforcement store")
	}
	return nil
}
func rejectDuplicateJSONKeys(decoder *json.Decoder) error {
	const maxJSONDepth = 64
	var walk func(int) error
	walk = func(depth int) error {
		if depth > maxJSONDepth {
			return errors.New("JSON nesting exceeds limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, keyErr := decoder.Token()
				if keyErr != nil {
					return keyErr
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("invalid JSON object key")
				}
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("duplicate JSON field %q", key)
				}
				seen[key] = struct{}{}
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing data in enforcement store")
	}
	return nil
}

func (s *FileStore) load(state *os.File) error {
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		if state != nil {
			return errors.New("enforcement store path disappeared while opening")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect enforcement store: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("enforcement store is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("enforcement store permissions are too broad")
	}
	if info.Size() > maxStoreBytes {
		return errors.New("enforcement store exceeds size limit")
	}
	if !storeStateFileHasSingleLink(info) {
		return errors.New("enforcement store must not have hard-link aliases")
	}
	if state == nil {
		return errors.New("enforcement store appeared while opening")
	}
	lockedInfo, err := state.Stat()
	if err != nil {
		return fmt.Errorf("inspect locked enforcement store: %w", err)
	}
	if !os.SameFile(info, lockedInfo) {
		return errors.New("enforcement store changed while opening")
	}
	data := make([]byte, lockedInfo.Size())
	if _, err = state.ReadAt(data, 0); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read enforcement store: %w", err)
	}
	var document fileStoreDocument
	if err = strictDecodeStore(data, &document); err != nil {
		return fmt.Errorf("decode enforcement store: %w", err)
	}
	if document.Version != fileStoreVersion || document.Attempts == nil {
		return errors.New("unsupported enforcement store")
	}
	if document.StorePath != s.path {
		return errors.New("enforcement store is bound to a different path")
	}
	if err = validateAttempts(document.Attempts); err != nil {
		return fmt.Errorf("validate enforcement store: %w", err)
	}
	s.attempts = cloneAttempts(document.Attempts)
	return nil
}
func (s *FileStore) persist() (bool, error) {
	if err := s.rejectLinkedCurrentState(); err != nil {
		return false, err
	}
	data, err := json.Marshal(fileStoreDocument{Version: fileStoreVersion, StorePath: s.path, Attempts: s.attempts})
	if err != nil {
		return false, err
	}
	if len(data) > maxStoreBytes {
		return false, errors.New("enforcement store exceeds size limit")
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), "."+filepath.Base(s.path)+".*")
	if err != nil {
		return false, err
	}
	name := file.Name()
	remove := true
	if err = lockStoreStateFile(file); err != nil {
		_ = file.Close()
		_ = os.Remove(name)
		return false, err
	}
	defer func() {
		if remove {
			_ = os.Remove(name)
		}
	}()
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		_ = file.Close()
		return false, err
	}
	if err = os.Rename(name, s.path); err != nil {
		_ = file.Close()
		return false, err
	}
	remove = false
	s.stateLocks = append(s.stateLocks, file)
	if err = s.pruneUnlinkedStateLocks(); err != nil {
		return true, err
	}
	if err = s.syncDirectory(filepath.Dir(s.path)); err != nil {
		return true, err
	}
	return true, nil
}
func syncStoreDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	err = directory.Sync()
	if closeErr := directory.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (s *FileStore) rejectLinkedCurrentState() error {
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		if len(s.stateLocks) != 0 {
			return errors.New("enforcement store path disappeared while open")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect enforcement store before persistence: %w", err)
	}
	if len(s.stateLocks) == 0 {
		return errors.New("enforcement store appeared while open")
	}
	current, err := s.stateLocks[len(s.stateLocks)-1].Stat()
	if err != nil {
		return fmt.Errorf("inspect locked enforcement store before persistence: %w", err)
	}
	if !os.SameFile(info, current) {
		return errors.New("enforcement store changed while open")
	}
	if !storeStateFileHasSingleLink(info) {
		return errors.New("enforcement store acquired a hard-link alias while open")
	}
	return nil
}

func (s *FileStore) pruneUnlinkedStateLocks() error {
	kept := make([]*os.File, 0, len(s.stateLocks))
	for index, file := range s.stateLocks {
		info, err := file.Stat()
		if err != nil {
			s.stateLocks = append(kept, s.stateLocks[index:]...)
			return fmt.Errorf("inspect locked enforcement store inode: %w", err)
		}
		if storeStateFileUnlinked(info) {
			if err = releaseStoreStateLock(file); err != nil {
				s.stateLocks = append(kept, s.stateLocks[index:]...)
				return err
			}
			continue
		}
		kept = append(kept, file)
	}
	s.stateLocks = kept
	return nil
}

func (s *FileStore) checkOpen() error {
	if s.closed {
		return ErrStoreClosed
	}
	if s.fatalErr != nil {
		return s.fatalErr
	}
	return nil
}

func (s *FileStore) recordPersistenceError(committed bool, err error) error {
	if !committed {
		return err
	}
	s.fatalErr = fmt.Errorf("enforcement store durability is uncertain; close and reopen: %w", err)
	return s.fatalErr
}
func (s *FileStore) Create(context Context) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return Attempt{}, err
	}
	context = cloneContext(context)
	if old, ok := s.attempts[context.AttemptID]; ok {
		if !equalContext(old.Context, context) {
			return Attempt{}, errors.New("attempt identity conflict")
		}
		return cloneAttempt(old), nil
	}
	attempt := Attempt{Context: context, Phase: PhaseRequested, Version: 1}
	s.attempts[context.AttemptID] = cloneAttempt(attempt)
	if committed, err := s.persist(); err != nil {
		if !committed {
			delete(s.attempts, context.AttemptID)
		}
		return cloneAttempt(attempt), s.recordPersistenceError(committed, err)
	}
	return cloneAttempt(attempt), nil
}
func (s *FileStore) Get(id string) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return Attempt{}, err
	}
	attempt, ok := s.attempts[id]
	if !ok {
		return Attempt{}, ErrAttemptNotFound
	}
	return cloneAttempt(attempt), nil
}
func (s *FileStore) CompareAndSwap(id string, version uint64, mutate func(*Attempt) error) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return Attempt{}, err
	}
	stored, ok := s.attempts[id]
	if !ok {
		return Attempt{}, ErrAttemptNotFound
	}
	if stored.Version != version {
		return Attempt{}, errors.New("attempt version conflict")
	}
	candidate := cloneAttempt(stored)
	if err := mutate(&candidate); err != nil {
		return Attempt{}, err
	}
	candidate = cloneAttempt(candidate)
	candidate.Version++
	prospective := cloneAttempts(s.attempts)
	prospective[id] = candidate
	if err := validateAttempts(prospective); err != nil {
		return Attempt{}, err
	}
	s.attempts[id] = candidate
	if committed, err := s.persist(); err != nil {
		if !committed {
			s.attempts[id] = stored
		}
		return cloneAttempt(candidate), s.recordPersistenceError(committed, err)
	}
	return cloneAttempt(candidate), nil
}
func (s *FileStore) List() []Attempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.fatalErr != nil {
		return nil
	}
	out := make([]Attempt, 0, len(s.attempts))
	for _, attempt := range s.attempts {
		out = append(out, cloneAttempt(attempt))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Context.AttemptID < out[j].Context.AttemptID })
	return out
}
func (s *FileStore) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	lock, stateLocks, path := s.lock, s.stateLocks, s.path
	s.lock = nil
	s.stateLocks = nil
	s.mu.Unlock()
	err := releaseStoreStateLocks(stateLocks)
	if lockErr := releaseStoreLock(lock); err == nil {
		err = lockErr
	}
	openStoreMu.Lock()
	delete(openStorePaths, path)
	openStoreMu.Unlock()
	return err
}

func cloneAttempts(input map[string]Attempt) map[string]Attempt {
	out := make(map[string]Attempt, len(input))
	for id, attempt := range input {
		out[id] = cloneAttempt(attempt)
	}
	return out
}
