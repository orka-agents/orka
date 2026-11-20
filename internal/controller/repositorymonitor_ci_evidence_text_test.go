package controller

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// repositoryMonitorEvidenceTextFixtures returns CI free text that carries a
// credential: every sanitizer regression case, and the shapes a security
// re-review found the sanitizer misses. Each value in secrets must stay out
// of the repair prompt unless the monitor opts in to CI free text.
func repositoryMonitorEvidenceTextFixtures() []repositoryMonitorEvidenceLeakCase {
	v := "Qv7Rm2" + "Lc9Wx4Tz8Np3Hs"
	b := base64.StdEncoding.EncodeToString([]byte("deploy:" + v))
	short := base64.StdEncoding.EncodeToString([]byte("u:p"))
	cases := repositoryMonitorEvidenceLeakCases()
	for _, missed := range []repositoryMonitorEvidenceLeakCase{
		{name: "docker quoted option", input: "+ docker login '-p" + v + "'", secrets: []string{v}},
		{name: "docker quoted long option", input: "+ docker login '--password' '" + v + "'", secrets: []string{v}},
		{name: "docker option continued", input: "+ docker login --password \\\n  " + v, secrets: []string{v}},
		{name: "docker option value wrapped", input: "+ docker login --password " + v[:10] + "\\\n" + v[10:], secrets: []string{v[:10], v[10:]}},
		{name: "docker quoted multiline value", input: "+ docker login -p 'violet meadow\n  copper kettle\n  amber forest'", secrets: []string{"violet meadow", "copper kettle", "amber forest"}},
		{name: "curl user quoted option", input: "+ curl '-u' 'deploy:" + v + "' https://ci.example.invalid", secrets: []string{v}},
		{name: "curl password leading spaces", input: "+ curl -u 'deploy: violet meadow copper' https://ci.example.invalid", secrets: []string{"violet meadow copper"}},
		{name: "curl user continued", input: "+ curl -u \\\n deploy:" + v + " https://ci.example.invalid", secrets: []string{v}},
		{name: "authorization custom short opaque", input: "Authorization: SSWS ab12cd", secrets: []string{"ab12cd"}},
		{name: "npm short base64", input: "//r/:_auth=" + short, secrets: []string{short}},
		{name: "docker auth escaped json", input: `{"message":"{\"auths\":{\"r\":{\"auth\":\"` + b + `\"}}}"}`, secrets: []string{b}},
		{name: "docker auth next line", input: "{\"auths\":{\"r\":{\"auth\":\n  \"" + b + "\"}}}", secrets: []string{b}},
		{name: "kubeconfig key block", input: "client-key-data: |\n  " + b, secrets: []string{b}},
		{name: "auth env block", input: "NEXUS_AUTH: |\n  " + b, secrets: []string{b}},
		{name: "azure quoted", input: "AccountKey=\"" + b + "\"", secrets: []string{b}},
		{name: "azure colon", input: "AccountKey: " + b, secrets: []string{b}},
		{name: "mask wrapped", input: "+ echo '::add-mask::" + v[:10] + "\n" + v[10:] + "'", secrets: []string{v[:10], v[10:]}},
		{name: "diagnostic quoted wrap", input: "got token \"" + v[:10] + "\n  " + v[10:] + "\"", secrets: []string{v[:10], v[10:]}},
		{name: "password tagged quote", input: "password: !!str \"violet meadow\n  copper kettle\n  amber forest\"", secrets: []string{"violet meadow", "copper kettle", "amber forest"}},
		{name: "password pwd wrap", input: "pwd: \"violet meadow\n  copper kettle\n  amber forest\"", secrets: []string{"violet meadow", "copper kettle", "amber forest"}},
		{name: "diff timestamped block", input: "2026-11-17T00:01:00Z + password: |\n2026-11-17T00:01:00Z +   violet meadow\n2026-11-17T00:01:00Z +   copper kettle", secrets: []string{"violet meadow", "copper kettle"}},
		{name: "plain multi-line yaml", input: "password:\n  violet meadow copper kettle", secrets: []string{"violet meadow copper kettle"}},
		{name: "option continued after a space", input: "+ docker login --password \\\n" + v, secrets: []string{v}},
	} {
		missed.input = "##[error]deploy failed\n\n" + missed.input + "\n\n--- FAIL: TestDeploy (0.01s)"
		cases = append(cases, missed)
	}
	return cases
}

// repositoryMonitorEvidenceTextTestGitHub reports one failing check run and
// one failing commit status, with text in every free-text field GitHub
// returns for them.
func repositoryMonitorEvidenceTextTestGitHub(text, statusURL string) *repositoryMonitorRepairLabelsGitHub {
	run := repositoryMonitorEvidenceTestRun(701, "deploy", "completed", "failure", 1)
	output := run["output"].(map[string]any)
	output["title"], output["summary"], output["text"] = text, text, text
	status := map[string]any{"id": 9101, "context": "external-ci", "state": "failure", "description": text}
	if statusURL != "" {
		status["target_url"] = statusURL
	}
	return &repositoryMonitorRepairLabelsGitHub{
		runs: map[string][]map[string]any{"head51": {run}},
		annotations: map[int64][]map[string]any{701: {{
			"path": "deploy/config.yaml", "start_line": 7, "end_line": 9, "annotation_level": "failure", "title": text, "message": text, "raw_details": text,
		}}},
		statusObjects: map[string][]map[string]any{"head51": {status}},
	}
}

func repositoryMonitorEvidenceTextTestPrompt(t *testing.T, github *repositoryMonitorRepairLabelsGitHub, optIn bool) (string, repositoryMonitorRepairCIInput) {
	t.Helper()
	reconciler, monitor := repositoryMonitorEvidenceTestReconciler(t, github, "ci-evidence-text")
	monitor.Spec.Repair.CIEvidence.Text = optIn
	pr := repositoryMonitorPullRequest{Number: 51, HeadSHA: "head51"}
	input := reconciler.buildRepositoryMonitorRepairCIInput(context.Background(), monitor, "orka-agents/orka", pr)
	prompt, err := reconciler.buildRepositoryMonitorRepairPromptWithCI(context.Background(), monitor, repositoryMonitorCommandIntentFixCI, "orka-agents/orka", pr, &store.MonitorItem{}, &input)
	if err != nil {
		t.Fatal(err)
	}
	// The prompt JSON-encodes the evidence; decode the escapes a credential
	// could hide behind.
	prompt = strings.NewReplacer(`\n`, "\n", `\"`, `"`, `\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(prompt)
	return prompt, input
}

// By default, the repair prompt carries no CI free text: only check and
// annotation structure and GitHub links.
func TestRepositoryMonitorCIEvidenceOmitsTextByDefault(t *testing.T) {
	for _, tc := range repositoryMonitorEvidenceTextFixtures() {
		t.Run(tc.name, func(t *testing.T) {
			prompt, input := repositoryMonitorEvidenceTextTestPrompt(t, repositoryMonitorEvidenceTextTestGitHub(tc.input, "https://ci.example.invalid/token/"+tc.name), false)
			for _, secret := range append([]string{"TestDeploy", "deploy failed", "ci.example.invalid"}, tc.secrets...) {
				if strings.Contains(prompt, secret) {
					t.Fatalf("default repair prompt carries CI text %q:\n%s", secret, prompt)
				}
			}
			evidence := input.Evidence
			if !evidence.TextOmitted || len(evidence.Checks) != 2 {
				t.Fatalf("evidence = %+v, want text omitted from two checks", evidence)
			}
			run, status := repositoryMonitorEvidenceByName(t, evidence, "deploy"), repositoryMonitorEvidenceByName(t, evidence, "external-ci")
			if run.ID != "check_run:701" || run.Conclusion != "failure" || run.Status != "completed" || run.App != "github-actions" ||
				run.URL != "https://github.com/orka-agents/orka/actions/runs/9/job/701" || run.Title != "" || run.Summary != "" || run.AnnotationCount != 1 {
				t.Fatalf("check run evidence = %+v", run)
			}
			if len(run.Annotations) != 1 || run.Annotations[0] != (repositoryMonitorCheckAnnotationEntry{Path: "deploy/config.yaml", StartLine: 7, EndLine: 9, Level: "failure"}) {
				t.Fatalf("annotations = %+v, want only their location and level", run.Annotations)
			}
			if status.ID != "commit_status:9101" || status.Conclusion != "failure" || status.Description != "" || status.URL != "" {
				t.Fatalf("commit status evidence = %+v", status)
			}
			if !strings.Contains(prompt, `"textOmitted": true`) {
				t.Fatalf("prompt does not say text was omitted:\n%s", prompt)
			}
		})
	}
}

// A monitor that opts in gets the free text, sanitized.
func TestRepositoryMonitorCIEvidenceTextOptIn(t *testing.T) {
	token := repositoryMonitorEvidenceTestGitHubToken
	text := "--- FAIL: TestDeploy (0.01s)\n    deploy_test.go:12: expected 200, got 401\n\nauth with " + token + " failed"
	prompt, input := repositoryMonitorEvidenceTextTestPrompt(t, repositoryMonitorEvidenceTextTestGitHub(text, "https://ci.example.invalid/job/12"), true)
	if strings.Contains(prompt, token[4:]) || strings.Contains(prompt, "ci.example.invalid") {
		t.Fatalf("opted-in repair prompt leaked a credential or an external link:\n%s", prompt)
	}
	evidence := input.Evidence
	run, status := repositoryMonitorEvidenceByName(t, evidence, "deploy"), repositoryMonitorEvidenceByName(t, evidence, "external-ci")
	if evidence.TextOmitted || !strings.Contains(run.Summary, "deploy_test.go:12: expected 200, got 401") || !strings.Contains(run.Title, "--- FAIL: TestDeploy") ||
		len(run.Annotations) != 1 || !strings.Contains(run.Annotations[0].Message, "expected 200, got 401") || run.Annotations[0].Title == "" ||
		!strings.Contains(status.Description, "expected 200, got 401") || status.URL != "" {
		t.Fatalf("opted-in evidence = %+v", evidence)
	}
	if strings.Contains(prompt, `"textOmitted": true`) {
		t.Fatalf("opted-in prompt says text was omitted:\n%s", prompt)
	}
}

func TestRepositoryMonitorEvidenceGitHubCheckURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://github.com/orka-agents/orka/actions/runs/9/job/101?pr=51#step:4:12": "https://github.com/orka-agents/orka/actions/runs/9/job/101",
		"https://github.com/Orka-Agents/Orka/actions/runs/9":                         "https://github.com/Orka-Agents/Orka/actions/runs/9",
		"https://github.com/orka-agents/orka/actions/runs/9/attempts/2":              "https://github.com/orka-agents/orka/actions/runs/9/attempts/2",
		"https://github.com/orka-agents/orka/runs/123":                               "https://github.com/orka-agents/orka/runs/123",
		"https://github.com/orka-agents/orka/actions/runs/9/job/x":                   "",
		"https://github.com/orka-agents/orka/actions/runs/9/artifacts/5":             "",
		"https://github.com/orka-agents/orka/pull/51/checks":                         "",
		"https://github.com/other/repo/actions/runs/9":                               "",
		"http://github.com/orka-agents/orka/actions/runs/9":                          "",
		"https://github.com.example.invalid/orka-agents/orka/actions/runs/9":         "",
		"https://user@github.com/orka-agents/orka/actions/runs/9":                    "",
		"https://github.com/orka-agents/orka/actions/runs/9%2Fjob%2F1":               "",
		"https://ci.example.invalid/token/" + repositoryMonitorEvidenceLeakValue:     "",
		"https://ci.example.invalid/job/12/console":                                  "",
		"": "",
	} {
		if got := repositoryMonitorEvidenceGitHubCheckURL(raw, "orka-agents", "orka"); got != want {
			t.Errorf("repositoryMonitorEvidenceGitHubCheckURL(%q) = %q, want %q", raw, got, want)
		}
	}
	if got := repositoryMonitorEvidenceGitHubCheckURL("https://github.com/orka-agents/orka/actions/runs/9", "", ""); got != "" {
		t.Errorf("a link without a monitored repository = %q, want it dropped", got)
	}
	// In both modes, a check run link is kept only on GitHub and a commit
	// status link only when it is a GitHub Actions page.
	for _, optIn := range []bool{false, true} {
		github := repositoryMonitorEvidenceTextTestGitHub("failed", "https://github.com/orka-agents/orka/actions/runs/77")
		github.runs["head51"][0]["html_url"] = "https://ci.example.invalid/download/" + repositoryMonitorEvidenceLeakValue
		_, input := repositoryMonitorEvidenceTextTestPrompt(t, github, optIn)
		if run := repositoryMonitorEvidenceByName(t, input.Evidence, "deploy"); run.URL != "" {
			t.Errorf("text=%t: external check run link = %q, want it dropped", optIn, run.URL)
		}
		if status := repositoryMonitorEvidenceByName(t, input.Evidence, "external-ci"); status.URL != "https://github.com/orka-agents/orka/actions/runs/77" {
			t.Errorf("text=%t: GitHub Actions status link = %q, want it kept", optIn, status.URL)
		}
	}
}

// Structured-only evidence is frozen per attempt like text: a retried
// handoff reuses the Task's prompt, and the prompt's input round-trips with
// its digest.
func TestRepositoryMonitorCIEvidenceDefaultIsFrozenPerAttempt(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	run := repositoryMonitorEvidenceTestRun(301, "unit", "completed", "failure", 1)
	run["head_sha"] = "head31"
	github := &repositoryMonitorRepairLabelsGitHub{
		runs:        map[string][]map[string]any{"head31": {run}},
		annotations: map[int64][]map[string]any{301: {{"annotation_level": "failure", "path": "a.go", "start_line": 3, "message": "first failure"}}},
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(github)
	t.Cleanup(server.Close)
	monitor, secret := repositoryMonitorInventoryTestObjects("ci-evidence-default-frozen")
	monitor.Spec.Repair.Enabled = true
	monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
	configureRepositoryMonitorTestWriteCredentials(monitor)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, GitHubAPIBaseURL: server.URL}
	command := &store.CommandEvent{ID: "cmd-ci-evidence-default-frozen", Intent: repositoryMonitorCommandIntentFixCI, Source: "api"}
	pr := repositoryMonitorPullRequest{Number: 31, HeadSHA: "head31", HeadBranch: "feature-fix", HeadRepo: "orka-agents/orka", BaseBranch: "main"}
	item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "31", Number: 31, HeadSHA: pr.HeadSHA}
	monitorRun := &store.MonitorRun{ID: "run-ci-evidence-default-frozen"}
	created, first, err := reconciler.createRepositoryMonitorRepairTaskWithEvidence(ctx, monitor, monitorRun, command, "orka-agents", "orka", pr, item, 1, 1)
	if err != nil || created != 1 || first == nil || !first.Evidence.TextOmitted {
		t.Fatalf("first handoff = (%d, %+v, %v)", created, first, err)
	}
	var task corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: monitor.Namespace, Name: repositoryMonitorRepairTaskName(monitor, pr, command)}, &task); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(task.Spec.Prompt, "first failure") {
		t.Fatalf("default prompt carries annotation text:\n%s", task.Spec.Prompt)
	}
	frozen, ok := repositoryMonitorRepairCIInputFromPrompt(task.Spec.Prompt, pr.HeadSHA)
	if !ok || frozen.digest() != first.digest() || !frozen.Evidence.TextOmitted {
		t.Fatalf("frozen input = %+v ok=%t, want digest %s", frozen, ok, first.digest())
	}

	// GitHub now reports different structure for the same head.
	github.annotations[301][0]["start_line"] = 30
	created, second, err := reconciler.createRepositoryMonitorRepairTaskWithEvidence(ctx, monitor, monitorRun, command, "orka-agents", "orka", pr, item, 1, 1)
	if err != nil || created != 0 || second.digest() != first.digest() {
		t.Fatalf("retried handoff = (%d, %v), want the existing Task's frozen evidence", created, err)
	}
	repositoryMonitorAssertRepairEvidenceRecorded(t, reconciler, monitorStore, monitor, monitorRun, command, pr, task.Name, second)
}
