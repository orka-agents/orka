package enforcementprovider

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Controller owns durable state and serializes startup, close, and readiness.
type Controller struct {
	mu           sync.RWMutex
	store        *FileStore
	orchestrator Orchestrator
	ready        bool
	closed       bool
}

func NewController(statePath string, provider Provider, trust TrustProfile, now func() time.Time) (*Controller, error) {
	if provider == nil {
		return nil, errors.New("enforcement provider is required")
	}
	if now == nil {
		now = time.Now
	}
	store, err := NewFileStore(statePath)
	if err != nil {
		return nil, err
	}
	return &Controller{store: store, orchestrator: Orchestrator{Store: store, Provider: provider, Trust: trust, Now: now}}, nil
}
func (c *Controller) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("enforcement controller is closed")
	}
	if c.ready {
		return nil
	}
	attempts, err := c.store.List()
	if err != nil {
		return fmt.Errorf("list enforcement recovery state: %w", err)
	}
	results := c.orchestrator.Recover(attempts)
	failures := make([]string, 0, len(results))
	for id, err := range results {
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", id, err))
		}
	}
	sort.Strings(failures)
	if len(failures) != 0 {
		return fmt.Errorf("enforcement recovery failed: %s", strings.Join(failures, "; "))
	}
	c.ready = true
	return nil
}
func (c *Controller) requireReadyLocked() error {
	if c.closed {
		return errors.New("enforcement controller is closed")
	}
	if !c.ready {
		return errors.New("enforcement controller is not ready")
	}
	return nil
}
func (c *Controller) Hold(a Authorization) (Attempt, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.requireReadyLocked(); err != nil {
		return Attempt{}, err
	}
	if _, err := c.orchestrator.Authorize(a); err != nil {
		return Attempt{}, err
	}
	return c.orchestrator.Activate(a.Context.AttemptID)
}
func (c *Controller) ConsumeProof(attemptID, proofID string) (Attempt, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.requireReadyLocked(); err != nil {
		return Attempt{}, err
	}
	return c.orchestrator.ConsumeProof(attemptID, proofID)
}
func (c *Controller) Release(attemptID string) (Attempt, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.requireReadyLocked(); err != nil {
		return Attempt{}, err
	}
	return c.orchestrator.Release(attemptID)
}
func (c *Controller) Cleanup(attemptID string) (Attempt, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.requireReadyLocked(); err != nil {
		return Attempt{}, err
	}
	return c.orchestrator.Cleanup(attemptID)
}
func (c *Controller) Attempt(id string) (Attempt, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.requireReadyLocked(); err != nil {
		return Attempt{}, err
	}
	return c.store.Get(id)
}
func (c *Controller) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.ready = false
	return c.store.Close()
}
