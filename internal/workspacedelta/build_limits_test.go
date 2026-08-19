package workspacedelta

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBuildWithLimitsContextHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "file.txt", "before\n", 0o644)
	baseline := captureTestBaseline(t, root, Options{})
	writeTestFile(t, root, "file.txt", "after\n", 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := BuildWithLimitsContext(ctx, baseline, root, IntentWrite, BuildLimits{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildWithLimitsContext error = %v, want context.Canceled", err)
	}
}

func TestBuildWithLimitsRejectsCumulativeChangedContentBeforeRetention(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "one.txt", "before one\n", 0o644)
	writeTestFile(t, root, "two.txt", "before two\n", 0o644)
	baseline := captureTestBaseline(t, root, Options{})

	writeTestFile(t, root, "one.txt", strings.Repeat("a", 700), 0o644)
	writeTestFile(t, root, "two.txt", strings.Repeat("b", 700), 0o644)

	_, err := BuildWithLimits(baseline, root, IntentWrite, BuildLimits{MaxArtifactBytes: 1024})
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("BuildWithLimits error = %v, want ErrLimitExceeded", err)
	}
	var pathErr *PathError
	if !errors.As(err, &pathErr) || pathErr.Op != "retain changed content" {
		t.Fatalf("BuildWithLimits error = %v, want pre-retention changed-content rejection", err)
	}
	if !strings.Contains(err.Error(), "changed file content exceeds 1024 bytes") {
		t.Fatalf("BuildWithLimits error = %v, want negotiated byte limit", err)
	}

	result, err := Build(baseline, root, IntentWrite)
	if err != nil {
		t.Fatalf("Build with baseline defaults: %v", err)
	}
	if result.Classification != ClassificationWriteDelta || len(result.Artifact) <= 1024 {
		t.Fatalf("default Build result = classification %q, artifact bytes %d", result.Classification, len(result.Artifact))
	}
}

func TestBuildWithLimitsBoundsArchiveConstruction(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "file.txt", "before\n", 0o644)
	baseline := captureTestBaseline(t, root, Options{})
	writeTestFile(t, root, "file.txt", "after\n", 0o644)

	_, err := BuildWithLimits(baseline, root, IntentWrite, BuildLimits{MaxArtifactBytes: 512})
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("BuildWithLimits error = %v, want ErrLimitExceeded", err)
	}
	if !strings.Contains(err.Error(), "artifact exceeds 512 bytes") {
		t.Fatalf("BuildWithLimits error = %v, want bounded archive construction", err)
	}
}
