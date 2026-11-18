package patchverification

import (
	"archive/tar"
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func stagingArchive(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, header := range headers {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg && header.Size != 0 {
			if _, err := writer.Write(bytes.Repeat([]byte("x"), int(header.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func stagingInput(t *testing.T, archive []byte) PodInput {
	t.Helper()
	manifest := testManifest()
	manifest.Sources.Original.ArchiveDigest = Digest(archive)
	manifest.Environment.Dependencies = map[string]string{"orka.kubernetes.policy": KubernetesPolicyVersion}
	content := []byte("#!/bin/sh\nexit 0\n")
	manifest.Files = []FrozenFile{{Path: "run", Content: content, Digest: Digest(content), Executable: true}}
	input := PodInput{
		Version: PodProtocolVersion, Manifest: manifest, Side: Original, CheckID: "case-one", Archive: archive,
	}
	bindStagingInput(t, &input)
	return input
}

func bindStagingInput(t *testing.T, input *PodInput) {
	t.Helper()
	var err error
	input.Binding, err = NewRunBinding(input.Manifest, "attempt-one", "task-original", "task-patched")
	if err != nil {
		t.Fatal(err)
	}
}

func stagingRoots(t *testing.T) (string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, checks := filepath.Join(root, "source"), filepath.Join(root, "checks")
	for _, path := range []string{source, checks} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.Chmod(path, 0700)
			}
			return nil
		}); err != nil {
			t.Error(err)
		}
	})
	return source, checks
}

func TestStagePodInputExactReadOnlyFiles(t *testing.T) {
	archive := stagingArchive(t,
		&tar.Header{Name: "nested/", Typeflag: tar.TypeDir, Mode: 0755},
		&tar.Header{Name: "nested/source", Typeflag: tar.TypeReg, Mode: 0644, Size: 3},
		&tar.Header{Name: "executable", Typeflag: tar.TypeReg, Mode: 0755, Size: 1})
	input := stagingInput(t, archive)
	source, checks := stagingRoots(t)
	if err := StagePodInput(context.Background(), input, source, checks); err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		name    string
		content string
		mode    os.FileMode
	}{
		{filepath.Join(source, "nested/source"), "xxx", 0444},
		{filepath.Join(source, "executable"), "x", 0555},
		{filepath.Join(checks, "run"), string(input.Manifest.Files[0].Content), 0555},
	} {
		content, err := os.ReadFile(file.name)
		if err != nil || string(content) != file.content {
			t.Fatal("staged content differs from the frozen input")
		}
		info, err := os.Stat(file.name)
		if err != nil || info.Mode().Perm() != file.mode {
			t.Fatal("staged file is not read-only with its exact executable bit")
		}
	}
	for _, path := range []string{source, checks, filepath.Join(source, "nested")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0555 {
			t.Fatal("source or check directory remained writable")
		}
	}
	if err := StagePodInput(context.Background(), input, source, checks); err == nil {
		t.Fatal("nonempty staging roots were reused")
	}
}

func TestStagePodInputHTTPServerCommand(t *testing.T) {
	input := stagingInput(t, stagingArchive(t))
	protected := httpManifest(t)
	input.Manifest.Checks, input.Manifest.Environment = protected.Checks, protected.Environment
	bindStagingInput(t, &input)
	before, err := ManifestDigest(input.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	source, checks := stagingRoots(t)
	if err := StagePodInput(context.Background(), input, source, checks); err != nil {
		t.Fatal(err)
	}
	after, err := ManifestDigest(input.Manifest)
	if err != nil || after != before || len(input.Manifest.Checks[0].Command) != 0 {
		t.Fatal("staging altered the protected contract or its frozen binding")
	}
	for _, version := range []int{1, PodProtocolVersion + 1} {
		changed := input
		changed.Version = version
		if _, err := ValidatePodInput(changed); err == nil {
			t.Fatal("an incompatible pod protocol accepted a protected check")
		}
	}
	input.Manifest.Checks[0].HTTP.ServerCommand[0] = "/checks/missing"
	bindStagingInput(t, &input)
	source, checks = stagingRoots(t)
	if err := StagePodInput(context.Background(), input, source, checks); err == nil {
		t.Fatal("an undeclared HTTP server executable was staged")
	}
}

func TestStagePodInputRejectsUnsafeArchives(t *testing.T) {
	for name, headers := range map[string][]*tar.Header{
		"traversal":     {{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0644, Size: 1}},
		"absolute":      {{Name: "/escape", Typeflag: tar.TypeReg, Mode: 0644, Size: 1}},
		"symlink":       {{Name: "alias", Typeflag: tar.TypeSymlink, Mode: 0755, Linkname: "../escape"}},
		"hardlink":      {{Name: "alias", Typeflag: tar.TypeLink, Mode: 0644, Linkname: "source"}},
		"special":       {{Name: "device", Typeflag: tar.TypeChar, Mode: 0644}},
		"writable mode": {{Name: "source", Typeflag: tar.TypeReg, Mode: 0666, Size: 1}},
		"duplicate": {
			{Name: "source", Typeflag: tar.TypeReg, Mode: 0644, Size: 1},
			{Name: "source", Typeflag: tar.TypeReg, Mode: 0644, Size: 1},
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := stagingInput(t, stagingArchive(t, headers...))
			source, checks := stagingRoots(t)
			if err := StagePodInput(context.Background(), input, source, checks); err == nil {
				t.Fatal("unsafe archive was staged")
			}
		})
	}
	t.Run("trailing archive data", func(t *testing.T) {
		archive := stagingArchive(t, &tar.Header{Name: "source", Typeflag: tar.TypeReg, Mode: 0644})
		archive = append(archive, make([]byte, 1024)...)
		input := stagingInput(t, archive)
		source, checks := stagingRoots(t)
		if err := StagePodInput(context.Background(), input, source, checks); err == nil {
			t.Fatal("unobserved archive bytes were accepted")
		}
	})
}

func TestStagePodInputRejectsUnfrozenCommandsAndPaths(t *testing.T) {
	archive := stagingArchive(t, &tar.Header{Name: "source", Typeflag: tar.TypeReg, Mode: 0644})
	for _, change := range []struct {
		name   string
		mutate func(*PodInput)
	}{
		{"check traversal", func(input *PodInput) { input.Manifest.Files[0].Path = "../escape" }},
		{"check absolute path", func(input *PodInput) { input.Manifest.Files[0].Path = "/escape" }},
		{"check git metadata", func(input *PodInput) { input.Manifest.Files[0].Path = ".git/config" }},
		{"unfrozen executable", func(input *PodInput) { input.Manifest.Checks[0].Command[0] = "/bin/sh" }},
		{"nonexecutable check", func(input *PodInput) { input.Manifest.Files[0].Executable = false }},
		{"environment injection", func(input *PodInput) {
			input.Manifest.Environment.Variables = map[string]string{"LD_PRELOAD": "/source/attack.so"}
		}},
		{"duplicate check file", func(input *PodInput) { input.Manifest.Files = append(input.Manifest.Files, input.Manifest.Files[0]) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			input := stagingInput(t, archive)
			change.mutate(&input)
			bindStagingInput(t, &input)
			source, checks := stagingRoots(t)
			if err := StagePodInput(context.Background(), input, source, checks); err == nil {
				t.Fatal("unsafe command or frozen file inventory was staged")
			}
		})
	}
	t.Run("cancellation", func(t *testing.T) {
		input := stagingInput(t, archive)
		source, checks := stagingRoots(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := StagePodInput(ctx, input, source, checks); err == nil {
			t.Fatal("canceled staging succeeded")
		}
	})
}
