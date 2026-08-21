package sqlite

import (
	"context"
	"testing"

	"github.com/orka-agents/orka/internal/store"
)

func TestSaveResultAdvancesLegacyStagingGeneration(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	if err := s.SaveResult(ctx, "ns", "legacy", []byte("one")); err != nil {
		t.Fatalf("SaveResult(first) error = %v", err)
	}
	assertResultStagingGeneration(t, s, "legacy", 1)

	if err := s.SaveResult(ctx, "ns", "legacy", []byte("two")); err != nil {
		t.Fatalf("SaveResult(second) error = %v", err)
	}
	assertResultStagingGeneration(t, s, "legacy", 2)
}

func TestSaveResultPreservesMonotonicGenerationWhenReplacingBoundResult(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	bound := &store.BoundResult{
		Namespace: "ns",
		TaskName:  "task",
		Data:      []byte("trusted"),
		Provenance: store.OutputProvenance{
			TaskUID:               "task-uid",
			JobUID:                "job-uid",
			PodUID:                "pod-uid",
			TaskAttempt:           1,
			ProducerKind:          store.OutputProducerKubernetesWorker,
			SubmissionNonceDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	if err := s.SaveBoundResult(ctx, bound); err != nil {
		t.Fatalf("SaveBoundResult() error = %v", err)
	}
	assertResultStagingGeneration(t, s, "task", 1)

	if err := s.SaveResult(ctx, "ns", "task", []byte("legacy replacement")); err != nil {
		t.Fatalf("SaveResult(first legacy replacement) error = %v", err)
	}
	assertResultStagingGeneration(t, s, "task", 2)

	if err := s.SaveResult(ctx, "ns", "task", []byte("second legacy replacement")); err != nil {
		t.Fatalf("SaveResult(second legacy replacement) error = %v", err)
	}
	assertResultStagingGeneration(t, s, "task", 3)

	var taskUID, producerKind string
	if err := s.db.QueryRow(`SELECT task_uid, producer_kind FROM results WHERE namespace = ? AND task_name = ?`, "ns", "task").Scan(&taskUID, &producerKind); err != nil {
		t.Fatalf("query legacy provenance error = %v", err)
	}
	if taskUID != "" || producerKind != string(store.OutputProducerLegacy) {
		t.Fatalf("legacy provenance = taskUID %q, producerKind %q", taskUID, producerKind)
	}
}

func assertResultStagingGeneration(t *testing.T, s *Store, taskName string, want int64) {
	t.Helper()
	var got int64
	if err := s.db.QueryRow(`SELECT staging_generation FROM results WHERE namespace = ? AND task_name = ?`, "ns", taskName).Scan(&got); err != nil {
		t.Fatalf("query staging generation error = %v", err)
	}
	if got != want {
		t.Fatalf("staging generation = %d, want %d", got, want)
	}
}
