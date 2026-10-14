package enforcementprovider

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Controller is the production integration seam for a provider transaction.
// It owns the durable store, runs startup recovery before becoming ready, and
// exposes phase-specific methods so callers cannot collapse proof consumption
// and external workload release into an in-memory callback.
type Controller struct {
	store        *FileStore
	orchestrator Orchestrator
	ready        bool
}

// NewController opens durable state and binds a provider plus
// operator-controlled trust. It performs no provider operation until Start.
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

// Start recovers every durable attempt. Any unresolved attempt prevents
// readiness; callers must continue holding affected workloads.
func (c *Controller) Start() error {
	results := c.orchestrator.Recover(c.store.List())
	var failures []string
	for id, err := range results {
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", id, err))
		}
	}
	if len(failures) != 0 {
		c.ready = false
		return fmt.Errorf("enforcement recovery failed: %s", strings.Join(failures, "; "))
	}
	c.ready = true
	return nil
}
func (c *Controller) requireReady() error {
	if !c.ready {
		return errors.New("enforcement controller is not ready")
	}
	return nil
}

// Hold authorizes and activates enforcement while the external workload stays held.
func (c *Controller) Hold(a Authorization) (Attempt, error) {
	if err := c.requireReady(); err != nil {
		return Attempt{}, err
	}
	if _, err := c.orchestrator.Authorize(a); err != nil {
		return Attempt{}, err
	}
	return c.orchestrator.Activate(a.Context.AttemptID)
}

// ConsumeProof durably records verified proof and the exact release obligation.
func (c *Controller) ConsumeProof(attemptID, proofID string) (Attempt, error) {
	if err := c.requireReady(); err != nil {
		return Attempt{}, err
	}
	return c.orchestrator.ConsumeProof(attemptID, proofID)
}

// Release executes only a previously durable exact release obligation.
func (c *Controller) Release(attemptID string) (Attempt, error) {
	if err := c.requireReady(); err != nil {
		return Attempt{}, err
	}
	return c.orchestrator.Release(attemptID)
}

func (c *Controller) Attempt(attemptID string) (Attempt, error) { return c.store.Get(attemptID) }
