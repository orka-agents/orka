package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const (
	// DefaultAgentExecutionSnapshotRetentionInterval is the default cadence for
	// reference-aware encrypted snapshot collection.
	DefaultAgentExecutionSnapshotRetentionInterval = time.Hour
	// DefaultAgentExecutionSnapshotRetention is the minimum audit/backup
	// retention period before an unreferenced snapshot becomes collectible.
	DefaultAgentExecutionSnapshotRetention = 30 * 24 * time.Hour
)

// AgentExecutionSnapshotRetentionManager removes encrypted execution
// snapshots only after both Kubernetes and durable-store references have been
// absent for a complete retention window. The in-memory unreferenced clock is
// intentionally conservative: a controller restart starts the window again,
// delaying collection rather than risking premature deletion.
type AgentExecutionSnapshotRetentionManager struct {
	APIReader client.Reader
	Store     store.AgentExecutionSnapshotLifecycleStore

	Retention time.Duration
	Interval  time.Duration
	Now       func() time.Time

	unreferencedSince map[string]time.Time
}

// NeedLeaderElection keeps lifecycle decisions on the single elected writer.
func (*AgentExecutionSnapshotRetentionManager) NeedLeaderElection() bool { return true }

// Start runs one pass immediately and then periodically until cancellation.
func (m *AgentExecutionSnapshotRetentionManager) Start(ctx context.Context) error {
	if err := m.validate(); err != nil {
		return err
	}
	interval := m.Interval
	if interval <= 0 {
		interval = DefaultAgentExecutionSnapshotRetentionInterval
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	logger := log.FromContext(ctx).WithName("agent-execution-snapshot-retention")
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			if err := m.collectOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error(err, "snapshot retention pass failed closed")
			}
			timer.Reset(interval)
		}
	}
}

func (m *AgentExecutionSnapshotRetentionManager) validate() error {
	if m == nil || m.APIReader == nil || m.Store == nil {
		return errors.New("snapshot retention requires an uncached Kubernetes reader and lifecycle store")
	}
	if m.retention() <= 0 {
		return errors.New("snapshot retention duration must be positive")
	}
	return nil
}

func (m *AgentExecutionSnapshotRetentionManager) retention() time.Duration {
	if m.Retention > 0 {
		return m.Retention
	}
	return DefaultAgentExecutionSnapshotRetention
}

func (m *AgentExecutionSnapshotRetentionManager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *AgentExecutionSnapshotRetentionManager) collectOnce(ctx context.Context) error {
	if err := m.validate(); err != nil {
		return err
	}
	now := m.now()
	metadata, err := m.Store.ListAgentExecutionSnapshotMetadataBefore(ctx, now.Add(-m.retention()))
	if err != nil {
		return fmt.Errorf("list retained execution snapshots: %w", err)
	}
	references, err := m.loadKubernetesReferences(ctx)
	if err != nil {
		return err
	}
	if m.unreferencedSince == nil {
		m.unreferencedSince = make(map[string]time.Time)
	}
	seen := make(map[string]struct{}, len(metadata))
	for _, item := range metadata {
		if err := item.Validate(); err != nil {
			return fmt.Errorf("validate execution snapshot lifecycle metadata: %w", err)
		}
		id := item.Key.ID()
		seen[id] = struct{}{}
		if references.retains(item.Key) {
			delete(m.unreferencedSince, id)
			continue
		}
		counts, err := m.Store.CountAgentExecutionSnapshotReferences(ctx, item.Key)
		if err != nil {
			return fmt.Errorf("count durable execution snapshot references for %s: %w", id, err)
		}
		if counts.Total() != 0 {
			delete(m.unreferencedSince, id)
			continue
		}
		unreferencedAt, observed := m.unreferencedSince[id]
		if !observed {
			m.unreferencedSince[id] = now
			continue
		}
		if now.Sub(unreferencedAt) < m.retention() {
			continue
		}

		// Re-list all Kubernetes references and re-count durable references at
		// the deletion boundary. Any uncertainty aborts the pass without
		// deleting this or later candidates.
		latestReferences, err := m.loadKubernetesReferences(ctx)
		if err != nil {
			return err
		}
		if latestReferences.retains(item.Key) {
			delete(m.unreferencedSince, id)
			continue
		}
		counts, err = m.Store.CountAgentExecutionSnapshotReferences(ctx, item.Key)
		if err != nil {
			return fmt.Errorf("recount durable execution snapshot references for %s: %w", id, err)
		}
		if counts.Total() != 0 {
			delete(m.unreferencedSince, id)
			continue
		}
		if err := m.Store.DeleteAgentExecutionSnapshot(ctx, item.Key); err != nil {
			return fmt.Errorf("delete unreferenced execution snapshot %s: %w", id, err)
		}
		delete(m.unreferencedSince, id)
		log.FromContext(ctx).Info("deleted expired unreferenced execution snapshot",
			"taskUID", item.Key.TaskUID,
			"snapshotDigest", abbreviatedSnapshotDigest(item.Key.Digest),
			"schemaVersion", item.SchemaVersion,
		)
	}
	for id := range m.unreferencedSince {
		if _, ok := seen[id]; !ok {
			delete(m.unreferencedSince, id)
		}
	}
	return nil
}

type agentExecutionSnapshotKubernetesReferences struct {
	taskUIDs             map[string]struct{}
	sessionLeaseTaskUIDs map[string]struct{}
}

func (m *AgentExecutionSnapshotRetentionManager) loadKubernetesReferences(
	ctx context.Context,
) (agentExecutionSnapshotKubernetesReferences, error) {
	result := agentExecutionSnapshotKubernetesReferences{
		taskUIDs:             make(map[string]struct{}),
		sessionLeaseTaskUIDs: make(map[string]struct{}),
	}
	tasks := &corev1alpha1.TaskList{}
	if err := m.APIReader.List(ctx, tasks); err != nil {
		return result, fmt.Errorf("list Tasks for snapshot retention: %w", err)
	}
	for i := range tasks.Items {
		if uid := strings.TrimSpace(string(tasks.Items[i].UID)); uid != "" {
			result.taskUIDs[uid] = struct{}{}
		}
	}
	sessions := &corev1alpha1.RuntimeSessionControlList{}
	if err := m.APIReader.List(ctx, sessions); err != nil {
		return result, fmt.Errorf("list RuntimeSessionControls for snapshot retention: %w", err)
	}
	for i := range sessions.Items {
		session := &sessions.Items[i]
		if session.Status.MutationLease != nil {
			if taskUID := strings.TrimSpace(session.Status.MutationLease.TaskUID); taskUID != "" {
				result.sessionLeaseTaskUIDs[taskUID] = struct{}{}
			}
		}
	}
	return result, nil
}

func (r agentExecutionSnapshotKubernetesReferences) retains(key store.AgentExecutionSnapshotKey) bool {
	if _, ok := r.taskUIDs[key.TaskUID]; ok {
		return true
	}
	_, ok := r.sessionLeaseTaskUIDs[key.TaskUID]
	return ok
}

func abbreviatedSnapshotDigest(digest string) string {
	const prefixLength = len("sha256:") + 12
	if len(digest) <= prefixLength {
		return digest
	}
	return digest[:prefixLength]
}
