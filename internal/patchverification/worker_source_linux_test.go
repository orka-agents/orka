//go:build linux

package patchverification

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func workerInputFixture(t *testing.T, action Action) PreparationInput {
	t.Helper()
	healthy, err := HTTPExpectation(200, "patched\n")
	require.NoError(t, err)
	failure, err := HTTPExpectation(200, "original\n")
	require.NoError(t, err)
	file := FrozenFile{Path: "server.sh", Content: []byte("#!/bin/sh\nexec /usr/bin/server /src/value.txt\n"), Executable: true}
	file.Digest = Digest(file.Content)
	input := PreparationInput{Version: PreparationProtocolVersion, ID: "synthetic-run", Namespace: "default", ParentTaskName: "parent", ParentTaskUID: "parent-uid", TaskUID: "prepare-uid",
		Manifest: Manifest{Version: SchemaVersion, Action: action, Problem: "synthetic behavior", Scope: []string{"value"}, Files: []FrozenFile{file},
			Sources: Sources{Repository: "https://github.com/example/fixture", Original: SourceIdentity{Commit: strings.Repeat("b", 40), Tree: strings.Repeat("c", 40)}},
			Environment: Environment{Image: "example/tool@" + Digest(nil), ImageID: Digest(nil), Platform: "linux/amd64", Profile: LocalServices,
				Dependencies: map[string]string{"orka.kubernetes.policy": KubernetesPolicyVersion, PreparationPolicyKey: PreparationPolicy, PreparationConfigurationKey: Digest([]byte("fixture authority"))}},
			Checks: []Check{{ID: "reproduction", Kind: Reproduction, HTTP: &HTTPCheck{Version: HTTPCheckVersion, ServerCommand: []string{"/checks/server.sh"}, Path: "/"},
				Healthy: healthy, Failure: failure, TimeoutSeconds: 5}}}}
	input.Manifest.ReportDigest, err = StableReportDigest(input.Manifest.Problem, input.Manifest.Scope)
	require.NoError(t, err)
	if action == VerifyPatch {
		input.Patch = []byte("--- a/value.txt\n+++ b/value.txt\n@@ -1 +1 @@\n-original\n+patched\n")
		input.Manifest.DeclaredChanges = []DeclaredChange{{Kind: "source", Paths: []string{"value.txt"}, Description: "synthetic value change"}}
		normal := input.Manifest.Checks[0]
		normal.ID, normal.Kind = "normal", Normal
		input.Manifest.Checks = append(input.Manifest.Checks, normal)
	}
	require.NoError(t, ValidatePreparationInput(input))
	return input
}

func TestWorkerPreparationProtocolRejectsMutationAndTrailingData(t *testing.T) {
	input := workerInputFixture(t, VerifyPatch)
	content, err := json.Marshal(input)
	require.NoError(t, err)
	decoded, err := DecodePreparationInput(bytes.NewReader(content))
	require.NoError(t, err)
	require.Equal(t, input, decoded)
	for _, raw := range [][]byte{append(append([]byte{}, content...), []byte("{}")...), bytes.Repeat([]byte("x"), MaxPreparationInputBytes+1)} {
		_, err := DecodePreparationInput(bytes.NewReader(raw))
		require.Error(t, err)
	}
	for _, change := range []func(*PreparationInput){
		func(i *PreparationInput) { i.Manifest.Sources.Original.Tree = "HEAD" },
		func(i *PreparationInput) { i.Manifest.Sources.Repository = "file:///repository" },
		func(i *PreparationInput) {
			i.Manifest.Sources.Repository = "https://github.com/owner/repo?credential=x"
		},
		func(i *PreparationInput) { i.Manifest.Files[0].Path = "../source" },
		func(i *PreparationInput) { i.Manifest.Checks[0].HTTP.ServerCommand = []string{"/src/server"} },
		func(i *PreparationInput) {
			i.Manifest.Checks[0].HTTP = nil
			i.Manifest.Checks[0].Command = []string{"/checks/server.sh"}
		},
		func(i *PreparationInput) {
			i.Manifest.Environment.Variables = map[string]string{"LD_PRELOAD": "/src/subject"}
		},
	} {
		var altered PreparationInput
		require.NoError(t, json.Unmarshal(content, &altered))
		change(&altered)
		require.Error(t, ValidatePreparationInput(altered))
	}
	first, err := PreparationInputDigest(input)
	require.NoError(t, err)
	input.TaskUID = "other-api-uid"
	second, err := PreparationInputDigest(input)
	require.NoError(t, err)
	require.Equal(t, first, second)
	input.ParentTaskUID = "replaced-parent"
	third, err := PreparationInputDigest(input)
	require.NoError(t, err)
	require.NotEqual(t, first, third)
}

func TestWorkerArchivesVerifyExactObjectsApplyPatchAndPreserveChecks(t *testing.T) {
	input := workerInputFixture(t, VerifyPatch)
	repository := t.TempDir()
	sourceFixtureGit(t, repository, nil, "init", "--quiet")
	sourceFixtureWrite(t, filepath.Join(repository, "value.txt"), "original\n", 0600)
	sourceFixtureWrite(t, filepath.Join(repository, ".gitattributes"), "value.txt export-ignore\n", 0600)
	sourceFixtureGit(t, repository, nil, "add", ".")
	sourceFixtureGit(t, repository, nil, "commit", "--quiet", "-m", "synthetic source")
	input.Manifest.Sources.Original.Commit = strings.TrimSpace(string(sourceFixtureGit(t, repository, nil, "rev-parse", "HEAD")))
	input.Manifest.Sources.Original.Tree = strings.TrimSpace(string(sourceFixtureGit(t, repository, nil, "rev-parse", "HEAD^{tree}")))
	git := sourceTestGit(t, "sha1")
	tree, err := git.importCommit(t.Context(), filepath.Join(repository, ".git", "objects"), input.Manifest.Sources.Original.Commit)
	require.NoError(t, err)
	require.Equal(t, input.Manifest.Sources.Original.Tree, tree)
	before, err := json.Marshal(input)
	require.NoError(t, err)
	report, err := prepareWorkerArchives(t.Context(), git, input, tree)
	require.NoError(t, err)
	require.NoError(t, ValidatePreparationReport(input, *report))
	require.Equal(t, input.Manifest.Files, report.Manifest.Files)
	require.NotEqual(t, report.Manifest.Sources.Original.Tree, report.Manifest.Sources.Patched.Tree)
	require.Equal(t, Digest(input.Patch), report.Manifest.Sources.PatchDigest)
	for _, item := range []struct {
		source SourceIdentity
		want   string
	}{
		{report.Manifest.Sources.Original, "original\n"}, {report.Manifest.Sources.Patched, "patched\n"},
	} {
		reader := tar.NewReader(bytes.NewReader(report.Provenance[item.source.ArchiveDigest]))
		found := false
		for {
			header, err := reader.Next()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			if header.Name == "value.txt" {
				content, err := io.ReadAll(reader)
				require.NoError(t, err)
				require.Equal(t, item.want, string(content))
				found = true
			}
		}
		require.True(t, found, "Git attributes must not hide raw tree blobs")
	}
	after, err := json.Marshal(input)
	require.NoError(t, err)
	require.Equal(t, before, after)
	// No checkout, hooks, or candidate executable is needed to prepare either side.
	_, err = os.Stat(filepath.Join(git.root, "value.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestWorkerArchiveSizeAndCandidatePathBounds(t *testing.T) {
	input := workerInputFixture(t, VerifyPatch)
	repository := t.TempDir()
	sourceFixtureGit(t, repository, nil, "init", "--quiet")
	sourceFixtureWrite(t, filepath.Join(repository, "value.txt"), "original\n", 0600)
	sourceFixtureGit(t, repository, nil, "add", ".")
	sourceFixtureGit(t, repository, nil, "commit", "--quiet", "-m", "synthetic source")
	input.Manifest.Sources.Original.Commit = strings.TrimSpace(string(sourceFixtureGit(t, repository, nil, "rev-parse", "HEAD")))
	git := sourceTestGit(t, "sha1")
	tree, err := git.importCommit(t.Context(), filepath.Join(repository, ".git", "objects"), input.Manifest.Sources.Original.Commit)
	require.NoError(t, err)
	input.Manifest.Sources.Original.Tree = tree
	_, err = git.archiveLimit(t.Context(), tree, 1)
	require.Error(t, err)
	for _, patch := range []string{
		"diff --git a/../escape b/../escape\nnew file mode 100644\n--- /dev/null\n+++ b/../escape\n@@ -0,0 +1 @@\n+escape\n",
		"diff --git a/link b/link\nnew file mode 120000\n--- /dev/null\n+++ b/link\n@@ -0,0 +1 @@\n+/checks/server.sh\n",
		"--- a/value.txt\n+++ b/value.txt\n@@ -1 +1 @@\n-wrong original\n+patched\n",
	} {
		input.Patch = []byte(patch)
		report, err := prepareWorkerArchives(t.Context(), git, input, tree)
		require.Error(t, err)
		require.Nil(t, report)
	}
}

func TestWorkerPreparationEntrypointDoesNotOfferLocalRepositoryFallback(t *testing.T) {
	input := workerInputFixture(t, ValidateReport)
	for _, params := range [][2]string{{"/workspace", "/usr/bin/git"}, {t.TempDir(), "/worker"}, {"/", "/worker"}} {
		result, err := PrepareWorkerSource(t.Context(), input, params[0], params[1])
		require.Error(t, err)
		require.Nil(t, result)
	}
}

func TestWorkerPreparationScreensAllProvenanceWithoutRewriting(t *testing.T) {
	for _, content := range []string{
		`{"password":{"type":"string"}}`,
		`{"password":"","client_secret":""}`,
		`password := os.Getenv("PASSWORD")`,
		`password = "synthetic-fixture-only"`,
		`{"password":{"type":"string","default":"synthetic-fixture-only"}}`,
		"-----BEGIN " + "PRIVATE KEY-----\nsynthetic-fixture-only\n",
	} {
		for _, side := range []string{Original, Patched, "checks", "patch"} {
			t.Run(side+"/"+Digest([]byte(content))[:18], func(t *testing.T) {
				input := workerInputFixture(t, VerifyPatch)
				repository, original := sourceTestRepository(t, sourceSHA1)
				if side == Original {
					sourceFixtureWrite(t, filepath.Join(repository, "config.json"), content, 0600)
					sourceFixtureGit(t, repository, nil, "add", ".")
					sourceFixtureGit(t, repository, nil, "commit", "--quiet", "-m", "synthetic screening")
					original = strings.TrimSpace(string(sourceFixtureGit(t, repository, nil, "rev-parse", "HEAD")))
				}
				if side == Patched || side == "patch" {
					sourceFixtureWrite(t, filepath.Join(repository, "config.json"), content, 0600)
					sourceFixtureGit(t, repository, nil, "add", ".")
					input.Patch = sourceFixtureGit(t, repository, nil, "diff", "--cached", "--binary", original, "--")
				}
				if side == "checks" {
					input.Manifest.Files = append(input.Manifest.Files, FrozenFile{Path: "config.json", Content: []byte(content), Digest: Digest([]byte(content))})
				}
				input.Manifest.Sources.Original.Commit = original
				git := sourceTestGit(t, sourceSHA1)
				tree, err := git.importCommit(t.Context(), filepath.Join(repository, ".git/objects"), original)
				require.NoError(t, err)
				input.Manifest.Sources.Original.Tree = tree
				report, err := prepareWorkerArchives(t.Context(), git, input, tree)
				if ContainsCredentialMaterial([]byte(content)) {
					require.Error(t, err)
					require.Nil(t, report)
				} else {
					require.NoError(t, err)
					require.NoError(t, ValidatePreparationReport(input, *report))
					require.Equal(t, input.Manifest.Files, report.Manifest.Files)
					require.Equal(t, input.Patch, report.Provenance[report.Manifest.Sources.PatchDigest])
				}
			})
		}
	}
}

func TestScopedWorkerPreparationImportsOnlySelectedPathsAndAppliesPatch(t *testing.T) {
	input := workerInputFixture(t, VerifyPatch)
	repository := t.TempDir()
	sourceFixtureGit(t, repository, nil, "init", "--quiet")
	sourceFixtureWrite(t, filepath.Join(repository, "pkg/sub/value.go"), "package sub\ntype Opts struct{ Password string }\nfunc build(info Opts) Opts { return Opts{Password: info.Password} }\n", 0600)
	sourceFixtureWrite(t, filepath.Join(repository, "pkg/other/other.go"), "package other\nfunc Name() string { return \"original\" }\n", 0600)
	sourceFixtureWrite(t, filepath.Join(repository, "vendor/dep/huge.txt"), strings.Repeat("x", MaxPublicSourceArchiveBytes+1024)+"\n", 0600)
	sourceFixtureGit(t, repository, nil, "add", ".")
	sourceFixtureGit(t, repository, nil, "commit", "--quiet", "-m", "synthetic large repository")
	commit := strings.TrimSpace(string(sourceFixtureGit(t, repository, nil, "rev-parse", "HEAD")))
	rootTree := strings.TrimSpace(string(sourceFixtureGit(t, repository, nil, "rev-parse", "HEAD^{tree}")))

	input.Manifest.Sources.Original.Commit = commit
	input.Manifest.Sources.Original.Tree = rootTree
	input.Patch = []byte("--- a/pkg/other/other.go\n+++ b/pkg/other/other.go\n@@ -1,2 +1,2 @@\n package other\n-func Name() string { return \"original\" }\n+func Name() string { return \"patched\" }\n")
	input.Manifest.DeclaredChanges = []DeclaredChange{{Kind: "source", Paths: []string{"pkg/other/other.go"}, Description: "synthetic scoped patch"}}

	objectsDir := filepath.Join(repository, ".git", "objects")
	unscopedGit := sourceTestGit(t, sourceSHA1)
	fullTree, err := unscopedGit.importCommit(t.Context(), objectsDir, commit)
	require.NoError(t, err)
	_, err = prepareWorkerArchives(t.Context(), unscopedGit, input, fullTree)
	require.Error(t, err, "unscoped archive must fail when unselected vendor files exceed MaxPublicSourceArchiveBytes")

	input.Manifest.Sources.Paths = []string{"pkg/other/other.go", "pkg/sub/value.go"}
	require.NoError(t, ValidatePreparationInput(input))
	scopedGit := sourceTestGit(t, sourceSHA1)
	resolvedTree, entries, err := scopedGit.resolveScopedCommitEntries(t.Context(), objectsDir, commit, input.Manifest.Sources.Paths)
	require.NoError(t, err)
	require.Equal(t, rootTree, resolvedTree)
	require.Len(t, entries, 2)
	scopedTree, err := scopedGit.importScopedBlobs(t.Context(), objectsDir, entries, len(rootTree))
	require.NoError(t, err)
	report, err := prepareWorkerArchives(t.Context(), scopedGit, input, scopedTree)
	require.NoError(t, err)
	require.NoError(t, ValidatePreparationReport(input, *report))
	require.Equal(t, rootTree, report.Manifest.Sources.Original.Tree)
	require.Equal(t, []string{"pkg/other/other.go", "pkg/sub/value.go"}, report.Manifest.Sources.Paths)

	for _, item := range []struct {
		source SourceIdentity
		want   string
	}{
		{report.Manifest.Sources.Original, "package other\nfunc Name() string { return \"original\" }\n"},
		{report.Manifest.Sources.Patched, "package other\nfunc Name() string { return \"patched\" }\n"},
	} {
		reader := tar.NewReader(bytes.NewReader(report.Provenance[item.source.ArchiveDigest]))
		files := make(map[string]string)
		for {
			header, err := reader.Next()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			if header.Typeflag == tar.TypeReg {
				content, err := io.ReadAll(reader)
				require.NoError(t, err)
				files[header.Name] = string(content)
			}
		}
		require.Equal(t, item.want, files["pkg/other/other.go"])
	}
}
