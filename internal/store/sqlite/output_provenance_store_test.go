package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/orka-agents/orka/internal/store"
)

const testSHA256DigestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func testOutputProvenance() store.OutputProvenance {
	return store.OutputProvenance{
		TaskUID:               "task-uid",
		JobUID:                "job-uid",
		PodUID:                "pod-uid",
		TaskAttempt:           1,
		ProducerKind:          store.OutputProducerKubernetesWorker,
		SubmissionNonceDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
}

func TestBoundResultAttemptCAS(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	first := &store.BoundResult{Namespace: "ns", TaskName: "task", Data: []byte("one"), Provenance: testOutputProvenance()}
	if err := s.SaveBoundResult(ctx, first); err != nil {
		t.Fatalf("SaveBoundResult(first) error = %v", err)
	}
	if first.Provenance.StagingGeneration != 1 || first.Provenance.ContentSHA256 == "" {
		t.Fatalf("first provenance = %#v", first.Provenance)
	}
	second := &store.BoundResult{Namespace: "ns", TaskName: "task", Data: []byte("two"), Provenance: testOutputProvenance()}
	if err := s.SaveBoundResult(ctx, second); err != nil {
		t.Fatalf("SaveBoundResult(second) error = %v", err)
	}
	if second.Provenance.StagingGeneration != 2 {
		t.Fatalf("second generation = %d", second.Provenance.StagingGeneration)
	}
	got, err := s.GetBoundResult(ctx, "ns", "task", "task-uid", 1)
	if err != nil || string(got.Data) != "two" || got.Provenance.StagingGeneration != 2 {
		t.Fatalf("GetBoundResult() = %#v, %v", got, err)
	}
	retryProvenance := testOutputProvenance()
	retryProvenance.TaskAttempt = 2
	retryProvenance.JobUID = "job-uid-2"
	retryProvenance.PodUID = "pod-uid-2"
	retryProvenance.SubmissionNonceDigest = testSHA256DigestB
	retry := &store.BoundResult{Namespace: "ns", TaskName: "task", Data: []byte("retry"), Provenance: retryProvenance}
	if err := s.SaveBoundResult(ctx, retry); err != nil {
		t.Fatalf("SaveBoundResult(retry) error = %v", err)
	}
	if retry.Provenance.StagingGeneration != 3 {
		t.Fatalf("retry generation = %d", retry.Provenance.StagingGeneration)
	}
	oldAttempt := &store.BoundResult{Namespace: "ns", TaskName: "task", Data: []byte("late"), Provenance: testOutputProvenance()}
	if err := s.SaveBoundResult(ctx, oldAttempt); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("old attempt replay error = %v", err)
	}

	wrong := &store.BoundResult{Namespace: "ns", TaskName: "task", Data: []byte("attack"), Provenance: testOutputProvenance()}
	wrong.Provenance.PodUID = "other-pod"
	if err := s.SaveBoundResult(ctx, wrong); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("wrong writer error = %v", err)
	}
	if _, err := s.GetBoundResult(ctx, "ns", "task", "other-task", 1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("wrong task read error = %v", err)
	}
}

func TestBoundArtifactAttemptCAS(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	artifact := &store.BoundArtifact{
		Namespace: "ns", TaskName: "task", Filename: "proof.txt", ContentType: "text/plain",
		Data: []byte("proof"), Provenance: testOutputProvenance(),
	}
	if err := s.SaveBoundArtifact(ctx, artifact); err != nil {
		t.Fatalf("SaveBoundArtifact() error = %v", err)
	}
	got, err := s.GetBoundArtifact(ctx, "ns", "task", "proof.txt", "task-uid", 1)
	if err != nil || string(got.Data) != "proof" || got.Provenance.StagingGeneration != 1 {
		t.Fatalf("GetBoundArtifact() = %#v, %v", got, err)
	}
	items, err := s.ListBoundArtifacts(ctx, "ns", "task", "task-uid", 1)
	if err != nil || len(items) != 1 || items[0].Filename != "proof.txt" {
		t.Fatalf("ListBoundArtifacts() = %#v, %v", items, err)
	}
}

func TestLegacyOutputIsNotUpgradedByAssertion(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	if err := s.SaveResult(ctx, "ns", "legacy", []byte("legacy")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBoundResult(ctx, "ns", "legacy", "task-uid", 1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("legacy bound read error = %v", err)
	}
}

func TestLegacyResultReplacementClearsVerifiedProvenance(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	bound := &store.BoundResult{Namespace: "ns", TaskName: "task", Data: []byte("trusted"), Provenance: testOutputProvenance()}
	if err := s.SaveBoundResult(ctx, bound); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveResult(ctx, "ns", "task", []byte("legacy replacement")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBoundResult(ctx, "ns", "task", "task-uid", 1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("legacy replacement retained verified provenance: %v", err)
	}
}
