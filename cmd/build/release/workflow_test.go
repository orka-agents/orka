package main

import (
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestVersionsAlwaysTargetReleaseLines(t *testing.T) {
	for _, version := range []string{testVersion, "v0.2.0-beta.1", "v0.2.0-rc.1"} {
		branch, err := branchFor(version)
		must(t, err)
		if branch != testBranch {
			t.Fatalf("wrong branch %q for %q", branch, version)
		}
	}
	for _, version := range []string{"0.2.0", "v00.2.0", "v0.02.0", "v0.2.00", "v0.2.0-dev", "v0.2.0-rc.01", "v0.2.0\n"} {
		_, err := branchFor(version)
		wantError(t, err, "version must")
	}
}

func TestEnvironmentRequiresExplicitBranchAndRealApproval(t *testing.T) {
	type scenario struct {
		config   map[string]any
		policies []branchPolicy
		valid    bool
	}
	policy := branchPolicy{Type: "branch", Name: testBranch}
	base := validEnvironment()
	cases := make([]scenario, 0, 16)
	cases = append(cases, scenario{config: base, policies: []branchPolicy{policy}, valid: true})
	for key, value := range map[string]any{
		"protection_rules": []any{}, "can_admins_bypass": true, "deployment_branch_policy": nil,
	} {
		config := maps.Clone(base)
		config[key] = value
		cases = append(cases, scenario{config: config, policies: []branchPolicy{policy}})
	}
	config := maps.Clone(base)
	config["protection_rules"] = []any{map[string]any{"type": "required_reviewers", "reviewers": []any{}}}
	cases = append(cases, scenario{config: config, policies: []branchPolicy{policy}})
	config = maps.Clone(base)
	delete(config, "can_admins_bypass")
	cases = append(cases, scenario{config: config, policies: []branchPolicy{policy}})
	for _, bad := range []branchPolicy{{Type: "branch", Name: "release-*"}, {Type: "tag", Name: testBranch},
		{Type: "branch", Name: "feature/unreviewed"}, {Type: "branch", Name: "release-00.2"}} {
		cases = append(cases, scenario{config: base, policies: []branchPolicy{bad}},
			scenario{config: base, policies: []branchPolicy{policy, bad}})
	}
	for _, name := range []string{"release", qualificationEnvironment} {
		for index, entry := range cases {
			f := newReleaseFixture(t)
			f.api = func(request apiRequest) (any, error, bool) {
				if strings.Contains(request.path, "/deployment-branch-policies?") {
					return map[string]any{"branch_policies": entry.policies}, nil, true
				}
				if strings.HasSuffix(request.path, "/environments/"+name) {
					return entry.config, nil, true
				}
				return nil, nil, false
			}
			err := f.w.checkEnvironment(name, testBranch)
			if (err == nil) != entry.valid {
				t.Fatalf("%s case %d: %v", name, index, err)
			}
		}
	}
	for _, defaultBranch := range []string{"main", "trunk"} {
		f := newReleaseFixture(t)
		policies := make([]branchPolicy, 0, 3)
		policies = append(policies, policy, branchPolicy{Type: "branch", Name: "release-1.0"})
		f.api = func(request apiRequest) (any, error, bool) {
			if request.path == repoAPI {
				return map[string]string{"default_branch": defaultBranch}, nil, true
			}
			if strings.Contains(request.path, "/deployment-branch-policies?") {
				return map[string]any{"branch_policies": policies}, nil, true
			}
			return nil, nil, false
		}
		must(t, f.w.checkEnvironment("release", testBranch))
		policies = append(policies, branchPolicy{Type: "branch", Name: defaultBranch})
		must(t, f.w.checkEnvironment(qualificationEnvironment, testBranch))
		must(t, f.w.checkEnvironment(qualificationEnvironment, defaultBranch))
		wantError(t, f.w.checkEnvironment("release", testBranch), "unrelated branch")
	}
}

func TestMissingEnvironmentStopsPreparationBeforePushOrDispatch(t *testing.T) {
	f := newReleaseFixture(t)
	f.env["GITHUB_REF"] = "refs/heads/main"
	f.api = func(request apiRequest) (any, error, bool) {
		if strings.Contains(request.path, "/environments/") {
			return nil, errors.New("environment absent"), true
		}
		return nil, nil, false
	}
	wantError(t, f.w.prepare(testVersion), "GitHub API")
	for _, call := range f.commands {
		if slices.Contains(call.args, "push") {
			t.Fatal("pushed before checking environment")
		}
	}
	for _, request := range f.requests {
		if request.method != http.MethodGet {
			t.Fatal("dispatched before checking environment")
		}
	}
}

func TestCandidateMustBeCheckedOutDispatchAndCurrentHead(t *testing.T) {
	f := newReleaseFixture(t)
	must(t, f.w.checkContext(testBranch, testSHA))
	for key, value := range map[string]string{"GITHUB_REF": "refs/heads/main", "GITHUB_SHA": strings.Repeat("c", 40),
		"GITHUB_EVENT_NAME": "push", "GITHUB_REPOSITORY": "external/orka"} {
		previous := f.env[key]
		f.env[key] = value
		wantError(t, f.w.checkContext(testBranch, testSHA), "release must run")
		f.env[key] = previous
	}
	f.api = func(request apiRequest) (any, error, bool) {
		if strings.Contains(request.path, "/git/ref/heads/") {
			return gitRef{Object: gitObject{SHA: strings.Repeat("c", 40)}}, nil, true
		}
		return nil, nil, false
	}
	wantError(t, f.w.checkContext(testBranch, testSHA), "branch moved")
	f.command = func(spec commandSpec) (commandResult, bool) {
		if slices.Equal(spec.args, []string{"git", "rev-parse", "HEAD"}) {
			return commandResult{stdout: strings.Repeat("d", 40)}, true
		}
		return commandResult{}, false
	}
	wantError(t, f.w.checkContext(testBranch, testSHA), "checkout differs")
}

func TestBundleRejectsChangedChartImagesAndRunIdentity(t *testing.T) {
	f := newReleaseFixture(t)
	changes := []func(*candidateBundle){
		func(data *candidateBundle) { data.CandidateSHA = "not-a-sha" },
		func(data *candidateBundle) { data.BuildRunAttempt = "0" },
		func(data *candidateBundle) { data.Repository = "external/orka" },
		func(data *candidateBundle) { data.Branch = "main" },
		func(data *candidateBundle) {
			data.Images = map[string]string{"controller": "ghcr.io/orka-agents/orka:latest"}
		},
		func(data *candidateBundle) { data.Chart.File = "../outside" },
	}
	for _, change := range changes {
		data := f.data
		change(&data)
		must(t, writeJSON(filepath.Join(f.directory, candidateFile), data))
		_, err := loadBundle(f.directory)
		wantError(t, err, "")
	}
	must(t, writeJSON(filepath.Join(f.directory, candidateFile), f.data))
	writeTestFile(t, filepath.Join(f.directory, f.data.Chart.File), "different package")
	_, err := loadBundle(f.directory)
	wantError(t, err, "chart bytes changed")
}

func TestQualificationBindsPackagedChartAndObservedImages(t *testing.T) {
	f := newReleaseFixture(t)
	must(t, verifyReport(f.directory, f.report(t)))
	for _, field := range []string{"buildRunID", "buildRunAttempt", "version", "bundleSHA256", "unexpected"} {
		report := f.report(t)
		report.Release[field] = "different"
		wantError(t, verifyReport(f.directory, report), "different release artifacts")
	}
	for _, change := range []func(*acceptanceReport){
		func(r *acceptanceReport) { r.Chart.Install = false }, func(r *acceptanceReport) { r.Chart.ContainerTask = false },
		func(r *acceptanceReport) { r.Chart.Recovery = false }, func(r *acceptanceReport) { r.Chart.NoReplay = false },
		func(r *acceptanceReport) { r.Chart.OppositeModeRejected = false },
		func(r *acceptanceReport) {
			r.Images["codex"] = observedImage{RequestedImage: "image@sha256:" + strings.Repeat("c", 64)}
		},
	} {
		report := f.report(t)
		change(&report)
		wantError(t, verifyReport(f.directory, report), "")
	}
}

func TestBundleDownloadRechecksRunAttemptAndArtifactIdentity(t *testing.T) {
	artifact := runArtifact{Name: "release-candidate-123-1", Expired: new(false)}
	type scenario struct {
		before, after workflowRun
		artifacts     []runArtifact
		valid         bool
	}
	good := scenario{
		before: testRun(releaseWorkflow), after: testRun(releaseWorkflow), artifacts: []runArtifact{artifact}, valid: true,
	}
	cases := make([]scenario, 0, 12)
	cases = append(cases, good)
	for _, change := range []func(*scenario){
		func(s *scenario) { s.before.HeadSHA = strings.Repeat("c", 40) },
		func(s *scenario) { s.before.HeadRepository.FullName = "external/orka" },
		func(s *scenario) { s.before.Status = "completed" }, func(s *scenario) { s.after.RunAttempt = 2 },
		func(s *scenario) { s.after.Status = "completed" },
		func(s *scenario) { s.artifacts = []runArtifact{artifact, artifact} },
		func(s *scenario) { s.artifacts = []runArtifact{{Name: artifact.Name, Expired: new(true)}} },
		func(s *scenario) { s.artifacts = []runArtifact{{Name: artifact.Name}} },
		func(s *scenario) { s.artifacts = []runArtifact{{Name: "release-candidate-123-2"}} },
	} {
		entry := good
		entry.valid = false
		change(&entry)
		cases = append(cases, entry)
	}
	for index, entry := range cases {
		f := newReleaseFixture(t)
		reads := 0
		f.api = func(request apiRequest) (any, error, bool) {
			if strings.Contains(request.path, "/artifacts?") {
				return map[string]any{"artifacts": entry.artifacts}, nil, true
			}
			if request.path == runPath(123) {
				reads++
				if reads == 1 {
					return entry.before, nil, true
				}
				return entry.after, nil, true
			}
			return nil, nil, false
		}
		f.command = func(spec commandSpec) (commandResult, bool) {
			if len(spec.args) >= 3 && slices.Equal(spec.args[:3], []string{"gh", "run", "download"}) {
				copyTestBundle(t, f.directory, spec.args[len(spec.args)-1])
				return commandResult{}, true
			}
			return commandResult{}, false
		}
		err := f.w.downloadBundle("123", "1", testBranch, testSHA, filepath.Join(f.root, "download"))
		if (err == nil) != entry.valid {
			t.Fatalf("case %d: %v", index, err)
		}
	}
}

func TestDispatchCorrelatesNewRunInsteadOfReusingOldSuccess(t *testing.T) {
	f := newReleaseFixture(t)
	previous := testRun(releaseWorkflow)
	previous.ID = 120
	previous.Status = "completed"
	previous.Conclusion = "success"
	unrelated := testRun(releaseWorkflow)
	unrelated.ID = 121
	unrelated.DisplayTitle = "Release (someone-else)"
	expected := testRun(releaseWorkflow)
	reads := 0
	f.api = func(request apiRequest) (any, error, bool) {
		if strings.HasSuffix(request.path, "/dispatches") {
			return nil, nil, true
		}
		if strings.Contains(request.path, "/runs?event=") {
			reads++
			runs := []workflowRun{previous}
			if reads >= 2 {
				runs = append(runs, unrelated)
			}
			if reads >= 3 {
				runs = append(runs, expected)
			}
			return map[string]any{"workflow_runs": runs}, nil, true
		}
		return nil, nil, false
	}
	run, err := f.w.dispatch(releaseWorkflow, testBranch, testSHA, map[string]string{"dispatch_id": "prepare-10-1"}, false)
	must(t, err)
	if run.ID != 123 || reads != 3 {
		t.Fatalf("wrong dispatch run: %+v, reads %d", run, reads)
	}
}

func TestFailedUnobservedAmbiguousAndMovedDispatchesStop(t *testing.T) {
	for _, failure := range []string{"rejected", "unobserved", "ambiguous", "moved"} {
		t.Run(failure, func(t *testing.T) {
			f := newReleaseFixture(t)
			reads := 0
			f.api = func(request apiRequest) (any, error, bool) {
				if strings.Contains(request.path, "/git/ref/heads/") && failure == "moved" {
					return gitRef{Object: gitObject{SHA: strings.Repeat("c", 40)}}, nil, true
				}
				if strings.HasSuffix(request.path, "/dispatches") {
					if failure == "rejected" {
						return nil, errors.New("rejected"), true
					}
					return nil, nil, true
				}
				if strings.Contains(request.path, "/runs?event=") {
					reads++
					runs := []workflowRun{}
					if failure == "ambiguous" && reads > 1 {
						other := testRun(releaseWorkflow)
						other.ID = 124
						runs = []workflowRun{testRun(releaseWorkflow), other}
					}
					return map[string]any{"workflow_runs": runs}, nil, true
				}
				return nil, nil, false
			}
			_, err := f.w.dispatch(releaseWorkflow, testBranch, testSHA, map[string]string{"dispatch_id": "prepare-10-1"}, false)
			wantError(t, err, "")
			if failure == "moved" && len(f.requests) != 1 {
				t.Fatal("moved branch caused dispatch API calls")
			}
		})
	}
}

func qualificationSequence(f *releaseFixture, states []string, conclusion string) {
	lists, next := 0, 0
	f.api = func(request apiRequest) (any, error, bool) {
		if request.paginated {
			return nil, nil, false
		}
		if strings.HasSuffix(request.path, "/dispatches") || strings.HasSuffix(request.path, "/cancel") {
			return nil, nil, true
		}
		run := testRun(qualificationWorkflow)
		run.ID = 456
		run.DisplayTitle = "Release Qualification (release-123-1)"
		if strings.Contains(request.path, "/runs?event=") {
			lists++
			if lists == 1 {
				return map[string]any{"workflow_runs": []workflowRun{}}, nil, true
			}
			run.Status = states[0]
			run.Conclusion = conclusion
			return map[string]any{"workflow_runs": []workflowRun{run}}, nil, true
		}
		if request.path == runPath(456) {
			next++
			run.Status = states[next]
			run.Conclusion = conclusion
			return run, nil, true
		}
		return nil, nil, false
	}
}

func TestFailedLiveGateNeverPreparesApprovalEvidence(t *testing.T) {
	f := newReleaseFixture(t)
	qualificationSequence(f, []string{"completed"}, "failure")
	wantError(t, f.w.qualify(f.directory), "qualification failed")
	requireNoEvidence(t, f)
}

func TestBusyLiveGateStopsBeforeDispatchOrApprovalEvidence(t *testing.T) {
	for _, status := range []string{"queued", "in_progress", "waiting", "requested", "pending"} {
		f := newReleaseFixture(t)
		f.api = func(request apiRequest) (any, error, bool) {
			if request.paginated {
				run := testRun(qualificationWorkflow)
				run.Status = status
				return map[string]any{"workflow_runs": []workflowRun{run}}, nil, true
			}
			return nil, nil, false
		}
		wantError(t, f.w.qualify(f.directory), "another release qualification run")
		requireNoEvidence(t, f)
		for _, request := range f.requests {
			if request.method != http.MethodGet {
				t.Fatal("busy qualification dispatched another run")
			}
		}
	}
}

func TestRacedQualificationCancelsOnlyItsQueuedChildWithoutApprovalEvidence(t *testing.T) {
	for _, states := range [][]string{{"pending"}, {"queued", "pending"}} {
		f := newReleaseFixture(t)
		qualificationSequence(f, states, "")
		wantError(t, f.w.qualify(f.directory), "queued behind another run; cancellation requested")
		requireNoEvidence(t, f)
		last := f.requests[len(f.requests)-1]
		if last.path != runPath(456)+"/cancel" || last.method != http.MethodPost {
			t.Fatalf("wrong cancellation: %+v", last)
		}
	}
}

func TestQualificationAllowsApprovalWaitingAndRunnerQueues(t *testing.T) {
	f := newReleaseFixture(t)
	qualificationSequence(f, []string{"queued", "waiting", "in_progress", "in_progress", "completed"}, "success")
	advances := []time.Duration{10 * time.Minute, 89 * time.Minute, 3 * time.Hour, time.Hour}
	f.w.sleep = func(time.Duration) {
		if len(advances) == 0 {
			t.Fatal("continued polling after qualification completed")
		}
		f.clock = f.clock.Add(advances[0])
		advances = advances[1:]
	}
	inputs := map[string]string{"dispatch_id": "release-123-1"}
	run, err := f.w.dispatch(qualificationWorkflow, testBranch, testSHA, inputs, true)
	must(t, err)
	if run.Status != "completed" || run.Conclusion != "success" {
		t.Fatalf("wrong completed run: %+v", run)
	}
	for _, request := range f.requests {
		if strings.HasSuffix(request.path, "/cancel") {
			t.Fatal("cancelled a healthy qualification run")
		}
	}
}

func TestQualificationWritesEvidenceForTestedArtifacts(t *testing.T) {
	f := newReleaseFixture(t)
	qualificationSequence(f, []string{"completed"}, "success")
	f.command = func(spec commandSpec) (commandResult, bool) {
		if spec.args[0] == "bash" {
			path := qualificationPath(f.root, 456, 1)
			must(t, os.MkdirAll(filepath.Dir(path), 0o700))
			must(t, writeJSON(path, f.report(t)))
			return commandResult{}, true
		}
		return commandResult{}, false
	}
	must(t, f.w.qualify(f.directory))
	var proof qualificationProof
	must(t, readJSON(filepath.Join(f.directory, qualificationFile), &proof))
	if proof.RunID != "456" || proof.RunAttempt != "1" ||
		proof.CandidateSHA256 != hashTestFile(t, filepath.Join(f.directory, candidateFile)) ||
		proof.AcceptanceSHA256 != hashTestFile(t, filepath.Join(f.directory, acceptanceFile)) {
		t.Fatalf("incorrect proof: %+v", proof)
	}
}

func TestWorkflowOutputsAndCommandsDoNotExposeResponseContents(t *testing.T) {
	f := newReleaseFixture(t)
	f.w.run = func(commandSpec) commandResult {
		return commandResult{stdout: "private response marker", stderr: "private error marker",
			err: errors.New("private error marker")}
	}
	_, err := f.w.command("git", "fetch")
	wantError(t, err, "git command failed")
	if strings.Contains(err.Error(), "private") {
		t.Fatal("command error disclosed output")
	}
	wantError(t, decodeJSON([]byte(`{"key":"private response marker", "bad": }`), &map[string]string{}), "invalid JSON")
	wantError(t, f.w.output("key", "value\nother=value"), "invalid workflow output")
}

func TestQualificationTimeoutCancelsItsChildWithoutApprovalEvidence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		states      []string
		advances    []time.Duration
		wantError   string
		cancelFails bool
	}{
		{
			name:      "approval waiting",
			states:    []string{"queued", "waiting", "waiting", "waiting"},
			advances:  []time.Duration{10 * time.Minute, 89 * time.Minute, time.Minute},
			wantError: "qualification approval timed out after 90 minutes",
		},
		{
			name:      "overall running",
			states:    []string{"in_progress", "in_progress", "in_progress"},
			advances:  []time.Duration{5*time.Hour + 39*time.Minute, time.Minute},
			wantError: "qualification timed out",
		},
		{
			name:      "overall waiting after a long runner queue",
			states:    []string{"queued", "waiting", "waiting"},
			advances:  []time.Duration{5 * time.Hour, 40 * time.Minute},
			wantError: "qualification timed out",
		},
		{
			name:        "cancellation failure",
			states:      []string{"waiting", "waiting"},
			advances:    []time.Duration{90 * time.Minute},
			wantError:   "cancellation request failed",
			cancelFails: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReleaseFixture(t)
			qualificationSequence(f, tc.states, "")
			api := f.api
			f.api = func(request apiRequest) (any, error, bool) {
				if tc.cancelFails && strings.HasSuffix(request.path, "/cancel") {
					return nil, errors.New("cancellation unavailable"), true
				}
				return api(request)
			}
			advances := tc.advances
			f.w.sleep = func(time.Duration) {
				if len(advances) == 0 {
					t.Fatal("continued polling after the qualification deadline")
				}
				f.clock = f.clock.Add(advances[0])
				advances = advances[1:]
			}
			err := f.w.qualify(f.directory)
			wantError(t, err, tc.wantError)
			if len(advances) != 0 {
				t.Fatal("stopped before the qualification deadline")
			}
			requireNoEvidence(t, f)
			last := f.requests[len(f.requests)-1]
			if last.path != runPath(456)+"/cancel" || last.method != http.MethodPost {
				t.Fatalf("wrong cancellation: %+v", last)
			}
			if tc.cancelFails && strings.Contains(err.Error(), "cancellation requested") {
				t.Fatal("reported a rejected cancellation as requested")
			}
		})
	}
}

func TestMissingRunIdentityStopsBeforeExternalEffects(t *testing.T) {
	for _, key := range []string{"GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT"} {
		f := newReleaseFixture(t)
		delete(f.env, key)
		wantError(t, f.w.prepare(testVersion), "invalid workflow run identity")
		wantError(t, f.w.validateCandidate(testVersion, testSHA), "invalid workflow run identity")
		wantError(t, f.w.qualify(f.directory), "invalid workflow run identity")
		if len(f.commands) != 0 {
			t.Fatal("missing run identity caused external effects")
		}
	}
}

func TestNullAPIResponsesAndMissingRunFieldsFailClosed(t *testing.T) {
	f := newReleaseFixture(t)
	f.command = func(commandSpec) (commandResult, bool) { return commandResult{stdout: "null"}, true }
	_, err := f.w.tagRef(testVersion)
	wantError(t, err, "missing JSON response")
	_, err = paginated[workflowRun](f.w, repoAPI+"/actions/workflows/"+releaseWorkflow+"/runs", "workflow_runs")
	wantError(t, err, "missing JSON response")
	run := testRun(releaseWorkflow)
	run.ID = 0
	wantError(t, validateRun(run, releaseWorkflow, testBranch, testSHA), "trusted candidate")
	run = testRun(releaseWorkflow)
	run.RunAttempt = 0
	wantError(t, validateRun(run, releaseWorkflow, testBranch, testSHA), "trusted candidate")
}
