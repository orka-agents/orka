package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	values += "controllerRuntime:\n"
	for _, provider := range versionedRuntimeProviders {
		values += "  " + provider + "Image: " + imageRepository("acp-"+provider+"-runtime") + ":0.1.1\n"
	}
	values += "other:\n  image:\n    repository: ghcr.io/example/other\n    tag: \"2.7.0\"\n"
	files := map[string]string{
		"Makefile":                             "VERSION := v0.1.1\n# preserved\nother:\n\t@echo 'unchanged'\n",
		"cmd/build/helmify/static/Chart.yaml":  "apiVersion: v2\nname: orka\nversion: 0.1.1\nappVersion: \"v0.1.1\"\n",
		"cmd/build/helmify/static/values.yaml": values,
		"config/manager/manager.yaml": "args:\n  - --ai-worker-image=ghcr.io/orka-agents/orka/ai-worker:0.1.1\n" +
			"  - --general-worker-image=ghcr.io/orka-agents/orka/general-worker:0.1.1\n",
		"config/manager/kustomization.yaml": "images:\n  - name: controller\n" +
			"    newName: ghcr.io/orka-agents/orka\n    newTag: 0.1.1\n",
	}
	for name, content := range files {
		writeTestFile(t, filepath.Join(root, name), content)
	}
	return root, files
}

// Use the real source files, but overlay the upcoming defaults so this fixture
// works both before and after the development chart inputs land.
func repositoryDevelopmentInputs(t *testing.T) map[string]string {
	t.Helper()
	edits := map[string][]replacement{
		makefilePath: {{pattern: `^VERSION [?:]= .*$`, value: "VERSION ?= v0.0.0-dev", count: 1}},
		chartInputPath: {
			{pattern: `^version: .*$`, value: "version: 0.0.0-dev", count: 1},
			{pattern: `^appVersion: .*$`, value: `appVersion: "v0.0.0-dev"`, count: 1},
		},
	}
	for _, name := range versionedImages {
		edits[valuesInputPath] = append(edits[valuesInputPath], replacement{
			pattern: `^([ \t]+repository: ` + regexp.QuoteMeta(imageRepository(name)) + `\n` +
				`(?:[ \t]*(?:#.*)?\n)*[ \t]+tag: ).*$`, value: `${1}""`, count: 1,
		})
	}
	for _, provider := range versionedRuntimeProviders {
		edits[valuesInputPath] = append(edits[valuesInputPath], replacement{
			pattern: `^([ \t]+` + provider + `Image: ).*$`, value: `${1}""`, count: 1,
		})
	}
	files := make(map[string]string)
	for _, path := range []string{makefilePath, chartInputPath, valuesInputPath,
		"config/manager/manager.yaml", "config/manager/kustomization.yaml"} {
		content := readTestFile(t, filepath.Join("..", "..", "..", path))
		for _, edit := range edits[path] {
			pattern := regexp.MustCompile("(?m)" + edit.pattern)
			if count := len(pattern.FindAllStringIndex(content, -1)); count != edit.count {
				t.Fatalf("expected %d development fixture fields in %s, found %d", edit.count, path, count)
			}
			content = pattern.ReplaceAllString(content, edit.value)
		}
		files[path] = content
	}
	return files
}

func TestUpdateVersionPreservesFormattingAndUpdatesEveryReleaseImage(t *testing.T) {
	for _, assignment := range []string{":=", "?="} {
		t.Run(assignment, func(t *testing.T) {
			root, before := versionFixture(t)
			before[makefilePath] = strings.Replace(before[makefilePath], "VERSION :=", "VERSION "+assignment, 1)
			writeTestFile(t, filepath.Join(root, makefilePath), before[makefilePath])
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
			// Repeating the same update preserves the complete file contents.
			must(t, updateVersion(root, "v9.8.7-rc.3"))
		})
	}
}

func TestRepositoryVersionDefaultHonorsMakeOverrides(t *testing.T) {
	content := readTestFile(t, filepath.Join("..", "..", "..", makefilePath))
	assignments := regexp.MustCompile(`(?m)^VERSION [?:]= .*$`).FindAllString(content, -1)
	if len(assignments) != 1 {
		t.Fatalf("repository Makefile has %d VERSION defaults, want one", len(assignments))
	}
	path := filepath.Join(t.TempDir(), makefilePath)
	writeTestFile(t, path, assignments[0]+"\nprint-version:\n\t@printf '%s' '$(VERSION)'\n")
	for _, test := range []struct {
		name, environment, commandLine, want string
	}{
		{name: "default", want: "v0.0.0-dev"},
		{name: "environment", environment: "v9.8.7", want: "v9.8.7"},
		{name: "command line", commandLine: "v9.8.7-rc.3", want: "v9.8.7-rc.3"},
		{name: "command line takes precedence", environment: "v9.8.7", commandLine: "v9.8.7-rc.3", want: "v9.8.7-rc.3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "make", "--no-print-directory", "-s", "-f", path, "print-version")
			for _, item := range os.Environ() {
				key, _, _ := strings.Cut(item, "=")
				if key != "VERSION" && key != "MAKEFLAGS" && key != "MFLAGS" && key != "GNUMAKEFLAGS" {
					cmd.Env = append(cmd.Env, item)
				}
			}
			if test.environment != "" {
				cmd.Env = append(cmd.Env, "VERSION="+test.environment)
			}
			if test.commandLine != "" {
				cmd.Args = append(cmd.Args, "VERSION="+test.commandLine)
			}
			output, err := cmd.CombinedOutput()
			must(t, err)
			if string(output) != test.want {
				t.Fatalf("Makefile VERSION = %q, want %q", output, test.want)
			}
		})
	}
}

func TestUpdateVersionAcceptsCurrentRepositoryInputs(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		makefilePath, chartInputPath, valuesInputPath,
		"config/manager/manager.yaml", "config/manager/kustomization.yaml",
	} {
		content, err := os.ReadFile(filepath.Join("..", "..", "..", path))
		must(t, err)
		writeTestFile(t, filepath.Join(root, path), string(content))
	}
	must(t, newWorkflow(root).execute([]string{"update-version", "v9.8.7-rc.3"}))
}

func TestUpdateVersionStampsRepositoryDevelopmentInputs(t *testing.T) {
	root := t.TempDir()
	before := repositoryDevelopmentInputs(t)
	for path, content := range before {
		writeTestFile(t, filepath.Join(root, path), content)
	}
	const tag = "v9.8.7-rc.3"
	const version = "9.8.7-rc.3"
	must(t, newWorkflow(root).execute([]string{"update-version", tag}))
	for path, content := range before {
		expected := content
		switch path {
		case makefilePath, chartInputPath:
			expected = strings.ReplaceAll(content, "0.0.0-dev", version)
		case valuesInputPath:
			if count := strings.Count(content, `tag: ""`); count != len(versionedImages) {
				t.Fatalf("expected %d empty image tags, found %d", len(versionedImages), count)
			}
			expected = strings.ReplaceAll(content, `tag: ""`, `tag: "`+version+`"`)
			for _, provider := range versionedRuntimeProviders {
				field := provider + `Image: ""`
				if strings.Count(content, field) != 1 {
					t.Fatalf("expected one empty %s runtime image", provider)
				}
				expected = strings.ReplaceAll(expected, field,
					provider+"Image: "+imageRepository("acp-"+provider+"-runtime")+":"+version)
			}
		case "config/manager/manager.yaml":
			for _, name := range []string{"ai-worker", "general-worker"} {
				pattern := regexp.MustCompile(`(` + regexp.QuoteMeta(imageRepository(name)) + `:)[^\s]+`)
				expected = pattern.ReplaceAllString(expected, `${1}`+version)
			}
		case "config/manager/kustomization.yaml":
			expected = regexp.MustCompile(`(?m)^(\s*newTag:)\s*.*$`).ReplaceAllString(content, `${1} `+version)
		}
		if actual := readTestFile(t, filepath.Join(root, path)); actual != expected {
			t.Fatalf("unexpected release edit to %s", path)
		}
	}
	must(t, updateVersion(root, tag))
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
	for _, tag := range []string{
		"0.2.0", "v0.2", "v0.2.0\n", "v0.2.0-dev", "v0.0.0-dev", "v0.2.0-alpha.1", "v0.2.0+build",
	} {
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
