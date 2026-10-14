package enforcementprovider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// FileStore is a crash-safe local Store implementation. Controllers may use
// the same Store interface with another transactional database adapter.
type FileStore struct {
	mu       sync.Mutex
	path     string
	attempts map[string]Attempt
}
type fileStoreDocument struct {
	Version  int                `json:"version"`
	Attempts map[string]Attempt `json:"attempts"`
}

func NewFileStore(path string) (*FileStore, error) {
	s := &FileStore{path: path, attempts: map[string]Attempt{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *FileStore) load() error {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read enforcement store: %w", err)
	}
	var d fileStoreDocument
	if err = json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("decode enforcement store: %w", err)
	}
	if d.Version != 1 || d.Attempts == nil {
		return errors.New("unsupported enforcement store")
	}
	s.attempts = d.Attempts
	return nil
}
func (s *FileStore) persist() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(fileStoreDocument{Version: 1, Attempts: s.attempts})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, s.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *FileStore) Create(c Context) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.attempts[c.AttemptID]; ok {
		if !equalContext(old.Context, c) {
			return Attempt{}, errors.New("attempt identity conflict")
		}
		return old, nil
	}
	a := Attempt{Context: c, Phase: PhaseRequested, Version: 1}
	s.attempts[c.AttemptID] = a
	if err := s.persist(); err != nil {
		delete(s.attempts, c.AttemptID)
		return Attempt{}, err
	}
	return a, nil
}
func (s *FileStore) Get(id string) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[id]
	if !ok {
		return a, errors.New("attempt not found")
	}
	return a, nil
}
func (s *FileStore) CompareAndSwap(id string, v uint64, fn func(*Attempt) error) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[id]
	if !ok {
		return a, errors.New("attempt not found")
	}
	if a.Version != v {
		return a, errors.New("attempt version conflict")
	}
	if err := fn(&a); err != nil {
		return Attempt{}, err
	}
	a.Version++
	old := s.attempts[id]
	s.attempts[id] = a
	if err := s.persist(); err != nil {
		s.attempts[id] = old
		return Attempt{}, err
	}
	return a, nil
}

// List returns a stable snapshot for controller startup recovery.
func (s *FileStore) List() []Attempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Attempt, 0, len(s.attempts))
	for _, a := range s.attempts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Context.AttemptID < out[j].Context.AttemptID })
	return out
}
