package workspacedelta

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A rebased baseline is the trusted baseline with the published delta
// applied: the tree that produced the delta builds no further delta from it.
func TestRebaseAppliesThePublishedDeltaWithoutReadingTheTree(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "keep.txt", "keep\n", 0o644)
	writeTestFile(t, root, "modify.txt", "before\n", 0o644)
	writeTestFile(t, root, "delete.txt", "gone\n", 0o644)
	writeTestFile(t, root, "replace/inner.txt", "dir\n", 0o644)
	baseline := captureTestBaseline(t, root, Options{})

	writeTestFile(t, root, "modify.txt", "after\n", 0o644)
	writeTestFile(t, root, "added/new.txt", "new\n", 0o644)
	if err := os.Remove(filepath.Join(root, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "replace")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "replace", "now a file\n", 0o644)
	if err := os.Symlink("keep.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	delta, err := Build(baseline, root, IntentWrite)
	if err != nil || delta.Classification != ClassificationWriteDelta {
		t.Fatalf("Build = %v, %v", delta.Classification, err)
	}

	rebased, err := baseline.Rebase(context.Background(), delta.Changes, delta.Deletions)
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	if rebased.ManifestDigest() == baseline.ManifestDigest() {
		t.Fatal("rebasing a non-empty delta must change the baseline identity")
	}
	next, err := Build(rebased, root, IntentWrite)
	if err != nil {
		t.Fatalf("Build from rebased baseline: %v", err)
	}
	if next.Classification != ClassificationNoChange {
		t.Fatalf("rebased baseline classification = %q, changes %#v, deletions %#v", next.Classification, next.Changes, next.Deletions)
	}
	// The original baseline is immutable.
	again, err := Build(baseline, root, IntentWrite)
	if err != nil || again.ManifestDigest != delta.ManifestDigest {
		t.Fatalf("original baseline changed after Rebase: %v, %v", again.ManifestDigest, err)
	}
}

// Anything the live tree holds beyond the published delta rolls into the
// next delta, as on a cold resume (decision 2).
func TestRebaseLeavesUnpublishedLeftoversForTheNextDelta(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.txt", "a\n", 0o644)
	baseline := captureTestBaseline(t, root, Options{})
	writeTestFile(t, root, "a.txt", "a2\n", 0o644)
	published, err := Build(baseline, root, IntentWrite)
	if err != nil {
		t.Fatal(err)
	}
	// Written after the delta froze: never published.
	writeTestFile(t, root, "leftover.txt", "late\n", 0o644)
	rebased, err := baseline.Rebase(context.Background(), published.Changes, published.Deletions)
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	next, err := Build(rebased, root, IntentWrite)
	if err != nil {
		t.Fatal(err)
	}
	if next.Classification != ClassificationWriteDelta || len(next.Changes) != 1 || next.Changes[0].Path != "leftover.txt" ||
		next.Changes[0].Operation != ChangeAdded || len(next.Deletions) != 0 {
		t.Fatalf("next delta = %#v, want only the leftover", next.Changes)
	}
}

func TestRebaseRefusesADeltaThatDoesNotApply(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.txt", "a\n", 0o644)
	writeTestFile(t, root, ".git/config", "[core]\n", 0o600)
	baseline := captureTestBaseline(t, root, Options{})
	ctx := context.Background()
	digest := DigestPrefix + strings.Repeat("ab", 32)
	for name, tc := range map[string]struct {
		changes   []Change
		deletions []Deletion
	}{
		"add existing":         {changes: []Change{{Path: "a.txt", Operation: ChangeAdded, Kind: EntryFile, Mode: 0o644, Size: 2, Digest: digest}}},
		"modify missing":       {changes: []Change{{Path: "b.txt", Operation: ChangeModified, Kind: EntryFile, Mode: 0o644, Size: 2, Digest: digest}}},
		"delete missing":       {deletions: []Deletion{{Path: "b.txt", Kind: EntryFile}}},
		"delete wrong kind":    {deletions: []Deletion{{Path: "a.txt", Kind: EntryDirectory}}},
		"replace undeleted":    {changes: []Change{{Path: "a.txt", Operation: ChangeReplaced, Kind: EntryDirectory, Mode: 0o755}}},
		"protected path":       {changes: []Change{{Path: ".git/hooks", Operation: ChangeAdded, Kind: EntryDirectory, Mode: 0o755}}},
		"file without digest":  {changes: []Change{{Path: "c.txt", Operation: ChangeAdded, Kind: EntryFile, Mode: 0o644, Size: 2}}},
		"symlink without dest": {changes: []Change{{Path: "l", Operation: ChangeAdded, Kind: EntrySymlink, Mode: 0o777}}},
		"duplicate change": {changes: []Change{
			{Path: "c.txt", Operation: ChangeAdded, Kind: EntryFile, Mode: 0o644, Size: 2, Digest: digest},
			{Path: "c.txt", Operation: ChangeAdded, Kind: EntryFile, Mode: 0o644, Size: 2, Digest: digest},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := baseline.Rebase(ctx, tc.changes, tc.deletions); err == nil {
				t.Fatal("Rebase accepted a delta that does not apply")
			} else if !errors.Is(err, ErrRebaseMismatch) && !errors.Is(err, ErrUnsafeFileType) && !errors.Is(err, ErrInvalidBaseline) {
				if _, ok := errors.AsType[*PathError](err); !ok {
					t.Fatalf("unexpected error type %T: %v", err, err)
				}
			}
		})
	}
	var nilSnapshot *Snapshot
	if _, err := nilSnapshot.Rebase(ctx, nil, nil); !errors.Is(err, ErrInvalidBaseline) {
		t.Fatalf("nil baseline = %v, want ErrInvalidBaseline", err)
	}
}
