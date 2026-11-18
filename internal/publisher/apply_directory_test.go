package publisher

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/workspacedelta"
)

type testTreeEntry struct {
	content string
	mode    os.FileMode
	link    string
	dir     bool
}

func writeTestTree(t *testing.T, root string, tree map[string]testTreeEntry) {
	t.Helper()
	names := make([]string, 0, len(tree))
	for name := range tree {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := tree[name]
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("mkdir parent of %s: %v", name, err)
		}
		switch {
		case entry.dir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", name, err)
			}
		case entry.link != "":
			if err := os.Symlink(entry.link, target); err != nil {
				t.Fatalf("symlink %s: %v", name, err)
			}
		default:
			mode := entry.mode
			if mode == 0 {
				mode = 0o644
			}
			mustWriteFile(t, target, entry.content, mode)
		}
	}
}

// buildCapturedDelta captures baseline, applies mutate in a scratch copy, and
// returns the resulting write delta plus a fresh checkout of the baseline.
func buildCapturedDelta(t *testing.T, baseline map[string]testTreeEntry, mutate func(root string)) (workspacedelta.Result, string) {
	t.Helper()
	scratch := filepath.Join(t.TempDir(), "scratch")
	writeTestTree(t, scratch, baseline)
	snapshot, err := workspacedelta.Capture(scratch, workspacedelta.Options{})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	mutate(scratch)
	delta, err := workspacedelta.BuildWithLimits(snapshot, scratch, workspacedelta.IntentWrite, workspacedelta.BuildLimits{})
	if err != nil {
		t.Fatalf("BuildWithLimits: %v", err)
	}
	if delta.Classification != workspacedelta.ClassificationWriteDelta {
		t.Fatalf("classification = %s, want write delta", delta.Classification)
	}
	checkout := filepath.Join(t.TempDir(), "checkout")
	writeTestTree(t, checkout, baseline)
	return delta, checkout
}

type handDelta struct {
	changes   []workspacedelta.Change
	deletions []workspacedelta.Deletion
	symlinks  []workspacedelta.Symlink
	files     map[string]string
}

// build encodes a hand-written delta with valid manifests, so tests can reach
// the filesystem rules that a real capture would never produce.
func (d handDelta) build(t *testing.T) ([]byte, string) {
	t.Helper()
	encode := func(value any) []byte {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return append(data, '\n')
	}
	deletions := encode(deletionDocument{Schema: workspacedelta.ManifestSchema, Deletions: d.deletions})
	symlinks := encode(symlinkDocument{Schema: workspacedelta.ManifestSchema, Symlinks: d.symlinks})
	changes := append([]workspacedelta.Change(nil), d.changes...)
	for index := range changes {
		if changes[index].Kind == workspacedelta.EntryFile {
			content := []byte(d.files[changes[index].Path])
			changes[index].Size = int64(len(content))
			changes[index].Digest = digestBytes(content)
		}
	}
	manifest := encode(workspacedelta.Manifest{
		Schema: workspacedelta.ManifestSchema, Entries: changes,
		DeletionsDigest: digestBytes(deletions), SymlinksDigest: digestBytes(symlinks),
	})
	type entry struct {
		name string
		data []byte
	}
	entries := make([]entry, 0, 3+len(d.files))
	entries = append(entries, entry{manifestArchivePath, manifest}, entry{deletionsArchivePath, deletions}, entry{symlinksArchivePath, symlinks})
	for name, content := range d.files {
		entries = append(entries, entry{fileArchivePrefix + name, []byte(content)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, current := range entries {
		if err := writer.WriteHeader(&tar.Header{
			Name: current.name, Mode: 0o644, Size: int64(len(current.data)), ModTime: time.Unix(0, 0).UTC(),
			Typeflag: tar.TypeReg, Format: tar.FormatPAX,
		}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := writer.Write(current.data); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buffer.Bytes(), digestBytes(buffer.Bytes())
}

func readTree(t *testing.T, root string) map[string]testTreeEntry {
	t.Helper()
	tree := map[string]testTreeEntry{}
	if err := filepath.Walk(root, func(current string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, current)
		if err != nil || relative == "." {
			return err
		}
		relative = filepath.ToSlash(relative)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(current)
			if err != nil {
				return err
			}
			tree[relative] = testTreeEntry{link: target}
		case info.IsDir():
			tree[relative] = testTreeEntry{dir: true}
		default:
			data, err := os.ReadFile(current)
			if err != nil {
				return err
			}
			tree[relative] = testTreeEntry{content: string(data), mode: info.Mode().Perm()}
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return tree
}

func TestApplyDeltaToDirectoryMatchesCapturedFinalTree(t *testing.T) {
	baseline := map[string]testTreeEntry{
		"README.md":          {content: "readme\n"},
		"main.go":            {content: "package main\n"},
		"delete.txt":         {content: "gone\n"},
		"old/a.txt":          {content: "a\n"},
		"old/nested/b.txt":   {content: "b\n"},
		"latest":             {link: "main.go"},
		"replace-me":         {content: "file becomes dir\n"},
		"scripts/run.sh":     {content: "#!/bin/sh\n", mode: 0o644},
		"keep/untouched.txt": {content: "keep\n"},
	}
	mutate := func(root string) {
		mustWriteFile(t, filepath.Join(root, "main.go"), "package main // changed\n", 0o644)
		mustWriteFile(t, filepath.Join(root, "new/dir/added.txt"), "added\n", 0o644)
		if err := os.Remove(filepath.Join(root, "delete.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(root, "old")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(root, "latest")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("README.md", filepath.Join(root, "latest")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(root, "replace-me")); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, filepath.Join(root, "replace-me/inner.txt"), "inner\n", 0o644)
		if err := os.Chmod(filepath.Join(root, "scripts/run.sh"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	delta, checkout := buildCapturedDelta(t, baseline, mutate)
	expectedRoot := filepath.Join(t.TempDir(), "expected")
	writeTestTree(t, expectedRoot, baseline)
	mutate(expectedRoot)

	if err := ApplyDeltaToDirectory(context.Background(), checkout, delta.Artifact, delta.ArtifactDigest, ".", false); err != nil {
		t.Fatalf("ApplyDeltaToDirectory: %v", err)
	}
	got, want := readTree(t, checkout), readTree(t, expectedRoot)
	if len(got) != len(want) {
		t.Fatalf("applied tree = %#v, want %#v", got, want)
	}
	for name, entry := range want {
		if got[name] != entry {
			t.Fatalf("entry %q = %#v, want %#v", name, got[name], entry)
		}
	}
}

func TestApplyDeltaToDirectoryRootsBeneathRelativeRoot(t *testing.T) {
	sub := map[string]testTreeEntry{"main.go": {content: "package app\n"}}
	delta, _ := buildCapturedDelta(t, sub, func(root string) {
		mustWriteFile(t, filepath.Join(root, "main.go"), "package app // changed\n", 0o644)
		mustWriteFile(t, filepath.Join(root, "new.txt"), "new\n", 0o644)
	})
	checkout := filepath.Join(t.TempDir(), "repo")
	writeTestTree(t, checkout, map[string]testTreeEntry{
		"main.go":              {content: "package root\n"},
		"services/app/main.go": {content: "package app\n"},
	})
	if err := ApplyDeltaToDirectory(context.Background(), checkout, delta.Artifact, delta.ArtifactDigest, "services/app", false); err != nil {
		t.Fatalf("ApplyDeltaToDirectory: %v", err)
	}
	tree := readTree(t, checkout)
	if tree["main.go"].content != "package root\n" || tree["services/app/main.go"].content != "package app // changed\n" ||
		tree["services/app/new.txt"].content != "new\n" {
		t.Fatalf("relative-root application = %#v", tree)
	}
	if _, ok := tree["new.txt"]; ok {
		t.Fatalf("delta was applied at the repository root: %#v", tree)
	}
}

func TestApplyDeltaToDirectoryRejectsUnsafeDeltasBeforeWriting(t *testing.T) {
	baseline := map[string]testTreeEntry{
		"main.go":    {content: "package main\n"},
		"docs/a.md":  {content: "a\n"},
		"linked":     {link: "docs"},
		"exists.txt": {content: "exists\n"},
	}
	fileChange := func(path string, operation workspacedelta.ChangeOperation) workspacedelta.Change {
		return workspacedelta.Change{Path: path, Operation: operation, Kind: workspacedelta.EntryFile, Mode: 0o644}
	}
	cases := []struct {
		name         string
		delta        handDelta
		relativeRoot string
		deny         bool
		tamper       func(artifact []byte) []byte
	}{
		{
			name:   "digest mismatch",
			delta:  handDelta{changes: []workspacedelta.Change{fileChange("new.txt", workspacedelta.ChangeAdded)}, files: map[string]string{"new.txt": "x"}},
			tamper: func(artifact []byte) []byte { return append(append([]byte(nil), artifact...), 0) },
		},
		{
			name:  "path escape",
			delta: handDelta{changes: []workspacedelta.Change{fileChange("../escape.txt", workspacedelta.ChangeAdded)}, files: map[string]string{"../escape.txt": "x"}},
		},
		{
			name:         "relative root escape",
			delta:        handDelta{changes: []workspacedelta.Change{fileChange("x.txt", workspacedelta.ChangeAdded)}, files: map[string]string{"x.txt": "x"}},
			relativeRoot: "../outside",
		},
		{
			name: "symlink target escape",
			delta: handDelta{
				changes:  []workspacedelta.Change{{Path: "evil", Operation: workspacedelta.ChangeAdded, Kind: workspacedelta.EntrySymlink, Mode: 0o777, Target: "../../etc"}},
				symlinks: []workspacedelta.Symlink{{Path: "evil", Target: "../../etc", Mode: 0o777}},
			},
		},
		{
			name:  "write through in-repository symlink parent",
			delta: handDelta{changes: []workspacedelta.Change{fileChange("linked/b.md", workspacedelta.ChangeAdded)}, files: map[string]string{"linked/b.md": "b\n"}},
		},
		{
			name: "child below a symlink the delta creates",
			delta: handDelta{
				changes: []workspacedelta.Change{
					{Path: "fresh", Operation: workspacedelta.ChangeAdded, Kind: workspacedelta.EntrySymlink, Mode: 0o777, Target: "docs"},
					fileChange("fresh/c.md", workspacedelta.ChangeAdded),
				},
				symlinks: []workspacedelta.Symlink{{Path: "fresh", Target: "docs", Mode: 0o777}},
				files:    map[string]string{"fresh/c.md": "c\n"},
			},
		},
		{
			name:  "git metadata",
			delta: handDelta{changes: []workspacedelta.Change{fileChange(".git/config", workspacedelta.ChangeAdded)}, files: map[string]string{".git/config": "x"}},
		},
		{
			name:  "denied control path",
			delta: handDelta{changes: []workspacedelta.Change{fileChange(".github/workflows/ci.yml", workspacedelta.ChangeAdded)}, files: map[string]string{".github/workflows/ci.yml": "on: push\n"}},
			deny:  true,
		},
		{
			name:  "added path already exists",
			delta: handDelta{changes: []workspacedelta.Change{fileChange("exists.txt", workspacedelta.ChangeAdded)}, files: map[string]string{"exists.txt": "x"}},
		},
		{
			name:  "modified path missing",
			delta: handDelta{changes: []workspacedelta.Change{fileChange("missing.txt", workspacedelta.ChangeModified)}, files: map[string]string{"missing.txt": "x"}},
		},
		{
			name:  "deletion kind mismatch",
			delta: handDelta{deletions: []workspacedelta.Deletion{{Path: "linked", Kind: workspacedelta.EntryFile}}},
		},
		{
			name:  "directory deletion omits descendant",
			delta: handDelta{deletions: []workspacedelta.Deletion{{Path: "docs", Kind: workspacedelta.EntryDirectory}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkout := filepath.Join(t.TempDir(), "checkout")
			writeTestTree(t, checkout, baseline)
			before := readTree(t, checkout)
			artifact, digest := tc.delta.build(t)
			if tc.tamper != nil {
				artifact = tc.tamper(artifact)
			}
			err := ApplyDeltaToDirectory(context.Background(), checkout, artifact, digest, tc.relativeRoot, tc.deny)
			if !errors.Is(err, ErrUnsafeDelta) {
				t.Fatalf("ApplyDeltaToDirectory error = %v, want ErrUnsafeDelta", err)
			}
			after := readTree(t, checkout)
			if len(after) != len(before) {
				t.Fatalf("rejected delta changed the checkout: %#v", after)
			}
			for name, entry := range before {
				if after[name] != entry {
					t.Fatalf("rejected delta changed %q: %#v", name, after[name])
				}
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(checkout), "escape.txt")); !os.IsNotExist(err) {
				t.Fatalf("delta escaped the repository root: %v", err)
			}
		})
	}
}

func TestApplyDeltaToDirectoryControlPathsAllowedWithoutDeny(t *testing.T) {
	delta, checkout := buildCapturedDelta(t, map[string]testTreeEntry{"README.md": {content: "r\n"}}, func(root string) {
		mustWriteFile(t, filepath.Join(root, ".github/workflows/ci.yml"), "on: push\n", 0o644)
	})
	if err := ApplyDeltaToDirectory(context.Background(), checkout, delta.Artifact, delta.ArtifactDigest, "", true); !errors.Is(err, ErrUnsafeDelta) {
		t.Fatalf("deny=true error = %v, want ErrUnsafeDelta", err)
	}
	if err := ApplyDeltaToDirectory(context.Background(), checkout, delta.Artifact, delta.ArtifactDigest, "", false); err != nil {
		t.Fatalf("deny=false: %v", err)
	}
	if got := string(mustReadFile(t, filepath.Join(checkout, ".github/workflows/ci.yml"))); got != "on: push\n" {
		t.Fatalf("control path content = %q", got)
	}
}
