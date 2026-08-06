package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const defaultAgentExecutionOwnershipProjectionInterval = 10 * time.Second

type agentExecutionEpochSource interface {
	CurrentFence(context.Context) (store.ControllerEpochFence, error)
}

// AgentExecutionOwnershipStatusProjector publishes the exact externally held
// ownership fence set into the singleton AgentExecutionControl status. It is a
// leader-only runnable and readiness stays closed until projection succeeds.
type AgentExecutionOwnershipStatusProjector struct {
	Client    client.Client
	APIReader client.Reader
	Ownership *AgentExecutionOwnershipLock
	Epochs    agentExecutionEpochSource
	Interval  time.Duration

	mu      sync.RWMutex
	ready   bool
	lastErr error
}

func (p *AgentExecutionOwnershipStatusProjector) NeedLeaderElection() bool { return true }

func (p *AgentExecutionOwnershipStatusProjector) Start(ctx context.Context) error {
	if p == nil || p.Client == nil || p.APIReader == nil || p.Ownership == nil || p.Epochs == nil {
		return fmt.Errorf("agent execution ownership status projector dependencies are required")
	}
	interval := p.Interval
	if interval <= 0 {
		interval = defaultAgentExecutionOwnershipProjectionInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := p.project(ctx); err != nil {
			p.setResult(err)
		} else {
			p.setResult(nil)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (p *AgentExecutionOwnershipStatusProjector) ReadyzChecker() func(*http.Request) error {
	return func(_ *http.Request) error {
		p.mu.RLock()
		defer p.mu.RUnlock()
		if p.ready {
			return nil
		}
		if p.lastErr != nil {
			return p.lastErr
		}
		return errors.New("agent execution ownership status has not been projected")
	}
}

func (p *AgentExecutionOwnershipStatusProjector) project(ctx context.Context) error {
	snapshot, ready := p.Ownership.Snapshot()
	if !ready {
		return errors.New("complete agent execution ownership fence set is unavailable")
	}
	fence, err := p.Epochs.CurrentFence(ctx)
	if err != nil {
		return fmt.Errorf("read controller ownership epoch: %w", err)
	}
	control := &corev1alpha1.AgentExecutionControl{}
	key := client.ObjectKey{
		Namespace: corev1alpha1.AgentExecutionControlNamespace,
		Name:      corev1alpha1.AgentExecutionControlName,
	}
	if err := p.APIReader.Get(ctx, key, control); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("required AgentExecutionControl %s/%s is missing", key.Namespace, key.Name)
		}
		return fmt.Errorf("read AgentExecutionControl ownership target: %w", err)
	}
	if control.UID == "" {
		return errors.New("AgentExecutionControl UID is missing")
	}

	legacy := make([]corev1alpha1.AgentExecutionLegacyLeaseFence, 0, len(snapshot.Legacy))
	for _, lease := range snapshot.Legacy {
		if lease.UID == "" || lease.ResourceVersion == "" {
			return fmt.Errorf("legacy ownership Lease %s/%s has incomplete Kubernetes identity", lease.Namespace, lease.Name)
		}
		legacy = append(legacy, corev1alpha1.AgentExecutionLegacyLeaseFence{
			Namespace: lease.Namespace, Name: lease.Name, UID: lease.UID, ResourceVersion: lease.ResourceVersion,
		})
	}
	desired := &corev1alpha1.AgentExecutionOwnershipStatus{
		LeaseNamespace:    snapshot.GlobalLease.Namespace,
		LeaseName:         snapshot.GlobalLease.Name,
		UID:               snapshot.GlobalLease.UID,
		ControllerEpoch:   fence.Epoch,
		LegacyLeaseFences: legacy,
	}
	if desired.LeaseNamespace != corev1alpha1.AgentExecutionControlNamespace ||
		desired.LeaseName != corev1alpha1.AgentExecutionOwnershipLeaseName || desired.UID == "" || desired.ControllerEpoch < 1 {
		return errors.New("global ownership Lease identity or controller epoch is incomplete")
	}
	if reflect.DeepEqual(control.Status.Ownership, desired) {
		return nil
	}
	base := control.DeepCopy()
	control.Status.Ownership = desired
	if err := p.Client.Status().Patch(ctx, control, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("project AgentExecutionControl ownership status: %w", err)
	}
	return nil
}

func (p *AgentExecutionOwnershipStatusProjector) setResult(err error) {
	p.mu.Lock()
	p.ready = err == nil
	p.lastErr = err
	p.mu.Unlock()
}
