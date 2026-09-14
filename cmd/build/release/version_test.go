package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func versionFixture(t *testing.T) (string, map[string]string) {
	t.Helper()
	root := t.TempDir()
	values := "# preserved comment\n"
	for _, name := range versionedImages {
		values += name + ":\n  image:\n    repository: " + imageRepository(name) +
			"\n    # image comment\n    tag: \"0.1.1\"\n"
	}
	values += "other:\n  image:\n    repository: ghcr.io/example/other\n    tag: \"2.7.0\"\n"
	files := map[string]string{
		"Makefile":                             "VERSION := v0.1.1\n# preserved\nother:\n\t@echo 'unchanged'\n",
		"cmd/build/helmify/static/Chart.yaml":  "apiVersion: v2\nname: orka\nversion: 0.1.1\nappVersion: \"v0.1.1\"\n",
		"cmd/build/helmify/static/values.yaml": values,
		"config/manager/manager.yaml": "args:\n  - --ai-worker-image=ghcr.io/orka-agents/orka/ai-worker:0.1.1\n" +
			"  - --general-worker-image=ghcr.io/orka-agents/orka/general-worker:0.1.1\n",
		"config/manager/kustomization.yaml": "images:\n  - name: ghcr.io/orka-agents/orka\n    newTag: 0.1.1\n" +
			"  - name: controller\n    newTag: 0.1.1\n",
	}
	for name, content := range files {
		writeTestFile(t, filepath.Join(root, name), content)
	}
	return root, files
}

func TestUpdateVersionPreservesFormattingAndUpdatesEveryReleaseImage(t *testing.T) {
	root, before := versionFixture(t)
	must(t, updateVersion(root, "v9.8.7-rc.3"))
	for name, content := range before {
		expected := strings.ReplaceAll(content, "0.1.1", "9.8.7-rc.3")
		if actual := readTestFile(t, filepath.Join(root, name)); actual != expected {
			t.Fatalf("unexpected edit to %s:\n%s", name, actual)
		}
		info, err := os.Stat(filepath.Join(root, name))
		must(t, err)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("changed file permissions for %s", name)
		}
	}
	// Repeating the same update is safe and preserves the complete file contents.
	must(t, updateVersion(root, "v9.8.7-rc.3"))
}

func TestUpdateVersionRejectsMissingOrDuplicateFieldsBeforeWriting(t *testing.T) {
	for _, change := range []string{"missing", "duplicate"} {
		root, before := versionFixture(t)
		path := "cmd/build/helmify/static/Chart.yaml"
		if change == "missing" {
			before[path] = strings.ReplaceAll(before[path], "version: 0.1.1\n", "")
		} else {
			before[path] += "version: 0.1.1\n"
		}
		writeTestFile(t, filepath.Join(root, path), before[path])
		wantError(t, updateVersion(root, testVersion), "expected 1 replacements")
		for name, expected := range before {
			if readTestFile(t, filepath.Join(root, name)) != expected {
				t.Fatalf("failed validation changed %s", name)
			}
		}
	}
}

func TestUpdateVersionRejectsInvalidTagsWithoutWriting(t *testing.T) {
	root, before := versionFixture(t)
	for _, tag := range []string{"0.2.0", "v0.2", "v0.2.0\n", "v0.2.0-dev"} {
		wantError(t, updateVersion(root, tag), "usage:")
	}
	for name, expected := range before {
		if readTestFile(t, filepath.Join(root, name)) != expected {
			t.Fatalf("invalid version changed %s", name)
		}
	}
}

func TestReleaseCLIValidatesArgumentsAndUsesItsWorkingDirectory(t *testing.T) {
	root, _ := versionFixture(t)
	w := newWorkflow(root)
	for _, args := range [][]string{
		nil, {"unknown"}, {"prepare"}, {"update-version", testVersion, "extra"}, {"download", "123"},
	} {
		wantError(t, w.execute(args), "")
	}
	must(t, w.execute([]string{"update-version", testVersion}))
	if !strings.HasPrefix(readTestFile(t, filepath.Join(root, "Makefile")), "VERSION := v0.2.0\n") {
		t.Fatal("CLI did not update working directory")
	}
	f := newReleaseFixture(t)
	must(t, f.w.execute([]string{"check-bundle", "bundle"}))
	writeTestFile(t, filepath.Join(f.directory, f.data.Chart.File), "changed bytes")
	wantError(t, f.w.execute([]string{"check-bundle", "bundle"}), "chart bytes changed")
}
