package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	testSHA     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testDigest  = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testBranch  = "release-0.2"
	testVersion = "v0.2.0"
)

type apiRequest struct {
	path, method string
	payload      map[string]any
	paginated    bool
}

type releaseFixture struct {
	w               *workflow
	root, directory string
	data            candidateBundle
	env             map[string]string
	clock           time.Time
	commands        []commandSpec
	requests        []apiRequest
	api             func(apiRequest) (any, error, bool)
	command         func(commandSpec) (commandResult, bool)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantError(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), contains) {
		t.Fatalf("want error containing %q, got %v", contains, err)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte(contents), 0o600))
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	must(t, err)
	return string(data)
}

func hashTestFile(t *testing.T, path string) string {
	t.Helper()
	digest, err := fileHash(path)
	must(t, err)
	return digest
}

func validEnvironment() map[string]any {
	return map[string]any{
		"deployment_branch_policy": map[string]any{"custom_branch_policies": true}, "can_admins_bypass": false,
		"protection_rules": []any{map[string]any{"type": "required_reviewers", "reviewers": []any{map[string]any{"id": 42}}}},
	}
}

func testRun(name string) workflowRun {
	return workflowRun{ID: 123, RunAttempt: 1, Status: "in_progress", Event: "workflow_dispatch",
		Path: ".github/workflows/" + name, HeadSHA: testSHA, HeadBranch: testBranch,
		Repository: namedRepository{FullName: repository}, HeadRepository: namedRepository{FullName: repository},
		DisplayTitle: "Release v0.2.0 (prepare-10-1)", HTMLURL: "https://github.com/" + repository + "/actions/runs/123"}
}

func newReleaseFixture(t *testing.T) *releaseFixture {
	t.Helper()
	f := &releaseFixture{root: t.TempDir(), clock: time.Unix(0, 0), env: map[string]string{
		"GITHUB_REPOSITORY": repository, "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/" + testBranch,
		"GITHUB_SHA": testSHA, "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "1",
	}}
	f.directory = filepath.Join(f.root, "bundle")
	f.w = newWorkflow(f.root)
	f.w.stdout = io.Discard
	f.w.env = func(key string) string { return f.env[key] }
	f.w.now = func() time.Time { return f.clock }
	f.w.sleep = func(wait time.Duration) { f.clock = f.clock.Add(wait) }
	f.w.get = func(string) ([]byte, error) {
		t.Fatal("unexpected HTTP request")
		return nil, errors.New("unexpected HTTP request")
	}
	f.w.run = func(spec commandSpec) commandResult { return f.run(t, spec) }
	for _, name := range imageNames {
		writeTestFile(t, filepath.Join(f.directory, "digests", "digest-"+name+".txt"), testDigest)
	}
	writeTestFile(t, filepath.Join(f.directory, "orka-0.2.0.tgz"), "packaged chart bytes")
	for _, target := range cliTargets {
		path := filepath.Join(f.directory, target.archiveName(testVersion))
		writeTestFile(t, path, "CLI archive "+target.os+"/"+target.arch)
	}
	must(t, writeCLIChecksums(f.directory, testVersion))
	writeTestFile(t, filepath.Join(f.directory, cliChecksumFile(testVersion)+".bundle"), "signature bundle fixture")
	must(t, f.w.bundle(f.directory, testVersion, testSHA, "1"))
	var err error
	f.data, err = loadBundle(f.directory)
	must(t, err)
	return f
}

func (f *releaseFixture) run(t *testing.T, spec commandSpec) commandResult {
	t.Helper()
	f.commands = append(f.commands, spec)
	if f.command != nil {
		if result, handled := f.command(spec); handled {
			return result
		}
	}
	args := spec.args
	if len(args) >= 3 && args[0] == "gh" && args[1] == "api" {
		request := apiRequest{path: args[2], method: http.MethodGet, paginated: slices.Contains(args, "--paginate")}
		if request.paginated {
			request.path = args[len(args)-1]
		}
		if index := slices.Index(args, "--method"); index >= 0 {
			request.method = args[index+1]
		}
		if spec.input != "" {
			must(t, json.Unmarshal([]byte(spec.input), &request.payload))
		}
		f.requests = append(f.requests, request)
		var result any
		var err error
		handled := false
		if f.api != nil {
			result, err, handled = f.api(request)
		}
		if !handled {
			result = f.defaultAPI(t, request)
		}
		if request.paginated {
			result = []any{result}
		}
		encoded, encodeErr := json.Marshal(result)
		must(t, encodeErr)
		return commandResult{stdout: string(encoded), err: err}
	}
	if slices.Equal(args, []string{"git", "rev-parse", "HEAD"}) {
		return commandResult{stdout: f.env["GITHUB_SHA"]}
	}
	if slices.Equal(args, []string{"git", "status", "--porcelain"}) {
		return commandResult{}
	}
	t.Fatalf("unexpected external command: %v", args)
	return commandResult{err: errors.New("unexpected command")}
}

func (f *releaseFixture) defaultAPI(t *testing.T, request apiRequest) any {
	t.Helper()
	if request.method != http.MethodGet {
		t.Fatalf("unexpected API mutation: %s %s", request.method, request.path)
	}
	switch {
	case request.path == repoAPI:
		return map[string]string{"default_branch": "main"}
	case strings.Contains(request.path, "/git/ref/heads/"):
		return gitRef{Object: gitObject{SHA: f.env["GITHUB_SHA"]}}
	case strings.Contains(request.path, "/deployment-branch-policies?"):
		return map[string]any{"branch_policies": []branchPolicy{{Type: "branch", Name: testBranch}}}
	case strings.Contains(request.path, "/environments/"):
		return validEnvironment()
	case strings.Contains(request.path, "/git/matching-refs/"):
		return []gitRef{}
	case strings.Contains(request.path, "/actions/workflows/") && request.paginated:
		return map[string]any{"workflow_runs": []workflowRun{}}
	default:
		t.Fatalf("unexpected API read: %s", request.path)
		return nil
	}
}

func (f *releaseFixture) report(t *testing.T) acceptanceReport {
	t.Helper()
	report := acceptanceReport{Result: "qualified", CandidateSHA: f.data.CandidateSHA,
		Release: map[string]string{"version": f.data.Version, "buildRunID": f.data.BuildRunID,
			"buildRunAttempt": f.data.BuildRunAttempt,
			"bundleSHA256":    hashTestFile(t, filepath.Join(f.directory, candidateFile)),
		},
		BuiltImages: map[string]string{}, Images: map[string]observedImage{},
		Chart: chartAcceptance{PackageSHA256: f.data.Chart.SHA256, Install: true, ContainerTask: true,
			Recovery: true, NoReplay: true, OppositeModeRejected: true},
	}
	for role, name := range imageRoles {
		report.BuiltImages[role] = f.data.Images[name]
		report.Images[role] = observedImage{RequestedImage: f.data.Images[name]}
	}
	return report
}

func (f *releaseFixture) qualification(t *testing.T) {
	t.Helper()
	must(t, writeJSON(filepath.Join(f.directory, acceptanceFile), f.report(t)))
	proof := qualificationProof{RunID: "456", RunAttempt: "1",
		CandidateSHA256:  hashTestFile(t, filepath.Join(f.directory, candidateFile)),
		AcceptanceSHA256: hashTestFile(t, filepath.Join(f.directory, acceptanceFile)),
	}
	must(t, writeJSON(filepath.Join(f.directory, qualificationFile), proof))
}

func (f *releaseFixture) record(draft bool) releaseRecord {
	metadata := metadataFor(f.data, qualificationProof{RunID: "456"})
	assets := make([]runArtifact, 0, 4+len(f.data.CLI))
	for _, name := range []string{candidateFile, qualificationFile, acceptanceFile, f.data.Chart.File} {
		assets = append(assets, runArtifact{Name: name})
	}
	for _, asset := range f.data.CLI {
		assets = append(assets, runArtifact{Name: asset.File})
	}
	return releaseRecord{ID: 789, TagName: metadata.TagName, TargetCommitish: metadata.TargetCommitish,
		Name: metadata.Name, Prerelease: &metadata.Prerelease, Body: metadata.Body, Draft: &draft,
		Assets: assets,
	}
}

func copyTestBundle(t *testing.T, source, destination string) {
	t.Helper()
	must(t, os.CopyFS(destination, os.DirFS(source)))
}

func requireNoEvidence(t *testing.T, f *releaseFixture) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(f.directory, qualificationFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("qualification failure left approval evidence: %v", err)
	}
}
