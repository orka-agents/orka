/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/store/sqlite"
	"github.com/sozercan/orka/internal/workerenv"
)

const (
	githubWebhookTestDefaultBranch = "main"
	githubWebhookTestGitSecret     = "git-credentials"
	githubWebhookTestHeadSHA       = "head-sha"
	githubWebhookTestNewHeadSHA    = "new-head-sha"
)

func TestGitHubWebhook_IssueImplementLabelCreatesAgentTask(t *testing.T) {
	secret := configureGitHubWebhookTest(t, map[string]string{
		githubLabelTriggerAgentEnv:     "codex-agent",
		githubLabelTriggerGitSecretEnv: githubWebhookTestGitSecret,
	})
	fc := newGitHubWebhookFakeClient(t, runtimeAgent("codex-agent"))
	server := NewServer(fc, nil, ServerConfig{})

	body := []byte(`{
		"action":"labeled",
		"label":{"name":"agent:implement"},
		"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
		"issue":{"number":12,"title":"Add health endpoint","body":"Please add /healthz.","html_url":"https://github.com/sozercan/vekil/issues/12"},
		"sender":{"login":"octocat"}
	}`)

	delivery := "delivery-1"
	resp := performSignedGitHubWebhook(t, server, githubEventIssues, delivery, secret, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusCreated, readRespBody(t, resp))
	}

	var task corev1alpha1.Task
	replayKey := githubWebhookReplayKey(body)
	if err := fc.Get(t.Context(), types.NamespacedName{Name: githubWebhookTaskNameForBody(githubActionImplement, 12, body), Namespace: "default"}, &task); err != nil {
		t.Fatalf("created task not found: %v", err)
	}
	if task.Spec.Type != corev1alpha1.TaskTypeAgent {
		t.Fatalf("task type = %q, want agent", task.Spec.Type)
	}
	if task.Spec.AgentRef == nil || task.Spec.AgentRef.Name != "codex-agent" {
		t.Fatalf("agentRef = %#v, want codex-agent", task.Spec.AgentRef)
	}
	if task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.Workspace == nil {
		t.Fatal("agent runtime workspace missing")
	}
	ws := task.Spec.AgentRuntime.Workspace
	if ws.GitRepo != "https://github.com/sozercan/vekil.git" {
		t.Errorf("gitRepo = %q", ws.GitRepo)
	}
	if ws.Branch != githubWebhookTestDefaultBranch {
		t.Errorf("branch = %q, want main", ws.Branch)
	}
	wantPushBranch := "orka/implement-issue-12-" + githubReplayKeySuffix(replayKey)
	if ws.PushBranch != wantPushBranch {
		t.Errorf("pushBranch = %q, want %q", ws.PushBranch, wantPushBranch)
	}
	if ws.GitSecretRef == nil || ws.GitSecretRef.Name != githubWebhookTestGitSecret {
		t.Fatalf("gitSecretRef = %#v, want %s", ws.GitSecretRef, githubWebhookTestGitSecret)
	}
	if task.Labels[labels.LabelCreatedBy] != githubWebhookCreatedBy {
		t.Errorf("created-by label = %q", task.Labels[labels.LabelCreatedBy])
	}
	if task.Labels[labels.LabelGitHubAction] != githubActionImplement {
		t.Errorf("github action label = %q", task.Labels[labels.LabelGitHubAction])
	}
	if task.Annotations[labels.AnnotationGitHubDelivery] != delivery {
		t.Errorf("delivery annotation = %q", task.Annotations[labels.AnnotationGitHubDelivery])
	}
	if !strings.Contains(task.Spec.Prompt, "agent:implement") || !strings.Contains(task.Spec.Prompt, "Please add /healthz.") {
		t.Errorf("prompt missing trigger context: %s", task.Spec.Prompt)
	}
}

func TestGitHubWebhook_PullRequestUpdateBranchUsesHeadBranch(t *testing.T) {
	secret := configureGitHubWebhookTest(t, map[string]string{
		githubLabelTriggerAgentEnv:     "claude-agent",
		githubLabelTriggerGitSecretEnv: githubWebhookTestGitSecret,
	})
	fc := newGitHubWebhookFakeClient(t, runtimeAgent("claude-agent"))
	server := NewServer(fc, nil, ServerConfig{})

	body := []byte(strings.ReplaceAll(`{
		"action":"labeled",
		"label":{"name":"agent:update-branch"},
		"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
		"pull_request":{
			"number":34,
			"title":"Feature branch",
			"body":"Update me",
			"html_url":"https://github.com/sozercan/vekil/pull/34",
			"base":{"ref":"main","sha":"base-sha","repo":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"}},
			"head":{"ref":"feature/x","sha":"{{HEAD_SHA}}","repo":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"}}
		},
		"sender":{"login":"octocat"}
	}`, "{{HEAD_SHA}}", githubWebhookTestHeadSHA))

	delivery := "delivery-2"
	resp := performSignedGitHubWebhook(t, server, githubEventPullRequest, delivery, secret, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusCreated, readRespBody(t, resp))
	}

	var task corev1alpha1.Task
	if err := fc.Get(t.Context(), types.NamespacedName{Name: githubWebhookTaskNameForBody(githubActionUpdateBranch, 34, body), Namespace: "default"}, &task); err != nil {
		t.Fatalf("created task not found: %v", err)
	}
	ws := task.Spec.AgentRuntime.Workspace
	if ws.Branch != "feature/x" {
		t.Errorf("branch = %q, want feature/x", ws.Branch)
	}
	if ws.Ref != githubWebhookTestHeadSHA {
		t.Errorf("ref = %q, want %s", ws.Ref, githubWebhookTestHeadSHA)
	}
	if ws.PushBranch != "feature/x" {
		t.Errorf("pushBranch = %q, want feature/x", ws.PushBranch)
	}
	if ws.GitSecretRef == nil || ws.GitSecretRef.Name != githubWebhookTestGitSecret {
		t.Fatalf("gitSecretRef = %#v, want %s for same-repo PR", ws.GitSecretRef, githubWebhookTestGitSecret)
	}
	if ws.PRBaseBranch != githubWebhookTestDefaultBranch {
		t.Errorf("prBaseBranch = %q, want main", ws.PRBaseBranch)
	}
	if got := githubWebhookTaskEnvValue(task.Spec.Env, workerenv.AllowEmptyPushBranch); got != "true" {
		t.Errorf("%s = %q, want true", workerenv.AllowEmptyPushBranch, got)
	}
	if got := githubWebhookTaskEnvValue(task.Spec.Env, workerenv.PRBaseRepo); got != "https://github.com/sozercan/vekil.git" {
		t.Errorf("%s = %q, want base repo clone URL", workerenv.PRBaseRepo, got)
	}
	if got := githubWebhookTaskEnvValue(task.Spec.Env, workerenv.PRBaseSHA); got != "base-sha" {
		t.Errorf("%s = %q, want base-sha", workerenv.PRBaseSHA, got)
	}
	if !strings.Contains(task.Spec.Prompt, "Update the pull request branch") {
		t.Errorf("prompt = %s", task.Spec.Prompt)
	}
}

func TestGitHubWebhook_PullRequestImplementUsesForkHeadRepo(t *testing.T) {
	secret := configureGitHubWebhookTest(t, map[string]string{
		githubLabelTriggerAgentEnv:     "codex-agent",
		githubLabelTriggerGitSecretEnv: githubWebhookTestGitSecret,
	})
	fc := newGitHubWebhookFakeClient(t, runtimeAgent("codex-agent"))
	server := NewServer(fc, nil, ServerConfig{})

	body := []byte(`{
		"action":"labeled",
		"label":{"name":"agent:implement"},
		"repository":{"full_name":"sozercan/orka","html_url":"https://github.com/sozercan/orka","clone_url":"https://github.com/sozercan/orka.git","default_branch":"main"},
		"pull_request":{
			"number":35,
			"title":"Fork change",
			"body":"Implement on fork",
			"html_url":"https://github.com/sozercan/orka/pull/35",
			"base":{"ref":"main","sha":"base-sha","repo":{"full_name":"sozercan/orka","html_url":"https://github.com/sozercan/orka","clone_url":"https://github.com/sozercan/orka.git","default_branch":"main"}},
			"head":{"ref":"feature/fork-change","sha":"fork-head-sha","repo":{"full_name":"contributor/orka","html_url":"https://github.com/contributor/orka","clone_url":"https://github.com/contributor/orka.git","default_branch":"main"}}
		},
		"sender":{"login":"octocat"}
	}`)

	delivery := "delivery-fork-pr"
	resp := performSignedGitHubWebhook(t, server, githubEventPullRequest, delivery, secret, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusCreated, readRespBody(t, resp))
	}

	var task corev1alpha1.Task
	if err := fc.Get(t.Context(), types.NamespacedName{Name: githubWebhookTaskNameForBody(githubActionImplement, 35, body), Namespace: "default"}, &task); err != nil {
		t.Fatalf("created task not found: %v", err)
	}
	ws := task.Spec.AgentRuntime.Workspace
	if ws.GitRepo != "https://github.com/contributor/orka.git" {
		t.Errorf("gitRepo = %q, want fork head repo", ws.GitRepo)
	}
	if ws.Branch != "feature/fork-change" {
		t.Errorf("branch = %q, want feature/fork-change", ws.Branch)
	}
	if ws.Ref != "fork-head-sha" {
		t.Errorf("ref = %q, want fork-head-sha", ws.Ref)
	}
	if ws.PushBranch != "" {
		t.Errorf("pushBranch = %q, want empty for fork PR without safe git credentials", ws.PushBranch)
	}
	if ws.GitSecretRef != nil {
		t.Fatalf("gitSecretRef = %#v, want nil for fork PR", ws.GitSecretRef)
	}
	if ws.PRBaseBranch != githubWebhookTestDefaultBranch {
		t.Errorf("prBaseBranch = %q, want main", ws.PRBaseBranch)
	}
	if !strings.Contains(task.Spec.Prompt, "Orka will not push them automatically") {
		t.Errorf("prompt missing no-push guidance: %s", task.Spec.Prompt)
	}
}

func TestGitHubWebhook_PullRequestMissingHeadRepoFailsClosedForGitSecret(t *testing.T) {
	secret := configureGitHubWebhookTest(t, map[string]string{
		githubLabelTriggerAgentEnv:     "codex-agent",
		githubLabelTriggerGitSecretEnv: githubWebhookTestGitSecret,
	})
	fc := newGitHubWebhookFakeClient(t, runtimeAgent("codex-agent"))
	server := NewServer(fc, nil, ServerConfig{})

	body := []byte(`{
		"action":"labeled",
		"label":{"name":"agent:implement"},
		"repository":{"full_name":"sozercan/orka","html_url":"https://github.com/sozercan/orka","clone_url":"https://github.com/sozercan/orka.git","default_branch":"main"},
		"pull_request":{
			"number":36,
			"title":"Unknown head repo",
			"body":"Implement with unknown head repo",
			"html_url":"https://github.com/sozercan/orka/pull/36",
			"base":{"ref":"main","sha":"base-sha","repo":{"full_name":"sozercan/orka","html_url":"https://github.com/sozercan/orka","clone_url":"https://github.com/sozercan/orka.git","default_branch":"main"}},
			"head":{"ref":"feature/unknown-head","sha":"unknown-head-sha","repo":null}
		},
		"sender":{"login":"octocat"}
	}`)

	delivery := "delivery-missing-head-pr"
	resp := performSignedGitHubWebhook(t, server, githubEventPullRequest, delivery, secret, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusCreated, readRespBody(t, resp))
	}

	var task corev1alpha1.Task
	if err := fc.Get(t.Context(), types.NamespacedName{Name: githubWebhookTaskNameForBody(githubActionImplement, 36, body), Namespace: "default"}, &task); err != nil {
		t.Fatalf("created task not found: %v", err)
	}
	ws := task.Spec.AgentRuntime.Workspace
	if ws.GitRepo != "https://github.com/sozercan/orka.git" {
		t.Errorf("gitRepo = %q, want base repository fallback", ws.GitRepo)
	}
	if ws.Branch != "feature/unknown-head" {
		t.Errorf("branch = %q, want feature/unknown-head", ws.Branch)
	}
	if ws.PushBranch != "" {
		t.Errorf("pushBranch = %q, want empty for PR without verified head repository", ws.PushBranch)
	}
	if ws.GitSecretRef != nil {
		t.Fatalf("gitSecretRef = %#v, want nil for PR without verified head repository", ws.GitSecretRef)
	}
	if !strings.Contains(task.Spec.Prompt, "Orka will not push them automatically") {
		t.Errorf("prompt missing no-push guidance: %s", task.Spec.Prompt)
	}
}

func TestGitHubWebhook_PullRequestReviewMountsGitSecretWithoutPushBranch(t *testing.T) {
	secret := configureGitHubWebhookTest(t, map[string]string{
		githubLabelTriggerAgentEnv:     "codex-agent",
		githubLabelTriggerGitSecretEnv: githubWebhookTestGitSecret,
	})
	fc := newGitHubWebhookFakeClient(t, runtimeAgent("codex-agent"))
	server := NewServer(fc, nil, ServerConfig{})

	body := []byte(strings.ReplaceAll(`{
		"action":"labeled",
		"label":{"name":"agent:review"},
		"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
		"pull_request":{
			"number":37,
			"title":"Review me",
			"body":"Please review",
			"html_url":"https://github.com/sozercan/vekil/pull/37",
			"base":{"ref":"main","sha":"base-sha","repo":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"}},
			"head":{"ref":"feature/review","sha":"{{HEAD_SHA}}","repo":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"}}
		},
		"sender":{"login":"octocat"}
	}`, "{{HEAD_SHA}}", githubWebhookTestHeadSHA))

	resp := performSignedGitHubWebhook(t, server, githubEventPullRequest, "delivery-review-pr", secret, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusCreated, readRespBody(t, resp))
	}

	var task corev1alpha1.Task
	if err := fc.Get(t.Context(), types.NamespacedName{Name: githubWebhookTaskNameForBody(githubActionReview, 37, body), Namespace: "default"}, &task); err != nil {
		t.Fatalf("created task not found: %v", err)
	}
	ws := task.Spec.AgentRuntime.Workspace
	if ws.PushBranch != "" {
		t.Errorf("pushBranch = %q, want empty for review action", ws.PushBranch)
	}
	if ws.GitSecretRef == nil || ws.GitSecretRef.Name != githubWebhookTestGitSecret {
		t.Fatalf("gitSecretRef = %#v, want %s for private repo clone/auth", ws.GitSecretRef, githubWebhookTestGitSecret)
	}
}

func TestGitHubWebhook_ToIssuesMountsGitSecretWithoutPushBranch(t *testing.T) {
	secret := configureGitHubWebhookTest(t, map[string]string{
		githubLabelTriggerAgentEnv:     "codex-agent",
		githubLabelTriggerGitSecretEnv: githubWebhookTestGitSecret,
	})
	fc := newGitHubWebhookFakeClient(t, runtimeAgent("codex-agent"))
	server := NewServer(fc, nil, ServerConfig{})

	body := []byte(`{
		"action":"labeled",
		"label":{"name":"agent:to-issues"},
		"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
		"issue":{"number":38,"title":"Plan this","body":"Break this into issues.","html_url":"https://github.com/sozercan/vekil/issues/38"},
		"sender":{"login":"octocat"}
	}`)

	resp := performSignedGitHubWebhook(t, server, githubEventIssues, "delivery-to-issues", secret, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusCreated, readRespBody(t, resp))
	}

	var task corev1alpha1.Task
	if err := fc.Get(t.Context(), types.NamespacedName{Name: githubWebhookTaskNameForBody(githubActionToIssues, 38, body), Namespace: "default"}, &task); err != nil {
		t.Fatalf("created task not found: %v", err)
	}
	ws := task.Spec.AgentRuntime.Workspace
	if ws.PushBranch != "" {
		t.Errorf("pushBranch = %q, want empty for to-issues action", ws.PushBranch)
	}
	if ws.GitSecretRef == nil || ws.GitSecretRef.Name != githubWebhookTestGitSecret {
		t.Fatalf("gitSecretRef = %#v, want %s", ws.GitSecretRef, githubWebhookTestGitSecret)
	}
}

func TestGitHubWebhook_IgnoresIssuePullRequestStub(t *testing.T) {
	secret := configureGitHubWebhookTest(t, map[string]string{
		githubLabelTriggerAgentEnv: "codex-agent",
	})
	fc := newGitHubWebhookFakeClient(t, runtimeAgent("codex-agent"))
	server := NewServer(fc, nil, ServerConfig{})

	body := []byte(`{
		"action":"labeled",
		"label":{"name":"agent:update-branch"},
		"repository":{"full_name":"sozercan/orka","html_url":"https://github.com/sozercan/orka","clone_url":"https://github.com/sozercan/orka.git","default_branch":"main"},
		"issue":{"number":35,"title":"Fork change","body":"Implement on fork","html_url":"https://github.com/sozercan/orka/issues/35","pull_request":{"html_url":"https://github.com/sozercan/orka/pull/35"}},
		"sender":{"login":"octocat"}
	}`)

	resp := performSignedGitHubWebhook(t, server, githubEventIssues, "delivery-pr-stub", secret, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d; body: %s", resp.StatusCode, readRespBody(t, resp))
	}
	assertNoTasks(t, fc)
}

func TestGitHubWebhook_SignedPayloadControlsIdempotency(t *testing.T) {
	secret := configureGitHubWebhookTest(t, map[string]string{
		githubLabelTriggerAgentEnv: "codex-agent",
	})
	fc := newGitHubWebhookFakeClient(t, runtimeAgent("codex-agent"))
	server := NewServer(fc, nil, ServerConfig{})
	body := []byte(`{"action":"labeled","label":{"name":"agent:implement"},"repository":{"full_name":"sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},"issue":{"number":1,"title":"Do it","body":"Body","html_url":"https://github.com/sozercan/vekil/issues/1"}}`)

	first := performSignedGitHubWebhook(t, server, githubEventIssues, "same-delivery", secret, body)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d; body: %s", first.StatusCode, readRespBody(t, first))
	}
	duplicate := performSignedGitHubWebhook(t, server, githubEventIssues, "same-delivery", secret, body)
	if duplicate.StatusCode != http.StatusAccepted {
		t.Fatalf("duplicate status = %d; body: %s", duplicate.StatusCode, readRespBody(t, duplicate))
	}
	headerReplay := performSignedGitHubWebhook(t, server, githubEventIssues, "new-delivery", secret, body)
	if headerReplay.StatusCode != http.StatusAccepted {
		t.Fatalf("header replay status = %d; body: %s", headerReplay.StatusCode, readRespBody(t, headerReplay))
	}

	changedBody := []byte(`{"action":"labeled","label":{"name":"agent:implement"},"repository":{"full_name":"sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},"issue":{"number":1,"title":"Do it","body":"Changed body","html_url":"https://github.com/sozercan/vekil/issues/1"}}`)
	second := performSignedGitHubWebhook(t, server, githubEventIssues, "new-delivery", secret, changedBody)
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("changed payload status = %d; body: %s", second.StatusCode, readRespBody(t, second))
	}

	var tasks corev1alpha1.TaskList
	if err := fc.List(t.Context(), &tasks); err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks.Items) != 2 {
		t.Fatalf("task count = %d, want 2", len(tasks.Items))
	}
}

func TestGitHubWebhook_IgnoresNonAgentLabelsAndUnsupportedTargets(t *testing.T) {
	secret := configureGitHubWebhookTest(t, map[string]string{
		githubLabelTriggerAgentEnv: "codex-agent",
	})
	fc := newGitHubWebhookFakeClient(t, runtimeAgent("codex-agent"))
	server := NewServer(fc, nil, ServerConfig{})

	body := []byte(`{"action":"labeled","label":{"name":"bug"},"repository":{"full_name":"sozercan/vekil"},"issue":{"number":1,"title":"Bug","body":"Body","html_url":"https://github.com/sozercan/vekil/issues/1"}}`)
	resp := performSignedGitHubWebhook(t, server, githubEventIssues, "delivery-ignore", secret, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d; body: %s", resp.StatusCode, readRespBody(t, resp))
	}
	assertNoTasks(t, fc)

	body = []byte(`{"action":"labeled","label":{"name":"agent:review"},"repository":{"full_name":"sozercan/vekil"},"issue":{"number":1,"title":"Bug","body":"Body","html_url":"https://github.com/sozercan/vekil/issues/1"}}`)
	resp = performSignedGitHubWebhook(t, server, githubEventIssues, "delivery-review-issue", secret, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d; body: %s", resp.StatusCode, readRespBody(t, resp))
	}
	assertNoTasks(t, fc)
}

func TestGitHubWebhook_PullRequestEventQueuesRepositoryMonitorRun(t *testing.T) {
	secret := configureGitHubWebhookTest(t, nil)
	monitorStore := setupGitHubWebhookMonitorStore(t)
	monitor := githubWebhookRepositoryMonitor("repo-monitor", true)
	fc := newGitHubWebhookFakeClient(t, monitor)
	server := NewServer(fc, nil, ServerConfig{RepositoryMonitorStore: monitorStore})

	body := []byte(strings.ReplaceAll(`{
		"action":"synchronize",
		"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
		"pull_request":{
			"number":34,
			"title":"Feature branch",
			"body":"Update me",
			"html_url":"https://github.com/sozercan/vekil/pull/34",
			"state":"open",
			"base":{"ref":"main","sha":"base-sha","repo":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"}},
			"head":{"ref":"feature/x","sha":"{{HEAD_SHA}}","repo":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"}}
		},
		"sender":{"login":"octocat"}
	}`, "{{HEAD_SHA}}", githubWebhookTestHeadSHA))

	resp := performSignedGitHubWebhook(t, server, githubEventPullRequest, "delivery-exact-1", secret, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusCreated, readRespBody(t, resp))
	}
	assertNoTasks(t, fc)

	runs, _, err := monitorStore.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: "default", MonitorName: "repo-monitor", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorRuns() error = %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %#v, want one exact event run", runs)
	}
	run := runs[0]
	if run.Trigger != githubMonitorTriggerPullRequestEvent || run.TargetKind != repositoryMonitorTargetKindPullRequest || run.TargetNumber != 34 || run.TargetSHA != githubWebhookTestHeadSHA || run.Phase != repositoryMonitorRunPhaseQueued {
		t.Fatalf("run = %#v, want queued exact PR event run", run)
	}
	var signaled corev1alpha1.RepositoryMonitor
	if err := fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "repo-monitor"}, &signaled); err != nil {
		t.Fatalf("Get RepositoryMonitor() error = %v", err)
	}
	if signaled.Annotations[repositoryMonitorRunRequestAnnotation] != run.ID {
		t.Fatalf("run signal annotation = %q, want %q", signaled.Annotations[repositoryMonitorRunRequestAnnotation], run.ID)
	}

	events, _, err := monitorStore.ListMonitorEvents(t.Context(), store.MonitorEventFilter{Namespace: "default", MonitorName: "repo-monitor", RunID: run.ID, EventType: githubMonitorEventTypeExactRunQueued, Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorEvents() error = %v", err)
	}
	if len(events) != 1 || events[0].ItemNumber != 34 || events[0].ItemSHA != githubWebhookTestHeadSHA {
		t.Fatalf("events = %#v, want exact event audit for PR head", events)
	}

	newHeadBody := []byte(strings.ReplaceAll(string(body), githubWebhookTestHeadSHA, githubWebhookTestNewHeadSHA))
	updated := performSignedGitHubWebhook(t, server, githubEventPullRequest, "delivery-exact-2", secret, newHeadBody)
	if updated.StatusCode != http.StatusCreated {
		t.Fatalf("updated status = %d, want %d; body: %s", updated.StatusCode, http.StatusCreated, readRespBody(t, updated))
	}
	runs, _, err = monitorStore.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: "default", MonitorName: "repo-monitor", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorRuns(after newer head) error = %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs after newer head = %#v, want old and new queued exact-event runs", runs)
	}
	foundNewHead := false
	for _, queuedRun := range runs {
		if queuedRun.TargetSHA == githubWebhookTestNewHeadSHA {
			foundNewHead = true
		}
	}
	if !foundNewHead {
		t.Fatalf("runs after newer head = %#v, want queued run for newest head", runs)
	}

	duplicate := performSignedGitHubWebhook(t, server, githubEventPullRequest, "delivery-exact-2", secret, newHeadBody)
	if duplicate.StatusCode != http.StatusAccepted {
		t.Fatalf("duplicate status = %d, want %d; body: %s", duplicate.StatusCode, http.StatusAccepted, readRespBody(t, duplicate))
	}
	runs, _, err = monitorStore.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: "default", MonitorName: "repo-monitor", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorRuns(after duplicate) error = %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs after duplicate = %#v, want duplicate exact event ignored", runs)
	}
}

func TestGitHubWebhook_PullRequestEventRequiresExactEventEnabledMonitor(t *testing.T) {
	secret := configureGitHubWebhookTest(t, nil)
	monitorStore := setupGitHubWebhookMonitorStore(t)
	monitor := githubWebhookRepositoryMonitor("repo-monitor", false)
	fc := newGitHubWebhookFakeClient(t, monitor)
	server := NewServer(fc, nil, ServerConfig{RepositoryMonitorStore: monitorStore})

	body := []byte(strings.ReplaceAll(`{
		"action":"synchronize",
		"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
		"pull_request":{
			"number":34,
			"title":"Feature branch",
			"body":"Update me",
			"html_url":"https://github.com/sozercan/vekil/pull/34",
			"base":{"ref":"main","sha":"base-sha","repo":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"}},
			"head":{"ref":"feature/x","sha":"{{HEAD_SHA}}","repo":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"}}
		}
	}`, "{{HEAD_SHA}}", githubWebhookTestHeadSHA))

	resp := performSignedGitHubWebhook(t, server, githubEventPullRequest, "delivery-exact-disabled", secret, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusAccepted, readRespBody(t, resp))
	}
	runs, _, err := monitorStore.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: "default", MonitorName: "repo-monitor", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorRuns() error = %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %#v, want none when exact events are disabled", runs)
	}
	assertNoTasks(t, fc)
}

func TestGitHubWebhook_PullRequestEventQueuesBehindRunningMonitorRun(t *testing.T) {
	secret := configureGitHubWebhookTest(t, nil)
	monitorStore := setupGitHubWebhookMonitorStore(t)
	monitor := githubWebhookRepositoryMonitor("repo-monitor", true)
	fc := newGitHubWebhookFakeClient(t, monitor)
	server := NewServer(fc, nil, ServerConfig{RepositoryMonitorStore: monitorStore})
	if err := monitorStore.CreateMonitorRun(t.Context(), &store.MonitorRun{
		ID:               "running-run",
		MonitorNamespace: "default",
		MonitorName:      "repo-monitor",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseRunning,
	}); err != nil {
		t.Fatalf("CreateMonitorRun(running) error = %v", err)
	}

	body := []byte(strings.ReplaceAll(`{
		"action":"synchronize",
		"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
		"pull_request":{
			"number":34,
			"title":"Feature branch",
			"body":"Update me",
			"html_url":"https://github.com/sozercan/vekil/pull/34",
			"base":{"ref":"main","sha":"base-sha","repo":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"}},
			"head":{"ref":"feature/x","sha":"{{HEAD_SHA}}","repo":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"}}
		}
	}`, "{{HEAD_SHA}}", githubWebhookTestHeadSHA))

	resp := performSignedGitHubWebhook(t, server, githubEventPullRequest, "delivery-running", secret, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusCreated, readRespBody(t, resp))
	}

	queuedRuns, _, err := monitorStore.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: "default", MonitorName: "repo-monitor", Phase: repositoryMonitorRunPhaseQueued, Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorRuns(queued) error = %v", err)
	}
	if len(queuedRuns) != 1 || queuedRuns[0].TargetSHA != githubWebhookTestHeadSHA {
		t.Fatalf("queued runs = %#v, want one queued exact event run for head %s", queuedRuns, githubWebhookTestHeadSHA)
	}

	runningRuns, _, err := monitorStore.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: "default", MonitorName: "repo-monitor", Phase: repositoryMonitorRunPhaseRunning, Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorRuns(running) error = %v", err)
	}
	if len(runningRuns) != 1 || runningRuns[0].ID != "running-run" {
		t.Fatalf("running runs = %#v, want existing running run retained", runningRuns)
	}
}

func TestGitHubWebhook_PullRequestEventQueuesBehindQueuedManualRun(t *testing.T) {
	secret := configureGitHubWebhookTest(t, nil)
	monitorStore := setupGitHubWebhookMonitorStore(t)
	monitor := githubWebhookRepositoryMonitor("repo-monitor", true)
	fc := newGitHubWebhookFakeClient(t, monitor)
	server := NewServer(fc, nil, ServerConfig{RepositoryMonitorStore: monitorStore})
	if err := monitorStore.CreateMonitorRun(t.Context(), &store.MonitorRun{
		ID:               "manual-run",
		MonitorNamespace: "default",
		MonitorName:      "repo-monitor",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseQueued,
	}); err != nil {
		t.Fatalf("CreateMonitorRun(manual) error = %v", err)
	}

	body := []byte(strings.ReplaceAll(`{
		"action":"synchronize",
		"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
		"pull_request":{
			"number":34,
			"title":"Feature branch",
			"body":"Update me",
			"html_url":"https://github.com/sozercan/vekil/pull/34",
			"base":{"ref":"main","sha":"base-sha","repo":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"}},
			"head":{"ref":"feature/x","sha":"{{HEAD_SHA}}","repo":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"}}
		}
	}`, "{{HEAD_SHA}}", githubWebhookTestHeadSHA))

	resp := performSignedGitHubWebhook(t, server, githubEventPullRequest, "delivery-queued", secret, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusCreated, readRespBody(t, resp))
	}

	queuedRuns, _, err := monitorStore.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: "default", MonitorName: "repo-monitor", Phase: repositoryMonitorRunPhaseQueued, Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorRuns(queued) error = %v", err)
	}
	if len(queuedRuns) != 2 {
		t.Fatalf("queued runs = %#v, want manual run plus exact event run", queuedRuns)
	}
	foundExact := false
	for _, run := range queuedRuns {
		if run.Trigger == githubMonitorTriggerPullRequestEvent && run.TargetSHA == githubWebhookTestHeadSHA {
			foundExact = true
		}
	}
	if !foundExact {
		t.Fatalf("queued runs = %#v, want queued exact event run retained behind manual run", queuedRuns)
	}
}

func TestGitHubWebhook_PullRequestEventUsesDistinctRunIDsForNormalizedMonitorNames(t *testing.T) {
	secret := configureGitHubWebhookTest(t, nil)
	monitorStore := setupGitHubWebhookMonitorStore(t)
	dotted := githubWebhookRepositoryMonitor("repo.monitor", true)
	dashed := githubWebhookRepositoryMonitor("repo-monitor", true)
	fc := newGitHubWebhookFakeClient(t, dotted, dashed)
	server := NewServer(fc, nil, ServerConfig{RepositoryMonitorStore: monitorStore})

	body := []byte(strings.ReplaceAll(`{
		"action":"synchronize",
		"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
		"pull_request":{
			"number":34,
			"title":"Feature branch",
			"body":"Update me",
			"html_url":"https://github.com/sozercan/vekil/pull/34",
			"base":{"ref":"main","sha":"base-sha","repo":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"}},
			"head":{"ref":"feature/x","sha":"{{HEAD_SHA}}","repo":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"}}
		}
	}`, "{{HEAD_SHA}}", githubWebhookTestHeadSHA))

	resp := performSignedGitHubWebhook(t, server, githubEventPullRequest, "delivery-normalized", secret, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusCreated, readRespBody(t, resp))
	}

	runs, _, err := monitorStore.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: "default", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorRuns() error = %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %#v, want one run per matching monitor", runs)
	}
	runIDs := map[string]struct{}{}
	for _, run := range runs {
		runIDs[run.ID] = struct{}{}
	}
	if len(runIDs) != 2 {
		t.Fatalf("runs = %#v, want distinct run IDs for distinct monitor names", runs)
	}
}

func TestGitHubWebhook_RejectsInvalidSignature(t *testing.T) {
	secret := configureGitHubWebhookTest(t, map[string]string{
		githubLabelTriggerAgentEnv: "codex-agent",
	})
	fc := newGitHubWebhookFakeClient(t, runtimeAgent("codex-agent"))
	server := NewServer(fc, nil, ServerConfig{})
	body := []byte(`{"action":"labeled","label":{"name":"agent:implement"}}`)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(githubEventHeader, githubEventIssues)
	req.Header.Set(githubDeliveryHeader, "bad-signature")
	req.Header.Set(githubSignature256Header, signGitHubWebhook(body, "wrong-secret"))
	resp, err := server.app.Test(req)
	if err != nil {
		t.Fatalf("test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	_ = secret
	assertNoTasks(t, fc)
}

func TestGitHubWebhook_PingVerifiesSignatureWithoutAuth(t *testing.T) {
	secret := configureGitHubWebhookTest(t, nil)
	server := NewServer(newGitHubWebhookFakeClient(t), nil, ServerConfig{})
	body := []byte(`{"zen":"Keep it logically awesome."}`)

	resp := performSignedGitHubWebhook(t, server, githubEventPing, "ping-1", secret, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d; body: %s", resp.StatusCode, readRespBody(t, resp))
	}
}

func configureGitHubWebhookTest(t *testing.T, env map[string]string) string {
	t.Helper()
	secret := "webhook-secret"
	keys := []string{
		githubWebhookSecretEnv,
		githubLabelTriggerAgentEnv,
		githubLabelTriggerGitSecretEnv,
		githubLabelTriggerNamespaceEnv,
		githubLabelTriggerPrefixEnv,
		githubLabelTriggerTimeoutEnv,
		githubLabelTriggerMaxTurnsEnv,
		githubActionAgentEnv(githubActionImplement),
		githubActionAgentEnv(githubActionUpdateBranch),
		githubActionAgentEnv(githubActionReview),
		githubActionAgentEnv(githubActionToIssues),
	}
	for _, key := range keys {
		t.Setenv(key, "")
	}
	t.Setenv(githubWebhookSecretEnv, secret)
	for key, value := range env {
		t.Setenv(key, value)
	}
	return secret
}

func newGitHubWebhookFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add orka scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func runtimeAgent(name string) *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex},
		},
	}
}

func githubWebhookRepositoryMonitor(name string, exactEvents bool) *corev1alpha1.RepositoryMonitor {
	return &corev1alpha1.RepositoryMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/sozercan/vekil",
			Branch:  githubWebhookTestDefaultBranch,
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
			Review: corev1alpha1.RepositoryMonitorReviewSpec{
				ExactEventEnabled: exactEvents,
			},
		},
	}
}

func setupGitHubWebhookMonitorStore(t *testing.T) store.RepositoryMonitorStore {
	t.Helper()
	db, err := sqlite.NewDB(":memory:")
	if err != nil {
		t.Fatalf("NewDB() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return sqlite.NewStore(db, ":memory:")
}

func performSignedGitHubWebhook(t *testing.T, server *Server, event, delivery, secret string, body []byte) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(githubEventHeader, event)
	req.Header.Set(githubDeliveryHeader, delivery)
	req.Header.Set(githubSignature256Header, signGitHubWebhook(body, secret))
	resp, err := server.app.Test(req)
	if err != nil {
		t.Fatalf("test request failed: %v", err)
	}
	return resp
}

func githubWebhookTaskNameForBody(action string, number int, body []byte) string {
	return githubTaskName(action, number, githubWebhookReplayKey(body))
}

func signGitHubWebhook(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return githubSignature256Prefix + hex.EncodeToString(mac.Sum(nil))
}

func readRespBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close() //nolint:errcheck
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return buf.String()
}

func githubWebhookTaskEnvValue(envVars []corev1.EnvVar, name string) string {
	for _, envVar := range envVars {
		if envVar.Name == name {
			return envVar.Value
		}
	}
	return ""
}

func assertNoTasks(t *testing.T, c client.Client) {
	t.Helper()
	var tasks corev1alpha1.TaskList
	if err := c.List(t.Context(), &tasks); err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks.Items) != 0 {
		encoded, _ := json.Marshal(tasks.Items)
		t.Fatalf("task count = %d, want 0: %s", len(tasks.Items), string(encoded))
	}
}
