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
	"github.com/sozercan/orka/internal/workerenv"
)

const githubWebhookTestDefaultBranch = "main"

func TestGitHubWebhook_IssueImplementLabelCreatesAgentTask(t *testing.T) {
	secret := configureGitHubWebhookTest(t, map[string]string{
		githubLabelTriggerAgentEnv:     "codex-agent",
		githubLabelTriggerGitSecretEnv: "git-credentials",
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
	if err := fc.Get(t.Context(), types.NamespacedName{Name: githubWebhookTaskNameForDelivery(githubActionImplement, 12, delivery), Namespace: "default"}, &task); err != nil {
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
	wantPushBranch := "orka/implement-issue-12-" + githubReplayKeySuffix(delivery)
	if ws.PushBranch != wantPushBranch {
		t.Errorf("pushBranch = %q, want %q", ws.PushBranch, wantPushBranch)
	}
	if ws.GitSecretRef == nil || ws.GitSecretRef.Name != "git-credentials" {
		t.Fatalf("gitSecretRef = %#v, want git-credentials", ws.GitSecretRef)
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
		githubLabelTriggerGitSecretEnv: "git-credentials",
	})
	fc := newGitHubWebhookFakeClient(t, runtimeAgent("claude-agent"))
	server := NewServer(fc, nil, ServerConfig{})

	body := []byte(`{
		"action":"labeled",
		"label":{"name":"agent:update-branch"},
		"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
		"pull_request":{
			"number":34,
			"title":"Feature branch",
			"body":"Update me",
			"html_url":"https://github.com/sozercan/vekil/pull/34",
			"base":{"ref":"main","sha":"base-sha","repo":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"}},
			"head":{"ref":"feature/x","sha":"head-sha","repo":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"}}
		},
		"sender":{"login":"octocat"}
	}`)

	delivery := "delivery-2"
	resp := performSignedGitHubWebhook(t, server, githubEventPullRequest, delivery, secret, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusCreated, readRespBody(t, resp))
	}

	var task corev1alpha1.Task
	if err := fc.Get(t.Context(), types.NamespacedName{Name: githubWebhookTaskNameForDelivery(githubActionUpdateBranch, 34, delivery), Namespace: "default"}, &task); err != nil {
		t.Fatalf("created task not found: %v", err)
	}
	ws := task.Spec.AgentRuntime.Workspace
	if ws.Branch != "feature/x" {
		t.Errorf("branch = %q, want feature/x", ws.Branch)
	}
	if ws.Ref != "head-sha" {
		t.Errorf("ref = %q, want head-sha", ws.Ref)
	}
	if ws.PushBranch != "feature/x" {
		t.Errorf("pushBranch = %q, want feature/x", ws.PushBranch)
	}
	if ws.GitSecretRef == nil || ws.GitSecretRef.Name != "git-credentials" {
		t.Fatalf("gitSecretRef = %#v, want git-credentials for same-repo PR", ws.GitSecretRef)
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
		githubLabelTriggerGitSecretEnv: "git-credentials",
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
	if err := fc.Get(t.Context(), types.NamespacedName{Name: githubWebhookTaskNameForDelivery(githubActionImplement, 35, delivery), Namespace: "default"}, &task); err != nil {
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
		githubLabelTriggerGitSecretEnv: "git-credentials",
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
	if err := fc.Get(t.Context(), types.NamespacedName{Name: githubWebhookTaskNameForDelivery(githubActionImplement, 36, delivery), Namespace: "default"}, &task); err != nil {
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

func TestGitHubWebhook_DeliveryIDControlsIdempotency(t *testing.T) {
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
	second := performSignedGitHubWebhook(t, server, githubEventIssues, "new-delivery", secret, body)
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("second status = %d; body: %s", second.StatusCode, readRespBody(t, second))
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

func githubWebhookTaskNameForDelivery(action string, number int, delivery string) string {
	return githubTaskName(action, number, delivery)
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
