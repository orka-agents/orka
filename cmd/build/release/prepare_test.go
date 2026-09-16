package main

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type preparationFixture struct {
	*releaseFixture
	remote, checkout, main string
	dispatches             int
}

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	result := runCommand(commandSpec{dir: dir, args: append([]string{"git"}, args...)})
	if result.err != nil {
		t.Fatalf("local fixture git command failed: %v: %s", args, result.stderr)
	}
	return strings.TrimSpace(result.stdout)
}

func newPreparationFixture(t *testing.T) *preparationFixture {
	t.Helper()
	f := newReleaseFixture(t)
	p := &preparationFixture{
		releaseFixture: f, remote: filepath.Join(f.root, "origin.git"), checkout: filepath.Join(f.root, "checkout"),
	}
	testGit(t, f.root, "init", "--bare", p.remote)
	testGit(t, f.root, "clone", p.remote, p.checkout)
	p.git(t, "checkout", "--orphan", "main")
	p.git(t, "config", "user.name", "Test")
	p.git(t, "config", "user.email", "test@example.invalid")
	p.git(t, "config", "commit.gpgsign", "false")
	var values strings.Builder
	for _, name := range versionedImages {
		values.WriteString(name + ":\n  image:\n    repository: " + imageRepository(name) + "\n    tag: \"0.1.1\"\n")
	}
	values.WriteString("controllerRuntime:\n")
	for _, provider := range versionedRuntimeProviders {
		values.WriteString("  " + provider + "Image: " + imageRepository("acp-"+provider+"-runtime") + ":0.1.1\n")
	}
	values.WriteString("other:\n  image:\n    repository: ghcr.io/example/other\n    tag: \"2.7.0\"\n")
	files := map[string]string{
		"cmd/build/helmify/static/Chart.yaml":  "apiVersion: v2\nname: orka\nversion: 0.1.1\nappVersion: \"v0.1.1\"\n",
		"cmd/build/helmify/static/values.yaml": values.String(),
		"config/manager/manager.yaml":          "initial\n", "config/manager/kustomization.yaml": "initial\n",
		"cmd/build/release/main.go":      "package main // release tooling fixture\n",
		"manifest_staging/generated.txt": "initial\n",
		"deploy/generated.txt":           "initial\n", "charts/orka/generated.txt": "initial\n",
		"Makefile": "VERSION := v0.1.1\n" +
			"release-manifest:\n\t@printf '%s\\n' '$(NEWVERSION)' > manifest_staging/generated.txt\n" +
			"promote-staging-manifest:\n\t@cp manifest_staging/generated.txt deploy/generated.txt\n" +
			"\t@cp manifest_staging/generated.txt charts/orka/generated.txt\n" +
			"verify-release-manifest:\n\t@test \"$$(cat deploy/generated.txt)\" = '$(NEWVERSION)'\n",
	}
	for name, content := range files {
		writeTestFile(t, filepath.Join(p.checkout, name), content)
	}
	p.git(t, "add", ".")
	p.git(t, "commit", "-m", "initial")
	p.main = p.git(t, "rev-parse", "HEAD")
	p.git(t, "push", "origin", "main")
	f.w.root = p.checkout
	f.env["GITHUB_REF"], f.env["GITHUB_SHA"] = "refs/heads/main", p.main
	f.command = func(spec commandSpec) (commandResult, bool) {
		if spec.args[0] == "gh" {
			return commandResult{}, false
		}
		spec.stream = false
		return runCommand(spec), true
	}
	f.api = func(request apiRequest) (any, error, bool) { return p.apiRequest(t, request) }
	return p
}

func (p *preparationFixture) git(t *testing.T, args ...string) string {
	t.Helper()
	return testGit(t, p.checkout, args...)
}

func (p *preparationFixture) ref(t *testing.T, name string) string {
	t.Helper()
	return testGit(t, p.root, "--git-dir", p.remote, "rev-parse", name)
}

func (p *preparationFixture) apiRequest(t *testing.T, request apiRequest) (any, error, bool) {
	t.Helper()
	if strings.Contains(request.path, "/git/ref/heads/") {
		branch := strings.Split(request.path, "/git/ref/heads/")[1]
		return gitRef{Object: gitObject{SHA: p.ref(t, "refs/heads/"+branch)}}, nil, true
	}
	if strings.HasSuffix(request.path, "/git/matching-refs/heads/"+testBranch) {
		refs := testGit(t, p.root, "--git-dir", p.remote, "for-each-ref",
			"--format=%(refname) %(objectname)", "refs/heads/"+testBranch)
		result := []gitRef{}
		for line := range strings.SplitSeq(refs, "\n") {
			if line == "" {
				continue
			}
			parts := strings.Fields(line)
			result = append(result, gitRef{Ref: parts[0], Object: gitObject{SHA: parts[1]}})
		}
		return result, nil, true
	}
	if strings.HasSuffix(request.path, "/dispatches") {
		if request.method != http.MethodPost || request.payload["ref"] != testBranch {
			t.Fatal("wrong release dispatch")
		}
		inputs := request.payload["inputs"].(map[string]any)
		if inputs["candidate_sha"] != p.ref(t, "refs/heads/"+testBranch) {
			t.Fatal("dispatched wrong candidate")
		}
		p.dispatches++
		return nil, nil, true
	}
	if strings.Contains(request.path, "/runs?event=") {
		runs := []workflowRun{}
		if p.dispatches != 0 {
			run := testRun(releaseWorkflow)
			run.HeadSHA = p.ref(t, "refs/heads/"+testBranch)
			run.DisplayTitle = "Release (prepare-123-1)"
			runs = append(runs, run)
		}
		return map[string]any{"workflow_runs": runs}, nil, true
	}
	return nil, nil, false
}

func TestPreparationCreatesReleaseBranchAndLeavesMainUnchanged(t *testing.T) {
	p := newPreparationFixture(t)
	must(t, p.w.prepare(testVersion))
	candidate := p.ref(t, "refs/heads/"+testBranch)
	if p.ref(t, "refs/heads/main") != p.main || candidate == p.main || p.git(t, "rev-parse", candidate+"^") != p.main {
		t.Fatal("preparation changed main or lost the source parent")
	}
	if !strings.Contains(p.git(t, "log", "-1", "--format=%B"), "Signed-off-by: github-actions[bot]") {
		t.Fatal("missing signoff")
	}
	generated := strings.TrimSpace(readTestFile(t, filepath.Join(p.checkout, "deploy/generated.txt")))
	if generated != testVersion || p.dispatches != 1 {
		t.Fatal("preparation did not generate and dispatch the release")
	}
}

func TestExistingReleaseLineIsParentAndConcurrentPushIsPreserved(t *testing.T) {
	p := newPreparationFixture(t)
	p.git(t, "checkout", "-b", testBranch)
	writeTestFile(t, filepath.Join(p.checkout, "backport.txt"), "existing release backport\n")
	p.git(t, "add", "backport.txt")
	p.git(t, "commit", "-m", "backport")
	base := p.git(t, "rev-parse", "HEAD")
	p.git(t, "push", "origin", testBranch)
	p.git(t, "checkout", "main")
	must(t, p.w.prepare(testVersion))
	if p.git(t, "rev-parse", "HEAD^") != base || p.ref(t, "refs/heads/main") != p.main {
		t.Fatal("release lost its backport parent")
	}
	if readTestFile(t, filepath.Join(p.checkout, "backport.txt")) != "existing release backport\n" {
		t.Fatal("backport content changed")
	}
	testGit(t, p.root, "--git-dir", p.remote, "update-ref", "refs/heads/"+testBranch, base)
	p.git(t, "checkout", "main")
	p.dispatches = 0
	command := p.command
	p.command = func(spec commandSpec) (commandResult, bool) {
		leased := slices.ContainsFunc(spec.args, func(arg string) bool {
			return strings.HasPrefix(arg, "--force-with-lease=")
		})
		if slices.Contains(spec.args, "push") && leased {
			testGit(t, p.root, "--git-dir", p.remote, "update-ref", "refs/heads/"+testBranch, p.main)
		}
		return command(spec)
	}
	wantError(t, p.w.prepare(testVersion), "expected branch head may also have changed")
	if p.dispatches != 0 || p.ref(t, "refs/heads/"+testBranch) != p.main {
		t.Fatal("preparation overwrote or dispatched a raced branch")
	}
}

func (p *preparationFixture) publishUntrustedFixture(t *testing.T, path, contents string) string {
	t.Helper()
	p.git(t, "checkout", "-B", testBranch, "main")
	writeTestFile(t, filepath.Join(p.checkout, path), contents)
	p.git(t, "add", path)
	p.git(t, "commit", "-m", "unreviewed release tooling")
	base := p.git(t, "rev-parse", "HEAD")
	p.git(t, "push", "--force", "origin", testBranch)
	p.git(t, "checkout", "main")
	return base
}

func (p *preparationFixture) requirePreparationStopped(t *testing.T, base string) {
	t.Helper()
	wantError(t, p.w.prepare(testVersion), "release automation differs")
	if p.dispatches != 0 || p.git(t, "rev-parse", "HEAD") != p.main || p.ref(t, "refs/heads/"+testBranch) != base {
		t.Fatal("untrusted tooling reached checkout or dispatch")
	}
	if readTestFile(t, filepath.Join(p.checkout, "deploy/generated.txt")) != "initial\n" {
		t.Fatal("untrusted generator ran")
	}
	if _, err := os.Stat(filepath.Join(p.checkout, "untrusted-command-ran")); !os.IsNotExist(err) {
		t.Fatal("untrusted command ran")
	}
}

func TestReleaseToolingChangesStopBeforeCheckoutGenerationOrDispatch(t *testing.T) {
	p := newPreparationFixture(t)
	static := filepath.Join(p.checkout, "cmd/build/helmify/static")
	chart := strings.ReplaceAll(readTestFile(t, filepath.Join(static, "Chart.yaml")), "name: orka", "name: altered")
	values := strings.ReplaceAll(readTestFile(t, filepath.Join(static, "values.yaml")), "\"2.7.0\"", "\"2.8.0\"")
	const untrustedScript = "#!/bin/sh\ntouch untrusted-command-ran\n"
	changes := map[string]string{
		"Makefile":                                          "VERSION := $(shell touch untrusted-command-ran)\n",
		"GNUmakefile":                                       "release-manifest:\n\t@touch untrusted-command-ran\n",
		".github/workflows/release.yml":                     "name: altered release workflow\n",
		"scripts/release-helper.sh":                         "# altered release automation\n",
		"cmd/build/release/main.go":                         "package main // altered release command\n",
		"cmd/build/release/untrusted.go":                    "package main\nfunc init() {}\n",
		"cmd/build/helmify/untrusted.go":                    "package main\nfunc init() {}\n",
		"cmd/build/helmify/static/templates/untrusted.yaml": "{{ fail \"unreviewed template\" }}\n",
		"cmd/build/helmify/static/Chart.yaml":               chart,
		"cmd/build/helmify/static/values.yaml":              values,
		"bin/controller-gen":                                untrustedScript,
		"vendor/modules.txt":                                "# unreviewed vendored toolchain\n",
		"go.mod":                                            "module untrusted.invalid/release\n", "go.work": "go 1.27\n",

		".agents/skills/kindctl/bin/kindctl":                                              untrustedScript,
		".agents/skills/vekil-reverse-proxy-deploy/scripts/deploy_vekil_reverse_proxy.sh": untrustedScript,
	}
	for path, contents := range changes {
		t.Run(path, func(t *testing.T) { p.requirePreparationStopped(t, p.publishUntrustedFixture(t, path, contents)) })
	}
}

func TestPreviousReleaseVersionDoesNotChangeToolingIdentity(t *testing.T) {
	for _, versions := range []struct{ previous, next string }{
		{"0.2.0", "v0.2.1"},
		{"0.2.0-rc.1", "v0.2.0-rc.2"},
		{"0.2.0-rc.2", "v0.2.0"},
	} {
		t.Run(versions.next, func(t *testing.T) {
			p := newPreparationFixture(t)
			p.git(t, "checkout", "-b", testBranch)
			for _, name := range []string{makefilePath, chartInputPath, valuesInputPath} {
				path := filepath.Join(p.checkout, name)
				writeTestFile(t, path, strings.ReplaceAll(readTestFile(t, path), "0.1.1", versions.previous))
				p.git(t, "add", name)
			}
			p.git(t, "commit", "-m", "previous release version")
			base := p.git(t, "rev-parse", "HEAD")
			p.git(t, "push", "origin", testBranch)
			p.git(t, "checkout", "main")
			must(t, p.w.prepare(versions.next))
			if p.git(t, "rev-parse", "HEAD^") != base || p.ref(t, "refs/heads/main") != p.main {
				t.Fatal("version-only change was not preserved")
			}
		})
	}
}

func TestRuntimeRepositoryChangesStopBeforeCheckoutOrGeneration(t *testing.T) {
	p := newPreparationFixture(t)
	values := readTestFile(t, filepath.Join(p.checkout, valuesInputPath))
	for _, provider := range versionedRuntimeProviders {
		t.Run(provider, func(t *testing.T) {
			changed := strings.ReplaceAll(values,
				imageRepository("acp-"+provider+"-runtime"), "registry.example/changed-runtime")
			p.requirePreparationStopped(t, p.publishUntrustedFixture(t, valuesInputPath, changed))
		})
	}
}

func TestStaticChartSymlinksStopBeforeCheckoutOrGeneration(t *testing.T) {
	p := newPreparationFixture(t)
	outside := filepath.Join(p.root, "outside-chart-fixture.txt")
	writeTestFile(t, outside, "harmless fixture outside the chart\n")
	for _, name := range []string{"cmd/build/helmify/static/Chart.yaml", "cmd/build/helmify/static/values.yaml",
		"cmd/build/helmify/static/templates/external.txt", "cmd/build/helmify/static/Chart.yaml ",
		"cmd/build/helmify/static/values.yaml "} {
		t.Run(name, func(t *testing.T) {
			p.git(t, "checkout", "-B", testBranch, "main")
			path := filepath.Join(p.checkout, name)
			must(t, os.MkdirAll(filepath.Dir(path), 0o700))
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			must(t, os.Symlink(outside, path))
			p.git(t, "add", name)
			p.git(t, "commit", "-m", "untrusted chart symlink")
			base := p.git(t, "rev-parse", "HEAD")
			p.git(t, "push", "--force", "origin", testBranch)
			p.git(t, "checkout", "main")
			p.requirePreparationStopped(t, base)
		})
	}
}

func TestVersionedInputsPreserveWhitespaceAndFileModes(t *testing.T) {
	p := newPreparationFixture(t)
	for _, name := range []string{makefilePath, chartInputPath, valuesInputPath} {
		for _, change := range []string{"whitespace", "mode"} {
			p.git(t, "checkout", "-B", testBranch, "main")
			path := filepath.Join(p.checkout, name)
			if change == "whitespace" {
				writeTestFile(t, path, "\n"+readTestFile(t, path))
			} else {
				must(t, os.Chmod(path, 0o755))
			}
			p.git(t, "add", name)
			p.git(t, "commit", "-m", "changed preparation input identity")
			base := p.git(t, "rev-parse", "HEAD")
			p.git(t, "checkout", "main")
			wantError(t, p.w.checkPreparationAutomation(p.main, base), "release automation differs")
			if p.git(t, "rev-parse", "HEAD") != p.main {
				t.Fatal("identity check changed checkout")
			}
		}
	}
}

func TestUnexpectedUntrackedGenerationOutputStopsBeforePush(t *testing.T) {
	p := newPreparationFixture(t)
	path := filepath.Join(p.checkout, "Makefile")
	changed := strings.Replace(readTestFile(t, path),
		"release-manifest:\n", "release-manifest:\n\t@touch unexpected.txt\n", 1)
	writeTestFile(t, path, changed)
	p.git(t, "add", "Makefile")
	p.git(t, "commit", "-m", "fixture generator writes an unexpected path")
	p.main = p.git(t, "rev-parse", "HEAD")
	p.env["GITHUB_SHA"] = p.main
	p.git(t, "push", "origin", "main")
	wantError(t, p.w.prepare(testVersion), "unexpected source file")
	refs := testGit(t, p.root, "--git-dir", p.remote, "for-each-ref", "--format=%(refname)", "refs/heads/"+testBranch)
	if refs != "" || p.dispatches != 0 {
		t.Fatal("unexpected generation was pushed or dispatched")
	}
}

func TestExactChartPublicationPreservesSiteRepairsIndexAndRetries(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("Helm is required for the chart publication integration test")
	}
	f := newReleaseFixture(t)
	source := filepath.Join(f.root, "chart")
	writeTestFile(t, filepath.Join(source, "Chart.yaml"), "apiVersion: v2\nname: orka\nversion: 0.2.0\n")
	result := runCommand(commandSpec{dir: f.root, args: []string{"helm", "package", source, "--destination", f.directory}})
	if result.err != nil {
		t.Fatalf("helm package: %s", result.stderr)
	}
	must(t, f.w.bundle(f.directory, testVersion, testSHA, "1"))
	var err error
	f.data, err = loadBundle(f.directory)
	must(t, err)
	remote, checkout := filepath.Join(f.root, "pages.git"), filepath.Join(f.root, "pages")
	testGit(t, f.root, "init", "--bare", remote)
	testGit(t, f.root, "clone", remote, checkout)
	testGit(t, checkout, "checkout", "--orphan", pagesBranch)
	writeTestFile(t, filepath.Join(checkout, "index.html"), "preserved website")
	must(t, os.MkdirAll(filepath.Join(checkout, "charts"), 0o700))
	must(t, copyFile(filepath.Join(f.directory, f.data.Chart.File), filepath.Join(checkout, "charts", f.data.Chart.File)))
	testGit(t, checkout, "add", ".")
	testGit(t, checkout, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false",
		"commit", "-m", "existing chart, missing index")
	testGit(t, checkout, "push", "origin", pagesBranch)
	f.api = func(apiRequest) (any, error, bool) { return []gitRef{{Ref: "refs/heads/" + pagesBranch}}, nil, true }
	f.command = func(spec commandSpec) (commandResult, bool) {
		if spec.args[0] == "gh" {
			return commandResult{}, false
		}
		if len(spec.args) >= 4 && slices.Equal(spec.args[:4], []string{"git", "remote", "add", "origin"}) {
			spec.args = []string{"git", "remote", "add", "origin", remote}
		}
		return runCommand(spec), true
	}
	first, firstHash, err := f.w.publishChart(f.data, f.directory)
	must(t, err)
	second, secondHash, err := f.w.publishChart(f.data, f.directory)
	must(t, err)
	if first != second || firstHash != secondHash {
		t.Fatal("identical publication retry created another commit")
	}
	if testGit(t, f.root, "--git-dir", remote, "show", "gh-pages:index.html") != "preserved website" {
		t.Fatal("publication replaced website")
	}
	index := testGit(t, f.root, "--git-dir", remote, "show", "gh-pages:charts/index.yaml")
	if !strings.Contains(index, f.data.Chart.SHA256) {
		t.Fatal("index does not contain tested chart digest")
	}
	archive := runCommand(commandSpec{
		dir: f.root, args: []string{"git", "--git-dir", remote, "show", "gh-pages:charts/" + f.data.Chart.File},
	})
	must(t, archive.err)
	if hashBytes([]byte(archive.stdout)) != f.data.Chart.SHA256 {
		t.Fatal("publication changed the tested chart bytes")
	}
}
