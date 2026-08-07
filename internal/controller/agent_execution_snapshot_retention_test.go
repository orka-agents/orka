package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestAgentExecutionSnapshotRetentionRequiresTwoReferenceFreeWindows(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	item := snapshotRetentionMetadata("task-uid", "snapshot", now.Add(-2*time.Hour))
	lifecycle := newSnapshotRetentionLifecycleStore(item)
	manager := &AgentExecutionSnapshotRetentionManager{
		APIReader: emptySnapshotRetentionClient(t),
		Store:     lifecycle, Retention: time.Hour, Now: func() time.Time { return now },
	}

	require.NoError(t, manager.collectOnce(ctx))
	require.Empty(t, lifecycle.deleted)

	now = now.Add(59 * time.Minute)
	require.NoError(t, manager.collectOnce(ctx))
	require.Empty(t, lifecycle.deleted)

	now = now.Add(time.Minute)
	require.NoError(t, manager.collectOnce(ctx))
	require.Equal(t, []store.AgentExecutionSnapshotKey{item.Key}, lifecycle.deleted)
}

func TestAgentExecutionSnapshotRetentionHonorsEveryKubernetesReference(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	taskItem := snapshotRetentionMetadata("task-reference", "task", now.Add(-3*time.Hour))
	adjudicationItem := snapshotRetentionMetadata("adjudication-reference", "adjudication", now.Add(-3*time.Hour))
	leaseItem := snapshotRetentionMetadata("lease-reference", "lease", now.Add(-3*time.Hour))
	lifecycle := newSnapshotRetentionLifecycleStore(taskItem, adjudicationItem, leaseItem)

	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: "retained-task", UID: types.UID(taskItem.Key.TaskUID),
	}}
	adjudication := &corev1alpha1.AgentExecutionAdjudication{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "retained-adjudication"},
		Spec: corev1alpha1.AgentExecutionAdjudicationSpec{
			TaskRef: corev1alpha1.AgentExecutionSubjectReference{UID: types.UID(adjudicationItem.Key.TaskUID)},
		},
	}
	session := &corev1alpha1.RuntimeSessionControl{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "retained-session"},
		Status: corev1alpha1.RuntimeSessionControlStatus{
			MutationLease: &corev1alpha1.RuntimeSessionMutationLeaseStatus{TaskUID: leaseItem.Key.TaskUID},
		},
	}
	manager := &AgentExecutionSnapshotRetentionManager{
		APIReader: snapshotRetentionClient(t, task, adjudication, session),
		Store:     lifecycle, Retention: time.Hour, Now: func() time.Time { return now },
	}

	require.NoError(t, manager.collectOnce(ctx))
	now = now.Add(2 * time.Hour)
	require.NoError(t, manager.collectOnce(ctx))
	require.Empty(t, lifecycle.deleted)
	require.Empty(t, manager.unreferencedSince)
}

func TestAgentExecutionSnapshotRetentionDoesNotTreatLineageConfigAsSnapshot(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	item := snapshotRetentionMetadata("lineage-task", "snapshot", now.Add(-3*time.Hour))
	lifecycle := newSnapshotRetentionLifecycleStore(item)
	session := &corev1alpha1.RuntimeSessionControl{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "unrelated-lineage"},
		Status: corev1alpha1.RuntimeSessionControlStatus{
			Lineage: &corev1alpha1.RuntimeSessionLineageStatus{ConfigDigest: item.Key.Digest},
		},
	}
	manager := &AgentExecutionSnapshotRetentionManager{
		APIReader: snapshotRetentionClient(t, session),
		Store:     lifecycle, Retention: time.Hour, Now: func() time.Time { return now },
	}

	require.NoError(t, manager.collectOnce(ctx))
	now = now.Add(time.Hour)
	require.NoError(t, manager.collectOnce(ctx))
	require.Equal(t, []store.AgentExecutionSnapshotKey{item.Key}, lifecycle.deleted)
}

func TestAgentExecutionSnapshotRetentionDurableReferenceResetsClock(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	item := snapshotRetentionMetadata("task-uid", "durable", now.Add(-3*time.Hour))
	lifecycle := newSnapshotRetentionLifecycleStore(item)
	manager := &AgentExecutionSnapshotRetentionManager{
		APIReader: emptySnapshotRetentionClient(t),
		Store:     lifecycle, Retention: time.Hour, Now: func() time.Time { return now },
	}

	require.NoError(t, manager.collectOnce(ctx))
	now = now.Add(time.Hour)
	lifecycle.references[item.Key.ID()] = store.AgentExecutionSnapshotReferenceCounts{SessionTurns: 1}
	require.NoError(t, manager.collectOnce(ctx))
	require.Empty(t, manager.unreferencedSince)

	now = now.Add(time.Hour)
	delete(lifecycle.references, item.Key.ID())
	require.NoError(t, manager.collectOnce(ctx))
	require.Empty(t, lifecycle.deleted)

	now = now.Add(time.Hour)
	require.NoError(t, manager.collectOnce(ctx))
	require.Equal(t, []store.AgentExecutionSnapshotKey{item.Key}, lifecycle.deleted)
}

func snapshotRetentionMetadata(taskUID, seed string, createdAt time.Time) store.AgentExecutionSnapshotMetadata {
	return store.AgentExecutionSnapshotMetadata{
		Key: store.AgentExecutionSnapshotKey{
			TaskUID: taskUID,
			Digest:  store.CanonicalAgentExecutionSnapshotDigest([]byte(seed)),
		},
		SchemaVersion: store.AgentExecutionSnapshotSchemaVersion,
		CreatedAt:     createdAt,
	}
}

func emptySnapshotRetentionClient(t *testing.T) client.Client {
	return snapshotRetentionClient(t)
}

func snapshotRetentionClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

type snapshotRetentionLifecycleStore struct {
	metadata   map[string]store.AgentExecutionSnapshotMetadata
	references map[string]store.AgentExecutionSnapshotReferenceCounts
	deleted    []store.AgentExecutionSnapshotKey
}

func newSnapshotRetentionLifecycleStore(items ...store.AgentExecutionSnapshotMetadata) *snapshotRetentionLifecycleStore {
	result := &snapshotRetentionLifecycleStore{
		metadata:   make(map[string]store.AgentExecutionSnapshotMetadata, len(items)),
		references: make(map[string]store.AgentExecutionSnapshotReferenceCounts),
	}
	for _, item := range items {
		result.metadata[item.Key.ID()] = item
	}
	return result
}

func (s *snapshotRetentionLifecycleStore) ListAgentExecutionSnapshotMetadataBefore(
	_ context.Context,
	cutoff time.Time,
) ([]store.AgentExecutionSnapshotMetadata, error) {
	result := make([]store.AgentExecutionSnapshotMetadata, 0, len(s.metadata))
	for _, item := range s.metadata {
		if item.CreatedAt.Before(cutoff) {
			result = append(result, item)
		}
	}
	return result, nil
}

func (s *snapshotRetentionLifecycleStore) CountAgentExecutionSnapshotReferences(
	_ context.Context,
	key store.AgentExecutionSnapshotKey,
) (store.AgentExecutionSnapshotReferenceCounts, error) {
	return s.references[key.ID()], nil
}

func (s *snapshotRetentionLifecycleStore) DeleteAgentExecutionSnapshot(
	_ context.Context,
	key store.AgentExecutionSnapshotKey,
) error {
	delete(s.metadata, key.ID())
	s.deleted = append(s.deleted, key)
	return nil
}
